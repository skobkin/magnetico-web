# Real-data 0.7.x → 0.8.0 migration rehearsal — `0.8.0-rc1`

**Artifact under test:** `docker.io/skobkin/magnetico-web:0.8.0-rc1`
(`sha256:6cb84539ca1b87fb3d793ac9c1b3f040d33c1bdd24ebb6309e316de8972ccc43`, created 2026-11-27 00:47 UTC), which reports
`magnetico 0.8.0-rc1 (commit 1de903331477, go1.27.4, linux/amd64)`. That commit was `master` at test time, so the
documentation reviewed here is the one shipped with the image. **Run:** 2026-11-27, on the owner's laptop, in disposable
loopback-only containers. **Branch of this report:** `prerelease-fixes`.

**Two goals.**
1. Prove that the migration commands work on **real** legacy data and that the code is ready for a production deployment.
2. Check that `UPGRADE.md` and [`docs/migration_0.8.0.md`](../migration_0.8.0.md) guide an administrator through the
   upgrade correctly.

The procedure was executed as an administrator would do it: from the docs, with the published image, one command at a
time. Section numbers in §§3–7 are those of the guide **as shipped in `0.8.0-rc1`**; the corrected guide is organised
differently, and §8 records how every finding was resolved. Deviations from the docs are recorded as findings. The owner chose the following options before the run: hybrid
roles, the PHP time zone `Europe/Moscow`, Mailpit for SMTP, a loopback `http://` web, web and ingest running at the end
(no crawler), and this report committed to a branch.

No production system, live DHT or reverse proxy was involved. The report contains no credential, hash, token, email,
username of a migrated user or torrent name. Records are referred to by counts and numeric ids only.

## 1. Summary

| Area | Result |
|---|---|
| Torrent migration | **Works on real data.** 3,457,046 torrents / 123,460,039 file rows (the 10 % consistent sample) imported with **0 record errors** in ≈ 13.6 min. `verify-content` **PASS** (every row equal, 55 min). `verify-decode` **PASS**. The documented SIGTERM → exit 2 → resume was exercised deliberately. Two unplanned host reboots (a power-loss equivalent) interrupted `verify-decode` twice, and the documented recovery ("run the same command again") resumed it both times from its checkpoint. PostgreSQL recovered cleanly. 17 GB legacy → 2.8 GB |
| User migration | **Works on the real legacy accounts.** Audit: all 35 hashes *verified* (argon2id and argon2i), and every data-shape check was 0. Dry run, final preview, final import (35 users, 366 invitations, 6 legacy tokens → RSS tokens, 0.6 s), `users verify` and the idempotent re-run all behaved exactly as documented. No warnings, no refusals |
| Services | `web` starts as the restricted runtime role with the documented minimum grants. Search (ASCII, Cyrillic, CJK), the 123,480-file torrent (directory pages), both file-browser modes, magnet redirects, legacy token in RSS v1 and v2 (200) and on the JSON API (401), the account page, the administrator pages and an email verification over STARTTLS all work. `ingest` starts as documented and refuses unauthenticated requests |
| Code defects | **None found in the migration or service code.** Only small UX items (§4.4) |
| Documentation | **Not release-ready as shipped in RC1.** 3 major documentation errors (one documented command fails as written; `UPGRADE.md` contradicts itself; the post-migration checklist cannot be satisfied), `UPGRADE.md` is not a brief entry point, and several gaps and stale statements remain (§4). **Corrected on `prerelease-fixes`** (§8) |
| Migrated login | The owner signed in with their migrated account and its original password; the hash was rehashed on that first sign-in (`web user list`: `current`). This is one real account: the other 34 migrated passwords were not tested and stay `legacy` until their owners sign in |
| Owner request | `verify-decode` is **not needed** as a cutover gate when `verify-content` passed (§5). Documenting that removes one full-database scan from the procedure |

**Go / no-go.** The migration code and the image are ready for production use as far as this rehearsal can show: no data
defect, every verification passed, and resume works after both a graceful and an abrupt stop. The release should wait for
the documentation fixes **D1–D3** and the **`UPGRADE.md` rewrite (G1)**. The other items can follow, but they are cheap
to fix in the same pass. *Update:* the documentation findings are resolved on `prerelease-fixes` without a code change
(§8); the UX items X1–X5 are deferred.

## 2. Artifact, environment and dataset

