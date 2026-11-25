# Task 09B — final local preflight report

Branch `refactor/0.8.0-unify-and-new-storage`, baseline `fdef36fb` (Task 09A accepted). Everything ran on the development
VM against disposable databases; no production data, no live DHT traffic, no tag, no publication.

## 1. Migration verification, issue #52

**Findings.** Reading the finish path (`finishVerificationOutcome`) against every ordering of cancellation, commit and
lock loss found two more product defects behind the one fixed in 09A, both reproduced first:

| Case | Before | Now |
|---|---|---|
| the finishing transaction is **unresolved** when the cancellation arrives (the COMMIT has not reached the server) | the reconciliation read the row around it, saw `running`, and the pass was reported as a resumable interruption; marking it interrupted then blocked 30 s on the row lock and failed, whatever the transaction did next | the row is read `FOR SHARE`, so the read waits for the transaction to end: it then commits (the durable pass is returned) or rolls back (a settled `interrupted`, exit 2, the next invocation resumes it) |
| the outcome stays unresolved for the whole bound | "interrupted, marking also failed" after 45 s | reported as unknown (`FatalError`, exit 1, "run the verification again"); nothing is recorded |
| the **run lock is lost** while the verdict is durable but unacknowledged | the durable row was reported as this pass's success (it may be another process's that adopted the run) | the lock loss is reported (exit 1), never a verdict and never an interruption |
| already committed (the 09A case) | fixed in 09A | unchanged |

**Tests** (`internal/migration/verify_ambiguous_finish_test.go`, each for `verify-decode` and `verify-content`, each failed
first against the previous code): `TestVerifyFinishAlreadyCommittedIsTheDurableVerdict`,
`...UnresolvedThenCommittedIsTheDurableVerdict`, `...UnresolvedThenRolledBackIsAResumableInterruption`,
`...OutcomeNeverResolvedIsReportedUnknown`, `...LockLostDuringTheAmbiguousCommitIsNotASuccess`. The 200 ms sleep is gone: the
cancellation is sent only when the database itself shows the durable verdict (or, for the unresolved cases, when
PostgreSQL reports the reconciliation blocked on a row lock). The TCP fixture tracks and closes every connection and
goroutine and waits for them in its cleanup (no hour-long sleep; a goroutine that could not end would hang the test).
The one test that lowers the reconciliation bound (`finishReconcileTimeout`, 500 ms) is serial and asserts only that an
unresolved outcome is *unknown*. The contract is in `docs/specs/legacy-torrent-verification-recovery-v1.md`.
Commit `c1ac851b`.

## 2. Release engineering

References inspected (read only, nothing modified): `meshmap-lite` (one Woodpecker file; GoReleaser `release --clean` with
`GITEA_TOKEN` on tag; `plugin-docker-buildx:6` pushing to Docker Hub and GHCR on tag), `aimon`, `yave`, `qBtRemoteGo`,
`go4updates`, `meshgo` (the same GoReleaser pattern). Result (commit `58d57b66`):

* `.woodpecker/ci.yaml` runs every gate for every pull request, with no secret and no privilege: format, lint, vet (both tag
  sets), build, the ordinary suite, **the race suite (after it, never beside it)**, **govulncheck**, OpenAPI, a native
  snapshot build with `ci/native-smoke.sh`, hadolint, the **front-end self-tests** (pinned Node, three time zones),
  workflow invariants.
* `.woodpecker/release.yaml` (release tags only, `depends_on: [ci]`) has two steps: `release-forgejo`
  (`goreleaser release --clean`, prerelease for `-rc.N`) and `release-docker` (the buildx plugin: linux/amd64 and
  linux/arm64, docker.io and ghcr.io, exactly the release tag, no `latest`, no cache).
