# Development

How to build, check and test the repository. The rules for changes are in
[`engineering-standards.md`](engineering-standards.md); agent-specific guidance is in [`../AGENTS.md`](../AGENTS.md).

## Build

```sh
go build ./...                                 # everything
go build -o magnetico ./cmd/magnetico          # the executable
./magnetico --version                          # version, commit, Go version, platform (no config or database needed)
```

The container image and the release archives are built by CI; building the image locally is described in
[`container.md`](container.md#building-the-image-yourself).

## Vet and lint

```sh
go vet ./... && go vet -tags loadtest,diag ./...
golangci-lint fmt --diff ./... && golangci-lint run ./...   # must report 0 issues
```

## Test

```sh
go test -p 3 ./...                             # the whole regular suite
```

Integration tests bootstrap their own disposable PostgreSQL: a container from the locally cached `postgres:17` image,
an explicit `MAGNETICO_TEST_POSTGRES_DSN` pointing at a **trust-authentication** server, or local
`initdb`/`postgres` binaries. They skip when none is available unless `MAGNETICO_TEST_REQUIRE_POSTGRES=1` (CI sets
it), and they never touch any other server. `-p 3` is the verified parallelism.

Which tests run where (regular CI, release CI, explicit manual execution only), the exact commands for each and the
mapping of historical report commands: [`testing.md`](testing.md).

## Further reading

| Guide | Contents |
|---|---|
| [`testing.md`](testing.md) | Test classes, exact commands, heavy and manual-only tests |
| [`development-fixtures.md`](development-fixtures.md) | A synthetic torrent dataset and a local web UI for manual testing |
| [`engineering-standards.md`](engineering-standards.md) | Engineering practices for this repository |
| [`release-engineering.md`](release-engineering.md) | CI workflows, tags, the image, release publication and secrets |
| [`specs/`](specs/) | Normative contracts; changing them needs the owner's approval |
