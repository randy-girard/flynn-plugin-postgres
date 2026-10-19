package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
	"github.com/randy-girard/flynn/pkg/dbruntime"
	"github.com/randy-girard/flynn/pkg/httphelper"
)

// instanceReadyTimeout is how long pg:wait will poll for the new postgres
// process to register in discoverd after initdb and TLS setup.
const instanceReadyTimeout = 5 * time.Minute

type appReleaseClient interface {
	GetApp(string) (*ct.App, error)
	GetAppRelease(string) (*ct.Release, error)
}

func loadLivePostgres(c appReleaseClient, name string) *postgres.Instance {
	if inst := loadLivePostgresDepth(c, name, 0); inst != nil {
		return inst
	}
	return loadLivePostgresByScan(c, name)
}

func loadLivePostgresDepth(c appReleaseClient, name string, depth int) *postgres.Instance {
	name = strings.TrimSpace(name)
	if c == nil || name == "" || depth > 2 {
		return nil
	}
	app, err := c.GetApp(name)
	if err != nil || app == nil {
		return nil
	}
	rel, err := c.GetAppRelease(app.ID)
	if err != nil || rel == nil || rel.Env == nil {
		return nil
	}
	ident := strings.TrimSpace(rel.Env["FLYNN_POSTGRES"])
	if ident != "" && ident != app.Name && ident != name {
		if inst := loadLivePostgresDepth(c, ident, depth+1); inst != nil {
			return inst
		}
	}
	if !postgres.IsolatedInstanceApp(app.Name) {
		return nil
	}
	return postgres.InstanceFromEnv(app.ID, app.Name, rel.Env)
}

func loadLivePostgresByScan(c appReleaseClient, idOrApp string) *postgres.Instance {
	idOrApp = strings.TrimSpace(idOrApp)
	if idOrApp == "" {
		return nil
	}
	if inst := loadLivePostgresByResourceID(c, idOrApp); inst != nil {
		return inst
	}
	lister, ok := c.(interface {
		AppList() ([]*ct.App, error)
	})
	if !ok {
		return nil
	}
	apps, err := lister.AppList()
	if err != nil {
		return nil
	}
	for _, app := range apps {
		if app == nil || !postgres.IsolatedInstanceApp(app.Name) {
			continue
		}
		if app.Name == idOrApp || app.ID == idOrApp {
			return loadLivePostgresDepth(c, app.Name, 0)
		}
		rel, err := c.GetAppRelease(app.ID)
		if err != nil || rel == nil || rel.Env == nil {
			continue
		}
		if rel.Env[postgres.ResourceIDEnv] == idOrApp || rel.Env["FLYNN_POSTGRES"] == idOrApp {
			return loadLivePostgresDepth(c, app.Name, 0)
		}
	}
	return nil
}

func loadLivePostgresByResourceID(c appReleaseClient, idOrApp string) *postgres.Instance {
	lister, ok := c.(interface {
		ResourceListAll() ([]*ct.Resource, error)
	})
	if !ok {
		return nil
	}
	list, err := lister.ResourceListAll()
	if err != nil {
		return nil
	}
	for _, r := range list {
		if !resourceMatchesInstance(r, idOrApp) {
			continue
		}
		name := ""
		if r.Env != nil {
			name = strings.TrimSpace(r.Env["FLYNN_POSTGRES"])
		}
		if name == "" {
			name = strings.TrimSpace(r.ExternalID)
		}
		if inst := loadLivePostgresDepth(c, name, 0); inst != nil {
			return inst
		}
	}
	return nil
}

func (h *handler) live() bool {
	return h != nil && h.client != nil && h.imageID != ""
}

// instanceControl is the controller subset startInstance needs. Tests fake it.
type instanceControl interface {
	CreateApp(*ct.App) error
	CreateRelease(string, *ct.Release) error
	ScaleAppRelease(string, string, ct.ScaleOptions) error
	SetAppRelease(string, string) error
	DeleteApp(string) (*ct.AppDeletion, error)
	GetApp(string) (*ct.App, error)
	GetAppRelease(string) (*ct.Release, error)
	JobList(string) ([]*ct.Job, error)
	JobListActive() ([]*ct.Job, error)
}

