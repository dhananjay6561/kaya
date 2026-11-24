import type { LlmConfig } from "../../config/index.js";
import { createGroqLlm } from "./groq.js";
import { createStubLlm } from "./stub.js";
import type { LlmProvider } from "./types.js";

export { LlmError, type LlmErrorCode, type LlmErrorInfo } from "./errors.js";
export { createGroqLlm, type GroqOptions } from "./groq.js";
export { createStubLlm, type StubLlm, type StubLlmOptions } from "./stub.js";
export type * from "./types.js";

export function createLlm(config: LlmConfig): LlmProvider {
  switch (config.provider) {
    case "groq":
      return createGroqLlm(config.groq);
    case "stub":
      return createStubLlm();
  }
}
