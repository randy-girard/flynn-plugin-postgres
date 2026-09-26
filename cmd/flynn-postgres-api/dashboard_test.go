package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/randy-girard/flynn-plugin-postgres"
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
