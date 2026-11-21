import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { ChannelNotStartedError, createChannel, createStubChannel } from "./index.js";

describe("stub channel", () => {
  it("records what it sends once started", async () => {
    const channel = createStubChannel();
    await channel.start();
    const sent = await channel.send({ text: "hello DJ" });
    assert.ok(sent.id);
    assert.ok(sent.sentAt instanceof Date);
    assert.deepEqual(channel.sent, [{ text: "hello DJ" }]);
  });

  it("refuses to send or receive before start and after stop", async () => {
    const channel = createStubChannel();
    await assert.rejects(channel.send({ text: "x" }), ChannelNotStartedError);
    await channel.start();
    await channel.stop();
    await assert.rejects(channel.receive("x"), ChannelNotStartedError);
  });

  it("refuses an empty message", async () => {
    const channel = createStubChannel();
    await channel.start();
    await assert.rejects(channel.send({ text: "  " }));
  });

  it("delivers received messages to every handler until it unsubscribes", async () => {
    const channel = createStubChannel();
    await channel.start();
    const seen: string[] = [];
    const off = channel.onMessage((m) => {
      seen.push(`a:${m.text}`);
    });
    channel.onMessage(async (m) => {
      seen.push(`b:${m.text}`);
    });

    await channel.receive("one");
    off();
    await channel.receive("two");
    assert.deepEqual(seen, ["a:one", "b:one", "b:two"]);
  });

  it("passes a handler failure back to the caller", async () => {
    const channel = createStubChannel();
    await channel.start();
    channel.onMessage(() => {
      throw new Error("handler broke");
    });
    await assert.rejects(channel.receive("x"), /handler broke/);
  });
});

describe("createChannel", () => {
  it("builds the channel named in config", () => {
    assert.equal(createChannel({ provider: "stub" }).name, "stub");
  });
});
