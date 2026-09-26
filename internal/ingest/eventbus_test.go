package ingest

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/api"
	"github.com/snarg/tr-engine/internal/auth"
)

// ── Principals ────────────────────────────────────────────────────────

func holder(p *auth.Principal) *atomic.Pointer[auth.Principal] {
	h := new(atomic.Pointer[auth.Principal])
	h.Store(p)
	return h
}

var (
	adminKey      = &auth.Principal{Kind: auth.KindKey, KeyID: 1, Scopes: auth.Scopes{auth.ScopeAdmin}}
	editKey       = &auth.Principal{Kind: auth.KindKey, KeyID: 2, Scopes: auth.Scopes{auth.ScopeEdit}}
	listenKey     = &auth.Principal{Kind: auth.KindKey, KeyID: 3, Scopes: auth.Scopes{auth.ScopeListen}}
	anonListen    = &auth.Principal{Kind: auth.KindAnonymous, Scopes: auth.Scopes{auth.ScopeListen}}
	anonOff       = &auth.Principal{Kind: auth.KindAnonymous, Scopes: auth.Scopes{}}
	uploadKey     = &auth.Principal{Kind: auth.KindKey, KeyID: 4, Scopes: auth.Scopes{auth.ScopeUpload}}
	restrictedKey = &auth.Principal{Kind: auth.KindKey, KeyID: 5, Scopes: auth.Scopes{auth.ScopeListen},
		Restrictions: []auth.Restriction{{Talkgroups: []auth.TG{{SystemID: 1, Tgid: 100}}}}}
	// restrictedAnon is an anonymous policy of listen with allow_all minus
	// 1:666.
	restrictedAnon = &auth.Principal{Kind: auth.KindAnonymous, Scopes: auth.Scopes{auth.ScopeListen},
		Restrictions: []auth.Restriction{{AllowAll: true, ExcludeTalkgroups: []auth.TG{{SystemID: 1, Tgid: 666}}}}}
)

// ── EventBus Publish/Subscribe ────────────────────────────────────────

