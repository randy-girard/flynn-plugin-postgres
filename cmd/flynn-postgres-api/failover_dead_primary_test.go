package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
)

// NEW-33: auto-failover with the primary's job gone. The watch promotes
// only a follower that replayed the WAL position the primary had when it was
// last seen running, through the verified promote (DATA-03): fence and stamp
// the old primary deposed first, restart the follower as the primary, check
// it is writable, then record it.

// killPrimary stops the leader the way a lost host does: no job left.
func killPrimary(rig *promoteRig, leader *postgres.Instance) {
	rig.ctl.mu.Lock()
	for id := range rig.ctl.scale[leader.App] {
		rig.ctl.scale[leader.App][id] = 0
	}
	rig.ctl.mu.Unlock()
	rig.node(leader.App).up = false
	rig.mu.Lock()
	rig.ops = nil
	rig.mu.Unlock()
}

// runOutage calls the watch every tick for d of observed downtime.
func runOutage(rig *promoteRig, fol *postgres.Instance, start time.Time, d time.Duration) *failoverDowntime {
	down := newFailoverDowntime()
	now := start
	rig.h.clock = func() time.Time { return now }
	for elapsed := time.Duration(0); elapsed <= d; elapsed += autoFailoverTick {
		now = start.Add(elapsed)
		rig.h.considerFailover(down, fol)
	}
	return down
}

func TestAutoFailoverPromotesCaughtUpFollowerOfDeadPrimary(t *testing.T) {
	rig, leader, fol := newPromoteRig(t, true)
	start := time.Now()
	rig.h.samples.record(leader.App, primarySample{WAL: 5000, At: start.Add(-3 * time.Second)})
	killPrimary(rig, leader)
	runOutage(rig, fol, start, autoFailoverGrace+autoFailoverTick)

	got, _ := rig.store.Get(fol.ID)
	if got.Role != postgres.RolePrimary || got.ServiceAlias != leader.App {
		t.Fatalf("follower %s alias %q, want promoted primary claiming %s; ops %v", got.Role, got.ServiceAlias, leader.App, rig.ops)
	}
	if rel := rig.ctl.currentRelease(t, fol.App); rel.Env["FLYNN_POSTGRES_ROLE"] != "primary" || rel.Env[postgres.EnvServiceAlias] != leader.App {
		t.Fatalf("promoted release env %v", rel.Env)
	}
	if rel := rig.ctl.currentRelease(t, leader.App); rel.Env["FLYNN_POSTGRES_ROLE"] != "deposed" {
		t.Fatalf("old primary release role %q, want deposed", rel.Env["FLYNN_POSTGRES_ROLE"])
	}
	if rig.node(leader.App).up {
		t.Fatal("old primary was started again")
	}
	deposed := rig.index("release " + leader.App + " role=deposed")
	restart := rig.index("release " + fol.App + " role=primary")
	probe := rig.index("sql " + fol.App + " probe")
	if deposed < 0 || restart < 0 || probe < 0 || !(deposed < restart && restart < probe) {
		t.Fatalf("want old primary stamped deposed < follower restarted primary < write probe; ops %v", rig.ops)
	}
	if rig.node(fol.App).recovery {
		t.Fatal("promoted node still in recovery")
	}
}

func TestAutoFailoverRefusesFollowerBehindPrimarysLastSighting(t *testing.T) {
	rig, leader, fol := newPromoteRig(t, true)
	start := time.Now()
	// The primary was at 6000 when last seen; the follower replayed 5000
	// and, with the primary gone, receives nothing more.
	rig.h.samples.record(leader.App, primarySample{WAL: 6000, At: start.Add(-2 * time.Second)})
	killPrimary(rig, leader)
	runOutage(rig, fol, start, 2*autoFailoverGrace)

	if got, _ := rig.store.Get(fol.ID); got.Role != postgres.RoleFollower {
		t.Fatalf("promoted a follower that is behind: %s", got.Role)
	}
	if ops := append(rig.opsWith("scale "), rig.opsWith("release ")...); len(ops) != 0 {
		t.Fatalf("refused failover changed jobs or releases: %v", ops)
	}
}

