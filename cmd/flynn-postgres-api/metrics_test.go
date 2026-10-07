package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/randy-girard/flynn-plugin-postgres"
	"github.com/randy-girard/flynn-plugin-postgres/internal/dashui"
	ct "github.com/randy-girard/flynn/controller/types"
)

func TestReportInstanceMetricsDoesNotRaiseIntoServerLog(t *testing.T) {
	for _, q := range []string{postgres.SnapshotSQL, postgres.StatStatementsExistsSQL, postgres.StatStatementsSQL, postgres.ActivitySlowSQL} {
		lower := strings.ToLower(q)
		if strings.Contains(lower, "log_min_messages") || strings.Contains(q, "RAISE") || strings.Contains(q, "$flynn_metrics$") {
			t.Fatalf("metrics SQL must not RAISE into the server log: %s", q)
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
	var queries []string
	runSQL = func(connURL, query string) (string, error) {
		queries = append(queries, query)
		if strings.Contains(query, "concat_ws") {
			return "100|2|1|0|50|1|1|10|0|3|0|0", nil
		}
		if strings.Contains(query, "pg_extension") {
			return "t", nil
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
	for _, q := range queries {
		if strings.Contains(strings.ToUpper(q), "CREATE EXTENSION") {
			t.Fatalf("metrics must not CREATE EXTENSION as the tenant role: %s", q)
		}
	}
}

func TestCollectPostgresDiagnosticsFailedIsSparse(t *testing.T) {
	orig := runSQL
	t.Cleanup(func() { runSQL = orig })
	runSQL = func(string, string) (string, error) {
		return "", fmt.Errorf("dial refused")
	}
	diag := collectPostgresDiagnostics(&postgres.Instance{ID: "res-1", App: "postgresql-harbor-12345"})
	if diag.Series["service_available"] != 0 || diag.Series["errors"] != 1 {
		t.Fatalf("%+v", diag.Series)
	}
	if _, ok := diag.Series["db_size_bytes"]; ok {
		t.Fatalf("failed scrape must not fill gauges: %+v", diag.Series)
	}
	if _, ok := diag.Series["max_connections"]; ok {
		t.Fatalf("failed scrape must not fill gauges: %+v", diag.Series)
	}
}

func TestCollectPostgresSlowQueriesSkipsMissingExtension(t *testing.T) {
	orig := runSQL
	t.Cleanup(func() { runSQL = orig })
	var queries []string
	runSQL = func(_ string, query string) (string, error) {
		queries = append(queries, query)
		if strings.Contains(query, "pg_extension") {
			return "f", nil
		}
		if strings.Contains(query, "pg_stat_activity") {
			return `[{"query":"SELECT now()","calls":1,"mean_ms":800,"total_ms":800,"max_ms":800}]`, nil
		}
		if strings.Contains(query, "FROM pg_stat_statements") {
			t.Fatal("must not query pg_stat_statements when the extension is missing")
		}
		return "", fmt.Errorf("unexpected %s", query)
	}
	got, err := collectPostgresSlowQueries("postgres://ignored")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Query != "SELECT now()" {
		t.Fatalf("%+v", got)
	}
}

func TestReportInstanceMetricsPostsWebhook(t *testing.T) {
	origSQL := runSQL
	var queries []string
	t.Cleanup(func() { runSQL = origSQL })
	runSQL = func(_ string, query string) (string, error) {
		queries = append(queries, query)
		return "50|1|2|0|20|1|1|5|0|1|0|0", nil
	}

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
	var raise bool
	for _, q := range queries {
		lower := strings.ToLower(q)
		if strings.Contains(lower, "log_min_messages") {
			t.Fatalf("tenant cannot SET log_min_messages: %s", q)
		}
		if strings.Contains(q, "RAISE LOG") || strings.Contains(q, "$flynn_metrics$") {
			raise = true
		}
	}
	if raise {
		t.Fatalf("must not RAISE LOG into the instance server log: %v", queries)
	}
	if len(posted) != 2 {
		t.Fatalf("posted=%+v", posted)
	}
	gotApp := map[string]bool{}
	for _, ev := range posted {
		gotApp[ev.AppID] = true
		if ev.Plugin != "postgres" {
			t.Fatalf("posted=%+v", posted)
		}
		if ev.Series["service_available"] != 1 {
			t.Fatalf("series=%+v", ev.Series)
		}
	}
	if !gotApp["shop"] || !gotApp["postgresql-harbor-12345"] {
		t.Fatalf("posted apps=%v", gotApp)
	}
}

func TestAllMetricsInstancesDedupByApp(t *testing.T) {
	store := postgres.NewStore()
	inst, _, err := store.Provision(postgres.ProvisionRequest{App: "shop", Tenant: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	h := newHandler(store)
	h.listAllResources = func() ([]*ct.Resource, error) {
		return []*ct.Resource{{
			ID: "controller-copy",
			Env: map[string]string{
				"FLYNN_POSTGRES": inst.App,
				"DATABASE_URL":   "postgres://db",
			},
		}}, nil
	}
	got := h.allMetricsInstances()
	if len(got) != 1 {
		t.Fatalf("got %d instances, want 1 (store+controller copies of the same app)", len(got))
	}
	if got[0].App != inst.App {
		t.Fatalf("app=%q want %q", got[0].App, inst.App)
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
