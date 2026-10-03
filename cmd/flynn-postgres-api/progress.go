package main

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/julienschmidt/httprouter"
	"github.com/randy-girard/flynn-plugin-postgres"
	"github.com/randy-girard/flynn-plugin-postgres/internal/dashui"
	"github.com/randy-girard/flynn/pkg/httphelper"
)

const (
	sqlBasebackup     = `SELECT COALESCE(backup_streamed,0), COALESCE(backup_total,0), COALESCE(phase,'') FROM pg_stat_progress_basebackup LIMIT 1`
	sqlWalBytes       = `SELECT COALESCE(pg_wal_lsn_diff(pg_current_wal_lsn(), '0/0'),0)`
	sqlReplayBytes    = `SELECT pg_is_in_recovery()::int, COALESCE(pg_wal_lsn_diff(COALESCE(pg_last_wal_replay_lsn(), '0/0'), '0/0'),0)`
	sqlReplicationLag = `SELECT COALESCE(pg_wal_lsn_diff(pg_current_wal_lsn(), COALESCE(replay_lsn, flush_lsn, write_lsn, '0/0')),0), COALESCE(state,'') FROM pg_stat_replication LIMIT 1`
)

func (h *handler) progress(w http.ResponseWriter, _ *http.Request, p httprouter.Params) {
	got, err := h.store.Progress(p.ByName("id"))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	httphelper.JSON(w, 200, got)
}

func (h *handler) dashProgress(w http.ResponseWriter, r *http.Request, sess *dashui.Session) {
	id := strings.TrimSpace(r.URL.Query().Get("instance"))
	report := h.progressReport(sess, id)
	dashui.WriteJSON(w, 200, report)
}

type progressReport struct {
	Followers []postgres.ReplicaProgress `json:"followers"`
	Upgrade   *postgres.Task             `json:"upgrade,omitempty"`
}

func (h *handler) progressReport(sess *dashui.Session, instance string) progressReport {
	out := progressReport{Followers: []postgres.ReplicaProgress{}}
	if instance != "" {
		if p, err := h.store.Progress(instance); err == nil {
			out.Followers = append(out.Followers, p)
		}
	} else {
		seen := map[string]bool{}
		for _, inst := range h.instancesFor(sess) {
			if inst == nil {
				continue
			}
			ref := instanceRef(inst)
			if seen[ref] {
				continue
			}
			seen[ref] = true
			if p, err := h.store.Progress(ref); err == nil {
				out.Followers = append(out.Followers, p)
			}
		}
	}
	var primary *postgres.Instance
	for _, inst := range h.instancesFor(sess) {
		if inst != nil && inst.Role != postgres.RoleFollower {
			primary = inst
			break
		}
	}
	if primary != nil {
		out.Upgrade = h.store.LatestUpgrade(instanceRef(primary))
	}
	if out.Upgrade == nil {
		for _, task := range h.store.ListTasks() {
			if task != nil && task.Kind == postgres.TaskKindUpgrade && task.Status != postgres.TaskDone && task.Status != postgres.TaskFailed {
				out.Upgrade = task
				break
			}
		}
	}
	return out
}

func (h *handler) liveReplicaProgress(inst *postgres.Instance) (*postgres.ReplicaProgress, error) {
	if inst == nil {
		return nil, nil
	}
	if inst.Role != postgres.RoleFollower {
		return liveDiscoverdProgress(inst), nil
	}
	p := postgres.ReplicaProgress{
		Follower: firstNonEmpty(inst.App, inst.ID),
		Leader:   inst.LeaderID,
		Phase:    postgres.PhaseStarting,
		Percent:  0,
	}
	p.Message = postgres.FormatProgress(p)
	if !h.live() {
		return nil, nil
	}
	leader, _ := h.store.Get(inst.LeaderID)
	if leader == nil && inst.LeaderID != "" {
		leader, _ = h.store.Get(strings.TrimSpace(inst.LeaderID))
	}
	if leader != nil {
		if copied, total, phase, ok := queryBasebackup(leader.ConnectionURL()); ok {
			p.Phase = postgres.PhaseBasebackup
			p.BytesCopied = copied
			p.BytesTotal = total
			p.Percent = backupPercentLive(copied, total)
			if strings.TrimSpace(phase) != "" && total <= 0 {
				p.Message = "basebackup " + phase
			} else {
				p.Message = postgres.FormatProgress(p)
			}
			return &p, nil
		}
		if lag, ok := queryPrimaryReplicationLag(leader.ConnectionURL()); ok {
			return streamingOrReady(&p, lag), nil
		}
	}
	if inst.ConnectionURL() != "" {
		recovering, replay, ok := queryFollowerReplay(inst.ConnectionURL())
		if ok {
			if !recovering {
				p.Phase = postgres.PhaseReady
				p.Percent = 100
				p.Ready = true
				p.Message = postgres.FormatProgress(p)
				return &p, nil
			}
			lag := replay
			if leader != nil {
				if wal, wok := queryWalBytes(leader.ConnectionURL()); wok && wal >= replay {
					lag = wal - replay
				}
			}
			return streamingOrReady(&p, lag), nil
		}
	}
	return &p, nil
}

