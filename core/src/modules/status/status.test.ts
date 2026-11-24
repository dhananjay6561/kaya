import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { after, before, describe, it } from "node:test";
import { createDb, type Db } from "../../lib/db.js";
import { createLogger } from "../../lib/logger.js";
import { createTestDb, type TestDb, testDatabaseUrl } from "../../lib/test-db.js";
import { createMemoryPause, createPause } from "../pause/index.js";
import { formatDuration, formatStatus, readStatus, type Status } from "./status.js";

const logger = createLogger({ level: "silent", service: "core", version: "test" });
const minute = 60_000;
const now = new Date("2026-11-24T10:00:00Z");
const ago = (ms: number) => new Date(now.getTime() - ms);

describe("status text", () => {
  it("writes durations a person reads at a glance", () => {
    assert.equal(formatDuration(0), "under a minute");
    assert.equal(formatDuration(59_999), "under a minute");
    assert.equal(formatDuration(minute), "1m");
    assert.equal(formatDuration(59 * minute), "59m");
    assert.equal(formatDuration(133 * minute), "2h 13m");
    assert.equal(formatDuration(3 * 24 * 60 * minute + 5 * 60 * minute), "3d 5h");
  });

  const healthy: Status = {
    version: "8fc3488",
    startedAt: ago(133 * minute),
    now,
    pause: { paused: false },
    connection: { state: "connected", occurredAt: ago(12 * minute) },
    lastStoredAt: ago(2 * minute),
    eventsWaiting: 0,
    eventsDead: 0,
  };

  it("says Kaya is up, connected, answering and storing", () => {
    assert.equal(
      formatStatus(healthy),
      [
        "Kaya 8fc3488, up 2h 13m.",
        "WhatsApp: connected for 12m.",
        "Answering messages.",
        "Last incoming message stored 2m ago.",
      ].join("\n"),
    );
  });

  it("says how long Kaya has been paused and how to undo it", () => {
    const text = formatStatus({ ...healthy, pause: { paused: true, since: ago(5 * minute) } });
    assert.match(text, /^Paused for 5m\. Send \/resume to start again\.$/m);
    assert.doesNotMatch(text, /Answering/);
  });

  it("names trouble only when there is some", () => {
    const text = formatStatus({
      ...healthy,
      connection: {
        state: "disconnected",
        reason: "connection lost, reconnecting",
        occurredAt: ago(3 * minute),
      },
      eventsWaiting: 4,
      eventsDead: 2,
    });
    assert.match(text, /^WhatsApp: disconnected for 3m \(connection lost, reconnecting\)\.$/m);
    assert.match(text, /^Events waiting for core: 4\.$/m);
    assert.match(text, /^Events core gave up on: 2\.$/m);
  });

  it("says how much of the chat history is searchable", () => {
    const text = formatStatus({ ...healthy, searchIndex: { indexed: 12_345, total: 100_787 } });
    assert.match(text, /^Search index: 12,345 of 1,00,787 messages \(12%\)\.$/m);
    assert.doesNotMatch(formatStatus(healthy), /Search index/);
  });

  it("copes with a fresh install that has heard nothing yet", () => {
    const { connection: _c, lastStoredAt: _l, ...fresh } = healthy;
    const text = formatStatus(fresh);
    assert.match(text, /^WhatsApp: no report from the gateway yet\.$/m);
    assert.match(text, /^No incoming messages stored yet\.$/m);
  });

  it("never shows a negative age when clocks disagree", () => {
    assert.match(formatStatus({ ...healthy, lastStoredAt: ago(-5 * minute) }), /under a minute/);
  });
});

describe("status from the database", {
  skip: !testDatabaseUrl && "TEST_DATABASE_URL not set",
}, () => {
  let testDb: TestDb;
  let db: Db;

  before(async () => {
    testDb = await createTestDb(testDatabaseUrl ?? "");
    db = createDb(testDb.url("kaya_core"), logger);
  });

  after(async () => {
    await db.close();
    await testDb.drop();
  });

  it("reads what core is allowed to read", async () => {
    const startedAt = new Date();
    const empty = await readStatus({
      pool: db.pool,
      pause: createPause(db.pool),
      version: "test",
      startedAt,
    });
    assert.equal(empty.connection, undefined);
    assert.equal(empty.lastStoredAt, undefined);
    assert.deepEqual(empty.pause, { paused: false });
    assert.equal(empty.eventsWaiting, 0);

    const contact = await testDb.admin.query<{ id: string }>(
      "SELECT contact_id_for('100000000000001@lid', '919999999999@s.whatsapp.net') AS id",
    );
    const own = contact.rows[0]?.id;
    const chat = await testDb.admin.query<{ id: string }>(
      "INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id",
      [own],
    );
    const friend = await testDb.admin.query<{ id: string }>(
      "SELECT contact_id_for('100000000000002@lid', '919888888888@s.whatsapp.net') AS id",
    );
    await testDb.admin.query(
      `INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, body, source, created_at)
       VALUES ($1, '3EB0INCOMING', $3, false, now(), 'text', 'hi', 'live', now() - interval '2 hours'),
              ($1, '3EB0STATUS', $2, true, now(), 'text', '/status', 'live', now())`,
      [chat.rows[0]?.id, own, friend.rows[0]?.id],
    );
    await testDb.admin.query(
      "INSERT INTO connection_state (state, own_jid, occurred_at) VALUES ('connected', '919999999999@s.whatsapp.net', now())",
    );
    const id = randomUUID();
    await testDb.admin.query(
      "INSERT INTO gateway_events (id, type, payload) VALUES ($1, 'history.synced', $2)",
      [id, JSON.stringify({ id, type: "history.synced" })],
    );

    const full = await readStatus({
      pool: db.pool,
      pause: createMemoryPause(),
      version: "test",
      startedAt,
    });
    assert.equal(full.connection?.state, "connected");
    const age = Date.now() - (full.lastStoredAt?.getTime() ?? 0);
    assert.ok(
      age > 110 * minute && age < 130 * minute,
      "the owner's own messages, like the /status itself, do not count",
    );
    assert.equal(full.eventsWaiting, 1);
    assert.equal(full.eventsDead, 0);
  });
});
