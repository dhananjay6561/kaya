import { randomUUID } from "node:crypto";
import type { ChatChannel, IncomingMessage, MessageHandler, OutgoingMessage } from "./types.js";

export type StubChannel = ChatChannel &
  Readonly<{
    sent: readonly OutgoingMessage[];
    receive(text: string): Promise<IncomingMessage>;
  }>;

export class ChannelNotStartedError extends Error {
  constructor(readonly channel: string) {
    super(`chat channel "${channel}" is not started`);
    this.name = "ChannelNotStartedError";
  }
}

export function createStubChannel(): StubChannel {
  const sent: OutgoingMessage[] = [];
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
    async send(message: OutgoingMessage, options: Readonly<{ signal?: AbortSignal }> = {}) {
      options.signal?.throwIfAborted();
      assertStarted();
      if (message.text.trim() === "") throw new Error("message text is empty");
      sent.push(message);
      return { id: randomUUID(), sentAt: new Date() };
    },
    onMessage(handler: MessageHandler) {
      handlers.add(handler);
      return () => handlers.delete(handler);
    },
    async receive(text: string) {
      assertStarted();
      const message: IncomingMessage = { id: randomUUID(), text, receivedAt: new Date() };
      for (const handler of handlers) await handler(message);
      return message;
    },
  });
}
