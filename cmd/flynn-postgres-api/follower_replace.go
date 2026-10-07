package main

import (
	"context"
	"strings"

	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
)

func (h *handler) adoptLiveFollowers(leader *postgres.Instance) {
	if h == nil || h.store == nil {
		return
	}
	var c appListReleaser
	if h.client != nil {
		c = h.client
	}
	adoptLiveFollowers(h.store, c, leader)
}

func adoptLiveFollowers(s *postgres.Store, c appListReleaser, leader *postgres.Instance) {
	if s == nil || c == nil || leader == nil {
		return
	}
	apps, err := c.AppList()
	if err != nil {
		return
	}
	for _, app := range apps {
		if app == nil || !postgres.IsolatedInstanceApp(app.Name) || app.Name == leader.App {
			continue
		}
		rel, relErr := c.GetAppRelease(app.ID)
		if relErr != nil || rel == nil || rel.Env == nil {
			rel, relErr = c.GetAppRelease(app.Name)
		}
		if relErr != nil || rel == nil || rel.Env == nil {
			continue
		}
		inst := postgres.InstanceFromEnv(app.ID, app.Name, rel.Env)
		if inst == nil || inst.Role != postgres.RoleFollower {
			continue
		}
		inst.App = app.Name
		if inst.LeaderID != leader.App && inst.LeaderID != leader.ID {
			continue
		}
		s.Adopt(inst)
	}
}

func (h *handler) startUpgrade(id string, opts postgres.UpgradeOptions) (*postgres.Task, error) {
	if h == nil || h.store == nil {
		return nil, postgres.ErrNotFound
	}
	if inst, err := h.store.Get(id); err == nil && inst != nil {
		h.adoptLiveFollowers(inst)
	}
	return h.store.StartUpgrade(id, opts)
}

func (h *handler) upgradeInProgress(id string) bool {
	if h == nil || h.store == nil {
		return false
	}
	task := h.store.LatestUpgrade(id)
	if task == nil {
		return false
	}
	return task.Status != postgres.TaskDone && task.Status != postgres.TaskFailed
}

func (h *handler) followerStreaming(fol *postgres.Instance) bool {
	if fol == nil || strings.TrimSpace(fol.ConnectionURL()) == "" {
		return false
	}
	recovering, _, ok := queryFollowerReplay(fol.ConnectionURL())
	return ok && recovering
}

// recreateFollower starts a new streaming replica, rewrites the old follower's
// controller attachments onto it, then drops the old isolated app.
func (h *handler) recreateFollower(old, primary *postgres.Instance) error {
	if h == nil || h.store == nil || old == nil || primary == nil {
		return nil
	}
	primary, err := h.store.Get(firstNonEmptyLocal(primary.ID, primary.App))
	if err != nil || primary == nil {
		return err
	}
	old, err = h.store.Get(firstNonEmptyLocal(old.ID, old.App))
	if err != nil || old == nil {
		return err
	}
	auto := old.AutoFailover
	runtime := old.Runtime
	tenant := firstNonEmptyLocal(old.Tenant, primary.Tenant, primary.App)
	_ = h.store.SetAutoFailover(old.ID, false)
	nf, _, err := h.store.Provision(postgres.ProvisionRequest{
		Follow:       firstNonEmptyLocal(primary.ID, primary.App),
		Mode:         postgres.ModeStreaming,
		Runtime:      runtime,
		AutoFailover: auto,
		Tenant:       tenant,
	})
	if err != nil {
		_ = h.store.SetAutoFailover(old.ID, auto)
		return err
	}
	rollback := func() {
		_ = h.afterDrop()(nf)
		h.store.Forget(nf.ID)
		_ = h.store.SetAutoFailover(old.ID, auto)
	}
	if err := h.startInstance(nf); err != nil {
		rollback()
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), postgres.DefaultUpgradeTimeout)
	defer cancel()
	if err := h.store.Wait(ctx, nf.ID); err != nil {
		rollback()
		return err
	}
	if err := h.cutoverAttachedApps(old, nf); err != nil {
		rollback()
		return err
	}
	if h.log != nil {
		h.log.Info("replaced follower", "from", old.App, "to", nf.App, "primary", primary.App)
	}
	h.emitTopology(postgres.CodeFollowerStarted, nf, primary.App, nf.App, "replaced="+old.App)
	dropErr := h.afterDrop()(old)
	h.store.Forget(old.ID)
	return dropErr
}

