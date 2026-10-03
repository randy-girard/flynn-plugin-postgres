package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/randy-girard/flynn-plugin-postgres"
	"github.com/randy-girard/flynn-plugin-postgres/internal/dashui"
)

func TestParsePostgresSnapshot(t *testing.T) {
	raw := "26322537984|16|4|0|200|0.97144|0.97086|1200|4096|5609354|0|12\n"
	got, ok := parsePostgresSnapshot(raw)
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

func TestFormatHerokuPostgresLine(t *testing.T) {
	line := formatHerokuPostgresLine("postgresql-harbor-12345", "res-abc", map[string]float64{
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
		"heroku-postgres",
		"source=postgresql-harbor-12345",
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

func TestParseSlowQueries(t *testing.T) {
	raw := `[{"query":"SELECT 1","calls":12,"mean_ms":140.2,"total_ms":1680,"max_ms":400}]`
	got, err := parseSlowQueries(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Query != "SELECT 1" || got[0].Calls != 12 {
		t.Fatalf("%+v", got)
	}
}

func TestCollectPostgresDiagnosticsUsesSQL(t *testing.T) {
	orig := runSQL
	t.Cleanup(func() { runSQL = orig })
	runSQL = func(connURL, query string) (string, error) {
		if strings.Contains(query, "concat_ws") {
			return "100|2|1|0|50|1|1|10|0|3|0|0", nil
		}
		if strings.Contains(query, "pg_stat_statements") && strings.Contains(query, "json_agg") {
			return `[{"query":"SELECT slow","calls":4,"mean_ms":210,"total_ms":840,"max_ms":400}]`, nil
		}
		return "", nil
	}
	inst := &postgres.Instance{ID: "res-1", App: "postgresql-harbor-12345", Tenant: "shop"}
	diag := collectPostgresDiagnostics(inst)
	if diag.Series["db_size_bytes"] != 100 || diag.Series["service_available"] != 1 {
		t.Fatalf("%+v", diag.Series)
	}
	if len(diag.SlowQueries) != 1 || diag.SlowQueries[0].Query != "SELECT slow" {
		t.Fatalf("%+v", diag.SlowQueries)
	}
}

func TestReportInstanceMetricsPostsAndLogs(t *testing.T) {
	origSQL := runSQL
	origLog := metricsLogLine
	var logs []string
	t.Cleanup(func() {
		runSQL = origSQL
		metricsLogLine = origLog
	})
	runSQL = func(string, string) (string, error) {
		return "50|1|2|0|20|1|1|5|0|1|0|0", nil
	}
	metricsLogLine = func(line string) { logs = append(logs, line) }

	var posted []dashui.MetricEvent
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev dashui.MetricEvent
		if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
			t.Errorf("decode: %v", err)
		}
		posted = append(posted, ev)
		w.WriteHeader(204)
	}))
	t.Cleanup(ts.Close)
	t.Setenv("DASHBOARD_METRICS_URL", ts.URL)

	h := newHandler(postgres.NewStore())
	h.reportInstanceMetrics(&postgres.Instance{
		ID:          "res-1",
		App:         "postgresql-harbor-12345",
		Tenant:      "shop",
		Attachments: []postgres.Attachment{{App: "shop"}},
	})
	if len(logs) != 1 || !strings.Contains(logs[0], "heroku-postgres") {
		t.Fatalf("logs=%v", logs)
	}
	if len(posted) != 1 || posted[0].AppID != "shop" || posted[0].Plugin != "postgres" {
		t.Fatalf("posted=%+v", posted)
	}
	if posted[0].Series["service_available"] != 1 {
		t.Fatalf("series=%+v", posted[0].Series)
	}
}

func TestDashboardMetricsAndDiagnostics(t *testing.T) {
	t.Setenv("DASHBOARD_SSO_OPTIONAL", "1")
	orig := runSQL
	t.Cleanup(func() { runSQL = orig })
	runSQL = func(string, string) (string, error) {
		return "10|1|1|0|20|1|1|2|0|1|0|0", nil
	}
	store := postgres.NewStore()
	if _, _, err := store.Provision(postgres.ProvisionRequest{App: "shop-a", Tenant: "shop-a"}); err != nil {
		t.Fatal(err)
	}
	h := newHandler(store)
	req := httptest.NewRequest(http.MethodGet, "/dashboard/metrics", nil)
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("metrics %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "db_size_bytes") || !strings.Contains(body, "active_connections") {
		t.Fatalf("metrics page: %s", body)
	}

	req = httptest.NewRequest(http.MethodGet, "/dashboard/api/diagnostics", nil)
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("diagnostics %d %s", rec.Code, rec.Body.String())
	}
	var diag addonDiagnostics
	if err := json.Unmarshal(rec.Body.Bytes(), &diag); err != nil {
		t.Fatal(err)
	}
	if diag.Plugin != "postgres" || diag.Series["service_available"] != 1 {
		t.Fatalf("%+v", diag)
	}
}
