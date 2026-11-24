export type LlmErrorCode =
  | "rate_limited"
  | "timeout"
  | "unavailable"
  | "auth"
  | "rejected"
  | "bad_response";

export type LlmErrorInfo = Readonly<{ status?: number; detail?: string; retryAfterMs?: number }>;

export class LlmError extends Error {
  readonly status: number | undefined;
  readonly detail: string | undefined;
  readonly retryAfterMs: number | undefined;

  constructor(
    readonly provider: string,
    readonly code: LlmErrorCode,
    info: LlmErrorInfo = {},
  ) {
    const status = info.status ? ` (HTTP ${info.status})` : "";
    const detail = info.detail ? `, ${info.detail}` : "";
    super(`${provider} could not answer: ${code}${status}${detail}`);
    this.name = "LlmError";
    this.status = info.status;
    this.detail = info.detail;
    this.retryAfterMs = info.retryAfterMs;
  }
}
