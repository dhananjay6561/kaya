package ingest

import (
	"errors"
	"testing"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

const sentAtUnix uint64 = 1795339800

// fakeParser does what whatsmeow's ParseWebMessage does for the fields the mapper reads.
type fakeParser struct{}

func (fakeParser) ParseWebMessage(chat types.JID, web *waWeb.WebMessageInfo) (*events.Message, error) {
	if web.GetKey().GetID() == "UNPARSEABLE" {
		return nil, errors.New("no sender")
	}
	info := types.MessageInfo{
		MessageSource: types.MessageSource{Chat: chat, IsFromMe: web.GetKey().GetFromMe(), IsGroup: chat.Server == types.GroupServer},
		ID:            web.GetKey().GetID(),
		PushName:      web.GetPushName(),
		Timestamp:     sentAt,
	}
	switch {
	case info.IsFromMe:
		info.Sender, _ = types.ParseJID(ownPN)
	case info.IsGroup:
		info.Sender, _ = types.ParseJID(web.GetParticipant())
	default:
		info.Sender = chat
	}
	evt := &events.Message{Info: info, RawMessage: web.GetMessage(), SourceWebMsg: web}
	evt.UnwrapRaw()
	return evt, nil
}

func historyMsg(id string, fromMe bool, participant string, msg *waE2E.Message) *waHistorySync.HistorySyncMsg {
	web := &waWeb.WebMessageInfo{
		Key:              &waCommon.MessageKey{ID: proto.String(id), FromMe: proto.Bool(fromMe)},
		Message:          msg,
		MessageTimestamp: proto.Uint64(sentAtUnix),
	}
	if participant != "" {
		web.Participant = proto.String(participant)
	}
	return &waHistorySync.HistorySyncMsg{Message: web}
}

func TestFromHistory(t *testing.T) {
	data := &waHistorySync.HistorySync{
		Conversations: []*waHistorySync.Conversation{
			{ID: proto.String(rahulPN), Messages: []*waHistorySync.HistorySyncMsg{
				historyMsg("H1", false, "", textMsg("old message")),
				historyMsg("H2", true, "", textMsg("my old reply")),
				historyMsg("UNPARSEABLE", false, "", textMsg("x")),
				historyMsg("H3", false, "", nil),
				historyMsg("H4", false, "", &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_EPHEMERAL_SETTING.Enum()}}),
			}},
			{ID: proto.String(groupJID), Name: proto.String(" Trip planning "), Messages: []*waHistorySync.HistorySyncMsg{
				historyMsg("G1", false, priyaLID, textMsg("who is in?")),
			}},
			{ID: proto.String("status@broadcast"), Messages: []*waHistorySync.HistorySyncMsg{historyMsg("S1", false, rahulPN, textMsg("my day"))}},
			{ID: proto.String("120363111111111111@newsletter"), Messages: []*waHistorySync.HistorySyncMsg{historyMsg("N1", false, "", textMsg("news"))}},
			{ID: proto.String("not a jid")},
		},
		PhoneNumberToLidMappings: []*waHistorySync.PhoneNumberToLIDMapping{
			{PnJID: proto.String(rahulPN), LidJID: proto.String(rahulLID)},
			{PnJID: proto.String(rahulPN)},
		},
		Pushnames: []*waHistorySync.Pushname{
			{ID: proto.String(rahulPN), Pushname: proto.String("Rahul")},
			{ID: proto.String("911234567890@s.whatsapp.net"), Pushname: proto.String("-")},
		},
	}

	chunk := FromHistory(data, fakeParser{}, own)

	if chunk.Conversations != 2 {
		t.Fatalf("want 2 conversations, got %d", chunk.Conversations)
	}
	if len(chunk.Dead) != 1 || chunk.Dead[0].WAID != "UNPARSEABLE" || chunk.Dead[0].ErrorCode != "unparseable" ||
		chunk.Dead[0].ChatJID != rahulPN || len(chunk.Dead[0].Raw) == 0 || string(chunk.Dead[0].Info) != `{"source":"history"}` {
		t.Fatalf("a message that cannot be parsed becomes a dead letter: %+v", chunk.Dead)
	}
	var ids []string
	for _, rec := range chunk.Records {
		if rec.Op != OpStore || rec.Source != SourceHistory || !rec.At.Equal(sentAt) || rec.raw == nil {
			t.Fatalf("every history record is a stored message from history: %+v", rec)
		}
		ids = append(ids, rec.WAID)
	}
	if len(ids) != 3 || ids[0] != "H1" || ids[1] != "H2" || ids[2] != "G1" {
		t.Fatalf("got records %v", ids)
	}
	if chunk.Records[1].Sender != own || !chunk.Records[1].FromMe {
		t.Fatalf("own history messages are sent by the own account: %+v", chunk.Records[1])
	}
	if chunk.Records[2].Chat != (ChatRef{Kind: ChatGroup, JID: groupJID}) || chunk.Records[2].Sender != (Address{LID: priyaLID}) {
		t.Fatalf("group record: %+v", chunk.Records[2])
	}
	if len(chunk.Names) != 1 || chunk.Names[0] != (ChatName{Chat: ChatRef{Kind: ChatGroup, JID: groupJID}, Name: "Trip planning"}) {
		t.Fatalf("names: %+v", chunk.Names)
	}
	if len(chunk.Mappings) != 1 || chunk.Mappings[0] != (Address{LID: rahulLID, PN: rahulPN}) {
		t.Fatalf("mappings: %+v", chunk.Mappings)
	}
}
