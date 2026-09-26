package api

// Real-HTTP tests of the SSE stream and the live-audio WebSocket through the
// whole pipeline (buildRouter over a real PostgreSQL): per-principal
// filtering, replay, and the re-check and close signals of §7.5. Skipped
// unless TEST_DATABASE_URL is set (see auth_db_test.go).

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/audio"
	"github.com/snarg/tr-engine/internal/auth"
	"github.com/snarg/tr-engine/internal/database"
)

// streamTestLive is a LiveDataSource with a minimal event bus: it applies
// the subscriber's principal (read for every event) and the client's type
// filter, and replays after a last event ID. The real bus is tested in
// internal/ingest; this one lets the api tests drive the stream handler.
type streamTestLive struct {
	LiveDataSource // nil; only SubscribeSince is used

	mu   sync.Mutex
	seq  uint64
	ring []SSEEvent
	subs map[*streamTestSub]bool
}

type streamTestSub struct {
	ch        chan SSEEvent
	filter    EventFilter
	principal *atomic.Pointer[auth.Principal]
}

func newStreamTestLive() *streamTestLive {
	return &streamTestLive{subs: make(map[*streamTestSub]bool)}
}

func (l *streamTestLive) allowed(e SSEEvent, f EventFilter, p *auth.Principal) bool {
	if !SSEEventAllowed(p, e.Type, e.SystemID, e.Tgid) {
		return false
	}
	if len(f.Types) == 0 {
		return true
	}
	for _, t := range f.Types {
		if t == e.Type {
			return true
		}
	}
	return false
}

func (l *streamTestLive) SubscribeSince(lastID string, f EventFilter, p *atomic.Pointer[auth.Principal]) ([]SSEEvent, <-chan SSEEvent, func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var replay []SSEEvent
	if lastID != "" {
		after := false
		for _, e := range l.ring {
			if after && l.allowed(e, f, p.Load()) {
				replay = append(replay, e)
			}
			after = after || e.ID == lastID
		}
	}
	sub := &streamTestSub{ch: make(chan SSEEvent, 256), filter: f, principal: p}
	l.subs[sub] = true
	return replay, sub.ch, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.subs[sub] {
			delete(l.subs, sub)
			close(sub.ch)
		}
	}
}

// publish sends an event whose data is the JSON string data.
func (l *streamTestLive) publish(typ string, sys, tg int, data string) SSEEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	js, _ := json.Marshal(data)
	e := SSEEvent{ID: fmt.Sprintf("%d-%d", time.Now().UnixMilli(), l.seq), Type: typ, SystemID: sys, Tgid: tg, Data: js, Seq: l.seq}
	l.ring = append(l.ring, e)
	for sub := range l.subs {
		if l.allowed(e, sub.filter, sub.principal.Load()) {
			sub.ch <- e
		}
	}
	return e
}

// sseMsg is one message read from an SSE stream.
type sseMsg struct {
	ID, Event, Data string
}

// sseClient reads an SSE response in the background.
type sseClient struct {
	msgs chan sseMsg // closed when the stream ends
}

// openSSE connects to an SSE URL and fails the test unless it answers 200.
func openSSE(t *testing.T, target string, header http.Header) *sseClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("GET %s: %v", target, err)
	}
	if resp.StatusCode != http.StatusOK {
		var body strings.Builder
		bufio.NewReader(resp.Body).WriteTo(&body)
		resp.Body.Close()
		cancel()
		t.Fatalf("GET %s: %d %s", target, resp.StatusCode, body.String())
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	c := &sseClient{msgs: make(chan sseMsg, 256)}
	go func() {
		defer close(c.msgs)
		sc := bufio.NewScanner(resp.Body)
		var m sseMsg
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if m != (sseMsg{}) {
					c.msgs <- m
				}
				m = sseMsg{}
			case strings.HasPrefix(line, ":"):
			case strings.HasPrefix(line, "id: "):
				m.ID = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				m.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				m.Data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		resp.Body.Close()
	})
	return c
}

