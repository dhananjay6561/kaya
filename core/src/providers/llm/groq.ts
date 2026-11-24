import { z } from "zod";
import type { GroqConfig } from "../../config/index.js";
import { LlmError } from "./errors.js";
import type {
  CallOptions,
  Completion,
  CompletionRequest,
  FinishReason,
  LlmProvider,
  ModelTier,
} from "./types.js";

export type GroqOptions = Readonly<{
  fetch?: typeof fetch;
  baseUrl?: string;
  timeoutMs?: number;
  deadlineMs?: number;
  maxRetries?: number;
  retryBaseMs?: number;
  maxRetryWaitMs?: number;
}>;

const reasoningEffort: Record<ModelTier, "low" | "medium"> = { routine: "low", heavy: "medium" };
const defaultMaxTokens: Record<ModelTier, number> = { routine: 2_048, heavy: 8_192 };
const finishReasons = new Set<string>(["stop", "length", "tool_calls", "content_filter"]);

const responseSchema = z.object({
  model: z.string(),
  choices: z
    .array(
      z.object({
        message: z.object({ content: z.string().nullable().optional() }),
        finish_reason: z.string().nullable().optional(),
      }),
    )
    .min(1),
  usage: z.object({ prompt_tokens: z.number(), completion_tokens: z.number() }),
});

const errorSchema = z.object({ error: z.object({ code: z.string().nullish() }) });

const safeDetail = (code: unknown) =>
  typeof code === "string" && /^[a-z][a-z0-9_]{0,60}$/.test(code) ? code : undefined;

const tooLong = /context_length|too_large|too_long|max_tokens/;

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

export function createGroqLlm(config: GroqConfig, options: GroqOptions = {}): LlmProvider {
  const send = options.fetch ?? fetch;
  const baseUrl = (options.baseUrl ?? "https://api.groq.com/openai/v1").replace(/\/+$/, "");
  const timeoutMs = options.timeoutMs ?? 30_000;
  const deadlineMs = options.deadlineMs ?? 45_000;
  const maxRetries = options.maxRetries ?? 2;
  const retryBaseMs = options.retryBaseMs ?? 1_000;
  const maxRetryWaitMs = options.maxRetryWaitMs ?? 10_000;
  const models: Record<ModelTier, string> = {
    routine: config.routineModel,
    heavy: config.heavyModel,
  };

  async function once(
    body: string,
    signal: AbortSignal | undefined,
    budgetMs: number,
  ): Promise<Completion> {
    const timeout = AbortSignal.timeout(Math.min(timeoutMs, budgetMs));
    const stopped = (): unknown => {
      if (signal?.aborted) return signal.reason;
      if (timeout.aborted) return new LlmError("groq", "timeout");
      return undefined;
    };
    const readJson = async (response: Response) => {
      try {
        return await response.json();
      } catch {
        const reason = stopped();
        if (reason !== undefined) throw reason;
        return undefined;
      }
    };
    let response: Response;
    try {
      response = await send(`${baseUrl}/chat/completions`, {
        method: "POST",
        headers: {
          Authorization: `Bearer ${config.apiKey}`,
          "Content-Type": "application/json",
        },
        body,
        signal: signal ? AbortSignal.any([signal, timeout]) : timeout,
      });
    } catch (err) {
      const reason = stopped();
      if (reason !== undefined) throw reason;
      const cause = (err as { cause?: { code?: unknown } } | null)?.cause?.code;
      const detail = safeDetail(typeof cause === "string" ? cause.toLowerCase() : undefined);
      throw new LlmError("groq", "unavailable", detail ? { detail } : {});
    }

    if (!response.ok) {
      const detail = safeDetail(errorSchema.safeParse(await readJson(response)).data?.error.code);
      const status = response.status;
      const info = { status, ...(detail ? { detail } : {}) };
      if (status === 429) {
        const after = retryAfterMs(response);
        throw new LlmError("groq", "rate_limited", {
          ...info,
          ...(after === undefined ? {} : { retryAfterMs: after }),
        });
      }
      if (status === 401 || status === 403) throw new LlmError("groq", "auth", info);
      if (status >= 500) throw new LlmError("groq", "unavailable", info);
      if (status === 413 || (detail && tooLong.test(detail))) {
        throw new LlmError("groq", "too_long", info);
      }
      if (status === 404 || detail === "model_not_found" || detail === "model_decommissioned") {
        throw new LlmError("groq", "misconfigured", info);
      }
      throw new LlmError("groq", "rejected", info);
    }

    const parsed = responseSchema.safeParse(await readJson(response));
    if (!parsed.success) throw new LlmError("groq", "bad_response", { status: response.status });
    const { choices, usage, model } = parsed.data;
    const choice = choices[0];
    const reason = choice?.finish_reason ?? "stop";
    return Object.freeze({
      text: choice?.message.content ?? "",
      model,
      finishReason: (finishReasons.has(reason) ? reason : "stop") as FinishReason,
      usage: Object.freeze({
        inputTokens: usage.prompt_tokens,
        outputTokens: usage.completion_tokens,
      }),
    });
  }

  return Object.freeze({
    name: "groq",
    async complete(request: CompletionRequest, callOptions: CallOptions = {}): Promise<Completion> {
      const { signal } = callOptions;
      signal?.throwIfAborted();
      if (request.messages.length === 0) throw new Error("completion needs at least one message");
      const body = JSON.stringify({
        model: models[request.tier],
        messages: request.messages,
        max_completion_tokens: request.maxTokens ?? defaultMaxTokens[request.tier],
        ...(request.temperature === undefined ? {} : { temperature: request.temperature }),
        reasoning_effort: reasoningEffort[request.tier],
        include_reasoning: false,
      });

      const deadline = Date.now() + deadlineMs;
      for (let attempt = 0; ; attempt++) {
        try {
          return await once(body, signal, deadline - Date.now());
        } catch (err) {
          if (!(err instanceof LlmError) || attempt >= maxRetries) throw err;
          if (!["rate_limited", "unavailable", "timeout"].includes(err.code)) throw err;
          const delay = err.retryAfterMs ?? retryBaseMs * 2 ** attempt;
          if (delay > maxRetryWaitMs || Date.now() + delay >= deadline) throw err;
          await wait(delay, signal);
        }
      }
    },
  });
}
