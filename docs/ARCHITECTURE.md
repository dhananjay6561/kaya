# Kaya — architecture

How the code is organised and the standards every change follows. The reasons behind each choice
are in `DECISIONS.md`. The feature spec, the phase-by-phase build plan and the hosting notes are
kept outside this repository.

## Services

| Service | Language | Owns |
|---|---|---|
| `gateway` | Go (whatsmeow) | The WhatsApp connection: pairing, session, receiving, sending, media. Knows nothing about the agent. |
| `core` | Go (decision 72) | The agent: commands, LLM calls, tools, safety rules, confirmations, scheduler, browser control |
| `services/docs` (later) | Python | PPT and PDF generation, Drive upload |
| `services/voice` (later) | Python (Pipecat) | Live phone calls |
| `db` | Postgres + pgvector | All shared data |

Each service does one job in the language with the best libraries for it, and can be restarted,
updated or replaced on its own. Replacing WhatsApp's transport (for example with the official API)
only touches `gateway`.

## Repo layout

```
kaya/
├── core/                   Go agent (its own Go module)
│   ├── cmd/core/           main package: wires the packages together; serve, send, healthcheck
│   └── internal/           one package per feature, with its tests:
│       ├── config/         env loading and validation
│       ├── gateway/        the event consumer (gateway_events)
│       ├── channel/        DJ's own chat: the command inbox and sends through send_requests
│       ├── commands/       slash commands and the rules for late and failed commands
│       ├── pause/, status/ /pause and /resume, and /status
│       ├── llm/            the Provider interface, Groq, the stub; o200k/ counts gpt-oss tokens
│       ├── memory/         the conversation (exchanges) and the context package
│       ├── chat/           system prompt, replies, WhatsApp formatting
│       ├── embeddings/     the embeddings Provider interface, Voyage, the stub
│       ├── search/         the index (chunks and the indexer that keeps them embedded) and search:
│       │                   meaning and words fused, filters, recall of older exchanges
│       ├── facts/          facts about DJ: the store, and the learner that reads each exchange
│       ├── masking/        placeholders for private details before a cloud model
│       └── text/           text rules kept from the TypeScript core
├── gateway/                Go WhatsApp gateway (its own Go module)
│   ├── cmd/gateway/        main package
│   └── internal/           config, whatsapp, ingest, events, send, media, testdb
├── kit/                    Go code both services share (its own module): logging, httpapi (the
│                           /health server), envfile, store (the Postgres pool), testdb
├── services/               Python services (docs, voice), added in their stages
├── contracts/              JSON Schemas for every message between services, and the Go types and
│                           validator for them (its own module)
├── db/migrations/          plain SQL migrations (dbmate)
├── deploy/                 Victus host setup, compose override, backups, hosting checklist
├── docs/                   spec, architecture, infra, decisions, phases
├── scripts/                host checks run through make (check-network: link MTU)
├── compose.yml             base stack; compose.mac.yml / deploy/compose.victus.yml per machine
├── Makefile                one entry point for dev, lint, test and build across languages
└── .env.example            every setting for every service
```

Folders appear when a phase needs them; empty placeholders are not committed.

## How services talk

