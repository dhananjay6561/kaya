import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import { describe, it } from "node:test";
import { generateContractTypes, generatedTypesPath } from "./generate-types.js";
import { ContractError, createContracts, newSendRequest } from "./index.js";
import { samplesDir } from "./schemas.js";

const contracts = createContracts();

function samples(schema: string, kind: "valid" | "invalid"): [string, string][] {
  const dir = `${samplesDir}${schema}/${kind}/`;
  const files = readdirSync(dir).filter((f) => f.endsWith(".json"));
  assert.ok(files.length > 0, `no samples in ${dir}`);
  return files.map((f) => [f.replace(/\.json$/, ""), readFileSync(`${dir}${f}`, "utf8")]);
}

describe("event contract", () => {
  for (const [name, raw] of samples("event", "valid")) {
    it(`accepts ${name} unchanged`, () => {
      const event = contracts.parseEvent(raw);
      assert.deepEqual(event, JSON.parse(raw));
    });
  }

  for (const [name, raw] of samples("event", "invalid")) {
    it(`rejects ${name}`, () => {
      assert.throws(() => contracts.parseEvent(raw), ContractError);
    });
  }

  it("narrows the data type by the event type", () => {
    const [, raw] = samples("event", "valid").find(([n]) => n === "message-stored-live") ?? [];
    const event = contracts.parseEvent(raw);
    assert.equal(event.type, "message.stored");
    if (event.type === "message.stored") assert.equal(event.data.message_id, 42);
  });

  it("rejects text that is not JSON with a clear problem", () => {
    assert.throws(
      () => contracts.parseEvent("{not json"),
      (err: unknown) => err instanceof ContractError && err.problems[0] === "not JSON",
    );
  });

  it("names the failing field", () => {
    const [, raw] = samples("event", "invalid").find(([n]) => n === "progress-over-100") ?? [];
    assert.throws(
      () => contracts.parseEvent(raw),
      (err: unknown) => err instanceof ContractError && err.message.includes("/data/progress"),
    );
  });
});

describe("send-request contract", () => {
  for (const [name, raw] of samples("send-request", "valid")) {
    it(`accepts ${name} unchanged`, () => {
      assert.deepEqual(contracts.parseSendRequest(raw), JSON.parse(raw));
      assert.deepEqual(contracts.parseSendRequest(Buffer.from(raw)), JSON.parse(raw));
    });
  }

  for (const [name, raw] of samples("send-request", "invalid")) {
    it(`rejects ${name}`, () => {
      assert.throws(() => contracts.parseSendRequest(raw), ContractError);
    });
  }

  it("builds requests that pass the contract", () => {
    const request = newSendRequest("919999999999@s.whatsapp.net", "on my way", {
      wa_id: "3EB0C767D26B5A7F2A1F",
      sender_jid: "911111111111@s.whatsapp.net",
    });
    const encoded = contracts.encodeSendRequest(request);
    assert.deepEqual(JSON.parse(encoded), request);
    assert.notEqual(newSendRequest("1@s.whatsapp.net", "x").id, request.id);
  });

  it("refuses to encode a request that breaks the contract", () => {
    assert.throws(
      () => contracts.encodeSendRequest(newSendRequest("+919999999999", "hi")),
      ContractError,
    );
  });
});

describe("generated types", () => {
  it("match the schemas; run `make contracts` after changing a schema", async () => {
    assert.equal(readFileSync(generatedTypesPath, "utf8"), await generateContractTypes());
  });
});
