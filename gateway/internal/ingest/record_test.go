package ingest

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waVnameCert"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

const (
	ownPN    = "919000000000@s.whatsapp.net"
	ownLID   = "100000000000099@lid"
	rahulPN  = "911111111111@s.whatsapp.net"
	rahulLID = "100000000000001@lid"
	priyaLID = "100000000000002@lid"
	groupJID = "120363000000000000@g.us"
)

var (
	own    = Own{PN: ownPN, LID: ownLID}
	sentAt = time.Date(2026, 11, 22, 9, 30, 0, 0, time.UTC)
)

func jid(t *testing.T, raw string) types.JID {
	t.Helper()
	j, err := types.ParseJID(raw)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// dmFrom is a direct message from Rahul, addressed by LID with his phone number as the alternative.
func dmFrom(t *testing.T, msg *waE2E.Message) *events.Message {
	t.Helper()
	sender := jid(t, "100000000000001:7@lid")
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat: sender.ToNonAD(), Sender: sender, SenderAlt: jid(t, rahulPN),
			},
			ID: "3EB0AAAA", Timestamp: sentAt, PushName: "Rahul",
		},
		Message: msg,
	}
}

// dmFromMe is a direct message DJ sent to Rahul from his phone.
func dmFromMe(t *testing.T, msg *waE2E.Message) *events.Message {
	t.Helper()
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat: jid(t, rahulLID), Sender: jid(t, "919000000000:3@s.whatsapp.net"), IsFromMe: true,
				RecipientAlt: jid(t, rahulPN),
			},
			ID: "3EB0BBBB", Timestamp: sentAt,
		},
		Message: msg,
	}
}

func groupFrom(t *testing.T, sender string, msg *waE2E.Message) *events.Message {
	t.Helper()
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: jid(t, groupJID), Sender: jid(t, sender), IsGroup: true},
			ID:            "3EB0CCCC", Timestamp: sentAt,
		},
		Message: msg,
	}
}

func textMsg(s string) *waE2E.Message { return &waE2E.Message{Conversation: proto.String(s)} }

func body(rec Record) string {
	if rec.Body == nil {
		return "<nil>"
	}
	return *rec.Body
}

func TestDirectMessageKnowsBothSpellingsOfThePerson(t *testing.T) {
	rec := FromMessage(dmFrom(t, textMsg("  Saturday 7pm works \n")), own, SourceLive)
	rahul := Address{LID: rahulLID, PN: rahulPN}
	if rec.Op != OpStore || rec.Kind != "text" || body(rec) != "Saturday 7pm works" {
		t.Fatalf("got %+v", rec)
	}
	if rec.Chat != (ChatRef{Kind: ChatDirect, Contact: rahul}) || rec.Sender != rahul || rec.FromMe {
		t.Fatalf("addressing: %+v", rec)
	}
	if rec.WAID != "3EB0AAAA" || !rec.At.Equal(sentAt) || rec.Source != SourceLive || rec.PushName != "Rahul" {
		t.Fatalf("metadata: %+v", rec)
	}
}

func TestOwnMessagesAreSentByTheOwnAccountInThePeersChat(t *testing.T) {
	rec := FromMessage(dmFromMe(t, textMsg("on my way")), own, SourceLive)
	if rec.Sender != own || !rec.FromMe {
		t.Fatalf("sender: %+v", rec)
	}
	if rec.Chat.Contact != (Address{LID: rahulLID, PN: rahulPN}) {
		t.Fatalf("chat: %+v", rec.Chat)
	}
}

func TestSelfChatIsADirectChatWithTheOwnAccount(t *testing.T) {
	evt := dmFromMe(t, textMsg("/help"))
	evt.Info.Chat = jid(t, ownPN)
	evt.Info.RecipientAlt = types.EmptyJID
	rec := FromMessage(evt, own, SourceLive)
	if rec.Chat != (ChatRef{Kind: ChatDirect, Contact: own}) || rec.Sender != own {
		t.Fatalf("got %+v", rec)
	}
}

func TestGroupMessage(t *testing.T) {
	rec := FromMessage(groupFrom(t, "100000000000002:4@lid", textMsg("hi all")), own, SourceLive)
	if rec.Chat != (ChatRef{Kind: ChatGroup, JID: groupJID}) || rec.Sender != (Address{LID: priyaLID}) {
		t.Fatalf("got %+v", rec)
	}
}

