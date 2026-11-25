import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { LlmError } from "./errors.js";
import { createGroqLlm, type GroqOptions } from "./groq.js";

type Call = { url: string; init: RequestInit; body: Record<string, unknown> };

const ok = (content: string | null, extra: Record<string, unknown> = {}) =>
  new Response(
    JSON.stringify({
      model: "openai/gpt-oss-20b",
      choices: [{ message: { role: "assistant", content }, finish_reason: "stop", ...extra }],
      usage: { prompt_tokens: 83, completion_tokens: 19 },
    }),
    { status: 200, headers: { "content-type": "application/json" } },
  );

const failure = (status: number, code?: string, headers: Record<string, string> = {}) =>
  new Response(JSON.stringify({ error: { message: "secret prompt text echoed back", code } }), {
    status,
    headers,
  });

function groq(replies: (() => Response | Promise<Response>)[], options: GroqOptions = {}) {
  const calls: Call[] = [];
  const fake: typeof fetch = async (input, init = {}) => {
    calls.push({ url: String(input), init, body: JSON.parse(String(init.body)) });
    const next = replies.shift();
    if (!next) throw new Error("no more replies");
    return next();
  };
  const llm = createGroqLlm(
    {
      apiKey: "test-key-not-real",
      heavyModel: "big-model",
      routineModel: "small-model",
      contextTokens: 4_000,
    },
    { fetch: fake, retryBaseMs: 1, ...options },
  );
  return { llm, calls };
}

const ask = { tier: "routine", messages: [{ role: "user", content: "hi" }] } as const;

