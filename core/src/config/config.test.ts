import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { type Config, ConfigError, type GroqConfig, loadConfig } from "./config.js";

const valid = {
  DATABASE_URL: "postgres://kaya:kaya@localhost:5432/kaya",
  GROQ_API_KEY: "test-key-not-real",
};

function groqOf(config: Config): GroqConfig {
  assert.equal(config.llm.provider, "groq");
  if (config.llm.provider !== "groq") assert.fail("expected groq");
  return config.llm.groq;
}

function problemsFor(env: NodeJS.ProcessEnv): string[] {
  try {
    loadConfig(env, "/base");
  } catch (error) {
    assert.ok(error instanceof ConfigError);
    return error.problems;
  }
  assert.fail("expected a ConfigError");
}

describe("loadConfig", () => {
  it("applies defaults when only required values are set", () => {
    const config = loadConfig(valid, "/base");
    assert.equal(config.logLevel, "info");
    assert.equal(config.port, 3000);
    assert.equal(config.listenHost, "127.0.0.1");
    assert.equal(config.timeZone, "UTC");
    assert.equal(config.dataDir, "/base/data");
    assert.equal(groqOf(config).heavyModel, "openai/gpt-oss-120b");
    assert.equal(groqOf(config).routineModel, "openai/gpt-oss-20b");
    assert.equal(groqOf(config).contextTokens, 3_000);
    assert.equal(config.channel.provider, "gateway");
    assert.equal(config.telephony.provider, "stub");
  });

  it("reads every value when set", () => {
    const config = loadConfig(
      {
        ...valid,
        LOG_LEVEL: "debug",
        CORE_PORT: "8080",
        LISTEN_HOST: "0.0.0.0",
        DATA_DIR: "/srv/kaya",
        TIMEZONE: "Asia/Kolkata",
        GROQ_MODEL_HEAVY: "heavy",
        GROQ_MODEL_ROUTINE: "routine",
        GROQ_CONTEXT_TOKENS: "12000",
      },
      "/base",
    );
    assert.equal(config.logLevel, "debug");
    assert.equal(config.port, 8080);
    assert.equal(config.listenHost, "0.0.0.0");
    assert.equal(config.dataDir, "/srv/kaya");
    assert.equal(config.timeZone, "Asia/Kolkata");
    assert.equal(groqOf(config).heavyModel, "heavy");
    assert.equal(groqOf(config).routineModel, "routine");
    assert.equal(groqOf(config).contextTokens, 12_000);
  });

  it("accepts postgresql:// URLs", () => {
    const config = loadConfig({ ...valid, DATABASE_URL: "postgresql://u:p@db/kaya" }, "/base");
    assert.equal(config.databaseUrl, "postgresql://u:p@db/kaya");
  });

  it("prefers CORE_DATABASE_URL over the shared DATABASE_URL", () => {
    const config = loadConfig(
      { ...valid, CORE_DATABASE_URL: "postgres://kaya_core@db:5432/kaya" },
      "/base",
    );
    assert.equal(config.databaseUrl, "postgres://kaya_core@db:5432/kaya");
    assert.equal(
      loadConfig({ ...valid, CORE_DATABASE_URL: "" }, "/base").databaseUrl,
      valid.DATABASE_URL,
    );
  });

  it("switches providers through config alone", () => {
    const config = loadConfig(
      { DATABASE_URL: valid.DATABASE_URL, LLM_PROVIDER: "stub", CHAT_CHANNEL: "stub" },
      "/base",
    );
    assert.deepEqual(config.llm, { provider: "stub" });
    assert.equal(config.channel.provider, "stub");
  });

  it("needs GROQ_API_KEY only when the LLM provider is groq", () => {
    assert.deepEqual(problemsFor({ DATABASE_URL: valid.DATABASE_URL, LLM_PROVIDER: "groq" }), [
      "GROQ_API_KEY: required",
    ]);
    assert.doesNotThrow(() =>
      loadConfig({ DATABASE_URL: valid.DATABASE_URL, LLM_PROVIDER: "stub" }, "/base"),
    );
  });

  it("rejects unknown providers", () => {
    const problems = problemsFor({
      ...valid,
      LLM_PROVIDER: "openai",
      CHAT_CHANNEL: "telegram",
      TELEPHONY_PROVIDER: "skype",
    });
    assert.equal(problems.length, 3);
    for (const key of ["LLM_PROVIDER", "CHAT_CHANNEL", "TELEPHONY_PROVIDER"]) {
      assert.ok(
        problems.some((p) => p.startsWith(`${key}: `)),
        key,
      );
    }
  });

  it("lists every missing required value at once", () => {
    assert.deepEqual(problemsFor({}), ["DATABASE_URL: required", "GROQ_API_KEY: required"]);
  });

  it("treats empty values as missing", () => {
    assert.deepEqual(problemsFor({ DATABASE_URL: "", GROQ_API_KEY: "" }), [
      "DATABASE_URL: required",
      "GROQ_API_KEY: required",
    ]);
  });

  it("rejects invalid values with a clear reason", () => {
    const problems = problemsFor({
      ...valid,
      CORE_PORT: "70000",
      DATABASE_URL: "http://localhost/kaya",
      LOG_LEVEL: "loud",
      TIMEZONE: "India/Delhi",
    });
    assert.equal(problems.length, 4);
    assert.ok(problems.includes("TIMEZONE: must be an IANA time zone such as Asia/Kolkata"));
    assert.ok(problems.includes("CORE_PORT: must be between 1 and 65535"));
    assert.ok(problems.includes("DATABASE_URL: must be a postgres:// URL"));
    assert.ok(problems.some((p) => p.startsWith("LOG_LEVEL: ")));
  });

  it("refuses a context budget too small to hold a question", () => {
    assert.deepEqual(problemsFor({ ...valid, GROQ_CONTEXT_TOKENS: "100" }), [
      "GROQ_CONTEXT_TOKENS: must be at least 500",
    ]);
  });

  it("rejects a port that isn't a number", () => {
    assert.deepEqual(problemsFor({ ...valid, CORE_PORT: "abc" }), ["CORE_PORT: must be a number"]);
  });

  it("never puts secret values in the error", () => {
    const secret = "gsk_super_secret_value";
    assert.throws(
      () => loadConfig({ ...valid, GROQ_API_KEY: secret, CORE_PORT: "x" }),
      (error: Error) => error instanceof ConfigError && !error.message.includes(secret),
    );
  });

  it("returns a frozen config", () => {
    const config = loadConfig(valid, "/base");
    assert.ok(Object.isFrozen(config));
    assert.ok(Object.isFrozen(config.llm));
    assert.ok(Object.isFrozen(groqOf(config)));
    assert.ok(Object.isFrozen(config.channel));
    assert.ok(Object.isFrozen(config.telephony));
  });
});
