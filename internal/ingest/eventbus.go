package ingest

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/snarg/tr-engine/internal/api"
	"github.com/snarg/tr-engine/internal/metrics"
)

// EventBus provides pub-sub event distribution for SSE subscribers.
// It maintains a ring buffer for replay on reconnect.
type EventBus struct {
	mu          sync.RWMutex
	subscribers map[uint64]subscriber
	nextID      uint64
	seq         atomic.Uint64

	// Ring buffer for replay (60s of events)
	ring     []api.SSEEvent
	ringSize int
	ringHead int
	ringMu   sync.RWMutex
}

type subscriber struct {
	ch     chan api.SSEEvent
	filter api.EventFilter
}

// NewEventBus creates an event bus with the given ring buffer size.
func NewEventBus(ringSize int) *EventBus {
	return &EventBus{
		subscribers: make(map[uint64]subscriber),
		ring:        make([]api.SSEEvent, ringSize),
		ringSize:    ringSize,
	}
}

// Subscribe registers a new subscriber and returns a channel and cancel function.
// When ctx is cancelled, the subscriber is automatically removed and the channel
// is closed, preventing goroutine leaks if cancel is not called explicitly.
func (eb *EventBus) Subscribe(filter api.EventFilter) (<-chan api.SSEEvent, func()) {
	eb.mu.Lock()
	id := eb.nextID
	eb.nextID++
	ch := make(chan api.SSEEvent, 64)
	eb.subscribers[id] = subscriber{ch: ch, filter: filter}
	eb.mu.Unlock()

	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			eb.mu.Lock()
			delete(eb.subscribers, id)
			close(ch)
			eb.mu.Unlock()
		})
	}
	return ch, cleanup
}

// ReplaySince returns buffered events since the given event ID.
// If lastEventID has been overwritten (ring buffer wrapped), all available events
// are returned so the client doesn't silently miss everything.
func (eb *EventBus) ReplaySince(lastEventID string, filter api.EventFilter) []api.SSEEvent {
	eb.ringMu.RLock()
	defer eb.ringMu.RUnlock()

	var events []api.SSEEvent
	found := lastEventID == ""

	for i := 0; i < eb.ringSize; i++ {
		idx := (eb.ringHead + i) % eb.ringSize
		e := eb.ring[idx]
		if e.ID == "" {
			continue
		}
		if !found {
			if e.ID == lastEventID {
				found = true
			}
			continue
		}
		if matchesFilter(e, filter) {
			events = append(events, e)
		}
	}

	// If the lastEventID was not found (overwritten by ring wrap), replay all
	// available events rather than returning nothing.
	if !found && lastEventID != "" {
		for i := 0; i < eb.ringSize; i++ {
			idx := (eb.ringHead + i) % eb.ringSize
			e := eb.ring[idx]
			if e.ID != "" && matchesFilter(e, filter) {
				events = append(events, e)
			}
		}
	}

	return events
}

// EventData holds all fields needed to publish an SSE event.
type EventData struct {
	Type      string
	SubType   string
	SystemID  int
	SiteID    int
	Tgid      int
	UnitID    int
	Emergency bool
	Payload   any
}

// Publish sends an event to all matching subscribers and adds it to the ring buffer.
func (eb *EventBus) Publish(e EventData) {
	data, err := json.Marshal(e.Payload)
	if err != nil {
		return
	}

	metrics.SSEEventsPublishedTotal.Inc()

	seq := eb.seq.Add(1)
	event := api.SSEEvent{
		ID:        fmt.Sprintf("%d-%d", time.Now().UnixMilli(), seq),
		Type:      e.Type,
		SubType:   e.SubType,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		SystemID:  e.SystemID,
		SiteID:    e.SiteID,
		Tgid:      e.Tgid,
		UnitID:    e.UnitID,
		Emergency: e.Emergency,
		Data:      data,
	}

	// Add to ring buffer
	eb.ringMu.Lock()
	eb.ring[eb.ringHead] = event
	eb.ringHead = (eb.ringHead + 1) % eb.ringSize
	eb.ringMu.Unlock()

	// Distribute to subscribers
	eb.mu.RLock()
	for _, sub := range eb.subscribers {
		if matchesFilter(event, sub.filter) {
			select {
			case sub.ch <- event:
			default:
				// Drop if subscriber is slow
			}
		}
	}
	eb.mu.RUnlock()
}

// SubscriberCount returns the current number of SSE subscribers.
func (eb *EventBus) SubscriberCount() int {
	eb.mu.RLock()
	n := len(eb.subscribers)
	eb.mu.RUnlock()
	return n
}

func matchesFilter(e api.SSEEvent, f api.EventFilter) bool {
	if f.EmergencyOnly && !e.Emergency {
		return false
	}
	if len(f.Types) > 0 {
		match := false
		for _, t := range f.Types {
			t = strings.TrimSpace(t)
			if base, sub, ok := strings.Cut(t, ":"); ok {
				// Compound filter: "unit_event:call" matches type + subtype
				if base == e.Type && sub == e.SubType {
					match = true
					break
				}
			} else {
				if t == e.Type {
					match = true
					break
				}
			}
		}
		if !match {
			return false
		}
	}
	if len(f.Systems) > 0 && e.SystemID != 0 {
		match := false
		for _, s := range f.Systems {
			if s == e.SystemID {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}
	if len(f.Sites) > 0 && e.SiteID != 0 {
		match := false
		for _, s := range f.Sites {
			if s == e.SiteID {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}
	if len(f.Tgids) > 0 && e.Tgid != 0 {
		match := false
		for _, tg := range f.Tgids {
			if tg == e.Tgid {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}
	if len(f.Units) > 0 && e.UnitID != 0 {
		match := false
		for _, u := range f.Units {
			if u == e.UnitID {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}
	return true
}
