package main

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/randy-girard/flynn-plugin-postgres"
	controller "github.com/randy-girard/flynn/controller/client"
	ct "github.com/randy-girard/flynn/controller/types"
	"github.com/randy-girard/flynn/pkg/dbruntime"
)

const fiveGiB int64 = 5 * 1024 * 1024 * 1024

// sizeCtl records the releases a provision creates and serves a database
// runtime catalog.
type sizeCtl struct {
	controller.Client
	cat      dbruntime.Catalog
	apps     []*ct.App
	releases []*ct.Release
	current  map[string]*ct.Release
}

func newSizeCtl(t *testing.T, allowCustom bool) *sizeCtl {
	t.Helper()
	cat := dbruntime.Catalog{AllowCustomSizes: allowCustom}
	if err := cat.EnsureEngine(dbruntime.EnginePostgres); err != nil {
		t.Fatal(err)
	}
	return &sizeCtl{cat: cat, current: map[string]*ct.Release{}}
}

func (f *sizeCtl) ListDBRuntimes() (*dbruntime.Catalog, error) { c := f.cat; return &c, nil }
func (f *sizeCtl) CreateApp(app *ct.App) error {
	if app.ID == "" {
		app.ID = app.Name
	}
	f.apps = append(f.apps, app)
	return nil
}
func (f *sizeCtl) CreateRelease(appID string, rel *ct.Release) error {
	rel.ID = "rel-" + appID + "-" + strconv.Itoa(len(f.releases))
	rel.AppID = appID
	f.releases = append(f.releases, rel)
	return nil
}
func (f *sizeCtl) ScaleAppRelease(string, string, ct.ScaleOptions) error { return nil }
func (f *sizeCtl) SetAppRelease(appID, releaseID string) error {
	for _, r := range f.releases {
		if r.ID == releaseID {
			f.current[appID] = r
		}
	}
	return nil
}
func (f *sizeCtl) GetAppRelease(appID string) (*ct.Release, error) {
	if r, ok := f.current[appID]; ok {
		return r, nil
	}
	return nil, controller.ErrNotFound
}
func (f *sizeCtl) GetApp(name string) (*ct.App, error) {
	for _, a := range f.apps {
		if a.ID == name || a.Name == name {
			return a, nil
		}
	}
	return nil, controller.ErrNotFound
}
func (f *sizeCtl) DeleteApp(id string) (*ct.AppDeletion, error) {
	return &ct.AppDeletion{AppID: id}, nil
}
func (f *sizeCtl) JobList(string) ([]*ct.Job, error) { return nil, nil }
func (f *sizeCtl) JobListActive() ([]*ct.Job, error) { return nil, nil }

func (f *sizeCtl) lastVolumes(t *testing.T) []ct.VolumeReq {
	t.Helper()
	if len(f.releases) == 0 {
		t.Fatal("no release created")
	}
	return f.releases[len(f.releases)-1].Processes[postgres.ProcessName].Volumes
}

func provisionWithBody(t *testing.T, ctl *sizeCtl, body string) (*httptest.ResponseRecorder, *postgres.Store) {
	t.Helper()
	store := postgres.NewStore()
	h := newHandler(store)
	h.client = ctl
	h.imageID = "00000000-0000-0000-0000-0000000000aa"
	w := httptest.NewRecorder()
	h.provision(w, httptest.NewRequest("POST", "/databases", strings.NewReader(body)), nil)
	return w, store
}

// addons:create postgres --disk 5G sends disk in the provider config.
func TestProvisionCustomDiskSetsVolumeSize(t *testing.T) {
	ctl := newSizeCtl(t, true)
	w, _ := provisionWithBody(t, ctl, `{"app":"web","runtime":"custom","cpu":500,"memory":536870912,"disk":`+strconv.FormatInt(fiveGiB, 10)+`}`)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	vols := ctl.lastVolumes(t)
	if len(vols) != 1 || vols[0].Path != postgres.VolumePath || vols[0].Size != fiveGiB {
		t.Fatalf("volumes %#v, want %s sized %d", vols, postgres.VolumePath, fiveGiB)
	}
}

// The dashboard sends only the runtime name.
func TestProvisionPublishedRuntimeSetsVolumeSize(t *testing.T) {
	ctl := newSizeCtl(t, false)
	medium, _ := ctl.cat.Find(dbruntime.EnginePostgres, "medium")
	w, _ := provisionWithBody(t, ctl, `{"app":"web","runtime":"medium"}`)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if vols := ctl.lastVolumes(t); len(vols) != 1 || vols[0].Size != medium.Disk {
		t.Fatalf("volumes %#v, want size %d", vols, medium.Disk)
	}
}

// The provider config comes from the tenant: a raw disk still needs
// allow-custom even when the CLI is bypassed.
func TestProvisionRejectsCustomDiskWhenDisabled(t *testing.T) {
	ctl := newSizeCtl(t, false)
	w, store := provisionWithBody(t, ctl, `{"app":"web","runtime":"small","disk":`+strconv.FormatInt(fiveGiB, 10)+`}`)
	if w.Code == 200 {
		t.Fatalf("custom disk accepted with custom sizes disabled: %s", w.Body.String())
	}
	if len(ctl.releases) != 0 {
		t.Fatalf("release created: %d", len(ctl.releases))
	}
	if n := len(store.All()); n != 0 {
		t.Fatalf("rejected provision left %d instances in the store", n)
	}
}

// A follower of a custom-sized leader has no published runtime to size
// from, so it gets the leader's volume size.
func TestStartFollowerInheritsLeaderVolumeSize(t *testing.T) {
	ctl := newSizeCtl(t, false)
	ctl.apps = append(ctl.apps, &ct.App{ID: "postgresql-leader-11111", Name: "postgresql-leader-11111"})
	lead := &ct.Release{ID: "lead", Processes: map[string]ct.ProcessType{
		postgres.ProcessName: {Volumes: []ct.VolumeReq{{Path: postgres.VolumePath, Size: fiveGiB}}},
	}}
	ctl.releases = append(ctl.releases, lead)
	ctl.current["postgresql-leader-11111"] = lead
	leader := &postgres.Instance{App: "postgresql-leader-11111", Runtime: "custom", ServiceHost: "leader.postgresql-leader-11111.discoverd"}
	fol := &postgres.Instance{App: "postgresql-follow-22222", Role: postgres.RoleFollower, LeaderID: leader.App, Runtime: "custom"}
	if err := startIsolatedInstance(ctl, "00000000-0000-0000-0000-0000000000aa", fol, leader, nil, func(string, time.Duration) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if vols := ctl.lastVolumes(t); len(vols) != 1 || vols[0].Size != fiveGiB {
		t.Fatalf("follower volumes %#v, want size %d", vols, fiveGiB)
	}
}
