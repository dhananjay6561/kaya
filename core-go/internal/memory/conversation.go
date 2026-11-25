package memory

import (
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dhananjay6561/kaya/core/internal/text"
	"github.com/dhananjay6561/kaya/kit/store"
)

// DefaultExchanges is how many past exchanges ReadConversation reads when asked for none.
const DefaultExchanges = 50

const conversationSQL = `
SELECT q.body AS question, a.body AS answer, q.sent_at
FROM (SELECT * FROM exchanges ORDER BY id DESC LIMIT $1 * 2) e
JOIN messages q ON q.id = e.question_message_id
JOIN messages a ON a.chat_id = q.chat_id AND a.wa_id = e.answer_wa_id AND a.from_me
WHERE q.deleted_at IS NULL AND a.deleted_at IS NULL
  AND q.body IS NOT NULL AND a.body IS NOT NULL
  AND (q.edited_at IS NULL OR q.edited_at <= e.created_at)
ORDER BY e.id DESC
LIMIT $1`

// ReadConversation returns the latest exchanges between DJ and Kaya, oldest first, however long
// ago they were. An exchange whose question or answer was deleted, or whose question was edited
// after it was answered, is skipped.
func ReadConversation(ctx context.Context, db *store.DB, maxExchanges int) ([]Exchange, error) {
	if maxExchanges <= 0 {
		maxExchanges = DefaultExchanges
	}
	var out []Exchange
	err := db.InTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, conversationSQL, maxExchanges)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Exchange, error) {
			var e Exchange
			var at time.Time
			err := row.Scan(&e.Question, &e.Answer, &at)
			e.Question = text.Trim(e.Question)
			e.At = at
			return e, err
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	slices.Reverse(out)
	return out, nil
}
