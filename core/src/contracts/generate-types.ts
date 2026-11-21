import { readFileSync, writeFileSync } from "node:fs";
import { compile } from "json-schema-to-typescript";
import { loadSchemas, localPathFor, schemaBaseUrl, schemasDir } from "./schemas.js";

export const generatedTypesPath = new URL("./types.gen.ts", import.meta.url);

const banner =
  "// Generated from contracts/schemas by core/src/contracts/generate-types.ts. Run `make contracts`; do not edit.";

export async function generateContractTypes(): Promise<string> {
  const roots = loadSchemas().filter((s) => !s.$id.endsWith("/defs.schema.json"));
  const all = {
    $id: `${schemaBaseUrl}all.schema.json`,
    title: "ContractPayload",
    description: "Any payload in the gateway ↔ core contract",
    oneOf: roots.map((s) => ({ $ref: s.$id })),
  };
  const output = await compile(all, all.title, {
    bannerComment: "",
    cwd: schemasDir,
    additionalProperties: false,
    unreachableDefinitions: false,
    style: { singleQuote: false, printWidth: 100, trailingComma: "all" },
    $refOptions: {
      resolve: {
        http: false,
        kaya: {
          order: 1,
          canRead: (file: { url: string }) => localPathFor(file.url) !== undefined,
          read: (file: { url: string }) => readFileSync(localPathFor(file.url) ?? "", "utf8"),
        },
      },
    },
  });
  return `${banner}\n\n${output.trim()}\n`;
}

if (import.meta.url === `file://${process.argv[1]}`) {
  writeFileSync(generatedTypesPath, await generateContractTypes());
  console.log(`wrote ${generatedTypesPath.pathname}`);
}
