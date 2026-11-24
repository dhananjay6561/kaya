// Package envfile loads a Kaya .env file into the process environment.
package envfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/joho/godotenv"
)

// Find walks up from dir looking for .env, stopping at the repo root (the folder with .git).
func Find(dir string) (string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(dir, ".env")
		if exists(candidate) {
			return candidate, nil
		}
		if exists(filepath.Join(dir, ".git")) {
			return "", nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil
		}
		dir = parent
	}
}

// Load loads KAYA_ENV_FILE, or the nearest .env, into the process environment.
// Variables already set are never overridden. It returns the loaded path, or "" if none was found.
func Load() (string, error) {
	path := os.Getenv("KAYA_ENV_FILE")
	if path == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		if path, err = Find(cwd); err != nil || path == "" {
			return "", err
		}
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if err := godotenv.Load(path); err != nil {
		return "", fmt.Errorf("load %s: %w", path, err)
	}
	return path, nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, fs.ErrNotExist)
}
