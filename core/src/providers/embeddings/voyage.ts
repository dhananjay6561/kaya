import { z } from "zod";
import { EmbeddingError } from "./errors.js";
import type { EmbeddingProvider, EmbedKind, EmbedOptions } from "./types.js";

export type VoyageConfig = Readonly<{ apiKey: string; model: string; dimensions: number }>;

export type VoyageOptions = Readonly<{
  fetch?: typeof fetch;
  baseUrl?: string;
  timeoutMs?: number;
  maxRetries?: number;
  retryBaseMs?: number;
  maxRetryWaitMs?: number;
}>;

const responseSchema = z.object({
  data: z.array(z.object({ index: z.number().int(), embedding: z.array(z.number()) })),
});

function retryAfterMs(response: Response): number | undefined {
  const header = response.headers.get("retry-after");
  if (!header) return undefined;
  const seconds = Number(header);
  if (Number.isFinite(seconds)) return Math.max(0, seconds * 1_000);
  const at = Date.parse(header);
  return Number.isNaN(at) ? undefined : Math.max(0, at - Date.now());
}

function wait(ms: number, signal: AbortSignal | undefined): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(signal.reason);
      return;
    }
    const timer = setTimeout(done, ms);
    const aborted = () => {
      clearTimeout(timer);
      reject(signal?.reason);
    };
    function done() {
      signal?.removeEventListener("abort", aborted);
      resolve();
    }
    signal?.addEventListener("abort", aborted, { once: true });
  });
}

export function createVoyageEmbeddings(
  config: VoyageConfig,
  options: VoyageOptions = {},
): EmbeddingProvider {
  const send = options.fetch ?? fetch;
  const baseUrl = (options.baseUrl ?? "https://api.voyageai.com/v1").replace(/\/+$/, "");
  const timeoutMs = options.timeoutMs ?? 60_000;
  const maxRetries = options.maxRetries ?? 4;
  const retryBaseMs = options.retryBaseMs ?? 2_000;
  const maxRetryWaitMs = options.maxRetryWaitMs ?? 60_000;

  async function once(
    texts: readonly string[],
    kind: EmbedKind,
    signal: AbortSignal | undefined,
  ): Promise<number[][]> {
    const timeout = AbortSignal.timeout(timeoutMs);
    const stopped = (): unknown => {
      if (signal?.aborted) return signal.reason;
      if (timeout.aborted) return new EmbeddingError("voyage", "timeout");
      return undefined;
    };
    let response: Response;
    try {
      response = await send(`${baseUrl}/embeddings`, {
        method: "POST",
        headers: {
          Authorization: `Bearer ${config.apiKey}`,
          "Content-Type": "application/json",
        },
        body: JSON.stringify({
          model: config.model,
          input: texts,
          input_type: kind,
          output_dimension: config.dimensions,
        }),
        signal: signal ? AbortSignal.any([signal, timeout]) : timeout,
      });
    } catch {
      throw stopped() ?? new EmbeddingError("voyage", "unavailable");
    }
    if (!response.ok) {
      await response.body?.cancel().catch(() => {});
      const status = response.status;
      if (status === 429) {
        throw new EmbeddingError("voyage", "rate_limited", status, retryAfterMs(response));
      }
      if (status === 401 || status === 403) throw new EmbeddingError("voyage", "auth", status);
      if (status >= 500) throw new EmbeddingError("voyage", "unavailable", status);
      throw new EmbeddingError("voyage", "rejected", status);
    }
    let body: unknown;
    try {
      body = await response.json();
    } catch {
      throw stopped() ?? new EmbeddingError("voyage", "bad_response", response.status);
    }
    const parsed = responseSchema.safeParse(body);
    if (!parsed.success || parsed.data.data.length !== texts.length) {
      throw new EmbeddingError("voyage", "bad_response", response.status);
    }
    const vectors: number[][] = new Array(texts.length);
    for (const item of parsed.data.data) {
      if (item.embedding.length !== config.dimensions || item.index >= texts.length) {
        throw new EmbeddingError("voyage", "bad_response", response.status);
      }
      vectors[item.index] = item.embedding;
    }
    if (vectors.some((v) => !v)) throw new EmbeddingError("voyage", "bad_response");
    return vectors;
  }

  return Object.freeze({
    name: "voyage",
    model: config.model,
    dimensions: config.dimensions,
    locality: "cloud",
    maxBatch: 128,
    async embed(texts: readonly string[], kind: EmbedKind, callOptions: EmbedOptions = {}) {
      const { signal } = callOptions;
      signal?.throwIfAborted();
      if (texts.length === 0) return [];
      if (texts.length > 128) throw new Error("at most 128 texts per call");
      for (let attempt = 0; ; attempt++) {
        try {
          return await once(texts, kind, signal);
        } catch (err) {
          if (!(err instanceof EmbeddingError) || attempt >= maxRetries) throw err;
          if (!["rate_limited", "unavailable", "timeout"].includes(err.code)) throw err;
          const delay = err.retryAfterMs ?? retryBaseMs * 2 ** attempt;
          if (delay > maxRetryWaitMs) throw err;
          await wait(delay, signal);
        }
      }
    },
  });
}
