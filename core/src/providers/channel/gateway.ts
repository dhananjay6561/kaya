import { createHash } from "node:crypto";
import type pg from "pg";
import { type Contracts, newSendRequest } from "../../contracts/index.js";
import type { Logger } from "../../lib/logger.js";
import { type MessageListener, readConnection } from "../../modules/gateway/index.js";
import { createInbox, type Inbox } from "./inbox.js";
import { ChannelNotStartedError, IdempotencyKeyReusedError } from "./stub.js";
import {
  answerOf,
  type ChatChannel,
  type Handled,
  type IncomingMessage,
  type MessageHandler,
  type OutgoingMessage,
  type SendOptions,
  type SentMessage,
} from "./types.js";

export type IncomingEvents = Readonly<{
  onMessage(listener: MessageListener): () => void;
}>;

export type SendResults = Readonly<{
  onSendResult(requestId: string, listener: () => void): () => void;
}>;

export type GatewayChannelDeps = Readonly<{
  pool: pg.Pool;
  contracts: Contracts;
  results?: SendResults;
  incoming?: IncomingEvents;
  logger?: Logger;
  inboxPollMs?: number;
  isUrgent?: (text: string) => boolean;
  ttlMs?: number;
  pollMs?: number;
  graceMs?: number;
  sendingGraceMs?: number;
}>;

export class GatewayNotLinkedError extends Error {
  constructor() {
    super("the gateway has not reported a linked WhatsApp account yet");
    this.name = "GatewayNotLinkedError";
  }
}

export class SendFailedError extends Error {
  constructor(
    readonly requestId: string,
    readonly code: string,
  ) {
    super(`the message was not sent: ${code}`);
    this.name = "SendFailedError";
  }
}

export function requestIdFor(idempotencyKey: string): string {
  const hash = createHash("sha256").update(`kaya:channel:${idempotencyKey}`).digest();
  hash[6] = ((hash[6] ?? 0) & 0x0f) | 0x50;
  hash[8] = ((hash[8] ?? 0) & 0x3f) | 0x80;
  const hex = hash.subarray(0, 16).toString("hex");
  return [
    hex.slice(0, 8),
    hex.slice(8, 12),
    hex.slice(12, 16),
    hex.slice(16, 20),
    hex.slice(20),
  ].join("-");
}

type Row = {
  state: string;
  wa_id: string | null;
  error: string | null;
  finished_at: Date | null;
  text: string | null;
  reply_to: string | null;
  overdue: boolean;
  abandoned: boolean;
};

