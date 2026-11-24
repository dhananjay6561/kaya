package ingest

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/dhananjay6561/kaya/contracts"
	"github.com/dhananjay6561/kaya/gateway/internal/events"
	"github.com/dhananjay6561/kaya/gateway/internal/store"
	"github.com/dhananjay6561/kaya/gateway/internal/testdb"
)

type fixture struct {
	t       *testing.T
	gateway *store.DB
	admin   *store.DB
	store   *Store
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	gateway, admin := testdb.MigratedPair(t, "kaya_gateway")
	v, err := contracts.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, gateway: gateway, admin: admin, store: NewStore(events.NewOutbox(v))}
}

func (f *fixture) apply(rec Record) Applied {
	f.t.Helper()
	var out Applied
	err := f.gateway.InTx(context.Background(), func(tx pgx.Tx) (err error) {
		out, err = f.store.Apply(context.Background(), tx, rec)
		return err
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *fixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.admin.Query(context.Background(), query, args, &n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fixture) text(query string, args ...any) string {
	f.t.Helper()
	var s *string
	if err := f.admin.Query(context.Background(), query, args, &s); err != nil {
		f.t.Fatal(err)
	}
	if s == nil {
		return "<nil>"
	}
	return *s
}

var (
	rahul = Address{LID: rahulLID, PN: rahulPN}
	dm    = ChatRef{Kind: ChatDirect, Contact: rahul}
	group = ChatRef{Kind: ChatGroup, JID: groupJID}
)

func message(chat ChatRef, sender Address, waID, text string) Record {
	return Record{
		Op: OpStore, Chat: chat, Sender: sender, WAID: waID, At: sentAt, Source: SourceLive,
		Kind: "text", Body: &text, FromMe: sender == own,
	}
}

func TestApplyStoresAMessageOnceWithItsEvent(t *testing.T) {
	f := newFixture(t)
	rec := message(dm, rahul, "MSG1", "Saturday 7pm works")
	rec.PushName = "Rahul"

	if got := f.apply(rec); !got.Changed || got.NewGroup {
		t.Fatalf("first delivery: %+v", got)
	}
	if got := f.apply(rec); got.Changed {
		t.Fatalf("a redelivered message changes nothing, got %+v", got)
	}

	if n := f.count("SELECT count(*) FROM messages"); n != 1 {
		t.Fatalf("want 1 message, got %d", n)
	}
	if n := f.count("SELECT count(*) FROM gateway_events WHERE type = 'message.stored'"); n != 1 {
		t.Fatalf("want 1 event, got %d", n)
	}
	if n := f.count(`SELECT count(*) FROM gateway_events e JOIN messages m ON m.id = (e.payload #>> '{data,message_id}')::bigint
		WHERE e.payload #>> '{data,change}' = 'new' AND e.payload #>> '{data,wa_id}' = 'MSG1'
			AND (e.payload #>> '{data,chat_id}')::bigint = m.chat_id AND (e.payload #>> '{data,sender_id}')::bigint = m.sender_id
			AND e.payload::text NOT LIKE '%Saturday%'`); n != 1 {
		t.Fatal("the event must point at the stored row and never carry the body")
	}
	if got := f.text("SELECT push_name FROM contacts WHERE lid = $1 AND pn = $2", rahulLID, rahulPN); got != "Rahul" {
		t.Fatalf("push name: %s", got)
	}
	if n := f.count("SELECT count(*) FROM chats WHERE kind = 'direct' AND last_message_at = $1", sentAt); n != 1 {
		t.Fatal("the chat must exist once with its last message time")
	}
}

func TestBothSpellingsOfADirectChatAreOneChat(t *testing.T) {
	f := newFixture(t)
	f.apply(message(ChatRef{Kind: ChatDirect, Contact: Address{PN: rahulPN}}, Address{PN: rahulPN}, "MSG1", "by phone number"))
	f.apply(message(ChatRef{Kind: ChatDirect, Contact: Address{LID: rahulLID}}, Address{LID: rahulLID}, "MSG2", "by lid"))
	f.apply(message(dm, own, "MSG3", "my reply"))
	f.apply(message(dm, rahul, "MSG1", "by phone number"))

	if n := f.count("SELECT count(*) FROM chats"); n != 1 {
		t.Fatalf("want one chat after the LID and number are linked, got %d", n)
	}
	if n := f.count("SELECT count(*) FROM messages"); n != 3 {
		t.Fatalf("want 3 messages, got %d", n)
	}
	if n := f.count("SELECT count(*) FROM contacts"); n != 2 {
		t.Fatalf("want Rahul and DJ, got %d contacts", n)
	}
}

func TestGroupMessagesReplyAndNewGroupSignal(t *testing.T) {
	f := newFixture(t)
	if got := f.apply(message(group, rahul, "G1", "who is in?")); !got.NewGroup {
		t.Fatal("the first message of an unknown group reports a new group")
	}
	reply := message(group, own, "G2", "me")
	reply.ReplyTo = &Target{WAID: "G1", Sender: Address{LID: rahulLID}}
	expires := sentAt.Add(24 * time.Hour)
	reply.ExpiresAt = &expires
	if got := f.apply(reply); got.NewGroup {
		t.Fatal("a known group is not new")
	}

	if n := f.count(`SELECT count(*) FROM messages r JOIN messages o ON o.wa_id = r.reply_to_wa_id AND o.sender_id = r.reply_to_sender_id
		WHERE r.wa_id = 'G2' AND r.from_me AND r.expires_at = $1`, expires); n != 1 {
		t.Fatal("the reply must point at the original message's sender")
	}
	if got := f.text("SELECT name FROM chats WHERE jid = $1", groupJID); got != "<nil>" {
		t.Fatalf("a new group has no name yet, got %s", got)
	}
}

func TestEditsAndRevokes(t *testing.T) {
	f := newFixture(t)
	f.apply(message(dm, rahul, "MSG1", "Saturday 7pm"))
	edited := "Saturday 8pm"
	edit := Record{Op: OpEdit, Chat: dm, Sender: rahul, WAID: "EDIT1", At: sentAt.Add(time.Minute), Body: &edited, Target: Target{WAID: "MSG1", Sender: rahul}}

	if got := f.apply(edit); !got.Changed {
		t.Fatalf("edit: %+v", got)
	}
	if got := f.apply(edit); got.Changed || got.NoTarget {
		t.Fatalf("a repeated edit changes nothing, got %+v", got)
	}
	older := edit
	stale := "Saturday 6pm"
	older.Body, older.At = &stale, sentAt.Add(30*time.Second)
	f.apply(older)
	if got := f.text("SELECT body FROM messages WHERE wa_id = 'MSG1'"); got != "Saturday 8pm" {
		t.Fatalf("the newest edit wins, got %s", got)
	}

	notHis := edit
	notHis.Sender, notHis.Target.Sender = own, own
	if got := f.apply(notHis); !got.NoTarget {
		t.Fatalf("an edit only reaches the editor's own message, got %+v", got)
	}

	revoke := Record{Op: OpRevoke, Chat: dm, Sender: rahul, WAID: "REV1", At: sentAt.Add(2 * time.Minute), Target: Target{WAID: "MSG1", Sender: rahul}}
	if got := f.apply(revoke); !got.Changed {
		t.Fatalf("revoke: %+v", got)
	}
	if got := f.apply(revoke); got.Changed {
		t.Fatal("a repeated revoke changes nothing")
	}
	late := edit
	late.At = sentAt.Add(3 * time.Minute)
	if got := f.apply(late); got.Changed {
		t.Fatal("a revoked message can no longer be edited")
	}
	if n := f.count("SELECT count(*) FROM messages WHERE wa_id = 'MSG1' AND body IS NULL AND deleted_at IS NOT NULL AND edited_at IS NOT NULL"); n != 1 {
		t.Fatal("a revoke clears the body and keeps the row")
	}
	if got := f.text("SELECT string_agg(payload #>> '{data,change}', ',' ORDER BY seq) FROM gateway_events"); got != "new,edited,deleted" {
		t.Fatalf("events: %s", got)
	}

	missing := revoke
	missing.Target.WAID = "NEVERSEEN"
	if got := f.apply(missing); !got.NoTarget || got.Changed {
		t.Fatalf("a revoke for a message that is not stored is dropped, got %+v", got)
	}
	elsewhere := revoke
	elsewhere.Chat = ChatRef{Kind: ChatGroup, JID: "120363999999999999@g.us"}
	if got := f.apply(elsewhere); !got.NoTarget {
		t.Fatal("a revoke in a chat that is not stored is dropped")
	}
	if n := f.count("SELECT count(*) FROM chats"); n != 1 {
		t.Fatal("a dropped change must not create a chat")
	}
}

func TestReactions(t *testing.T) {
	f := newFixture(t)
	f.apply(message(dm, own, "MSG1", "on my way"))
	react := func(emoji string, at time.Time) Record {
		return Record{Op: OpReact, Chat: dm, Sender: rahul, WAID: "R" + emoji, At: at, Emoji: emoji, Target: Target{WAID: "MSG1", Sender: own}}
	}
	f.apply(react("👍", sentAt.Add(time.Minute)))
	f.apply(react("❤️", sentAt.Add(2*time.Minute)))
	f.apply(react("👍", sentAt.Add(time.Minute)))
	if got := f.text("SELECT emoji FROM reactions"); got != "❤️" {
		t.Fatalf("one reaction per person per message, the newest; got %s", got)
	}
	f.apply(react("", sentAt.Add(90*time.Second)))
	if n := f.count("SELECT count(*) FROM reactions"); n != 1 {
		t.Fatal("an older removal must not delete a newer reaction")
	}
	f.apply(react("", sentAt.Add(3*time.Minute)))
	if n := f.count("SELECT count(*) FROM reactions"); n != 0 {
		t.Fatal("removing a reaction deletes the row")
	}
	if n := f.count("SELECT count(*) FROM gateway_events"); n != 1 {
		t.Fatal("reactions emit no event")
	}
}

func TestChatNames(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	set := func(name string, at time.Time) {
		t.Helper()
		err := f.gateway.InTx(ctx, func(tx pgx.Tx) error { return f.store.SetChatName(ctx, tx, group, name, at) })
		if err != nil {
			t.Fatal(err)
		}
	}
	name := func() string { return f.text("SELECT name FROM chats WHERE jid = $1", groupJID) }

	set("Trip planning", sentAt)
	if name() != "Trip planning" {
		t.Fatalf("a name creates the group row, got %s", name())
	}
	set("Old name from history", sentAt.Add(-time.Hour))
	set("Name with no time", time.Time{})
	set("", sentAt.Add(time.Hour))
	if name() != "Trip planning" {
		t.Fatalf("only a newer name replaces a name, got %s", name())
	}
	set("Goa trip", sentAt.Add(time.Hour))
	if name() != "Goa trip" {
		t.Fatalf("a rename applies, got %s", name())
	}

	f.apply(message(ChatRef{Kind: ChatGroup, JID: "120363000000000001@g.us"}, rahul, "G1", "hi"))
	err := f.gateway.InTx(ctx, func(tx pgx.Tx) error {
		return f.store.SetChatName(ctx, tx, ChatRef{Kind: ChatGroup, JID: "120363000000000001@g.us"}, "Filled later", time.Time{})
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.text("SELECT name FROM chats WHERE jid = '120363000000000001@g.us'"); got != "Filled later" {
		t.Fatalf("a name without a time fills an empty name, got %s", got)
	}
}

func TestContactNames(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	set := func(n ContactNames) {
		t.Helper()
		if err := f.gateway.InTx(ctx, func(tx pgx.Tx) error { return f.store.SetContactNames(ctx, tx, n) }); err != nil {
			t.Fatal(err)
		}
	}
	names := func() string {
		return f.text(`SELECT concat_ws('|', coalesce(saved_name, '-'), coalesce(first_name, '-'), coalesce(push_name, '-'),
			coalesce(business_name, '-')) FROM contacts WHERE pn = $1`, rahulPN)
	}

	set(ContactNames{Address: Address{PN: rahulPN}, Saved: "Rahul Sharma", First: "Rahul", Push: "rahul.s", AddressBook: true})
	if names() != "Rahul Sharma|Rahul|rahul.s|-" {
		t.Fatalf("got %s", names())
	}
	set(ContactNames{Address: rahul, Business: "Sharma Travels"})
	if names() != "Rahul Sharma|Rahul|rahul.s|Sharma Travels" {
		t.Fatalf("a record without address-book names leaves them alone, got %s", names())
	}
	set(ContactNames{Address: rahul, AddressBook: true})
	if names() != "-|-|rahul.s|Sharma Travels" {
		t.Fatalf("removing the contact from the address book clears the saved names only, got %s", names())
	}
	if n := f.count("SELECT count(*) FROM contacts"); n != 1 {
		t.Fatalf("want one contact, got %d", n)
	}
}

func TestDeadLetters(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	err := f.gateway.InTx(ctx, func(tx pgx.Tx) error {
		return f.store.AddDeadLetter(ctx, tx, DeadLetter{WAID: "MSG1", ChatJID: groupJID, ErrorCode: "23514", Info: []byte(`{"from_me":false}`), Raw: []byte{1, 2}})
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := f.count("SELECT count(*) FROM ingest_dead WHERE wa_id = 'MSG1' AND sender_jid IS NULL AND error_code = '23514' AND raw = '\\x0102'"); n != 1 {
		t.Fatal("the dead letter must be stored")
	}
}

func voiceNote(waID string, sha string) Record {
	rec := message(dm, rahul, waID, "")
	rec.Kind, rec.Body = "voice", nil
	rec.Media = &Media{
		MimeType: "audio/ogg", Size: 4096, DurationS: 7, SHA256: sha, Type: "audio",
		Key: []byte{1}, DirectPath: "/v/t62/voice", EncSHA256: []byte{2},
	}
	return rec
}

func TestMediaRowsAndKeys(t *testing.T) {
	f := newFixture(t)
	sha := "abababababababababababababababababababababababababababababababab"
	f.apply(voiceNote("V1", sha))
	f.apply(voiceNote("V1", sha))

	bare := message(dm, rahul, "DOC1", "the deck")
	bare.Kind = "document"
	bare.Media = &Media{MimeType: "application/pdf", FileName: "deck.pdf", Type: "document"}
	f.apply(bare)

	old := voiceNote("V2", sha)
	old.Source = SourceHistory
	f.apply(old)

	if n := f.count(`SELECT count(*) FROM media m JOIN messages g ON g.id = m.message_id
		WHERE g.wa_id = 'V1' AND m.mime_type = 'audio/ogg' AND m.size_bytes = 4096 AND m.duration_s = 7 AND m.sha256 = $1
			AND m.state = 'remote' AND m.width IS NULL AND m.file_name IS NULL`, sha); n != 1 {
		t.Fatal("the voice note must have one media row with its details")
	}
	if n := f.count("SELECT count(*) FROM media_keys WHERE media_type = 'audio' AND direct_path = '/v/t62/voice'"); n != 2 {
		t.Fatalf("live and history messages both keep their keys, got %d", n)
	}
	if n := f.count(`SELECT count(*) FROM media m JOIN messages g ON g.id = m.message_id LEFT JOIN media_keys k USING (message_id)
		WHERE g.wa_id = 'DOC1' AND m.file_name = 'deck.pdf' AND m.sha256 IS NULL AND k.message_id IS NULL`); n != 1 {
		t.Fatal("a file that cannot be fetched has a media row and no keys")
	}
}

func TestARevokeForgetsTheFile(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sha := "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
	f.apply(voiceNote("V1", sha))
	f.apply(voiceNote("V2", sha))
	if err := f.admin.Query(ctx, "WITH u AS (UPDATE media SET state = 'stored', stored_at = now() RETURNING 1) SELECT count(*) FROM u", nil, new(int)); err != nil {
		t.Fatal(err)
	}
	revoke := func(waID string) Applied {
		return f.apply(Record{Op: OpRevoke, Chat: dm, Sender: rahul, WAID: "REV" + waID, At: sentAt.Add(time.Minute), Source: SourceLive, Target: Target{WAID: waID, Sender: rahul}})
	}

	if got := revoke("V1"); !got.Changed || got.RemovedFile != "" {
		t.Fatalf("the file stays while another stored message has it, got %+v", got)
	}
	if got := revoke("V2"); got.RemovedFile != sha {
		t.Fatalf("the last message to go takes the file with it, got %+v", got)
	}
	if n := f.count("SELECT count(*) FROM media") + f.count("SELECT count(*) FROM media_keys"); n != 0 {
		t.Fatal("a revoked message keeps no media rows or keys")
	}
}

func TestAForwardedMessageIsStoredAsOne(t *testing.T) {
	f := newFixture(t)
	written := message(dm, rahul, "3EB0WRITTEN", "my own words")
	passedOn := message(dm, rahul, "3EB0PASSEDON", "someone else's words")
	passedOn.Forwarded = true
	f.apply(written)
	f.apply(passedOn)
	if got := f.text("SELECT string_agg(wa_id, ',' ORDER BY wa_id) FROM messages WHERE forwarded"); got != "3EB0PASSEDON" {
		t.Fatalf("got %q", got)
	}
}
