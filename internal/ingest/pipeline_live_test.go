package ingest

import (
	"encoding/json"
	"testing"

	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/api"
	"github.com/snarg/tr-engine/internal/auth"
)

// GET /recorders answers "recorders": [] rather than null before any
// recorder has reported (§15).
func TestLatestRecordersNeverNil(t *testing.T) {
	p := &Pipeline{}
	recs := p.LatestRecorders()
	if recs == nil {
		t.Fatal("LatestRecorders() = nil, want an empty slice")
	}
	js, _ := json.Marshal(recs)
	if string(js) != "[]" {
		t.Errorf("JSON = %s, want []", js)
	}

	p.recorderCache.Store("tr-1_0", api.RecorderStateData{ID: "tr-1_0"})
	if recs := p.LatestRecorders(); len(recs) != 1 || recs[0].ID != "tr-1_0" {
		t.Errorf("LatestRecorders() = %+v", recs)
	}
}

func TestPipelineSubscribeSince(t *testing.T) {
	p := &Pipeline{eventBus: NewEventBus(zerolog.Nop(), 16)}
	p.PublishEvent(EventData{Type: "call_start", SystemID: 1, Tgid: 100, Payload: "a"})
	p.PublishEvent(EventData{Type: "console", Payload: "log"})
	replay, ch, cancel := p.SubscribeSince("unknown", api.EventFilter{}, holder(listenKey))
	defer cancel()
	if len(replay) != 1 || replay[0].Type != "call_start" {
		t.Errorf("replay = %+v, want only the call_start (console needs admin)", replay)
	}
	p.PublishEvent(EventData{Type: "call_end", SystemID: 1, Tgid: 100, Payload: "b"})
	if e := <-ch; e.Type != "call_end" {
		t.Errorf("live event = %+v", e)
	}
}

// After a merge, data captured under the merged-away system ID (buffered
// events, active calls, events published later from captured state) is
// checked against restrictions under the target ID, which is what the merge
// rewrote them to (r1-03).
func TestRewriteSystemIDRelabelsInMemoryState(t *testing.T) {
	p := &Pipeline{
		eventBus:    NewEventBus(zerolog.Nop(), 16),
		activeCalls: newActiveCallMap(),
	}
	p.PublishEvent(EventData{Type: "call_end", SystemID: 2, Tgid: 100, Payload: "excluded"})
	p.PublishEvent(EventData{Type: "call_end", SystemID: 2, Tgid: 200, Payload: "allowed"})
	p.activeCalls.Set("tr-2_100", activeCallEntry{CallID: 1, SystemID: 2, Tgid: 100})
	p.activeCalls.Set("tr-2_200", activeCallEntry{CallID: 2, SystemID: 2, Tgid: 200})
	p.activeCalls.Set("tr-3_100", activeCallEntry{CallID: 3, SystemID: 3, Tgid: 100})

	// The merge of 2 into 1 rewrote this restriction from 2:100 to 1:100
	// (keeping 2:100 too, but a restriction created after the merge names
	// only the target).
	excl := &auth.Principal{Kind: auth.KindAnonymous, Scopes: auth.Scopes{auth.ScopeListen},
		Restrictions: []auth.Restriction{{AllowAll: true, ExcludeTalkgroups: []auth.TG{{SystemID: 1, Tgid: 100}}}}}
	allow := &auth.Principal{Kind: auth.KindKey, KeyID: 9, Scopes: auth.Scopes{auth.ScopeListen},
		Restrictions: []auth.Restriction{{Systems: []int{1}}}}
	p.RewriteSystemID(2, 1)

	replay, _, cancel := p.SubscribeSince("unknown", api.EventFilter{}, holder(excl))
	cancel()
	if len(replay) != 1 || replay[0].Tgid != 200 || replay[0].SystemID != 1 {
		t.Errorf("replay after merge = %+v, want only the tg 200 event under system 1", replay)
	}
	replay, _, cancel = p.SubscribeSince("unknown", api.EventFilter{}, holder(allow))
	cancel()
	if len(replay) != 2 {
		t.Errorf("allow-list replay after merge = %d events, want 2 (the data moved to system 1)", len(replay))
	}

	for _, c := range p.ActiveCalls() {
		want := 1
		if c.CallID == 3 {
			want = 3
		}
		if c.SystemID != want {
			t.Errorf("active call %d: system %d, want %d", c.CallID, c.SystemID, want)
		}
	}

	// An event published later from state captured before the merge (a
	// queued transcription) goes out under the target.
	_, ch, cancel := p.SubscribeSince("", api.EventFilter{}, holder(excl))
	defer cancel()
	p.PublishEvent(EventData{Type: "transcription", SystemID: 2, Tgid: 100, Payload: "secret"})
	p.PublishEvent(EventData{Type: "transcription", SystemID: 2, Tgid: 200, Payload: "fine"})
	if e := <-ch; e.Tgid != 200 || e.SystemID != 1 {
		t.Errorf("live event after merge = %+v, want the tg 200 one under system 1", e)
	}

	// Chains: 1 merged into 5 later maps 2 to 5 as well.
	p.RewriteSystemID(1, 5)
	if got := p.currentSystemID(2); got != 5 {
		t.Errorf("currentSystemID(2) after 2→1→5 = %d, want 5", got)
	}
}
