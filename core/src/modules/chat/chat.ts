import type { Logger } from "../../lib/logger.js";
import type { LlmProvider } from "../../providers/llm/index.js";
import { buildContext, type Exchange } from "../context/index.js";

export type Question = Readonly<{ id: string; text: string; sentAt: Date }>;

export type ChatOptions = Readonly<{
  llm: LlmProvider;
  logger: Logger;
  timeZone: string;
  history?: (question: Question) => Promise<readonly Exchange[]>;
  now?: () => Date;
  maxReplyLength?: number;
}>;

export function systemPrompt(now: Date, timeZone: string): string {
  const parts = Object.fromEntries(
    new Intl.DateTimeFormat("en-GB", {
      timeZone,
      weekday: "long",
      day: "numeric",
      month: "long",
      year: "numeric",
      hour: "2-digit",
      minute: "2-digit",
      hourCycle: "h23",
    })
      .formatToParts(now)
      .map((part) => [part.type, part.value]),
  );
  const date = `${parts.weekday} ${parts.day} ${parts.month} ${parts.year}, ${parts.hour}:${parts.minute}`;
  return [
    "You are Kaya, a personal assistant. The person you work for writes to you in their own WhatsApp chat, so every message you get is from them.",
    "Answer briefly and directly, in the language they write in, even when the question is about another language. Use short paragraphs or a few lines starting with '- '. No tables, no headings, no Markdown links.",
    "For emphasis use WhatsApp formatting: *bold* and _italic_.",
    "Your knowledge stops at your training date and you cannot check anything live; say so when a question depends on recent events, prices or news.",
    "You see your recent conversation in this chat, but not older messages. You cannot yet read their other chats, search the web, open links or files, set reminders or do anything outside this reply. If they ask for one of those, say plainly that you can't do it yet. Never claim to have done something you did not do.",
    `It is now ${date} (${timeZone}).`,
  ].join("\n");
}

function outsideCode(text: string, change: (part: string) => string): string {
  return text
    .split(/(```[\s\S]*?```|`[^`\n]+`)/)
    .map((part, i) => (i % 2 === 1 ? part : change(part)))
    .join("");
}

const around = (marker: string) =>
  new RegExp(`(?<![\\w*_])${marker}(?=\\S)(.+?)(?<=\\S)${marker}(?![\\w*_])`, "g");

export function toWhatsApp(text: string): string {
  const changed = outsideCode(text, (part) =>
    part
      .replace(/^[ \t]*#{1,6}[ \t]+(.+?)(?:[ \t]+#+)?[ \t]*$/gm, (_, title: string) => {
        return `*${title.replace(/\*+|_{2,}/g, "")}*`;
      })
      .replace(around("\\*\\*\\*"), "*_$1_*")
      .replace(around("\\*\\*"), "*$1*")
      .replace(around("__"), "_$1_")
      .replace(/\[([^\]]+)\]\((https?:\/\/[^\s]+?)\)(?!\))/g, "$1 ($2)")
      .replace(/^([ \t]*)[*+][ \t]+/gm, "$1- "),
  );
  return changed
    .split("\n")
    .map((line) => line.trimEnd())
    .join("\n")
    .trim();
}

export function clipTo(text: string, length: number): string {
  const characters = Array.from(text.toWellFormed());
  if (characters.length <= length) return characters.join("");
  return `${characters.slice(0, length).join("")}…`;
}

export async function chatReply(
  question: Question,
  options: ChatOptions,
  signal: AbortSignal,
): Promise<string> {
  const now = options.now?.() ?? new Date();
  const history = (await options.history?.(question)) ?? [];
  const context = buildContext({
    system: systemPrompt(now, options.timeZone),
    history,
    question: question.text,
    budget: options.llm.contextTokens,
  });
  const started = performance.now();
  const completion = await options.llm.complete(
    { tier: "routine", messages: context.messages },
    { signal },
  );
  options.logger.info(
    {
      model: completion.model,
      finish_reason: completion.finishReason,
      input_tokens: completion.usage.inputTokens,
      output_tokens: completion.usage.outputTokens,
      estimated_tokens: context.tokens,
      exchanges: context.exchanges,
      ms: Math.round(performance.now() - started),
    },
    "the model answered",
  );
  const answer = toWhatsApp(completion.text);
  const ranOut = completion.finishReason === "length";
  if (!answer) {
    return ranOut
      ? "I ran out of room before I could answer. Try a narrower question."
      : "I had no answer to that. Try asking another way.";
  }
  const limit = options.maxReplyLength ?? 8_000;
  const cut = Array.from(answer).length > limit;
  const reply = clipTo(answer, limit);
  return ranOut || cut ? `${reply}\n\n(Cut short here. Ask for less, or for the rest.)` : reply;
}
