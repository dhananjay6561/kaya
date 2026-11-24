import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { createLogger } from "../../lib/logger.js";
import { createStubLlm, LlmError } from "../../providers/llm/index.js";
import { chatReply, clipTo, systemPrompt, toWhatsApp } from "./chat.js";

const logger = createLogger({ level: "silent", service: "core", version: "test" });
const now = new Date("2026-11-24T19:00:00Z");
const signal = new AbortController().signal;
const q = (text: string) => ({ id: "3EB0Q", text, sentAt: now });

describe("system prompt", () => {
  it("gives the date and time where the owner is", () => {
    const prompt = systemPrompt(now, "Asia/Kolkata");
    assert.match(prompt, /Wednesday 25 November 2026, 00:30 \(Asia\/Kolkata\)/);
    assert.match(systemPrompt(now, "UTC"), /Tuesday 24 November 2026, 19:00 \(UTC\)/);
  });

  it("tells the truth about where the conversation goes", () => {
    assert.match(systemPrompt(now, "UTC", "cloud"), /sent to an AI service in the cloud/);
    assert.doesNotMatch(systemPrompt(now, "UTC", "cloud"), /run entirely on their own computer/);
    assert.match(systemPrompt(now, "UTC", "local"), /run entirely on their own computer/);
    assert.doesNotMatch(systemPrompt(now, "UTC", "local"), /in the cloud/);
  });

  it("says what Kaya cannot do yet, so it does not pretend", () => {
    const prompt = systemPrompt(now, "UTC");
    assert.match(prompt, /never offer them, promise them or suggest you will: reminders/);
    assert.match(prompt, /reading their other WhatsApp chats, searching the web/);
    assert.match(prompt, /There are no sessions and nothing resets/);
    assert.match(prompt, /cannot yet pin it as a lasting fact/);
    assert.match(prompt, /marked with how long ago/);
    assert.match(prompt, /cannot check anything live/);
    assert.match(prompt, /Never make up facts/);
    assert.match(prompt, /Read past typos and autocorrect/);
    assert.match(prompt, /answer in Hinglish in Latin letters, never in Devanagari/);
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
  it("asks the stronger model with the system prompt and the message", async () => {
    const llm = createStubLlm({ reply: () => "**Hi!** I'm Kaya." });
    const reply = await chatReply(
      q("who are you?"),
      { llm, logger, timeZone: "UTC", now: () => now },
      signal,
    );
    assert.deepEqual(reply, { text: "*Hi!* I'm Kaya.", remember: true });
    const request = llm.requests[0];
    assert.equal(request?.tier, "heavy");
    assert.equal(request?.effort, "medium");
    assert.equal(request?.maxTokens, 3_072);
    assert.equal(request?.messages.length, 2);
    assert.equal(request?.messages[0]?.role, "system");
    assert.deepEqual(request?.messages[1], { role: "user", content: "who are you?" });
  });

  it("carries the recent conversation, asked for the question being answered", async () => {
    const llm = createStubLlm({ reply: () => "Namaste" });
    const seen: string[] = [];
    await chatReply(
      q("and in Hindi?"),
      {
        llm,
        logger,
        timeZone: "UTC",
        now: () => now,
        history: async (question) => {
          seen.push(question.id);
          return [{ question: "how do I say hello?", answer: "Hello.", at: now }];
        },
      },
      signal,
    );
    assert.deepEqual(seen, ["3EB0Q"]);
    assert.deepEqual(
      llm.requests[0]?.messages.slice(1).map((m) => [m.role, m.content]),
      [
        ["user", "(sent just now) how do I say hello?"],
        ["assistant", "Hello."],
        ["user", "and in Hindi?"],
      ],
    );
  });

  it("fits the conversation to the model's budget", async () => {
    const llm = createStubLlm({ reply: () => "ok", contextTokens: 1_000 });
    const long = "x".repeat(2_000);
    await chatReply(
      q("next?"),
      {
        llm,
        logger,
        timeZone: "UTC",
        now: () => now,
        history: async () => [
          { question: long, answer: long, at: now },
          { question: "short", answer: "reply", at: now },
        ],
      },
      signal,
    );
    assert.deepEqual(
      llm.requests[0]?.messages.slice(1).map((m) => m.content),
      ["(sent just now) short", "reply", "next?"],
    );
  });

  it("says so when the model gives nothing back", async () => {
    const llm = createStubLlm({ reply: () => "   " });
    assert.deepEqual(await chatReply(q("x"), { llm, logger, timeZone: "UTC" }, signal), {
      text: "I had no answer to that. Try asking another way.",
      remember: false,
    });
  });

  it("clips a long answer and marks one the model could not finish", async () => {
    const long = createStubLlm({ reply: () => "y".repeat(5_000) });
    const reply = await chatReply(
      q("x"),
      { llm: long, logger, timeZone: "UTC", maxReplyLength: 10 },
      signal,
    );
    assert.equal(
      reply.text,
      `${"y".repeat(10)}…\n\n(Cut short here. Ask for less, or for the rest.)`,
    );

    const cut = {
      name: "cut",
      locality: "local" as const,
      contextTokens: 4_000,
      countTokens: (text: string) => text.length,
      complete: async () => ({
        text: "half an answer",
        model: "m",
        finishReason: "length" as const,
        usage: { inputTokens: 1, outputTokens: 1 },
      }),
    };
    assert.match(
      (await chatReply(q("x"), { llm: cut, logger, timeZone: "UTC" }, signal)).text,
      /^half an answer\n\n\(Cut short here\. Ask for less, or for the rest\.\)$/,
    );

    const empty = { ...cut, complete: async () => ({ ...(await cut.complete()), text: "" }) };
    assert.deepEqual(await chatReply(q("x"), { llm: empty, logger, timeZone: "UTC" }, signal), {
      text: "I ran out of room before I could answer. Try a narrower question.",
      remember: false,
    });
  });

  it("answers without the conversation when it cannot be read, but not when the db is away", async () => {
    const llm = createStubLlm({ reply: () => "fine" });
    const broken = async () => {
      throw new Error("permission denied for table commands");
    };
    const reply = await chatReply(
      q("hi"),
      { llm, logger, timeZone: "UTC", history: broken },
      signal,
    );
    assert.equal(reply.text, "fine");
    assert.equal(llm.requests[0]?.messages.length, 2);

    const away = async () => {
      throw Object.assign(new Error("connect ECONNREFUSED"), { code: "ECONNREFUSED" });
    };
    await assert.rejects(
      chatReply(q("hi"), { llm, logger, timeZone: "UTC", history: away }, signal),
      /ECONNREFUSED/,
    );
  });

  it("asks again with less effort when the model spent all its room thinking", async () => {
    const efforts: (string | undefined)[] = [];
    const llm = {
      ...createStubLlm(),
      complete: async (request: { effort?: string }) => {
        efforts.push(request.effort);
        return {
          text: request.effort === "low" ? "here you go" : "",
          model: "m",
          finishReason: "length" as const,
          usage: { inputTokens: 1, outputTokens: 1 },
        };
      },
    };
    const reply = await chatReply(q("hard one"), { llm, logger, timeZone: "UTC" }, signal);
    assert.deepEqual(efforts, ["medium", "low"]);
    assert.match(reply.text, /^here you go/);
  });

  it("asks again without the conversation when the model says the request is too long", async () => {
    const sizes: number[] = [];
    const llm = {
      ...createStubLlm(),
      complete: async (request: { messages: readonly unknown[] }) => {
        sizes.push(request.messages.length);
        if (request.messages.length > 2) throw new LlmError("groq", "too_long", { status: 413 });
        return {
          text: "short answer",
          model: "m",
          finishReason: "stop" as const,
          usage: { inputTokens: 1, outputTokens: 1 },
        };
      },
    };
    const reply = await chatReply(
      q("hi"),
      {
        llm,
        logger,
        timeZone: "UTC",
        history: async () => [{ question: "old", answer: "older", at: now }],
      },
      signal,
    );
    assert.equal(reply.text, "short answer");
    assert.deepEqual(sizes, [4, 2]);
  });
});
