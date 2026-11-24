import assert from "node:assert/strict";
import { after, before, beforeEach, describe, it } from "node:test";
import pg from "pg";
import { createContracts, type SendRequest } from "../../contracts/index.js";
import { createTestDb, eventually, type TestDb, testDatabaseUrl } from "../../lib/test-db.js";
import {
  ChannelNotStartedError,
  createGatewayChannel,
  GatewayNotLinkedError,
  IdempotencyKeyReusedError,
  requestIdFor,
  SendFailedError,
} from "./index.js";

const contracts = createContracts();
const own = "919999999999@s.whatsapp.net";

describe("requestIdFor", () => {
  it("gives the same lowercase UUID for the same key and another for another", () => {
    const id = requestIdFor("reply-to-42");
    assert.match(id, /^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
    assert.equal(requestIdFor("reply-to-42"), id);
    assert.notEqual(requestIdFor("reply-to-43"), id);
  });
});

describe("gateway channel", { skip: !testDatabaseUrl && "TEST_DATABASE_URL not set" }, () => {
  let testDb: TestDb;
  let core: pg.Pool;

  before(async () => {
    testDb = await createTestDb(testDatabaseUrl ?? "");
    core = new pg.Pool({ connectionString: testDb.url("kaya_core"), max: 4 });
  });

  after(async () => {
    await core.end();
    await testDb.drop();
  });

  beforeEach(async () => {
    await testDb.admin.query("TRUNCATE send_requests, connection_state");
  });

  async function linked() {
    await testDb.admin.query(
      "INSERT INTO connection_state (state, own_jid, occurred_at) VALUES ('connected', $1, now())",
      [own],
    );
  }

  async function channel(
    options: { pollMs?: number; graceMs?: number; sendingGraceMs?: number } = {},
  ) {
    const c = createGatewayChannel({
      pool: core,
      contracts,
      pollMs: options.pollMs ?? 5,
      ...options,
    });
    await c.start();
    return c;
  }

  async function queued(): Promise<{ id: string; payload: SendRequest; state: string }[]> {
    const { rows } = await testDb.admin.query<{ id: string; payload: SendRequest; state: string }>(
      "SELECT id::text, payload, state FROM send_requests ORDER BY seq",
    );
    return rows;
  }

  async function gatewaySends(id: string, waId: string) {
    await testDb.admin.query(
      "UPDATE send_requests SET state = 'sending', wa_id = $2 WHERE id = $1",
      [id, waId],
    );
    await testDb.admin.query(
      "UPDATE send_requests SET state = 'sent', finished_at = now() WHERE id = $1",
      [id],
    );
  }

  it("queues a self-chat request and answers once the gateway has sent it", async () => {
    await linked();
    const c = await channel();
    const sending = c.send({ idempotencyKey: "k1", text: "  hello DJ  ", replyTo: "3EB0QUESTION" });

    const [row] = await eventually("the queued request", async () => {
      const rows = await queued();
      return rows.length > 0 && rows;
    });
    assert.ok(row);
    const request = contracts.parseSendRequest(row.payload);
    assert.equal(row.id, requestIdFor("k1"));
    assert.equal(row.state, "pending");
    assert.equal(request.to, own);
    assert.deepEqual(request.authorization, { kind: "self_chat" });
    assert.deepEqual(request.content, { type: "text", text: "hello DJ" });
    assert.deepEqual(request.reply_to, { wa_id: "3EB0QUESTION", sender_jid: own });

    await gatewaySends(row.id, "3EB0ANSWER");
    const sent = await sending;
    assert.equal(sent.id, "3EB0ANSWER");
    assert.ok(sent.sentAt instanceof Date);
  });

  it("sends a repeated idempotency key only once", async () => {
    await linked();
    const c = await channel();
    const first = c.send({ idempotencyKey: "same", text: "hi" });
    const second = c.send({ idempotencyKey: "same", text: "hi" });
    const rows = await eventually("the queued request", async () => {
      const found = await queued();
      return found.length > 0 && found;
    });
    await gatewaySends(rows[0]?.id ?? "", "3EB0ONCE");
    assert.deepEqual(await second, await first);
    assert.deepEqual(await c.send({ idempotencyKey: "same", text: "hi" }), await first);
    assert.equal((await queued()).length, 1);
  });

  it("wakes at once when told the result is in", async () => {
    await linked();
    const listeners = new Map<string, () => void>();
    const c = createGatewayChannel({
      pool: core,
      contracts,
      pollMs: 60_000,
      results: {
        onSendResult(id, listener) {
          listeners.set(id, listener);
          return () => listeners.delete(id);
        },
      },
    });
    await c.start();
    const sending = c.send({ idempotencyKey: "fast", text: "hi" });
    const id = requestIdFor("fast");
    await eventually("the waiter", async () => listeners.has(id));
    await gatewaySends(id, "3EB0FAST");
    listeners.get(id)?.();
    assert.equal((await sending).id, "3EB0FAST");
    assert.equal(listeners.size, 0);
  });

  it("reports why the gateway did not send", async () => {
    await linked();
    const c = await channel();
    const sending = c.send({ idempotencyKey: "refused", text: "hi" });
    const id = requestIdFor("refused");
    await eventually("the queued request", async () => (await queued()).length === 1);
    await testDb.admin.query(
      "UPDATE send_requests SET state = 'failed', error = 'outcome_unknown', finished_at = now() WHERE id = $1",
      [id],
    );
    await assert.rejects(
      sending,
      (err: unknown) =>
        err instanceof SendFailedError && err.code === "outcome_unknown" && err.requestId === id,
    );
  });

  it("keeps waiting while the gateway is sending, and says unknown if it never hears back", async () => {
    await linked();
    const day = 24 * 60 * 60_000;
    const id = requestIdFor("slow");
    const first = (await channel()).send({ idempotencyKey: "slow", text: "hi" });
    await eventually("the queued request", async () => (await queued()).length === 1);
    await testDb.admin.query(
      "UPDATE send_requests SET state = 'sending', wa_id = '3EB0SLOW' WHERE id = $1",
      [id],
    );

    const pastExpiry = await channel({ graceMs: -day, sendingGraceMs: 2 * day });
    let settled = false;
    const patient = pastExpiry.send({ idempotencyKey: "slow", text: "hi" }).finally(() => {
      settled = true;
    });
    await new Promise((resolve) => setTimeout(resolve, 60));
    assert.equal(
      settled,
      false,
      "a request that is being sent may still go out, even past its expiry",
    );

    const longAfter = await channel({ graceMs: -day, sendingGraceMs: 0 });
    await assert.rejects(
      longAfter.send({ idempotencyKey: "slow", text: "hi" }),
      (err: unknown) => err instanceof SendFailedError && err.code === "outcome_unknown",
    );

    await testDb.admin.query(
      "UPDATE send_requests SET state = 'sent', finished_at = now() WHERE id = $1",
      [id],
    );
    assert.equal((await first).id, "3EB0SLOW");
    assert.equal((await patient).id, "3EB0SLOW");
  });

  it("refuses a key that was already used for a different message", async () => {
    await linked();
    const c = await channel();
    const first = c.send({ idempotencyKey: "reused", text: "the first message" });
    await eventually("the queued request", async () => (await queued()).length === 1);
    await assert.rejects(
      c.send({ idempotencyKey: "reused", text: "a different message" }),
      IdempotencyKeyReusedError,
    );
    await assert.rejects(
      c.send({ idempotencyKey: "reused", text: "the first message", replyTo: "3EB0OTHER" }),
      IdempotencyKeyReusedError,
    );
    await gatewaySends(requestIdFor("reused"), "3EB0FIRST");
    assert.equal((await first).id, "3EB0FIRST");
    assert.equal((await queued()).length, 1);
  });

  it("keeps the text queued first when told the text may change, but still checks the reply", async () => {
    await linked();
    const c = await channel();
    const first = c.send({ idempotencyKey: "status", text: "up 1m" });
    await eventually("the queued request", async () => (await queued()).length === 1);
    const retried = c.send({ idempotencyKey: "status", text: "up 2m" }, { textMayChange: true });
    await assert.rejects(
      c.send(
        { idempotencyKey: "status", text: "up 2m", replyTo: "3EB0OTHER" },
        { textMayChange: true },
      ),
      IdempotencyKeyReusedError,
    );
    await gatewaySends(requestIdFor("status"), "3EB0STATUS");
    assert.equal((await retried).id, "3EB0STATUS");
    assert.equal((await first).id, "3EB0STATUS");
    const rows = await queued();
    assert.equal(rows.length, 1);
    assert.deepEqual(rows[0]?.payload.content, { type: "text", text: "up 1m" });
  });

  it("follows a message already queued under a key, and reports none when there is none", async () => {
    await linked();
    const c = await channel();
    assert.equal(await c.follow("never-sent"), undefined);
    const first = c.send({ idempotencyKey: "followed", text: "whatever it said" });
    await eventually("the queued request", async () => (await queued()).length === 1);
    const following = c.follow("followed");
    await gatewaySends(requestIdFor("followed"), "3EB0FOLLOWED");
    assert.equal((await following)?.id, "3EB0FOLLOWED");
    assert.equal((await first).id, "3EB0FOLLOWED");
    assert.equal((await c.follow("followed"))?.id, "3EB0FOLLOWED");
    assert.equal((await queued()).length, 1, "following never queues anything");
  });

  it("stops waiting even when the caller gives up in the middle of a check", async () => {
    await linked();
    const giveUp = new AbortController();
    let checks = 0;
    const watched = {
      query: (text: string, values?: unknown[]) => {
        if (text.includes("FROM send_requests") && ++checks === 2) {
          giveUp.abort(new Error("gave up"));
        }
        return core.query(text, values);
      },
    } as unknown as pg.Pool;
    const c = createGatewayChannel({ pool: watched, contracts, pollMs: 60_000 });
    await c.start();
    const started = performance.now();
    await assert.rejects(
      c.send({ idempotencyKey: "mid-check", text: "hi" }, { signal: giveUp.signal }),
      /gave up/,
    );
    assert.ok(performance.now() - started < 5_000);
  });

  it("gives up waiting a while after the request has expired", async () => {
    await linked();
    const c = await channel({ graceMs: -24 * 60 * 60_000 });
    await assert.rejects(
      c.send({ idempotencyKey: "nobody home", text: "hi" }),
      (err: unknown) => err instanceof SendFailedError && err.code === "no_answer",
    );
    assert.equal((await queued())[0]?.state, "pending");
  });

  it("stops waiting when the caller gives up, leaving the request queued", async () => {
    await linked();
    const c = await channel({ pollMs: 60_000 });
    const giveUp = new AbortController();
    const sending = c.send({ idempotencyKey: "impatient", text: "hi" }, { signal: giveUp.signal });
    await eventually("the queued request", async () => (await queued()).length === 1);
    giveUp.abort(new Error("gave up"));
    await assert.rejects(sending, /gave up/);
  });

  it("ends a wait at once when the channel stops", async () => {
    await linked();
    const c = await channel({ pollMs: 60_000 });
    const sending = c.send({ idempotencyKey: "interrupted by a stop", text: "hi" });
    sending.catch(() => {});
    await eventually("the queued request", async () => (await queued()).length === 1);
    const began = performance.now();
    await c.stop();
    await assert.rejects(sending, ChannelNotStartedError);
    assert.ok(performance.now() - began < 1_000);
    assert.equal((await queued())[0]?.state, "pending", "the request itself stays queued");
  });

  it("lets one message set how long it stays worth sending", async () => {
    await linked();
    const c = await channel();
    const sending = c.send({ idempotencyKey: "short-lived", text: "hi" }, { ttlMs: 45_000 });
    sending.catch(() => {});
    const [row] = await eventually("the queued request", async () => {
      const rows = await queued();
      return rows.length > 0 && rows;
    });
    const request = contracts.parseSendRequest(row?.payload);
    assert.equal(Date.parse(request.expires_at) - Date.parse(request.created_at), 45_000);
    await c.stop();
  });

  it("refuses to send before an account is linked, before start and when empty", async () => {
    const c = await channel();
    await assert.rejects(c.send({ idempotencyKey: "k", text: "hi" }), GatewayNotLinkedError);
    await testDb.admin.query(
      "INSERT INTO connection_state (state, reason, occurred_at) VALUES ('logged_out', 'not paired: run make pair', now())",
    );
    await assert.rejects(c.send({ idempotencyKey: "k", text: "hi" }), GatewayNotLinkedError);
    await assert.rejects(c.send({ idempotencyKey: "k", text: "   " }));
    await assert.rejects(c.send({ idempotencyKey: " ", text: "hi" }));
    assert.equal((await queued()).length, 0);

    await c.check(AbortSignal.timeout(100));
    await c.stop();
    await assert.rejects(c.send({ idempotencyKey: "k", text: "hi" }), ChannelNotStartedError);
    await assert.rejects(c.check(AbortSignal.timeout(100)), ChannelNotStartedError);
  });
});
