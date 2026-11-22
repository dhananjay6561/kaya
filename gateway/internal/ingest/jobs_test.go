package ingest

import (
	"context"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func announcement(t *testing.T, waID string, progress uint32) *events.Message {
	t.Helper()
	evt := dmFromMe(t, &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_HISTORY_SYNC_NOTIFICATION.Enum(),
		HistorySyncNotification: &waE2E.HistorySyncNotification{
			SyncType: waE2E.HistorySyncType_RECENT.Enum(), ChunkOrder: proto.Uint32(1), Progress: proto.Uint32(progress),
			MediaKey: []byte("a key that must never leak"), DirectPath: proto.String("/v/t62/secret-path"),
		},
	}})
	evt.Info.ID = waID
	evt.Info.Category = "peer"
	evt.Info.Chat = types.NewJID("919000000000", types.DefaultUserServer)
	return evt
}

func history() *waHistorySync.HistorySync {
	return &waHistorySync.HistorySync{
		SyncType: waHistorySync.HistorySync_RECENT.Enum(),
		Conversations: []*waHistorySync.Conversation{
			{ID: proto.String(rahulPN), Messages: []*waHistorySync.HistorySyncMsg{
				historyMsg("H1", false, "", textMsg("old message")),
				historyMsg("H2", true, "", textMsg("my old reply")),
				historyMsg("has a space", false, "", textMsg("the database refuses this id")),
				historyMsg("UNPARSEABLE", false, "", textMsg("x")),
			}},
			{ID: proto.String(groupJID), Name: proto.String("Trip planning"), Messages: []*waHistorySync.HistorySyncMsg{
				historyMsg("G1", false, priyaLID, textMsg("who is in?")),
			}},
		},
		PhoneNumberToLidMappings: []*waHistorySync.PhoneNumberToLIDMapping{{PnJID: proto.String(rahulPN), LidJID: proto.String(rahulLID)}},
	}
}