- **gateway → core:** events (`contracts/schemas/v1/event.schema.json`):
  - `connection.state`: connecting, pairing, connected, disconnected, logged out. The gateway
    writes one each time the state or its reason changes, stamped with when it changed. Changes
    made while the database is away are kept in memory and written in order once it answers.
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
    and a `state`. A transaction that adds an event holds one advisory lock from that point until
    it ends, so `seq` order is commit order; without it an event with a lower `seq` could become
    visible after one with a higher `seq` had already been read.
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
    - A send error counts as a plain failure only when it is certain WhatsApp did not take the
      message (it refused it, or the recipient is not on WhatsApp). A timeout, a dropped
      connection or anything unrecognised is `outcome_unknown`, because the message may have gone
      out.
    - Once the outcome is known the gateway keeps trying to record it until the database takes it.
      The row state, the `send.result` event and the stored copy of the message commit together.
    - WhatsApp does not send a device its own messages back, so the gateway stores what it sent in
      `messages` itself. `send_requests.wa_id` tells a message Kaya sent from one DJ typed.
    - While WhatsApp is disconnected, requests wait; those that expire in the meantime are still
      answered with `expired`.
    - A trigger allows only `pending → sending | failed` and `sending → sent | failed`, so an
      outcome that was reported can never be rewritten, by any role.
  - **What core does with events.** One consumer takes pending events in `seq` order, one
    transaction each: the handler's writes and the `handled` mark commit together. It keeps the
    latest `connection.state` in its own one-row table, `connection_state`, which is where it
    learns DJ's own JID. `send()` on the chat channel inserts a request, then waits for the row to
    reach `sent` or `failed`; the `send.result` event only wakes that wait early. The same
    idempotency key always maps to the same request id, so a retry never queues a second message,
    and reusing a key for a different message is refused. A request the gateway never picked up
    is given up on shortly after it expires, when it can no longer be sent; one the gateway is in
    the middle of sending is waited for, and reported as `outcome_unknown` if no result ever comes.
  - **Poison rows.** A row that fails the reader's validation, or whose handler fails for a
    non-transient reason, moves to `dead` with an error code and is skipped, so draining never
    loops on it. Transient failures retry with backoff, without reordering later rows.
  - **Retention.** Each reader deletes what it finished more than 7 days ago: core its handled
    events, the gateway its sent and failed requests. `dead` events stay until someone looks at
    them.
  - **Roles.** Each service has its own Postgres role (see "Data ownership"). The gateway can only
    insert into the events table; in the send-requests table it can record outcomes but cannot
    add a request or change what one says. Core is the reverse: it adds requests, with no say
    over their state, and consumes events.
- **Commands.** The chat channel is also how DJ talks to Kaya. A message is a command only when
  all of these hold: DJ typed it (`from_me`, and not forwarded), it is live text less than a day
  old, it is in his own chat, the sender is the account the gateway reports as linked, and its
  WhatsApp id is not one the gateway sent. Everything else, in every other chat, is data.
  - Core records a command in its `commands` table inside the transaction that marks the
    `message.stored` event handled, once per message, so a redelivered event never runs it twice.
  - A separate worker then takes pending commands oldest first, one at a time, outside that
    transaction, so slow work never holds up the event queue and a failure there never marks an
    event dead. A command interrupted by a restart runs again; replies carry an idempotency key
    made from the command, so a rerun does not answer twice. One that keeps being interrupted
    is given up on; an orderly stop or a database outage does not count against it.
  - Only one worker runs commands, held by an advisory lock for as long as it lives, so a second
    core process can never run the same command. More than 20 commands in a minute are dropped
    unanswered, which also ends any loop a fault might start.
  - A command core took in more than 10 minutes after it was sent is not acted on: Kaya says so
    instead. After a day it says nothing. A reply is worth sending until its command is 10
    minutes old; if it could not go out by then, Kaya says so once and the command is marked
    failed.
- **Contracts:** every payload has a JSON Schema (draft 2020-12) in `contracts/schemas/v1/` and
  carries `"version": 1`. Both sides validate at the boundary: the writer before inserting, the
  reader before handling.
  - Formats are pinned with patterns, so any validator reads them the same way: lowercase UUIDs,
    and timestamps in UTC with an uppercase `T` and `Z`.
  - Go: hand-written types in the `contracts` module, used by the gateway and core. Every time field uses
    `contracts.Time`, which always encodes as UTC. Validation errors name the field and the rule,
    never the value, so they are safe to log.
  - Shared samples in `contracts/samples/v1/<schema>/{valid,invalid}/` are checked by the contract
    tests, and every valid sample must also round-trip through the Go types unchanged. The samples are a separate Go package that
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
  everything it creates: today `commands` (its inbox of what DJ typed in his own chat, kept 30
  days after it finishes), `exchanges` (the conversation memory, kept for good), `facts` and
  `fact_learning` (what Kaya knows about DJ, and how far the learner has read), `pause_state`, and
  the search index, `chunks` and `message_chunks`; later tasks, jobs, pending actions and usage. Core's tables point at messages by id or WhatsApp
  id, never by foreign key, because the chat tables belong to the gateway.
