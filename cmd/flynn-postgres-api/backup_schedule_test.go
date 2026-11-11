package main

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
)

func TestPlanBackupPassSpreadsHostsAndSkipsFollowers(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	resources := []*ct.Resource{
		{ID: "b", Env: map[string]string{"FLYNN_POSTGRES_ROLE": "primary", "FLYNN_POSTGRES": "pg-b"}},
		{ID: "a", Env: map[string]string{"FLYNN_POSTGRES_ROLE": "primary", "FLYNN_POSTGRES": "pg-a"}},
		{ID: "c", Env: map[string]string{"FLYNN_POSTGRES": "pg-c"}},
		{ID: "f", Env: map[string]string{"FLYNN_POSTGRES_ROLE": "follower"}},
		{ID: "d", Env: map[string]string{"POSTGRES_ROLE": "deposed"}},
	}
	hosts := map[string]string{"a": "host-a", "b": "host-a", "c": "host-b"}
	got := planBackupPass(now, resources, nil, map[string]bool{"b": true}, func(res *ct.Resource) string {
		return hosts[res.ID]
	})
	if strings.Join(got, ",") != "c" {
		t.Fatalf("host-a is full, admitted %v", got)
	}
	open := planBackupPass(now, resources[:2], nil, nil, func(res *ct.Resource) string { return "host-a" })
	if strings.Join(open, ",") != "a" {
		t.Fatalf("cap admitted %v", open)
	}
	fresh := now.Add(-time.Hour)
	notDue := planBackupPass(now, resources[:1], map[string]postgres.BackupStatus{
		"b": {LastSuccessAt: fresh},
	}, nil, func(*ct.Resource) string { return "host-a" })
	if len(notDue) != 0 {
		t.Fatalf("fresh success was due: %v", notDue)
	}
}

func TestRunBackupPassSkipsFollowersOfTheLease(t *testing.T) {
	h := newHandler(postgres.NewStore())
	rec := &recordingBackups{}
	h.backupAPI = rec
	h.backupLeader = func() bool { return false }
	h.backupResources = func() ([]*ct.Resource, error) {
		return []*ct.Resource{{ID: "a", Env: map[string]string{"FLYNN_POSTGRES_ROLE": "primary"}}}, nil
	}
	h.backupStatuses = func() (map[string]postgres.BackupStatus, error) {
		return map[string]postgres.BackupStatus{}, nil
	}
	h.runBackupPass(time.Now())
	if got := rec.list(); len(got) != 0 {
		t.Fatalf("follower of the lease dumped: %v", got)
	}
	h.backupLeader = func() bool { return true }
	h.runBackupPass(time.Now())
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && strings.Join(rec.list(), ",") != "a" {
		time.Sleep(10 * time.Millisecond)
	}
	if strings.Join(rec.list(), ",") != "a" {
		t.Fatalf("leader created %v", rec.list())
	}
}

type recordingBackups struct {
	mu      sync.Mutex
	created []string
}

func (r *recordingBackups) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.created...)
}

func (r *recordingBackups) View(string) (backupView, error) { return backupView{}, nil }
func (r *recordingBackups) Create(ref string) (postgres.BackupResult, error) {
	r.mu.Lock()
	r.created = append(r.created, ref)
	r.mu.Unlock()
	return postgres.BackupResult{Timestamp: "20261111T120000Z"}, nil
}
func (r *recordingBackups) Restore(_, timestamp string) error {
	if _, err := postgres.ParseBackupTimestamp(timestamp); err != nil {
		return postgres.ErrBackupNotListed
	}
	return nil
}
