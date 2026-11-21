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
	want := Config{LogLevel: "info", DataDir: "/base/data", Port: 3001, DatabaseURL: validURL}
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
	}), "/base")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := Config{LogLevel: "debug", DataDir: "/srv/kaya", Port: 9000, DatabaseURL: "postgresql://db:5432/kaya"}
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