func TestEventBusPublishSubscribe(t *testing.T) {
	t.Run("subscriber_receives_published_event", func(t *testing.T) {
		eb := NewEventBus(zerolog.Nop(), 64)
		replay, ch, cancel := eb.SubscribeSince("", api.EventFilter{}, holder(listenKey))
		defer cancel()
		if replay != nil {
			t.Fatalf("replay without a last event ID: %v", replay)
		}

		eb.Publish(EventData{
			Type:     "call_start",
			SystemID: 1,
			Tgid:     100,
			Payload:  map[string]string{"msg": "hello"},
		})

		select {
		case evt := <-ch:
			if evt.Type != "call_start" {
				t.Errorf("Type = %q, want call_start", evt.Type)
			}
			if evt.SystemID != 1 {
				t.Errorf("SystemID = %d, want 1", evt.SystemID)
			}
			if evt.Tgid != 100 {
				t.Errorf("Tgid = %d, want 100", evt.Tgid)
			}
			if ms, opaque, ok := strings.Cut(evt.ID, "-"); !ok || len(ms) < 13 || len(opaque) != 32 || evt.Seq != 1 {
				t.Errorf("ID = %q, Seq = %d; want <ms>-<32 hex> and 1", evt.ID, evt.Seq)
			}
			// Verify data is valid JSON
			var payload map[string]string
			if err := json.Unmarshal(evt.Data, &payload); err != nil {
				t.Fatalf("Data is not valid JSON: %v", err)
			}
			if payload["msg"] != "hello" {
				t.Errorf("payload msg = %q, want hello", payload["msg"])
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for event")
		}
	})

	t.Run("filtered_subscriber_misses_non_matching", func(t *testing.T) {
		eb := NewEventBus(zerolog.Nop(), 64)
		_, ch, cancel := eb.SubscribeSince("", api.EventFilter{Types: []string{"call_end"}}, holder(listenKey))
		defer cancel()

		eb.Publish(EventData{Type: "call_start", Payload: "x"})

		select {
		case evt := <-ch:
			t.Fatalf("should not receive event, got %+v", evt)
		case <-time.After(50 * time.Millisecond):
			// expected
		}
	})

	t.Run("cancel_stops_delivery", func(t *testing.T) {
		eb := NewEventBus(zerolog.Nop(), 64)
		_, ch, cancel := eb.SubscribeSince("", api.EventFilter{}, holder(listenKey))
		cancel()
		cancel() // idempotent

		eb.Publish(EventData{Type: "call_start", Payload: "x"})

		select {
		case _, ok := <-ch:
			if ok {
				t.Fatal("should not receive event after cancel")
			}
		case <-time.After(50 * time.Millisecond):
			t.Fatal("timed out waiting for closed channel")
		}
		if n := eb.SubscriberCount(); n != 0 {
			t.Errorf("SubscriberCount = %d after cancel", n)
		}
	})

	t.Run("multiple_subscribers", func(t *testing.T) {
		eb := NewEventBus(zerolog.Nop(), 64)
		_, ch1, cancel1 := eb.SubscribeSince("", api.EventFilter{}, holder(listenKey))
		defer cancel1()
		_, ch2, cancel2 := eb.SubscribeSince("", api.EventFilter{}, holder(anonListen))
		defer cancel2()

		eb.Publish(EventData{Type: "call_start", Payload: "x"})

		for i, ch := range []<-chan api.SSEEvent{ch1, ch2} {
			select {
			case evt := <-ch:
				if evt.Type != "call_start" {
					t.Errorf("subscriber %d: Type = %q, want call_start", i, evt.Type)
				}
			case <-time.After(time.Second):
				t.Fatalf("subscriber %d: timed out", i)
			}
		}
	})

	t.Run("principal_is_read_per_event_and_swappable", func(t *testing.T) {
		eb := NewEventBus(zerolog.Nop(), 64)
		h := holder(restrictedKey)
		_, ch, cancel := eb.SubscribeSince("", api.EventFilter{}, h)
		defer cancel()

		eb.Publish(EventData{Type: "call_start", SystemID: 1, Tgid: 200, Payload: "x"})
		eb.Publish(EventData{Type: "call_start", SystemID: 1, Tgid: 100, Payload: "y"})
		h.Store(listenKey) // the re-check widened the key
		eb.Publish(EventData{Type: "call_start", SystemID: 1, Tgid: 200, Payload: "z"})
		h.Store(nil) // the re-check found listen gone
		eb.Publish(EventData{Type: "call_start", SystemID: 1, Tgid: 100, Payload: "w"})

		var got []string
		for len(got) < 2 {
			select {
			case e := <-ch:
				got = append(got, string(e.Data))
			case <-time.After(100 * time.Millisecond):
				t.Fatalf("got %v, want 2 events", got)
			}
		}
		select {
		case e := <-ch:
			t.Fatalf("event after the principal was cleared: %s", e.Data)
		case <-time.After(50 * time.Millisecond):
		}
		// x: 1:200 is outside the restriction; w: the principal was cleared.
		if strings.Join(got, ",") != `"y","z"` {
			t.Errorf("got %v, want y, z", got)
		}
	})

	t.Run("nil_holder_gets_nothing", func(t *testing.T) {
		eb := NewEventBus(zerolog.Nop(), 64)
		_, ch, cancel := eb.SubscribeSince("", api.EventFilter{}, nil)
		defer cancel()
		eb.Publish(EventData{Type: "call_start", SystemID: 1, Tgid: 100, Payload: "x"})
		select {
		case e := <-ch:
			t.Fatalf("subscriber without a principal got %+v", e)
		case <-time.After(50 * time.Millisecond):
		}
	})
}

// ── EventBus SubscribeSince replay ───────────────────────────────────

// ringIDs returns the IDs of the buffered events, oldest first.
func ringIDs(eb *EventBus) []string {
	eb.mu.RLock()
	defer eb.mu.RUnlock()
	var ids []string
	for i := 0; i < eb.ringSize; i++ {
		if e := eb.ring[(eb.ringHead+i)%eb.ringSize]; e.ID != "" {
			ids = append(ids, e.ID)
		}
	}
	return ids
}

func TestEventBusSubscribeSince(t *testing.T) {
	t.Run("replay_after_specific_id", func(t *testing.T) {
		eb := NewEventBus(zerolog.Nop(), 64)
		eb.Publish(EventData{Type: "call_start", Payload: "a"})
		eb.Publish(EventData{Type: "call_end", Payload: "b"})
		eb.Publish(EventData{Type: "call_end", Payload: "c"})
		ids := ringIDs(eb)

		replay, _, cancel := eb.SubscribeSince(ids[0], api.EventFilter{}, holder(listenKey))
		defer cancel()
		if len(replay) != 2 || string(replay[0].Data) != `"b"` || string(replay[1].Data) != `"c"` {
			t.Fatalf("replay = %+v, want b, c", replay)
		}

		replay, _, cancel2 := eb.SubscribeSince(ids[2], api.EventFilter{}, holder(listenKey))
		defer cancel2()
		if len(replay) != 0 {
			t.Errorf("replay after the newest event = %+v, want none", replay)
		}
	})

	t.Run("replay_with_filter_and_principal", func(t *testing.T) {
		eb := NewEventBus(zerolog.Nop(), 64)
		eb.Publish(EventData{Type: "console", Payload: "log"})
		eb.Publish(EventData{Type: "call_start", SystemID: 1, Tgid: 100, Payload: "a"})
		eb.Publish(EventData{Type: "call_start", SystemID: 2, Tgid: 100, Payload: "b"})
		eb.Publish(EventData{Type: "call_start", SystemID: 1, Tgid: 200, Payload: "c"})
		eb.Publish(EventData{Type: "recorder_update", Payload: "r"})

		for _, tc := range []struct {
			name   string
			p      *auth.Principal
			filter api.EventFilter
			want   string
		}{
			{"admin", adminKey, api.EventFilter{}, `"log","a","b","c","r"`},
			{"listen", listenKey, api.EventFilter{}, `"a","b","c","r"`},
			{"listen, system filter", listenKey, api.EventFilter{Systems: []int{1}}, `"a","c","r"`},
			{"restricted", restrictedKey, api.EventFilter{}, `"a"`},
			{"restricted, filter can't widen", restrictedKey, api.EventFilter{Systems: []int{2}}, ``},
			{"anonymous off", anonOff, api.EventFilter{}, ``},
		} {
			replay, _, cancel := eb.SubscribeSince("unknown-id", tc.filter, holder(tc.p))
			cancel()
			var got []string
			for _, e := range replay {
				got = append(got, string(e.Data))
			}
			if strings.Join(got, ",") != tc.want {
				t.Errorf("%s: replay = %s, want %s", tc.name, strings.Join(got, ","), tc.want)
			}
		}
	})

	t.Run("unknown_lastID_replays_all", func(t *testing.T) {
		eb := NewEventBus(zerolog.Nop(), 4)
		for i := 0; i < 6; i++ { // wraps the ring: the first two are gone
			eb.Publish(EventData{Type: "call_start", Payload: i})
		}
		replay, _, cancel := eb.SubscribeSince("1-1", api.EventFilter{}, holder(listenKey))
		defer cancel()
		// When lastEventID is not found (overwritten by ring wrap), all
		// available events are returned so the client doesn't silently miss
		// everything.
		if len(replay) != 4 || replay[0].Seq != 3 || replay[3].Seq != 6 {
			t.Fatalf("replay = %+v, want seqs 3..6", replay)
		}
	})

	t.Run("buffer_leaves_room_after_replay", func(t *testing.T) {
		eb := NewEventBus(zerolog.Nop(), 256)
		for i := 0; i < 101; i++ {
			eb.Publish(EventData{Type: "call_start", Payload: i})
		}
		first := ringIDs(eb)[0]
		replay, ch, cancel := eb.SubscribeSince(first, api.EventFilter{}, holder(listenKey))
		defer cancel()
		if len(replay) != 100 || cap(ch) != 164 {
			t.Errorf("len(replay) = %d, cap(ch) = %d; want 100 and 164", len(replay), cap(ch))
		}
		_, ch2, cancel2 := eb.SubscribeSince("", api.EventFilter{}, holder(listenKey))
		defer cancel2()
		if cap(ch2) != 64 {
			t.Errorf("cap(ch) without replay = %d, want 64", cap(ch2))
		}
	})

	t.Run("replayed_events_are_not_sent_live", func(t *testing.T) {
		eb := NewEventBus(zerolog.Nop(), 64)
		eb.Publish(EventData{Type: "call_start", Payload: "a"})
		eb.Publish(EventData{Type: "call_start", Payload: "b"})
		replay, ch, cancel := eb.SubscribeSince("unknown", api.EventFilter{}, holder(listenKey))
		defer cancel()
		eb.Publish(EventData{Type: "call_start", Payload: "c"})
		if len(replay) != 2 {
			t.Fatalf("replay = %+v", replay)
		}
		select {
		case e := <-ch:
			if string(e.Data) != `"c"` || e.Seq != 3 {
				t.Errorf("live event = %s (seq %d), want \"c\" (seq 3)", e.Data, e.Seq)
			}
		case <-time.After(time.Second):
			t.Fatal("no live event")
		}
		select {
		case e := <-ch:
			t.Errorf("extra live event %s", e.Data)
		case <-time.After(50 * time.Millisecond):
		}
	})
}

// TestSubscribeSinceConcurrentPublish subscribes with a last event ID while
// several goroutines publish, and checks that replay + live events are
// exactly the events after that ID: no gap, no duplicate. Run it with -race.
//
// Fewer events are published after the prefill than a subscriber's channel
// holds, so a slow reader can't cause drops (which would look like gaps);
// the scenario is repeated to vary the interleaving.
func TestSubscribeSinceConcurrentPublish(t *testing.T) {
	const (
		rounds     = 200
		prefill    = 200
		publishers = 4
		perPub     = 15 // publishers*perPub < subscriberBuffer
		joiners    = 12
		total      = prefill + publishers*perPub
	)
	for round := 0; round < rounds; round++ {
		eb := NewEventBus(zerolog.Nop(), 1024) // the ring never wraps
		for i := 0; i < prefill; i++ {
			eb.Publish(EventData{Type: "call_start", SystemID: 1, Tgid: 100, Payload: i})
		}
		ids := ringIDs(eb)

		type result struct {
			afterSeq uint64
			seqs     []uint64
			cancel   func()
			ch       <-chan api.SSEEvent
		}
		results := make([]result, joiners)

		var wg sync.WaitGroup
		start := make(chan struct{})
		for p := 0; p < publishers; p++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for i := 0; i < perPub; i++ {
					eb.Publish(EventData{Type: "call_start", SystemID: 1, Tgid: 100, Payload: i})
					runtime.Gosched()
				}
			}()
		}
		for j := 0; j < joiners; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for range j % 4 {
					runtime.Gosched()
				}
				idx := (j*17 + round) % prefill
				replay, ch, cancel := eb.SubscribeSince(ids[idx], api.EventFilter{}, holder(listenKey))
				r := result{afterSeq: uint64(idx + 1), cancel: cancel, ch: ch}
				for _, e := range replay {
					r.seqs = append(r.seqs, e.Seq)
				}
				results[j] = r
			}()
		}
		close(start)
		wg.Wait()

		for j := range results {
			r := &results[j]
			r.cancel()
			for e := range r.ch { // everything sent live, then closed
				r.seqs = append(r.seqs, e.Seq)
			}
			want := uint64(total) - r.afterSeq
			if uint64(len(r.seqs)) != want {
				t.Errorf("round %d joiner %d (after %d): got %d events, want %d", round, j, r.afterSeq, len(r.seqs), want)
			}
			for i, seq := range r.seqs {
				if seq != r.afterSeq+uint64(i)+1 {
					t.Errorf("round %d joiner %d (after %d): event %d has seq %d, want %d (gap or duplicate)",
						round, j, r.afterSeq, i, seq, r.afterSeq+uint64(i)+1)
					break
				}
			}
		}
		if t.Failed() {
			return
		}
	}
}

