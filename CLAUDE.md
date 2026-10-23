# CLAUDE.md

Guidance for Claude Code when working in this repository.

## Read first, in this order

1. [`AGENTS.md`](AGENTS.md) — authoritative agent guidance: safety rules, task order, scope restrictions. If anything disagrees with this file, `AGENTS.md` wins.
2. [`docs/engineering-standards.md`](docs/engineering-standards.md) — engineering practices for this repository.
3. [`docs/specs/`](docs/specs/) — normative contracts (`wire-format-v1.md`, `postgresql-persistence-v2.md`). Do not change spec content without owner approval.
4. [`README.md`](README.md) — project status, provenance and build commands.

Task documents and phase reports live in [`docs/wip/`](docs/wip/).

## Commands

- Build: `go build ./...`
- Test: `go test ./...`
- Vet: `go vet ./...`