func (h *handler) replaceBrokenFollowers() {
	if h == nil || h.store == nil || !h.live() {
		return
	}
	for _, fol := range h.store.All() {
		if fol == nil || fol.Role != postgres.RoleFollower {
			continue
		}
		if skip := h.skipFollowerReplace(fol); skip != "" {
			continue
		}
		recovering, _, ok := queryFollowerReplay(fol.ConnectionURL())
		if !ok || recovering {
			continue
		}
		primary, err := h.store.Get(fol.LeaderID)
		if err != nil || primary == nil {
			continue
		}
		if err := h.recreateFollower(fol, primary); err != nil && h.log != nil {
			h.log.Error("replace broken follower", "app", fol.App, "err", err)
		}
	}
}

// skipFollowerReplace is why this replica must not be torn down. A second API
// worker used to treat a just-promoted primary as a writable "broken follower"
// of the old leader and replace it (fjord/valley).
func (h *handler) skipFollowerReplace(fol *postgres.Instance) string {
	if h == nil {
		return "not a follower"
	}
	var primary *postgres.Instance
	if h.store != nil && fol != nil && strings.TrimSpace(fol.LeaderID) != "" {
		primary, _ = h.store.Get(fol.LeaderID)
	}
	leaderSwap := false
	leaderGone := false
	if primary != nil {
		leaderSwap = imageSwapClaimHeld(h.client, primary.App) || imageSwapClaimHeld(h.client, fol.LeaderID)
		if h.client != nil {
			if _, err := h.client.GetApp(primary.App); err != nil {
				leaderGone = true
			}
		}
	}
	return followerReplaceSkip(fol, primary, livePostgresRole(h.client, folApp(fol)), h.upgradeInProgress(folLeader(fol)) || h.upgradeInProgress(folApp(fol)), leaderSwap, leaderGone)
}

func folApp(fol *postgres.Instance) string {
	if fol == nil {
		return ""
	}
	return fol.App
}

func folLeader(fol *postgres.Instance) string {
	if fol == nil {
		return ""
	}
	return fol.LeaderID
}

// followerReplaceSkip is the replace-follower guard. liveRole is POSTGRES_ROLE
// on the isolated app (not the in-memory store), so a just-promoted primary is
// not torn down by another worker that still has RoleFollower in memory.
func followerReplaceSkip(fol, primary *postgres.Instance, liveRole string, upgradeBusy, leaderSwap, leaderGone bool) string {
	if fol == nil || fol.Role != postgres.RoleFollower {
		return "not a follower"
	}
	if upgradeBusy {
		return "upgrade in progress"
	}
	switch strings.ToLower(strings.TrimSpace(liveRole)) {
	case "primary", "standalone", "deposed":
		return "live role " + strings.ToLower(strings.TrimSpace(liveRole))
	}
	if primary == nil {
		return "leader missing"
	}
	if primary.Role == postgres.RoleDeposed || primary.Role == postgres.RoleFollower {
		return "leader not primary"
	}
	if leaderSwap {
		return "leader image swap"
	}
	if leaderGone {
		return "leader app gone"
	}
	return ""
}

func livePostgresRole(c interface {
	GetAppRelease(string) (*ct.Release, error)
}, app string) string {
	if c == nil {
		return ""
	}
	app = strings.TrimSpace(app)
	if app == "" {
		return ""
	}
	rel, err := c.GetAppRelease(app)
	if err != nil || rel == nil || rel.Env == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(rel.Env["POSTGRES_ROLE"]))
}

func imageSwapClaimHeld(c interface {
	GetApp(string) (*ct.App, error)
}, app string) bool {
	if c == nil {
		return false
	}
	app = strings.TrimSpace(app)
	if app == "" {
		return false
	}
	a, err := c.GetApp(app)
	if err != nil || a == nil || a.Meta == nil {
		return false
	}
	return strings.TrimSpace(a.Meta[imageSwapMetaKey]) != ""
}