func TestKinds(t *testing.T) {
	poll := &waE2E.PollCreationMessage{Name: proto.String("Dinner where?")}
	for name, tc := range map[string]struct {
		msg  *waE2E.Message
		kind string
		body string
	}{
		"extended text": {&waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("a link")}}, "text", "a link"},
		"image":         {&waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("the venue")}}, "image", "the venue"},
		"image bare":    {&waE2E.Message{ImageMessage: &waE2E.ImageMessage{}}, "image", "<nil>"},
		"video":         {&waE2E.Message{VideoMessage: &waE2E.VideoMessage{Caption: proto.String("clip")}}, "video", "clip"},
		"video note":    {&waE2E.Message{PtvMessage: &waE2E.VideoMessage{}}, "video", "<nil>"},
		"voice":         {&waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: proto.Bool(true)}}, "voice", "<nil>"},
		"audio":         {&waE2E.Message{AudioMessage: &waE2E.AudioMessage{}}, "audio", "<nil>"},
		"document":      {&waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{Caption: proto.String("the deck"), FileName: proto.String("deck.pdf")}}, "document", "the deck"},
		"sticker":       {&waE2E.Message{StickerMessage: &waE2E.StickerMessage{}}, "sticker", "<nil>"},
		"location": {&waE2E.Message{LocationMessage: &waE2E.LocationMessage{
			Name: proto.String("Cafe Blue"), Address: proto.String("MG Road"),
			DegreesLatitude: proto.Float64(12.97), DegreesLongitude: proto.Float64(77.59),
		}}, "location", "Cafe Blue, MG Road"},
		"live location": {&waE2E.Message{LiveLocationMessage: &waE2E.LiveLocationMessage{Caption: proto.String("on the way")}}, "location", "on the way"},
		"contact": {&waE2E.Message{ContactMessage: &waE2E.ContactMessage{
			DisplayName: proto.String("Priya"), Vcard: proto.String("TEL:+919999999999"),
		}}, "contact", "Priya"},
		"contacts": {&waE2E.Message{ContactsArrayMessage: &waE2E.ContactsArrayMessage{DisplayName: proto.String("2 contacts")}}, "contact", "2 contacts"},
		"poll v1":  {&waE2E.Message{PollCreationMessage: poll}, "poll", "Dinner where?"},
		"poll v3":  {&waE2E.Message{PollCreationMessageV3: poll}, "poll", "Dinner where?"},
		"poll v4": {&waE2E.Message{PollCreationMessageV4: &waE2E.FutureProofMessage{
			Message: &waE2E.Message{PollCreationMessage: poll},
		}}, "poll", "Dinner where?"},
		"poll v6":      {&waE2E.Message{PollCreationMessageV6: poll}, "poll", "Dinner where?"},
		"unknown type": {&waE2E.Message{GroupInviteMessage: &waE2E.GroupInviteMessage{}}, "other", "<nil>"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := FromMessage(dmFrom(t, tc.msg), own, SourceLive)
			if rec.Op != OpStore || rec.Kind != tc.kind || body(rec) != tc.body {
				t.Fatalf("got op=%d kind=%s body=%s (%s)", rec.Op, rec.Kind, body(rec), rec.SkipReason)
			}
		})
	}
}

func TestSkips(t *testing.T) {
	status := dmFrom(t, textMsg("my day"))
	status.Info.Chat = types.StatusBroadcastJID
	status.Info.IsGroup = true
	newsletter := dmFrom(t, textMsg("news"))
	newsletter.Info.Chat = jid(t, "120363111111111111@newsletter")
	peer := dmFromMe(t, textMsg("sync"))
	peer.Info.Category = "peer"
	skdm := &waE2E.SenderKeyDistributionMessage{GroupID: proto.String(groupJID)}

	for name, evt := range map[string]*events.Message{
		"status":         status,
		"newsletter":     newsletter,
		"peer":           peer,
		"nil message":    dmFrom(t, nil),
		"key share only": groupFrom(t, rahulLID, &waE2E.Message{SenderKeyDistributionMessage: skdm, MessageContextInfo: &waE2E.MessageContextInfo{}}),
		"poll vote":      dmFrom(t, &waE2E.Message{PollUpdateMessage: &waE2E.PollUpdateMessage{}}),
		"pin":            dmFrom(t, &waE2E.Message{PinInChatMessage: &waE2E.PinInChatMessage{}}),
		"keep in chat":   dmFrom(t, &waE2E.Message{KeepInChatMessage: &waE2E.KeepInChatMessage{}}),
		"enc reaction":   dmFrom(t, &waE2E.Message{EncReactionMessage: &waE2E.EncReactionMessage{}}),
		"other protocol": dmFrom(t, &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
			Type: waE2E.ProtocolMessage_EPHEMERAL_SETTING.Enum(),
		}}),
	} {
		t.Run(name, func(t *testing.T) {
			if rec := FromMessage(evt, own, SourceLive); rec.Op != OpSkip || rec.SkipReason == "" {
				t.Fatalf("want a skip with a reason, got %+v", rec)
			}
		})
	}
}

