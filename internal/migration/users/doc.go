// Package users is the W8 offline migrator of the legacy PHP/Symfony accounts
// (schema "users": users, invites, api_tokens) into the magnetico_web schema.
// The contract is docs/wip/07H-user-migration-plan.md.
//
// The migration is one target transaction: an interrupted or failed run
// rolls back completely. Sequences are the one PostgreSQL object that is not
// transactional for nextval()/setval(), so the migrator never calls either:
// every row is inserted with an explicit id (OVERRIDING SYSTEM VALUE) and the
// three identity columns are advanced with the transactional
// "ALTER TABLE … ALTER COLUMN id RESTART WITH n". A dry-run is read-only on
// both databases and writes nothing, sequence state included.
//
// Nothing in this package prints or returns a secret: records are identified
// by numeric id and finding code, database errors are reduced to SQLSTATE and
// constraint name, and reports carry counts, ids, classes and digests only.
package users
