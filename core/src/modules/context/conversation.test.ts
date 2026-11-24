import assert from "node:assert/strict";
import { after, before, beforeEach, describe, it } from "node:test";
import { createDb, type Db } from "../../lib/db.js";
import { createLogger } from "../../lib/logger.js";
import { createTestDb, type TestDb, testDatabaseUrl } from "../../lib/test-db.js";
import { readConversation } from "./conversation.js";

const logger = createLogger({ level: "silent", service: "core", version: "test" });
const ownPn = "919999999999@s.whatsapp.net";
const minute = 60_000;

describe("conversation", { skip: !testDatabaseUrl && "TEST_DATABASE_URL not set" }, () => {
  let testDb: TestDb;
  let db: Db;
  let own: number;
  let chat: number;
  let otherChat: number;
  const now = new Date(Math.floor(Date.now() / 1_000) * 1_000);
  const ago = (ms: number) => new Date(now.getTime() - ms);

  before(async () => {
    testDb = await createTestDb(testDatabaseUrl ?? "");
    db = createDb(testDb.url("kaya_core"), logger);
    const contact = await testDb.admin.query<{ id: string }>(
      "SELECT contact_id_for('100000000000001@lid', $1) AS id",
      [ownPn],
    );
    own = Number(contact.rows[0]?.id);
    const created = async (sql: string, values: unknown[]) =>
      Number((await testDb.admin.query<{ id: string }>(sql, values)).rows[0]?.id);
    chat = await created(
      "INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id",
      [own],
    );
    otherChat = await created("INSERT INTO chats (kind, jid) VALUES ('group', $1) RETURNING id", [
      "120363000000000001@g.us",
    ]);
  });

  after(async () => {
    await db.close();
    await testDb.drop();
  });

  beforeEach(async () => {
    await testDb.admin.query("TRUNCATE commands, messages CASCADE");
  });

  let seq = 0;
  async function stored(
    chatId: number,
    waId: string,
    body: string,
    sentAt: Date,
    extra: { deleted?: boolean; edited?: Date } = {},
  ): Promise<number> {
    const { rows } = await testDb.admin.query<{ id: string }>(
      `INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, body, source, deleted_at, edited_at)
       VALUES ($1, $2, $3, true, $4, 'text', $5, 'live', $6, $7) RETURNING id`,
      [chatId, waId, own, sentAt, body, extra.deleted ? now : null, extra.edited ?? null],
    );
    return Number(rows[0]?.id);
  }

  async function asked(
    body: string,
    sentAt: Date,
    options: {
      answer?: string | null;
      state?: "done" | "failed" | "pending";
      questionDeleted?: boolean;
      answerDeleted?: boolean;
      editedAfter?: boolean;
      answerChat?: number;
    } = {},
  ): Promise<string> {
    const n = ++seq;
    const waId = `3EB0ASK${n}`;
    const messageId = await stored(chat, waId, body, sentAt, {
      ...(options.questionDeleted ? { deleted: true } : {}),
      ...(options.editedAfter ? { edited: new Date(now.getTime() + minute) } : {}),
    });
    const answerWaId = options.answer === null ? null : `3EB0ANS${n}`;
    if (answerWaId) {
      await stored(
        options.answerChat ?? chat,
        answerWaId,
        options.answer ?? `answer to ${body}`,
        sentAt,
        {
          ...(options.answerDeleted ? { deleted: true } : {}),
        },
      );
    }
    const state = options.state ?? "done";
    await testDb.admin.query(
      `INSERT INTO commands (message_id, wa_id, sent_at, state, error, finished_at, answer_wa_id)
       VALUES ($1, $2, $3, $4, $5, $6, $7)`,
      [
        messageId,
        waId,
        sentAt,
        state,
        state === "failed" ? "interrupted" : null,
        state === "pending" ? null : now,
        answerWaId,
      ],
    );
    return waId;
  }

  const read = (current: string, sentAt: Date, extra = {}) =>
    readConversation(db.pool, { before: current, sentAt, ...extra });

  it("pairs each answered question with Kaya's answer, oldest first", async () => {
    await asked("what is the capital of France?", ago(5 * minute));
    await asked("and of Spain?", ago(3 * minute));
    const current = await asked("and in Hindi?", now, { answer: null, state: "pending" });
    assert.deepEqual(
      (await read(current, now)).map((e) => [e.question, e.answer]),
      [
        ["what is the capital of France?", "answer to what is the capital of France?"],
        ["and of Spain?", "answer to and of Spain?"],
      ],
    );
  });

  it("leaves out unanswered commands before counting, so they never crowd out answers", async () => {
    await asked("kept", ago(10 * minute));
    for (let i = 0; i < 5; i++)
      await asked(`/status ${i}`, ago((9 - i) * minute), { answer: null });
    const current = await asked("now", now, { answer: null, state: "pending" });
    assert.deepEqual(
      (await read(current, now, { maxExchanges: 3 })).map((e) => e.question),
      ["kept"],
    );
  });

  it("keeps an answer DJ saw even when the command later failed", async () => {
    await asked("answered then interrupted", ago(2 * minute), { state: "failed" });
    const current = await asked("now", now, { answer: null, state: "pending" });
    assert.equal((await read(current, now)).length, 1);
  });

  it("drops an exchange whose question or answer was deleted, or whose question was edited later", async () => {
    await asked("deleted question", ago(5 * minute), { questionDeleted: true });
    await asked("deleted answer", ago(4 * minute), { answerDeleted: true });
    await asked("edited afterwards", ago(3 * minute), { editedAfter: true });
    await asked("answer in another chat", ago(2 * minute), { answerChat: otherChat });
    await asked("kept", ago(1 * minute));
    const current = await asked("now", now, { answer: null, state: "pending" });
    assert.deepEqual(
      (await read(current, now)).map((e) => e.question),
      ["kept"],
    );
  });

  it("looks back only so far, and only so many", async () => {
    await asked("yesterday", ago(13 * 60 * minute));
    for (let i = 1; i <= 4; i++) await asked(`recent ${i}`, ago((10 - i) * minute));
    const current = await asked("now", now, { answer: null, state: "pending" });
    assert.deepEqual(
      (await read(current, now, { maxExchanges: 3 })).map((e) => e.question),
      ["recent 2", "recent 3", "recent 4"],
    );
  });

  it("orders by when Kaya took commands in, so questions sent in the same second all count", async () => {
    await asked("first in the burst", now);
    await asked("second in the burst", now);
    const current = await asked("third in the burst", now, { answer: null, state: "pending" });
    await asked("taken in later", now);
    assert.deepEqual(
      (await read(current, now)).map((e) => e.question),
      ["first in the burst", "second in the burst"],
    );
  });

  it("never includes the question being answered, even once it has an answer", async () => {
    await asked("earlier", ago(minute));
    const current = await asked("this one", now);
    assert.deepEqual(
      (await read(current, now)).map((e) => e.question),
      ["earlier"],
    );
  });

  it("reads everything before an unknown command, for a question not recorded yet", async () => {
    await asked("earlier", ago(minute));
    assert.equal((await read("3EB0UNKNOWN", now)).length, 1);
  });
});
