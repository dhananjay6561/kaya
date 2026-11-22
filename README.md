# kaya

A self-hosted personal agent that lives on WhatsApp and runs on my own laptop.

- [Phases](docs/PHASES.md): build plan in small phases
- [Architecture](docs/ARCHITECTURE.md): services, repo layout, engineering standards
- [Spec](docs/SPEC.md): what it does, safety rules
- [Infra](docs/INFRA.md): services, networking, data layout, host setup
- [Decisions](docs/DECISIONS.md): what is settled and what is still open

## Development

Needs Node 24, Go 1.26, make and Docker with Compose. Run everything from the repo root.

```sh
make install       # install dependencies
make dev           # run core from source
make dev-gateway   # run the WhatsApp gateway from source
make db            # start only Postgres and apply migrations, for running services from source
make up            # build and run the whole stack in Docker, wait until healthy
make pair          # link Kaya to WhatsApp (QR code; PHONE=919876543210 for a pairing code)
make down          # stop the stack (data stays in DATA_DIR)
make migration name=add_tasks   # new empty migration in db/migrations
make test-db       # migrations and schema tests on a throwaway database
make check         # lint, typecheck, test and build everything (what CI runs)
make help          # list every target
```

Copy `.env.example` to `.env` and fill in the required values first. The gateway and core each log in
to Postgres with their own limited role; set `GATEWAY_DB_PASSWORD` and `CORE_DB_PASSWORD` (and change
`POSTGRES_PASSWORD`) before real chats go in.

Tests that need Postgres run when `TEST_DATABASE_URL` is set and are skipped otherwise (CI always
sets it). With `make db` running:

```sh
TEST_DATABASE_URL=postgres://kaya:kaya@localhost:5432/kaya make test
```

### Linking WhatsApp

`make pair` links Kaya to your WhatsApp as a linked device. It rebuilds the gateway image, stops
the gateway if it is running, shows a QR code to scan under WhatsApp > Settings > Linked devices >
Link a device, and afterwards starts the gateway again if it was running before. Without a camera handy, or on a headless box, use a pairing code:
`make pair PHONE=919876543210` (full international number). The device shows up as "Kaya".

The session is stored in Postgres and survives restarts. The gateway's `/health` shows the
connection under `info.whatsapp`. To unlink, remove Kaya under Linked devices on the phone; the
gateway notices on its next connection, drops the session and reports `logged_out`.

How the code is organised: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## License

[MIT](LICENSE)
