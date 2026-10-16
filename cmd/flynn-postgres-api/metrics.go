package main

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/randy-girard/flynn-plugin-postgres"
	"github.com/randy-girard/flynn-plugin-postgres/internal/dashui"
	ct "github.com/randy-girard/flynn/controller/types"
)

var postgresMetricSeries = []string{
	"service_available",
	"db_size_bytes",
	"index_size_bytes",
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

// nextMetricsMinute is the UTC minute boundary after now.
func nextMetricsMinute(now time.Time) time.Time {
	return now.UTC().Truncate(time.Minute).Add(time.Minute)
}

// metricsLoop is the only producer of dashboard samples. It sleeps until each
// minute boundary, then posts every instance stamped with that minute. Both
// API replicas may post; the dashboard dedupes per minute.
func (h *handler) metricsLoop() {
	for {
		at := nextMetricsMinute(time.Now())
		time.Sleep(time.Until(at))
		h.reportAllMetrics(at)
	}
}

func (h *handler) reportAllMetrics(at time.Time) {
	h.refreshDashboardMetricsEnv()
	resources := h.allControllerResources()
	for _, inst := range h.metricsInstances(resources) {
		h.reportInstanceMetrics(inst, resources, at)
	}
}

func (h *handler) reportInstanceMetrics(inst *postgres.Instance, resources []*ct.Resource, at time.Time) {
	if inst == nil {
		return
	}
	diag := collectPostgresDiagnostics(inst)
	if diag.Series == nil {
		diag.Series = map[string]float64{"errors": 1, "service_available": 0}
	}
	for _, target := range h.metricTargets(inst, resources) {
		for _, appID := range target.apps {
			dashui.PostMetrics(dashui.MetricEvent{
				AppID:      appID,
				ResourceID: target.resourceID,
				Plugin:     "postgres",
				Timestamp:  at.UTC(),
				Series:     diag.Series,
			})
		}
	}
}

type metricTarget struct {
	resourceID string
	apps       []string
}

// metricTargets is one target per controller resource that refers to inst:
// the resource's apps plus the instance app, keyed by the resource UUID. An
// instance with no controller resource posts to its own app and instance ID,
// unless it is deposed: its resource has already moved to the promoted one.
func (h *handler) metricTargets(inst *postgres.Instance, resources []*ct.Resource) []metricTarget {
	if inst == nil {
		return nil
	}
	instApp := h.metricAppID(inst.App)
	var out []metricTarget
	for _, r := range resources {
		if !resourceRefersTo(r, inst) {
			continue
		}
		apps := append(append([]string{}, r.Apps...), instApp)
		out = append(out, metricTarget{resourceID: r.ID, apps: uniqueNonEmpty(apps...)})
	}
	if len(out) == 0 && inst.Role != postgres.RoleDeposed {
		out = append(out, metricTarget{resourceID: inst.ID, apps: uniqueNonEmpty(instApp)})
	}
	return out
}

func resourceRefersTo(r *ct.Resource, inst *postgres.Instance) bool {
	if r == nil || inst == nil || strings.TrimSpace(r.ID) == "" {
		return false
	}
	if app := strings.TrimSpace(inst.App); app != "" && strings.TrimSpace(r.Env["FLYNN_POSTGRES"]) == app {
		return true
	}
	id := strings.TrimSpace(inst.ID)
	return id != "" && (strings.TrimSpace(r.ExternalID) == id || r.ID == id)
}

// metricAppID resolves the instance app name to its UUID when the controller
// is reachable.
func (h *handler) metricAppID(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || h == nil || h.client == nil {
		return name
	}
	if app, err := h.client.GetApp(name); err == nil && app != nil && strings.TrimSpace(app.ID) != "" {
		return app.ID
	}
	return name
}

func (h *handler) refreshDashboardMetricsEnv() {
	if h == nil || h.client == nil {
		return
	}
	dashui.ResolveMetricsEnv(func(name string) map[string]string {
		rel, err := h.client.GetAppRelease(name)
		if err != nil || rel == nil {
			return nil
		}
		return rel.Env
	})
}

func uniqueNonEmpty(ids ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
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
		Series:      map[string]float64{},
		SlowQueries: []slowQuery{},
	}
	failed := func() addonDiagnostics {
		out.Series = map[string]float64{"errors": 1, "service_available": 0}
		return out
	}
	if inst == nil {
		return failed()
	}
	out.Source = firstNonEmpty(inst.App, inst.ID)
	out.Addon = firstNonEmpty(inst.ID, inst.App)
	url := strings.TrimSpace(inst.MaintenanceURL())
	if url == "" {
		return failed()
	}
	// Do not CREATE EXTENSION here: POSTGRES_USER is NOSUPERUSER. The
	// postgres job installs pg_stat_statements as the OS postgres role.
	raw, err := runSQL(url, postgres.SnapshotSQL)
	if err != nil {
		return failed()
	}
	parsed, ok := postgres.ParsePostgresSnapshot(raw)
	if !ok {
		return failed()
	}
	out.Series = parsed
	out.Series["service_available"] = 1
	out.Series["errors"] = 0
	slow, _ := collectPostgresSlowQueries(url)
	out.SlowQueries = slow
	out.Series["slow_query_count"] = float64(len(slow))
	return out
}

func collectPostgresSlowQueries(connURL string) ([]slowQuery, error) {
	if pgStatStatementsAvailable(connURL) {
		raw, err := runSQL(connURL, postgres.StatStatementsSQL)
		if err == nil {
			return parseSlowQueries(raw)
		}
	}
	raw, err := runSQL(connURL, postgres.ActivitySlowSQL)
	if err != nil {
		return nil, err
	}
	return parseSlowQueries(raw)
}

func pgStatStatementsAvailable(connURL string) bool {
	raw, err := runSQL(connURL, postgres.StatStatementsExistsSQL)
	if err != nil {
		return false
	}
	v := strings.ToLower(strings.TrimSpace(raw))
	if i := strings.IndexByte(v, '\n'); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v == "t" || v == "true"
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
	kept := out[:0]
	for _, q := range out {
		if postgres.IsCatalogSlowQuery(q.Query) {
			continue
		}
		kept = append(kept, q)
	}
	if kept == nil {
		kept = []slowQuery{}
	}
	return kept, nil
}

func (h *handler) allMetricsInstances() []*postgres.Instance {
	return h.metricsInstances(h.allControllerResources())
}

func (h *handler) metricsInstances(resources []*ct.Resource) []*postgres.Instance {
	seen := map[string]bool{}
	var out []*postgres.Instance
	add := func(inst *postgres.Instance) {
		if inst == nil {
			return
		}
		// Prefer the isolated app name so the in-memory store copy and the
		// controller resource for the same database are not both scraped.
		key := firstNonEmpty(inst.App, inst.ID)
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
	for _, r := range resources {
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
	add(inst.App)
	return out
}
