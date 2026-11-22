// Package ingest turns WhatsApp events into rows: contacts, chats, messages and reactions, plus the
// event that tells core about each change.
package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Address is every way WhatsApp is known to address one person or bot, as device-less JIDs.
type Address struct {
	LID string
	PN  string
	Bot string
}

// IsZero reports whether no address is known.
func (a Address) IsZero() bool { return a == Address{} }

// Own is the linked account's own addresses.
type Own = Address

// Chat kinds, as stored in chats.kind.
const (
	ChatDirect    = "direct"
	ChatGroup     = "group"
	ChatBroadcast = "broadcast"
)

// ChatRef identifies a chat: a direct chat by the other person, anything else by its JID.
type ChatRef struct {
	Kind    string
	JID     string
	Contact Address
}

// Target points at an earlier message: the one replied to, edited, revoked or reacted to.
type Target struct {
	WAID   string
	Sender Address
}

// Op is what a record asks the store to do.
type Op int

// The operations a record can ask for.
const (
	OpSkip Op = iota
	OpStore
	OpEdit
	OpRevoke
	OpReact
)

// Message sources, as stored in messages.source.
const (
	SourceLive    = "live"
	SourceHistory = "history"
)

// Record is one WhatsApp event reduced to what gets stored.
type Record struct {
	Op         Op
	SkipReason string

	Chat   ChatRef
	Sender Address
	WAID   string
	FromMe bool
	At     time.Time
	Source string

	Kind      string
	Body      *string
	ReplyTo   *Target
	ViewOnce  bool
	ExpiresAt *time.Time

	PushName     string
	BusinessName string

	// Media describes the message's file, for kinds that have one. Nil for view-once and stickers.
	Media *Media

	Target Target
	Emoji  string

	// raw is the message as WhatsApp sent it, kept only to make a dead letter if storing fails.
	raw proto.Message
}

// Media is what WhatsApp says about a file, plus what it takes to fetch it.
type Media struct {
	MimeType  string
	Size      int64
	FileName  string
	DurationS int
	Width     int
	Height    int
	// SHA256 is the hash of the file itself, in hex; empty when WhatsApp sent none or a malformed one.
	SHA256 string

	// Type is image, video, audio or document: which of WhatsApp's key derivations the file uses.
	Type       string
	Key        []byte
	DirectPath string
	EncSHA256  []byte
}

// Fetchable reports whether everything needed to download the file is known.
func (m *Media) Fetchable() bool {
	return m != nil && len(m.Key) > 0 && m.DirectPath != "" && len(m.EncSHA256) > 0 && m.SHA256 != ""
}

func skip(reason string) Record { return Record{Op: OpSkip, SkipReason: reason} }