func TestAutoFailoverWaitsWithoutRecentPrimarySighting(t *testing.T) {
	for name, seed := range map[string]func(*promoteRig, *postgres.Instance, time.Time){
		"none": func(*promoteRig, *postgres.Instance, time.Time) {},
		"stale": func(rig *promoteRig, leader *postgres.Instance, start time.Time) {
			rig.h.samples.record(leader.App, primarySample{WAL: 5000, At: start.Add(-10 * time.Minute)})
		},
	} {
		t.Run(name, func(t *testing.T) {
			rig, leader, fol := newPromoteRig(t, true)
			start := time.Now()
			seed(rig, leader, start)
			killPrimary(rig, leader)
			runOutage(rig, fol, start, 2*autoFailoverGrace)
			if got, _ := rig.store.Get(fol.ID); got.Role != postgres.RoleFollower {
				t.Fatalf("promoted with no recent sighting of the primary: %s", got.Role)
			}
			if ops := rig.opsWith("release "); len(ops) != 0 {
				t.Fatalf("releases written: %v", ops)
			}
		})
	}
}

func TestAutoFailoverDoesNotPromoteWhenDeposedStampFails(t *testing.T) {
	rig, leader, fol := newPromoteRig(t, true)
	start := time.Now()
	rig.h.samples.record(leader.App, primarySample{WAL: 5000, At: start.Add(-time.Second)})
	killPrimary(rig, leader)
	rig.ctl.failRelease = map[string]error{leader.App: errors.New("controller unavailable")}
	runOutage(rig, fol, start, autoFailoverGrace+autoFailoverTick)

	if got, _ := rig.store.Get(fol.ID); got.Role != postgres.RoleFollower {
		t.Fatalf("promoted although the old primary could not be stamped deposed: %s", got.Role)
	}
	if ops := rig.opsWith("release " + fol.App + " role=primary"); len(ops) != 0 {
		t.Fatalf("follower restarted as primary with a non-deposed old primary: %v", rig.ops)
	}
	if rel := rig.ctl.currentRelease(t, fol.App); rel.Env["FLYNN_POSTGRES_ROLE"] != "follower" {
		t.Fatalf("follower release role %q", rel.Env["FLYNN_POSTGRES_ROLE"])
	}
}

func TestFailoverTickSamplesPrimaryOnEveryProcess(t *testing.T) {
	rig, leader, fol := newPromoteRig(t, true)
	rig.h.failoverLeader = func() bool { return false }
	now := time.Now()
	rig.h.clock = func() time.Time { return now }
	rig.h.failoverTick(newFailoverDowntime(), false)
	sample, ok := rig.h.samples.get(leader.App)
	if !ok || sample.WAL != 5000 || !sample.At.Equal(now) {
		t.Fatalf("sample %+v ok %v, want WAL 5000 at %v", sample, ok, now)
	}
	if got, _ := rig.store.Get(fol.ID); got.Role != postgres.RoleFollower {
		t.Fatalf("a process that is not the lease holder acted: %s", got.Role)
	}
}

func TestLeaseHolderAdoptsFailoverTargetFromController(t *testing.T) {
	rig, leader, fol := newPromoteRig(t, true)
	// This process never served the follower: only the leader is known.
	fresh := postgres.NewStore()
	l := *leader
	l.Followers = nil
	fresh.Adopt(&l)
	rig.h.store = fresh
	rig.store = fresh
	rig.ctl.listApps = true
	relEnv := rig.ctl.currentRelease(t, fol.App).Env
	relEnv["FLYNN_POSTGRES_USER"] = fol.AppUser
	relEnv["FLYNN_POSTGRES_PASSWORD"] = fol.AppPassword
	rig.h.failoverTick(newFailoverDowntime(), false)
	var found *postgres.Instance
	for _, f := range fresh.AutoFailoverFollowers() {
		if f.App == fol.App {
			found = f
		}
	}
	if found == nil {
		t.Fatalf("auto-failover follower %s not adopted from the controller", fol.App)
	}
	if found.LeaderID != leader.App && found.LeaderID != leader.ID {
		t.Fatalf("adopted follower leader %q", found.LeaderID)
	}
	if _, ok := rig.h.samples.get(leader.App); !ok {
		t.Fatal("primary of the adopted follower was not sampled")
	}
}

