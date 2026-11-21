import { type Route, sendJson } from "../../lib/http-server.js";

export type Checker = Readonly<{ check(signal: AbortSignal): Promise<void> }>;

export type HealthOptions = Readonly<{
  service: string;
  version: string;
  checks: Readonly<Record<string, Checker>>;
  timeoutMs?: number;
}>;

type CheckResult = { status: "ok" | "down"; duration_ms: number };

export function createHealthRoute(options: HealthOptions): Route {
  const timeoutMs = options.timeoutMs ?? 2_000;

  return async (req, res, { logger }) => {
    if (req.method !== "GET" && req.method !== "HEAD") {
      res.writeHead(405, { allow: "GET, HEAD" }).end();
      return;
    }

    const signal = AbortSignal.timeout(timeoutMs);
    const entries = await Promise.all(
      Object.entries(options.checks).map(
        async ([name, checker]): Promise<[string, CheckResult]> => {
          const started = performance.now();
          try {
            await checker.check(signal);
            return [name, { status: "ok", duration_ms: Math.round(performance.now() - started) }];
          } catch (err) {
            logger.warn({ err, check: name }, "health check failed");
            return [name, { status: "down", duration_ms: Math.round(performance.now() - started) }];
          }
        },
      ),
    );

    const healthy = entries.every(([, result]) => result.status === "ok");
    sendJson(res, healthy ? 200 : 503, {
      status: healthy ? "ok" : "unhealthy",
      service: options.service,
      version: options.version,
      checks: Object.fromEntries(entries),
    });
  };
}

export async function checkHealth(url: string, timeoutMs = 3_000): Promise<void> {
  const res = await fetch(url, { signal: AbortSignal.timeout(timeoutMs) });
  await res.body?.cancel();
  if (!res.ok) throw new Error(`${url} answered ${res.status}`);
}