* Removed: `ci/publishnative`, `ci/ciguard`, `promote-image`, `verify-published`, `verify-native`, `check-attestations`,
  `release-repos`, the OCI-layout/dind/image-scan/qualified-checksum steps and their ~2,100 lines of tests and fixtures. The
  invariants in `internal/repocheck` now describe the two ordinary workflows (no secret in `ci.yaml`, secrets only in the two
  release steps, tag-only, the agreed repositories/platforms/tag, no `latest`, no privileged request, the Go floor, every
  gate present, timing-sensitive suites alone). Mutation-checked.
* `woodpecker-cli lint` is valid for both files; for `release.yaml` it needs `--plugins-privileged
  woodpeckerci/plugin-docker-buildx`, the same server setting the owner's `meshmap-lite` comment documents (the step does not
  ask for privilege itself). `goreleaser check` is clean.

**Checks that cannot safely run in a pull-request workflow** (documented in `docs/release-engineering.md`): building and
running the container image and scanning it need a privileged Docker daemon; they remain local pre-tag steps
(`ci/image-smoke.sh`, Trivy), as in 08/09A. Not verified here: any real Woodpecker run, GoReleaser's behaviour against a real
Forgejo, the plugin push, whether GoReleaser's `prerelease: auto` behaves as documented for Gitea releases, and the
multi-architecture build in the plugin (the Dockerfile cross-compiles, no emulation, as verified locally in 09A). The owner
verifies these with the RC0 tag; the RC0 checklist says what to look at.

## 3. Real-browser check of the Admin UI

No browser was usable through the existing tooling (the `obscura` browser refuses loopback by design, and I did not work
around it). Instead a **headless Chrome 159** (`chromedp/headless-shell`, started with its debugging port on loopback only
and removed afterwards) was driven over the DevTools protocol (`docs/wip/09B-measurements/browser/driver.js`, Node 22) against
a disposable loopback deployment: PostgreSQL 17 container, the `ingest` and `web` binaries of this commit, a synthetic crawler
client delivering real records (about 3,800 in 8 minutes), the 101-torrent development fixture set, one administrator whose
password was set through the real one-time link, and **96 hours of synthetic hourly history plus 20 days of storage rows**
(copies of the real hourly row scaled by an integer factor) so the charts have something to draw. The production CSP was not
touched (`internal/web/server/middleware.go` is unchanged; the check compares the served header with it). No mouse or keyboard
injection into the desktop was involved.

| Check | Result |
|---|---|
| anonymous `/admin` | redirected to sign-in |
| administrator after the one-time link and sign-in | reaches `/admin` |
| served CSP | exactly `default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'` |
| five chart groups (throughput, outcomes, queue, traffic, storage) | all draw (1, 1, 2, 6, 2 uPlot canvases), no loading or error status, textual summaries present |
| legend click, resize, Day / Month / Year / Earlier | series toggles, the plot redraws (556 → 691 px), every granularity draws |
| light and dark theme | dark background `rgb(14, 20, 24)` |
| requests | only to `http://127.0.0.1:18081` |
| CSP violations, console errors, exceptions | none |
| every administrator page and `/torrents` at 360 px | **after two fixes**, exactly 360 px wide |

**Defects found and fixed** (commit `6a6c656b`; visible only in a real browser): at 360 px the header navigation of an
administrator (the extra *Admin* link) was wider than the screen on **every signed-in page** (+29 px), and the administrator
tables (`.table-wrap` had no CSS at all) widened the overview by up to 69 px, crawlers 36, users 6, trackers 120. The
navigation now shrinks and scrolls and tables scroll inside their card (`app.css`); a static test pins the two rules; the
first run's measurements are in this report, the final run in `browser/run-final.txt`. Screenshots (desktop light and dark,
phone, crawlers, trackers) are in `browser/shots/`.

**Not covered:** Firefox, a real phone, keyboard/screen reader, a reverse proxy, real sparse data (the history is
synthetic, so "gaps are gaps, not zeros" was only checked by the Node self-tests). The RC0 checklist keeps these.

## 4. Final verification from a clean checkout

`VERIFICATION_PLACEHOLDER`

## 5. Forgejo

`FORGEJO_PLACEHOLDER`