func TestMatchesFilter(t *testing.T) {
	tests := []struct {
		name   string
		event  api.SSEEvent
		filter api.EventFilter
		want   bool
	}{
		// Empty filter matches everything
		{
			name:   "empty_filter_matches_all",
			event:  api.SSEEvent{Type: "call_start", SystemID: 1, Tgid: 100},
			filter: api.EventFilter{},
			want:   true,
		},

		// Type matching
		{
			name:   "type_match",
			event:  api.SSEEvent{Type: "call_start"},
			filter: api.EventFilter{Types: []string{"call_start"}},
			want:   true,
		},
		{
			name:   "type_no_match",
			event:  api.SSEEvent{Type: "call_start"},
			filter: api.EventFilter{Types: []string{"call_end"}},
			want:   false,
		},
		{
			name:   "type_multiple_one_matches",
			event:  api.SSEEvent{Type: "call_end"},
			filter: api.EventFilter{Types: []string{"call_start", "call_end"}},
			want:   true,
		},

		// Compound type syntax
		{
			name:   "compound_type_exact_match",
			event:  api.SSEEvent{Type: "unit_event", SubType: "call"},
			filter: api.EventFilter{Types: []string{"unit_event:call"}},
			want:   true,
		},
		{
			name:   "compound_type_wrong_subtype",
			event:  api.SSEEvent{Type: "unit_event", SubType: "on"},
			filter: api.EventFilter{Types: []string{"unit_event:call"}},
			want:   false,
		},
		{
			name:   "plain_type_matches_any_subtype",
			event:  api.SSEEvent{Type: "unit_event", SubType: "call"},
			filter: api.EventFilter{Types: []string{"unit_event"}},
			want:   true,
		},
		{
			name:   "mixed_compound_and_plain",
			event:  api.SSEEvent{Type: "call_start"},
			filter: api.EventFilter{Types: []string{"unit_event:call", "call_start"}},
			want:   true,
		},

		// System filter
		{
			name:   "system_match",
			event:  api.SSEEvent{Type: "call_start", SystemID: 1},
			filter: api.EventFilter{Systems: []int{1, 2}},
			want:   true,
		},
		{
			name:   "system_no_match",
			event:  api.SSEEvent{Type: "call_start", SystemID: 3},
			filter: api.EventFilter{Systems: []int{1, 2}},
			want:   false,
		},
		{
			name:   "system_zero_passes_through",
			event:  api.SSEEvent{Type: "recorder_update", SystemID: 0},
			filter: api.EventFilter{Systems: []int{1}},
			want:   true,
		},

		// Site filter
		{
			name:   "site_match",
			event:  api.SSEEvent{Type: "call_start", SiteID: 5},
			filter: api.EventFilter{Sites: []int{5}},
			want:   true,
		},
		{
			name:   "site_zero_passes_through",
			event:  api.SSEEvent{Type: "rate_update", SiteID: 0},
			filter: api.EventFilter{Sites: []int{5}},
			want:   true,
		},

		// Tgid filter
		{
			name:   "tgid_match",
			event:  api.SSEEvent{Type: "call_start", Tgid: 100},
			filter: api.EventFilter{Tgids: []int{100, 200}},
			want:   true,
		},
		{
			name:   "tgid_no_match",
			event:  api.SSEEvent{Type: "call_start", Tgid: 300},
			filter: api.EventFilter{Tgids: []int{100, 200}},
			want:   false,
		},

		// Unit filter
		{
			name:   "unit_match",
			event:  api.SSEEvent{Type: "unit_event", UnitID: 42},
			filter: api.EventFilter{Units: []int{42}},
			want:   true,
		},
		{
			name:   "unit_zero_passes_through",
			event:  api.SSEEvent{Type: "call_start", UnitID: 0},
			filter: api.EventFilter{Units: []int{42}},
			want:   true,
		},

		// Multi-dimension AND logic
		{
			name:   "multi_all_pass",
			event:  api.SSEEvent{Type: "call_start", SystemID: 1, Tgid: 100},
			filter: api.EventFilter{Types: []string{"call_start"}, Systems: []int{1}, Tgids: []int{100}},
			want:   true,
		},
		{
			name:   "multi_one_fails",
			event:  api.SSEEvent{Type: "call_start", SystemID: 1, Tgid: 300},
			filter: api.EventFilter{Types: []string{"call_start"}, Systems: []int{1}, Tgids: []int{100}},
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesFilter(tt.event, tt.filter, adminKey)
			if got != tt.want {
				t.Errorf("matchesFilter(%+v, %+v) = %v, want %v", tt.event, tt.filter, got, tt.want)
			}
		})
	}
}