// waitInstanceReady waits until the postgres service has a discoverd
// instance. Tests replace it so provision does not talk to a cluster.
var waitInstanceReady = func(service string, timeout time.Duration) error {
	if os.Getenv("DISCOVERD_AUTH_KEY") == "" {
		return fmt.Errorf("DISCOVERD_AUTH_KEY is not set on the postgres plugin job; reinstall the plugin so discoverd Auth-Key is injected")
	}
	c := postgres.NewDiscoverdClient()
	_, err := c.Instances(service, timeout)
	return postgres.WrapDiscoverdAuth(err)
}

// peekDiscoverdInstances is a non-blocking ready check for pg:wait and
// dashboard progress. Tests replace it so they do not talk to a cluster.
var peekDiscoverdInstances = func(service string) bool {
	service = strings.TrimSpace(service)
	if service == "" {
		return false
	}
	insts, err := postgres.NewDiscoverdClient().Service(service).Instances()
	return err == nil && len(insts) > 0
}

type appDeleter interface {
	DeleteApp(string) (*ct.AppDeletion, error)
}

// startIsolatedAppDeletion enqueues controller app_deletion and returns.
// Client.DeleteApp waits up to 60s for EventTypeAppDeletion; resource:remove
// must not block on that teardown.
func startIsolatedAppDeletion(c appDeleter, name string) {
	name = strings.TrimSpace(name)
	if c == nil || name == "" {
		return
	}
	go func() {
		_, _ = c.DeleteApp(name)
	}()
}

// copyClusterDiscoverdEnv copies discoverd address/key from the plugin API
// process onto an isolated instance so RegisterInstance can authenticate
// (SEC-003). flynn-host also injects the key when the daemon has it.
func copyClusterDiscoverdEnv(env map[string]string) {
	if env == nil {
		return
	}
	for _, k := range []string{"DISCOVERD_AUTH_KEY", "DISCOVERD"} {
		if env[k] == "" {
			if v := os.Getenv(k); v != "" {
				env[k] = v
			}
		}
	}
}

// stampInstanceMetricsPort sets FLYNN_METRICS_PORT on an isolated instance
// release env so flynn-host can scrape GET /metrics/log. Every path that
// creates an instance release calls it. The port is not a process Port.
func stampInstanceMetricsPort(env map[string]string) {
	if env == nil {
		return
	}
	env[postgres.MetricsPortEnv] = postgres.DefaultMetricsPort
}

// instanceScaleOptions places the postgres process without waiting for job
// "up". The process type sets Service, so the scheduler keeps the job in
// starting until discoverd registration. That happens after initdb,
// bootstrap, and TLS — longer than ScaleStartingStuckTimeout (30s). A 5m
// ScaleAppRelease wait enables stall probes (timeout > DefaultDeployTimeout)
// and fails while postgres is still starting. resource:add returns after
// scale; pg:wait and the dashboard poll discoverd.
func instanceScaleOptions() ct.ScaleOptions {
	return instanceScaleOptionsWithTags(nil)
}

func instanceScaleOptionsWithTags(tags map[string]map[string]string) ct.ScaleOptions {
	timeout := instanceReadyTimeout
	return ct.ScaleOptions{
		Processes: map[string]int{postgres.ProcessName: postgres.DefaultNodes},
		Tags:      tags,
		Timeout:   &timeout,
		NoWait:    true,
	}
}

// startInstance creates one app, one volume, and one postgres process.
// Followers run pg_basebackup against the leader, then stream WAL.
func (h *handler) startInstance(inst *postgres.Instance) error {
	if h == nil {
		return fmt.Errorf("missing handler")
	}
	var leader *postgres.Instance
	if inst != nil && inst.LeaderID != "" && h.store != nil {
		leader, _ = h.store.Get(inst.LeaderID)
	}
	if inst != nil && inst.Role == postgres.RoleFollower && (leader == nil || strings.TrimSpace(leader.ConnectionURL()) == "") {
		return fmt.Errorf("follow requires a running primary")
	}
	tags, err := h.autoFailoverPlacementTags(inst, leader)
	if err != nil {
		return err
	}
	tags = dbruntime.MergeProcessTags(h.runtimePlacementTags(inst), tags)
	err = startIsolatedInstance(h.client, h.imageID, inst, leader, tags, waitInstanceReady)
	if err == nil && inst != nil && inst.Role == postgres.RoleFollower {
		leaderApp := ""
		if leader != nil {
			leaderApp = leader.App
		}
		h.emitTopology(postgres.CodeFollowerStarted, inst, leaderApp, inst.App, "")
	}
	return err
}

