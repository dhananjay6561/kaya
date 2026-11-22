package ingest

import (
	"strings"

	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Parser turns a message from a history sync into the event the live mapper takes. whatsmeow's
// client implements it.
type Parser interface {
	ParseWebMessage(chatJID types.JID, webMsg *waWeb.WebMessageInfo) (*events.Message, error)
}

// ChatName is a name a history sync gives for a group or broadcast list. History carries no time
// for it, so it only ever fills a missing name.
type ChatName struct {
	Chat ChatRef
	Name string
}

// HistoryChunk is one history sync payload reduced to what gets stored.
type HistoryChunk struct {
	Records []Record
	Names   []ChatName
	// Mappings link a LID to its phone number, so a person addressed either way is one contact.
	Mappings []Address
	// Dead are messages that could not be mapped; they are kept as dead letters.
	Dead []DeadLetter
	// Conversations is how many chats the payload carried.
	Conversations int
}

var chatServers = map[string]bool{
	types.DefaultUserServer: true, types.HiddenUserServer: true, types.HostedServer: true, types.HostedLIDServer: true,
	types.BotServer: true, types.GroupServer: true, types.BroadcastServer: true,
}

var historyInfo = []byte(`{"source":"history"}`)

// FromHistory maps a history sync. Status posts are left out, like live ones. Every record has
// SourceHistory, so storing it emits no per-message event. Push names are not taken from here:
// whatsmeow keeps them in its contact store, which is copied after the job.
func FromHistory(data *waHistorySync.HistorySync, parser Parser, own Own) HistoryChunk {
	var chunk HistoryChunk
	for _, mapping := range data.GetPhoneNumberToLidMappings() {
		a := mergeAddress(parseAddress(mapping.GetLidJID()), parseAddress(mapping.GetPnJID()))
		if a.LID != "" && a.PN != "" {
			chunk.Mappings = append(chunk.Mappings, a)
		}
	}
	for _, conv := range data.GetConversations() {
		chat, err := types.ParseJID(conv.GetID())
		if err != nil || chat.User == "" || chat == types.StatusBroadcastJID || !chatServers[chat.Server] {
			continue
		}
		chunk.Conversations++
		if name := strings.TrimSpace(conv.GetName()); name != "" {
			switch chat.Server {
			case types.GroupServer:
				chunk.Names = append(chunk.Names, ChatName{Chat: ChatRef{Kind: ChatGroup, JID: chat.ToNonAD().String()}, Name: name})
			case types.BroadcastServer:
				chunk.Names = append(chunk.Names, ChatName{Chat: ChatRef{Kind: ChatBroadcast, JID: chat.ToNonAD().String()}, Name: name})
			}
		}
		alt := otherSpelling(chat, conv)
		for _, item := range conv.GetMessages() {
			web := item.GetMessage()
			if web == nil || web.GetMessage() == nil {
				continue
			}
			rec, code := historyRecord(chat, alt, web, parser, own)
			switch {
			case code != "":
				d := DeadLetter{WAID: web.GetKey().GetID(), ChatJID: chat.ToNonAD().String(), ErrorCode: code, Info: historyInfo}
				d.Raw, _ = proto.Marshal(web)
				chunk.Dead = append(chunk.Dead, d)
			case rec.Op == OpStore:
				rec.raw = web
				chunk.Records = append(chunk.Records, rec)
				chunk.Records = append(chunk.Records, historyReactions(rec, web, own)...)
			}
		}
	}
	return chunk
}

// otherSpelling returns the second address history gives for a direct chat: the phone number of a
// chat keyed by LID, or the LID of one keyed by number.
func otherSpelling(chat types.JID, conv *waHistorySync.Conversation) types.JID {
	raw := conv.GetPnJID()
	if chat.Server == types.DefaultUserServer || chat.Server == types.HostedServer || chat.Server == types.LegacyUserServer {
		raw = conv.GetLidJID()
	}
	if raw == "" || chat.Server == types.GroupServer || chat.Server == types.BroadcastServer {
		return types.EmptyJID
	}
	alt, err := types.ParseJID(raw)
	if err != nil {
		return types.EmptyJID
	}
	return alt.ToNonAD()
}

func historyRecord(chat, alt types.JID, web *waWeb.WebMessageInfo, parser Parser, own Own) (rec Record, code string) {
	defer func() {
		if r := recover(); r != nil {
			code = "mapper_panic"
		}
	}()
	evt, err := parser.ParseWebMessage(chat, web)
	if err != nil {
		return Record{}, "unparseable"
	}
	if !alt.IsEmpty() {
		if evt.Info.IsFromMe {
			evt.Info.RecipientAlt = alt
		} else {
			evt.Info.SenderAlt = alt
		}
	}
	return FromMessage(evt, own, SourceHistory), ""
}

// historyReactions maps the reactions history attaches to a message. Live reactions arrive as
// messages of their own; history folds them into the message they belong to.
func historyReactions(target Record, web *waWeb.WebMessageInfo, own Own) []Record {
	var out []Record
	for _, reaction := range web.GetReactions() {
		key := reaction.GetKey()
		if reaction.GetText() == "" {
			continue
		}
		var sender Address
		switch {
		case key.GetFromMe():
			sender = own
		case target.Chat.Kind == ChatDirect:
			sender = target.Chat.Contact
		default:
			sender = parseAddress(key.GetParticipant())
		}
		if sender.IsZero() {
			continue
		}
		out = append(out, Record{
			Op: OpReact, Chat: target.Chat, Sender: sender, WAID: key.GetID(), FromMe: key.GetFromMe(),
			At: saneTime(reaction.GetSenderTimestampMS(), target.At), Source: SourceHistory,
			Emoji: reaction.GetText(), Target: Target{WAID: target.WAID, Sender: target.Sender},
		})
	}
	return out
}
