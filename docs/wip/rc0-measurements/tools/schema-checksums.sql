-- Row count and order-independent row digest of every table in the schemas that
-- `migrate cleanup` must leave untouched (magneticod, magnetico_web, magnetico_ingest,
-- magnetico_stats when present), plus whether magnetico_migration exists. Read-only.
-- Prints digests only, no row contents. Run before and after cleanup and diff.
\set QUIET on
BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY;
SET LOCAL max_parallel_workers_per_gather = 6;
SELECT format('SELECT %L AS tbl, count(*) AS rows, COALESCE(sum((''x'' || substr(md5(t::text), 1, 15))::bit(60)::bigint::numeric), 0) AS digest FROM %I.%I t',
              n.nspname || '.' || c.relname, n.nspname, c.relname)
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE c.relkind IN ('r', 'p') AND n.nspname IN ('magneticod', 'magnetico_web', 'magnetico_ingest', 'magnetico_stats')
 ORDER BY 1 \gexec
SELECT 'magnetico_migration schema present' AS check, EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'magnetico_migration') AS value;
COMMIT;
