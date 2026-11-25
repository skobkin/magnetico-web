# Release engineering

How a tag becomes native binaries on Forgejo and a multi-platform image on Docker Hub and GHCR, what runs for every
pull request, and what an operator must configure. Read [`container.md`](container.md) for using the image and
[`testing.md`](testing.md) for the test classes.

The shape is deliberately the conventional one of the owner's other repositories: one verification workflow for every
pull request, one tag-only release workflow in which **GoReleaser** creates the Forgejo release and the **Woodpecker
buildx plugin** pushes the image. (Task 08 and Task 09A had built a custom qualification and promotion system around
it; Task 09B removed it: byte-for-byte identity between what a pull request verified and what a tag publishes is not a
requirement.)

## Configured versus verified

| Item | State |
|---|---|
| `Dockerfile`, `.dockerignore`, `.goreleaser.yaml`, `.woodpecker/ci.yaml`, `.woodpecker/release.yaml` | **configured and committed** |
| Both workflow files | **linted** with `woodpecker-cli lint` (v3.20.1; `--plugins-privileged woodpeckerci/plugin-docker-buildx`, see below) and pinned by `internal/repocheck`; **never run on a Woodpecker server** |
| `goreleaser check` and a snapshot build of both archives, `ci/native-smoke.sh` | **verified locally** (also inside the pinned `goreleaser/goreleaser:v2.20.1` image) |
| The image (amd64 and arm64 in one `docker buildx` invocation), `ci/image-smoke.sh` on amd64, Trivy for both platforms | **verified locally** during Task 08 and Task 09A; not part of any workflow |
| arm64 image | **built and scanned; never executed** |
| Real Woodpecker build, GoReleaser publication to Forgejo, the plugin's push to Docker Hub and GHCR | **never performed** (no tag exists, no credentials were used here): the owner verifies them with the `0.8.0-rc.0` tag |

## Roles

Forgejo (`git.skobk.in/skobkin/magnetico-web`) is the primary repository, GitHub (`skobkin/magnetico-web`) a mirror,
Woodpecker the CI. Forgejo hosts the **native release artifacts** (releases) but is **not** an OCI registry
publication target. Images go to `docker.io/skobkin/magnetico-web` and `ghcr.io/skobkin/magnetico-web` only.

## Tags

Existing tags are `0.2.0-symfony6-rc1` … `0.7.1` (PHP era). The Go line continues the same numbering. New tags must match

```
^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$        e.g. 0.8.0-rc.0, 0.8.0-rc.1, 0.8.0, 0.8.1
```

(release candidates are spelled `-rc.N` with a dot, as SemVer orders them; the historical `-rc1` spelling is not
accepted for new tags). A tag outside the grammar triggers **neither** workflow, so it can never publish. A
release-candidate tag publishes exactly its version tag (`0.8.0-rc.0`) and a **prerelease** on Forgejo (GoReleaser's
`prerelease: auto`); a stable tag publishes the stable version and a stable release. **No floating alias is applied**
(`latest` is the owner's later, deliberate decision: add it to the `tags` of `release-docker` and update
`internal/repocheck` on purpose).

## What runs when

| Event | `ci.yaml` (verification) | `release.yaml` | Forgejo release | OCI publication |
|---|---|---|---|---|
| push to `master` or the 0.8.0 branch | yes | no | no | no |
| push to any other branch | no (its pull request verifies it) | no | no | no |
| pull request | yes, **merged state** (no secrets, no privilege, no registry) | no | no | no |
| tag matching the grammar | yes | yes, after `ci` | yes | yes |
| manual run | yes | no | no | no |

While a pull request from the 0.8.0 branch is open, that branch has both a push and a pull-request pipeline (the one
accepted duplication; remove `refactor/0.8.0-unify-and-new-storage` from the `push` list of `ci.yaml` after the merge).

## Verification: `ci.yaml`, for every pull request

All of these run for every pull request, in this order of dependency; none needs a secret or a privilege.