// TestMatchesFilterPrincipal checks the per-type access of §7.3 for every
// event type and principal kind, before any client filter.
func TestMatchesFilterPrincipal(t *testing.T) {
	events := map[string]api.SSEEvent{
		"call_start":          {Type: "call_start", SystemID: 1, Tgid: 100},
		"call_start other tg": {Type: "call_start", SystemID: 1, Tgid: 200},
		"call_start no sys":   {Type: "call_start", SystemID: 0, Tgid: 100},
		"call_update":         {Type: "call_update", SystemID: 1, Tgid: 100},
		"call_end":            {Type: "call_end", SystemID: 1, Tgid: 100},
		"call_end excluded":   {Type: "call_end", SystemID: 1, Tgid: 666},
		"transcription":       {Type: "transcription", SystemID: 1, Tgid: 100},
		"unit_event call":     {Type: "unit_event", SubType: "call", SystemID: 1, Tgid: 100, UnitID: 7},
		"unit_event on":       {Type: "unit_event", SubType: "on", SystemID: 1, UnitID: 7},
		"unit_event off":      {Type: "unit_event", SubType: "off", SystemID: 1, UnitID: 7},
		"recorder_update":     {Type: "recorder_update"},
		"rate_update":         {Type: "rate_update", SystemID: 1},
		"trunking_message":    {Type: "trunking_message", SystemID: 1},
		"console":             {Type: "console"},
		"future type":         {Type: "future_type", SystemID: 1, Tgid: 100},
	}
	listenAll := map[string]bool{
		"call_start": true, "call_start other tg": true, "call_start no sys": true, "call_update": true,
		"call_end": true, "call_end excluded": true, "transcription": true, "unit_event call": true,
		"unit_event on": true, "unit_event off": true, "recorder_update": true, "rate_update": true,
		"trunking_message": true,
	}
	adminAll := map[string]bool{"console": true, "future type": true}
	for k := range listenAll {
		adminAll[k] = true
	}
	for _, tc := range []struct {
		name string
		p    *auth.Principal
		want map[string]bool // events delivered; every other one is dropped
	}{
		{"nil principal", nil, nil},
		{"anonymous, access off", anonOff, nil},
		{"upload-only key", uploadKey, nil},
		{"anonymous listen", anonListen, listenAll},
		{"listen key", listenKey, listenAll},
		{"edit key", editKey, listenAll},
		{"admin key", adminKey, adminAll},
		{"restricted key", restrictedKey, map[string]bool{
			"call_start": true, "call_update": true, "call_end": true, "transcription": true, "unit_event call": true,
		}},
		{"restricted anonymous policy", restrictedAnon, map[string]bool{
			"call_start": true, "call_start other tg": true, "call_update": true, "call_end": true,
			"transcription": true, "unit_event call": true,
		}},
	} {
		for name, e := range events {
			if got := matchesFilter(e, api.EventFilter{}, tc.p); got != tc.want[name] {
				t.Errorf("%s / %s: matchesFilter = %v, want %v", tc.name, name, got, tc.want[name])
			}
		}
	}

	// A restricted principal gets no zero-value pass-through, and a client
	// filter never widens it.
	for _, f := range []api.EventFilter{{}, {Systems: []int{1, 2}}, {Tgids: []int{100, 200}}, {Types: []string{"recorder_update", "unit_event"}}} {
		if matchesFilter(events["recorder_update"], f, restrictedKey) || matchesFilter(events["unit_event on"], f, restrictedKey) ||
			matchesFilter(events["call_start other tg"], f, restrictedKey) {
			t.Errorf("filter %+v widened a restricted principal", f)
		}
	}
	// The client filter still narrows what the principal allows.
	if matchesFilter(events["call_start"], api.EventFilter{Types: []string{"call_end"}}, restrictedKey) {
		t.Error("the client's type filter was ignored for a restricted principal")
	}
}

