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
> before anything reaches the cloud. It searches your chats by meaning and answers questions about
> them with sources. Tools and acting for you are on the roadmap. See [Project status and roadmap](#project-status-and-roadmap).

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
contact, and it works with everything your WhatsApp account can see, plus the web.

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
  that the request is allowed, sends it at most once, and reports the outcome. Kaya writes to
  your own chat (`make send text="hello"`), and to anyone else only for an action you said yes to,
  which the gateway checks for itself.
- **A command channel in your own chat.** What you type in "Message yourself" is a command; every
  other message in every other chat is only data. Kaya tells its own replies from your messages, so
  it never answers itself, and it does not act on a command it only gets to minutes late.
- **Built-in commands.** `/help` lists them, `/status` says whether Kaya is up and connected,
  `/pause` stops every answer until `/resume`. The pause is kept in the database, so it holds
  across restarts, and while paused Kaya still answers its slash commands. `/facts`, `/remember`
  and `/forget` show and change what Kaya knows about you; `/tasks` and `/done` show and close
  the tasks found in your chats; `/summary` sums up today or one chat; `/alerts` switches alerts.
- **Answers from Groq, with lasting memory.** Anything else you type in your own chat is answered by
  Groq's `openai/gpt-oss-120b`, in your language and style, with today's date in your time zone.
  Kaya builds the context itself: facts about you, your conversation with it (kept for good) and
  what a search of your chats finds, fitted to a token budget set per model, so follow-ups work and
  the model can be swapped. Timeouts, limited retries, and a plain notice in the chat when the model cannot answer; logs keep
  token counts, never text.
- **Masking before the cloud.** Anything in a cloud model's context that you did not type has phone
  numbers, emails, OTPs, card and account numbers, Aadhaar, PAN, UPI ids and addresses replaced by
  placeholders like `[PHONE_1]`, which are put back in the answer before you see it. A local model
  gets the text as it is.
- **A search index of your chats.** Messages of one chat sent close together are grouped into
  chunks and embedded by Voyage `voyage-4`, after masking (names go as they are), into pgvector in
  your own database. Only a hash of each chunk's text is kept, never a second copy of it. Edits,
  deletes and history that arrives late make the affected chunk again; `/status` shows how much of
  your history is searchable.
- **Search, and a long memory.** Search finds chunks by meaning (your question's vector against the
  index) and by words (Postgres full text search, with no language rules so Hinglish and names
  match as typed), fused into one ranking, with filters by chat, contact and date. Each question you
  ask Kaya also brings back up to three older exchanges with it that bear on the question, beyond
  the recent conversation. Try it from a terminal: `docker compose exec core /core search "money"`.
- **Talks like a friend, not a bot.** Kaya texts like a close friend who happens to be scarily
  capable: short, warm, cheeky, in your language and script, and honest about what she can and
  cannot do yet.
- **Answers from your chats.** Ask "what did Rahul say about Saturday?", "usne ab kya bola?" or
  "¿qué dijo María ayer?" and a small model first works out what to search for: who you mean (even
  from your last question), what about, whether you want the newest messages, and which days. Kaya
  then searches your other chats, matching names in any script and despite typos, and gives the
  model up to three excerpts with who said what, in which chat and when, so the answer can say where
  it came from. The excerpts are masked, kept out of the instructions in a message of their own,
  marked as data the model must never obey, and escaped so no message can break out of them; a name
  someone chose for themselves shows as `~Name`, so nobody can pass for you or Kaya. If the search
  finds nothing, could not run, or a name matches nobody, the model is told. Kaya never sees a whole
  chat, only the excerpts it found.
- **Facts about you.** After each answer a small model reads what you said and keeps what is worth
  knowing for months, as one short line each: your work, the people in your life, where you live,
  how you like answers. It changes a fact when you say it changed and drops one when you say to.
  Facts go into every answer's context. They come only from your own words, never from someone
  else's message an answer quotes, and codes, card, account and ID numbers are never kept.
  `/remember` adds one yourself, `/facts` lists them with numbers, `/forget 12` drops one.
- **Tags and tasks from your chats.** Every new message in your other chats is read once, a few
  minutes after it arrives, a chat at a time in batches, and tagged: important (a question to you,
  something urgent, money, family, a deadline), ignore (promotions, forwards, chatter that is not
  about you), task (something you were asked or promised to do) or reminder (a meeting, call, bill
  or birthday with a day). Only your own to-dos count: what you tell someone else to do is theirs,
  and a message too vague to act on gives nothing. Tasks and reminders get a title that makes sense
  on its own and a due date; a repeat of one already open (an hourly reminder, a "don't forget") is
  not added again. `/tasks` lists the open ones with where they came from, marks late tasks
  overdue and drops a reminder once its time has passed; `/done 12` closes one. Messages are masked before they
  reach the model, and tagging stays inside a daily token allowance so your questions always have
  room. Older history is not tagged.
- **Summaries on request.** `/summary` sums up what came in today across your chats, direct chats
  and important ones first, promotions left out; `/summary Office Team` sums up one chat's last day,
  found by name the way search finds people and groups. Each summary leads with what needs you,
  then a line or two per chat, masked on the way to the model like everything else; when a day is
  too long for the model's budget, the newest messages are kept and the summary says how many were
  left out.
- **Tools.** The model can call tools while it answers, see what they return and call again
  before it replies: the time anywhere in the world now or at a given time, with daylight saving
  handled ("9pm PST mere liye kitne baje?"), and your open tasks ("kya pending hai?"). Arguments
  are checked against each tool's schema, results are masked before the model reads them, a few
  rounds is the limit, and for now every tool only reads; anything that acts for you will wait for
  your "yes".
- **Alerts.** Kaya keeps reading your chats and messages you in your own chat when something needs
  you: someone waiting on your answer, money, health, family, a plan changing, a deadline, or a
  message asking for an OTP or money that looks like a scam. Urgent ones come at once; the rest are
  gathered into one message within 45 minutes. Reminders get a heads-up 30 minutes before their
  time. Nothing is sent once you have replied or the message was deleted, a note never carries a
  link, and one about a code or password always warns you. There are no quiet hours;
  `ALERTS_PER_HOUR` caps the messages (10 by default, reminders always go), and `/alerts off` and
  `/alerts on` switch them. Alerts from the last 12 hours are part of your conversation with Kaya,
  so a reply like "what did she want?" or "remind me later" knows which one you mean.
- **Messages sent for you, only after your yes.** "Tell Rahul I'll be late" or "office group mein
  bol do standup 11 baje" makes Kaya draft the message, find the contact or group by name, and put
  exactly what will be sent and to whom (a group, or a contact with their number) at the top of her
  reply; it goes out only if you say yes, however you put it ("Yess", "haan bhej de yaar", 👍),
  within 10 minutes, after that question and before anything else from Kaya. A no in any words
  cancels it, a change ("haan but say 7 pm") gets a new draft, and anything else is just a new
  message, while the draft keeps waiting. A small model reads only your reply and the draft, never
  anyone else's messages, and when it can't tell, nothing is sent. The gateway sends it only if your yes
  is in your own chat, after Kaya asked, for that same chat and that same text, and only once;
  `SENDS_PER_HOUR` (20) caps them.
- **Reminders and scheduled messages.** "6 baje shaurya ko call yaad dilana", "20 min baad chai",
  "every weekday at 9 send me my open tasks", "har mahine 1 tareekh ko rent": Kaya sets it and puts
  the exact time under her answer (⏰ Sat 28 Nov, 18:00: Call Shaurya), worked out by code in your
  time zone, so what you read is when it fires. A reminder arrives on the minute; one that repeats
  moves on to its next time; an "ask" job (your tasks every morning) is answered fresh each time.
  "10 min baad" right after a reminder works, "mere reminders dikhao" lists them, and "gym wala
  cancel kar do" stops one. If Kaya was off when one was due, it still comes, saying so. The
  scheduler sleeps until the next one is due and wakes when you set one, so it costs nothing while
  waiting, and a plain reminder needs no model call when it fires.
- **Voice notes.** Send Kaya a voice note and it is a command like any message you type: "kal 6
  baje yaad dilana" spoken works the same as typed. Everyone else's new voice notes are
  transcribed in the background, so they can be searched, tagged and summed up like text. Groq's
  Whisper (`whisper-large-v3-turbo`) does it in about a second, steered to write Hindi in Latin
  letters the way your chats are written. The audio goes to Groq as it is, since audio cannot be
  masked; the text it gives back is masked like any message before a model sees it. Older voice
  notes from before this was switched on are left alone.
- **Web search.** Ask about anything recent or changing, "iphone 17 ka price kya hai abhi?" or
  "who won the IPL final?", and Kaya searches the web and answers the way a friend who just checked
  would ("Reliance Digital pe ₹82,900 se start ho raha hai"), with the one link it used under the
  answer. That link is put there by Kaya's code from the search results, so it is never made up.
  Every price, number and name in an answer is checked against what the search actually showed, and
  anything it did not show is taken out before you see it. Small talk and things it already knows
  never trigger a search. Search runs through
  [SearXNG](https://github.com/searxng/searxng), a search engine you host yourself that asks
  Google, DuckDuckGo and Yahoo at once, so there is no search account or API key, and no single
  company gets all your queries.
  Kaya can limit a search to the last day, week, month or year when freshness matters. If no engine
  answers, Kaya says web search is not working right now instead of guessing. A query that would
  carry a private detail (a phone number, email, address or code) is refused before it leaves your
  machine.
- **Least-privilege database logins.** The gateway and core each have their own Postgres role.
  Core, the part that will talk to an LLM, can read chats but cannot see the WhatsApp session, its
  encryption keys, or the keys needed to fetch media.
- **Versioned service contracts.** JSON Schemas for every message between the gateway and core,
  validated by one shared Go module against shared sample payloads.
- **Any model, chosen in `.env`.** Kaya talks to models through one OpenAI-compatible client, so
  switching from Groq to OpenAI, Gemini, OpenRouter, Anthropic's compatible endpoint or a local
  Ollama is a change of settings, not code (`LLM_PROVIDER=compatible`, `LLM_BASE_URL`,
  `LLM_API_KEY`, the models). The background work (tagging, facts, planning) can run on a provider
  or key of its own (`ROUTINE_LLM_*`), so it never uses up the allowance your answers need, and voice
  notes can go to any OpenAI-compatible transcription service. A provider marked local only works
  at a local or private address, so chats are never sent unmasked to a cloud by mistake. The chat
  channel and telephony also sit behind interfaces with a `stub`.
- **Production basics.** Structured JSON logs with request ids, a `/health` endpoint with a database
  check in both services, clean shutdown on SIGTERM, and distroless, non-root, read-only containers
  pinned by digest.

### Planned

In build order. Status for each stage is in the [roadmap](#project-status-and-roadmap).

- Reading a page from a link, and research answers that read a few pages and cite them
- Gmail, Google Calendar and Google Drive
- PPT and PDF decks made from a topic or a chat, uploaded to Drive, with the link sent back
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
- Masking runs on everything sent to Groq that you did not type yourself (Kaya's own past answers
  and the chat excerpts it finds for a question) and on every chunk of chat text sent to Voyage to
  be embedded. Excerpts reach the model outside its instructions, marked as data, inside tags named
  afresh for every request and escaped, so a planted message cannot close its block or pass for
  you or Kaya. Contact and group names
  go as they are. Only placeholders and per-kind counts ever appear in logs.
- Web search: before a query leaves, it is checked with the same masking rules, and any query
  holding a phone number, email, address or code is refused, so neither Kaya nor a planted message
  can send your details to a search engine. Search results are public pages, so they reach the
  model unmasked (prices and dates stay readable) but marked as data like excerpts, never as
  instructions. SearXNG listens only inside the stack (or on `127.0.0.1` on a Mac), keeps no
  account and needs no key.
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
| `searxng` | Python ([SearXNG](https://github.com/searxng/searxng)) | Web search: asks several search engines and returns their results as JSON to core. A Compose service on Linux; on a Mac it runs outside Docker (`make searxng-mac`) |

### System overview

```mermaid
flowchart LR
    phone["Your phone<br/>WhatsApp"]
    llm["LLM provider<br/>Groq, or any OpenAI-compatible"]
    embed["Embeddings<br/>Voyage"]
    stt["Transcription<br/>Groq Whisper"]
    searxng["searxng<br/>web search"]
    engines["Search engines<br/>Google, DuckDuckGo, Yahoo"]

    subgraph host["Your machine, Docker Compose"]
        gateway["gateway<br/>Go, whatsmeow"]
        core["core<br/>Go agent"]
        db[("db<br/>Postgres 18, pgvector")]
        migrate["migrate<br/>dbmate, one-shot"]
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
    core -->|"voice notes"| stt
    core -->|"queries"| searxng
    searxng --> engines
    core -.-> docs
    core -.-> browser
    core -.-> voice

    classDef planned stroke-dasharray: 5 5
    class docs,browser,voice planned
```

*Figure 1. System architecture. Solid boxes and lines are built today: the gateway, core, Postgres,
the migration step, events from the gateway to core, send requests from core to the gateway, and
core asking Groq with masked context, Groq Whisper for voice note transcripts, Voyage for vectors of masked chunks, and SearXNG for
web searches. SearXNG is drawn outside the Compose stack because on a Mac it runs on the Mac itself; on Linux it is a Compose
service. Dashed boxes and lines are planned: the docs, browser and voice services.*

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
arrive before the message it targets. Core's migrations add a full text index on message text and trigram
indexes on contact and group names, for search, although the gateway owns these tables.*

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
    messages ||..o{ facts : "was said in, no FK"
    exchanges ||..o| fact_learning : "read up to"
    messages ||..o| message_tags : "is tagged in, no FK"
    messages ||..o{ tasks : "gave, no FK"
    messages ||..o| alerts : "needs DJ, no FK"
    tasks ||..o| alerts : "comes up in, no FK"
    jobs ||--o{ job_runs : "fired as"

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
    facts {
        bigint id PK
        text body UK "one short line, unique ignoring case"
        text source "told or learned"
        bigint message_id "the /remember or the question"
        timestamptz created_at
        timestamptz updated_at
    }
    fact_learning {
        smallint id PK "always one row"
        bigint exchange_id "last exchange learned from"
        timestamptz updated_at
    }
    message_tags {
        bigint message_id PK
        text_array tags "important, ignore, task, reminder"
        text skipped "set when passed over"
        timestamptz created_at
    }
    tasks {
        bigint id PK
        bigint message_id "unique with kind"
        bigint chat_id
        text kind "task or reminder"
        text title
        timestamptz due_at
        boolean due_has_time
        text state "open or done"
        timestamptz created_at
        timestamptz done_at
    }
    tagging {
        smallint id PK "always one row"
        bigint started_after "only later messages are tagged"
        date usage_day
        int usage_tokens "spent today"
    }
    alerts {
        bigint id PK
        bigint message_id UK "or task_id"
        bigint task_id UK "a reminder coming up"
        bigint chat_id
        boolean urgent "sent at once"
        text note "one line for DJ"
        text batch "the message it goes out in"
        timestamptz sent_at
        text skipped "answered, deleted, stale, off"
    }
    pending_actions {
        uuid id PK
        text kind "send_message"
        text target_jid "the chat it writes to"
        text summary "what DJ was asked"
        jsonb payload
        text state "pending, confirmed, declined, expired, superseded, done, failed"
        timestamptz expires_at "10 minutes"
        bigint decided_by "DJ's yes or no, no FK"
        text result
    }
    jobs {
        bigint id PK
        text text "what to remind of, or to do"
        boolean ask "answer it when it fires"
        text repeat "none, hourly, daily, weekdays, weekly, monthly"
        int every
        smallint_array weekdays
        text local_time "HH:MM in DJ's zone"
        timestamptz next_at "when it fires next"
        text state "active, done, cancelled"
    }
    job_runs {
        bigint job_id PK "jobs.id"
        timestamptz due_at PK
        timestamptz sent_at
        text text "what was sent"
    }
    transcripts {
        bigint message_id PK "the voice note, no FK"
        text state "done or failed"
        text text "what was said"
        text language
        numeric duration_s
    }
    alerting {
        smallint id PK "always one row"
        boolean enabled "/alerts on, /alerts off"
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
question and answer, kept for good. `facts` is what Kaya knows about DJ, told with `/remember` or
learned from his exchanges; `fact_learning` is how far the learner has read them, moved in the same
transaction as the facts each exchange gave. `message_tags` has a row for every new message tagging
has read, `tasks` the tasks and reminders it found, and `tagging` where it started and what it spent
today. `alerts` are heads-ups for DJ: a message tagging found needs him, or a reminder coming up;
`alerting` is the `/alerts` switch. `pending_actions` holds what Kaya would do outside your own chat
until you say yes or no. `jobs` are reminders and scheduled messages, and `job_runs` each time one
fired, so a time never fires twice. `pause_state` holds `/pause`. `gateway_events` and
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
| LLM | One OpenAI-compatible client; Groq (`openai/gpt-oss-120b`, `openai/gpt-oss-20b`) by default, any compatible provider by settings |
| Embeddings | Voyage `voyage-4` behind a provider interface, 1024 dimensions, stored as `halfvec` with an HNSW index |
| Web search | Self-hosted SearXNG (Google, DuckDuckGo, Yahoo), pinned by digest |
| Runtime | Docker Compose, distroless images, non-root, read-only root filesystem |
| Quality | golangci-lint, `go test -race`, golden files from the TypeScript core, SQL schema tests, GitHub Actions |

## Repository layout

```
kaya/
├── core/               Go agent (its own Go module)
│   ├── cmd/core/       main package: serve, send, healthcheck
│   └── internal/       config, gateway (events), channel (inbox, sends), commands, pause, status,
│                       llm (Groq, stub, o200k token counter), memory, chat, masking, text,
│                       embeddings (Voyage, stub), search (index, search, excerpts), facts,
│                       tagging (tags and tasks), summary, agent (tool registry and loop), tools,
│                       alerts, actions (confirm-before-act), jobs (reminders, scheduler),
│                       voice (transcription), web (SearXNG client)
├── gateway/            Go WhatsApp gateway (its own Go module)
│   ├── cmd/gateway/    main package: serve, pair, media, healthcheck
│   └── internal/       config, whatsapp, ingest, events, send, media, testdb
├── kit/                Go code both services share: logging, httpapi, envfile, store, testdb
├── contracts/          JSON Schemas, shared sample payloads, and the Go types and validator
├── db/
│   ├── migrations/     plain SQL migrations (dbmate)
│   └── tests/          SQL schema tests
├── docs/               spec, architecture, infra, decisions, phases
├── services/searxng/   SearXNG settings, shared by the container and the Mac install
├── scripts/            host scripts run through make (check-network, searxng-mac)
├── compose.yml         base stack
├── compose.mac.yml     per-machine override (ports on 127.0.0.1, SearXNG outside Docker)
├── Makefile            one entry point for dev, lint, test and build
└── .env.example        every setting for every service
```

Folders such as `deploy/` (home-server kit) and the Python docs and voice services under
`services/` appear in the phases that need them.

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
  with `LLM_PROVIDER=groq` and no key. To use another model, set `LLM_PROVIDER=compatible` and the
  `LLM_*` settings described in `.env.example`.
- `SEARXNG_SECRET`: any long random string (`openssl rand -hex 32`). Set `WEB_SEARCH_URL=` (empty)
  to run without web search.

### 2. Start the stack

```sh
make up
```

This builds the images, starts Postgres, applies the migrations, starts the gateway and core, and
waits until all of them are healthy. Data stays in `DATA_DIR` (`./data` by default) across restarts.
On Linux it starts SearXNG too.

On a Mac, SearXNG runs outside Docker, because from inside a Docker Desktop container most search
engines time out ([curl_cffi#915](https://github.com/lexiforest/curl_cffi/issues/915)). Successful
searches are not logged; when Kaya answers with links, search is working. Once, before
or after `make up`:

```sh
make searxng-mac
```

It installs the same SearXNG version as the pinned image into `DATA_DIR/searxng` (needs Python 3.10
or newer and git), runs it on `127.0.0.1:8888` as a login item, and core reaches it through
`host.docker.internal`. `make searxng-mac-stop` removes it.

### 3. Check health

```sh
curl -s http://127.0.0.1:3001/health   # gateway
curl -s http://127.0.0.1:3000/health   # core
```

Each answers 200 with `"status":"ok"` when its database check passes, and 503 otherwise. Before
pairing, the gateway reports WhatsApp under `info.whatsapp` as `logged_out` with the reason
`not paired: run make pair`. That state never fails the health check.

If the gateway keeps reconnecting (`Keepalive timed out` in `make logs`) or Kaya replies late, run
`make check-network`. It finds the largest packet your internet link carries and warns when this
machine sends bigger ones, which stalls large WhatsApp replies (common on PPPoE broadband, which
carries 1492 bytes while Wi-Fi and Ethernet default to 1500). It prints the fix for your system.
TCP MSS clamping on the router fixes every device at once. On macOS, Docker Desktop opens container
connections from the host, so the host's MTU is enough (a VM-based setup such as Colima may need its
VM's MTU set too). On Linux, containers sit on Docker's own bridge networks, which the check also
reads (Docker Desktop for Linux and rootless Docker work like macOS, so that part does not apply
there), so also set Docker's MTU in `/etc/docker/daemon.json` (Docker 24 or newer):

```json
{"mtu": 1492, "default-network-opts": {"bridge": {"com.docker.network.driver.mtu": "1492"}}}
```

Then restart Docker and run `make down && make up` so the networks are made again.

To see web search working, open SearXNG's own page and search there; it is the same engine Kaya
uses. On a Mac that is http://127.0.0.1:8888 (only reachable from the Mac itself), and:

```sh
./scripts/searxng-mac.sh status              # healthy or not running
tail -f data/searxng/searxng.log             # engines that failed, timed out or were blocked
```

On Linux SearXNG has no published port; `make ps` shows it, and `make logs` its output. In Activity
Monitor on a Mac it shows as `granian` (the server SearXNG runs on), and in System Settings > General >
Login Items as `bash`, since a script starts it; leave that on, or search stops after a restart.

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
make check-network # warn when packets are bigger than the internet link carries
make searxng-mac   # on a Mac: install and run SearXNG outside Docker, as a login item
make searxng-mac-stop  # on a Mac: stop SearXNG and remove the login item
make down          # stop the stack (data stays in DATA_DIR)
make migration name=add_tasks   # new empty migration in db/migrations
make migrate       # apply pending migrations
make rollback      # roll back the latest migration on the live db (asks first)
make format        # fix lint and formatting where possible
make test-db       # migrations and schema tests on a throwaway database
make check         # lint, test and build everything (what CI runs)
make help          # list every target
```

Copy `.env.example` to `.env` and fill in the required values first. Core run from source has web
search only when `WEB_SEARCH_URL` points at a SearXNG, for example `http://127.0.0.1:8888` after
`make searxng-mac`.

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
- **Live tests.** A few tests ask the real Groq (and, for web search, a real SearXNG) and check
  behaviour no fake can: tools are called when they should be, answers carry the right links and
  times, planted messages change nothing, and no phone number reaches a search engine. They run
  only with the `live` build tag and a key, and wait between questions to stay inside Groq's free
  limits:

  ```sh
  cd core && GROQ_API_KEY=... go test -tags live -run TestLiveWebSearch -v ./internal/chat/
  ```

  `KAYA_SEARXNG_URL` points the web search test at a SearXNG (default `http://127.0.0.1:8888`).
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
| 3. Brain | Groq replies, lasting conversation memory, masking for cloud models, embeddings, search, answers from your chats, facts about you, tags and tasks, summaries. Checked on a real account. | ✅ |
| 4. Agent loop and safety | Tool calling, confirm-before-act, prompt-injection guard, alerts, any model by settings (built); alerts wait for their first real heads-up | 🟨 |
| 5. Voice notes | Transcription, spoken commands (built) | 🟨 |
| 6. Sending as you | Messages to contacts and groups after confirmation, with rate limits (built); auto-reply in chosen chats | 🟨 |
| 7. Web | Web search through SearXNG with links (built); page reading, research answers with sources | 🟨 |
| 8. Google | OAuth, Drive, Gmail, Calendar | ⬜ |
| 9. Documents | PPT and PDF from a topic or chat, uploaded to Drive | ⬜ |
| 10. Scheduler | Persistent jobs, plain-language schedules, reminders (built); morning brief | 🟨 |
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