// FromMessage maps a decrypted message. source is SourceLive or SourceHistory.
func FromMessage(evt *events.Message, own Own, source string) Record {
	info := evt.Info
	switch {
	case info.Chat.Server == types.NewsletterServer:
		return skip("newsletter")
	case info.Chat == types.StatusBroadcastJID:
		return skip("status")
	case info.Category == "peer":
		return skip("peer")
	case evt.Message == nil:
		return skip("empty")
	}

	rec := Record{
		Chat:   chatRef(info, own),
		Sender: senderOf(info, own),
		WAID:   info.ID,
		FromMe: info.IsFromMe,
		At:     info.Timestamp,
		Source: source,
	}
	if rec.Chat.Kind == "" || rec.Sender.IsZero() || rec.WAID == "" || rec.At.IsZero() {
		return skip("unaddressed")
	}
	if name := strings.TrimSpace(info.PushName); name != "" && name != "-" {
		rec.PushName = name
	}
	if info.VerifiedName != nil {
		rec.BusinessName = strings.TrimSpace(info.VerifiedName.Details.GetVerifiedName())
	}

	msg := evt.Message
	if reaction := msg.GetReactionMessage(); reaction != nil {
		rec.Op = OpReact
		rec.Emoji = reaction.GetText()
		rec.Target = Target{WAID: reaction.GetKey().GetID(), Sender: keySender(reaction.GetKey(), rec, own)}
		rec.At = saneTime(reaction.GetSenderTimestampMS(), rec.At)
		return checkTarget(rec)
	}
	if protocol := msg.GetProtocolMessage(); protocol != nil {
		switch protocol.GetType() {
		case waE2E.ProtocolMessage_REVOKE:
			rec.Op = OpRevoke
			rec.Target = Target{WAID: protocol.GetKey().GetID(), Sender: rec.Sender}
			if info.Edit == types.EditAttributeAdminRevoke {
				rec.Target.Sender = parseAddress(protocol.GetKey().GetParticipant())
			}
			return checkTarget(rec)
		case waE2E.ProtocolMessage_MESSAGE_EDIT:
			rec.Op = OpEdit
			rec.Target = Target{WAID: protocol.GetKey().GetID(), Sender: rec.Sender}
			_, rec.Body = content(protocol.GetEditedMessage())
			rec.At = saneTime(protocol.GetTimestampMS(), rec.At)
			return checkTarget(rec)
		default:
			return skip("protocol")
		}
	}
	switch {
	case msg.GetPollUpdateMessage() != nil:
		return skip("poll vote")
	case msg.GetEncReactionMessage() != nil:
		return skip("encrypted reaction")
	case msg.GetPinInChatMessage() != nil:
		return skip("pin")
	case msg.GetKeepInChatMessage() != nil:
		return skip("keep in chat")
	case !hasContent(msg):
		return skip("no content")
	}

	rec.Op = OpStore
	rec.Kind, rec.Body = content(msg)
	rec.ViewOnce = evt.IsViewOnce || viewOnce(msg)
	if rec.ViewOnce {
		rec.Body = nil
	} else {
		rec.Media = media(msg)
	}
	if ctx := contextInfo(msg); ctx != nil {
		if id := ctx.GetStanzaID(); id != "" {
			if quoted := parseAddress(ctx.GetParticipant()); !quoted.IsZero() {
				rec.ReplyTo = &Target{WAID: id, Sender: quoted}
			}
		}
		if seconds := ctx.GetExpiration(); seconds > 0 {
			expires := info.Timestamp.Add(time.Duration(seconds) * time.Second)
			rec.ExpiresAt = &expires
		}
	}
	return rec
}

// FromUndecryptable maps a message that arrived without content. Only a view-once placeholder is
// stored: WhatsApp never gives linked devices the content of those.
func FromUndecryptable(evt *events.UndecryptableMessage, own Own) Record {
	if !evt.IsUnavailable || evt.UnavailableType != events.UnavailableTypeViewOnce {
		return skip("undecryptable")
	}
	info := evt.Info
	if info.Chat.Server == types.NewsletterServer || info.Chat == types.StatusBroadcastJID {
		return skip("status")
	}
	rec := Record{
		Op:       OpStore,
		Chat:     chatRef(info, own),
		Sender:   senderOf(info, own),
		WAID:     info.ID,
		FromMe:   info.IsFromMe,
		At:       info.Timestamp,
		Source:   SourceLive,
		Kind:     "other",
		ViewOnce: true,
	}
	if rec.Chat.Kind == "" || rec.Sender.IsZero() || rec.WAID == "" || rec.At.IsZero() {
		return skip("unaddressed")
	}
	return rec
}

var (
	earliest = time.Date(2009, 1, 1, 0, 0, 0, 0, time.UTC)
	latest   = time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)
)

// saneTime turns a sender's millisecond timestamp into a time, falling back when it is missing or
// absurd. The sender chooses this number, so it is never trusted to be a real date.
func saneTime(ms int64, fallback time.Time) time.Time {
	if ms <= 0 || ms > latest.UnixMilli() {
		return fallback
	}
	if t := time.UnixMilli(ms); t.After(earliest) {
		return t
	}
	return fallback
}

// dimension keeps a sender's number only if it fits the database column.
func dimension(v uint32) int {
	if v > math.MaxInt32 {
		return 0
	}
	return int(v)
}

func checkTarget(rec Record) Record {
	if rec.Target.WAID == "" || rec.Target.Sender.IsZero() {
		return skip("no target")
	}
	return rec
}

// keySender finds who sent the message a key points at. FromMe in a key is from the point of view of
// whoever wrote the key, so it means "the sender of this event".
func keySender(key *waCommon.MessageKey, rec Record, own Own) Address {
	switch {
	case key.GetFromMe():
		return rec.Sender
	case rec.Chat.Kind != ChatDirect:
		return parseAddress(key.GetParticipant())
	case rec.FromMe:
		return rec.Chat.Contact
	default:
		return own
	}
}

