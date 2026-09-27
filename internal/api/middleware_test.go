package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
)

// okHandler is a trivial handler that writes 200 OK.
var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
})

func TestRequestID(t *testing.T) {
	t.Run("generates_id_when_missing", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		RequestID(okHandler).ServeHTTP(rec, req)
		id := rec.Header().Get("X-Request-ID")
		if len(id) != 16 {
			t.Errorf("expected 16-char hex ID, got %q (len %d)", id, len(id))
		}
	})

	t.Run("preserves_valid_id", func(t *testing.T) {
		for _, id := range []string{"my-custom-id", "a.b_C-9", strings.Repeat("x", 64)} {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/", nil)
			req.Header.Set("X-Request-ID", id)
			RequestID(okHandler).ServeHTTP(rec, req)
			if got := rec.Header().Get("X-Request-ID"); got != id {
				t.Errorf("expected preserved ID %q, got %q", id, got)
			}
		}
	})

	t.Run("replaces_invalid_id", func(t *testing.T) {
		for _, id := range []string{strings.Repeat("x", 65), "has space", "semi;colon", "new\nline", "<script>", "ümlaut"} {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/", nil)
			req.Header.Set("X-Request-ID", id)
			var seen string
			RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = r.Header.Get("X-Request-ID")
			})).ServeHTTP(rec, req)
			got := rec.Header().Get("X-Request-ID")
			if got == id || len(got) != 16 {
				t.Errorf("%q: got %q, want a generated 16-char ID", id, got)
			}
			if seen != got {
				t.Errorf("%q: handler saw %q, response says %q", id, seen, got)
			}
		}
	})
}

func TestCORS(t *testing.T) {
	t.Run("every_response_allows_all_origins_without_credentials", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/v1/calls", nil)
		req.Header.Set("Origin", "https://evil.example")
		CORS(okHandler).ServeHTTP(rec, req)
		h := rec.Header()
		if h.Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("Allow-Origin = %q, want *", h.Get("Access-Control-Allow-Origin"))
		}
		if h.Get("Access-Control-Allow-Credentials") != "" {
			t.Error("Access-Control-Allow-Credentials must never be sent")
		}
		if h.Get("Access-Control-Expose-Headers") != "X-Request-ID, Retry-After, WWW-Authenticate" {
			t.Errorf("Expose-Headers = %q", h.Get("Access-Control-Expose-Headers"))
		}
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}
	})

	t.Run("options_answered_before_anything_else", func(t *testing.T) {
		called := false
		inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("OPTIONS", "/api/v1/keys", nil)
		req.Header.Set("Origin", "https://any.example")
		req.Header.Set("Access-Control-Request-Method", "DELETE")
		CORS(inner).ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Errorf("expected 204, got %d", rec.Code)
		}
		if called {
			t.Error("inner handler should not be called on OPTIONS")
		}
		h := rec.Header()
		for name, want := range map[string]string{
			"Access-Control-Allow-Origin":  "*",
			"Access-Control-Allow-Methods": "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS",
			"Access-Control-Allow-Headers": "Authorization, Content-Type, Last-Event-ID, X-Actor, X-Request-ID",
			"Access-Control-Max-Age":       "600",
		} {
			if got := h.Get(name); got != want {
				t.Errorf("%s = %q, want %q", name, got, want)
			}
		}
		if h.Get("Access-Control-Allow-Credentials") != "" {
			t.Error("Access-Control-Allow-Credentials must never be sent")
		}
	})
}

