import type pg from "pg";
import { isConnectionError } from "../../lib/db.js";
import type { Logger } from "../../lib/logger.js";
import { EmbeddingError, type EmbeddingProvider } from "../../providers/embeddings/index.js";
import { createMaskSession } from "../masking/index.js";
import type { PauseSwitch } from "../pause/index.js";
import { type ChatLine, type Chunk, chunkLines } from "./chunker.js";

export type IndexerOptions = Readonly<{
  pool: pg.Pool;
  embeddings: EmbeddingProvider;
  pause: PauseSwitch;
  logger: Logger;
  timeZone: string;
  settleMs?: number;
  batchMessages?: number;
  pollMs?: number;
  retryMs?: number;
  refusedMs?: number;
}>;

export type IndexProgress = Readonly<{ indexed: number; total: number }>;

export type Indexer = Readonly<{
  start(): void;
  stop(): Promise<void>;
  wake(): void;
  progress(): Promise<IndexProgress>;
  /** Runs one pass and reports how many messages it indexed; for tests and the first backfill. */
  runOnce(): Promise<number>;
}>;

type Row = {
  id: string;
  chat_id: string;
  sent_at: Date;
  sender: string;
  body: string;
  chat_title: string;
};

const indexerLock = 5_011_604;

const linesQuery = `
  SELECT m.id::text, m.chat_id::text, m.sent_at, m.body,
         CASE WHEN m.from_me THEN 'Me'
              ELSE coalesce(nullif(c.saved_name, ''), nullif(c.first_name, ''), nullif(c.push_name, ''),
                            nullif(c.business_name, ''), c.pn, 'Someone') END AS sender,
         coalesce(nullif(ch.name, ''), nullif(cc.saved_name, ''), nullif(cc.push_name, ''), cc.pn,
                  'a chat') AS chat_title
  FROM messages m
  JOIN chats ch ON ch.id = m.chat_id
  LEFT JOIN contacts c ON c.id = m.sender_id
  LEFT JOIN contacts cc ON cc.id = ch.contact_id
  WHERE m.kind = 'text' AND m.body IS NOT NULL AND btrim(m.body) <> '' AND m.deleted_at IS NULL`;

const asVector = (vector: readonly number[]) => `[${vector.join(",")}]`;

