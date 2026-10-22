package main

import (
	"strings"
	"testing"

	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
)

func TestParseMaintenanceTargets(t *testing.T) {
	if _, _, err := parseMaintenanceTargets(false, ""); err == nil {
		t.Fatal("empty selection must fail")
	}
	_, all, err := parseMaintenanceTargets(true, "")
	if err != nil || !all {
		t.Fatalf("all flag: all=%v err=%v", all, err)
	}
	only, everything, err := parseMaintenanceTargets(false, "postgresql-timber-43178, postgresql-harbor-10001")
	if err != nil || everything || len(only) != 2 {
		t.Fatalf("list: only=%v everything=%v err=%v", only, everything, err)
	}
	if !maintenanceSelected(only, "postgresql-timber-43178") {
		t.Fatal("listed instance must be selected")
	}
	if maintenanceSelected(only, "postgresql-other-10002") {
		t.Fatal("unlisted instance must be skipped")
	}
	if !maintenanceSelected(nil, "anything") {
		t.Fatal("nil filter is every instance")
	}
}

func TestAdoptAppNameKeepsOriginalResourceName(t *testing.T) {
	c := &fakeNameAdopter{}
	original := &ct.App{ID: "old", Name: "postgresql-timber-43178", Meta: map[string]string{"flynn-system-app": "true"}}
	temp := &ct.App{ID: "new", Name: "postgresql-meadow-99999", Meta: map[string]string{"flynn-system-app": "true"}}
	c.apps = []*ct.App{original, temp}
	c.releases = map[string]*ct.Release{
		"old": {ID: "rel-old", Processes: map[string]ct.ProcessType{postgres.ProcessName: {Service: original.Name}}},
		"new": {ID: "rel-new", Env: map[string]string{"FLYNN_POSTGRES": temp.Name, "PGHOST": "leader." + temp.Name + ".discoverd"}, Processes: map[string]ct.ProcessType{postgres.ProcessName: {Service: temp.Name}}},
	}
	prev := &postgres.Instance{App: original.Name, ServiceHost: "leader." + original.Name + ".discoverd"}
	next := &postgres.Instance{App: temp.Name, ServiceHost: "leader." + temp.Name + ".discoverd"}
	retired, err := adoptAppName(c, prev, next)
	if err != nil {
		t.Fatal(err)
	}
	if next.App != "postgresql-timber-43178" {
		t.Fatalf("replacement must take the original name, got %s", next.App)
	}
	if prev.App != retired || !strings.HasPrefix(retired, "postgresql-timber-43178-retired1") {
		t.Fatalf("previous app must be parked aside, got %s retired %s", prev.App, retired)
	}
	if next.ServiceHost != "leader.postgresql-timber-43178.discoverd" {
		t.Fatalf("service host %s", next.ServiceHost)
	}
	rel := c.releases["new"]
	if rel.Env["FLYNN_POSTGRES"] != "postgresql-timber-43178" {
		t.Fatalf("release env name %s", rel.Env["FLYNN_POSTGRES"])
	}
	if rel.Processes[postgres.ProcessName].Service != "postgresql-timber-43178" {
		t.Fatalf("service %s", rel.Processes[postgres.ProcessName].Service)
	}
	if c.appsByID("old").Name != retired {
		t.Fatalf("old app name %s", c.appsByID("old").Name)
	}
	if c.appsByID("new").Name != "postgresql-timber-43178" {
		t.Fatalf("new app name %s", c.appsByID("new").Name)
	}
}

type fakeNameAdopter struct {
	apps     []*ct.App
	releases map[string]*ct.Release
	current  map[string]string
	scaled   []string
}

func (f *fakeNameAdopter) appsByID(id string) *ct.App {
	for _, app := range f.apps {
		if app != nil && app.ID == id {
			return app
		}
	}
	return nil
}

func (f *fakeNameAdopter) GetApp(id string) (*ct.App, error) {
	for _, app := range f.apps {
		if app != nil && (app.ID == id || app.Name == id) {
			return app, nil
		}
	}
	return nil, ct.ErrNotFound
}

func (f *fakeNameAdopter) UpdateApp(app *ct.App) error {
	for i, cur := range f.apps {
		if cur != nil && cur.ID == app.ID {
			f.apps[i] = app
			return nil
		}
	}
	return ct.ErrNotFound
}

func (f *fakeNameAdopter) GetAppRelease(id string) (*ct.Release, error) {
	app, err := f.GetApp(id)
	if err != nil {
		return nil, err
	}
	if f.current != nil {
		if rid := f.current[app.ID]; rid != "" {
			return f.releases[rid], nil
		}
	}
	rel := f.releases[app.ID]
	if rel == nil {
		return nil, ct.ErrNotFound
	}
	return rel, nil
}

func (f *fakeNameAdopter) CreateRelease(appID string, release *ct.Release) error {
	if release.ID == "" {
		release.ID = "rel-next"
	}
	app, err := f.GetApp(appID)
	if err != nil {
		return err
	}
	f.releases[app.ID] = release
	return nil
}

func (f *fakeNameAdopter) SetAppRelease(appID, releaseID string) error {
	if f.current == nil {
		f.current = map[string]string{}
	}
	f.current[appID] = releaseID
	return nil
}

func (f *fakeNameAdopter) ScaleAppRelease(appID, releaseID string, _ ct.ScaleOptions) error {
	f.scaled = append(f.scaled, appID+":"+releaseID)
	return nil
}
