package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
	"github.com/randy-girard/flynn/discoverd/client"
)

// instanceReadyTimeout is how long provision waits for the new postgres
// process to register in discoverd after initdb and TLS setup.
const instanceReadyTimeout = 5 * time.Minute

type appReleaseClient interface {
	GetApp(string) (*ct.App, error)
	GetAppRelease(string) (*ct.Release, error)
}

func loadLivePostgres(c appReleaseClient, name string) *postgres.Instance {
	return loadLivePostgresDepth(c, name, 0)
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
	c := discoverd.NewClient()
	_, err := c.Instances(service, timeout)
	return err
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
// and fails while postgres is still starting. Readiness is
// waitInstanceReady, the same NoWait + ping pattern as plugin install.
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
		"POSTGRES_URL":      inst.ConnectionURL(),
		"ENGINE_VERSION":    postgres.EngineVersion(),
		"POSTGRES_VERSION":  postgres.EngineVersion(),
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
	if wait == nil {
		wait = waitInstanceReady
	}
	if err := wait(service, instanceReadyTimeout); err != nil {
		_, _ = c.DeleteApp(app.ID)
		return err
	}
	return nil
}
