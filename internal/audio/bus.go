package audio

import (
	"sync"
	"sync/atomic"

	"github.com/snarg/tr-engine/internal/auth"
)

const audioSubscriberBuffer = 256

// AudioFilter controls which frames a subscriber receives.
// Empty slices mean "receive all" for that dimension.
// Non-empty slices are OR-matched within the dimension; dimensions are AND-ed.
type AudioFilter struct {
	SystemIDs []int
	TGIDs     []int
}

type audioSubscriber struct {
	ch     chan AudioFrame
	filter AudioFilter // the client's choice; UpdateFilter replaces it
	mu     sync.RWMutex
	// principal is who the subscriber listens as, fixed at Subscribe and
	// kept apart from the client's filter; only its owner swaps it (the
	// stream re-check, §7.5).
	principal *atomic.Pointer[auth.Principal]
}

// AudioBus distributes encoded audio frames to subscribers.
type AudioBus struct {
	mu          sync.RWMutex
	subscribers map[uint64]*audioSubscriber
	nextID      atomic.Uint64
	chToID      map[<-chan AudioFrame]uint64 // reverse lookup for UpdateFilter
}

// NewAudioBus creates a new AudioBus ready for use.
func NewAudioBus() *AudioBus {
	return &AudioBus{
		subscribers: make(map[uint64]*audioSubscriber),
		chToID:      make(map[<-chan AudioFrame]uint64),
	}
}

// Subscribe returns a channel that receives the audio frames principal may
// hear (PrincipalAllows) that match filter, and a cancel function. principal
// is read for every frame, so its owner can swap it; a nil principal (or a
// nil pointer in it) hears nothing.
func (ab *AudioBus) Subscribe(filter AudioFilter, principal *atomic.Pointer[auth.Principal]) (<-chan AudioFrame, func()) {
	if principal == nil {
		principal = new(atomic.Pointer[auth.Principal]) // holds nil: nothing passes
	}
	id := ab.nextID.Add(1)
	ch := make(chan AudioFrame, audioSubscriberBuffer)
	sub := &audioSubscriber{ch: ch, filter: filter, principal: principal}

	ab.mu.Lock()
	ab.subscribers[id] = sub
	ab.chToID[(<-chan AudioFrame)(ch)] = id
	ab.mu.Unlock()

	cancel := func() {
		ab.mu.Lock()
		delete(ab.subscribers, id)
		delete(ab.chToID, (<-chan AudioFrame)(ch))
		ab.mu.Unlock()
		close(ch)
	}
	return ch, cancel
}

// UpdateFilter changes the filter for an existing subscriber identified by its
// channel. It never changes the subscriber's principal, so a client can't
// widen what it hears (§7.4).
func (ab *AudioBus) UpdateFilter(ch <-chan AudioFrame, filter AudioFilter) {
	ab.mu.RLock()
	id, ok := ab.chToID[ch]
	if !ok {
		ab.mu.RUnlock()
		return
	}
	sub := ab.subscribers[id]
	ab.mu.RUnlock()

	sub.mu.Lock()
	sub.filter = filter
	sub.mu.Unlock()
}

// Publish sends a frame to all matching subscribers. Non-blocking -- drops frames
// for slow subscribers rather than blocking the publisher.
func (ab *AudioBus) Publish(frame AudioFrame) {
	ab.mu.RLock()
	defer ab.mu.RUnlock()

	for _, sub := range ab.subscribers {
		sub.mu.RLock()
		match := matchesAudioFilter(frame, sub.filter, sub.principal.Load())
		sub.mu.RUnlock()

		if match {
			select {
			case sub.ch <- frame:
			default:
				// Slow subscriber -- drop frame
			}
		}
	}
}

// SubscriberCount returns the current number of subscribers.
func (ab *AudioBus) SubscriberCount() int {
	ab.mu.RLock()
	defer ab.mu.RUnlock()
	return len(ab.subscribers)
}

// PrincipalAllows reports whether p may hear frame: p needs listen, and a
// restricted p only hears talkgroups every restriction allows (a frame
// without a system or talkgroup never passes a restriction). A nil p hears
// nothing.
func PrincipalAllows(p *auth.Principal, frame AudioFrame) bool {
	if !p.Has(auth.ScopeListen) {
		return false
	}
	return !p.Restricted() || p.AllowsTG(frame.SystemID, frame.TGID)
}

// matchesAudioFilter reports whether a subscriber acting as p, with the
// client filter f, gets frame: p must allow it (PrincipalAllows), whatever
// the client's subscribe message says, and then the filter must match.
func matchesAudioFilter(frame AudioFrame, f AudioFilter, p *auth.Principal) bool {
	if !PrincipalAllows(p, frame) {
		return false
	}
	if len(f.SystemIDs) > 0 {
		found := false
		for _, id := range f.SystemIDs {
			if frame.SystemID == id {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	if len(f.TGIDs) > 0 {
		found := false
		for _, id := range f.TGIDs {
			if frame.TGID == id {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	return true
}
