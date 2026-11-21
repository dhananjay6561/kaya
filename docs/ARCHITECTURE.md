# Kaya — architecture

How the code is organised and the standards every change follows. Deployment and hardware are in
`INFRA.md`; the reasons behind each choice are in `DECISIONS.md`.

## Services

| Service | Language | Owns |
|---|---|---|
| `gateway` | Go (whatsmeow) | The WhatsApp connection: pairing, session, receiving, sending, media. Knows nothing about the agent. |
| `core` | TypeScript (Node 24) | The agent: commands, LLM calls, tools, safety rules, confirmations, scheduler, browser control |
| `services/docs` (later) | Python | PPT and PDF generation, Drive upload |
| `services/voice` (later) | Python (Pipecat) | Live phone calls |
| `db` | Postgres + pgvector | All shared data |

Each service does one job in the language with the best libraries for it, and can be restarted,
updated or replaced on its own. Replacing WhatsApp's transport (for example with the official API)
only touches `gateway`.

## Repo layout

```
kaya/
├── core/                   TypeScript agent (npm workspace)
│   └── src/
│       ├── index.ts        entry point: wires modules together, nothing else
│       ├── config/         env loading and validation
│       ├── modules/        one folder per feature (commands, chats, tools, ...)
│       ├── providers/      swappable backends behind interfaces (llm/, channel/, telephony/)
│       └── lib/            small shared helpers
├── gateway/                Go WhatsApp gateway (its own Go module)
│   ├── cmd/gateway/        main package
│   └── internal/           config, whatsapp, store, events, httpapi
├── services/               Python services (docs, voice), added in their stages
├── contracts/              JSON Schemas for every message between services
├── db/migrations/          plain SQL migrations (dbmate)
├── deploy/                 Victus host setup, compose override, backups, hosting checklist
├── docs/                   spec, architecture, infra, decisions, phases
├── compose.yml             base stack; compose.mac.yml / deploy/compose.victus.yml per machine
├── Makefile                one entry point for dev, lint, test and build across languages
└── .env.example            every setting for every service
```

Folders appear when a phase needs them; empty placeholders are not committed.

## How services talk

- **gateway → core:** an event for each incoming or sent message, contact or group change, and
  connection state change.
- **core → gateway:** a send request (text, media, reply).
- **core → docs / voice:** HTTP requests.
- Every payload has a JSON Schema in `contracts/`. Both sides validate at the boundary and have
  matching types. A change to a contract is its own PR and updates both sides.
- The transport between gateway and core is chosen in phase 1.0 (see the open question in
  `DECISIONS.md`).

## Data ownership

- One schema, in `db/migrations/`, as plain SQL run by dbmate before the services start. Neither
  language runs its own migration system.
- whatsmeow manages its own session tables; nothing else touches them.
- `gateway` writes chats, contacts and messages. `core` reads them and owns everything it creates
  (tasks, jobs, pending actions, usage).

## Engineering standards

- **Types:** TypeScript `strict` with `noUncheckedIndexedAccess`; Go with `go vet` and golangci-lint.
- **Config:** only from environment variables, validated once at startup. A bad config stops the
  service with every problem listed. Secrets never appear in logs or errors.
- **Modules:** each feature lives in its own folder with a small public entry (`index.ts` or a Go
  package). Modules depend on interfaces, not on each other's internals. Providers are chosen by
  config.
- **Logging:** structured JSON to stdout, one logger per service (pino in core, `log/slog` in Go).
  Every line has `time`, `level`, `msg`, `service` and `version`. A request id travels in the
  `X-Request-Id` header and appears on every log line for that request. Errors are logged with an
  allowlist of fields, never whole objects, so attached clients or credentials can't leak.
- **Reliability:** every service has `/health`, shuts down cleanly on SIGTERM, and handles every
  event idempotently, so a retry or restart never duplicates work.
- **Health:** `GET /health` answers
  `{"status":"ok|unhealthy","service","version","checks":{"db":{"status":"ok|down","duration_ms"}}}`,
  with 200 when every check passes and 503 otherwise. Each check has a 2-second limit, and failure
  details go to the logs, not the response. A service starts even if a dependency is down and
  reports it through `/health` until it recovers.
- **Errors:** typed errors at boundaries, never swallowed. Retries with backoff only for network
  calls.
- **Tests:** every phase ships tests for its logic. Anything touching the database is tested
  against a real Postgres. Tests use made-up data, never real chats.
- **Dependencies:** pinned exactly; updated through Dependabot PRs.
- **Containers:** multi-stage builds, non-root user, read-only filesystem where possible.
- **Commands:** `make lint`, `make test` and `make build` at the root cover every language.
