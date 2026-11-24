import type { ChatMessage } from "../../providers/llm/index.js";

export type Exchange = Readonly<{ question: string; answer: string; at: Date }>;

export type ContextPackage = Readonly<{
  messages: readonly ChatMessage[];
  tokens: number;
  exchanges: number;
  leftOut: number;
}>;

export type CountTokens = (text: string) => number;

const requestOverhead = 48;
const leftOutNote = "Older parts of this conversation were left out to save space.";

export function howLongAgo(ms: number): string {
  const minutes = Math.floor(Math.max(0, ms) / 60_000);
  if (minutes < 1) return "just now";
  if (minutes < 60) return `${minutes} min ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours} h ago`;
  const days = Math.floor(hours / 24);
  return days === 1 ? "1 day ago" : `${days} days ago`;
}

/** Shortens text from the middle, keeping its start and its end, until it fits. */
export function shorten(text: string, maxTokens: number, count: CountTokens): string {
  if (count(text) <= maxTokens) return text;
  const characters = Array.from(text);
  let keep = characters.length;
  while (keep > 16) {
    keep = Math.floor(keep * 0.7);
    const half = Math.floor(keep / 2);
    const candidate = `${characters.slice(0, half).join("")} … ${characters.slice(-half).join("")}`;
    if (count(candidate) <= maxTokens) return candidate;
  }
  return "…";
}

export function buildContext(input: {
  system: string;
  history: readonly Exchange[];
  question: string;
  budget: number;
  count: CountTokens;
  now?: Date;
}): ContextPackage {
  const { count, budget, history } = input;
  const now = input.now ?? new Date();
  const marked = (exchange: Exchange, question: string) =>
    `(sent ${howLongAgo(now.getTime() - exchange.at.getTime())}) ${question}`;
  const system = history.length > 0 ? `${input.system}\n${leftOutNote}` : input.system;
  let tokens = requestOverhead + count(system) + count(input.question);
  const kept: ChatMessage[] = [];
  for (const [i, exchange] of [...history].reverse().entries()) {
    const fullQuestion = marked(exchange, exchange.question);
    const cost = count(fullQuestion) + count(exchange.answer);
    if (tokens + cost <= budget) {
      tokens += cost;
      kept.unshift(
        { role: "user", content: fullQuestion },
        { role: "assistant", content: exchange.answer },
      );
      continue;
    }
    const room = budget - tokens;
    if (i === 0 && room >= 64) {
      const marker = count(marked(exchange, ""));
      const question = marked(
        exchange,
        shorten(exchange.question, Math.floor(room / 2) - marker, count),
      );
      const answer = shorten(exchange.answer, room - count(question), count);
      tokens += count(question) + count(answer);
      kept.unshift({ role: "user", content: question }, { role: "assistant", content: answer });
    }
    break;
  }
  const exchanges = kept.length / 2;
  const leftOut = history.length - exchanges;
  const messages: ChatMessage[] = [
    { role: "system", content: leftOut > 0 ? system : input.system },
    ...kept,
    { role: "user", content: input.question },
  ];
  return Object.freeze({ messages, tokens, exchanges, leftOut });
}