// Event IDs don't reveal the global sequence: a restricted subscriber can't
// count the events it wasn't shown from gaps between its IDs (r1-13). They
// are still unique, and replay still finds them.
func TestEventBusOpaqueIDs(t *testing.T) {
	eb := NewEventBus(zerolog.Nop(), 64)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		eb.Publish(EventData{Type: "call_end", SystemID: 1, Tgid: 100 + i%2, Payload: i})
	}
	ids := ringIDs(eb)
	for i, id := range ids {
		if seen[id] {
			t.Fatalf("duplicate event ID %q", id)
		}
		seen[id] = true
		_, opaque, _ := strings.Cut(id, "-")
		if _, err := hex.DecodeString(opaque); err != nil || len(opaque) != 32 {
			t.Errorf("ID %q: opaque part is not 32 hex digits", id)
		}
		if strings.HasSuffix(id, fmt.Sprintf("-%d", i+1)) {
			t.Errorf("ID %q ends in its sequence number", id)
		}
	}
	// Another bus (another process) numbers the same sequence differently.
	other := NewEventBus(zerolog.Nop(), 4)
	other.Publish(EventData{Type: "call_end", Payload: 0})
	if o := ringIDs(other)[0]; o[strings.Index(o, "-"):] == ids[0][strings.Index(ids[0], "-"):] {
		t.Error("two buses gave sequence 1 the same ID")
	}

	restricted := &auth.Principal{Kind: auth.KindKey, KeyID: 7, Scopes: auth.Scopes{auth.ScopeListen},
		Restrictions: []auth.Restriction{{Talkgroups: []auth.TG{{SystemID: 1, Tgid: 100}}}}}
	replay, _, cancel := eb.SubscribeSince(ids[9], api.EventFilter{}, holder(restricted))
	defer cancel()
	if len(replay) != 20 {
		t.Errorf("replay after the 10th event = %d events, want the 20 later tg 100 ones", len(replay))
	}
}

// Once the ring is full every publish evicts an event; that is warned about
// at most once per evictionLogInterval, with a count (not once per event).
func TestEventBusEvictionWarningRateLimited(t *testing.T) {
	var logs bytes.Buffer
	eb := NewEventBus(zerolog.New(&logs), 4)
	for i := 0; i < 100; i++ {
		eb.Publish(EventData{Type: "call_end", SystemID: 1, Tgid: 1, Payload: i})
	}
	if n := strings.Count(logs.String(), "replay buffer full"); n != 1 {
		t.Fatalf("%d eviction warnings for 96 evictions, want 1:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), `"evicted":1`) {
		t.Errorf("first warning should count the first eviction: %s", logs.String())
	}
	// After the interval the next eviction reports how many happened since.
	eb.mu.Lock()
	eb.evictLogTime = time.Now().Add(-evictionLogInterval)
	eb.mu.Unlock()
	logs.Reset()
	eb.Publish(EventData{Type: "call_end", SystemID: 1, Tgid: 1, Payload: "x"})
	if !strings.Contains(logs.String(), `"evicted":96`) {
		t.Errorf("second warning = %s, want the 96 evictions since the first", logs.String())
	}
}
