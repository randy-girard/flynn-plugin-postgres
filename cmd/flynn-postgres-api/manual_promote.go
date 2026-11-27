package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
)

// Manual promote and unfollow bounds (DATA-03). Tests shorten them.
var (
	// manualCatchupTimeout is how long a follower has to replay the
	// primary's current WAL position. pg:wait is the long wait; a promote
	// of a follower that is still behind after this is refused.
	manualCatchupTimeout = 20 * time.Second
	// manualVerifyTimeout is how long the restarted job has to come up
	// out of recovery and accept a write.
	manualVerifyTimeout = 3 * time.Minute
	manualPoll          = time.Second
)

const (
	sqlReceiveReplay = `SELECT COALESCE(pg_wal_lsn_diff(pg_last_wal_receive_lsn(), '0/0'),0), COALESCE(pg_wal_lsn_diff(pg_last_wal_replay_lsn(), '0/0'),0)`
	// sqlWriteProbe fails on a standby and in a read-only transaction. A
	// temp table touches no tenant table and is dropped at commit.
	sqlWriteProbe = `CREATE TEMP TABLE flynn_write_probe (id int) ON COMMIT DROP; INSERT INTO flynn_write_probe VALUES (1)`
)

type promoteOptions struct {
	// Force promotes when the primary cannot be read, so the follower
	// cannot be shown to have its last writes. Those writes are lost.
	Force bool
	// PrimaryDown is auto-failover (NEW-33): the primary's job is gone, so
	// its WAL position cannot be read. Target is the position the watch
	// read when it last saw the primary running; the follower must have
	// replayed it. Only an auto-failover follower or platform replica.
	PrimaryDown bool
	Target      int64
}

