import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { after, before, describe, it } from "node:test";
import pg from "pg";
import { createContracts, type GatewayEvent } from "../../contracts/index.js";
import { createLogger } from "../../lib/logger.js";
import { createTestDb, type TestDb, testDatabaseUrl } from "../../lib/test-db.js";
import { createGatewayEvents, readConnection } from "./events.js";

const logger = createLogger({ level: "silent", service: "core", version: "test" });
const contracts = createContracts();

function event(type: string, data: unknown, at = "2026-11-22T10:00:00Z"): GatewayEvent {
  return contracts.parseEvent({ id: randomUUID(), version: 1, type, occurred_at: at, data });
}

describe("gateway events", { skip: !testDatabaseUrl && "TEST_DATABASE_URL not set" }, () => {
  let testDb: TestDb;
  let core: pg.Pool;

  before(async () => {
    testDb = await createTestDb(testDatabaseUrl ?? "");
    core = new pg.Pool({ connectionString: testDb.url("kaya_core"), max: 2 });
  });

  after(async () => {
    await core.end();
    await testDb.drop();
  });

  async function handle(events: ReturnType<typeof createGatewayEvents>, e: GatewayEvent) {
    const client = await core.connect();
    try {
      const followUp = await events.handle(e, client);
      followUp?.();
    } finally {
      client.release();
    }
  }

  it("keeps the latest connection state, whatever order the events are handled in", async () => {
    const events = createGatewayEvents(logger);
    assert.equal(await readConnection(core), undefined);

    const own = "919999999999@s.whatsapp.net";
    await handle(
      events,
      event("connection.state", { state: "connected", own_jid: own }, "2026-11-22T10:00:00Z"),
    );
    await handle(
      events,
      event(
        "connection.state",
        { state: "disconnected", own_jid: own, reason: "connection lost, reconnecting" },
        "2026-11-22T10:05:00Z",
      ),
    );
    await handle(
      events,
      event("connection.state", { state: "connecting", own_jid: own }, "2026-11-22T09:00:00Z"),
    );

    const connection = await readConnection(core);
    assert.deepEqual(connection, {
      state: "disconnected",
      reason: "connection lost, reconnecting",
      ownJid: own,
      occurredAt: new Date("2026-11-22T10:05:00Z"),
    });

    await handle(
      events,
      event(
        "connection.state",
        { state: "disconnected", reason: "session store unavailable" },
        "2026-11-22T10:30:00Z",
      ),
    );
    assert.equal(
      (await readConnection(core))?.ownJid,
      own,
      "a state that does not name the account does not make core forget whose it is",
    );

    await handle(
      events,
      event("connection.state", { state: "logged_out" }, "2026-11-22T11:00:00Z"),
    );
    assert.deepEqual(await readConnection(core), {
      state: "logged_out",
      occurredAt: new Date("2026-11-22T11:00:00Z"),
    });
  });

  it("tells whoever is waiting for a send what became of it, and nobody else", async () => {
    const events = createGatewayEvents(logger);
    const mine = randomUUID();
    const got: string[] = [];
    const forget = events.onSendResult(mine, (result) =>
      got.push(`${result.status} ${result.wa_id}`),
    );
    events.onSendResult(randomUUID(), () => got.push("someone else's"));

    const client = await core.connect();
    try {
      const followUp = await events.handle(
        event("send.result", { request_id: mine, status: "sent", wa_id: "3EB0AAAA" }),
        client,
      );
      assert.deepEqual(got, [], "nobody is told before the event is committed as handled");
      followUp?.();
    } finally {
      client.release();
    }
    assert.deepEqual(got, ["sent 3EB0AAAA"]);

    forget();
    await handle(
      events,
      event("send.result", { request_id: mine, status: "sent", wa_id: "3EB0AAAA" }),
    );
    assert.deepEqual(got, ["sent 3EB0AAAA"]);
  });

  it("hands each stored message to its listeners inside the transaction", async () => {
    const events = createGatewayEvents(logger);
    const seen: number[] = [];
    const off = events.onMessage(async (message, tx) => {
      const { rows } = await tx.query<{ one: number }>("SELECT 1 AS one");
      seen.push(message.message_id * (rows[0]?.one ?? 0));
    });
    const stored = (id: number) =>
      event("message.stored", {
        message_id: id,
        chat_id: 1,
        sender_id: 1,
        wa_id: "3EB0BBBB",
        from_me: false,
        kind: "text",
        source: "live",
        change: "new",
        sent_at: "2026-11-22T10:00:00Z",
      });
    await handle(events, stored(7));
    off();
    await handle(events, stored(8));
    assert.deepEqual(seen, [7]);

    events.onMessage(async () => {
      throw new Error("listener bug");
    });
    await assert.rejects(handle(events, stored(9)), /listener bug/);
  });
});
