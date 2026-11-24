import type { Checker } from "../../modules/health/index.js";

export type IncomingMessage = Readonly<{
  id: string;
  text: string;
  /** When DJ sent it. */
  sentAt: Date;
  /** When Kaya first took it in. The gap to sentAt is how long Kaya was away. */
  receivedAt: Date;
}>;

export type OutgoingMessage = Readonly<{
  idempotencyKey: string;
  text: string;
  replyTo?: string;
}>;

export type SendOptions = Readonly<{
  signal?: AbortSignal;
  /** How long the message stays worth sending. After that it is dropped, not sent late. */
  ttlMs?: number;
  /** A repeat under the same idempotency key may carry other text; the text queued first is kept. */
  textMayChange?: boolean;
}>;

export type SentMessage = Readonly<{ id: string; sentAt: Date }>;

export type MessageHandler = (
  message: IncomingMessage,
  context: Readonly<{ signal: AbortSignal }>,
) => Promise<void> | void;

export interface ChatChannel extends Checker {
  readonly name: string;
  start(): Promise<void>;
  stop(): Promise<void>;
  send(message: OutgoingMessage, options?: SendOptions): Promise<SentMessage>;
  /** Waits for a message already queued under this key; undefined if none was queued. */
  follow(idempotencyKey: string, options?: SendOptions): Promise<SentMessage | undefined>;
  onMessage(handler: MessageHandler): () => void;
}