// promoteFollower is pg:promote, the API and the dashboard (DATA-03). A
// manual promote is a switchover (NEW-51): the leader's apps follow the
// promoted primary and the old primary is retired.
//
//  1. read the primary's WAL position and wait, bounded, until the follower
//     has replayed it (refused, with nothing changed, if it has not; an
//     unreadable primary needs Force);
//  2. stop the old primary and stamp it deposed (it never starts writable
//     again), then wait until the follower has applied every byte it
//     received, so no write the primary acknowledged is lost;
//  3. restart the follower's job as a primary (its release role decides
//     standby.signal) and verify it left recovery and accepts a write;
//  4. only then record the new roles. An auto-failover follower claims the
//     old primary's discoverd name; a plain follower's promote rewrites the
//     old primary's attachments to the promoted host (the leader's login,
//     which replication copied) and deploys each attached app (Rewritten).
//
// A failure in 2-3 puts the follower back on its previous release and
// starts the old primary again on its own. A failed app deploy in 4 is
// reported; pg:promote again finishes it.
func (h *handler) promoteFollower(id string, opts promoteOptions) (*postgres.PromoteResult, error) {
	// The failover watch promotes platform replicas; users may not.
	if !opts.PrimaryDown {
		if err := h.rejectPlatformReplicaControl(id); err != nil {
			return nil, err
		}
	}
	if !h.live() {
		res, err := h.promoteFenced(id, opts.PrimaryDown)
		if err != nil {
			return nil, err
		}
		if opts.PrimaryDown {
			err = h.recordPromotion(res, postgres.CodeFailover, "reason=auto")
		} else {
			err = h.afterManualPromote(res)
		}
		if err != nil {
			return nil, err
		}
		return res, nil
	}
	fol, err := h.store.Get(id)
	if err != nil {
		return nil, err
	}
	if fol.Role != postgres.RoleFollower || strings.TrimSpace(fol.LeaderID) == "" {
		if h.alreadyWritable(fol) {
			// A retry: finish moving the retired primary's apps.
			res := &postgres.PromoteResult{Promoted: fol, PreviousLeader: h.retiredBy(fol)}
			return res, h.switchoverApps(res)
		}
		return nil, postgres.ErrNotFollower
	}
	if !h.beginSwitch(fol.ID) {
		return nil, fmt.Errorf("a promote or unfollow of %s is already running; retry when it finishes", fol.App)
	}
	defer h.switching.Delete(fol.ID)
	if alias, done := h.finishedElsewhere(fol); done {
		// Another API process already promoted it; record that here.
		if alias != "" {
			return h.store.PromoteKeepingService(fol.ID)
		}
		res, err := h.store.Switchover(fol.ID)
		if err != nil {
			return nil, err
		}
		return res, h.switchoverApps(res)
	}
	leader, err := h.store.Get(fol.LeaderID)
	if err != nil || leader == nil {
		return nil, fmt.Errorf("promote refused: primary %s of %s is not known to the plugin; nothing was changed", fol.LeaderID, fol.App)
	}
	failover := fol.AutoFailover || fol.PlatformReplica
	var target int64
	var haveTarget bool
	if opts.PrimaryDown {
		if !failover {
			return nil, fmt.Errorf("promote refused: %s is not an auto-failover follower; nothing was changed", fol.App)
		}
		if behind, why := h.failoverTargetBehind(fol, opts.Target); behind {
			return nil, fmt.Errorf("promote refused: follower %s: %s (primary %s was last seen at WAL %d); nothing was changed", fol.App, why, leader.App, opts.Target)
		}
		target, haveTarget = opts.Target, true
	} else {
		target, haveTarget, err = h.followerCaughtUp(fol, leader, opts.Force)
		if err != nil {
			return nil, err
		}
	}
	// Every manual promote stops the old primary first (a switchover): a
	// write it took after the catch-up check would otherwise be lost, and
	// two writable primaries must never run.
	var leaderPrev *ct.Release
	fenced := false
	{
		leaderPrev, _ = h.client.GetAppRelease(leader.App)
		if err := h.fenceIsolatedApp(leader.App); err != nil {
			return nil, fmt.Errorf("promote refused: could not stop the current primary %s: %w; nothing was changed", leader.App, err)
		}
		fenced = true
		deposed := *leader
		deposed.Role = postgres.RoleDeposed
		deposed.ReadOnly = true
		deposed.LeaderID = fol.ID
		if err := h.stampIsolatedRole(&deposed); err != nil {
			return nil, h.rollbackPromote(fol, leader, nil, leaderPrev, fenced, fmt.Errorf("stamp %s deposed: %w", leader.App, err))
		}
		if err := h.waitApplied(fol, target, haveTarget); err != nil {
			return nil, h.rollbackPromote(fol, leader, nil, leaderPrev, fenced, err)
		}
	}
	folPrev, err := h.client.GetAppRelease(fol.App)
	if err != nil || folPrev == nil {
		if err == nil {
			err = fmt.Errorf("no release")
		}
		return nil, h.rollbackPromote(fol, leader, nil, leaderPrev, fenced, fmt.Errorf("read %s release: %w", fol.App, err))
	}
	next := promotedCopy(fol, leader, failover)
	if err := h.promoteIsolatedJob(next); err != nil {
		return nil, h.rollbackPromote(fol, leader, folPrev, leaderPrev, fenced, fmt.Errorf("restart %s as primary: %w", fol.App, err))
	}
	if err := h.verifyWritable(next, target, haveTarget); err != nil {
		return nil, h.rollbackPromote(fol, leader, folPrev, leaderPrev, fenced, err)
	}
	var res *postgres.PromoteResult
	if failover {
		res, err = h.store.PromoteKeepingService(fol.ID)
	} else {
		res, err = h.store.Switchover(fol.ID)
	}
	if err != nil {
		return nil, h.rollbackPromote(fol, leader, folPrev, leaderPrev, fenced, err)
	}
	if opts.PrimaryDown {
		err = h.recordPromotion(res, postgres.CodeFailover, "reason=auto")
	} else {
		err = h.afterManualPromote(res)
	}
	if err != nil {
		return res, err
	}
	return res, nil
}

// afterManualPromote records a verified promotion: topology, releases of
// the deposed primary, resource env, the replacement replica, and (plain
// follower) the switchover of the old primary's apps.
func (h *handler) afterManualPromote(res *postgres.PromoteResult) error {
	if err := h.recordPromotion(res, postgres.CodePromoted, "reason=manual"); err != nil {
		return err
	}
	return h.switchoverApps(res)
}

// switchoverApps moves the retired primary's attachments (and the promoted
// instance's own, onto its writable login) to the promoted primary and
// deploys each app, then sets res.Rewritten to what the controller now has.
// A promote that kept the old discoverd name (auto-failover) has nothing to
// move. Without a controller the store's in-memory rewrite stands.
func (h *handler) switchoverApps(res *postgres.PromoteResult) error {
	if res == nil || res.Promoted == nil || strings.TrimSpace(res.Promoted.ServiceAlias) != "" || !h.live() {
		return nil
	}
	rewritten, err := h.cutoverAttachedAppsReport(res.PreviousLeader, res.Promoted)
	res.Rewritten = rewritten
	if err != nil {
		return fmt.Errorf("%s is the primary, but its apps were not all moved: %w; run pg:promote %s again to finish", res.Promoted.App, err, res.Promoted.App)
	}
	return nil
}

