# Kaya: a self-hosted personal AI agent you talk to on WhatsApp

[![CI](https://github.com/dhananjay6561/kaya/actions/workflows/ci.yml/badge.svg)](https://github.com/dhananjay6561/kaya/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Kaya is a self-hosted, privacy-first personal AI agent that lives in WhatsApp. It links to your own
WhatsApp account as a linked device, keeps your chats in a Postgres database with pgvector on a
machine you own, and is being built to answer questions about your chats, search the web, make
documents, run scheduled jobs and act for you, always after you confirm. The WhatsApp gateway is
written in Go, and so is the agent; the whole stack runs with Docker Compose. Your chats and
your WhatsApp session stay in your own database; only masked, relevant slices go to a cloud model
when a task needs one.

> **Status: early development.** The WhatsApp side (pairing, message and history storage, media,
> sending) and the command channel are built and checked on a real phone. In your own chat Kaya
> already answers through Groq, remembers your conversation with it, and masks private details
> before anything reaches the cloud. Your chats are being turned into a search index. Answers from
> your other chats, tools and acting for you are on the roadmap. See [Project status and roadmap](#project-status-and-roadmap).

## Contents

- [Overview](#overview)
- [Features](#features)
- [Safety and privacy principles](#safety-and-privacy-principles)
- [Architecture](#architecture)
- [Tech stack](#tech-stack)
- [Repository layout](#repository-layout)
- [Getting started](#getting-started)
- [Development](#development)
- [Testing](#testing)
- [Project status and roadmap](#project-status-and-roadmap)
- [Documentation](#documentation)
- [Contributing](#contributing)
- [Security](#security)
- [License](#license)

## Overview

Kaya is a personal WhatsApp assistant for one person: you. You message it the way you message a
contact, and it works with everything your WhatsApp account can see.

- **Self-hosted.** It runs on your own computer with Docker Compose. Today that is a Mac; the plan is
  a dedicated Linux laptop at home, reachable only over Tailscale.
- **Split by job.** One Go service owns the WhatsApp connection, another is the agent, and they
  share one Postgres database. Each can be restarted, updated or replaced on its own.
- **Built in small phases.** Every phase ships with tests and a clear "done when" check. Stage
  status is in the [roadmap](#project-status-and-roadmap).

## Features

### Working today

- **One-command stack.** `make up` builds and starts Postgres 18 with pgvector, runs the database
  migrations, then starts the gateway and core, and waits until every service is healthy.
- **WhatsApp pairing as a linked device.** `make pair` links Kaya by a QR code in the terminal or by
  an 8-character pairing code. The session is stored in Postgres and survives restarts.
- **A connection that looks after itself.** The gateway reconnects after a drop with a growing wait,
  notices a connection that has gone silent and replaces it, detects a logout from the phone, and
  writes every change of state as an event for the agent. `/health` shows the state and since when.
- **Live ingest.** Every new message is stored with its chat and contact, and a person's LID, phone
  number and device addresses are treated as one contact. Edits, "delete for everyone" and reactions
  are applied as they arrive. A message is acknowledged to WhatsApp only after it is stored, so a
  crash or a database outage does not lose it. While the database is away the gateway drops the
  WhatsApp connection, so nothing arrives that it could not store.
- **History sync.** The chat history WhatsApp sends when a device links goes through a durable
  queue: each chunk is stored before it is confirmed, so a crash repeats a chunk instead of losing
  it.
- **Media on request.** Photos, videos, voice notes and documents get a metadata row. A file is
  downloaded only when asked for (`make media id=...`), with a size cap and an hourly cap.
- **Events to the agent, sends through the gateway.** Core reads every gateway event in order from
  a Postgres queue, one transaction per event. To send, it queues a request; the gateway checks
  that the request is allowed, sends it at most once, and reports the outcome. For now the only
  chat Kaya may write to is your own (`make send text="hello"`).
- **A command channel in your own chat.** What you type in "Message yourself" is a command; every
  other message in every other chat is only data. Kaya tells its own replies from your messages, so
  it never answers itself, and it does not act on a command it only gets to minutes late.
- **Built-in commands.** `/help` lists them, `/status` says whether Kaya is up and connected,
  `/pause` stops every answer until `/resume`. The pause is kept in the database, so it holds
  across restarts, and while paused Kaya still answers its slash commands.
- **Answers from Groq, with lasting memory.** Anything else you type in your own chat is answered by
  Groq's `openai/gpt-oss-120b`, in your language and style, with today's date in your time zone.
  Kaya builds the context itself: your conversation with it, kept for good and fitted to a token
  budget set per model, so follow-ups work and the model can be swapped. Never your other chats.
  Timeouts, limited retries, and a plain notice in the chat when the model cannot answer; logs keep
  token counts, never text.
- **Masking before the cloud.** Anything in a cloud model's context that you did not type has phone
  numbers, emails, OTPs, card and account numbers, Aadhaar, PAN, UPI ids and addresses replaced by
  placeholders like `[PHONE_1]`, which are put back in the answer before you see it. A local model
  gets the text as it is.
- **A search index of your chats.** Messages of one chat sent close together are grouped into
  chunks and embedded by Voyage `voyage-4`, after masking (names go as they are), into pgvector in
  your own database. Only a hash of each chunk's text is kept, never a second copy of it. Edits,
  deletes and history that arrives late make the affected chunk again; `/status` shows how much of
  your history is searchable. Searching it comes next.
- **Least-privilege database logins.** The gateway and core each have their own Postgres role.
  Core, the part that will talk to an LLM, can read chats but cannot see the WhatsApp session, its
  encryption keys, or the keys needed to fetch media.
- **Versioned service contracts.** JSON Schemas for every message between the gateway and core,
  validated by one shared Go module against shared sample payloads.
- **Swappable providers.** Interfaces in core for the LLM, the chat channel and telephony, each with
  a `stub` implementation. Groq is the configured LLM provider.
- **Production basics.** Structured JSON logs with request ids, a `/health` endpoint with a database
  check in both services, clean shutdown on SIGTERM, and distroless, non-root, read-only containers
  pinned by digest.

### Planned

In build order. Status for each stage is in the [roadmap](#project-status-and-roadmap).

- Ask questions about your chats and get sourced answers, with meaning-based search over pgvector,
  tagging and summaries
- An agent loop with tools, confirm-before-act, masking of private details and a prompt-injection
  guard
- Voice-note transcription, so spoken commands and voice notes become searchable
- Sending messages to contacts and groups for you, only after you say yes
- Web search with SearXNG, reading pages, and research answers with links
- Gmail, Google Calendar and Google Drive
- PPT and PDF decks made from a topic or a chat, uploaded to Drive, with the link sent back
- Scheduled jobs, reminders and a morning brief
- Acting on websites you are logged into, and ordering with spending caps
- Phone calls from Kaya's own number, with a transcript and summary sent to you
- Kaya as its own WhatsApp contact on a spare number
- Metrics and dashboards with Prometheus and Grafana, and alerts on a fallback channel
- Backups and hosting on a home Linux machine

## Safety and privacy principles

Kaya can read private chats and will act on logged-in accounts, so every phase follows these
rules:

1. **Confirm before acting outward.** Kaya never messages anyone else, spends money or places a call
   without your "yes" on WhatsApp.
2. **Chat text is data, never instructions.** Only your own messages to Kaya are commands. A planted
   "ignore previous instructions" inside someone else's message is just text.
3. **It talks in one place.** It reads everywhere but replies only in its own chat with you.
4. **No stored passwords.** You log in once in Kaya's browser profile and it reuses the session.
5. **Masking before the cloud.** Fixed rules strip phone numbers, emails, OTPs, card and account
   numbers and addresses before any chat text reaches a cloud model, and only the messages a task
   needs are sent.

Most of the enforcement arrives with the agent stages. Some of it is already in place:

- The send-request contract only allows sends to people and groups (never Status, broadcast lists,
  channels or bots), requires an authorization (your own self-chat, or a confirmed pending action)
  and expires within 24 hours. The gateway is the last gate before the WhatsApp socket.
- Events between services carry row ids, never message text, so queues and logs never hold chat
  content.
- Masking runs on everything sent to Groq that you did not type yourself, today Kaya's own past
  answers, and on every chunk of chat text sent to Voyage to be embedded. Contact and group names
  go as they are. Only placeholders and per-kind counts ever appear in logs.
- The gateway container gets only the settings it needs and never sees API keys. WhatsApp addresses
  and raw protocol messages are masked in its logs.

Accepted risk: linked-device libraries such as whatsmeow are unofficial, so there is a small risk of
a WhatsApp ban. Reading is low risk; bulk sending is what gets numbers banned, and Kaya is not built
to do it.

## Architecture

Kaya is a small set of services in one Docker Compose stack. Each service does one job in the
language with the best libraries for it, and they talk only through versioned contracts.

| Service | Language | Owns |
|---|---|---|
| `gateway` | Go (whatsmeow) | The WhatsApp connection: pairing, session, receiving and sending. Knows nothing about the agent. |
| `core` | Go | The agent: commands, LLM calls, tools, safety rules, confirmations, scheduler |
| `db` | Postgres 18 + pgvector | All shared data, including the WhatsApp session |
| `migrate` | dbmate | Applies `db/migrations/` once before the services start |

### System overview

```mermaid
flowchart LR
    phone["Your phone<br/>WhatsApp"]
    llm["LLM provider<br/>Groq"]
    embed["Embeddings<br/>Voyage"]

    subgraph host["Your machine, Docker Compose"]
        gateway["gateway<br/>Go, whatsmeow"]
        core["core<br/>Go agent"]
        db[("db<br/>Postgres 18, pgvector")]
        migrate["migrate<br/>dbmate, one-shot"]
        whisper["whisper<br/>voice notes"]
        searxng["searxng<br/>web search"]
        docs["docs<br/>PPT and PDF"]
        browser["browser<br/>Playwright, noVNC"]
        voice["voice<br/>phone calls"]
    end

    phone <-->|"linked device"| gateway
    migrate -->|"schema"| db
    gateway -->|"WhatsApp session"| db
    core -->|"SQL"| db
    gateway -->|"chats, events outbox"| db
    db -->|"events, in order"| core
    core -->|"send requests via Postgres outbox"| gateway
    core -->|"masked context"| llm
    core -->|"masked chunks"| embed
    core -.-> whisper
    core -.-> searxng
    core -.-> docs
    core -.-> browser
    core -.-> voice

    classDef planned stroke-dasharray: 5 5
    class whisper,searxng,docs,browser,voice planned
```

*Figure 1. System architecture. Solid boxes and lines are built today: the gateway, core, Postgres,
the migration step, events from the gateway to core, send requests from core to the gateway, and
core asking Groq with masked context and Voyage for vectors of masked chunks. Dashed boxes and lines are planned: the whisper, search, docs,
browser and voice services.*

### How the gateway and core talk

The gateway and core never call each other directly. Each direction is an outbox table in the shared
Postgres database, used as a durable queue: the writer inserts the validated payload in the same
transaction as the data it describes and sends `NOTIFY` as a wake-up, and exactly one reader drains
the rows in order. Delivery is at least once, so every handler is idempotent, and a message is never
sent twice.

Both directions are built: the gateway writes an event as it stores each message, core drains them
in order, and everything Kaya says goes out as a send request the gateway checks and sends.

```mermaid
sequenceDiagram
    autonumber
    participant WA as WhatsApp
    participant GW as gateway
    participant PG as Postgres
    participant CO as core

    Note over GW,CO: Events, gateway to core
    WA->>GW: New message
    GW->>PG: In one transaction, upsert the message row and insert a message.stored event
    GW->>PG: NOTIFY as a wake-up
    PG-->>CO: Notification
    CO->>PG: Read pending events in seq order
    CO->>CO: Validate against event.schema.json
    CO->>PG: In one transaction, apply the handler and mark the event handled

    Note over GW,CO: Send requests, core to gateway
    CO->>PG: Insert a send request as pending and NOTIFY
    PG-->>GW: Notification
    GW->>GW: Validate, check authorization and expires_at
    GW->>PG: Generate the WhatsApp message id and mark the row sending
    GW->>WA: Send the message
    GW->>PG: Mark it sent or failed and insert a send.result event
    Note over GW,PG: After a crash, a row left in sending is never resent. It reports outcome_unknown.
```

*Figure 2. Event and send-request flow through the Postgres outbox, with NOTIFY as a wake-up and
one ordered reader per table. On start, or after a missed notification, each reader drains every
pending row in order.*

Events are thin: `message.stored` carries the message, chat and sender ids, never the body, and is
emitted only when a row was actually inserted or changed. The full rules, including poison rows,
retention and per-service database roles, are in
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md#how-services-talk).

### Data model

One schema lives in `db/migrations/` as plain SQL, applied by dbmate before the services start.
whatsmeow keeps its session in its own tables, in a `whatsmeow` schema that only the gateway's
database role can see. The diagram shows the chat tables; the gateway also has working tables core
cannot read (`media_keys`, `media_requests`, `history_jobs`, `pending_changes`, `ingest_dead`) and
the `gateway_events` outbox.

```mermaid
erDiagram
    contacts ||--o| chats : "direct chat with"
    contacts ||--o{ messages : "sends"
    contacts |o--o{ messages : "is replied to in"
    chats ||--o{ messages : "contains"
    chats ||--o{ reactions : "has"
    contacts ||--o{ reactions : "reacts"
    contacts ||--o{ reactions : "is reacted to"
    messages |o..o{ reactions : "targeted by, no FK"

    contacts {
        bigint id PK
        text lid UK "LID JID"
        text pn UK "phone-number JID"
        text bot UK "bot JID"
        text saved_name
        text first_name
        text push_name
        text business_name
        timestamptz created_at
        timestamptz updated_at
    }
    chats {
        bigint id PK
        text kind "direct, group, broadcast or newsletter"
        bigint contact_id FK, UK "direct chats only"
        text jid UK "every kind except direct"
        text name
        timestamptz last_message_at
        timestamptz created_at
        timestamptz updated_at
    }
    messages {
        bigint id PK
        bigint chat_id FK
        text wa_id "WhatsApp message id"
        bigint sender_id FK
        boolean from_me
        timestamptz sent_at
        text kind "text, image, voice and more"
        text body "null once revoked"
        text reply_to_wa_id
        bigint reply_to_sender_id FK
        text source "live or history"
        timestamptz edited_at
        timestamptz deleted_at
        timestamptz created_at
        timestamptz updated_at
    }
    reactions {
        bigint chat_id PK, FK
        text target_wa_id PK
        bigint target_sender_id PK, FK
        bigint sender_id PK, FK
        text emoji
        timestamptz reacted_at
        timestamptz updated_at
    }
```

*Figure 3. Entity relationship diagram of contacts, chats, messages and reactions. A message is
unique by chat, WhatsApp id and sender. A reaction has no foreign key to its message, because it can
arrive before the message it targets.*

A media message also has a `media` row (type, size, duration and whether the file is downloaded).

Core keeps its own tables next to the chat tables. They point at messages by id but have no foreign
keys to them, because the chat tables belong to the gateway.

```mermaid
erDiagram
    messages ||..o| commands : "is the command, no FK"
    messages ||..o| exchanges : "is the question, no FK"
    messages ||..o| exchanges : "is the answer, by wa_id"
    send_requests ||..o| messages : "becomes, by wa_id"
    messages ||..o| message_chunks : "is indexed in, no FK"
    chunks ||--|{ message_chunks : "holds"

    commands {
        bigint id PK
        bigint message_id UK "the message DJ typed"
        text wa_id
        timestamptz sent_at
        text state "pending, done or failed"
        boolean urgent "slash commands run first"
        int attempts
        text error "a short code"
        timestamptz created_at
        timestamptz finished_at
    }
    exchanges {
        bigint id PK
        bigint question_message_id UK
        text answer_wa_id "Kaya's answer"
        timestamptz created_at
    }
    chunks {
        bigint id PK
        bigint chat_id
        timestamptz first_sent_at
        timestamptz last_sent_at
        int message_count
        text text_hash "of the text, never the text"
        text model "voyage-4"
        halfvec embedding "1024, HNSW cosine index"
        text skipped "set when the service refused it"
    }
    message_chunks {
        bigint message_id PK
        bigint chunk_id FK
        timestamptz message_updated_at "as read"
    }
    pause_state {
        boolean only_row PK "always one row"
        timestamptz paused_at "null while running"
        text changed_by
        timestamptz changed_at
    }
    gateway_events {
        bigint seq PK
        uuid id UK
        text type "connection.state, message.stored, history.synced, send.result"
        jsonb payload
        text state "pending, handled or dead"
        text error
        timestamptz created_at
        timestamptz finished_at
    }
    send_requests {
        bigint seq PK
        uuid id UK
        jsonb payload
        timestamptz expires_at
        text state "pending, sending, sent or failed"
        text wa_id UK "chosen before sending"
        text error
        timestamptz created_at
        timestamptz finished_at
    }
```

*Figure 4. Core's tables and the two outboxes. `chunks` and `message_chunks` are the search index:
each chunk is a few messages of one chat embedded as one vector, and each indexed message points at
its chunk. `commands` is the inbox of what DJ typed in his own
chat; finished rows are deleted after 30 days. `exchanges` is the conversation memory, one row per
question and answer, kept for good. `pause_state` holds `/pause`. `gateway_events` and
`send_requests` are the queues between the services; handled and finished rows are deleted after 7
days.*

A person is one `contacts` row however WhatsApp addresses them. The gateway resolves every address
through `contact_id_for()`, which merges two rows (with their chats, messages and reactions) once a
LID and a phone number turn out to be the same person. Edits and revokes update the message they
target instead of adding rows.

## Tech stack

| Layer | Choice |
|---|---|
| WhatsApp gateway | Go 1.26, [whatsmeow](https://github.com/tulir/whatsmeow), `log/slog` |
| Agent | Go 1.26, `log/slog`, pgx; Groq over plain `net/http`, its own o200k token counter |
| Database | Postgres 18 with pgvector, migrations with dbmate |
| Contracts | JSON Schema 2020-12, validated with santhosh-tekuri/jsonschema in a shared Go module |
| LLM | Groq behind a provider interface (`openai/gpt-oss-120b` and `openai/gpt-oss-20b`) |
| Embeddings | Voyage `voyage-4` behind a provider interface, 1024 dimensions, stored as `halfvec` with an HNSW index |
| Runtime | Docker Compose, distroless images, non-root, read-only root filesystem |
| Quality | golangci-lint, `go test -race`, golden files from the TypeScript core, SQL schema tests, GitHub Actions |

## Repository layout

```
kaya/
├── core/               Go agent (its own Go module)
│   ├── cmd/core/       main package: serve, send, healthcheck
│   └── internal/       config, gateway (events), channel (inbox, sends), commands, pause, status,
│                       llm (Groq, stub, o200k token counter), memory, chat, masking, text
├── gateway/            Go WhatsApp gateway (its own Go module)
│   ├── cmd/gateway/    main package: serve, pair, media, healthcheck
│   └── internal/       config, whatsapp, ingest, events, send, media, testdb
├── kit/                Go code both services share: logging, httpapi, envfile, store, testdb
├── contracts/          JSON Schemas, shared sample payloads, and the Go types and validator
├── db/
│   ├── migrations/     plain SQL migrations (dbmate)
│   └── tests/          SQL schema tests
├── docs/               spec, architecture, infra, decisions, phases
├── compose.yml         base stack
├── compose.mac.yml     per-machine override (ports on 127.0.0.1)
├── Makefile            one entry point for dev, lint, test and build
└── .env.example        every setting for every service
```

Folders such as `services/` (Python docs and voice services) and `deploy/` (home-server kit) appear
in the phases that need them.

## Getting started

### Requirements

- Go 1.26 and make
- Docker with Compose
- A phone with WhatsApp, to link Kaya as a device

Run everything from the repo root.

### 1. Configure

```sh
git clone https://github.com/dhananjay6561/kaya.git
cd kaya
cp .env.example .env
```

Fill in the required values in `.env`. Every setting is documented in the file. Two to check first:

- `POSTGRES_PASSWORD`, `GATEWAY_DB_PASSWORD` and `CORE_DB_PASSWORD`. The gateway and core each log
  in to Postgres with their own limited role; change all three from the development values before
  real chats go in (and the matching `*_DATABASE_URL` values if you run a service from source).
- `GROQ_API_KEY`, or set `LLM_PROVIDER=stub` until you need a real model. Core refuses to start
  with `LLM_PROVIDER=groq` and no key.

### 2. Start the stack

```sh
make up
```

This builds the images, starts Postgres, applies the migrations, starts the gateway and core, and
waits until all of them are healthy. Data stays in `DATA_DIR` (`./data` by default) across restarts.

### 3. Check health

```sh
curl -s http://127.0.0.1:3001/health   # gateway
curl -s http://127.0.0.1:3000/health   # core
```

Each answers 200 with `"status":"ok"` when its database check passes, and 503 otherwise. Before
pairing, the gateway reports WhatsApp under `info.whatsapp` as `logged_out` with the reason
`not paired: run make pair`. That state never fails the health check.

### 4. Link WhatsApp

`make pair` links Kaya to your WhatsApp as a linked device. It rebuilds the gateway image, stops the
gateway if it is running, shows a QR code to scan under WhatsApp > Settings > Linked devices > Link a
device, and afterwards starts the gateway again if it was running before. Without a camera handy,
or on a headless box, use a pairing code: `make pair PHONE=919876543210` (full international
number). The device shows up as "Kaya".

```mermaid
sequenceDiagram
    actor You
    participant Make as make pair
    participant Compose as Docker Compose
    participant Pair as gateway pair command
    participant PG as Postgres
    participant WA as WhatsApp on your phone

    You->>Make: Run make pair, with PHONE set for a pairing code
    Make->>Compose: Build the gateway image
    opt The gateway is running
        Make->>Compose: Stop the gateway
    end
    Make->>Pair: Run the one-off pair command
    Pair->>PG: Look for a stored session
    alt A session is already stored
        Pair-->>You: Already paired, remove Kaya on the phone first
    else No session yet
        alt QR code
            Pair-->>You: QR code in the terminal, refreshed until scanned
            You->>WA: Linked devices, Link a device, scan the code
        else Pairing code
            Pair->>WA: Request a pairing code for the number
            Pair-->>You: 8-character code
            You->>WA: Link with phone number instead, enter the code
        end
        WA-->>Pair: Linked as Kaya
        Pair->>PG: Store the session in the whatsmeow tables
        Pair-->>You: Done, after the first connection is up
    end
    opt The gateway was running before
        Make->>Compose: Start the gateway and wait until healthy
    end
```

*Figure 5. WhatsApp pairing with `make pair`. Pairing is a one-off command, not part of the running
service, so two clients never share one session.*

The session is stored in Postgres and survives restarts. The gateway's `/health` shows the
connection under `info.whatsapp`. To unlink, remove Kaya under Linked devices on the phone; the
gateway notices on its next connection, drops the session and reports `logged_out`.

## Development

Needs Go 1.26, make and Docker with Compose. Run everything from the repo root.

```sh
make install       # download the Go modules
make dev           # run core from source
make dev-gateway   # run the WhatsApp gateway from source
make db            # start only Postgres and apply migrations, for running services from source
make up            # build and run the whole stack in Docker, wait until healthy
make pair          # link Kaya to WhatsApp (QR code; PHONE=919876543210 for a pairing code)
make pair-dev      # pair from source against the db from make db
make media id=42   # download the file of message 42 (photo, voice note, document)
make logs          # follow logs from every service
make ps            # show the status of every service
make down          # stop the stack (data stays in DATA_DIR)
make migration name=add_tasks   # new empty migration in db/migrations
make migrate       # apply pending migrations
make rollback      # roll back the latest migration on the live db (asks first)
make format        # fix lint and formatting where possible
make test-db       # migrations and schema tests on a throwaway database
make check         # lint, test and build everything (what CI runs)
make help          # list every target
```

Copy `.env.example` to `.env` and fill in the required values first.

How the code is organised, and the standards every change follows:
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Testing

```sh
make check   # lint, test and build both services
```

Tests that need Postgres run when `TEST_DATABASE_URL` is set and are skipped otherwise (CI always
sets it). With `make db` running:

```sh
TEST_DATABASE_URL=postgres://kaya:kaya@localhost:5432/kaya make test
```

- **Schema tests.** `make test-db` needs the db container running (`make db` or `make up`). On a
  throwaway `kaya_schema_test` database it migrates, reruns (which must apply nothing), rolls back,
  reapplies and runs the SQL tests in `db/tests/`. It never touches your live database.
- **Contract tests.** Every sample in `contracts/samples/v1/` must pass or fail as its folder says,
  and every valid sample must round-trip through the Go types unchanged.
- **Golden tests.** The agent was first written in TypeScript. Before that code was deleted, it
  wrote what its functions return on made-up inputs into `core` testdata: token counts for every
  Unicode character, context packages, system prompts, WhatsApp formatting, masking of a few
  thousand messages and exact Groq requests. The Go tests must still match them exactly, so the
  files are the spec; change one only together with a deliberate change of behaviour.
- **CI.** [GitHub Actions](.github/workflows/ci.yml) runs core and gateway jobs against a real
  Postgres with pgvector, brings up the full Compose stack and runs the schema tests, and reviews
  dependency changes on pull requests.

Tests use made-up data, never real chats.

## Project status and roadmap

<!-- The single place for stage status in this README. -->

Kaya is built in 17 stages of small phases.

Status key: ✅ done · 🟨 in progress · ⬜ not started

| Stage | Delivers | Status |
|---|---|---|
| 0. Foundation | Monorepo, config, Compose stack, migrations, logging, health, provider interfaces | ✅ |
| 1. WhatsApp gateway | Contracts, pairing, live ingest, history sync, media, connection health, delivery to core and sending. Checked end to end against a real WhatsApp account. | ✅ |
| 2. Talking to Kaya | Self-chat command channel and built-in commands `/help`, `/status`, `/pause`, `/resume`. Checked on a real account. | ✅ |
| 3. Brain | Groq replies, lasting conversation memory, masking for cloud models (built), local embeddings, meaning-based search, Q&A with sources, tagging, summaries | 🟨 |
| 4. Agent loop and safety | Masking, context picking, tool calling, confirm-before-act, prompt-injection guard, usage tracking | ⬜ |
| 5. Voice notes | Transcription, spoken commands | ⬜ |
| 6. Sending as you | Messages to contacts and groups after confirmation, with rate limits | ⬜ |
| 7. Web | Search, page reading, research answers with sources | ⬜ |
| 8. Google | OAuth, Drive, Gmail, Calendar | ⬜ |
| 9. Documents | PPT and PDF from a topic or chat, uploaded to Drive | ⬜ |
| 10. Scheduler | Persistent jobs, plain-language schedules, reminders, morning brief | ⬜ |
| 11. Browser | Logged-in browser profile, browser tools, guardrails | ⬜ |
| 12. Ordering | Cart up to checkout, pay on "yes" with spending caps, order status | ⬜ |
| 13. Phone calls | Outbound calls with AI disclosure, transcript and summary | ⬜ |
| 14. Kaya as a contact | A second device on a spare number | ⬜ |
| 15. Hardening | Fallback alerts, regression test set, metrics and dashboards (Prometheus, Grafana) | ⬜ |
| 16. Home hosting | Linux host setup, GPU model, backups, move day | ⬜ |

## Documentation

| Document | What it covers |
|---|---|
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | Services, repo layout, how services talk, engineering standards |
| [docs/DECISIONS.md](docs/DECISIONS.md) | Every settled decision with its reason, plus open questions |
| [contracts/README.md](contracts/README.md) | The gateway and core contracts, and how to change them |

## Contributing

Kaya is a personal project built in public. Issues, questions and ideas are welcome. If you want to
send a pull request, keep it to one small, focused change and read
[CONTRIBUTING.md](CONTRIBUTING.md) first: run `make check`, sign off every commit, and never commit
`.env`, keys, WhatsApp sessions or real chat data.

## Security

Kaya reads private chats, so security reports are taken seriously. Please report vulnerabilities
privately through
[GitHub's private vulnerability reporting](https://github.com/dhananjay6561/kaya/security/advisories/new),
not in a public issue. Details are in [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE)
