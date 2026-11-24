# Release engineering

How a tag becomes native binaries on Forgejo and a multi-platform image on Docker Hub and GHCR, what guards that path,
and what an operator must configure. Written for Task 08; read [`container.md`](container.md) for using the image and
[`testing.md`](testing.md) for the test classes.

## Configured versus verified

Nothing below has been published. This table is the honest state after Task 08.

| Item | State |
|---|---|
| `Dockerfile`, `.dockerignore`, `.goreleaser.yaml`, `.woodpecker/ci.yaml`, `.woodpecker/release.yaml`, the `ci/` tools | **configured and committed** |
| Image build (amd64 and arm64 in one invocation, SBOM + provenance), smoke test, image scan, promotion by digest | **verified locally**: built with `docker buildx`, `ci/image-smoke.sh` passed on the amd64 image (loaded from the layout and pulled back from a registry), Trivy reported 0 HIGH/CRITICAL findings for both platforms, `ci/promote-image.sh` with the real `crane` pushed the layout to two local registries with the same digest and both passed `ci/verify-published.sh` (`docs/wip/08-measurements/`) |
| arm64 image | **built, scanned, digest-verified; never executed** (no emulation or arm64 host was available) |
| Native archives | **verified locally**: a snapshot build and a build for a local, never-pushed tag in a scratch clone, `ci/native-smoke.sh`, reproducible checksums; also built inside the pinned `goreleaser/goreleaser:v2.20.1` image |
| Both Woodpecker workflows | **linted** with Woodpecker's own `woodpecker-cli lint` (v3.20.1: valid) and pinned by `internal/repocheck`; **never run on a Woodpecker server** |
| Pushes to Docker Hub, GHCR, Forgejo releases | **never performed** (no tag exists, no credentials were used) |
| `ci/ciguard` against a real Forgejo, the exact status-context name Woodpecker posts and that it records the pipeline URL as the status `target_url` (the guard fails closed if it does not), `docker load` into the `dind` service, the privileged plugin build on a real agent | **unverified**: they depend on the operator's Woodpecker/Forgejo/Docker versions; the first manual pre-flight run is the check |

## Roles

Forgejo (`git.skobk.in/skobkin/magnetico-web`) is the primary repository, GitHub (`skobkin/magnetico-web`) a mirror,
Woodpecker the CI. Forgejo hosts the **native release artifacts** (releases) but is **not** an OCI registry
publication target. Images go to `docker.io/skobkin/magnetico-web` and `ghcr.io/skobkin/magnetico-web` only.

## Tags

Existing tags are `0.2.0-symfony6-rc1` … `0.7.1` (PHP era; `0.7.0-rc1`, `0.7.0-rc2`, `0.7.1` are the latest). The Go line
continues the same numbering. New tags must match

```
^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$        e.g. 0.8.0-rc.0, 0.8.0-rc.1, 0.8.0, 0.8.1
```

(release candidates are spelled `-rc.N` with a dot, as in the task and as SemVer orders them; the historical `-rc1`
spelling is not accepted for new tags). A tag outside the grammar triggers **neither** workflow, so it can never publish.
A release-candidate tag publishes exactly its version tag (`0.8.0-rc.0`) and a GitHub-style **prerelease** on Forgejo;
a stable tag publishes the stable version and a stable release. **No floating alias is ever applied.**

## What runs when

| Event | `ci.yaml` (verification) | release gates (`release.yaml`) | Forgejo release | OCI publication |
|---|---|---|---|---|
| push to `master` or the 0.8.0 branch | yes | no | no | no |
| push to any other branch | no (its pull request verifies it) | no | no | no |
| pull request | yes, **merged state** (no secrets, no privilege, no registry) | no | no | no |
| tag matching the grammar | yes | yes, after `ci` | yes, after the gates | yes, after the gates |
| manual run | yes | yes (pre-flight) | **no** | **no** |

While a pull request from the 0.8.0 branch is open, that branch has both a push and a pull-request pipeline (the one
accepted duplication; remove `refactor/0.8.0-unify-and-new-storage` from the `push` list of `ci.yaml` after the merge).

