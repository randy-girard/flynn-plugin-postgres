package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

func TestParseSlowQueriesDropsCatalogStatements(t *testing.T) {
	raw := `[{"query":"SELECT EXISTS(SELECT $1 FROM pg_extension WHERE extname = $2)","calls":9,"mean_ms":40,"total_ms":360,"max_ms":80},{"query":"SELECT * FROM orders WHERE id = $1","calls":3,"mean_ms":12,"total_ms":36,"max_ms":20}]`
	got, err := parseSlowQueries(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Query != "SELECT * FROM orders WHERE id = $1" {
		t.Fatalf("%+v", got)
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

func captureMetricPosts(t *testing.T) *[]dashui.MetricEvent {
	t.Helper()
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
	return &posted
}

func TestNextMetricsMinute(t *testing.T) {
	now := time.Date(2026, 10, 11, 14, 3, 27, 500, time.FixedZone("x", -4*3600))
	got := nextMetricsMinute(now)
	want := time.Date(2026, 10, 11, 18, 4, 0, 0, time.UTC)
	if !got.Equal(want) || got.Location() != time.UTC {
		t.Fatalf("got %s want %s", got, want)
	}
	onBoundary := time.Date(2026, 10, 11, 18, 4, 0, 0, time.UTC)
	if got := nextMetricsMinute(onBoundary); !got.Equal(onBoundary.Add(time.Minute)) {
		t.Fatalf("on boundary got %s", got)
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
	posted := captureMetricPosts(t)

	at := time.Date(2026, 10, 11, 18, 4, 0, 0, time.UTC)
	h := newHandler(postgres.NewStore())
	h.reportInstanceMetrics(&postgres.Instance{
		ID:          "res-1",
		App:         "postgresql-harbor-12345",
		Tenant:      "shop",
		Attachments: []postgres.Attachment{{App: "shop"}},
	}, []*ct.Resource{{
		ID:         "11111111-2222-3333-4444-555555555555",
		ExternalID: "res-1",
		Apps:       []string{"aaaaaaaa-0000-0000-0000-000000000001", "aaaaaaaa-0000-0000-0000-000000000002"},
	}}, at)
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
	if len(*posted) != 3 {
		t.Fatalf("posted=%+v", *posted)
	}
	gotApp := map[string]bool{}
	for _, ev := range *posted {
		gotApp[ev.AppID] = true
		if ev.Plugin != "postgres" {
			t.Fatalf("posted=%+v", *posted)
		}
		if ev.ResourceID != "11111111-2222-3333-4444-555555555555" {
			t.Fatalf("resource_id must be the controller resource UUID: %+v", ev)
		}
		if !ev.Timestamp.Equal(at) {
			t.Fatalf("timestamp %s want minute %s", ev.Timestamp, at)
		}
		if ev.Series["service_available"] != 1 {
			t.Fatalf("series=%+v", ev.Series)
		}
	}
	for _, want := range []string{"aaaaaaaa-0000-0000-0000-000000000001", "aaaaaaaa-0000-0000-0000-000000000002", "postgresql-harbor-12345"} {
		if !gotApp[want] {
			t.Fatalf("posted apps=%v missing %s", gotApp, want)
		}
	}
}

func TestReportInstanceMetricsMatchesResourceByEnvPerResource(t *testing.T) {
	origSQL := runSQL
	t.Cleanup(func() { runSQL = origSQL })
	runSQL = func(string, string) (string, error) { return "", fmt.Errorf("dial refused") }
	posted := captureMetricPosts(t)

	at := time.Date(2026, 10, 11, 18, 5, 0, 0, time.UTC)
	h := newHandler(postgres.NewStore())
	h.reportInstanceMetrics(&postgres.Instance{ID: "res-9", App: "postgresql-harbor-12345"}, []*ct.Resource{
		{ID: "r-a", Env: map[string]string{"FLYNN_POSTGRES": "postgresql-harbor-12345"}, Apps: []string{"app-a"}},
		{ID: "r-b", ExternalID: "res-9", Apps: []string{"app-b"}},
		{ID: "r-other", Env: map[string]string{"FLYNN_POSTGRES": "postgresql-other-00000"}, Apps: []string{"app-c"}},
	}, at)
	got := map[string]bool{}
	for _, ev := range *posted {
		got[ev.ResourceID+"/"+ev.AppID] = true
		if len(ev.Series) != 2 || ev.Series["errors"] != 1 || ev.Series["service_available"] != 0 {
			t.Fatalf("failed scrape must post only errors/service_available: %+v", ev.Series)
		}
	}
	want := []string{"r-a/app-a", "r-a/postgresql-harbor-12345", "r-b/app-b", "r-b/postgresql-harbor-12345"}
	if len(*posted) != len(want) {
		t.Fatalf("posted=%v", got)
	}
	for _, k := range want {
		if !got[k] {
			t.Fatalf("posted=%v missing %s", got, k)
		}
	}
}

func TestReportInstanceMetricsWithoutResourcePostsInstanceApp(t *testing.T) {
	origSQL := runSQL
	t.Cleanup(func() { runSQL = origSQL })
	runSQL = func(string, string) (string, error) { return "50|1|2|0|20|1|1|5|0|1|0|0", nil }
	posted := captureMetricPosts(t)

	h := newHandler(postgres.NewStore())
	h.reportInstanceMetrics(&postgres.Instance{
		ID:          "res-1",
		App:         "postgresql-harbor-12345",
		Tenant:      "shop",
		Attachments: []postgres.Attachment{{App: "shop"}},
	}, nil, time.Date(2026, 10, 11, 18, 6, 0, 0, time.UTC))
	if len(*posted) != 1 {
		t.Fatalf("posted=%+v", *posted)
	}
	ev := (*posted)[0]
	if ev.AppID != "postgresql-harbor-12345" || ev.ResourceID != "res-1" {
		t.Fatalf("%+v", ev)
	}
}

// A deposed primary whose resource moved to the promoted follower must not
// post under its internal instance ID.
func TestReportInstanceMetricsSkipsDeposedWithoutResource(t *testing.T) {
	origSQL := runSQL
	t.Cleanup(func() { runSQL = origSQL })
	runSQL = func(string, string) (string, error) { return "50|1|2|0|20|1|1|5|0|1|0|0", nil }
	posted := captureMetricPosts(t)

	h := newHandler(postgres.NewStore())
	h.reportInstanceMetrics(&postgres.Instance{
		ID:   "d656964b37dc4e69",
		App:  "postgresql-swamp-20980",
		Role: postgres.RoleDeposed,
	}, []*ct.Resource{
		{ID: "c7056b27", Env: map[string]string{"FLYNN_POSTGRES": "postgresql-knoll-42396"}, Apps: []string{"app-one"}},
	}, time.Date(2026, 10, 11, 17, 39, 0, 0, time.UTC))
	if len(*posted) != 0 {
		t.Fatalf("posted=%+v", *posted)
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
	posted := captureMetricPosts(t)
	t.Cleanup(func() {
		if len(*posted) != 0 {
			t.Errorf("dashboard page views must not post samples: %+v", *posted)
		}
	})
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
