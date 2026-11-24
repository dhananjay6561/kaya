import assert from "node:assert/strict";
import { after, before, describe, it } from "node:test";
import { createDb, type Db } from "../../lib/db.js";
import { createLogger } from "../../lib/logger.js";
import { createTestDb, type TestDb, testDatabaseUrl } from "../../lib/test-db.js";
import {
  createMemoryPause,
  createPause,
  PauseStateMissingError,
  type PauseSwitch,
} from "./pause.js";

const logger = createLogger({ level: "silent", service: "core", version: "test" });

function behavesLikeAPause(make: () => Promise<PauseSwitch>) {
  it("starts running, pauses once and resumes once", async () => {
    const pause = await make();
    assert.deepEqual(await pause.state(), { paused: false });

    assert.equal(await pause.pause("cmd-1"), "paused");
    const paused = await pause.state();
    assert.ok(paused.paused && paused.since instanceof Date);
    assert.equal(await pause.pause("cmd-2"), "already_paused");
    assert.deepEqual(await pause.state(), paused, "a second pause keeps the first time");

    assert.equal(await pause.resume("cmd-3"), "resumed");
    assert.deepEqual(await pause.state(), { paused: false });
    assert.equal(await pause.resume("cmd-4"), "not_paused");
  });

  it("gives a repeated command the answer it got the first time", async () => {
    const pause = await make();
    assert.equal(await pause.pause("cmd-a"), "paused");
    assert.equal(await pause.pause("cmd-a"), "paused");
    assert.equal(await pause.resume("cmd-b"), "resumed");
    assert.equal(await pause.resume("cmd-b"), "resumed");
    assert.equal(await pause.pause("cmd-a"), "paused", "an old command run again still pauses");
    assert.equal(await pause.resume("cmd-c"), "resumed");
  });
}

describe("pause in memory", () => behavesLikeAPause(async () => createMemoryPause()));

describe("pause in the database", { skip: !testDatabaseUrl && "TEST_DATABASE_URL not set" }, () => {
  let testDb: TestDb;
  let db: Db;

  before(async () => {
    testDb = await createTestDb(testDatabaseUrl ?? "");
    db = createDb(testDb.url("kaya_core"), logger);
  });

  after(async () => {
    await db.close();
    await testDb.drop();
  });

  behavesLikeAPause(async () => {
    await testDb.admin.query("UPDATE pause_state SET paused_at = NULL, changed_by = NULL");
    return createPause(db.pool);
  });

  it("survives a restart: a new switch reads the same state", async () => {
    await testDb.admin.query("UPDATE pause_state SET paused_at = NULL, changed_by = NULL");
    await createPause(db.pool).pause("cmd-x");
    const state = await createPause(db.pool).state();
    assert.equal(state.paused, true);
  });

  it("refuses to guess when the row is gone", async () => {
    await testDb.admin.query("DELETE FROM pause_state");
    try {
      const pause = createPause(db.pool);
      await assert.rejects(pause.state(), PauseStateMissingError);
      await assert.rejects(pause.pause("cmd-y"), PauseStateMissingError);
      await assert.rejects(pause.resume("cmd-z"), PauseStateMissingError);
    } finally {
      await testDb.admin.query("INSERT INTO pause_state DEFAULT VALUES");
    }
  });
});
