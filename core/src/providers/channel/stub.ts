import { randomUUID } from "node:crypto";
import type {
  ChatChannel,
  IncomingMessage,
  MessageHandler,
  OutgoingMessage,
  SendOptions,
  SentMessage,
} from "./types.js";

export type StubChannel = ChatChannel &
  Readonly<{
    sent: readonly OutgoingMessage[];
    receive(text: string, options?: Readonly<{ sentAt?: Date }>): Promise<IncomingMessage>;
  }>;

export class ChannelNotStartedError extends Error {
  constructor(readonly channel: string) {
    super(`chat channel "${channel}" is not started`);
    this.name = "ChannelNotStartedError";
  }
}

export function createStubChannel(): StubChannel {
  const sent: OutgoingMessage[] = [];
  const byKey = new Map<string, SentMessage>();
  const handlers = new Set<MessageHandler>();
  let started = false;

  const assertStarted = () => {
    if (!started) throw new ChannelNotStartedError("stub");
  };

  return Object.freeze({
    name: "stub",
    sent,
    async start() {
      started = true;
    },
    async stop() {
      started = false;
    },
    async check(signal: AbortSignal) {
      signal.throwIfAborted();
      assertStarted();
    },
    async send(message: OutgoingMessage, options: SendOptions = {}) {
      options.signal?.throwIfAborted();
      assertStarted();
      if (message.idempotencyKey.trim() === "") throw new Error("idempotency key is empty");
      const earlier = byKey.get(message.idempotencyKey);
      if (earlier) return earlier;
      if (message.text.trim() === "") throw new Error("message text is empty");
      sent.push(message);
      const result = { id: randomUUID(), sentAt: new Date() };
      byKey.set(message.idempotencyKey, result);
      return result;
    },
    onMessage(handler: MessageHandler) {
      handlers.add(handler);
      return () => handlers.delete(handler);
    },
    async receive(text: string, options: Readonly<{ sentAt?: Date }> = {}) {
      assertStarted();
      const now = new Date();
      const message: IncomingMessage = {
        id: randomUUID(),
        text,
        sentAt: options.sentAt ?? now,
        receivedAt: now,
      };
      const context = { signal: new AbortController().signal };
      const results = await Promise.allSettled([...handlers].map(async (h) => h(message, context)));
      const failures = results.flatMap((r) => (r.status === "rejected" ? [r.reason] : []));
      if (failures.length > 0) {
        throw new AggregateError(failures, `${failures.length} message handler(s) failed`);
      }
      return message;
    },
  });
}
