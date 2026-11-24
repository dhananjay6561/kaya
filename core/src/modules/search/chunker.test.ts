import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { type ChatLine, chunkLines } from "./chunker.js";

const at = (iso: string) => new Date(iso);
let n = 0;
const line = (iso: string, sender: string, text: string): ChatLine => ({
  messageId: String(++n),
  sentAt: at(iso),
  sender,
  text,
});
const options = { timeZone: "Asia/Kolkata" };

describe("chunker", () => {
  it("writes who said what and when, under the chat's name, in the owner's time zone", () => {
    const [chunk] = chunkLines(
      "Rahul",
      [
        line("2026-11-24T08:35:00Z", "Rahul", "kal milte hain?"),
        line("2026-11-24T08:36:00Z", "Me", "haan\n5 baje"),
      ],
      options,
    );
    assert.equal(
      chunk?.text,
      "Chat: Rahul\n24 Nov 2026\n14:05 Rahul: kal milte hain?\n14:06 Me: haan 5 baje",
    );
    assert.equal(chunk?.messageIds.length, 2);
    assert.match(chunk?.hash ?? "", /^[0-9a-f]{64}$/);
  });

  it("starts a new chunk after a quiet gap", () => {
    const chunks = chunkLines(
      "Group",
      [
        line("2026-11-24T08:00:00Z", "A", "one"),
        line("2026-11-24T08:09:00Z", "B", "two"),
        line("2026-11-24T08:30:00Z", "A", "three"),
      ],
      options,
    );
    assert.deepEqual(
      chunks.map((c) => c.messageIds.length),
      [2, 1],
    );
    assert.deepEqual(chunks[1]?.firstSentAt, at("2026-11-24T08:30:00Z"));
  });

  it("closes a chunk that grows too long or has too many lines", () => {
    const many = Array.from({ length: 7 }, (_, i) =>
      line(`2026-11-24T08:0${i}:00Z`, "A", "x".repeat(100)),
    );
    assert.deepEqual(
      chunkLines("G", many, { ...options, maxChars: 250 }).map((c) => c.messageIds.length),
      [2, 2, 2, 1],
    );
    assert.deepEqual(
      chunkLines("G", many, { ...options, maxLines: 3 }).map((c) => c.messageIds.length),
      [3, 3, 1],
    );
  });

  it("keeps a very long message as one chunk, cut at a whole character", () => {
    const [chunk] = chunkLines(
      "G",
      [line("2026-11-24T08:00:00Z", "A", "😀".repeat(3_000))],
      options,
    );
    assert.equal(chunk?.messageIds.length, 1);
    assert.ok((chunk?.text.length ?? 0) < 3_500);
    assert.ok(chunk?.text.isWellFormed());
  });

  it("marks the day again when a conversation runs past midnight", () => {
    const [chunk] = chunkLines(
      "G",
      [line("2026-11-24T18:25:00Z", "A", "late"), line("2026-11-24T18:32:00Z", "B", "later")],
      options,
    );
    assert.equal(chunk?.text, "Chat: G\n24 Nov 2026\n23:55 A: late\n25 Nov 2026\n00:02 B: later");
  });

  it("gives the same text the same hash, and changed text a new one", () => {
    const make = (text: string) =>
      chunkLines(
        "G",
        [{ messageId: "1", sentAt: at("2026-11-24T08:00:00Z"), sender: "A", text }],
        options,
      )[0]?.hash;
    assert.equal(make("hello"), make("hello"));
    assert.notEqual(make("hello"), make("hello!"));
  });

  it("makes nothing from nothing", () => {
    assert.deepEqual(chunkLines("G", [], options), []);
  });
});
