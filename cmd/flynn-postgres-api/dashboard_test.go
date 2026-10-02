package main

import (
	"encoding/json"
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
	if err := store.AddUser(a.ID, "ada", "pw-a", ""); err != nil {
		t.Fatal(err)
	}
	b, _, err := store.Provision(postgres.ProvisionRequest{App: "shop-b", Tenant: "shop-b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddDatabase(b.ID, "tenant_b_secret"); err != nil {
		t.Fatal(err)
	}
	if err := store.AddUser(b.ID, "bea", "pw-b", ""); err != nil {
		t.Fatal(err)
	}
	h := newHandler(store)
	for _, path := range []string{"/dashboard/", "/dashboard/databases", "/dashboard/users", "/dashboard/backup", "/dashboard/replication", "/dashboard/settings"} {
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
		if path == "/dashboard/databases" && !strings.Contains(body, "Create database") {
			t.Fatalf("missing create database: %s", body)
		}
		if path == "/dashboard/backup" && !strings.Contains(body, "Download dump") {
			t.Fatalf("missing dump: %s", body)
		}
		if path == "/dashboard/replication" && strings.Contains(body, `name="replication"`) {
			t.Fatalf("follower form still has replication mode: %s", body)
		}
		if path == "/dashboard/settings" && !strings.Contains(body, "Delete resource") {
			t.Fatalf("missing delete resource: %s", body)
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

func TestDashReplicationUpgradeButton(t *testing.T) {
	t.Setenv("DASHBOARD_SSO_OPTIONAL", "1")
	store := postgres.NewStore()
	inst, _, err := store.Provision(postgres.ProvisionRequest{App: "shop-a", Tenant: "shop-a"})
	if err != nil {
		t.Fatal(err)
	}
	h := newHandler(store)
	req := httptest.NewRequest(http.MethodGet, "/dashboard/replication", nil)
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Upgrade") || !strings.Contains(rec.Body.String(), "pg:upgrade") {
		t.Fatalf("missing upgrade: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "Logical (major upgrade)") {
		t.Fatalf("add follower must not offer logical: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Add follower") {
		t.Fatalf("missing add follower: %s", rec.Body.String())
	}
	form := strings.NewReader("instance=" + inst.ID)
	req = httptest.NewRequest(http.MethodPost, "/dashboard/replication", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("post %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Upgrade started") {
		t.Fatalf("post body: %s", rec.Body.String())
	}
}

func TestDashReplicationJSONFollow(t *testing.T) {
	t.Setenv("DASHBOARD_SSO_OPTIONAL", "1")
	store := postgres.NewStore()
	inst, _, err := store.Provision(postgres.ProvisionRequest{App: "shop-a", Tenant: "shop-a"})
	if err != nil {
		t.Fatal(err)
	}
	h := newHandler(store)
	form := strings.NewReader("action=follow&instance=" + inst.App + "&replication=streaming")
	req := httptest.NewRequest(http.MethodPost, "/dashboard/replication", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("post %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) || !strings.Contains(rec.Body.String(), "Follower started") {
		t.Fatalf("json: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "<html") {
		t.Fatal("json POST must not return HTML")
	}
}

func TestDashReplicationAddFollower(t *testing.T) {
	t.Setenv("DASHBOARD_SSO_OPTIONAL", "1")
	store := postgres.NewStore()
	inst, _, err := store.Provision(postgres.ProvisionRequest{App: "shop-a", Tenant: "shop-a"})
	if err != nil {
		t.Fatal(err)
	}
	h := newHandler(store)
	form := strings.NewReader("action=follow&instance=" + inst.App + "&replication=streaming")
	req := httptest.NewRequest(http.MethodPost, "/dashboard/replication", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("post %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Follower started") {
		t.Fatalf("post body: %s", body)
	}
	if !strings.Contains(body, "Promote") || !strings.Contains(body, "Unfollow") {
		t.Fatalf("missing follower actions: %s", body)
	}
	if strings.Contains(body, ">Wait<") {
		t.Fatalf("wait belongs on the CLI, not the followers table: %s", body)
	}
}

func TestDashReplicationPromote(t *testing.T) {
	t.Setenv("DASHBOARD_SSO_OPTIONAL", "1")
	store := postgres.NewStore()
	leader, _, err := store.Provision(postgres.ProvisionRequest{App: "shop-a", Tenant: "shop-a"})
	if err != nil {
		t.Fatal(err)
	}
	fol, _, err := store.Provision(postgres.ProvisionRequest{App: "shop-a", Follow: leader.App})
	if err != nil {
		t.Fatal(err)
	}
	h := newHandler(store)
	form := strings.NewReader("action=promote&instance=" + fol.App)
	req := httptest.NewRequest(http.MethodPost, "/dashboard/replication", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("post %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Follower promoted") {
		t.Fatalf("post body: %s", rec.Body.String())
	}
	got, err := store.Get(fol.ID)
	if err != nil || got.Role != postgres.RolePrimary {
		t.Fatalf("role %+v %v", got, err)
	}
}

func TestDashReplicationUnfollow(t *testing.T) {
	t.Setenv("DASHBOARD_SSO_OPTIONAL", "1")
	store := postgres.NewStore()
	leader, _, err := store.Provision(postgres.ProvisionRequest{App: "shop-a", Tenant: "shop-a"})
	if err != nil {
		t.Fatal(err)
	}
	fol, _, err := store.Provision(postgres.ProvisionRequest{App: "shop-a", Follow: leader.App})
	if err != nil {
		t.Fatal(err)
	}
	h := newHandler(store)
	form := strings.NewReader("action=unfollow&instance=" + fol.App)
	req := httptest.NewRequest(http.MethodPost, "/dashboard/replication", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("post %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Replication stopped") {
		t.Fatalf("post body: %s", rec.Body.String())
	}
	got, err := store.Get(fol.ID)
	if err != nil || got.Role != postgres.RoleStandalone {
		t.Fatalf("role %+v %v", got, err)
	}
}

func TestDashCreateLogicalDatabase(t *testing.T) {
	t.Setenv("DASHBOARD_SSO_OPTIONAL", "1")
	store := postgres.NewStore()
	inst, _, err := store.Provision(postgres.ProvisionRequest{App: "shop-a", Tenant: "shop-a"})
	if err != nil {
		t.Fatal(err)
	}
	h := newHandler(store)
	form := strings.NewReader("name=shop_analytics&instance=" + inst.App)
	req := httptest.NewRequest(http.MethodPost, "/dashboard/databases", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("post %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "shop_analytics") || !strings.Contains(rec.Body.String(), "Created database") {
		t.Fatalf("post body: %s", rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/dashboard/api/databases", nil)
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"name":"shop_analytics"`) {
		t.Fatalf("list %d %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "/dashboard/api/databases", strings.NewReader(`{"name":"shop_json"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("json post without Accept %d %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/dashboard/api/databases", strings.NewReader(`{"name":"bad-name"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == 200 {
		t.Fatalf("accepted invalid name: %s", rec.Body.String())
	}
}

func TestDashDatabasesCreateButtonLivesInToolbar(t *testing.T) {
	t.Setenv("DASHBOARD_SSO_OPTIONAL", "1")
	store := postgres.NewStore()
	if _, _, err := store.Provision(postgres.ProvisionRequest{App: "shop-a", Tenant: "shop-a"}); err != nil {
		t.Fatal(err)
	}
	h := newHandler(store)
	req := httptest.NewRequest(http.MethodGet, "/dashboard/databases", nil)
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `class="tab-toolbar is-spread"`) || !strings.Contains(body, `href="databases?new=1"`) {
		t.Fatalf("create database should live in the tab toolbar: %s", body)
	}
	if !strings.Contains(body, `class="card table-card"`) || !strings.Contains(body, "<th>Database</th>") {
		t.Fatalf("databases should list in a table card: %s", body)
	}
	if strings.Contains(body, `id="postgres-db-panel"`) {
		t.Fatalf("create panel should stay closed until ?new=1: %s", body)
	}
	idxToolbar := strings.Index(body, `class="tab-toolbar is-spread"`)
	idxTable := strings.Index(body, `class="card table-card"`)
	idxCreate := strings.Index(body, "Create database")
	if idxToolbar < 0 || idxTable < 0 || idxCreate < 0 || idxCreate > idxTable {
		t.Fatalf("Create database must appear in the toolbar, not the table card: %s", body)
	}

	req = httptest.NewRequest(http.MethodGet, "/dashboard/databases?new=1", nil)
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("panel status %d %s", rec.Code, rec.Body.String())
	}
	panel := rec.Body.String()
	if !strings.Contains(panel, `id="postgres-db-panel"`) || !strings.Contains(panel, `role="dialog"`) || !strings.Contains(panel, `id="logical-db-name"`) {
		t.Fatalf("create panel: %s", panel)
	}
}

func TestDashFollowersListIsTable(t *testing.T) {
	t.Setenv("DASHBOARD_SSO_OPTIONAL", "1")
	store := postgres.NewStore()
	if _, _, err := store.Provision(postgres.ProvisionRequest{App: "shop-a", Tenant: "shop-a"}); err != nil {
		t.Fatal(err)
	}
	h := newHandler(store)
	req := httptest.NewRequest(http.MethodGet, "/dashboard/replication", nil)
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `class="tab-toolbar is-spread"`) || !strings.Contains(body, "Add follower") {
		t.Fatalf("add follower should live in the tab toolbar: %s", body)
	}
	if !strings.Contains(body, `class="card table-card"`) {
		t.Fatalf("followers should list in a table card: %s", body)
	}
	for _, col := range []string{"<th>Instance</th>", "<th>Database</th>", "<th>Role</th>", "<th>Host</th>"} {
		if !strings.Contains(body, col) {
			t.Fatalf("missing %s in %s", col, body)
		}
	}
	if !strings.Contains(body, "This database has no followers yet.") {
		t.Fatalf("empty followers row: %s", body)
	}
	idxToolbar := strings.Index(body, "Add follower")
	idxTable := strings.Index(body, `class="card table-card"`)
	if idxToolbar < 0 || idxTable < 0 || idxToolbar > idxTable {
		t.Fatalf("Add follower must appear in the toolbar, not the table card: %s", body)
	}
}

func TestDashBackupDumpEndpoint(t *testing.T) {
	t.Setenv("DASHBOARD_SSO_OPTIONAL", "1")
	store := postgres.NewStore()
	if _, _, err := store.Provision(postgres.ProvisionRequest{App: "shop-a", Tenant: "shop-a"}); err != nil {
		t.Fatal(err)
	}
	h := newHandler(store)
	req := httptest.NewRequest(http.MethodGet, "/dashboard/backup", nil)
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("backup %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Download dump") || !strings.Contains(body, "Restore dump") || strings.Contains(body, "<textarea") {
		t.Fatalf("backup page: %s", body)
	}
	req = httptest.NewRequest(http.MethodGet, "/dashboard/api/dump", nil)
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "flynn postgres dump") {
		t.Fatalf("dump %d %s", rec.Code, rec.Body.String())
	}
}

func TestDashCreateUserAPI(t *testing.T) {
	t.Setenv("DASHBOARD_SSO_OPTIONAL", "1")
	store := postgres.NewStore()
	inst, _, err := store.Provision(postgres.ProvisionRequest{App: "shop-a", Tenant: "shop-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddDatabase(inst.ID, "appdb"); err != nil {
		t.Fatal(err)
	}
	h := newHandler(store)
	req := httptest.NewRequest(http.MethodPost, "/dashboard/api/users", strings.NewReader(`{"name":"alice","password":"secret","database":"appdb","instance":"`+inst.App+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("create %d %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/dashboard/api/users", nil)
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"name":"alice"`) {
		t.Fatalf("list %d %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/dashboard/users", nil)
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Create user") || !strings.Contains(rec.Body.String(), "alice") {
		t.Fatalf("users page %d %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "/dashboard/api/users/drop", strings.NewReader(`{"name":"alice","instance":"`+inst.App+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("drop %d %s", rec.Code, rec.Body.String())
	}
	users, err := store.Users(inst.ID)
	if err != nil || len(users) != 0 {
		t.Fatalf("store users %+v %v", users, err)
	}
}

func TestDashListUsersIncludesStoreUserWhenSessionIsAppUUID(t *testing.T) {
	t.Setenv("DASHBOARD_SSO_OPTIONAL", "1")
	store := postgres.NewStore()
	inst, _, err := store.Provision(postgres.ProvisionRequest{App: "shop-a", Tenant: "shop-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddUser(inst.ID, "alice", "secret", inst.Databases[0].Name); err != nil {
		t.Fatal(err)
	}
	h := newHandler(store)
	h.listResources = func(app string) ([]*ct.Resource, error) {
		if app != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
			t.Fatalf("app %q", app)
		}
		return []*ct.Resource{{
			ID:         "res-1",
			ProviderID: "postgres",
			Env: map[string]string{
				"FLYNN_POSTGRES": inst.App,
				"PGDATABASE":     inst.Databases[0].Name,
				"PGUSER":         inst.AppUser,
				"POSTGRES_URL":   inst.ConnectionURL(),
			},
		}}, nil
	}
	req := httptest.NewRequest(http.MethodGet, "/dashboard/api/users", nil)
	req.Header.Set("X-Flynn-Dashboard-App", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"name":"alice"`) {
		t.Fatalf("list %d %s", rec.Code, rec.Body.String())
	}
}

func TestDashListUsersOnceAcrossFollowers(t *testing.T) {
	t.Setenv("DASHBOARD_SSO_OPTIONAL", "1")
	store := postgres.NewStore()
	leader, _, err := store.Provision(postgres.ProvisionRequest{App: "shop-a", Tenant: "shop-a"})
	if err != nil {
		t.Fatal(err)
	}
	db := leader.Databases[0].Name
	if err := store.AddUser(leader.ID, "alice", "secret", db); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Provision(postgres.ProvisionRequest{App: "shop-a", Follow: leader.App}); err != nil {
		t.Fatal(err)
	}
	h := newHandler(store)
	req := httptest.NewRequest(http.MethodGet, "/dashboard/api/users", nil)
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("list %d %s", rec.Code, rec.Body.String())
	}
	var users []userView
	if err := json.Unmarshal(rec.Body.Bytes(), &users); err != nil {
		t.Fatalf("json %v %s", err, rec.Body.String())
	}
	counts := map[string]int{}
	for _, u := range users {
		counts[u.Name]++
	}
	if counts["alice"] != 1 {
		t.Fatalf("alice should appear once after follow, got %#v", users)
	}
	if counts[leader.AppUser] != 1 {
		t.Fatalf("primary PGUSER should appear once, got %#v", users)
	}

	h.listResources = func(app string) ([]*ct.Resource, error) {
		if app != "shop-b" {
			t.Fatalf("app %q", app)
		}
		return []*ct.Resource{
			{
				ID:         "res-leader",
				ProviderID: "postgres",
				Env: map[string]string{
					"FLYNN_POSTGRES": "pg-orchid-xkhthp",
					"PGDATABASE":     "appdb",
					"PGUSER":         "app_leader",
					"POSTGRES_ROLE":  "primary",
				},
			},
			{
				ID:         "res-fol",
				ProviderID: "postgres",
				Env: map[string]string{
					"FLYNN_POSTGRES":  "pg-willow-abcdef",
					"PGDATABASE":      "appdb",
					"PGUSER":          "app_follower_login",
					"POSTGRES_ROLE":   "follower",
					"POSTGRES_LEADER": "pg-orchid-xkhthp",
				},
			},
		}, nil
	}
	req = httptest.NewRequest(http.MethodGet, "/dashboard/api/users", nil)
	req.Header.Set("X-Flynn-Dashboard-App", "shop-b")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("controller list %d %s", rec.Code, rec.Body.String())
	}
	users = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &users); err != nil {
		t.Fatalf("json %v %s", err, rec.Body.String())
	}
	counts = map[string]int{}
	for _, u := range users {
		counts[u.Name]++
	}
	if counts["app_leader"] != 1 || counts["app_follower_login"] != 0 {
		t.Fatalf("follower connection user should not be listed, got %#v", users)
	}
}

func TestCollectUsersListsCreatedUserOnceWithTwoDatabases(t *testing.T) {
	t.Setenv("DASHBOARD_SSO_OPTIONAL", "1")
	store := postgres.NewStore()
	leader, _, err := store.Provision(postgres.ProvisionRequest{App: "shop-a", Tenant: "shop-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddDatabase(leader.ID, "shop_analytics"); err != nil {
		t.Fatal(err)
	}
	if err := store.AddUser(leader.ID, "alice", "secret", "shop_analytics"); err != nil {
		t.Fatal(err)
	}
	h := newHandler(store)
	req := httptest.NewRequest(http.MethodGet, "/dashboard/api/users", nil)
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("list %d %s", rec.Code, rec.Body.String())
	}
	var users []userView
	if err := json.Unmarshal(rec.Body.Bytes(), &users); err != nil {
		t.Fatalf("json %v %s", err, rec.Body.String())
	}
	var alice []userView
	for _, u := range users {
		if u.Name == "alice" {
			alice = append(alice, u)
		}
	}
	if len(alice) != 1 || alice[0].Database != "shop_analytics" {
		t.Fatalf("alice should appear once on the granted database, got %#v", users)
	}
}

func TestDashSettingsDeletesFollowerNotLeaderWithFollowers(t *testing.T) {
	t.Setenv("DASHBOARD_SSO_OPTIONAL", "1")
	store := postgres.NewStore()
	leader, _, err := store.Provision(postgres.ProvisionRequest{App: "shop-a", Tenant: "shop-a"})
	if err != nil {
		t.Fatal(err)
	}
	fol, _, err := store.Provision(postgres.ProvisionRequest{App: "shop-a", Follow: leader.App})
	if err != nil {
		t.Fatal(err)
	}
	h := newHandler(store)
	req := httptest.NewRequest(http.MethodGet, "/dashboard/settings", nil)
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("get %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Delete resource") || !strings.Contains(body, leader.App) || !strings.Contains(body, fol.App) {
		t.Fatalf("settings page: %s", body)
	}
	if !strings.Contains(body, "disabled") {
		t.Fatal("primary with followers must disable delete")
	}
	form := strings.NewReader("action=delete&instance=" + leader.App)
	req = httptest.NewRequest(http.MethodPost, "/dashboard/settings", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == 200 && strings.Contains(rec.Body.String(), "Deleted resource "+leader.App) {
		t.Fatalf("deleted primary while follower exists: %s", rec.Body.String())
	}
	if rec.Code == 200 && !strings.Contains(rec.Body.String(), "followers") {
		t.Fatalf("expected followers error, got %d %s", rec.Code, rec.Body.String())
	}
	form = strings.NewReader("action=delete&instance=" + fol.App)
	req = httptest.NewRequest(http.MethodPost, "/dashboard/settings", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Deleted resource "+fol.App) {
		t.Fatalf("follower delete %d %s", rec.Code, rec.Body.String())
	}
	if _, err := store.Get(fol.ID); err == nil {
		t.Fatal("follower still in store")
	}
	form = strings.NewReader("action=delete&instance=" + leader.App)
	req = httptest.NewRequest(http.MethodPost, "/dashboard/settings", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Deleted resource "+leader.App) {
		t.Fatalf("leader delete %d %s", rec.Code, rec.Body.String())
	}
}
