package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
)

type res08Scale struct {
	app, release string
	procs        map[string]int
}

type res08Adopter struct {
	apps       []*ct.App
	releases   map[string]*ct.Release // by app id
	current    map[string]string
	formations map[string]*ct.Formation // by app id
	scaled     []res08Scale
	failSet    bool
}

func (f *res08Adopter) GetApp(id string) (*ct.App, error) {
	for _, a := range f.apps {
		if a.ID == id || a.Name == id {
			return a, nil
		}
	}
	return nil, ct.ErrNotFound
}

func (f *res08Adopter) UpdateApp(app *ct.App) error { return nil }

func (f *res08Adopter) GetAppRelease(id string) (*ct.Release, error) {
	a, err := f.GetApp(id)
	if err != nil {
		return nil, err
	}
	if rid := f.current[a.ID]; rid != "" {
		for _, r := range f.releases {
			if r.ID == rid {
				return r, nil
			}
		}
	}
	return f.releases[a.ID], nil
}

func (f *res08Adopter) CreateRelease(appID string, rel *ct.Release) error {
	rel.ID = "rel-rewritten"
	f.releases[appID+"-rewritten"] = rel
	return nil
}

func (f *res08Adopter) SetAppRelease(appID, relID string) error {
	if f.failSet && relID == "rel-rewritten" {
		return errors.New("boom")
	}
	if f.current == nil {
		f.current = map[string]string{}
	}
	f.current[appID] = relID
	return nil
}

func (f *res08Adopter) GetFormation(appID, relID string) (*ct.Formation, error) {
	if fm := f.formations[appID]; fm != nil {
		return fm, nil
	}
	return nil, ct.ErrNotFound
}

func (f *res08Adopter) ScaleAppRelease(appID, relID string, o ct.ScaleOptions) error {
	f.scaled = append(f.scaled, res08Scale{appID, relID, o.Processes})
	return nil
}

func res08Setup() (*res08Adopter, *postgres.Instance, *postgres.Instance) {
	f := &res08Adopter{
		apps: []*ct.App{{ID: "old", Name: "orig-1"}, {ID: "new", Name: "temp-2"}},
		releases: map[string]*ct.Release{
			"old": {ID: "rel-old", Processes: map[string]ct.ProcessType{postgres.ProcessName: {Service: "orig-1"}}},
			"new": {ID: "rel-new", Processes: map[string]ct.ProcessType{postgres.ProcessName: {Service: "temp-2"}}},
		},
	}
	return f, &postgres.Instance{App: "orig-1"}, &postgres.Instance{App: "temp-2"}
}

// RES-08: after the rename the replacement must be scaled back up on the
// rewritten release, otherwise the instance has zero processes.
func TestAdoptAppNameRestartsReplacement(t *testing.T) {
	f, prev, next := res08Setup()
	if _, err := adoptAppName(f, prev, next); err != nil {
		t.Fatal(err)
	}
	last := f.scaled[len(f.scaled)-1]
	if last.app != "new" || last.release != "rel-rewritten" || last.procs[postgres.ProcessName] != 1 {
		t.Fatalf("last scale = %+v, want new/rel-rewritten/%s:1 (all: %+v)", last, postgres.ProcessName, f.scaled)
	}
}

func TestAdoptAppNameRestoresOriginalProcessCounts(t *testing.T) {
	f, prev, next := res08Setup()
	f.formations = map[string]*ct.Formation{"new": {Processes: map[string]int{postgres.ProcessName: 3}}}
	if _, err := adoptAppName(f, prev, next); err != nil {
		t.Fatal(err)
	}
	last := f.scaled[len(f.scaled)-1]
	if last.app != "new" || last.procs[postgres.ProcessName] != 3 {
		t.Fatalf("last scale = %+v, want original count 3", last)
	}
}

func TestAdoptAppNameRollsBackOnError(t *testing.T) {
	f, prev, next := res08Setup()
	f.failSet = true
	if _, err := adoptAppName(f, prev, next); err == nil {
		t.Fatal("expected error")
	}
	if prev.App != "orig-1" || next.App != "temp-2" {
		t.Fatalf("names must be untouched: %s %s", prev.App, next.App)
	}
	if n, _ := f.GetApp("old"); strings.Contains(n.Name, "retired") {
		t.Fatalf("original app still renamed: %s", n.Name)
	}
	up := map[string]bool{}
	for _, s := range f.scaled {
		up[s.app] = s.procs[postgres.ProcessName] > 0
	}
	if !up["old"] || !up["new"] {
		t.Fatalf("both apps must be scaled back up after failure: %+v", f.scaled)
	}
}
