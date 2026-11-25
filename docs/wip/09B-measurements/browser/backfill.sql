-- Synthetic history for the browser check: copies of the current hourly row, scaled by a small
-- integer factor with a daily pattern (every linear identity and range constraint is preserved).
DO $$
DECLARE cols text; i int; k int; cur timestamptz;
BEGIN
  SELECT max(bucket_start) INTO cur FROM magnetico_stats.agg_hour;
  SELECT string_agg(CASE
      WHEN column_name = 'bucket_start' THEN '$1'
      WHEN column_name = 'gauge_last_at' THEN 'gauge_last_at - ($2 * interval ''1 hour'')'
      WHEN column_name = 'covered_mask' THEN '((1::bigint << 60) - 1)'
      WHEN data_type = 'bigint' THEN '(' || quote_ident(column_name) || ' * $3)::bigint'
      ELSE quote_ident(column_name) END, ', ' ORDER BY ordinal_position),
    string_agg(quote_ident(column_name), ', ' ORDER BY ordinal_position)
  INTO cols
  FROM information_schema.columns WHERE table_schema='magnetico_stats' AND table_name='agg_hour';
  FOR i IN 1..96 LOOP
    k := 1 + ((extract(hour from cur - i * interval '1 hour')::int + 6) % 12) / 3;   -- 1..4 over the day
    EXECUTE 'INSERT INTO magnetico_stats.agg_hour SELECT ' || cols || ' FROM magnetico_stats.agg_hour WHERE bucket_start = $4 ON CONFLICT DO NOTHING'
      USING cur - i * interval '1 hour', i, k, cur;
  END LOOP;
END $$;
SELECT count(*), min(bucket_start), max(bucket_start) FROM magnetico_stats.agg_hour;
SELECT count(*) AS storage_days FROM magnetico_stats.storage_day;
