import { dirname } from "node:path";
import { type Config, ConfigError, loadConfig, loadEnvFile } from "./config/index.js";
import { createDb } from "./lib/db.js";
import { startHttpServer } from "./lib/http-server.js";
import { createLogger, type Logger } from "./lib/logger.js";
import { createHealthRoute } from "./modules/health/index.js";

const version = process.env.KAYA_VERSION || process.env.npm_package_version || "dev";

function stopSignal(): Promise<NodeJS.Signals> {
  return new Promise((resolve) => {
    for (const signal of ["SIGINT", "SIGTERM"] as const) process.once(signal, resolve);
  });
}

function exitOnCrash(logger: Logger): void {
  const crash = (err: unknown, kind: string) => {
    logger.fatal({ err }, kind);
    logger.flush();
    process.exit(1);
  };
  process.on("uncaughtException", (err) => crash(err, "uncaught exception"));
  process.on("unhandledRejection", (err) => crash(err, "unhandled rejection"));
}

async function run(config: Config, logger: Logger): Promise<void> {
  const db = createDb(config.databaseUrl, logger);
  try {
    await db.check(AbortSignal.timeout(5_000));
  } catch (err) {
    logger.warn({ err }, "db not reachable at startup; health reports it until it is");
  }

  const server = await startHttpServer({
    host: config.listenHost,
    port: config.port,
    logger,
    routes: { "/health": createHealthRoute({ service: "core", version, checks: { db } }) },
  });
  logger.info(
    {
      addr: `${config.listenHost}:${server.port}`,
      llm: config.llm.provider,
      data_dir: config.dataDir,
    },
    "core started",
  );

  const signal = await stopSignal();
  logger.info({ signal }, "core stopping");
  await server.close();
  await db.close();
  logger.info("core stopped");
}

function loadOrExit(): Config | undefined {
  try {
    const envFile = loadEnvFile();
    return loadConfig(process.env, envFile ? dirname(envFile) : process.cwd());
  } catch (error) {
    if (!(error instanceof ConfigError)) throw error;
    console.error(error.message);
    process.exitCode = 1;
    return undefined;
  }
}

const config = loadOrExit();
if (config) {
  const logger = createLogger({ level: config.logLevel, service: "core", version });
  exitOnCrash(logger);
  run(config, logger).catch((err: unknown) => {
    logger.fatal({ err }, "core failed");
    logger.flush();
    process.exitCode = 1;
  });
}
