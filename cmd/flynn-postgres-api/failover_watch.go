package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
)

const autoFailoverGrace = 30 * time.Second
const autoFailoverTick = 5 * time.Second

func runningAppHost(c interface {
	JobList(string) ([]*ct.Job, error)
}, app string) (string, error) {
	if c == nil || strings.TrimSpace(app) == "" {
		return "", nil
	}
	jobs, err := c.JobList(app)
	if err != nil {
		return "", err
	}
	for _, j := range jobs {
		if j == nil || strings.TrimSpace(j.HostID) == "" {
			continue
		}
		switch j.State {
		case ct.JobStateUp, ct.JobStateStarting, ct.JobStatePending:
			return j.HostID, nil
		}
	}
	return "", nil
}

func clusterHasOtherHost(c interface {
	JobListActive() ([]*ct.Job, error)
}, avoid string) bool {
	if c == nil {
		return false
	}
	jobs, err := c.JobListActive()
	if err != nil {
		return false
	}
	for _, j := range jobs {
		if j != nil && j.HostID != "" && j.HostID != avoid {
			return true
		}
	}
	return false
}

func appJobRunning(c interface {
	JobList(string) ([]*ct.Job, error)
}, app string) bool {
	host, err := runningAppHost(c, app)
	return err == nil && host != ""
}

func (h *handler) fenceIsolatedApp(app string) error {
	if h == nil || h.client == nil || strings.TrimSpace(app) == "" {
		return nil
	}
	rel, err := h.client.GetAppRelease(app)
	if err != nil || rel == nil {
		return err
	}
	timeout := instanceReadyTimeout
	return h.client.ScaleAppRelease(app, rel.ID, ct.ScaleOptions{
		Processes: map[string]int{postgres.ProcessName: 0},
		Timeout:   &timeout,
		NoWait:    true,
	})
}

func (h *handler) unfenceIsolatedApp(app string) error {
	if h == nil || h.client == nil || strings.TrimSpace(app) == "" {
		return nil
	}
	rel, err := h.client.GetAppRelease(app)
	if err != nil || rel == nil {
		return err
	}
	return h.client.ScaleAppRelease(app, rel.ID, instanceScaleOptions())
}

func (h *handler) startFailoverWatch() {
	if h == nil || h.client == nil || h.store == nil {
		return
	}
	go func() {
		downSince := map[string]time.Time{}
		for {
			h.reconcileAutoFailover(downSince)
			time.Sleep(autoFailoverTick)
		}
	}()
}

func (h *handler) reconcileAutoFailover(downSince map[string]time.Time) {
	if h == nil || h.store == nil {
		return
	}
	h.pruneStoreGoneIsolated()
	for _, fol := range h.store.AutoFailoverFollowers() {
		if fol == nil {
			continue
		}
		leader, err := h.store.Get(fol.LeaderID)
		if err != nil || leader == nil {
			continue
		}
		if appJobRunning(h.client, leader.App) {
			delete(downSince, fol.ID)
			continue
		}
		if _, ok := downSince[fol.ID]; !ok {
			downSince[fol.ID] = time.Now()
			continue
		}
		if time.Since(downSince[fol.ID]) < autoFailoverGrace {
			continue
		}
		res, err := h.store.Promote(fol.ID)
		if err != nil {
			if h.log != nil {
				h.log.Error("auto-failover promote", "follower", fol.App, "err", err)
			}
			continue
		}
		delete(downSince, fol.ID)
		if res != nil {
			fromApp, toApp := "", ""
			if res.PreviousLeader != nil {
				fromApp = res.PreviousLeader.App
			}
			if res.Promoted != nil {
				toApp = res.Promoted.App
			}
			h.emitTopology(postgres.CodeFailover, res.Promoted, fromApp, toApp, "reason=auto")
			_ = h.stampIsolatedRole(res.Promoted)
			if res.PreviousLeader != nil {
				h.emitTopology(postgres.CodeDeposed, res.PreviousLeader, fromApp, toApp, "reason=auto")
				_ = h.stampIsolatedRole(res.PreviousLeader)
				_ = h.fenceIsolatedApp(res.PreviousLeader.App)
			}
			h.syncResourceEnv(nil, res.Promoted)
		}
		_ = h.ensureAutoFailoverReplica(res.Promoted)
	}
	for _, primary := range h.store.NeedsReplica() {
		_ = h.ensureAutoFailoverReplica(primary)
	}
	h.replaceBrokenFollowers()
}

func (h *handler) pruneStoreGoneIsolated() {
	if h == nil || h.store == nil || h.client == nil {
		return
	}
	_ = h.pruneGoneIsolatedInstances(h.store.All())
}

func (h *handler) ensureAutoFailoverReplica(primary *postgres.Instance) error {
	if h == nil || h.store == nil || primary == nil {
		return nil
	}
	primary, err := h.store.Get(firstNonEmptyLocal(primary.ID, primary.App))
	if err != nil || primary == nil {
		return err
	}
	if h.upgradeInProgress(primary.ID) || h.upgradeInProgress(primary.App) {
		return nil
	}
	if fol := autoFailoverFollowerOf(h, primary); fol != nil {
		if skip := h.skipFollowerReplace(fol); skip != "" {
			if h.followerStreaming(fol) {
				_ = h.store.SetReplicaPending(primary.ID, false)
			}
			return nil
		}
		if h.followerStreaming(fol) {
			_ = h.store.SetReplicaPending(primary.ID, false)
			return nil
		}
		recovering, _, ok := queryFollowerReplay(fol.ConnectionURL())
		if ok && !recovering {
			return h.recreateFollower(fol, primary)
		}
		return nil
	}
	host, _ := runningAppHost(h.client, primary.App)
	if host != "" && !clusterHasOtherHost(h.client, host) {
		_ = h.store.SetReplicaPending(primary.ID, true)
		return postgres.ErrAutoFailoverNoHost
	}
	tenant := firstNonEmptyLocal(primary.Tenant, primary.App)
	_, err = h.provisionFollow(tenant, tenant, firstNonEmptyLocal(primary.App, primary.ID), primary.Runtime, true)
	if err != nil {
		_ = h.store.SetReplicaPending(primary.ID, true)
		return err
	}
	_ = h.store.SetReplicaPending(primary.ID, false)
	return nil
}

func autoFailoverFollowerOf(h *handler, primary *postgres.Instance) *postgres.Instance {
	if h == nil || h.store == nil || primary == nil {
		return nil
	}
	for _, id := range primary.Followers {
		fol, err := h.store.Get(id)
		if err == nil && fol != nil && fol.AutoFailover && fol.Role == postgres.RoleFollower {
			return fol
		}
	}
	for _, fol := range h.store.AutoFailoverFollowers() {
		if fol.LeaderID == primary.ID || fol.LeaderID == primary.App {
			return fol
		}
	}
	return nil
}

func firstNonEmptyLocal(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func (h *handler) scaleIsolatedUp(app string, tags map[string]map[string]string) error {
	if h == nil || h.client == nil {
		return fmt.Errorf("missing controller")
	}
	rel, err := h.client.GetAppRelease(app)
	if err != nil || rel == nil {
		return err
	}
	timeout := instanceReadyTimeout
	return h.client.ScaleAppRelease(app, rel.ID, ct.ScaleOptions{
		Processes: map[string]int{postgres.ProcessName: postgres.DefaultNodes},
		Tags:      tags,
		Timeout:   &timeout,
		NoWait:    true,
	})
}