// retiredBy is the deposed primary a switchover to inst retired, if this
// process knows it.
func (h *handler) retiredBy(inst *postgres.Instance) *postgres.Instance {
	if h == nil || h.store == nil || inst == nil {
		return nil
	}
	for _, cand := range h.store.All() {
		if cand != nil && cand.Role == postgres.RoleDeposed && (cand.LeaderID == inst.ID || cand.LeaderID == inst.App) {
			return cand
		}
	}
	return nil
}

func (h *handler) recordPromotion(res *postgres.PromoteResult, code, reason string) error {
	if res == nil || res.Promoted == nil {
		return nil
	}
	fromApp, toApp := "", res.Promoted.App
	if res.PreviousLeader != nil {
		fromApp = res.PreviousLeader.App
	}
	h.emitTopology(code, res.Promoted, fromApp, toApp, reason)
	if err := h.stampIsolatedRole(res.Promoted); err != nil {
		return err
	}
	if res.PreviousLeader != nil && res.PreviousLeader.Role == postgres.RoleDeposed {
		h.emitTopology(postgres.CodeDeposed, res.PreviousLeader, fromApp, toApp, reason)
		_ = h.stampIsolatedRole(res.PreviousLeader)
		h.syncResourceEnv(nil, res.PreviousLeader)
		h.retargetFollowerJobs(res.Promoted)
		_ = h.ensureAutoFailoverReplica(res.Promoted)
	}
	h.syncResourceEnv(nil, res.Promoted)
	return nil
}

// unfollowFollower is pg:unfollow, the API and the dashboard (DATA-03):
// restart the follower's job standalone (no standby.signal, no
// primary_conninfo), verify it accepts a write, then record it. The
// upstream is not touched. A failure puts the follower back on its
// previous release.
func (h *handler) unfollowFollower(id string) (*postgres.Instance, error) {
	if err := h.rejectPlatformReplicaControl(id); err != nil {
		return nil, err
	}
	before, _ := h.store.Get(id)
	if h.live() && before != nil && (before.Role != postgres.RoleFollower || strings.TrimSpace(before.LeaderID) == "") {
		if h.alreadyWritable(before) {
			return before, nil
		}
		return nil, postgres.ErrNotFollower
	}
	if h.live() && before != nil {
		if !h.beginSwitch(before.ID) {
			return nil, fmt.Errorf("a promote or unfollow of %s is already running; retry when it finishes", before.App)
		}
		defer h.switching.Delete(before.ID)
		if _, done := h.finishedElsewhere(before); done {
			return h.store.Unfollow(id)
		}
		prev, err := h.client.GetAppRelease(before.App)
		if err != nil || prev == nil {
			if err == nil {
				err = fmt.Errorf("no release")
			}
			return nil, fmt.Errorf("unfollow refused: read %s release: %w; nothing was changed", before.App, err)
		}
		next := *before
		next.Role = postgres.RoleStandalone
		next.ReadOnly = false
		next.LeaderID = ""
		next.Mode = ""
		next.LagBytes = 0
		next.AutoFailover = false
		next.ServiceAlias = ""
		if err := h.promoteIsolatedJob(&next); err != nil {
			return nil, h.rollbackUnfollow(before, prev, fmt.Errorf("restart %s standalone: %w", before.App, err))
		}
		if err := h.verifyWritable(&next, 0, false); err != nil {
			return nil, h.rollbackUnfollow(before, prev, err)
		}
	}
	inst, err := h.store.Unfollow(id)
	if err != nil {
		return nil, err
	}
	h.emitFollowerRemoved(before, "reason=unfollow")
	if err := h.stampIsolatedRole(inst); err != nil {
		return nil, err
	}
	h.syncResourceEnv(nil, inst)
	return inst, nil
}

func promotedCopy(fol, leader *postgres.Instance, failover bool) *postgres.Instance {
	next := *fol
	next.Role = postgres.RolePrimary
	next.ReadOnly = false
	next.LeaderID = ""
	next.Mode = ""
	next.LagBytes = 0
	next.AutoFailover = false
	next.PlatformReplica = false
	next.ServiceAlias = ""
	if failover {
		// Same as the failover watch: DATABASE_URL keeps
		// leader.<original>.discoverd and the promoted job claims it.
		next.ServiceAlias = leader.StableService()
		next.ReplicaPending = true
	}
	return &next
}

