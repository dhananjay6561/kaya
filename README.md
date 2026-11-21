# kaya

A self-hosted personal agent that lives on WhatsApp and runs on my own laptop.

- [Phases](docs/PHASES.md): build plan in small phases
- [Spec](docs/SPEC.md): what it does, safety rules
- [Infra](docs/INFRA.md): services, networking, data layout, host setup
- [Decisions](docs/DECISIONS.md): what is settled and what is still open

## Development

Needs Node 24.

```sh
npm install
npm run dev        # run from source
npm run build      # compile to dist/
npm start          # run the build
npm run typecheck
npm run lint       # biome lint + format check
npm run format     # fix what biome can
```

## License

[MIT](LICENSE)
