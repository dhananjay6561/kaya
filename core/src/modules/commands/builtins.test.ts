import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { createLogger } from "../../lib/logger.js";
import {
  type ChatChannel,
  createStubChannel,
  SendFailedError,
} from "../../providers/channel/index.js";
import { LlmError } from "../../providers/llm/index.js";
import { createMemoryPause } from "../pause/index.js";
import { isBuiltinCommand, parseSlash } from "./builtins.js";
import { startCommands } from "./commands.js";

const logger = createLogger({ level: "silent", service: "core", version: "test" });
const minute = 60_000;
const ago = (ms: number) => new Date(Date.now() - ms);

describe("reading a slash command", () => {
  it("knows the four built-ins whatever the case and spacing", () => {
    assert.deepEqual(parseSlash("/help"), { kind: "builtin", name: "help" });
    assert.deepEqual(parseSlash("  /Pause \n"), { kind: "builtin", name: "pause" });
    assert.deepEqual(parseSlash("/STATUS"), { kind: "builtin", name: "status" });
    assert.deepEqual(parseSlash("/resume"), { kind: "builtin", name: "resume" });
  });

  it("refuses a built-in with words after it rather than guessing what they mean", () => {
    assert.deepEqual(parseSlash("/pause 2h"), { kind: "extra_text", name: "pause" });
    assert.deepEqual(parseSlash("/resume please"), { kind: "extra_text", name: "resume" });
  });

  it("treats any other slash as an unknown command and plain text as no command", () => {
    assert.deepEqual(parseSlash("/tasks"), { kind: "unknown", name: "tasks" });
    assert.deepEqual(parseSlash("/"), { kind: "unknown", name: "" });
    assert.deepEqual(parseSlash("/usr/bin is here"), { kind: "unknown", name: "usr/bin" });
    assert.equal(parseSlash("pause"), undefined);
    assert.equal(parseSlash("please /pause"), undefined);
    assert.equal(Array.from(parseSlash(`/${"😀".repeat(100)}`)?.name ?? "").length, 30);
    const family = "👨‍👩‍👧";
    assert.equal(parseSlash(`/${family.repeat(40)}`)?.name, family.repeat(30), "whole emoji only");
  });

  it("sees through what phones and keyboards add", () => {
    assert.deepEqual(parseSlash("\u200e/resume"), { kind: "builtin", name: "resume" });
    assert.deepEqual(parseSlash("/pause\u200b"), { kind: "builtin", name: "pause" });
    assert.deepEqual(parseSlash("/pause."), { kind: "builtin", name: "pause" });
    assert.deepEqual(parseSlash("/Status!"), { kind: "builtin", name: "status" });
    assert.deepEqual(parseSlash("／pause"), { kind: "builtin", name: "pause" });
    assert.deepEqual(parseSlash("/pause\u00a0now"), { kind: "extra_text", name: "pause" });
  });

  it("lets only the four built-ins past the command cap", () => {
    assert.equal(isBuiltinCommand("/pause"), true);
    assert.equal(isBuiltinCommand(" /Resume."), true);
    assert.equal(isBuiltinCommand("/pause 2h"), false);
    assert.equal(isBuiltinCommand("/nope"), false);
    assert.equal(isBuiltinCommand("hello"), false);
  });
});

function kaya(
  options: {
    status?: () => Promise<string>;
    chat?: (text: string, signal: AbortSignal) => Promise<string>;
    fail?: (text: string) => Error | undefined;
  } = {},
) {
  const channel = createStubChannel();
  const pause = createMemoryPause();
  const wrapped: ChatChannel = {
    ...channel,
    send: async (message, sendOptions) => {
      const failure = options.fail?.(message.text);
      if (failure) throw failure;
      return channel.send(message, sendOptions);
    },
  };
  const stop = startCommands({
    channel: wrapped,
    logger,
    pause,
    status: options.status ?? (async () => "status text"),
    chat: options.chat ?? (async (text) => `You said: ${text}`),
  });
  const say = async (text: string, sentAt?: Date, id?: string) => {
    const before = channel.sent.length;
    await channel.receive(text, { ...(sentAt ? { sentAt } : {}), ...(id ? { id } : {}) });
    return channel.sent.slice(before).map((m) => m.text);
  };
  return { channel, pause, stop, say };
}

