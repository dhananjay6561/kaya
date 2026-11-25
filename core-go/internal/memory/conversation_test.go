package memory

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/dhananjay6561/kaya/kit/store"
	"github.com/dhananjay6561/kaya/kit/testdb"
)

var bg = context.Background()

type conversation struct {
	t                    *testing.T
	core, admin          *store.DB
	own, chat, otherChat int64
	now                  time.Time
	seq                  int
}

func newConversation(t *testing.T) *conversation {
	t.Helper()
	core, admin := testdb.MigratedPair(t, "kaya_core")
	c := &conversation{t: t, core: core, admin: admin, now: time.Now().Truncate(time.Second)}
	c.own = c.one("SELECT contact_id_for('100000000000001@lid', $1)", "919999999999@s.whatsapp.net")
	c.chat = c.one("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", c.own)
	c.otherChat = c.one("INSERT INTO chats (kind, jid) VALUES ('group', $1) RETURNING id", "120363000000000001@g.us")
	return c
}

func (c *conversation) one(sql string, args ...any) int64 {
	c.t.Helper()
	var id int64
	if err := c.admin.QueryRow(bg, sql, args...).Scan(&id); err != nil {
		c.t.Fatalf("%s: %v", sql, err)
	}
	return id
}

func (c *conversation) exec(sql string, args ...any) {
	c.t.Helper()
	if _, err := c.admin.Exec(bg, sql, args...); err != nil {
		c.t.Fatalf("%s: %v", sql, err)
	}
}

type asked struct {
	noAnswer, questionDeleted, answerDeleted, editedAfter bool
	answerChat                                            int64
}

func (c *conversation) ask(body string, sentAt time.Time, o asked) {
	c.t.Helper()
	c.seq++
	stored := func(chat int64, waID, body string, deleted bool, edited *time.Time) int64 {
		var deletedAt *time.Time
		if deleted {
			deletedAt = &c.now
		}
		return c.one(`INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, body, source, deleted_at, edited_at)
		              VALUES ($1, $2, $3, true, $4, 'text', $5, 'live', $6, $7) RETURNING id`,
			chat, waID, c.own, sentAt, body, deletedAt, edited)
	}
	var edited *time.Time
	if o.editedAfter {
		later := c.now.Add(time.Minute)
		edited = &later
	}
	question := stored(c.chat, fmt.Sprintf("3EB0ASK%d", c.seq), body, o.questionDeleted, edited)
	if o.noAnswer {
		return
	}
	chat := c.chat
	if o.answerChat != 0 {
		chat = o.answerChat
	}
	answerWaID := fmt.Sprintf("3EB0ANS%d", c.seq)
	stored(chat, answerWaID, "answer to "+body, o.answerDeleted, nil)
	c.exec("INSERT INTO exchanges (question_message_id, answer_wa_id, created_at) VALUES ($1, $2, $3)",
		question, answerWaID, sentAt)
}

func (c *conversation) questions(maxExchanges int) []string {
	c.t.Helper()
	exchanges, err := ReadConversation(bg, c.core, maxExchanges)
	if err != nil {
		c.t.Fatal(err)
	}
	out := []string{}
	for _, e := range exchanges {
		out = append(out, e.Question)
	}
	return out
}

func same(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestReadConversationPairsEachQuestionWithKayasAnswerOldestFirst(t *testing.T) {
	c := newConversation(t)
	c.ask("  what is the capital of France?\n", c.now.Add(-5*time.Minute), asked{})
	c.ask("and of Spain?", c.now.Add(-3*time.Minute), asked{})
	c.ask("and in Hindi?", c.now, asked{noAnswer: true})
	exchanges, err := ReadConversation(bg, c.core, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got [][2]string
	for _, e := range exchanges {
		got = append(got, [2]string{e.Question, e.Answer})
	}
	want := [][2]string{
		{"what is the capital of France?", "answer to   what is the capital of France?\n"},
		{"and of Spain?", "answer to and of Spain?"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
	if !exchanges[0].At.Equal(c.now.Add(-5 * time.Minute)) {
		t.Fatalf("at = %v", exchanges[0].At)
	}
}

func TestReadConversationRemembersHoweverLongAgoKeepingTheLatestUpToTheLimit(t *testing.T) {
	c := newConversation(t)
	c.ask("last month", c.now.Add(-30*24*time.Hour), asked{})
	c.ask("last week", c.now.Add(-7*24*time.Hour), asked{})
	same(t, c.questions(0), []string{"last month", "last week"})
	for i := 1; i <= 3; i++ {
		c.ask(fmt.Sprintf("today %d", i), c.now.Add(-time.Duration(10-i)*time.Minute), asked{})
	}
	same(t, c.questions(3), []string{"today 1", "today 2", "today 3"})
}

func TestReadConversationDropsDeletedOrLaterEditedExchanges(t *testing.T) {
	c := newConversation(t)
	c.ask("deleted question", c.now.Add(-5*time.Minute), asked{questionDeleted: true})
	c.ask("deleted answer", c.now.Add(-4*time.Minute), asked{answerDeleted: true})
	c.ask("edited afterwards", c.now.Add(-3*time.Minute), asked{editedAfter: true})
	c.ask("answer in another chat", c.now.Add(-2*time.Minute), asked{answerChat: c.otherChat})
	c.ask("kept", c.now.Add(-time.Minute), asked{})
	same(t, c.questions(0), []string{"kept"})
}

func TestReadConversationKeepsTheOrderKayaAnsweredIn(t *testing.T) {
	c := newConversation(t)
	c.ask("answered first", c.now.Add(-time.Minute), asked{})
	c.ask("answered second, though sent earlier", c.now.Add(-5*time.Minute), asked{})
	c.ask("first in the burst", c.now, asked{})
	c.ask("second in the burst", c.now, asked{})
	same(t, c.questions(0), []string{"answered first", "answered second, though sent earlier", "first in the burst", "second in the burst"})
}

func TestReadConversationOutlivesTheCommandsItCameFrom(t *testing.T) {
	c := newConversation(t)
	c.ask("old question", c.now.Add(-40*24*time.Hour), asked{})
	c.exec("DELETE FROM commands")
	same(t, c.questions(0), []string{"old question"})
}
