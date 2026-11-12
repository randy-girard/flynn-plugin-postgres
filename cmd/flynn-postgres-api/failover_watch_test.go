package main

import (
	"errors"
	"testing"
	"time"

	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
)

type fakeJobClient struct {
	jobs      map[string][]*ct.Job
	active    []*ct.Job
	listErr   error
	activeErr error
}

func (f *fakeJobClient) JobList(app string) ([]*ct.Job, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.jobs == nil {
		return nil, nil
	}
	return f.jobs[app], nil
}

func (f *fakeJobClient) JobListActive() ([]*ct.Job, error) {
	if f.activeErr != nil {
		return nil, f.activeErr
	}
	return f.active, nil
}

func TestRunningAppHostIgnoresDownJobs(t *testing.T) {
	c := &fakeJobClient{jobs: map[string][]*ct.Job{
		"pg-a": {
			{HostID: "h1", State: ct.JobStateDown},
			{HostID: "h2", State: ct.JobStateUp},
		},
	}}
	host, err := runningAppHost(c, "pg-a")
	if err != nil || host != "h2" {
		t.Fatalf("host %q err %v", host, err)
	}
	if appJobRunning(c, "missing") {
		t.Fatal("missing app must not look running")
	}
}

func TestClusterHasOtherHost(t *testing.T) {
	c := &fakeJobClient{active: []*ct.Job{
		{HostID: "h1", State: ct.JobStateUp},
		{HostID: "h2", State: ct.JobStateUp},
	}}
	if !clusterHasOtherHost(c, "h1") {
		t.Fatal("expected another host")
	}
	solo := &fakeJobClient{active: []*ct.Job{{HostID: "h1", State: ct.JobStateUp}}}
	if clusterHasOtherHost(solo, "h1") {
		t.Fatal("only the avoided host is live")
	}
}

func stubPromotable(t *testing.T) {
	t.Helper()
	prevRec, prevWAL, prevBackup := readReplicaRecovery, readPrimaryWAL, readBasebackupActive
	readReplicaRecovery = func(string) (bool, int64, bool) { return true, 10, true }
	readPrimaryWAL = func(string) (int64, bool) { return 10, true }
	readBasebackupActive = func(string) (bool, bool) { return false, true }
	t.Cleanup(func() {
		readReplicaRecovery, readPrimaryWAL, readBasebackupActive = prevRec, prevWAL, prevBackup
	})
}

