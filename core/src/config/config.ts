import { resolve } from "node:path";
import { z } from "zod";

const optional = (value: unknown) => (value === "" ? undefined : value);

const portRange = { error: "must be between 1 and 65535" };

const defaultLlmProvider = "groq";

const schema = z.object({
  LOG_LEVEL: z.preprocess(
    optional,
    z.enum(["trace", "debug", "info", "warn", "error", "fatal"]).default("info"),
  ),
  DATA_DIR: z.preprocess(optional, z.string().default("./data")),
  LISTEN_HOST: z.preprocess(optional, z.string().default("127.0.0.1")),
  CORE_PORT: z.preprocess(
    optional,
    z.coerce
      .number({ error: "must be a number" })
      .int({ error: "must be a whole number" })
      .min(1, portRange)
      .max(65535, portRange)
      .default(3000),
  ),
  DATABASE_URL: z.preprocess(
    optional,
    z.url({
      protocol: /^postgres(ql)?$/,
      error: (issue) => (issue.input === undefined ? "required" : "must be a postgres:// URL"),
    }),
  ),
  LLM_PROVIDER: z.preprocess(optional, z.enum(["groq", "stub"]).default(defaultLlmProvider)),
  GROQ_API_KEY: z.preprocess(optional, z.string().optional()),
  GROQ_MODEL_HEAVY: z.preprocess(optional, z.string().default("openai/gpt-oss-120b")),
  GROQ_MODEL_ROUTINE: z.preprocess(optional, z.string().default("openai/gpt-oss-20b")),
  CHAT_CHANNEL: z.preprocess(optional, z.enum(["stub"]).default("stub")),
  TELEPHONY_PROVIDER: z.preprocess(optional, z.enum(["stub"]).default("stub")),
});

type Env = z.infer<typeof schema>;

export type GroqConfig = Readonly<{ apiKey: string; heavyModel: string; routineModel: string }>;

export type LlmConfig = Readonly<{ provider: "groq"; groq: GroqConfig } | { provider: "stub" }>;

export type Config = Readonly<{
  logLevel: Env["LOG_LEVEL"];
  dataDir: string;
  listenHost: string;
  port: number;
  databaseUrl: string;
  llm: LlmConfig;
  channel: Readonly<{ provider: Env["CHAT_CHANNEL"] }>;
  telephony: Readonly<{ provider: Env["TELEPHONY_PROVIDER"] }>;
}>;

export class ConfigError extends Error {
  constructor(readonly problems: string[]) {
    super(
      `Config is invalid. Fix these in .env (see .env.example):\n${problems.map((p) => `  - ${p}`).join("\n")}`,
    );
    this.name = "ConfigError";
  }
}

export function loadConfig(
  env: NodeJS.ProcessEnv = process.env,
  baseDir: string = process.cwd(),
): Config {
  const result = schema.safeParse({
    ...env,
    DATABASE_URL: optional(env.CORE_DATABASE_URL) ?? env.DATABASE_URL,
  });
  const problems = result.success
    ? []
    : result.error.issues.map((issue) => `${issue.path.join(".")}: ${issue.message}`);
  const llmProvider = optional(env.LLM_PROVIDER) ?? defaultLlmProvider;
  if (llmProvider === "groq" && optional(env.GROQ_API_KEY) === undefined) {
    problems.push("GROQ_API_KEY: required");
  }
  if (!result.success || problems.length > 0) throw new ConfigError(problems);
  const e = result.data;
  return Object.freeze({
    logLevel: e.LOG_LEVEL,
    dataDir: resolve(baseDir, e.DATA_DIR),
    listenHost: e.LISTEN_HOST,
    port: e.CORE_PORT,
    databaseUrl: e.DATABASE_URL,
    llm: llmConfig(e),
    channel: Object.freeze({ provider: e.CHAT_CHANNEL }),
    telephony: Object.freeze({ provider: e.TELEPHONY_PROVIDER }),
  });
}

function llmConfig(e: Env): LlmConfig {
  if (e.LLM_PROVIDER === "stub") return Object.freeze({ provider: "stub" });
  return Object.freeze({
    provider: "groq",
    groq: Object.freeze({
      apiKey: e.GROQ_API_KEY ?? "",
      heavyModel: e.GROQ_MODEL_HEAVY,
      routineModel: e.GROQ_MODEL_ROUTINE,
    }),
  });
}
