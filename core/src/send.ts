import { randomUUID } from "node:crypto";
import { dirname } from "node:path";
import { loadConfig, loadEnvFile } from "./config/index.js";
import { createContracts } from "./contracts/index.js";
import { createDb } from "./lib/db.js";
import { createLogger } from "./lib/logger.js";
import { createGatewayChannel } from "./providers/channel/index.js";

async function main(): Promise<void> {
  const text = (process.env.KAYA_TEXT ?? process.argv.slice(2).join(" ")).trim();
  if (text === "") throw new Error('usage: make send text="hello"');
  const envFile = loadEnvFile();
  const config = loadConfig(process.env, envFile ? dirname(envFile) : process.cwd());
  const db = createDb(
    config.databaseUrl,
    createLogger({ level: "silent", service: "core", version: "send" }),
  );
  try {
    const channel = createGatewayChannel({
      pool: db.pool,
      contracts: createContracts(),
      pollMs: 500,
      ttlMs: 60_000,
    });
    await channel.start();
    const sent = await channel.send({ idempotencyKey: randomUUID(), text });
    console.log(`sent to your own chat at ${sent.sentAt.toISOString()} (message ${sent.id})`);
  } finally {
    await db.close();
  }
}

main().catch((err: unknown) => {
  console.error(err instanceof Error ? err.message : String(err));
  process.exitCode = 1;
});
