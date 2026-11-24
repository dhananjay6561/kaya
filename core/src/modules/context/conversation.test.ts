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
    await testDb.admin.query("TRUNCATE exchanges, commands, messages CASCADE");
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
      questionDeleted?: boolean;
      answerDeleted?: boolean;
      editedAfter?: boolean;
      answerChat?: number;
    } = {},
  ): Promise<void> {
    const n = ++seq;
    const messageId = await stored(chat, `3EB0ASK${n}`, body, sentAt, {
      ...(options.questionDeleted ? { deleted: true } : {}),
      ...(options.editedAfter ? { edited: new Date(now.getTime() + minute) } : {}),
    });
    if (options.answer === null) return;
    const answerWaId = `3EB0ANS${n}`;
    await stored(
      options.answerChat ?? chat,
      answerWaId,
      options.answer ?? `answer to ${body}`,
      sentAt,
      {
        ...(options.answerDeleted ? { deleted: true } : {}),
      },
    );
    await testDb.admin.query(
      "INSERT INTO exchanges (question_message_id, answer_wa_id, created_at) VALUES ($1, $2, $3)",
      [messageId, answerWaId, now],
    );
  }

  const read = (extra = {}) => readConversation(db.pool, extra);

  it("pairs each question with Kaya's answer, oldest first", async () => {
    await asked("what is the capital of France?", ago(5 * minute));
    await asked("and of Spain?", ago(3 * minute));
    await asked("and in Hindi?", now, { answer: null });
    assert.deepEqual(
      (await read()).map((e) => [e.question, e.answer]),
      [
        ["what is the capital of France?", "answer to what is the capital of France?"],
        ["and of Spain?", "answer to and of Spain?"],
      ],
    );
  });

  it("remembers however long ago it was, keeping the latest ones up to the limit", async () => {
    await asked("last month", ago(30 * 24 * 60 * minute));
    await asked("last week", ago(7 * 24 * 60 * minute));
    assert.deepEqual(
      (await read()).map((e) => e.question),
      ["last month", "last week"],
    );
    for (let i = 1; i <= 3; i++) await asked(`today ${i}`, ago((10 - i) * minute));
    assert.deepEqual(
      (await read({ maxExchanges: 3 })).map((e) => e.question),
      ["today 1", "today 2", "today 3"],
    );
  });

  it("drops an exchange whose question or answer was deleted, or whose question was edited later", async () => {
    await asked("deleted question", ago(5 * minute), { questionDeleted: true });
    await asked("deleted answer", ago(4 * minute), { answerDeleted: true });
    await asked("edited afterwards", ago(3 * minute), { editedAfter: true });
    await asked("answer in another chat", ago(2 * minute), { answerChat: otherChat });
    await asked("kept", ago(1 * minute));
    assert.deepEqual(
      (await read()).map((e) => e.question),
      ["kept"],
    );
  });

  it("keeps the order Kaya answered in, even for questions sent in the same second", async () => {
    await asked("first in the burst", now);
    await asked("second in the burst", now);
    await asked("third in the burst", now);
    assert.deepEqual(
      (await read()).map((e) => e.question),
      ["first in the burst", "second in the burst", "third in the burst"],
    );
  });

  it("outlives the commands it came from", async () => {
    await asked("old question", ago(40 * 24 * 60 * minute));
    await testDb.admin.query("DELETE FROM commands");
    assert.equal((await read()).length, 1);
  });
});
