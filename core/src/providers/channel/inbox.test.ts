import assert from "node:assert/strict";
import { after, before, beforeEach, describe, it } from "node:test";
import pg from "pg";
import type { MessageStored } from "../../contracts/index.js";
import { createLogger } from "../../lib/logger.js";
import { createTestDb, eventually, type TestDb, testDatabaseUrl } from "../../lib/test-db.js";
import { createInbox, type Inbox } from "./inbox.js";
import type { IncomingMessage } from "./types.js";

const logger = createLogger({ level: "silent", service: "core", version: "test" });
const ownPn = "919999999999@s.whatsapp.net";
const ownLid = "100000000000001@lid";

describe("command inbox", { skip: !testDatabaseUrl && "TEST_DATABASE_URL not set" }, () => {
  let testDb: TestDb;
  let core: pg.Pool;
  let own: number;
  let friend: number;
  let selfChat: number;
  let friendChat: number;
  let group: number;
  let inbox: Inbox | undefined;
  let next = 0;

  before(async () => {
    testDb = await createTestDb(testDatabaseUrl ?? "");
    core = new pg.Pool({ connectionString: testDb.url("kaya_core"), max: 4 });
    const one = async (sql: string, values: unknown[] = []) =>
      Number((await testDb.admin.query<{ id: string }>(sql, values)).rows[0]?.id);
    own = await one("SELECT contact_id_for($1, $2) AS id", [ownLid, ownPn]);
    friend = await one("SELECT contact_id_for(NULL, '918888888888@s.whatsapp.net') AS id");
    selfChat = await one(
      "INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id",
      [own],
    );
    friendChat = await one(
      "INSERT INTO chats (kind, contact_id) VALUES ('direct', $1) RETURNING id",
      [friend],
    );
    group = await one(
      "INSERT INTO chats (kind, jid) VALUES ('group', '120363000000000000@g.us') RETURNING id",
    );
  });

  after(async () => {
    await inbox?.stop();
    await core.end();
    await testDb.drop();
  });

  beforeEach(async () => {
    await inbox?.stop();
    inbox = undefined;
    await testDb.admin.query("TRUNCATE commands, send_requests, connection_state");
    await testDb.admin.query("DELETE FROM messages");
    await testDb.admin.query(
      "INSERT INTO connection_state (state, own_jid, occurred_at) VALUES ('connected', $1, now())",
      [ownPn],
    );
  });

  type Fixture = Partial<{
    chat: number;
    sender: number;
    fromMe: boolean;
    kind: string;
    source: "live" | "history";
    change: "new" | "edited" | "deleted";
    body: string | null;
    deleted: boolean;
    forwarded: boolean;
    sentAt: Date;
  }>;

  async function message(fixture: Fixture = {}): Promise<MessageStored> {
    const fromMe = fixture.fromMe ?? true;
    const waId = `3EB0TEST${String(++next).padStart(4, "0")}`;
    const sentAt = fixture.sentAt ?? new Date();
    const { rows } = await testDb.admin.query<{ id: string }>(
      `INSERT INTO messages (chat_id, wa_id, sender_id, from_me, sent_at, kind, body, source, deleted_at, forwarded)
       VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`,
      [
        fixture.chat ?? selfChat,
        waId,
        fixture.sender ?? (fromMe ? own : friend),
        fromMe,
        sentAt,
        fixture.kind ?? "text",
        fixture.body === undefined ? "do the thing" : fixture.body,
        fixture.source ?? "live",
        fixture.deleted ? new Date() : null,
        fixture.forwarded ?? false,
      ],
    );
    return {
      message_id: Number(rows[0]?.id),
      chat_id: fixture.chat ?? selfChat,
      sender_id: fixture.sender ?? (fromMe ? own : friend),
      wa_id: waId,
      from_me: fromMe,
      kind: (fixture.kind ?? "text") as MessageStored["kind"],
      source: fixture.source ?? "live",
      change: fixture.change ?? "new",
      sent_at: sentAt.toISOString(),
    };
  }

  async function record(stored: MessageStored): Promise<boolean> {
    const box = inbox ?? createInbox({ pool: core, logger });
    const client = await core.connect();
    try {
      return await box.record(stored, client);
    } finally {
      client.release();
    }
  }

  async function commands(): Promise<string[]> {
    const { rows } = await testDb.admin.query<{ line: string }>(
      "SELECT wa_id || ' ' || state || coalesce(' ' || error, '') AS line FROM commands ORDER BY id",
    );
    return rows.map((row) => row.line);
  }

  it("takes a text DJ typed in his own chat, once", async () => {
    const typed = await message();
    assert.equal(await record(typed), true);
    assert.equal(await record(typed), false, "the same message is never a second command");
    assert.deepEqual(await commands(), [`${typed.wa_id} pending`]);
  });

  it("takes nothing else", async () => {
    const cases: Record<string, MessageStored> = {
      "a friend's message": await message({ chat: friendChat, fromMe: false }),
      "DJ's message to a friend": await message({ chat: friendChat }),
      "DJ's message in a group": await message({ chat: group }),
      "an old message from the history sync": await message({ source: "history" }),
      "an edit": await message({ change: "edited" }),
      "a deletion": await message({ change: "deleted" }),
      "a photo": await message({ kind: "image", body: "a caption" }),
      "a message deleted before it was looked at": await message({ deleted: true }),
      "someone else's words, forwarded by DJ to himself": await message({ forwarded: true }),
      "a message from two days ago, only handled now": await message({
        sentAt: new Date(Date.now() - 2 * 24 * 60 * 60_000),
      }),
    };
    for (const [what, stored] of Object.entries(cases)) {
      assert.equal(await record(stored), false, what);
    }

    const fromKaya = await message();
    await testDb.admin.query(
      `INSERT INTO send_requests (id, payload, expires_at)
       VALUES ('b3bb189e-8bf9-4888-9912-ace4e6543001',
               '{"id": "b3bb189e-8bf9-4888-9912-ace4e6543001", "expires_at": "2026-11-23T10:00:00Z"}',
               '2026-11-23T10:00:00Z')`,
    );
    await testDb.admin.query("UPDATE send_requests SET state = 'sending', wa_id = $1", [
      fromKaya.wa_id,
    ]);
    assert.equal(await record(fromKaya), false, "a message Kaya sent is not a command to Kaya");

    const typed = await message();
    await testDb.admin.query("UPDATE connection_state SET own_jid = '917777777777@s.whatsapp.net'");
    assert.equal(
      await record(typed),
      false,
      "the sender must be the account the gateway says is linked",
    );
    await testDb.admin.query("UPDATE connection_state SET own_jid = $1", [ownLid]);
    assert.equal(await record(typed), true, "either spelling of DJ's own address counts");

    assert.deepEqual(await commands(), [`${typed.wa_id} pending`]);
  });

  function started(
    deliver: (message: IncomingMessage, signal: AbortSignal) => Promise<void>,
    options: {
      maxAttempts?: number;
      maxPerMinute?: number;
      retentionMs?: number;
      purgeEveryMs?: number;
      exemptFromCap?: (text: string) => boolean;
    } = {},
    ready: () => boolean = () => true,
  ): Inbox {
    inbox = createInbox({
      pool: core,
      logger,
      pollMs: 60_000,
      retryMs: 5,
      maxRetryMs: 20,
      ...options,
    });
    inbox.start(deliver, ready);
    return inbox;
  }

  const attempts = async () =>
    (
      await testDb.admin.query<{ attempts: number }>("SELECT attempts FROM commands ORDER BY id")
    ).rows.map((row) => row.attempts);

  const untilStopped = (signal: AbortSignal) =>
    new Promise<void>((_, reject) => {
      signal.addEventListener("abort", () => reject(signal.reason), { once: true });
    });

  it("hands commands over oldest first, one at a time, with the text and both times", async () => {
    const sentAt = new Date(Date.now() - 5 * 60_000);
    const first = await message({ body: "  first  ", sentAt });
    const second = await message({ body: "second" });
    await record(first);
    await record(second);

    const seen: IncomingMessage[] = [];
    let running = 0;
    let overlapped = false;
    started(async (incoming) => {
      overlapped ||= ++running > 1;
      await new Promise((resolve) => setTimeout(resolve, 20));
      seen.push(incoming);
      running--;
    });
    await eventually("both commands", async () => seen.length === 2);
    assert.deepEqual(
      seen.map((m) => [m.id, m.text]),
      [
        [first.wa_id, "first"],
        [second.wa_id, "second"],
      ],
    );
    assert.deepEqual(seen[0]?.sentAt, sentAt);
    const takenIn = seen[0]?.receivedAt.getTime() ?? 0;
    assert.ok(
      Math.abs(takenIn - Date.now()) < 60_000,
      "receivedAt is when core recorded the command",
    );
    assert.equal(overlapped, false);
    await eventually("both marked done", async () =>
      (await commands()).every((c) => c.endsWith("done")),
    );
  });

  it("wakes for a command recorded while it is idle", async () => {
    const seen: string[] = [];
    const box = started(async (incoming) => {
      seen.push(incoming.text);
    });
    await new Promise((resolve) => setTimeout(resolve, 30));
    await record(await message({ body: "typed just now" }));
    box.wake();
    await eventually("the command, without waiting for the poll", async () => seen.length === 1);
  });

  it("keeps commands waiting while nobody is there to handle them", async () => {
    const typed = await message();
    await record(typed);
    let ready = false;
    const seen: string[] = [];
    const box = started(
      async (incoming) => {
        seen.push(incoming.id);
      },
      {},
      () => ready,
    );
    await new Promise((resolve) => setTimeout(resolve, 40));
    assert.deepEqual(await commands(), [`${typed.wa_id} pending`]);
    assert.deepEqual(await attempts(), [0]);
    ready = true;
    box.wake();
    await eventually("the command once a handler exists", async () => seen.length === 1);
  });

  it("marks a failed command and carries on with the next", async () => {
    const bad = await message({ body: "this one breaks" });
    const gone = await message({ body: "deleted before its turn" });
    const good = await message({ body: "this one works" });
    for (const stored of [bad, gone, good]) await record(stored);
    await testDb.admin.query("UPDATE messages SET deleted_at = now(), body = NULL WHERE id = $1", [
      gone.message_id,
    ]);

    const seen: string[] = [];
    started(async (incoming) => {
      if (incoming.text.includes("breaks")) {
        throw Object.assign(new Error("the reply was not sent"), { code: "no_answer" });
      }
      seen.push(incoming.text);
    });
    await eventually("the last command", async () => (await commands())[2]?.endsWith("done"));
    assert.deepEqual(seen, ["this one works"]);
    assert.deepEqual(await commands(), [
      `${bad.wa_id} failed no_answer`,
      `${gone.wa_id} failed message_gone`,
      `${good.wa_id} done`,
    ]);
  });

  it("drops commands that come faster than a person types", async () => {
    const typed = [];
    for (let i = 0; i < 5; i++) typed.push(await message({ body: `command ${i}` }));
    for (const stored of typed) await record(stored);
    const seen: string[] = [];
    started(
      async (incoming) => {
        seen.push(incoming.text);
      },
      { maxPerMinute: 3 },
    );
    await eventually("all five to be settled", async () =>
      (await commands()).every((c) => !c.endsWith("pending")),
    );
    assert.deepEqual(seen, ["command 0", "command 1", "command 2"]);
    assert.deepEqual(
      (await commands()).map((c) => c.split(" ").slice(1).join(" ")),
      ["done", "done", "done", "failed rate_limited", "failed rate_limited"],
    );
  });

  it("never drops a command the cap was told to let through", async () => {
    const typed = [];
    for (let i = 0; i < 4; i++) typed.push(await message({ body: `command ${i}` }));
    typed.push(await message({ body: "/pause" }));
    for (const stored of typed) await record(stored);
    const seen: string[] = [];
    started(
      async (incoming) => {
        seen.push(incoming.text);
      },
      { maxPerMinute: 2, exemptFromCap: (text) => text.startsWith("/") },
    );
    await eventually("all five to be settled", async () =>
      (await commands()).every((c) => !c.endsWith("pending")),
    );
    assert.deepEqual(seen, ["command 0", "command 1", "/pause"]);
  });

  it("runs each command in one worker even when two are started", async () => {
    for (let i = 0; i < 6; i++) await record(await message({ body: `command ${i}` }));
    const calls: string[] = [];
    const deliver = async (incoming: IncomingMessage) => {
      calls.push(incoming.text);
      await new Promise((resolve) => setTimeout(resolve, 5));
    };
    const one = createInbox({ pool: core, logger, pollMs: 10, retryMs: 5 });
    const two = createInbox({ pool: core, logger, pollMs: 10, retryMs: 5 });
    one.start(deliver);
    two.start(deliver);
    try {
      await eventually("all six", async () => (await commands()).every((c) => c.endsWith("done")));
      assert.equal(calls.length, 6, `each command must run once, got ${calls.join(", ")}`);
      assert.deepEqual(await attempts(), [1, 1, 1, 1, 1, 1]);
    } finally {
      await one.stop();
      await two.stop();
    }
  });

  it("stops at once, and an orderly stop never counts against the command", async () => {
    const typed = await message({ body: "waiting on a reply" });
    await record(typed);
    for (let run = 1; run <= 7; run++) {
      let entered = false;
      const box = started(async (_incoming, signal) => {
        entered = true;
        await untilStopped(signal);
      });
      await eventually(`run ${run} to start on it`, async () => entered);
      const began = performance.now();
      await box.stop();
      assert.ok(
        performance.now() - began < 1_000,
        "stop must not wait for the handler's own patience",
      );
      inbox = undefined;
    }
    assert.deepEqual(await commands(), [`${typed.wa_id} pending`]);
    assert.deepEqual(await attempts(), [0]);
  });

  it("gives up on a command that was started too many times without finishing", async () => {
    const typed = await message({ body: "crashes core every time" });
    await record(typed);
    await testDb.admin.query("UPDATE commands SET attempts = 2");
    let ran = false;
    started(
      async () => {
        ran = true;
      },
      { maxAttempts: 2 },
    );
    await eventually("the command to be given up on", async () =>
      (await commands())[0]?.endsWith("failed interrupted"),
    );
    assert.equal(ran, false);
  });

  it("leaves the command pending when the database goes away mid-way, and finishes it later", async () => {
    const typed = await message({ body: "hello" });
    await record(typed);
    let calls = 0;
    started(async () => {
      if (++calls === 1) {
        throw Object.assign(new Error("Connection terminated unexpectedly"), { code: "08006" });
      }
    });
    await eventually("the retry", async () => (await commands())[0]?.endsWith("done"));
    assert.equal(calls, 2);
    assert.deepEqual(await attempts(), [1], "an outage is not the command's fault");
  });

  it("deletes finished commands after a while and keeps the unfinished", async () => {
    const done = await message({ body: "done long ago" });
    await record(done);
    started(
      async () => {},
      { retentionMs: 0, purgeEveryMs: 0 },
      () => true,
    );
    await eventually(
      "the finished command to be deleted",
      async () => (await commands()).length === 0,
    );
    await inbox?.stop();
    inbox = undefined;

    const kept = await message({ body: "still to do" });
    await record(kept);
    started(
      async () => {},
      { retentionMs: 0, purgeEveryMs: 0 },
      () => false,
    );
    await new Promise((resolve) => setTimeout(resolve, 40));
    assert.deepEqual(await commands(), [`${kept.wa_id} pending`]);
  });
});
