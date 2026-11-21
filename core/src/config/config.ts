import { resolve } from "node:path";
import { z } from "zod";

const optional = (value: unknown) => (value === "" ? undefined : value);

const portRange = { error: "must be between 1 and 65535" };

const schema = z.object({
  LOG_LEVEL: z.preprocess(
    optional,
    z.enum(["trace", "debug", "info", "warn", "error", "fatal"]).default("info"),
  ),
  DATA_DIR: z.preprocess(optional, z.string().default("./data")),
  PORT: z.preprocess(
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
  LLM_PROVIDER: z.preprocess(optional, z.enum(["groq"]).default("groq")),
  GROQ_API_KEY: z.preprocess(optional, z.string({ error: "required" })),
  GROQ_MODEL_HEAVY: z.preprocess(optional, z.string().default("openai/gpt-oss-120b")),
  GROQ_MODEL_ROUTINE: z.preprocess(optional, z.string().default("openai/gpt-oss-20b")),
});

export type Config = Readonly<{
  logLevel: z.infer<typeof schema>["LOG_LEVEL"];
  dataDir: string;
  port: number;
  databaseUrl: string;
  llm: Readonly<{
    provider: z.infer<typeof schema>["LLM_PROVIDER"];
    groq: Readonly<{ apiKey: string; heavyModel: string; routineModel: string }>;
  }>;
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
  const result = schema.safeParse(env);
  if (!result.success) {
    throw new ConfigError(
      result.error.issues.map((issue) => `${issue.path.join(".")}: ${issue.message}`),
    );
  }
  const e = result.data;
  return Object.freeze({
    logLevel: e.LOG_LEVEL,
    dataDir: resolve(baseDir, e.DATA_DIR),
    port: e.PORT,
    databaseUrl: e.DATABASE_URL,
    llm: Object.freeze({
      provider: e.LLM_PROVIDER,
      groq: Object.freeze({
        apiKey: e.GROQ_API_KEY,
        heavyModel: e.GROQ_MODEL_HEAVY,
        routineModel: e.GROQ_MODEL_ROUTINE,
      }),
    }),
  });
}