func TestConsiderFailoverFencesBeforePromoteAndKeepsURL(t *testing.T) {
	stubPromotable(t)
	s := postgres.NewStore()
	leader, _, err := s.Provision(postgres.ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.EnvForApp(leader.ID, "shop")
	if err != nil {
		t.Fatal(err)
	}
	fol, _, err := s.Provision(postgres.ProvisionRequest{Follow: leader.ID, PlatformReplica: true})
	if err != nil {
		t.Fatal(err)
	}
	h := newHandler(s)
	scaled := ""
	h.jobs = func(string) ([]*ct.Job, error) { return nil, nil }
	h.getRelease = func(string) (*ct.Release, error) { return &ct.Release{ID: "rel-running"}, nil }
	h.scaleRelease = func(_, release string, opts ct.ScaleOptions) error {
		if opts.NoWait {
			t.Fatal("fence must wait for the job to stop")
		}
		scaled = release
		return nil
	}
	h.considerFailover(map[string]time.Time{fol.ID: time.Now().Add(-time.Minute)}, fol)
	if scaled != "rel-running" {
		t.Fatalf("scaled %q", scaled)
	}
	got, err := s.Get(fol.ID)
	if err != nil || got.Role != postgres.RolePrimary || got.ServiceAlias != leader.App {
		t.Fatalf("promoted %+v err %v", got, err)
	}
	after, err := s.EnvForApp(leader.ID, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if after["DATABASE_URL"] != before["DATABASE_URL"] {
		t.Fatalf("DATABASE_URL changed\n%s\n%s", before["DATABASE_URL"], after["DATABASE_URL"])
	}
}

func TestJobListErrorDoesNotPromote(t *testing.T) {
	stubPromotable(t)
	s := postgres.NewStore()
	leader, _, err := s.Provision(postgres.ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	fol, _, err := s.Provision(postgres.ProvisionRequest{Follow: leader.ID, PlatformReplica: true})
	if err != nil {
		t.Fatal(err)
	}
	h := newHandler(s)
	h.jobs = func(string) ([]*ct.Job, error) { return nil, errors.New("controller") }
	h.considerFailover(map[string]time.Time{fol.ID: time.Now().Add(-time.Minute)}, fol)
	got, _ := s.Get(fol.ID)
	if got.Role != postgres.RoleFollower {
		t.Fatalf("role %s", got.Role)
	}
}

func TestFenceFailureDoesNotPromote(t *testing.T) {
	stubPromotable(t)
	s := postgres.NewStore()
	leader, _, err := s.Provision(postgres.ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	fol, _, err := s.Provision(postgres.ProvisionRequest{Follow: leader.ID, PlatformReplica: true})
	if err != nil {
		t.Fatal(err)
	}
	h := newHandler(s)
	h.jobs = func(string) ([]*ct.Job, error) {
		return []*ct.Job{{ReleaseID: "rel-running", State: ct.JobStateUp}}, nil
	}
	h.scaleRelease = func(string, string, ct.ScaleOptions) error { return errors.New("host down") }
	h.considerFailover(map[string]time.Time{fol.ID: time.Now().Add(-time.Minute)}, fol)
	got, _ := s.Get(fol.ID)
	if got.Role != postgres.RoleFollower {
		t.Fatalf("role %s", got.Role)
	}
}

func TestSystemWatchIgnoresUserFollower(t *testing.T) {
	stubPromotable(t)
	s := postgres.NewStore()
	leader, _, err := s.Provision(postgres.ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	fol, _, err := s.Provision(postgres.ProvisionRequest{Follow: leader.ID, AutoFailover: true})
	if err != nil {
		t.Fatal(err)
	}
	h := newHandler(s)
	h.availability = func() (string, error) { return postgres.AvailabilitySystem, nil }
	h.jobs = func(string) ([]*ct.Job, error) { return nil, nil }
	h.getRelease = func(string) (*ct.Release, error) { return &ct.Release{ID: "rel"}, nil }
	h.scaleRelease = func(string, string, ct.ScaleOptions) error { return nil }
	h.hostFits = func() ([]postgres.HostFit, error) { return nil, errors.New("no hosts") }
	h.reconcileAutoFailover(map[string]time.Time{fol.ID: time.Now().Add(-time.Minute)})
	got, _ := s.Get(fol.ID)
	if got.Role != postgres.RoleFollower {
		t.Fatalf("user follower was promoted: %s", got.Role)
	}
}

func TestManualWatchIgnoresPlatformReplica(t *testing.T) {
	stubPromotable(t)
	s := postgres.NewStore()
	leader, _, err := s.Provision(postgres.ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	fol, _, err := s.Provision(postgres.ProvisionRequest{Follow: leader.ID, PlatformReplica: true})
	if err != nil {
		t.Fatal(err)
	}
	h := newHandler(s)
	h.availability = func() (string, error) { return postgres.AvailabilityManual, nil }
	h.jobs = func(string) ([]*ct.Job, error) { return nil, nil }
	h.getRelease = func(string) (*ct.Release, error) { return &ct.Release{ID: "rel"}, nil }
	h.scaleRelease = func(string, string, ct.ScaleOptions) error { return nil }
	h.reconcileAutoFailover(map[string]time.Time{fol.ID: time.Now().Add(-time.Minute)})
	got, _ := s.Get(fol.ID)
	if got.Role != postgres.RoleFollower {
		t.Fatalf("platform replica was promoted in manual mode: %s", got.Role)
	}
}

func TestFailoverLeaseFollowerDoesNotReconcile(t *testing.T) {
	stubPromotable(t)
	s := postgres.NewStore()
	leader, _, err := s.Provision(postgres.ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	fol, _, err := s.Provision(postgres.ProvisionRequest{Follow: leader.ID, AutoFailover: true})
	if err != nil {
		t.Fatal(err)
	}
	h := newHandler(s)
	h.failoverLeader = func() bool { return false }
	h.availability = func() (string, error) { return postgres.AvailabilityManual, nil }
	h.jobs = func(string) ([]*ct.Job, error) { return nil, nil }
	h.reconcileAutoFailover(map[string]time.Time{fol.ID: time.Now().Add(-time.Minute)})
	got, _ := s.Get(fol.ID)
	if got.Role != postgres.RoleFollower {
		t.Fatalf("non-leader promoted: %s", got.Role)
	}
}