func liveDiscoverdProgress(inst *postgres.Instance) *postgres.ReplicaProgress {
	p := &postgres.ReplicaProgress{
		Follower: firstNonEmpty(inst.App, inst.ID),
		Leader:   inst.LeaderID,
		Phase:    postgres.PhaseStarting,
		Percent:  5,
	}
	p.Message = postgres.FormatProgress(*p)
	if peekDiscoverdInstances(inst.App) {
		p.Phase = postgres.PhaseReady
		p.Percent = 100
		p.Ready = true
		p.Message = postgres.FormatProgress(*p)
	}
	return p
}

func streamingOrReady(p *postgres.ReplicaProgress, lag int64) *postgres.ReplicaProgress {
	if p == nil {
		p = &postgres.ReplicaProgress{}
	}
	p.LagBytes = lag
	if lag <= 0 {
		p.Phase = postgres.PhaseReady
		p.Percent = 100
		p.Ready = true
	} else {
		p.Phase = postgres.PhaseStreaming
		p.Percent = 95
		if lag > 0 {
			p.Percent = 90
		}
		p.Ready = false
	}
	p.Message = postgres.FormatProgress(*p)
	return p
}

func backupPercentLive(copied, total int64) int {
	if total <= 0 {
		if copied > 0 {
			return 5
		}
		return 1
	}
	n := int(copied * 90 / total)
	if n < 1 && copied > 0 {
		n = 1
	}
	if n > 90 {
		n = 90
	}
	if n < 0 {
		n = 0
	}
	return n
}

func queryBasebackup(connURL string) (copied, total int64, phase string, ok bool) {
	out, err := runSQL(connURL, sqlBasebackup)
	if err != nil {
		return 0, 0, "", false
	}
	copied, total, phase, ok = parseBasebackupRow(out)
	return copied, total, phase, ok
}

func parseBasebackupRow(out string) (copied, total int64, phase string, ok bool) {
	line := firstSQLLine(out)
	if line == "" {
		return 0, 0, "", false
	}
	parts := strings.Split(line, "|")
	if len(parts) < 2 {
		return 0, 0, "", false
	}
	copied, err1 := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	total, err2 := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, "", false
	}
	if len(parts) > 2 {
		phase = strings.TrimSpace(parts[2])
	}
	return copied, total, phase, true
}

func queryPrimaryReplicationLag(connURL string) (int64, bool) {
	out, err := runSQL(connURL, sqlReplicationLag)
	if err != nil {
		return 0, false
	}
	line := firstSQLLine(out)
	if line == "" {
		return 0, false
	}
	parts := strings.Split(line, "|")
	lag, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	if err != nil {
		return 0, false
	}
	return lag, true
}

func queryFollowerReplay(connURL string) (recovering bool, replay int64, ok bool) {
	out, err := runSQL(connURL, sqlReplayBytes)
	if err != nil {
		return false, 0, false
	}
	line := firstSQLLine(out)
	if line == "" {
		return false, 0, false
	}
	parts := strings.Split(line, "|")
	if len(parts) < 2 {
		return false, 0, false
	}
	rec, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	replay, err2 := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err1 != nil || err2 != nil {
		return false, 0, false
	}
	return rec == 1, replay, true
}

func queryWalBytes(connURL string) (int64, bool) {
	out, err := runSQL(connURL, sqlWalBytes)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(firstSQLLine(out)), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func firstSQLLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

func formatWaitLine(status string, p postgres.ReplicaProgress) string {
	msg := strings.TrimSpace(p.Message)
	if msg == "" {
		msg = postgres.FormatProgress(p)
	}
	status = strings.TrimSpace(status)
	if status == "" {
		return msg
	}
	return fmt.Sprintf("%s\t%s", status, msg)
}