| Step | What it checks |
|---|---|
| `merge-target` | a pull request is tested **merged** into its target branch (a conflict fails) |
| `fmt`, `lint` | `golangci-lint fmt --diff` and `golangci-lint run` (0 issues) |
| `vet`, `build` | `go vet` for both tag sets (`loadtest,diag` must stay compilable), a static build, `--version` |
| `dockerfile-lint` | `hadolint` |
| `frontend-selftest` | the Node self-tests of the administrator charts under three time zones; plain Node in an image pinned by version and digest, **only here**, never in a build of the product |
| `test` | the whole suite (unit, PostgreSQL integration, real-binary end-to-end, security regression, fault injection) against a disposable PostgreSQL 17 with `MAGNETICO_TEST_REQUIRE_POSTGRES=1`, `-p 3`, and the skip census (a test skipped for a reason not on `ci/skip-allowlist.txt` fails the run) |
| `race` | the same suite with `-race` |
| `govulncheck` | pinned v1.10.0 on the step's Go 1.27.4 toolchain, both tag sets |
| `openapi` | the OpenAPI document and the real binary's responses against it |
| `native-snapshot`, `native-smoke` | `goreleaser check`, a snapshot build of both archives and `ci/native-smoke.sh`: checksums, licence files, the uPlot script embedded in the executable, `CGO_ENABLED=0`, the right `GOARCH`, no build tags, the Go floor |
| `workflow-invariants` | `internal/repocheck` and the CI tools' tests: the workflow files' own security properties (no secret or privilege in `ci.yaml`, tag-only release, no `latest`, the Go floor, ...) |

Timing-sensitive suites run alone: `-p 3` is verified for a quiet agent and failures are never rerun, so `test` waits for
lint, vet, build and the Dockerfile check, `race` runs after `test`, and everything heavier runs after `race`.

**Checks that cannot safely run in a pull-request workflow.** Building and running the container image needs a
privileged Docker daemon (the buildx plugin and a `dind` service), and pull-request code runs in `ci.yaml`. They are
therefore **not** part of it; they run on a developer machine before a tag:

```sh
docker buildx build --platform linux/amd64,linux/arm64 \
  --output type=oci,tar=false,dest=.ci/image,name=staged.invalid/magnetico-web:staged .
tar -C .ci/image -cf - . | docker load
sh ci/image-smoke.sh --platform linux/amd64 staged.invalid/magnetico-web:staged   # run, credentials, schemas, SIGTERM, assets, non-root
trivy image --input .ci/image --platform linux/amd64 --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1   # and linux/arm64
```

`hadolint` (in `ci.yaml`) and the Dockerfile's own comments cover the static part; the runtime part of the image is
otherwise first exercised by the real release build.

## Release: `release.yaml`, tags only

```
tag ─► ci.yaml (every gate above, same commit)
   └─► release.yaml   (depends_on: [ci])
         release-forgejo   goreleaser release --clean   ► Forgejo release (+ both archives and checksums.txt)
         release-docker    plugin-docker-buildx          ► docker.io/skobkin/magnetico-web:<tag> and ghcr.io/…:<tag>
```

* **Publication waits for verification.** `depends_on: [ci]` is a standard Woodpecker workflow dependency: a failed
  `ci` workflow (tests, race suite, lint, vulnerability check, ...) means `release.yaml` does not run.
* **`release-forgejo`** is a plain `goreleaser release --clean` in the pinned `goreleaser/goreleaser:v2.20.1` image: the
  archives for `linux/amd64` and `linux/arm64` (reproducible: commit-dated entries, `-trimpath`, no tags), the SHA-256
  `checksums.txt`, the generated changelog as the release notes, `prerelease: auto`. The image ships Go 1.27.3 with
  `GOTOOLCHAIN=local`, which silently ignores `go.mod`'s `toolchain go1.27.4`: the step sets `GOTOOLCHAIN=go1.27.4+auto`.
  The Forgejo API URL is derived from `CI_FORGE_URL` (set by Woodpecker).
* **`release-docker`** is `woodpeckerci/plugin-docker-buildx:6` with `linux/amd64,linux/arm64`, SBOM and provenance
  attestations, `pull_image: true`, the release tag as the only tag, build arguments `VERSION` and `COMMIT`, and logins
  for both registries. Its automatic labels are off (they would replace the Dockerfile's
  `org.opencontainers.image.source`, the GitHub mirror that links the GHCR package to the repository, with the Forgejo
  URL). **No registry cache** is configured (a cache is a poisoning surface; BuildKit cache mounts suffice).
* **Untrusted code never sees credentials.** `ci.yaml` (the only workflow pull requests reach) has no `from_secret`, no
  registry login, no cache and no privileged step; `release.yaml` runs for tags only. Secrets are used by
  `release-forgejo` (Forgejo token) and `release-docker` (registry credentials) and nowhere else.
* **The Go security floor is 1.27.4 everywhere** (`golang:1.27.4-*` step images, the Dockerfile's compiler stage, the
  GoReleaser steps as above); `native-smoke` and `image-smoke` assert the built Go version.

