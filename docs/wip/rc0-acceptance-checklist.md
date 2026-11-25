# RC0 acceptance checklist (future)

**Status:** not started. This file only records items that were agreed ahead of the RC0 acceptance run (the migration items in the first section) and the open items found by task 07J (the sections after it); it is not a release plan. Every item runs against **disposable** PostgreSQL and synthetic or lab data, never against production.

## Migration

- [ ] **`migrate cleanup` exercise (disposable database).** On a disposable target that went through `migrate torrents` and both verifications (`verify-decode`, `verify-content`) with the final run green: `migrate errors export --manifest`, `migrate cleanup` (preview: every gate passes), then `migrate cleanup --execute --confirm-backup --confirm-export --evidence-manifest …` completes, `magnetico_migration` is gone, `magneticod`, `magnetico_web` and `magnetico_ingest` are untouched (row counts and checksums equal before and after), and a second `migrate cleanup` is a no-op. Record the command output (without credentials) in the RC0 report.
- [ ] **Negative case on the same exercise.** Start from an identical copy, store one new torrent through ingest or `persistence.Store.AddTorrent`, and confirm that `migrate cleanup --execute` and `migrate cleanup --execute --force-unverified --reason …` both refuse at gate G5 and leave the schema in place. (The automated regression test is `TestCleanupRefusedAfterANewTorrentIsStored`; this is the hands-on check with the real binary.)
- [ ] Production policy confirmed in the migration guide: `magnetico_migration` is kept by default, including after ingest starts; cleanup is optional and offline (`docs/migration_0.8.0.md` §8.3).

## Browser, device and accessibility (open after 07J)