export function createIndexer(options: IndexerOptions): Indexer {
  const { pool, embeddings, logger, pause } = options;
  const settleMs = options.settleMs ?? 10 * 60_000;
  const batchMessages = options.batchMessages ?? 2_000;
  const pollMs = options.pollMs ?? 60_000;
  const retryMs = options.retryMs ?? 30_000;
  const refusedMs = options.refusedMs ?? 10 * 60_000;

  let running = false;
  let halted = new AbortController();
  let loop: Promise<void> | undefined;
  let wakeUp: (() => void) | undefined;

  const sleep = (ms: number) =>
    new Promise<void>((resolve) => {
      const timer = setTimeout(done, ms);
      function done() {
        clearTimeout(timer);
        wakeUp = undefined;
        resolve();
      }
      wakeUp = done;
      if (!running) done();
    });

  function toChunks(rows: readonly Row[]): { chatId: string; chunk: Chunk }[] {
    const byChat = new Map<string, Row[]>();
    for (const row of rows) byChat.set(row.chat_id, [...(byChat.get(row.chat_id) ?? []), row]);
    return [...byChat].flatMap(([chatId, chatRows]) =>
      chunkLines(
        chatRows[0]?.chat_title ?? "a chat",
        chatRows.map(
          (row): ChatLine => ({
            messageId: row.id,
            sentAt: row.sent_at,
            sender: row.sender,
            text: row.body,
          }),
        ),
        { timeZone: options.timeZone },
      ).map((chunk) => ({ chatId, chunk })),
    );
  }

  async function store(items: readonly { chatId: string; chunk: Chunk }[], signal: AbortSignal) {
    const texts = items.map(({ chunk }) =>
      embeddings.locality === "cloud" ? createMaskSession().mask(chunk.text) : chunk.text,
    );
    const vectors = await embeddings.embed(texts, "document", { signal });
    const client = await pool.connect();
    try {
      await client.query("BEGIN");
      for (const [i, { chatId, chunk }] of items.entries()) {
        const { rows } = await client.query<{ id: string }>(
          `INSERT INTO chunks (chat_id, first_sent_at, last_sent_at, message_count, text_hash, model, embedding)
           VALUES ($1, $2, $3, $4, $5, $6, $7::halfvec) RETURNING id`,
          [
            chatId,
            chunk.firstSentAt,
            chunk.lastSentAt,
            chunk.messageIds.length,
            chunk.hash,
            embeddings.model,
            asVector(vectors[i] ?? []),
          ],
        );
        await client.query(
          `INSERT INTO message_chunks (message_id, chunk_id)
           SELECT unnest($1::bigint[]), $2 ON CONFLICT (message_id) DO NOTHING`,
          [chunk.messageIds, rows[0]?.id],
        );
      }
      await client.query("COMMIT");
    } catch (err) {
      await client.query("ROLLBACK").catch(() => {});
      throw err;
    } finally {
      client.release();
    }
  }

  /** Drops chunks whose text no longer matches their messages, so they are made again. */
  async function recheck(): Promise<void> {
    const { rows } = await pool.query<{ id: string; text_hash: string }>(
      `SELECT DISTINCT k.id::text, k.text_hash
       FROM chunks k
       JOIN message_chunks mc ON mc.chunk_id = k.id
       JOIN messages m ON m.id = mc.message_id
       WHERE m.updated_at > k.checked_at
       LIMIT 200`,
    );
    for (const chunk of rows) {
      const { rows: lines } = await pool.query<Row>(
        `${linesQuery} AND m.id IN (SELECT message_id FROM message_chunks WHERE chunk_id = $1)
         ORDER BY m.sent_at, m.id`,
        [chunk.id],
      );
      const rebuilt = toChunks(lines);
      const same = rebuilt.length === 1 && rebuilt[0]?.chunk.hash === chunk.text_hash;
      await pool.query(
        same
          ? "UPDATE chunks SET checked_at = now() WHERE id = $1"
          : "DELETE FROM chunks WHERE id = $1",
        [chunk.id],
      );
    }
  }

  async function pass(signal: AbortSignal): Promise<number> {
    await recheck();
    const { rows } = await pool.query<Row>(
      `${linesQuery}
         AND NOT EXISTS (SELECT 1 FROM message_chunks mc WHERE mc.message_id = m.id)
         AND m.sent_at < now() - make_interval(secs => $1)
       ORDER BY m.chat_id, m.sent_at, m.id
       LIMIT $2`,
      [settleMs / 1_000, batchMessages],
    );
    const items = toChunks(rows);
    for (let at = 0; at < items.length; at += embeddings.maxBatch) {
      signal.throwIfAborted();
      await store(items.slice(at, at + embeddings.maxBatch), signal);
    }
    return rows.length;
  }

  async function run(): Promise<void> {
    const guard = await pool.connect().catch(() => undefined);
    let locked = false;
    try {
      while (running) {
        try {
          if (!guard) throw new Error("no connection for the indexer lock");
          if (!locked) {
            const { rows } = await guard.query<{ locked: boolean }>(
              "SELECT pg_try_advisory_lock($1) AS locked",
              [indexerLock],
            );
            locked = rows[0]?.locked ?? false;
            if (!locked) {
              await sleep(pollMs);
              continue;
            }
          }
          if ((await pause.state()).paused) {
            await sleep(pollMs);
            continue;
          }
          const indexed = await pass(halted.signal);
          if (indexed > 0) {
            logger.info({ messages: indexed }, "messages added to the search index");
            continue;
          }
          await sleep(pollMs);
        } catch (err) {
          if (!running) return;
          if (err instanceof EmbeddingError && (err.code === "auth" || err.code === "rejected")) {
            logger.error(
              { error_code: err.code, status: err.status },
              "the embedding service refused; indexing waits",
            );
            await sleep(refusedMs);
            continue;
          }
          logger.warn(
            {
              err: isConnectionError(err) ? undefined : err,
              error_code: (err as { code?: string }).code,
            },
            "indexing failed; trying again",
          );
          await sleep(retryMs);
        }
      }
    } finally {
      if (guard) {
        if (locked)
          await guard.query("SELECT pg_advisory_unlock($1)", [indexerLock]).catch(() => {});
        guard.release();
      }
    }
  }

  return Object.freeze({
    start() {
      if (running) return;
      running = true;
      halted = new AbortController();
      loop = run();
    },
    async stop() {
      if (!running) return;
      running = false;
      halted.abort(new Error("the indexer is stopping"));
      wakeUp?.();
      await loop;
    },
    wake() {
      wakeUp?.();
    },
    async progress() {
      const { rows } = await pool.query<{ indexed: number; total: number }>(
        `SELECT (SELECT count(*)::int FROM message_chunks) AS indexed,
                (SELECT count(*)::int FROM messages m
                 WHERE m.kind = 'text' AND m.body IS NOT NULL AND btrim(m.body) <> ''
                   AND m.deleted_at IS NULL) AS total`,
      );
      return { indexed: rows[0]?.indexed ?? 0, total: rows[0]?.total ?? 0 };
    },
    runOnce: () => pass(new AbortController().signal),
  });
}
