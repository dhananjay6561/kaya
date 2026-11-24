import { isConnectionError } from "../../lib/db.js";
import type { Logger } from "../../lib/logger.js";
import {
  type ChatChannel,
  type IncomingMessage,
  SendFailedError,
} from "../../providers/channel/index.js";
import { LlmError } from "../../providers/llm/index.js";
import { clipTo } from "../chat/index.js";
import { chatAnswerKey } from "../context/index.js";
import type { PauseSwitch } from "../pause/index.js";
import { answerSlash, parseSlash } from "./builtins.js";

export type CommandsOptions = Readonly<{
  channel: ChatChannel;
  logger: Logger;
  pause: PauseSwitch;
  status(): Promise<string>;
  chat(message: IncomingMessage, signal: AbortSignal): Promise<string>;
  maxAgeMs?: number;
  silentAfterMs?: number;
  minReplyTtlMs?: number;
  noticeTtlMs?: number;
}>;

export const clip = (text: string) => clipTo(text, 1_000);

function failureNotice(err: unknown): string {
  if (!(err instanceof LlmError)) {
    return "Something went wrong with that. Send /status to see how things stand.";
  }
  switch (err.code) {
    case "rate_limited":
      return "I have hit my limit with the AI service for now. Try again in a minute.";
    case "auth":
      return "The AI service refused my key, so I could not answer. Check GROQ_API_KEY.";
    case "too_long":
      return "That is too long for the AI service. Try a shorter message.";
    case "misconfigured":
      return "The AI service does not know the model I am set to use. Check GROQ_MODEL_ROUTINE.";
    case "rejected":
      return "The AI service turned this message down. Try asking another way.";
    default:
      return "The AI service is not answering right now. Try again in a bit.";
  }
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

    const answer = {
      idempotencyKey: slash
        ? `command:${message.id}:${slash.kind}:${slash.name}`
        : chatAnswerKey(message.id),
      replyTo: message.id,
    };
    const produce = async (): Promise<string> => {
      try {
        return slash
          ? await answerSlash(slash, message.id, { pause, status: options.status })
          : await options.chat(message, signal);
      } catch (err) {
        if (signal.aborted || isConnectionError(err)) throw err;
        await channel.send(
          {
            idempotencyKey: `command:${message.id}:failed`,
            replyTo: message.id,
            text: failureNotice(err),
          },
          { signal, ttlMs: noticeTtlMs },
        );
        throw err;
      }
    };
    try {
      if (await channel.follow(answer.idempotencyKey, { signal })) return;
      if (Date.now() - message.sentAt.getTime() > maxAgeMs) {
        throw new SendFailedError(answer.idempotencyKey, "expired");
      }
      const text = await produce();
      // The answer is worth sending until the command itself would count as too old.
      const ttlMs = Math.max(minReplyTtlMs, maxAgeMs - (Date.now() - message.sentAt.getTime()));
      await channel.send({ ...answer, text }, { signal, ttlMs, textMayChange: true });
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
