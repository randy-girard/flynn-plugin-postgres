package main

import (
	"strings"
	"sync"
	"time"

	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
)

// autoFailoverMaxGap is the longest pause between two sightings of a down
// primary that still counts as one continuous outage. A longer gap (lease
// lost, controller errors, a stalled loop) starts the grace period over,
// since the primary may have come back in between.
const autoFailoverMaxGap = 2 * autoFailoverTick

// failoverSampleMaxAge is how long before the primary stopped its last
// running sighting may be. Auto-failover promotes only a follower that
// replayed the primary's WAL position at that sighting, so an older sample
// would let a follower that stopped streaming long before the outage be
// promoted.
const failoverSampleMaxAge = autoFailoverGrace

// failoverDowntime records, per auto-failover target, when this lease holder
// first saw the target's primary down (since) and when it last saw it down
// (seen). Both are cleared on every lease change.
type failoverDowntime struct {
	since  map[string]time.Time
	seen   map[string]time.Time
	logged map[string]string
}

func newFailoverDowntime() *failoverDowntime {
	d := &failoverDowntime{}
	d.reset()
	return d
}

func (d *failoverDowntime) reset() {
	d.since = map[string]time.Time{}
	d.seen = map[string]time.Time{}
	d.logged = map[string]string{}
}

func (d *failoverDowntime) forget(id string) {
	delete(d.since, id)
	delete(d.seen, id)
	delete(d.logged, id)
}

// firstNote is true the first time msg is noted for id in this outage, so a
// target that stays unpromotable is logged once, not every tick.
func (d *failoverDowntime) firstNote(id, msg string) bool {
	if d.logged[id] == msg {
		return false
	}
	d.logged[id] = msg
	return true
}

// observeDown records a down sighting at now and returns how long the
// primary has been down without a gap longer than autoFailoverMaxGap.
func (d *failoverDowntime) observeDown(id string, now time.Time) time.Duration {
	since, ok := d.since[id]
	if !ok || now.Sub(d.seen[id]) > autoFailoverMaxGap || now.Before(d.seen[id]) {
		since = now
		d.since[id] = now
	}
	d.seen[id] = now
	return now.Sub(since)
}

func (h *handler) now() time.Time {
	if h != nil && h.clock != nil {
		return h.clock()
	}
	return time.Now()
}

// primarySample is a sighting of a running primary: its WAL position and
// when it was read.
type primarySample struct {
	WAL int64
	At  time.Time
}

// failoverSamples holds the newest sample per primary app. Every API process
// samples (not only the lease holder), so the process that takes the lease
// after the holder died with the primary's host still has one.
type failoverSamples struct {
	mu sync.Mutex
	m  map[string]primarySample
}

func (s *failoverSamples) record(app string, sample primarySample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]primarySample{}
	}
	s.m[app] = sample
}

func (s *failoverSamples) get(app string) (primarySample, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sample, ok := s.m[app]
	return sample, ok
}

// primaryOutage is primaryObservedDown plus when the primary's newest job
// stopped, as the controller recorded it (zero when unknown).
func primaryOutage(h *handler, app string) (down, known bool, stopped time.Time) {
	jobs, err := h.listAppJobs(app)
	if err != nil {
		return false, false, time.Time{}
	}
	for _, j := range jobs {
		if j == nil {
			continue
		}
		if jobOccupies(j.State) {
			return false, true, time.Time{}
		}
		if j.UpdatedAt != nil && j.UpdatedAt.After(stopped) {
			stopped = *j.UpdatedAt
		}
	}
	return true, true, stopped
}

// failoverPrimaries are the primaries of this process's auto-failover
// followers (or platform replicas in system mode), once each.
func (h *handler) failoverPrimaries() []*postgres.Instance {
	if h == nil || h.store == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []*postgres.Instance
	targets := append(h.store.AutoFailoverFollowers(), h.store.PlatformReplicas()...)
	for _, fol := range targets {
		if fol == nil || strings.TrimSpace(fol.LeaderID) == "" {
			continue
		}
		leader, err := h.store.Get(fol.LeaderID)
		if err != nil || leader == nil || leader.Role != postgres.RolePrimary || seen[leader.App] {
			continue
		}
		seen[leader.App] = true
		out = append(out, leader)
	}
	return out
}

// sampleFailoverPrimaries records the WAL position of every running primary
// that has an auto-failover target. It runs on every tick on every API
// process.
func (h *handler) sampleFailoverPrimaries() {
	for _, leader := range h.failoverPrimaries() {
		if down, known := primaryObservedDown(h, leader.App); !known || down {
			continue
		}
		wal, ok := readPrimaryWAL(leader.MaintenanceURL())
		if !ok {
			continue
		}
		h.samples.record(leader.App, primarySample{WAL: wal, At: h.now()})
	}
}

// failoverTick is one watch tick. Every process adopts the failover targets
// it did not create and samples their primaries; only the lease holder acts.
func (h *handler) failoverTick(down *failoverDowntime, isLeader bool) {
	if h == nil || h.store == nil {
		return
	}
	if h.client != nil {
		h.adoptFailoverTargets(h.client)
	}
	h.sampleFailoverPrimaries()
	if isLeader {
		h.reconcileAutoFailover(down)
	}
}

