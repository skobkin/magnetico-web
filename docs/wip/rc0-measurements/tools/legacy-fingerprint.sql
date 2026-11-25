-- Order-independent logical fingerprint of the restored legacy torrent database
-- (RC0 automated acceptance). Read-only: run it before and after the migration and
-- compare. Prints only counts, sums and digests: no name, path or hash.
BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY;
SET LOCAL max_parallel_workers_per_gather = 6;
SELECT 'torrents' AS tbl, count(*) AS rows,
       sum(('x' || substr(md5(t::text), 1, 15))::bit(60)::bigint::numeric) AS row_digest_sum,
       max(id) AS max_id
  FROM magneticod.torrents t;
SELECT 'files' AS tbl, count(*) AS rows,
       sum(('x' || substr(md5(f::text), 1, 15))::bit(60)::bigint::numeric) AS row_digest_sum,
       max(id) AS max_id
  FROM magneticod.files f;
SELECT 'migrations' AS tbl, string_agg(schema_version::text, ',') FROM magneticod.migrations;
SELECT 'sequences' AS tbl, (SELECT last_value FROM magneticod.seq_torrents_id) AS torrents_seq,
       (SELECT last_value FROM magneticod.seq_files_id) AS files_seq;
SELECT 'ddl' AS tbl, md5(string_agg(c.relname || ':' || c.relkind::text || ':' || pg_get_userbyid(c.relowner), ',' ORDER BY c.relname)) AS relations_digest,
       (SELECT md5(string_agg(pg_get_constraintdef(oid), ',' ORDER BY conname)) FROM pg_constraint WHERE connamespace = 'magneticod'::regnamespace) AS constraints_digest
  FROM pg_class c WHERE c.relnamespace = 'magneticod'::regnamespace;
COMMIT;
