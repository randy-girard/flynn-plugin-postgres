package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/randy-girard/flynn-plugin-postgres"
	controller "github.com/randy-girard/flynn/controller/client"
	ct "github.com/randy-girard/flynn/controller/types"
)

// The Settings Destroy and Detach act as the dashboard user
// (dashUserController with the session's controller token), never with the
// plugin's CONTROLLER_KEY client, and unset the attachment env on the app.
// The handler's own client (pluginCtl) is only for lookups; every write on it
// is recorded and any method it does not define panics.

// fakeUserCtl is the controller as the dashboard user.
type fakeUserCtl struct {
	mu          sync.Mutex
	releases    map[string]*ct.Release
	provisioned []*ct.ResourceReq
	deleted     []string
	unbound     []string
	created     []*ct.Release
	deployed    []string
}

func (f *fakeUserCtl) ProvisionResource(req *ct.ResourceReq) (*ct.Resource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.provisioned = append(f.provisioned, req)
	return &ct.Resource{ID: "res-new"}, nil
}

func (f *fakeUserCtl) DeleteResource(provider, id string) (*ct.Resource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, provider+"|"+id)
	return &ct.Resource{ID: id}, nil
}

func (f *fakeUserCtl) DeleteResourceApp(provider, id, app string) (*ct.Resource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unbound = append(f.unbound, provider+"|"+id+"@"+app)
	return &ct.Resource{ID: id}, nil
}

func (f *fakeUserCtl) GetAppRelease(app string) (*ct.Release, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if rel := f.releases[app]; rel != nil {
		return rel, nil
	}
	return nil, controller.ErrNotFound
}

func (f *fakeUserCtl) CreateRelease(app string, r *ct.Release) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r.ID = "rel-next-" + app
	f.created = append(f.created, r)
	return nil
}

func (f *fakeUserCtl) SetAppRelease(app, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCurrent(app, id)
	return nil
}

func (f *fakeUserCtl) CreateDeployment(app, id string) (*ct.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deployed = append(f.deployed, app)
	f.setCurrent(app, id)
	return &ct.Deployment{}, nil
}

func (f *fakeUserCtl) setCurrent(app, id string) {
	for i := len(f.created) - 1; i >= 0; i-- {
		if f.created[i].ID == id {
			if f.releases == nil {
				f.releases = map[string]*ct.Release{}
			}
			f.releases[app] = f.created[i]
			return
		}
	}
}

func (f *fakeUserCtl) writes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.provisioned) + len(f.deleted) + len(f.unbound) + len(f.created) + len(f.deployed)
}

func (f *fakeUserCtl) env(app string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if rel := f.releases[app]; rel != nil {
		return rel.Env
	}
	return nil
}

// useUserClient makes user the controller client for the token "user-token".
func useUserClient(t *testing.T, user *fakeUserCtl) {
	t.Helper()
	prev := dashUserController
	dashUserController = func(token string) (dashResourceClient, error) {
		if token != "user-token" {
			return nil, errDashNoToken
		}
		return user, nil
	}
	t.Cleanup(func() { dashUserController = prev })
}

// pluginCtl is the plugin's own (CONTROLLER_KEY) client: lookups answer,
// writes are recorded.
type pluginCtl struct {
	controller.Client
	mu        sync.Mutex
	resources map[string][]*ct.Resource
	apps      []*ct.App
	releases  map[string]*ct.Release
	writes    []string
}

func (p *pluginCtl) AppResourceList(app string) ([]*ct.Resource, error) {
	return p.resources[app], nil
}
func (p *pluginCtl) ResourceListAll() ([]*ct.Resource, error) {
	seen := map[string]bool{}
	var out []*ct.Resource
	for _, list := range p.resources {
		for _, r := range list {
			if !seen[r.ID] {
				seen[r.ID] = true
				out = append(out, r)
			}
		}
	}
	return out, nil
}
func (p *pluginCtl) GetApp(name string) (*ct.App, error) { return &ct.App{ID: name, Name: name}, nil }
func (p *pluginCtl) AppList() ([]*ct.App, error)         { return p.apps, nil }
func (p *pluginCtl) GetAppRelease(app string) (*ct.Release, error) {
	if rel := p.releases[app]; rel != nil {
		return rel, nil
	}
	return nil, controller.ErrNotFound
}
func (p *pluginCtl) record(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.writes = append(p.writes, s)
}
func (p *pluginCtl) GetProvider(string) (*ct.Provider, error) {
	return &ct.Provider{ID: "postgres", Name: "postgres"}, nil
}
func (p *pluginCtl) DeleteResource(_, id string) (*ct.Resource, error) {
	p.record("DeleteResource " + id)
	return &ct.Resource{ID: id}, nil
}
func (p *pluginCtl) DeleteResourceApp(_, id, app string) (*ct.Resource, error) {
	p.record("DeleteResourceApp " + id + "@" + app)
	return &ct.Resource{ID: id}, nil
}
func (p *pluginCtl) DeleteApp(app string) (*ct.AppDeletion, error) {
	p.record("DeleteApp " + app)
	return &ct.AppDeletion{AppID: app}, nil
}
func (p *pluginCtl) CreateRelease(app string, _ *ct.Release) error {
	p.record("CreateRelease " + app)
	return nil
}
func (p *pluginCtl) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.writes)
}