## The release path: build once, qualify, publish exactly that

```
tag ─► ci.yaml (verification, same commit)
   └─► release.yaml
         require-verification   ci/ciguard: the ci workflow's status of THIS pipeline for CI_COMMIT_SHA (tag only)
         ├─ race                go test -race -p 3 + skip census          ┐
         ├─ govulncheck         pinned v1.10.0, both tag sets             │
         ├─ image-build         plugin-docker-buildx, dry_run, OCI layout │ gates
         │    ├─ image-smoke    docker load + ci/image-smoke.sh (amd64)   │
         │    └─ image-scan     Trivy HIGH/CRITICAL, amd64 and arm64      │
         └─ native-build ─► native-smoke   dist/ qualified               ┘
         promote-image          crane push of the SAME layout ► docker.io + ghcr.io (tag only)
         release-forgejo        goreleaser release ► Forgejo release       (tag only)
         verify-published       both digests == the qualified digest, 2 platforms, 2 attestations
         verify-published-smoke ci/image-smoke.sh on docker.io/…@digest
         verify-native          prerelease flag, assets, checksums.txt == qualified
```

Guarantees, and how each is enforced:

* **The published image is the qualified image.** `image-build` is the only build. Its output is an OCI layout
  (`.ci/image`, relative to the workspace: `CI_WORKSPACE` is runtime-only and cannot be substituted into a setting): one
  index holding `linux/amd64`, `linux/arm64` and two attestation manifests (SBOM and provenance, `mode=max`).
  `ci/check-attestations.sh` (in `image-scan`) requires an SPDX **and** a SLSA predicate in each attestation manifest —
  counting manifests cannot show an SBOM, provenance alone also yields two. The smoke test and the scanner read that layout; `promote-image` pushes it unchanged, so each registry
  reports exactly the qualified digest (the script fails otherwise, and `verify-published` checks again). The image
  digest differs between builds (provenance embeds timestamps), which is why nothing is rebuilt. `ci/promote-image.sh`
  never overwrites a release tag that already resolves to other bytes (checked for both repositories before anything
  is pushed) and is idempotent for the same digest; `ci/image-smoke.sh` judges the identity of `--version` (the tag
  and the commit the pipeline builds), not only its format. The plugin runs with
  `dry_run: true` against a non-routable `staged.invalid` repository: it cannot push anywhere.
