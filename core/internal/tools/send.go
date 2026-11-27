package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dhananjay6561/kaya/core/internal/actions"
	"github.com/dhananjay6561/kaya/core/internal/agent"
	"github.com/dhananjay6561/kaya/core/internal/channel"
	"github.com/dhananjay6561/kaya/core/internal/text"
	"github.com/dhananjay6561/kaya/kit/store"
)

// SendKind is the kind of action that sends a message for DJ.
const SendKind = "send_message"

const maxSendLength = 2000

// ChatFinder finds a chat by the name DJ calls it.
type ChatFinder interface {
	FindChat(ctx context.Context, name string) (int64, string, bool, error)
}

type sendPayload struct {
	ChatID int64  `json:"chat_id"`
	Title  string `json:"title"`
	Text   string `json:"text"`
}

// SendMessage drafts a WhatsApp message to one of DJ's contacts or groups. It never sends: it
// proposes, and only DJ's yes sends it.
func SendMessage(chats ChatFinder, db *store.DB) agent.Tool {
	return agent.Tool{
		Name: SendKind,
		Description: "Send a WhatsApp message for the owner to one of their contacts or groups, as them. It does not send " +
			"anything by itself: the exact message is shown to them and goes out only if they reply yes. Use it when they " +
			"ask you to message, tell, reply to or remind someone. Write the text the way they would write it to that " +
			"person, in the language they use with them, short; never add anything they did not ask for.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"to": {"type": "string", "description": "The contact or group, by the name the owner used."},
				"text": {"type": "string", "description": "The message exactly as it should be sent."}
			},
			"required": ["to", "text"],
			"additionalProperties": false
		}`),
		Outward: true,
		Propose: func(ctx context.Context, raw json.RawMessage) (actions.Proposal, error) {
			var args struct {
				To   string `json:"to"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				return actions.Proposal{}, err
			}
			message := text.Trim(args.Text)
			switch {
			case message == "":
				return actions.Proposal{}, agent.Problemf("the message is empty")
			case len([]rune(message)) > maxSendLength:
				return actions.Proposal{}, agent.Problemf("the message is too long; keep it under %d characters", maxSendLength)
			}
			chat, title, found, err := chats.FindChat(ctx, args.To)
			if err != nil {
				return actions.Proposal{}, err
			}
			if !found {
				return actions.Proposal{}, agent.Problemf("no contact or group in their chats matches %q; ask them who they mean", args.To)
			}
			jid, err := chatJID(ctx, db, chat)
			if err != nil {
				return actions.Proposal{}, err
			}
			if jid == "" {
				return actions.Proposal{}, agent.Problemf("%s cannot be messaged from here", title)
			}
			payload, err := json.Marshal(sendPayload{ChatID: chat, Title: title, Text: message})
			if err != nil {
				return actions.Proposal{}, err
			}
			return actions.Proposal{
				Kind:      SendKind,
				TargetJID: jid,
				Summary:   "Send to " + text.Clip(title, 80) + ":\n" + message,
				Payload:   payload,
			}, nil
		},
	}
}

// chatJID is where a direct chat or a group is written to: a group's JID, or the contact's phone
// number, else their LID. Anything else gives "".
func chatJID(ctx context.Context, db *store.DB, chat int64) (string, error) {
	var jid string
	err := db.QueryRow(ctx, `SELECT coalesce(CASE ch.kind WHEN 'group' THEN ch.jid WHEN 'direct' THEN coalesce(cc.pn, cc.lid) END, '')
		FROM chats ch LEFT JOIN contacts cc ON cc.id = ch.contact_id WHERE ch.id = $1`, chat).Scan(&jid)
	return jid, err
}

// Sender sends a message through the gateway.
type Sender interface {
	Send(ctx context.Context, m channel.OutgoingMessage, o channel.SendOptions) (channel.SentMessage, error)
}

// SendExecutor sends a confirmed message, at most perHour in any hour, and says how it went.
func SendExecutor(ch Sender, store *actions.Store, perHour int, now func() time.Time) actions.Executor {
	return func(ctx context.Context, a actions.Action) (string, error) {
		var p sendPayload
		if err := json.Unmarshal(a.Payload, &p); err != nil {
			return "", err
		}
		done, err := store.CountDone(ctx, SendKind, now().Add(-time.Hour))
		if err != nil {
			return "", err
		}
		if done > perHour {
			return fmt.Sprintf("I have already sent %d messages for you this hour, the most I send", perHour), errors.New("hourly limit")
		}
		_, err = ch.Send(ctx, channel.OutgoingMessage{IdempotencyKey: "action:" + a.ID, Text: p.Text, To: a.TargetJID, ActionID: a.ID},
			channel.SendOptions{TTL: 10 * time.Minute})
		var failed *channel.SendFailedError
		switch {
		case errors.As(err, &failed):
			return sendFailure(failed.Code), err
		case err != nil:
			return "", err
		}
		return "Sent to " + p.Title + ".", nil
	}
}

func sendFailure(code string) string {
	switch code {
	case "recipient_not_on_whatsapp":
		return "they are not on WhatsApp"
	case "expired", "no_answer":
		return "it could not go out in time"
	case "outcome_unknown":
		return "I am not sure it went out; check the chat before sending again"
	case "not_authorized":
		return "the gateway would not let it go out"
	default:
		return "WhatsApp did not take it (" + strings.ReplaceAll(code, "_", " ") + ")"
	}
}
