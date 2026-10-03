package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/randy-girard/flynn-plugin-postgres"
	"github.com/randy-girard/flynn-plugin-postgres/internal/dashui"
	ct "github.com/randy-girard/flynn/controller/types"
)

const metricsInterval = 20 * time.Second

var postgresMetricSeries = []string{
	"service_available",
	"db_size_bytes",
	"tables",
	"active_connections",
	"waiting_connections",
	"max_connections",
	"index_cache_hit_rate",
	"table_cache_hit_rate",
	"current_transaction",
	"xact_commit",
	"wal_bytes",
	"replay_lag_seconds",
	"follower_lag_bytes",
	"slow_query_count",
	"errors",
}

var metricsLogLine = func(line string) {
	fmt.Fprintln(os.Stderr, line)
}

type slowQuery struct {
	Query   string  `json:"query"`
	Calls   int64   `json:"calls"`
	MeanMS  float64 `json:"mean_ms"`
	TotalMS float64 `json:"total_ms"`
	MaxMS   float64 `json:"max_ms"`
}

type addonDiagnostics struct {
	Source      string             `json:"source"`
	Addon       string             `json:"addon"`
	Plugin      string             `json:"plugin"`
	Series      map[string]float64 `json:"series"`
	SlowQueries []slowQuery        `json:"slow_queries"`
}

const postgresSnapshotSQL = `SELECT concat_ws('|',
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

const pgStatStatementsSQL = `SELECT COALESCE(json_agg(t), '[]'::json) FROM (
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

const pgActivitySlowSQL = `SELECT COALESCE(json_agg(t), '[]'::json) FROM (
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

func (h *handler) metricsLoop() {
	h.reportAllMetrics()
	ticker := time.NewTicker(metricsInterval)
	defer ticker.Stop()
	for range ticker.C {
		h.reportAllMetrics()
	}
}

func (h *handler) reportAllMetrics() {
	for _, inst := range h.allMetricsInstances() {
		h.reportInstanceMetrics(inst)
	}
}

func (h *handler) reportInstanceMetrics(inst *postgres.Instance) {
	if inst == nil {
		return
	}
	diag := collectPostgresDiagnostics(inst)
	if diag.Series == nil {
		diag.Series = map[string]float64{"errors": 1, "service_available": 0}
	}
	source := firstNonEmpty(inst.App, inst.ID)
	addon := firstNonEmpty(inst.ID, inst.App)
	metricsLogLine(formatHerokuPostgresLine(source, addon, diag.Series))
	for _, appID := range instanceMetricApps(inst) {
		dashui.PostMetrics(dashui.MetricEvent{
			AppID:      appID,
			ResourceID: inst.ID,
			Plugin:     "postgres",
			Series:     diag.Series,
		})
	}
}

func (h *handler) diagnosticsFor(sess *dashui.Session) addonDiagnostics {
	empty := addonDiagnostics{Plugin: "postgres", Series: map[string]float64{}, SlowQueries: []slowQuery{}}
	if sess == nil {
		return empty
	}
	insts := h.instancesFor(sess)
	if len(insts) == 0 {
		return empty
	}
	return collectPostgresDiagnostics(insts[0])
}

func collectPostgresDiagnostics(inst *postgres.Instance) addonDiagnostics {
	out := addonDiagnostics{
		Plugin:      "postgres",
		Source:      "",
		Addon:       "",
		Series:      emptyPostgresSeries(),
		SlowQueries: []slowQuery{},
	}
	if inst == nil {
		out.Series["errors"] = 1
		return out
	}
	out.Source = firstNonEmpty(inst.App, inst.ID)
	out.Addon = firstNonEmpty(inst.ID, inst.App)
	url := strings.TrimSpace(inst.MaintenanceURL())
	if url == "" {
		out.Series["errors"] = 1
		return out
	}
	_, _ = runSQL(url, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements")
	raw, err := runSQL(url, postgresSnapshotSQL)
	if err != nil {
		out.Series["errors"] = 1
		out.Series["service_available"] = 0
		return out
	}
	if parsed, ok := parsePostgresSnapshot(raw); ok {
		for k, v := range parsed {
			out.Series[k] = v
		}
		out.Series["service_available"] = 1
		out.Series["errors"] = 0
	} else {
		out.Series["errors"] = 1
		out.Series["service_available"] = 0
	}
	slow, _ := collectPostgresSlowQueries(url)
	out.SlowQueries = slow
	out.Series["slow_query_count"] = float64(len(slow))
	return out
}

func collectPostgresSlowQueries(connURL string) ([]slowQuery, error) {
	raw, err := runSQL(connURL, pgStatStatementsSQL)
	if err != nil {
		raw, err = runSQL(connURL, pgActivitySlowSQL)
		if err != nil {
			return nil, err
		}
	}
	return parseSlowQueries(raw)
}

func parseSlowQueries(raw string) ([]slowQuery, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return []slowQuery{}, nil
	}
	var out []slowQuery
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []slowQuery{}
	}
	return out, nil
}

func parsePostgresSnapshot(raw string) (map[string]float64, bool) {
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

func emptyPostgresSeries() map[string]float64 {
	out := make(map[string]float64, len(postgresMetricSeries))
	for _, name := range postgresMetricSeries {
		out[name] = 0
	}
	return out
}

func formatHerokuPostgresLine(source, addon string, series map[string]float64) string {
	if series == nil {
		series = map[string]float64{}
	}
	return strings.Join([]string{
		"heroku-postgres",
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

func (h *handler) allMetricsInstances() []*postgres.Instance {
	seen := map[string]bool{}
	var out []*postgres.Instance
	add := func(inst *postgres.Instance) {
		if inst == nil {
			return
		}
		key := firstNonEmpty(inst.ID, inst.App)
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		if inst.ID != "" {
			seen[inst.ID] = true
		}
		if inst.App != "" {
			seen[inst.App] = true
		}
		out = append(out, inst)
	}
	if h != nil && h.store != nil {
		for _, inst := range h.store.All() {
			add(inst)
		}
	}
	for _, r := range h.allControllerResources() {
		inst := instanceFromResource(r, "")
		if inst == nil {
			continue
		}
		h.hydrateInstanceAttachments(inst, r)
		h.enrichFromLive(inst)
		add(inst)
	}
	return out
}

func (h *handler) allControllerResources() []*ct.Resource {
	if h == nil {
		return nil
	}
	if h.listAllResources != nil {
		list, err := h.listAllResources()
		if err != nil {
			return nil
		}
		return list
	}
	if h.client != nil {
		list, err := h.client.ResourceListAll()
		if err != nil {
			return nil
		}
		return list
	}
	return nil
}

func instanceMetricApps(inst *postgres.Instance) []string {
	if inst == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, a := range inst.Attachments {
		add(a.App)
	}
	add(inst.Tenant)
	return out
}