export function createGatewayChannel(deps: GatewayChannelDeps): ChatChannel {
  const { pool, contracts } = deps;
  const pollMs = deps.pollMs ?? 2_000;
  const graceMs = deps.graceMs ?? 30_000;
  const sendingGraceMs = deps.sendingGraceMs ?? 5 * 60_000;
  const handlers = new Set<MessageHandler>();
  let started = false;
  let stopping = new AbortController();
  let unsubscribe: (() => void) | undefined;
  const inbox: Inbox | undefined =
    deps.incoming && deps.logger
      ? createInbox({
          pool,
          logger: deps.logger,
          ...(deps.inboxPollMs === undefined ? {} : { pollMs: deps.inboxPollMs }),
          ...(deps.isUrgent ? { isUrgent: deps.isUrgent } : {}),
        })
      : undefined;

  async function deliver(
    message: IncomingMessage,
    signal: AbortSignal,
  ): Promise<Handled | undefined> {
    const results = await Promise.allSettled(
      [...handlers].map(async (handle) => handle(message, { signal })),
    );
    const failures = results.flatMap((r) => (r.status === "rejected" ? [r.reason] : []));
    if (failures.length === 1) throw failures[0];
    if (failures.length > 1) {
      throw new AggregateError(failures, `${failures.length} message handler(s) failed`);
    }
    for (const result of results) {
      const answerId = result.status === "fulfilled" ? answerOf(result.value) : undefined;
      if (answerId) return { answerId };
    }
    return undefined;
  }

  const assertStarted = () => {
    if (!started) throw new ChannelNotStartedError("gateway");
  };

  async function outcome(
    id: string,
    message: OutgoingMessage | undefined,
    textMayChange: boolean,
  ): Promise<SentMessage | undefined | "none"> {
    const { rows } = await pool.query<Row>(
      `SELECT state, wa_id, error, finished_at,
              payload -> 'content' ->> 'text' AS text,
              payload -> 'reply_to' ->> 'wa_id' AS reply_to,
              now() > expires_at + make_interval(secs => $2) AS overdue,
              now() > expires_at + make_interval(secs => $3) AS abandoned
       FROM send_requests WHERE id = $1`,
      [id, graceMs / 1_000, (graceMs + sendingGraceMs) / 1_000],
    );
    const row = rows[0];
    if (!row) return "none";
    if (message) {
      const textDiffers = row.text !== message.text.trim() && !textMayChange;
      if (textDiffers || row.reply_to !== (message.replyTo ?? null)) {
        throw new IdempotencyKeyReusedError(id);
      }
    }
    if (row.state === "sent" && row.wa_id && row.finished_at) {
      return { id: row.wa_id, sentAt: row.finished_at };
    }
    if (row.state === "failed") throw new SendFailedError(id, row.error ?? "failed");
    if (row.state === "pending" && row.overdue) throw new SendFailedError(id, "no_answer");
    if (row.state === "sending" && row.abandoned) throw new SendFailedError(id, "outcome_unknown");
    return undefined;
  }

  function nudge(id: string, ms: number, signal: AbortSignal | undefined): Promise<void> {
    return new Promise((resolve, reject) => {
      if (signal?.aborted) {
        reject(signal.reason);
        return;
      }
      const timer = setTimeout(done, ms);
      const forget = deps.results?.onSendResult(id, done);
      const aborted = () => {
        cleanup();
        reject(signal?.reason);
      };
      function cleanup() {
        clearTimeout(timer);
        forget?.();
        signal?.removeEventListener("abort", aborted);
      }
      function done() {
        cleanup();
        resolve();
      }
      signal?.addEventListener("abort", aborted, { once: true });
    });
  }

  const waitSignal = (options: SendOptions) =>
    options.signal ? AbortSignal.any([options.signal, stopping.signal]) : stopping.signal;

  async function waitFor(
    id: string,
    message: OutgoingMessage | undefined,
    textMayChange: boolean,
    signal: AbortSignal,
  ): Promise<SentMessage> {
    for (;;) {
      signal.throwIfAborted();
      const sent = await outcome(id, message, textMayChange);
      if (sent === "none") throw new Error("a queued send request disappeared");
      if (sent) return sent;
      await nudge(id, pollMs, signal);
    }
  }

  return Object.freeze({
    name: "gateway",
    async start() {
      if (started) return;
      started = true;
      stopping = new AbortController();
      if (inbox && deps.incoming) {
        unsubscribe = deps.incoming.onMessage(async (message, tx) =>
          (await inbox.record(message, tx)) ? inbox.wake : undefined,
        );
        inbox.start(deliver, () => handlers.size > 0);
      }
    },
    async stop() {
      if (!started) return;
      started = false;
      unsubscribe?.();
      unsubscribe = undefined;
      const inboxStopped = inbox?.stop();
      stopping.abort(new ChannelNotStartedError("gateway"));
      await inboxStopped;
    },
    async check(signal: AbortSignal) {
      signal.throwIfAborted();
      assertStarted();
    },
    async send(message: OutgoingMessage, options: SendOptions = {}) {
      options.signal?.throwIfAborted();
      assertStarted();
      const signal = waitSignal(options);
      const ttlMs = options.ttlMs ?? deps.ttlMs;
      if (message.idempotencyKey.trim() === "") throw new Error("idempotency key is empty");
      if (message.text.trim() === "") throw new Error("message text is empty");

      const id = requestIdFor(message.idempotencyKey);
      const textMayChange = options.textMayChange ?? false;
      const earlier = await outcome(id, message, textMayChange);
      if (earlier !== "none") return earlier ?? (await waitFor(id, message, textMayChange, signal));

      const own = (await readConnection(pool))?.ownJid;
      if (!own) throw new GatewayNotLinkedError();
      const request = {
        ...newSendRequest(
          own,
          message.text,
          { kind: "self_chat" },
          {
            ...(message.replyTo ? { replyTo: { wa_id: message.replyTo, sender_jid: own } } : {}),
            ...(ttlMs === undefined ? {} : { ttlMs }),
          },
        ),
        id,
      };
      await pool.query(
        "INSERT INTO send_requests (id, payload, expires_at) VALUES ($1, $2, $3) ON CONFLICT (id) DO NOTHING",
        [id, contracts.encodeSendRequest(request), request.expires_at],
      );
      return waitFor(id, message, textMayChange, signal);
    },
    async follow(idempotencyKey: string, options: SendOptions = {}) {
      options.signal?.throwIfAborted();
      assertStarted();
      const id = requestIdFor(idempotencyKey);
      const earlier = await outcome(id, undefined, true);
      if (earlier === "none") return undefined;
      return earlier ?? (await waitFor(id, undefined, true, waitSignal(options)));
    },
    onMessage(handler: MessageHandler) {
      handlers.add(handler);
      inbox?.wake();
      return () => handlers.delete(handler);
    },
  });
}