// alreadyWritable is a retry of a promote or unfollow that already
// finished: the store and the release say primary and the job is out of
// recovery. Anything else is not a no-op.
func (h *handler) alreadyWritable(inst *postgres.Instance) bool {
	if h == nil || inst == nil || h.client == nil {
		return false
	}
	if inst.Role != postgres.RolePrimary && inst.Role != postgres.RoleStandalone {
		return false
	}
	rel, err := h.client.GetAppRelease(inst.App)
	if err != nil || rel == nil || !strings.EqualFold(strings.TrimSpace(rel.Env["FLYNN_POSTGRES_ROLE"]), "primary") {
		return false
	}
	recovering, _, ok := readReplicaRecovery(inst.MaintenanceURL())
	return ok && !recovering
}

func (h *handler) beginSwitch(id string) bool {
	_, running := h.switching.LoadOrStore(id, struct{}{})
	return !running
}

// finishedElsewhere is a store follower whose release already says primary
// and whose job is out of recovery: the other API process (web runs two,
// each with its own store) promoted or unfollowed it. alias is the
// release's service alias (a failover-style promote).
func (h *handler) finishedElsewhere(inst *postgres.Instance) (string, bool) {
	if h == nil || h.client == nil || inst == nil {
		return "", false
	}
	rel, err := h.client.GetAppRelease(inst.App)
	if err != nil || rel == nil || !strings.EqualFold(strings.TrimSpace(rel.Env["FLYNN_POSTGRES_ROLE"]), "primary") {
		return "", false
	}
	recovering, _, ok := readReplicaRecovery(inst.MaintenanceURL())
	if !ok || recovering {
		return "", false
	}
	return strings.TrimSpace(rel.Env[postgres.EnvServiceAlias]), true
}

// followerCaughtUp reads the primary's WAL position and waits until the
// follower is a standby that has replayed it.
func (h *handler) followerCaughtUp(fol, leader *postgres.Instance, force bool) (int64, bool, error) {
	target, ok := readPrimaryWAL(leader.MaintenanceURL())
	if !ok {
		if !force {
			return 0, false, fmt.Errorf("promote refused: cannot read primary %s's WAL position, so %s cannot be shown to have its last writes; nothing was changed. Retry when the primary answers, or pass --force to promote anyway (writes %s has not received are lost)", leader.App, fol.App, fol.App)
		}
		if h.log != nil {
			h.log.Warn("FORCED PROMOTE: primary unreadable; writes the follower has not received are lost", "primary", leader.App, "follower", fol.App)
		}
		recovering, _, fok := readReplicaRecovery(fol.MaintenanceURL())
		if !fok {
			return 0, false, fmt.Errorf("promote refused: follower %s is not answering; nothing was changed", fol.App)
		}
		if !recovering {
			return 0, false, fmt.Errorf("promote refused: follower %s is not in recovery but is recorded as a follower; nothing was changed", fol.App)
		}
		return 0, false, nil
	}
	deadline := time.Now().Add(manualCatchupTimeout)
	last := ""
	for {
		recovering, replay, fok := readReplicaRecovery(fol.MaintenanceURL())
		switch {
		case !fok:
			last = "not answering"
		case !recovering:
			return 0, false, fmt.Errorf("promote refused: follower %s is not in recovery but is recorded as a follower; nothing was changed", fol.App)
		case replay >= target:
			return target, true, nil
		default:
			last = fmt.Sprintf("%d bytes behind (replayed %d of %d)", target-replay, replay, target)
		}
		if time.Now().After(deadline) {
			return 0, false, fmt.Errorf("promote refused: follower %s is %s primary %s after %s; run pg:wait %s and retry. Nothing was changed", fol.App, last, leader.App, manualCatchupTimeout, fol.App)
		}
		time.Sleep(manualPoll)
	}
}

