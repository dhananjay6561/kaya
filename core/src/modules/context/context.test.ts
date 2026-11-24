import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { buildContext, type Exchange, howLongAgo, shorten } from "./context.js";

const at = new Date("2026-11-24T10:00:00Z");
const count = (text: string) => Array.from(text).length;
const exchange = (n: number, size = 10): Exchange => ({
  question: `question ${n} ${"q".repeat(size)}`,
  answer: `answer ${n} ${"a".repeat(size)}`,
  at,
});
const firstWords = (text: string) =>
  text
    .replace(/^\(sent [^)]*\) /, "")
    .split(" ")
    .slice(0, 2)
    .join(" ");

describe("shorten", () => {
  it("keeps the start and the end and cuts the middle", () => {
    const text = `start ${"x".repeat(500)} end`;
    const short = shorten(text, 60, count);
    assert.ok(count(short) <= 60);
    assert.ok(short.startsWith("start"));
    assert.ok(short.endsWith("end"));
    assert.match(short, / … /);
  });

  it("leaves text that fits alone, and gives up with an ellipsis", () => {
    assert.equal(shorten("short", 60, count), "short");
    assert.equal(shorten("x".repeat(100), 2, count), "…");
  });
});

describe("context builder", () => {
  it("puts the system prompt first, past exchanges in order, then the question", () => {
    const built = buildContext({
      system: "be brief",
      history: [exchange(1), exchange(2)],
      question: "and in Hindi?",
      budget: 10_000,
      count,
    });
    assert.deepEqual(
      built.messages.map((m) => `${m.role}: ${firstWords(m.content)}`),
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
    assert.equal(built.leftOut, 0);
    assert.equal(built.messages[0]?.content, "be brief", "no note when nothing was left out");
  });

  it("keeps the newest exchanges that fit, drops older ones whole, and says so", () => {
    const history = [1, 2, 3, 4, 5].map((n) => exchange(n, 200));
    const built = buildContext({ system: "s", history, question: "now?", budget: 1_100, count });
    assert.equal(built.exchanges, 2);
    assert.equal(built.leftOut, 3);
    assert.deepEqual(
      built.messages.slice(1, -1).map((m) => firstWords(m.content)),
      ["question 4", "answer 4", "question 5", "answer 5"],
    );
    assert.match(
      built.messages[0]?.content ?? "",
      /Older parts of this conversation were left out/,
    );
    assert.ok(built.tokens <= 1_100);
  });

  it("shortens the newest exchange rather than drop it", () => {
    const big: Exchange = {
      question: `paste ${"p".repeat(5_000)} end`,
      answer: "summary here",
      at,
    };
    const built = buildContext({
      system: "s",
      history: [big],
      question: "make it shorter",
      budget: 600,
      count,
    });
    assert.equal(built.exchanges, 1);
    const [, question, answer] = built.messages;
    assert.ok(question?.content.startsWith("(sent ") && question.content.includes("paste"));
    assert.ok(question?.content.endsWith("end"));
    assert.equal(answer?.content, "summary here");
    assert.ok(built.tokens <= 600);
  });

  it("sends just the question when there is no room at all", () => {
    const built = buildContext({
      system: "s",
      history: [exchange(1)],
      question: "q",
      budget: 10,
      count,
    });
    assert.equal(built.exchanges, 0);
    assert.equal(built.messages.length, 2);
  });

  it("does not skip past an exchange that does not fit", () => {
    const history = [exchange(1, 5), exchange(2, 5_000), exchange(3, 5)];
    const built = buildContext({ system: "s", history, question: "q", budget: 300, count });
    assert.equal(built.exchanges, 1, "the conversation stays continuous: only the newest one");
    assert.equal(firstWords(built.messages[1]?.content ?? ""), "question 3");
  });

  it("counts with the tokenizer it is given", () => {
    const calls: string[] = [];
    buildContext({
      system: "s",
      history: [exchange(1)],
      question: "q",
      budget: 10_000,
      count: (text) => {
        calls.push(text);
        return 1;
      },
    });
    assert.ok(calls.includes("q") && calls.some((c) => c.includes("question 1")));
  });
});

describe("how long ago", () => {
  it("reads like a person would say it", () => {
    const minute = 60_000;
    assert.equal(howLongAgo(-5), "just now");
    assert.equal(howLongAgo(30_000), "just now");
    assert.equal(howLongAgo(5 * minute), "5 min ago");
    assert.equal(howLongAgo(3 * 60 * minute), "3 h ago");
    assert.equal(howLongAgo(26 * 60 * minute), "1 day ago");
    assert.equal(howLongAgo(9 * 24 * 60 * minute), "9 days ago");
  });

  it("marks each earlier question with its age, but not the answers or the new question", () => {
    const now = new Date(at.getTime() + 3 * 24 * 60 * 60_000);
    const built = buildContext({
      system: "s",
      history: [exchange(1)],
      question: "and today?",
      budget: 10_000,
      count,
      now,
    });
    assert.match(built.messages[1]?.content ?? "", /^\(sent 3 days ago\) question 1/);
    assert.match(built.messages[2]?.content ?? "", /^answer 1/);
    assert.equal(built.messages[3]?.content, "and today?");
  });
});
