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
make down          # stop the stack (data stays in DATA_DIR)
make migration name=add_tasks   # new empty migration in db/migrations
make check         # lint, typecheck, test and build everything (what CI runs)
make help          # list every target
```

Copy `.env.example` to `.env` and fill in the required values first.

How the code is organised: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## License

[MIT](LICENSE)
