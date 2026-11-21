import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { createDb } from "./db.js";
import { createLogger } from "./logger.js";

const url = process.env.TEST_DATABASE_URL;
const logger = createLogger({ level: "silent", service: "core", version: "test" });

describe("createDb", () => {
  it("checks a real Postgres", { skip: !url && "TEST_DATABASE_URL not set" }, async () => {
    const db = createDb(url ?? "", logger);
    try {
      await db.check(AbortSignal.timeout(5_000));
    } finally {
      await db.close();
    }
  });

  it("fails the check when the db is unreachable", async () => {
    const db = createDb("postgres://kaya@127.0.0.1:1/kaya", logger);
    try {
      await assert.rejects(db.check(AbortSignal.timeout(3_000)));
    } finally {
      await db.close();
    }
  });

  it("stops waiting when the signal aborts", async () => {
    const db = createDb("postgres://kaya@10.255.255.1:5432/kaya", logger);
    try {
      const started = Date.now();
      await assert.rejects(db.check(AbortSignal.timeout(100)), { name: "TimeoutError" });
      assert.ok(Date.now() - started < 2_000);
    } finally {
      await db.close();
    }
  });
});
