import type { ChatMessage } from "../../providers/llm/index.js";

export type Exchange = Readonly<{ question: string; answer: string; at: Date }>;

export type ContextPackage = Readonly<{
  messages: readonly ChatMessage[];
  tokens: number;
  exchanges: number;
}>;

const perMessage = 4;

/** A cautious count: about four Latin characters per token, and one for every other character. */
export function estimateTokens(text: string): number {
  let latin = 0;
  let other = 0;
  for (const character of text) {
    if ((character.codePointAt(0) ?? 0) < 0x250) latin++;
    else other++;
  }
  return Math.ceil(latin / 4) + other + perMessage;
}

export function buildContext(input: {
  system: string;
  history: readonly Exchange[];
  question: string;
  budget: number;
}): ContextPackage {
  let tokens = estimateTokens(input.system) + estimateTokens(input.question);
  const kept: ChatMessage[] = [];
  let exchanges = 0;
  for (const exchange of [...input.history].reverse()) {
    const cost = estimateTokens(exchange.question) + estimateTokens(exchange.answer);
    if (tokens + cost > input.budget) break;
    tokens += cost;
    exchanges++;
    kept.unshift(
      { role: "user", content: exchange.question },
      { role: "assistant", content: exchange.answer },
    );
  }
  const messages: ChatMessage[] = [
    { role: "system", content: input.system },
    ...kept,
    { role: "user", content: input.question },
  ];
  return Object.freeze({ messages, tokens, exchanges });
}
