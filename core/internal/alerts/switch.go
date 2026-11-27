package alerts

import (
	"context"

	"github.com/dhananjay6561/kaya/kit/store"
)

// Switch turns alerts on and off; /alerts uses it.
type Switch struct{ db *store.DB }

// NewSwitch returns the alerts switch on db.
func NewSwitch(db *store.DB) *Switch { return &Switch{db: db} }

// Enabled says whether alerts are on.
func (s *Switch) Enabled(ctx context.Context) (bool, error) {
	var on bool
	err := s.db.QueryRow(ctx, "SELECT enabled FROM alerting").Scan(&on)
	return on, err
}

// Set turns alerts on or off and says whether that changed anything. Turning them off passes over
// every alert still waiting, so turning them back on never brings a flood of old news.
func (s *Switch) Set(ctx context.Context, on bool) (bool, error) {
	changed, err := s.db.Exec(ctx, "UPDATE alerting SET enabled = $1, changed_at = now() WHERE enabled <> $1", on)
	if err != nil || changed == 0 || on {
		return err == nil && changed > 0, err
	}
	_, err = s.db.Exec(ctx, "UPDATE alerts SET skipped = 'off' WHERE sent_at IS NULL AND skipped IS NULL")
	return true, err
}

// Waiting is how many alerts wait to be sent, for /status.
func (s *Switch) Waiting(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, "SELECT count(*) FROM alerts WHERE sent_at IS NULL AND skipped IS NULL").Scan(&n)
	return n, err
}
