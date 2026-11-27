package main

import (
	"strconv"
	"testing"
	"time"

	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
	"github.com/randy-girard/flynn/host/resource"
	"github.com/randy-girard/flynn/pkg/dbruntime"
)

const mib int64 = 1 << 20

func (f *sizeCtl) lastProcess(t *testing.T) ct.ProcessType {
	t.Helper()
	if len(f.releases) == 0 {
		t.Fatal("no release created")
	}
	return f.releases[len(f.releases)-1].Processes[postgres.ProcessName]
}

func (f *sizeCtl) lastEnv(t *testing.T) map[string]string {
	t.Helper()
	if len(f.releases) == 0 {
		t.Fatal("no release created")
	}
	return f.releases[len(f.releases)-1].Env
}

func procLimits(t *testing.T, p ct.ProcessType) (cpu, mem int64) {
	t.Helper()
	c, m := p.Resources[resource.TypeCPU], p.Resources[resource.TypeMemory]
	if c.Limit == nil || m.Limit == nil {
		t.Fatalf("process has no cpu/memory limits: %#v", p.Resources)
	}
	return *c.Limit, *m.Limit
}

// addons:create postgres --cpu 300 --memory 384MB sends runtime "custom"
// with cpu and memory; the instance job must get those limits (NEW-26).
func TestProvisionCustomCPUAndMemorySetProcessLimits(t *testing.T) {
	ctl := newSizeCtl(t, true)
	w, _ := provisionWithBody(t, ctl, `{"app":"web","runtime":"custom","cpu":300,"memory":`+strconv.FormatInt(384*mib, 10)+`,"disk":`+strconv.FormatInt(fiveGiB, 10)+`}`)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if cpu, mem := procLimits(t, ctl.lastProcess(t)); cpu != 300 || mem != 384*mib {
		t.Fatalf("limits cpu %d mem %d, want 300 / %d", cpu, mem, 384*mib)
	}
	env := ctl.lastEnv(t)
	if env[dbruntime.EnvSharedBuffers] != "96MB" || env[dbruntime.EnvEffectiveCacheSize] != "288MB" || env[dbruntime.EnvMemoryLimit] != strconv.FormatInt(384*mib, 10) {
		t.Fatalf("engine memory env %v", env)
	}
}

func TestProvisionPublishedRuntimeSetsPresetLimits(t *testing.T) {
	ctl := newSizeCtl(t, false)
	medium, _ := ctl.cat.Find(dbruntime.EnginePostgres, "medium")
	w, _ := provisionWithBody(t, ctl, `{"app":"web","runtime":"medium","cpu":`+strconv.FormatInt(medium.CPU, 10)+`,"memory":`+strconv.FormatInt(medium.Memory, 10)+`}`)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if cpu, mem := procLimits(t, ctl.lastProcess(t)); cpu != medium.CPU || mem != medium.Memory {
		t.Fatalf("limits cpu %d mem %d", cpu, mem)
	}
	if got := ctl.lastEnv(t)[dbruntime.EnvSharedBuffers]; got != "256MB" {
		t.Fatalf("shared_buffers %q", got)
	}
}

// The provider config comes from the tenant: custom cpu/memory still need
// allow-custom, and memory below the engine minimum is refused, instead of
// silently starting the job with the controller default.
func TestProvisionRejectsCustomCPUMemory(t *testing.T) {
	for name, tc := range map[string]struct {
		allow bool
		body  string
	}{
		"disabled":      {false, `{"app":"web","runtime":"custom","cpu":300,"memory":` + strconv.FormatInt(384*mib, 10) + `}`},
		"published cpu": {false, `{"app":"web","runtime":"small","cpu":4000}`},
		"below minimum": {true, `{"app":"web","runtime":"custom","cpu":300,"memory":` + strconv.FormatInt(64*mib, 10) + `,"disk":` + strconv.FormatInt(fiveGiB, 10) + `}`},
	} {
		ctl := newSizeCtl(t, tc.allow)
		w, store := provisionWithBody(t, ctl, tc.body)
		if w.Code != 400 {
			t.Fatalf("%s: status %d: %s", name, w.Code, w.Body.String())
		}
		if len(ctl.releases) != 0 || len(store.All()) != 0 {
			t.Fatalf("%s: rejected provision left %d releases, %d instances", name, len(ctl.releases), len(store.All()))
		}
	}
}

