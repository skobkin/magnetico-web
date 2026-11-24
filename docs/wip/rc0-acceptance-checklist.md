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

