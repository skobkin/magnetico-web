DO $$
DECLARE cols text; i int; cur timestamptz;
BEGIN
  SELECT max(bucket_start) INTO cur FROM magnetico_stats.storage_day;
  SELECT string_agg(CASE
      WHEN column_name = 'bucket_start' THEN '$1'
      WHEN data_type = 'bigint' AND column_name NOT LIKE '%\_n' THEN '(' || quote_ident(column_name) || ' * (100 - $2) / 100)::bigint'
      ELSE quote_ident(column_name) END, ', ' ORDER BY ordinal_position)
  INTO cols FROM information_schema.columns WHERE table_schema='magnetico_stats' AND table_name='storage_day';
  FOR i IN 1..20 LOOP
    EXECUTE 'INSERT INTO magnetico_stats.storage_day SELECT ' || cols || ' FROM magnetico_stats.storage_day WHERE bucket_start = $3 ON CONFLICT DO NOTHING'
      USING cur - i * interval '1 day', i, cur;
  END LOOP;
END $$;
SELECT count(*), min(bucket_start), max(bucket_start) FROM magnetico_stats.storage_day;
