package api

import (
	"testing"
	"time"

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

		AuthEnabled: true,
		AuthToken:   "my-secret-auth-token",
		WriteToken:  "my-secret-write-token",
		LogLevel:    "info",

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

	// Secret fields should be redacted
	secretFields := map[string]string{
		"AuthToken":    "***",
		"WriteToken":   "***",
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
	if got := result["ReadTimeout"]; got != "5s" {
		t.Errorf("ReadTimeout: got %q, want %q", got, "5s")
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
