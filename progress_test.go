package postgres

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestStoredProgressTracksBasebackupThenLag(t *testing.T) {
	s := NewStore()
	leader, _, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	fol, _, err := s.Provision(ProvisionRequest{App: "shop", Follow: leader.ID})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := s.Progress(fol.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ready.Ready || ready.Percent != 100 {
		t.Fatalf("new in-memory follower should be ready: %+v", ready)
	}
	if err := s.SetBackupProgress(fol.ID, 40, 100); err != nil {
		t.Fatal(err)
	}
	p, err := s.Progress(fol.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Phase != PhaseBasebackup || p.Ready || p.Percent != 36 {
		t.Fatalf("basebackup: %+v", p)
	}
	if !strings.Contains(p.Message, "40 B") || !strings.Contains(p.Message, "36%") {
		t.Fatalf("message %q", p.Message)
	}
	if err := s.SetBackupProgress(fol.ID, 100, 100); err != nil {
		t.Fatal(err)
	}
	if err := s.SetLag(fol.ID, 80); err != nil {
		t.Fatal(err)
	}
	p, err = s.Progress(fol.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Phase != PhaseStreaming || p.Ready || p.Percent < 90 {
		t.Fatalf("streaming: %+v", p)
	}
	if err := s.SetLag(fol.ID, 0); err != nil {
		t.Fatal(err)
	}
	p, err = s.Progress(fol.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Ready || p.Phase != PhaseReady || p.Percent != 100 {
		t.Fatalf("ready: %+v", p)
	}
}

func TestWaitBlocksOnBasebackupProgress(t *testing.T) {
	s := NewStore()
	leader, _, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	fol, _, err := s.Provision(ProvisionRequest{App: "shop", Follow: leader.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetBackupProgress(fol.ID, 1, 100); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- s.Wait(context.Background(), fol.ID)
	}()
	time.Sleep(20 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("wait returned during basebackup: %v", err)
	default:
	}
	if err := s.SetBackupProgress(fol.ID, 100, 100); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("wait did not return after backup finished")
	}
}

func TestLiveProgressOverridesStoredLag(t *testing.T) {
	s := NewStore()
	leader, _, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	fol, _, err := s.Provision(ProvisionRequest{App: "shop", Follow: leader.ID})
	if err != nil {
		t.Fatal(err)
	}
	s.SetLiveProgress(func(inst *Instance) (*ReplicaProgress, error) {
		if inst.ID != fol.ID {
			t.Fatalf("unexpected inst %s", inst.ID)
		}
		return &ReplicaProgress{
			Phase:       PhaseBasebackup,
			Percent:     42,
			BytesCopied: 42,
			BytesTotal:  100,
		}, nil
	})
	p, err := s.Progress(fol.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Phase != PhaseBasebackup || p.Percent != 42 || p.Ready {
		t.Fatalf("%+v", p)
	}
	if !strings.Contains(p.Message, "42%") {
		t.Fatalf("message %q", p.Message)
	}
}

func TestFormatProgressAndBytes(t *testing.T) {
	if got := FormatBytes(1500); got != "1.5 KB" {
		t.Fatalf("bytes %s", got)
	}
	line := FormatProgress(ReplicaProgress{Phase: PhaseBasebackup, Percent: 12, BytesCopied: 1200, BytesTotal: 4096})
	if !strings.Contains(line, "12%") || !strings.Contains(line, "basebackup") {
		t.Fatalf("%s", line)
	}
	if got := FormatProgress(ReplicaProgress{Phase: PhaseReady, Follower: "postgresql-meadow-46110"}); got != "ready postgresql-meadow-46110" {
		t.Fatalf("%s", got)
	}
}

func TestUpgradeTaskIncludesFollowerProgress(t *testing.T) {
	s := NewStore()
	leader, _, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan string, 1)
	task, err := s.StartUpgrade(leader.ID, UpgradeOptions{
		AfterFollow: func(fol *Instance) error {
			if err := s.SetBackupProgress(fol.ID, 20, 100); err != nil {
				return err
			}
			started <- fol.ID
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var folID string
	select {
	case folID = <-started:
	case <-time.After(time.Second):
		t.Fatal("follower not created")
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Progress == nil || got.Progress.Phase != PhaseBasebackup {
		t.Fatalf("task %+v", got)
	}
	if got.Progress.Follower == "" && folID == "" {
		t.Fatal("missing follower")
	}
	if err := s.SetBackupProgress(folID, 100, 100); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		cur, err := s.GetTask(task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if cur.Status == TaskDone {
			return
		}
		if cur.Status == TaskFailed {
			t.Fatalf("task failed: %s", cur.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("upgrade did not finish")
}
