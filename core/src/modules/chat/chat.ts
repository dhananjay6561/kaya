import { isConnectionError } from "../../lib/db.js";
import type { Logger } from "../../lib/logger.js";
import { type Completion, LlmError, type LlmProvider } from "../../providers/llm/index.js";
import { buildContext, type Exchange } from "../context/index.js";

export type Question = Readonly<{ id: string; text: string; sentAt: Date }>;

/** `remember` is false for a canned reply that should not become part of the conversation. */
export type ChatAnswer = Readonly<{ text: string; remember: boolean }>;

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
    "You are Kaya, the personal assistant of the person who owns this WhatsApp account. They write to you in their own chat, so every message you get here is from them.",
    "",
    "About you, so you never have to guess:",
    "- You run on their own computer, not on a company's servers, and your memory is their own database.",
    "- You remember your conversations with them for good. You see the latest part of the conversation here; it is not cleared when a chat or session ends.",
    "- You are being built step by step. For now you can only talk. You cannot yet read their other WhatsApp chats, search the web, open links or files, set reminders or send messages for them. If they ask for one of those, say plainly that you can't do it yet.",
    "- Your knowledge stops at your training date and you cannot check anything live, so say so when a question depends on recent news, prices or events.",
    "",
    "How to talk:",
    "- Like a sharp, friendly assistant on WhatsApp: short, natural sentences, no filler.",
    "- Match their language and style. If they write Hinglish, answer in Hinglish; if English, in English, even when the question is about another language.",
    "- Use a list only when the answer really is a list. No headings, no tables, no Markdown links. For emphasis use *bold* or _italic_.",
    "- Messages are typed fast on a phone. Read past typos and autocorrect, including misspelt product or brand names, answer what they most likely meant and say what you assumed. Ask one short question only if you really cannot tell.",
    "- Never make up facts, products, links, numbers or steps. If you are not sure, say so.",
    "- Never claim to have done something you did not do, and never offer or promise what you cannot do yet: no reminders, no taking notes, no forgetting on request, no features you were not told about here.",
    "",
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

async function recentHistory(question: Question, options: ChatOptions) {
  if (!options.history) return [];
  try {
    return await options.history(question);
  } catch (err) {
    if (isConnectionError(err)) throw err;
    options.logger.warn({ err }, "could not read the conversation; answering without it");
    return [];
  }
}

export async function chatReply(
  question: Question,
  options: ChatOptions,
  signal: AbortSignal,
): Promise<ChatAnswer> {
  const now = options.now?.() ?? new Date();
  const { llm } = options;
  const ask = async (history: readonly Exchange[]) => {
    const context = buildContext({
      system: systemPrompt(now, options.timeZone),
      history,
      question: question.text,
      budget: llm.contextTokens,
      count: (text) => llm.countTokens(text),
    });
    const started = performance.now();
    const completion = await llm.complete(
      { tier: "heavy", effort: "medium", maxTokens: 2_048, messages: context.messages },
      { signal },
    );
    options.logger.info(
      {
        model: completion.model,
        finish_reason: completion.finishReason,
        input_tokens: completion.usage.inputTokens,
        output_tokens: completion.usage.outputTokens,
        counted_tokens: context.tokens,
        exchanges: context.exchanges,
        left_out: context.leftOut,
        ms: Math.round(performance.now() - started),
      },
      "the model answered",
    );
    return completion;
  };

  const history = await recentHistory(question, options);
  let completion: Completion;
  try {
    completion = await ask(history);
  } catch (err) {
    if (!(err instanceof LlmError) || err.code !== "too_long" || history.length === 0) throw err;
    options.logger.warn("the request was too long with the conversation; asking without it");
    completion = await ask([]);
  }

  const answer = toWhatsApp(completion.text);
  const ranOut = completion.finishReason === "length";
  if (!answer) {
    return {
      text: ranOut
        ? "I ran out of room before I could answer. Try a narrower question."
        : "I had no answer to that. Try asking another way.",
      remember: false,
    };
  }
  const limit = options.maxReplyLength ?? 8_000;
  const cut = Array.from(answer).length > limit;
  const reply = clipTo(answer, limit);
  return {
    text: ranOut || cut ? `${reply}\n\n(Cut short here. Ask for less, or for the rest.)` : reply,
    remember: true,
  };
}
