import type { GroqConfig } from "../../config/index.js";
import { ProviderNotReadyError } from "../errors.js";
import type { LlmProvider } from "./types.js";

export function createGroqLlm(_config: GroqConfig): LlmProvider {
  return Object.freeze({
    name: "groq",
    complete: () => Promise.reject(new ProviderNotReadyError("llm", "groq", "3.1")),
  });
}
