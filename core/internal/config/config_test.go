package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

var valid = map[string]string{
	"DATABASE_URL":   "postgres://localhost:5432/kaya",
	"GROQ_API_KEY":   "test-key-not-real",
	"VOYAGE_API_KEY": "test-key-not-real",
}

func env(values map[string]string, extra ...string) func(string) string {
	merged := map[string]string{}
	for k, v := range values {
		merged[k] = v
	}
	for i := 0; i+1 < len(extra); i += 2 {
		merged[extra[i]] = extra[i+1]
	}
	return func(key string) string { return merged[key] }
}

func load(t *testing.T, getenv func(string) string) Config {
	t.Helper()
	cfg, err := Load(getenv, "/base")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func problems(t *testing.T, getenv func(string) string) []string {
	t.Helper()
	_, err := Load(getenv, "/base")
	var cfgErr *Error
	if !errors.As(err, &cfgErr) {
		t.Fatalf("want a config error, got %v", err)
	}
	return cfgErr.Problems
}

func TestDefaults(t *testing.T) {
	cfg := load(t, env(valid))
	want := Config{
		LogLevel:    "info",
		DataDir:     "/base/data",
		ListenHost:  "127.0.0.1",
		Port:        3000,
		DatabaseURL: valid["DATABASE_URL"],
		TimeZone:    "UTC",
		LLM: LLM{Provider: "groq", Groq: Groq{
			APIKey:        "test-key-not-real",
			HeavyModel:    "openai/gpt-oss-120b",
			RoutineModel:  "openai/gpt-oss-20b",
			ContextTokens: 2000,
		}},
		Channel:             "gateway",
		Embeddings:          Embeddings{Provider: "voyage", Voyage: Voyage{APIKey: "test-key-not-real", Model: "voyage-4"}},
		Telephony:           "stub",
		TaggingTokensPerDay: 100_000,
		AlertsPerHour:       10,
		SendsPerHour:        20,
	}
	if cfg != want {
		t.Fatalf("got %+v, want %+v", cfg, want)
	}
}

func TestReadsEveryValue(t *testing.T) {
	cfg := load(t, env(valid,
		"LOG_LEVEL", "debug", "CORE_PORT", "8080", "LISTEN_HOST", "0.0.0.0", "DATA_DIR", "/srv/kaya",
		"TIMEZONE", "Asia/Kolkata", "GROQ_MODEL_HEAVY", "heavy", "GROQ_MODEL_ROUTINE", "routine",
		"GROQ_CONTEXT_TOKENS", "12000", "TAGGING_TOKENS_PER_DAY", "50000", "ALERTS_PER_HOUR", "4", "SENDS_PER_HOUR", "3",
	))
	if cfg.LogLevel != "debug" || cfg.Port != 8080 || cfg.ListenHost != "0.0.0.0" || cfg.DataDir != "/srv/kaya" ||
		cfg.TimeZone != "Asia/Kolkata" || cfg.LLM.Groq.HeavyModel != "heavy" ||
		cfg.LLM.Groq.RoutineModel != "routine" || cfg.LLM.Groq.ContextTokens != 12000 || cfg.TaggingTokensPerDay != 50000 || cfg.AlertsPerHour != 4 || cfg.SendsPerHour != 3 {
		t.Fatalf("not every value was read: %+v", cfg)
	}
}

func TestAcceptsPostgresqlURLs(t *testing.T) {
	load(t, env(valid, "DATABASE_URL", "postgresql://kaya@db/kaya"))
}

func TestPrefersCoreDatabaseURL(t *testing.T) {
	cfg := load(t, env(valid, "CORE_DATABASE_URL", "postgres://kaya_core@db/kaya"))
	if cfg.DatabaseURL != "postgres://kaya_core@db/kaya" {
		t.Fatalf("got %q", cfg.DatabaseURL)
	}
	if got := problems(t, env(valid, "CORE_DATABASE_URL", "http://db/kaya")); !slices.Equal(got, []string{"CORE_DATABASE_URL: must be a postgres:// URL"}) {
		t.Fatalf("got %v", got)
	}
}

func TestSwitchesProvidersThroughConfig(t *testing.T) {
	cfg := load(t, env(map[string]string{"DATABASE_URL": valid["DATABASE_URL"], "LLM_PROVIDER": "stub", "CHAT_CHANNEL": "stub", "EMBEDDINGS_PROVIDER": "stub"}))
	if cfg.LLM.Provider != "stub" || cfg.Channel != "stub" {
		t.Fatalf("got %+v", cfg)
	}
}

func TestGroqKeyOnlyForGroq(t *testing.T) {
	base := map[string]string{"DATABASE_URL": valid["DATABASE_URL"], "EMBEDDINGS_PROVIDER": "stub"}
	if got := problems(t, env(base, "LLM_PROVIDER", "groq")); !slices.Equal(got, []string{"GROQ_API_KEY: required"}) {
		t.Fatalf("got %v", got)
	}
	load(t, env(base, "LLM_PROVIDER", "stub"))
}

func TestRejectsUnknownProviders(t *testing.T) {
	got := problems(t, env(valid, "LLM_PROVIDER", "openai", "CHAT_CHANNEL", "telegram", "TELEPHONY_PROVIDER", "twilio"))
	for _, key := range []string{"LLM_PROVIDER", "CHAT_CHANNEL", "TELEPHONY_PROVIDER"} {
		if !slices.ContainsFunc(got, func(p string) bool { return strings.HasPrefix(p, key+": must be one of") }) {
			t.Fatalf("%s not reported in %v", key, got)
		}
	}
}

func TestListsEveryMissingValueAtOnce(t *testing.T) {
	got := problems(t, env(nil))
	if !slices.Equal(got, []string{"DATABASE_URL: required", "GROQ_API_KEY: required", "VOYAGE_API_KEY: required"}) {
		t.Fatalf("got %v", got)
	}
	if got := problems(t, env(map[string]string{"DATABASE_URL": "", "GROQ_API_KEY": "", "VOYAGE_API_KEY": ""})); len(got) != 3 {
		t.Fatalf("empty values should count as missing: %v", got)
	}
}

func TestRejectsInvalidValuesWithAReason(t *testing.T) {
	got := problems(t, env(valid,
		"CORE_PORT", "70000", "DATABASE_URL", "http://localhost/kaya", "LOG_LEVEL", "loud",
		"TIMEZONE", "India/Delhi", "GROQ_CONTEXT_TOKENS", "100", "TAGGING_TOKENS_PER_DAY", "100", "ALERTS_PER_HOUR", "-1",
	))
	for _, want := range []string{
		"CORE_PORT: must be between 1 and 65535",
		"DATABASE_URL: must be a postgres:// URL",
		"TIMEZONE: must be an IANA time zone such as Asia/Kolkata",
		"GROQ_CONTEXT_TOKENS: must be at least 500",
		"TAGGING_TOKENS_PER_DAY: must be 0 (off) or at least 5000",
		"ALERTS_PER_HOUR: must be 0 (off) or a positive whole number",
	} {
		if !slices.Contains(got, want) {
			t.Fatalf("%q missing from %v", want, got)
		}
	}
	if !slices.ContainsFunc(got, func(p string) bool { return strings.HasPrefix(p, "LOG_LEVEL: ") }) {
		t.Fatalf("LOG_LEVEL missing from %v", got)
	}
	if got := problems(t, env(valid, "CORE_PORT", "abc")); !slices.Equal(got, []string{"CORE_PORT: must be a number"}) {
		t.Fatalf("got %v", got)
	}
}

func TestNeverPutsSecretsInTheError(t *testing.T) {
	secret := "hunter2"
	url := fmt.Sprintf("http://user:%s@db/kaya", secret)
	_, err := Load(env(map[string]string{"DATABASE_URL": url, "GROQ_API_KEY": "gsk_secret"}, "CORE_PORT", "x"), "/base")
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "gsk_secret") {
		t.Fatalf("secret leaked or no error: %v", err)
	}
}