| | |
|---|---|
| Image | `docker.io/skobkin/magnetico-web:0.8.0-rc1@sha256:6cb84539…ccc43` (one-shot `docker run --rm` for every `migrate` / `web user` command; services through Compose with `read_only`, `cap_drop: ALL`, `no-new-privileges`) |
| PostgreSQL | `postgres:17` (17.12), `fsync=on`, `shared_buffers=512MB`, `work_mem=16MB`, `maintenance_work_mem=256MB`, container limit 2 GiB |
| Host | laptop, ≈ 7 GiB RAM free with swap already in use. Limits: migrate containers 1 GiB, web 512 MiB (`GOMEMLIMIT=384MiB`), ingest 384 MiB (`GOMEMLIMIT=192MiB`) |
| Torrent source | 10 % consistent extract of the real production `magneticod` database: plain `pg_dump` (PostgreSQL 16.14), restored in 419 s to 17 GB. Original v0.12.0 layout (`magneticod.files` unpartitioned, `migrations = 0`). 3,457,046 torrents, 123,460,039 file rows. The largest torrents in the sample have 123,480 and 71,756 files |
| Application source | full real `magnetico-web` 0.7.x database: plain `pg_dump` (PostgreSQL 16.15). 35 users, 366 invitations (33 used), 6 API tokens, 2 password-reset tokens, the 5 Doctrine migrations (table `public.migration_versions`) |
| Roles (hybrid) | `test` is the superuser and owns the restored legacy databases. The dump's own owner roles (`magneticod`, `postgres`, `magnetico-web`) were pre-created as `NOLOGIN`. Read-only sources: `legacy_torrents_ro`, `legacy_app_ro` (§3.2 grants). Target owner `magnetico`, which runs `migrate` and `ingest` (the documented default single account). `magnetico_web_rt` runs `web` and `magnetico_web_op` runs `web user …`, both with the verbatim grants of `web-deployment.md` §4 |
| Web | `http://127.0.0.1:18081` (loopback development mode; see W4 for why the container shares the host network). Mailpit (STARTTLS, private test CA via `SSL_CERT_FILE`) |

## 3. Procedure log

The guide sections are those of `docs/migration_0.8.0.md`. Times are wall-clock times of the one-shot container.

| Step | Command (as run) | Exit | Time | Result |
|---|---|---|---|---|
| §4.1 | `--version`, `--help`, `migrate --help` | 0 | — | version and subcommands as documented |
| §4.2 | `migrate schema-init` | 0 | 1.0 s | torrent schema created |
| §4.2 | `migrate web-schema-init` | 0 | 0.8 s | `magnetico_web` created, 12 trackers seeded |
| §4.2 | `web-deployment.md` §4 grants | — | — | **failed as the owner** at `GRANT magnetico_web_rt TO magnetico_web_op` (W1); finished by splitting the block |
| §4.4 | `psql … -f docs/wip/07H-audit.sql` as `legacy_app_ro` | 0 | < 1 s | 35 *verified* (18 argon2id `m=65536,t=4,p=1`, 17 argon2i `m=16384,t=2,p=4`); every shape counter 0 |
| §4.6 | `migrate users --source-timezone Europe/Moscow --dry-run --report-json …` | 0 | 1.4 s | `result=ok`, `needs_reset=0 unvetted=0` |
| (doc check) | the `docs/container.md` user-migration example (`MAGNETICO_MIGRATION_SOURCE_DSN`) | **1** | 0.9 s | `no source database URL (set MAGNETICO_MIGRATION_USERS_SOURCE_DSN …)` (D1) |
| §5 | freeze simulated: `REVOKE CONNECT … FROM PUBLIC`, `default_transaction_read_only = on`, `pg_stat_activity` empty; `pg_dump -Fc` of the application database (24 KB, listable) | — | — | the provided dumps served as the torrent backup and were proven restorable by this rehearsal |
| §6.1 | `migrate torrents --defer-secondary-indexes`, stopped with `docker stop` (SIGTERM) at 683,008 | **2** | 191 s | ≈ 4,150 torrents/s; immediate, clean stop; message says to re-run |
| §6.1 | the same command again | 0 | 624 s | resumed at exactly 683,008; data loaded in 8 min 42 s; 3 indexes rebuilt in ≈ 69 s; `import_complete`, 3,457,046 migrated, **0 record errors**, 2,247,594 blobs |
| §6.2 | `migrate verify-content` | 0 | 3,299 s | **PASS**: 3,457,046 verified, 0 mismatched / unverifiable / assumed single-file, 0 issues |
| §6.2 | `migrate verify-decode` started in parallel | **1** | 0.8 s | `migration refused: another migration run holds the lock on this target` (V1) |
| §6.2 | `migrate verify-decode` (interrupted by two host reboots, resumed twice) | 0 | last part 354 s | **PASS**: 3,457,046 observed, 2,247,594 verified, 1,209,452 assumed single-file (the documented blind spot), 0 issues. Total time not measurable |
| §7.1 | `migrate users … --dry-run --report-json …` | 0 | 0.7 s | identical source and plan digests; the source now reports `read-only=true` |
| §7.2 | `migrate users --source-timezone Europe/Moscow --source-writes-stopped --accept-needs-reset 0 --report-json …` | 0 | 0.6 s | imported 35 users, 366 invitations, 6 RSS tokens |
| §7.3 | `migrate users verify …` | 0 | 0.4 s | `result=verified` |
| §7.2 | the same final run again | 0 | 0.4 s | `already-migrated`, nothing written |
| §7.3 | `web user list` (as `magnetico_web_op`) | 0 | 0.4 s | 35 × `user` / `legacy` / `unverified`, as documented |
| §7.3 | `web user set-role <owner> admin` | 0 | 0.4 s | promoted |
| §9 | `migrate users verify …` after the promotion | **1** | 0.4 s | `verify-failed`, `mismatches: ["user-role:1"]` (D3) |
| §8.1 | `migrate ingest-schema-init`, `migrate stats-schema-init` | 0 | 0.5 s each | created |
| §8.1 | `web-deployment.md` stats grants | — | — | **failed**: `role "magnetico_ingest_rt" does not exist` (W3); applied without the ingest lines |
| §8.2 | `web` | — | — | first start refused (W4); then healthy in ≈ 6 s as `magnetico_web_rt` |
| §8.3 | `migrate errors export --output /reports/final-export.jsonl --manifest` | 0 | 0.7 s | 5 events, 4,530 bytes, manifest written |
| §8.3 | `migrate cleanup` (preview) and `migrate cleanup --dry-run --confirm-backup --confirm-export --evidence-manifest …` | 0 | 1.7 s | G1–G7 pass; with the attestations G1–G9 pass. **Not executed**: the migration schema is kept, as the guide recommends |
| §8.3 | `ingest` (owner account) | — | — | starts, warns that no crawler is registered, statistics sampling works, an unauthenticated POST gets `401` |

