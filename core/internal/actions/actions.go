// Package actions keeps what Kaya would do outside DJ's own chat until he says yes: a proposal is
// stored as a pending action, his next yes or no decides it, and only a yes runs it, once.
package actions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/dhananjay6561/kaya/core/internal/text"
	"github.com/dhananjay6561/kaya/kit/logging"
	"github.com/dhananjay6561/kaya/kit/store"
)

// Proposal is an action a tool wants to take: what kind, the chat it writes to, what DJ is asked
// to approve, and what running it needs.
type Proposal struct {
	Kind      string
	TargetJID string
	Summary   string
	Payload   json.RawMessage
}

// Action is a stored proposal and where it stands.
type Action struct {
	Proposal
	ID        string
	State     string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// States of an action.
const (
	Draft      = "draft"
	Pending    = "pending"
	Confirmed  = "confirmed"
	Declined   = "declined"
	Expired    = "expired"
	Superseded = "superseded"
	Done       = "done"
	Failed     = "failed"
)

// Store keeps pending actions.
type Store struct {
	db  *store.DB
	now func() time.Time
	ttl time.Duration
}

// NewStore returns actions on db that wait ttl for DJ's answer; nil now means time.Now.
func NewStore(db *store.DB, now func() time.Time, ttl time.Duration) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{db: db, now: now, ttl: ttl}
}

// TTL is how long an action waits for DJ's answer.
func (s *Store) TTL() time.Duration { return s.ttl }

const columns = `id::text, kind, target_jid, summary, payload, state, created_at, expires_at`

func scan(row pgx.Row) (Action, error) {
	var a Action
	err := row.Scan(&a.ID, &a.Kind, &a.TargetJID, &a.Summary, &a.Payload, &a.State, &a.CreatedAt, &a.ExpiresAt)
	return a, err
}

// Propose stores p as a draft: nothing can decide it until Shown says the message asking DJ
// reached his chat, so a yes never answers a question he did not see.
func (s *Store) Propose(ctx context.Context, p Proposal) (Action, error) {
	now := s.now()
	return scan(s.db.QueryRow(ctx, `INSERT INTO pending_actions (kind, target_jid, summary, payload, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING `+columns, p.Kind, p.TargetJID, p.Summary, p.Payload, now, now.Add(s.ttl)))
}

