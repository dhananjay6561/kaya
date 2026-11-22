package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"

	"github.com/dhananjay6561/kaya/gateway/internal/logging"
	"github.com/dhananjay6561/kaya/gateway/internal/store"
	"github.com/dhananjay6561/kaya/gateway/internal/testdb"
)

type fakeDownloader struct {
	content []byte
	errs    []error
	calls   int
	seen    []string
	during  func()
}

func (f *fakeDownloader) DownloadMediaWithPathToFile(_ context.Context, directPath string, _, _, _ []byte, mediaType whatsmeow.MediaType, mms string, allowNoHash bool, file whatsmeow.File) error {
	f.calls++
	f.seen = append(f.seen, directPath+"|"+string(mediaType)+"|"+mms)
	if allowNoHash {
		return errors.New("the hash must always be checked")
	}
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return err
		}
	}
	if f.during != nil {
		f.during()
	}
	// whatsmeow writes the download with io.Copy, which uses the destination's ReadFrom when it has one.
	_, err := io.Copy(file, bytes.NewReader(f.content))
	return err
}

type env struct {
	t      *testing.T
	admin  *store.DB
	files  *Files
	worker *Worker
	logs   *bytes.Buffer
	nextID int
}

func newEnv(t *testing.T) *env {
	t.Helper()
	gateway, admin := testdb.MigratedPair(t, "kaya_gateway")
	files := NewFiles(t.TempDir())
	var logs bytes.Buffer
	w := NewWorker(gateway, files, logging.New(&logs, "info"), 1<<20, 60)
	w.minFree = 0
	return &env{t: t, admin: admin, files: files, worker: w, logs: &logs}
}

// addMedia stores a message with a media row; withKeys adds what a download needs.
func (e *env) addMedia(sha string, size int64, withKeys bool) int64 {
	e.t.Helper()
	e.nextID++
	ctx := context.Background()
	var id int64
	err := e.admin.Query(ctx, `
		WITH c AS (SELECT contact_id_for(NULL, '911111111111@s.whatsapp.net') AS id),
		ch AS (INSERT INTO chats (kind, jid) VALUES ('group', $1) ON CONFLICT (jid) DO UPDATE SET jid = EXCLUDED.jid RETURNING id),
		m AS (INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, source)
			SELECT ch.id, $2, c.id, false, now(), 'voice', 'live' FROM ch, c RETURNING id)
		INSERT INTO media (message_id, mime_type, size_bytes, sha256) SELECT id, 'audio/ogg', $3, NULLIF($4, '') FROM m RETURNING message_id`,
		[]any{"120363000000000000@g.us", fmt.Sprintf("MEDIA%d", e.nextID), size, sha}, &id)
	if err != nil {
		e.t.Fatal(err)
	}
	if withKeys {
		e.exec("INSERT INTO media_keys (message_id, media_key, direct_path, file_enc_sha256, media_type) VALUES ($1, '\\x01', '/v/t62/secret-path', '\\x02', 'audio')", id)
	}
	return id
}

func (e *env) exec(query string, args ...any) {
	e.t.Helper()
	err := e.admin.Query(context.Background(), "WITH x AS ("+query+" RETURNING 1) SELECT count(*) FROM x", args, new(int))
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) request(id int64) { e.exec("INSERT INTO media_requests (message_id) VALUES ($1)", id) }

