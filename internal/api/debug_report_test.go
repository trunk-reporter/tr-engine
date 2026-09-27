package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/snarg/tr-engine/internal/config"
)

func TestSanitizeConfig(t *testing.T) {
	cfg := &config.Config{
		DatabaseURL:   "postgres://user:secret@db.example.com:5432/trengine",
		MQTTBrokerURL: "mqtt://mqttuser:mqttpass@broker.example.com:1883",
		MQTTTopics:    "#",
		MQTTClientID:  "tr-engine",
		MQTTUsername:   "mqttuser",
		MQTTPassword:   "mqttpass",

		AudioDir: "./audio",

		HTTPAddr:     ":8080",
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,

		LegacyAuth: config.LegacyAuthEnv{
			AuthToken:     "my-secret-auth-token",
			WriteToken:    "my-secret-write-token",
			AdminPassword: "my-admin-password",
		},
		LogLevel: "info",

		StreamListen:      ":9123",
		StreamSampleRate:  8000,
		StreamIdleTimeout: 30 * time.Second,

		STTProvider:   "whisper",
		WhisperURL:    "http://admin:whisperpass@whisper.local:8080/v1",
		WhisperAPIKey: "sk-whisper-secret",

		ElevenLabsAPIKey: "",

		DeepInfraAPIKey: "di-secret-key",

		LLMUrl: "http://llmuser:llmpass@llm.local:11434",

		RetentionRawMessages: 168 * time.Hour,
		RetentionAuditLog:    8760 * time.Hour,

		UnitTagSuggestions:         true,
		UnitTagSuggestionsMinCalls: 3,
		UnitTagSuggestionsMinShare: 0.2,
		UnitTagSuggestionsInterval: time.Minute,

		S3: config.S3Config{
			Bucket:    "my-audio-bucket",
			Endpoint:  "https://s3user:s3pass@s3.example.com",
			Region:    "us-east-1",
			AccessKey: "AKIAIOSFODNN7EXAMPLE",
			SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			Prefix:    "audio/",
		},
	}

	result := sanitizeConfig(cfg)

	// The removed auth variables are not part of the report at all.
	for _, field := range []string{"AuthToken", "WriteToken", "AuthEnabled", "CORSOrigins", "LegacyAuth", "AdminPassword"} {
		if _, ok := result[field]; ok {
			t.Errorf("%s should not be in the report", field)
		}
	}
	if js, _ := json.Marshal(result); strings.Contains(string(js), "my-secret") || strings.Contains(string(js), "my-admin-password") {
		t.Errorf("legacy auth secrets leaked into the report: %s", js)
	}

	// Secret fields should be redacted
	secretFields := map[string]string{
		"WhisperAPIKey": "***",
		"MQTTUsername":  "***",
		"MQTTPassword":  "***",
		"DeepInfraAPIKey": "***",
	}
	for field, expected := range secretFields {
		if got := result[field]; got != expected {
			t.Errorf("%s: got %q, want %q", field, got, expected)
		}
	}

	// Empty secrets should be empty string, not "***"
	if got := result["ElevenLabsAPIKey"]; got != "" {
		t.Errorf("ElevenLabsAPIKey (empty): got %q, want %q", got, "")
	}

	// DatabaseURL should have credentials stripped but host preserved
	dbURL := result["DatabaseURL"].(string)
	if dbURL != "postgres://db.example.com:5432/trengine" {
		t.Errorf("DatabaseURL: got %q, want credentials stripped", dbURL)
	}
	// Should not contain the password
	for _, secret := range []string{"user:", "secret@"} {
		if contains(dbURL, secret) {
			t.Errorf("DatabaseURL still contains %q", secret)
		}
	}

	// MQTTBrokerURL should have credentials stripped
	mqttURL := result["MQTTBrokerURL"].(string)
	if contains(mqttURL, "mqttuser") || contains(mqttURL, "mqttpass") {
		t.Errorf("MQTTBrokerURL still contains credentials: %q", mqttURL)
	}

	// WhisperURL should have credentials stripped
	whisperURL := result["WhisperURL"].(string)
	if contains(whisperURL, "admin") || contains(whisperURL, "whisperpass") {
		t.Errorf("WhisperURL still contains credentials: %q", whisperURL)
	}

	// LLMUrl should have credentials stripped
	llmURL := result["LLMUrl"].(string)
	if contains(llmURL, "llmuser") || contains(llmURL, "llmpass") {
		t.Errorf("LLMUrl still contains credentials: %q", llmURL)
	}

	// Non-sensitive fields should be preserved as-is
	if got := result["HTTPAddr"]; got != ":8080" {
		t.Errorf("HTTPAddr: got %q, want %q", got, ":8080")
	}
	if got := result["StreamListen"]; got != ":9123" {
		t.Errorf("StreamListen: got %q, want %q", got, ":9123")
	}
	if got := result["LogLevel"]; got != "info" {
		t.Errorf("LogLevel: got %q, want %q", got, "info")
	}
	if got := result["StreamSampleRate"]; got != 8000 {
		t.Errorf("StreamSampleRate: got %v, want %v", got, 8000)
	}

	// S3 nested map
	s3, ok := result["S3"].(map[string]any)
	if !ok {
		t.Fatal("S3 field is not a map[string]any")
	}
	if got := s3["AccessKey"]; got != "***" {
		t.Errorf("S3.AccessKey: got %q, want %q", got, "***")
	}
	if got := s3["SecretKey"]; got != "***" {
		t.Errorf("S3.SecretKey: got %q, want %q", got, "***")
	}
	if got := s3["Bucket"]; got != "my-audio-bucket" {
		t.Errorf("S3.Bucket: got %q, want %q", got, "my-audio-bucket")
	}
	if got := s3["Region"]; got != "us-east-1" {
		t.Errorf("S3.Region: got %q, want %q", got, "us-east-1")
	}
	if got := s3["Prefix"]; got != "audio/" {
		t.Errorf("S3.Prefix: got %q, want %q", got, "audio/")
	}
	// S3 endpoint should have credentials stripped
	s3Endpoint := s3["Endpoint"].(string)
	if contains(s3Endpoint, "s3user") || contains(s3Endpoint, "s3pass") {
		t.Errorf("S3.Endpoint still contains credentials: %q", s3Endpoint)
	}

	// Duration fields should be formatted as strings
	if got := result["RetentionRawMessages"]; got != "168h0m0s" {
		t.Errorf("RetentionRawMessages: got %q, want %q", got, "168h0m0s")
	}
	if got := result["RetentionAuditLog"]; got != "8760h0m0s" {
		t.Errorf("RetentionAuditLog: got %q, want %q", got, "8760h0m0s")
	}
	if got := result["ReadTimeout"]; got != "5s" {
		t.Errorf("ReadTimeout: got %q, want %q", got, "5s")
	}

	// Unit tag suggestion scanner settings
	if result["UnitTagSuggestions"] != true || result["UnitTagSuggestionsMinCalls"] != 3 ||
		result["UnitTagSuggestionsMinShare"] != 0.2 || result["UnitTagSuggestionsInterval"] != "1m0s" {
		t.Errorf("unit tag suggestion settings: got %v / %v / %v / %v", result["UnitTagSuggestions"],
			result["UnitTagSuggestionsMinCalls"], result["UnitTagSuggestionsMinShare"], result["UnitTagSuggestionsInterval"])
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestDebugReportSubmitForwards(t *testing.T) {
	// Mock debug-receiver that captures the forwarded request body
	var capturedBody []byte
	mockReceiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		capturedBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("mock receiver failed to read body: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer mockReceiver.Close()

	trDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(trDir, "config.json"), []byte(realisticTRConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		DatabaseURL:    "postgres://user:pass@localhost:5432/test",
		WhisperAPIKey:  "sk-whisper-secret",
		LegacyAuth:     config.LegacyAuthEnv{AuthToken: "secret-token"},
		DebugReportURL: mockReceiver.URL,
		TRDir:          trDir,
	}

	handler := NewDebugReportHandler(DebugReportOptions{
		DB:            nil,
		Config:        cfg,
		Live:          nil,
		AudioStreamer: nil,
		MQTT:          nil,
		Log:           zerolog.Nop(),
		Version:       "test-v0.0.1",
		StartTime:     time.Now(),
	})

	reqBody := `{"problem":"audio not working","userAgent":"TestBrowser/1.0","page":"https://engine.example/scanner.html?ticket=trt_abc.def","apiKey":"tre_from_the_page"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/debug-report", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.Submit(rec, req)

	// Assert response is 200 with {"ok":true}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var respBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &respBody); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if respBody["ok"] != true {
		t.Errorf("expected ok=true, got %v", respBody["ok"])
	}

	// Unmarshal the captured forwarded body
	var report map[string]any
	if err := json.Unmarshal(capturedBody, &report); err != nil {
		t.Fatalf("failed to unmarshal captured report: %v", err)
	}

	// Assert report_type
	if rt, ok := report["report_type"].(string); !ok || rt != "debug_report" {
		t.Errorf("expected report_type=%q, got %v", "debug_report", report["report_type"])
	}

	// Assert client.problem
	client, ok := report["client"].(map[string]any)
	if !ok {
		t.Fatal("report missing 'client' map")
	}
	if prob, ok := client["problem"].(string); !ok || prob != "audio not working" {
		t.Errorf("expected client.problem=%q, got %v", "audio not working", client["problem"])
	}

	// Assert server-side data
	server, ok := report["server"].(map[string]any)
	if !ok {
		t.Fatal("report missing 'server' map")
	}

	// Assert the config's secrets are redacted and the removed auth
	// variables are absent.
	cfgMap, ok := server["config"].(map[string]any)
	if !ok {
		t.Fatal("report missing 'server.config' map")
	}
	if got := cfgMap["WhisperAPIKey"]; got != "***" {
		t.Errorf("expected server.config.WhisperAPIKey=%q, got %v", "***", got)
	}
	if _, ok := cfgMap["AuthToken"]; ok {
		t.Error("server.config.AuthToken should not exist")
	}

	// Nothing secret from the engine, the TR config or the page survives,
	// at any depth.
	for _, secret := range []string{"secret-token", "sk-whisper-secret", "rdio-secret-key", "openmhz-secret",
		"bcfy-secret", "mqtt-pass", "mqttuser:", "hook-token", "tre_from_the_page", "trt_abc.def", "s3cr3t"} {
		if strings.Contains(string(capturedBody), secret) {
			t.Errorf("forwarded report contains %q", secret)
		}
	}
	// Ordinary TR settings are kept.
	trCfg, ok := server["tr_config"].(map[string]any)
	if !ok {
		t.Fatalf("report missing server.tr_config: %v", server["tr_config"])
	}
	if trCfg["captureDir"] != "/app/media" || trCfg["uploadServer"] != "https://api.openmhz.com" {
		t.Errorf("tr_config lost ordinary settings: %v", trCfg)
	}
	if client["page"] != "https://engine.example/scanner.html" {
		t.Errorf("client.page = %v, want the URL without its query", client["page"])
	}

	// Assert server.config.DatabaseURL does NOT contain "pass"
	dbURL, ok := cfgMap["DatabaseURL"].(string)
	if !ok {
		t.Fatal("server.config.DatabaseURL is not a string")
	}
	if strings.Contains(dbURL, "pass") {
		t.Errorf("server.config.DatabaseURL still contains password: %q", dbURL)
	}

	// Assert server.environment exists
	if _, ok := server["environment"]; !ok {
		t.Error("report missing 'server.environment'")
	}
}

func TestDebugReportDisabledReturns503(t *testing.T) {
	cfg := &config.Config{
		DebugReportDisable: true,
	}

	handler := NewDebugReportHandler(DebugReportOptions{
		Config:    cfg,
		Log:       zerolog.Nop(),
		Version:   "test-v0.0.1",
		StartTime: time.Now(),
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/debug-report", strings.NewReader(`{"problem":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.Submit(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d: %s", rec.Code, rec.Body.String())
	}
	// The usual error shape, with a code.
	var resp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Code != ErrServiceUnavail {
		t.Errorf("body = %s, want code %q", rec.Body.String(), ErrServiceUnavail)
	}
}

func TestDebugReportBadJSON(t *testing.T) {
	mockReceiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("mock receiver should not have been called for bad JSON")
		w.WriteHeader(http.StatusOK)
	}))
	defer mockReceiver.Close()

	cfg := &config.Config{
		DebugReportURL: mockReceiver.URL,
	}

	handler := NewDebugReportHandler(DebugReportOptions{
		Config:    cfg,
		Log:       zerolog.Nop(),
		Version:   "test-v0.0.1",
		StartTime: time.Now(),
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/debug-report", strings.NewReader("not json at all{{"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.Submit(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// realisticTRConfig is a trunk-recorder config.json with the secrets a real
// one carries: upload plugin API keys (rdio-scanner, OpenMHz), Broadcastify,
// and an MQTT plugin with credentials in the broker URL and in a field.
const realisticTRConfig = `{
  "ver": 2,
  "captureDir": "/app/media",
  "logLevel": "info",
  "uploadServer": "https://api.openmhz.com",
  "broadcastifyCallsServer": "https://api.broadcastify.com/call-upload?apiKey=bcfy-secret",
  "statusServer": "ws://admin:s3cr3t@status.example:3005/server",
  "sources": [{"center": 853000000, "rate": 2400000, "driver": "osmosdr", "device": "rtl=0"}],
  "systems": [{
    "shortName": "butco",
    "type": "p25",
    "control_channels": [853262500],
    "talkgroupsFile": "butco.csv",
    "unitTagsFile": "butco-units.csv",
    "apiKey": "openmhz-secret",
    "broadcastifyApiKey": "bcfy-secret",
    "broadcastifySystemId": 1234,
    "uploadScript": "upload.sh"
  }],
  "plugins": [
    {
      "name": "rdioscanner_uploader",
      "library": "librdioscanner_uploader.so",
      "server": "https://rdio.example.com",
      "systems": [{"shortName": "butco", "apiKey": "rdio-secret-key", "systemId": 1}]
    },
    {
      "name": "MQTT Status",
      "library": "libmqtt_status_plugin.so",
      "broker": "tcp://mqttuser:mqtt-pass@mqtt.example:1883",
      "topic": "tr/feeds",
      "username": "trunk",
      "password": "mqtt-pass",
      "Credentials": {"user": "x", "token": "hook-token"}
    }
  ]
}`

func TestRedactReport(t *testing.T) {
	var in any
	if err := json.Unmarshal([]byte(realisticTRConfig), &in); err != nil {
		t.Fatal(err)
	}
	out := redactReport(in).(map[string]any)
	js, _ := json.Marshal(out)
	for _, secret := range []string{"openmhz-secret", "bcfy-secret", "rdio-secret-key", "mqtt-pass", "s3cr3t", "hook-token", "mqttuser"} {
		if strings.Contains(string(js), secret) {
			t.Errorf("redacted config still contains %q: %s", secret, js)
		}
	}
	systems := out["systems"].([]any)[0].(map[string]any)
	if systems["apiKey"] != "***" || systems["broadcastifyApiKey"] != "***" {
		t.Errorf("system keys not redacted: %v", systems)
	}
	if systems["shortName"] != "butco" || systems["talkgroupsFile"] != "butco.csv" || systems["broadcastifySystemId"] != float64(1234) {
		t.Errorf("ordinary system settings changed: %v", systems)
	}
	mqtt := out["plugins"].([]any)[1].(map[string]any)
	if mqtt["broker"] != "tcp://mqtt.example:1883" || mqtt["password"] != "***" || mqtt["Credentials"] != "***" {
		t.Errorf("mqtt plugin = %v", mqtt)
	}
	if out["statusServer"] != "ws://status.example:3005/server" {
		t.Errorf("statusServer = %v", out["statusServer"])
	}
	if out["broadcastifyCallsServer"] != "https://api.broadcastify.com/call-upload" {
		t.Errorf("broadcastifyCallsServer = %v", out["broadcastifyCallsServer"])
	}

	// Case-insensitive names, empty values kept, booleans kept.
	got := redactReport(map[string]any{
		"AUTH_TOKEN": "x", "Pass": "y", "db_credential": 42.0, "secret": "", "apikey": nil,
		"authEnabled": true, "name": "not a secret", "note": "http://u:p@host/path?q=1#frag",
	}).(map[string]any)
	want := map[string]any{
		"AUTH_TOKEN": "***", "Pass": "***", "db_credential": "***", "secret": "", "apikey": nil,
		"authEnabled": true, "name": "not a secret", "note": "http://host/path",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}