func TestKeyShareWithContentIsStored(t *testing.T) {
	msg := textMsg("first message in the group")
	msg.SenderKeyDistributionMessage = &waE2E.SenderKeyDistributionMessage{}
	if rec := FromMessage(groupFrom(t, rahulLID, msg), own, SourceLive); rec.Op != OpStore || rec.Kind != "text" {
		t.Fatalf("got %+v", rec)
	}
}

func TestReplyExpiryAndBusinessName(t *testing.T) {
	evt := groupFrom(t, rahulLID, &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text: proto.String("yes"),
		ContextInfo: &waE2E.ContextInfo{
			StanzaID: proto.String("3EB0QUOTED"), Participant: proto.String("100000000000002:5@lid"),
			Expiration: proto.Uint32(86400),
		},
	}})
	evt.Info.VerifiedName = &types.VerifiedName{Details: &waVnameCert.VerifiedNameCertificate_Details{VerifiedName: proto.String("Blue Cafe")}}
	rec := FromMessage(evt, own, SourceLive)
	if rec.ReplyTo == nil || *rec.ReplyTo != (Target{WAID: "3EB0QUOTED", Sender: Address{LID: priyaLID}}) {
		t.Fatalf("reply: %+v", rec.ReplyTo)
	}
	if rec.ExpiresAt == nil || !rec.ExpiresAt.Equal(sentAt.Add(24*time.Hour)) {
		t.Fatalf("expiry: %v", rec.ExpiresAt)
	}
	if rec.BusinessName != "Blue Cafe" {
		t.Fatalf("business name: %q", rec.BusinessName)
	}
	if rec.Forwarded {
		t.Fatal("a reply the sender wrote is not a forward")
	}
	for name, ctx := range map[string]*waE2E.ContextInfo{
		"flagged": {IsForwarded: proto.Bool(true)},
		"scored":  {ForwardingScore: proto.Uint32(3)},
	} {
		forwarded := groupFrom(t, rahulLID, &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String("pay 500 to this number"), ContextInfo: ctx,
		}})
		if !FromMessage(forwarded, own, SourceLive).Forwarded {
			t.Fatalf("%s: a forwarded message must be marked as one", name)
		}
	}

	noSender := dmFrom(t, &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text: proto.String("yes"), ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("3EB0QUOTED")},
	}})
	if rec := FromMessage(noSender, own, SourceLive); rec.ReplyTo != nil {
		t.Fatal("a reply without a known sender is stored without the link")
	}
}

func TestViewOnceKeepsNoContent(t *testing.T) {
	evt := dmFrom(t, &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("secret")}})
	evt.IsViewOnce = true
	rec := FromMessage(evt, own, SourceLive)
	if rec.Op != OpStore || rec.Kind != "image" || !rec.ViewOnce || rec.Body != nil {
		t.Fatalf("got %+v", rec)
	}

	placeholder := &events.UndecryptableMessage{Info: evt.Info, IsUnavailable: true, UnavailableType: events.UnavailableTypeViewOnce}
	rec = FromUndecryptable(placeholder, own)
	if rec.Op != OpStore || rec.Kind != "other" || !rec.ViewOnce || rec.Body != nil || rec.Source != SourceLive {
		t.Fatalf("got %+v", rec)
	}
	if rec := FromUndecryptable(&events.UndecryptableMessage{Info: evt.Info}, own); rec.Op != OpSkip {
		t.Fatal("other undecryptable messages are skipped")
	}
}

func TestEditTargetsTheSendersOwnMessage(t *testing.T) {
	edit := dmFrom(t, &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type:          waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
		Key:           &waCommon.MessageKey{ID: proto.String("3EB0ORIG"), FromMe: proto.Bool(false), Participant: proto.String(ownPN)},
		EditedMessage: textMsg("Saturday 8pm"),
		TimestampMS:   proto.Int64(sentAt.Add(time.Minute).UnixMilli()),
	}})
	rec := FromMessage(edit, own, SourceLive)
	if rec.Op != OpEdit || body(rec) != "Saturday 8pm" || !rec.At.Equal(sentAt.Add(time.Minute)) {
		t.Fatalf("got %+v", rec)
	}
	if rec.Target != (Target{WAID: "3EB0ORIG", Sender: Address{LID: rahulLID, PN: rahulPN}}) {
		t.Fatalf("an edit can only target the editor's own message, got %+v", rec.Target)
	}
}

