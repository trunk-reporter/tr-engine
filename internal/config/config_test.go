package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	// Set required env vars for all subtests
	cleanup := setEnvs(t, map[string]string{
		"DATABASE_URL":   "postgres://localhost/test",
		"MQTT_BROKER_URL": "tcp://localhost:1883",
	})
	defer cleanup()

	t.Run("defaults", func(t *testing.T) {
		cfg, err := Load(Overrides{EnvFile: "nonexistent.env"})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.HTTPAddr != ":8080" {
			t.Errorf("HTTPAddr = %q, want :8080", cfg.HTTPAddr)
		}
		if cfg.LogLevel != "info" {
			t.Errorf("LogLevel = %q, want info", cfg.LogLevel)
		}
		if cfg.AudioDir != "./audio" {
			t.Errorf("AudioDir = %q, want ./audio", cfg.AudioDir)
		}
		if cfg.MQTTTopics != "#" {
			t.Errorf("MQTTTopics = %q, want #", cfg.MQTTTopics)
		}
		if cfg.MQTTClientID != "tr-engine" {
			t.Errorf("MQTTClientID = %q, want tr-engine", cfg.MQTTClientID)
		}
		if !cfg.RawStore {
			t.Error("RawStore = false, want true")
		}
	})

	t.Run("cli_overrides_take_priority", func(t *testing.T) {
		cfg, err := Load(Overrides{
			EnvFile:       "nonexistent.env",
			HTTPAddr:      ":9090",
			LogLevel:      "debug",
			DatabaseURL:   "postgres://override/db",
			MQTTBrokerURL: "tcp://override:1883",
			AudioDir:      "/tmp/audio",
		})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.HTTPAddr != ":9090" {
			t.Errorf("HTTPAddr = %q, want :9090", cfg.HTTPAddr)
		}
		if cfg.LogLevel != "debug" {
			t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
		}
		if cfg.DatabaseURL != "postgres://override/db" {
			t.Errorf("DatabaseURL = %q, want override", cfg.DatabaseURL)
		}
		if cfg.MQTTBrokerURL != "tcp://override:1883" {
			t.Errorf("MQTTBrokerURL = %q, want override", cfg.MQTTBrokerURL)
		}
		if cfg.AudioDir != "/tmp/audio" {
			t.Errorf("AudioDir = %q, want /tmp/audio", cfg.AudioDir)
		}
	})

	t.Run("env_vars_read", func(t *testing.T) {
		cfg, err := Load(Overrides{EnvFile: "nonexistent.env"})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.DatabaseURL != "postgres://localhost/test" {
			t.Errorf("DatabaseURL = %q, want postgres://localhost/test", cfg.DatabaseURL)
		}
		if cfg.MQTTBrokerURL != "tcp://localhost:1883" {
			t.Errorf("MQTTBrokerURL = %q, want tcp://localhost:1883", cfg.MQTTBrokerURL)
		}
	})

	t.Run("empty_overrides_use_env", func(t *testing.T) {
		cfg, err := Load(Overrides{EnvFile: "nonexistent.env"})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		// Empty override fields should not overwrite env values
		if cfg.DatabaseURL != "postgres://localhost/test" {
			t.Errorf("DatabaseURL = %q, want env value", cfg.DatabaseURL)
		}
	})
}

func TestLoadMissingRequired(t *testing.T) {
	// Clear any existing values
	cleanup := setEnvs(t, map[string]string{
		"DATABASE_URL":    "",
		"MQTT_BROKER_URL": "",
	})
	defer cleanup()
	os.Unsetenv("DATABASE_URL")
	os.Unsetenv("MQTT_BROKER_URL")

	_, err := Load(Overrides{EnvFile: "nonexistent.env"})
	if err == nil {
		t.Error("expected error when required env vars are missing")
	}
}