// waitApplied waits until a follower whose primary was just stopped has
// replayed everything it received (and, when known, the primary's last
// read position). A stopping primary sends its remaining WAL first.
func (h *handler) waitApplied(fol *postgres.Instance, target int64, haveTarget bool) error {
	deadline := time.Now().Add(manualCatchupTimeout)
	last := ""
	for {
		out, err := runSQL(fol.MaintenanceURL(), sqlReceiveReplay)
		if err != nil {
			last = "not answering: " + err.Error()
		} else {
			recv, replay, ok := parseTwoInts(out)
			switch {
			case !ok:
				last = "unreadable receive/replay position"
			case replay < recv:
				last = fmt.Sprintf("replayed %d of %d received bytes", replay, recv)
			case haveTarget && replay < target:
				last = fmt.Sprintf("replayed %d of the primary's %d", replay, target)
			default:
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("follower %s did not apply the stopped primary's last WAL within %s: %s", fol.App, manualCatchupTimeout, last)
		}
		time.Sleep(manualPoll)
	}
}

// verifyWritable polls the restarted job until it is out of recovery,
// accepts a write, and (when known) is at or past target.
func (h *handler) verifyWritable(inst *postgres.Instance, target int64, haveTarget bool) error {
	deadline := time.Now().Add(manualVerifyTimeout)
	last := ""
	for {
		recovering, _, ok := readReplicaRecovery(inst.MaintenanceURL())
		switch {
		case !ok:
			last = "not answering"
		case recovering:
			last = "still in recovery (read-only standby)"
		default:
			if _, err := runSQL(inst.MaintenanceURL(), sqlWriteProbe); err != nil {
				last = "write probe failed: " + err.Error()
			} else if wal, wok := readPrimaryWAL(inst.MaintenanceURL()); haveTarget && (!wok || wal < target) {
				last = fmt.Sprintf("WAL position %d is before the old primary's %d", wal, target)
			} else {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s failed verification within %s: %s", inst.App, manualVerifyTimeout, last)
		}
		time.Sleep(manualPoll)
	}
}

func (h *handler) rollbackPromote(fol, leader *postgres.Instance, folPrev, leaderPrev *ct.Release, fenced bool, cause error) error {
	var problems []string
	if folPrev != nil {
		if err := h.restoreRelease(fol.App, folPrev); err != nil {
			problems = append(problems, fmt.Sprintf("restore follower %s: %v", fol.App, err))
		}
	}
	if fenced {
		if leaderPrev != nil {
			if err := h.client.SetAppRelease(leader.App, leaderPrev.ID); err != nil {
				problems = append(problems, fmt.Sprintf("restore primary %s release: %v", leader.App, err))
			}
		}
		if err := h.unfenceIsolatedApp(leader.App); err != nil {
			problems = append(problems, fmt.Sprintf("restart primary %s: %v", leader.App, err))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("promote of %s failed: %v; rollback incomplete: %s", fol.App, cause, strings.Join(problems, "; "))
	}
	return fmt.Errorf("promote of %s failed: %v; rolled back: %s is a follower of %s again", fol.App, cause, fol.App, leader.App)
}

func (h *handler) rollbackUnfollow(fol *postgres.Instance, prev *ct.Release, cause error) error {
	if err := h.restoreRelease(fol.App, prev); err != nil {
		return fmt.Errorf("unfollow of %s failed: %v; rollback incomplete: %v", fol.App, cause, err)
	}
	return fmt.Errorf("unfollow of %s failed: %v; rolled back: %s is a follower again", fol.App, cause, fol.App)
}

// restoreRelease puts app back on prev and restarts its job there.
func (h *handler) restoreRelease(app string, prev *ct.Release) error {
	if h == nil || h.client == nil || prev == nil {
		return nil
	}
	a, err := h.client.GetApp(app)
	if err != nil || a == nil {
		if err == nil {
			err = errors.New("app not found")
		}
		return err
	}
	cur, err := h.client.GetAppRelease(a.ID)
	if err != nil || cur == nil {
		if err == nil {
			err = errors.New("no release")
		}
		return err
	}
	if cur.ID == prev.ID {
		// Nothing was restamped: make sure the job still runs.
		if err := h.client.ScaleAppRelease(a.ID, prev.ID, instanceScaleOptions()); err != nil {
			return err
		}
		return waitInstanceReady(app, instanceReadyTimeout)
	}
	if err := h.client.SetAppRelease(a.ID, prev.ID); err != nil {
		return err
	}
	if err := bounceIsolatedRelease(h.client, a.ID, cur.ID, prev.ID, cur); err != nil {
		return err
	}
	return waitInstanceReady(app, instanceReadyTimeout)
}

func parseTwoInts(out string) (int64, int64, bool) {
	parts := strings.Split(firstSQLLine(out), "|")
	if len(parts) < 2 {
		return 0, 0, false
	}
	a, err1 := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	b, err2 := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return a, b, true
}