// next returns the next message, or fails the test after timeout. ok is
// false when the stream ended.
func (c *sseClient) next(t *testing.T, timeout time.Duration) (sseMsg, bool) {
	t.Helper()
	select {
	case m, ok := <-c.msgs:
		return m, ok
	case <-time.After(timeout):
		t.Fatalf("no SSE message within %s", timeout)
		return sseMsg{}, false
	}
}

// expect reads the next message and checks it is an event with JSON string
// data want.
func (c *sseClient) expect(t *testing.T, event, want string) sseMsg {
	t.Helper()
	m, ok := c.next(t, 3*time.Second)
	js, _ := json.Marshal(want)
	if !ok || m.Event != event || m.Data != string(js) {
		t.Fatalf("got %+v (open=%v), want event %s data %s", m, ok, event, js)
	}
	return m
}

// expectClosed reads the auth signal and then the end of the stream.
func (c *sseClient) expectClosed(t *testing.T, code string, timeout time.Duration) {
	t.Helper()
	m, ok := c.next(t, timeout)
	if !ok || m.Event != "auth" || m.Data != `{"code":"`+code+`"}` || m.ID != "" {
		t.Fatalf("got %+v (open=%v), want event auth with code %s and no id", m, ok, code)
	}
	if m, ok := c.next(t, 3*time.Second); ok {
		t.Fatalf("the stream went on after the auth signal: %+v", m)
	}
}

// expectNothing checks no message arrives within d.
func (c *sseClient) expectNothing(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case m, ok := <-c.msgs:
		t.Fatalf("unexpected message %+v (open=%v)", m, ok)
	case <-time.After(d):
	}
}

// streamServer serves the real router over db, with live and an audio bus.
type streamServer struct {
	*httptest.Server
	db    *database.DB
	live  *streamTestLive
	audio *audio.AudioBus
	admin string // an admin key
}

// streamWriteTimeout is the HTTP_WRITE_TIMEOUT of the test router: every
// stream in these tests outlives it, which checks that ResponseTimeout
// leaves the stream routes alone.
const streamWriteTimeout = 300 * time.Millisecond

func newStreamServer(t *testing.T) *streamServer {
	t.Helper()
	db := integrationDB(t)
	s := &streamServer{db: db, live: newStreamTestLive(), audio: audio.NewAudioBus()}
	opts := allFeaturesOptions(newAuthenticator(db, nil, 1e9, 1<<30, zerolog.Nop()))
	opts.DB = db
	opts.Live = s.live
	opts.AudioStreamer = &mockAudioStreamer{bus: s.audio, enabled: true}
	opts.Config.WriteTimeout = streamWriteTimeout
	s.Server = httptest.NewServer(buildRouter(opts))
	t.Cleanup(s.Close)
	k, err := db.CreateAPIKey(context.Background(), database.NewAPIKey{Name: "admin", Scopes: auth.Scopes{auth.ScopeAdmin}})
	if err != nil {
		t.Fatal(err)
	}
	s.admin = k.Plaintext
	return s
}

// key creates a key through the API.
func (s *streamServer) key(t *testing.T, name string, scopes []string, restriction any) createdKey {
	t.Helper()
	body := map[string]any{"name": name, "scopes": scopes}
	if restriction != nil {
		body["restriction"] = restriction
	}
	var k createdKey
	if code := s.api(t, "POST", "/api/v1/keys", s.admin, body, &k); code != http.StatusCreated {
		t.Fatalf("create key %s: %d", name, code)
	}
	return k
}

