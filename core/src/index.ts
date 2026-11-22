import { dirname } from "node:path";
import { type Config, ConfigError, loadConfig, loadEnvFile } from "./config/index.js";
import { createContracts } from "./contracts/index.js";
import { createDb } from "./lib/db.js";
import { startHttpServer } from "./lib/http-server.js";
import { createLogger, type Logger } from "./lib/logger.js";
import { createEventConsumer, createGatewayEvents } from "./modules/gateway/index.js";
import { createHealthRoute } from "./modules/health/index.js";
import { createProviders } from "./providers/index.js";

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
  const stopping = stopSignal();
  const contracts = createContracts();
  const db = createDb(config.databaseUrl, logger);
  const gatewayEvents = createGatewayEvents(logger);
  const consumer = createEventConsumer({
    db,
    databaseUrl: config.databaseUrl,
    contracts,
    logger,
    handle: gatewayEvents.handle,
  });
  const providers = createProviders(config, {
    gateway: { pool: db.pool, contracts, results: gatewayEvents },
  });
  try {
    try {
      await db.check(AbortSignal.timeout(5_000));
    } catch (err) {
      logger.warn({ err }, "db not reachable at startup; health reports it until it is");
    }
    for (const [kind, provider] of Object.entries(providers)) {
      if (provider.name === "stub") {
        logger.warn({ kind }, `${kind} provider is the stub; nothing real happens through it`);
      }
    }
    await providers.channel.start();
    consumer.start();

    const server = await startHttpServer({
      host: config.listenHost,
      port: config.port,
      logger,
      routes: {
        "/health": createHealthRoute({
          service: "core",
          version,
          checks: { db, channel: providers.channel, events: consumer },
        }),
      },
    });
    try {
      logger.info(
        {
          addr: `${config.listenHost}:${server.port}`,
          llm: providers.llm.name,
          channel: providers.channel.name,
          telephony: providers.telephony.name,
          data_dir: config.dataDir,
        },
        "core started",
      );
      const signal = await stopping;
      logger.info({ signal }, "core stopping");
    } finally {
      await server.close();
    }
  } finally {
    await consumer.stop();
    await providers.channel.stop();
    await db.close();
  }
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
