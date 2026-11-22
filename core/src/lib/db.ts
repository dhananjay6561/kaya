import pg from "pg";
import type { Logger } from "./logger.js";

export type Db = Readonly<{
  pool: pg.Pool;
  check(signal?: AbortSignal): Promise<void>;
  close(): Promise<void>;
}>;

function untilAborted<T>(work: Promise<T>, signal: AbortSignal | undefined): Promise<T> {
  if (!signal) return work;
  return Promise.race([
    work,
    new Promise<never>((_, reject) => {
      if (signal.aborted) reject(signal.reason);
      signal.addEventListener("abort", () => reject(signal.reason), { once: true });
    }),
  ]);
}

export function createDb(url: string, logger: Logger): Db {
  const pool = new pg.Pool({
    connectionString: url,
    max: 10,
    idleTimeoutMillis: 5 * 60_000,
    connectionTimeoutMillis: 5_000,
    keepAlive: true,
    application_name: "kaya-core",
  });
  pool.on("error", (err) => logger.error({ err }, "idle db connection failed"));

  return Object.freeze({
    pool,
    async check(signal?: AbortSignal) {
      signal?.throwIfAborted();
      const connecting = pool.connect();
      let client: pg.PoolClient;
      try {
        client = await untilAborted(connecting, signal);
      } catch (err) {
        connecting.then(
          (late) => late.release(true),
          () => {},
        );
        throw err;
      }

      let released = false;
      const release = (destroy: boolean) => {
        if (released) return;
        released = true;
        client.release(destroy);
      };
      const onAbort = () => release(true);
      signal?.addEventListener("abort", onAbort, { once: true });
      try {
        const query = client.query("SELECT 1");
        query.catch(() => {});
        await untilAborted(query, signal);
        release(false);
      } catch (err) {
        release(true);
        throw err;
      } finally {
        signal?.removeEventListener("abort", onAbort);
      }
    },
    close: () => pool.end(),
  });
}

const connectionErrorClasses = /^(08|53|57)/;
const connectionErrorMessages =
  /connection terminated|connection error|timeout exceeded when trying to connect|pool .*ended/i;

export function isConnectionError(err: unknown): boolean {
  if (!(err instanceof Error)) return false;
  const code = (err as { code?: unknown }).code;
  if (typeof code === "string") {
    if (connectionErrorClasses.test(code) || code === "40001" || code === "40P01") return true;
    if (/^E[A-Z]+$/.test(code)) return true;
  }
  return connectionErrorMessages.test(err.message);
}
