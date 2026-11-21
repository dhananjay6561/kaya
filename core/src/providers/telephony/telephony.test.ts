import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { createStubTelephony, createTelephony } from "./index.js";

describe("stub telephony", () => {
  it("places a call that completes and can be looked up", async () => {
    const telephony = createStubTelephony();
    const call = await telephony.placeCall({ to: "+919800000000", brief: "ask about Saturday" });
    assert.equal(call.status, "completed");
    assert.equal(call.to, "+919800000000");
    assert.ok(call.transcript);
    assert.deepEqual(await telephony.getCall(call.id), call);
    assert.equal(await telephony.getCall("missing"), undefined);
    assert.equal(telephony.calls.length, 1);
  });

  it("rejects numbers that are not E.164", async () => {
    const telephony = createStubTelephony();
    for (const to of ["9800000000", "+0123456789", "+91 98000 00000", "+91-9800000000", ""]) {
      await assert.rejects(telephony.placeCall({ to, brief: "x" }), /E\.164/, to);
    }
    assert.equal(telephony.calls.length, 0);
  });

  it("rejects an empty brief", async () => {
    await assert.rejects(createStubTelephony().placeCall({ to: "+919800000000", brief: " " }));
  });
});

describe("createTelephony", () => {
  it("builds the provider named in config", () => {
    assert.equal(createTelephony({ provider: "stub" }).name, "stub");
  });
});
