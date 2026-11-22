package media

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrNoMedia means the message has no file: it is not a media message, or it is view-once.
var ErrNoMedia = errors.New("that message has no media")

// Fetch asks the running gateway to download a message's file and waits for the outcome. It only
// talks to the database: the gateway that holds the WhatsApp connection does the download. It
// returns the path of the stored file.
func Fetch(ctx context.Context, db DB, files *Files, messageID int64, wait, poll time.Duration) (string, error) {
	deadline := time.Now().Add(wait)
	asked := false
	for {
		var state string
		var sha256, code *string
		var queued bool
		err := db.InTx(ctx, func(tx pgx.Tx) error {
			err := tx.QueryRow(ctx, `
				SELECT m.state, m.sha256, m.error_code, EXISTS (SELECT FROM media_requests r WHERE r.message_id = m.message_id)
				FROM media m WHERE m.message_id = $1`, messageID).Scan(&state, &sha256, &code, &queued)
			if err != nil {
				return err
			}
			present := state == StateStored && sha256 != nil && files.Exists(*sha256)
			if present || state == StateExpired || asked {
				return nil
			}
			if state == StateStored {
				if _, err := tx.Exec(ctx, "UPDATE media SET state = 'remote', stored_at = NULL WHERE message_id = $1", messageID); err != nil {
					return err
				}
				state = StateRemote
			}
			_, err = tx.Exec(ctx, "INSERT INTO media_requests (message_id) VALUES ($1) ON CONFLICT (message_id) DO NOTHING", messageID)
			queued = true
			return err
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNoMedia
		}
		if err != nil {
			return "", fmt.Errorf("read media state: %w", err)
		}
		switch {
		case state == StateStored && sha256 != nil && files.Exists(*sha256):
			return files.Path(*sha256)
		case state == StateExpired:
			return "", errors.New("WhatsApp no longer has that file")
		case asked && !queued:
			return "", fmt.Errorf("the download failed: %s", deref(code))
		}
		asked = true
		if time.Now().After(deadline) {
			return "", errors.New("timed out waiting for the gateway; is it running and connected to WhatsApp?")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(poll):
		}
	}
}
