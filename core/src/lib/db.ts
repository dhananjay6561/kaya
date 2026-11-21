import pg from "pg";
import type { Logger } from "./logger.js";

export type Db = Readonly<{
  pool: pg.Pool;
  check(signal?: AbortSignal): Promise<void>;
  close(): Promise<void>;
}>;

function aborted(signal: AbortSignal): Promise<never> {
  return new Promise((_, reject) => {
    if (signal.aborted) reject(signal.reason);
    signal.addEventListener("abort", () => reject(signal.reason), { once: true });
  });
}

export function createDb(url: string, logger: Logger): Db {
  const pool = new pg.Pool({
    connectionString: url,
    max: 10,
    idleTimeoutMillis: 5 * 60_000,
    connectionTimeoutMillis: 5_000,
    application_name: "kaya-core",
  });
  pool.on("error", (err) => logger.error({ err }, "idle db connection failed"));

  return Object.freeze({
    pool,
    async check(signal?: AbortSignal) {
      const query = pool.query("SELECT 1");
      if (!signal) {
        await query;
        return;
      }
      query.catch(() => {});
      await Promise.race([query, aborted(signal)]);
    },
    close: () => pool.end(),
  });
}