## Recovery after a partial publication

`release-forgejo` and `release-docker` are independent, so one can succeed while the other fails. A pipeline re-run
**rebuilds** both. To recover, either delete the incomplete artifacts deliberately (the image tag in both registries,
and the Forgejo release for that tag, which GoReleaser will not replace; its behaviour on an existing release is
**not verified** here) and re-run, or cut the next tag (`0.8.0-rc.1`): a tag is cheap, an overwritten image is not.
The plugin pushes tags without a check, so never re-run a release for a tag whose image was already used.

## Operator configuration (not in the repository)

**Woodpecker server**

* `WOODPECKER_PLUGINS_PRIVILEGED` must include `woodpeckerci/plugin-docker-buildx:6` (the plugin needs a Docker daemon of
  its own). The step does not ask for privilege itself. Without the server setting the plugin step fails, and a local
  `woodpecker-cli lint` reports exactly that finding unless it is given `--plugins-privileged woodpeckerci/plugin-docker-buildx`.
* Keep the repository's *"require approval for pull requests from forks"* (or *all* pull requests) setting on, because
  `ci.yaml` runs pull-request code.
* Agents need outbound access to: the forge, Docker Hub and GHCR, `proxy.golang.org` and `sum.golang.org` (module
  downloads, the Go toolchain download of the GoReleaser step, `govulncheck`), and the Alpine mirrors used by the base images.

**Woodpecker secrets** (repository secrets, each restricted to the **`tag`** event; none may be available to
`pull_request`, `push` or `manual`):

| Secret | Used by | Content / permission |
|---|---|---|
| `DOCKER_LOGIN`, `DOCKER_TOKEN` | `release-docker` | Docker Hub user and an access token with **read & write** on `skobkin/magnetico-web` only |
| `GHCR_LOGIN`, `GHCR_TOKEN` | `release-docker` | GitHub user and a token with `write:packages` for `ghcr.io/skobkin/magnetico-web`; link the package to the `skobkin/magnetico-web` repository (the image's `org.opencontainers.image.source` label already points there) |
| `GITEA_TOKEN` | `release-forgejo` | a Forgejo token that may create and edit releases and upload assets in `skobkin/magnetico-web` (`write:repository`) |

**Registries.** Create `skobkin/magnetico-web` on Docker Hub (public) and let the first push create the GHCR package
(then set it public and link it to the repository).

## First release

Push the first release-candidate tag (`0.8.0-rc.0`) and watch the pipeline: `ci` first, then `release-forgejo` and
`release-docker`. Check `docker buildx imagetools inspect docker.io/skobkin/magnetico-web:0.8.0-rc.0` (and the same on
GHCR: both should show two platforms and the attestations), the Forgejo release page (a **prerelease** with both
archives, `checksums.txt` and the changelog), and run `ci/image-smoke.sh --platform linux/amd64` against the pushed image.

## Native artifacts

`goreleaser release --clean` produces `magnetico-web_<version>_linux_{amd64,arm64}.tar.gz` and `checksums.txt` (SHA-256),
attached to the Forgejo release; each archive holds the `magnetico` executable, `COPYING`, `README.md`, `UPGRADE.md` and
the licence files of the vendored front-end libraries under `licenses/` (the Swagger UI files and `uplot-LICENSE`; the
image carries the same under `/usr/share/licenses/magnetico/`). A missing licence file fails `ci/native-smoke.sh`,
`ci/image-smoke.sh` and `internal/repocheck`. The executables are static (`CGO_ENABLED=0`), built with `-trimpath` and
no build tags, with the version, commit, Go version and platform printed by `magnetico --version` (the same linker
values as the image). **Only Linux amd64 and arm64** are built: the crawler's queue lock has Unix and "other"
implementations, and no behaviour of the UDP crawler, queue locking or the cgroup-based tests has been demonstrated on
any other platform. The licences of the Go module dependencies are not bundled; generating a notice file for them is
an open owner decision.

## Known limitations

* No pipeline has run on Woodpecker; the first real run is the owner's `0.8.0-rc.0` tag.
* The container image is not built or run by any workflow before a tag (see above): its smoke test and scan are local.
* The amd64 image is executed by the smoke test; arm64 is built and scanned, **not executed**.
* The skip census allow-list contains two known gaps of the external-PostgreSQL mode (see `testing.md`).
* `-p 3` is a workaround for timing-sensitive tests (issue #53), not a fix.
