import { createHash } from "node:crypto";
import type { EmbeddingProvider, EmbedKind } from "./types.js";

export type StubEmbeddings = EmbeddingProvider &
  Readonly<{ calls: readonly { texts: readonly string[]; kind: EmbedKind }[] }>;

/** Turns each word into a fixed direction, so texts sharing words point the same way. */
export function createStubEmbeddings(dimensions = 16): StubEmbeddings {
  const calls: { texts: readonly string[]; kind: EmbedKind }[] = [];
  const vectorOf = (text: string) => {
    const vector = new Array<number>(dimensions).fill(0);
    for (const word of text.toLowerCase().match(/[\p{L}\p{N}]+/gu) ?? []) {
      const hash = createHash("sha256").update(word).digest();
      vector[(hash[0] ?? 0) % dimensions] = (vector[(hash[0] ?? 0) % dimensions] ?? 0) + 1;
    }
    const length = Math.hypot(...vector) || 1;
    return vector.map((x) => x / length);
  };
  return Object.freeze({
    name: "stub",
    model: "stub",
    dimensions,
    locality: "local",
    maxBatch: 8,
    calls,
    async embed(texts: readonly string[], kind: EmbedKind, options = {}) {
      (options as { signal?: AbortSignal }).signal?.throwIfAborted();
      calls.push({ texts: [...texts], kind });
      return texts.map(vectorOf);
    },
  });
}