func TestRevokes(t *testing.T) {
	revoke := func(key *waCommon.MessageKey) *waE2E.Message {
		return &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: key}}
	}
	own1 := FromMessage(groupFrom(t, rahulLID, revoke(&waCommon.MessageKey{
		ID: proto.String("3EB0ORIG"), Participant: proto.String(priyaLID),
	})), own, SourceLive)
	if own1.Op != OpRevoke || own1.Target != (Target{WAID: "3EB0ORIG", Sender: Address{LID: rahulLID}}) {
		t.Fatalf("a sender revoke only reaches the revoker's own message, got %+v", own1)
	}

	admin := groupFrom(t, rahulLID, revoke(&waCommon.MessageKey{ID: proto.String("3EB0ORIG"), Participant: proto.String(priyaLID)}))
	admin.Info.Edit = types.EditAttributeAdminRevoke
	rec := FromMessage(admin, own, SourceLive)
	if rec.Op != OpRevoke || rec.Target.Sender != (Address{LID: priyaLID}) {
		t.Fatalf("an admin revoke targets the participant, got %+v", rec)
	}

	admin.Message = revoke(&waCommon.MessageKey{ID: proto.String("3EB0ORIG")})
	if rec := FromMessage(admin, own, SourceLive); rec.Op != OpSkip {
		t.Fatalf("an admin revoke without a participant is skipped, got %+v", rec)
	}
}

func TestReactionTargets(t *testing.T) {
	react := func(emoji string, key *waCommon.MessageKey) *waE2E.Message {
		return &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
			Key: key, Text: proto.String(emoji), SenderTimestampMS: proto.Int64(sentAt.Add(time.Second).UnixMilli()),
		}}
	}
	key := func(fromMe bool, participant string) *waCommon.MessageKey {
		k := &waCommon.MessageKey{ID: proto.String("3EB0ORIG"), FromMe: proto.Bool(fromMe)}
		if participant != "" {
			k.Participant = proto.String(participant)
		}
		return k
	}
	rahul := Address{LID: rahulLID, PN: rahulPN}

	for name, tc := range map[string]struct {
		evt  *events.Message
		want Address
	}{
		"rahul reacts to his own message":  {dmFrom(t, react("👍", key(true, ""))), rahul},
		"rahul reacts to DJ's message":     {dmFrom(t, react("👍", key(false, ""))), own},
		"DJ reacts to rahul's message":     {dmFromMe(t, react("👍", key(false, ""))), rahul},
		"DJ reacts to his own message":     {dmFromMe(t, react("👍", key(true, ""))), own},
		"group: reacts to someone else's":  {groupFrom(t, rahulLID, react("👍", key(false, "100000000000002:5@lid"))), Address{LID: priyaLID}},
		"group: reacts to their own":       {groupFrom(t, rahulLID, react("👍", key(true, ""))), Address{LID: rahulLID}},
		"group: reacts to DJ's by his LID": {groupFrom(t, rahulLID, react("👍", key(false, ownLID))), Address{LID: ownLID}},
	} {
		t.Run(name, func(t *testing.T) {
			rec := FromMessage(tc.evt, own, SourceLive)
			if rec.Op != OpReact || rec.Emoji != "👍" || rec.Target != (Target{WAID: "3EB0ORIG", Sender: tc.want}) {
				t.Fatalf("got %+v", rec)
			}
			if !rec.At.Equal(sentAt.Add(time.Second)) {
				t.Fatalf("reaction time: %v", rec.At)
			}
		})
	}

	removed := FromMessage(dmFrom(t, react("", key(true, ""))), own, SourceLive)
	if removed.Op != OpReact || removed.Emoji != "" {
		t.Fatalf("removing a reaction is a reaction with no emoji, got %+v", removed)
	}
	if rec := FromMessage(groupFrom(t, rahulLID, react("👍", key(false, ""))), own, SourceLive); rec.Op != OpSkip {
		t.Fatalf("a group reaction without a participant has no target, got %+v", rec)
	}
}