// TestLoadDatabaseURLFlagOnly checks that --database-url alone satisfies
// the required DATABASE_URL.
func TestLoadDatabaseURLFlagOnly(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	os.Unsetenv("DATABASE_URL")
	cfg, err := Load(Overrides{EnvFile: "nonexistent.env", DatabaseURL: "postgres://flag/db"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DatabaseURL != "postgres://flag/db" {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
}

func TestStreamConfig(t *testing.T) {
	cleanup := setEnvs(t, map[string]string{
		"DATABASE_URL":       "postgres://localhost/test",
		"MQTT_BROKER_URL":    "tcp://localhost:1883",
		"STREAM_LISTEN":      ":9123",
		"STREAM_OPUS_BITRATE": "24000",
		"STREAM_MAX_CLIENTS": "25",
		"STREAM_IDLE_TIMEOUT": "45s",
	})
	defer cleanup()

	cfg, err := Load(Overrides{EnvFile: "nonexistent.env"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.StreamListen != ":9123" {
		t.Errorf("StreamListen = %q, want :9123", cfg.StreamListen)
	}
	if cfg.StreamOpusBitrate != 24000 {
		t.Errorf("StreamOpusBitrate = %d, want 24000", cfg.StreamOpusBitrate)
	}
	if cfg.StreamMaxClients != 25 {
		t.Errorf("StreamMaxClients = %d, want 25", cfg.StreamMaxClients)
	}
	if cfg.StreamIdleTimeout != 45*time.Second {
		t.Errorf("StreamIdleTimeout = %v, want 45s", cfg.StreamIdleTimeout)
	}
}

func TestStreamConfigDefaults(t *testing.T) {
	cleanup := setEnvs(t, map[string]string{
		"DATABASE_URL":    "postgres://localhost/test",
		"MQTT_BROKER_URL": "tcp://localhost:1883",
	})
	defer cleanup()

	cfg, err := Load(Overrides{EnvFile: "nonexistent.env"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.StreamListen != "" {
		t.Errorf("StreamListen = %q, want empty", cfg.StreamListen)
	}
	if cfg.StreamSampleRate != 8000 {
		t.Errorf("StreamSampleRate = %d, want 8000", cfg.StreamSampleRate)
	}
	if cfg.StreamOpusBitrate != 16000 {
		t.Errorf("StreamOpusBitrate = %d, want 16000", cfg.StreamOpusBitrate)
	}
	if cfg.StreamMaxClients != 50 {
		t.Errorf("StreamMaxClients = %d, want 50", cfg.StreamMaxClients)
	}
	if cfg.StreamIdleTimeout != 30*time.Second {
		t.Errorf("StreamIdleTimeout = %v, want 30s", cfg.StreamIdleTimeout)
	}
}

func TestUnitTagSuggestionsConfig(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("MQTT_BROKER_URL", "tcp://localhost:1883")

	cfg, err := Load(Overrides{EnvFile: "nonexistent.env"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UnitTagSuggestions {
		t.Error("UnitTagSuggestions = true, want false (opt-in)")
	}
	if cfg.UnitTagSuggestionsMinCalls != 3 || cfg.UnitTagSuggestionsMinShare != 0.2 ||
		cfg.UnitTagSuggestionsInterval != 60*time.Second {
		t.Errorf("defaults = %d/%g/%v, want 3/0.2/60s", cfg.UnitTagSuggestionsMinCalls,
			cfg.UnitTagSuggestionsMinShare, cfg.UnitTagSuggestionsInterval)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate defaults: %v", err)
	}

	t.Setenv("UNIT_TAG_SUGGESTIONS", "true")
	t.Setenv("UNIT_TAG_SUGGESTIONS_MIN_CALLS", "5")
	t.Setenv("UNIT_TAG_SUGGESTIONS_MIN_SHARE", "0.5")
	t.Setenv("UNIT_TAG_SUGGESTIONS_INTERVAL", "2m")
	cfg, err = Load(Overrides{EnvFile: "nonexistent.env"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.UnitTagSuggestions || cfg.UnitTagSuggestionsMinCalls != 5 ||
		cfg.UnitTagSuggestionsMinShare != 0.5 || cfg.UnitTagSuggestionsInterval != 2*time.Minute {
		t.Errorf("parsed = %v/%d/%g/%v", cfg.UnitTagSuggestions, cfg.UnitTagSuggestionsMinCalls,
			cfg.UnitTagSuggestionsMinShare, cfg.UnitTagSuggestionsInterval)
	}

	for _, tc := range []struct{ key, val string }{
		{"UNIT_TAG_SUGGESTIONS_MIN_CALLS", "0"},
		{"UNIT_TAG_SUGGESTIONS_MIN_SHARE", "1.5"},
		{"UNIT_TAG_SUGGESTIONS_MIN_SHARE", "-0.1"},
		{"UNIT_TAG_SUGGESTIONS_INTERVAL", "0s"},
	} {
		t.Run(tc.key+"="+tc.val, func(t *testing.T) {
			t.Setenv(tc.key, tc.val)
			cfg, err := Load(Overrides{EnvFile: "nonexistent.env"})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if err := cfg.Validate(); err == nil {
				t.Error("Validate accepted an invalid value")
			}
		})
	}
}

// TestLoad_LegacyAuthVariables checks the removed auth variables are read
// verbatim into LegacyAuth (for the one-time import and the warnings) and
// that nothing clears, derives or generates them any more.
func TestLoad_LegacyAuthVariables(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("MQTT_BROKER_URL", "tcp://localhost:1883")
	t.Setenv("AUTH_ENABLED", "false")
	t.Setenv("AUTH_TOKEN", "read-token")
	t.Setenv("WRITE_TOKEN", "write-token")
	t.Setenv("ADMIN_USERNAME", "root")
	t.Setenv("ADMIN_PASSWORD", "hunter22hunter22")
	t.Setenv("JWT_SECRET", "jwt-secret")
	t.Setenv("CORS_ORIGINS", "https://a.example")

	cfg, err := Load(Overrides{EnvFile: "nonexistent.env"})
	if err != nil {
		t.Fatal(err)
	}
	want := LegacyAuthEnv{
		AuthEnabled:   "false",
		AuthToken:     "read-token",
		WriteToken:    "write-token",
		AdminUsername: "root",
		AdminPassword: "hunter22hunter22",
		JWTSecret:     "jwt-secret",
		CORSOrigins:   "https://a.example",
	}
	if cfg.LegacyAuth != want {
		t.Errorf("LegacyAuth = %+v, want %+v", cfg.LegacyAuth, want)
	}
	var names []string
	for _, v := range cfg.LegacyAuth.Set() {
		names = append(names, v.Name)
	}
	if got := strings.Join(names, ","); got != "AUTH_ENABLED,AUTH_TOKEN,WRITE_TOKEN,ADMIN_USERNAME,ADMIN_PASSWORD,JWT_SECRET,CORS_ORIGINS" {
		t.Errorf("Set() = %s", got)
	}
}

func TestLoad_LegacyAuthUnset(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("MQTT_BROKER_URL", "tcp://localhost:1883")
	for _, v := range []string{"AUTH_ENABLED", "AUTH_TOKEN", "WRITE_TOKEN", "ADMIN_USERNAME", "ADMIN_PASSWORD", "JWT_SECRET", "CORS_ORIGINS"} {
		t.Setenv(v, "") // restored after the test
		os.Unsetenv(v)
	}
	cfg, err := Load(Overrides{EnvFile: "nonexistent.env"})
	if err != nil {
		t.Fatal(err)
	}
	// No defaults: ADMIN_USERNAME used to default to "admin", which would
	// now produce a warning for a variable nobody set.
	if cfg.LegacyAuth != (LegacyAuthEnv{}) || len(cfg.LegacyAuth.Set()) != 0 {
		t.Errorf("LegacyAuth = %+v, want all unset", cfg.LegacyAuth)
	}
}

func TestLoad_TrustedProxiesDefault(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/test")
	t.Setenv("MQTT_BROKER_URL", "tcp://localhost:1883")

	cfg, err := Load(Overrides{EnvFile: "nonexistent.env"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TrustedProxies != "loopback,private" {
		t.Errorf("TrustedProxies = %q, want loopback,private", cfg.TrustedProxies)
	}
}

// setEnvs sets environment variables and returns a cleanup function.
func setEnvs(t *testing.T, envs map[string]string) func() {
	t.Helper()
	originals := make(map[string]string)
	unset := make([]string, 0)

	for k, v := range envs {
		if orig, ok := os.LookupEnv(k); ok {
			originals[k] = orig
		} else {
			unset = append(unset, k)
		}
		os.Setenv(k, v)
	}

	return func() {
		for k, v := range originals {
			os.Setenv(k, v)
		}
		for _, k := range unset {
			os.Unsetenv(k)
		}
	}
}