describe("built-in commands", () => {
  it("lists the commands on /help", async () => {
    const { channel, say } = kaya();
    await channel.start();
    const [help] = await say("/help");
    assert.match(help ?? "", /^Commands:/);
    for (const name of ["/help", "/status", "/pause", "/resume"]) {
      assert.ok(help?.includes(name), `${name} is listed`);
    }
    assert.doesNotMatch(help ?? "", /paused/);
  });

  it("answers /status with the status", async () => {
    const { channel, say } = kaya();
    await channel.start();
    assert.deepEqual(await say("/status"), ["status text"]);
  });

  it("stops every other answer between /pause and /resume", async () => {
    const { channel, pause, say } = kaya();
    await channel.start();
    assert.deepEqual(await say("before"), ["You said: before"]);

    const [paused] = await say("/pause");
    assert.match(paused ?? "", /^Paused\./);
    assert.equal((await pause.state()).paused, true);

    assert.deepEqual(await say("hello?"), []);
    assert.deepEqual(await say("anyone", ago(30 * minute)), [], "not even an 'I was away'");
    assert.match((await say("/pause"))[0] ?? "", /^Already paused\./);
    assert.match(
      (await say("/help"))[0] ?? "",
      /I am paused: only slash commands get an answer\.$/,
    );
    assert.deepEqual(await say("/status"), ["status text"]);
    assert.match((await say("/nope"))[0] ?? "", /^\/nope is not a command I know\./);

    assert.match((await say("/resume"))[0] ?? "", /^Resumed\./);
    assert.equal((await pause.state()).paused, false);
    assert.deepEqual(await say("after"), ["You said: after"]);
    assert.match((await say("/resume"))[0] ?? "", /^I was not paused\./);
  });

  it("does nothing for a built-in with words after it", async () => {
    const { channel, pause, say } = kaya();
    await channel.start();
    assert.match((await say("/pause for 2h"))[0] ?? "", /^\/pause takes nothing after it/);
    assert.equal((await pause.state()).paused, false);
  });

  it("does not act on a slash command taken in late, and says so", async () => {
    const { channel, pause, say } = kaya();
    await channel.start();
    const [late] = await say("/pause", ago(11 * minute));
    assert.match(late ?? "", /^I was away when you sent this, so I did not act on it\./);
    assert.equal((await pause.state()).paused, false);
    assert.deepEqual(await say("/resume", ago(25 * 60 * minute)), []);
  });

  it("answers a retried /pause the same way, once", async () => {
    const { channel, pause, say } = kaya();
    await channel.start();
    assert.match((await say("/pause", undefined, "3EB0PAUSE"))[0] ?? "", /^Paused\./);
    assert.deepEqual(await say("/pause", undefined, "3EB0PAUSE"), [], "already answered");
    assert.equal(channel.sent.length, 1);
    assert.equal((await pause.state()).paused, true);
  });

  it("keeps the first answer when a retried /status would read differently", async () => {
    let calls = 0;
    const { channel, say } = kaya({ status: async () => `up ${++calls}m` });
    await channel.start();
    assert.deepEqual(await say("/status", undefined, "3EB0STATUS"), ["up 1m"]);
    assert.deepEqual(await say("/status", undefined, "3EB0STATUS"), []);
    assert.equal(calls, 2);
  });

  it("answers anew when a command was edited into another before its retry", async () => {
    const { channel, pause, say } = kaya();
    await channel.start();
    assert.deepEqual(await say("/status", undefined, "3EB0EDITED"), ["status text"]);
    assert.match((await say("/pause", undefined, "3EB0EDITED"))[0] ?? "", /^Paused\./);
    assert.equal((await pause.state()).paused, true);
  });

  it("keeps the first answer when a retried message is answered differently", async () => {
    let calls = 0;
    const { channel, say } = kaya({ chat: async () => `answer ${++calls}` });
    await channel.start();
    assert.deepEqual(await say("what is 2+2", undefined, "3EB0CHAT"), ["answer 1"]);
    assert.deepEqual(await say("what is 2+2", undefined, "3EB0CHAT"), []);
  });

  it("points to /status when a slash answer could not go out in time", async () => {
    const { channel, say } = kaya({
      fail: (text) =>
        text.startsWith("Paused") ? new SendFailedError("request-id", "expired") : undefined,
    });
    await channel.start();
    await assert.rejects(say("/pause"));
    assert.deepEqual(
      channel.sent.map((m) => m.text),
      ["I could not answer this in time. Send /status to see how things stand."],
    );
  });

  it("tells DJ when working out an answer fails, and fails the command", async () => {
    const { channel, say } = kaya({
      status: async () => {
        throw new Error("permission denied for table gateway_events");
      },
    });
    await channel.start();
    await assert.rejects(say("/status"));
    assert.deepEqual(
      channel.sent.map((m) => m.text),
      ["Something went wrong with that. Send /status to see how things stand."],
    );
  });

  it("says nothing when the database is away, so the command runs again later", async () => {
    const { channel, say } = kaya({
      status: async () => {
        throw Object.assign(new Error("connect ECONNREFUSED"), { code: "ECONNREFUSED" });
      },
    });
    await channel.start();
    await assert.rejects(say("/status"));
    assert.deepEqual(channel.sent, []);
  });
});

describe("plain messages", () => {
  it("are answered by the model, with the message and a signal", async () => {
    const asked: { text: string; signal: AbortSignal }[] = [];
    const { channel, say } = kaya({
      chat: async (text, signal) => {
        asked.push({ text, signal });
        return "Four.";
      },
    });
    await channel.start();
    assert.deepEqual(await say("what is 2+2"), ["Four."]);
    assert.equal(asked[0]?.text, "what is 2+2");
    assert.ok(asked[0]?.signal instanceof AbortSignal);
  });

  it("never reach the model while paused or when taken in late", async () => {
    let asked = 0;
    const { channel, say } = kaya({
      chat: async () => {
        asked++;
        return "answer";
      },
    });
    await channel.start();
    await say("/pause");
    assert.deepEqual(await say("hello"), []);
    await say("/resume");
    assert.match((await say("late one", ago(11 * minute)))[0] ?? "", /^I was away/);
    assert.equal(asked, 0);
  });

  it("get a plain notice when the model fails, and the command fails with its code", async () => {
    const cases = [
      ["rate_limited", /^I have hit my limit with the AI service/],
      ["auth", /refused my key/],
      ["rejected", /turned this message down/],
      ["timeout", /is not answering right now/],
      ["unavailable", /is not answering right now/],
      ["bad_response", /is not answering right now/],
    ] as const;
    for (const [code, notice] of cases) {
      const { channel, say } = kaya({
        chat: async () => {
          throw new LlmError("groq", code);
        },
      });
      await channel.start();
      await assert.rejects(
        say("hello"),
        (err: unknown) => err instanceof AggregateError && err.errors[0]?.code === code,
      );
      assert.equal(channel.sent.length, 1, code);
      assert.match(channel.sent[0]?.text ?? "", notice);
      assert.match(channel.sent[0]?.idempotencyKey ?? "", /:failed$/);
    }
  });
});
