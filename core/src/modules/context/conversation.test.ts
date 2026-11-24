import assert from "node:assert/strict";
import { after, before, beforeEach, describe, it } from "node:test";
import { createContracts, newSendRequest } from "../../contracts/index.js";
import { createDb, type Db } from "../../lib/db.js";
import { createLogger } from "../../lib/logger.js";
import { createTestDb, type TestDb, testDatabaseUrl } from "../../lib/test-db.js";
import { requestIdFor } from "../../providers/channel/index.js";
import { chatAnswerKey, readConversation } from "./conversation.js";

const logger = createLogger({ level: "silent", service: "core", version: "test" });
const contracts = createContracts();
const ownPn = "919999999999@s.whatsapp.net";
const minute = 60_000;

describe("conversation", { skip: !testDatabaseUrl && "TEST_DATABASE_URL not set" }, () => {
  let testDb: TestDb;
  let db: Db;
  let own: number;
  let chat: number;
  const now = new Date();
  const ago = (ms: number) => new Date(now.getTime() - ms);

  before(async () => {
    testDb = await createTestDb(testDatabaseUrl ?? "");
    db = createDb(testDb.url("kaya_core"), logger);
    const contact = await testDb.admin.query<{ id: string }>(
      "SELECT contact_id_for('100000000000001@lid', $1) AS id",
      [ownPn],
    );
    own = Number(contact.rows[0]?.id);
    const created = await testDb.admin.query<{ id: string }>(
      "INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id",
      [own],
    );
    chat = Number(created.rows[0]?.id);
  });

  after(async () => {
    await db.close();
    await testDb.drop();
  });

  beforeEach(async () => {
    await testDb.admin.query("TRUNCATE commands, send_requests, messages CASCADE");
  });

  let seq = 0;
  async function asked(
    body: string,
    sentAt: Date,
    options: {
      state?: string;
      key?: (waId: string) => string;
      answer?: string | null;
      deleted?: boolean;
    } = {},
  ): Promise<string> {
    const waId = `3EB0ASK${++seq}`;
    const message = await testDb.admin.query<{ id: string }>(
      `INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, body, source, deleted_at)
       VALUES ($1, $2, $3, true, $4, 'text', $5, 'live', $6) RETURNING id`,
      [chat, waId, own, sentAt, body, options.deleted ? now : null],
    );
    const state = options.state ?? "done";
    await testDb.admin.query(
      `INSERT INTO commands (message_id, wa_id, sent_at, state, error, finished_at)
       VALUES ($1, $2, $3, $4, $5, now())`,
      [message.rows[0]?.id, waId, sentAt, state, state === "failed" ? "rate_limited" : null],
    );
    if (options.answer !== null) {
      const key = (options.key ?? chatAnswerKey)(waId);
      const request = {
        ...newSendRequest(ownPn, options.answer ?? `answer to ${body}`, { kind: "self_chat" }),
        id: requestIdFor(key),
      };
      await testDb.admin.query(
        "INSERT INTO send_requests (id, payload, expires_at) VALUES ($1, $2, $3)",
        [request.id, contracts.encodeSendRequest(request), request.expires_at],
      );
      await testDb.admin.query(
        "UPDATE send_requests SET state = 'sending', wa_id = $2 WHERE id = $1",
        [request.id, `3EB0ANS${seq}`],
      );
      await testDb.admin.query(
        "UPDATE send_requests SET state = 'sent', finished_at = now() WHERE id = $1",
        [request.id],
      );
    }
    return waId;
  }

  it("pairs each answered question with Kaya's answer, oldest first", async () => {
    await asked("what is the capital of France?", ago(5 * minute));
    await asked("and of Spain?", ago(3 * minute));
    const current = await asked("and in Hindi?", ago(0), { answer: null });
    const exchanges = await readConversation(db.pool, { before: now, excludeWaId: current });
    assert.deepEqual(
      exchanges.map((e) => [e.question, e.answer]),
      [
        ["what is the capital of France?", "answer to what is the capital of France?"],
        ["and of Spain?", "answer to and of Spain?"],
      ],
    );
  });

  it("leaves out slash commands, failures, deleted questions and unanswered ones", async () => {
    await asked("/status", ago(6 * minute), { key: (w) => `command:${w}:builtin:status` });
    await asked("busy question", ago(5 * minute), { state: "failed", answer: null });
    await asked("deleted question", ago(4 * minute), { deleted: true });
    await asked("never answered", ago(3 * minute), { answer: null });
    await asked("kept", ago(2 * minute));
    const exchanges = await readConversation(db.pool, { before: now });
    assert.deepEqual(
      exchanges.map((e) => e.question),
      ["kept"],
    );
  });

  it("looks back only so far, and only so many", async () => {
    await asked("yesterday", ago(13 * 60 * minute));
    for (let i = 1; i <= 4; i++) await asked(`recent ${i}`, ago((10 - i) * minute));
    const exchanges = await readConversation(db.pool, { before: now, maxExchanges: 3 });
    assert.deepEqual(
      exchanges.map((e) => e.question),
      ["recent 2", "recent 3", "recent 4"],
    );
  });

  it("never includes the question being answered or anything after it", async () => {
    await asked("earlier", ago(5 * minute));
    const current = await asked("now", ago(2 * minute));
    await asked("later", ago(1 * minute));
    const exchanges = await readConversation(db.pool, {
      before: ago(2 * minute),
      excludeWaId: current,
    });
    assert.deepEqual(
      exchanges.map((e) => e.question),
      ["earlier"],
    );
  });
});
