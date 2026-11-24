import type { Logger } from "../../lib/logger.js";
import {
  type ChatChannel,
  type IncomingMessage,
  SendFailedError,
} from "../../providers/channel/index.js";
import type { PauseSwitch } from "../pause/index.js";
import { answerSlash, parseSlash } from "./builtins.js";

export type CommandsOptions = Readonly<{
  channel: ChatChannel;
  logger: Logger;
  pause: PauseSwitch;
  status(): Promise<string>;
  maxAgeMs?: number;
  silentAfterMs?: number;
  minReplyTtlMs?: number;
  noticeTtlMs?: number;
}>;

const previewLength = 1_000;

// Whole characters only: cutting an emoji in half leaves text that cannot be stored or sent.
export function clip(text: string): string {
  const characters = Array.from(text.toWellFormed());
  if (characters.length <= previewLength) return characters.join("");
  return `${characters.slice(0, previewLength).join("")}…`;
}

const neverSent = new Set(["expired", "no_answer"]);

export function startCommands(options: CommandsOptions): () => void {
  const { channel, logger, pause } = options;
  const maxAgeMs = options.maxAgeMs ?? 10 * 60_000;
  const silentAfterMs = options.silentAfterMs ?? 24 * 60 * 60_000;
  const minReplyTtlMs = options.minReplyTtlMs ?? 30_000;
  const noticeTtlMs = options.noticeTtlMs ?? 60 * 60_000;

  return channel.onMessage(async (message: IncomingMessage, { signal }) => {
    // How late Kaya took the command in, not how late it is being run: the same command gets the
    // same answer however many times it is started.
    const lateMs = message.receivedAt.getTime() - message.sentAt.getTime();
    if (lateMs > silentAfterMs) {
      logger.warn({ command: message.id }, "a command more than a day old was skipped");
      return;
    }
    const slash = parseSlash(message.text);
    if (!slash && (await pause.state()).paused) {
      logger.info({ command: message.id }, "Kaya is paused; a message was left unanswered");
      return;
    }
    const reply = { idempotencyKey: `command:${message.id}`, replyTo: message.id };
    if (lateMs > maxAgeMs) {
      await channel.send(
        {
          ...reply,
          text: `I was away when you sent this, so I did not act on it. Send it again if you still want it:\n${clip(message.text)}`,
        },
        { signal, ttlMs: noticeTtlMs },
      );
      return;
    }

    // The answer is worth sending until the command itself would count as too old.
    const ttlMs = Math.max(minReplyTtlMs, maxAgeMs - (Date.now() - message.sentAt.getTime()));
    const text = slash
      ? await answerSlash(slash, message.id, { pause, status: options.status })
      : `You said: ${clip(message.text)}`;
    try {
      // A slash answer can come out differently when a command is retried (a status changes by
      // the minute); the first one queued is the answer.
      await channel.send({ ...reply, text }, { signal, ttlMs, textMayChange: slash !== undefined });
    } catch (err) {
      if (!(err instanceof SendFailedError) || !neverSent.has(err.code)) throw err;
      logger.warn(
        { command: message.id, error_code: err.code },
        "a reply could not be sent in time",
      );
      await channel.send(
        {
          idempotencyKey: `command:${message.id}:unanswered`,
          replyTo: message.id,
          text: slash
            ? "I could not answer this in time. Send /status to see how things stand."
            : `I could not answer this in time, so nothing was done. Send it again if you still want it:\n${clip(message.text)}`,
        },
        { signal, ttlMs: noticeTtlMs },
      );
      throw err;
    }
  });
}
