import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { createLogger } from "../../lib/logger.js";
import { createStubChannel } from "../../providers/channel/index.js";
import { createMemoryPause } from "../pause/index.js";
import { parseSlash } from "./builtins.js";
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
    assert.deepEqual(parseSlash("/pause."), { kind: "unknown", name: "pause." });
    assert.equal(parseSlash("pause"), undefined);
    assert.equal(parseSlash("please /pause"), undefined);
    assert.equal(Array.from(parseSlash(`/${"😀".repeat(100)}`)?.name ?? "").length, 30);
  });
});

function kaya() {
  const channel = createStubChannel();
  const pause = createMemoryPause();
  const stop = startCommands({ channel, logger, pause, status: async () => "status text" });
  const say = async (text: string, sentAt?: Date) => {
    const before = channel.sent.length;
    await channel.receive(text, sentAt ? { sentAt } : {});
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
      /I am paused: only these commands get an answer\.$/,
    );
    assert.deepEqual(await say("/status"), ["status text"]);
    assert.match((await say("/nope"))[0] ?? "", /^I don't know \/nope\./);

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

  it("stays paused when /pause is run a second time, as after a restart", async () => {
    const { pause } = kaya();
    assert.equal(await pause.pause("cmd"), "paused");
    assert.equal(await pause.pause("cmd"), "paused");
  });
});
