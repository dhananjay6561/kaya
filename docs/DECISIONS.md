# Kaya — decisions

Newest at the bottom. When a decision changes, mark the old one superseded instead of deleting it.

## Settled (2026-11-20)

| # | Decision | Why |
|---|---|---|
| 1 | Build our own agent instead of running OpenClaw | Full control over what it can touch, since it reads chats and can spend money. OpenClaw has had security problems with exposed instances and untrusted community skills. |
| 2 | Host on DJ's Victus (Linux Mint), Docker Compose | DJ owns it outright, and it's dedicated to this |
| 3 | Agent hub in Node/TypeScript | Baileys (the WhatsApp library) is Node |
| 4 | WhatsApp via Baileys as a linked device on DJ's number (QR or pairing code) | The official Business API can't read personal chats. This is also how the real IRA did it: DJ scanned a QR. |
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

## Open questions

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