func (e *env) state(id int64) string {
	e.t.Helper()
	var s string
	err := e.admin.Query(context.Background(), `
		SELECT m.state || '/' || coalesce(m.error_code, '-') || '/' ||
			CASE WHEN EXISTS (SELECT FROM media_requests r WHERE r.message_id = m.message_id) THEN 'queued' ELSE 'closed' END
		FROM media m WHERE message_id = $1`, []any{id}, &s)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func TestARequestedFileIsDownloadedOnce(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	dl := &fakeDownloader{content: []byte("the voice note")}
	first := e.addMedia(shaA, 14, true)
	same := e.addMedia(shaA, 14, true)
	unasked := e.addMedia(shaB, 5, true)

	if e.worker.Step(ctx, dl) || dl.calls != 0 {
		t.Fatal("nothing is downloaded unless it is asked for")
	}
	e.request(first)
	e.request(same)
	for range 2 {
		if !e.worker.Step(ctx, dl) {
			t.Fatal("want two requests handled")
		}
	}
	if e.worker.Step(ctx, dl) {
		t.Fatal("then nothing is left")
	}

	if dl.calls != 1 || dl.seen[0] != "/v/t62/secret-path|WhatsApp Audio Keys|audio" {
		t.Fatalf("a file that is already on disk is not downloaded again: %d calls, %v", dl.calls, dl.seen)
	}
	path, _ := e.files.Path(shaA)
	if got, err := os.ReadFile(path); err != nil || string(got) != "the voice note" { //nolint:gosec // test path
		t.Fatalf("stored file: %q, %v", got, err)
	}
	if e.state(first) != "stored/-/closed" || e.state(same) != "stored/-/closed" || e.state(unasked) != "remote/-/closed" {
		t.Fatalf("states: %s, %s, %s", e.state(first), e.state(same), e.state(unasked))
	}
	if strings.Contains(e.logs.String(), "secret-path") {
		t.Fatalf("the download path must never be logged: %s", e.logs)
	}
}

func TestOutcomes(t *testing.T) {
	transient := errors.New("read tcp: connection reset")
	for name, tc := range map[string]struct {
		size     int64
		withKeys bool
		content  string
		errs     []error
		steps    int
		want     string
		calls    int
	}{
		"gone from whatsapp (404)":     {size: 10, withKeys: true, errs: []error{whatsmeow.ErrMediaDownloadFailedWith404}, steps: 1, want: "expired/-/closed", calls: 1},
		"gone from whatsapp (410)":     {size: 10, withKeys: true, errs: []error{whatsmeow.ErrMediaDownloadFailedWith410}, steps: 1, want: "expired/-/closed", calls: 1},
		"refused (403) can be retried": {size: 10, withKeys: true, errs: []error{whatsmeow.ErrMediaDownloadFailedWith403}, steps: 1, want: "failed/http_403/closed", calls: 1},
		"wrong content":                {size: 10, withKeys: true, errs: []error{whatsmeow.ErrInvalidMediaSHA256}, steps: 1, want: "failed/bad_hash/closed", calls: 1},
		"no keys":                      {size: 10, withKeys: false, steps: 1, want: "failed/no_keys/closed", calls: 0},
		"declared too large":           {size: 2 << 20, withKeys: true, steps: 1, want: "failed/too_large/closed", calls: 0},
		"larger than declared":         {size: 10, withKeys: true, content: strings.Repeat("x", (1<<20)+cipherSlack+1), steps: 1, want: "failed/too_large/closed", calls: 1},
		"network error is retried":     {size: 10, withKeys: true, errs: []error{transient, nil}, content: "ok", steps: 2, want: "stored/-/closed", calls: 2},
		"gives up after five tries":    {size: 10, withKeys: true, errs: []error{transient, transient, transient, transient, transient}, steps: 5, want: "failed/download_error/closed", calls: 5},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			dl := &fakeDownloader{content: []byte(tc.content), errs: tc.errs}
			id := e.addMedia(shaA, tc.size, tc.withKeys)
			e.request(id)
			for range tc.steps {
				e.worker.Step(context.Background(), dl)
			}
			if got := e.state(id); got != tc.want || dl.calls != tc.calls {
				t.Fatalf("got %s after %d downloads, want %s after %d", got, dl.calls, tc.want, tc.calls)
			}
			if _, err := os.Stat(e.files.tempPath(id)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("no half-downloaded file may be left behind")
			}
			if stored := strings.HasPrefix(tc.want, "stored"); e.files.Exists(shaA) != stored {
				t.Fatalf("file on disk: %v, want %v", e.files.Exists(shaA), stored)
			}
		})
	}
}

func TestLimitsAndSweep(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	dl := &fakeDownloader{content: []byte("x")}
	e.worker.maxPerHour = 1
	now := time.Now()
	e.worker.now = func() time.Time { return now }
	a, b := e.addMedia(shaA, 1, true), e.addMedia(shaB, 1, true)
	e.request(a)
	e.request(b)

	e.worker.Step(ctx, dl)
	if e.worker.Step(ctx, dl) || e.state(b) != "remote/-/queued" || dl.calls != 1 {
		t.Fatalf("the hourly limit leaves the request queued: %s", e.state(b))
	}
	now = now.Add(61 * time.Minute)
	if !e.worker.Step(ctx, dl) || e.state(b) != "stored/-/closed" {
		t.Fatalf("after an hour the request is handled: %s", e.state(b))
	}

	low := e.addMedia(strings.Repeat("ef", 32), 1, true)
	e.request(low)
	e.worker.minFree = 1 << 62
	e.worker.Step(ctx, dl)
	if e.state(low) != "failed/low_disk/closed" {
		t.Fatalf("a nearly full disk refuses downloads: %s", e.state(low))
	}

	e.exec("DELETE FROM media WHERE message_id = $1", b)
	e.worker.Sweep(ctx)
	if !e.files.Exists(shaA) || e.files.Exists(shaB) {
		t.Fatal("the sweep removes the file whose row is gone and keeps the other")
	}
}

