import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { ProviderNotReadyError } from "../errors.js";
import { createLlm, createStubLlm } from "./index.js";

describe("stub llm", () => {
  it("answers from the last user message and reports the tier and usage", async () => {
    const llm = createStubLlm();
    const result = await llm.complete({
      tier: "routine",
      messages: [
        { role: "system", content: "be brief" },
        { role: "user", content: "first" },
        { role: "assistant", content: "ok" },
        { role: "user", content: "what did Rahul say?" },
      ],
    });
    assert.equal(result.text, "stub reply: what did Rahul say?");
    assert.equal(result.model, "stub-routine");
    assert.ok(result.usage.inputTokens > 0);
    assert.ok(result.usage.outputTokens > 0);
    assert.equal(llm.requests.length, 1);
  });

  it("uses a custom reply when given one", async () => {
    const llm = createStubLlm({ reply: (r) => `tier ${r.tier}` });
    const result = await llm.complete({
      tier: "heavy",
      messages: [{ role: "user", content: "x" }],
    });
    assert.equal(result.text, "tier heavy");
    assert.equal(result.model, "stub-heavy");
  });

  it("rejects an empty conversation", async () => {
    await assert.rejects(createStubLlm().complete({ tier: "routine", messages: [] }));
  });

  it("rejects when the signal is already aborted", async () => {
    const controller = new AbortController();
    controller.abort();
    await assert.rejects(
      createStubLlm().complete(
        { tier: "routine", messages: [{ role: "user", content: "x" }] },
        { signal: controller.signal },
      ),
      { name: "AbortError" },
    );
  });
});

describe("createLlm", () => {
  it("builds the provider named in config", async () => {
    assert.equal(createLlm({ provider: "stub" }).name, "stub");

    const groq = createLlm({
      provider: "groq",
      groq: { apiKey: "test-key-not-real", heavyModel: "h", routineModel: "r" },
    });
    assert.equal(groq.name, "groq");
    await assert.rejects(
      groq.complete({ tier: "routine", messages: [{ role: "user", content: "x" }] }),
      (err: unknown) => err instanceof ProviderNotReadyError && err.phase === "3.1",
    );
  });
});
