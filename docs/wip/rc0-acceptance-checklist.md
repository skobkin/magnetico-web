# RC0 acceptance checklist (future)

**Status:** not started. This file only records items that were agreed ahead of the RC0 acceptance run; it is not a release plan. Every item runs against **disposable** PostgreSQL and synthetic or lab data, never against production.

## Migration

- [ ] **`migrate cleanup` exercise (disposable database).** On a disposable target that went through `migrate torrents` and both verifications (`verify-decode`, `verify-content`) with the final run green: `migrate errors export --manifest`, `migrate cleanup` (preview: every gate passes), then `migrate cleanup --execute --confirm-backup --confirm-export --evidence-manifest …` completes, `magnetico_migration` is gone, `magneticod`, `magnetico_web` and `magnetico_ingest` are untouched (row counts and checksums equal before and after), and a second `migrate cleanup` is a no-op. Record the command output (without credentials) in the RC0 report.
- [ ] **Negative case on the same exercise.** Start from an identical copy, store one new torrent through ingest or `persistence.Store.AddTorrent`, and confirm that `migrate cleanup --execute` and `migrate cleanup --execute --force-unverified --reason …` both refuse at gate G5 and leave the schema in place. (The automated regression test is `TestCleanupRefusedAfterANewTorrentIsStored`; this is the hands-on check with the real binary.)
- [ ] Production policy confirmed in the migration guide: `magnetico_migration` is kept by default, including after ingest starts; cleanup is optional and offline (`docs/migration_0.8.0.md` §8.3).
