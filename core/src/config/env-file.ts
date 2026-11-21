import { existsSync } from "node:fs";
import { dirname, join, resolve } from "node:path";

export function findEnvFile(startDir: string = process.cwd()): string | undefined {
  let dir = resolve(startDir);
  for (;;) {
    const candidate = join(dir, ".env");
    if (existsSync(candidate)) return candidate;
    if (existsSync(join(dir, ".git"))) return undefined;
    const parent = dirname(dir);
    if (parent === dir) return undefined;
    dir = parent;
  }
}

export function loadEnvFile(env: NodeJS.ProcessEnv = process.env): string | undefined {
  const path = env.KAYA_ENV_FILE ? resolve(env.KAYA_ENV_FILE) : findEnvFile();
  if (path) process.loadEnvFile(path);
  return path;
}