func startIsolatedInstance(c instanceControl, imageID string, inst *postgres.Instance, leader *postgres.Instance, tags map[string]map[string]string, wait func(string, time.Duration) error) error {
	if c == nil {
		return fmt.Errorf("missing controller")
	}
	if inst == nil {
		return fmt.Errorf("missing instance")
	}
	if !postgres.IsolatedInstanceApp(inst.App) {
		return fmt.Errorf("postgres plugin must not start tenant app %q as a datastore instance", inst.App)
	}
	db := "postgres"
	if len(inst.Databases) > 0 && inst.Databases[0].Name != "" {
		db = inst.Databases[0].Name
	}
	service := inst.App
	env := map[string]string{
		"FLYNN_POSTGRES":    service,
		"POSTGRES_USER":     inst.AppUser,
		"POSTGRES_PASSWORD": inst.AppPassword,
		"POSTGRES_DB":       db,
		"ENGINE_VERSION":    postgres.EngineVersion(),
		"POSTGRES_VERSION":  postgres.EngineVersion(),
	}
	if inst.ID != "" {
		env[postgres.ResourceIDEnv] = inst.ID
	}
	copyClusterDiscoverdEnv(env)
	stampInstanceMetricsPort(env)
	if leader != nil && inst.Role == postgres.RoleFollower {
		env["FLYNN_POSTGRES_PRIMARY_URL"] = leader.ConnectionURL()
		env["FLYNN_POSTGRES_ROLE"] = "follower"
		if leader.App != "" {
			env["FLYNN_POSTGRES_LEADER"] = leader.App
		}
	} else {
		env["FLYNN_POSTGRES_ROLE"] = "primary"
	}
	if inst.AutoFailover {
		env[postgres.EnvAutoFailover] = "true"
	}
	if inst.ReplicaPending {
		env["FLYNN_POSTGRES_REPLICA_PENDING"] = "true"
	}
	proc := ct.ProcessType{
		Args:    []string{"/bin/start-flynn-postgres", "postgres"},
		Service: service,
		Ports:   []ct.Port{{Port: 5432, Proto: "tcp"}},
		Volumes: []ct.VolumeReq{{Path: postgres.VolumePath}},
	}
	applyRuntimeResources(c, dbruntime.EnginePostgres, inst.Runtime, &proc)
	release := &ct.Release{
		ArtifactIDs: []string{imageID},
		Meta:        map[string]string{},
		Env:         env,
		Processes: map[string]ct.ProcessType{
			postgres.ProcessName: proc,
		},
	}
	app := &ct.App{
		Name:     service,
		Strategy: "one-down-one-up",
		Meta: map[string]string{
			"flynn-system-app":  "true",
			"flynn-datastore":   "true",
			"flynn-expose-port": "5432",
			"flynn-expose-tls":  "required",
		},
	}
	if err := c.CreateApp(app); err != nil {
		return err
	}
	if err := c.CreateRelease(app.ID, release); err != nil {
		_, _ = c.DeleteApp(app.ID)
		return err
	}
	if err := c.ScaleAppRelease(app.ID, release.ID, instanceScaleOptionsWithTags(tags)); err != nil {
		_, _ = c.DeleteApp(app.ID)
		return err
	}
	if err := c.SetAppRelease(app.ID, release.ID); err != nil {
		_, _ = c.DeleteApp(app.ID)
		return err
	}
	// Return after the job is scheduled. pg:wait and the dashboard poll
	// discoverd; resource:add must not block on initdb.
	_ = wait
	return nil
}

type isolatedReleaseClient interface {
	GetApp(string) (*ct.App, error)
	GetAppRelease(string) (*ct.Release, error)
	CreateRelease(string, *ct.Release) error
	SetAppRelease(string, string) error
}

func (h *handler) stampIsolatedRole(inst *postgres.Instance) error {
	if h == nil || h.client == nil || inst == nil {
		return nil
	}
	var leader *postgres.Instance
	if (inst.Role == postgres.RoleFollower || inst.Role == postgres.RoleDeposed) && inst.LeaderID != "" && h.store != nil {
		leader, _ = h.store.Get(inst.LeaderID)
	}
	return stampIsolatedRole(h.client, inst, leader)
}

