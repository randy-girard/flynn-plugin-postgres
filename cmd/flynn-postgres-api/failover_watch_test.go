package main

import (
	"testing"

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
