# Kaya — architecture

How the code is organised and the standards every change follows. The reasons behind each choice
are in `DECISIONS.md`. The feature spec, the phase-by-phase build plan and the hosting notes are
kept outside this repository.

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

- **gateway → core:** events (`contracts/schemas/v1/event.schema.json`):
  - `connection.state`: connecting, pairing, connected, disconnected, logged out
  - `message.stored`: a message row was inserted or changed (new, edited or deleted). It carries
    row ids (`message_id`, `chat_id`, `sender_id`), never the body. The gateway emits it only when
    the upsert actually inserted or changed the row, so a redelivered or overlapping copy of a
    message emits nothing. Core keys command handling on `(message_id, change)`, never on the
    event id.
  - `history.synced`: one history-sync batch was stored
  - `send.result`: what happened to a send request
- **core → gateway:** send requests (`send-request.schema.json`): text to a person or a group
  (never Status, broadcast lists, channels or bots), optionally quoting a message. Each carries an
  `authorization` and an `expires_at`. The gateway enforces both before sending:
  - `self_chat` may only go to DJ's own JID.
  - `confirmed` needs a pending action (4.6) that exists, is confirmed and targets the same chat.
  - Nothing goes out after `expires_at`; the gateway reports `send.result` failed with `expired`.
  - `expires_at` must be after `created_at` and at most 24 h later. A schema can't compare two
    fields, so both validators check this on top of it, and the shared samples cover it.

  Media comes later as a new content type.
- **core → docs / voice:** HTTP requests.
- **Transport: Postgres.** Each direction is an outbox table in the shared database, used as a
  durable queue. The tables arrive with the first phase that writes to them: 1.2 for events, 1.6
  for send requests.
  - **Writing.** The writer inserts the validated payload as `jsonb` in the same transaction as
    the data it describes, then sends `NOTIFY` as a wake-up. Rows have a `bigint` identity `seq`
    and a `state`.
  - **Reading.** Each table has exactly one consumer: core for events, the gateway for send
    requests. It `LISTEN`s, and on start (or a missed notify) drains pending rows in `seq` order.
    With one ordered consumer, `connection.state` changes apply in the order they happened.
  - **At-least-once, made safe.** Delivery is at least once, so every effect must be idempotent.
    The reader records the event id as handled in the same transaction as the handler's database
    writes; a crash before that commit replays the event, a crash after it doesn't.
  - **Sends can't be made idempotent that way,** because the WhatsApp send happens outside the
    database. Send requests go `pending → sending → sent | failed`. The gateway generates the
    WhatsApp message id first (`GenerateMessageID`, passed as `SendRequestExtra.ID`) and commits it
    with `sending` before the network call. On restart it never resends a row left in `sending`;
    it reports `send.result` failed with `outcome_unknown` and lets DJ decide. A message is never
    sent twice and never silently lost.
  - **Poison rows.** A row that fails the reader's validation, or whose handler fails for a
    non-transient reason, moves to `dead` with an error code and is skipped, so draining never
    loops on it. Transient failures retry with backoff, without reordering later rows.
  - **Retention.** Handled rows are deleted after 7 days; `dead` rows stay until someone looks at
    them.
  - **Roles.** Each service has its own Postgres role (see "Data ownership"). The gateway can only
    insert into the events table and, from 1.6, update the send-requests table; core the reverse.
    Neither can write the other's queue.
- **Contracts:** every payload has a JSON Schema (draft 2020-12) in `contracts/schemas/v1/` and
  carries `"version": 1`. Both sides validate at the boundary: the writer before inserting, the
  reader before handling.
  - Formats are pinned with patterns, so ajv and the Go validator agree: lowercase UUIDs, and
    timestamps in UTC with an uppercase `T` and `Z`.
  - Go: hand-written types in `gateway/internal/contracts`. Every time field uses
    `contracts.Time`, which always encodes as UTC. Validation errors name the field and the rule,
    never the value, so they are safe to log.
  - TypeScript: types generated into `core/src/contracts/types.gen.ts` (`make contracts`).
  - Shared samples in `contracts/samples/v1/<schema>/{valid,invalid}/` are checked by both test
    suites. Every valid sample must also round-trip through the Go types unchanged, and core's
    tests fail if the generated types are out of date. The samples are a separate Go package that
    only tests import, so they never ship in the gateway binary.
  - A contract change is its own PR and updates the schema, the samples and both sides. A
    breaking change gets a new version folder.

## Data ownership

- One schema, in `db/migrations/`, as plain SQL run by dbmate before the services start. Neither
  language runs its own migration system.
- whatsmeow manages its own tables (the session, encryption keys and its event buffer) in the
  `whatsmeow` schema, which the gateway role owns. Nothing else touches them, and core cannot see
  inside the schema.
- `gateway` writes contacts, chats, messages and reactions. It resolves every person through
  `contact_id_for()`, never by inserting into `contacts` directly. `core` reads them and owns
  everything it creates (tasks, jobs, pending actions, usage).
