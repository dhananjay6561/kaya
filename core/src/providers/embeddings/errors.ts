export type EmbeddingErrorCode =
  | "rate_limited"
  | "timeout"
  | "unavailable"
  | "auth"
  | "rejected"
  | "bad_response";

export class EmbeddingError extends Error {
  constructor(
    readonly provider: string,
    readonly code: EmbeddingErrorCode,
    readonly status?: number,
    readonly retryAfterMs?: number,
  ) {
    super(`${provider} could not embed: ${code}${status ? ` (HTTP ${status})` : ""}`);
    this.name = "EmbeddingError";
  }
}
