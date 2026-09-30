package postgres

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestUpgradeFollowsWaitsAndPromotes(t *testing.T) {
	s := NewStore()
	leader, env, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	before := env["DATABASE_URL"]
	res, err := s.Upgrade(context.Background(), leader.ID, UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Follower == nil || res.Promoted == nil || res.PreviousLeader == nil {
		t.Fatalf("result: %+v", res)
	}
	if res.Promoted.ReadOnly || res.Promoted.Role != RolePrimary {
		t.Fatalf("promoted: %+v", res.Promoted)
	}
	if res.PreviousLeader.App != leader.App {
		t.Fatal("old leader resource was removed")
	}
	rewritten, err := s.EnvForApp(leader.ID, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if rewritten["DATABASE_URL"] == before || rewritten["DATABASE_URL"] == "" {
		t.Fatalf("primary URL not rewritten: %#v", rewritten)
	}
}

func TestUpgradeRejectsFollowerAndOverlap(t *testing.T) {
	s := NewStore()
	leader, _, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	fol, _, err := s.Provision(ProvisionRequest{App: "shop", Follow: leader.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Upgrade(context.Background(), fol.ID, UpgradeOptions{}); !errors.Is(err, ErrNotPrimary) {
		t.Fatalf("follower upgrade: %v", err)
	}
	block := make(chan struct{})
	opts := UpgradeOptions{
		AfterFollow: func(*Instance) error {
			<-block
			return nil
		},
	}
	task, err := s.StartUpgrade(leader.ID, opts)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != TaskPending && task.Status != TaskFollowing && task.Status != TaskWaiting {
		t.Fatalf("status %s", task.Status)
	}
	if _, err := s.StartUpgrade(leader.ID, UpgradeOptions{}); !errors.Is(err, ErrUpgradeInProgress) {
		t.Fatalf("overlap: %v", err)
	}
	close(block)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := s.GetTask(task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == TaskDone {
			return
		}
		if got.Status == TaskFailed {
			t.Fatalf("task failed: %s", got.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("background upgrade did not finish")
}

func TestUpgradeWaitsForLag(t *testing.T) {
	s := NewStore()
	leader, _, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan string, 1)
	errc := make(chan error, 1)
	go func() {
		_, err := s.Upgrade(context.Background(), leader.ID, UpgradeOptions{
			AfterFollow: func(fol *Instance) error {
				if err := s.SetLag(fol.ID, 99); err != nil {
					return err
				}
				started <- fol.ID
				return nil
			},
		})
		errc <- err
	}()
	var folID string
	select {
	case folID = <-started:
	case <-time.After(time.Second):
		t.Fatal("follower not created")
	}
	select {
	case err := <-errc:
		t.Fatalf("upgrade returned before lag zero: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := s.SetLag(folID, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("upgrade did not finish after lag zero")
	}
}

func TestUpgradeRecreatesFollowersOnNewPrimary(t *testing.T) {
	s := NewStore()
	leader, _, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	oldFol, _, err := s.Provision(ProvisionRequest{App: "shop", Follow: leader.ID})
	if err != nil {
		t.Fatal(err)
	}
	oldFolID := oldFol.ID
	oldFolApp := oldFol.App
	dropped := []string{}
	res, err := s.Upgrade(context.Background(), leader.ID, UpgradeOptions{
		AfterDrop: func(old *Instance) error {
			dropped = append(dropped, old.ID)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Promoted == nil || res.Promoted.ID == leader.ID {
		t.Fatalf("promoted: %+v", res.Promoted)
	}
	if len(res.ReplacedFollowers) != 1 {
		t.Fatalf("replaced=%d %+v", len(res.ReplacedFollowers), res.ReplacedFollowers)
	}
	got := res.ReplacedFollowers[0]
	if got.Old.ID != oldFolID || got.New == nil || got.New.Role != RoleFollower {
		t.Fatalf("replace: %+v", got)
	}
	if got.New.LeaderID != res.Promoted.ID {
		t.Fatalf("new follower leader %s want %s", got.New.LeaderID, res.Promoted.ID)
	}
	if _, err := s.Get(oldFolID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old follower still in store: %v", err)
	}
	if len(dropped) != 1 || dropped[0] != oldFolID {
		t.Fatalf("dropped=%v want %s", dropped, oldFolID)
	}
	if len(res.Dropped) != 1 || res.Dropped[0] != oldFolApp {
		t.Fatalf("dropped apps=%v", res.Dropped)
	}
	names := s.FollowerApps(res.Promoted.ID)
	if len(names) != 1 || names[0] != got.New.App {
		t.Fatalf("new primary followers=%v", names)
	}
}
