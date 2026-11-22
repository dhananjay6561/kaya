import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { after, before, describe, it } from "node:test";
import { createContracts, type SendRequest } from "../../contracts/index.js";
import { createDb, type Db } from "../../lib/db.js";
import { createLogger } from "../../lib/logger.js";
import { createTestDb, eventually, type TestDb, testDatabaseUrl } from "../../lib/test-db.js";
import { createGatewayChannel } from "../../providers/channel/index.js";
import { createEventConsumer, createGatewayEvents } from "../gateway/index.js";
import { startCommands } from "./commands.js";

const logger = createLogger({ level: "silent", service: "core", version: "test" });
const contracts = createContracts();
const ownPn = "919999999999@s.whatsapp.net";

describe("a command, end to end", { skip: !testDatabaseUrl && "TEST_DATABASE_URL not set" }, () => {
  let testDb: TestDb;
  let db: Db;
  let stop: () => Promise<void>;
  let own: number;
  let selfChat: number;

  before(async () => {
    testDb = await createTestDb(testDatabaseUrl ?? "");
    db = createDb(testDb.url("kaya_core"), logger);
    const one = async (sql: string, values: unknown[] = []) =>
      Number((await testDb.admin.query<{ id: string }>(sql, values)).rows[0]?.id);
    own = await one("SELECT contact_id_for('100000000000001@lid', $1) AS id", [ownPn]);
    selfChat = await one(
      "INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id",
      [own],
    );

    const gatewayEvents = createGatewayEvents(logger);
    const consumer = createEventConsumer({
      db,
      databaseUrl: testDb.url("kaya_core"),
      contracts,
      logger,
      handle: gatewayEvents.handle,
      retryMs: 5,
    });
    const channel = createGatewayChannel({
      pool: db.pool,
      contracts,
      results: gatewayEvents,
      incoming: gatewayEvents,
      logger,
      pollMs: 20,
    });
    startCommands({ channel, logger });
    await channel.start();
    consumer.start();
    stop = async () => {
      await consumer.stop();
      await channel.stop();
    };
  });

  after(async () => {
    await stop();
    await db.close();
    await testDb.drop();
  });

  async function emit(type: string, data: unknown) {
    const event = {
      id: randomUUID(),
      version: 1,
      type,
      occurred_at: new Date().toISOString(),
      data,
    };
    await testDb.admin.query("INSERT INTO gateway_events (id, type, payload) VALUES ($1, $2, $3)", [
      event.id,
      type,
      JSON.stringify(event),
    ]);
  }

  async function stored(waId: string, body: string) {
    const { rows } = await testDb.admin.query<{ id: string }>(
      `INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, body, source)
       VALUES ($1, $2, $3, true, now(), 'text', $4, 'live') RETURNING id`,
      [selfChat, waId, own, body],
    );
    const data = {
      message_id: Number(rows[0]?.id),
      chat_id: selfChat,
      sender_id: own,
      wa_id: waId,
      from_me: true,
      kind: "text",
      source: "live",
      change: "new",
      sent_at: new Date().toISOString(),
    };
    await emit("message.stored", data);
    return data;
  }

  const requests = async () =>
    (
      await testDb.admin.query<{ id: string; state: string; payload: SendRequest }>(
        "SELECT id::text, state, payload FROM send_requests ORDER BY seq",
      )
    ).rows;

  const idle = () =>
    eventually("every event to be handled", async () => {
      const { rows } = await testDb.admin.query(
        "SELECT 1 FROM gateway_events WHERE state = 'pending' UNION ALL SELECT 1 FROM commands WHERE state = 'pending'",
      );
      return rows.length === 0;
    });

  it("answers DJ once and never answers its own reply", async () => {
    await emit("connection.state", { state: "connected", own_jid: ownPn });
    const typed = await stored("3EB0TYPEDBYDJ", "hello kaya");

    const [request] = await eventually("the reply to be queued", async () => {
      const rows = await requests();
      return rows.length > 0 && rows;
    });
    assert.ok(request);
    const payload = contracts.parseSendRequest(request.payload);
    assert.equal(payload.to, ownPn);
    assert.deepEqual(payload.authorization, { kind: "self_chat" });
    assert.deepEqual(payload.content, { type: "text", text: "You said: hello kaya" });
    assert.deepEqual(payload.reply_to, { wa_id: "3EB0TYPEDBYDJ", sender_jid: ownPn });
    assert.ok(Date.parse(payload.expires_at) - Date.parse(payload.created_at) <= 10 * 60_000);

    await testDb.admin.query(
      "UPDATE send_requests SET state = 'sending', wa_id = '3EB0KAYAREPLY' WHERE id = $1",
      [request.id],
    );
    await testDb.admin.query(
      "UPDATE send_requests SET state = 'sent', finished_at = now() WHERE id = $1",
      [request.id],
    );
    await stored("3EB0KAYAREPLY", "You said: hello kaya");
    await emit("send.result", { request_id: request.id, status: "sent", wa_id: "3EB0KAYAREPLY" });
    await emit("message.stored", typed);
    await idle();

    assert.equal(
      (await requests()).length,
      1,
      "the reply, a repeat of the event and Kaya's own message add nothing",
    );
    const { rows } = await testDb.admin.query<{ wa_id: string; state: string }>(
      "SELECT wa_id, state FROM commands",
    );
    assert.deepEqual(rows, [{ wa_id: "3EB0TYPEDBYDJ", state: "done" }]);
    const dead = await testDb.admin.query("SELECT 1 FROM gateway_events WHERE state = 'dead'");
    assert.equal(dead.rowCount, 0);
  });
});
