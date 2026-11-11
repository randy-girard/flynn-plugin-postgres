package main

import (
	"os"
	"sync/atomic"
	"time"

	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
	discoverd "github.com/randy-girard/flynn/discoverd/client"
)

const backupServiceName = "postgres-backup"

func (h *handler) startBackupSchedule() {
	if h == nil || h.client == nil || os.Getenv("FLYNN_BACKUP_SCHEDULE") == "off" {
		return
	}
	go h.backupLeaderLoop()
}

func (h *handler) backupLeaderLoop() {
	var leader atomic.Bool
	h.backupLeader = leader.Load
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}
	addr := ":" + port
	for {
		dc := postgres.NewDiscoverdClient()
		hb, err := dc.AddServiceAndRegister(backupServiceName, addr)
		if err != nil {
			if h.log != nil {
				h.log.Error("postgres backup lease", "err", err)
			}
			time.Sleep(time.Second)
			continue
		}
		self := hb.Addr()
		leaders := make(chan *discoverd.Instance)
		stream, err := dc.Service(backupServiceName).Leaders(leaders)
		if err != nil {
			_ = hb.Close()
			time.Sleep(time.Second)
			continue
		}
		ticker := time.NewTicker(postgres.BackupTick)
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
				isLeader := inst.Addr == self
				leader.Store(isLeader)
				if isLeader {
					go h.runBackupPass(time.Now().UTC())
				}
			case now := <-ticker.C:
				if leader.Load() {
					h.runBackupPass(now.UTC())
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

func (h *handler) runBackupPass(now time.Time) {
	if h == nil || h.backupLeader == nil || !h.backupLeader() {
		return
	}
	resources, err := h.postgresResources()
	if err != nil {
		if h.log != nil {
			h.log.Error("list postgres resources for backup", "err", err)
		}
		return
	}
	statuses, err := h.backupStatusMap()
	if err != nil {
		if h.log != nil {
			h.log.Error("list tenant backup status", "err", err)
		}
		return
	}
	extra := map[string]bool{}
	h.backupInflight.Range(func(key, _ any) bool {
		if id, ok := key.(string); ok {
			extra[id] = true
		}
		return true
	})
	for _, id := range planBackupPass(now, resources, statuses, extra, func(res *ct.Resource) string {
		app := ""
		if res != nil && res.Env != nil {
			app = res.Env["FLYNN_POSTGRES"]
		}
		return h.hostFor(res, app)
	}) {
		if _, loaded := h.backupInflight.LoadOrStore(id, true); loaded {
			continue
		}
		go func(resourceID string) {
			defer h.backupInflight.Delete(resourceID)
			if _, err := h.currentBackups().Create(resourceID); err != nil && h.log != nil {
				h.log.Error("scheduled tenant backup", "err", err, "resource", resourceID)
			}
		}(id)
	}
}

func (h *handler) backupStatusMap() (map[string]postgres.BackupStatus, error) {
	if h.backupStatuses != nil {
		return h.backupStatuses()
	}
	list, err := newHTTPStatus().List()
	if err != nil {
		return nil, err
	}
	out := map[string]postgres.BackupStatus{}
	for _, st := range list {
		out[st.ResourceID] = st
	}
	return out, nil
}

func planBackupPass(now time.Time, resources []*ct.Resource, statuses map[string]postgres.BackupStatus, extraRunning map[string]bool, hostOf func(*ct.Resource) string) []string {
	var cands []postgres.BackupCandidate
	for _, res := range resources {
		if res == nil || res.ID == "" || !resourceIsPrimary(res) {
			continue
		}
		st := statuses[res.ID]
		host := ""
		if hostOf != nil {
			host = hostOf(res)
		}
		cands = append(cands, postgres.BackupCandidate{
			ResourceID: res.ID,
			HostID:     host,
			Due:        postgres.BackupDue(st.LastSuccessAt, now, res.ID),
			Running:    extraRunning[res.ID] || postgres.RunningFresh(st, now),
		})
	}
	return postgres.AdmitBackups(cands, postgres.BackupHostCap)
}

func resourceIsPrimary(res *ct.Resource) bool {
	if res == nil {
		return false
	}
	role := ""
	if res.Env != nil {
		role = res.Env["FLYNN_POSTGRES_ROLE"]
		if role == "" {
			role = res.Env["POSTGRES_ROLE"]
		}
	}
	return postgres.PrimaryRole(role)
}
