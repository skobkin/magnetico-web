# CLAUDE.md

Guidance for Claude Code when working in this repository.

## Read first, in this order

1. [`AGENTS.md`](AGENTS.md) — authoritative agent guidance: safety rules, task order, scope restrictions. If anything disagrees with this file, `AGENTS.md` wins.
2. [`docs/engineering-standards.md`](docs/engineering-standards.md) — engineering practices for this repository.
3. [`docs/specs/`](docs/specs/) — normative contracts (`wire-format-v1.md`, `postgresql-persistence-v2.md`). Do not change spec content without owner approval.
4. [`README.md`](README.md) — project status, provenance and build commands.

Task documents and phase reports live in [`docs/wip/`](docs/wip/).

## Commands

- Build: `go build ./...`; the executable: `go build -o magnetico ./cmd/magnetico` (`magnetico --version` needs no configuration)
- Test: `go test -p 3 ./...` (needs a disposable trust-auth PostgreSQL via `MAGNETICO_TEST_POSTGRES_DSN`; set
  `MAGNETICO_TEST_REQUIRE_POSTGRES=1` so a missing database fails instead of skipping). Classes and exact commands: [`docs/testing.md`](docs/testing.md)
- Vet: `go vet ./... && go vet -tags loadtest,diag ./...`
- Lint: `golangci-lint fmt --diff ./... && golangci-lint run ./...` (must report 0 issues)
- CI, tags, image and secrets: [`docs/release-engineering.md`](docs/release-engineering.md); running the image: [`docs/container.md`](docs/container.md)