// stampIsolatedRole rewrites the isolated postgres app release so the
// dashboard does not keep treating an unfollowed copy as a replica of its
// former leader (POSTGRES_LEADER / POSTGRES_PRIMARY_URL on that app).
func stampIsolatedRole(c isolatedReleaseClient, inst *postgres.Instance, leader *postgres.Instance) error {
	if c == nil || inst == nil || strings.TrimSpace(inst.App) == "" {
		return nil
	}
	if !postgres.IsolatedInstanceApp(inst.App) {
		return fmt.Errorf("postgres plugin must not rewrite tenant app %q", inst.App)
	}
	app, err := c.GetApp(inst.App)
	if err != nil || app == nil {
		return err
	}
	rel, err := c.GetAppRelease(app.ID)
	if err != nil || rel == nil {
		return err
	}
	env := map[string]string{}
	for k, v := range rel.Env {
		env[k] = v
	}
	applyPostgresResourceEnv(inst, env, leader)
	stampInstanceMetricsPort(env)
	if envEqual(rel.Env, env) {
		return nil
	}
	next := *rel
	next.ID = ""
	next.Env = env
	if err := c.CreateRelease(app.ID, &next); err != nil {
		return err
	}
	return c.SetAppRelease(app.ID, next.ID)
}

func (h *handler) promoteIsolatedJob(inst *postgres.Instance) error {
	if h == nil {
		return nil
	}
	return promoteIsolatedJob(h.client, inst, waitInstanceReady)
}

func promoteIsolatedJob(c isolatedImageClient, inst *postgres.Instance, wait func(string, time.Duration) error) error {
	if c == nil || inst == nil {
		return nil
	}
	app, err := c.GetApp(inst.App)
	if err != nil || app == nil {
		return err
	}
	oldRel, err := c.GetAppRelease(app.ID)
	if err != nil || oldRel == nil {
		return err
	}
	oldID := oldRel.ID
	if err := stampIsolatedRole(c, inst, nil); err != nil {
		return err
	}
	newRel, err := c.GetAppRelease(app.ID)
	if err != nil || newRel == nil {
		return err
	}
	if err := bounceIsolatedRelease(c, app.ID, oldID, newRel.ID, oldRel); err != nil {
		return err
	}
	if wait == nil {
		return nil
	}
	return wait(inst.App, instanceReadyTimeout)
}

func bounceIsolatedRelease(c isolatedImageClient, appID, oldReleaseID, newReleaseID string, oldRel *ct.Release) error {
	if c == nil {
		return nil
	}
	zeros := map[string]int{postgres.ProcessName: 0}
	if oldRel != nil {
		for name := range oldRel.Processes {
			zeros[name] = 0
		}
	}
	stopTimeout := 60 * time.Second
	if oldReleaseID != "" {
		if err := c.ScaleAppRelease(appID, oldReleaseID, ct.ScaleOptions{
			Processes: zeros,
			Timeout:   &stopTimeout,
		}); err != nil {
			return err
		}
	}
	return c.ScaleAppRelease(appID, newReleaseID, instanceScaleOptions())
}

func envEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			return false
		}
	}
	return true
}

func missingApp(err error) bool {
	if err == nil {
		return true
	}
	if httphelper.IsObjectNotFoundError(err) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "404")
}

type orphanReaper interface {
	AppList() ([]*ct.App, error)
	GetAppRelease(string) (*ct.Release, error)
	DeleteApp(string) (*ct.AppDeletion, error)
	ResourceListAll() ([]*ct.Resource, error)
}

func resourceKeepsInstance(res *ct.Resource, keep map[string]bool) {
	if res == nil || keep == nil {
		return
	}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s != "" {
			keep[s] = true
		}
	}
	add(res.ExternalID)
	if res.Env == nil {
		return
	}
	add(res.Env["FLYNN_POSTGRES"])
	add(res.Env[postgres.ResourceIDEnv])
}

// orphanFollowerGrace is how long an unreferenced follower may exist while
// an image refresh or follower replace is still copying. After that the
// replica is finished leftover and the reaper deletes it.
const orphanFollowerGrace = postgres.DefaultUpgradeTimeout

