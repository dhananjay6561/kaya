import assert from "node:assert/strict";
import { type AddressInfo, createServer, type Socket } from "node:net";
import { describe, it } from "node:test";
import { createDb, isConnectionError } from "./db.js";
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

  it("frees the pooled client when a check times out on a db that stops answering", async () => {
    const sockets = new Set<Socket>();
    const silent = createServer((socket) => {
      sockets.add(socket);
      socket.once("data", () => {
        socket.write(Buffer.from([0x52, 0, 0, 0, 8, 0, 0, 0, 0]));
        socket.write(Buffer.from([0x5a, 0, 0, 0, 5, 0x49]));
      });
      socket.on("close", () => sockets.delete(socket));
    });
    await new Promise<void>((resolve) => silent.listen(0, "127.0.0.1", resolve));
    const { port } = silent.address() as AddressInfo;
    const db = createDb(`postgres://kaya@127.0.0.1:${port}/kaya`, logger);
    try {
      for (let i = 0; i < 12; i++) {
        const started = Date.now();
        await assert.rejects(db.check(AbortSignal.timeout(50)), { name: "TimeoutError" });
        assert.ok(Date.now() - started < 1_000, `check ${i} waited for a pooled client`);
      }
      await new Promise((resolve) => setTimeout(resolve, 50));
      assert.equal(db.pool.totalCount, 0, "timed-out clients must be destroyed, not kept");
      const closing = Date.now();
      await db.close();
      assert.ok(Date.now() - closing < 1_000, "close must not wait on stuck queries");
    } finally {
      for (const socket of sockets) socket.destroy();
      await new Promise((resolve) => silent.close(resolve));
    }
  });
});

describe("isConnectionError", () => {
  const coded = (code: string, message = "boom") => Object.assign(new Error(message), { code });

  it("recognises a database that is away", () => {
    for (const err of [
      coded("08006"),
      coded("08P01"),
      coded("57P01"),
      coded("57P03"),
      coded("53300"),
      coded("40001"),
      coded("40P01"),
      coded("ECONNRESET"),
      coded("ECONNREFUSED"),
      coded("EPIPE"),
      coded("ETIMEDOUT"),
      coded("ENOTFOUND"),
      new Error("Connection terminated unexpectedly"),
      new Error("Connection terminated"),
      new Error("Client has encountered a connection error and is not queryable"),
      new Error("timeout exceeded when trying to connect"),
    ]) {
      assert.equal(
        isConnectionError(err),
        true,
        `${(err as { code?: string }).code ?? err.message}`,
      );
    }
  });

  it("does not mistake a failure of the work itself for an outage", () => {
    for (const err of [
      coded("23505"),
      coded("23514"),
      coded("22P02"),
      coded("42501"),
      coded("57014"),
      coded("ENOENT"),
      coded("EACCES"),
      coded("EINVAL"),
      new Error("Connection error."),
      new Error("the upstream connection error was logged"),
      new TypeError("cannot read properties of undefined"),
      "Connection terminated",
      undefined,
    ]) {
      assert.equal(isConnectionError(err), false, String(err));
    }
  });
});