User-migration reports (`--report-json`) contained counts, numeric ids and digests only, as documented.

### 3.1 Smoke tests (§8.2)

The migrated passwords are unknown to the tester, so the session checks used a rehearsal account created with
`web user add` + `web user set-password` (stdin), as §8.2 allows. It is still in the database (id 37, role `user`).
The owner then signed in with their own migrated account and original password (§6).

| Check | Result |
|---|---|
| `/healthz`, `/readyz`, `/robots.txt` | 200 |
| `/magnet/{infohash}` | 307 to a `magnet:` URI with the seeded trackers |
| Legacy token: `/api/v1/feed/rss/last?api-token=…` | 200 RSS, 1,000 items (`page=2`: 1,000) |
| Legacy token: `/api/v2/feeds/latest.rss?feed_token=…` | 200 RSS, 1,000 items |
| Legacy token as Bearer on `/api/v2/torrents` | 401 `application/problem+json` |
| Wrong or missing token on the v1 feed | 401 |
| `MAGNETICO_WEB_RSS_V1_ENABLED=false` (temporary second instance, removed afterwards) | v1 path 404 with and without a token; v2 feed 200 |
| Legacy UI URL `/auth/login` | 404 (documented) |
| `/torrents`, `/admin` without a session | 303 to sign-in |
| Search, ASCII term | 100 rows, 0.23 s cold, 0.02 s warm (size order) |
| Search, Cyrillic and CJK words from the data | 12 and 2 rows, 9.5 ms and 17 ms |
| Torrent with 123,480 files | details 2.6 ms; full tree `422 full-tree-unavailable` (by design); directory page 36 ms |
| Torrent with 71,756 files | directory page 46 ms |
| 9-file torrent / single-file torrent without a blob | full tree 200 / 200 |
| Account page, API documentation (`/api`) | 200 |
| `/admin` as a non-administrator | 403 |
| Administrator pages (rehearsal account promoted temporarily, demoted afterwards) | all six pages 200; overview shows ingest `up` |
| Email added on the account page | the confirmation email arrived in Mailpit over STARTTLS with the private CA (95 ms) |
| Service logs (web 73 lines, ingest 7 lines) | contain none of the token, password or DSN values used, and no query strings |
| Memory after the tests | web 243 MiB, ingest 15 MiB, PostgreSQL 580 MiB |

## 4. Findings

