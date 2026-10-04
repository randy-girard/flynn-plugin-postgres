package postgres

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// SnapshotSQL is the instance metrics probe. The isolated postgres job prints
// the formatted sample to its own stdout so flynn-host can tag it as a system
// line on the resource app (flynn[postgres.N], same as host job metrics).
const SnapshotSQL = `SELECT concat_ws('|',
  COALESCE((SELECT SUM(pg_database_size(oid)) FROM pg_database WHERE datallowconn),0),
  COALESCE((SELECT COUNT(*) FROM pg_stat_user_tables),0),
  COALESCE((SELECT COUNT(*) FROM pg_stat_activity WHERE state IS DISTINCT FROM 'idle' AND pid <> pg_backend_pid()),0),
  COALESCE((SELECT COUNT(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock'),0),
  COALESCE((SELECT setting::int FROM pg_settings WHERE name = 'max_connections'),0),
  COALESCE((SELECT CASE WHEN SUM(idx_blks_hit+idx_blks_read)=0 THEN 1 ELSE SUM(idx_blks_hit)::float/SUM(idx_blks_hit+idx_blks_read) END FROM pg_statio_user_indexes),1),
  COALESCE((SELECT CASE WHEN SUM(heap_blks_hit+heap_blks_read)=0 THEN 1 ELSE SUM(heap_blks_hit)::float/SUM(heap_blks_hit+heap_blks_read) END FROM pg_statio_user_tables),1),
  COALESCE((SELECT SUM(xact_commit) FROM pg_stat_database),0),
  COALESCE((SELECT SUM(wal_bytes) FROM pg_stat_wal),0),
  COALESCE((SELECT pg_current_xact_id()::text::bigint),0),
  COALESCE((SELECT CASE WHEN pg_is_in_recovery() THEN EXTRACT(EPOCH FROM (now() - pg_last_xact_replay_timestamp())) ELSE 0 END),0),
  COALESCE((SELECT CASE WHEN pg_is_in_recovery() THEN pg_wal_lsn_diff(pg_last_wal_receive_lsn(), pg_last_wal_replay_lsn()) ELSE 0 END),0)
);`

const StatStatementsExistsSQL = `SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname = 'pg_stat_statements')`

const StatStatementsSQL = `SELECT COALESCE(json_agg(t), '[]'::json) FROM (
  SELECT left(regexp_replace(query, E'[\\n\\r]+', ' ', 'g'), 240) AS query,
         calls,
         mean_exec_time AS mean_ms,
         total_exec_time AS total_ms,
         max_exec_time AS max_ms
  FROM pg_stat_statements
  WHERE query NOT ILIKE '%pg_stat_statements%'
  ORDER BY mean_exec_time DESC
  LIMIT 15
) t`

const ActivitySlowSQL = `SELECT COALESCE(json_agg(t), '[]'::json) FROM (
  SELECT left(regexp_replace(query, E'[\\n\\r]+', ' ', 'g'), 240) AS query,
         1::bigint AS calls,
         EXTRACT(EPOCH FROM (now()-query_start))*1000 AS mean_ms,
         EXTRACT(EPOCH FROM (now()-query_start))*1000 AS total_ms,
         EXTRACT(EPOCH FROM (now()-query_start))*1000 AS max_ms
  FROM pg_stat_activity
  WHERE state = 'active'
    AND pid <> pg_backend_pid()
    AND query_start < now() - interval '500 milliseconds'
    AND query NOT ILIKE '%pg_stat%'
  ORDER BY query_start
  LIMIT 15
) t`

// InstanceMetricIDs are the source/addon fields stamped on sample# lines from
// the isolated postgres job (FLYNN_POSTGRES is the resource app name).
func InstanceMetricIDs() (source, addon string) {
	source = firstNonEmpty(os.Getenv("FLYNN_POSTGRES"), os.Getenv("FLYNN_APP_NAME"), os.Getenv("POSTGRES_DB"), "postgres")
	addon = firstNonEmpty(os.Getenv(ResourceIDEnv), source)
	return source, addon
}

func ParsePostgresSnapshot(raw string) (map[string]float64, bool) {
	line := strings.TrimSpace(raw)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	parts := strings.Split(line, "|")
	if len(parts) < 12 {
		return nil, false
	}
	nums := make([]float64, 12)
	for i := 0; i < 12; i++ {
		n, err := strconv.ParseFloat(strings.TrimSpace(parts[i]), 64)
		if err != nil {
			return nil, false
		}
		nums[i] = n
	}
	return map[string]float64{
		"db_size_bytes":        nums[0],
		"tables":               nums[1],
		"active_connections":   nums[2],
		"waiting_connections":  nums[3],
		"max_connections":      nums[4],
		"index_cache_hit_rate": nums[5],
		"table_cache_hit_rate": nums[6],
		"xact_commit":          nums[7],
		"wal_bytes":            nums[8],
		"current_transaction":  nums[9],
		"replay_lag_seconds":   nums[10],
		"follower_lag_bytes":   nums[11],
	}, true
}

func FormatFlynnPostgresLine(source, addon string, series map[string]float64) string {
	if series == nil {
		series = map[string]float64{}
	}
	return strings.Join([]string{
		"flynn-postgres",
		"source=" + firstNonEmpty(source, "postgres"),
		"addon=" + firstNonEmpty(addon, source),
		fmt.Sprintf("sample#service-available=%.0f", series["service_available"]),
		fmt.Sprintf("sample#db_size=%.0fbytes", series["db_size_bytes"]),
		fmt.Sprintf("sample#tables=%.0f", series["tables"]),
		fmt.Sprintf("sample#active-connections=%.0f", series["active_connections"]),
		fmt.Sprintf("sample#waiting-connections=%.0f", series["waiting_connections"]),
		fmt.Sprintf("sample#max-connections=%.0f", series["max_connections"]),
		fmt.Sprintf("sample#index-cache-hit-rate=%.5f", series["index_cache_hit_rate"]),
		fmt.Sprintf("sample#table-cache-hit-rate=%.5f", series["table_cache_hit_rate"]),
		fmt.Sprintf("sample#current_transaction=%.0f", series["current_transaction"]),
		fmt.Sprintf("sample#xact-commit=%.0f", series["xact_commit"]),
		fmt.Sprintf("sample#wal-bytes=%.0f", series["wal_bytes"]),
		fmt.Sprintf("sample#follower-lag-bytes=%.0f", series["follower_lag_bytes"]),
		fmt.Sprintf("sample#replay-lag-seconds=%.3f", series["replay_lag_seconds"]),
		fmt.Sprintf("sample#slow-queries=%.0f", series["slow_query_count"]),
	}, " ")
}