func chatRef(info types.MessageInfo, own Own) ChatRef {
	chat := info.Chat.ToNonAD()
	switch chat.Server {
	case types.GroupServer:
		return ChatRef{Kind: ChatGroup, JID: chat.String()}
	case types.BroadcastServer:
		return ChatRef{Kind: ChatBroadcast, JID: chat.String()}
	}
	alt := info.SenderAlt
	if info.IsFromMe {
		alt = info.RecipientAlt
	}
	contact := address(chat, alt, own)
	if contact.IsZero() {
		return ChatRef{}
	}
	return ChatRef{Kind: ChatDirect, Contact: contact}
}

func senderOf(info types.MessageInfo, own Own) Address {
	if info.IsFromMe {
		return own
	}
	return address(info.Sender, info.SenderAlt, own)
}

// address merges a JID and its alternative spelling into one Address. Anything that is the linked
// account itself becomes own, so both of its spellings are always known.
func address(jid, alt types.JID, own Own) Address {
	var a Address
	for _, j := range []types.JID{jid, alt} {
		j = j.ToNonAD()
		switch j.Server {
		case types.HiddenUserServer, types.HostedLIDServer:
			a.LID = j.String()
		case types.DefaultUserServer, types.HostedServer:
			a.PN = j.String()
		case types.LegacyUserServer:
			j.Server = types.DefaultUserServer
			a.PN = j.String()
		case types.BotServer:
			a.Bot = j.String()
		}
	}
	if (a.LID != "" && a.LID == own.LID) || (a.PN != "" && a.PN == own.PN) {
		return own
	}
	return a
}

// AddressOf merges a JID and its alternative spelling (either may be empty) into one Address.
func AddressOf(jid, alt types.JID) Address {
	return address(jid, alt, Address{})
}

func parseAddress(raw string) Address {
	if raw == "" {
		return Address{}
	}
	jid, err := types.ParseJID(raw)
	if err != nil {
		return Address{}
	}
	return address(jid, types.EmptyJID, Address{})
}

func text(s string) *string {
	if s = strings.TrimSpace(s); s == "" {
		return nil
	}
	return &s
}

// content maps what a person would see to a messages.kind and the text worth keeping. Contact cards
// keep only the display name and locations only the place, never numbers or coordinates.
func content(msg *waE2E.Message) (kind string, body *string) {
	switch {
	case msg == nil:
		return "other", nil
	case msg.GetConversation() != "":
		return "text", text(msg.GetConversation())
	case msg.GetExtendedTextMessage() != nil:
		return "text", text(msg.GetExtendedTextMessage().GetText())
	case msg.GetImageMessage() != nil:
		return "image", text(msg.GetImageMessage().GetCaption())
	case msg.GetVideoMessage() != nil:
		return "video", text(msg.GetVideoMessage().GetCaption())
	case msg.GetPtvMessage() != nil:
		return "video", text(msg.GetPtvMessage().GetCaption())
	case msg.GetAudioMessage() != nil:
		if msg.GetAudioMessage().GetPTT() {
			return "voice", nil
		}
		return "audio", nil
	case msg.GetDocumentMessage() != nil:
		return "document", text(msg.GetDocumentMessage().GetCaption())
	case msg.GetStickerMessage() != nil:
		return "sticker", nil
	case msg.GetLocationMessage() != nil:
		loc := msg.GetLocationMessage()
		return "location", text(strings.Join(nonEmpty(loc.GetName(), loc.GetAddress()), ", "))
	case msg.GetLiveLocationMessage() != nil:
		return "location", text(msg.GetLiveLocationMessage().GetCaption())
	case msg.GetContactMessage() != nil:
		return "contact", text(msg.GetContactMessage().GetDisplayName())
	case msg.GetContactsArrayMessage() != nil:
		return "contact", text(msg.GetContactsArrayMessage().GetDisplayName())
	}
	for _, poll := range []*waE2E.PollCreationMessage{
		msg.GetPollCreationMessage(), msg.GetPollCreationMessageV2(), msg.GetPollCreationMessageV3(),
		msg.GetPollCreationMessageV4().GetMessage().GetPollCreationMessage(),
		msg.GetPollCreationMessageV5(), msg.GetPollCreationMessageV6(),
	} {
		if poll != nil {
			return "poll", text(poll.GetName())
		}
	}
	return "other", nil
}

