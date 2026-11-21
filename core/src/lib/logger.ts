import { type DestinationStream, type LevelWithSilent, type Logger, pino } from "pino";

export type { Logger };

export type LoggerOptions = Readonly<{
  level: LevelWithSilent;
  service: string;
  version: string;
}>;

const safeErrorFields = ["code", "errno", "syscall", "severity", "routine"] as const;

type SerializedError = {
  type: string;
  message: string;
  stack?: string;
  cause?: SerializedError;
} & {
  [K in (typeof safeErrorFields)[number]]?: string | number;
};

export function serializeError(value: unknown, depth = 0): SerializedError {
  if (!(value instanceof Error)) return { type: typeof value, message: String(value) };
  const out: SerializedError = { type: value.name, message: value.message, stack: value.stack };
  const record = value as unknown as Record<string, unknown>;
  for (const key of safeErrorFields) {
    const field = record[key];
    if (typeof field === "string" || typeof field === "number") out[key] = field;
  }
  if (value.cause !== undefined && depth < 3) out.cause = serializeError(value.cause, depth + 1);
  return out;
}

export function createLogger(options: LoggerOptions, destination?: DestinationStream): Logger {
  return pino(
    {
      level: options.level,
      base: { service: options.service, version: options.version },
      timestamp: pino.stdTimeFunctions.isoTime,
      formatters: { level: (label) => ({ level: label }) },
      serializers: { err: (err: unknown) => serializeError(err) },
    },
    destination ?? pino.destination({ dest: 1, sync: false }),
  );
}
