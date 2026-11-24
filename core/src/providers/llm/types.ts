export type ChatRole = "system" | "user" | "assistant";

export type ChatMessage = Readonly<{ role: ChatRole; content: string }>;

export type ModelTier = "routine" | "heavy";

export type ReasoningEffort = "low" | "medium" | "high";

export type CompletionRequest = Readonly<{
  tier: ModelTier;
  effort?: ReasoningEffort;
  messages: readonly ChatMessage[];
  maxTokens?: number;
  temperature?: number;
}>;

export type Usage = Readonly<{ inputTokens: number; outputTokens: number }>;

export type FinishReason = "stop" | "length" | "tool_calls" | "content_filter";

export type Completion = Readonly<{
  text: string;
  model: string;
  finishReason: FinishReason;
  usage: Usage;
}>;

export type CallOptions = Readonly<{ signal?: AbortSignal }>;

export type Locality = "cloud" | "local";

export interface LlmProvider {
  readonly name: string;
  /** Where requests go: context for a cloud model leaves the machine. */
  readonly locality: Locality;
  /** How many tokens of context a request may carry. */
  readonly contextTokens: number;
  /** Tokens one message takes for this model, its chat framing included. */
  countTokens(text: string): number;
  complete(request: CompletionRequest, options?: CallOptions): Promise<Completion>;
}
