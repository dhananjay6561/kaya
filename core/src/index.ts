import { dirname } from "node:path";
import { ConfigError, loadConfig, loadEnvFile } from "./config/index.js";

const version = process.env.npm_package_version ?? "dev";

try {
  const envFile = loadEnvFile();
  const config = loadConfig(process.env, envFile ? dirname(envFile) : process.cwd());
  console.log(
    `kaya ${version} starting (llm: ${config.llm.provider}, data: ${config.dataDir}, port: ${config.port})`,
  );
} catch (error) {
  if (!(error instanceof ConfigError)) throw error;
  console.error(error.message);
  process.exitCode = 1;
}