func TestTimeZonesAreFoundWhateverTheirCase(t *testing.T) {
	for _, c := range []struct{ given, want string }{
		{"Asia/Kolkata", "Asia/Kolkata"},
		{"asia/kolkata", "Asia/Kolkata"},
		{" ASIA/KOLKATA ", "Asia/Kolkata"},
		{"utc", "UTC"},
		{"america/new_york", "America/New_York"},
		{"America/Port-au-Prince", "America/Port-au-Prince"},
		{"US/Eastern", "US/Eastern"},
		{"Etc/GMT+5", "Etc/GMT+5"},
	} {
		if got := load(t, env(valid, "TIMEZONE", c.given)).TimeZone; got != c.want {
			t.Errorf("%q: got %q, want %q", c.given, got, c.want)
		}
	}
	for _, bad := range []string{"Local", "local", "India/Delhi", "+05:30", "asia"} {
		if got := problems(t, env(valid, "TIMEZONE", bad)); !slices.Equal(got, []string{"TIMEZONE: must be an IANA time zone such as Asia/Kolkata"}) {
			t.Errorf("%q: got %v", bad, got)
		}
	}
}

func TestDatabaseURLsAreTrimmedAndSocketsAllowed(t *testing.T) {
	if got := load(t, env(valid, "DATABASE_URL", "  postgres://localhost/kaya  ")).DatabaseURL; got != "postgres://localhost/kaya" {
		t.Fatalf("got %q", got)
	}
	load(t, env(valid, "DATABASE_URL", "postgres:///kaya?host=/var/run/postgresql"))
	if got := load(t, env(valid, "CORE_DATABASE_URL", "  ")).DatabaseURL; got != valid["DATABASE_URL"] {
		t.Fatalf("an empty CORE_DATABASE_URL should fall back to DATABASE_URL, got %q", got)
	}
	if got := load(t, env(valid, "CORE_PORT", " 8080 ")).Port; got != 8080 {
		t.Fatalf("got %d", got)
	}
}

