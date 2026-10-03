package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
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
	return postgres.InstanceFromEnv(app.ID, app.Name, rel.Env)
}

func loadLivePostgresByScan(c appReleaseClient, idOrApp string) *postgres.Instance {
	idOrApp = strings.TrimSpace(idOrApp)
	lister, ok := c.(interface {
		AppList() ([]*ct.App, error)
	})
	if !ok || idOrApp == "" {
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

// instanceScaleOptions places the postgres process without waiting for job
// "up". The process type sets Service, so the scheduler keeps the job in
// starting until discoverd registration. That happens after initdb,
// bootstrap, and TLS — longer than ScaleStartingStuckTimeout (30s). A 5m
// ScaleAppRelease wait enables stall probes (timeout > DefaultDeployTimeout)
// and fails while postgres is still starting. resource:add returns after
// scale; pg:wait and the dashboard poll discoverd.
func instanceScaleOptions() ct.ScaleOptions {
	timeout := instanceReadyTimeout
	return ct.ScaleOptions{
		Processes: map[string]int{postgres.ProcessName: postgres.DefaultNodes},
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
	return startIsolatedInstance(h.client, h.imageID, inst, leader, waitInstanceReady)
}

func startIsolatedInstance(c instanceControl, imageID string, inst *postgres.Instance, leader *postgres.Instance, wait func(string, time.Duration) error) error {
	if c == nil {
		return fmt.Errorf("missing controller")
	}
	if inst == nil {
		return fmt.Errorf("missing instance")
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
	if leader != nil && inst.Role == postgres.RoleFollower {
		env["POSTGRES_PRIMARY_URL"] = leader.ConnectionURL()
		env["POSTGRES_ROLE"] = "follower"
		if leader.App != "" {
			env["POSTGRES_LEADER"] = leader.App
		}
	} else {
		env["POSTGRES_ROLE"] = "primary"
	}
	release := &ct.Release{
		ArtifactIDs: []string{imageID},
		Meta:        map[string]string{},
		Env:         env,
		Processes: map[string]ct.ProcessType{
			postgres.ProcessName: {
				Args:    []string{"/bin/start-flynn-postgres", "postgres"},
				Service: service,
				Ports:   []ct.Port{{Port: 5432, Proto: "tcp"}},
				Volumes: []ct.VolumeReq{{Path: postgres.VolumePath}},
			},
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
	if err := c.ScaleAppRelease(app.ID, release.ID, instanceScaleOptions()); err != nil {
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
	if inst.Role == postgres.RoleFollower && inst.LeaderID != "" && h.store != nil {
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
			if leader := strings.TrimSpace(rel.Env["POSTGRES_LEADER"]); leader != "" {
				keep[leader] = true
			}
		}
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
		if _, err := c.DeleteApp(app.ID); err != nil && !missingApp(err) {
			return deleted, err
		}
		deleted = append(deleted, app.Name)
	}
	return deleted, nil
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
