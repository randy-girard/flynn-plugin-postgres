package main

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
)

type fakeInstanceControl struct {
	apps     []*ct.App
	releases []*ct.Release
	scaled   []ct.ScaleOptions
	current  map[string]string
	deleted  []string
}

func (f *fakeInstanceControl) CreateApp(app *ct.App) error {
	if app.ID == "" {
		app.ID = app.Name
	}
	f.apps = append(f.apps, app)
	return nil
}

func (f *fakeInstanceControl) CreateRelease(appID string, release *ct.Release) error {
	if release.ID == "" {
		release.ID = "rel-" + appID
		if n := len(f.releases); n > 0 {
			release.ID += "-" + strconv.Itoa(n)
		}
	}
	f.releases = append(f.releases, release)
	return nil
}

func (f *fakeInstanceControl) ScaleAppRelease(_, _ string, opts ct.ScaleOptions) error {
	f.scaled = append(f.scaled, opts)
	return nil
}

func (f *fakeInstanceControl) GetApp(id string) (*ct.App, error) {
	for _, a := range f.apps {
		if a != nil && (a.ID == id || a.Name == id) {
			return a, nil
		}
	}
	return nil, errors.New("not found")
}

func (f *fakeInstanceControl) GetAppRelease(id string) (*ct.Release, error) {
	if f.current != nil {
		if rid := f.current[id]; rid != "" {
			for _, r := range f.releases {
				if r != nil && r.ID == rid {
					return r, nil
				}
			}
		}
	}
	if len(f.releases) > 0 {
		return f.releases[len(f.releases)-1], nil
	}
	return nil, errors.New("not found")
}

func (f *fakeInstanceControl) SetAppRelease(appID, releaseID string) error {
	if f.current == nil {
		f.current = map[string]string{}
	}
	f.current[appID] = releaseID
	return nil
}

func (f *fakeInstanceControl) DeleteApp(appID string) (*ct.AppDeletion, error) {
	f.deleted = append(f.deleted, appID)
	return &ct.AppDeletion{AppID: appID}, nil
}

func (f *fakeInstanceControl) JobList(string) ([]*ct.Job, error) {
	return nil, nil
}

func (f *fakeInstanceControl) JobListActive() ([]*ct.Job, error) {
	return nil, nil
}

func TestRefreshIsolatedPluginImageUpdatesArtifact(t *testing.T) {
	ctrl := &fakeInstanceControl{
		apps: []*ct.App{{ID: "a1", Name: "postgresql-basin-73690"}},
		releases: []*ct.Release{{
			ID:          "old",
			ArtifactIDs: []string{"img-old"},
			Env:         map[string]string{"ENGINE_VERSION": postgres.EngineVersion(), "FLYNN_POSTGRES": "postgresql-basin-73690"},
			Processes:   map[string]ct.ProcessType{postgres.ProcessName: {}},
		}},
		current: map[string]string{"a1": "old"},
	}
	inst := &postgres.Instance{App: "postgresql-basin-73690", EngineVersion: postgres.EngineVersion()}
	if err := refreshIsolatedPluginImage(ctrl, "img-new", inst); err != nil {
		t.Fatal(err)
	}
	if ctrl.current["a1"] == "old" {
		t.Fatal("must set the new release")
	}
	found := false
	for _, r := range ctrl.releases {
		if len(r.ArtifactIDs) > 0 && r.ArtifactIDs[0] == "img-new" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing new artifact")
	}
	if len(ctrl.scaled) < 2 {
		t.Fatalf("scale calls %d", len(ctrl.scaled))
	}
	if ctrl.scaled[0].Processes[postgres.ProcessName] != 0 {
		t.Fatalf("first scale must stop old job: %#v", ctrl.scaled[0])
	}
	if ctrl.scaled[1].Processes[postgres.ProcessName] != postgres.DefaultNodes {
		t.Fatalf("second scale must start new job: %#v", ctrl.scaled[1])
	}
}

func TestRefreshIsolatedPluginImageSkipsMatchingImage(t *testing.T) {
	ctrl := &fakeInstanceControl{
		apps: []*ct.App{{ID: "a1", Name: "postgresql-basin-73690"}},
		releases: []*ct.Release{{
			ID:          "cur",
			ArtifactIDs: []string{"img-new"},
			Env:         map[string]string{"ENGINE_VERSION": postgres.EngineVersion()},
		}},
		current: map[string]string{"a1": "cur"},
	}
	err := refreshIsolatedPluginImage(ctrl, "img-new", &postgres.Instance{App: "postgresql-basin-73690", EngineVersion: postgres.EngineVersion()})
	if err != nil {
		t.Fatal(err)
	}
	if len(ctrl.scaled) != 0 {
		t.Fatalf("matching image must not restart: %#v", ctrl.scaled)
	}
}

