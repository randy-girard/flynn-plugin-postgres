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
	beforeKey, before := firstAppURL(env)
	if before == "" {
		t.Fatalf("leader env: %#v", env)
	}
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
	if rewritten[beforeKey] == before || rewritten[beforeKey] == "" {
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

func TestUpgradeAfterPromoteAndDropPrevious(t *testing.T) {
	s := NewStore()
	leader, _, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	oldApp := leader.App
	var promotedID string
	res, err := s.Upgrade(context.Background(), leader.ID, UpgradeOptions{
		Mode: ModeStreaming,
		AfterPromote: func(p *PromoteResult) error {
			if p == nil || p.Promoted == nil || p.PreviousLeader == nil {
				t.Fatalf("promote result: %+v", p)
			}
			promotedID = p.Promoted.ID
			if p.PreviousLeader.Role != RoleDeposed {
				t.Fatalf("drop-previous must depose old primary: %+v", p.PreviousLeader)
			}
			return nil
		},
		AfterDrop: func(old *Instance) error {
			if old == nil || old.App != oldApp {
				t.Fatalf("drop %v want %s", old, oldApp)
			}
			return nil
		},
		DropPrevious: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Promoted == nil || res.Promoted.ID != promotedID {
		t.Fatalf("promoted %+v", res.Promoted)
	}
	if _, err := s.Get(leader.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old primary still in store: %v", err)
	}
	found := false
	for _, app := range res.Dropped {
		if app == oldApp {
			found = true
		}
	}
	if !found {
		t.Fatalf("dropped=%v want %s", res.Dropped, oldApp)
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

func TestUpgradeAfterReplaceRewritesThenDropsOldFollower(t *testing.T) {
	s := NewStore()
	leader, _, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	oldFol, _, err := s.Provision(ProvisionRequest{App: "shop", Follow: leader.ID})
	if err != nil {
		t.Fatal(err)
	}
	var replacedOld, replacedNew string
	dropped := []string{}
	res, err := s.Upgrade(context.Background(), leader.ID, UpgradeOptions{
		AfterReplace: func(old, nf *Instance) error {
			replacedOld, replacedNew = old.ID, nf.ID
			return nil
		},
		AfterDrop: func(old *Instance) error {
			dropped = append(dropped, old.ID)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if replacedOld != oldFol.ID || replacedNew == "" || replacedNew == oldFol.ID {
		t.Fatalf("replace old=%s new=%s want old %s", replacedOld, replacedNew, oldFol.ID)
	}
	if len(dropped) != 0 {
		t.Fatalf("AfterReplace must own the old follower drop: %v", dropped)
	}
	if len(res.ReplacedFollowers) != 1 || res.ReplacedFollowers[0].New.ID != replacedNew {
		t.Fatalf("replaced=%+v", res.ReplacedFollowers)
	}
	if _, err := s.Get(oldFol.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old follower still in store: %v", err)
	}
}

func TestUpgradeReplacesAdoptedLiveFollower(t *testing.T) {
	s := NewStore()
	leader, _, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	live := &Instance{
		ID:           "postgresql-valley-30620",
		App:          "postgresql-valley-30620",
		Role:         RoleFollower,
		LeaderID:     leader.App,
		AutoFailover: true,
		ReadOnly:     true,
		Mode:         ModeStreaming,
	}
	if got := s.Adopt(live); got == nil || got.App != live.App {
		t.Fatalf("adopt: %+v", got)
	}
	res, err := s.Upgrade(context.Background(), leader.ID, UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ReplacedFollowers) != 1 {
		t.Fatalf("replaced=%d %+v", len(res.ReplacedFollowers), res.ReplacedFollowers)
	}
	got := res.ReplacedFollowers[0]
	if got.Old.App != live.App || got.New == nil || got.New.LeaderID != res.Promoted.ID {
		t.Fatalf("replace: %+v promoted=%s", got, res.Promoted.ID)
	}
	if _, err := s.Get(live.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old live follower still in store: %v", err)
	}
	if !got.New.AutoFailover {
		t.Fatal("replacement must keep auto-failover")
	}
}
