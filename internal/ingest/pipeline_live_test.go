package ingest

import (
	"encoding/json"
	"testing"

	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/api"
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