Severity: **major** means an administrator following the docs fails or is misled; **minor** means friction or an
inconsistency an attentive administrator works around; **cosmetic** is self-explanatory. Each item has a suggested fix.

### 4.1 Documentation errors

**D1 (major) — the `docs/container.md` user-migration example cannot work.** The example passes
`-e MAGNETICO_MIGRATION_SOURCE_DSN=…` to `migrate users`. The user migration reads only
`MAGNETICO_MIGRATION_USERS_SOURCE_DSN` (the guide §4.3 and `web-deployment.md` §10 say so, and the binary agrees). As
written, the example exits 1 with `no source database URL (set MAGNETICO_MIGRATION_USERS_SOURCE_DSN or the
migrateUsersSourceDatabaseURL file key; there is no fallback to the target)`. *Fix:* use the right variable in the
example.

**D2 (major) — `UPGRADE.md` contradicts itself about the ingest token.** The "New `ingest` service" paragraph lists
`TOKEN` as "required, ≥ 32 bytes, environment only, must equal the crawler's". A few lines below, the text says
`MAGNETICO_INGEST_TOKEN` is removed and setting it is a startup error. The first paragraph is stale (it predates per-crawler
credentials). *Fix:* remove `TOKEN` from that list. G1 rewrites the section anyway.

**D3 (major) — the post-migration checklist cannot be satisfied.** §7.3 promotes the first administrator with
`web user set-role`, which is required. After that, `migrate users verify` fails permanently
(`reconciliation mismatches: 1 (first: [user-role:1])`, exit 1). Every first login adds another divergence, because it
rehashes the password. §9 nevertheless requires "`magnetico migrate users verify` exited 0". A dry run on such a target
also reports `state=diverged`. *Fix:* in §9, say "exited 0 in §7.3, **before** the promotion". Say in §7.3 that verify is
meaningful only between the import and the first change (promotion, login, password reset), and keep its report as the
evidence. *Optional code change:* let `verify` classify expected post-migration changes (role, rehash, `last_login`,
sessions) separately from real mismatches, so it stays usable afterwards.

**D4 (minor) — stale "not released / not run" statements.** The guide's "Honest limits" paragraph and §4.1 say that "no
0.8.0 release or release image exists yet" and that "the user migration has never seen real legacy accounts". The status
box of `docs/container.md` says "no image has been published yet". `UPGRADE.md` still has the heading
"Unreleased 0.8.0 (development branch)" and says the instructions "reach `master` when the owner merges the 0.8.0 branch".
*Fix:* update them for the release candidate and for the release. This report can be cited as the real-data rehearsal.

**D5 (minor) — the `UPGRADE.md` "Deployment order" is incomplete.** It lists `schema-init`, `migrate torrents`,
`ingest-schema-init`, `stats-schema-init` and crawlers. It omits `web-schema-init`, the user migration and starting `web`,
so it disagrees with the guide. *Fix:* remove it in favour of the guide (G1), or complete it.

**D6 (minor) — the `web-deployment.md` stats-grants block cannot be run as written.** It grants to `magnetico_ingest_rt`,
a role that no document tells the administrator to create. The default deployment runs ingest as the owner account.
`psql -v ON_ERROR_STOP=1` stops with `role "magnetico_ingest_rt" does not exist`. *Fix:* split the block into the web-role
part and an optional ingest-role part, and say the latter is unnecessary with the default single account.

**D7 (minor) — the minimum-grants block needs a role administrator on PostgreSQL 16+.** As the database owner, the object
grants succeed, but `GRANT magnetico_web_rt TO magnetico_web_op` fails with `permission denied to grant role …: Only roles
with the ADMIN option on role "magnetico_web_rt" may grant this role`. The docs do not say which role runs the block.
*Fix:* say "run the role statements as a superuser (or a `CREATEROLE` role with `ADMIN OPTION`), the object grants as the
schema owner". The tests probably run them as a superuser, which hides this.

**D8 (minor) — two sources of truth for the user migration.** `web-deployment.md` §10 repeats much of guide §7 and ends
with "The full checklist is §9 of the plan" (the `docs/wip/07H` plan, not the guide). `migrate users --help` points to
`docs/web-deployment.md §10` instead of the guide. *Fix:* reduce §10 to a pointer to `docs/migration_0.8.0.md`, and point
the help text there.

### 4.2 Documentation gaps

