import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { after, before, describe, it } from "node:test";
import { createContracts, type GatewayEvent } from "../../contracts/index.js";
import { createDb, type Db } from "../../lib/db.js";
import { createLogger } from "../../lib/logger.js";
import { createTestDb, eventually, type TestDb, testDatabaseUrl } from "../../lib/test-db.js";
import { createEventConsumer, type EventConsumer, type EventHandler } from "./consumer.js";

const logger = createLogger({ level: "silent", service: "core", version: "test" });
const contracts = createContracts();

function connectionEvent(state: string, at = new Date()): GatewayEvent {
  return contracts.parseEvent({
    id: randomUUID(),
    version: 1,
    type: "connection.state",
    occurred_at: at.toISOString(),
    data: { state },
  });
}

describe("gateway event consumer", {
  skip: !testDatabaseUrl && "TEST_DATABASE_URL not set",
}, () => {
  let testDb: TestDb;
  let db: Db;
  let consumer: EventConsumer | undefined;

  before(async () => {
    testDb = await createTestDb(testDatabaseUrl ?? "");
    db = createDb(testDb.url("kaya_core"), logger);
  });

  after(async () => {
    await consumer?.stop();
    await db.close();
    await testDb.drop();
  });

  async function add(event: unknown, id = (event as { id: string }).id, type = "connection.state") {
    await testDb.admin.query("INSERT INTO gateway_events (id, type, payload) VALUES ($1, $2, $3)", [
      id,
      type,
      JSON.stringify(event),
    ]);
  }

  async function states(): Promise<string[]> {
    const { rows } = await testDb.admin.query<{ line: string }>(
      "SELECT state || coalesce(' ' || error, '') AS line FROM gateway_events ORDER BY seq",
    );
    return rows.map((row) => row.line);
  }

  async function reset() {
    await consumer?.stop();
    consumer = undefined;
    await testDb.admin.query("TRUNCATE gateway_events");
    await testDb.admin.query("DROP TABLE IF EXISTS seen");
    await testDb.admin.query("CREATE TABLE seen (state text); GRANT ALL ON seen TO kaya_core");
  }

  async function run(
    handle: EventHandler,
    options: { pollMs?: number; purgeEveryMs?: number; keepQueue?: boolean } = {},
  ) {
    if (!options.keepQueue) await reset();
    consumer = createEventConsumer({
      db,
      databaseUrl: testDb.url("kaya_core"),
      contracts,
      logger,
      handle,
      pollMs: options.pollMs ?? 60_000,
      retryMs: 5,
      maxRetryMs: 20,
      ...(options.purgeEveryMs === undefined ? {} : { purgeEveryMs: options.purgeEveryMs }),
    });
    consumer.start();
  }

  const record: EventHandler = async (event, tx) => {
    if (event.type === "connection.state") {
      await tx.query("INSERT INTO seen (state) VALUES ($1)", [event.data.state]);
    }
    return undefined;
  };

  async function seen(): Promise<string[]> {
    const { rows } = await testDb.admin.query<{ state: string }>(
      "SELECT state FROM seen ORDER BY ctid",
    );
    return rows.map((row) => row.state);
  }

  it("handles events in order, woken by the insert and not by the poll", async () => {
    await run(record);
    await eventually("the listener", async () => {
      const { rows } = await testDb.admin.query(
        "SELECT 1 FROM pg_stat_activity WHERE application_name = 'kaya-core-listen' AND datname = current_database()",
      );
      return rows.length > 0;
    });
    for (const state of ["connecting", "connected", "disconnected"])
      await add(connectionEvent(state));

    await eventually("three handled events", async () => (await seen()).length === 3);
    assert.deepEqual(await seen(), ["connecting", "connected", "disconnected"]);
    assert.deepEqual(await states(), ["handled", "handled", "handled"]);
  });

  it("drains what was queued before it started", async () => {
    await reset();
    await add(connectionEvent("connected"));
    await add(connectionEvent("disconnected"));
    await run(record, { keepQueue: true });
    await eventually("both events", async () => (await seen()).length === 2);
    assert.deepEqual(await seen(), ["connected", "disconnected"]);
    assert.deepEqual(await states(), ["handled", "handled"]);
  });

  it("sets aside an event that does not fit the contract and goes on", async () => {
    await run(record);
    const id = randomUUID();
    await add({ id, type: "connection.state", data: { state: "sleeping" } }, id);
    await add(connectionEvent("connected"));
    await eventually("the valid event", async () => (await seen()).length === 1);
    assert.deepEqual(await states(), ["dead invalid_event", "handled"]);
  });

  it("sets aside an event whose handler fails, undoing what the handler wrote", async () => {
    await run(async (event, tx) => {
      await record(event, tx);
      if (event.type === "connection.state" && event.data.state === "logged_out") {
        throw new Error("handler bug");
      }
      return undefined;
    });
    await add(connectionEvent("connecting"));
    await add(connectionEvent("logged_out"));
    await add(connectionEvent("connected"));
    await eventually("the event after the bad one", async () =>
      (await seen()).includes("connected"),
    );
    assert.deepEqual(await seen(), ["connecting", "connected"]);
    assert.deepEqual(await states(), ["handled", "dead handler_failed", "handled"]);
  });

  it("retries an event after a connection failure without skipping or reordering", async () => {
    let failures = 2;
    await run(async (event, tx) => {
      if (event.type === "connection.state" && event.data.state === "connected" && failures-- > 0) {
        throw Object.assign(new Error("Connection terminated unexpectedly"), { code: "08006" });
      }
      return record(event, tx);
    });
    await add(connectionEvent("connected"));
    await add(connectionEvent("disconnected"));
    await eventually("both events", async () => (await seen()).length === 2);
    assert.deepEqual(await seen(), ["connected", "disconnected"]);
    assert.deepEqual(await states(), ["handled", "handled"]);
  });

  it("runs a follow-up only once the event is committed as handled", async () => {
    const observed: string[] = [];
    let check: Promise<void> | undefined;
    await run(async () => () => {
      check = states().then((lines) => {
        observed.push(...lines);
      });
    });
    await add(connectionEvent("connected"));
    await eventually("the follow-up", async () => check !== undefined);
    await check;
    assert.deepEqual(observed, ["handled"]);
  });

  it("deletes handled events after a week and keeps the rest", async () => {
    await run(record, { pollMs: 10, purgeEveryMs: 0 });
    const old = connectionEvent("connected");
    const dead = connectionEvent("connected");
    await add(old);
    await add(connectionEvent("disconnected"));
    await add(dead);
    await eventually("handled events", async () => (await states()).every((s) => s === "handled"));
    await testDb.admin.query(
      "UPDATE gateway_events SET finished_at = now() - interval '8 days' WHERE id = $1",
      [old.id],
    );
    await testDb.admin.query(
      "UPDATE gateway_events SET state = 'dead', error = 'handler_failed', finished_at = now() - interval '8 days' WHERE id = $1",
      [dead.id],
    );
    await eventually("the purge", async () => (await states()).length === 2);
    assert.deepEqual(await states(), ["handled", "dead handler_failed"]);
  });

  it("reports through its health check whether it is running", async () => {
    await run(record);
    await consumer?.check(AbortSignal.timeout(100));
    const stopped = performance.now();
    await consumer?.stop();
    assert.ok(performance.now() - stopped < 1_000, "stop must not wait out the poll");
    await assert.rejects(async () => consumer?.check(AbortSignal.timeout(100)));
  });

  it("stops at once, whatever it is in the middle of", async () => {
    for (const waitMs of [0, 1, 3, 10, 30]) {
      await run(record);
      await new Promise((resolve) => setTimeout(resolve, waitMs));
      const stopped = performance.now();
      await consumer?.stop();
      assert.ok(performance.now() - stopped < 1_000, `stop took too long ${waitMs} ms after start`);
    }
  });
});
