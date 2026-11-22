import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { createLogger } from "../../lib/logger.js";
import {
  type ChatChannel,
  createStubChannel,
  IdempotencyKeyReusedError,
} from "../../providers/channel/index.js";
import { startCommands } from "./commands.js";

const logger = createLogger({ level: "silent", service: "core", version: "test" });
const minute = 60_000;

describe("commands", () => {
  it("answers what DJ typed, quoting it, with a reply that is not worth sending late", async () => {
    const channel = createStubChannel();
    const options: unknown[] = [];
    const watched: ChatChannel = {
      ...channel,
      send: (message, sendOptions) => {
        options.push(sendOptions);
        return channel.send(message, sendOptions);
      },
    };
    startCommands({ channel: watched, logger });
    await channel.start();
    const message = await channel.receive("remind me to call mum");

    assert.deepEqual(channel.sent, [
      {
        idempotencyKey: `command:${message.id}`,
        text: "You said: remind me to call mum",
        replyTo: message.id,
      },
    ]);
    assert.deepEqual(options, [{ ttlMs: 2 * minute }]);
  });

  it("does not act on a command it only gets to much later, and says so", async () => {
    const channel = createStubChannel();
    let now = Date.now();
    startCommands({ channel, logger, now: () => now });
    await channel.start();

    now += 9 * minute;
    await channel.receive("fresh enough");
    now += 2 * minute;
    await channel.receive("typed before the clock moved");
    assert.equal(channel.sent[0]?.text, "You said: fresh enough");

    const late = createStubChannel();
    const sentAt = Date.now();
    startCommands({ channel: late, logger, now: () => sentAt + 11 * minute });
    await late.start();
    await late.receive("order the usual");
    assert.match(
      late.sent[0]?.text ?? "",
      /^I was away when you sent this, so I did not act on it\./,
    );
    assert.match(late.sent[0]?.text ?? "", /order the usual$/);
  });

  it("says nothing about a command more than a day old", async () => {
    const channel = createStubChannel();
    const sentAt = Date.now();
    startCommands({ channel, logger, now: () => sentAt + 25 * 60 * minute });
    await channel.start();
    await channel.receive("yesterday's note");
    assert.deepEqual(channel.sent, []);
  });

  it("keeps a very long message's echo short", async () => {
    const channel = createStubChannel();
    startCommands({ channel, logger });
    await channel.start();
    await channel.receive("x".repeat(5_000));
    assert.equal(channel.sent[0]?.text.length, "You said: ".length + 1_000 + 1);
  });

  it("treats a command it already answered as done, and passes other failures on", async () => {
    const channel = createStubChannel();
    let failure: Error = new IdempotencyKeyReusedError("3f0c1c2e-0000-5000-8000-000000000000");
    const failing: ChatChannel = {
      ...channel,
      send: async () => {
        throw failure;
      },
    };
    startCommands({ channel: failing, logger });
    await channel.start();
    await channel.receive("already answered before a restart");

    failure = new Error("the gateway never answered");
    await assert.rejects(
      channel.receive("this one fails"),
      (err: unknown) =>
        err instanceof AggregateError && /never answered/.test(String(err.errors[0])),
    );
  });

  it("stops answering once it is switched off", async () => {
    const channel = createStubChannel();
    const stop = startCommands({ channel, logger });
    await channel.start();
    stop();
    await channel.receive("anyone there?");
    assert.deepEqual(channel.sent, []);
  });
});
