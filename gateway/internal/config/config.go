// Package config loads and validates the gateway's settings from environment variables.
package config

import (
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

var logLevels = []string{"trace", "debug", "info", "warn", "error", "fatal"}

// Config holds the gateway settings, already validated.
type Config struct {
	LogLevel    string
	DataDir     string
	Port        int
	DatabaseURL string
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

	cfg := Config{
		LogLevel: orDefault(getenv("LOG_LEVEL"), "info"),
		DataDir:  orDefault(getenv("DATA_DIR"), "./data"),
		Port:     3001,
	}

	if !slices.Contains(logLevels, cfg.LogLevel) {
		fail("LOG_LEVEL", "must be one of "+strings.Join(logLevels, ", "))
	}

	if raw := getenv("GATEWAY_PORT"); raw != "" {
		port, err := strconv.Atoi(raw)
		switch {
		case err != nil:
			fail("GATEWAY_PORT", "must be a number")
		case port < 1 || port > 65535:
			fail("GATEWAY_PORT", "must be between 1 and 65535")
		default:
			cfg.Port = port
		}
	}

	cfg.DatabaseURL = getenv("DATABASE_URL")
	switch u, err := url.Parse(cfg.DatabaseURL); {
	case cfg.DatabaseURL == "":
		fail("DATABASE_URL", "required")
	case err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "":
		fail("DATABASE_URL", "must be a postgres:// URL")
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
