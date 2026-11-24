import type pg from "pg";
import { type Connection, readConnection } from "../gateway/index.js";
import type { PauseState, PauseSwitch } from "../pause/index.js";

export type Status = Readonly<{
  version: string;
  startedAt: Date;
  now: Date;
  pause: PauseState;
  connection?: Connection;
  lastStoredAt?: Date;
  eventsWaiting: number;
  eventsDead: number;
}>;

export type StatusSource = Readonly<{
  pool: pg.Pool;
  pause: PauseSwitch;
  version: string;
  startedAt: Date;
}>;

export async function readStatus(source: StatusSource): Promise<Status> {
  const { pool } = source;
  const [pause, connection, last, events] = await Promise.all([
    source.pause.state(),
    readConnection(pool),
    pool.query<{ created_at: Date }>(
      "SELECT created_at FROM messages WHERE NOT from_me ORDER BY id DESC LIMIT 1",
    ),
    pool.query<{ waiting: number; dead: number }>(
      `SELECT (SELECT count(*)::int FROM gateway_events WHERE state = 'pending') AS waiting,
              (SELECT count(*)::int FROM gateway_events WHERE state = 'dead') AS dead`,
    ),
  ]);
  const lastStoredAt = last.rows[0]?.created_at;
  return Object.freeze({
    version: source.version,
    startedAt: source.startedAt,
    now: new Date(),
    pause,
    ...(connection ? { connection } : {}),
    ...(lastStoredAt ? { lastStoredAt } : {}),
    eventsWaiting: events.rows[0]?.waiting ?? 0,
    eventsDead: events.rows[0]?.dead ?? 0,
  });
}

const minute = 60_000;
const hour = 60 * minute;
const day = 24 * hour;

export function formatDuration(ms: number): string {
  if (ms < minute) return "under a minute";
  if (ms < hour) return `${Math.floor(ms / minute)}m`;
  if (ms < day) return `${Math.floor(ms / hour)}h ${Math.floor((ms % hour) / minute)}m`;
  return `${Math.floor(ms / day)}d ${Math.floor((ms % day) / hour)}h`;
}

export function formatStatus(status: Status): string {
  const since = (at: Date) => formatDuration(Math.max(0, status.now.getTime() - at.getTime()));
  const { connection } = status;
  const lines = [
    `Kaya ${status.version}, up ${since(status.startedAt)}.`,
    connection
      ? `WhatsApp: ${connection.state} for ${since(connection.occurredAt)}${connection.reason ? ` (${connection.reason})` : ""}.`
      : "WhatsApp: no report from the gateway yet.",
    status.pause.paused
      ? `Paused for ${since(status.pause.since)}. Send /resume to start again.`
      : "Answering messages.",
    status.lastStoredAt
      ? `Last incoming message stored ${since(status.lastStoredAt)} ago.`
      : "No incoming messages stored yet.",
  ];
  if (status.eventsWaiting > 0) lines.push(`Events waiting for core: ${status.eventsWaiting}.`);
  if (status.eventsDead > 0) lines.push(`Events core gave up on: ${status.eventsDead}.`);
  return lines.join("\n");
}
