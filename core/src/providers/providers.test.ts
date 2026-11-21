import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { loadConfig } from "../config/index.js";
import { createProviders } from "./index.js";

const base = { DATABASE_URL: "postgres://localhost:5432/kaya" };

describe("createProviders", () => {
  it("picks every provider from config, so swapping one is a config change", async () => {
    const groq = createProviders(loadConfig({ ...base, GROQ_API_KEY: "test-key-not-real" }, "/"));
    assert.deepEqual(
      [groq.llm.name, groq.channel.name, groq.telephony.name],
      ["groq", "stub", "stub"],
    );

    const stub = createProviders(loadConfig({ ...base, LLM_PROVIDER: "stub" }, "/"));
    assert.equal(stub.llm.name, "stub");
    const reply = await stub.llm.complete({
      tier: "routine",
      messages: [{ role: "user", content: "ping" }],
    });
    assert.equal(reply.text, "stub reply: ping");
  });

  it("gives each call fresh instances", () => {
    const config = loadConfig({ ...base, LLM_PROVIDER: "stub" }, "/");
    assert.notEqual(createProviders(config).channel, createProviders(config).channel);
  });
});
