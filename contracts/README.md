# Contracts

JSON Schemas (draft 2020-12) for every message between Kaya's services, plus sample payloads that
every side's tests check. How they are carried and used: "How services talk" in
[docs/ARCHITECTURE.md](../docs/ARCHITECTURE.md).

```
schemas/v1/
├── defs.schema.json           shared pieces: uuid, timestamp, jid, message kind, error
├── event.schema.json          gateway → core
└── send-request.schema.json   core → gateway
samples/v1/<schema>/
├── valid/                     must pass on every side, and round-trip through the Go types
└── invalid/                   must fail on every side, each for one reason named in the file name
```

This folder is also a Go module (`contracts.go` embeds the schemas and samples) and an npm workspace
package (`@kaya/contracts`), so both services load the same files.

## Changing a contract

1. Edit the schema. Add a valid sample for anything new and an invalid sample for each new rule.
2. Update the Go types in `gateway/internal/contracts/types.go`.
3. Run `make contracts` to regenerate `core/src/contracts/types.gen.ts`.
4. Run `make check`. Both test suites run every sample.

Adding an optional field or a new event type is compatible. Removing or renaming a field, or
tightening a rule, is breaking: add `schemas/v2/` and handle both versions until every service has
moved.
