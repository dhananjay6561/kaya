# Kaya — infrastructure

## Environments

Kaya is developed and run on DJ's Mac for now. The Victus is the later home. Moving there should
mean copying `.env` and running the deploy kit, not changing code.

| | Mac (now) | Victus (later) |
|---|---|---|
| Compose | `compose.yml` + `compose.mac.yml` | `compose.yml` + `compose.victus.yml` |
| Data folder (`DATA_DIR`) | `./data` | `/srv/kaya` |
| Ollama | Native app (Docker on macOS has no GPU), `LLM_BASE_URL=http://host.docker.internal:11434` | Container with the NVIDIA GPU, `LLM_BASE_URL=http://ollama:11434` |
| Voice webhooks | Temporary Cloudflare tunnel | Named Cloudflare tunnel |
| Remote access, firewall, backups | Not needed | Tailscale, UFW, restic |
| Uptime | Stops when the Mac sleeps | Always on |

Rules that keep it portable:

- Every host, port, path, key and provider choice comes from `.env`; `.env.example` lists them all.
- The LLM, Claude, telephony and the WhatsApp chat option (self-chat / spare SIM / Cloud API) each
  sit behind one interface and are chosen in config.
- Database migrations run on startup.
- `deploy/` holds the Victus kit: Mint host setup script, compose override, backup config and a
  hosting-day checklist.

Can't be done ahead of hosting day: re-pairing WhatsApp on the Victus (fresh QR is cleaner than
copying the session), logging into the browser profile once, and checking that the chosen local
model fits the 3050 Ti's 4 GB VRAM.

## Hardware

HP Victus laptop, used only for this (once hosted).

- GPU: RTX 3050 Ti, 4 GB VRAM
- RAM: 24 GB
- Disk: 1 TB SSD
- OS: Linux Mint

## Overview

```
                    ┌──────────── Internet ─────────────┐
   Your phone       │  Claude API   ElevenLabs/Deepgram │   Exotel/Plivo
   (WhatsApp)       │  Google APIs  shopping sites      │   (phone number)
        │           └───────▲───────────▲───────────────┘        │
        │ linked device     │ outbound  │                         │ calls + audio
        │ (outbound only)   │ only      │                         ▼
┌───────┼───────────────────┼───────────┼──────── Victus · Linux Mint ──────────┐
│       ▼                   │           │                  ┌──────────────┐     │
│  ┌──────────────────────────────┐     │                  │ cloudflared  │◀────┘
│  │ core (TS) — the agent hub     │     │                  │ (tunnel: only│
│  │ • WhatsApp via gateway (Go)  │─────┘                  │  /voice/*)   │
│  │ • router: local or Claude?   │                        └──────┬───────┘
│  │ • PII masking rules          │                               ▼
│  │ • confirm-before-act         │◀──── call result ───── ┌──────────────┐
│  │ • scheduler (cron jobs)      │───── "call Rahul" ───▶ │ voice        │
│  └──┬──────┬──────┬──────┬──────┘                        │ (Pipecat)    │
│     ▼      ▼      ▼      ▼                               └──────────────┘
│  ┌──────┐┌──────┐┌───────┐┌──────────┐┌──────────┐┌─────────┐
│  │ollama││ db   ││whisper││ browser  ││ docs     ││ searxng │
│  │ GPU  ││ PG + ││ CPU   ││Playwright││ pptx/pdf ││ search  │
│  │ 4B + ││pgvec.││ voice ││ + noVNC  ││ → Drive  ││         │
│  │ embed││      ││ notes ││          ││          ││         │
│  └──────┘└──────┘└───────┘└──────────┘└──────────┘└─────────┘
│
│  Host: Tailscale (only way in) · UFW (block all incoming) · restic backups
└───────────────────────────────────────────────────────────────────────────────┘
```

## Services (one Docker Compose file)

| # | Service | Job | Runs on | RAM |
|---|---|---|---|---|
| 1a | gateway | Go + whatsmeow: WhatsApp connection, pairing, receiving and sending | CPU | ~0.1 GB |
| 1b | core | TypeScript agent: commands, LLM calls, tools, confirmations, scheduler | CPU | ~1 GB |
| 2 | ollama | Local ~4B model (Qwen3 4B / Gemma 3 4B class) + embedding model | GPU, ~3.5 GB VRAM | ~1 GB |
| 3 | db | Postgres + pgvector: chats, tasks, call logs, embeddings | CPU | ~0.5–1 GB |
| 4 | whisper | Voice-note transcription (faster-whisper small) | CPU | ~1 GB |
| 5 | browser | Chromium with logged-in profiles; noVNC for manual login/OTP | CPU | ~2–4 GB |
| 6 | docs | Python + LibreOffice: PPT/PDF, Drive upload | CPU | ~0.5 GB, spikes |
| 7 | voice | Pipecat: live calls (cloud STT, Claude, ElevenLabs) | CPU | ~0.5–1 GB |
| 8 | searxng | Self-hosted web search | CPU | ~0.3 GB |
| 9 | cloudflared | Public entry, only for the phone provider's `/voice/*` routes | CPU | ~50 MB |
| | **Total** | | | **~8–11 of 24 GB RAM, ~3.5 of 4 GB VRAM, under 20 GB disk** |

