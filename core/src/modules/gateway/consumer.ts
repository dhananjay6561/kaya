import pg from "pg";
import { ContractError, type Contracts, type GatewayEvent } from "../../contracts/index.js";
import { type Db, isConnectionError } from "../../lib/db.js";
import type { Logger } from "../../lib/logger.js";
import type { Checker } from "../health/index.js";

export type AfterCommit = () => void;

export type EventHandler = (
  event: GatewayEvent,
  tx: pg.ClientBase,
) => Promise<AfterCommit | undefined>;

export type EventConsumerOptions = Readonly<{
  db: Db;
  databaseUrl: string;
  contracts: Contracts;
  logger: Logger;
  handle: EventHandler;
  pollMs?: number;
  retryMs?: number;
  maxRetryMs?: number;
  retentionMs?: number;
  purgeEveryMs?: number;
}>;

export type EventConsumer = Checker &
  Readonly<{
    start(): void;
    stop(): Promise<void>;
  }>;

export class ConsumerNotRunningError extends Error {
  constructor() {
    super("the gateway event consumer is not running");
    this.name = "ConsumerNotRunningError";
  }
}

const channel = "gateway_events";

export function createEventConsumer(options: EventConsumerOptions): EventConsumer {
  const { db, contracts, logger, handle } = options;
  const pollMs = options.pollMs ?? 30_000;
  const retryMs = options.retryMs ?? 1_000;
  const maxRetryMs = options.maxRetryMs ?? 30_000;
  const retentionMs = options.retentionMs ?? 7 * 24 * 60 * 60_000;
  const purgeEveryMs = options.purgeEveryMs ?? 60 * 60_000;

  let running = false;
  let loops: Promise<unknown> | undefined;
  let woken = false;
  let wakeUp: (() => void) | undefined;
  let halt: (() => void) | undefined;
  let nextPurge = 0;

  const wake = () => {
    woken = true;
    wakeUp?.();
  };

  const pause = (ms: number) =>
    new Promise<void>((resolve) => {
      if (woken || !running) {
        woken = false;
        resolve();
        return;
      }
      const timer = setTimeout(done, ms);
      function done() {
        clearTimeout(timer);
        wakeUp = undefined;
        woken = false;
        resolve();
      }
      wakeUp = done;
    });

  async function step(): Promise<boolean> {
    const client = await db.pool.connect();
    let broken = false;
    const lost = () => {
      broken = true;
    };
    client.on("error", lost);
    try {
      await client.query("BEGIN");
      const { rows } = await client.query<{ seq: string; payload: unknown }>(
        "SELECT seq, payload FROM gateway_events WHERE state = 'pending' ORDER BY seq LIMIT 1 FOR UPDATE",
      );
      const row = rows[0];
      if (!row) {
        await client.query("COMMIT");
        return false;
      }

      let event: GatewayEvent | undefined;
      let dead: string | undefined;
      let after: AfterCommit | undefined;
      try {
        event = contracts.parseEvent(row.payload);
      } catch (err) {
        if (!(err instanceof ContractError)) throw err;
        dead = "invalid_event";
        logger.error(
          { seq: row.seq, problems: err.problems },
          "a gateway event does not fit the contract",
        );
      }
      if (event) {
        await client.query("SAVEPOINT handler");
        try {
          after = await handle(event, client);
          await client.query("RELEASE SAVEPOINT handler");
        } catch (err) {
          if (isConnectionError(err)) throw err;
          await client.query("ROLLBACK TO SAVEPOINT handler");
          dead = "handler_failed";
          after = undefined;
          logger.error(
            { err, seq: row.seq, event_id: event.id, type: event.type },
            "a gateway event could not be handled and was set aside",
          );
        }
      }

      if (dead) {
        await client.query(
          "UPDATE gateway_events SET state = 'dead', error = $2, finished_at = now() WHERE seq = $1",
          [row.seq, dead],
        );
      } else {
        await client.query(
          "UPDATE gateway_events SET state = 'handled', finished_at = now() WHERE seq = $1",
          [row.seq],
        );
      }
      await client.query("COMMIT");
      try {
        after?.();
      } catch (err) {
        logger.error({ err, seq: row.seq }, "a follow-up to a handled gateway event failed");
      }
      return true;
    } catch (err) {
      broken = true;
      await client.query("ROLLBACK").catch(() => {});
      throw err;
    } finally {
      client.off("error", lost);
      client.release(broken);
    }
  }

  async function purge(): Promise<void> {
    if (Date.now() < nextPurge) return;
    nextPurge = Date.now() + purgeEveryMs;
    const { rowCount } = await db.pool.query(
      "DELETE FROM gateway_events WHERE state = 'handled' AND finished_at < now() - make_interval(secs => $1)",
      [retentionMs / 1_000],
    );
    if (rowCount) logger.info({ events: rowCount }, "old handled gateway events deleted");
  }

  async function drain(): Promise<void> {
    let delay = retryMs;
    let failing = false;
    while (running) {
      try {
        const handled = await step();
        if (failing) logger.info("gateway events are being handled again");
        failing = false;
        delay = retryMs;
        if (handled) continue;
        await purge();
        await pause(pollMs);
      } catch (err) {
        if (!running) return;
        if (!failing) logger.warn({ err }, "could not handle gateway events, trying again");
        failing = true;
        await pause(delay);
        delay = Math.min(delay * 2, maxRetryMs);
      }
    }
  }

  async function listen(): Promise<void> {
    while (running) {
      const client = new pg.Client({
        connectionString: options.databaseUrl,
        application_name: "kaya-core-listen",
        keepAlive: true,
        connectionTimeoutMillis: 5_000,
      });
      const closed = new Promise<void>((resolve) => {
        client.on("error", () => resolve());
        client.once("end", () => resolve());
        halt = resolve;
      });
      const listening = async () => {
        await client.connect();
        await client.query(`LISTEN ${channel}`);
        wake();
      };
      try {
        client.on("notification", wake);
        await Promise.race([listening(), closed]);
        await closed;
      } catch (err) {
        if (running) logger.debug({ err }, "the gateway event listener lost its connection");
      } finally {
        halt = undefined;
        client.end().catch(() => {});
      }
      if (running) {
        await new Promise<void>((resolve) => {
          const timer = setTimeout(resolve, retryMs);
          halt = () => {
            clearTimeout(timer);
            resolve();
          };
        });
        halt = undefined;
      }
    }
  }

  return Object.freeze({
    start() {
      if (running) return;
      running = true;
      loops = Promise.all([drain(), listen()]);
    },
    async stop() {
      if (!running) return;
      running = false;
      wake();
      halt?.();
      await loops;
    },
    async check(signal: AbortSignal) {
      signal.throwIfAborted();
      if (!running) throw new ConsumerNotRunningError();
    },
  });
}