const amberURL = "postgres://u:pw@leader.pg-amber.discoverd:5432/db"

// lifecycleFixture: shop-a's instance a backs resource res-a, owned by owner
// and attached to shop-a and shop-b; both releases carry the attachment URL.
func lifecycleFixture(t *testing.T, owner string) (*handler, *fakeUserCtl, *pluginCtl, *postgres.Instance) {
	t.Helper()
	t.Setenv("DASHBOARD_SSO_OPTIONAL", "1")
	store := postgres.NewStore()
	a, _, err := store.Provision(postgres.ProvisionRequest{App: "shop-a", Tenant: "shop-a"})
	if err != nil {
		t.Fatal(err)
	}
	res := &ct.Resource{ID: "res-a", ProviderID: "postgres", OwnerApp: owner, Apps: []string{"shop-b", "shop-a"}, Env: map[string]string{
		"FLYNN_POSTGRES": a.App, "FLYNN_POSTGRES_ROLE": "primary", "DATABASE_URL": amberURL, "FLYNN_POSTGRES_AMBER_URL": amberURL,
	}}
	plugin := &pluginCtl{resources: map[string][]*ct.Resource{"shop-a": {res}, "shop-b": {res}}}
	h := newHandler(store)
	h.client = plugin
	user := &fakeUserCtl{releases: map[string]*ct.Release{
		"shop-a": {ID: "rel-a", Env: map[string]string{"KEEP": "1", "DATABASE_URL": amberURL, "FLYNN_POSTGRES_AMBER_URL": amberURL}},
		"shop-b": {ID: "rel-b", ArtifactIDs: []string{"art"}, Env: map[string]string{"KEEP": "1", "FLYNN_POSTGRES_AMBER_URL": amberURL}},
	}}
	useUserClient(t, user)
	return h, user, plugin, a
}

func settingsReq(app, perms string, form url.Values, token bool) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/dashboard/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Flynn-Dashboard-App", app)
	req.Header.Set("X-Flynn-Dashboard-Permissions", perms)
	req.Header.Set("X-Flynn-Dashboard-User", "u1")
	if token {
		req.Header.Set("X-Flynn-Access-Token", "user-token")
	}
	return req
}

func serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func destroyForm(inst, confirm string) url.Values {
	return url.Values{"action": {"delete"}, "instance": {inst}, "confirm": {confirm}}
}

func detachForm(inst string) url.Values {
	return url.Values{"action": {"detach"}, "instance": {inst}}
}

// shop-b is attached to shop-a's instance. Settings Detach unbinds shop-b and
// removes the env on shop-b; it never deletes the resource. Destroy from
// shop-b is refused.
func TestDashSettingsDetachFromAttachedAppOnlyDetaches(t *testing.T) {
	h, user, plugin, a := lifecycleFixture(t, "shop-a")
	if rec := serve(h, settingsReq("shop-b", "*", destroyForm(a.App, a.App), true)); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner destroy: want 403, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(h, settingsReq("shop-b", "app:resources:write", detachForm(a.App), true)); rec.Code != http.StatusForbidden {
		t.Fatalf("detach without Manage: want 403, got %d %s", rec.Code, rec.Body.String())
	}
	if user.writes()+plugin.count() != 0 {
		t.Fatalf("refused requests changed state: user deleted %v unbound %v, plugin %v", user.deleted, user.unbound, plugin.writes)
	}
	rec := serve(h, settingsReq("shop-b", "app:write", detachForm(a.App), true))
	if rec.Code != http.StatusOK {
		t.Fatalf("detach %d %s", rec.Code, rec.Body.String())
	}
	if len(user.deleted) != 0 || strings.Join(user.unbound, ",") != "postgres|res-a@shop-b" {
		t.Fatalf("deleted %v unbound %v", user.deleted, user.unbound)
	}
	env := user.env("shop-b")
	if _, ok := env["FLYNN_POSTGRES_AMBER_URL"]; ok || env["KEEP"] != "1" || len(user.deployed) != 1 {
		t.Fatalf("shop-b env after detach: %v deployed %v", env, user.deployed)
	}
	if plugin.count() != 0 {
		t.Fatalf("detach wrote with the plugin key: %v", plugin.writes)
	}
	if _, err := h.store.Get(a.ID); err != nil {
		t.Fatalf("detach forgot the owner's instance: %v", err)
	}
}

// The owning app cannot Detach; Destroy is its action.
func TestDashSettingsDetachByOwnerRefused(t *testing.T) {
	h, user, plugin, a := lifecycleFixture(t, "shop-a")
	rec := serve(h, settingsReq("shop-a", "app:admin", detachForm(a.App), true))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Destroy") {
		t.Fatalf("owner detach: want 409 naming Destroy, got %d %s", rec.Code, rec.Body.String())
	}
	if user.writes()+plugin.count() != 0 {
		t.Fatalf("owner detach changed state: user %v plugin %v", user.unbound, plugin.writes)
	}
}

