package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
)

type fakeListReleaser struct {
	apps []*ct.App
	rels map[string]*ct.Release
}

func (f *fakeListReleaser) AppList() ([]*ct.App, error) {
	return f.apps, nil
}

func (f *fakeListReleaser) GetAppRelease(id string) (*ct.Release, error) {
	if f.rels == nil {
		return nil, errors.New("not found")
	}
	if rel := f.rels[id]; rel != nil {
		return rel, nil
	}
	return nil, errors.New("not found")
}

func TestAdoptLiveFollowersKeepsFollowerAppWhenFlynnPostgresPointsAtLeader(t *testing.T) {
	s := postgres.NewStore()
	leader := s.Adopt(&postgres.Instance{
		ID:   "postgresql-basin-73690",
		App:  "postgresql-basin-73690",
		Role: postgres.RolePrimary,
	})
	if leader == nil {
		t.Fatal("adopt leader")
	}
	c := &fakeListReleaser{
		apps: []*ct.App{
			{ID: "basin-id", Name: "postgresql-basin-73690"},
			{ID: "valley-id", Name: "postgresql-valley-30620"},
		},
		rels: map[string]*ct.Release{
			"basin-id": {Env: map[string]string{
				"FLYNN_POSTGRES": "postgresql-basin-73690",
				"POSTGRES_ROLE":  "primary",
			}},
			"valley-id": {Env: map[string]string{
				"FLYNN_POSTGRES":  "postgresql-basin-73690",
				"POSTGRES_ROLE":   "follower",
				"POSTGRES_LEADER": "postgresql-basin-73690",
			}},
		},
	}
	adoptLiveFollowers(s, c, leader)
	fol, err := s.Get("postgresql-valley-30620")
	if err != nil {
		t.Fatal(err)
	}
	if fol.App != "postgresql-valley-30620" {
		t.Fatalf("follower app %s (FLYNN_POSTGRES must not steal the isolated app name)", fol.App)
	}
	if fol.Role != postgres.RoleFollower || fol.LeaderID != leader.App {
		t.Fatalf("follower: %+v", fol)
	}
}

