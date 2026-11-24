import type { Locality } from "../llm/index.js";

/** A document is stored and searched; a query is what is searched with. */
export type EmbedKind = "document" | "query";

export type EmbedOptions = Readonly<{ signal?: AbortSignal }>;

export interface EmbeddingProvider {
  readonly name: string;
  readonly model: string;
  readonly dimensions: number;
  readonly locality: Locality;
  /** Most texts one call may carry. */
  readonly maxBatch: number;
  embed(texts: readonly string[], kind: EmbedKind, options?: EmbedOptions): Promise<number[][]>;
}
