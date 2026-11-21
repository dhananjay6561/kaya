import assert from "node:assert/strict";
import { describe, it } from "node:test";
import {
  type HttpServerOptions,
  requestIdHeader,
  sendJson,
  startHttpServer,
} from "./http-server.js";
import { createLogger } from "./logger.js";

function capture() {
  const lines: Record<string, unknown>[] = [];
  const logger = createLogger(
    { level: "debug", service: "core", version: "test" },
    { write: (chunk: string) => lines.push(JSON.parse(chunk)) },
  );
  return { lines, logger };
}

async function withServer(
  routes: HttpServerOptions["routes"],
  fn: (base: string, lines: Record<string, unknown>[]) => Promise<void>,
) {
  const { lines, logger } = capture();
  const server = await startHttpServer({ host: "127.0.0.1", port: 0, logger, routes });
  try {
    await fn(`http://127.0.0.1:${server.port}`, lines);
  } finally {
    await server.close();
  }
}

describe("startHttpServer", () => {
  it("routes by path and ignores the query string", async () => {
    await withServer(
      { "/ping": (_req, res) => sendJson(res, 200, { pong: true }) },
      async (base) => {
        const res = await fetch(`${base}/ping?x=1`);
        assert.equal(res.status, 200);
        assert.equal(res.headers.get("content-type"), "application/json");
        assert.deepEqual(await res.json(), { pong: true });
      },
    );
  });

  it("returns 404 for unknown paths, including inherited object keys", async () => {
    await withServer({}, async (base) => {
      assert.equal((await fetch(`${base}/nope`)).status, 404);
      assert.equal((await fetch(`${base}/constructor`)).status, 404);
    });
  });

  it("keeps a valid request id, replaces an invalid one, and logs it", async () => {
    await withServer({ "/ping": (_req, res) => sendJson(res, 200, {}) }, async (base, lines) => {
      const kept = await fetch(`${base}/ping`, { headers: { [requestIdHeader]: "abc-123" } });
      assert.equal(kept.headers.get(requestIdHeader), "abc-123");

      const replaced = await fetch(`${base}/ping`, { headers: { [requestIdHeader]: "bad id" } });
      assert.match(replaced.headers.get(requestIdHeader) ?? "", /^[a-f0-9]{24}$/);

      const logged = lines.find((l) => l.msg === "http request" && l.request_id === "abc-123");
      assert.ok(logged, "request log line with the id");
      assert.equal(logged.status, 200);
      assert.equal(logged.path, "/ping");
    });
  });

  it("turns a thrown error into a 500 and logs it", async () => {
    await withServer(
      {
        "/boom": () => {
          throw new Error("secret detail");
        },
      },
      async (base, lines) => {
        const res = await fetch(`${base}/boom`);
        assert.equal(res.status, 500);
        const text = await res.text();
        assert.ok(!text.includes("secret detail"));
        assert.ok(lines.some((l) => l.msg === "request failed" && l.level === "error"));
      },
    );
  });

  it("stops answering after close", async () => {
    const { logger } = capture();
    const server = await startHttpServer({ host: "127.0.0.1", port: 0, logger, routes: {} });
    const base = `http://127.0.0.1:${server.port}`;
    assert.equal((await fetch(`${base}/x`)).status, 404);
    await server.close();
    await assert.rejects(fetch(`${base}/x`));
  });

  it("rejects when the port is taken", async () => {
    const { logger } = capture();
    const server = await startHttpServer({ host: "127.0.0.1", port: 0, logger, routes: {} });
    try {
      await assert.rejects(
        startHttpServer({ host: "127.0.0.1", port: server.port, logger, routes: {} }),
        { code: "EADDRINUSE" },
      );
    } finally {
      await server.close();
    }
  });
});
