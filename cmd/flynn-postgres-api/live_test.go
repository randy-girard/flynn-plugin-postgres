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
		App:         "pg-shop",
		AppUser:     "app_u",
		AppPassword: "apppw",
		Databases:   []postgres.Database{{Name: "db_shop"}},
		ServiceHost: "leader.pg-shop.discoverd",
	}
	err := startIsolatedInstance(ctrl, "img-1", inst, nil, func(service string, timeout time.Duration) error {
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
	if waited != "pg-shop" || waitFor != instanceReadyTimeout {
		t.Fatalf("discoverd wait service=%q timeout=%s", waited, waitFor)
	}
	if len(ctrl.deleted) != 0 {
		t.Fatalf("deleted %v", ctrl.deleted)
	}
	if ctrl.current["pg-shop"] == "" {
		t.Fatal("current release not set")
	}
	if len(ctrl.releases) != 1 {
		t.Fatalf("releases %d", len(ctrl.releases))
	}
	if ctrl.releases[0].Env["DISCOVERD_AUTH_KEY"] != "" {
		t.Fatalf("empty process env must not invent DISCOVERD_AUTH_KEY: %v", ctrl.releases[0].Env)
	}
	proc := ctrl.releases[0].Processes[postgres.ProcessName]
	if proc.Service != "pg-shop" || strings.Contains(proc.Service, postgres.PlatformApplianceHost) {
		t.Fatalf("service %q", proc.Service)
	}
}

func TestStartIsolatedInstanceDeletesAppWhenDiscoverdWaitFails(t *testing.T) {
	ctrl := &fakeInstanceControl{}
	err := startIsolatedInstance(ctrl, "img-1", &postgres.Instance{App: "pg-fail"}, nil, func(string, time.Duration) error {
		return errDiscoverdWait
	})
	if !errors.Is(err, errDiscoverdWait) {
		t.Fatalf("got %v", err)
	}
	if len(ctrl.deleted) != 1 || ctrl.deleted[0] != "pg-fail" {
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
		App:         "pg-auth",
		AppUser:     "u",
		AppPassword: "p",
		Databases:   []postgres.Database{{Name: "db"}},
	}, nil, func(string, time.Duration) error { return nil })
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

func TestStartIsolatedFollowerSetsPrimaryURL(t *testing.T) {
	ctrl := &fakeInstanceControl{}
	leader := &postgres.Instance{
		App:         "pg-leader",
		AppUser:     "app_u",
		AppPassword: "apppw",
		Databases:   []postgres.Database{{Name: "db_shop"}},
		ServiceHost: "leader.pg-leader.discoverd",
	}
	fol := &postgres.Instance{
		App:         "pg-follower",
		AppUser:     "app_u",
		AppPassword: "apppw",
		Role:        postgres.RoleFollower,
		Databases:   []postgres.Database{{Name: "db_shop"}},
	}
	err := startIsolatedInstance(ctrl, "img-1", fol, leader, func(string, time.Duration) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if ctrl.releases[0].Env["POSTGRES_PRIMARY_URL"] != leader.ConnectionURL() {
		t.Fatalf("POSTGRES_PRIMARY_URL=%q", ctrl.releases[0].Env["POSTGRES_PRIMARY_URL"])
	}
	if ctrl.releases[0].Env["POSTGRES_ROLE"] != "follower" || ctrl.releases[0].Env["POSTGRES_LEADER"] != "pg-leader" {
		t.Fatalf("role env %#v", ctrl.releases[0].Env)
	}
}

func TestStampIsolatedRoleClearsFollowerMarkers(t *testing.T) {
	ctrl := &fakeInstanceControl{}
	leader := &postgres.Instance{
		App:         "pg-leader",
		AppUser:     "app_u",
		AppPassword: "apppw",
		Databases:   []postgres.Database{{Name: "db_shop"}},
		ServiceHost: "leader.pg-leader.discoverd",
	}
	fol := &postgres.Instance{
		App:         "pg-follower",
		AppUser:     "app_u",
		AppPassword: "apppw",
		Role:        postgres.RoleFollower,
		Databases:   []postgres.Database{{Name: "db_shop"}},
	}
	if err := startIsolatedInstance(ctrl, "img-1", fol, leader, func(string, time.Duration) error { return nil }); err != nil {
		t.Fatal(err)
	}
	fol.Role = postgres.RoleStandalone
	fol.LeaderID = ""
	fol.ReadOnly = false
	if err := stampIsolatedRole(ctrl, fol, nil); err != nil {
		t.Fatal(err)
	}
	rel, err := ctrl.GetAppRelease("pg-follower")
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
	apps     map[string]*ct.App
	releases map[string]*ct.Release
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

var errDiscoverdWait = errors.New("discoverd wait failed")