- **Media.** A media message gets a `media` row (what the file is, and whether it is `remote`,
  `stored`, `expired` or `failed`) and, when it can be fetched, a `media_keys` row that only the
  gateway can read. Nothing is downloaded unless a `media_requests` row asks for it; the gateway
  then streams the file to `media/<sha256[0:2]>/<sha256>` under its data folder, with a size cap,
  an hourly cap and a free-space floor, and records the outcome in `media.state`. Readers find a
  stored file by the `sha256` in `media`. Paths are built from that hash only, never from a file
  name.
- **Roles.** Each service logs in as its own Postgres role, `kaya_gateway` or `kaya_core`, with
  exactly the grants it needs: the gateway writes the chat tables and can only insert into
  `gateway_events`; core reads the chat tables, consumes `gateway_events` and adds rows to
  `send_requests`. The roles and grants
  are in the migrations; the passwords come from `.env` and are set by the one-shot `db-roles`
  Compose step. The admin login (`POSTGRES_USER`) is used only by `migrate` and `db-roles`. A
  migration that adds a table also grants it, and `db/tests/schema.sql` asserts the whole matrix.
- **Live ingest.** The gateway acknowledges a WhatsApp message only after its transaction commits:
  contact, chat, message and the `message.stored` event together. While Postgres is away it
  retries for 30 s, then drops the connection without acknowledging and reconnects once the
  database answers, and WhatsApp delivers the message again. The gateway also checks the database
  every few seconds and drops the connection itself after two failed checks: whatsmeow needs the
  database to decrypt, so during an outage a message fails inside whatsmeow before the gateway is
  asked to store it, stays unacknowledged, and is sent again only on a new connection. A message
  the database refuses (a constraint, a bug in the mapper) is copied to `ingest_dead`, which only
  the gateway can read, and
  then acknowledged, so one bad message never blocks the rest and can be replayed after a fix.
  Storing the same message twice changes nothing and emits nothing.
- **Search index.** One worker in core, under its own advisory lock, groups text messages (and
  captions and poll questions) of one chat sent within 10 minutes into chunks of up to 30 messages
  or 1,500 characters, once they are 10 minutes old by when they were sent and when they were
  stored, and once the chat's conversation has paused. It reads chat by chat, oldest waiting
  messages first, so a chat that is still talking never holds up the history behind it. Each chunk is written as the chat's name,
  the day and `time sender: text` lines, masked for a cloud provider (with the messages before it
  as context, and code-shaped numbers masked whenever an OTP, code or PIN is mentioned), embedded, and stored with a hash of its unmasked text and the model's name;
  the text itself is never stored again. `message_chunks` keeps the `updated_at` each message was
  read with, so an edit, a delete, a hard delete or a move to another chat is noticed even if it
  landed during the call, and the chunk is made again only if its text changed. A message that
  lands inside an existing chunk's time span makes that chunk again with it. A chunk the service
  refuses is stored as skipped (after a probe shows the service takes requests at all), a rate limit halves the batch and waits as asked, a refused key or
  a write the database refuses stops indexing for 10 minutes, and it waits while Kaya is paused.