func viewOnce(msg *waE2E.Message) bool {
	return msg.GetImageMessage().GetViewOnce() || msg.GetVideoMessage().GetViewOnce() ||
		msg.GetPtvMessage().GetViewOnce() || msg.GetAudioMessage().GetViewOnce()
}

type mediaFields interface {
	GetMimetype() string
	GetFileLength() uint64
	GetFileSHA256() []byte
	GetFileEncSHA256() []byte
	GetMediaKey() []byte
	GetDirectPath() string
}

// media describes the file of an image, video, audio, voice or document message. Stickers get
// nothing: they are never fetched.
func media(msg *waE2E.Message) *Media {
	var m Media
	var f mediaFields
	switch {
	case msg.GetImageMessage() != nil:
		img := msg.GetImageMessage()
		f, m.Type, m.Width, m.Height = img, "image", dimension(img.GetWidth()), dimension(img.GetHeight())
	case msg.GetVideoMessage() != nil:
		v := msg.GetVideoMessage()
		f, m.Type, m.Width, m.Height, m.DurationS = v, "video", dimension(v.GetWidth()), dimension(v.GetHeight()), dimension(v.GetSeconds())
	case msg.GetPtvMessage() != nil:
		v := msg.GetPtvMessage()
		f, m.Type, m.Width, m.Height, m.DurationS = v, "video", dimension(v.GetWidth()), dimension(v.GetHeight()), dimension(v.GetSeconds())
	case msg.GetAudioMessage() != nil:
		a := msg.GetAudioMessage()
		f, m.Type, m.DurationS = a, "audio", dimension(a.GetSeconds())
	case msg.GetDocumentMessage() != nil:
		d := msg.GetDocumentMessage()
		f, m.Type, m.FileName = d, "document", strings.TrimSpace(d.GetFileName())
	default:
		return nil
	}
	m.MimeType = strings.TrimSpace(f.GetMimetype())
	if size := f.GetFileLength(); size <= math.MaxInt64 {
		m.Size = int64(size)
	}
	if hash := f.GetFileSHA256(); len(hash) == sha256.Size {
		m.SHA256 = hex.EncodeToString(hash)
	}
	m.Key, m.DirectPath, m.EncSHA256 = f.GetMediaKey(), f.GetDirectPath(), f.GetFileEncSHA256()
	return &m
}

func nonEmpty(values ...string) []string {
	var out []string
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func contextInfo(msg *waE2E.Message) *waE2E.ContextInfo {
	switch {
	case msg.GetExtendedTextMessage() != nil:
		return msg.GetExtendedTextMessage().GetContextInfo()
	case msg.GetImageMessage() != nil:
		return msg.GetImageMessage().GetContextInfo()
	case msg.GetVideoMessage() != nil:
		return msg.GetVideoMessage().GetContextInfo()
	case msg.GetPtvMessage() != nil:
		return msg.GetPtvMessage().GetContextInfo()
	case msg.GetAudioMessage() != nil:
		return msg.GetAudioMessage().GetContextInfo()
	case msg.GetDocumentMessage() != nil:
		return msg.GetDocumentMessage().GetContextInfo()
	case msg.GetStickerMessage() != nil:
		return msg.GetStickerMessage().GetContextInfo()
	case msg.GetLocationMessage() != nil:
		return msg.GetLocationMessage().GetContextInfo()
	case msg.GetLiveLocationMessage() != nil:
		return msg.GetLiveLocationMessage().GetContextInfo()
	case msg.GetContactMessage() != nil:
		return msg.GetContactMessage().GetContextInfo()
	case msg.GetContactsArrayMessage() != nil:
		return msg.GetContactsArrayMessage().GetContextInfo()
	}
	return nil
}

// hasContent is false for messages that only carry encryption housekeeping.
func hasContent(msg *waE2E.Message) bool {
	found := false
	msg.ProtoReflect().Range(func(field protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		switch field.Name() {
		case "senderKeyDistributionMessage", "fastRatchetKeySenderKeyDistributionMessage", "messageContextInfo":
			return true
		}
		found = true
		return false
	})
	return found
}
