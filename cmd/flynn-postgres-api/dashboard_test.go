package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
)

func TestDashboardHidesOtherTenants(t *testing.T) {
	t.Setenv("DASHBOARD_SSO_OPTIONAL", "1")
	store := postgres.NewStore()
	a, _, err := store.Provision(postgres.ProvisionRequest{App: "shop-a", Tenant: "shop-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddDatabase(a.ID, "tenant_a_only"); err != nil {
		t.Fatal(err)
	}
	if err := store.AddUser(a.ID, "ada", "pw-a"); err != nil {
		t.Fatal(err)
	}
	b, _, err := store.Provision(postgres.ProvisionRequest{App: "shop-b", Tenant: "shop-b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddDatabase(b.ID, "tenant_b_secret"); err != nil {
		t.Fatal(err)
	}
	if err := store.AddUser(b.ID, "bea", "pw-b"); err != nil {
		t.Fatal(err)
	}
	h := newHandler(store)
	for _, path := range []string{"/dashboard/", "/dashboard/databases", "/dashboard/users", "/dashboard/backup", "/dashboard/replication"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s status %d %s", path, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if strings.Contains(body, "tenant_b_secret") || strings.Contains(body, "bea") || strings.Contains(body, b.App) {
			t.Fatalf("%s leaked tenant b: %s", path, body)
		}
		if path == "/dashboard/databases" && !strings.Contains(body, "tenant_a_only") {
			t.Fatalf("missing tenant a database: %s", body)
		}
	}
}

func TestDashCardCountsAttachedPostgres(t *testing.T) {
	t.Setenv("DASHBOARD_SSO_OPTIONAL", "1")
	store := postgres.NewStore()
	inst, _, err := store.Provision(postgres.ProvisionRequest{App: "shop-a", Tenant: "shop-a"})
	if err != nil {
		t.Fatal(err)
	}
	h := newHandler(store)
	req := httptest.NewRequest(http.MethodGet, "/dashboard/card", nil)
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"attached":true`) {
		t.Fatalf("attached: %s", body)
	}
	if strings.Contains(body, `"summary":"0 postgres instance(s)"`) {
		t.Fatalf("zero count: %s", body)
	}
	if !strings.Contains(body, inst.App) {
		t.Fatalf("missing instance name %q: %s", inst.App, body)
	}
}

func TestDashReplicationListsControllerFollower(t *testing.T) {
	t.Setenv("DASHBOARD_SSO_OPTIONAL", "1")
	h := newHandler(postgres.NewStore())
	h.listResources = func(app string) ([]*ct.Resource, error) {
		if app != "shop-a" {
			t.Fatalf("app %q", app)
		}
		return []*ct.Resource{
			{
				ID:         "res-leader",
				ProviderID: "prov-pg",
				Env: map[string]string{
					"FLYNN_POSTGRES": "pg-orchid-xkhthp",
					"PGDATABASE":     "db_a803c7ba",
					"POSTGRES_ROLE":  "primary",
				},
			},
			{
				ID:         "res-fol",
				ProviderID: "prov-pg",
				Env: map[string]string{
					"FLYNN_POSTGRES":  "pg-willow-abcdef",
					"PGDATABASE":      "db_a803c7ba",
					"POSTGRES_ROLE":   "follower",
					"POSTGRES_LEADER": "pg-orchid-xkhthp",
				},
			},
		}, nil
	}
	req := httptest.NewRequest(http.MethodGet, "/dashboard/replication", nil)
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "pg-willow-abcdef") || !strings.Contains(body, "follower") {
		t.Fatalf("replication: %s", body)
	}
	if !strings.Contains(body, "pg-orchid-xkhthp") {
		t.Fatalf("leader missing: %s", body)
	}
	req = httptest.NewRequest(http.MethodGet, "/dashboard/databases", nil)
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("databases status %d %s", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()
	if !strings.Contains(body, "pg-willow-abcdef") || !strings.Contains(body, "db_a803c7ba") {
		t.Fatalf("databases: %s", body)
	}
}

func TestDashCardCountsControllerResourceWhenStoreEmpty(t *testing.T) {
	t.Setenv("DASHBOARD_SSO_OPTIONAL", "1")
	h := newHandler(postgres.NewStore())
	h.listResources = func(app string) ([]*ct.Resource, error) {
		if app != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
			t.Fatalf("app %q", app)
		}
		return []*ct.Resource{{
			ID:         "res-1",
			ProviderID: "prov-pg",
			Env: map[string]string{
				"FLYNN_POSTGRES": "pg-harbor-kxmnpq",
				"PGDATABASE":     "db_pg_harbor_kxmnpq",
			},
		}}, nil
	}
	req := httptest.NewRequest(http.MethodGet, "/dashboard/card", nil)
	req.Header.Set("X-Flynn-Dashboard-App", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"attached":true`) || !strings.Contains(body, "pg-harbor-kxmnpq") {
		t.Fatalf("card: %s", body)
	}
}
