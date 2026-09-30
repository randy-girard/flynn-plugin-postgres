package main

import (
	"errors"
	"net/http"
	"strings"

	"github.com/julienschmidt/httprouter"
	"github.com/randy-girard/flynn-plugin-postgres"
	"github.com/randy-girard/flynn/pkg/httphelper"
)

func (h *handler) afterFollow() func(*postgres.Instance) error {
	return func(fol *postgres.Instance) error {
		if h == nil || !h.live() {
			return nil
		}
		return h.startInstance(fol)
	}
}

func (h *handler) afterDrop() func(*postgres.Instance) error {
	return func(old *postgres.Instance) error {
		if h == nil || !h.live() || old == nil || strings.TrimSpace(old.App) == "" {
			return nil
		}
		_, err := h.client.DeleteApp(old.App)
		return err
	}
}

func (h *handler) upgradeOptions(mode postgres.ReplicationMode, runtime string) postgres.UpgradeOptions {
	return postgres.UpgradeOptions{
		Mode:        mode,
		Runtime:     runtime,
		AfterFollow: h.afterFollow(),
		AfterDrop:   h.afterDrop(),
	}
}

func (h *handler) upgrade(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	var body struct {
		Replication string `json:"replication"`
		Runtime     string `json:"runtime"`
	}
	_ = decode(r, &body)
	task, err := h.store.StartUpgrade(p.ByName("id"), h.upgradeOptions(postgres.ReplicationMode(body.Replication), body.Runtime))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	httphelper.JSON(w, http.StatusAccepted, task)
}

func (h *handler) getUpgrade(w http.ResponseWriter, _ *http.Request, p httprouter.Params) {
	task := h.store.LatestUpgrade(p.ByName("id"))
	if task == nil {
		writeAPIError(w, postgres.ErrNotFound)
		return
	}
	httphelper.JSON(w, 200, task)
}

func (h *handler) getTask(w http.ResponseWriter, _ *http.Request, p httprouter.Params) {
	task, err := h.store.GetTask(p.ByName("id"))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	httphelper.JSON(w, 200, task)
}

func (h *handler) listTasks(w http.ResponseWriter, _ *http.Request, _ httprouter.Params) {
	httphelper.JSON(w, 200, h.store.ListTasks())
}

func (h *handler) clusterUpgrades(w http.ResponseWriter, _ *http.Request, _ httprouter.Params) {
	started, skipped := h.beginClusterUpgrades()
	httphelper.JSON(w, http.StatusAccepted, map[string]any{
		"tasks":   started,
		"skipped": skipped,
	})
}

func (h *handler) autoStartClusterUpgrades() {
	if h == nil || !h.live() {
		return
	}
	go func() {
		started, skipped := h.beginClusterUpgrades()
		if h.log != nil {
			h.log.Info("cluster upgrades", "started", len(started), "skipped", len(skipped))
		}
	}()
}

func (h *handler) beginClusterUpgrades() (started []*postgres.Task, skipped []string) {
	if h == nil {
		return nil, nil
	}
	for _, inst := range h.upgradeCandidates() {
		if h.alreadyCurrent(inst) {
			skipped = append(skipped, inst.App)
			continue
		}
		task, err := h.store.StartUpgrade(inst.ID, h.upgradeOptions(postgres.ModeLogical, inst.Runtime))
		if errors.Is(err, postgres.ErrUpgradeInProgress) {
			if cur := h.store.LatestUpgrade(inst.ID); cur != nil {
				started = append(started, cur)
			}
			continue
		}
		if err != nil {
			skipped = append(skipped, inst.App+": "+err.Error())
			continue
		}
		started = append(started, task)
	}
	return started, skipped
}

func (h *handler) upgradeCandidates() []*postgres.Instance {
	seen := map[string]bool{}
	var out []*postgres.Instance
	add := func(inst *postgres.Instance) {
		if inst == nil || inst.Role == postgres.RoleFollower {
			return
		}
		key := inst.ID
		if key == "" {
			key = inst.App
		}
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		if inst.App != "" {
			seen[inst.App] = true
		}
		out = append(out, inst)
	}
	for _, inst := range h.store.Primaries() {
		add(inst)
	}
	if h.client == nil {
		return out
	}
	apps, err := h.client.AppList()
	if err != nil {
		return out
	}
	for _, app := range apps {
		if app == nil {
			continue
		}
		live := loadLivePostgres(h.client, app.Name)
		if live == nil {
			continue
		}
		if got, err := h.store.Get(live.ID); err == nil {
			add(got)
			continue
		}
		if live.App != "" {
			if got, err := h.store.Get(live.App); err == nil {
				add(got)
				continue
			}
		}
		add(live)
	}
	return out
}

func (h *handler) alreadyCurrent(inst *postgres.Instance) bool {
	if h == nil || !h.live() || inst == nil || strings.TrimSpace(inst.App) == "" {
		return false
	}
	rel, err := h.client.GetAppRelease(inst.App)
	if err != nil || rel == nil || len(rel.ArtifactIDs) == 0 {
		return false
	}
	if rel.Env != nil {
		if v := strings.TrimSpace(rel.Env["ENGINE_VERSION"]); postgres.NeedsEngineUpgrade(v, postgres.EngineVersion()) {
			return false
		}
	}
	if inst.EngineVersion != "" && postgres.NeedsEngineUpgrade(inst.EngineVersion, postgres.EngineVersion()) {
		return false
	}
	return rel.ArtifactIDs[0] == h.imageID
}

func (h *handler) version(w http.ResponseWriter, _ *http.Request, _ httprouter.Params) {
	httphelper.JSON(w, 200, h.versionReport())
}

func (h *handler) versionReport() postgres.VersionReport {
	rep := h.store.VersionReport()
	seen := map[string]bool{}
	for _, row := range rep.Instances {
		if row.App != "" {
			seen[row.App] = true
		}
		if row.ID != "" {
			seen[row.ID] = true
		}
	}
	for _, inst := range h.upgradeCandidates() {
		if inst == nil {
			continue
		}
		if seen[inst.App] || seen[inst.ID] {
			continue
		}
		v := strings.TrimSpace(inst.EngineVersion)
		if v == "" && h.live() && inst.App != "" {
			if rel, err := h.client.GetAppRelease(inst.App); err == nil && rel != nil && rel.Env != nil {
				v = strings.TrimSpace(rel.Env["ENGINE_VERSION"])
			}
		}
		row := postgres.InstanceVersion{
			ID:               inst.ID,
			App:              inst.App,
			Role:             string(inst.Role),
			Version:          v,
			UpgradeAvailable: postgres.NeedsEngineUpgrade(v, postgres.EngineVersion()),
			Followers:        len(inst.Followers),
		}
		if row.UpgradeAvailable {
			rep.UpgradeAvailable = true
		}
		rep.Instances = append(rep.Instances, row)
	}
	return rep
}
