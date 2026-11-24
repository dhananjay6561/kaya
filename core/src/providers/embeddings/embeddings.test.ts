import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { EmbeddingError } from "./errors.js";
import { createStubEmbeddings } from "./stub.js";
import { createVoyageEmbeddings, type VoyageOptions } from "./voyage.js";

const vector = (n: number, dims = 4) => Array.from({ length: dims }, (_, i) => n + i / 10);

const ok = (count: number, dims = 4, order?: number[]) =>
  new Response(
    JSON.stringify({
      data: (order ?? [...Array(count).keys()]).map((i) => ({
        index: i,
        embedding: vector(i, dims),
      })),
      model: "voyage-4",
      usage: { total_tokens: 10 },
    }),
    { status: 200 },
  );

function voyage(replies: (() => Response)[], options: VoyageOptions = {}) {
  const calls: { url: string; body: Record<string, unknown>; auth: string }[] = [];
  const fake: typeof fetch = async (input, init = {}) => {
    calls.push({
      url: String(input),
      body: JSON.parse(String(init.body)),
      auth: (init.headers as Record<string, string>).Authorization ?? "",
    });
    const next = replies.shift();
    if (!next) throw new Error("no more replies");
    return next();
  };
  const provider = createVoyageEmbeddings(
    { apiKey: "test-key-not-real", model: "voyage-4", dimensions: 4 },
    { fetch: fake, retryBaseMs: 1, ...options },
  );
  return { provider, calls };
}

describe("voyage embeddings", () => {
  it("asks for the model, kind and size, and returns vectors in input order", async () => {
    const { provider, calls } = voyage([() => ok(3, 4, [2, 0, 1])]);
    const vectors = await provider.embed(["a", "b", "c"], "document");
    assert.deepEqual(vectors, [vector(0), vector(1), vector(2)]);
    assert.equal(calls[0]?.url, "https://api.voyageai.com/v1/embeddings");
    assert.equal(calls[0]?.auth, "Bearer test-key-not-real");
    assert.deepEqual(calls[0]?.body, {
      model: "voyage-4",
      input: ["a", "b", "c"],
      input_type: "document",
      output_dimension: 4,
    });
    assert.equal(provider.locality, "cloud");
  });

  it("marks a query as a query", async () => {
    const { provider, calls } = voyage([() => ok(1)]);
    await provider.embed(["what did Rahul say?"], "query");
    assert.equal(calls[0]?.body.input_type, "query");
  });

  it("sends nothing for nothing, and refuses an oversized batch", async () => {
    const { provider, calls } = voyage([]);
    assert.deepEqual(await provider.embed([], "document"), []);
    await assert.rejects(provider.embed(Array(129).fill("x"), "document"));
    assert.equal(calls.length, 0);
  });

  it("retries when Voyage is busy or down", async () => {
    const { provider, calls } = voyage([
      () => new Response("{}", { status: 429, headers: { "retry-after": "0" } }),
      () => new Response("{}", { status: 503 }),
      () => ok(1),
    ]);
    assert.equal((await provider.embed(["a"], "document")).length, 1);
    assert.equal(calls.length, 3);
  });

  it("does not retry a refused key or request, and keeps the body out of the error", async () => {
    for (const [status, code] of [
      [401, "auth"],
      [400, "rejected"],
    ] as const) {
      const { provider, calls } = voyage([
        () => new Response(JSON.stringify({ detail: "secret chat text" }), { status }),
      ]);
      await assert.rejects(provider.embed(["a"], "document"), (err: unknown) => {
        assert.ok(err instanceof EmbeddingError);
        assert.equal(err.code, code);
        assert.doesNotMatch(err.message, /secret/);
        return true;
      });
      assert.equal(calls.length, 1);
    }
  });

  it("rejects an answer of the wrong shape or size", async () => {
    for (const reply of [
      () => ok(1, 3),
      () => ok(1),
      () => new Response("<html>", { status: 200 }),
    ]) {
      const { provider } = voyage([reply]);
      await assert.rejects(
        provider.embed(["a", "b"], "document"),
        (err: unknown) => err instanceof EmbeddingError && err.code === "bad_response",
      );
    }
  });

  it("gives up on a long rate limit instead of waiting it out", async () => {
    const { provider, calls } = voyage(
      [() => new Response("{}", { status: 429, headers: { "retry-after": "600" } })],
      { maxRetryWaitMs: 1_000 },
    );
    await assert.rejects(
      provider.embed(["a"], "document"),
      (err: unknown) => err instanceof EmbeddingError && err.code === "rate_limited",
    );
    assert.equal(calls.length, 1);
  });

  it("stops at once when the caller gives up", async () => {
    const controller = new AbortController();
    const fake: typeof fetch = (_input, init) =>
      new Promise((_, reject) => {
        init?.signal?.addEventListener("abort", () => reject(init.signal?.reason));
        setTimeout(() => controller.abort(new Error("core is stopping")), 5);
      });
    const provider = createVoyageEmbeddings(
      { apiKey: "k", model: "voyage-4", dimensions: 4 },
      { fetch: fake },
    );
    await assert.rejects(
      provider.embed(["a"], "document", { signal: controller.signal }),
      /core is stopping/,
    );
  });
});

describe("stub embeddings", () => {
  it("points texts that share words the same way, and records each call", async () => {
    const stub = createStubEmbeddings(16);
    const [a, b, c] = await stub.embed(
      ["money transfer", "transfer the money", "pizza tonight"],
      "document",
    );
    const cos = (x: number[] = [], y: number[] = []) =>
      x.reduce((sum, v, i) => sum + v * (y[i] ?? 0), 0);
    assert.ok(cos(a, b) > cos(a, c));
    assert.equal(a?.length, 16);
    assert.equal(stub.calls.length, 1);
  });
});
