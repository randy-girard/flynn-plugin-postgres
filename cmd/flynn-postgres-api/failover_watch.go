package main

import (
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
	discoverd "github.com/randy-girard/flynn/discoverd/client"
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
	if h == nil || strings.TrimSpace(app) == "" {
		return nil
	}
	return fenceRunningApp(h, app)
}

// fenceRunningApp scales the release that is actually running to 0 and waits
// until that job is gone. A list or scale error refuses promotion.
func fenceRunningApp(h *handler, app string) error {
	jobs, err := h.listAppJobs(app)
	if err != nil {
		return err
	}
	ids := map[string]struct{}{}
	for _, j := range jobs {
		if j == nil || !jobOccupies(j.State) || strings.TrimSpace(j.ReleaseID) == "" {
			continue
		}
		ids[j.ReleaseID] = struct{}{}
	}
	if len(ids) == 0 {
		rel, err := h.releaseOf(app)
		if err != nil {
			return err
		}
		if rel != nil && rel.ID != "" {
			ids[rel.ID] = struct{}{}
		}
	}
	timeout := 60 * time.Second
	for id := range ids {
		if err := h.scaleTo(app, id, ct.ScaleOptions{
			Processes: map[string]int{postgres.ProcessName: 0},
			Timeout:   &timeout,
		}); err != nil {
			return err
		}
	}
	jobs, err = h.listAppJobs(app)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if j != nil && jobOccupies(j.State) {
			return fmt.Errorf("primary %s did not stop", app)
		}
	}
	return nil
}

func jobOccupies(state ct.JobState) bool {
	switch state {
	case ct.JobStateUp, ct.JobStateStarting, ct.JobStatePending, ct.JobStateStopping:
		return true
	default:
		return false
	}
}

func (h *handler) listAppJobs(app string) ([]*ct.Job, error) {
	if h != nil && h.jobs != nil {
		return h.jobs(app)
	}
	if h == nil || h.client == nil {
		return nil, fmt.Errorf("missing controller")
	}
	return h.client.JobList(app)
}

func (h *handler) releaseOf(app string) (*ct.Release, error) {
	if h != nil && h.getRelease != nil {
		return h.getRelease(app)
	}
	if h == nil || h.client == nil {
		return nil, fmt.Errorf("missing controller")
	}
	return h.client.GetAppRelease(app)
}

func (h *handler) scaleTo(app, release string, opts ct.ScaleOptions) error {
	if h != nil && h.scaleRelease != nil {
		return h.scaleRelease(app, release, opts)
	}
	if h == nil || h.client == nil {
		return fmt.Errorf("missing controller")
	}
	return h.client.ScaleAppRelease(app, release, opts)
}

// primaryObservedDown is true only when a successful job list shows no
// occupying job. A list error is not "down".
func primaryObservedDown(h *handler, app string) (down, known bool) {
	jobs, err := h.listAppJobs(app)
	if err != nil {
		return false, false
	}
	for _, j := range jobs {
		if j != nil && jobOccupies(j.State) {
			return false, true
		}
	}
	return true, true
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

const failoverServiceName = "postgres-failover"

var (
	readReplicaRecovery  = queryFollowerReplay
	readPrimaryWAL       = queryWalBytes
	readBasebackupActive = basebackupActive
)

func (h *handler) startFailoverWatch() {
	if h == nil || h.client == nil || h.store == nil {
		return
	}
	var leader atomic.Bool
	h.failoverLeader = leader.Load
	go h.failoverLeaseLoop(&leader)
}

func (h *handler) failoverLeaseLoop(leader *atomic.Bool) {
	downSince := map[string]time.Time{}
	var lastReap time.Time
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}
	for {
		dc := postgres.NewDiscoverdClient()
		hb, err := dc.AddServiceAndRegister(failoverServiceName, ":"+port)
		if err != nil {
			if h.log != nil {
				h.log.Error("postgres failover lease", "err", err)
			}
			time.Sleep(time.Second)
			continue
		}
		self := hb.Addr()
		leaders := make(chan *discoverd.Instance)
		stream, err := dc.Service(failoverServiceName).Leaders(leaders)
		if err != nil {
			_ = hb.Close()
			time.Sleep(time.Second)
			continue
		}
		ticker := time.NewTicker(autoFailoverTick)
		stopped := false
		for !stopped {
			select {
			case inst, ok := <-leaders:
				if !ok {
					leader.Store(false)
					stopped = true
					break
				}
				if inst == nil {
					continue
				}
				leader.Store(inst.Addr == self)
			case <-ticker.C:
				if leader.Load() {
					h.reconcileAutoFailover(downSince)
					if time.Since(lastReap) >= time.Minute {
						h.reapOrphanInstances()
						lastReap = time.Now()
					}
				}
			}
		}
		ticker.Stop()
		_ = hb.Close()
		if stream != nil {
			_ = stream.Close()
		}
		time.Sleep(time.Second)
	}
}

