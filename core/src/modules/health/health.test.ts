import assert from "node:assert/strict";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import { describe, it } from "node:test";
import { startHttpServer } from "../../lib/http-server.js";
import { createLogger } from "../../lib/logger.js";
import { type Checker, checkHealth, createHealthRoute } from "./health.js";

type HealthBody = {
  status: string;
  service: string;
  version: string;
  checks: Record<string, { status: string; duration_ms: number }>;
};

const healthy: Checker = { check: async () => {} };
const unhealthy: Checker = {
  check: async () => {
    throw new Error("connection refused");
  },
};
const slow: Checker = {
  check: (signal) =>
    new Promise((_, reject) => signal.addEventListener("abort", () => reject(signal.reason))),
};

async function withHealth(
  checks: Record<string, Checker>,
  fn: (url: string, logs: string[]) => Promise<void>,
  timeoutMs?: number,
) {
  const logs: string[] = [];
  const logger = createLogger(
    { level: "debug", service: "core", version: "test" },
    { write: (chunk: string) => logs.push(chunk) },
  );
  const route = createHealthRoute({ service: "core", version: "v1", checks, timeoutMs });
  const server = await startHttpServer({
    host: "127.0.0.1",
    port: 0,
    logger,
    routes: { "/health": route },
  });
  try {
    await fn(`http://127.0.0.1:${server.port}/health`, logs);
  } finally {
    await server.close();
  }
}

describe("health route", () => {
  it("reports ok when every check passes", async () => {
    await withHealth({ db: healthy }, async (url) => {
      const res = await fetch(url);
      assert.equal(res.status, 200);
      const body = (await res.json()) as HealthBody;
      assert.equal(body.status, "ok");
      assert.equal(body.service, "core");
      assert.equal(body.version, "v1");
      assert.equal(body.checks.db?.status, "ok");
      assert.equal(typeof body.checks.db?.duration_ms, "number");
    });
  });

  it("answers 503 and names the failing check without exposing the error", async () => {
    await withHealth({ db: unhealthy }, async (url, logs) => {
      const res = await fetch(url);
      assert.equal(res.status, 503);
      const text = await res.text();
      assert.ok(!text.includes("connection refused"));
      const body = JSON.parse(text) as HealthBody;
      assert.equal(body.status, "unhealthy");
      assert.equal(body.checks.db?.status, "down");
      assert.ok(logs.some((l) => l.includes("connection refused")));
    });
  });

  it("gives up on slow checks after the timeout", async () => {
    await withHealth(
      { db: slow },
      async (url) => {
        const started = Date.now();
        assert.equal((await fetch(url)).status, 503);
        assert.ok(Date.now() - started < 1_000);
      },
      50,
    );
  });

  it("allows HEAD and rejects other methods", async () => {
    await withHealth({ db: healthy }, async (url) => {
      assert.equal((await fetch(url, { method: "HEAD" })).status, 200);
      const post = await fetch(url, { method: "POST" });
      assert.equal(post.status, 405);
      assert.equal(post.headers.get("allow"), "GET, HEAD");
    });
  });
});

describe("checkHealth", () => {
  it("passes on 200 and fails on 503", async () => {
    await withHealth({ db: healthy }, async (url) => checkHealth(url));
    await withHealth({ db: unhealthy }, async (url) => {
      await assert.rejects(checkHealth(url), /answered 503/);
    });
  });

  it("fails when nothing listens", async () => {
    const other = createServer();
    await new Promise<void>((resolve) => other.listen(0, "127.0.0.1", resolve));
    const { port } = other.address() as AddressInfo;
    await new Promise((resolve) => other.close(resolve));
    await assert.rejects(checkHealth(`http://127.0.0.1:${port}/health`));
  });
});
