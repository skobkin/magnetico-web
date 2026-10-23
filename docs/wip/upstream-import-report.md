# Upstream import report — task 01

Task: [`docs/wip/01-import-go-upstream.md`](01-import-go-upstream.md) — pinned import of
`tgragnato/magnetico` into `refactor/0.8.0-unify-and-new-storage`. Executed 2026-10-23.
No push of anything except the single end-of-task push of this branch; `master`, tags and
`origin` configuration untouched.

## Commits

| Commit | Content |
|---|---|
| `fa7a749c0945148c86b7373e2608e428953b8810` | **Commit A** — two-parent merge: upstream snapshot + seed docs; CI placeholder swap (see below) |
| `c459c09c` | **Commit B** — README rewritten with provenance (only `README.md` changed vs. A) |
| HEAD after the report commit | this report |

Parents of Commit A (`git rev-list --parents -n 1`):

```
fa7a749c0945148c86b7373e2608e428953b8810 5521441a1bf8269631c26ac25c82c76a13ed8ec9 f80660653e7d1045b95d020a60b12a08e9a3c9b3
```

Both ancestries visible in `git log --graph`: the magnetico-web development history via
parent 1, the upstream Go ancestry via parent 2.

## Recorded provenance

- Branch: `refactor/0.8.0-unify-and-new-storage`; `origin` = `ssh://git@git.skobk.in/skobkin/magnetico-web.git`.
- Upstream remote added locally as `upstream` = `https://github.com/tgragnato/magnetico.git`
  (lives only in local `.git/config`; remote configuration is not pushed).
- **Pinned upstream SHA:** `f80660653e7d1045b95d020a60b12a08e9a3c9b3`
  (`2026-10-19 23:46:17 +0200` — *"build(deps): bump github.com/klauspost/compress from 1.20.1 to 1.20.3"*),
  verified reachable after fetch and still `main` head via `git ls-remote` at execution time.
- Seed commit (pre-import HEAD): `5521441a1bf8269631c26ac25c82c76a13ed8ec9`.
- PHP removal commit: `5b988e8bba6e314c0f7fee5fe2b38062c6a1c40b`; last PHP code commit:
  `3d2aa69700fbd4ba4f888338b45e5d2c098a1a85` (recoverable on `master`).

## Git strategy

Preferred approach from the task doc, used as-is: **union merge of unrelated histories**.
Pre-merge proofs: `comm -12` of the two `ls-tree -r --name-only` listings was empty
(zero path overlap between seed tree and upstream tree) and the upstream tree contains no
gitlinks — therefore the merge could only produce the clean union, and it did
(“Automatic merge went well; stopped before committing”, 177 staged adds, no conflicts).
The only additional staged changes inside the merge were the owner-approved rewrites of
`.woodpecker/ci.yaml` (PHP pipeline → minimal Go placeholder; `docker-publish` step
commented out with `DOCKER_LOGIN`/`DOCKER_TOKEN`/`DOCKER_REPO` secrets usage preserved for
later reuse) — the file was kept, not deleted, per owner decision.

## Changed-file inventory

- **Added:** 177 upstream paths (`.github/`, `bencode/`, `dht/`, `doc/`, `merkle/`,
  `metadata/`, `metainfo/`, `opflags/`, `persistence/`, `stats/`, `types/`, `web/`,
  `.gitignore`, `.golangci.yml`, `CLAUDE.md`, `COPYING`, `Dockerfile`, `README.md`,
  `go.mod`, `go.sum`, `main.go`).
- **Modified in Commit A:** `.woodpecker/ci.yaml` (Go placeholder, publish commented out).
- **Modified in Commit B:** `README.md` (rewritten; upstream version replaced).
- No PHP/Symfony runtime files, no `.woodpecker` PHP steps, no stale MIT root license.

## Verification results

All checks below were executed and passed:

