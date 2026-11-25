# Contracts

JSON Schemas (draft 2020-12) for every message between Kaya's services, plus sample payloads that
every side's tests check. How they are carried and used: "How services talk" in
[docs/ARCHITECTURE.md](../docs/ARCHITECTURE.md).

```
schemas/v1/
├── defs.schema.json           shared pieces: uuid, timestamp, jid, message kind, error
├── event.schema.json          gateway → core
└── send-request.schema.json   core → gateway
samples/
├── samples.go                 Go package embedding v1/; only tests import it
└── v1/<schema>/
    ├── valid/                 must pass, and round-trip through the Go types
    └── invalid/               must fail, each for one reason named in the file name
```

This folder is a Go module: `contracts.go` embeds the schemas, and `types.go` and `validate.go`
hold the Go types and validator both services use, so the gateway and core load the same files.

## Changing a contract

1. Edit the schema. Add a valid sample for anything new and an invalid sample for each new rule.
2. Update the Go types in `contracts/types.go`.
3. Run `make check`. The contract tests run every sample, and both services build against the
   change.

Adding an optional field or a new event type is compatible. Removing or renaming a field, or
tightening a rule, is breaking: add `schemas/v2/` and handle both versions until every service has
moved.