// The owner's Destroy needs Admin and the typed instance name, then deletes
// the resource through the controller as the user and drops the env.
func TestDashSettingsDestroyByOwnerNeedsAdminAndConfirm(t *testing.T) {
	h, user, plugin, a := lifecycleFixture(t, "shop-a")
	for _, tc := range []struct {
		perms, confirm string
		code           int
	}{
		{"app:resources:write", a.App, http.StatusForbidden},
		{"app:write", a.App, http.StatusForbidden},
		{"app:admin", "", http.StatusBadRequest},
		{"app:admin", "pg-other", http.StatusBadRequest},
	} {
		if rec := serve(h, settingsReq("shop-a", tc.perms, destroyForm(a.App, tc.confirm), true)); rec.Code != tc.code {
			t.Errorf("%s confirm %q: want %d, got %d %s", tc.perms, tc.confirm, tc.code, rec.Code, rec.Body.String())
		}
	}
	if user.writes()+plugin.count() != 0 {
		t.Fatalf("refused destroy changed state: user %v plugin %v", user.deleted, plugin.writes)
	}
	rec := serve(h, settingsReq("shop-a", "app:admin", destroyForm(a.App, a.App), true))
	if rec.Code != http.StatusOK {
		t.Fatalf("destroy %d %s", rec.Code, rec.Body.String())
	}
	if strings.Join(user.deleted, ",") != "postgres|res-a" || len(user.unbound) != 0 {
		t.Fatalf("deleted %v unbound %v", user.deleted, user.unbound)
	}
	env := user.env("shop-a")
	if _, ok := env["FLYNN_POSTGRES_AMBER_URL"]; ok || env["DATABASE_URL"] != "" || env["KEEP"] != "1" {
		t.Fatalf("shop-a env after destroy: %v", env)
	}
	if plugin.count() != 0 {
		t.Fatalf("destroy wrote with the plugin key: %v", plugin.writes)
	}
}

// A primary with followers cannot be destroyed from the dashboard.
func TestDashSettingsDestroyRefusedWhilePrimaryHasFollowers(t *testing.T) {
	h, user, plugin, a := lifecycleFixture(t, "shop-a")
	fol, _, err := h.store.Provision(postgres.ProvisionRequest{App: "shop-a", Follow: a.App})
	if err != nil {
		t.Fatal(err)
	}
	plugin.apps = []*ct.App{{ID: fol.App, Name: fol.App}}
	plugin.releases = map[string]*ct.Release{fol.App: {Env: map[string]string{"FLYNN_POSTGRES": fol.App, "FLYNN_POSTGRES_LEADER": a.App, "FLYNN_POSTGRES_ROLE": "follower"}}}
	rec := serve(h, settingsReq("shop-a", "app:admin", destroyForm(a.App, a.App), true))
	if rec.Code != http.StatusConflict || !strings.Contains(strings.ToLower(rec.Body.String()), "follower") {
		t.Fatalf("destroy with followers: want 409 naming followers, got %d %s", rec.Code, rec.Body.String())
	}
	if user.writes()+plugin.count() != 0 {
		t.Fatalf("refused destroy changed state: user %v plugin %v", user.deleted, plugin.writes)
	}
}

// Settings offers the owner Destroy with a typed confirmation and an attached
// non-owner only Detach.
func TestDashSettingsOwnerDestroyNonOwnerDetach(t *testing.T) {
	h, _, _, a := lifecycleFixture(t, "shop-a")
	get := func(app string) string {
		req := httptest.NewRequest(http.MethodGet, "/dashboard/settings", nil)
		req.Header.Set("X-Flynn-Dashboard-App", app)
		req.Header.Set("X-Flynn-Dashboard-Permissions", "app:admin")
		return serve(h, req).Body.String()
	}
	body := get("shop-a")
	if !strings.Contains(body, `name="action" value="delete"`) || !strings.Contains(body, `name="confirm"`) || !strings.Contains(body, ">Destroy resource</button>") ||
		strings.Contains(body, `name="action" value="detach"`) || !strings.Contains(body, a.App) {
		t.Fatalf("owner settings:\n%s", body)
	}
	body = get("shop-b")
	if !strings.Contains(body, `name="action" value="detach"`) || strings.Contains(body, `name="action" value="delete"`) || !strings.Contains(body, "keeps running") {
		t.Fatalf("non-owner settings:\n%s", body)
	}
}

// Without the user's controller token nothing changes and the plugin's key
// is never used instead.
func TestDashSettingsLifecycleNeedsUserToken(t *testing.T) {
	h, user, plugin, a := lifecycleFixture(t, "shop-a")
	for _, c := range []struct {
		app  string
		form url.Values
	}{
		{"shop-a", destroyForm(a.App, a.App)},
		{"shop-b", detachForm(a.App)},
	} {
		if rec := serve(h, settingsReq(c.app, "*", c.form, false)); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %v without a user token: want 401, got %d %s", c.app, c.form, rec.Code, rec.Body.String())
		}
	}
	if user.writes()+plugin.count() != 0 {
		t.Fatalf("tokenless requests changed state: user %d plugin %v", user.writes(), plugin.writes)
	}
}