func nextJob(t *testing.T, i *Ingester) *HistoryJob {
	t.Helper()
	job, err := i.NextHistoryJob(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestAHistoryAnnouncementIsQueuedOnce(t *testing.T) {
	f := newFixture(t)
	i, logs := newIngester(f, f.gateway)
	queued := 0
	i.HistoryQueued = func() { queued++ }
	ctx := context.Background()

	for range 2 {
		if !i.Handle(ctx, own, announcement(t, "HIST1", 40)) {
			t.Fatal("an announcement is acknowledged once its job is stored")
		}
	}
	if n := f.count("SELECT count(*) FROM history_jobs WHERE wa_id = 'HIST1' AND state = 'pending' AND sync_type = 'RECENT' AND progress = 40 AND notification IS NOT NULL"); n != 1 {
		t.Fatalf("want one pending job, got %d", n)
	}
	if queued != 2 || f.count("SELECT count(*) FROM messages") != 0 {
		t.Fatal("the worker is woken and the announcement itself is not a message")
	}
	if logged := logs.String(); len(logged) > 0 && (contains(logged, "secret-path") || contains(logged, "never leak")) {
		t.Fatalf("the announcement must never be logged: %s", logged)
	}

	down := &downDB{err: context.DeadlineExceeded}
	offline, _ := newIngester(f, down)
	if offline.Handle(ctx, own, announcement(t, "HIST2", 50)) {
		t.Fatal("an announcement must not be acknowledged while the database is away")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestStoreHistory(t *testing.T) {
	f := newFixture(t)
	i, _ := newIngester(f, f.gateway)
	ctx := context.Background()
	i.Handle(ctx, own, announcement(t, "HIST1", 100))
	job := nextJob(t, i)
	if job == nil || job.State != JobPending || len(job.Notification) == 0 {
		t.Fatalf("got %+v", job)
	}

	for range 2 {
		if err := i.StoreHistory(ctx, job, history(), fakeParser{}, own); err != nil {
			t.Fatal(err)
		}
	}

	if n := f.count("SELECT count(*) FROM messages WHERE source = 'history'"); n != 3 {
		t.Fatalf("want 3 history messages stored once, got %d", n)
	}
	if n := f.count("SELECT count(*) FROM gateway_events WHERE type = 'message.stored'"); n != 0 {
		t.Fatalf("history must emit no per-message events, got %d", n)
	}
	if n := f.count(`SELECT count(*) FROM gateway_events WHERE type = 'history.synced'
		AND payload #>> '{data,messages}' = '3' AND payload #>> '{data,chats}' = '2'
		AND payload #>> '{data,progress}' = '100' AND payload #>> '{data,complete}' = 'true'`); n != 1 {
		t.Fatalf("want one history.synced event with the inserted counts, got %d (%s)", n, f.text("SELECT string_agg(payload::text, ' ') FROM gateway_events"))
	}
	if n := f.count("SELECT count(*) FROM history_jobs WHERE state = 'stored' AND notification IS NULL AND messages = 3 AND chats = 2"); n != 1 {
		t.Fatal("the job must be stored with its key cleared")
	}
	if n := f.count("SELECT count(*) FROM ingest_dead WHERE info ->> 'source' = 'history'"); n != 2 {
		t.Fatalf("the refused and the unparseable message are dead letters, once each; got %d", n)
	}
	if n := f.count("SELECT count(*) FROM ingest_dead WHERE wa_id = 'has a space' AND error_code = '23514' AND length(raw) > 0"); n != 1 {
		t.Fatal("the refused message keeps its code and raw copy")
	}
	if n := f.count("SELECT count(*) FROM contacts WHERE lid = $1 AND pn = $2", rahulLID, rahulPN); n != 1 {
		t.Fatal("the lid mapping must link Rahul's two addresses")
	}
	if got := f.text("SELECT name FROM chats WHERE jid = $1", groupJID); got != "Trip planning" {
		t.Fatalf("group name from history: %s", got)
	}

	stored := nextJob(t, i)
	if stored == nil || stored.State != JobStored {
		t.Fatalf("a stored job still needs its receipt: %+v", stored)
	}
	if err := i.FinishHistory(ctx, stored); err != nil {
		t.Fatal(err)
	}
	if nextJob(t, i) != nil || f.count("SELECT count(*) FROM history_jobs WHERE state = 'done'") != 1 {
		t.Fatal("a finished job is done")
	}
}

func TestALiveCopyOfAHistoryMessageIsAnnouncedOnce(t *testing.T) {
	f := newFixture(t)
	old := message(dm, rahul, "MSG1", "sent during the first sync")
	old.Source = SourceHistory
	old.PushName = "Old name"
	if got := f.apply(old); !got.Changed {
		t.Fatalf("history insert: %+v", got)
	}
	live := message(dm, rahul, "MSG1", "sent during the first sync")
	live.PushName = "Rahul"
	if got := f.apply(live); !got.Changed {
		t.Fatal("the live copy turns the history row live")
	}
	if got := f.apply(live); got.Changed {
		t.Fatal("a second live copy changes nothing")
	}
	again := old
	again.PushName = "Old name"
	f.apply(again)

	if n := f.count("SELECT count(*) FROM messages WHERE source = 'live'"); n != 1 {
		t.Fatal("want one message, now live")
	}
	if n := f.count("SELECT count(*) FROM gateway_events WHERE payload #>> '{data,change}' = 'new' AND payload #>> '{data,source}' = 'live'"); n != 1 {
		t.Fatalf("core must hear of the message exactly once, got %d", n)
	}
	if got := f.text("SELECT push_name FROM contacts WHERE pn = $1", rahulPN); got != "Rahul" {
		t.Fatalf("an old name from history never replaces a newer one, got %s", got)
	}
}

func TestARevokeThatArrivesBeforeItsMessageIsAppliedAfterHistory(t *testing.T) {
	f := newFixture(t)
	i, _ := newIngester(f, f.gateway)
	ctx := context.Background()
	rahulByPN := Address{PN: rahulPN}
	chat := ChatRef{Kind: ChatDirect, Contact: rahulByPN}

	revoke := Record{Op: OpRevoke, Chat: chat, Sender: rahulByPN, WAID: "REV1", At: sentAt.Add(time.Hour), Source: SourceLive, Target: Target{WAID: "H1", Sender: rahulByPN}}
	if got := f.apply(revoke); !got.Parked || !got.NoTarget {
		t.Fatalf("a revoke with no target yet is parked, got %+v", got)
	}
	if n := f.count("SELECT count(*) FROM pending_changes"); n != 1 {
		t.Fatal("want one parked change")
	}

	i.Handle(ctx, own, announcement(t, "HIST1", 10))
	if err := i.StoreHistory(ctx, nextJob(t, i), history(), fakeParser{}, own); err != nil {
		t.Fatal(err)
	}
	if n := f.count("SELECT count(*) FROM messages WHERE wa_id = 'H1' AND body IS NULL AND deleted_at IS NOT NULL"); n != 1 {
		t.Fatal("the message must end up deleted, not stored with its text")
	}
	if n := f.count("SELECT count(*) FROM pending_changes"); n != 0 {
		t.Fatal("an applied change is cleared")
	}
	if n := f.count("SELECT count(*) FROM gateway_events WHERE payload #>> '{data,change}' = 'deleted'"); n != 1 {
		t.Fatal("core hears of the deletion")
	}
}

func TestParkedChangesAreDroppedAfterAWeek(t *testing.T) {
	f := newFixture(t)
	f.apply(Record{Op: OpRevoke, Chat: dm, Sender: rahul, WAID: "REV1", At: sentAt, Source: SourceLive, Target: Target{WAID: "NEVER", Sender: rahul}})
	ctx := context.Background()
	if err := f.admin.Query(ctx, "UPDATE pending_changes SET created_at = now() - interval '8 days' RETURNING 1", nil, new(int)); err != nil {
		t.Fatal(err)
	}
	i, _ := newIngester(f, f.gateway)
	i.Handle(ctx, own, announcement(t, "HIST1", 10))
	if err := i.StoreHistory(ctx, nextJob(t, i), &waHistorySync.HistorySync{}, fakeParser{}, own); err != nil {
		t.Fatal(err)
	}
	if n := f.count("SELECT count(*) FROM pending_changes"); n != 0 {
		t.Fatal("a change that waited a week is dropped")
	}
	if n := f.count("SELECT count(*) FROM gateway_events"); n != 0 {
		t.Fatal("a chunk without chats emits nothing")
	}
}

func TestFailingAndExpiringHistoryJobs(t *testing.T) {
	f := newFixture(t)
	i, logs := newIngester(f, f.gateway)
	ctx := context.Background()
	i.Handle(ctx, own, announcement(t, "HIST1", 10))
	i.Handle(ctx, own, announcement(t, "HIST2", 20))
	i.Handle(ctx, own, announcement(t, "HIST3", 30))

	first := nextJob(t, i)
	for attempt := 1; attempt <= historyMaxAttempts; attempt++ {
		dead, err := i.FailHistory(ctx, first, "bad_blob", false)
		if err != nil || dead != (attempt == historyMaxAttempts) {
			t.Fatalf("attempt %d: dead=%v err=%v", attempt, dead, err)
		}
	}
	second := nextJob(t, i)
	if second.WAID != "HIST2" {
		t.Fatalf("a dead job no longer blocks the queue, got %s", second.WAID)
	}
	if dead, _ := i.FailHistory(ctx, second, "http_410", true); !dead {
		t.Fatal("a final failure gives up at once")
	}
	if err := f.admin.Query(ctx, "UPDATE history_jobs SET created_at = now() - interval '8 days' WHERE wa_id = 'HIST3' RETURNING 1", nil, new(int)); err != nil {
		t.Fatal(err)
	}
	if nextJob(t, i) != nil {
		t.Fatal("a job that waited over a week is given up on")
	}
	if n := f.count("SELECT count(*) FROM history_jobs WHERE state = 'dead' AND notification IS NULL AND error_code IN ('bad_blob', 'http_410', 'waited_too_long')"); n != 3 {
		t.Fatalf("want three dead jobs with their keys cleared, got %d", n)
	}
	if !contains(logs.String(), "gave up on a history chunk") {
		t.Fatal("giving up must be logged")
	}
}

func TestHistoryKeepsReactionsAndBothSpellings(t *testing.T) {
	reacted := historyMsg("H1", false, "", textMsg("old message"))
	reacted.Message.Reactions = []*waWeb.Reaction{
		{Key: &waCommon.MessageKey{ID: proto.String("R1"), FromMe: proto.Bool(true)}, Text: proto.String("👍"), SenderTimestampMS: proto.Int64(253402300800000)},
		{Key: &waCommon.MessageKey{ID: proto.String("R2")}, Text: proto.String("")},
	}
	data := &waHistorySync.HistorySync{
		Conversations: []*waHistorySync.Conversation{{
			ID: proto.String(rahulLID), PnJID: proto.String(rahulPN), Messages: []*waHistorySync.HistorySyncMsg{reacted},
		}},
		PhoneNumberToLidMappings: []*waHistorySync.PhoneNumberToLIDMapping{
			{PnJID: proto.String("912222222222@c.us"), LidJID: proto.String(priyaLID)},
		},
	}

	chunk := FromHistory(data, fakeParser{}, own)
	if len(chunk.Records) != 2 || chunk.Records[0].Chat.Contact != (Address{LID: rahulLID, PN: rahulPN}) {
		t.Fatalf("the chat must know both of Rahul's addresses: %+v", chunk.Records)
	}
	reaction := chunk.Records[1]
	if reaction.Op != OpReact || reaction.Emoji != "👍" || reaction.Sender != own || !reaction.At.Equal(sentAt) ||
		reaction.Target != (Target{WAID: "H1", Sender: Address{LID: rahulLID, PN: rahulPN}}) {
		t.Fatalf("got %+v", reaction)
	}
	if len(chunk.Mappings) != 1 || chunk.Mappings[0] != (Address{LID: priyaLID, PN: "912222222222@s.whatsapp.net"}) {
		t.Fatalf("a number spelled @c.us is the same number: %+v", chunk.Mappings)
	}

	f := newFixture(t)
	i, _ := newIngester(f, f.gateway)
	ctx := context.Background()
	i.Handle(ctx, own, announcement(t, "HIST1", 10))
	if err := i.StoreHistory(ctx, nextJob(t, i), data, fakeParser{}, own); err != nil {
		t.Fatal(err)
	}
	if n := f.count("SELECT count(*) FROM reactions WHERE emoji = '👍' AND target_wa_id = 'H1'"); n != 1 {
		t.Fatalf("want the history reaction stored, got %d", n)
	}
}

func TestAnOddNameDoesNotFailAHistoryChunk(t *testing.T) {
	f := newFixture(t)
	i, _ := newIngester(f, f.gateway)
	ctx := context.Background()
	data := history()
	data.Conversations[1].Name = proto.String("Trip\x00planning")
	i.Handle(ctx, own, announcement(t, "HIST1", 10))
	if err := i.StoreHistory(ctx, nextJob(t, i), data, fakeParser{}, own); err != nil {
		t.Fatalf("a name the database refuses is skipped: %v", err)
	}
	if n := f.count("SELECT count(*) FROM history_jobs WHERE state = 'stored' AND messages = 3"); n != 1 {
		t.Fatal("the chunk must still be stored")
	}
}

func TestAReactionCanArriveBeforeItsChat(t *testing.T) {
	f := newFixture(t)
	early := Record{Op: OpReact, Chat: group, Sender: rahul, WAID: "R1", At: sentAt, Source: SourceLive, Emoji: "👍", Target: Target{WAID: "G1", Sender: own}}
	if got := f.apply(early); got.NoTarget || !got.NewGroup {
		t.Fatalf("got %+v", got)
	}
	if n := f.count("SELECT count(*) FROM reactions"); n != 1 {
		t.Fatal("the reaction must be stored")
	}
}

func TestAParkedRevokeIsAppliedWhenItsMessageArrivesLive(t *testing.T) {
	f := newFixture(t)
	f.apply(Record{Op: OpRevoke, Chat: dm, Sender: rahul, WAID: "REV1", At: sentAt.Add(time.Minute), Source: SourceLive, Target: Target{WAID: "LATE", Sender: rahul}})
	f.apply(message(dm, rahul, "LATE", "this was deleted before it arrived"))
	if n := f.count("SELECT count(*) FROM messages WHERE wa_id = 'LATE' AND body IS NULL AND deleted_at IS NOT NULL"); n != 1 {
		t.Fatal("the message must end up deleted")
	}
	if n := f.count("SELECT count(*) FROM pending_changes"); n != 0 {
		t.Fatal("the parked change is cleared")
	}
}

func TestParkedChangesArePurgedOnConnect(t *testing.T) {
	f := newFixture(t)
	i, _ := newIngester(f, f.gateway)
	f.apply(Record{Op: OpRevoke, Chat: dm, Sender: rahul, WAID: "REV1", At: sentAt, Source: SourceLive, Target: Target{WAID: "NEVER", Sender: rahul}})
	if err := f.admin.Query(context.Background(), "UPDATE pending_changes SET created_at = now() - interval '8 days' RETURNING 1", nil, new(int)); err != nil {
		t.Fatal(err)
	}
	if !i.Handle(context.Background(), own, &events.Connected{}) || f.count("SELECT count(*) FROM pending_changes") != 0 {
		t.Fatal("connecting drops changes that waited over a week")
	}
}

func TestARefusedAnnouncementIsKept(t *testing.T) {
	f := newFixture(t)
	i, _ := newIngester(f, f.gateway)
	if !i.Handle(context.Background(), own, announcement(t, "HIST\x00BAD", 10)) {
		t.Fatal("acknowledged once a copy is kept")
	}
	if n := f.count("SELECT count(*) FROM ingest_dead WHERE error_code = 'history_22021' AND length(raw) > 0"); n != 1 {
		t.Fatalf("the announcement must be kept as a dead letter, got %d", n)
	}
}

func TestAWaitingJobDoesNotBlockTheOnesBehindIt(t *testing.T) {
	f := newFixture(t)
	i, _ := newIngester(f, f.gateway)
	ctx := context.Background()
	i.Handle(ctx, own, announcement(t, "HIST1", 10))
	i.Handle(ctx, own, announcement(t, "HIST2", 20))
	first := nextJob(t, i)
	second, err := i.NextHistoryJob(ctx, []int64{first.ID})
	if err != nil || second == nil || second.WAID != "HIST2" {
		t.Fatalf("got %+v, %v", second, err)
	}
	if none, _ := i.NextHistoryJob(ctx, []int64{first.ID, second.ID}); none != nil {
		t.Fatalf("got %+v", none)
	}
}
