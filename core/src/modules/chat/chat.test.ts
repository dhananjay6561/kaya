import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { createLogger } from "../../lib/logger.js";
import { createStubLlm } from "../../providers/llm/index.js";
import { chatReply, clipTo, systemPrompt, toWhatsApp } from "./chat.js";

const logger = createLogger({ level: "silent", service: "core", version: "test" });
const now = new Date("2026-11-24T19:00:00Z");
const signal = new AbortController().signal;

describe("system prompt", () => {
  it("gives the date and time where the owner is", () => {
    const prompt = systemPrompt(now, "Asia/Kolkata");
    assert.match(prompt, /Wednesday 25 November 2026, 00:30 \(Asia\/Kolkata\)/);
    assert.match(systemPrompt(now, "UTC"), /Tuesday 24 November 2026, 19:00 \(UTC\)/);
  });

  it("says what Kaya cannot do yet, so it does not pretend", () => {
    const prompt = systemPrompt(now, "UTC");
    assert.match(prompt, /cannot yet read their other chats, search the web/);
    assert.match(prompt, /do not see earlier messages/);
    assert.match(prompt, /cannot check anything live/);
    assert.match(prompt, /Never claim to have done something you did not do/);
  });
});

describe("WhatsApp formatting", () => {
  it("turns Markdown into what WhatsApp shows", () => {
    assert.equal(
      toWhatsApp("This is **important** and __this__ too"),
      "This is *important* and _this_ too",
    );
    assert.equal(toWhatsApp("## Plan\nstep"), "*Plan*\nstep");
    assert.equal(
      toWhatsApp("see [the docs](https://example.com/a)"),
      "see the docs (https://example.com/a)",
    );
    assert.equal(toWhatsApp("* one\n  + two"), "- one\n  - two");
  });

  it("leaves code blocks alone and trims", () => {
    const code = "```\n**not bold**\n# not a heading\n```";
    assert.equal(toWhatsApp(`  **a**\n${code}\n**b**  `), `*a*\n${code}\n*b*`);
  });

  it("leaves code and words with underscores or stars in them alone", () => {
    for (const text of [
      "`__init__` and `f(**a, **b)` stay",
      "my__var__name.py",
      "a ** b and c ** d",
      "lone ** here",
      "```\n**not bold**\n# not a heading\n```",
    ]) {
      assert.equal(toWhatsApp(text), text);
    }
  });

  it("flattens nested emphasis WhatsApp cannot show", () => {
    assert.equal(toWhatsApp("***bold italic*** here"), "*_bold italic_* here");
    assert.equal(toWhatsApp("## **Plan**"), "*Plan*");
    assert.equal(toWhatsApp("## Title ##"), "*Title*");
  });

  it("does not take a hash in a title or a bare hash for a heading", () => {
    assert.equal(toWhatsApp("### Learning C#"), "*Learning C#*");
    assert.equal(toWhatsApp("#\n\nnot heading"), "#\n\nnot heading");
  });

  it("keeps brackets that belong to a link", () => {
    assert.equal(
      toWhatsApp("[wiki](https://en.wikipedia.org/wiki/Foo_(bar))"),
      "wiki (https://en.wikipedia.org/wiki/Foo_(bar))",
    );
  });

  it("stays fast on long runs of spaces and markers", () => {
    const started = performance.now();
    toWhatsApp(`${" ".repeat(30_000)}x${" ".repeat(30_000)}`);
    toWhatsApp("**a".repeat(3_000));
    toWhatsApp("__a".repeat(3_000));
    assert.ok(performance.now() - started < 500);
  });

  it("drops the spaces Markdown uses for line breaks", () => {
    assert.equal(toWhatsApp("- one  \n- two\t\nend"), "- one\n- two\nend");
  });

  it("keeps WhatsApp's own formatting", () => {
    assert.equal(toWhatsApp("*bold* _italic_ ~gone~"), "*bold* _italic_ ~gone~");
  });

  it("clips at whole characters", () => {
    assert.equal(clipTo("abc", 3), "abc");
    assert.equal(clipTo("ab😀😀", 3), "ab😀…");
  });
});

describe("chat reply", () => {
  it("asks the routine model with the system prompt and the message", async () => {
    const llm = createStubLlm({ reply: () => "**Hi!** I'm Kaya." });
    const reply = await chatReply(
      "who are you?",
      { llm, logger, timeZone: "UTC", now: () => now },
      signal,
    );
    assert.equal(reply, "*Hi!* I'm Kaya.");
    const request = llm.requests[0];
    assert.equal(request?.tier, "routine");
    assert.equal(request?.messages.length, 2);
    assert.equal(request?.messages[0]?.role, "system");
    assert.deepEqual(request?.messages[1], { role: "user", content: "who are you?" });
  });

  it("says so when the model gives nothing back", async () => {
    const llm = createStubLlm({ reply: () => "   " });
    assert.equal(
      await chatReply("x", { llm, logger, timeZone: "UTC" }, signal),
      "I had no answer to that. Try asking another way.",
    );
  });

  it("clips a long answer and marks one the model could not finish", async () => {
    const long = createStubLlm({ reply: () => "y".repeat(5_000) });
    const reply = await chatReply(
      "x",
      { llm: long, logger, timeZone: "UTC", maxReplyLength: 10 },
      signal,
    );
    assert.equal(reply, `${"y".repeat(10)}…\n\n(Cut short here. Ask for less, or for the rest.)`);

    const cut = {
      name: "cut",
      complete: async () => ({
        text: "half an answer",
        model: "m",
        finishReason: "length" as const,
        usage: { inputTokens: 1, outputTokens: 1 },
      }),
    };
    assert.match(
      await chatReply("x", { llm: cut, logger, timeZone: "UTC" }, signal),
      /^half an answer\n\n\(Cut short here\. Ask for less, or for the rest\.\)$/,
    );

    const empty = { ...cut, complete: async () => ({ ...(await cut.complete()), text: "" }) };
    assert.equal(
      await chatReply("x", { llm: empty, logger, timeZone: "UTC" }, signal),
      "I ran out of room before I could answer. Try a narrower question.",
    );
  });
});