- `git status --short` clean after every commit; no phantom CRLF status noise appeared.
- Seed preservation: `git diff 5521441..A -- AGENTS.md UPGRADE.md docs` empty **and**
  per-file blob-OID equality over all 8 protected paths (`AGENTS.md`, `UPGRADE.md`,
  2×`docs/specs/`, 4×`docs/wip/`) — all identical.
- Upstream fidelity: `git diff --name-status f8066065..A` contains **exactly 9 `A` entries**
  (the 8 seed files + `.woodpecker/ci.yaml`) and nothing else — every one of the 177
  upstream paths is byte-identical to the pinned commit. The exclusion-form diff
  (`-- . ':(exclude)AGENTS.md' ':(exclude)UPGRADE.md' ':(exclude)docs'`) shows only the
  added `.woodpecker/ci.yaml`.
- `git diff 5521441..A`: exactly 177 `A` + 1 `M` (`.woodpecker/ci.yaml`).
- `git show A:go.mod`: module path `tgragnato.it/magnetico/v2` unchanged (as required for
  the snapshot commit; renaming belongs to task 02).
- PHP recoverability: `git show 3d2aa69:README.md`,
  `git show 3d2aa69:templates/Account/account.html.twig`, `git show 3d2aa69:LICENSE`
  (MIT) all still retrievable; 0 PHP-era paths in the new HEAD tree.

## License / attribution note

Upstream (both `boramalper/magnetico` and `tgragnato/magnetico`) is **AGPL-3.0**; its
`COPYING` and notices are imported unchanged. The historical PHP application was MIT; its
license survives only in Git history and is **not** presented as covering the imported Go
code. README (Commit B) states both facts explicitly. The pre-existing root `LICENSE`
was already removed by the owner's PHP cleanup commit, so no stale MIT-only root license
exists. No license conflicts discovered. The imported upstream `CLAUDE.md` is left
unmodified as part of the raw snapshot, for review in task 02.

## Toolchain and build/test outcomes

- `git version 2.56.0`; local toolchain `go version go1.27.1-X:nodwarf5 linux/amd64`
  (satisfies upstream `go 1.26.0` / `toolchain go1.27.0`; no toolchain download needed).
  Host had gcc/pkg-config/libzmq preinstalled, so cgo dependencies compiled natively.
- `go build ./...` → **exit 0** (all packages compile).
- `go test ./...` → **exit 1**; upstream test failures, recorded without fixes per the
  task instructions:
  - `tgragnato.it/magnetico/v2/persistence`: `TestSqlite3Database_GetNumberOfQueryTorrents`
    panics with a nil-pointer dereference at `persistence/sqlite3_test.go:126` (inside the
    upstream test). Deterministic here. (Note for task 02: the SQLite driver and its tests
    are scheduled for removal anyway.)
  - `tgragnato.it/magnetico/v2/web`: deterministic nil-pointer panic at
    `web/statistics.go:123` reached via `TestAPIStatistics/valid_request`
    (`statistics_test.go:89`). Because the panic aborts the whole test binary, the
    `FAIL`ed test name varies between runs (`TestApiTorrent` in one full run); the panic
    site is reproducible every time.
  - All other packages pass: `bencode`, `dht`, `dht/mainline`, `merkle`, `metadata`,
    `metadata/btconn`, `metainfo`, `opflags`, `stats`, `types/infohash`,
    `types/infohash-v2` (root package has no test files).
- No database connections, schema migrations, deployments or production services were
  touched (the schema-update log lines above come from upstream tests using an in-test
  SQLite database).

## CI note

`.woodpecker/ci.yaml` now contains a single `build-and-test` step
(`golang:1.27-alpine`, verified to exist on Docker Hub, multi-arch) plus the commented-out
`docker-publish` step retaining the existing secret references. Consequences: Woodpecker
still gates this branch (now with Go build/test), publishing stays disabled, and Forgejo
Actions is disabled for this repository per the owner, so the imported upstream
`.github/workflows` are inert. The Go CI will be properly adapted in a later task.

## Blockers

None. Phase 1 is complete; stopping here for owner review before task 02.
