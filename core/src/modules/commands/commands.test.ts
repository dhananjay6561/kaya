import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { createLogger } from "../../lib/logger.js";
import {
  type ChatChannel,
  createStubChannel,
  type OutgoingMessage,
  SendFailedError,
  type SendOptions,
} from "../../providers/channel/index.js";
import { createMemoryPause } from "../pause/index.js";
import { clip, startCommands } from "./commands.js";

const logger = createLogger({ level: "silent", service: "core", version: "test" });
const minute = 60_000;
const ago = (ms: number) => new Date(Date.now() - ms);
const deps = () => ({
  pause: createMemoryPause(),
  status: async () => "all good",
  chat: async (message: { text: string }) => `You said: ${clip(message.text)}`,
});

function watched(fail?: (message: OutgoingMessage) => Error | undefined) {
  const channel = createStubChannel();
  const calls: { message: OutgoingMessage; options: SendOptions | undefined }[] = [];
  const wrapped: ChatChannel = {
    ...channel,
    send: async (message, options) => {
      calls.push({ message, options });
      const failure = fail?.(message);
      if (failure) throw failure;
      return channel.send(message, options);
    },
  };
  return { channel, wrapped, calls };
}

describe("commands", () => {
  it("answers what DJ typed, quoting it, for as long as the answer is still worth having", async () => {
    const { channel, wrapped, calls } = watched();
    startCommands({ channel: wrapped, logger, ...deps() });
    await channel.start();
    const message = await channel.receive("remind me to call mum", { sentAt: ago(4 * minute) });

    assert.deepEqual(channel.sent, [
      {
        idempotencyKey: `command:${message.id}:chat`,
        replyTo: message.id,
        text: "You said: remind me to call mum",
      },
    ]);
    const ttl = calls[0]?.options?.ttlMs ?? 0;
    assert.ok(
      ttl > 5 * minute && ttl <= 6 * minute,
      `the reply expires when the command turns 10 minutes old, got ${ttl}`,
    );
    assert.ok(
      calls[0]?.options?.signal instanceof AbortSignal,
      "the wait must end when core stops",
    );
  });

  it("does not act on a command it took in more than 10 minutes late, and says so", async () => {
    const { channel, wrapped, calls } = watched();
    startCommands({ channel: wrapped, logger, ...deps() });
    await channel.start();

    await channel.receive("just in time", { sentAt: ago(9 * minute) });
    assert.equal(channel.sent[0]?.text, "You said: just in time");

    const late = await channel.receive("order the usual", { sentAt: ago(11 * minute) });
    assert.deepEqual(channel.sent[1], {
      idempotencyKey: `command:${late.id}`,
      replyTo: late.id,
      text: "I was away when you sent this, so I did not act on it. Send it again if you still want it:\norder the usual",
    });
    assert.equal(calls[1]?.options?.ttlMs, 60 * minute);
  });

  it("says nothing about a command more than a day old", async () => {
    const { channel, wrapped } = watched();
    startCommands({ channel: wrapped, logger, ...deps() });
    await channel.start();
    await channel.receive("yesterday's note", { sentAt: ago(25 * 60 * minute) });
    assert.deepEqual(channel.sent, []);
  });

  it("tells DJ when the answer could not be sent in time, and reports the command as failed", async () => {
    for (const code of ["expired", "no_answer"]) {
      const { channel, wrapped } = watched((message) =>
        message.text.startsWith("You said") ? new SendFailedError("request-id", code) : undefined,
      );
      startCommands({ channel: wrapped, logger, ...deps() });
      await channel.start();
      await assert.rejects(
        channel.receive("turn the lights off"),
        (err: unknown) =>
          err instanceof AggregateError && (err.errors[0] as SendFailedError).code === code,
      );
      assert.equal(channel.sent.length, 1);
      assert.match(channel.sent[0]?.idempotencyKey ?? "", /^command:.+:unanswered$/);
      assert.match(
        channel.sent[0]?.text ?? "",
        /^I could not answer this in time, so nothing was done\./,
      );
      assert.match(channel.sent[0]?.text ?? "", /turn the lights off$/);
    }
  });

  it("sends nothing more when the answer may have gone out after all", async () => {
    const { channel, wrapped } = watched(
      () => new SendFailedError("request-id", "outcome_unknown"),
    );
    startCommands({ channel: wrapped, logger, ...deps() });
    await channel.start();
    await assert.rejects(channel.receive("hello"));
    assert.deepEqual(channel.sent, []);
  });

  it("cuts a long message at a whole character", () => {
    assert.equal(clip("short"), "short");
    const long = clip(`${"x".repeat(999)}😀😀`);
    assert.equal(long, `${"x".repeat(999)}😀…`);
    assert.ok(long.isWellFormed());
    assert.ok(clip(`broken \ud83d surrogate`).isWellFormed());
    assert.equal(Array.from(clip("😀".repeat(5_000))).length, 1_001);
  });

  it("stops answering once it is switched off", async () => {
    const channel = createStubChannel();
    const stop = startCommands({ channel, logger, ...deps() });
    await channel.start();
    stop();
    await channel.receive("anyone there?");
    assert.deepEqual(channel.sent, []);
  });
});
