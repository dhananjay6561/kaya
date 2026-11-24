import { isConnectionError } from "../../lib/db.js";
import type { Logger } from "../../lib/logger.js";
import {
  type Completion,
  LlmError,
  type LlmProvider,
  type Locality,
  type ReasoningEffort,
} from "../../providers/llm/index.js";
import { buildContext, type Exchange } from "../context/index.js";
import { createMaskSession } from "../masking/index.js";

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

export function systemPrompt(now: Date, timeZone: string, locality: Locality = "cloud"): string {
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
    locality === "local"
      ? "- You run entirely on their own computer: your memory is their own database, and the model answering is on their machine too."
      : "- Your memory is stored on their own computer, in their own database. To answer, the conversation you see here is sent to an AI service in the cloud, so private details in it (phone numbers, emails, codes, account numbers, addresses) are hidden behind placeholders such as [PHONE_1]. When you refer to one, write the placeholder exactly as it is; they see the real value.",
    "- There are no sessions and nothing resets: every message they have sent you and every answer is stored for good, whatever time passes. You see only the most recent part of it here, and earlier messages are marked with how long ago they were sent; treat old ones as background, not as the current topic.",
    "- If they ask you to remember something: it is saved in your conversation history, but you cannot yet pin it as a lasting fact, so after many more messages you may no longer see it. Say that plainly.",
    "- You are being built step by step, and for now you can only talk. These you cannot do yet, so never offer them, promise them or suggest you will: reminders, alarms, notes, to-do lists, reading their other WhatsApp chats, searching the web, opening links or files, sending messages for them. If they ask for one, say plainly that you can't do it yet.",
    "- Your knowledge stops at your training date and you cannot check anything live, so say so when a question depends on recent news, prices or events.",
    "",
    "How to talk:",
    "- Like a sharp, friendly assistant on WhatsApp: short, natural sentences, no filler.",
    "- Match their language and style. If they write Hinglish (Hindi in Latin letters), answer in Hinglish in Latin letters, never in Devanagari unless they use it; if English, in English, even when the question is about another language.",
    "- Use a list only when the answer really is a list. No headings, no tables, no Markdown links. For emphasis use *bold* or _italic_.",
    "- Messages are typed fast on a phone. Read past typos and autocorrect, including misspelt product or brand names, answer what they most likely meant and say what you assumed. Ask one short question only if you really cannot tell.",
    "- Never make up facts, products, links, numbers or steps. If you are not sure, say so.",
    "- Never claim to have done something you did not do, and never describe features you were not told about here.",
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
  const privacy = llm.locality === "cloud" ? createMaskSession() : undefined;
  const ask = async (history: readonly Exchange[], effort: ReasoningEffort = "medium") => {
    const context = buildContext({
      system: systemPrompt(now, options.timeZone, llm.locality),
      history,
      question: question.text,
      budget: llm.contextTokens,
      count: (text) => llm.countTokens(text),
      now,
    });
    const started = performance.now();
    const completion = await llm.complete(
      { tier: "heavy", effort, maxTokens: 3_072, messages: context.messages },
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

  const history = (await recentHistory(question, options)).map((exchange) =>
    privacy
      ? { ...exchange, answer: privacy.mask(exchange.answer, { context: exchange.question }) }
      : exchange,
  );
  let completion: Completion;
  try {
    completion = await ask(history);
  } catch (err) {
    if (!(err instanceof LlmError) || err.code !== "too_long" || history.length === 0) throw err;
    options.logger.warn("the request was too long with the conversation; asking without it");
    completion = await ask([]);
  }
  if (!completion.text.trim() && completion.finishReason === "length") {
    options.logger.warn("the model spent its room thinking; asking again with less effort");
    completion = await ask(history, "low");
  }

  const masked = privacy?.counts() ?? {};
  if (Object.keys(masked).length > 0)
    options.logger.info({ masked }, "private details were masked");
  const answer = toWhatsApp(privacy ? privacy.restore(completion.text) : completion.text);
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