// Shown makes the draft id the one action waiting for DJ, once the message asking him, waID, was
// sent at at; any other still waiting is superseded, so a yes only ever answers the newest question.
// False means the draft was no longer one.
func (s *Store) Shown(ctx context.Context, id, waID string, at time.Time) (bool, error) {
	var shown bool
	err := s.db.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "UPDATE pending_actions SET state = 'superseded' WHERE state = 'pending' AND id <> $1::uuid", id); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE pending_actions SET state = 'pending', shown_at = $3, shown_wa_id = $2,
			expires_at = least(greatest(expires_at, $4::timestamptz), created_at + interval '1 hour')
			WHERE id = $1::uuid AND state = 'draft'`, id, waID, at, s.now().Add(s.ttl))
		shown = tag.RowsAffected() == 1
		return err
	})
	return shown, err
}

// Open is the action waiting for DJ's answer, or nil; one past its time is marked expired first.
func (s *Store) Open(ctx context.Context) (*Action, error) {
	now := s.now()
	if _, err := s.db.Exec(ctx, `UPDATE pending_actions SET state = 'expired'
		WHERE state = 'pending' AND expires_at <= $1 OR state = 'draft' AND created_at < $2`, now, now.Add(-time.Hour)); err != nil {
		return nil, err
	}
	a, err := scan(s.db.QueryRow(ctx, `SELECT `+columns+` FROM pending_actions WHERE state = 'pending' ORDER BY created_at DESC LIMIT 1`))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &a, err
}

// DecidedBy is the action DJ's message decided, or nil.
func (s *Store) DecidedBy(ctx context.Context, messageID int64) (*Action, string, error) {
	var result string
	var a Action
	err := s.db.QueryRow(ctx, `SELECT `+columns+`, coalesce(result, '') FROM pending_actions WHERE decided_by = $1`, messageID).
		Scan(&a.ID, &a.Kind, &a.TargetJID, &a.Summary, &a.Payload, &a.State, &a.CreatedAt, &a.ExpiresAt, &result)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", nil
	}
	return &a, result, err
}

// Decide records DJ's answer to the waiting action a, given by his message; false means it does
// not answer it: the action was no longer waiting, or his message came before the question or
// after another message from Kaya, which it more likely answers.
func (s *Store) Decide(ctx context.Context, a Action, messageID int64, yes bool) (bool, error) {
	state := Declined
	if yes {
		state = Confirmed
	}
	n, err := s.db.Exec(ctx, `UPDATE pending_actions pa SET state = $2, decided_by = $3, decided_at = now()
		FROM messages y
		WHERE pa.id = $1::uuid AND pa.state = 'pending' AND pa.expires_at > $4 AND y.id = $3 AND y.sent_at > pa.shown_at
		  AND NOT EXISTS (
			SELECT 1 FROM messages k JOIN send_requests r ON r.wa_id = k.wa_id
			WHERE k.chat_id = y.chat_id AND r.state = 'sent' AND k.wa_id <> pa.shown_wa_id
			  AND k.sent_at > pa.shown_at AND k.sent_at < y.sent_at)`, a.ID, state, messageID, s.now())
	return n == 1, err
}

// Finish records how a confirmed action went.
func (s *Store) Finish(ctx context.Context, a Action, ok bool, result string) error {
	state := Failed
	if ok {
		state = Done
	}
	_, err := s.db.Exec(ctx, `UPDATE pending_actions SET state = $2, result = $3 WHERE id = $1::uuid AND state = 'confirmed'`,
		a.ID, state, text.Clip(result, 500))
	return err
}

// RecentlyLapsed says whether the last action DJ was asked about expired in the last hour without
// an answer, so a late yes can be told so.
func (s *Store) RecentlyLapsed(ctx context.Context) (bool, error) {
	var lapsed bool
	err := s.db.QueryRow(ctx, `SELECT coalesce((SELECT state = 'expired' AND expires_at > $1 FROM pending_actions
		WHERE shown_at IS NOT NULL ORDER BY shown_at DESC LIMIT 1), false)`, s.now().Add(-time.Hour)).Scan(&lapsed)
	return lapsed, err
}

// CountDone is how many actions of kind DJ confirmed since, run or not, so one that may have gone
// out counts too.
func (s *Store) CountDone(ctx context.Context, kind string, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM pending_actions WHERE kind = $1 AND state IN ('confirmed', 'done', 'failed') AND decided_at > $2`,
		kind, since).Scan(&n)
	return n, err
}

var (
	yesWords = []string{
		"yes", "y", "yeah", "yep", "yup", "ok", "okay", "sure", "send it", "do it", "go ahead",
		"confirm", "confirmed", "haan", "han", "ha", "haa", "haanji", "hanji", "haan ji", "bhej", "bhej do", "bhejo",
		"kar do", "karo", "theek hai", "thik hai", "chalega", "हाँ", "हां", "हा", "भेज दो", "👍", "✅",
	}
	noWords = []string{
		"no", "n", "nope", "nah", "cancel", "stop", "dont", "don't", "do not", "nahi", "nahin", "nai", "na", "mat",
		"mat bhejo", "mat bhej", "rehne do", "rehnde", "ruk", "ruko", "नहीं", "नही", "मत", "👎", "❌",
	}
)

// Decision reads a reply to a confirmation: yes or no only when the whole message is one of a few
// plain words, in English, Hinglish or Hindi, however its letters are drawn out ("yesss", "haaan");
// anything longer is not a decision, so "yes but at 7" never confirms.
func Decision(message string) (yes, no bool) {
	cleaned := strings.ToLower(strings.TrimFunc(message, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsPunct(r) && r != '\''
	}))
	cleaned = squeeze(strings.Join(strings.Fields(cleaned), " "))
	for _, w := range yesWords {
		if cleaned == squeeze(w) {
			return true, false
		}
	}
	for _, w := range noWords {
		if cleaned == squeeze(w) {
			return false, true
		}
	}
	return false, false
}

