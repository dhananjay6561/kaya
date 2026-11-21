import { randomUUID } from "node:crypto";
import { Ajv2020, type ErrorObject, type ValidateFunction } from "ajv/dist/2020.js";
import formatsPlugin from "ajv-formats";
import { loadSchemas, schemaBaseUrl } from "./schemas.js";
import type { GatewayEvent, ReplyTo, SendRequest } from "./types.gen.js";

const addFormats = formatsPlugin as unknown as typeof formatsPlugin.default;

export class ContractError extends Error {
  constructor(
    readonly schema: string,
    readonly problems: readonly string[],
  ) {
    super(`payload does not match the ${schema} contract: ${problems.join("; ")}`);
    this.name = "ContractError";
  }
}

export type Contracts = Readonly<{
  parseEvent(raw: unknown): GatewayEvent;
  parseSendRequest(raw: unknown): SendRequest;
  encodeSendRequest(request: SendRequest): string;
}>;

function describe(errors: ErrorObject[] | null | undefined): string[] {
  return (errors ?? []).map((e) => `${e.instancePath || "/"} ${e.message ?? "is invalid"}`);
}

function parser<T>(name: string, validate: ValidateFunction): (raw: unknown) => T {
  return (raw) => {
    let value = raw;
    if (typeof raw === "string" || raw instanceof Uint8Array) {
      try {
        value = JSON.parse(typeof raw === "string" ? raw : new TextDecoder().decode(raw));
      } catch {
        throw new ContractError(name, ["not JSON"]);
      }
    }
    if (!validate(value)) throw new ContractError(name, describe(validate.errors));
    return value as T;
  };
}

export function createContracts(): Contracts {
  const ajv = new Ajv2020({
    allErrors: true,
    strict: true,
    strictRequired: false,
    discriminator: true,
  });
  addFormats(ajv, ["uuid", "date-time"]);
  for (const schema of loadSchemas()) ajv.addSchema(schema);

  const compiled = (file: string) => {
    const validate = ajv.getSchema(`${schemaBaseUrl}${file}`);
    if (!validate) throw new Error(`contract schema ${file} is missing`);
    return validate;
  };
  const parseEvent = parser<GatewayEvent>("event", compiled("event.schema.json"));
  const parseSendRequest = parser<SendRequest>(
    "send-request",
    compiled("send-request.schema.json"),
  );

  return Object.freeze({
    parseEvent,
    parseSendRequest,
    encodeSendRequest(request: SendRequest) {
      return JSON.stringify(parseSendRequest(request));
    },
  });
}

export function newSendRequest(to: string, text: string, replyTo?: ReplyTo): SendRequest {
  return {
    id: randomUUID(),
    version: 1,
    created_at: new Date().toISOString(),
    to,
    content: { type: "text", text },
    ...(replyTo ? { reply_to: replyTo } : {}),
  };
}
