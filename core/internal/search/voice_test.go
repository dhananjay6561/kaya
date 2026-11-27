package search

import (
	"context"
	"testing"

	"github.com/dhananjay6561/kaya/kit/testdb"
)

func TestATranscribedVoiceNoteIsReadLikeAMessage(t *testing.T) {
	ctx := context.Background()
	core, admin := testdb.MigratedPair(t, "kaya_core")
	var rahul, chat, heard, unheard int64
	if err := admin.QueryRow(ctx, "SELECT contact_id_for('100000000000002@lid', '918888888888@s.whatsapp.net')").Scan(&rahul); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, "INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", rahul).Scan(&chat); err != nil {
		t.Fatal(err)
	}
	for i, id := range []*int64{&heard, &unheard} {
		if err := admin.QueryRow(ctx, `INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, source)
			VALUES ($1, $2, $3, false, now() - make_interval(mins => $4), 'voice', 'live') RETURNING id`,
			chat, "3EB0VOICE"+string(rune('A'+i)), rahul, 10-i).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := admin.Exec(ctx, `INSERT INTO transcripts (message_id, state, text, error) VALUES ($1, 'done', 'kal 5 baje milte hain', NULL),
		($2, 'failed', '', 'no_file')`, heard, unheard); err != nil {
		t.Fatal(err)
	}
	lines, err := ReadChatLines(ctx, core, "m.chat_id = $1 ORDER BY m.sent_at", chat)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0].MessageID != heard || lines[0].Text != "(voice note) kal 5 baje milte hain" {
		t.Fatalf("lines %+v", lines)
	}
}
