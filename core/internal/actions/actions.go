// Package actions keeps what Kaya would do outside DJ's own chat until he says yes: a proposal is
// stored as a pending action, his next yes or no decides it, and only a yes runs it, once.
package actions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

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

// answers is true when DJ's message y can answer action pa: it came after Kaya asked him, with no
// other message from Kaya in between, which it would more likely answer.
const answers = `y.sent_at > pa.shown_at AND NOT EXISTS (
	SELECT 1 FROM messages k JOIN send_requests r ON r.wa_id = k.wa_id
	WHERE k.chat_id = y.chat_id AND r.state = 'sent' AND k.wa_id <> pa.shown_wa_id
	  AND k.sent_at > pa.shown_at AND k.sent_at < y.sent_at)`

// Answers says whether DJ's message could answer the waiting action a, before anything is spent
// reading it.
func (s *Store) Answers(ctx context.Context, a Action, messageID int64) (bool, error) {
	var ok bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pending_actions pa, messages y
		WHERE pa.id = $1::uuid AND pa.state = 'pending' AND pa.expires_at > $3 AND y.id = $2 AND `+answers+`)`,
		a.ID, messageID, s.now()).Scan(&ok)
	return ok, err
}

// Decide records DJ's answer to the waiting action a, given by his message; false means it does
// not answer it: the action was no longer waiting, or his message came before the question or
// after another message from Kaya.
func (s *Store) Decide(ctx context.Context, a Action, messageID int64, yes bool) (bool, error) {
	state := Declined
	if yes {
		state = Confirmed
	}
	n, err := s.db.Exec(ctx, `UPDATE pending_actions pa SET state = $2, decided_by = $3, decided_at = now()
		FROM messages y
		WHERE pa.id = $1::uuid AND pa.state = 'pending' AND pa.expires_at > $4 AND y.id = $3 AND `+answers,
		a.ID, state, messageID, s.now())
	return n == 1, err
}

// Lapsed is the last action DJ was asked about when it expired in the last hour unanswered and his
// message could have answered it, so a late yes can be told so; nil otherwise.
func (s *Store) Lapsed(ctx context.Context, messageID int64) (*Action, error) {
	a, err := scan(s.db.QueryRow(ctx, `SELECT pa.id::text, pa.kind, pa.target_jid, pa.summary, pa.payload, pa.state, pa.created_at, pa.expires_at FROM (
			SELECT * FROM pending_actions WHERE shown_at IS NOT NULL ORDER BY shown_at DESC LIMIT 1) pa, messages y
		WHERE pa.state = 'expired' AND pa.expires_at > $2 AND y.id = $1 AND `+answers, messageID, s.now().Add(-time.Hour)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &a, err
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

// CountDone is how many actions of kind DJ confirmed since, run or not, so one that may have gone
// out counts too.
func (s *Store) CountDone(ctx context.Context, kind string, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM pending_actions WHERE kind = $1 AND state IN ('confirmed', 'done', 'failed') AND decided_at > $2`,
		kind, since).Scan(&n)
	return n, err
}

// Executor runs a confirmed action and says what happened, in words for DJ.
type Executor func(ctx context.Context, a Action) (string, error)

// Runner decides the waiting action from DJ's replies and runs confirmed ones.
type Runner struct {
	Store     *Store
	Executors map[string]Executor
	Logger    *slog.Logger
	// Judge reads what DJ's reply means for the action he was asked about.
	Judge Judge
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

// Handle answers DJ's message when it decides the action he was asked about, and reports false
// when it does not, so it goes on to the chat model. The Judge reads it only when it could answer
// that action, so ordinary messages cost nothing; a message seen before gets the answer it got
// then, and a yes runs the action at most once. When the Judge cannot read it, nothing is done and
// DJ is asked to reply again.
func (r *Runner) Handle(ctx context.Context, waID, message string) (string, bool, error) {
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
		return r.late(ctx, messageID, message)
	}
	if ok, err := r.Store.Answers(ctx, *open, messageID); err != nil || !ok {
		return "", false, err
	}
	meaning, err := r.Judge(ctx, open.Summary, message)
	switch {
	case err != nil && ctx.Err() != nil:
		return "", false, context.Cause(ctx)
	case err != nil:
		r.Logger.Warn("could not read a reply to an action; nothing was done", "action", open.ID, logging.Err(err))
		return "I couldn't make out whether that was a yes, so I did nothing. Reply again in a moment.", true, nil
	case meaning != Yes && meaning != No:
		r.Logger.Info("a reply to an action was not a yes or no", "action", open.ID, "meaning", meaning)
		return "", false, nil
	}
	ok, err := r.Store.Decide(ctx, *open, messageID, meaning == Yes)
	if err != nil || !ok {
		return "", false, err
	}
	if meaning == No {
		r.Logger.Info("an action was declined", "action", open.ID, "kind", open.Kind)
		return "Okay, I won't.", true, nil
	}
	r.Logger.Info("an action was confirmed", "action", open.ID, "kind", open.Kind)
	return r.run(ctx, *open)
}

// late tells DJ a yes came after the action he was asked about expired; anything else goes on.
func (r *Runner) late(ctx context.Context, messageID int64, message string) (string, bool, error) {
	lapsed, err := r.Store.Lapsed(ctx, messageID)
	if err != nil || lapsed == nil {
		return "", false, err
	}
	if meaning, err := r.Judge(ctx, lapsed.Summary, message); err != nil || meaning != Yes {
		return "", false, nil
	}
	return "That one waited too long, so I did nothing. Ask me again if you still want it.", true, nil
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
