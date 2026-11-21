import { readdirSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

export const schemaBaseUrl = "https://kaya.invalid/contracts/v1/";

const packageRoot = new URL("./", import.meta.resolve("@kaya/contracts/package.json"));

export const schemasDir = fileURLToPath(new URL("schemas/v1/", packageRoot));

export const samplesDir = fileURLToPath(new URL("samples/v1/", packageRoot));

export type JsonSchema = Record<string, unknown> & { $id: string };

export function loadSchemas(): JsonSchema[] {
  return readdirSync(schemasDir)
    .filter((name) => name.endsWith(".schema.json"))
    .sort()
    .map((name) => JSON.parse(readFileSync(`${schemasDir}${name}`, "utf8")) as JsonSchema);
}

export function localPathFor(url: string): string | undefined {
  return url.startsWith(schemaBaseUrl)
    ? `${schemasDir}${url.slice(schemaBaseUrl.length).split("#")[0]}`
    : undefined;
}