func customLeader(ctl *sizeCtl) *postgres.Instance {
	r := make(resource.Resources)
	resource.ApplyNamedLimitsWithReserve(r, 768*mib, 700, false)
	ctl.apps = append(ctl.apps, &ct.App{ID: "postgresql-leader-11111", Name: "postgresql-leader-11111"})
	lead := &ct.Release{ID: "lead", Processes: map[string]ct.ProcessType{
		postgres.ProcessName: {Resources: r, Volumes: []ct.VolumeReq{{Path: postgres.VolumePath, Size: fiveGiB}}},
	}}
	ctl.releases = append(ctl.releases, lead)
	ctl.current["postgresql-leader-11111"] = lead
	return &postgres.Instance{App: "postgresql-leader-11111", Runtime: "custom", ServiceHost: "leader.postgresql-leader-11111.discoverd"}
}

// A follower, upgrade copy or replacement of a custom leader names the
// unpublished "custom" runtime with no cpu/memory of its own; it keeps the
// leader's limits.
func TestStartFollowerInheritsCustomLeaderLimits(t *testing.T) {
	ctl := newSizeCtl(t, true)
	leader := customLeader(ctl)
	fol := &postgres.Instance{App: "postgresql-follow-22222", Role: postgres.RoleFollower, LeaderID: leader.App, Runtime: "custom"}
	if err := startIsolatedInstance(ctl, "00000000-0000-0000-0000-0000000000aa", fol, leader, nil, func(string, time.Duration) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if cpu, mem := procLimits(t, ctl.lastProcess(t)); cpu != 700 || mem != 768*mib {
		t.Fatalf("follower limits cpu %d mem %d", cpu, mem)
	}
	if got := ctl.lastEnv(t)[dbruntime.EnvSharedBuffers]; got != "192MB" {
		t.Fatalf("follower shared_buffers %q", got)
	}
}

// A follower that names its own published runtime keeps that runtime's limits.
func TestStartFollowerPublishedRuntimeKeepsItsLimits(t *testing.T) {
	ctl := newSizeCtl(t, true)
	leader := customLeader(ctl)
	small, _ := ctl.cat.Find(dbruntime.EnginePostgres, "small")
	fol := &postgres.Instance{App: "postgresql-follow-33333", Role: postgres.RoleFollower, LeaderID: leader.App, Runtime: "small"}
	if err := startIsolatedInstance(ctl, "00000000-0000-0000-0000-0000000000aa", fol, leader, nil, func(string, time.Duration) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if cpu, mem := procLimits(t, ctl.lastProcess(t)); cpu != small.CPU || mem != small.Memory {
		t.Fatalf("follower limits cpu %d mem %d", cpu, mem)
	}
}

// The store copies a custom leader's cpu and memory to a follower that
// inherits its runtime.
func TestProvisionFollowerInheritsLeaderCPUMemory(t *testing.T) {
	store := postgres.NewStore()
	lead, _, err := store.Provision(postgres.ProvisionRequest{Tenant: "web", App: "web", Runtime: "custom", CPU: 700, Memory: 768 * mib})
	if err != nil {
		t.Fatal(err)
	}
	if lead.CPU != 700 || lead.Memory != 768*mib {
		t.Fatalf("leader cpu %d mem %d", lead.CPU, lead.Memory)
	}
	fol, _, err := store.Provision(postgres.ProvisionRequest{Tenant: "web", App: "web", Follow: lead.ID})
	if err != nil {
		t.Fatal(err)
	}
	if fol.Runtime != "custom" || fol.CPU != 700 || fol.Memory != 768*mib {
		t.Fatalf("follower runtime %q cpu %d mem %d", fol.Runtime, fol.CPU, fol.Memory)
	}
}
