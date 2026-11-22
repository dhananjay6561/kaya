package ingest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/dhananjay6561/kaya/gateway/internal/logging"
)

func newIngester(f *fixture, db DB) (*Ingester, *bytes.Buffer) {
	var logs bytes.Buffer
	i := NewIngester(db, f.store, logging.New(&logs, "info"))
	i.retryFirst, i.retryMax, i.retryFor = time.Millisecond, 4*time.Millisecond, 50*time.Millisecond
	return i, &logs
}

func TestHandleStoresLiveMessagesAndAcknowledgesThem(t *testing.T) {
	f := newFixture(t)
	i, logs := newIngester(f, f.gateway)
	ctx := context.Background()
	newGroups := 0
	i.NewGroup = func() { newGroups++ }

	first := dmFrom(t, textMsg("Saturday 7pm works"))
	for range 2 {
		if !i.Handle(ctx, own, first) {
			t.Fatal("a stored message, and its redelivery, are acknowledged")
		}
	}
	if !i.Handle(ctx, own, groupFrom(t, rahulLID, textMsg("hi all"))) || newGroups != 1 {
		t.Fatalf("a message from an unknown group is stored and reported, got %d reports", newGroups)
	}
	edit := dmFrom(t, &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(), Key: &waCommon.MessageKey{ID: proto.String("3EB0AAAA")},
		EditedMessage: textMsg("Saturday 8pm"), TimestampMS: proto.Int64(sentAt.Add(time.Minute).UnixMilli()),
	}})
	edit.Info.ID = "3EB0EDIT"
	if !i.Handle(ctx, own, edit) {
		t.Fatal("an edit is acknowledged")
	}
	placeholder := &events.UndecryptableMessage{Info: first.Info, IsUnavailable: true, UnavailableType: events.UnavailableTypeViewOnce}
	placeholder.Info.ID = "3EB0VIEWONCE"
	if !i.Handle(ctx, own, placeholder) {
		t.Fatal("a view-once placeholder is acknowledged")
	}
	if !i.Handle(ctx, own, &events.Receipt{}) || !i.Handle(ctx, own, dmFrom(t, &waE2E.Message{PollUpdateMessage: &waE2E.PollUpdateMessage{}})) {
		t.Fatal("events and message kinds that are not stored are acknowledged")
	}

	if n := f.count("SELECT count(*) FROM messages"); n != 3 {
		t.Fatalf("want 3 messages, got %d", n)
	}
	if got := f.text("SELECT body FROM messages WHERE wa_id = '3EB0AAAA'"); got != "Saturday 8pm" {
		t.Fatalf("the edit must be applied, got %s", got)
	}
	if n := f.count("SELECT count(*) FROM messages WHERE view_once AND body IS NULL AND kind = 'other'"); n != 1 {
		t.Fatal("the view-once placeholder must be stored without content")
	}
	if n := f.count("SELECT count(*) FROM ingest_dead"); n != 0 {
		t.Fatal("nothing should be set aside")
	}
	if strings.Contains(logs.String(), "Saturday") || strings.Contains(logs.String(), "911111111111") {
		t.Fatalf("logs must not carry message text or numbers: %s", logs)
	}
}

func TestAMessageTheDatabaseRefusesIsSetAsideThenAcknowledged(t *testing.T) {
	f := newFixture(t)
	i, logs := newIngester(f, f.gateway)
	bad := dmFrom(t, textMsg("a secret that fails to store"))
	bad.Info.ID = "has a space"
	bad.RawMessage = bad.Message

	if !i.Handle(context.Background(), own, bad) {
		t.Fatal("a message that can never be stored is acknowledged once a copy is kept")
	}
	if n := f.count(`SELECT count(*) FROM ingest_dead WHERE wa_id = 'has a space' AND error_code = '23514'
		AND chat_jid = '100000000000001@lid' AND sender_jid = '100000000000001@lid' AND info ->> 'from_me' = 'false' AND length(raw) > 0`); n != 1 {
		t.Fatal("the copy must be in ingest_dead with its code and metadata")
	}
	if n := f.count("SELECT count(*) FROM messages") + f.count("SELECT count(*) FROM gateway_events"); n != 0 {
		t.Fatal("nothing else may be stored")
	}
	if !strings.Contains(logs.String(), "23514") || strings.Contains(logs.String(), "secret") || strings.Contains(logs.String(), "has a space") {
		t.Fatalf("the log names the code, never the content: %s", logs)
	}
}

