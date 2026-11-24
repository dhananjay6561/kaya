import assert from "node:assert/strict";
import { after, before, beforeEach, describe, it } from "node:test";
import { createDb, type Db } from "../../lib/db.js";
import { createLogger } from "../../lib/logger.js";
import { createTestDb, eventually, type TestDb, testDatabaseUrl } from "../../lib/test-db.js";
import { createStubEmbeddings, type EmbeddingProvider } from "../../providers/embeddings/index.js";
import { createMemoryPause } from "../pause/index.js";
import { createIndexer } from "./indexer.js";

const logger = createLogger({ level: "silent", service: "core", version: "test" });
const hour = 60 * 60_000;

describe("search indexer", { skip: !testDatabaseUrl && "TEST_DATABASE_URL not set" }, () => {
  let testDb: TestDb;
  let db: Db;
  let rahul: number;
  let own: number;
  let chat: number;
  let seq = 0;
  const ago = (ms: number) => new Date(Date.now() - ms);

  before(async () => {
    testDb = await createTestDb(testDatabaseUrl ?? "");
    db = createDb(testDb.url("kaya_core"), logger);
    const id = async (sql: string, values: unknown[]) =>
      Number((await testDb.admin.query<{ id: string }>(sql, values)).rows[0]?.id);
    own = await id("SELECT contact_id_for('100000000000001@lid', $1) AS id", [
      "919999999999@s.whatsapp.net",
    ]);
    rahul = await id("SELECT contact_id_for('100000000000002@lid', $1) AS id", [
      "919888888888@s.whatsapp.net",
    ]);
    await testDb.admin.query("UPDATE contacts SET saved_name = 'Rahul' WHERE id = $1", [rahul]);
    chat = await id("INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id", [
      rahul,
    ]);
  });

  after(async () => {
    await db.close();
    await testDb.drop();
  });

  beforeEach(async () => {
    await testDb.admin.query("TRUNCATE chunks, message_chunks, messages CASCADE");
  });

  async function said(
    body: string | null,
    sentAt: Date,
    extra: { fromMe?: boolean; kind?: string; deleted?: boolean } = {},
  ): Promise<number> {
    const { rows } = await testDb.admin.query<{ id: string }>(
      `INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, body, source, deleted_at)
       VALUES ($1, $2, $3, $4, $5, $6, $7, 'live', $8) RETURNING id`,
      [
        chat,
        `3EB0IDX${++seq}`,
        extra.fromMe ? own : rahul,
        extra.fromMe ?? false,
        sentAt,
        extra.kind ?? "text",
        body,
        extra.deleted ? new Date() : null,
      ],
    );
    return Number(rows[0]?.id);
  }

  function recording(locality: "cloud" | "local" = "local") {
    const stub = createStubEmbeddings(1024);
    const sent: string[] = [];
    const provider: EmbeddingProvider = {
      ...stub,
      locality,
      maxBatch: 2,
      embed: async (texts, kind, options) => {
        sent.push(...texts);
        return stub.embed(texts, kind, options);
      },
    };
    return { provider, sent };
  }

  const indexer = (provider: EmbeddingProvider, pause = createMemoryPause()) =>
    createIndexer({
      pool: db.pool,
      embeddings: provider,
      pause,
      logger,
      timeZone: "Asia/Kolkata",
      pollMs: 20,
      retryMs: 20,
    });

  const chunkRows = async () =>
    (
      await testDb.admin.query<{ messages: string[]; model: string; count: number }>(
        `SELECT array_agg(mc.message_id::text ORDER BY mc.message_id) AS messages, k.model, k.message_count AS count
         FROM chunks k JOIN message_chunks mc ON mc.chunk_id = k.id GROUP BY k.id ORDER BY k.id`,
      )
    ).rows;

  it("indexes settled text messages into chunks, every message in exactly one", async () => {
    const a = await said("kal milte hain?", ago(5 * hour));
    const b = await said("haan 5 baje", ago(5 * hour - 60_000), { fromMe: true });
    const c = await said("next week then", ago(2 * hour));
    const { provider, sent } = recording();
    assert.equal(await indexer(provider).runOnce(), 3);
    assert.deepEqual(
      (await chunkRows()).map((r) => r.messages),
      [[String(a), String(b)], [String(c)]],
    );
    assert.match(
      sent[0] ?? "",
      /^Chat: Rahul\n\d+ \w+ \d{4}\n\d\d:\d\d Rahul: kal milte hain\?\n\d\d:\d\d Me: haan 5 baje$/,
    );
    assert.equal((await chunkRows())[0]?.model, "stub");
    assert.equal(await indexer(provider).runOnce(), 0, "a second pass finds nothing new");
  });

  it("leaves out deleted, empty, non-text and still-recent messages", async () => {
    await said("gone", ago(hour), { deleted: true });
    await said("   ", ago(hour));
    await said(null, ago(hour), { kind: "image" });
    await said("just now", ago(60_000));
    const kept = await said("kept", ago(hour));
    const { provider } = recording();
    assert.equal(await indexer(provider).runOnce(), 1);
    assert.deepEqual(
      (await chunkRows()).map((r) => r.messages),
      [[String(kept)]],
    );
  });

  it("masks private details before text goes to a cloud service, and not for a local one", async () => {
    await said("mera number 9876543210 hai, OTP 482913", ago(hour));
    const cloud = recording("cloud");
    await indexer(cloud.provider).runOnce();
    assert.doesNotMatch(cloud.sent.join("\n"), /9876543210|482913/);
    assert.match(cloud.sent.join("\n"), /\[PHONE_1\].*\[OTP_1\]/);

    await testDb.admin.query("TRUNCATE chunks, message_chunks");
    const local = recording("local");
    await indexer(local.provider).runOnce();
    assert.match(local.sent.join("\n"), /9876543210/);
  });

  it("makes a chunk again when one of its messages is edited or deleted, and not otherwise", async () => {
    const first = await said("see you at 5", ago(3 * hour));
    const second = await said("bring the charger", ago(3 * hour - 60_000));
    const { provider, sent } = recording();
    const worker = indexer(provider);
    await worker.runOnce();
    await testDb.admin.query("UPDATE messages SET source = 'history' WHERE id = $1", [first]);
    await worker.runOnce();
    assert.equal(sent.length, 1, "a change that leaves the text alone costs nothing");

    await testDb.admin.query("UPDATE messages SET body = 'see you at 6' WHERE id = $1", [first]);
    await worker.runOnce();
    assert.equal(sent.length, 2);
    assert.match(sent[1] ?? "", /see you at 6/);
    await testDb.admin.query("UPDATE messages SET deleted_at = now() WHERE id = $1", [second]);
    await worker.runOnce();
    assert.deepEqual(
      (await chunkRows()).map((r) => r.messages),
      [[String(first)]],
    );
  });

  it("reports how much is searchable", async () => {
    await said("one", ago(hour));
    await said("two", ago(hour));
    const worker = indexer(recording().provider);
    assert.deepEqual(await worker.progress(), { indexed: 0, total: 2 });
    await worker.runOnce();
    assert.deepEqual(await worker.progress(), { indexed: 2, total: 2 });
  });

  it("waits while Kaya is paused, then catches up", async () => {
    await said("while paused", ago(hour));
    const pause = createMemoryPause();
    await pause.pause("cmd");
    const worker = indexer(recording().provider, pause);
    worker.start();
    await new Promise((resolve) => setTimeout(resolve, 100));
    assert.equal((await chunkRows()).length, 0);
    await pause.resume("cmd2");
    worker.wake();
    await eventually("the message to be indexed", async () => (await chunkRows()).length === 1);
    await worker.stop();
  });
});