**G1 (major for the release) — `UPGRADE.md` is not a brief entry point.** The 0.8.0 section is about 150 lines. Most of
it addresses developers who followed the 0.8.0 branch, not 0.7.x administrators: the reinitialisation of development
`magnetico_stats` and `magnetico_ingest` schemas that were never released, "a password change now cancels pending email
confirmation links" (0.7.x had no email confirmation), code and test pointers of the legacy-environment warning, raw
quarantine SQL, and a "Removed from `crawl`" table (0.7.x had no `magnetico crawl`; its crawler was a different program).
D2 and D5 live in that text too. *Fix:* an `UPGRADE.md` 0.8.0 entry of a dozen lines. It should state what 0.8.0 is (a Go
rewrite, a new database, separate crawler and ingest), the one-way and manual character of the upgrade, and the
decisions to prepare (time zone, password-hash audit, administrator, maintenance window). It should end with **one link**
to `docs/migration_0.8.0.md`. Move the development-branch notes out (to the release notes or a development changelog),
and the crawler/ingest operations to `docs/crawler.md`, which already covers them.

**G2 (minor) — the verification steps are under-specified.** §6.2 "Run both" does not say that they must run one after
the other. Started together, the second is refused (V1). *Fix:* "run them one after the other (they share the target's
run lock; a concurrent run is refused with exit 1)". Also see §5: `verify-content` alone should become the gate.

**G3 (minor) — the audit script is only in the repository, under `docs/wip/`.** §4.4 runs `psql … -f
docs/wip/07H-audit.sql`. The image does not contain it, so an administrator who only has the image needs a checkout of
the tag and a host `psql`, and `wip` signals "work in progress". *Fix:* move it to a stable path (for example
`scripts/legacy-users-audit.sql`), link it with the tag in the URL, and mention that the migrator's own dry run
is authoritative. Alternatively, make the dry run's hash-category summary the documented audit.

**G4 (minor) — no ready SQL for the roles.** §3.2 gives the users-source grants but only "SELECT only" for the torrent
source, and §4.2 has no example of creating the target role and database. *Fix:* add the few statements used here,
for example `GRANT CONNECT ON DATABASE …; GRANT USAGE ON SCHEMA magneticod …; GRANT SELECT ON ALL TABLES IN SCHEMA
magneticod …` and `CREATE ROLE … LOGIN; CREATE DATABASE … OWNER …`.

**G5 (minor) — reports and where to keep them.** `docs/container.md` correctly asks for a `10001:10001`-owned `/reports`.
The resulting files are `0600` and owned by uid 10001, so the administrator cannot read them without `sudo`, and the
docs do not say so. §12 asks to preserve the reports but not to keep `/reports` on durable storage. In this rehearsal
the first set was written to a tmpfs and lost in a reboot. *Fix:* one sentence on each point.

**G6 (minor) — loopback smoke testing with the image.** §8.2 suggests smoke-testing on "the loopback address". The
loopback `http://` development mode also requires a loopback **listen** address. A bridge-networked container therefore
refuses to start (`development mode (http:// public URL) requires a loopback listen address: its relaxed cookie rules
must never be exposed to a network`, a restart loop). The guard and its message are good; only the docs are missing.
*Fix:* say that with the image this needs `network_mode: host` (or a private TLS hostname behind the proxy).

**G7 (minor) — no complete 0.8.0 deployment example.** The guide and `docs/container.md` link the `docker-stacks`
deployment, which "temporarily still describes the old PHP deployment". The compose fragment in `docs/container.md` has
no PostgreSQL, roles or network wiring. *Fix:* update `docker-stacks` before the release, or add a complete example.

### 4.3 Bugs

None in the migration or service code. Everything listed above is documentation, and everything below is
presentation.

### 4.4 UX (code, small)

**X1 (minor) — the lock refusal message names the wrong operation.** A verification refused because another run holds the
target lock says `migration refused: another migration run holds the lock on this target`. *Fix:* name the command, and if
possible the holder (import, verify-decode, verify-content, retry, cleanup).

**X2 (minor) — misleading verification ETA and counter.** `verify-content` predicted `eta=9m03s` at 30 s (6,025 rows/s).
The scan then averaged 1,058 rows/s and took 55 min. The `blobs=` field stays 0 throughout both verifications. *Fix:* a
smoothed or file-row-weighted rate, and drop `blobs=` from the verification progress (or fill it).

**X3 (cosmetic) — duplicated final progress lines.** The last progress line of the import (`import_complete`) and of
`verify-decode` (`extras`) is printed twice.

**X4 (cosmetic) — config failure printed as a run summary.** `migrate users` without a users source prints a full
`result=refused … users=0 invites=0 …` summary, and writes a report, before the actual configuration error.

**X5 (cosmetic) — timestamps without a zone.** The `migrate` commands log with the Go `log` default (`2026/11/27 01:35:07`,
UTC in the container, no zone marker). `web` logs RFC 3339 with `Z`.