func TestNeedsAVoyageKeyOnlyForVoyage(t *testing.T) {
	base := map[string]string{"DATABASE_URL": valid["DATABASE_URL"], "GROQ_API_KEY": "k"}
	if got := problems(t, env(base)); !slices.Equal(got, []string{"VOYAGE_API_KEY: required"}) {
		t.Fatalf("got %v", got)
	}
	cfg := load(t, env(base, "EMBEDDINGS_PROVIDER", "stub"))
	if cfg.Embeddings.Provider != "stub" {
		t.Fatalf("provider %q", cfg.Embeddings.Provider)
	}
	if got := problems(t, env(valid, "EMBEDDINGS_PROVIDER", "openai")); !slices.Equal(got, []string{"EMBEDDINGS_PROVIDER: must be one of voyage, stub"}) {
		t.Fatalf("got %v", got)
	}
	if cfg := load(t, env(valid, "VOYAGE_MODEL", "voyage-4-large")); cfg.Embeddings.Voyage.Model != "voyage-4-large" {
		t.Fatalf("model %q", cfg.Embeddings.Voyage.Model)
	}
}

func TestAlertsCanBeTurnedOff(t *testing.T) {
	if cfg := load(t, env(valid, "ALERTS_PER_HOUR", "0")); cfg.AlertsPerHour != 0 {
		t.Fatalf("got %d", cfg.AlertsPerHour)
	}
}

func TestTaggingCanBeTurnedOff(t *testing.T) {
	if cfg := load(t, env(valid, "TAGGING_TOKENS_PER_DAY", "0")); cfg.TaggingTokensPerDay != 0 {
		t.Fatalf("got %d", cfg.TaggingTokensPerDay)
	}
}
