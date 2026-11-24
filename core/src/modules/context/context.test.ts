import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { buildContext, type Exchange, estimateTokens } from "./context.js";

const at = new Date("2026-11-24T10:00:00Z");
const exchange = (n: number, size = 10): Exchange => ({
  question: `question ${n} ${"q".repeat(size)}`,
  answer: `answer ${n} ${"a".repeat(size)}`,
  at,
});

describe("token estimate", () => {
  it("counts Latin text at about four characters a token", () => {
    assert.equal(estimateTokens(""), 4);
    assert.equal(estimateTokens("a".repeat(400)), 104);
  });

  it("counts Hindi and emoji as a token each, so the budget is never underestimated", () => {
    assert.equal(estimateTokens("नमस्ते"), 6 + 4);
    assert.equal(estimateTokens("😀😀"), 2 + 4);
    assert.ok(estimateTokens("kya haal hai") < estimateTokens("क्या हाल है"));
  });
});

describe("context builder", () => {
  it("puts the system prompt first, past exchanges in order, then the question", () => {
    const built = buildContext({
      system: "be brief",
      history: [exchange(1), exchange(2)],
      question: "and in Hindi?",
      budget: 10_000,
    });
    assert.deepEqual(
      built.messages.map((m) => `${m.role}: ${m.content.split(" ").slice(0, 2).join(" ")}`),
      [
        "system: be brief",
        "user: question 1",
        "assistant: answer 1",
        "user: question 2",
        "assistant: answer 2",
        "user: and in",
      ],
    );
    assert.equal(built.exchanges, 2);
  });

  it("keeps the newest exchanges that fit and drops a question with its answer", () => {
    const history = [1, 2, 3, 4, 5].map((n) => exchange(n, 200));
    const one =
      estimateTokens(history[0]?.question ?? "") + estimateTokens(history[0]?.answer ?? "");
    const fixed = estimateTokens("s") + estimateTokens("now?");
    const built = buildContext({
      system: "s",
      history,
      question: "now?",
      budget: fixed + 2 * one + 1,
    });
    assert.equal(built.exchanges, 2);
    assert.deepEqual(
      built.messages.slice(1, -1).map((m) => m.content.split(" ").slice(0, 2).join(" ")),
      ["question 4", "answer 4", "question 5", "answer 5"],
    );
    assert.ok(built.tokens <= fixed + 2 * one + 1);
  });

  it("sends just the question when the budget has no room for history", () => {
    const built = buildContext({ system: "s", history: [exchange(1)], question: "q", budget: 1 });
    assert.equal(built.exchanges, 0);
    assert.equal(built.messages.length, 2);
  });

  it("does not skip an old exchange to fit after a big one is dropped", () => {
    const history = [exchange(1, 5), exchange(2, 5_000), exchange(3, 5)];
    const built = buildContext({ system: "s", history, question: "q", budget: 200 });
    assert.equal(built.exchanges, 1, "the conversation stays continuous: only the newest one");
  });
});
