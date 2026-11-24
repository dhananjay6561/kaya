import type { EmbeddingsConfig } from "../../config/index.js";
import { createStubEmbeddings } from "./stub.js";
import type { EmbeddingProvider } from "./types.js";
import { createVoyageEmbeddings } from "./voyage.js";

export { EmbeddingError, type EmbeddingErrorCode } from "./errors.js";
export { createStubEmbeddings, type StubEmbeddings } from "./stub.js";
export type * from "./types.js";
export { createVoyageEmbeddings, type VoyageConfig, type VoyageOptions } from "./voyage.js";

export function createEmbeddings(config: EmbeddingsConfig): EmbeddingProvider {
  return config.provider === "voyage"
    ? createVoyageEmbeddings(config.voyage)
    : createStubEmbeddings(config.dimensions);
}
