import type { Logger } from "../../lib/logger.js";
import {
  type ChatChannel,
  IdempotencyKeyReusedError,
  type IncomingMessage,
} from "../../providers/channel/index.js";

export type CommandsOptions = Readonly<{
  channel: ChatChannel;
  logger: Logger;
  maxAgeMs?: number;
  silentAfterMs?: number;
  replyTtlMs?: number;
  now?: () => number;
}>;

const previewLength = 1_000;

function clip(text: string): string {
  return text.length > previewLength ? `${text.slice(0, previewLength)}…` : text;
}

export function replyTo(message: IncomingMessage, ageMs: number, maxAgeMs: number): string {
  if (ageMs > maxAgeMs) {
    return `I was away when you sent this, so I did not act on it. Send it again if you still want it:\n${clip(message.text)}`;
  }
  return `You said: ${clip(message.text)}`;
}

export function startCommands(options: CommandsOptions): () => void {
  const { channel, logger } = options;
  const maxAgeMs = options.maxAgeMs ?? 10 * 60_000;
  const silentAfterMs = options.silentAfterMs ?? 24 * 60 * 60_000;
  const replyTtlMs = options.replyTtlMs ?? 2 * 60_000;
  const now = options.now ?? Date.now;

  return channel.onMessage(async (message) => {
    const ageMs = now() - message.receivedAt.getTime();
    if (ageMs > silentAfterMs) {
      logger.warn({ command: message.id }, "a command more than a day old was skipped");
      return;
    }
    try {
      await channel.send(
        {
          idempotencyKey: `command:${message.id}`,
          text: replyTo(message, ageMs, maxAgeMs),
          replyTo: message.id,
        },
        { ttlMs: replyTtlMs },
      );
    } catch (err) {
      if (!(err instanceof IdempotencyKeyReusedError)) throw err;
      logger.info({ command: message.id }, "this command was already answered");
    }
  });
}
