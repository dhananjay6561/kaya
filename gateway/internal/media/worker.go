package media

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"go.mau.fi/whatsmeow"
)

// Media states, as stored in media.state.
const (
	StateRemote  = "remote"
	StateStored  = "stored"
	StateExpired = "expired"
	StateFailed  = "failed"
)

const (
	maxAttempts  = 5
	minFreeBytes = 2 << 30
	// cipherSlack is how much larger than the file itself the encrypted download may be.
	cipherSlack = 64 << 10
)

var errTooLarge = errors.New("file is larger than the limit")

// DB runs a function in one transaction.
type DB interface {
	InTx(ctx context.Context, fn func(pgx.Tx) error) error
}

// Downloader fetches and decrypts a file from WhatsApp into file. whatsmeow's client implements it.
type Downloader interface {
	DownloadMediaWithPathToFile(ctx context.Context, directPath string, encFileHash, fileHash, mediaKey []byte,
		mediaType whatsmeow.MediaType, mmsType string, allowNoHash bool, file whatsmeow.File) error
}

var mediaTypes = map[string]struct {
	keys whatsmeow.MediaType
	mms  string
}{
	"image":    {whatsmeow.MediaImage, "image"},
	"video":    {whatsmeow.MediaVideo, "video"},
	"audio":    {whatsmeow.MediaAudio, "audio"},
	"document": {whatsmeow.MediaDocument, "document"},
}

// Worker downloads the files that media_requests asks for, one at a time. It never downloads
// anything nobody asked for.
type Worker struct {
	db     DB
	files  *Files
	logger *slog.Logger

	maxBytes   int64
	maxPerHour int
	minFree    float64
	now        func() time.Time
	recent     []time.Time
}

// NewWorker returns a worker that refuses files over maxBytes and stops at maxPerHour downloads.
func NewWorker(db DB, files *Files, logger *slog.Logger, maxBytes int64, maxPerHour int) *Worker {
	return &Worker{
		db: db, files: files, logger: logger,
		maxBytes: maxBytes, maxPerHour: maxPerHour, minFree: minFreeBytes, now: time.Now,
	}
}

type request struct {
	messageID  int64
	state      string
	sha256     *string
	size       *int64
	key        []byte
	directPath *string
	encSHA256  []byte
	mediaType  *string
}

func (w *Worker) next(ctx context.Context) (*request, error) {
	var r request
	err := w.db.InTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT r.message_id, m.state, m.sha256, m.size_bytes, k.media_key, k.direct_path, k.file_enc_sha256, k.media_type
			FROM media_requests r JOIN media m USING (message_id) LEFT JOIN media_keys k USING (message_id)
			ORDER BY r.requested_at, r.message_id LIMIT 1`).
			Scan(&r.messageID, &r.state, &r.sha256, &r.size, &r.key, &r.directPath, &r.encSHA256, &r.mediaType)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// resolve records the outcome on the media row and removes the request, in one transaction. found
// is false when the row is gone, which happens when the message was deleted for everyone while its
// file was downloading; unused then says whether any stored message still has that file.
func (w *Worker) resolve(ctx context.Context, messageID int64, state, code, sha256 string) (found, unused bool, err error) {
	err = w.db.InTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE media SET state = $2, error_code = NULLIF($3, ''), stored_at = CASE WHEN $2 = 'stored' THEN now() END
			WHERE message_id = $1`, messageID, state, code)
		if err != nil {
			return err
		}
		found = tag.RowsAffected() > 0
		if !found && sha256 != "" {
			var used bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT FROM media WHERE sha256 = $1 AND state = 'stored')", sha256).Scan(&used); err != nil {
				return err
			}
			unused = !used
		}
		_, err = tx.Exec(ctx, "DELETE FROM media_requests WHERE message_id = $1", messageID)
		return err
	})
	return found, unused, err
}

// Step handles the oldest request and reports whether to go straight on to the next. It returns
// false when there is nothing to do, the hourly limit is reached, or a download failed in a way
// worth retrying later.
func (w *Worker) Step(ctx context.Context, dl Downloader) bool {
	r, err := w.next(ctx)
	if err != nil {
		w.logger.Warn("could not read the media queue", "error_code", "db")
		return false
	}
	if r == nil {
		return false
	}
	state, code, retry := w.handle(ctx, dl, r)
	if retry {
		return w.countAttempt(ctx, r.messageID)
	}
	if state == "" {
		return false
	}
	found, unused, err := w.resolve(ctx, r.messageID, state, code, deref(r.sha256))
	if err != nil {
		w.logger.Warn("could not record a media download", "error_code", "db")
		return false
	}
	if !found {
		if state == StateStored && unused {
			_ = w.files.Remove(deref(r.sha256))
		}
		w.logger.Info("media request dropped: its message is gone", "message_id", r.messageID)
		return true
	}
	if state == StateStored {
		w.logger.Info("media stored", "message_id", r.messageID)
	} else {
		w.logger.Warn("media not stored", "message_id", r.messageID, "state", state, "error_code", code)
	}
	return true
}

