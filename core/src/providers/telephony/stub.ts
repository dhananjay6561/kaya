import { randomUUID } from "node:crypto";
import { type Call, type CallRequest, e164, type TelephonyProvider } from "./types.js";

export type StubTelephony = TelephonyProvider & Readonly<{ calls: readonly CallRequest[] }>;

export function createStubTelephony(): StubTelephony {
  const requests: CallRequest[] = [];
  const calls = new Map<string, Call>();

  return Object.freeze({
    name: "stub",
    calls: requests,
    async placeCall(request: CallRequest, options: Readonly<{ signal?: AbortSignal }> = {}) {
      options.signal?.throwIfAborted();
      if (!e164.test(request.to))
        throw new Error("call target must be an E.164 number like +919812345678");
      if (request.brief.trim() === "") throw new Error("call brief is empty");
      requests.push(request);
      const now = new Date();
      const call: Call = {
        id: randomUUID(),
        to: request.to,
        status: "completed",
        startedAt: now,
        endedAt: now,
        transcript: "stub call: no audio",
      };
      calls.set(call.id, call);
      return call;
    },
    async getCall(id: string) {
      return calls.get(id);
    },
  });
}
