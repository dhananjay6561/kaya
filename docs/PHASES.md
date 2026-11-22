# Kaya — build phases

Each phase is small: one focused change that can be built, tested and used on its own, usually in
one or two sessions. Phases run in order unless marked optional. Everything up to stage 15 runs on
the Mac; stage 16 moves Kaya to the Victus.

Status key: ⬜ not started · 🟨 in progress · ✅ done

## Stage 0 — Foundation

| # | Phase | Done when | Status |
|---|---|---|---|
| 0.1 | Repo skeleton: Node/TS project, folder layout, lint and format, `npm run dev` | `npm run dev` starts and exits cleanly | ✅ |
| 0.2 | Monorepo layout: TypeScript moves to `core/` (npm workspace), shared tsconfig base, root Makefile, CI per service | `make lint test build` passes from the root | ✅ |
| 0.3 | Core config: `.env.example` with every setting, typed loader that fails fast on missing values, tests | Missing a required value gives a clear error at startup | ✅ |
| 0.4 | Gateway skeleton: Go module with `cmd/` and `internal/`, config from the same `.env`, golangci-lint, tests, CI job, Dependabot and CodeQL for Go | `make lint test build` covers Go too | ✅ |
| 0.5 | Compose base: `compose.yml` + `compose.mac.yml`, Postgres with pgvector, Dockerfiles for core and gateway (multi-stage, non-root), health checks | `docker compose up` brings the stack up healthy | ✅ |
| 0.6 | Migrations: dbmate with `db/migrations/`, first schema (chats, contacts, messages), run as a one-shot step before the services | Fresh db gets the schema automatically; rerun is a no-op | ✅ |
| 0.7 | Logging, errors and health in both services: structured JSON logs, `/health`, clean shutdown on SIGTERM | `/health` reports db status in both | ✅ |
| 0.8 | Provider interfaces in core (LLM, chat channel, telephony) with stubs chosen from config | Swapping a provider is a config change | ✅ |

## Stage 1 — WhatsApp gateway (Go)

| # | Phase | Done when | Status |
|---|---|---|---|
| 1.0 | Gateway ↔ core contract: event and send-request schemas in `contracts/`, types in Go and TypeScript, transport chosen | Both sides validate the same sample payloads | ✅ |
| 1.1 | whatsmeow connection: pair by QR (terminal or a small page) or pairing code; session in Postgres | Shows up under Linked devices and stays linked after a restart | 🟨 |
| 1.2 | Live ingest: store every new message (DMs, groups, own sent messages), contacts and group names | Messages sent from the phone appear in the db within seconds | 🟨 |
| 1.3 | History sync: store the history WhatsApp sends on first link, without duplicates | Older chats are in the db | ⬜ |
| 1.4 | Media: store metadata for images, voice notes and documents; download on request | A voice note has a row and can be fetched on demand | ⬜ |
| 1.5 | Connection health: reconnect with backoff, detect logout, publish connection-state events | Killing the network and restoring it reconnects on its own | ⬜ |
| 1.6 | Delivery to core and the send API: core gets an event for every message and can send through the gateway | A message on the phone reaches core; core sends a reply that arrives | ⬜ |

## Stage 2 — Talking to Kaya (self-chat)

| # | Phase | Done when | Status |
|---|---|---|---|
| 2.1 | Command channel: only DJ's messages in "Message yourself" are commands, everything else is data; Kaya never treats its own replies as commands | Kaya echoes commands in the self-chat and ignores everything else | ⬜ |
| 2.2 | Built-in commands without any LLM: `/help`, `/status`, `/pause`, `/resume` | `/pause` stops all replies until `/resume` | ⬜ |

## Stage 3 — Brain (Groq)

