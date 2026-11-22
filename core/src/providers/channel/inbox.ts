import type pg from "pg";
import type { MessageStored } from "../../contracts/index.js";
import { isConnectionError } from "../../lib/db.js";
import type { Logger } from "../../lib/logger.js";
import type { IncomingMessage } from "./types.js";

export type InboxOptions = Readonly<{
  pool: pg.Pool;
  logger: Logger;
  pollMs?: number;
  retryMs?: number;
  maxRetryMs?: number;
  maxAttempts?: number;
  retentionMs?: number;
  purgeEveryMs?: number;
}>;

export type Inbox = Readonly<{
  record(message: MessageStored, tx: pg.ClientBase): Promise<boolean>;
  start(deliver: (message: IncomingMessage) => Promise<void>): void;
  stop(): Promise<void>;
  wake(): void;
}>;

type Row = { id: string; message_id: string; wa_id: string; sent_at: Date; attempts: number };

function failureCode(err: unknown): string {
  const code = (err as { code?: unknown } | null)?.code;
  return typeof code === "string" && /^[a-z][a-z0-9_]*$/.test(code) ? code : "handler_failed";
}

export function createInbox(options: InboxOptions): Inbox {
  const { pool, logger } = options;
  const pollMs = options.pollMs ?? 30_000;
  const retryMs = options.retryMs ?? 1_000;
  const maxRetryMs = options.maxRetryMs ?? 30_000;
  const maxAttempts = options.maxAttempts ?? 5;
  const retentionMs = options.retentionMs ?? 30 * 24 * 60 * 60_000;
  const purgeEveryMs = options.purgeEveryMs ?? 60 * 60_000;

  let running = false;
  let loop: Promise<void> | undefined;
  let woken = false;
  let wakeUp: (() => void) | undefined;
  let nextPurge = 0;

  const wake = () => {
    woken = true;
    wakeUp?.();
  };

  const pause = (ms: number) =>
    new Promise<void>((resolve) => {
      if (woken || !running) {
        woken = false;
        resolve();
        return;
      }
      const timer = setTimeout(done, ms);
      function done() {
        clearTimeout(timer);
        wakeUp = undefined;
        woken = false;
        resolve();
      }
      wakeUp = done;
    });

  const finish = (id: string, error?: string) =>
    pool.query(
      "UPDATE commands SET state = $2, error = $3, finished_at = now() WHERE id = $1 AND state = 'pending'",
      [id, error ? "failed" : "done", error ?? null],
    );

  async function step(deliver: (message: IncomingMessage) => Promise<void>): Promise<boolean> {
    const { rows } = await pool.query<Row>(
      `UPDATE commands SET attempts = attempts + 1
       WHERE id = (SELECT id FROM commands WHERE state = 'pending' ORDER BY id LIMIT 1)
       RETURNING id, message_id, wa_id, sent_at, attempts`,
    );
    const row = rows[0];
    if (!row) return false;
    if (row.attempts > maxAttempts) {
      logger.error({ command_id: row.id }, "a command kept being interrupted and was given up on");
      await finish(row.id, "interrupted");
      return true;
    }

    const found = await pool.query<{ body: string | null }>(
      "SELECT body FROM messages WHERE id = $1 AND deleted_at IS NULL",
      [row.message_id],
    );
    const text = found.rows[0]?.body?.trim();
    if (!text) {
      await finish(row.id, "message_gone");
      return true;
    }

    try {
      await deliver({ id: row.wa_id, text, receivedAt: row.sent_at });
    } catch (err) {
      if (!running) return false;
      if (isConnectionError(err)) throw err;
      const code = failureCode(err);
      logger.error({ err, command_id: row.id, error_code: code }, "a command failed");
      await finish(row.id, code);
      return true;
    }
    await finish(row.id);
    return true;
  }

  async function purge(): Promise<void> {
    if (Date.now() < nextPurge) return;
    nextPurge = Date.now() + purgeEveryMs;
    await pool.query("DELETE FROM commands WHERE finished_at < now() - make_interval(secs => $1)", [
      retentionMs / 1_000,
    ]);
  }

  async function run(deliver: (message: IncomingMessage) => Promise<void>): Promise<void> {
    let delay = retryMs;
    let failing = false;
    while (running) {
      try {
        const worked = await step(deliver);
        if (failing) logger.info("commands are being handled again");
        failing = false;
        delay = retryMs;
        if (worked) continue;
        await purge();
        await pause(pollMs);
      } catch (err) {
        if (!running) return;
        if (!failing) logger.warn({ err }, "could not work through commands, trying again");
        failing = true;
        await pause(delay);
        delay = Math.min(delay * 2, maxRetryMs);
      }
    }
  }

  return Object.freeze({
    async record(message: MessageStored, tx: pg.ClientBase) {
      const typedByDj =
        message.from_me &&
        message.kind === "text" &&
        message.source === "live" &&
        message.change === "new";
      if (!typedByDj) return false;
      const { rowCount } = await tx.query(
        `INSERT INTO commands (message_id, wa_id, sent_at)
         SELECT m.id, m.wa_id, m.sent_at
         FROM messages m
         JOIN chats c ON c.id = m.chat_id
         JOIN contacts own ON own.id = m.sender_id
         JOIN connection_state linked ON linked.own_jid IN (own.pn, own.lid)
         WHERE m.id = $1
           AND m.from_me AND m.kind = 'text' AND m.source = 'live' AND m.deleted_at IS NULL
           AND c.kind = 'direct' AND c.contact_id = m.sender_id
           AND NOT EXISTS (SELECT 1 FROM send_requests sent WHERE sent.wa_id = m.wa_id)
         ON CONFLICT (message_id) DO NOTHING`,
        [message.message_id],
      );
      return (rowCount ?? 0) > 0;
    },
    start(deliver: (message: IncomingMessage) => Promise<void>) {
      if (running) return;
      running = true;
      loop = run(deliver);
    },
    async stop() {
      if (!running) return;
      running = false;
      wake();
      await loop;
    },
    wake,
  });
}