* **The published archives are the qualified archives.** GoReleaser's OSS edition has no `publish` command, so
  `release-forgejo` rebuilds; builds are reproducible (the binary and archive timestamps are the commit's), the
  qualified `checksums.txt` is kept, and `verify-native` requires the published one to be byte-identical.
* **Publication needs verification of the same commit.** Workflow `depends_on: [ci]` (never optional: Woodpecker
  documents that a dependency that was not built blocks the dependent) and `ci/ciguard`, which reads the forge's commit
  statuses for exactly `CI_COMMIT_SHA` and succeeds only when the newest `ci/woodpecker/tag/ci` status **that belongs to
  the current pipeline** is `success`. A status belongs to it when its `target_url` is `CI_PIPELINE_URL` (or below it on a
  path-segment boundary, so a workflow id may follow; pipeline 1 is not 12). A green status of an **earlier pipeline for
  the same commit** (a tag re-run, two tags on one commit) therefore never substitutes, and the pipeline-level aggregate
  status, which may stay pending while the release workflow itself runs, is a different context and never counts.
  Failure, error, a missing or pending status, a status of another commit or pipeline, an empty or unusable
  `CI_PIPELINE_URL`, an API error, a non-tag event, a malformed tag or SHA all refuse. Every publication step depends on the guard and on all five gates.
* **Untrusted code never sees credentials.** `ci.yaml` (the only workflow pull requests reach) has no `from_secret`, no
  registry login, no cache, no privileged step and no Docker daemon; `release.yaml` is tag/manual only. Secrets are used
  by `promote-image` (registry credentials) and `release-forgejo` (Forgejo token) and nowhere else.
* **No registry cache exists.** A cache export was tried and failed locally (concurrent layer writes with two
  platforms), could not be rehearsed against the CI daemon, and every cache is a poisoning surface. Builds use BuildKit
  cache mounts only (effective on a persistent builder). A cache can be added later in tag-only steps.
* **The Go security floor is 1.27.4 everywhere.** The `golang:1.27.4-*` step images and the image's compiler stage use
  `GOTOOLCHAIN=local`; **the pinned GoReleaser image ships Go 1.27.3 with `GOTOOLCHAIN=local`**, which silently ignored
  `go.mod`'s `toolchain go1.27.4` (a release built there reported `go1.27.3`), so its steps set
  `GOTOOLCHAIN=go1.27.4+auto`; `native-smoke` and `image-smoke` assert the built Go version and
  `internal/repocheck` pins all of it.
* **`latest` is a deliberate owner switch.** It stays commented out (`# - latest` in `promote-image`) for every release
  and `ci/promote-image.sh` refuses it, `0.8`, `stable`, `edge` and every tag outside the grammar. Enabling it means
  promoting the same layout under that tag and updating `internal/repocheck` on purpose.

* **Timing-sensitive suites run alone.** `-p 3` is verified for a quiet agent, and failures are never rerun, so in
  `ci.yaml` the `test` step waits for lint, vet, build and the Dockerfile check and the heavier steps (OpenAPI,
  the GoReleaser snapshot, the invariants) run after it; in `release.yaml` the image build, the native build and
  `govulncheck` wait for the race suite. The pipeline is slower than a fully parallel one by design.

## Recovery after a partial publication

`promote-image` (registries) and `release-forgejo` (Forgejo) are independent, so one can succeed while the other fails,
and a pipeline re-run **rebuilds** the image with a different digest. Therefore:

* a re-run whose `promote-image` finds a tag that already resolves to other bytes **stops** ("refusing to overwrite a
  published tag"); it never replaces a published image;
* to recover, either delete the incomplete artifacts deliberately (the image tag in both registries, and the Forgejo
  release for that tag) and re-run, or cut the next tag (`0.8.0-rc.1`): a tag is cheap, an overwritten image is not;
* what GoReleaser does when the Forgejo release already exists is **not verified** here: expect to delete that release
  before a re-run.

**First release and GHCR.** `verify-published` pulls anonymously. If the first push creates the GHCR package private
(not verified here), that step fails right after a successful publication: set the package public (and linked to the
repository) and re-run, or run `ci/verify-published.sh` by hand. The artifacts themselves are fine in that case.

## Operator configuration (not in the repository)

**Woodpecker server**

* `WOODPECKER_PLUGINS_PRIVILEGED` must include `woodpeckerci/plugin-docker-buildx:6` (the build step needs a Docker
  daemon of its own).
* The repository must be **trusted** for privileged execution (the plugin step and the `dind` service of `release.yaml`).
  Privileged execution exists only in `release.yaml`; keep the repository's *"require approval for pull requests from
  forks"* (or *all* pull requests) setting on, because `ci.yaml` runs pull-request code.
* Keep the default commit-status format (`WOODPECKER_STATUS_CONTEXT_FORMAT` = `{{ .context }}/{{ .event }}/{{ .workflow }}`,
  context `ci/woodpecker`): `require-verification` waits for the status `ci/woodpecker/tag/ci`. A different format makes
  the guard refuse everything (it fails closed); update `--context` in `release.yaml` together with it.
* Agents need outbound access to: the forge, Docker Hub and GHCR (pulls and pushes), `proxy.golang.org` and `sum.golang.org`
  (module downloads, the Go toolchain download of the GoReleaser steps, `crane`, `govulncheck`), `mirror.gcr.io`
  (the Trivy vulnerability database) and the Alpine mirrors used by the base images.
* A private Forgejo repository additionally needs read credentials for `merge-target` and for `ci/ciguard`
  (`GUARD_TOKEN`); the repository is assumed public (it is mirrored publicly).

**Woodpecker secrets** (repository secrets, each restricted to the **`tag`** event; none may be available to
`pull_request`, `push` or `manual`):

| Secret | Used by | Content / permission |
|---|---|---|
| `DOCKER_LOGIN`, `DOCKER_TOKEN` | `promote-image` | Docker Hub user and an access token with **read & write** on `skobkin/magnetico-web` only |
| `GHCR_LOGIN`, `GHCR_TOKEN` | `promote-image` | GitHub user and a token with `write:packages` for `ghcr.io/skobkin/magnetico-web`; link the package to the `skobkin/magnetico-web` repository (the image's `org.opencontainers.image.source` label already points there) |
| `GITEA_TOKEN` | `release-forgejo` | a Forgejo token that may create releases and upload assets in `skobkin/magnetico-web` (`write:repository`) |

**Registries.** Create `skobkin/magnetico-web` on Docker Hub (public) and let the first push create the GHCR package
(then set it public and link it to the repository).

## First run and local rehearsal

1. Run `release.yaml` **manually** (no tag): it executes `ci.yaml` and every gate, builds a snapshot of the archives and
   the image, and publishes nothing. Fix anything that depends on your Woodpecker, then
2. push the first release-candidate tag (`0.8.0-rc.0`) and watch the publication steps; check
   `docker buildx imagetools inspect docker.io/skobkin/magnetico-web:0.8.0-rc.0` and the Forgejo release page.

Everything except the Woodpecker-specific parts can be rehearsed locally with throwaway registries (this is how the
promotion was verified; the override exists for rehearsals only and no workflow sets it):

```sh
docker run -d --name reg-a -p 127.0.0.1:55001:5000 registry:2 && docker run -d --name reg-b -p 127.0.0.1:55002:5000 registry:2
docker buildx build --platform linux/amd64,linux/arm64 --sbom=true --provenance=mode=max \
  --output type=oci,tar=false,dest=.ci/image,name=staged.invalid/magnetico-web:staged .
export GOTOOLCHAIN=go1.27.4 MAGNETICO_PROMOTE_REHEARSAL=1 \
  MAGNETICO_PROMOTE_REPOS="127.0.0.1:55001/magnetico-web 127.0.0.1:55002/magnetico-web"
sh ci/promote-image.sh .ci/image 0.8.0-rc.0
sh ci/verify-published.sh "$(grep -Eo 'sha256:[0-9a-f]{64}' .ci/image/index.json | head -n 1)" 0.8.0-rc.0
tar -C .ci/image -cf - . | docker load && sh ci/image-smoke.sh --platform linux/amd64 staged.invalid/magnetico-web:staged
```

## Native artifacts

`goreleaser release --clean` (run by `release-forgejo`) produces `magnetico-web_<version>_linux_{amd64,arm64}.tar.gz`
and `checksums.txt` (SHA-256), attached to the Forgejo release; each archive holds the `magnetico` executable,
`COPYING`, `README.md`, `UPGRADE.md` and the Swagger UI licence files under `licenses/`. The executables are static
(`CGO_ENABLED=0`), built with `-trimpath` and no build tags, with the version, commit, Go version and platform printed by
`magnetico --version` (the same linker values as the image). **Only Linux amd64 and arm64** are built: the crawler's
queue lock has Unix and "other" implementations, and no behaviour of the UDP crawler, queue locking or the cgroup-based
tests has been demonstrated on any other platform, so none was added. The licences of the Go module dependencies are not
bundled; generating a notice file for them is an open owner decision.

## Known limitations

* No pipeline has run on Woodpecker; the unverified items above are verified by the first manual run.
* `docker load` of the multi-platform layout into the `dind` daemon assumes Docker 29's containerd image store (the
  `docker:29-dind` default); with the classic store only the host platform is loaded, which is all the smoke test needs.
* The amd64 image is executed by the smoke test; arm64 is built, scanned and digest-verified, **not executed**.
* The skip census allow-list contains two known gaps of the external-PostgreSQL mode (see `testing.md`).
* `-p 3` is a workaround for timing-sensitive tests (issues #52, #53), not a fix.
* The image scanner and the vulnerability database are downloaded at run time; a scanner or database outage fails the gate.
