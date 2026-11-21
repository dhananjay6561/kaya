import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { ConfigError, loadConfig } from "./config.js";

const valid = {
  DATABASE_URL: "postgres://kaya:kaya@localhost:5432/kaya",
  GROQ_API_KEY: "test-key-not-real",
};

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
    assert.equal(config.dataDir, "/base/data");
    assert.equal(config.llm.provider, "groq");
    assert.equal(config.llm.groq.heavyModel, "openai/gpt-oss-120b");
    assert.equal(config.llm.groq.routineModel, "openai/gpt-oss-20b");
  });

  it("reads every value when set", () => {
    const config = loadConfig(
      {
        ...valid,
        LOG_LEVEL: "debug",
        CORE_PORT: "8080",
        DATA_DIR: "/srv/kaya",
        GROQ_MODEL_HEAVY: "heavy",
        GROQ_MODEL_ROUTINE: "routine",
      },
      "/base",
    );
    assert.equal(config.logLevel, "debug");
    assert.equal(config.port, 8080);
    assert.equal(config.dataDir, "/srv/kaya");
    assert.equal(config.llm.groq.heavyModel, "heavy");
    assert.equal(config.llm.groq.routineModel, "routine");
  });

  it("accepts postgresql:// URLs", () => {
    const config = loadConfig({ ...valid, DATABASE_URL: "postgresql://u:p@db/kaya" }, "/base");
    assert.equal(config.databaseUrl, "postgresql://u:p@db/kaya");
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
    });
    assert.equal(problems.length, 3);
    assert.ok(problems.includes("CORE_PORT: must be between 1 and 65535"));
    assert.ok(problems.includes("DATABASE_URL: must be a postgres:// URL"));
    assert.ok(problems.some((p) => p.startsWith("LOG_LEVEL: ")));
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
    assert.ok(Object.isFrozen(config.llm.groq));
  });
});