// keepNewestFollower marks one unreferenced follower of each kept primary
// while that copy is still inside orphanFollowerGrace. CreatedAt wins; a
// missing timestamp is not in progress. Equal timestamps break ties by name.
func keepNewestFollower(apps []*ct.App, releases map[string]*ct.Release, keep map[string]bool, isolated func(string) bool, leaderEnv string) {
	type cand struct {
		app     *ct.App
		created time.Time
	}
	groups := map[string][]cand{}
	for _, app := range apps {
		if app == nil || !isolated(app.Name) || keep[app.Name] || keep[app.ID] {
			continue
		}
		rel := releases[app.ID]
		if rel == nil || rel.Env == nil {
			continue
		}
		leader := strings.TrimSpace(rel.Env[leaderEnv])
		if leader == "" || !keep[leader] {
			continue
		}
		var created time.Time
		if app.CreatedAt != nil {
			created = app.CreatedAt.UTC()
		}
		groups[leader] = append(groups[leader], cand{app, created})
	}
	newer := func(a, b cand) bool {
		switch {
		case !a.created.IsZero() && b.created.IsZero():
			return true
		case a.created.IsZero() && !b.created.IsZero():
			return false
		case !a.created.Equal(b.created):
			return a.created.After(b.created)
		default:
			return a.app.Name > b.app.Name
		}
	}
	for _, group := range groups {
		best := 0
		for i := 1; i < len(group); i++ {
			if newer(group[i], group[best]) {
				best = i
			}
		}
		created := group[best].created
		if created.IsZero() || time.Since(created) > orphanFollowerGrace {
			continue
		}
		keep[group[best].app.Name] = true
		keep[group[best].app.ID] = true
	}
}

func reapOrphanPostgresApps(c orphanReaper) ([]string, error) {
	if c == nil {
		return nil, nil
	}
	resources, err := c.ResourceListAll()
	if err != nil {
		return nil, err
	}
	keep := map[string]bool{}
	for _, res := range resources {
		resourceKeepsInstance(res, keep)
	}
	apps, err := c.AppList()
	if err != nil {
		return nil, err
	}
	releases := map[string]*ct.Release{}
	for _, app := range apps {
		if app == nil || !postgres.IsolatedInstanceApp(app.Name) {
			continue
		}
		rel, relErr := c.GetAppRelease(app.ID)
		if relErr != nil || rel == nil {
			continue
		}
		releases[app.ID] = rel
		if rel.Env == nil {
			rel.Env = map[string]string{}
		}
		if keep[app.Name] || keep[app.ID] || keep[rel.Env[postgres.ResourceIDEnv]] || keep[rel.Env["FLYNN_POSTGRES"]] {
			keep[app.Name] = true
			if leader := strings.TrimSpace(rel.Env["FLYNN_POSTGRES_LEADER"]); leader != "" {
				keep[leader] = true
			}
		}
	}
	// An image refresh or a broken-follower replace creates one replica that is
	// not a Resource yet. Keep it only while that copy is still in progress.
	// A finished leftover is deleted, including one left behind by a failed run.
	keepNewestFollower(apps, releases, keep, postgres.IsolatedInstanceApp, "FLYNN_POSTGRES_LEADER")
	// An empty resource list is not proof that every database is unused.
	if len(resources) == 0 && len(releases) > 0 {
		return nil, errors.New("no resources listed while postgres instance apps exist")
	}
	var deleted []string
	for _, app := range apps {
		if app == nil || !postgres.IsolatedInstanceApp(app.Name) {
			continue
		}
		if app.Meta["flynn-plugin"] == "true" {
			continue
		}
		rel := releases[app.ID]
		if keep[app.Name] || keep[app.ID] {
			continue
		}
		if rel != nil && rel.Env != nil && (keep[rel.Env["FLYNN_POSTGRES"]] || keep[rel.Env[postgres.ResourceIDEnv]]) {
			continue
		}
		// A primary holds the data. Only a finished follower may be reaped.
		if !orphanFollowerRelease(rel) {
			continue
		}
		if who, refErr := ReferencingApp(c, app.Name); refErr != nil {
			return deleted, refErr
		} else if who != "" {
			continue
		}
		if _, err := c.DeleteApp(app.ID); err != nil && !missingApp(err) {
			return deleted, err
		}
		deleted = append(deleted, app.Name)
	}
	return deleted, nil
}

func orphanFollowerRelease(rel *ct.Release) bool {
	if rel == nil || rel.Env == nil {
		return false
	}
	return rel.Env["FLYNN_POSTGRES_ROLE"] == string(postgres.RoleFollower) || strings.TrimSpace(rel.Env["FLYNN_POSTGRES_LEADER"]) != ""
}

func (h *handler) reapOrphanInstances() {
	if h == nil || h.client == nil {
		return
	}
	deleted, err := reapOrphanPostgresApps(h.client)
	if h.log != nil {
		if err != nil {
			h.log.Error("reap orphan postgres apps", "err", err, "deleted", len(deleted))
			return
		}
		if len(deleted) > 0 {
			h.log.Info("reaped orphan postgres instance apps", "count", len(deleted), "apps", strings.Join(deleted, ","))
		}
	}
}

