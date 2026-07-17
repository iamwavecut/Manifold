# Contributing

Open an issue before a large architectural change. Small fixes can go directly to a focused pull request.

## Development setup

```bash
go version
node --version
npm ci
make verify
```

Use Go 1.26 and Node 26. Keep changes narrow and follow [`AGENTS.md`](AGENTS.md).

For API changes:

1. Register the operation or schema in Huma.
2. Return only the shared semantic problem model.
3. Add negative tests and public error documentation.
4. Run `make openapi` and commit the generated contract.
5. Run `make openapi-check`.

For identity changes, preserve slug/XID/revision rules and add rename cascade tests. Do not introduce UUIDs.

For UI changes, run `npm run check` and `npm run build`. For skill changes, run `make skill-check`.

By contributing, you agree that your contribution is licensed under `AGPL-3.0-or-later`.
