package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
)

func dashPost(h *handler, app, path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Flynn-Dashboard-App", app)
	req.Header.Set("X-Flynn-Dashboard-Permissions", "app:resources:write")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func twoTenants(t *testing.T) (*postgres.Store, *postgres.Instance, *postgres.Instance) {
	t.Helper()
	t.Setenv("DASHBOARD_SSO_OPTIONAL", "1")
	store := postgres.NewStore()
	a, _, err := store.Provision(postgres.ProvisionRequest{App: "shop-a", Tenant: "shop-a"})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := store.Provision(postgres.ProvisionRequest{App: "shop-b", Tenant: "shop-b"})
	if err != nil {
		t.Fatal(err)
	}
	return store, a, b
}

// TestDashRejectsInstanceOfAnotherApp: a session for shop-a that names shop-b's
// instance (by app name, id, or controller resource id) must not reach it.
func TestDashRejectsInstanceOfAnotherApp(t *testing.T) {
	posts := []struct {
		path string
		form url.Values
	}{
		{"/dashboard/settings", url.Values{"action": {"delete"}, "instance": {"B_APP"}}},
		{"/dashboard/replication", url.Values{"action": {"promote"}, "instance": {"B_APP"}}},
		{"/dashboard/replication", url.Values{"action": {"unfollow"}, "instance": {"B_ID"}}},
		{"/dashboard/replication", url.Values{"action": {"remove"}, "instance": {"B_ID"}}},
		{"/dashboard/replication", url.Values{"action": {"wait"}, "instance": {"B_ID"}}},
		{"/dashboard/replication", url.Values{"action": {"follow"}, "instance": {"B_APP"}}},
		{"/dashboard/replication", url.Values{"action": {"upgrade"}, "instance": {"B_APP"}}},
		{"/dashboard/users", url.Values{"name": {"evil"}, "password": {"pw"}, "instance": {"B_APP"}}},
		{"/dashboard/api/users", url.Values{"name": {"evil"}, "password": {"pw"}, "instance": {"B_APP"}}},
		{"/dashboard/api/users/drop", url.Values{"name": {"evil"}, "instance": {"B_APP"}}},
		{"/dashboard/databases", url.Values{"name": {"evildb"}, "instance": {"B_APP"}}},
		{"/dashboard/api/databases", url.Values{"name": {"evildb"}, "instance": {"B_ID"}}},
	}
	for _, p := range posts {
		store, _, b := twoTenants(t)
		if p.form.Get("instance") == "B_APP" {
			p.form.Set("instance", b.App)
		} else {
			p.form.Set("instance", b.ID)
		}
		h := newHandler(store)
		h.listResources = func(app string) ([]*ct.Resource, error) { return nil, nil }
		wantDBs := len(b.Databases)
		rec := dashPost(h, "shop-a", p.path, p.form)
		if rec.Code == 200 {
			t.Errorf("%s %v: foreign instance accepted: %s", p.path, p.form, rec.Body.String())
		}
		got, err := store.Get(b.ID)
		if err != nil {
			t.Errorf("%s %v: shop-b instance was removed: %v", p.path, p.form, err)
			continue
		}
		if len(got.Users) != 0 || len(got.Databases) != wantDBs || got.Role != b.Role {
			t.Errorf("%s %v: shop-b mutated: users=%v dbs=%v role=%s", p.path, p.form, got.Users, got.Databases, got.Role)
		}
	}

	store, _, b := twoTenants(t)
	h := newHandler(store)
	req := httptest.NewRequest(http.MethodGet, "/dashboard/api/progress?instance="+b.ID, nil)
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	req.Header.Set("X-Flynn-Dashboard-Permissions", "app:resources:write")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), b.App) {
		t.Fatalf("progress leaked foreign instance: %s", rec.Body.String())
	}
}

// The session app's own instance still works by app name, by id, and by the
// controller resource id.
func TestDashOwnInstanceStillResolves(t *testing.T) {
	store, a, _ := twoTenants(t)
	h := newHandler(store)
	h.listResources = func(app string) ([]*ct.Resource, error) {
		if app != "shop-a" {
			return nil, nil
		}
		return []*ct.Resource{{
			ID:         "controller-res-uuid",
			ProviderID: "postgres",
			Env:        map[string]string{"FLYNN_POSTGRES": a.App, "FLYNN_POSTGRES_ROLE": "primary"},
		}}, nil
	}
	for i, ref := range []string{a.App, a.ID, "controller-res-uuid"} {
		db := "okdb" + string(rune('a'+i))
		rec := dashPost(h, "shop-a", "/dashboard/api/databases", url.Values{"name": {db}, "instance": {ref}})
		if rec.Code != 200 {
			t.Fatalf("own instance %q: %d %s", ref, rec.Code, rec.Body.String())
		}
	}
	rec := dashPost(h, "shop-a", "/dashboard/api/users", url.Values{"name": {"ada"}, "password": {"pw"}, "instance": {a.App}})
	if rec.Code != 200 {
		t.Fatalf("own user create: %d %s", rec.Code, rec.Body.String())
	}
	rec = dashPost(h, "shop-a", "/dashboard/settings", url.Values{"action": {"delete"}, "instance": {"controller-res-uuid"}})
	if rec.Code != 200 {
		t.Fatalf("own delete: %d %s", rec.Code, rec.Body.String())
	}
}
