# Kaya — decisions

Newest at the bottom. When a decision changes, mark the old one superseded instead of deleting it.

## Settled (2026-11-20)

| # | Decision | Why |
|---|---|---|
| 1 | Build our own agent instead of running OpenClaw | Full control over what it can touch, since it reads chats and can spend money. OpenClaw has had security problems with exposed instances and untrusted community skills. |
| 2 | Host on DJ's Victus (Linux Mint), Docker Compose | DJ owns it outright, and it's dedicated to this |
| 3 | ~~Agent hub in Node/TypeScript, including WhatsApp~~ Superseded by #23 | Baileys (the WhatsApp library) is Node |
| 4 | WhatsApp as a linked device on DJ's number (QR or pairing code) | The official Business API can't read personal chats. This is also how the real IRA did it: DJ scanned a QR. |
| 5 | Hybrid models: local ~4B on Ollama is the gatekeeper, Claude API for multi-step work | 4 GB VRAM can't run a model good enough for long tool chains; local keeps raw chats on the laptop and cuts cost |
| 6 | Privacy = fixed masking rules + local model picking relevant messages | A 4B model alone misses things; fixed rules are predictable |
| 7 | Postgres + pgvector (supersedes "SQLite to start") | Several services share data, and pgvector gives meaning-based chat search |
| 8 | Whisper on the CPU | Leaves the GPU free for the LLM |
| 9 | Calls always use cloud STT + Claude + ElevenLabs | Live calls need sub-second replies; the local model is too slow |
| 10 | Kaya identifies as an AI on calls, never as DJ | Passing as a human deceives the person called |
| 11 | Confirm before any outward action (messages, payments, calls) | It has access to DJ's accounts and money |
| 12 | Tailscale is the only way in; Cloudflare Tunnel only for `/voice/*` | No open ports on a box holding all of DJ's sessions |
| 13 | Browser logins: DJ logs in once via noVNC, no stored passwords | Passwords never live on disk |
| 14 | Start talking to Kaya in the self-chat (option A); spare SIM (B) later | Zero setup to start; B makes Kaya a real contact once it works |

## Settled (2026-11-21)

| # | Decision | Why |
|---|---|---|
| 15 | Build and run on DJ's Mac for now; the Victus comes later. Everything stays ready to move: one Compose file plus per-machine overrides, all settings in `.env`, swappable providers, a `deploy/` kit for hosting day. | DJ wants it as a project first, with hosting a quick plug-in step later instead of a rebuild |
| 16 | On the Mac, Ollama runs natively, not in Docker | Docker on macOS can't use the GPU; Ollama's API is the same either way, so the agent only sees a different URL |

| 17 | Groq is the only LLM provider for now (OpenAI-compatible API, key in `.env` as `GROQ_API_KEY`). Partly supersedes #5: no local gatekeeper model yet. Ollama and Claude can be added later behind the same LLM interface. | DJ's call: simplest way to start. Trade-off: any chat text a request touches goes to Groq. The masking rules (#6) still run before every call. |
| 18 | Groq models: `openai/gpt-oss-120b` for multi-step and tool work, `openai/gpt-oss-20b` for routine work, both set in `.env` | Both have 131k context and tool calling on Groq (models checked 2026-11-21) |
| 19 | Public GitHub repo, one PR per phase, merged right away without waiting for CI, rebase-merge so the micro commits stay | DJ's workflow choice. The repo is public, so `.env` and anything with secrets or chat data must never be committed. |
| 20 | The project is named **Kaya** (replaces the working name "Ira"). Repo: `dhananjay6561/kaya`, public. | DJ, 2026-11-21 |
| 21 | MIT license | DJ's choice: permissive and the usual pick for personal open source |
| 22 | GitHub hardening: ruleset on `main` (PR required with 0 approvals, linear history, no force push or deletion, no bypass), `v*` tag protection, secret scanning + push protection, Dependabot alerts/updates, CodeQL (extended), private vulnerability reporting, read-only Actions token, actions pinned to commit hashes, rebase-only merges, approval for outside contributors' workflow runs. CI is not a required check. | Secure public repo without slowing a solo, merge-right-away PR flow. Required checks or reviews would block DJ's own merges. |
| 23 | Split by language: `gateway` (Go + whatsmeow) owns the WhatsApp connection, `core` (TypeScript) is the agent, docs and voice are Python services. They talk only through contracts. Supersedes #3; #4 now uses whatsmeow instead of Baileys. | Checked 2026-11-21: since August whatsmeow had 100+ commits, Baileys 4, and Baileys is still on `7.0.0-rc14` from July. whatsmeow runs the production mautrix-whatsapp bridge. The WhatsApp link is the part that must not break, so it gets the best-maintained library; everything else stays where its libraries are strongest. |
| 24 | Monorepo: `core/`, `gateway/`, `services/<name>/`, `contracts/`, `db/migrations/`, `deploy/`, `docs/`. npm workspaces for TypeScript, a Go module in `gateway/`, a root Makefile as the one entry point. Full layout in `ARCHITECTURE.md`. | One repo and one PR per change across services; it grows by adding folders |
| 25 | One schema in `db/migrations/` as plain SQL, run by dbmate before services start. whatsmeow keeps its own session tables. | Two languages share the database; a language-neutral tool avoids two migration systems |
| 26 | WhatsApp session stored in Postgres through whatsmeow's session store, not in a `whatsapp-auth` folder | One thing to back up and to move to the Victus |
| 27 | Every message between services has a JSON Schema in `contracts/`, validated on both sides | Two languages can't share types directly; the schema is the single source |

