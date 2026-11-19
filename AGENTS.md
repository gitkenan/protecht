# Agents

protecht is an early Go prototype: devices register, upload hashed evidence, and submit events that reference that evidence. The server stores both and serves an activity timeline as HTML and JSON.

The HTTP contract, run instructions, and current assumptions live in the [README](README.md). The package boundaries are in [docs/architecture.md](docs/architecture.md). Significant changes go in [CHANGELOG.md](CHANGELOG.md). Do not copy those documents here.

## Commands

Requires Go 1.23+. The only third-party dependency is `github.com/mattn/go-sqlite3` (cgo).

```sh
go test ./...
go build ./cmd/server ./cmd/device ./cmd/agent
```

## Conventions

- Read the relevant code and documentation before changing them.
- Write unit tests before the functional code. Red, then green, then refactor.
- Update documentation when a change makes it inaccurate, or when it introduces architectural knowledge worth keeping. Add detail only when the work creates a real need for it.
- Update the changelog for significant behavioural, API, or architectural changes. Not for every edit.
- Record an architectural decision when one is actually made. Do not pre-create ADRs for hypothetical choices.
- Run the tests that cover the change, and report what was actually verified.
- Keep each fact in one place. Link to it instead of restating it.
- Do not add documentation, abstractions, or infrastructure without a present need.