Whisper stays on the CPU so the GPU is free for the LLM. Services are added phase by phase. Phase 1
needs only gateway, core and db.

## WhatsApp connection

Two separate connections:

**1. Reading DJ's chats (linked device).** The gateway (Go, whatsmeow) becomes a linked device on DJ's
own number, like WhatsApp Web. Pairing is by QR (served on a small page over Tailscale, or in the
terminal) or by an 8-character pairing code ("Link with phone number instead"), which is easier on
a headless box. The session is stored in Postgres through whatsmeow's session store and survives restarts.

- On first link it syncs recent history; WhatsApp decides how far back. After that, everything
  arrives live, including messages DJ sends from his phone.
- It uses one of the 4 linked-device slots.
- If the phone is offline for ~14 days, WhatsApp logs out all linked devices.
- Removing it under Linked devices on the phone cuts access at once (kill switch).
- On logout, the gateway reports it and core alerts DJ through the fallback channel and he re-pairs.

**2. The chat DJ talks to Kaya in.**

| Option | How | Trade-off |
|---|---|---|
| A. Self-chat | Kaya replies in DJ's "Message yourself" chat using connection 1 | Free, no setup; doesn't feel like a separate contact |
| B. Spare SIM | Second gateway device on a cheap prepaid number saved as "Kaya" | Feels like a real contact and can message first; ban risk sits on the spare number |
| C. WhatsApp Business Cloud API | Official business number via Meta webhooks through the tunnel | No ban risk; business setup, paid templates for messages it sends first, 24-hour reply window |

Plan: start with A, move to B once things work, C only if Kaya ever becomes a product for others.
With B, a message is a command only when it comes from DJ's number to Kaya.

Keeping ban risk low: no bulk sending from the main number, keep the main number mostly read-only,
set a proper device name, back off slowly on reconnects.

## Networking and security

- **Incoming:** UFW denies all. No router port forwards.
- **DJ's access:** Tailscale only, for SSH (keys only) and the noVNC browser screen.
- **Public entry:** the Cloudflare Tunnel exposes only the voice service's webhook and audio routes,
  and every request is checked against the provider's signature.
- **Containers:** talk over a private Docker network. Only cloudflared faces the internet.
- **Secrets:** `.env` with `chmod 600` (or Docker secrets), never in git.
- **Disk:** full-disk encryption (LUKS), chosen in the Mint installer.
- **Updates:** `unattended-upgrades` for OS patches. Container images updated by hand monthly.

## Data layout on the Victus

```
/srv/kaya/
├── postgres/         chats, tasks, call transcripts, embeddings, WhatsApp session
├── browser-profile/  logged-in sessions (most sensitive folder)
├── models/           ollama + whisper models (~5 GB)
├── files/            generated decks/PDFs before Drive upload
├── compose.yml
└── .env              keys (chmod 600)
```

**Backups:** restic nightly, encrypted, to Drive or Backblaze B2. Covers a `postgres` dump (which
includes the WhatsApp session) and `browser-profile`. Keep 7 daily and 4 weekly. Models are not backed up.

## One-time host setup (Mint)

1. Install Mint with LUKS encryption.
2. NVIDIA driver, Docker, `nvidia-container-toolkit` (so the ollama container gets the GPU).
3. Disable sleep/suspend; set `HandleLidSwitch=ignore` in `/etc/systemd/logind.conf`.
4. BIOS: cap battery charge at ~80% if HP offers it; turn on "power on after AC loss" if present.
5. Tailscale, UFW (`deny incoming`), SSH keys only.
6. Wired Ethernet if possible; laptop stand for airflow.

## Reliability

- Every container: `restart: unless-stopped` and a health check.
- Alerts on WhatsApp for logouts, API failures, disk over 80%. If WhatsApp itself is down, fall back
  to a Telegram bot or email.
- If the internet drops, WhatsApp messages sync on reconnect and scheduled jobs retry.
- Optional Uptime Kuma dashboard over Tailscale.

## Accounts and keys

| What | For | Note |
|---|---|---|
| Anthropic API key | Claude | Billed per use |
| Google Cloud project + OAuth client | Drive, Gmail, Calendar | In "Testing" status, refresh tokens expire after ~7 days |
| Tailscale | Remote access | Free tier |
| Backup storage | restic | Drive or Backblaze B2 |
| Telephony number | Calls (Phase 8) | Indian virtual numbers need KYC; some providers sell only to businesses. Check this first. |
| ElevenLabs, Deepgram | Calls (Phase 8) | DJ already uses ElevenLabs |

## How a request flows

**"What did Rahul say about Saturday?"**
WhatsApp → gateway → core → router: simple → ollama searches the db by meaning and answers → reply. Nothing
leaves the laptop.

**"Make a workshop deck from the group chat and put it on Drive"**
WhatsApp → gateway → core → router: hard → ollama picks the relevant messages → masking rules strip private
details → Claude plans the deck → docs service builds PPT and PDF → Drive upload → link back on
WhatsApp.

**"Call Rahul and ask if Saturday 6pm works"**
Core finds the number → asks DJ to confirm → voice service dials from Kaya's number → STT → Claude →
ElevenLabs → transcript and summary back on WhatsApp ("Rahul said Saturday works, but 7pm, not 6").
