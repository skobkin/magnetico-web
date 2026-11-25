-- Read-only anomaly audit of a restored legacy (magnetico v0.12, schema magneticod)
-- torrent database (RC0 automated acceptance). Prints aggregates only: never a name,
-- a path or an info hash. Predicts the migrator's per-record errors from the
-- production validation (persistence/validate.go, codec.DefaultLimits) and lists the
-- shapes that are valid but unusual (stored verbatim, never normalised).
BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY;
SET LOCAL max_parallel_workers_per_gather = 6;
SET LOCAL work_mem = '256MB';
\echo == torrents
SELECT count(*) AS torrents,
       count(*) FILTER (WHERE octet_length(info_hash) <> 20) AS bad_hash_len,
       count(*) FILTER (WHERE name = '') AS empty_name,
       count(*) FILTER (WHERE name ~ '[\x01-\x1f\x7f]') AS name_ctrl,
       count(*) FILTER (WHERE name ~ '[‎‏‪-‮⁦-⁩]') AS name_bidi,
       count(*) FILTER (WHERE name <> btrim(name)) AS name_outer_space,
       count(*) FILTER (WHERE name ~ '�') AS name_replacement_char,
       max(octet_length(name)) AS name_max_bytes,
       percentile_disc(0.5) WITHIN GROUP (ORDER BY octet_length(name)) AS name_p50_bytes,
       min(total_size) AS min_total, max(total_size) AS max_total,
       to_timestamp(min(discovered_on)) AS first_discovered, to_timestamp(max(discovered_on)) AS last_discovered,
       count(*) FILTER (WHERE discovered_on > extract(epoch FROM now())) AS discovered_in_future
  FROM magneticod.torrents;
\echo == files
SELECT count(*) AS files,
       count(*) FILTER (WHERE torrent_id IS NULL) AS null_owner,
       count(*) FILTER (WHERE size < 0) AS negative_size,
       count(*) FILTER (WHERE size = 0) AS zero_size,
       count(*) FILTER (WHERE path = '') AS empty_path,
       count(*) FILTER (WHERE path ~ '[\x01-\x1f\x7f]') AS path_ctrl,
       count(*) FILTER (WHERE path ~ '(^|/)\.\.?(/|$)') AS path_dot_components,
       count(*) FILTER (WHERE path ~ '//' OR path LIKE '/%' OR path LIKE '%/') AS path_empty_components,
       count(*) FILTER (WHERE path LIKE '%\\%') AS path_backslash,
       count(*) FILTER (WHERE path ~ '�') AS path_replacement_char,
       max(octet_length(path)) AS path_max_bytes,
       count(*) FILTER (WHERE octet_length(path) > 65536) AS path_over_codec_limit
  FROM magneticod.files;
\echo == per torrent (file lists)
WITH agg AS (
  SELECT torrent_id, count(*) AS n, sum(size) AS s, sum(octet_length(path)) AS pbytes,
         count(*) - count(DISTINCT path) AS dup_paths,
         max(array_length(string_to_array(path, '/'), 1)) AS depth
    FROM magneticod.files WHERE torrent_id IS NOT NULL GROUP BY torrent_id)
SELECT (SELECT count(*) FROM magneticod.torrents t WHERE NOT EXISTS (SELECT 1 FROM magneticod.files f WHERE f.torrent_id = t.id)) AS torrents_without_files,
       (SELECT count(*) FROM agg WHERE NOT EXISTS (SELECT 1 FROM magneticod.torrents t WHERE t.id = agg.torrent_id)) AS orphan_owner_ids,
       count(*) AS torrents_with_files,
       count(*) FILTER (WHERE a.n = 1) AS single_file,
       count(*) FILTER (WHERE a.n > 1) AS multi_file,
       count(*) FILTER (WHERE a.s <> t.total_size) AS size_sum_mismatch,
       count(*) FILTER (WHERE a.n > 250000) AS over_file_limit,
       count(*) FILTER (WHERE a.pbytes > 64 * 1024 * 1024) AS over_aggregate_path_limit,
       count(*) FILTER (WHERE a.depth > 256) AS over_depth_limit,
       count(*) FILTER (WHERE a.dup_paths > 0) AS torrents_with_duplicate_paths,
       sum(a.dup_paths) AS duplicate_path_rows,
       max(a.n) AS max_files, percentile_disc(ARRAY[0.5, 0.9, 0.99, 0.999]) WITHIN GROUP (ORDER BY a.n) AS files_p50_p90_p99_p999,
       max(a.depth) AS max_depth
  FROM agg a JOIN magneticod.torrents t ON t.id = a.torrent_id;
\echo == single-file shape (no-blob eligibility: one file whose path equals the name and size equals the total)
SELECT count(*) FILTER (WHERE f.path = t.name AND f.size = t.total_size) AS no_blob_eligible,
       count(*) FILTER (WHERE f.path <> t.name) AS atypical_path,
       count(*) FILTER (WHERE f.path = t.name AND f.size <> t.total_size) AS atypical_size
  FROM magneticod.torrents t
  JOIN LATERAL (SELECT path, size FROM magneticod.files f WHERE f.torrent_id = t.id) f ON true
 WHERE (SELECT count(*) FROM magneticod.files f2 WHERE f2.torrent_id = t.id) = 1;
\echo == discovery by year
SELECT extract(year FROM to_timestamp(discovered_on))::int AS year, count(*) FROM magneticod.torrents GROUP BY 1 ORDER BY 1;
COMMIT;
