package config

import (
	"os"
	"path/filepath"
	"testing"
)

func makeRepo(t *testing.T, withEnv bool) string {
	t.Helper()
	repo := t.TempDir()
	for _, dir := range []string{".git", filepath.Join("gateway", "cmd")} {
		if err := os.MkdirAll(filepath.Join(repo, dir), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if withEnv {
		writeFile(t, filepath.Join(repo, ".env"), "KAYA_TEST_VALUE=from-file\n")
	}
	return repo
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFindEnvFileInParent(t *testing.T) {
	repo := makeRepo(t, true)
	got, err := FindEnvFile(filepath.Join(repo, "gateway", "cmd"))
	if err != nil || got != filepath.Join(repo, ".env") {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestFindEnvFileStopsAtRepoRoot(t *testing.T) {
	outer := t.TempDir()
	writeFile(t, filepath.Join(outer, ".env"), "OUTSIDE=1\n")
	repo := filepath.Join(outer, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	got, err := FindEnvFile(repo)
	if err != nil || got != "" {
		t.Fatalf("got %q, %v; want nothing", got, err)
	}
}

func TestLoadEnvFileKeepsRealVariables(t *testing.T) {
	file := filepath.Join(t.TempDir(), "custom.env")
	writeFile(t, file, "KAYA_TEST_A=from-file\nKAYA_TEST_B=from-file\n")
	t.Setenv("KAYA_ENV_FILE", file)
	t.Setenv("KAYA_TEST_A", "")
	t.Setenv("KAYA_TEST_B", "from-env")
	if err := os.Unsetenv("KAYA_TEST_A"); err != nil {
		t.Fatal(err)
	}

	got, err := LoadEnvFile()
	if err != nil || got != file {
		t.Fatalf("got %q, %v", got, err)
	}
	if v := os.Getenv("KAYA_TEST_A"); v != "from-file" {
		t.Errorf("KAYA_TEST_A = %q, want from-file", v)
	}
	if v := os.Getenv("KAYA_TEST_B"); v != "from-env" {
		t.Errorf("KAYA_TEST_B = %q, want from-env", v)
	}
}

func TestLoadEnvFileMissingExplicitFile(t *testing.T) {
	t.Setenv("KAYA_ENV_FILE", filepath.Join(t.TempDir(), "nope.env"))
	if _, err := LoadEnvFile(); err == nil {
		t.Fatal("expected an error for a missing KAYA_ENV_FILE")
	}
}