## 5. Is `verify-decode` needed for a 0.7.x → 0.8.0 migration? (owner request)

**No: `verify-content` is sufficient as the cutover gate.** The specifications already rank them this way:
[`legacy-torrent-migration-v1.md`](../specs/legacy-torrent-migration-v1.md) §10 calls `verify-content` the "preferred
cutover gate", and [`postgresql-persistence-v2.md`](../specs/postgresql-persistence-v2.md) calls `verify-decode` an
"operator fallback when a full source read is too slow". The code shows why `verify-content` covers what matters:

- **Every stored file list is decoded.** It decodes every target blob with the same `codec.Decode` and limits as
  `verify-decode` (`internal/migration/verify_content.go`, `decodeEntry`), so framing, codec, ordering and limit errors
  surface the same way.
- **Every row is compared with the source.** It compares, per id, the infohash, name, total size, discovery time, stored
  `file_count` (added in 04B, `ccb79e20`) and the complete `(path, size)` multiset including duplicates.
- **It closes `verify-decode`'s blind spot.** For blob-less single-file rows it compares the reconstructed file with the
  source, which `verify-decode` cannot do.
- **Same extra checks.** Its extras phase runs the same orphan-blob scan and also detects extra target rows.
- **The import already enforced the rest.** The invariants `verify-decode` re-checks (positive total, non-negative
  sizes, overflow-checked sum equal to the total) were enforced at import time by `persistence/validate.go`. Rows proven
  equal to the source cannot violate them.

**What only `verify-decode` adds:** the `noncanonical_single_file_blob` finding (an ordinary single file stored with a blob
instead of the blob-less form). That is a storage representation detail; the data is still equal to the source. The other
advantage is that it needs no source access.

**One hard dependency:** the optional `migrate cleanup` gate **G7** requires a current passing verdict of **both** kinds
(`internal/migration/forensics/cleanup.go`, `evaluateVerificationGates`). Without `verify-decode` the gate fails. That
failure is waivable with `--force-unverified --reason "…"`, and the reason is recorded. Cleanup is optional and not the
default; `ingest` needs no verification verdict.

**Cost.** On this 10 % sample `verify-content` took 55 min. On the same sample, the 04B lab run and the RC0 acceptance
measured `verify-decode` at 58 min and 42 min. On the full database (≈ 10×) dropping `verify-decode` saves many hours of maintenance window. Strictly, though,
`verify-decode` only delays ingest: it never touches the legacy database, and `web` never writes torrents.

**Recommendation.**
1. *Documentation (no code change):* in §6.2 and §9, make `verify-content` the required gate. Describe `verify-decode` as
   optional: the fallback when the source cannot be read in full, or a prerequisite only if `migrate cleanup` will be run
   without a waiver.
2. *Optional code follow-up (a specification change, owner approval needed):* add the canonical-representation check to
   `verify-content`, so that it is a strict superset, and let cleanup G7 accept a passing `verify-content` alone.

## 6. Post-migration checklist (§9)