func (h *handler) reconcileAutoFailover(downSince map[string]time.Time) {
	if h == nil || h.store == nil {
		return
	}
	if h.failoverLeader != nil && !h.failoverLeader() {
		return
	}
	mode, err := h.currentAvailability()
	if err != nil {
		return
	}
	h.hostsCached = false
	h.pruneStoreGoneIsolated()
	switch mode {
	case postgres.AvailabilitySystem:
		for _, fol := range h.store.PlatformReplicas() {
			h.considerFailover(downSince, fol)
		}
		h.provisionMissingPlatformReplicas()
	default:
		for _, fol := range h.store.AutoFailoverFollowers() {
			if fol == nil || fol.PlatformReplica {
				continue
			}
			h.considerFailover(downSince, fol)
		}
		for _, primary := range h.store.NeedsReplica() {
			_ = h.ensureAutoFailoverReplica(primary)
		}
	}
	h.replaceBrokenFollowers()
}

func (h *handler) considerFailover(downSince map[string]time.Time, fol *postgres.Instance) {
	if h == nil || h.store == nil || fol == nil {
		return
	}
	leader, err := h.store.Get(fol.LeaderID)
	if err != nil || leader == nil {
		return
	}
	down, known := primaryObservedDown(h, leader.App)
	if !known {
		return
	}
	if !down {
		delete(downSince, fol.ID)
		return
	}
	if _, ok := downSince[fol.ID]; !ok {
		downSince[fol.ID] = time.Now()
		return
	}
	if time.Since(downSince[fol.ID]) < autoFailoverGrace {
		return
	}
	if !h.targetPromotable(fol, leader) {
		return
	}
	if err := h.fenceIsolatedApp(leader.App); err != nil {
		if h.log != nil {
			h.log.Error("auto-failover fence", "primary", leader.App, "err", err)
		}
		return
	}
	res, err := h.store.PromoteKeepingService(fol.ID)
	if err != nil {
		if h.log != nil {
			h.log.Error("auto-failover promote", "follower", fol.App, "err", err)
		}
		return
	}
	delete(downSince, fol.ID)
	if res == nil {
		return
	}
	fromApp, toApp := "", ""
	if res.PreviousLeader != nil {
		fromApp = res.PreviousLeader.App
	}
	if res.Promoted != nil {
		toApp = res.Promoted.App
	}
	h.emitTopology(postgres.CodeFailover, res.Promoted, fromApp, toApp, "reason=auto")
	if res.PreviousLeader != nil {
		h.emitTopology(postgres.CodeDeposed, res.PreviousLeader, fromApp, toApp, "reason=auto")
		_ = h.stampIsolatedRole(res.PreviousLeader)
		_ = h.fenceIsolatedApp(res.PreviousLeader.App)
	}
	if res.Promoted != nil {
		_ = h.stampIsolatedRole(res.Promoted)
		if err := h.promoteIsolatedJob(res.Promoted); err != nil && h.log != nil {
			h.log.Error("auto-failover bounce", "app", res.Promoted.App, "err", err)
		}
		h.retargetFollowerJobs(res.Promoted)
	}
}

// retargetFollowerJobs restarts user followers so they stream from the
// stable discoverd name the new primary claims.
func (h *handler) retargetFollowerJobs(primary *postgres.Instance) {
	if h == nil || h.store == nil || primary == nil {
		return
	}
	for _, id := range primary.Followers {
		fol, err := h.store.Get(id)
		if err != nil || fol == nil || fol.Role != postgres.RoleFollower || fol.PlatformReplica {
			continue
		}
		if err := h.retargetFollowerJob(fol); err != nil && h.log != nil {
			h.log.Error("retarget follower", "app", fol.App, "err", err)
		}
	}
}

func (h *handler) retargetFollowerJob(fol *postgres.Instance) error {
	if h == nil || h.client == nil || fol == nil || strings.TrimSpace(fol.App) == "" {
		return nil
	}
	var leader *postgres.Instance
	if h.store != nil && fol.LeaderID != "" {
		leader, _ = h.store.Get(fol.LeaderID)
	}
	app, err := h.client.GetApp(fol.App)
	if err != nil || app == nil {
		return err
	}
	oldRel, err := h.client.GetAppRelease(app.ID)
	if err != nil || oldRel == nil {
		return err
	}
	if err := stampIsolatedRole(h.client, fol, leader); err != nil {
		return err
	}
	newRel, err := h.client.GetAppRelease(app.ID)
	if err != nil || newRel == nil {
		return err
	}
	return bounceIsolatedRelease(h.client, app.ID, oldRel.ID, newRel.ID, oldRel)
}

