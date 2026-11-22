package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const validURL = "postgres://localhost:5432/kaya"

func env(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(map[string]string{"DATABASE_URL": validURL}), "/base")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := Config{
		LogLevel: "info", DataDir: "/base/data", ListenHost: "127.0.0.1", Port: 3001, DatabaseURL: validURL,
		MediaMaxBytes: 100 << 20, MediaMaxPerHour: 60,
	}
	if cfg != want {
		t.Errorf("got %+v, want %+v", cfg, want)
	}
}

func TestLoadAllValues(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"DATABASE_URL": "postgresql://db:5432/kaya",
		"LOG_LEVEL":    "debug",
		"DATA_DIR":     "/srv/kaya",
		"GATEWAY_PORT": "9000",
		"LISTEN_HOST":  "0.0.0.0",
	}), "/base")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := Config{
		LogLevel: "debug", DataDir: "/srv/kaya", ListenHost: "0.0.0.0", Port: 9000,
		DatabaseURL: "postgresql://db:5432/kaya", MediaMaxBytes: 100 << 20, MediaMaxPerHour: 60,
	}
	if cfg != want {
		t.Errorf("got %+v, want %+v", cfg, want)
	}
}

func TestLoadProblems(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want []string
	}{
		{"nothing set", map[string]string{}, []string{"DATABASE_URL: required"}},
		{
			"port not a number",
			map[string]string{"DATABASE_URL": validURL, "GATEWAY_PORT": "abc"},
			[]string{"GATEWAY_PORT: must be a number"},
		},
		{
			"port out of range",
			map[string]string{"DATABASE_URL": validURL, "GATEWAY_PORT": "70000"},
			[]string{"GATEWAY_PORT: must be between 1 and 65535"},
		},
		{
			"wrong scheme",
			map[string]string{"DATABASE_URL": "http://localhost/kaya"},
			[]string{"DATABASE_URL: must be a postgres:// URL"},
		},
		{
			"no host",
			map[string]string{"DATABASE_URL": "postgres://"},
			[]string{"DATABASE_URL: must be a postgres:// URL"},
		},
		{"everything wrong at once", map[string]string{"LOG_LEVEL": "loud", "GATEWAY_PORT": "0"}, []string{
			"LOG_LEVEL: must be one of trace, debug, info, warn, error, fatal",
			"GATEWAY_PORT: must be between 1 and 65535",
			"DATABASE_URL: required",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(env(tt.env), "/base")
			var cfgErr *Error
			if !errors.As(err, &cfgErr) {
				t.Fatalf("expected *Error, got %v", err)
			}
			if !reflect.DeepEqual(cfgErr.Problems, tt.want) {
				t.Errorf("got %q, want %q", cfgErr.Problems, tt.want)
			}
		})
	}
}

func TestErrorNeverIncludesValues(t *testing.T) {
	secret := "postgres-password-not-real"
	_, err := Load(env(map[string]string{"DATABASE_URL": "mysql://u:" + secret + "@h/db"}), "/base")
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("error must exist and must not include the value: %v", err)
	}
}

func TestGatewayDatabaseURLWinsOverTheSharedOne(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"DATABASE_URL":         "postgres://db:5432/shared",
		"GATEWAY_DATABASE_URL": "postgres://db:5432/gateway",
	}), "/base")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseURL != "postgres://db:5432/gateway" {
		t.Fatalf("got %q", cfg.DatabaseURL)
	}
	_, err = Load(env(map[string]string{"DATABASE_URL": validURL, "GATEWAY_DATABASE_URL": "http://x/y"}), "/base")
	if err == nil || !strings.Contains(err.Error(), "GATEWAY_DATABASE_URL: must be a postgres:// URL") {
		t.Fatalf("got %v", err)
	}
}

func TestMediaLimits(t *testing.T) {
	cfg, err := Load(env(map[string]string{"DATABASE_URL": validURL}), "/base")
	if err != nil || cfg.MediaMaxBytes != 100<<20 || cfg.MediaMaxPerHour != 60 {
		t.Fatalf("defaults: %+v, %v", cfg, err)
	}
	cfg, err = Load(env(map[string]string{"DATABASE_URL": validURL, "MEDIA_MAX_BYTES": "1048576", "MEDIA_MAX_PER_HOUR": "5"}), "/base")
	if err != nil || cfg.MediaMaxBytes != 1<<20 || cfg.MediaMaxPerHour != 5 {
		t.Fatalf("set values: %+v, %v", cfg, err)
	}
	_, err = Load(env(map[string]string{"DATABASE_URL": validURL, "MEDIA_MAX_BYTES": "0", "MEDIA_MAX_PER_HOUR": "many"}), "/base")
	if err == nil || !strings.Contains(err.Error(), "MEDIA_MAX_BYTES") || !strings.Contains(err.Error(), "MEDIA_MAX_PER_HOUR") {
		t.Fatalf("got %v", err)
	}
}