func TestBroadcastListAndBotChats(t *testing.T) {
	list := dmFromMe(t, textMsg("party on Saturday"))
	list.Info.Chat = jid(t, "1700000000@broadcast")
	list.Info.IsGroup = true
	if rec := FromMessage(list, own, SourceLive); rec.Chat != (ChatRef{Kind: ChatBroadcast, JID: "1700000000@broadcast"}) {
		t.Fatalf("got %+v", rec.Chat)
	}

	bot := dmFrom(t, textMsg("hello from the bot"))
	bot.Info.Chat = jid(t, "13135550002@bot")
	bot.Info.Sender = bot.Info.Chat
	bot.Info.SenderAlt = types.EmptyJID
	want := Address{Bot: "13135550002@bot"}
	if rec := FromMessage(bot, own, SourceLive); rec.Chat.Contact != want || rec.Sender != want {
		t.Fatalf("got %+v", rec)
	}
}

func mediaFixture() (key, encHash, hash []byte) {
	return bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32), bytes.Repeat([]byte{0xab}, 32)
}

func TestMediaIsCapturedForFilesOnly(t *testing.T) {
	key, encHash, hash := mediaFixture()
	hexHash := strings.Repeat("ab", 32)
	voice := &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
		PTT: proto.Bool(true), Mimetype: proto.String("audio/ogg; codecs=opus"), FileLength: proto.Uint64(4096), Seconds: proto.Uint32(7),
		MediaKey: key, FileEncSHA256: encHash, FileSHA256: hash, DirectPath: proto.String("/v/t62/voice"),
	}}
	rec := FromMessage(dmFrom(t, voice), own, SourceLive)
	want := Media{
		MimeType: "audio/ogg; codecs=opus", Size: 4096, DurationS: 7, SHA256: hexHash, Type: "audio",
		Key: key, DirectPath: "/v/t62/voice", EncSHA256: encHash,
	}
	if rec.Kind != "voice" || rec.Media == nil || !reflect.DeepEqual(*rec.Media, want) || !rec.Media.Fetchable() {
		t.Fatalf("got %+v", rec.Media)
	}

	doc := FromMessage(dmFrom(t, &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
		FileName: proto.String(" ../../deck.pdf "), Mimetype: proto.String("application/pdf"), FileSHA256: []byte{1, 2},
	}}), own, SourceLive)
	if doc.Media == nil || doc.Media.Type != "document" || doc.Media.FileName != "../../deck.pdf" || doc.Media.SHA256 != "" || doc.Media.Fetchable() {
		t.Fatalf("a document keeps its name as data; a malformed hash is dropped and it is not fetchable: %+v", doc.Media)
	}

	video := FromMessage(dmFrom(t, &waE2E.Message{PtvMessage: &waE2E.VideoMessage{Width: proto.Uint32(640), Height: proto.Uint32(640), Seconds: proto.Uint32(12)}}), own, SourceLive)
	if video.Media == nil || video.Media.Type != "video" || video.Media.Width != 640 || video.Media.DurationS != 12 {
		t.Fatalf("got %+v", video.Media)
	}

	for name, msg := range map[string]*waE2E.Message{
		"text":    textMsg("hi"),
		"sticker": {StickerMessage: &waE2E.StickerMessage{MediaKey: key, DirectPath: proto.String("/v/t62/s")}},
		"contact": {ContactMessage: &waE2E.ContactMessage{DisplayName: proto.String("Priya")}},
	} {
		if rec := FromMessage(dmFrom(t, msg), own, SourceLive); rec.Media != nil {
			t.Fatalf("%s must have no media, got %+v", name, rec.Media)
		}
	}
}

func TestViewOnceNeverKeepsKeys(t *testing.T) {
	key, encHash, hash := mediaFixture()
	image := &waE2E.ImageMessage{MediaKey: key, FileEncSHA256: encHash, FileSHA256: hash, DirectPath: proto.String("/v/t62/i"), Caption: proto.String("secret")}

	wrapped := dmFrom(t, &waE2E.Message{ImageMessage: image})
	wrapped.IsViewOnce = true
	if rec := FromMessage(wrapped, own, SourceLive); !rec.ViewOnce || rec.Media != nil || rec.Body != nil {
		t.Fatalf("a view-once wrapper keeps no keys and no caption: %+v", rec)
	}

	flagged := proto.Clone(image).(*waE2E.ImageMessage)
	flagged.ViewOnce = proto.Bool(true)
	if rec := FromMessage(dmFrom(t, &waE2E.Message{ImageMessage: flagged}), own, SourceLive); !rec.ViewOnce || rec.Media != nil || rec.Body != nil {
		t.Fatalf("the inner view-once flag counts too: %+v", rec)
	}
}