func TestFetch(t *testing.T) {
	e := newEnv(t)
	dl := &fakeDownloader{content: []byte("the voice note")}
	id := e.addMedia(shaA, 14, true)
	gone := e.addMedia(shaB, 14, true)
	e.exec("UPDATE media SET state = 'expired' WHERE message_id = $1", gone)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for ctx.Err() == nil {
			e.worker.Step(ctx, dl)
			time.Sleep(5 * time.Millisecond)
		}
	}()
	fetch := func(messageID int64) (string, error) {
		return Fetch(ctx, e.worker.db, e.files, messageID, 2*time.Second, 5*time.Millisecond)
	}

	path, err := fetch(id)
	want, _ := e.files.Path(shaA)
	if err != nil || path != want {
		t.Fatalf("got %q, %v", path, err)
	}
	if again, err := fetch(id); err != nil || again != want || dl.calls != 1 {
		t.Fatalf("a stored file is returned without another download: %q, %v, %d calls", again, err, dl.calls)
	}

	if err := os.Remove(want); err != nil {
		t.Fatal(err)
	}
	if path, err := fetch(id); err != nil || path != want || dl.calls != 2 {
		t.Fatalf("a stored row whose file is missing is downloaded again: %q, %v, %d calls", path, err, dl.calls)
	}

	if _, err := fetch(gone); err == nil || !strings.Contains(err.Error(), "no longer has") {
		t.Fatalf("an expired file is reported, got %v", err)
	}
	if _, err := fetch(999999); !errors.Is(err, ErrNoMedia) {
		t.Fatalf("got %v", err)
	}
	noKeys := e.addMedia("", 14, false)
	if _, err := fetch(noKeys); err == nil || !strings.Contains(err.Error(), "no_keys") {
		t.Fatalf("a failed download names its code, got %v", err)
	}

	cancel()
	idle := NewWorker(e.worker.db, e.files, logging.New(&bytes.Buffer{}, "info"), 1<<20, 60)
	waiting := e.addMedia(strings.Repeat("12", 32), 14, true)
	_, err = Fetch(context.Background(), idle.db, e.files, waiting, 30*time.Millisecond, 5*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("with no gateway working, fetch times out: %v", err)
	}
}

func TestTheCapHoldsHoweverTheDownloadIsWritten(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "capped")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	capped := &cappedFile{f: file, max: 1024}
	if _, ok := any(capped).(io.ReaderFrom); ok {
		t.Fatal("the capped file must not offer ReadFrom, or io.Copy skips the cap")
	}
	n, err := io.Copy(capped, bytes.NewReader(make([]byte, 8<<20)))
	if !errors.Is(err, errTooLarge) || n > 1024 {
		t.Fatalf("copied %d bytes, err %v", n, err)
	}
	if info, _ := file.Stat(); info.Size() > 1024 {
		t.Fatalf("%d bytes reached the disk past a 1024-byte cap", info.Size())
	}
	if _, err := capped.WriteAt([]byte("x"), 1024); !errors.Is(err, errTooLarge) {
		t.Fatalf("WriteAt past the cap: %v", err)
	}
	if err := capped.Truncate(2048); !errors.Is(err, errTooLarge) {
		t.Fatalf("Truncate past the cap: %v", err)
	}
}

func TestAFileWhoseMessageWasDeletedDuringTheDownloadIsNotKept(t *testing.T) {
	e := newEnv(t)
	id := e.addMedia(shaA, 5, true)
	e.request(id)
	dl := &fakeDownloader{content: []byte("photo")}
	dl.during = func() { e.exec("DELETE FROM media WHERE message_id = $1", id) }

	if !e.worker.Step(context.Background(), dl) {
		t.Fatal("the request is closed")
	}
	if e.files.Exists(shaA) {
		t.Fatal("the file of a message that is gone must not stay on disk")
	}
	if strings.Contains(e.logs.String(), "media stored") {
		t.Fatalf("nothing was stored: %s", e.logs)
	}
}
