import type pg from "pg";

export type PauseState = Readonly<{ paused: false } | { paused: true; since: Date }>;

export type PauseSwitch = Readonly<{
  state(): Promise<PauseState>;
  /** `by` names the command; asking again with the same one gives the same answer. */
  pause(by: string): Promise<"paused" | "already_paused">;
  resume(by: string): Promise<"resumed" | "not_paused">;
}>;

export class PauseStateMissingError extends Error {
  constructor() {
    super("the pause_state row is missing; the migrations have not run");
    this.name = "PauseStateMissingError";
  }
}

function only<T>(rows: T[]): T {
  const row = rows[0];
  if (!row) throw new PauseStateMissingError();
  return row;
}

export function createPause(pool: pg.Pool): PauseSwitch {
  return Object.freeze({
    async state(): Promise<PauseState> {
      const { rows } = await pool.query<{ paused_at: Date | null }>(
        "SELECT paused_at FROM pause_state",
      );
      const pausedAt = only(rows).paused_at;
      return pausedAt ? { paused: true, since: pausedAt } : { paused: false };
    },
    async pause(by: string) {
      const { rows } = await pool.query<{ this_one: boolean }>(
        `UPDATE pause_state
         SET paused_at = coalesce(paused_at, now()),
             changed_by = CASE WHEN paused_at IS NULL THEN $1 ELSE changed_by END,
             changed_at = CASE WHEN paused_at IS NULL THEN now() ELSE changed_at END
         RETURNING changed_by IS NOT DISTINCT FROM $1 AS this_one`,
        [by],
      );
      return only(rows).this_one ? "paused" : "already_paused";
    },
    async resume(by: string) {
      const { rows } = await pool.query<{ this_one: boolean }>(
        `UPDATE pause_state
         SET paused_at = NULL,
             changed_by = CASE WHEN paused_at IS NOT NULL THEN $1 ELSE changed_by END,
             changed_at = CASE WHEN paused_at IS NOT NULL THEN now() ELSE changed_at END
         RETURNING changed_by IS NOT DISTINCT FROM $1 AS this_one`,
        [by],
      );
      return only(rows).this_one ? "resumed" : "not_paused";
    },
  });
}

export function createMemoryPause(): PauseSwitch {
  let pausedAt: Date | undefined;
  let changedBy: string | undefined;
  return Object.freeze({
    async state(): Promise<PauseState> {
      return pausedAt ? { paused: true, since: pausedAt } : { paused: false };
    },
    async pause(by: string) {
      if (!pausedAt) {
        pausedAt = new Date();
        changedBy = by;
      }
      return changedBy === by ? "paused" : "already_paused";
    },
    async resume(by: string) {
      if (pausedAt) {
        pausedAt = undefined;
        changedBy = by;
      }
      return changedBy === by ? "resumed" : "not_paused";
    },
  });
}
