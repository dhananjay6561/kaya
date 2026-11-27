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

// Voyage holds the settings of the Voyage embeddings provider.
type Voyage struct {
	APIKey string
	Model  string
}

// Embeddings names the embeddings provider and its settings. Vectors always have 1024 dimensions,
// the size of the search index's column.
type Embeddings struct {
	Provider string
	Voyage   Voyage
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
	Embeddings  Embeddings
	Channel     string
	Telephony   string
	// TaggingTokensPerDay is the most routine-model tokens tagging spends a day; 0 turns tagging off.
	TaggingTokensPerDay int
	// AlertsPerHour is the most alert messages Kaya sends DJ in an hour; 0 turns alerts off.
	AlertsPerHour int
	// SendsPerHour is the most messages Kaya sends to other chats for DJ in an hour, each after his
	// yes; 0 turns sending for him off.
	SendsPerHour int
	// TranscribeModel is the speech model voice notes are transcribed with, on Groq.
	TranscribeModel string
	// MediaDir is where the gateway keeps downloaded files, which core reads (read-only).
	MediaDir string
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
		Embeddings: Embeddings{
			Provider: oneOf("EMBEDDINGS_PROVIDER", "voyage", "voyage", "stub"),
			Voyage: Voyage{
				APIKey: getenv("VOYAGE_API_KEY"),
				Model:  orDefault(getenv("VOYAGE_MODEL"), "voyage-4"),
			},
		},
		Channel:   oneOf("CHAT_CHANNEL", "gateway", "gateway", "stub"),
		Telephony: oneOf("TELEPHONY_PROVIDER", "stub", "stub"),
	}

	dbKey := "CORE_DATABASE_URL"
	if strings.TrimSpace(getenv(dbKey)) == "" {
		dbKey = "DATABASE_URL"
	}
	cfg.DatabaseURL = strings.TrimSpace(getenv(dbKey))
	switch u, err := url.Parse(cfg.DatabaseURL); {
	case cfg.DatabaseURL == "":
		fail("DATABASE_URL", "required")
	case err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql"):
		fail(dbKey, "must be a postgres:// URL")
	}

	if raw := strings.TrimSpace(getenv("CORE_PORT")); raw != "" {
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

	if zone, ok := canonicalZone(cfg.TimeZone); ok {
		cfg.TimeZone = zone
	} else {
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
	cfg.TaggingTokensPerDay = 100_000
	if raw := strings.TrimSpace(getenv("TAGGING_TOKENS_PER_DAY")); raw != "" {
		n, err := strconv.Atoi(raw)
		switch {
		case err != nil:
			fail("TAGGING_TOKENS_PER_DAY", "must be a whole number")
		case n != 0 && n < 5000:
			fail("TAGGING_TOKENS_PER_DAY", "must be 0 (off) or at least 5000")
		default:
			cfg.TaggingTokensPerDay = n
		}
	}
	cfg.AlertsPerHour = 10
	if raw := strings.TrimSpace(getenv("ALERTS_PER_HOUR")); raw != "" {
		n, err := strconv.Atoi(raw)
		switch {
		case err != nil || n < 0:
			fail("ALERTS_PER_HOUR", "must be 0 (off) or a positive whole number")
		default:
			cfg.AlertsPerHour = n
		}
	}
	cfg.SendsPerHour = 20
	if raw := strings.TrimSpace(getenv("SENDS_PER_HOUR")); raw != "" {
		n, err := strconv.Atoi(raw)
		switch {
		case err != nil || n < 0:
			fail("SENDS_PER_HOUR", "must be 0 (off) or a positive whole number")
		default:
			cfg.SendsPerHour = n
		}
	}
	if cfg.LLM.Provider == "groq" && cfg.LLM.Groq.APIKey == "" {
		fail("GROQ_API_KEY", "required")
	}
	if cfg.Embeddings.Provider == "voyage" && cfg.Embeddings.Voyage.APIKey == "" {
		fail("VOYAGE_API_KEY", "required")
	}

	if len(problems) > 0 {
		return Config{}, &Error{Problems: problems}
	}
	if !filepath.IsAbs(cfg.DataDir) {
		cfg.DataDir = filepath.Join(baseDir, cfg.DataDir)
	}
	cfg.TranscribeModel = orDefault(getenv("GROQ_MODEL_TRANSCRIBE"), "whisper-large-v3-turbo")
	cfg.MediaDir = orDefault(getenv("MEDIA_DIR"), filepath.Join(cfg.DataDir, "gateway", "media"))
	if !filepath.IsAbs(cfg.MediaDir) {
		cfg.MediaDir = filepath.Join(baseDir, cfg.MediaDir)
	}
	return cfg, nil
}

// canonicalZone finds an IANA zone whatever its case, and gives its proper spelling, so a value
// accepted on a Mac (whose file system ignores case) is accepted the same way in a container.
func canonicalZone(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || strings.EqualFold(name, "local") {
		return "", false
	}
	for _, short := range []string{"UTC", "GMT"} {
		if strings.EqualFold(name, short) {
			return short, true
		}
	}
	for _, candidate := range []string{name, titled(name), strings.ToUpper(name)} {
		if loc, err := time.LoadLocation(candidate); err == nil && validZoneName(candidate) {
			return loc.String(), true
		}
	}
	return "", false
}

// validZoneName accepts the way IANA spells zones: each part starts with a capital and has small
// letters too ("Asia/Kolkata"), unless it is a short code or carries digits ("US", "Etc/GMT+5").
func validZoneName(name string) bool {
	for part := range strings.SplitSeq(name, "/") {
		switch {
		case part == "" || part[0] < 'A' || part[0] > 'Z':
			return false
		case strings.ToUpper(part) != part:
		case len(part) <= 3 || strings.ContainsAny(part, "0123456789"):
		default:
			return false
		}
	}
	return true
}

func titled(name string) string {
	parts := strings.Split(name, "/")
	for i, part := range parts {
		words := strings.FieldsFunc(part, func(r rune) bool { return r == '_' || r == '-' })
		seps := strings.FieldsFunc(part, func(r rune) bool { return r != '_' && r != '-' })
		var b strings.Builder
		for j, word := range words {
			b.WriteString(strings.ToUpper(word[:1]) + strings.ToLower(word[1:]))
			if j < len(seps) {
				b.WriteString(seps[j])
			}
		}
		parts[i] = b.String()
	}
	return strings.Join(parts, "/")
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