// adoptFailoverTargets loads auto-failover followers and platform replicas
// from the controller into this process's store. The store is per API
// process, so the lease holder may never have served the follower it must
// promote (the other process provisioned it, or plugin:update restarted
// both). Each app's release is read again only when it changes.
func (h *handler) adoptFailoverTargets(c appListReleaser) {
	if h == nil || h.store == nil || c == nil {
		return
	}
	apps, err := c.AppList()
	if err != nil {
		return
	}
	h.adoptMu.Lock()
	defer h.adoptMu.Unlock()
	if h.adoptSeen == nil {
		h.adoptSeen = map[string]string{}
	}
	live := make(map[string]struct{}, len(apps))
	for _, app := range apps {
		if app == nil || !postgres.IsolatedInstanceApp(app.Name) {
			continue
		}
		live[app.ID] = struct{}{}
		if app.ReleaseID != "" && h.adoptSeen[app.ID] == app.ReleaseID {
			continue
		}
		rel, err := c.GetAppRelease(app.ID)
		if err != nil || rel == nil || rel.Env == nil {
			continue
		}
		h.adoptSeen[app.ID] = app.ReleaseID
		inst := postgres.InstanceFromEnv(app.ID, app.Name, rel.Env)
		if inst == nil || inst.Role != postgres.RoleFollower || (!inst.AutoFailover && !inst.PlatformReplica) {
			continue
		}
		inst.App = app.Name
		h.store.Adopt(inst)
	}
	for id := range h.adoptSeen {
		if _, ok := live[id]; !ok {
			delete(h.adoptSeen, id)
		}
	}
}

// considerFailover promotes fol when its primary's job has been gone for
// autoFailoverGrace, but only through the verified promote (DATA-03) and
// only when fol has replayed the WAL position the primary had when it was
// last seen running (NEW-33). With no such sighting it waits and logs once:
// pg:promote --force is the operator's way out.
func (h *handler) considerFailover(down *failoverDowntime, fol *postgres.Instance) {
	if h == nil || h.store == nil || down == nil || fol == nil {
		return
	}
	leader, err := h.store.Get(fol.LeaderID)
	if err != nil || leader == nil {
		return
	}
	primaryDown, known, stopped := primaryOutage(h, leader.App)
	if !known || !primaryDown {
		if known {
			down.forget(fol.ID)
		}
		return
	}
	now := h.now()
	downFor := down.observeDown(fol.ID, now)
	if downFor < autoFailoverGrace {
		return
	}
	ref := stopped
	if ref.IsZero() || ref.After(now) {
		ref = now.Add(-downFor)
	}
	sample, ok := h.samples.get(leader.App)
	if !ok || ref.Sub(sample.At) > failoverSampleMaxAge {
		if down.firstNote(fol.ID, "no sample") {
			h.logFailover("failover target not promoted: no sighting of the running primary within "+failoverSampleMaxAge.String()+" of its stop; run pg:promote --force to promote anyway", "primary", leader.App, "target", fol.App)
		}
		return
	}
	if behind, why := h.failoverTargetBehind(fol, sample.WAL); behind {
		if down.firstNote(fol.ID, why) {
			h.logFailover("failover target not promotable", "primary", leader.App, "target", fol.App, "reason", why)
		}
		return
	}
	res, err := h.promoteFollower(fol.ID, promoteOptions{PrimaryDown: true, Target: sample.WAL})
	if err != nil {
		if down.firstNote(fol.ID, "promote failed") {
			h.logFailover("failover promote failed", "primary", leader.App, "target", fol.App, "err", err)
		}
		return
	}
	down.forget(fol.ID)
	if res != nil && res.Promoted != nil {
		h.logFailover("failover promoted", "primary", leader.App, "target", res.Promoted.App, "primary_down_for", downFor.Truncate(time.Second))
	}
}

// failoverTargetBehind is checked before anything is stopped: with its
// primary gone a follower receives nothing more, so one that has applied
// all it received but is still short of target cannot catch up.
func (h *handler) failoverTargetBehind(fol *postgres.Instance, target int64) (bool, string) {
	recovering, _, ok := readReplicaRecovery(fol.MaintenanceURL())
	if !ok {
		return true, "follower not answering"
	}
	if !recovering {
		return true, "follower not in recovery"
	}
	out, err := runSQL(fol.MaintenanceURL(), sqlReceiveReplay)
	if err != nil {
		return true, "follower not answering"
	}
	recv, replay, ok := parseTwoInts(out)
	if !ok {
		return true, "unreadable receive/replay position"
	}
	if replay >= target {
		return false, ""
	}
	if recv > replay {
		return true, "still applying received WAL"
	}
	return true, "behind the primary's last sighting"
}

func (h *handler) logFailover(msg string, ctx ...interface{}) {
	if h != nil && h.log != nil {
		h.log.Info(msg, ctx...)
	}
}

// countsTowardHostProcessCap matches the scheduler: datastore apps and tenant
// apps count toward flynn-max-processes, other system apps do not.
func countsTowardHostProcessCap(meta map[string]string) bool {
	if meta["flynn-datastore"] == "true" {
		return true
	}
	return meta["flynn-system-app"] != "true"
}

// hostFitTTL is how long one host-fit read is reused. It is short so the
// API process that is not the lease holder does not place a follower from a
// stale view (DATA-25).
const hostFitTTL = autoFailoverTick

// countHostJobs is each host's process and volume-job count, skipping jobs
// the scheduler does not count toward its process cap.
func countHostJobs(jobs []*ct.Job, apps []*ct.App) (procs, singletons map[string]int) {
	meta := make(map[string]map[string]string, len(apps))
	for _, a := range apps {
		if a != nil {
			meta[a.ID] = a.Meta
		}
	}
	procs = map[string]int{}
	singletons = map[string]int{}
	for _, j := range jobs {
		if j == nil || j.HostID == "" || !jobOccupies(j.State) {
			continue
		}
		if !countsTowardHostProcessCap(meta[j.AppID]) {
			continue
		}
		procs[j.HostID]++
		if len(j.VolumeIDs) > 0 {
			singletons[j.HostID]++
		}
	}
	return procs, singletons
}