// handle works out one request. An empty state with retry false means "leave it queued for now".
func (w *Worker) handle(ctx context.Context, dl Downloader, r *request) (state, code string, retry bool) {
	if r.state == StateExpired {
		return StateExpired, "", false
	}
	kind, known := mediaTypes[deref(r.mediaType)]
	if r.sha256 == nil || len(r.key) == 0 || r.directPath == nil || len(r.encSHA256) == 0 || !known {
		return StateFailed, "no_keys", false
	}
	hash, err := hex.DecodeString(*r.sha256)
	if err != nil {
		return StateFailed, "no_keys", false
	}
	if w.files.Exists(*r.sha256) {
		return StateStored, "", false
	}
	if r.size != nil && *r.size > w.maxBytes {
		return StateFailed, "too_large", false
	}
	if free, err := w.files.FreeBytes(); err != nil || free < w.minFree {
		return StateFailed, "low_disk", false
	}
	if !w.allow() {
		w.logger.Warn("hourly media download limit reached; requests wait", "limit", w.maxPerHour)
		return "", "", false
	}

	file, err := w.files.createTemp(r.messageID)
	if err != nil {
		return StateFailed, "disk", false
	}
	capped := &cappedFile{f: file, max: w.maxBytes + cipherSlack}
	err = dl.DownloadMediaWithPathToFile(ctx, *r.directPath, r.encSHA256, hash, r.key, kind.keys, kind.mms, false, capped)
	if err == nil {
		err = file.Sync()
	}
	_ = file.Close()
	if err == nil {
		if err = w.files.commit(r.messageID, *r.sha256); err == nil {
			return StateStored, "", false
		}
		err = fmt.Errorf("disk: %w", err)
	}
	w.files.discardTemp(r.messageID)

	switch {
	case ctx.Err() != nil:
		return "", "", false
	case errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith404), errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith410):
		return StateExpired, "", false
	case errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith403):
		return StateFailed, "http_403", false
	case errors.Is(err, whatsmeow.ErrInvalidMediaSHA256), errors.Is(err, whatsmeow.ErrInvalidMediaEncSHA256),
		errors.Is(err, whatsmeow.ErrInvalidMediaHMAC):
		return StateFailed, "bad_hash", false
	case errors.Is(err, errTooLarge):
		return StateFailed, "too_large", false
	}
	return "", "", true
}

// countAttempt records a failed try. After too many the request is closed as failed.
func (w *Worker) countAttempt(ctx context.Context, messageID int64) bool {
	var attempts int
	err := w.db.InTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "UPDATE media_requests SET attempts = attempts + 1 WHERE message_id = $1 RETURNING attempts", messageID).Scan(&attempts)
	})
	if err != nil || attempts < maxAttempts {
		w.logger.Warn("media download failed, trying again later", "message_id", messageID, "attempts", attempts)
		return false
	}
	if _, _, err := w.resolve(ctx, messageID, StateFailed, "download_error", ""); err != nil {
		return false
	}
	w.logger.Warn("media not stored", "message_id", messageID, "state", StateFailed, "error_code", "download_error")
	return true
}

func (w *Worker) allow() bool {
	now := w.now()
	kept := w.recent[:0]
	for _, t := range w.recent {
		if now.Sub(t) < time.Hour {
			kept = append(kept, t)
		}
	}
	w.recent = kept
	if len(w.recent) >= w.maxPerHour {
		return false
	}
	w.recent = append(w.recent, now)
	return true
}

// Sweep clears unfinished downloads and files no stored row refers to.
func (w *Worker) Sweep(ctx context.Context) {
	removed, err := w.files.Sweep(func(sha256 string) (bool, error) {
		var used bool
		err := w.db.InTx(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT EXISTS (SELECT FROM media WHERE sha256 = $1 AND state = 'stored')", sha256).Scan(&used)
		})
		return used, err
	})
	if err != nil {
		w.logger.Warn("could not finish sweeping media files", "error_code", "sweep")
		return
	}
	if removed > 0 {
		w.logger.Info("removed media files no message refers to", "files", removed)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// cappedFile refuses to grow past max, so a sender cannot fill the disk by lying about a size. It
// wraps the file instead of embedding it: an embedded *os.File would lend out ReadFrom, which
// io.Copy prefers over Write, and the cap would never be checked.
type cappedFile struct {
	f   *os.File
	max int64
}

func (c *cappedFile) Read(p []byte) (int, error)                   { return c.f.Read(p) }
func (c *cappedFile) ReadAt(p []byte, off int64) (int, error)      { return c.f.ReadAt(p, off) }
func (c *cappedFile) Seek(offset int64, whence int) (int64, error) { return c.f.Seek(offset, whence) }
func (c *cappedFile) Stat() (os.FileInfo, error)                   { return c.f.Stat() }

func (c *cappedFile) Write(p []byte) (int, error) {
	offset, err := c.f.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	if offset+int64(len(p)) > c.max {
		return 0, errTooLarge
	}
	return c.f.Write(p)
}

func (c *cappedFile) WriteAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > c.max {
		return 0, errTooLarge
	}
	return c.f.WriteAt(p, off)
}

func (c *cappedFile) Truncate(size int64) error {
	if size > c.max {
		return errTooLarge
	}
	return c.f.Truncate(size)
}

var _ whatsmeow.File = (*cappedFile)(nil)
