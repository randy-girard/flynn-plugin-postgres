package postgres

import (
	"strings"
	"testing"
)

func TestParsePostgresSnapshot(t *testing.T) {
	raw := "26322537984|16|4|0|200|0.97144|0.97086|1200|4096|5609354|0|12\n"
	got, ok := ParsePostgresSnapshot(raw)
	if !ok {
		t.Fatal("parse failed")
	}
	if got["db_size_bytes"] != 26322537984 || got["tables"] != 16 || got["active_connections"] != 4 {
		t.Fatalf("%+v", got)
	}
	if got["index_cache_hit_rate"] != 0.97144 || got["follower_lag_bytes"] != 12 {
		t.Fatalf("%+v", got)
	}
}

func TestFormatFlynnPostgresLine(t *testing.T) {
	line := FormatFlynnPostgresLine("postgresql-basin-73690", "res-abc", map[string]float64{
		"service_available":    1,
		"db_size_bytes":        1024,
		"tables":               3,
		"active_connections":   2,
		"waiting_connections":  0,
		"max_connections":      100,
		"index_cache_hit_rate": 0.99,
		"table_cache_hit_rate": 0.98,
		"current_transaction":  12,
		"xact_commit":          8,
		"wal_bytes":            64,
		"follower_lag_bytes":   0,
		"replay_lag_seconds":   0.25,
		"slow_query_count":     1,
	})
	for _, want := range []string{
		"flynn-postgres",
		"source=postgresql-basin-73690",
		"addon=res-abc",
		"sample#service-available=1",
		"sample#db_size=1024bytes",
		"sample#active-connections=2",
		"sample#index-cache-hit-rate=0.99000",
		"sample#slow-queries=1",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing %q in %s", want, line)
		}
	}
}

func TestFormatFlynnPostgresLineOmitsMissingGauges(t *testing.T) {
	line := FormatFlynnPostgresLine("postgresql-basin-73690", "res-abc", map[string]float64{
		"service_available": 0,
		"errors":            1,
	})
	if !strings.Contains(line, "sample#service-available=0") {
		t.Fatalf("missing health sample: %s", line)
	}
	if strings.Contains(line, "sample#db_size=") || strings.Contains(line, "sample#active-connections=") {
		t.Fatalf("failed scrape must not emit zero gauges: %s", line)
	}
}

func TestInstanceMetricIDsUseResourceApp(t *testing.T) {
	t.Setenv("FLYNN_POSTGRES", "postgresql-basin-73690")
	t.Setenv("FLYNN_APP_NAME", "postgresql-basin-73690")
	t.Setenv(ResourceIDEnv, "39cd10de1737e3e7")
	t.Setenv("POSTGRES_DB", "appdb")
	source, addon := InstanceMetricIDs()
	if source != "postgresql-basin-73690" {
		t.Fatalf("source=%q", source)
	}
	if addon != "39cd10de1737e3e7" {
		t.Fatalf("addon=%q", addon)
	}
}

func TestInstanceMetricIDsFallback(t *testing.T) {
	t.Setenv("FLYNN_POSTGRES", "")
	t.Setenv("FLYNN_APP_NAME", "")
	t.Setenv(ResourceIDEnv, "")
	t.Setenv("POSTGRES_DB", "appdb")
	source, addon := InstanceMetricIDs()
	if source != "appdb" || addon != "appdb" {
		t.Fatalf("source=%q addon=%q", source, addon)
	}
}

func TestSnapshotSQLSkipsXactIdOnReplica(t *testing.T) {
	if !strings.Contains(SnapshotSQL, "pg_is_in_recovery()") || !strings.Contains(SnapshotSQL, "pg_current_xact_id()") {
		t.Fatal("replica metrics must not call pg_current_xact_id() during recovery")
	}
	idx := strings.Index(SnapshotSQL, "pg_current_xact_id()")
	window := SnapshotSQL[:idx]
	if !strings.Contains(window[strings.LastIndex(window, "CASE"):], "pg_is_in_recovery()") {
		t.Fatal("pg_current_xact_id must be behind a recovery check")
	}
}

func TestSnapshotSQLDoesNotRaise(t *testing.T) {
	for _, q := range []string{SnapshotSQL, StatStatementsExistsSQL, StatStatementsSQL, ActivitySlowSQL} {
		if strings.Contains(q, "RAISE") || strings.Contains(strings.ToLower(q), "log_min_messages") {
			t.Fatalf("instance metrics SQL must not RAISE into the server log: %s", q)
		}
	}
}