Task 07J could **not** run a real browser: the available headless browser (obscura) refuses loopback addresses through its own SSRF guard (`Access to private/internal IP address 127.0.0.1 is not allowed`; it can be started with `--allow-private-network` or `OBSCURA_ALLOW_PRIVATE_NETWORK=1`, which was not done because that is a change to the tool's configuration, not to this repository). HTTP-level tests and the in-process tests are **not** browser acceptance. To run on a real browser (desktop and phone), against a disposable database seeded with `magnetico dev seed-torrents`:

- [ ] Sign in, sign out, remember-me, and every account tab, including the email section in its states (none, unverified/migrated, verified, pending replacement) **with and without SMTP configured**, the "Remove email" action and its confirmation panel, in light and dark themes and with the system theme.
- [ ] Search, sort, page size and keyset pagination (next/previous); empty and no-result states; the long-name and special-character fixtures; the keyboard shortcut for the search box.
- [ ] **Both file-browser modes** (a 60-torrent fixture for the tree, the 10,000-file fixture for directory pages): expand/collapse, local filter, breadcrumbs, previous/next, loading, error and retry states, long and unusual names (bidi and control markers, duplicates, `.`/`..`, empty components).
- [ ] **Swagger UI**: renders under the real Content-Security-Policy; the *Authorize* dialog with a pasted API token; an authenticated **Try it out** of `GET /torrents`, `/torrents/{id}`, `/files/tree` (including the `422` of the 10,000-file torrent) and `/files`; the request is same-origin, carries `Authorization: Bearer …` and **no cookie**; the feed endpoint with an RSS token; non-`GET` operations offer no Try-it-out; a reload clears the authorization.
- [ ] Console: no errors; network panel: **no request to any other host** on any page.
- [ ] Keyboard-only operation and visible focus on every page; screen-reader pass on the account page and the file browser (the tree is a list of rows with `aria-expanded` folder buttons, not an ARIA tree); contrast of the new notice and status styles in both themes (computed with the `07F-w5-measurements/contrast.txt` method: not run for the new styles).
- [ ] A 360 px-wide layout of every page, and a real phone.
- [ ] Screenshots, if kept, must not show token-creation, invitation-link, setup-link or reset-link pages.

## Reverse proxy and network (open after 07J)

No real proxy was exercised. With the proxy actually chosen for production (Traefik is the documented default; names and defaults must be checked for the version in use):

- [ ] The upstream timeouts exceed the 70 s search write deadline (at least 90 s), idle timeouts do not cut keep-alive connections early, and a 60 s search through the proxy completes or is cut by the application, not the proxy.
- [ ] The proxy routes **only** the configured public host name to the service (the `Host` header is not validated by the application) and the port is unreachable otherwise.
- [ ] `X-Forwarded-For` is appended by the proxy and the trusted set is narrow; behind it the rate limits identify the real client.
- [ ] The proxy's access log **does not record** feed query strings (`feed_token`, `api-token`), the `api-token` header or cookie, or the `Authorization` header of the ingest endpoint, and the registration/reset/verification link path segments are masked.
- [ ] Response caching is off for every route; TLS termination, HSTS and redirects behave as intended.

## Operations (open after 07J)

- [ ] **Container memory** is confirmed with the real image: the web service within the limit recommended in `07J-web-integration-security-report.md` (the `GOMEMLIMIT` value and the container limit are different numbers), the ingest service with the Task 06 recommendation, under the real image's entrypoint and user.
- [ ] **Real SMTP provider**: STARTTLS and implicit TLS with the provider's certificate chain (including a private CA via `SSL_CERT_FILE` if used); a verification, a recovery and a change-notice email arrive; a deliberately wrong password and an unreachable host log only failure categories.
- [ ] **Search plans on a production-like copy** of the migrated library (the real `name` distribution and word correlation): re-run `docs/wip/07J-measurements/explain.sh`, in particular the shape of **two common words that rarely occur together** (see the 07J report: a plan that walks the date index through the whole table), and decide with the owner whether an index proposal is justified. No index was added by 07J.
- [ ] **Live-DHT acceptance** of crawler → ingest → PostgreSQL → web with CI-built images on a separate DHT-capable host (never from the development VM).
- [ ] **User migration** dry-run on a copy of the real PHP accounts (password-hash audit, unsupported-hash count, email handling), then the offline cutover checklist of `docs/web-deployment.md` §10.
- [ ] After the first `migrate users` on real data, an administrator is promoted with `web user set-role` and the removal of a migrated historical email address is checked by the owner of such an account.
- [ ] **A soak of at least 24 hours** with the real image under the recommended limits and a representative mix (07J's soak was 2.5 hours and left the heap-after-GC trend of finding F9 inconclusive: +3 %/h over 90 minutes, not reproduced in the 45-minute diagnostic window): the soak is judged on **bounded, explainable** memory behaviour, not on memory being returned to the OS: after the cache fill, heap-after-GC, RSS, goroutines and descriptors must plateau (no sustained positive trend beyond the screening thresholds of the 07J report, any excursion explained by cache occupancy or a heap profile), the cgroup must record no `high`/`max`/`oom_kill` events, and after the idle phase goroutines and descriptors must be back at their baseline. Retained heap or RSS at idle is acceptable when the bounded caches (file index, existence cache) account for it; the Go scavenger releases pages lazily and RSS is **not** required to fall back to its pre-load level. Investigate finding F9 (the inconclusive +3 %/h heap-after-GC trend) with the `diag` build (`go build -tags diag -o magnetico-diag ./cmd/magnetico`, `MAGNETICO_DIAG_ADDR`) and heap profiles during this run.
- [ ] **Toolchain**: the CI/OCI build uses Go **1.27.4 or newer** (`go.mod` pins `toolchain go1.27.4` after the standard-library advisories of finding F10), and `govulncheck` is part of the pipeline (run with the same toolchain: a checker built by an older Go cannot analyse this code).

## Statistics and the administrator section (open after Task 09)

Task 09 qualified the statistics on synthetic data, with real binaries on loopback, and could not run a real browser or a real proxy (`docs/wip/09-statistics-admin-report.md`). The remaining checks:

- [ ] **uPlot under the strict CSP in more browsers** (Task 09B ran headless Chrome 159 against a disposable loopback deployment with the production CSP: five chart groups drew from data, legend toggles, redraw on resize, all period buttons, light/dark, no CSP violation, no console error, no request to another host, every administrator page exactly 360 px wide after two layout fixes; `docs/wip/09B-final-local-preflight-report.md`). Still open: current Firefox, a real phone, a keyboard and screen-reader pass, a visual judgement of the textual summaries against the plotted values, and gaps-are-gaps on real sparse data.
- [ ] **Keyboard and screen reader** on the overview and the listings (focus order, the period navigation, the summaries), contrast of the stat badges in both themes.
- [ ] **Access, with real accounts:** the `Admin` link appears only for an administrator; an ordinary user and an anonymous visitor get `403`/the sign-in page on every `/admin` URL; a just-demoted administrator loses access on the next request; the pages show no hash, code, token or payload.
- [ ] **≥ 24 h with real crawlers (live DHT) on a separate host:** the overview's received, acknowledged, enqueued and settled agree with the crawler and ingest logs; at a quiet moment `received − acknowledged − refused` is zero; backlog and duplicate traffic read as `docs/operator-statistics.md` says; the storage figures and the measured growth match what PostgreSQL reports.
- [ ] **With the production reverse proxy:** crawler last addresses are the real client addresses (`MAGNETICO_INGEST_TRUSTED_PROXIES` narrow), the proxy's access log does not record the `Authorization` header of the ingest endpoint, and a rotated or disabled credential is refused within 60 s through the proxy.
- [ ] **Queue sampling at production scale with a cold cache** (a queue of millions of rows after a long outage): the sampler's cycle time, and the `approximate` flags if a count exceeds its 2 s budget; admission latency while it runs.
- [ ] **Two `ingest` instances** against one database: queue and storage figures shown once (not summed), event counts summed, an instance listed as left out of a rate when it is not fresh, the election handing over when the consumer owner stops.
- [ ] **Front-end self-tests in CI** (`frontend-selftest` in `.woodpecker/ci.yaml`): covered by the pull-request pipeline item under Release engineering.
- [ ] **The cleartext HTTP/2 evaluation** (`docs/wip/09-statistics-admin-report.md`): repeat it over the real network between a crawler host and ingest before deciding to keep HTTP/1.1 keep-alive.

## Release engineering (simplified in Task 09B)

Task 09B replaced the custom qualification and promotion system by the conventional workflows of the owner's other repositories (`docs/release-engineering.md`): `ci.yaml` runs every gate for every pull request; `release.yaml` (tags only) runs GoReleaser and the buildx plugin. Nothing has run on a real Woodpecker server, and no disposable Forgejo, tag-mode rehearsal or release preflight campaign is planned: **the owner verifies the real build and publication with the RC0 tag.** What to look at then:

- [ ] **Operator configuration** in place as listed in `docs/release-engineering.md`: `WOODPECKER_PLUGINS_PRIVILEGED` includes `woodpeckerci/plugin-docker-buildx:6`, tag-only secrets `DOCKER_LOGIN`/`DOCKER_TOKEN`, `GHCR_LOGIN`/`GHCR_TOKEN`, `GITEA_TOKEN` with the documented permissions, the Docker Hub repository and the GHCR package (public, linked to the GitHub mirror), the fork-pull-request approval setting.
- [ ] **A pull request pipeline is green on the real server**, including the new `race`, `govulncheck`, `native-snapshot`/`native-smoke` and `frontend-selftest` steps (the last under all three time zones).
- [ ] **The first release-candidate tag (`0.8.0-rc1`)**: `ci` runs first and `release.yaml` waits for it; `release-forgejo` creates a **prerelease** with both archives, `checksums.txt` and the generated changelog; `release-docker` pushes the release tag (and only it) to both registries; `docker buildx imagetools inspect` shows two platforms and the attestations on both; the pushed image passes `ci/image-smoke.sh --platform linux/amd64`. A tag outside the grammar must start neither workflow.
- [ ] **Before the tag, locally:** `ci/image-smoke.sh` and a Trivy scan of an image built from the tagged commit (they need a privileged Docker daemon and are not in `ci.yaml`, see `docs/release-engineering.md`).
- [ ] **The arm64 image is actually run** on arm64 hardware (`ci/image-smoke.sh --platform linux/arm64 docker.io/skobkin/magnetico-web:0.8.0-rc1`): built and scanned, never executed.
- [ ] **A pull request cannot read a secret** (a deliberately failing `ci` also blocks the release workflow).
- [ ] **Decision on `latest`** (and on any floating alias): not applied for any release until the owner adds it to `release-docker`, planned around the stable 0.8.0 or 0.8.1 release; update `internal/repocheck` deliberately.
- [ ] **Decision on a registry build cache** (none is configured) and on bundling the licences of the Go module dependencies in the native archives.
- [ ] **Removal of the temporary legacy-PHP-environment warning** (`internal/legacywarn`, one call in `internal/app/app.go`, the section in `UPGRADE.md`) around version 0.10.0.
- [ ] After the 0.8.0 merge, remove `refactor/0.8.0-unify-and-new-storage` from the `push` branches of `.woodpecker/ci.yaml`.
