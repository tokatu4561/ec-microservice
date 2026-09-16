\set ON_ERROR_STOP on
\pset pager off
SELECT pid,state,wait_event_type,wait_event,pg_blocking_pids(pid) AS blocking_pids,
  clock_timestamp()-xact_start AS transaction_age, left(query,160) AS query
FROM pg_stat_activity WHERE application_name='learning-load-api' ORDER BY pid;