func applyRuntimeResources(c any, engine, name string, proc *ct.ProcessType) {
	if proc == nil {
		return
	}
	type cataloguer interface {
		ListDBRuntimes() (*dbruntime.Catalog, error)
	}
	g, ok := c.(cataloguer)
	if !ok {
		return
	}
	cat, err := g.ListDBRuntimes()
	if err != nil || cat == nil {
		return
	}
	if r := dbruntime.ProcessResources(cat, engine, name); r != nil {
		proc.Resources = r
	}
}

func (h *handler) runtimePlacementTags(inst *postgres.Instance) map[string]map[string]string {
	if h == nil || h.client == nil || inst == nil {
		return nil
	}
	cat, err := h.client.ListDBRuntimes()
	if err != nil {
		return nil
	}
	return dbruntime.CatalogProcessTags(cat, dbruntime.EnginePostgres, inst.Runtime, postgres.ProcessName)
}

func (h *handler) autoFailoverPlacementTags(inst, leader *postgres.Instance) (map[string]map[string]string, error) {
	if inst == nil || !inst.AutoFailover || leader == nil || strings.TrimSpace(leader.App) == "" {
		return nil, nil
	}
	if h == nil || h.client == nil {
		return nil, postgres.ErrAutoFailoverNoHost
	}
	host, err := runningAppHost(h.client, leader.App)
	if err != nil {
		return nil, err
	}
	if host == "" || !clusterHasOtherHost(h.client, host) {
		return nil, postgres.ErrAutoFailoverNoHost
	}
	return map[string]map[string]string{
		postgres.ProcessName: {"flynn-avoid-host-ids": host},
	}, nil
}

type isolatedImageClient interface {
	GetApp(string) (*ct.App, error)
	GetAppRelease(string) (*ct.Release, error)
	CreateRelease(string, *ct.Release) error
	ScaleAppRelease(string, string, ct.ScaleOptions) error
	SetAppRelease(string, string) error
}

func isolatedImageStale(rel *ct.Release, imageID, storedEngine string) bool {
	if rel == nil || strings.TrimSpace(imageID) == "" || len(rel.ArtifactIDs) == 0 {
		return false
	}
	if strings.TrimSpace(rel.ArtifactIDs[0]) == strings.TrimSpace(imageID) {
		return false
	}
	return clusterUpgradeSkip(rel, nil, storedEngine)
}

// imageRefreshAction is swap (follower promote) for a stale primary, inplace
// bounce for a leftover replica, or skip when the squashfs already matches.
func imageRefreshAction(inst *postgres.Instance, rel *ct.Release, imageID string) string {
	if inst == nil || !isolatedImageStale(rel, imageID, inst.EngineVersion) {
		return "skip"
	}
	if inst.Role == postgres.RoleFollower {
		return "inplace"
	}
	return "swap"
}

