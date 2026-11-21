import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { createLogger } from "./logger.js";

function capture() {
  const lines: Record<string, unknown>[] = [];
  return {
    lines,
    stream: { write: (chunk: string) => lines.push(JSON.parse(chunk)) },
  };
}

describe("createLogger", () => {
  it("writes JSON lines with service, version, iso time and level names", () => {
    const { lines, stream } = capture();
    const logger = createLogger({ level: "trace", service: "core", version: "v1" }, stream);
    logger.trace("a");
    logger.info({ count: 2 }, "b");
    logger.fatal("c");

    assert.equal(lines.length, 3);
    assert.deepEqual(
      lines.map((l) => l.level),
      ["trace", "info", "fatal"],
    );
    for (const line of lines) {
      assert.equal(line.service, "core");
      assert.equal(line.version, "v1");
      assert.ok(!Number.isNaN(Date.parse(String(line.time))));
    }
    assert.equal(lines[1]?.msg, "b");
    assert.equal(lines[1]?.count, 2);
  });

  it("drops lines below the level", () => {
    const { lines, stream } = capture();
    const logger = createLogger({ level: "warn", service: "core", version: "v1" }, stream);
    logger.info("hidden");
    logger.warn("shown");
    assert.deepEqual(
      lines.map((l) => l.msg),
      ["shown"],
    );
  });

  it("serialises errors with their message and stack", () => {
    const { lines, stream } = capture();
    const logger = createLogger({ level: "info", service: "core", version: "v1" }, stream);
    logger.error({ err: new Error("boom") }, "failed");
    const err = lines[0]?.err as { message: string; stack: string };
    assert.equal(err.message, "boom");
    assert.ok(err.stack.includes("Error: boom"));
  });

  it("keeps only safe fields from errors, so attached clients never reach the logs", () => {
    const { lines, stream } = capture();
    const logger = createLogger({ level: "info", service: "core", version: "v1" }, stream);
    const err = Object.assign(new Error("terminated", { cause: new Error("socket closed") }), {
      code: "57P01",
      client: { connectionParameters: { password: "db-password-not-real", host: "db" } },
    });
    logger.error({ err }, "failed");

    const line = JSON.stringify(lines[0]);
    assert.ok(!line.includes("db-password-not-real"));
    assert.ok(!line.includes("connectionParameters"));
    const logged = lines[0]?.err as { code: string; cause: { message: string } };
    assert.equal(logged.code, "57P01");
    assert.equal(logged.cause.message, "socket closed");
  });

  it("serialises thrown non-errors", () => {
    const { lines, stream } = capture();
    const logger = createLogger({ level: "info", service: "core", version: "v1" }, stream);
    logger.error({ err: "plain string" }, "failed");
    assert.deepEqual(lines[0]?.err, { type: "string", message: "plain string" });
  });
});