// squeeze writes each run of one letter once, so "yesss" and "yes", "haaan" and "han" compare equal.
func squeeze(s string) string {
	var b strings.Builder
	var last rune
	for i, r := range s {
		if i > 0 && r == last && unicode.IsLetter(r) {
			continue
		}
		b.WriteRune(r)
		last = r
	}
	return b.String()
}

// Executor runs a confirmed action and says what happened, in words for DJ.
type Executor func(ctx context.Context, a Action) (string, error)

// Runner decides the waiting action from DJ's replies and runs confirmed ones.
type Runner struct {
	Store     *Store
	Executors map[string]Executor
	Logger    *slog.Logger
	// MessageID finds the messages.id of DJ's message by its WhatsApp id, 0 when it is not stored.
	MessageID func(ctx context.Context, waID string) (int64, error)
}

// Replay answers a message that already decided an action, as it was answered then, running a
// confirmed one that never finished (at most once, by its key); false for any other message.
func (r *Runner) Replay(ctx context.Context, waID string) (string, bool, error) {
	messageID, err := r.MessageID(ctx, waID)
	if err != nil || messageID == 0 {
		return "", false, err
	}
	decided, result, err := r.Store.DecidedBy(ctx, messageID)
	if err != nil || decided == nil {
		return "", false, err
	}
	switch decided.State {
	case Confirmed:
		return r.run(ctx, *decided)
	case Declined:
		return "Okay, I won't.", true, nil
	default:
		return result, true, nil
	}
}

// Handle answers DJ's message when it decides an action, and reports false when it does not, so it
// goes on to the chat model. A message seen before gets the answer it got then; a yes runs the
// action at most once.
func (r *Runner) Handle(ctx context.Context, waID, message string) (string, bool, error) {
	yes, no := Decision(message)
	if !yes && !no {
		return "", false, nil
	}
	if answer, replayed, err := r.Replay(ctx, waID); err != nil || replayed {
		return answer, replayed, err
	}
	messageID, err := r.MessageID(ctx, waID)
	if err != nil || messageID == 0 {
		return "", false, err
	}
	open, err := r.Store.Open(ctx)
	if err != nil {
		return "", false, err
	}
	if open == nil {
		lapsed, err := r.Store.RecentlyLapsed(ctx)
		if err != nil || !lapsed || no {
			return "", false, err
		}
		return "That one waited too long, so I did nothing. Ask me again if you still want it.", true, nil
	}
	ok, err := r.Store.Decide(ctx, *open, messageID, yes)
	if err != nil || !ok {
		return "", false, err
	}
	if no {
		r.Logger.Info("an action was declined", "action", open.ID, "kind", open.Kind)
		return "Okay, I won't.", true, nil
	}
	r.Logger.Info("an action was confirmed", "action", open.ID, "kind", open.Kind)
	return r.run(ctx, *open)
}

func (r *Runner) run(ctx context.Context, a Action) (string, bool, error) {
	execute, ok := r.Executors[a.Kind]
	if !ok {
		err := r.Store.Finish(ctx, a, false, "nothing can run this kind of action")
		return "I can't do that kind of thing yet, so nothing was done.", true, err
	}
	result, err := execute(ctx, a)
	if err != nil {
		if ctx.Err() != nil {
			return "", false, context.Cause(ctx)
		}
		r.Logger.Warn("a confirmed action failed", "action", a.ID, "kind", a.Kind, logging.Err(err))
		answer := fmt.Sprintf("I couldn't do it: %s", result)
		if result == "" {
			answer = "I couldn't do it, so nothing was done."
		}
		return answer, true, r.Store.Finish(ctx, a, false, answer)
	}
	return result, true, r.Store.Finish(ctx, a, true, result)
}

// ConfirmLine is what Kaya puts at the top of her answer when she proposes an action, so DJ always
// sees exactly what a yes will do before anything the model wrote.
func ConfirmLine(a Action, ttl time.Duration) string {
	return fmt.Sprintf("*%s*\nReply *yes* to do it or *no* to cancel (within %d min).", a.Summary, int(ttl.Minutes()))
}
