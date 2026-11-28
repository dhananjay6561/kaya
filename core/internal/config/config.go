// Package config loads and validates core's settings from environment variables.
package config

import (
	"net"
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

// Compatible is any provider with an OpenAI-compatible API (OpenAI, Gemini, OpenRouter, Ollama,
// and Groq itself), set entirely from .env. Kind is "groq" for Groq's own defaults (address,
// gpt-oss models and their exact token counts), or "compatible".
type Compatible struct {
	Kind          string
	BaseURL       string
	APIKey        string
	HeavyModel    string
	RoutineModel  string
	ContextTokens int
	// Local says the provider runs on DJ's own machine or network, so nothing is masked for it.
	Local bool
	// Effort says whether the provider takes reasoning_effort.
	Effort bool
	// TokenFactor multiplies Kaya's token counts, which are exact only for gpt-oss and OpenAI's
	// o200k models, so other models' requests still fit.
	TokenFactor float64
}

// LLM names the LLM provider and its settings.
type LLM struct {
	Provider   string
	Groq       Groq
	Compatible Compatible
	// Routine, when set, answers the routine tier instead: tagging, facts, planning and judging.
	Routine *Compatible
}

// Transcribe is the OpenAI-compatible speech-to-text service voice notes go to.
type Transcribe struct {
	BaseURL string
	APIKey  string
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
	// WebSearchURL is the SearXNG core searches the web through; "" turns web search off.
	WebSearchURL string
	// Transcribe is where voice notes are turned into text; no key turns transcription off.
	Transcribe Transcribe
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
			Provider: oneOf("LLM_PROVIDER", "groq", "groq", "compatible", "stub"),
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
	if raw := strings.TrimSpace(getenv("WEB_SEARCH_URL")); raw != "" {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			fail("WEB_SEARCH_URL", "must be an http:// or https:// URL, or empty to turn web search off")
		} else {
			cfg.WebSearchURL = raw
		}
	}
	if cfg.LLM.Provider == "groq" && cfg.LLM.Groq.APIKey == "" {
		fail("GROQ_API_KEY", "required")
	}
	if cfg.LLM.Provider == "compatible" {
		cfg.LLM.Compatible = compatible("LLM_", "compatible", getenv, fail)
	}
	if kind := strings.TrimSpace(getenv("ROUTINE_LLM_PROVIDER")); kind != "" {
		switch kind {
		case "groq", "compatible":
			r := compatible("ROUTINE_LLM_", kind, getenv, fail)
			cfg.LLM.Routine = &r
		default:
			fail("ROUTINE_LLM_PROVIDER", "must be groq or compatible, or empty to use LLM_PROVIDER for everything")
		}
	}
	cfg.Transcribe = Transcribe{
		BaseURL: orDefault(strings.TrimSpace(getenv("TRANSCRIBE_BASE_URL")), "https://api.groq.com/openai/v1"),
		APIKey:  orDefault(strings.TrimSpace(getenv("TRANSCRIBE_API_KEY")), cfg.LLM.Groq.APIKey),
	}
	if !webURL(cfg.Transcribe.BaseURL) {
		fail("TRANSCRIBE_BASE_URL", "must be an http:// or https:// URL")
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
	cfg.MediaDir = orDefault(getenv("MEDIA_DIR"), filepath.Join(cfg.DataDir, "media"))
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

// compatible reads one OpenAI-compatible provider from the settings that start with prefix, such
// as LLM_BASE_URL or ROUTINE_LLM_BASE_URL. A routine provider has one model, ROUTINE_LLM_MODEL.
func compatible(prefix, kind string, getenv func(string) string, fail func(key, reason string)) Compatible {
	get := func(name string) string { return strings.TrimSpace(getenv(prefix + name)) }
	c := Compatible{Kind: kind, BaseURL: get("BASE_URL"), APIKey: get("API_KEY"), ContextTokens: 4000, TokenFactor: 1.25}
	if kind == "groq" {
		c.BaseURL = orDefault(c.BaseURL, "https://api.groq.com/openai/v1")
		c.TokenFactor, c.Effort = 1, true
	}
	if prefix == "ROUTINE_LLM_" {
		model := get("MODEL")
		if kind == "groq" {
			model = orDefault(model, "openai/gpt-oss-20b")
		}
		c.HeavyModel, c.RoutineModel = model, model
	} else {
		c.HeavyModel = get("MODEL_HEAVY")
		c.RoutineModel = orDefault(get("MODEL_ROUTINE"), c.HeavyModel)
	}
	switch {
	case c.BaseURL == "":
		fail(prefix+"BASE_URL", "required, such as https://api.openai.com/v1")
	case !webURL(c.BaseURL):
		fail(prefix+"BASE_URL", "must be an http:// or https:// URL")
	}
	if c.HeavyModel == "" {
		if prefix == "ROUTINE_LLM_" {
			fail(prefix+"MODEL", "required")
		} else {
			fail(prefix+"MODEL_HEAVY", "required")
		}
	}
	switch get("LOCALITY") {
	case "", "cloud":
	case "local":
		if !onThisNetwork(c.BaseURL) {
			fail(prefix+"LOCALITY", "local is only for a model on this machine or network (localhost, host.docker.internal, a Compose service or a private address); this one would get chats unmasked")
		} else {
			c.Local = true
		}
	default:
		fail(prefix+"LOCALITY", "must be cloud or local")
	}
	if c.APIKey == "" && !c.Local {
		fail(prefix+"API_KEY", "required for a cloud provider")
	}
	switch get("REASONING_EFFORT") {
	case "":
	case "true":
		c.Effort = true
	case "false":
		c.Effort = false
	default:
		fail(prefix+"REASONING_EFFORT", "must be true or false")
	}
	if raw := get("CONTEXT_TOKENS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 500 {
			fail(prefix+"CONTEXT_TOKENS", "must be a whole number of at least 500")
		} else {
			c.ContextTokens = n
		}
	}
	if raw := get("TOKEN_FACTOR"); raw != "" {
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil || f < 1 || f > 3 {
			fail(prefix+"TOKEN_FACTOR", "must be a number from 1 to 3")
		} else {
			c.TokenFactor = f
		}
	}
	return c
}

func webURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil
}

// onThisNetwork says whether a URL's host is this machine, the Docker host, a Compose service
// (a name with no dots) or a private address.
func onThisNetwork(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" || host == "host.docker.internal" || (host != "" && !strings.Contains(host, ".") && !strings.Contains(host, ":")) {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}
