package voice

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dhananjay6561/kaya/core/internal/pause"
	"github.com/dhananjay6561/kaya/kit/store"
	"github.com/dhananjay6561/kaya/kit/testdb"
)

var bg = context.Background()

func TestGroqSendsTheFileWithAHinglishHintAndReadsWhatWasSaid(t *testing.T) {
	var fields map[string]string
	var file string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/transcriptions" || r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "wrong", 400)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		fields = map[string]string{}
		for k, v := range r.MultipartForm.Value {
			fields[k] = v[0]
		}
		f, h, _ := r.FormFile("file")
		raw, _ := io.ReadAll(f)
		file = h.Filename + ":" + string(raw)
		_, _ = io.WriteString(w, `{"text":" kal 5 baje milte hain ","language":"hindi","duration":2.5}`)
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "abc")
	if err := os.WriteFile(path, []byte("OggS"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := NewGroq("k", "whisper-large-v3-turbo", srv.URL).Transcribe(bg, path)
	if err != nil || got != (Transcript{Text: "kal 5 baje milte hain", Language: "hindi", Duration: 2.5, Model: "whisper-large-v3-turbo"}) {
		t.Fatalf("got %+v, %v", got, err)
	}
	if fields["model"] != "whisper-large-v3-turbo" || fields["response_format"] != "verbose_json" || !strings.Contains(fields["prompt"], "Haan bhai") ||
		file != "voice.ogg:OggS" {
		t.Fatalf("fields %v, file %q", fields, file)
	}
}

func TestGroqSaysWhetherTryingAgainCanHelp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "abc")
	_ = os.WriteFile(path, []byte("x"), 0o600)
	for status, retry := range map[int]bool{429: true, 503: true, 400: false, 413: false} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		_, err := NewGroq("k", "m", srv.URL).Transcribe(bg, path)
		srv.Close()
		var failed *Error
		if !errors.As(err, &failed) || failed.Retry != retry {
			t.Errorf("HTTP %d: %v", status, err)
		}
	}
}

type fakeTranscriber struct {
	heard []string
	err   error
}

func (f *fakeTranscriber) Transcribe(_ context.Context, path string) (Transcript, error) {
	f.heard = append(f.heard, filepath.Base(path))
	if f.err != nil {
		return Transcript{}, f.err
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return Transcript{}, errors.New("cannot read the file")
	}
	return Transcript{Text: "kal 5 baje milte hain", Language: "hindi", Duration: 2, Model: "test"}, nil
}

type rig struct {
	t           *testing.T
	core, admin *store.DB
	media       string
	rahul, chat int64
	seq         int
	speech      *fakeTranscriber
	woken       int
	now         time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	core, admin := testdb.MigratedPair(t, "kaya_core")
	r := &rig{t: t, core: core, admin: admin, media: t.TempDir(), speech: &fakeTranscriber{}, now: time.Now()}
	r.one(&r.rahul, "SELECT contact_id_for('100000000000002@lid', '918888888888@s.whatsapp.net')")
	r.one(&r.chat, "INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", r.rahul)
	return r
}

func (r *rig) one(dst *int64, sql string, args ...any) {
	r.t.Helper()
	if err := r.admin.QueryRow(bg, sql, args...).Scan(dst); err != nil {
		r.t.Fatalf("%s: %v", sql, err)
	}
}

func (r *rig) exec(sql string, args ...any) {
	r.t.Helper()
	if _, err := r.admin.Exec(bg, sql, args...); err != nil {
		r.t.Fatalf("%s: %v", sql, err)
	}
}

// voice stores a voice note with its media row in the given state.
func (r *rig) voice(state string) int64 {
	r.t.Helper()
	r.seq++
	var id int64
	r.one(&id, `INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, source)
		VALUES ($1, $2, $3, false, now(), 'voice', 'live') RETURNING id`, r.chat, "3EB0V"+string(rune('A'+r.seq)), r.rahul)
	r.exec(`INSERT INTO media (message_id, mime_type, size_bytes) VALUES ($1, 'audio/ogg; codecs=opus', 4000)`, id)
	if state != "remote" {
		r.exec(`UPDATE media SET state = $2 WHERE message_id = $1`, id, state)
	}
	return id
}

