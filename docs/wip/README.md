# Magnetico 0.8.0 — agent task sequence

This directory contains **implementation assignments**, not automatic execution instructions. The owner starts and reviews each task separately in Plan Mode before authorizing code changes.

| Order | Agent task | Gate / expected deliverable |
|---|---|---|
| 1 | [`01-import-go-upstream.md`](01-import-go-upstream.md) | Pinned `tgragnato/magnetico` import preserving seed docs, both ancestries where feasible, AGPL attribution; stop for review |
| 2 | [`02-upstream-cleanup.md`](02-upstream-cleanup.md) | Scoped Go crawler without old web/backends/automatic legacy schema migration; stop for review |
| 3 | [`03-storage-persistence.md`](03-storage-persistence.md) | Versioned codec, fresh PostgreSQL persistence and integration tests; stop for review |

Approved reference contracts live in [`docs/specs/`](../specs/): [`wire-format-v1.md`](../specs/wire-format-v1.md) and [`postgresql-persistence-v2.md`](../specs/postgresql-persistence-v2.md).

**Branch:** `refactor/0.8.0-unify-and-new-storage` in the existing `magnetico-web` Forgejo repository. The PHP history remains on `master`. These steps must never operate on a production database. The two user-made commits (PHP cleanup and seed docs) **precede task 1**.

Later tasks for torrent/user migration, replacement web UI, CI and production cutover are **not yet written**. The existence of this index is not permission to start them.
