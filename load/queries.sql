\set ON_ERROR_STOP on
\pset pager off
-- EXPLAINや整合性照合より先に実行し、それらのSQLを混ぜない。
SELECT query, sum(calls) AS calls,
  round(sum(calls)::numeric / NULLIF((SELECT count(*) FROM orders),0),2) AS calls_per_order,
  round(sum(total_exec_time)::numeric,2) AS total_ms,
  round((sum(total_exec_time)/NULLIF(sum(calls),0))::numeric,3) AS mean_ms,
  sum(rows) AS rows, sum(shared_blks_hit) AS cache_hits, sum(shared_blks_read) AS block_reads
FROM pg_stat_statements
WHERE dbid=(SELECT oid FROM pg_database WHERE datname=current_database())
  AND query NOT ILIKE '%pg_stat%'
GROUP BY query ORDER BY sum(total_exec_time) DESC LIMIT 20;