- **Search.** A query is embedded as a `query` and matched against the chunks' vectors through a
  `VectorStore` (pgvector's HNSW index, with iterative scanning so a filter rarely empties the
  result), and its words against the messages with Postgres full text search (`simple`
  configuration, a GIN index on `messages`, which the migration adds although the gateway owns the
  table). The two rankings are fused by reciprocal rank. Filters narrow by chat, by contact (a chunk
  where they wrote, or their direct chat) and by time. Only vectors from the configured model are
  compared. For every question in chat, Kaya works out which recent exchanges the context will
  carry, then recalls up to three older exchanges with Kaya from chunks of DJ's own chat that start
  before the oldest of those and are within cosine distance 0.6 of the question, leaving out the
  carried ones by id. They join the context package before the recent conversation, masked like
  it, in a quarter of the budget set aside for them plus what the conversation leaves. The query
  for recall has a 5-second deadline and no retries, so a slow embedding service costs a reply a
  few seconds at most.
- **Answers from chats.** Before a question is answered, the routine model plans a search of DJ's
  other chats from the question and the last two exchanges (strict JSON: whether to search, what for
  in the question's own language, the people and groups named, whether it asks for the newest
  messages, and a date range). Names are matched by trigram similarity in any script (`pg_trgm`),
  by the names DJ saved or, for a contact he did not save, the names they chose; among equally close
  names, whoever DJ talked to most recently comes first. Chunks of the named people or groups are
  searched within cosine distance 0.7 with a 0.1 head start, alongside all chats within 0.6, inside
  the plan's dates, and merged; DJ's own chat with Kaya (the direct chat with the linked account, or
  the chat of the latest exchange while it is logged out) is left out in SQL. A plan asking for the
  newest messages puts the two newest chunks of the best match first, labelled as the newest. The
  plan's query is embedded once and shared with recall. If planning fails, the question itself is
  searched for. Each excerpt is rendered like a chunk, with self-chosen sender and chat names marked
  `~`, and masked with the question's mask session for a cloud model, using the messages just
  before it as context. The excerpts go in a user message of their own just before the question,
  inside tags named afresh for each request, with angle brackets, look-alikes and entities escaped;
  a note in the system message says they are data and never instructions and who "Me", "Kaya" and
  `~` are. When the search finds nothing, fails or finds too much to fit, and when a name matches
  nobody, the system message says so. Excerpts take at most half the room the system prompt and the
  question leave; if the request is still too long, the conversation is dropped first and then the
  excerpts. Kaya's past answers are masked as if a code were mentioned, since one may repeat a code
  from an excerpt.