func TestRefreshIsolatedPluginImageSkipsTenantApp(t *testing.T) {
	err := refreshIsolatedPluginImage(&fakeInstanceControl{}, "img-new", &postgres.Instance{App: "app-one"})
	if err == nil || !strings.Contains(err.Error(), "tenant") {
		t.Fatalf("got %v", err)
	}
}

func TestRefreshIsolatedPluginImageSkipsEngineBehind(t *testing.T) {
	ctrl := &fakeInstanceControl{
		apps: []*ct.App{{ID: "a1", Name: "postgresql-basin-73690"}},
		releases: []*ct.Release{{
			ID:          "old",
			ArtifactIDs: []string{"img-old"},
			Env:         map[string]string{"ENGINE_VERSION": "15"},
		}},
		current: map[string]string{"a1": "old"},
	}
	err := refreshIsolatedPluginImage(ctrl, "img-new", &postgres.Instance{App: "postgresql-basin-73690", EngineVersion: "15"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ctrl.scaled) != 0 {
		t.Fatalf("engine upgrade must own the restart: %#v", ctrl.scaled)
	}
}

func TestImageRefreshActionSwapsStalePrimary(t *testing.T) {
	engine := postgres.EngineVersion()
	stale := &ct.Release{ArtifactIDs: []string{"old"}, Env: map[string]string{"ENGINE_VERSION": engine}}
	primary := &postgres.Instance{App: "postgresql-basin-73690", Role: postgres.RolePrimary, EngineVersion: engine}
	if got := imageRefreshAction(primary, stale, "new"); got != "swap" {
		t.Fatalf("primary stale=%s", got)
	}
	fol := &postgres.Instance{App: "postgresql-orchid-80127", Role: postgres.RoleFollower, EngineVersion: engine}
	if got := imageRefreshAction(fol, stale, "new"); got != "inplace" {
		t.Fatalf("follower stale=%s", got)
	}
	current := &ct.Release{ArtifactIDs: []string{"new"}, Env: map[string]string{"ENGINE_VERSION": engine}}
	if got := imageRefreshAction(primary, current, "new"); got != "skip" {
		t.Fatalf("matching image=%s", got)
	}
}

func TestPromoteIsolatedJobStampsPrimaryAndBounces(t *testing.T) {
	ctrl := &fakeInstanceControl{
		apps: []*ct.App{{ID: "a1", Name: "postgresql-orchid-80127"}},
		releases: []*ct.Release{{
			ID:          "old",
			ArtifactIDs: []string{"img"},
			Env:         map[string]string{"FLYNN_POSTGRES": "postgresql-orchid-80127", "POSTGRES_ROLE": "follower", "ENGINE_VERSION": postgres.EngineVersion()},
			Processes:   map[string]ct.ProcessType{postgres.ProcessName: {}},
		}},
		current: map[string]string{"a1": "old"},
	}
	inst := &postgres.Instance{App: "postgresql-orchid-80127", Role: postgres.RolePrimary}
	if err := promoteIsolatedJob(ctrl, inst, func(string, time.Duration) error { return nil }); err != nil {
		t.Fatal(err)
	}
	rel, err := ctrl.GetAppRelease("a1")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Env["POSTGRES_ROLE"] != "primary" {
		t.Fatalf("role=%s", rel.Env["POSTGRES_ROLE"])
	}
	if rel.Env["POSTGRES_SERVICE_ALIAS"] != "" {
		t.Fatalf("must not alias the old service: %s", rel.Env["POSTGRES_SERVICE_ALIAS"])
	}
	if len(ctrl.scaled) < 2 {
		t.Fatalf("bounce scales=%d", len(ctrl.scaled))
	}
	if ctrl.scaled[0].Processes[postgres.ProcessName] != 0 {
		t.Fatalf("first bounce scale must stop old job: %#v", ctrl.scaled[0])
	}
}

func TestImageRefreshOptionsStreamingDropsPrevious(t *testing.T) {
	h := newHandler(postgres.NewStore())
	opts := h.imageRefreshOptions(&postgres.Instance{Runtime: "perf-l"})
	if opts.Mode != postgres.ModeStreaming {
		t.Fatalf("mode=%s", opts.Mode)
	}
	if !opts.DropPrevious {
		t.Fatal("image refresh must drop the old primary after cutover")
	}
	if opts.AfterPromote == nil {
		t.Fatal("image refresh must promote the replica live")
	}
}

func TestInstanceScaleOptionsDoesNotWaitForJobUp(t *testing.T) {
	opts := instanceScaleOptions()
	if !opts.NoWait {
		t.Fatal("ScaleAppRelease must NoWait; initdb keeps the job starting until discoverd")
	}
	if opts.Processes[postgres.ProcessName] != postgres.DefaultNodes {
		t.Fatalf("processes %#v", opts.Processes)
	}
	if opts.Timeout == nil || *opts.Timeout != instanceReadyTimeout {
		t.Fatalf("timeout %v", opts.Timeout)
	}
	// A 5m wait (without NoWait) is longer than DefaultDeployTimeout, so
	// ScaleAppRelease would enable 30s stall probes and fail during initdb.
	if instanceReadyTimeout <= 2*time.Minute {
		t.Fatalf("discoverd wait %s must outlast initdb", instanceReadyTimeout)
	}
}

func TestStartIsolatedInstanceScalesWithoutWaitingForJobUp(t *testing.T) {
	t.Setenv("DISCOVERD_AUTH_KEY", "")
	t.Setenv("DISCOVERD", "")
	ctrl := &fakeInstanceControl{}
	var waited string
	var waitFor time.Duration
	inst := &postgres.Instance{
		App:         "pg-harbor-kxmnpq",
		AppUser:     "app_u",
		AppPassword: "apppw",
		Databases:   []postgres.Database{{Name: "db_shop"}},
		ServiceHost: "leader.pg-harbor-kxmnpq.discoverd",
	}
	err := startIsolatedInstance(ctrl, "img-1", inst, nil, nil, func(service string, timeout time.Duration) error {
		waited = service
		waitFor = timeout
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ctrl.scaled) != 1 || !ctrl.scaled[0].NoWait {
		t.Fatalf("scale %#v", ctrl.scaled)
	}
	if waited != "" {
		t.Fatalf("provision must return before discoverd; waited %q", waited)
	}
	if waitFor != 0 {
		t.Fatalf("timeout %s", waitFor)
	}
	if len(ctrl.deleted) != 0 {
		t.Fatalf("deleted %v", ctrl.deleted)
	}
	if ctrl.current["pg-harbor-kxmnpq"] == "" {
		t.Fatal("current release not set")
	}
	if len(ctrl.releases) != 1 {
		t.Fatalf("releases %d", len(ctrl.releases))
	}
	if ctrl.releases[0].Env["DISCOVERD_AUTH_KEY"] != "" {
		t.Fatalf("empty process env must not invent DISCOVERD_AUTH_KEY: %v", ctrl.releases[0].Env)
	}
	proc := ctrl.releases[0].Processes[postgres.ProcessName]
	if proc.Service != "pg-harbor-kxmnpq" || strings.Contains(proc.Service, postgres.PlatformApplianceHost) {
		t.Fatalf("service %q", proc.Service)
	}
}

type hangAppDeleter struct {
	started chan struct{}
	block   chan struct{}
}

func (h hangAppDeleter) DeleteApp(string) (*ct.AppDeletion, error) {
	close(h.started)
	<-h.block
	return &ct.AppDeletion{}, nil
}

func TestStartIsolatedAppDeletionReturnsBeforeTeardown(t *testing.T) {
	d := hangAppDeleter{started: make(chan struct{}), block: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		startIsolatedAppDeletion(d, "pg-shop")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("resource delete waited for app teardown")
	}
	select {
	case <-d.started:
	case <-time.After(time.Second):
		t.Fatal("DeleteApp was not started")
	}
	close(d.block)
}

func TestStartIsolatedFollowerDoesNotWaitForDiscoverd(t *testing.T) {
	ctrl := &fakeInstanceControl{}
	waited := false
	leader := &postgres.Instance{App: "postgresql-meadow-11111", AppUser: "u", AppPassword: "p"}
	fol := &postgres.Instance{
		App:         "postgresql-upland-22222",
		AppUser:     "u",
		AppPassword: "p",
		Role:        postgres.RoleFollower,
		LeaderID:    leader.App,
		Databases:   []postgres.Database{{Name: "db"}},
	}
	err := startIsolatedInstance(ctrl, "img-1", fol, leader, nil, func(string, time.Duration) error {
		waited = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if waited {
		t.Fatal("follower start must return before discoverd so wait/progress can stream copy percent")
	}
	if len(ctrl.scaled) != 1 || !ctrl.scaled[0].NoWait {
		t.Fatalf("scale %#v", ctrl.scaled)
	}
}

func TestStartIsolatedInstanceDoesNotWaitForDiscoverd(t *testing.T) {
	ctrl := &fakeInstanceControl{}
	waited := false
	err := startIsolatedInstance(ctrl, "img-1", &postgres.Instance{App: "pg-harbor-aaaaaa"}, nil, nil, func(string, time.Duration) error {
		waited = true
		return errDiscoverdWait
	})
	if err != nil {
		t.Fatal(err)
	}
	if waited {
		t.Fatal("provision must return after scale; pg:wait / the dashboard poll discoverd")
	}
	if len(ctrl.deleted) != 0 {
		t.Fatalf("deleted %v", ctrl.deleted)
	}
}

func TestWaitInstanceReadyRequiresDiscoverdAuthKey(t *testing.T) {
	t.Setenv("DISCOVERD_AUTH_KEY", "")
	err := waitInstanceReady("pg-shop", time.Second)
	if err == nil || !strings.Contains(err.Error(), "DISCOVERD_AUTH_KEY") {
		t.Fatalf("got %v", err)
	}
}

func TestStartIsolatedInstanceCopiesDiscoverdAuthKey(t *testing.T) {
	t.Setenv("DISCOVERD_AUTH_KEY", "disc-secret")
	t.Setenv("DISCOVERD", "http://192.0.2.200:1111")
	ctrl := &fakeInstanceControl{}
	err := startIsolatedInstance(ctrl, "img-1", &postgres.Instance{
		App:         "pg-harbor-bbbbbb",
		AppUser:     "u",
		AppPassword: "p",
		Databases:   []postgres.Database{{Name: "db"}},
	}, nil, nil, func(string, time.Duration) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(ctrl.releases) != 1 {
		t.Fatalf("releases %d", len(ctrl.releases))
	}
	env := ctrl.releases[0].Env
	if env["DISCOVERD_AUTH_KEY"] != "disc-secret" {
		t.Fatalf("DISCOVERD_AUTH_KEY=%q", env["DISCOVERD_AUTH_KEY"])
	}
	if env["DISCOVERD"] != "http://192.0.2.200:1111" {
		t.Fatalf("DISCOVERD=%q", env["DISCOVERD"])
	}
}

func TestStartIsolatedInstanceStampsResourceID(t *testing.T) {
	ctrl := &fakeInstanceControl{}
	err := startIsolatedInstance(ctrl, "img-1", &postgres.Instance{
		ID:          "res-abc",
		App:         "postgresql-upland-88340",
		AppUser:     "u",
		AppPassword: "p",
		Databases:   []postgres.Database{{Name: "db"}},
	}, nil, nil, func(string, time.Duration) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if ctrl.releases[0].Env[postgres.ResourceIDEnv] != "res-abc" {
		t.Fatalf("env %#v", ctrl.releases[0].Env)
	}
}

func TestStartIsolatedFollowerSetsPrimaryURL(t *testing.T) {
	ctrl := &fakeInstanceControl{}
	leader := &postgres.Instance{
		App:         "pg-harbor-cccccc",
		AppUser:     "app_u",
		AppPassword: "apppw",
		Databases:   []postgres.Database{{Name: "db_shop"}},
		ServiceHost: "leader.pg-harbor-cccccc.discoverd",
	}
	fol := &postgres.Instance{
		App:         "pg-harbor-dddddd",
		AppUser:     "app_u",
		AppPassword: "apppw",
		Role:        postgres.RoleFollower,
		Databases:   []postgres.Database{{Name: "db_shop"}},
	}
	err := startIsolatedInstance(ctrl, "img-1", fol, leader, nil, func(string, time.Duration) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if ctrl.releases[0].Env["POSTGRES_PRIMARY_URL"] != leader.ConnectionURL() {
		t.Fatalf("POSTGRES_PRIMARY_URL=%q", ctrl.releases[0].Env["POSTGRES_PRIMARY_URL"])
	}
	if ctrl.releases[0].Env["POSTGRES_ROLE"] != "follower" || ctrl.releases[0].Env["POSTGRES_LEADER"] != "pg-harbor-cccccc" {
		t.Fatalf("role env %#v", ctrl.releases[0].Env)
	}
}

func TestStampIsolatedRoleClearsFollowerMarkers(t *testing.T) {
	ctrl := &fakeInstanceControl{}
	leader := &postgres.Instance{
		App:         "pg-harbor-cccccc",
		AppUser:     "app_u",
		AppPassword: "apppw",
		Databases:   []postgres.Database{{Name: "db_shop"}},
		ServiceHost: "leader.pg-harbor-cccccc.discoverd",
	}
	fol := &postgres.Instance{
		App:         "pg-harbor-dddddd",
		AppUser:     "app_u",
		AppPassword: "apppw",
		Role:        postgres.RoleFollower,
		Databases:   []postgres.Database{{Name: "db_shop"}},
	}
	if err := startIsolatedInstance(ctrl, "img-1", fol, leader, nil, func(string, time.Duration) error { return nil }); err != nil {
		t.Fatal(err)
	}
	fol.Role = postgres.RoleStandalone
	fol.LeaderID = ""
	fol.ReadOnly = false
	if err := stampIsolatedRole(ctrl, fol, nil); err != nil {
		t.Fatal(err)
	}
	rel, err := ctrl.GetAppRelease("pg-harbor-dddddd")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Env["POSTGRES_ROLE"] != "primary" {
		t.Fatalf("role %#v", rel.Env)
	}
	if rel.Env["POSTGRES_LEADER"] != "" || rel.Env["POSTGRES_PRIMARY_URL"] != "" {
		t.Fatalf("leftover follower markers %#v", rel.Env)
	}
	if rel.Env["POSTGRES_USER"] != "app_u" || rel.Env["POSTGRES_PASSWORD"] != "apppw" || rel.Env["POSTGRES_DB"] != "db_shop" {
		t.Fatalf("isolated instance must keep login env %#v", rel.Env)
	}
}

type fakeAppRelease struct {
	apps      map[string]*ct.App
	releases  map[string]*ct.Release
	resources []*ct.Resource
}

func (f fakeAppRelease) GetApp(id string) (*ct.App, error) {
	if a := f.apps[id]; a != nil {
		return a, nil
	}
	return nil, errors.New("not found")
}

func (f fakeAppRelease) GetAppRelease(id string) (*ct.Release, error) {
	if r := f.releases[id]; r != nil {
		return r, nil
	}
	return nil, errors.New("not found")
}

func (f fakeAppRelease) ResourceListAll() ([]*ct.Resource, error) {
	return f.resources, nil
}

func TestLiveFollowerAppNamesIgnoresGoneReplica(t *testing.T) {
	leader := &postgres.Instance{App: "postgresql-fjord-21944", ID: "res-leader"}
	c := fakeAppRelease{
		apps: map[string]*ct.App{
			"postgresql-fjord-21944": {ID: "p", Name: "postgresql-fjord-21944"},
		},
		releases: map[string]*ct.Release{
			"p": {Env: map[string]string{"FLYNN_POSTGRES": "postgresql-fjord-21944"}},
		},
	}
	names, err := liveFollowerAppNames(c, leader)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Fatalf("deleted replica still listed: %q", names)
	}
}

func TestLiveFollowerAppNamesFindsReplica(t *testing.T) {
	leader := &postgres.Instance{App: "postgresql-fjord-21944"}
	c := fakeAppRelease{
		apps: map[string]*ct.App{
			"postgresql-fjord-21944":  {ID: "p", Name: "postgresql-fjord-21944"},
			"postgresql-orchid-80127": {ID: "f", Name: "postgresql-orchid-80127"},
		},
		releases: map[string]*ct.Release{
			"p": {Env: map[string]string{"FLYNN_POSTGRES": "postgresql-fjord-21944"}},
			"f": {Env: map[string]string{
				"FLYNN_POSTGRES":  "postgresql-orchid-80127",
				"POSTGRES_LEADER": "postgresql-fjord-21944",
				"POSTGRES_ROLE":   "follower",
			}},
		},
	}
	names, err := liveFollowerAppNames(c, leader)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "postgresql-orchid-80127" {
		t.Fatalf("got %q", names)
	}
}

func (f fakeAppRelease) AppList() ([]*ct.App, error) {
	seen := map[string]bool{}
	var out []*ct.App
	for _, a := range f.apps {
		if a == nil {
			continue
		}
		key := a.ID
		if key == "" {
			key = a.Name
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, a)
	}
	return out, nil
}

func TestLoadLivePostgresFollowsIdentityEnv(t *testing.T) {
	c := fakeAppRelease{
		apps: map[string]*ct.App{
			"shop":             {ID: "shop-id", Name: "shop"},
			"pg-orchid-xkhthp": {ID: "pg-id", Name: "pg-orchid-xkhthp"},
		},
		releases: map[string]*ct.Release{
			"shop-id": {Env: map[string]string{"FLYNN_POSTGRES": "pg-orchid-xkhthp"}},
			"pg-id": {Env: map[string]string{
				"FLYNN_POSTGRES":    "pg-orchid-xkhthp",
				"POSTGRES_USER":     "app_live",
				"POSTGRES_PASSWORD": "secret",
				"POSTGRES_DB":       "db_pg_orchid_xkhthp",
			}},
		},
	}
	inst := loadLivePostgres(c, "pg-orchid-xkhthp")
	if inst == nil || inst.App != "pg-orchid-xkhthp" || inst.AppUser != "app_live" {
		t.Fatalf("direct %+v", inst)
	}
	viaShop := loadLivePostgres(c, "shop")
	if viaShop == nil || viaShop.App != "pg-orchid-xkhthp" || viaShop.ID != "pg-id" {
		t.Fatalf("via shop %+v", viaShop)
	}
	if loadLivePostgres(c, "missing") != nil {
		t.Fatal("missing")
	}
}

func TestLoadLivePostgresIgnoresTenantApp(t *testing.T) {
	c := fakeAppRelease{
		apps: map[string]*ct.App{
			"app-one":                 {ID: "app-one", Name: "app-one"},
			"postgresql-harbor-12345": {ID: "pg-id", Name: "postgresql-harbor-12345"},
		},
		releases: map[string]*ct.Release{
			"app-one": {Env: map[string]string{
				"DATABASE_URL":   "postgres://u:p@leader.postgresql-harbor-12345.discoverd:5432/db?sslmode=require",
				"FLYNN_POSTGRES": "postgresql-harbor-12345",
			}},
			"pg-id": {Env: map[string]string{
				"FLYNN_POSTGRES": "postgresql-harbor-12345",
				"POSTGRES_USER":  "app_live",
				"ENGINE_VERSION": "15",
			}},
		},
	}
	viaTenant := loadLivePostgres(c, "app-one")
	if viaTenant == nil || viaTenant.App != "postgresql-harbor-12345" {
		t.Fatalf("must follow FLYNN_POSTGRES to isolated instance, got %+v", viaTenant)
	}
	tenantOnly := fakeAppRelease{
		apps: map[string]*ct.App{
			"app-one": {ID: "app-one", Name: "app-one"},
		},
		releases: map[string]*ct.Release{
			"app-one": {Env: map[string]string{
				"DATABASE_URL": "postgres://u:p@leader.postgresql-harbor-12345.discoverd:5432/db?sslmode=require",
			}},
		},
	}
	if inst := loadLivePostgres(tenantOnly, "app-one"); inst != nil {
		t.Fatalf("tenant DATABASE_URL must not hydrate a postgres instance: %+v", inst)
	}
}

func TestStartIsolatedInstanceRefusesTenantApp(t *testing.T) {
	ctrl := &fakeInstanceControl{}
	err := startIsolatedInstance(ctrl, "img-1", &postgres.Instance{
		App:         "app-one",
		AppUser:     "u",
		AppPassword: "p",
		Databases:   []postgres.Database{{Name: "db"}},
	}, nil, nil, func(string, time.Duration) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "tenant") {
		t.Fatalf("got %v", err)
	}
	if len(ctrl.apps) != 0 || len(ctrl.releases) != 0 || len(ctrl.scaled) != 0 {
		t.Fatalf("plugin boot must not deploy tenant apps: apps=%d releases=%d scales=%d", len(ctrl.apps), len(ctrl.releases), len(ctrl.scaled))
	}
}

func TestStampIsolatedRoleRefusesTenantApp(t *testing.T) {
	ctrl := &fakeInstanceControl{}
	err := stampIsolatedRole(ctrl, &postgres.Instance{App: "app-one"}, nil)
	if err == nil || !strings.Contains(err.Error(), "tenant") {
		t.Fatalf("got %v", err)
	}
	if len(ctrl.releases) != 0 || len(ctrl.current) != 0 {
		t.Fatalf("must not rewrite tenant release: releases=%d current=%v", len(ctrl.releases), ctrl.current)
	}
}

func TestLoadLivePostgresFindsResourceID(t *testing.T) {
	c := fakeAppRelease{
		apps: map[string]*ct.App{
			"postgresql-upland-88340": {ID: "app-uuid", Name: "postgresql-upland-88340"},
		},
		releases: map[string]*ct.Release{
			"app-uuid": {Env: map[string]string{
				"FLYNN_POSTGRES":       "postgresql-upland-88340",
				postgres.ResourceIDEnv: "aabbccddeeff0011",
				"POSTGRES_USER":        "app_live",
			}},
		},
	}
	inst := loadLivePostgres(c, "aabbccddeeff0011")
	if inst == nil || inst.App != "postgresql-upland-88340" || inst.ID != "aabbccddeeff0011" {
		t.Fatalf("%+v", inst)
	}
}

func TestLoadLivePostgresFindsControllerResourceID(t *testing.T) {
	c := fakeAppRelease{
		apps: map[string]*ct.App{
			"postgresql-concave-78237": {ID: "app-uuid", Name: "postgresql-concave-78237"},
		},
		releases: map[string]*ct.Release{
			"app-uuid": {Env: map[string]string{
				"FLYNN_POSTGRES": "postgresql-concave-78237",
				"POSTGRES_USER":  "app_live",
			}},
		},
		resources: []*ct.Resource{{
			ID:         "controller-res-uuid",
			ExternalID: "plugin-id",
			Env: map[string]string{
				"FLYNN_POSTGRES": "postgresql-concave-78237",
			},
		}},
	}
	inst := loadLivePostgres(c, "controller-res-uuid")
	if inst == nil || inst.App != "postgresql-concave-78237" {
		t.Fatalf("%+v", inst)
	}
}

func TestStartInstanceRequiresPrimaryForFollower(t *testing.T) {
	h := newHandler(postgres.NewStore())
	err := h.startInstance(&postgres.Instance{
		App:      "postgresql-willow-11111",
		Role:     postgres.RoleFollower,
		LeaderID: "postgresql-concave-22222",
	})
	if err == nil || !strings.Contains(err.Error(), "running primary") {
		t.Fatalf("got %v", err)
	}
}

func TestApplyPostgresResourceEnvStampsLeaderApp(t *testing.T) {
	fol := &postgres.Instance{
		App:      "postgresql-willow-11111",
		Role:     postgres.RoleFollower,
		LeaderID: "postgresql-concave-22222",
	}
	env := map[string]string{}
	applyPostgresResourceEnv(fol, env, &postgres.Instance{App: "postgresql-concave-22222"})
	if env["POSTGRES_ROLE"] != "follower" || env["POSTGRES_LEADER"] != "postgresql-concave-22222" || env["FLYNN_POSTGRES"] != "postgresql-willow-11111" {
		t.Fatalf("%v", env)
	}
	env = map[string]string{}
	applyPostgresResourceEnv(fol, env, nil)
	if env["POSTGRES_LEADER"] != "postgresql-concave-22222" {
		t.Fatalf("leader id fallback: %v", env)
	}
}

type fakeOrphanReaper struct {
	apps      []*ct.App
	releases  map[string]*ct.Release
	resources []*ct.Resource
	deleted   []string
	listErr   error
}

func (f *fakeOrphanReaper) AppList() ([]*ct.App, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.apps, nil
}

func (f *fakeOrphanReaper) GetAppRelease(id string) (*ct.Release, error) {
	if r := f.releases[id]; r != nil {
		return r, nil
	}
	return nil, errors.New("not found")
}

func (f *fakeOrphanReaper) DeleteApp(id string) (*ct.AppDeletion, error) {
	f.deleted = append(f.deleted, id)
	return &ct.AppDeletion{}, nil
}

func (f *fakeOrphanReaper) ResourceListAll() ([]*ct.Resource, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.resources, nil
}

func TestReapOrphanPostgresApps(t *testing.T) {
	orphan := &ct.App{ID: "orphan-id", Name: "postgresql-upland-88340"}
	kept := &ct.App{ID: "kept-id", Name: "postgresql-quartz-76554"}
	plugin := &ct.App{ID: "plugin-id", Name: "postgres-plugin", Meta: map[string]string{"flynn-plugin": "true"}}
	platform := &ct.App{ID: "plat-id", Name: "postgres"}
	c := &fakeOrphanReaper{
		apps: []*ct.App{orphan, kept, plugin, platform},
		releases: map[string]*ct.Release{
			"orphan-id": {Env: map[string]string{"FLYNN_POSTGRES": "postgresql-upland-88340"}},
			"kept-id":   {Env: map[string]string{"FLYNN_POSTGRES": "postgresql-quartz-76554"}},
		},
		resources: []*ct.Resource{
			{ExternalID: "res-kept", Env: map[string]string{"FLYNN_POSTGRES": "postgresql-quartz-76554"}},
		},
	}
	deleted, err := reapOrphanPostgresApps(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != "postgresql-upland-88340" {
		t.Fatalf("deleted %v", deleted)
	}
	if len(c.deleted) != 1 || c.deleted[0] != "orphan-id" {
		t.Fatalf("delete calls %v", c.deleted)
	}
}

func TestReapOrphanKeepsUpgradeReplica(t *testing.T) {
	primary := &ct.App{ID: "basin-id", Name: "postgresql-basin-73690"}
	replica := &ct.App{ID: "meadow-id", Name: "postgresql-meadow-99423"}
	c := &fakeOrphanReaper{
		apps: []*ct.App{primary, replica},
		releases: map[string]*ct.Release{
			"basin-id":  {Env: map[string]string{"FLYNN_POSTGRES": "postgresql-basin-73690", "POSTGRES_ROLE": "primary"}},
			"meadow-id": {Env: map[string]string{"FLYNN_POSTGRES": "postgresql-meadow-99423", "POSTGRES_ROLE": "follower", "POSTGRES_LEADER": "postgresql-basin-73690"}},
		},
		resources: []*ct.Resource{
			{ExternalID: "res-basin", Env: map[string]string{"FLYNN_POSTGRES": "postgresql-basin-73690"}},
		},
	}
	deleted, err := reapOrphanPostgresApps(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 0 {
		t.Fatalf("deleted in-progress upgrade replica %v", deleted)
	}
}

type fakeAppMeta struct {
	apps []*ct.App
}

func (f *fakeAppMeta) GetApp(id string) (*ct.App, error) {
	for _, a := range f.apps {
		if a != nil && (a.ID == id || a.Name == id) {
			out := *a
			out.Meta = map[string]string{}
			for k, v := range a.Meta {
				out.Meta[k] = v
			}
			return &out, nil
		}
	}
	return nil, errors.New("not found")
}

func (f *fakeAppMeta) UpdateAppMeta(app *ct.App) error {
	if app == nil {
		return errors.New("missing app")
	}
	for _, a := range f.apps {
		if a != nil && (a.ID == app.ID || a.Name == app.Name || a.Name == app.ID) {
			a.Meta = map[string]string{}
			for k, v := range app.Meta {
				a.Meta[k] = v
			}
			return nil
		}
	}
	return errors.New("not found")
}

func TestClaimImageSwapLastWriterWins(t *testing.T) {
	orig := imageSwapSettle
	imageSwapSettle = 0
	t.Cleanup(func() { imageSwapSettle = orig })
	f := &fakeAppMeta{apps: []*ct.App{{ID: "basin-id", Name: "postgresql-basin-73690", Meta: map[string]string{}}}}
	if !claimImageSwap(f, "postgresql-basin-73690", "job-a") {
		t.Fatal("job-a should claim")
	}
	if !claimImageSwap(f, "postgresql-basin-73690", "job-b") {
		t.Fatal("job-b should overwrite")
	}
	app, err := f.GetApp("postgresql-basin-73690")
	if err != nil {
		t.Fatal(err)
	}
	if app.Meta[imageSwapMetaKey] != "job-b" {
		t.Fatalf("held %q", app.Meta[imageSwapMetaKey])
	}
}

func TestClaimImageSwapMissingApp(t *testing.T) {
	orig := imageSwapSettle
	imageSwapSettle = 0
	t.Cleanup(func() { imageSwapSettle = orig })
	f := &fakeAppMeta{}
	if claimImageSwap(f, "postgresql-basin-73690", "job-a") {
		t.Fatal("missing app must not claim")
	}
}

func TestImageSwapReplicaExists(t *testing.T) {
	c := &fakeOrphanReaper{
		apps: []*ct.App{
			{ID: "basin-id", Name: "postgresql-basin-73690"},
			{ID: "upland-id", Name: "postgresql-upland-22935"},
			{ID: "meadow-id", Name: "postgresql-meadow-99423"},
		},
		releases: map[string]*ct.Release{
			"upland-id": {ArtifactIDs: []string{"img-old"}, Env: map[string]string{"POSTGRES_LEADER": "postgresql-basin-73690"}},
			"meadow-id": {ArtifactIDs: []string{"img-new"}, Env: map[string]string{"POSTGRES_LEADER": "postgresql-basin-73690"}},
		},
	}
	if !imageSwapReplicaExists(c, "postgresql-basin-73690", "img-new") {
		t.Fatal("refresh replica on the new image must count as in progress")
	}
	if imageSwapReplicaExists(c, "postgresql-basin-73690", "img-newer") {
		t.Fatal("an old user follower must not block a later image")
	}
}

func TestReapOrphanPostgresAppsFailsClosed(t *testing.T) {
	c := &fakeOrphanReaper{
		apps:    []*ct.App{{ID: "orphan-id", Name: "postgresql-upland-88340"}},
		listErr: errors.New("controller down"),
	}
	deleted, err := reapOrphanPostgresApps(c)
	if err == nil || len(deleted) != 0 {
		t.Fatalf("got %v %v", deleted, err)
	}
}

var errDiscoverdWait = errors.New("discoverd wait failed")
