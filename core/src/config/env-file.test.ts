import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, realpathSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { after, describe, it } from "node:test";
import { findEnvFile, loadEnvFile } from "./env-file.js";

const root = realpathSync(mkdtempSync(join(tmpdir(), "kaya-env-")));
after(() => rmSync(root, { recursive: true, force: true }));

function makeRepo(name: string, withEnv: boolean): string {
  const repo = join(root, name);
  mkdirSync(join(repo, ".git"), { recursive: true });
  mkdirSync(join(repo, "core", "src"), { recursive: true });
  if (withEnv) writeFileSync(join(repo, ".env"), "KAYA_TEST_VALUE=from-file\n");
  return repo;
}

describe("findEnvFile", () => {
  it("finds .env in a parent folder", () => {
    const repo = makeRepo("found", true);
    assert.equal(findEnvFile(join(repo, "core", "src")), join(repo, ".env"));
  });

  it("stops at the repo root", () => {
    writeFileSync(join(root, ".env"), "OUTSIDE=1\n");
    const repo = makeRepo("missing", false);
    assert.equal(findEnvFile(join(repo, "core")), undefined);
  });
});

describe("loadEnvFile", () => {
  it("loads the file named by KAYA_ENV_FILE without overriding real variables", () => {
    const file = join(root, "custom.env");
    writeFileSync(file, "KAYA_TEST_A=from-file\nKAYA_TEST_B=from-file\n");
    process.env.KAYA_TEST_B = "from-env";
    assert.equal(loadEnvFile({ KAYA_ENV_FILE: file }), file);
    assert.equal(process.env.KAYA_TEST_A, "from-file");
    assert.equal(process.env.KAYA_TEST_B, "from-env");
    delete process.env.KAYA_TEST_A;
    delete process.env.KAYA_TEST_B;
  });

  it("fails loudly when KAYA_ENV_FILE points at a missing file", () => {
    assert.throws(() => loadEnvFile({ KAYA_ENV_FILE: join(root, "nope.env") }), { code: "ENOENT" });
  });
});