- **Facts about DJ.** `facts` holds one short line per fact, either told with `/remember` or learned,
  unique ignoring case, at most 200. One worker in core, under its own advisory lock, reads each
  exchange once, in order: it gives the routine model (strict JSON, medium effort) the kept facts
  with their numbers and the exchange (Kaya's answer masked for a cloud model), and gets back facts
  to add, each new or replacing a kept one by number, and numbers to drop. The prompt allows facts
  only from DJ's own words; the answer is there to make sense of them, never a source, so another
  person's message quoted in an answer cannot become a lasting fact. Code then refuses any fact
  holding a placeholder, or a code, card, account or ID number by the masking rules, and caps each
  exchange at five new facts and three dropped. The facts and the move of the `fact_learning`
  cursor commit in one transaction, so an exchange is never learned from twice or half; a deleted
  or later-edited exchange is passed over without a call, an answer the model refuses or garbles is
  passed over, and an outage is retried with backoff. Exchanges are at least 20 seconds apart, so a
  backlog never takes the routine model's allowance from DJ's questions, and learning waits while
  Kaya is paused. Every question's context carries the facts that fit in half the room the system
  prompt and the question leave, those DJ told first, then the newest, in the system message,
  marked as background and not instructions. They are DJ's own words, so they go unmasked.
- **History sync.** Each history chunk WhatsApp announces becomes a row in `history_jobs`, stored
  in the transaction that acknowledges the announcement. A single worker downloads the chunk,
  stores it in batches of at most 200 messages (a savepoint per message, so one bad message
  becomes a dead letter and the rest go in), and confirms it to WhatsApp only after the job is
  marked `stored`. History rows have `source = 'history'` and emit no `message.stored`; core gets
  one `history.synced` per chunk and backfills by scanning. Core must therefore accept an `edited`
  or `deleted` event for a message it never saw as `new`. Live edits and revokes that arrive before
  their message are kept in `pending_changes` and applied after each job.

## Engineering standards

- **Types:** Go with `go vet` and golangci-lint (gosec, staticcheck, gofumpt and more).
- **Config:** only from environment variables, validated once at startup. A bad config stops the
  service with every problem listed. Secrets never appear in logs or errors.
- **Modules:** each feature is its own Go package with a small exported surface. Modules depend on interfaces, not on each other's internals. Providers are chosen by
  config.
- **Providers:** each kind is a package holding the interface, one file per implementation, and a
  constructor that switches on the config value and fails on an unknown one; config accepts only
  implementations that exist. Every kind has a `stub` implementation for development and tests.
  Kinds so far:
  - `llm` (`internal/llm`): `Complete()` with a `routine` or `heavy` tier, returning text, model,
    token usage and a finish reason (`stop`, `length`, `tool_calls`, `content_filter`).
  - `channel` (`internal/channel`): the one conversation between DJ and Kaya (`Start`, `Stop`,
    `Send`, `OnMessage`).
    Reading the rest of WhatsApp goes through the database, not the channel. `Start()` never
    fails because a dependency is down; it connects in the background, and the channel is a
    `/health` check like the db. Every handler sees every message; failures are collected and
    reported together.
  - `embeddings` (`internal/embeddings`): `Embed()` turns up to `MaxBatch()` texts into 1024-long
    vectors, as a `document` to store or a `query` to search with. Each provider names its model,
    which is stored with every vector, and says whether it is a cloud service, so text is masked
    before it leaves.
  - `telephony` comes with the phone-call stage: `PlaceCall()` to an E.164 number with a brief,
    and `GetCall()` for status and transcript. Until then `TELEPHONY_PROVIDER` only accepts `stub`.
  - Every outward action (`Send`, `PlaceCall`) takes a required idempotency key. A repeated key
    returns the first result and never acts twice. Stubs never report that an outward action
    happened.
- **Metrics (from stage 15):** every service exposes `GET /metrics` in Prometheus format on its
  existing HTTP port. Names start with `kaya_` and carry the unit (`kaya_ingest_delay_seconds`).
  Labels hold only small fixed sets (service, kind, state, outcome): never a JID, a name, a chat id
  or message text, both for privacy and because every distinct label value is a new time series.
  Counters for what happened, gauges for what is waiting, histograms for how long things took.
- **Logging:** structured JSON to stdout, one `log/slog` JSON logger per service (`kit/logging`).
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
  optional `info` object, such as the gateway's `"info":{"whatsapp":{"state","reason","since"}}`,
  which uses the `connection.state` contract's states and fixed reason strings.
- **Connection.** whatsmeow reconnects by itself after an ordinary drop. The gateway starts a new
  connection when the first attempt fails or whatsmeow gives up, waiting twice as long after each
  failure, up to a minute, and starting over once a connection has held for a minute. A connection
  that stops answering pings is reported as `disconnected` and replaced after a minute of silence.
  After a logout, a session opened elsewhere or an outdated client it stops trying and says so.
- **Errors:** typed errors at boundaries, never swallowed. Retries with backoff only for network
  calls.
- **Tests:** every phase ships tests for its logic. Anything touching the database is tested
  against a real Postgres. Tests use made-up data, never real chats.
- **Golden files:** core was first written in TypeScript and ported to Go (decisions 72 to 75).
  Before the TypeScript was deleted, it wrote what its functions return on made-up inputs into
  `core/internal/*/testdata`, and the Go tests must still match those files exactly. They are the
  spec: change one only with a deliberate change of behaviour, recorded in `DECISIONS.md`.
- **Dependencies:** pinned exactly; updated through Dependabot PRs.
- **Containers:** multi-stage builds, non-root user, read-only filesystem where possible.
- **Commands:** `make lint`, `make test` and `make build` at the root cover every module.
