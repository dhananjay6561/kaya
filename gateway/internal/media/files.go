// Package media downloads message files on request and keeps them on disk, named by their hash.
package media

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
)

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ErrBadHash means a hash is not 64 lowercase hex characters, so it cannot name a file.
var ErrBadHash = errors.New("not a sha-256 in hex")

// Files is the on-disk store: <dir>/<first two characters>/<sha256>, with downloads in progress
// under <dir>/tmp. Paths are built from validated hashes only, never from anything a sender chose.
type Files struct {
	dir string
}

// NewFiles keeps files under dataDir/media.
func NewFiles(dataDir string) *Files {
	return &Files{dir: filepath.Join(dataDir, "media")}
}

// Path returns where the file with this hash is, or would be, stored.
func (f *Files) Path(sha256 string) (string, error) {
	if !sha256Hex.MatchString(sha256) {
		return "", ErrBadHash
	}
	return filepath.Join(f.dir, sha256[:2], sha256), nil
}

// Exists reports whether the file with this hash is stored.
func (f *Files) Exists(sha256 string) bool {
	path, err := f.Path(sha256)
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func (f *Files) tempPath(messageID int64) string {
	return filepath.Join(f.dir, "tmp", strconv.FormatInt(messageID, 10)+".part")
}

// createTemp opens an empty file for a download in progress.
func (f *Files) createTemp(messageID int64) (*os.File, error) {
	path := f.tempPath(messageID)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create media folder: %w", err)
	}
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o640) //nolint:gosec // path is built from a number
}

// commit moves a finished download into place under its hash.
func (f *Files) commit(messageID int64, sha256 string) error {
	final, err := f.Path(sha256)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o750); err != nil {
		return fmt.Errorf("create media folder: %w", err)
	}
	return os.Rename(f.tempPath(messageID), final)
}

func (f *Files) discardTemp(messageID int64) {
	_ = os.Remove(f.tempPath(messageID))
}

// Remove deletes the file with this hash. A file that is already gone is not an error.
func (f *Files) Remove(sha256 string) error {
	path, err := f.Path(sha256)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// FreeBytes reports roughly how much space is left where the files are kept. The block size has a
// different integer type on each platform, so the sum is done in floating point.
func (f *Files) FreeBytes() (float64, error) {
	if err := os.MkdirAll(f.dir, 0o750); err != nil {
		return 0, err
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(f.dir, &stat); err != nil {
		return 0, err
	}
	return float64(stat.Bavail) * float64(stat.Bsize), nil
}

// Sweep clears downloads that never finished and deletes stored files that keep reports as unused,
// which covers a crash between a database change and its file change. It returns how many files it
// removed.
func (f *Files) Sweep(keep func(sha256 string) (bool, error)) (int, error) {
	if err := os.RemoveAll(filepath.Join(f.dir, "tmp")); err != nil {
		return 0, err
	}
	removed := 0
	shards, err := os.ReadDir(f.dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	for _, shard := range shards {
		if !shard.IsDir() || len(shard.Name()) != 2 {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(f.dir, shard.Name()))
		if err != nil {
			return removed, err
		}
		for _, entry := range entries {
			name := entry.Name()
			if !sha256Hex.MatchString(name) || name[:2] != shard.Name() {
				continue
			}
			used, err := keep(name)
			if err != nil {
				return removed, err
			}
			if !used && f.Remove(name) == nil {
				removed++
			}
		}
	}
	return removed, nil
}