func (h *handler) refreshIsolatedImages() {
	if h == nil || !h.live() {
		return
	}
	swapping := map[string]bool{}
	for _, inst := range h.imageRefreshCandidates() {
		if inst == nil || inst.Role == postgres.RoleFollower {
			continue
		}
		rel, err := h.client.GetAppRelease(inst.App)
		if err != nil || imageRefreshAction(inst, rel, h.imageID) != "swap" {
			continue
		}
		if imageSwapReplicaExists(h.client, inst.App, h.imageID) {
			markImageSwap(swapping, inst)
			if h.log != nil {
				h.log.Info("image refresh follower-swap already in progress", "app", inst.App)
			}
			continue
		}
		if !claimImageSwap(h.client, inst.App, imageSwapClaimToken()) {
			markImageSwap(swapping, inst)
			if h.log != nil {
				h.log.Info("image refresh follower-swap claimed by another worker", "app", inst.App)
			}
			continue
		}
		if imageSwapReplicaExists(h.client, inst.App, h.imageID) {
			markImageSwap(swapping, inst)
			if h.log != nil {
				h.log.Info("image refresh follower-swap already in progress", "app", inst.App)
			}
			continue
		}
		h.emitTopology(postgres.CodeSwapStarted, inst, inst.App, "", "reason=image-refresh action=create-replica")
		task, err := h.startUpgrade(inst.ID, h.imageRefreshOptions(inst))
		if err == nil || errors.Is(err, postgres.ErrUpgradeInProgress) {
			markImageSwap(swapping, inst)
			if err == nil && task != nil {
				h.watchUpgradeTask(inst.App, task.ID)
			}
			continue
		}
		h.emitTopology(postgres.CodeSwapFailed, inst, inst.App, "", "reason=image-refresh err="+err.Error())
		if h.log != nil {
			h.log.Error("image refresh follower-swap", "app", inst.App, "err", err)
		}
		if err := refreshIsolatedPluginImage(h.client, h.imageID, inst); err != nil && h.log != nil {
			h.log.Error("refresh isolated image", "app", inst.App, "err", err)
		}
	}
	for _, inst := range h.imageRefreshCandidates() {
		if inst == nil || swapping[inst.ID] || swapping[inst.App] || swapping[inst.LeaderID] {
			continue
		}
		if inst.Role != postgres.RoleFollower {
			continue
		}
		if skip := h.skipFollowerReplace(inst); skip != "" {
			continue
		}
		if !h.followerStreaming(inst) {
			// A crash-looping replica is not streaming, so recreate waits until
			// it times out and the stale image never gets replaced. Bounce
			// onto the current image first; start.sh restores standby.signal.
			if rel, err := h.client.GetAppRelease(inst.App); err == nil && imageRefreshAction(inst, rel, h.imageID) == "inplace" {
				if err := refreshIsolatedPluginImage(h.client, h.imageID, inst); err != nil && h.log != nil {
					h.log.Error("refresh isolated image", "app", inst.App, "err", err)
				}
				continue
			}
			primary, err := h.store.Get(inst.LeaderID)
			if err == nil && primary != nil {
				if err := h.recreateFollower(inst, primary); err != nil && h.log != nil {
					h.log.Error("replace leftover follower", "app", inst.App, "err", err)
				}
			}
			continue
		}
		if err := refreshIsolatedPluginImage(h.client, h.imageID, inst); err != nil && h.log != nil {
			h.log.Error("refresh isolated image", "app", inst.App, "err", err)
		}
	}
}

func markImageSwap(swapping map[string]bool, inst *postgres.Instance) {
	if swapping == nil || inst == nil {
		return
	}
	swapping[inst.ID] = true
	swapping[inst.App] = true
	for _, fid := range inst.Followers {
		swapping[fid] = true
	}
}

type appListReleaser interface {
	AppList() ([]*ct.App, error)
	GetAppRelease(string) (*ct.Release, error)
}

// imageSwapReplicaExists is true when another worker already started a
// follower of this primary on the current plugin image.
func imageSwapReplicaExists(c appListReleaser, leaderApp, imageID string) bool {
	if c == nil {
		return false
	}
	leaderApp = strings.TrimSpace(leaderApp)
	imageID = strings.TrimSpace(imageID)
	if leaderApp == "" || imageID == "" {
		return false
	}
	apps, err := c.AppList()
	if err != nil {
		return false
	}
	for _, app := range apps {
		if app == nil || app.Name == leaderApp || !postgres.IsolatedInstanceApp(app.Name) {
			continue
		}
		rel, relErr := c.GetAppRelease(app.ID)
		if relErr != nil || rel == nil || rel.Env == nil {
			continue
		}
		if strings.TrimSpace(rel.Env["FLYNN_POSTGRES_LEADER"]) != leaderApp {
			continue
		}
		if len(rel.ArtifactIDs) > 0 && strings.TrimSpace(rel.ArtifactIDs[0]) == imageID {
			return true
		}
	}
	return false
}

const imageSwapMetaKey = "flynn-pg-image-swap"

var imageSwapSettle = 500 * time.Millisecond

type appMetaClient interface {
	GetApp(string) (*ct.App, error)
	UpdateAppMeta(*ct.App) error
}

func imageSwapClaimToken() string {
	if id := strings.TrimSpace(os.Getenv("FLYNN_JOB_ID")); id != "" {
		return id
	}
	host, _ := os.Hostname()
	if host = strings.TrimSpace(host); host != "" {
		return host
	}
	return "postgres-api"
}

func claimImageSwap(c appMetaClient, appName, token string) bool {
	if c == nil {
		return true
	}
	appName = strings.TrimSpace(appName)
	token = strings.TrimSpace(token)
	if appName == "" || token == "" {
		return false
	}
	app, err := c.GetApp(appName)
	if err != nil || app == nil {
		return false
	}
	if app.Meta == nil {
		app.Meta = map[string]string{}
	}
	app.Meta[imageSwapMetaKey] = token
	if err := c.UpdateAppMeta(app); err != nil {
		return false
	}
	if imageSwapSettle > 0 {
		time.Sleep(imageSwapSettle)
	}
	app, err = c.GetApp(appName)
	if err != nil || app == nil || app.Meta == nil {
		return false
	}
	return strings.TrimSpace(app.Meta[imageSwapMetaKey]) == token
}

