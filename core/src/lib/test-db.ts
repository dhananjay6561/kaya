import { randomBytes } from "node:crypto";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import pg from "pg";

const migrationsDir = join(import.meta.dirname, "..", "..", "..", "db", "migrations");
const migrationLock = 5_011_601;

export type TestDb = Readonly<{
  admin: pg.Pool;
  url(role?: string): string;
  drop(): Promise<void>;
}>;

export const testDatabaseUrl = process.env.TEST_DATABASE_URL;

export async function createTestDb(baseUrl: string): Promise<TestDb> {
  const name = `kaya_test_${randomBytes(6).toString("hex")}`;
  const root = new pg.Client({ connectionString: baseUrl });
  await root.connect();
  await root.query(`CREATE DATABASE ${name}`);

  const target = new URL(baseUrl);
  target.pathname = `/${name}`;
  const admin = new pg.Pool({ connectionString: target.href, max: 2 });
  admin.on("error", () => {});
  await root.query("SELECT pg_advisory_lock($1)", [migrationLock]);
  try {
    for (const file of readdirSync(migrationsDir).sort()) {
      if (!file.endsWith(".sql")) continue;
      const [up] = readFileSync(join(migrationsDir, file), "utf8").split("-- migrate:down");
      await admin.query(up ?? "");
    }
  } finally {
    await root.query("SELECT pg_advisory_unlock($1)", [migrationLock]);
  }

  return Object.freeze({
    admin,
    url(role?: string) {
      const url = new URL(target.href);
      if (role) url.searchParams.set("options", `-c role=${role}`);
      return url.href;
    },
    async drop() {
      await admin.end();
      // A connection that is still closing would be cut off by the forced drop and report that
      // as an error nobody is listening for, so give every connection time to finish first.
      const deadline = Date.now() + 3_000;
      while (Date.now() < deadline) {
        const { rows } = await root.query<{ open: number }>(
          "SELECT count(*)::int AS open FROM pg_stat_activity WHERE datname = $1",
          [name],
        );
        if (rows[0]?.open === 0) break;
        await new Promise((resolve) => setTimeout(resolve, 10));
      }
      await root.query(`DROP DATABASE ${name} WITH (FORCE)`);
      await root.end();
    },
  });
}

export async function eventually<T>(
  what: string,
  probe: () => Promise<T | undefined | false>,
  timeoutMs = 5_000,
): Promise<T> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const value = await probe();
    if (value !== undefined && value !== false) return value;
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
}