func TestAMapperPanicIsSetAside(t *testing.T) {
	f := newFixture(t)
	i, _ := newIngester(f, f.gateway)
	evt := dmFrom(t, textMsg("x"))
	ok := i.message(context.Background(), evt.Info, evt.Message, func() Record { panic("unexpected shape") })
	if !ok || f.count("SELECT count(*) FROM ingest_dead WHERE error_code = 'mapper_panic' AND wa_id = '3EB0AAAA'") != 1 {
		t.Fatal("a panic in the mapper must end as a dead letter, acknowledged")
	}
}

type downDB struct {
	calls atomic.Int32
	err   error
}

func (d *downDB) InTx(context.Context, func(pgx.Tx) error) error {
	d.calls.Add(1)
	return d.err
}

func TestNothingIsAcknowledgedWhileTheDatabaseIsAway(t *testing.T) {
	f := newFixture(t)
	down := &downDB{err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}}
	i, logs := newIngester(f, down)
	ctx := context.Background()

	started := time.Now()
	if i.Handle(ctx, own, dmFrom(t, textMsg("hello"))) {
		t.Fatal("a message must not be acknowledged while the database is away")
	}
	if down.calls.Load() < 3 || time.Since(started) < 20*time.Millisecond {
		t.Fatalf("it must retry for the whole window first (%d calls in %s)", down.calls.Load(), time.Since(started))
	}
	if i.Handle(ctx, own, &events.GroupInfo{JID: types.NewJID("120363000000000000", types.GroupServer), Name: &types.GroupName{Name: "Trip"}}) {
		t.Fatal("a group name must not be acknowledged while the database is away")
	}
	if i.Handle(ctx, own, &events.Contact{JID: types.NewJID("911111111111", types.DefaultUserServer), Action: &waSyncAction.ContactAction{FullName: proto.String("Rahul")}}) {
		t.Fatal("a contact must not be acknowledged while the database is away")
	}
	if !strings.Contains(logs.String(), "database unavailable") {
		t.Fatalf("the outage must be logged: %s", logs)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	before := down.calls.Load()
	if i.Handle(cancelled, own, dmFrom(t, textMsg("hello"))) || down.calls.Load()-before > 1 {
		t.Fatal("shutting down stops retrying and acknowledges nothing")
	}
}

func TestNamesFromEvents(t *testing.T) {
	f := newFixture(t)
	i, _ := newIngester(f, f.gateway)
	ctx := context.Background()
	group := types.NewJID("120363000000000000", types.GroupServer)

	ok := i.Handle(ctx, own, &events.JoinedGroup{GroupInfo: types.GroupInfo{JID: group, GroupName: types.GroupName{Name: "Trip planning", NameSetAt: sentAt}}})
	ok = ok && i.Handle(ctx, own, &events.GroupInfo{JID: group, Name: &types.GroupName{Name: "Goa trip", NameSetAt: sentAt.Add(time.Hour)}})
	ok = ok && i.Handle(ctx, own, &events.GroupInfo{JID: group})
	ok = ok && i.Handle(ctx, own, &events.Contact{
		JID:    types.NewJID("100000000000001", types.HiddenUserServer),
		Action: &waSyncAction.ContactAction{FullName: proto.String("Rahul Sharma"), FirstName: proto.String("Rahul"), PnJID: proto.String(rahulPN)},
	})
	if !ok {
		t.Fatal("name events are acknowledged")
	}
	if got := f.text("SELECT name FROM chats WHERE jid = $1", groupJID); got != "Goa trip" {
		t.Fatalf("group name: %s", got)
	}
	if got := f.text("SELECT saved_name || '|' || first_name FROM contacts WHERE lid = $1 AND pn = $2", rahulLID, rahulPN); got != "Rahul Sharma|Rahul" {
		t.Fatalf("contact: %s", got)
	}

	i.Handle(ctx, own, &events.Contact{JID: types.NewJID("911111111111", types.DefaultUserServer)})
	if got := f.text("SELECT saved_name FROM contacts WHERE pn = $1", rahulPN); got != "<nil>" {
		t.Fatalf("a removed contact loses its saved name, got %s", got)
	}
}

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		err       error
		code      string
		permanent bool
	}{
		{&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, "", false},
		{&pgconn.ConnectError{}, "", false},
		{context.DeadlineExceeded, "", false},
		{io.ErrUnexpectedEOF, "", false},
		{errors.New("conn closed"), "", false},
		{errors.New("unable to encode 4294967295 into binary format for int4"), "client_error", true},
		{errors.New("json: unsupported value"), "client_error", true},
		{pgErr("08006"), "", false},
		{pgErr("57P01"), "", false},
		{pgErr("53300"), "", false},
		{pgErr("40001"), "", false},
		{pgErr("40P01"), "", false},
		{pgErr("23514"), "23514", true},
		{pgErr("42501"), "42501", true},
	} {
		if code, permanent := classify(tc.err); code != tc.code || permanent != tc.permanent {
			t.Errorf("%v: got %q %v", tc.err, code, permanent)
		}
	}
}