func TestAPIHeaders(t *testing.T) {
	for _, tc := range []struct {
		path string
		api  bool
	}{
		{"/api/v1/calls", true},
		{"/api/v1", true},
		{"/api/v10/x", false},
		{"/index.html", false},
		{"/metrics", false},
	} {
		rec := httptest.NewRecorder()
		APIHeaders(okHandler).ServeHTTP(rec, httptest.NewRequest("GET", tc.path, nil))
		cc, vary := rec.Header().Get("Cache-Control"), rec.Header().Get("Vary")
		if tc.api && (cc != "no-store" || vary != "Authorization") {
			t.Errorf("%s: Cache-Control %q, Vary %q; want no-store, Authorization", tc.path, cc, vary)
		}
		if !tc.api && (cc != "" || vary != "") {
			t.Errorf("%s: Cache-Control %q, Vary %q; want none", tc.path, cc, vary)
		}
	}
}

func TestRecoverer(t *testing.T) {
	t.Run("normal_request_passes_through", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		Recoverer(okHandler).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}
	})

	t.Run("panic_produces_500_json", func(t *testing.T) {
		panicker := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			panic("test panic")
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		Recoverer(panicker).ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("expected 500, got %d", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("expected application/json, got %q", ct)
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("response is not valid JSON: %v", err)
		}
		if body["error"] != "internal server error" {
			t.Errorf("expected error message, got %v", body)
		}
	})
}

func TestMaxBodySize(t *testing.T) {
	var readErr error
	h := MaxBodySize(8)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/", strings.NewReader("0123456789")))
	if readErr == nil {
		t.Error("reading more than the limit should fail")
	}
}

func TestResponseTimeout(t *testing.T) {
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(time.Second):
		case <-r.Context().Done():
		}
	})
	rec := httptest.NewRecorder()
	ResponseTimeout(10*time.Millisecond)(slow).ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/calls", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "request_timeout") {
		t.Errorf("slow handler: %d %s, want 503 request_timeout", rec.Code, rec.Body.String())
	}

	// Streaming endpoints are not wrapped.
	quick := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	for _, path := range []string{"/api/v1/events/stream", "/api/v1/calls/1/audio", "/api/v1/audio/live"} {
		rec := httptest.NewRecorder()
		ResponseTimeout(10*time.Millisecond)(quick).ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: got %d, want 200 (not wrapped)", path, rec.Code)
		}
	}
}

// lockedBuffer is a bytes.Buffer safe for a logger written from server
// goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Every request gets an access line, including refusals on paths that end in
// /audio/live and WebSocket upgrades, which still work through the logger
// (r1-09).
func TestLoggerLogsEveryRequest(t *testing.T) {
	var logs lockedBuffer
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/audio/live", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("refuse") != "" {
			WriteErrorWithCode(w, http.StatusUnauthorized, ErrInvalidTicket, "invalid ticket")
			return
		}
		c, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		c.WriteMessage(websocket.TextMessage, []byte("hi"))
		c.Close()
	})
	mux.HandleFunc("/foo/audio/live", func(w http.ResponseWriter, r *http.Request) {
		WriteErrorWithCode(w, http.StatusUnauthorized, ErrInvalidKey, "invalid key")
	})
	srv := httptest.NewServer(RequestID(Logger(zerolog.New(&logs))(mux)))
	defer srv.Close()

	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/api/v1/audio/live", nil)
	if err != nil {
		t.Fatalf("upgrade through the logger failed: %v", err)
	}
	if _, msg, err := c.ReadMessage(); err != nil || string(msg) != "hi" {
		t.Errorf("read = %q, %v", msg, err)
	}
	c.Close()
	for _, path := range []string{"/foo/audio/live", "/api/v1/audio/live?refuse=1", "/api/v1/talkgroups/1%2Faudio%2Flive"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	deadline := time.Now().Add(2 * time.Second)
	want := []string{
		`"path":"/api/v1/audio/live","status":101`,
		`"path":"/foo/audio/live","status":401`,
		`"path":"/api/v1/audio/live","status":401`,
		`"path":"/api/v1/talkgroups/1/audio/live","status":404`,
	}
	for {
		out := logs.String()
		missing := ""
		for _, w := range want {
			if !strings.Contains(out, w) {
				missing = w
				break
			}
		}
		if missing == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no access line with %s in:\n%s", missing, out)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