- **Media.** A media message gets a `media` row (what the file is, and whether it is `remote`,
  `stored`, `expired` or `failed`) and, when it can be fetched, a `media_keys` row that only the
  gateway can read. Nothing is downloaded unless a `media_requests` row asks for it; the gateway
  then streams the file to `media/<sha256[0:2]>/<sha256>` under its data folder, with a size cap,
  an hourly cap and a free-space floor, and records the outcome in `media.state`. Readers find a
  stored file by the `sha256` in `media`. Paths are built from that hash only, never from a file
  name.
- **Roles.** Each service logs in as its own Postgres role, `kaya_gateway` or `kaya_core`, with
  exactly the grants it needs: the gateway writes the chat tables and can only insert into
  `gateway_events`; core reads the chat tables and consumes `gateway_events`. The roles and grants
  are in the migrations; the passwords come from `.env` and are set by the one-shot `db-roles`
  Compose step. The admin login (`POSTGRES_USER`) is used only by `migrate` and `db-roles`. A
  migration that adds a table also grants it, and `db/tests/schema.sql` asserts the whole matrix.
- **Live ingest.** The gateway acknowledges a WhatsApp message only after its transaction commits:
  contact, chat, message and the `message.stored` event together. While Postgres is away it
  retries for 30 s, then drops the connection without acknowledging and reconnects once the
  database answers, and WhatsApp delivers the message again. A message the database refuses (a
  constraint, a bug in the mapper) is copied to `ingest_dead`, which only the gateway can read, and
  then acknowledged, so one bad message never blocks the rest and can be replayed after a fix.
  Storing the same message twice changes nothing and emits nothing.
- **History sync.** Each history chunk WhatsApp announces becomes a row in `history_jobs`, stored
  in the transaction that acknowledges the announcement. A single worker downloads the chunk,
  stores it in batches of at most 200 messages (a savepoint per message, so one bad message
  becomes a dead letter and the rest go in), and confirms it to WhatsApp only after the job is
  marked `stored`. History rows have `source = 'history'` and emit no `message.stored`; core gets
  one `history.synced` per chunk and backfills by scanning. Core must therefore accept an `edited`
  or `deleted` event for a message it never saw as `new`. Live edits and revokes that arrive before
  their message are kept in `pending_changes` and applied after each job.

## Engineering standards

- **Types:** TypeScript `strict` with `noUncheckedIndexedAccess`; Go with `go vet` and golangci-lint.
- **Config:** only from environment variables, validated once at startup. A bad config stops the
  service with every problem listed. Secrets never appear in logs or errors.
- **Modules:** each feature lives in its own folder with a small public entry (`index.ts` or a Go
  package). Modules depend on interfaces, not on each other's internals. Providers are chosen by
  config.
- **Providers:** `core/src/providers/<kind>/` holds the interface (`types.ts`), one file per
  implementation, and a factory in `index.ts` that switches on the config value. The config enum
  lists only implementations that exist, and the switch is exhaustive, so adding a provider means
  adding a file, an enum value and a case; TypeScript fails the build if one is missed. Every kind
  has a `stub` implementation for development and tests. Kinds so far:
  - `llm`: `complete()` with a `routine` or `heavy` tier, returning text, model and token usage
  - `llm` returns a `finishReason` (`stop`, `length`, `tool_calls`, `content_filter`) with every
    completion.
  - `channel`: the one conversation between DJ and Kaya (`start`, `stop`, `send`, `onMessage`).
    Reading the rest of WhatsApp goes through the database, not the channel. `start()` never
    fails because a dependency is down; it connects in the background, and the channel is a
    `/health` check like the db. Every handler sees every message; failures are collected and
    reported together.
  - `telephony`: `placeCall()` to an E.164 number with a brief, and `getCall()` for status and
    transcript
  - Every outward action (`send`, `placeCall`) takes a required `idempotencyKey`. A repeated key
    returns the first result and never acts twice. Stubs never report that an outward action
    happened.
- **Metrics (from stage 15):** every service exposes `GET /metrics` in Prometheus format on its
  existing HTTP port. Names start with `kaya_` and carry the unit (`kaya_ingest_delay_seconds`).
  Labels hold only small fixed sets (service, kind, state, outcome): never a JID, a name, a chat id
  or message text, both for privacy and because every distinct label value is a new time series.
  Counters for what happened, gauges for what is waiting, histograms for how long things took.
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
  reports it through `/health` until it recovers. State that must not fail health goes under an
  optional `info` object, such as the gateway's `"info":{"whatsapp":{"state","reason"}}`, which uses
  the `connection.state` contract's states and fixed reason strings.
- **Errors:** typed errors at boundaries, never swallowed. Retries with backoff only for network
  calls.
- **Tests:** every phase ships tests for its logic. Anything touching the database is tested
  against a real Postgres. Tests use made-up data, never real chats.
- **Dependencies:** pinned exactly; updated through Dependabot PRs.
- **Containers:** multi-stage builds, non-root user, read-only filesystem where possible.
- **Commands:** `make lint`, `make test` and `make build` at the root cover every language.