// api makes a JSON request to the server and returns the status.
func (s *streamServer) api(t *testing.T, method, path, key string, body, out any) int {
	t.Helper()
	var rd *strings.Reader
	if body != nil {
		js, _ := json.Marshal(body)
		rd = strings.NewReader(string(js))
	} else {
		rd = strings.NewReader("")
	}
	req, _ := http.NewRequest(method, s.URL+path, rd)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode < 300 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("%s %s: decode: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

func (s *streamServer) ticket(t *testing.T, key string, body any) string {
	t.Helper()
	var tk struct {
		Ticket string `json:"ticket"`
	}
	if code := s.api(t, "POST", "/api/v1/tickets", key, body, &tk); code != http.StatusOK || tk.Ticket == "" {
		t.Fatalf("mint ticket: %d", code)
	}
	return tk.Ticket
}

func bearerHeader(key string) http.Header {
	return http.Header{"Authorization": {"Bearer " + key}}
}

func TestIntegrationSSEStream(t *testing.T) {
	s := newStreamServer(t)
	stream := s.URL + "/api/v1/events/stream"

	t.Run("revoking the key ends the stream with invalid_key", func(t *testing.T) {
		k := s.key(t, "sse revoke", []string{"listen"}, nil)
		c := openSSE(t, stream, bearerHeader(k.Key))
		s.live.publish("call_start", 1, 100, "before")
		c.expect(t, "call_start", "before")
		time.Sleep(streamWriteTimeout + 200*time.Millisecond) // outlive HTTP_WRITE_TIMEOUT
		s.live.publish("call_end", 1, 100, "still open")
		c.expect(t, "call_end", "still open")

		if code := s.api(t, "DELETE", fmt.Sprintf("/api/v1/keys/%d", k.ID), s.admin, nil, nil); code != http.StatusNoContent {
			t.Fatalf("revoke: %d", code)
		}
		c.expectClosed(t, streamInvalidKey, 3*time.Second)
	})

	t.Run("anonymous policy off ends the stream with key_required", func(t *testing.T) {
		if code := s.api(t, "PUT", "/api/v1/anonymous-access", s.admin, map[string]any{"access": "listen", "restriction": nil}, nil); code != http.StatusOK {
			t.Fatalf("PUT anonymous-access: %d", code)
		}
		c := openSSE(t, stream, nil)
		s.live.publish("call_start", 1, 100, "anonymous")
		c.expect(t, "call_start", "anonymous")
		s.live.publish("console", 0, 0, "admins only")
		s.live.publish("rate_update", 1, 0, "rates")
		c.expect(t, "rate_update", "rates")

		if code := s.api(t, "PUT", "/api/v1/anonymous-access", s.admin, map[string]any{"access": "off", "restriction": nil}, nil); code != http.StatusOK {
			t.Fatalf("PUT anonymous-access: %d", code)
		}
		c.expectClosed(t, streamKeyRequired, 3*time.Second)
	})

	t.Run("anonymous restriction change applies to the open stream", func(t *testing.T) {
		if code := s.api(t, "PUT", "/api/v1/anonymous-access", s.admin, map[string]any{"access": "listen", "restriction": nil}, nil); code != http.StatusOK {
			t.Fatalf("PUT anonymous-access: %d", code)
		}
		c := openSSE(t, stream, nil)
		if code := s.api(t, "PUT", "/api/v1/anonymous-access", s.admin, map[string]any{
			"access": "listen", "restriction": map[string]any{"allow_all": true, "exclude_talkgroups": []string{"1:666"}},
		}, nil); code != http.StatusOK {
			t.Fatalf("PUT anonymous-access: %d", code)
		}
		waitForSwap(t, s, c, "call_start", 1, 666, 1, 100)
		s.live.publish("recorder_update", 0, 0, "dropped for restricted principals")
		s.live.publish("call_end", 1, 667, "allowed")
		c.expect(t, "call_end", "allowed")
		s.api(t, "PUT", "/api/v1/anonymous-access", s.admin, map[string]any{"access": "off", "restriction": nil}, nil)
		c.expectClosed(t, streamKeyRequired, 3*time.Second)
	})

	t.Run("a ticket stream ends at expiry with ticket_expired", func(t *testing.T) {
		k := s.key(t, "sse ticket", []string{"listen"}, nil)
		secret, err := s.db.GetOrCreateTicketSecret(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		exp := time.Now().Add(3 * time.Second).Truncate(time.Second)
		tk, err := auth.SignTicket(secret, auth.TicketPayload{KeyID: k.ID, ExpiresAt: exp})
		if err != nil {
			t.Fatal(err)
		}
		// A header naming a revoked key is ignored: the ticket wins.
		c := openSSE(t, stream+"?ticket="+url.QueryEscape(tk), bearerHeader("tre_not_a_key"))
		s.live.publish("call_start", 1, 100, "via ticket")
		c.expect(t, "call_start", "via ticket")
		s.live.publish("console", 0, 0, "tickets never get console")
		c.expectClosed(t, streamTicketExpired, 5*time.Second)
		if late := time.Since(exp); late < 0 || late > time.Second {
			t.Errorf("closed %s after the ticket's expiry", late)
		}
	})

	t.Run("a restriction change that keeps listen swaps the principal", func(t *testing.T) {
		k := s.key(t, "sse narrowed", []string{"listen"}, nil)
		c := openSSE(t, stream, bearerHeader(k.Key))
		s.live.publish("call_start", 2, 200, "unrestricted")
		c.expect(t, "call_start", "unrestricted")

		if code := s.api(t, "PATCH", fmt.Sprintf("/api/v1/keys/%d", k.ID), s.admin,
			map[string]any{"restriction": map[string]any{"systems": []int{1}}}, nil); code != http.StatusOK {
			t.Fatalf("PATCH: %d", code)
		}
		waitForSwap(t, s, c, "call_start", 2, 200, 1, 100)
		s.live.publish("unit_event", 1, 0, "unit on: no talkgroup")
		s.live.publish("trunking_message", 1, 0, "dropped")
		s.live.publish("transcription", 2, 201, "other system")
		s.live.publish("transcription", 1, 101, "allowed")
		c.expect(t, "transcription", "allowed")

		// Widening again also applies in place.
		if code := s.api(t, "PATCH", fmt.Sprintf("/api/v1/keys/%d", k.ID), s.admin,
			map[string]any{"restriction": nil}, nil); code != http.StatusOK {
			t.Fatalf("PATCH: %d", code)
		}
		deadline := time.Now().Add(3 * time.Second)
		for i := 0; ; i++ {
			s.live.publish("rate_update", 1, 0, "unrestricted again")
			s.live.publish("call_start", 1, 100, fmt.Sprintf("marker %d", i))
			first, _ := c.next(t, 3*time.Second)
			if first.Event == "rate_update" {
				c.expect(t, "call_start", fmt.Sprintf("marker %d", i))
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the widened key never got rate_update")
			}
			time.Sleep(20 * time.Millisecond)
		}

		// Losing listen (re-scoped to upload only) ends the stream.
		if code := s.api(t, "PATCH", fmt.Sprintf("/api/v1/keys/%d", k.ID), s.admin,
			map[string]any{"scopes": []string{"upload"}}, nil); code != http.StatusOK {
			t.Fatalf("PATCH: %d", code)
		}
		c.expectClosed(t, streamInsufficientScope, 3*time.Second)
	})

	t.Run("per-type access for restricted, listen and admin keys", func(t *testing.T) {
		restricted := s.key(t, "sse restricted", []string{"listen"}, map[string]any{"talkgroups": []string{"1:100"}})
		listen := s.key(t, "sse listen", []string{"listen"}, nil)
		rc := openSSE(t, stream, bearerHeader(restricted.Key))
		lc := openSSE(t, stream, bearerHeader(listen.Key))
		ac := openSSE(t, stream+"?types=console,call_end", bearerHeader(s.admin))
		s.live.publish("console", 0, 0, "log line")
		s.live.publish("recorder_update", 0, 0, "recorder")
		s.live.publish("unit_event", 1, 0, "unit off")
		s.live.publish("call_start", 1, 101, "other talkgroup")
		s.live.publish("call_start", 0, 100, "no system")
		s.live.publish("call_end", 1, 100, "allowed")

		rc.expect(t, "call_end", "allowed")
		lc.expect(t, "recorder_update", "recorder")
		lc.expect(t, "unit_event", "unit off")
		lc.expect(t, "call_start", "other talkgroup")
		lc.expect(t, "call_start", "no system")
		lc.expect(t, "call_end", "allowed")
		ac.expect(t, "console", "log line")
		ac.expect(t, "call_end", "allowed")
	})

	t.Run("last_event_id replay with a ticket", func(t *testing.T) {
		k := s.key(t, "sse replay", []string{"listen"}, map[string]any{"systems": []int{1}})
		e1 := s.live.publish("call_start", 1, 100, "e1")
		s.live.publish("call_start", 1, 100, "e2")
		s.live.publish("call_start", 2, 100, "e3: outside the key's restriction")
		e4 := s.live.publish("call_end", 1, 100, "e4")

		tk := s.ticket(t, k.Key, nil)
		c := openSSE(t, stream+"?ticket="+url.QueryEscape(tk)+"&last_event_id="+url.QueryEscape(e1.ID), nil)
		m := c.expect(t, "call_start", "e2")
		if m.ID == "" {
			t.Error("replayed event without an id")
		}
		c.expect(t, "call_end", "e4")
		s.live.publish("call_end", 1, 100, "e5 live")
		c.expect(t, "call_end", "e5 live")

		// The Last-Event-ID header wins over the query parameter.
		tk2 := s.ticket(t, k.Key, map[string]any{"restriction": map[string]any{"talkgroups": []string{"1:100"}}})
		c2 := openSSE(t, stream+"?ticket="+url.QueryEscape(tk2)+"&last_event_id="+url.QueryEscape(e1.ID),
			http.Header{"Last-Event-ID": {e4.ID}})
		c2.expect(t, "call_end", "e5 live")
		c2.expectNothing(t, 100*time.Millisecond)
	})

	t.Run("a CLI change reaches the stream through the periodic re-check", func(t *testing.T) {
		k := s.key(t, "sse cli", []string{"listen"}, nil)
		streamRecheckOverride.Store(int64(300 * time.Millisecond))
		t.Cleanup(func() { streamRecheckOverride.Store(0) })
		c := openSSE(t, stream, bearerHeader(k.Key))
		// The CLI changes the database from another process: no generation
		// bump reaches this one.
		gen := auth.Generation()
		if _, err := s.db.Pool.Exec(context.Background(), "UPDATE api_keys SET revoked_at = now() WHERE id = $1", k.ID); err != nil {
			t.Fatal(err)
		}
		c.expectClosed(t, streamInvalidKey, 3*time.Second)
		if auth.Generation() != gen {
			t.Error("the generation moved; the periodic re-check was not what closed the stream")
		}
	})
}

// waitForSwap publishes pairs of events, one the new principal forbids
// (fsys:ftg) and one it allows (asys:atg), until an allowed one arrives
// without its forbidden twin: the stream's principal has been swapped. It
// then checks that forbidden events stay out.
func waitForSwap(t *testing.T, s *streamServer, c *sseClient, typ string, fsys, ftg, asys, atg int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for i := 0; ; i++ {
		s.live.publish(typ, fsys, ftg, fmt.Sprintf("forbidden %d", i))
		s.live.publish(typ, asys, atg, fmt.Sprintf("allowed %d", i))
		first, _ := c.next(t, 3*time.Second)
		if first.Data == fmt.Sprintf("%q", fmt.Sprintf("allowed %d", i)) {
			break
		}
		c.next(t, 3*time.Second) // the allowed one
		if time.Now().After(deadline) {
			t.Fatal("the stream's principal was never swapped")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for i := 0; i < 5; i++ {
		s.live.publish(typ, fsys, ftg, "forbidden after the swap")
	}
	s.live.publish(typ, asys, atg, "allowed after the swap")
	c.expect(t, typ, "allowed after the swap")
}

// wsFrame is a binary audio frame as a client sees it.
type wsFrame struct {
	sys, tg int
	data    string
}

// wsClient reads a live-audio WebSocket in the background.
type wsClient struct {
	conn   *websocket.Conn
	frames chan wsFrame
	done   chan struct{} // closed when reading fails; err says why
	err    error
}

// wsDial opens the live-audio WebSocket.
func wsDial(t *testing.T, s *streamServer, query string, header http.Header) *wsClient {
	t.Helper()
	u := "ws" + strings.TrimPrefix(s.URL, "http") + "/api/v1/audio/live" + query
	conn, resp, err := websocket.DefaultDialer.Dial(u, header)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial %s: %v (status %d)", u, err, status)
	}
	t.Cleanup(func() { conn.Close() })
	c := &wsClient{conn: conn, frames: make(chan wsFrame, 1024), done: make(chan struct{})}
	go func() {
		for {
			typ, data, err := conn.ReadMessage()
			if err != nil {
				c.err = err
				close(c.done)
				return
			}
			if typ == websocket.BinaryMessage && len(data) >= 14 {
				c.frames <- wsFrame{
					sys:  int(binary.BigEndian.Uint16(data[0:2])),
					tg:   int(binary.BigEndian.Uint32(data[2:6])),
					data: string(data[14:]),
				}
			}
		}
	}()
	return c
}

func (c *wsClient) subscribe(t *testing.T, systems, tgids []int) {
	t.Helper()
	msg, _ := json.Marshal(subscribeMsg{Type: "subscribe", Systems: systems, TGIDs: tgids})
	if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
		t.Fatal(err)
	}
}

var wsMarker atomic.Int64

// wsSync publishes a marker frame on sys:tg, preceded by a frame on each
// forbidden talkgroup, until the marker arrives. It fails if a forbidden
// frame arrives; frames left over from earlier markers are skipped. It also
// establishes that the last subscribe message was processed.
func wsSync(t *testing.T, s *streamServer, c *wsClient, sys, tg int, forbidden ...[2]int) {
	t.Helper()
	if !wsWaitFor(t, s, c, sys, tg, forbidden...) {
		t.Fatalf("no frame on %d:%d within 3s", sys, tg)
	}
}

// wsWaitFor is wsSync that reports a timeout instead of failing.
func wsWaitFor(t *testing.T, s *streamServer, c *wsClient, sys, tg int, forbidden ...[2]int) bool {
	t.Helper()
	token := fmt.Sprintf("marker %d", wsMarker.Add(1))
	deadline := time.After(3 * time.Second)
	for {
		for _, f := range forbidden {
			s.audio.Publish(audio.AudioFrame{SystemID: f[0], TGID: f[1], Data: []byte("forbidden")})
		}
		s.audio.Publish(audio.AudioFrame{SystemID: sys, TGID: tg, Data: []byte(token)})
		wait := time.After(20 * time.Millisecond)
	read:
		for {
			select {
			case f := <-c.frames:
				switch {
				case f.data == "forbidden":
					t.Fatalf("got a frame on forbidden talkgroup %d:%d", f.sys, f.tg)
				case f.data == token:
					if f.sys != sys || f.tg != tg {
						t.Fatalf("marker arrived as %d:%d, want %d:%d", f.sys, f.tg, sys, tg)
					}
					return true
				}
			case <-c.done:
				t.Fatalf("socket closed while waiting for a frame: %v", c.err)
			case <-wait:
				break read
			case <-deadline:
				return false
			}
		}
	}
}

// wsExpectClose waits for the socket to close and checks the close code and
// reason.
func wsExpectClose(t *testing.T, c *wsClient, code int, reason string) {
	t.Helper()
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		t.Fatalf("socket still open, want close %d %q", code, reason)
	}
	var ce *websocket.CloseError
	if !errors.As(c.err, &ce) || ce.Code != code || ce.Text != reason {
		t.Fatalf("read error %v, want close %d %q", c.err, code, reason)
	}
}

func TestIntegrationAudioWebSocket(t *testing.T) {
	s := newStreamServer(t)

	t.Run("revoking the key closes the socket with 4401", func(t *testing.T) {
		k := s.key(t, "ws revoke", []string{"listen"}, nil)
		c := wsDial(t, s, "", bearerHeader(k.Key)) // the upgrade works through the pipeline
		c.subscribe(t, nil, []int{100})
		wsSync(t, s, c, 1, 100, [2]int{1, 101})
		time.Sleep(streamWriteTimeout + 200*time.Millisecond) // outlive HTTP_WRITE_TIMEOUT
		wsSync(t, s, c, 1, 100)

		if code := s.api(t, "DELETE", fmt.Sprintf("/api/v1/keys/%d", k.ID), s.admin, nil, nil); code != http.StatusNoContent {
			t.Fatalf("revoke: %d", code)
		}
		wsExpectClose(t, c, wsCloseUnauthorized, streamInvalidKey)
	})

	t.Run("losing listen closes the socket with 4403", func(t *testing.T) {
		k := s.key(t, "ws rescope", []string{"listen"}, nil)
		c := wsDial(t, s, "", bearerHeader(k.Key))
		c.subscribe(t, nil, nil)
		wsSync(t, s, c, 1, 100)
		if code := s.api(t, "PATCH", fmt.Sprintf("/api/v1/keys/%d", k.ID), s.admin,
			map[string]any{"scopes": []string{"upload"}}, nil); code != http.StatusOK {
			t.Fatalf("PATCH: %d", code)
		}
		wsExpectClose(t, c, wsCloseForbidden, streamInsufficientScope)
	})

	t.Run("anonymous policy off closes the socket with 4401 key_required", func(t *testing.T) {
		if code := s.api(t, "PUT", "/api/v1/anonymous-access", s.admin, map[string]any{"access": "listen", "restriction": nil}, nil); code != http.StatusOK {
			t.Fatalf("PUT anonymous-access: %d", code)
		}
		c := wsDial(t, s, "", nil)
		c.subscribe(t, nil, nil)
		wsSync(t, s, c, 3, 300)
		if code := s.api(t, "PUT", "/api/v1/anonymous-access", s.admin, map[string]any{"access": "off", "restriction": nil}, nil); code != http.StatusOK {
			t.Fatalf("PUT anonymous-access: %d", code)
		}
		wsExpectClose(t, c, wsCloseUnauthorized, streamKeyRequired)
	})

	t.Run("a restricted key hears only allowed talkgroups", func(t *testing.T) {
		k := s.key(t, "ws restricted", []string{"listen"}, map[string]any{"systems": []int{1}, "exclude_talkgroups": []string{"1:666"}})
		c := wsDial(t, s, "", bearerHeader(k.Key))
		// Asking for everything, and then for forbidden talkgroups by name,
		// doesn't widen the restriction.
		c.subscribe(t, nil, nil)
		wsSync(t, s, c, 1, 100, [2]int{2, 100}, [2]int{1, 666}, [2]int{1, 0}, [2]int{0, 100})
		c.subscribe(t, []int{1, 2}, []int{100, 666, 200})
		wsSync(t, s, c, 1, 200, [2]int{2, 100}, [2]int{1, 666}, [2]int{2, 200})

		// A restriction change applies to the open socket.
		if code := s.api(t, "PATCH", fmt.Sprintf("/api/v1/keys/%d", k.ID), s.admin,
			map[string]any{"restriction": map[string]any{"talkgroups": []string{"2:200"}}}, nil); code != http.StatusOK {
			t.Fatalf("PATCH: %d", code)
		}
		if !wsWaitFor(t, s, c, 2, 200) {
			t.Fatal("the socket's principal was never swapped")
		}
		wsSync(t, s, c, 2, 200, [2]int{1, 100}, [2]int{1, 200})
	})

	t.Run("a ticket socket closes at expiry with 4401 ticket_expired", func(t *testing.T) {
		k := s.key(t, "ws ticket", []string{"listen"}, nil)
		secret, err := s.db.GetOrCreateTicketSecret(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		exp := time.Now().Add(2 * time.Second).Truncate(time.Second)
		tk, err := auth.SignTicket(secret, auth.TicketPayload{KeyID: k.ID, ExpiresAt: exp,
			Narrowing: &auth.Restriction{Talkgroups: []auth.TG{{SystemID: 1, Tgid: 100}}}})
		if err != nil {
			t.Fatal(err)
		}
		c := wsDial(t, s, "?ticket="+url.QueryEscape(tk), nil)
		c.subscribe(t, nil, nil)
		wsSync(t, s, c, 1, 100, [2]int{1, 101}) // the narrowing applies
		wsExpectClose(t, c, wsCloseUnauthorized, streamTicketExpired)
	})

	t.Run("an invalid ticket is refused before the upgrade", func(t *testing.T) {
		u := "ws" + strings.TrimPrefix(s.URL, "http") + "/api/v1/audio/live?ticket=trt_bogus.bogus"
		_, resp, err := websocket.DefaultDialer.Dial(u, nil)
		if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("dial with a bad ticket: err=%v resp=%v, want 401", err, resp)
		}
	})
}
