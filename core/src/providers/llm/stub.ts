import type { CallOptions, Completion, CompletionRequest, LlmProvider } from "./types.js";

export type StubLlm = LlmProvider & Readonly<{ requests: readonly CompletionRequest[] }>;

export type StubLlmOptions = Readonly<{ reply?: (request: CompletionRequest) => string }>;

const roughTokens = (text: string) => Math.ceil(text.length / 4);

function defaultReply(request: CompletionRequest): string {
  const lastUser = request.messages.findLast((m) => m.role === "user");
  return `stub reply: ${lastUser?.content ?? ""}`;
}

export function createStubLlm(options: StubLlmOptions = {}): StubLlm {
  const requests: CompletionRequest[] = [];
  const reply = options.reply ?? defaultReply;

  return Object.freeze({
    name: "stub",
    requests,
    async complete(request: CompletionRequest, callOptions: CallOptions = {}): Promise<Completion> {
      callOptions.signal?.throwIfAborted();
      if (request.messages.length === 0) throw new Error("completion needs at least one message");
      requests.push(request);
      const text = reply(request);
      const input = request.messages.reduce((sum, m) => sum + roughTokens(m.content), 0);
      return {
        text,
        model: `stub-${request.tier}`,
        usage: { inputTokens: input, outputTokens: roughTokens(text) },
      };
    },
  });
}