func TestHostFitsCacheExpiresAndNeverKeepsErrors(t *testing.T) {
	h := newHandler(postgres.NewStore())
	now := time.Now()
	h.clock = func() time.Time { return now }
	calls := 0
	fail := true
	h.hostFits = func() ([]postgres.HostFit, error) {
		calls++
		if fail {
			return nil, errors.New("controller down")
		}
		return []postgres.HostFit{{ID: "h1"}}, nil
	}
	if _, err := h.currentHostFits(); err == nil {
		t.Fatal("want the load error")
	}
	fail = false
	if hosts, err := h.currentHostFits(); err != nil || len(hosts) != 1 {
		t.Fatalf("error was cached: %v %v", hosts, err)
	}
	if calls != 2 {
		t.Fatalf("calls %d, want 2", calls)
	}
	if _, err := h.currentHostFits(); err != nil || calls != 2 {
		t.Fatalf("a fresh read was not reused within the TTL: calls %d err %v", calls, err)
	}
	now = now.Add(hostFitTTL + time.Second)
	if _, err := h.currentHostFits(); err != nil || calls != 3 {
		t.Fatalf("an old read was reused: calls %d err %v", calls, err)
	}
}

func TestCountHostJobsSkipsSystemAppsLikeTheScheduler(t *testing.T) {
	apps := []*ct.App{
		{ID: "router", Meta: map[string]string{"flynn-system-app": "true"}},
		{ID: "pg-sys", Meta: map[string]string{"flynn-system-app": "true", "flynn-datastore": "true"}},
		{ID: "tenant"},
	}
	jobs := []*ct.Job{
		{HostID: "h1", AppID: "router", State: ct.JobStateUp},
		{HostID: "h1", AppID: "router", State: ct.JobStateUp},
		{HostID: "h1", AppID: "pg-sys", State: ct.JobStateUp, VolumeIDs: []string{"v"}},
		{HostID: "h1", AppID: "tenant", State: ct.JobStateUp},
		{HostID: "h1", AppID: "tenant", State: ct.JobStateDown},
	}
	procs, singletons := countHostJobs(jobs, apps)
	if procs["h1"] != 2 || singletons["h1"] != 1 {
		t.Fatalf("procs %d singletons %d, want 2 and 1 (system router jobs do not count)", procs["h1"], singletons["h1"])
	}
}

func TestFollowerReplaceSkipsWhileLeaderDownOrFollowerStarting(t *testing.T) {
	leader := &postgres.Instance{ID: "l", App: "postgresql-a-00001", Role: postgres.RolePrimary}
	fol := &postgres.Instance{ID: "f", App: "postgresql-b-00002", Role: postgres.RoleFollower, LeaderID: "l"}
	if got := followerReplaceSkip(fol, leader, "follower", false, false, false, true); got != "leader down" {
		t.Fatalf("skip %q, want leader down", got)
	}
	h := newHandler(postgres.NewStore())
	now := time.Now()
	h.clock = func() time.Time { return now }
	created := now.Add(-30 * time.Second)
	h.jobs = func(string) ([]*ct.Job, error) {
		return []*ct.Job{{State: ct.JobStateUp, CreatedAt: &created}}, nil
	}
	if !h.followerJobStarting(fol) {
		t.Fatal("a 30s old follower job must count as starting")
	}
	now = now.Add(followerStartGrace)
	if h.followerJobStarting(fol) {
		t.Fatal("follower still starting after the grace")
	}
}

func TestReplacementReplicaOwnedByTenant(t *testing.T) {
	h := newHandler(postgres.NewStore())
	h.listAllResources = func() ([]*ct.Resource, error) {
		return []*ct.Resource{{
			ID:       "res-1",
			Env:      map[string]string{"FLYNN_POSTGRES": "postgresql-b-00002"},
			Apps:     []string{"other-app", "tenant-app"},
			OwnerApp: "tenant-app",
		}}, nil
	}
	// Loaded from its release: no tenant.
	primary := &postgres.Instance{ID: "postgresql-b-00002", App: "postgresql-b-00002", Role: postgres.RolePrimary}
	if got := h.replicaOwner(primary); got != "tenant-app" {
		t.Fatalf("owner %q, want the resource owner tenant-app", got)
	}
	primary.Tenant = "postgresql-b-00002"
	if got := h.replicaOwner(primary); got != "tenant-app" {
		t.Fatalf("owner %q: an instance app must never own the replica", got)
	}
	primary.Tenant = "shop"
	if got := h.replicaOwner(primary); got != "shop" {
		t.Fatalf("owner %q, want the stored tenant", got)
	}
}

