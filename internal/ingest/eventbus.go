package ingest

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/api"
	"github.com/snarg/tr-engine/internal/auth"
	"github.com/snarg/tr-engine/internal/metrics"
)

// EventBus provides pub-sub event distribution for SSE subscribers.
// It maintains a ring buffer for replay on reconnect.
//
// One mutex covers the ring, the sequence counter and the subscriber set:
// Publish holds it while it numbers an event, adds it to the ring and
// distributes it, and SubscribeSince holds it while it snapshots the ring and
// registers, so an event is either replayed to a new subscriber or sent to it
// live, never both and never neither (§7.3).
type EventBus struct {
	mu          sync.RWMutex
	subscribers map[uint64]*subscriber
	nextID      uint64
	seq         uint64 // last published sequence number

	// Ring buffer for replay (60s of events)
	ring     []api.SSEEvent
	ringSize int
	ringHead int

	log zerolog.Logger
}

type subscriber struct {
	ch     chan api.SSEEvent
	filter api.EventFilter
	// principal is who the subscriber acts as, kept apart from the client's
	// filter; the stream re-check swaps it (§7.5).
	principal *atomic.Pointer[auth.Principal]
	// afterSeq is the last sequence number published before the subscriber
	// registered, at or above the highest replayed one: only later events
	// are sent live.
	afterSeq uint64
}

// NewEventBus creates an event bus with the given ring buffer size.
func NewEventBus(log zerolog.Logger, ringSize int) *EventBus {
	return &EventBus{
		subscribers: make(map[uint64]*subscriber),
		ring:        make([]api.SSEEvent, ringSize),
		ringSize:    ringSize,
		log:         log,
	}
}

// subscriberBuffer is the minimum channel buffer of a subscriber.
const subscriberBuffer = 64

// SubscribeSince registers a subscriber that receives the events principal
// may see (api.SSEEventAllowed) and that match filter. principal is read for
// every event, so the caller can swap it; a nil principal (or a nil pointer
// in it) receives nothing.
//
// With a non-empty lastEventID it also returns the buffered events after
// that ID that pass the same checks. If the ID is no longer buffered (the
// ring wrapped, or the server restarted), every buffered event is returned
// rather than none, so the client doesn't silently miss everything. The
// snapshot and the registration happen under the lock Publish holds, and
// live events are only those numbered after the last replayed one.
//
// The channel's buffer is max(64, len(replay)+64). cancel unsubscribes and
// closes the channel; it may be called more than once.
func (eb *EventBus) SubscribeSince(lastEventID string, filter api.EventFilter, principal *atomic.Pointer[auth.Principal]) ([]api.SSEEvent, <-chan api.SSEEvent, func()) {
	if principal == nil {
		principal = new(atomic.Pointer[auth.Principal]) // holds nil: nothing passes
	}

	eb.mu.Lock()
	var replay []api.SSEEvent
	if lastEventID != "" {
		replay = eb.replayLocked(lastEventID, filter, principal.Load())
	}
	sub := &subscriber{
		ch:        make(chan api.SSEEvent, max(subscriberBuffer, len(replay)+subscriberBuffer)),
		filter:    filter,
		principal: principal,
		// Everything numbered so far is in the ring (and so replayable) or
		// gone; only later events go out live.
		afterSeq: eb.seq,
	}
	id := eb.nextID
	eb.nextID++
	eb.subscribers[id] = sub
	eb.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			eb.mu.Lock()
			delete(eb.subscribers, id)
			close(sub.ch)
			eb.mu.Unlock()
		})
	}
	return replay, sub.ch, cancel
}

// replayLocked returns the buffered events after lastEventID, oldest first,
// that p may see and that match filter; every buffered event if lastEventID
// is not in the ring. eb.mu must be held.
func (eb *EventBus) replayLocked(lastEventID string, filter api.EventFilter, p *auth.Principal) []api.SSEEvent {
	found := false
	for i := 0; i < eb.ringSize; i++ {
		if eb.ring[(eb.ringHead+i)%eb.ringSize].ID == lastEventID {
			found = true
			break
		}
	}

	var events []api.SSEEvent
	after := !found // not buffered: replay everything
	for i := 0; i < eb.ringSize; i++ {
		e := eb.ring[(eb.ringHead+i)%eb.ringSize]
		if e.ID == "" {
			continue
		}
		if !after {
			after = e.ID == lastEventID
			continue
		}
		if matchesFilter(e, filter, p) {
			events = append(events, e)
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
		metrics.SSEEventsDroppedTotal.WithLabelValues("marshal_error").Inc()
		eb.log.Error().Err(err).Str("event_type", e.Type).Msg("sse: dropped event due to marshal error")
		return
	}

	metrics.SSEEventsPublishedTotal.Inc()

	// Numbering, the ring and distribution happen under one lock, so ring
	// order is sequence order and SubscribeSince sees a consistent cut.
	eb.mu.Lock()
	defer eb.mu.Unlock()

	eb.seq++
	seq := eb.seq
	now := time.Now()
	event := api.SSEEvent{
		ID:        fmt.Sprintf("%d-%d", now.UnixMilli(), seq),
		Type:      e.Type,
		SubType:   e.SubType,
		Timestamp: now.UTC().Format(time.RFC3339),
		SystemID:  e.SystemID,
		SiteID:    e.SiteID,
		Tgid:      e.Tgid,
		UnitID:    e.UnitID,
		Emergency: e.Emergency,
		Data:      data,
		Seq:       seq,
	}

	// Add to ring buffer
	if eb.ring[eb.ringHead].ID != "" {
		metrics.SSEEventsDroppedTotal.WithLabelValues("ring_eviction").Inc()
		eb.log.Warn().Str("evicted_id", eb.ring[eb.ringHead].ID).Str("event_type", e.Type).Msg("sse: ring buffer eviction — event lost from replay")
	}
	eb.ring[eb.ringHead] = event
	eb.ringHead = (eb.ringHead + 1) % eb.ringSize

	// Distribute to subscribers
	for _, sub := range eb.subscribers {
		if seq <= sub.afterSeq || !matchesFilter(event, sub.filter, sub.principal.Load()) {
			continue
		}
		select {
		case sub.ch <- event:
		default:
			// Drop if subscriber is slow
			metrics.SSEEventsDroppedTotal.WithLabelValues("slow_subscriber").Inc()
			eb.log.Warn().Str("event_type", e.Type).Str("event_id", event.ID).Int("subscriber_count", len(eb.subscribers)).Msg("sse: dropped event for slow subscriber")
		}
	}
}

// SubscriberCount returns the current number of SSE subscribers.
func (eb *EventBus) SubscriberCount() int {
	eb.mu.RLock()
	n := len(eb.subscribers)
	eb.mu.RUnlock()
	return n
}

// matchesFilter reports whether a subscriber acting as p, with the client
// filter f, gets event e. The principal's per-type access comes first
// (api.SSEEventAllowed, §7.3): it needs the type's scope, and a restricted p
// gets only talkgroup-scoped types for non-zero systems and talkgroups its
// restrictions allow. Only then do the client's filters apply, in which a
// zero-valued event field passes that dimension.
func matchesFilter(e api.SSEEvent, f api.EventFilter, p *auth.Principal) bool {
	if !api.SSEEventAllowed(p, e.Type, e.SystemID, e.Tgid) {
		return false
	}
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
