package voice

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dhananjay6561/kaya/core/internal/pause"
	"github.com/dhananjay6561/kaya/kit/logging"
	"github.com/dhananjay6561/kaya/kit/store"
)

// Options configure a Worker. Zero values take the defaults noted.
type Options struct {
	DB          *store.DB
	Transcriber Transcriber
	Pause       pause.Switch
	Logger      *slog.Logger
	// MediaDir is where the gateway keeps downloaded files, by their sha256.
	MediaDir string
	// Heard is called when a voice note DJ sent Kaya has its transcript, so the command waiting
	// for it runs at once; nil does nothing.
	Heard func()
	// MaxBytes is the largest file sent to be transcribed (25 MB); Wait is how long a file may take
	// to arrive before the voice note is given up on (10 minutes).
	MaxBytes int64
	Wait     time.Duration
	// Poll is how often new voice notes are looked for (3 s); RetryMax bounds the wait after a
	// failure (5 minutes).
	Poll     time.Duration
	RetryMax time.Duration
	Now      func() time.Time
}

const workerLock = 5011609

// Worker transcribes new voice notes: those DJ sends Kaya first, then everyone else's.
type Worker struct {
	o    Options
	lock *store.Lock

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	running bool
}

// NewWorker checks the options and returns a stopped worker.
func NewWorker(o Options) (*Worker, error) {
	if o.DB == nil || o.Transcriber == nil || o.Pause == nil || o.Logger == nil || o.MediaDir == "" {
		return nil, errors.New("voice needs a database, a transcriber, the pause switch, a logger and the media folder")
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = 25 << 20
	}
	if o.Wait == 0 {
		o.Wait = 10 * time.Minute
	}
	if o.Poll == 0 {
		o.Poll = 3 * time.Second
	}
	if o.RetryMax == 0 {
		o.RetryMax = 5 * time.Minute
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Worker{o: o, lock: store.NewLock(o.DB, workerLock)}, nil
}

// Start runs the worker until Stop.
func (w *Worker) Start() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.running {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	w.cancel, w.done, w.running = cancel, done, true
	go func() {
		defer close(done)
		w.run(ctx)
	}()
}

// Stop ends the worker and waits for it.
func (w *Worker) Stop() {
	w.mu.Lock()
	cancel, done := w.cancel, w.done
	w.running = false
	w.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

func sleep(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func (w *Worker) run(ctx context.Context) {
	defer w.lock.Release()
	delay := w.o.Poll
	failing := false
	for ctx.Err() == nil {
		err := w.lock.Hold(ctx)
		if err == nil {
			err = w.Step(ctx)
		}
		switch {
		case ctx.Err() != nil:
			return
		case err == nil:
			if failing {
				w.o.Logger.Info("transcribing works again")
			}
			failing, delay = false, w.o.Poll
			sleep(ctx, w.o.Poll)
		case errors.Is(err, store.ErrLockBusy):
			sleep(ctx, 30*time.Second)
		default:
			if !failing {
				w.o.Logger.Warn("transcribing failed; trying again", logging.Err(err))
			}
			failing = true
			sleep(ctx, delay)
			delay = min(delay*2, w.o.RetryMax)
		}
	}
}

type note struct {
	id       int64
	command  bool
	state    string
	sha      *string
	size     *int64
	received time.Time
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Step transcribes every new voice note whose file is in, asks the gateway for the files of the
// rest, and passes over those whose file will not come.
func (w *Worker) Step(ctx context.Context) error {
	st, err := w.o.Pause.State(ctx)
	if err != nil || st.Paused {
		return err
	}
	var notes []note
	err = w.o.DB.InTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT m.id, EXISTS (SELECT 1 FROM commands c WHERE c.message_id = m.id),
				coalesce(md.state, 'none'), md.sha256, md.size_bytes, m.created_at
			FROM messages m LEFT JOIN media md ON md.message_id = m.id
			WHERE m.kind = 'voice' AND m.source = 'live' AND m.deleted_at IS NULL
			  AND m.id > (SELECT started_after FROM transcribing)
			  AND NOT EXISTS (SELECT 1 FROM transcripts t WHERE t.message_id = m.id)
			ORDER BY 2 DESC, m.id LIMIT 20`)
		if err != nil {
			return err
		}
		notes, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (note, error) {
			var n note
			err := row.Scan(&n.id, &n.command, &n.state, &n.sha, &n.size, &n.received)
			return n, err
		})
		return err
	})
	if err != nil {
		return err
	}
	for _, n := range notes {
		if err := w.handle(ctx, n); err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) handle(ctx context.Context, n note) error {
	switch n.state {
	case "stored":
	case "failed", "expired", "none":
		return w.failed(ctx, n, "file_"+n.state)
	default:
		if w.o.Now().Sub(n.received) > w.o.Wait {
			return w.failed(ctx, n, "file_late")
		}
		_, err := w.o.DB.Exec(ctx, `INSERT INTO media_requests (message_id) VALUES ($1) ON CONFLICT DO NOTHING`, n.id)
		return err
	}
	switch {
	case n.sha == nil || !sha256Hex.MatchString(*n.sha):
		return w.failed(ctx, n, "no_file")
	case n.size != nil && *n.size > w.o.MaxBytes:
		return w.failed(ctx, n, "too_large")
	}
	path := filepath.Join(w.o.MediaDir, (*n.sha)[:2], *n.sha)
	if _, err := os.Stat(path); err != nil {
		if w.o.Now().Sub(n.received) > w.o.Wait {
			return w.failed(ctx, n, "no_file")
		}
		return nil
	}
	started := time.Now()
	t, err := w.o.Transcriber.Transcribe(ctx, path)
	var failed *Error
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return context.Cause(ctx)
	case errors.As(err, &failed) && !failed.Retry:
		return w.failed(ctx, n, failed.Code)
	default:
		return err
	}
	if _, err := w.o.DB.Exec(ctx, `INSERT INTO transcripts (message_id, state, text, language, duration_s, model)
		VALUES ($1, 'done', $2, $3, $4, $5) ON CONFLICT (message_id) DO NOTHING`,
		n.id, t.Text, t.Language, t.Duration, t.Model); err != nil {
		return err
	}
	w.o.Logger.Info("a voice note was transcribed", "message", n.id, "command", n.command, "language", t.Language,
		"seconds", t.Duration, "ms", time.Since(started).Milliseconds())
	if n.command && w.o.Heard != nil {
		w.o.Heard()
	}
	return nil
}

func (w *Worker) failed(ctx context.Context, n note, code string) error {
	w.o.Logger.Warn("a voice note could not be transcribed", "message", n.id, "error_code", code)
	_, err := w.o.DB.Exec(ctx, `INSERT INTO transcripts (message_id, state, error) VALUES ($1, 'failed', $2)
		ON CONFLICT (message_id) DO NOTHING`, n.id, code)
	if err == nil && n.command && w.o.Heard != nil {
		w.o.Heard()
	}
	return err
}