func TestNoReplacementReplicaOnceReleaseNoLongerPending(t *testing.T) {
	rig, leader, _ := newPromoteRig(t, true)
	_ = rig.store.SetReplicaPending(leader.ID, true)
	// The leader has no auto-failover follower in this store, and its release
	// no longer says it needs one (a user destroyed the replica on the
	// other process).
	rig.store.Forget(rig.store.AutoFailoverFollowers()[0].ID)
	if err := rig.h.ensureAutoFailoverReplica(leader); err != nil {
		t.Fatalf("ensure: %v (it tried to provision a replica)", err)
	}
	if got, _ := rig.store.Get(leader.ID); got.ReplicaPending {
		t.Fatal("store still says the primary needs a replica")
	}
}

func TestDeprovisionAutoFailoverReplicaClearsPrimaryPending(t *testing.T) {
	rig, leader, fol := newPromoteRig(t, true)
	_ = rig.store.SetReplicaPending(leader.ID, true)
	_ = rig.h.stampIsolatedRole(mustGet(t, rig.store, leader.ID))
	if rel := rig.ctl.currentRelease(t, leader.App); rel.Env["FLYNN_POSTGRES_REPLICA_PENDING"] != "true" {
		t.Fatalf("setup: release env %v", rel.Env)
	}
	req := "/databases?id=" + fol.ID
	rec := rig.del(req)
	if rec.Code != 200 {
		t.Fatalf("deprovision %d %s", rec.Code, rec.Body.String())
	}
	if got := mustGet(t, rig.store, leader.ID); got.ReplicaPending {
		t.Fatal("store still wants a replica after the user destroyed it")
	}
	if rel := rig.ctl.currentRelease(t, leader.App); strings.TrimSpace(rel.Env["FLYNN_POSTGRES_REPLICA_PENDING"]) != "" {
		t.Fatalf("leader release still says pending: %v", rel.Env)
	}
}

func mustGet(t *testing.T, s *postgres.Store, id string) *postgres.Instance {
	t.Helper()
	inst, err := s.Get(id)
	if err != nil || inst == nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return inst
}

func (r *promoteRig) del(path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec
}

// downFor is a downtime record that has seen fol's primary down for d.
func downFor(id string, d time.Duration) *failoverDowntime {
	down := newFailoverDowntime()
	now := time.Now()
	down.since[id] = now.Add(-d)
	down.seen[id] = now
	return down
}

type deposedLister struct{ rels map[string]map[string]string }

func (d *deposedLister) AppList() ([]*ct.App, error) {
	var out []*ct.App
	for name := range d.rels {
		out = append(out, &ct.App{ID: name, Name: name})
	}
	return out, nil
}
func (d *deposedLister) GetAppRelease(id string) (*ct.Release, error) {
	return &ct.Release{ID: "rel-" + id, Env: d.rels[id]}, nil
}

// After a failover the deposed old primary's release names the promoted
// primary as FLYNN_POSTGRES_LEADER. It is not a follower and must not keep
// the promoted primary from being removed (seen live: "still has
// followers: <old primary>").
func TestDeposedOldPrimaryIsNotAFollower(t *testing.T) {
	promoted := &postgres.Instance{ID: "p", App: "postgresql-basin-00001", Role: postgres.RolePrimary}
	c := &deposedLister{rels: map[string]map[string]string{
		"postgresql-oxbow-00002": {"FLYNN_POSTGRES_ROLE": "deposed", "FLYNN_POSTGRES_LEADER": promoted.App},
		"postgresql-range-00003": {"FLYNN_POSTGRES_ROLE": "follower", "FLYNN_POSTGRES_LEADER": promoted.App},
	}}
	names, err := liveFollowerAppNames(c, promoted)
	if err != nil || len(names) != 1 || names[0] != "postgresql-range-00003" {
		t.Fatalf("followers %v err %v, want only the real follower", names, err)
	}
}
