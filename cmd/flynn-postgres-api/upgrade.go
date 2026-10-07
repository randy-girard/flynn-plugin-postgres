package main

import (
	"errors"
	"net/http"
	"strings"

	"github.com/julienschmidt/httprouter"
	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
	"github.com/randy-girard/flynn/pkg/httphelper"
)

func (h *handler) afterFollow() func(*postgres.Instance) error {
	return func(fol *postgres.Instance) error {
		if h == nil || !h.live() {
			return nil
		}
		err := h.startInstance(fol)
		if h.log != nil {
			app := ""
			if fol != nil {
				app = fol.App
			}
			if err != nil {
				h.log.Error("upgrade after-follow", "app", app, "err", err)
			} else {
				h.log.Info("upgrade after-follow", "app", app)
			}
		}
		return err
	}
}

func (h *handler) afterDrop() func(*postgres.Instance) error {
	return func(old *postgres.Instance) error {
		if h == nil || !h.live() || old == nil || strings.TrimSpace(old.App) == "" {
			return nil
		}
		_, err := h.client.DeleteApp(old.App)
		if h.log != nil {
			if err != nil {
				h.log.Error("upgrade after-drop", "app", old.App, "err", err)
			} else {
				h.log.Info("upgrade after-drop", "app", old.App)
			}
		}
		return err
	}
}

func (h *handler) afterReplace() func(old, nf *postgres.Instance) error {
	return func(old, nf *postgres.Instance) error {
		if h == nil || !h.live() {
			return nil
		}
		if err := h.cutoverAttachedApps(old, nf); err != nil {
			if h.log != nil {
				h.log.Error("upgrade after-replace cutover", "from", oldApp(old), "to", oldApp(nf), "err", err)
			}
			return err
		}
		return h.afterDrop()(old)
	}
}

func oldApp(inst *postgres.Instance) string {
	if inst == nil {
		return ""
	}
	return inst.App
}

func (h *handler) upgradeOptions(mode postgres.ReplicationMode, runtime string) postgres.UpgradeOptions {
	return postgres.UpgradeOptions{
		Mode:         mode,
		Runtime:      runtime,
		AfterFollow:  h.afterFollow(),
		AfterPromote: h.afterPromote(),
		AfterDrop:    h.afterDrop(),
		AfterReplace: h.afterReplace(),
	}
}

func (h *handler) imageRefreshOptions(inst *postgres.Instance) postgres.UpgradeOptions {
	runtime := ""
	if inst != nil {
		runtime = inst.Runtime
	}
	opts := h.upgradeOptions(postgres.ModeStreaming, runtime)
	opts.DropPrevious = true
	return opts
}

func (h *handler) afterPromote() func(*postgres.PromoteResult) error {
	return func(res *postgres.PromoteResult) error {
		if h == nil || !h.live() || res == nil || res.Promoted == nil {
			return nil
		}
		fromApp, toApp := "", res.Promoted.App
		if res.PreviousLeader != nil {
			fromApp = res.PreviousLeader.App
		}
		h.emitTopology(postgres.CodeReplicaReady, res.Promoted, fromApp, toApp, "")
		if err := h.promoteIsolatedJob(res.Promoted); err != nil {
			if h.log != nil {
				h.log.Error("upgrade after-promote stamp", "app", res.Promoted.App, "err", err)
			}
			return err
		}
		h.emitTopology(postgres.CodePromoted, res.Promoted, fromApp, toApp, "")
		if res.PreviousLeader != nil {
			h.emitTopology(postgres.CodeDeposed, res.PreviousLeader, fromApp, toApp, "")
		}
		err := h.cutoverAttachedApps(res.PreviousLeader, res.Promoted)
		if err != nil && h.log != nil {
			h.log.Error("upgrade after-promote cutover", "app", res.Promoted.App, "err", err)
		}
		return err
	}
}

func (h *handler) upgrade(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	var body struct {
		Replication string `json:"replication"`
		Runtime     string `json:"runtime"`
	}
	_ = decode(r, &body)
	task, err := h.startUpgrade(p.ByName("id"), h.upgradeOptions(postgres.ReplicationMode(body.Replication), body.Runtime))
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

// autoStartClusterUpgrades upgrades primaries whose ENGINE_VERSION is behind
// this plugin image. Image-only rebuilds use a streaming follower swap in
// refreshIsolatedImages (not a logical dump/restore). Tenant Flynn apps are
// never candidates: plugin:update restarts this API, and a CreateRelease /
// ScaleAppRelease on a user app can leave formation at zero if the worker
// dies mid-rollout.
func (h *handler) autoStartClusterUpgrades() {
	if h == nil || !h.live() {
		return
	}
	go func() {
		h.reapOrphanInstances()
		h.refreshIsolatedImages()
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
		task, err := h.startUpgrade(inst.ID, h.upgradeOptions(postgres.ModeLogical, inst.Runtime))
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

// isolatedUpgradeCandidate is a primary postgres instance app. Tenant Flynn
// apps must never be cluster-upgrade targets: plugin:update restarts this API
// and must not CreateRelease/ScaleAppRelease on user apps.
func isolatedUpgradeCandidate(inst *postgres.Instance) bool {
	if inst == nil || inst.Role == postgres.RoleFollower {
		return false
	}
	return postgres.IsolatedInstanceApp(inst.App)
}

func (h *handler) upgradeCandidates() []*postgres.Instance {
	seen := map[string]bool{}
	var out []*postgres.Instance
	add := func(inst *postgres.Instance) {
		if !isolatedUpgradeCandidate(inst) {
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
		if app == nil || !postgres.IsolatedInstanceApp(app.Name) {
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
	if inst == nil || strings.TrimSpace(inst.App) == "" {
		return false
	}
	if !h.live() {
		return skipClusterUpgrade(inst.EngineVersion)
	}
	rel, err := h.client.GetAppRelease(inst.App)
	return clusterUpgradeSkip(rel, err, inst.EngineVersion)
}

// clusterUpgradeSkip is the live-cluster boot decision. A missing or
// unreadable release is not "engine behind." Matching or unknown
// ENGINE_VERSION skips. The plugin image id on the release is irrelevant:
// plugin:update --rebuild always uploads a new layer, which used to start
// follow/promote on every instance.
func clusterUpgradeSkip(rel *ct.Release, err error, storedEngine string) bool {
	if err != nil || rel == nil {
		return true
	}
	installed := strings.TrimSpace(storedEngine)
	if rel.Env != nil {
		if v := strings.TrimSpace(rel.Env["ENGINE_VERSION"]); v != "" {
			installed = v
		}
	}
	return skipClusterUpgrade(installed)
}

// skipClusterUpgrade is true when the instance already runs this plugin's
// engine. A new plugin image (plugin:update --rebuild) is not an engine
// upgrade; follow/promote must not run on every API boot.
func skipClusterUpgrade(installedEngine string) bool {
	return !postgres.NeedsEngineUpgrade(installedEngine, postgres.EngineVersion())
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