func pgErr(code string) error {
	return &pgconn.PgError{Code: code, Message: "boom", Detail: "Failing row contains (secret)."}
}

func TestScrubDropsRowDetails(t *testing.T) {
	if got := scrub(pgErr("23514")).Error(); strings.Contains(got, "secret") || !strings.Contains(got, "23514") {
		t.Fatalf("got %q", got)
	}
}

func TestACraftedMessageCannotBlockIngest(t *testing.T) {
	huge := uint32(4294967295)
	for name, evt := range map[string]*events.Message{
		"image with impossible dimensions": dmFrom(t, &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Width: &huge, Height: &huge}}),
		"video with impossible duration":   dmFrom(t, &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Seconds: &huge}}),
		"edit from the year 10000": dmFrom(t, &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
			Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(), Key: &waCommon.MessageKey{ID: proto.String("NOTSTORED")},
			EditedMessage: textMsg("x"), TimestampMS: proto.Int64(253402300800000),
		}}),
		"reaction from the year 10000": dmFrom(t, &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
			Key: &waCommon.MessageKey{ID: proto.String("NOTSTORED"), FromMe: proto.Bool(true)}, Text: proto.String("👍"),
			SenderTimestampMS: proto.Int64(253402300800000),
		}}),
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			i, logs := newIngester(f, f.gateway)
			if !i.Handle(context.Background(), own, evt) {
				t.Fatalf("the message must be stored or set aside, then acknowledged: %s", logs)
			}
			next := dmFrom(t, textMsg("the next message"))
			next.Info.ID = "3EB0NEXT"
			if !i.Handle(context.Background(), own, next) || f.count("SELECT count(*) FROM messages WHERE body = 'the next message'") != 1 {
				t.Fatal("the message after it must still be stored")
			}
		})
	}
}

func TestAnErrorThatIsNotTheDatabaseBeingAwayIsSetAside(t *testing.T) {
	f := newFixture(t)
	i, _ := newIngester(f, &downDB{err: errors.New("unable to encode 4294967295 into binary format for int4")})
	started := time.Now()
	evt := dmFrom(t, textMsg("x"))
	// The fake database refuses everything the same way, so the dead letter is refused too; the
	// point is that the handler gives up at once instead of treating it as an outage.
	if !i.Handle(context.Background(), own, evt) || time.Since(started) > 40*time.Millisecond {
		t.Fatalf("a client-side error must not be retried like an outage (took %s)", time.Since(started))
	}
}

func TestADeadLetterTheDatabaseRefusesIsStillKept(t *testing.T) {
	f := newFixture(t)
	i, logs := newIngester(f, f.gateway)
	evt := dmFrom(t, textMsg("x"))
	evt.Info.ID = "has\x00nul"
	if !i.Handle(context.Background(), own, evt) {
		t.Fatalf("must be acknowledged: %s", logs)
	}
	if n := f.count("SELECT count(*) FROM ingest_dead WHERE wa_id IS NULL AND info ->> 'wa_id_hex' = '686173006e756c'"); n != 1 {
		t.Fatalf("the copy is kept with its id hex-encoded, got %d rows (%s)", n, logs)
	}
}

func TestAPanicAnywhereInTheHandlerIsNotAcknowledgedAsStored(t *testing.T) {
	f := newFixture(t)
	i, _ := newIngester(f, f.gateway)
	i.NewGroup = func() { panic("a bug after the message was stored") }
	if !i.Handle(context.Background(), own, groupFrom(t, rahulLID, textMsg("hi"))) {
		t.Fatal("the message was stored before the panic, so it is acknowledged")
	}
	if n := f.count("SELECT count(*) FROM messages"); n != 1 {
		t.Fatal("the message must be stored")
	}

	broken := NewIngester(nil, f.store, logging.New(&bytes.Buffer{}, "info"))
	if broken.Handle(context.Background(), own, &events.GroupInfo{JID: types.NewJID("120363000000000000", types.GroupServer), Name: &types.GroupName{Name: "x"}}) {
		t.Fatal("a panic while handling an event that is not a message is not acknowledged")
	}
}