## Settled (2026-11-22)

| # | Decision | Why |
|---|---|---|
| 28 | Containers: core on `distroless/nodejs24`, gateway on `distroless/static`, both as the non-root user with a read-only root filesystem and no capabilities. Postgres is `pgvector/pgvector` on Postgres 18. Every base image is pinned by version and digest, and Dependabot updates them. | Small images with no shell or package manager to exploit; digests make builds repeatable |
| 29 | Each service has a minimal `/health` from phase 0.5 so Compose health checks work; phase 0.7 adds db status. A `healthcheck` command inside each image calls it, since distroless images have no curl. | `docker compose up` can only report healthy if the services run and answer |
| 30 | Services bind to `127.0.0.1` by default (`LISTEN_HOST`); Compose binds `0.0.0.0` inside containers and publishes ports on `127.0.0.1` only | Nothing is reachable from the network unless Tailscale or the tunnel is set up for it |
| 31 | The gateway container gets only the settings it uses, not the whole `.env` | The service holding the WhatsApp session never sees API keys |
| 32 | Migrations run in a one-shot `migrate` Compose service (dbmate, as a non-root user, read-only); core and gateway start only after it succeeds. No `schema.sql` dump is committed. | Services never start against an old schema, and neither language runs migrations itself (#25). The migration files are the schema's single source. |
| 33 | First schema: `chats`, `contacts`, `messages`, keyed by WhatsApp JIDs stored as text. A message is unique by chat, sender and WhatsApp id, so live delivery and history sync can both upsert the same message without duplicates. Media columns come in phase 1.4. | Matches how WhatsApp identifies messages; tables grow when a phase needs them |
| 34 | Schema tests are plain SQL in `db/tests/`, run against the real Postgres in CI (`make test-db`) along with a no-op rerun and a rollback | Constraints are part of the contract between gateway and core, so they get tested like code |
| 35 | Logging with pino in core and `log/slog` in the gateway, both writing JSON to stdout with the same core fields and lowercase level names. Request ids travel in `X-Request-Id`. | Standard, fast libraries; one log shape makes it possible to follow a request across services |
| 36 | `/health` checks the db on every call (2 s limit) and answers 503 when it's down. Services don't exit when the db is unreachable; they start and report it. The response never includes error details. | Docker and later monitoring see a real readiness signal, a db restart doesn't crash-loop the services, and the endpoint can't leak internals |
| 37 | Core logs errors through an allowlist serialiser (type, message, stack, code and a few more, plus the cause chain) | `pg` attaches its whole client to errors; logging that is noisy and a leak risk |
| 38 | Database tests run when `TEST_DATABASE_URL` is set; CI starts a pgvector Postgres for the core and gateway jobs | Real-Postgres tests (ARCHITECTURE) without making every local test run need Docker |
| 39 | Providers in core follow one pattern: interface, one file per implementation, an exhaustive factory switch on a config enum that lists only built implementations, and a `stub` for each kind | Swapping is a config change, and the type checker catches a provider that is configurable but not wired |
| 40 | `LLM_PROVIDER` is `groq` or `stub` (default groq, unchanged). `GROQ_API_KEY` is required only for groq. Until phase 3.1 the groq provider exists but its calls reject with `ProviderNotReadyError`, so the default config keeps working. | Keeps decision #17 as the default while letting development and tests run with no key |
| 41 | `CHAT_CHANNEL` and `TELEPHONY_PROVIDER` start with `stub` only; the gateway channel arrives in 1.6 and a phone provider in stage 13 | No placeholder for a provider whose transport isn't decided yet (1.0, 13.1) |
| 42 | The chat channel is only the DJ ↔ Kaya conversation. Reading other chats goes through the database the gateway fills. | Keeps safety rule 3 (Kaya talks in one place) in the shape of the interface |

## Open questions

- **Gateway → core transport (phase 1.0):** HTTP webhooks, Postgres LISTEN/NOTIFY, or a small queue
  such as NATS. Must survive either side restarting without losing events.

- **Embeddings:** Groq has no embedding model. Options: a small local embedding model (Ollama
  `nomic-embed-text`, or one run inside the Node process) or a hosted embeddings API. Needed by
  phase 3.2.
- **Voice notes:** Groq hosts `whisper-large-v3-turbo`, which could replace the local Whisper service
  in stage 5.
- **Prompt-injection guard:** Groq hosts `meta-llama/llama-prompt-guard-2-86m`, a candidate for
  phase 4.7.

- **Spare SIM for Kaya?** Decides when option B is possible. Asked on 2026-11-20, not answered.
- **Agent framework:** Claude Agent SDK, or a plain Anthropic SDK tool loop with our own router in
  front. The local-first router makes the plain loop the likelier fit; decide at the start of Phase 1.
- **Exact local model (once a local model is added back):** Qwen3 4B, Gemma 3 4B or newer. Pick by
  what's current and what fits in ~3.5 GB VRAM.
- **Telephony provider:** Exotel vs Plivo vs Twilio, depending on whether DJ can get an Indian virtual
  number as an individual (KYC).
- **Fallback alert channel:** Telegram bot or email.
- **Backup target:** Drive or Backblaze B2.