describe("groq", () => {
  it("asks the model for the tier and returns text, model, reason and usage", async () => {
    const { llm, calls } = groq([() => ok("Hello there!")]);
    const result = await llm.complete(ask);
    assert.deepEqual(result, {
      text: "Hello there!",
      model: "openai/gpt-oss-20b",
      finishReason: "stop",
      usage: { inputTokens: 83, outputTokens: 19 },
    });
    assert.equal(calls[0]?.url, "https://api.groq.com/openai/v1/chat/completions");
    assert.equal(
      (calls[0]?.init.headers as Record<string, string> | undefined)?.Authorization,
      "Bearer test-key-not-real",
    );
    assert.deepEqual(calls[0]?.body, {
      model: "small-model",
      messages: [{ role: "user", content: "hi" }],
      max_completion_tokens: 1024,
      reasoning_effort: "low",
      include_reasoning: false,
    });
  });

  it("uses the heavy model with more room and effort, and passes the caller's limits", async () => {
    const { llm, calls } = groq([() => ok("x"), () => ok("y")], {
      baseUrl: "http://localhost:9/v1/",
    });
    await llm.complete({ ...ask, tier: "heavy", maxTokens: 50, temperature: 0.2 });
    await llm.complete({ ...ask, tier: "heavy", effort: "low" });
    assert.equal(calls[0]?.url, "http://localhost:9/v1/chat/completions");
    assert.equal(calls[0]?.body.model, "big-model");
    assert.equal(calls[0]?.body.max_completion_tokens, 50);
    assert.equal(calls[0]?.body.temperature, 0.2);
    assert.equal(calls[0]?.body.reasoning_effort, "medium");
    assert.equal(calls[1]?.body.reasoning_effort, "low", "the caller can ask for less effort");
  });

  it("reports a cut-off answer and an empty one as they are", async () => {
    const { llm } = groq([() => ok("partial", { finish_reason: "length" }), () => ok(null)]);
    assert.equal((await llm.complete(ask)).finishReason, "length");
    assert.equal((await llm.complete(ask)).text, "");
  });

  it("retries when Groq is busy or down, then succeeds", async () => {
    const { llm, calls } = groq([
      () => failure(429, "rate_limit_exceeded", { "retry-after": "0" }),
      () => failure(503),
      () => ok("finally"),
    ]);
    assert.equal((await llm.complete(ask)).text, "finally");
    assert.equal(calls.length, 3);
  });

  it("gives up after the retries and says why, without the prompt or Groq's message", async () => {
    const { llm, calls } = groq([() => failure(500), () => failure(502), () => failure(503)]);
    await assert.rejects(llm.complete(ask), (err: unknown) => {
      assert.ok(err instanceof LlmError);
      assert.equal(err.code, "unavailable");
      assert.equal(err.status, 503);
      assert.doesNotMatch(err.message, /secret|hi/);
      return true;
    });
    assert.equal(calls.length, 3);
  });

  it("does not wait out a long rate limit", async () => {
    const { llm, calls } = groq([
      () => failure(429, "rate_limit_exceeded", { "retry-after": "60" }),
    ]);
    await assert.rejects(llm.complete(ask), (err: unknown) => {
      assert.ok(err instanceof LlmError);
      assert.equal(err.code, "rate_limited");
      assert.equal(err.retryAfterMs, 60_000);
      assert.equal(err.detail, "rate_limit_exceeded");
      return true;
    });
    assert.equal(calls.length, 1);
  });

  it("does not retry a refused key or a refused request", async () => {
    for (const [status, code] of [
      [401, "auth"],
      [403, "auth"],
      [400, "rejected"],
      [413, "too_long"],
      [404, "misconfigured"],
    ] as const) {
      const { llm, calls } = groq([() => failure(status, "invalid_api_key")]);
      await assert.rejects(llm.complete(ask), (err: unknown) => {
        return err instanceof LlmError && err.code === code && err.status === status;
      });
      assert.equal(calls.length, 1);
    }
  });

  it("names a too-long message and a model Groq does not know, whatever the status", async () => {
    for (const [code, expected] of [
      ["context_length_exceeded", "too_long"],
      ["model_decommissioned", "misconfigured"],
    ] as const) {
      const { llm } = groq([() => failure(400, code)]);
      await assert.rejects(
        llm.complete(ask),
        (err: unknown) => err instanceof LlmError && err.code === expected,
      );
    }
  });

  it("reads a Retry-After date", async () => {
    const at = new Date(Date.now() + 30_000).toUTCString();
    const { llm, calls } = groq([() => failure(429, undefined, { "retry-after": at })]);
    await assert.rejects(llm.complete(ask), (err: unknown) => {
      assert.ok(err instanceof LlmError);
      assert.ok((err.retryAfterMs ?? 0) > 20_000 && (err.retryAfterMs ?? 0) <= 30_000);
      return true;
    });
    assert.equal(calls.length, 1, "too long to wait for");
  });

  it("doubles its wait between tries and gives up rather than wait too long", async () => {
    const { llm, calls } = groq([() => failure(503), () => failure(503), () => ok("x")], {
      retryBaseMs: 6,
      maxRetryWaitMs: 10,
    });
    await assert.rejects(llm.complete(ask), (err: unknown) => {
      return err instanceof LlmError && err.code === "unavailable";
    });
    assert.equal(calls.length, 2, "the second wait (12 ms) is over the 10 ms limit");
  });

  it("stops retrying at the overall deadline", async () => {
    let calls = 0;
    const fake: typeof fetch = (_input, init) =>
      new Promise((_, reject) => {
        calls++;
        init?.signal?.addEventListener("abort", () => reject(init.signal?.reason));
      });
    const llm = createGroqLlm(
      { apiKey: "k", heavyModel: "h", routineModel: "r", contextTokens: 4_000 },
      { fetch: fake, timeoutMs: 1_000, deadlineMs: 30, maxRetries: 5, retryBaseMs: 1 },
    );
    const started = Date.now();
    await assert.rejects(
      llm.complete(ask),
      (err: unknown) => err instanceof LlmError && err.code === "timeout",
    );
    assert.ok(Date.now() - started < 500, "the deadline, not the per-call timeout, ends it");
    assert.ok(calls <= 2);
  });

  it("stops waiting between tries when the caller gives up", async () => {
    const controller = new AbortController();
    const { llm, calls } = groq([() => failure(503), () => ok("never")], { retryBaseMs: 5_000 });
    setTimeout(() => controller.abort(new Error("core is stopping")), 10);
    await assert.rejects(llm.complete(ask, { signal: controller.signal }), /core is stopping/);
    assert.equal(calls.length, 1);
  });

  it("calls a timeout while reading the answer a timeout, not a bad answer", async () => {
    const fake: typeof fetch = async (_input, init) => {
      const body = new ReadableStream({
        start(controller) {
          controller.enqueue(new TextEncoder().encode('{"model":'));
          init?.signal?.addEventListener("abort", () => controller.error(init.signal?.reason));
        },
      });
      return new Response(body, { status: 200 });
    };
    const llm = createGroqLlm(
      { apiKey: "k", heavyModel: "h", routineModel: "r", contextTokens: 4_000 },
      { fetch: fake, timeoutMs: 20, maxRetries: 0 },
    );
    await assert.rejects(
      llm.complete(ask),
      (err: unknown) => err instanceof LlmError && err.code === "timeout",
    );
  });

  it("keeps a network failure's code", async () => {
    const fake: typeof fetch = async () => {
      throw new TypeError("fetch failed", { cause: { code: "ECONNRESET" } });
    };
    const llm = createGroqLlm(
      { apiKey: "k", heavyModel: "h", routineModel: "r", contextTokens: 4_000 },
      { fetch: fake, maxRetries: 0 },
    );
    await assert.rejects(
      llm.complete(ask),
      (err: unknown) =>
        err instanceof LlmError && err.code === "unavailable" && err.detail === "econnreset",
    );
  });

  it("keeps only safe error codes from Groq", async () => {
    const { llm } = groq([() => failure(400, "Bad Thing: <script>")]);
    await assert.rejects(
      llm.complete(ask),
      (err: unknown) => err instanceof LlmError && err.detail === undefined,
    );
  });

  it("calls a malformed answer what it is", async () => {
    const { llm } = groq([() => new Response("<html>oops</html>", { status: 200 })]);
    await assert.rejects(
      llm.complete(ask),
      (err: unknown) => err instanceof LlmError && err.code === "bad_response",
    );
  });

  it("times out a call that hangs, and retries it", async () => {
    let calls = 0;
    const fake: typeof fetch = (_input, init) =>
      new Promise((_, reject) => {
        calls++;
        init?.signal?.addEventListener("abort", () => reject(init.signal?.reason));
      });
    const hanging = createGroqLlm(
      { apiKey: "k", heavyModel: "h", routineModel: "r", contextTokens: 4_000 },
      { fetch: fake, timeoutMs: 20, maxRetries: 1, retryBaseMs: 1 },
    );
    await assert.rejects(
      hanging.complete(ask),
      (err: unknown) => err instanceof LlmError && err.code === "timeout",
    );
    assert.equal(calls, 2);
  });

  it("stops at once when the caller gives up, with the caller's reason", async () => {
    const controller = new AbortController();
    const fake: typeof fetch = (_input, init) =>
      new Promise((_, reject) => {
        init?.signal?.addEventListener("abort", () => reject(init.signal?.reason));
        setTimeout(() => controller.abort(new Error("core is stopping")), 5);
      });
    const llm = createGroqLlm(
      { apiKey: "k", heavyModel: "h", routineModel: "r", contextTokens: 4_000 },
      { fetch: fake },
    );
    await assert.rejects(llm.complete(ask, { signal: controller.signal }), /core is stopping/);
  });

  it("counts tokens the way gpt-oss does, with the chat framing", () => {
    const { llm } = groq([]);
    assert.equal(llm.countTokens("hello how are you"), 4 + 4);
    assert.ok(llm.countTokens("order id 4839201, txn 99381") > 10, "numbers cost more than words");
  });

  it("counts text that looks like a special token as plain text", () => {
    const { llm } = groq([]);
    assert.equal(llm.countTokens("<|endoftext|>"), 7 + 4);
    assert.equal(llm.countTokens("say <|start|> now"), 7 + 4);
    assert.equal(llm.countTokens("<|reserved_200050|>"), 8 + 4);
  });

  it("does not call out for an empty conversation", async () => {
    const { llm, calls } = groq([]);
    await assert.rejects(llm.complete({ tier: "routine", messages: [] }));
    assert.equal(calls.length, 0);
  });
});
