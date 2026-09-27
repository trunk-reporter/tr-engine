package audio

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/snarg/tr-engine/internal/auth"
)

// listener returns a principal holder for p, as the live-audio handler
// passes to Subscribe.
func listener(p *auth.Principal) *atomic.Pointer[auth.Principal] {
	h := new(atomic.Pointer[auth.Principal])
	h.Store(p)
	return h
}

// unrestricted is a holder with an unrestricted listen principal.
func unrestricted() *atomic.Pointer[auth.Principal] {
	return listener(&auth.Principal{Kind: auth.KindKey, KeyID: 1, Scopes: auth.Scopes{auth.ScopeListen}})
}

func makeFrame(systemID, tgid int) AudioFrame {
	return AudioFrame{
		SystemID:  systemID,
		TGID:      tgid,
		UnitID:    100,
		Seq:       1,
		Timestamp: 1000,
		Format:    AudioFormatPCM,
		Data:      []byte{0x01, 0x02, 0x03, 0x04},
	}
}

func TestAudioBusPublishToSubscriber(t *testing.T) {
	bus := NewAudioBus()
	ch, cancel := bus.Subscribe(AudioFilter{TGIDs: []int{1001}}, unrestricted())
	defer cancel()

	frame := makeFrame(1, 1001)
	bus.Publish(frame)

	select {
	case got := <-ch:
		if got.SystemID != 1 {
			t.Errorf("SystemID = %d, want 1", got.SystemID)
		}
		if got.TGID != 1001 {
			t.Errorf("TGID = %d, want 1001", got.TGID)
		}
		if got.UnitID != 100 {
			t.Errorf("UnitID = %d, want 100", got.UnitID)
		}
		if got.Seq != 1 {
			t.Errorf("Seq = %d, want 1", got.Seq)
		}
		if got.Format != AudioFormatPCM {
			t.Errorf("Format = %v, want PCM", got.Format)
		}
		if len(got.Data) != 4 {
			t.Errorf("Data length = %d, want 4", len(got.Data))
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for frame")
	}
}

func TestAudioBusFilterByTGID(t *testing.T) {
	bus := NewAudioBus()
	ch, cancel := bus.Subscribe(AudioFilter{TGIDs: []int{1001}}, unrestricted())
	defer cancel()

	// Publish frame with wrong TGID
	bus.Publish(makeFrame(1, 2002))

	select {
	case <-ch:
		t.Fatal("received frame that should have been filtered out")
	case <-time.After(200 * time.Millisecond):
		// Expected: no frame received
	}
}

func TestAudioBusFilterBySystem(t *testing.T) {
	bus := NewAudioBus()
	ch, cancel := bus.Subscribe(AudioFilter{SystemIDs: []int{1}, TGIDs: []int{1001}}, unrestricted())
	defer cancel()

	// Publish with wrong system
	bus.Publish(makeFrame(2, 1001))

	select {
	case <-ch:
		t.Fatal("received frame with wrong system ID")
	case <-time.After(200 * time.Millisecond):
		// Expected: filtered out
	}

	// Publish with correct system
	bus.Publish(makeFrame(1, 1001))

	select {
	case got := <-ch:
		if got.SystemID != 1 || got.TGID != 1001 {
			t.Errorf("got SystemID=%d TGID=%d, want 1/1001", got.SystemID, got.TGID)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for matching frame")
	}
}

func TestAudioBusEmptyFilterReceivesAll(t *testing.T) {
	bus := NewAudioBus()
	ch, cancel := bus.Subscribe(AudioFilter{}, unrestricted())
	defer cancel()

	bus.Publish(makeFrame(1, 1001))
	bus.Publish(makeFrame(2, 2002))

	received := 0
	timeout := time.After(500 * time.Millisecond)
	for received < 2 {
		select {
		case <-ch:
			received++
		case <-timeout:
			t.Fatalf("received %d frames, want 2", received)
		}
	}
}

func TestAudioBusCancelUnsubscribes(t *testing.T) {
	bus := NewAudioBus()
	ch, cancel := bus.Subscribe(AudioFilter{}, unrestricted())

	cancel()

	// Channel should be closed
	_, ok := <-ch
	if ok {
		t.Fatal("channel should be closed after cancel")
	}

	// Subscriber count should be 0
	if n := bus.SubscriberCount(); n != 0 {
		t.Errorf("SubscriberCount = %d, want 0", n)
	}

	// Publish should not panic
	bus.Publish(makeFrame(1, 1001))
}

func TestAudioBusSlowSubscriberDropsFrames(t *testing.T) {
	bus := NewAudioBus()
	ch, cancel := bus.Subscribe(AudioFilter{}, unrestricted())
	defer cancel()

	// Publish 300 frames without reading (buffer is 256)
	for i := 0; i < 300; i++ {
		bus.Publish(makeFrame(1, 1001))
	}

	// Drain the channel
	drained := 0
	for {
		select {
		case <-ch:
			drained++
		default:
			goto done
		}
	}
done:

	if drained == 0 {
		t.Fatal("should have received some frames")
	}
	if drained > audioSubscriberBuffer {
		t.Errorf("drained %d frames, should be at most %d (buffer size)", drained, audioSubscriberBuffer)
	}
	t.Logf("drained %d/%d frames (buffer=%d)", drained, 300, audioSubscriberBuffer)
}

func TestAudioBusMultipleSubscribers(t *testing.T) {
	bus := NewAudioBus()

	ch1, cancel1 := bus.Subscribe(AudioFilter{TGIDs: []int{1001}}, unrestricted())
	defer cancel1()
	ch2, cancel2 := bus.Subscribe(AudioFilter{TGIDs: []int{1001}}, unrestricted())
	defer cancel2()

	bus.Publish(makeFrame(1, 1001))

	for i, ch := range []<-chan AudioFrame{ch1, ch2} {
		select {
		case got := <-ch:
			if got.TGID != 1001 {
				t.Errorf("subscriber %d: TGID = %d, want 1001", i, got.TGID)
			}
		case <-time.After(200 * time.Millisecond):
			t.Errorf("subscriber %d: timed out", i)
		}
	}

	if n := bus.SubscriberCount(); n != 2 {
		t.Errorf("SubscriberCount = %d, want 2", n)
	}
}

func TestAudioBusUpdateFilter(t *testing.T) {
	bus := NewAudioBus()
	ch, cancel := bus.Subscribe(AudioFilter{TGIDs: []int{1001}}, unrestricted())
	defer cancel()

	// Update filter to TGID 2002
	bus.UpdateFilter(ch, AudioFilter{TGIDs: []int{2002}})

	// Old TGID should not match
	bus.Publish(makeFrame(1, 1001))

	select {
	case <-ch:
		t.Fatal("received frame for old TGID after filter update")
	case <-time.After(200 * time.Millisecond):
		// Expected
	}

	// New TGID should match
	bus.Publish(makeFrame(1, 2002))

	select {
	case got := <-ch:
		if got.TGID != 2002 {
			t.Errorf("TGID = %d, want 2002", got.TGID)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for frame with updated filter")
	}
}

// restrictedListener is a listen key restricted to system 1's talkgroups
// except 1:1002, plus talkgroup 2:2001.
func restrictedListener() *auth.Principal {
	return &auth.Principal{
		Kind:   auth.KindKey,
		KeyID:  2,
		Scopes: auth.Scopes{auth.ScopeListen},
		Restrictions: []auth.Restriction{{
			Systems:           []int{1},
			Talkgroups:        []auth.TG{{SystemID: 2, Tgid: 2001}},
			ExcludeTalkgroups: []auth.TG{{SystemID: 1, Tgid: 1002}},
		}},
	}
}

func TestPrincipalAllows(t *testing.T) {
	restricted := restrictedListener()
	ticket := &auth.Principal{Kind: auth.KindTicket, KeyID: 3, Scopes: auth.Scopes{auth.ScopeListen},
		Restrictions: []auth.Restriction{*restricted.Restrictions[0].Normalize(), {Talkgroups: []auth.TG{{SystemID: 1, Tgid: 1001}}}}}
	nothing := &auth.Principal{Kind: auth.KindTicket, KeyID: 3, Scopes: auth.Scopes{auth.ScopeListen},
		Restrictions: []auth.Restriction{{}}}
	for _, tc := range []struct {
		name    string
		p       *auth.Principal
		sys, tg int
		want    bool
	}{
		{"nil principal", nil, 1, 1001, false},
		{"no scopes (anonymous, access off)", &auth.Principal{Kind: auth.KindAnonymous}, 1, 1001, false},
		{"upload-only key", &auth.Principal{Kind: auth.KindKey, Scopes: auth.Scopes{auth.ScopeUpload}}, 1, 1001, false},
		{"anonymous listen", &auth.Principal{Kind: auth.KindAnonymous, Scopes: auth.Scopes{auth.ScopeListen}}, 1, 1001, true},
		{"admin key", &auth.Principal{Kind: auth.KindKey, Scopes: auth.Scopes{auth.ScopeAdmin}}, 7, 7, true},
		{"unrestricted, no talkgroup", &auth.Principal{Kind: auth.KindKey, Scopes: auth.Scopes{auth.ScopeListen}}, 1, 0, true},
		{"restricted: allowed system", restricted, 1, 1001, true},
		{"restricted: excluded talkgroup", restricted, 1, 1002, false},
		{"restricted: allowed talkgroup", restricted, 2, 2001, true},
		{"restricted: other talkgroup of that system", restricted, 2, 2002, false},
		{"restricted: other system", restricted, 3, 1001, false},
		{"restricted: zero talkgroup", restricted, 1, 0, false},
		{"restricted: zero system", restricted, 0, 1001, false},
		{"ticket narrowing: both allow", ticket, 1, 1001, true},
		{"ticket narrowing: key allows, narrowing doesn't", ticket, 1, 1003, false},
		{"allow-nothing narrowing", nothing, 1, 1001, false},
	} {
		if got := PrincipalAllows(tc.p, makeFrame(tc.sys, tc.tg)); got != tc.want {
			t.Errorf("%s: PrincipalAllows(%d:%d) = %v, want %v", tc.name, tc.sys, tc.tg, got, tc.want)
		}
	}
}

// TestAudioBusRestrictedSubscriber checks that a restricted subscriber hears
// only allowed talkgroups whatever its filter says, including after a client
// subscribe that tries to widen it, and that a nil principal hears nothing.
func TestAudioBusRestrictedSubscriber(t *testing.T) {
	bus := NewAudioBus()
	holder := listener(restrictedListener())
	ch, cancel := bus.Subscribe(AudioFilter{}, holder)
	defer cancel()

	publishAll := func() {
		for _, f := range [][2]int{{1, 1001}, {1, 1002}, {2, 2001}, {2, 2002}, {3, 3001}, {1, 0}} {
			bus.Publish(makeFrame(f[0], f[1]))
		}
	}
	drain := func() []string {
		var got []string
		for {
			select {
			case f := <-ch:
				got = append(got, fmt.Sprintf("%d:%d", f.SystemID, f.TGID))
			case <-time.After(50 * time.Millisecond):
				return got
			}
		}
	}

	// An empty filter ("everything") is clamped to the restriction.
	publishAll()
	if got := strings.Join(drain(), ","); got != "1:1001,2:2001" {
		t.Errorf("empty filter: got %s, want 1:1001,2:2001", got)
	}

	// A subscribe naming forbidden systems and talkgroups doesn't widen it.
	bus.UpdateFilter(ch, AudioFilter{SystemIDs: []int{1, 2, 3}, TGIDs: []int{1002, 2002, 3001, 2001}})
	publishAll()
	if got := strings.Join(drain(), ","); got != "2:2001" {
		t.Errorf("widening filter: got %s, want 2:2001", got)
	}
	if holder.Load() == nil || !holder.Load().Restricted() {
		t.Error("UpdateFilter changed the subscriber's principal")
	}

	// Swapping the principal takes effect on the next frame.
	holder.Store(&auth.Principal{Kind: auth.KindKey, KeyID: 2, Scopes: auth.Scopes{auth.ScopeListen}})
	publishAll()
	if got := strings.Join(drain(), ","); got != "1:1002,2:2001,2:2002,3:3001" {
		t.Errorf("after swap to unrestricted: got %s, want 1:1002,2:2001,2:2002,3:3001", got)
	}

	// A holder emptied by the re-check (listen lost) hears nothing.
	holder.Store(nil)
	publishAll()
	if got := drain(); len(got) != 0 {
		t.Errorf("nil principal heard %v", got)
	}

	// So does a subscriber registered without a holder.
	ch2, cancel2 := bus.Subscribe(AudioFilter{}, nil)
	defer cancel2()
	bus.Publish(makeFrame(1, 1001))
	select {
	case f := <-ch2:
		t.Errorf("subscriber without a principal heard %d:%d", f.SystemID, f.TGID)
	case <-time.After(50 * time.Millisecond):
	}
}
