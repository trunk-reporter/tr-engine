package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/auth"
	"github.com/snarg/tr-engine/internal/database"
	"github.com/snarg/tr-engine/internal/mqttclient"
)

// logLines returns the JSON lines logged to b.
func logLines(t *testing.T, b *lockedBuffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("log line %q: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}

// panickyLive panics in UnitAffiliations (GET /api/v1/unit-affiliations).
type panickyLive struct{ mockLiveData }

func (*panickyLive) UnitAffiliations() []UnitAffiliationData { panic("affiliations exploded") }

// A handler panic is logged, with the stack of the handler that panicked
// (through ResponseTimeout's http.TimeoutHandler), and the access line
// records the 500 Recoverer sends (r3-06): Recoverer runs inside Logger.
func TestRouterLogsHandlerPanic(t *testing.T) {
	store := newStubAuthStore()
	store.add("listen-key", database.APIKey{Scopes: auth.Scopes{auth.ScopeListen}})
	var logs lockedBuffer
	opts := allFeaturesOptions(newTestAuthenticator(store))
	opts.Log = zerolog.New(&logs)
	opts.Live = &panickyLive{}
	r := buildRouter(opts)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/unit-affiliations", nil)
	req.Header.Set("Authorization", "Bearer listen-key")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), `"internal_error"`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}

	var recovered, access bool
	for _, l := range logLines(t, &logs) {
		switch l["message"] {
		case "recovered from panic":
			recovered = true
			if l["panic"] != "affiliations exploded" || l["path"] != "/api/v1/unit-affiliations" {
				t.Errorf("panic line = %v", l)
			}
			if stack, _ := l["stack"].(string); !strings.Contains(stack, "panickyLive") {
				t.Errorf("the stack does not show the handler that panicked:\n%s", stack)
			}
		case "request":
			access = true
			if l["status"] != float64(http.StatusInternalServerError) {
				t.Errorf("access line = %v, want status 500", l)
			}
		}
	}
	if !recovered || !access {
		t.Errorf("logged recovered=%v access=%v:\n%v", recovered, access, logLines(t, &logs))
	}
}

// Without MQTT_BROKER_URL main passes a nil *mqttclient.Client; POST
// /debug-report then reports MQTT as not connected instead of panicking in
// IsConnected (r3-06).
func TestDebugReportWithoutMQTT(t *testing.T) {
	var forwarded []byte
	var mu sync.Mutex
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		forwarded = b
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	store := newStubAuthStore()
	store.add("admin-key", database.APIKey{Scopes: auth.Scopes{auth.ScopeAdmin}})
	opts := allFeaturesOptions(newTestAuthenticator(store))
	opts.Config.DebugReportDisable = false
	opts.Config.DebugReportURL = receiver.URL
	opts.MQTT = (*mqttclient.Client)(nil)
	r := buildRouter(opts)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/debug-report", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer admin-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	var report map[string]any
	if err := json.Unmarshal(forwarded, &report); err != nil {
		t.Fatalf("forwarded %q: %v", forwarded, err)
	}
	if !strings.Contains(string(forwarded), `"mqtt_connected":false`) {
		t.Errorf("forwarded report = %s", forwarded)
	}

	var nilClient *mqttclient.Client
	if nilClient.IsConnected() {
		t.Error("a nil client reports connected")
	}
}
