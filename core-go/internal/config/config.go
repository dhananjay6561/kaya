// Package config loads and validates core's settings from environment variables.
package config

import (
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

var logLevels = []string{"trace", "debug", "info", "warn", "error", "fatal"}

// Groq holds the settings of the Groq LLM provider.
type Groq struct {
	APIKey        string
	HeavyModel    string
	RoutineModel  string
	ContextTokens int
}

// LLM names the LLM provider and its settings.
type LLM struct {
	Provider string
	Groq     Groq
}

// Config holds core's settings, already validated.
type Config struct {
	LogLevel    string
	DataDir     string
	ListenHost  string
	Port        int
	DatabaseURL string
	TimeZone    string
	LLM         LLM
	Channel     string
	Telephony   string
}

// Error lists every problem found in the config. It never includes the values themselves.
type Error struct {
	Problems []string
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("Config is invalid. Fix these in .env (see .env.example):")
	for _, p := range e.Problems {
		b.WriteString("\n  - ")
		b.WriteString(p)
	}
	return b.String()
}

// Load reads the config from getenv. A relative DATA_DIR is resolved against baseDir.
func Load(getenv func(string) string, baseDir string) (Config, error) {
	var problems []string
	fail := func(key, reason string) { problems = append(problems, key+": "+reason) }
	oneOf := func(key, fallback string, allowed ...string) string {
		value := orDefault(getenv(key), fallback)
		if !slices.Contains(allowed, value) {
			fail(key, "must be one of "+strings.Join(allowed, ", "))
		}
		return value
	}

	cfg := Config{
		LogLevel:   oneOf("LOG_LEVEL", "info", logLevels...),
		DataDir:    orDefault(getenv("DATA_DIR"), "./data"),
		ListenHost: orDefault(getenv("LISTEN_HOST"), "127.0.0.1"),
		Port:       3000,
		TimeZone:   orDefault(getenv("TIMEZONE"), "UTC"),
		LLM: LLM{
			Provider: oneOf("LLM_PROVIDER", "groq", "groq", "stub"),
			Groq: Groq{
				APIKey:        getenv("GROQ_API_KEY"),
				HeavyModel:    orDefault(getenv("GROQ_MODEL_HEAVY"), "openai/gpt-oss-120b"),
				RoutineModel:  orDefault(getenv("GROQ_MODEL_ROUTINE"), "openai/gpt-oss-20b"),
				ContextTokens: 2000,
			},
		},
		Channel:   oneOf("CHAT_CHANNEL", "gateway", "gateway", "stub"),
		Telephony: oneOf("TELEPHONY_PROVIDER", "stub", "stub"),
	}

	dbKey := "CORE_DATABASE_URL"
	if getenv(dbKey) == "" {
		dbKey = "DATABASE_URL"
	}
	cfg.DatabaseURL = getenv(dbKey)
	switch u, err := url.Parse(cfg.DatabaseURL); {
	case cfg.DatabaseURL == "":
		fail("DATABASE_URL", "required")
	case err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "":
		fail(dbKey, "must be a postgres:// URL")
	}

	if raw := getenv("CORE_PORT"); raw != "" {
		port, err := strconv.Atoi(raw)
		switch {
		case err != nil:
			fail("CORE_PORT", "must be a number")
		case port < 1 || port > 65535:
			fail("CORE_PORT", "must be between 1 and 65535")
		default:
			cfg.Port = port
		}
	}

	if _, err := time.LoadLocation(cfg.TimeZone); err != nil || cfg.TimeZone == "Local" {
		fail("TIMEZONE", "must be an IANA time zone such as Asia/Kolkata")
	}

	if raw := getenv("GROQ_CONTEXT_TOKENS"); raw != "" {
		n, err := strconv.Atoi(raw)
		switch {
		case err != nil:
			fail("GROQ_CONTEXT_TOKENS", "must be a whole number")
		case n < 500:
			fail("GROQ_CONTEXT_TOKENS", "must be at least 500")
		default:
			cfg.LLM.Groq.ContextTokens = n
		}
	}
	if cfg.LLM.Provider == "groq" && cfg.LLM.Groq.APIKey == "" {
		fail("GROQ_API_KEY", "required")
	}

	if len(problems) > 0 {
		return Config{}, &Error{Problems: problems}
	}
	if !filepath.IsAbs(cfg.DataDir) {
		cfg.DataDir = filepath.Join(baseDir, cfg.DataDir)
	}
	return cfg, nil
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