func (h *handler) targetPromotable(fol, leader *postgres.Instance) bool {
	if fol == nil || leader == nil {
		return false
	}
	recovering, replay, ok := readReplicaRecovery(fol.ConnectionURL())
	if !ok {
		return false
	}
	wal, wok := readPrimaryWAL(leader.ConnectionURL())
	lag := wal - replay
	if lag < 0 {
		lag = 0
	}
	active, bok := readBasebackupActive(leader.ConnectionURL())
	if !bok {
		return false
	}
	return postgres.ReplicaPromotable(postgres.ReplicaHealth{
		InRecovery: recovering,
		LagBytes:   lag,
		LagKnown:   wok,
		Basebackup: active,
	})
}

func (h *handler) rejectPlatformReplicaControl(id string) error {
	if h == nil || h.store == nil {
		return nil
	}
	inst, err := h.store.Get(id)
	if err != nil || inst == nil || !inst.PlatformReplica {
		return nil
	}
	return fmt.Errorf("the platform replica is managed by the cluster")
}

func (h *handler) currentAvailability() (string, error) {
	if h != nil && h.availability != nil {
		return h.availability()
	}
	return fetchPostgresAvailability()
}

func (h *handler) provisionMissingPlatformReplicas() {
	if h == nil || h.store == nil {
		return
	}
	var ids []string
	for _, primary := range h.store.MissingPlatformReplica() {
		if primary == nil {
			continue
		}
		ids = append(ids, firstNonEmptyLocal(primary.ID, primary.App))
	}
	created := 0
	for _, id := range postgres.BatchIDs(ids, postgres.PlatformReplicaBatch) {
		if created >= postgres.PlatformReplicaBatch {
			break
		}
		primary, err := h.store.Get(id)
		if err != nil || primary == nil || h.upgradeInProgress(primary.ID) || h.upgradeInProgress(primary.App) {
			continue
		}
		host, _ := runningAppHost(h.client, primary.App)
		if host == "" || !h.otherHostFits(host, primary.Runtime) {
			_ = h.store.SetReplicaPending(primary.ID, true)
			continue
		}
		if err := h.createPlatformReplica(primary); err != nil {
			_ = h.store.SetReplicaPending(primary.ID, true)
			if h.log != nil {
				h.log.Error("platform replica", "primary", primary.App, "err", err)
			}
			continue
		}
		created++
	}
}

func (h *handler) createPlatformReplica(primary *postgres.Instance) error {
	if h == nil || h.store == nil || primary == nil {
		return nil
	}
	inst, _, err := h.store.Provision(postgres.ProvisionRequest{
		Tenant:          firstNonEmptyLocal(primary.Tenant, primary.App),
		Follow:          firstNonEmptyLocal(primary.ID, primary.App),
		Runtime:         primary.Runtime,
		PlatformReplica: true,
	})
	if err != nil {
		return err
	}
	if err := h.startInstance(inst); err != nil {
		h.store.Forget(inst.ID)
		return err
	}
	_ = h.store.SetReplicaPending(primary.ID, false)
	return nil
}

func (h *handler) otherHostFits(avoid, runtime string) bool {
	hosts, err := h.currentHostFits()
	if err != nil {
		return false
	}
	mem, disk := h.runtimeNeed(runtime)
	_, ok := postgres.OtherHostCanTake(hosts, avoid, mem, disk)
	return ok
}

func (h *handler) currentHostFits() ([]postgres.HostFit, error) {
	if h != nil && h.hostsCached {
		return h.hostFit, h.hostFitErr
	}
	var hosts []postgres.HostFit
	var err error
	if h != nil && h.hostFits != nil {
		hosts, err = h.hostFits()
	} else {
		hosts, err = h.loadHostFits()
	}
	if h != nil {
		h.hostFit = hosts
		h.hostFitErr = err
		h.hostsCached = true
	}
	return hosts, err
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
	// A second API worker has an empty store after plugin:update or a host
	// restart. Adopt the follower apps that already exist before provisioning
	// another one.
	h.adoptLiveFollowers(primary)
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
	// A user follower still starts when no other host can take the volume.
	// autoFailoverPlacementTags then omits flynn-avoid-host-ids.
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
