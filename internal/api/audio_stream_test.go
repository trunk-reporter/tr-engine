package api

import (
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	"github.com/snarg/tr-engine/internal/audio"
	"github.com/snarg/tr-engine/internal/auth"
)

type mockAudioStreamer struct {
	bus     *audio.AudioBus
	enabled bool
}

func (m *mockAudioStreamer) SubscribeAudio(filter audio.AudioFilter, principal *atomic.Pointer[auth.Principal]) (<-chan audio.AudioFrame, func()) {
	return m.bus.Subscribe(filter, principal)
}

func (m *mockAudioStreamer) UpdateAudioFilter(ch <-chan audio.AudioFrame, filter audio.AudioFilter) {
	m.bus.UpdateFilter(ch, filter)
}

func (m *mockAudioStreamer) AudioStreamEnabled() bool { return m.enabled }

func (m *mockAudioStreamer) AudioStreamStatus() *AudioStreamStatusData {
	return &AudioStreamStatusData{Enabled: m.enabled, ConnectedClients: m.bus.SubscriberCount()}
}

func (m *mockAudioStreamer) AudioJitterStats() map[string]audio.StreamJitterSnapshot {
	return nil
}

// newTestAudioStreamServer creates a test HTTP server with the audio stream
// handler, without the auth pipeline: every connection acts as an
// unrestricted listen key.
func newTestAudioStreamServer(streamer AudioStreamer, maxClients int) *httptest.Server {
	return newTestAudioStreamServerAs(streamer, maxClients,
		&auth.Principal{Kind: auth.KindKey, KeyID: 1, Scopes: auth.Scopes{auth.ScopeListen}})
}

// newTestAudioStreamServerAs is newTestAudioStreamServer with every
// connection acting as p (nil: no principal, as without the pipeline).
func newTestAudioStreamServerAs(streamer AudioStreamer, maxClients int, p *auth.Principal) *httptest.Server {
	r := chi.NewRouter()
	h := NewAudioStreamHandler(streamer, maxClients)
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if p != nil {
					req = withPrincipal(req, p, nil)
				}
				next.ServeHTTP(w, req)
			})
		})
		h.Routes(r)
	})
	return httptest.NewServer(r)
}

// wsURL converts an httptest server URL to a WebSocket URL.
func wsURL(s *httptest.Server, path string) string {
	return "ws" + strings.TrimPrefix(s.URL, "http") + path
}

func TestAudioStreamWebSocketConnect(t *testing.T) {
	streamer := &mockAudioStreamer{bus: audio.NewAudioBus(), enabled: true}
	srv := newTestAudioStreamServer(streamer, 10)
	defer srv.Close()

	conn, resp, err := websocket.DefaultDialer.Dial(wsURL(srv, "/api/v1/audio/live"), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusSwitchingProtocols)
	}
}

func TestAudioStreamSubscribeAndReceive(t *testing.T) {
	bus := audio.NewAudioBus()
	streamer := &mockAudioStreamer{bus: bus, enabled: true}
	srv := newTestAudioStreamServer(streamer, 10)
	defer srv.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(srv, "/api/v1/audio/live"), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Send subscribe message for TGID 1001, system 1
	subMsg, _ := json.Marshal(subscribeMsg{
		Type:    "subscribe",
		TGIDs:   []int{1001},
		Systems: []int{1},
	})
	if err := conn.WriteMessage(websocket.TextMessage, subMsg); err != nil {
		t.Fatalf("write subscribe failed: %v", err)
	}

	// Give time for the subscribe to be processed
	time.Sleep(50 * time.Millisecond)

	// Publish a frame to the bus
	bus.Publish(audio.AudioFrame{
		SystemID:   1,
		TGID:       1001,
		UnitID:     100,
		SampleRate: 8000,
		Seq:        1,
		Timestamp:  1000,
		Format:     audio.AudioFormatPCM,
		Data:       []byte{0xDE, 0xAD, 0xBE, 0xEF},
	})

	// Read the binary message
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	msgType, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}

	if msgType != websocket.BinaryMessage {
		t.Fatalf("message type = %d, want BinaryMessage (%d)", msgType, websocket.BinaryMessage)
	}

	if len(data) < 14 {
		t.Fatalf("data length = %d, want >= 14", len(data))
	}

	// Parse 14-byte header
	systemID := binary.BigEndian.Uint16(data[0:2])
	tgid := binary.BigEndian.Uint32(data[2:6])
	// timestamp at bytes 6-9 (skip, it's relative)
	seq := binary.BigEndian.Uint16(data[10:12])
	sampleRate := binary.BigEndian.Uint16(data[12:14])

	if systemID != 1 {
		t.Errorf("system_id = %d, want 1", systemID)
	}
	if tgid != 1001 {
		t.Errorf("tgid = %d, want 1001", tgid)
	}
	if seq != 1 {
		t.Errorf("seq = %d, want 1", seq)
	}
	if sampleRate != 8000 {
		t.Errorf("sample_rate = %d, want 8000", sampleRate)
	}

	// Verify audio payload
	payload := data[14:]
	if len(payload) != 4 {
		t.Errorf("payload length = %d, want 4", len(payload))
	}
	if payload[0] != 0xDE || payload[1] != 0xAD || payload[2] != 0xBE || payload[3] != 0xEF {
		t.Errorf("payload = %x, want DEADBEEF", payload)
	}
}

func TestAudioStreamKeepalive(t *testing.T) {
	streamer := &mockAudioStreamer{bus: audio.NewAudioBus(), enabled: true}
	srv := newTestAudioStreamServer(streamer, 10)
	defer srv.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(srv, "/api/v1/audio/live"), nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	// Wait for the first keepalive (every 15s)
	conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	msgType, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}

	if msgType != websocket.TextMessage {
		t.Fatalf("message type = %d, want TextMessage (%d)", msgType, websocket.TextMessage)
	}

	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if msg["type"] != "keepalive" {
		t.Errorf("type = %v, want keepalive", msg["type"])
	}

	if _, ok := msg["active_streams"]; !ok {
		t.Error("missing active_streams field")
	}
}

func TestAudioStreamMaxClients(t *testing.T) {
	streamer := &mockAudioStreamer{bus: audio.NewAudioBus(), enabled: true}
	srv := newTestAudioStreamServer(streamer, 2)
	defer srv.Close()

	// Connect first two clients (should succeed)
	conn1, _, err := websocket.DefaultDialer.Dial(wsURL(srv, "/api/v1/audio/live"), nil)
	if err != nil {
		t.Fatalf("client 1 dial failed: %v", err)
	}
	defer conn1.Close()

	conn2, _, err := websocket.DefaultDialer.Dial(wsURL(srv, "/api/v1/audio/live"), nil)
	if err != nil {
		t.Fatalf("client 2 dial failed: %v", err)
	}
	defer conn2.Close()

	// Give the server time to register both clients
	time.Sleep(50 * time.Millisecond)

	// Third client should fail
	_, resp, err := websocket.DefaultDialer.Dial(wsURL(srv, "/api/v1/audio/live"), nil)
	if err == nil {
		t.Fatal("client 3 dial should have failed")
	}
	if resp != nil && resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
}