func TestRecreateFollowerRestoresAutoFailoverWhenStartFails(t *testing.T) {
	s := postgres.NewStore()
	leader, _, err := s.Provision(postgres.ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	fol, _, err := s.Provision(postgres.ProvisionRequest{App: "shop", Follow: leader.ID, AutoFailover: true})
	if err != nil {
		t.Fatal(err)
	}
	h := &handler{store: s}
	if err := h.recreateFollower(fol, leader); err == nil {
		t.Fatal("start without controller must fail")
	}
	got, err := s.Get(fol.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.AutoFailover {
		t.Fatal("failed recreate must restore auto-failover on the old replica")
	}
	for _, inst := range s.All() {
		if inst.Role == postgres.RoleFollower && inst.ID != fol.ID {
			t.Fatalf("leftover replacement follower %s", inst.App)
		}
	}
}

func TestSkipFollowerReplaceLeaderMissing(t *testing.T) {
	s := postgres.NewStore()
	fol := s.Adopt(&postgres.Instance{
		ID:       "postgresql-fjord-67828",
		App:      "postgresql-fjord-67828",
		Role:     postgres.RoleFollower,
		LeaderID: "postgresql-valley-30620",
	})
	h := &handler{store: s}
	if got := h.skipFollowerReplace(fol); got != "leader missing" {
		t.Fatalf("skip %q", got)
	}
}

func TestSkipFollowerReplaceLeaderDeposed(t *testing.T) {
	s := postgres.NewStore()
	leader := s.Adopt(&postgres.Instance{
		ID:   "postgresql-valley-30620",
		App:  "postgresql-valley-30620",
		Role: postgres.RoleDeposed,
	})
	fol := s.Adopt(&postgres.Instance{
		ID:       "postgresql-fjord-67828",
		App:      "postgresql-fjord-67828",
		Role:     postgres.RoleFollower,
		LeaderID: leader.App,
	})
	h := &handler{store: s}
	if got := h.skipFollowerReplace(fol); got != "leader not primary" {
		t.Fatalf("skip %q", got)
	}
}

func TestLivePostgresRoleReadsRelease(t *testing.T) {
	c := &fakeListReleaser{rels: map[string]*ct.Release{
		"postgresql-prairie-69929": {Env: map[string]string{"POSTGRES_ROLE": "primary"}},
	}}
	if got := livePostgresRole(c, "postgresql-prairie-69929"); got != "primary" {
		t.Fatalf("role %q", got)
	}
	if got := livePostgresRole(c, "missing"); got != "" {
		t.Fatalf("missing %q", got)
	}
}

func TestFollowerReplaceSkipLivePrimaryNotTornDown(t *testing.T) {
	// Worker B still has prairie as RoleFollower of valley after worker A promoted it.
	fol := &postgres.Instance{
		App:      "postgresql-prairie-69929",
		Role:     postgres.RoleFollower,
		LeaderID: "postgresql-valley-30620",
	}
	valley := &postgres.Instance{App: "postgresql-valley-30620", Role: postgres.RolePrimary}
	if got := followerReplaceSkip(fol, valley, "primary", false, false, false); got != "live role primary" {
		t.Fatalf("promoted primary must not be replaced: %q", got)
	}
}

func TestFollowerReplaceSkipAllowsWritableReplicaOfLivePrimary(t *testing.T) {
	fol := &postgres.Instance{
		App:      "postgresql-valley-30620",
		Role:     postgres.RoleFollower,
		LeaderID: "postgresql-basin-73690",
	}
	leader := &postgres.Instance{App: "postgresql-basin-73690", Role: postgres.RolePrimary}
	if got := followerReplaceSkip(fol, leader, "follower", false, false, false); got != "" {
		t.Fatalf("writable leftover replica of a live primary should be replaced: %q", got)
	}
}

func TestFollowerReplaceSkipImageSwapAndGoneLeader(t *testing.T) {
	fol := &postgres.Instance{
		App:      "postgresql-fjord-67828",
		Role:     postgres.RoleFollower,
		LeaderID: "postgresql-valley-30620",
	}
	leader := &postgres.Instance{App: "postgresql-valley-30620", Role: postgres.RolePrimary}
	if got := followerReplaceSkip(fol, leader, "follower", false, true, false); got != "leader image swap" {
		t.Fatalf("swap: %q", got)
	}
	if got := followerReplaceSkip(fol, leader, "follower", true, false, false); got != "upgrade in progress" {
		t.Fatalf("upgrade: %q", got)
	}
	if got := followerReplaceSkip(fol, leader, "follower", false, false, true); got != "leader app gone" {
		t.Fatalf("gone: %q", got)
	}
	if got := followerReplaceSkip(nil, leader, "", false, false, false); got != "not a follower" {
		t.Fatalf("nil: %q", got)
	}
}

func TestImageSwapClaimHeld(t *testing.T) {
	f := &fakeAppMeta{apps: []*ct.App{
		{Name: "postgresql-valley-30620", Meta: map[string]string{imageSwapMetaKey: "job-a"}},
		{Name: "postgresql-upland-22935", Meta: map[string]string{}},
	}}
	if !imageSwapClaimHeld(f, "postgresql-valley-30620") {
		t.Fatal("claimed swap must block follower replace")
	}
	if imageSwapClaimHeld(f, "postgresql-upland-22935") {
		t.Fatal("empty meta is not a claim")
	}
	if imageSwapClaimHeld(f, "missing") {
		t.Fatal("missing app is not a claim")
	}
}

func TestReplicaWaitingForPrimaryMessage(t *testing.T) {
	got := replicaWaitingForPrimary(&postgres.ReplicaProgress{}, "postgresql-valley-30620")
	if got.Ready || !got.Available || got.Percent != 90 {
		t.Fatalf("%+v", got)
	}
	if !strings.Contains(got.Message, "unreachable") || !strings.Contains(got.Message, "postgresql-valley-30620") {
		t.Fatalf("message %q", got.Message)
	}
}