| # | Phase | Done when | Status |
|---|---|---|---|
| 3.1 | Groq client behind the LLM interface (OpenAI-compatible); routine and heavy models from `.env`; plain chat replies | Kaya answers a plain question through Groq | ⬜ |
| 3.2 | Embeddings (provider per the open question in DECISIONS.md): embed messages into pgvector on ingest, plus a backfill job for history | Every stored text message has an embedding | ⬜ |
| 3.3 | Meaning-based chat search | "Chats about money" finds them even without the word "money" | ⬜ |
| 3.4 | Q&A over chats with sources (who, which chat, when) and contact name matching | "What did Rahul say about Saturday?" gets a correct, sourced answer | ⬜ |
| 3.5 | Tagging on ingest: important / ignore / task / reminder | New messages get tags; `/tasks` lists open ones | ⬜ |
| 3.6 | On-demand summaries: `/summary` for today or for one chat | Summary of a busy group is accurate and short | ⬜ |

## Stage 4 — Agent loop and safety

| # | Phase | Done when | Status |
|---|---|---|---|
| 4.1 | Optional, later: a second provider (Ollama local or Claude) behind the same interface | A config switch changes the provider | ⬜ |
| 4.2 | Masking rules for phone numbers, emails, OTPs, card/account numbers, addresses, with tests | Tests pass on real-looking samples | ⬜ |
| 4.3 | Optional, later (needs 4.1): router between local and cloud per request; `/local-only` switch | Simple asks stay local; local-only sends nothing out | ⬜ |
| 4.4 | Context picking: only the relevant messages are sent, and masking runs before anything leaves | The LLM only receives the masked slice a task needs | ⬜ |
| 4.5 | Agent loop with a tool registry (tool calling on Groq's gpt-oss models) | The model can call a test tool and use the result | ⬜ |
| 4.6 | Confirm-before-act: pending actions, yes/no reply, expiry; every outward tool must go through it | An outward action waits for "yes" and expires if ignored | ⬜ |
| 4.7 | Prompt-injection guard: chat text wrapped and marked as data; test cases with hostile messages | Planted "ignore previous instructions" messages change nothing | ⬜ |
| 4.8 | Usage and cost tracking, `/usage` | Shows today's and this month's API spend | ⬜ |

## Stage 5 — Voice notes

| # | Phase | Done when | Status |
|---|---|---|---|
| 5.1 | Whisper service (faster-whisper small, CPU) in Compose | A voice-note file comes back as text | ⬜ |
| 5.2 | DJ's voice notes to Kaya are commands; other voice notes are transcribed in the background and become searchable | Speaking a command works; a friend's voice note answers a Q&A | ⬜ |

## Stage 6 — Sending as DJ

| # | Phase | Done when | Status |
|---|---|---|---|
| 6.1 | Send to a contact or group: Kaya shows the draft, sends on "yes", with rate limits | "Tell Rahul I'll be late" sends only after confirmation | ⬜ |

## Stage 7 — Web

| # | Phase | Done when | Status |
|---|---|---|---|
| 7.1 | SearXNG in Compose + search tool | Kaya returns search results | ⬜ |
| 7.2 | Page fetch with readable-text extraction | Kaya summarises a pasted link | ⬜ |
| 7.3 | Research answers with sources | A research question gets an answer with links | ⬜ |

## Stage 8 — Google

| # | Phase | Done when | Status |
|---|---|---|---|
| 8.1 | Google Cloud project, OAuth login flow, token storage and refresh (watch the 7-day expiry in Testing) | Tokens survive restarts and refresh | ⬜ |
| 8.2 | Drive: upload a file, create a share link | A test file lands in Drive and the link opens | ⬜ |
| 8.3 | Gmail: search and read; drafts and send behind confirm | "Any mail from X today?" works | ⬜ |
| 8.4 | Calendar: read events; create events behind confirm | "Am I free Saturday?" works | ⬜ |

## Stage 9 — Documents

| # | Phase | Done when | Status |
|---|---|---|---|
| 9.1 | Docs service (Python, python-pptx, LibreOffice) with a small HTTP API, in Compose | Service builds a test deck | ⬜ |
| 9.2 | Deck outline from a topic (Claude) | Outline has sensible slides and notes | ⬜ |
| 9.3 | PPTX from the outline with a clean theme | Deck opens and looks presentable | ⬜ |
| 9.4 | PDF export | PDF matches the deck | ⬜ |
| 9.5 | End to end: topic from a chat → deck + PDF → Drive → link on WhatsApp | The workshop example works from one message | ⬜ |

## Stage 10 — Scheduler

| # | Phase | Done when | Status |
|---|---|---|---|
| 10.1 | Persistent job store; missed runs handled after downtime | Jobs survive restarts | ⬜ |
| 10.2 | Jobs from plain language ("every morning 8am send me X"), `/jobs`, cancel | Created, listed and cancelled from chat | ⬜ |
| 10.3 | Reminders and a morning brief (summaries, calendar, open tasks) | Brief arrives on time | ⬜ |

## Stage 11 — Browser

| # | Phase | Done when | Status |
|---|---|---|---|
| 11.1 | Browser service: Playwright Chromium with a persistent profile + noVNC | Browser screen opens in a web page | ⬜ |
| 11.2 | Manual login: Kaya sends the noVNC link, DJ logs in once, session is kept | A site stays logged in after a restart | ⬜ |
| 11.3 | Browser tools for the agent: open, read, click, type, screenshot back to WhatsApp | Kaya completes a simple task on a logged-in site | ⬜ |
| 11.4 | Guardrails: never types passwords, stops before any payment or submit that commits DJ, site allowlist | Guardrail tests pass | ⬜ |

## Stage 12 — Ordering

| # | Phase | Done when | Status |
|---|---|---|---|
| 12.1 | Fill a cart on one site and stop at checkout with a summary | Cart is right and nothing is paid | ⬜ |
| 12.2 | Pay on "yes", with per-order and per-day spending caps, and an order log | Over-cap orders are refused | ⬜ |
| 12.3 | Order status checks | "Where's my order?" works | ⬜ |

## Stage 13 — Phone calls

| # | Phase | Done when | Status |
|---|---|---|---|
| 13.1 | Pick the telephony provider and get a number (KYC); no code | Number active | ⬜ |
| 13.2 | Voice service (Pipecat) in Compose + temporary tunnel; answers a test call | Test call connects and speaks | ⬜ |
| 13.3 | Outbound call with a fixed script and AI disclosure | Kaya calls DJ's phone and says who it is | ⬜ |
| 13.4 | Live conversation: streaming STT → Claude → ElevenLabs, with the brief and boundaries | Short call stays on topic and declines off-brief asks | ⬜ |
| 13.5 | Confirm before dialling; transcript and one-line summary back on WhatsApp | Full "call Rahul" flow works | ⬜ |
| 13.6 | Webhook signature checks and call limits | Unsigned requests are rejected | ⬜ |

## Stage 14 — Kaya as a contact (spare SIM)

| # | Phase | Done when | Status |
|---|---|---|---|
| 14.1 | Second gateway device on the spare number; command channel switched by config | DJ chats with "Kaya" as a contact | ⬜ |
| 14.2 | Kaya messages first (briefs, reminders, alerts) | Morning brief arrives from Kaya | ⬜ |

## Stage 15 — Hardening

| # | Phase | Done when | Status |
|---|---|---|---|
| 15.1 | Alerts with a fallback channel (Telegram bot or email) when WhatsApp is down | Logout alert arrives on the fallback | ⬜ |
| 15.2 | Test set for the router, masking and Q&A that runs before every change | One command runs it | ⬜ |
| 15.3 | Optional: Uptime Kuma or a simple status page | Health of every service visible | ⬜ |

## Stage 16 — Hosting on the Victus

| # | Phase | Done when | Status |
|---|---|---|---|
| 16.1 | Mint host setup script: NVIDIA driver, Docker, nvidia-container-toolkit, Tailscale, UFW, sleep and lid settings | Script runs clean on a fresh Mint | ⬜ |
| 16.2 | `compose.victus.yml`: Ollama with the GPU, named Cloudflare tunnel, `/srv/kaya` | Stack runs on the Victus | ⬜ |
| 16.3 | Check the chosen local model fits 4 GB VRAM; switch model if not | Q&A speed acceptable on the 3050 Ti | ⬜ |
| 16.4 | Backups with restic, plus one test restore | Restore brings back a working db | ⬜ |
| 16.5 | Move day: migrate the db, re-pair WhatsApp, log into the browser profile, switch the tunnel | Kaya runs from the Victus and the Mac copy is off | ⬜ |
