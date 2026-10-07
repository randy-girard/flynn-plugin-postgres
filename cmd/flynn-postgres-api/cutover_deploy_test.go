package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
)

type fakeTenantDeploy struct {
	apps      []*ct.App
	releases  map[string]*ct.Release
	created   []string
	deployed  []string
	deployErr error
}

func (f *fakeTenantDeploy) GetApp(id string) (*ct.App, error) {
	for _, a := range f.apps {
		if a != nil && (a.ID == id || a.Name == id) {
			return a, nil
		}
	}
	return nil, errors.New("not found")
}

func (f *fakeTenantDeploy) GetAppRelease(id string) (*ct.Release, error) {
	if f.releases == nil {
		return nil, errors.New("not found")
	}
	if rel := f.releases[id]; rel != nil {
		return rel, nil
	}
	return nil, errors.New("not found")
}

func (f *fakeTenantDeploy) CreateRelease(appID string, release *ct.Release) error {
	if release.ID == "" {
		release.ID = "rel-new"
	}
	f.created = append(f.created, appID)
	if f.releases == nil {
		f.releases = map[string]*ct.Release{}
	}
	f.releases[appID] = release
	return nil
}

func (f *fakeTenantDeploy) DeployAppRelease(appID, releaseID string, _ <-chan struct{}) error {
	if f.deployErr != nil {
		return f.deployErr
	}
	f.deployed = append(f.deployed, appID+"/"+releaseID)
	return nil
}

func TestDeployTenantPostgresEnvCreatesDeployment(t *testing.T) {
	prev := &postgres.Instance{App: "postgresql-basin-73690", ServiceHost: "leader.postgresql-basin-73690.discoverd"}
	next := &postgres.Instance{App: "postgresql-orchid-80127", ServiceHost: "leader.postgresql-orchid-80127.discoverd", Role: postgres.RolePrimary}
	orig := &ct.Release{
		ID: "live",
		Env: map[string]string{
			"DATABASE_URL":   "postgres://u:p@leader.postgresql-basin-73690.discoverd:5432/db?sslmode=require",
			"FLYNN_POSTGRES": prev.App,
		},
		Meta: map[string]string{"git": "true", "git.commit": "deadbeef"},
	}
	ctrl := &fakeTenantDeploy{
		apps:     []*ct.App{{ID: "shop", Name: "shop"}},
		releases: map[string]*ct.Release{"shop": orig},
	}
	if err := deployTenantPostgresEnv(ctrl, "shop", prev, next); err != nil {
		t.Fatal(err)
	}
	if len(ctrl.created) != 1 || len(ctrl.deployed) != 1 {
		t.Fatalf("created=%v deployed=%v", ctrl.created, ctrl.deployed)
	}
	rel := ctrl.releases["shop"]
	if rel.Env["DATABASE_URL"] != "postgres://u:p@leader.postgresql-orchid-80127.discoverd:5432/db?sslmode=require" {
		t.Fatalf("DATABASE_URL=%s", rel.Env["DATABASE_URL"])
	}
	if rel.Env["FLYNN_POSTGRES"] != next.App {
		t.Fatalf("FLYNN_POSTGRES=%s", rel.Env["FLYNN_POSTGRES"])
	}
	if rel.Meta[metaSystemDeploy] != "true" {
		t.Fatalf("system deploy meta: %#v", rel.Meta)
	}
	note := rel.Meta[metaSystemDeployNote]
	if !strings.Contains(note, "System deployment") || !strings.Contains(note, "not a git push") {
		t.Fatalf("note=%q", note)
	}
	if !strings.Contains(note, prev.App) || !strings.Contains(note, next.App) {
		t.Fatalf("note missing instance names: %q", note)
	}
	if orig.Meta[metaSystemDeploy] != "" {
		t.Fatal("must not stamp system-deploy on the previous release")
	}
}

func TestDeployTenantPostgresEnvSkipsIsolatedApp(t *testing.T) {
	ctrl := &fakeTenantDeploy{
		apps: []*ct.App{{ID: "a1", Name: "postgresql-orchid-80127"}},
		releases: map[string]*ct.Release{
			"a1": {ID: "live", Env: map[string]string{"DATABASE_URL": "postgres://old"}},
		},
	}
	next := &postgres.Instance{App: "postgresql-orchid-80127"}
	if err := deployTenantPostgresEnv(ctrl, "a1", nil, next); err != nil {
		t.Fatal(err)
	}
	if len(ctrl.deployed) != 0 {
		t.Fatalf("must not deploy isolated instance: %v", ctrl.deployed)
	}
}

func TestDeployTenantPostgresEnvKeepsOldPrimaryOnFailure(t *testing.T) {
	prev := &postgres.Instance{App: "postgresql-basin-73690", ServiceHost: "leader.postgresql-basin-73690.discoverd"}
	next := &postgres.Instance{App: "postgresql-orchid-80127", ServiceHost: "leader.postgresql-orchid-80127.discoverd"}
	ctrl := &fakeTenantDeploy{
		apps: []*ct.App{{ID: "shop", Name: "shop"}},
		releases: map[string]*ct.Release{
			"shop": {ID: "live", Env: map[string]string{
				"DATABASE_URL": "postgres://u:p@leader.postgresql-basin-73690.discoverd:5432/db",
			}},
		},
		deployErr: errors.New("deploy failed"),
	}
	if err := deployTenantPostgresEnv(ctrl, "shop", prev, next); err == nil {
		t.Fatal("deploy failure must abort so the old primary is not dropped")
	}
}