func clearImageSwapClaim(c appMetaClient, appName string) {
	if c == nil {
		return
	}
	app, err := c.GetApp(appName)
	if err != nil || app == nil {
		return
	}
	if app.Meta == nil || strings.TrimSpace(app.Meta[imageSwapMetaKey]) == "" {
		return
	}
	delete(app.Meta, imageSwapMetaKey)
	_ = c.UpdateAppMeta(app)
}

func (h *handler) watchUpgradeTask(app, taskID string) {
	if h == nil || h.store == nil || strings.TrimSpace(taskID) == "" {
		return
	}
	if h.log != nil {
		h.log.Info("image refresh follower-swap started", "app", app, "task", taskID)
	}
	var src *postgres.Instance
	if h.store != nil {
		src, _ = h.store.Get(app)
	}
	go func() {
		for {
			task, err := h.store.GetTask(taskID)
			if err != nil || task == nil || task.Status == postgres.TaskDone || task.Status == postgres.TaskFailed {
				clearImageSwapClaim(h.client, app)
				if h.log != nil {
					if task != nil && task.Status == postgres.TaskFailed {
						h.log.Error("image refresh follower-swap failed", "app", app, "task", taskID, "err", task.Error)
					} else if task != nil {
						h.log.Info("image refresh follower-swap done", "app", app, "task", taskID)
					}
				}
				if task != nil && (task.Status == postgres.TaskDone || task.Status == postgres.TaskFailed) {
					promoted := ""
					if task.Status == postgres.TaskDone && src != nil {
						if cur, gerr := h.store.Get(src.ID); gerr == nil && cur != nil {
							promoted = cur.App
						}
					}
					code := postgres.CodeSwapDone
					extra := "task=" + taskID
					if task.Status == postgres.TaskFailed {
						code = postgres.CodeSwapFailed
						if task.Error != "" {
							extra += " err=" + task.Error
						}
					}
					h.emitTopology(code, src, app, promoted, extra)
				}
				return
			}
			time.Sleep(time.Second)
		}
	}()
}

func (h *handler) imageRefreshCandidates() []*postgres.Instance {
	seen := map[string]bool{}
	var out []*postgres.Instance
	add := func(inst *postgres.Instance) {
		if inst == nil || !postgres.IsolatedInstanceApp(inst.App) {
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
	if h.store != nil {
		for _, inst := range h.store.All() {
			add(inst)
		}
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
		add(loadLivePostgres(h.client, app.Name))
	}
	return out
}

// refreshIsolatedPluginImage moves an existing isolated postgres app onto the
// current plugin squashfs without a logical engine upgrade. plugin:update
// --rebuild only restarts postgres-plugin; this is what picks up
// the GET /metrics/log endpoint in already-provisioned databases.
func refreshIsolatedPluginImage(c isolatedImageClient, imageID string, inst *postgres.Instance) error {
	if c == nil || inst == nil || strings.TrimSpace(imageID) == "" {
		return nil
	}
	if !postgres.IsolatedInstanceApp(inst.App) {
		return fmt.Errorf("postgres plugin must not rewrite tenant app %q", inst.App)
	}
	app, err := c.GetApp(inst.App)
	if err != nil || app == nil {
		return err
	}
	rel, err := c.GetAppRelease(app.ID)
	if err != nil || rel == nil {
		return err
	}
	if !isolatedImageStale(rel, imageID, inst.EngineVersion) {
		return nil
	}
	next := *rel
	next.ID = ""
	next.ArtifactIDs = []string{imageID}
	next.Env = make(map[string]string, len(rel.Env)+1)
	for k, v := range rel.Env {
		next.Env[k] = v
	}
	stampInstanceMetricsPort(next.Env)
	if err := c.CreateRelease(app.ID, &next); err != nil {
		return err
	}
	zeros := map[string]int{postgres.ProcessName: 0}
	for name := range rel.Processes {
		zeros[name] = 0
	}
	stopTimeout := 60 * time.Second
	if err := c.ScaleAppRelease(app.ID, rel.ID, ct.ScaleOptions{
		Processes: zeros,
		Timeout:   &stopTimeout,
	}); err != nil {
		return err
	}
	if err := c.ScaleAppRelease(app.ID, next.ID, instanceScaleOptions()); err != nil {
		return err
	}
	return c.SetAppRelease(app.ID, next.ID)
}