| Item | Status |
|---|---|
| `verify-content` passed with no findings and `verify-decode` passed, before any new torrent | ✅ both PASS at data revision 0 |
| Torrent count of the new database equals the legacy count minus unresolved errors | ✅ 3,457,046 = 3,457,046, 0 errors |
| `migrate users verify` exited 0; reports stored | ✅ before the promotion; ❌ afterwards, by design (D3) |
| `web user list`: `needs-reset` count equals N; administrator can sign in | ✅ N = 0; the owner signed in as the promoted administrator with the migrated password |
| A migrated user with a supported hash signs in | ✅ for one account (the owner's; like all 35, its hash is in a *verified* family of the audit); `web user list` then showed `current` for it. The other 34 migrated passwords were not tested |
| Legacy token works on the v2 feed and is refused (401) by the JSON API | ✅ |
| v1 feed on: the legacy RSS URL works unchanged; off: 404 | ✅ on (main instance); ✅ off (a temporary second instance with `MAGNETICO_WEB_RSS_V1_ENABLED=false`: 404 with and without a token, v2 feed still 200) |
| Search finds ASCII and non-ASCII names; a very large torrent opens | ✅ |
| `/magnet/{infohash}` redirects; tracker list as expected | ✅ 12 seeded trackers |
| Reverse-proxy access log and timeouts | n/a (no proxy in this rehearsal; covered by RC0) |
| Ingest accepts records, crawler `unauthorized=0` | n/a (no crawler, owner decision); ingest refuses unauthenticated requests |
| Logs contain no secrets; SMTP delivers a verification email | ✅ / ✅ |
| Backups and reports stored safely | partially (local disposable copies; see G5) |

## 7. Not covered

- Logins with the other 34 migrated passwords. One real migrated login (the owner's) and its rehash were tested; every
  account's hash was classified *verified* by the audit and the dry run, which is what predicts that the others work.
- Traefik, TLS and the access-log configuration: out of scope here; Traefik 3.8.0 was covered by the RC0 acceptance.
- A crawler and live DHT traffic: out of scope here and **not** covered by RC0 either. The live-DHT acceptance on a
  separate host is still open (`docs/wip/rc0-acceptance-checklist.md`).
- `migrate cleanup --execute` and `retry-errors`. Cleanup was previewed only, and the import produced no record errors to
  retry.
- The full (100 %) database. The timings above scale roughly linearly with the source size, and the verifications are
  the long part.

## 8. Disposition of the findings (`prerelease-fixes`)

The owner accepted the rehearsal and decided:

- one target account for everything by default, with restricted roles as optional, separate hardening;
- `verify-content` as the only torrent verification of the normal procedure, with `verify-decode` a target-only
  diagnostic;
- `users verify` right after the import and before any account change;
- no change to the external `docker-stacks` repository in this pass.

The corrections below change documentation and one test path only: no runtime behaviour, schema, verification
semantics, cleanup gate, CLI text or release engineering changed. A new release candidate is not needed for them.

| Finding | Disposition |
|---|---|
| D1 container example uses the torrent source variable | **Fixed.** `container.md` shows the three variables in an env file and says that the user migration reads only `MAGNETICO_MIGRATION_USERS_SOURCE_DSN` |
| D2 `UPGRADE.md` lists `TOKEN` for ingest | **Fixed** by the rewrite (G1). The removal of the shared token is kept, in the part for development-build installations |
| D3 checklist requires `users verify` after promotion | **Fixed.** Guide §8.3: verify right after the import, before promotion, sign-in or any `web user` command. It keeps the report and explains why later mismatches are expected and why re-importing is wrong. Checklist §12 refers to that verification and does not require another. Verification code unchanged |
| D4 stale "no release / not run on real data" | **Fixed** in the guide, `container.md` and `crawler.md`. RC1 is recorded as published and rehearsed on real data. `UPGRADE.md` heading: "0.8.0 (unreleased; release candidate `0.8.0-rc1` published 2026-11-27)" |
| D5 incomplete deployment order in `UPGRADE.md` | **Fixed.** Removed; the guide's phases are the only order |
| D6 statistics grants name `magnetico_ingest_rt` | **Fixed.** The block grants to the restricted web role only. It is run by the owner account. The default single account needs no grant; `ingest` runs as the owner |
| D7 membership grant needs `ADMIN OPTION` | **Fixed.** `web-deployment.md` §4 says who runs each statement. The marked grants block now grants to both roles directly (`TO magnetico_web_rt, magnetico_web_op`), so the owner account can run all of it, and role membership is not needed. Membership is described as an alternative, with its PostgreSQL 16+ requirement |
| D8 two sources of truth for the user migration | **Fixed in the docs.** `web-deployment.md` §10 is a short pointer to the guide; the README no longer lists "user migration" for it. *Deferred:* `migrate users --help` still names `docs/web-deployment.md §10`. That section stays as a pointer, so the reference still resolves; changing the text is a CLI change for a later release |
| G1 `UPGRADE.md` not a brief entry point | **Fixed.** A short 0.8.0 entry: what changes, the main breaks, the decisions, one link to the guide. The development-branch notes are dropped (the email-link change, the web-schema-init lock key, code pointers). A separate part for installations of development builds keeps what runtime messages point to: the removed crawl database setting, the removed ingest token, and the ingest and statistics schema reinitialisation (`internal/repocheck` checks the ingest one). The ingest memory figures and the quarantine SQL moved to `crawler.md` |
| G2 verification ordering | **Fixed.** Guide §7: one run at a time (shared lock, exit 1 with the observed message). `verify-content` is the only required gate. `verify-decode` is Appendix A (target-only diagnostic, its prerequisites and limits). Both verdicts are required only in Appendix B (optional cleanup), which does not suggest waiving gates. Gate G7 is unchanged |
| G3 audit SQL under `docs/wip/` | **Fixed.** Moved to `docs/legacy-users-audit.sql` (test updated). For image-only installations the guide gives the release-tag download (the GitHub mirror; Forgejo's raw URLs answer `curl` with a bot-protection redirect) and a `docker run postgres:17 psql` command with a libpq env file. It says the dry run is authoritative |
| G4 no role or database SQL | **Fixed.** Guide §4.2: target account and database, and the legacy read-only account with exact grants. Guide §5: the freeze statements |
| G5 report ownership and durability | **Fixed.** Run one-shot commands with `--user "$(id -u):$(id -g)"` and a directory you own on durable storage. Otherwise use a `10001`-owned directory and read through the image (`--entrypoint cat`) or as root. Guide §§4.1, 14; `container.md` |
| G6 loopback testing in a container | **Fixed.** Guide §9.2 and `container.md`: host networking with a loopback listen address and an SSH tunnel, or a private TLS hostname. The guard is unchanged |
| G7 no complete deployment example | **As the owner decided:** the `docker-stacks` link is marked as the PHP deployment, incompatible with 0.8.0, in README, `UPGRADE.md` and `container.md`. The owner updates that repository at the production deployment |
| X1–X5 UX items | **Deferred** (code changes): X1 the lock message names the operation, X2 the verification ETA and `blobs=` counter, X3 duplicated final progress lines, X4 a configuration error printed as a run summary, X5 timestamps without a zone |

**Found during this pass and fixed:**

- `container.md` claimed that environment variables keep credentials out of process listings, but its examples passed
  `-e NAME=postgres://…password…`, which is visible in `ps` on the host and stays in the shell history. The examples now
  use `--env-file`, and the guide does the same; it no longer has `export` lines with credentials. The doc also says
  that Docker access gives access to a running container's environment.
- `crawler.md` began with a box describing the removed shared ingest token. It also said that the live-DHT acceptance
  happened in the release-candidate phase, and one troubleshooting row still said "ingest was not restarted".
- The guide still said there is no administrator UI and recommended "query-string masking" at the proxy, and its
  §7.3 used a separate operator role. `web-deployment.md` said that no real reverse proxy had been exercised
  (Traefik 3.8.0 was, in RC0) and described an interim root page.
- `UPGRADE.md`, `crawler.md` and `container.md` pointed to each other for option tables and measurements that the
  rewrite moved.

**Recorded, not changed:**

- The README says that stable releases also update `latest`, but `.woodpecker/release.yaml` never applied it. On the
  owner's decision, `latest` was added to the image's tags in a separate commit, before the first stable release
  (0.8.0).
- The administrator overview caches the statistics state for up to 60 s. A grant applied while `web` runs shows up
  within a minute.

**Validation of the corrections.** These checks ran on a disposable PostgreSQL 17.12 with the `0.8.0-rc1` image; the
SQL was extracted from the documents themselves.

- **Target account and database.** The guide's statements created them, and `schema-init` (including `pg_trgm`),
  `web-schema-init`, `ingest-schema-init` and `stats-schema-init` ran through the guide's wrapper function as that
  non-superuser owner. One env file held all three migration variables, and the user dry run read the users source and
  wrote its report as the operator's uid, mode `0600`.
- **Legacy read-only account.** The statements gave exactly `SELECT` on the documented tables.
- **Freeze.** The statements refused the legacy writer roles and kept the read-only account connected.
- **Restricted roles.** The marked grants block and the statistics block ran as the owner account. The owner's
  `GRANT magnetico_web_rt TO magnetico_web_op` failed with the documented message, and a `CREATEROLE` creator could grant
  it.
- **Restricted roles in use.** `web` started as the runtime role with the guide's loopback host-network command. Every
  administrator command ran as the operator role, and all six administrator pages loaded without a privilege warning.
  As a negative control, revoking a statistics grant produced the warning.
- **Audit.** The documented `docker run … psql -f -` audit ran read-only against the rehearsal's legacy application
  database.
- **Repository tests** (`MAGNETICO_TEST_REQUIRE_POSTGRES=1`). These passed: `internal/repocheck` (documentation
  checks, links, the reinitialisation section), `internal/web/webschema` (the documented grants suffice; every missing
  privilege is named), `internal/migration/users` (the audit at its new path agrees with the Go classifier) and
  `internal/e2e/cli` (the documented grants applied verbatim, with the real binary starting as the runtime role and the
  administrator commands running as the operator role). `go vet` and `golangci-lint` report 0 issues on the changed
  package.
  The two `--version` format tests of `internal/e2e/cli` fail on this workstation only, because its `go env` sets
  `GOEXPERIMENT=nodwarf5` and the toolchain then reports `go1.27.4-X:nodwarf5`. With `GOEXPERIMENT=dwarf5` (the
  default) they pass.