// arrive stores a voice note's file as the gateway would.
func (r *rig) arrive(id int64) {
	r.t.Helper()
	sha := strings.Repeat(string(rune('a'+int(id%6))), 64)
	r.exec(`UPDATE media SET state = 'stored', sha256 = $2, stored_at = now() WHERE message_id = $1`, id, sha)
	if err := os.MkdirAll(filepath.Join(r.media, sha[:2]), 0o700); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.media, sha[:2], sha), []byte("OggS"), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) worker() *Worker {
	r.t.Helper()
	w, err := NewWorker(Options{DB: r.core, Transcriber: r.speech, Pause: &pause.Memory{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		MediaDir: r.media, Heard: func() { r.woken++ }, Now: func() time.Time { return r.now }})
	if err != nil {
		r.t.Fatal(err)
	}
	return w
}

func (r *rig) transcript(id int64) (state, text, errCode string) {
	r.t.Helper()
	var e *string
	err := r.admin.QueryRow(bg, `SELECT state, text, error FROM transcripts WHERE message_id = $1`, id).Scan(&state, &text, &e)
	if err != nil {
		return "", "", ""
	}
	if e != nil {
		errCode = *e
	}
	return state, text, errCode
}

func TestAVoiceNoteIsFetchedThenTranscribed(t *testing.T) {
	r := newRig(t)
	w := r.worker()
	id := r.voice("remote")
	if err := w.Step(bg); err != nil {
		t.Fatal(err)
	}
	var asked int64
	r.one(&asked, "SELECT count(*) FROM media_requests WHERE message_id = $1", id)
	if asked != 1 || len(r.speech.heard) != 0 {
		t.Fatalf("asked %d, heard %v", asked, r.speech.heard)
	}
	r.arrive(id)
	if err := w.Step(bg); err != nil {
		t.Fatal(err)
	}
	if state, text, _ := r.transcript(id); state != "done" || text != "kal 5 baje milte hain" || len(r.speech.heard) != 1 || r.woken != 0 {
		t.Fatalf("state %q text %q heard %v woken %d", state, text, r.speech.heard, r.woken)
	}
	if err := w.Step(bg); err != nil || len(r.speech.heard) != 1 {
		t.Fatalf("transcribed twice: %v %v", r.speech.heard, err)
	}
}

func TestTheOwnersVoiceCommandGoesFirstAndWakesTheInbox(t *testing.T) {
	r := newRig(t)
	w := r.worker()
	other := r.voice("remote")
	mine := r.voice("remote")
	r.exec(`INSERT INTO commands (message_id, wa_id, sent_at) SELECT id, wa_id, sent_at FROM messages WHERE id = $1`, mine)
	r.arrive(other)
	r.arrive(mine)
	r.speech.err = &Error{Code: "http_429", Retry: true}
	if err := w.Step(bg); err == nil {
		t.Fatal("a busy service was not reported")
	}
	if len(r.speech.heard) != 1 {
		t.Fatalf("heard %v", r.speech.heard)
	}
	var shaOfMine string
	if err := r.admin.QueryRow(bg, "SELECT sha256 FROM media WHERE message_id = $1", mine).Scan(&shaOfMine); err != nil || r.speech.heard[0] != shaOfMine {
		t.Fatalf("the command was not first: heard %v, mine %s", r.speech.heard, shaOfMine)
	}
	r.speech.err = nil
	if err := w.Step(bg); err != nil {
		t.Fatal(err)
	}
	if state, _, _ := r.transcript(mine); state != "done" || r.woken != 1 {
		t.Fatalf("state %q woken %d", state, r.woken)
	}
}

func TestAVoiceNoteWhoseFileWillNotComeIsPassedOver(t *testing.T) {
	r := newRig(t)
	w := r.worker()
	gone := r.voice("expired")
	late := r.voice("remote")
	refused := r.voice("remote")
	r.arrive(refused)
	r.speech.err = &Error{Code: "http_400"}
	if err := w.Step(bg); err != nil {
		t.Fatal(err)
	}
	if state, _, _ := r.transcript(late); state != "" {
		t.Fatalf("a file just asked for was given up: %s", state)
	}
	r.now = r.now.Add(11 * time.Minute)
	if err := w.Step(bg); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[int64]string{gone: "file_expired", late: "file_late", refused: "http_400"} {
		if state, _, code := r.transcript(id); state != "failed" || code != want {
			t.Errorf("message %d: %s %s, want failed %s", id, state, code, want)
		}
	}
}

func TestSilenceAndTheHintAreNeverTakenForSpeech(t *testing.T) {
	path := filepath.Join(t.TempDir(), "abc")
	_ = os.WriteFile(path, []byte("x"), 0o600)
	for body, want := range map[string]string{
		`{"text":"Haan bhai, kal milte hain.","segments":[{"text":"Haan bhai, kal milte hain.","no_speech_prob":0.1,"avg_logprob":-0.2}]}`:                                       "no_speech",
		`{"text":"Thank you.","segments":[{"text":"Thank you.","no_speech_prob":0.9,"avg_logprob":-1.2}]}`:                                                                       "no_speech",
		`{"text":"call me. Thank you.","segments":[{"text":"call me.","no_speech_prob":0.1,"avg_logprob":-0.3},{"text":"Thank you.","no_speech_prob":0.95,"avg_logprob":-1.5}]}`: "call me.",
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
		got, err := NewGroq("k", "m", srv.URL).Transcribe(bg, path)
		srv.Close()
		var failed *Error
		switch {
		case want == "no_speech" && (!errors.As(err, &failed) || failed.Code != "no_speech" || failed.Retry):
			t.Errorf("%s: got %q, %v", body, got.Text, err)
		case want != "no_speech" && (err != nil || got.Text != want):
			t.Errorf("%s: got %q, %v", body, got.Text, err)
		}
	}
}

func TestAVoiceNoteThatKeepsFailingIsGivenUpWithoutHoldingUpTheRest(t *testing.T) {
	r := newRig(t)
	w := r.worker()
	stuck := r.voice("remote")
	fine := r.voice("remote")
	r.arrive(stuck)
	r.arrive(fine)
	var shaOfStuck string
	if err := r.admin.QueryRow(bg, "SELECT sha256 FROM media WHERE message_id = $1", stuck).Scan(&shaOfStuck); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(r.media, shaOfStuck[:2], shaOfStuck)); err != nil {
		t.Fatal(err)
	}
	_ = os.Mkdir(filepath.Join(r.media, shaOfStuck[:2], shaOfStuck), 0o700)
	for range 5 {
		if err := w.Step(bg); err != nil {
			t.Fatal(err)
		}
	}
	if state, _, code := r.transcript(stuck); state != "failed" || code != "gave_up" {
		t.Fatalf("stuck: %s %s", state, code)
	}
	if state, _, _ := r.transcript(fine); state != "done" {
		t.Fatalf("the next voice note waited behind a bad one: %s", state)
	}
}
