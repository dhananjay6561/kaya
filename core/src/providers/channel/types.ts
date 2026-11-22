import type { Checker } from "../../modules/health/index.js";

export type IncomingMessage = Readonly<{ id: string; text: string; receivedAt: Date }>;

export type OutgoingMessage = Readonly<{
  idempotencyKey: string;
  text: string;
  replyTo?: string;
}>;

export type SendOptions = Readonly<{
  signal?: AbortSignal;
  /** How long the message stays worth sending. After that it is dropped, not sent late. */
  ttlMs?: number;
}>;

export type SentMessage = Readonly<{ id: string; sentAt: Date }>;

export type MessageHandler = (message: IncomingMessage) => Promise<void> | void;

export interface ChatChannel extends Checker {
  readonly name: string;
  start(): Promise<void>;
  stop(): Promise<void>;
  send(message: OutgoingMessage, options?: SendOptions): Promise<SentMessage>;
  onMessage(handler: MessageHandler): () => void;
}
