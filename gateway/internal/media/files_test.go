package media

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var (
	shaA = strings.Repeat("ab", 32)
	shaB = strings.Repeat("cd", 32)
)

func writeStored(t *testing.T, f *Files, messageID int64, sha, content string) {
	t.Helper()
	tmp, err := f.createTemp(messageID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.WriteString(content); err != nil {
		t.Fatal(err)
	}
	_ = tmp.Close()
	if err := f.commit(messageID, sha); err != nil {
		t.Fatal(err)
	}
}

func TestPathsComeOnlyFromValidHashes(t *testing.T) {
	dir := t.TempDir()
	f := NewFiles(dir)
	path, err := f.Path(shaA)
	if err != nil || path != filepath.Join(dir, "media", "ab", shaA) {
		t.Fatalf("got %q, %v", path, err)
	}
	for _, bad := range []string{"", "../../etc/passwd", strings.ToUpper(shaA), shaA + "0", shaA[:63] + "/", "deck.pdf"} {
		if _, err := f.Path(bad); !errors.Is(err, ErrBadHash) {
			t.Fatalf("%q must be refused, got %v", bad, err)
		}
		if f.Exists(bad) || !errors.Is(f.Remove(bad), ErrBadHash) {
			t.Fatalf("%q must never reach the disk", bad)
		}
	}
}

func TestStoreRemoveAndSweep(t *testing.T) {
	f := NewFiles(t.TempDir())
	writeStored(t, f, 1, shaA, "voice note")
	writeStored(t, f, 2, shaB, "photo")
	if !f.Exists(shaA) || !f.Exists(shaB) {
		t.Fatal("committed files must exist")
	}
	path, _ := f.Path(shaA)
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("files are readable by the owner and group only: %v %v", info, err)
	}
	if info, _ := os.Stat(filepath.Dir(path)); info.Mode().Perm() != 0o750 {
		t.Fatalf("folders: %v", info.Mode().Perm())
	}

	half, err := f.createTemp(3)
	if err != nil {
		t.Fatal(err)
	}
	_ = half.Close()
	stray := filepath.Join(filepath.Dir(path), "notes.txt")
	if err := os.WriteFile(stray, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	removed, err := f.Sweep(func(sha string) (bool, error) { return sha == shaA, nil })
	if err != nil || removed != 1 {
		t.Fatalf("want the one unused file removed, got %d, %v", removed, err)
	}
	if !f.Exists(shaA) || f.Exists(shaB) {
		t.Fatal("the sweep keeps used files and removes unused ones")
	}
	if _, err := os.Stat(f.tempPath(3)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unfinished downloads are cleared")
	}
	if _, err := os.Stat(stray); err != nil {
		t.Fatal("the sweep only touches files it named itself")
	}

	if err := f.Remove(shaA); err != nil || f.Exists(shaA) {
		t.Fatal("remove deletes the file")
	}
	if err := f.Remove(shaA); err != nil {
		t.Fatalf("removing a file that is gone is fine, got %v", err)
	}
	if free, err := f.FreeBytes(); err != nil || free == 0 {
		t.Fatalf("free space: %v, %v", free, err)
	}
}
