package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/inconshreveable/log15"
	"github.com/julienschmidt/httprouter"
	"github.com/randy-girard/flynn-plugin-postgres"
	"github.com/randy-girard/flynn-plugin-postgres/internal/dashui"
	"github.com/randy-girard/flynn/controller/client"
	ct "github.com/randy-girard/flynn/controller/types"
	"github.com/randy-girard/flynn/pkg/httphelper"
	"github.com/randy-girard/flynn/pkg/shutdown"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "task" {
		if err := runPluginTask(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if dashui.DevEnabled() {
		runDashboardDev()
		return
	}
	log := log15.New("app", "postgres-api")
	store := postgres.NewStore()
	h := newHandler(store)
	h.log = log
	h.imageID = os.Getenv("POSTGRES_IMAGE_ID")
	if key := os.Getenv("CONTROLLER_KEY"); key != "" && h.imageID != "" {
		client, err := controller.NewClient("", key)
		if err != nil {
			shutdown.Fatal(err)
		}
		h.client = client
		store.NameTaken = func(name string) bool {
			_, err := client.GetApp(name)
			return err == nil
		}
		store.LoadMissing = func(name string) *postgres.Instance {
			return loadLivePostgres(client, name)
		}
	}
	addr := ":3000"
	if port := os.Getenv("PORT"); port != "" {
		addr = ":" + port
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		shutdown.Fatal(err)
	}
	log.Info("listening", "addr", addr, "provider", postgres.ProviderURL())
	h.autoStartClusterUpgrades()
	go h.metricsLoop()
	h.startFailoverWatch()
	if err := http.Serve(ln, h); err != nil && !errors.Is(err, http.ErrServerClosed) {
		shutdown.Fatal(err)
	}
}

type handler struct {
	store            *postgres.Store
	router           *httprouter.Router
	client           controller.Client
	imageID          string
	log              log15.Logger
	listResources    func(app string) ([]*ct.Resource, error)
	listAllResources func() ([]*ct.Resource, error)
	appDisplayName   func(string) string
	appReleaseEnv    func(string) map[string]string
	liveApps         followerAppLister
}

func newHandler(store *postgres.Store) *handler {
	h := &handler{store: store, router: httprouter.New()}
	if store != nil {
		store.SetLiveProgress(h.liveReplicaProgress)
	}
	h.router.GET("/ping", func(w http.ResponseWriter, _ *http.Request, _ httprouter.Params) {
		w.WriteHeader(http.StatusOK)
	})
	h.router.POST("/databases", h.provision)
	h.router.DELETE("/databases", h.deprovision)
	h.router.GET("/databases/:id", h.info)
	h.router.POST("/databases/:id/users", h.addUser)
	h.router.GET("/databases/:id/users", h.listUsers)
	h.router.POST("/databases/:id/databases", h.addDatabase)
	h.router.POST("/databases/:id/write", h.write)
	h.router.POST("/databases/:id/wait", h.wait)
	h.router.GET("/databases/:id/progress", h.progress)
	h.router.POST("/databases/:id/follow", h.follow)
	h.router.POST("/databases/:id/promote", h.promote)
	h.router.POST("/databases/:id/unfollow", h.unfollow)
	h.router.POST("/databases/:id/attach", h.attach)
	h.router.POST("/databases/:id/detach", h.detach)
	h.router.POST("/databases/:id/env-set", h.envSet)
	h.router.POST("/databases/:id/upgrade", h.upgrade)
	h.router.GET("/databases/:id/upgrade", h.getUpgrade)
	h.router.GET("/tasks", h.listTasks)
	h.router.GET("/tasks/:id", h.getTask)
	h.router.POST("/cluster/upgrades", h.clusterUpgrades)
	h.router.GET("/cluster/upgrades", h.listTasks)
	h.router.GET("/version", h.version)
	h.mountDashboard()
	return h
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.router.ServeHTTP(w, r)
}

type provisionBody struct {
	Platform     bool   `json:"platform"`
	App          string `json:"app"`
	Tenant       string `json:"tenant"`
	As           string `json:"as"`
	Follow       string `json:"follow"`
	Runtime      string `json:"runtime"`
	Replication  string `json:"replication"`
	AutoFailover bool   `json:"auto_failover"`
}

func (h *handler) provision(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	var body provisionBody
	if err := decode(r, &body); err != nil {
		writeAPIError(w, err)
		return
	}
	if body.Platform || strings.Contains(postgres.ProviderURL(), postgres.PlatformApplianceHost) {
		writeAPIError(w, postgres.ErrPlatformAppliance)
		return
	}
	inst, env, err := h.store.Provision(postgres.ProvisionRequest{
		Tenant:       body.Tenant,
		App:          body.App,
		As:           body.As,
		Follow:       body.Follow,
		Mode:         postgres.ReplicationMode(body.Replication),
		Runtime:      body.Runtime,
		AutoFailover: body.AutoFailover,
	})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if h.live() {
		if err := h.startInstance(inst); err != nil {
			writeAPIError(w, err)
			return
		}
	}
	if len(env) == 0 {
		env = postgres.AttachmentKeys(body.As, "", "", inst.ConnectionURL(), nil, true)
	}
	var leader *postgres.Instance
	if inst.LeaderID != "" {
		leader, _ = h.store.Get(inst.LeaderID)
	}
	applyPostgresResourceEnv(inst, env, leader)
	stripTenantPostgresCredentials(env)
	httphelper.JSON(w, 200, map[string]any{
		"id":   inst.ID,
		"env":  env,
		"plan": inst.NodePlan(),
		"host": inst.ServiceHost,
	})
}

func (h *handler) deprovision(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	id = strings.TrimPrefix(id, "/databases/")
	if id == "" {
		writeAPIError(w, postgres.ErrNotFound)
		return
	}
	inst, err := h.store.Get(id)
	if err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			if postgres.IsolatedInstanceApp(id) {
				w.WriteHeader(http.StatusOK)
				return
			}
			writeAPIError(w, err)
			return
		}
		writeAPIError(w, err)
		return
	}
	if err := postgres.CanDeleteResource(inst, h.followerApps(inst)); err != nil {
		writeAPIError(w, err)
		return
	}
	if h.live() && h.client != nil && strings.TrimSpace(inst.App) != "" {
		startIsolatedAppDeletion(h.client, inst.App)
	}
	h.store.Forget(inst.ID)
	w.WriteHeader(http.StatusOK)
}

type followerAppLister interface {
	AppList() ([]*ct.App, error)
	GetAppRelease(string) (*ct.Release, error)
}

func (h *handler) followerLister() followerAppLister {
	if h == nil {
		return nil
	}
	if h.liveApps != nil {
		return h.liveApps
	}
	if h.client != nil {
		return h.client
	}
	return nil
}

func (h *handler) followerApps(inst *postgres.Instance) []string {
	if inst == nil {
		return nil
	}
	if lister := h.followerLister(); lister != nil {
		live, err := liveFollowerAppNames(lister, inst)
		if err == nil {
			if h.store != nil {
				h.store.ReconcileFollowers(inst.ID, live)
			}
			return live
		}
	}
	if h.store == nil {
		return nil
	}
	return h.store.FollowerApps(inst.ID)
}

func liveFollowerAppNames(c followerAppLister, inst *postgres.Instance) ([]string, error) {
	if c == nil || inst == nil {
		return nil, nil
	}
	apps, err := c.AppList()
	if err != nil {
		return nil, err
	}
	leader := strings.TrimSpace(inst.App)
	leaderID := strings.TrimSpace(inst.ID)
	var names []string
	seen := map[string]bool{}
	for _, app := range apps {
		if app == nil {
			continue
		}
		rel, err := c.GetAppRelease(app.ID)
		if err != nil || rel == nil || rel.Env == nil {
			continue
		}
		pointsAt := strings.TrimSpace(rel.Env["POSTGRES_LEADER"])
		if pointsAt == "" {
			continue
		}
		if !strings.EqualFold(pointsAt, leader) && !strings.EqualFold(pointsAt, leaderID) {
			continue
		}
		n := strings.TrimSpace(app.Name)
		if n == "" || n == leader || seen[n] {
			continue
		}
		seen[n] = true
		names = append(names, n)
	}
	return names, nil
}

func (h *handler) info(w http.ResponseWriter, _ *http.Request, p httprouter.Params) {
	id := p.ByName("id")
	info, err := h.store.Info(id)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if views := h.controllerAttachmentViews(id, info.App, info.ID); len(views) > 0 {
		info.Attachments = views
	}
	httphelper.JSON(w, 200, info)
}

func (h *handler) addUser(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	var body struct {
		Name     string `json:"name"`
		Password string `json:"password"`
		Database string `json:"database"`
	}
	if err := decode(r, &body); err != nil {
		writeAPIError(w, err)
		return
	}
	if err := h.createUserOnInstance(p.ByName("id"), body.Name, body.Password, body.Database); err != nil {
		writeAPIError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *handler) listUsers(w http.ResponseWriter, _ *http.Request, p httprouter.Params) {
	users, err := h.store.Users(p.ByName("id"))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	httphelper.JSON(w, 200, users)
}

func (h *handler) addDatabase(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	var body struct {
		Name string `json:"name"`
	}
	if err := decode(r, &body); err != nil {
		writeAPIError(w, err)
		return
	}
	if err := h.createLogicalDatabase(p.ByName("id"), body.Name); err != nil {
		writeAPIError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *handler) write(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	var body struct {
		DB    string `json:"db"`
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := decode(r, &body); err != nil {
		writeAPIError(w, err)
		return
	}
	if err := h.store.Write(p.ByName("id"), body.DB, body.Key, body.Value); err != nil {
		writeAPIError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *handler) wait(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	ctx := r.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, postgres.DefaultUpgradeTimeout)
	defer cancel()
	if err := h.store.Wait(ctx, p.ByName("id")); err != nil {
		writeAPIError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *handler) follow(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	var body struct {
		App          string `json:"app"`
		Runtime      string `json:"runtime"`
		AutoFailover bool   `json:"auto_failover"`
	}
	if err := decode(r, &body); err != nil {
		writeAPIError(w, err)
		return
	}
	env, err := h.provisionFollow(strings.TrimSpace(body.App), "", strings.TrimSpace(p.ByName("id")), strings.TrimSpace(body.Runtime), body.AutoFailover)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	httphelper.JSON(w, 200, map[string]any{"env": env})
}

// provisionFollow creates a streaming replica the same way the dashboard does:
// controller ProvisionResource so flynn pg / flynn resource list the follower.
func (h *handler) provisionFollow(appName, appRef, leader, runtime string, autoFailover bool) (map[string]string, error) {
	appName = strings.TrimSpace(appName)
	leader = strings.TrimSpace(leader)
	if appName == "" || leader == "" {
		return nil, fmt.Errorf("follow requires a postgres resource and app name")
	}
	if strings.TrimSpace(appRef) == "" {
		appRef = appName
	}
	if h.store != nil {
		if inst, err := h.store.Get(leader); err == nil && inst != nil {
			leader = firstNonEmptyLocal(inst.App, inst.ID, leader)
		}
	}
	if h.client != nil {
		p, err := h.client.GetProvider("postgres")
		if err != nil {
			return nil, err
		}
		cfg, err := json.Marshal(provisionBody{
			App:          appName,
			Follow:       leader,
			Runtime:      runtime,
			Replication:  string(postgres.ModeStreaming),
			AutoFailover: autoFailover,
		})
		if err != nil {
			return nil, err
		}
		raw := json.RawMessage(cfg)
		res, err := h.client.ProvisionResource(&ct.ResourceReq{
			ProviderID: p.ID,
			Apps:       []string{appRef},
			Config:     &raw,
		})
		if err != nil {
			return nil, err
		}
		if res == nil {
			return nil, fmt.Errorf("follow: empty resource")
		}
		return res.Env, nil
	}
	inst, env, err := h.store.Provision(postgres.ProvisionRequest{
		App:          appName,
		Follow:       leader,
		Mode:         postgres.ModeStreaming,
		Runtime:      runtime,
		AutoFailover: autoFailover,
	})
	if err != nil {
		return nil, err
	}
	if env == nil {
		env = map[string]string{}
	}
	var leaderInst *postgres.Instance
	if inst != nil && inst.LeaderID != "" {
		leaderInst, _ = h.store.Get(inst.LeaderID)
	}
	applyPostgresResourceEnv(inst, env, leaderInst)
	stripTenantPostgresCredentials(env)
	return env, nil
}

func (h *handler) promote(w http.ResponseWriter, _ *http.Request, p httprouter.Params) {
	res, err := h.store.Promote(p.ByName("id"))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if res != nil {
		fromApp, toApp := "", ""
		if res.PreviousLeader != nil {
			fromApp = res.PreviousLeader.App
		}
		if res.Promoted != nil {
			toApp = res.Promoted.App
		}
		h.emitTopology(postgres.CodePromoted, res.Promoted, fromApp, toApp, "reason=manual")
		if err := h.stampIsolatedRole(res.Promoted); err != nil {
			writeAPIError(w, err)
			return
		}
		if res.PreviousLeader != nil && res.PreviousLeader.Role == postgres.RoleDeposed {
			h.emitTopology(postgres.CodeDeposed, res.PreviousLeader, fromApp, toApp, "reason=manual")
			_ = h.stampIsolatedRole(res.PreviousLeader)
			_ = h.fenceIsolatedApp(res.PreviousLeader.App)
			h.syncResourceEnv(nil, res.PreviousLeader)
			_ = h.ensureAutoFailoverReplica(res.Promoted)
		}
		h.syncResourceEnv(nil, res.Promoted)
	}
	httphelper.JSON(w, 200, res)
}

func (h *handler) unfollow(w http.ResponseWriter, _ *http.Request, p httprouter.Params) {
	inst, err := h.store.Unfollow(p.ByName("id"))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if err := h.stampIsolatedRole(inst); err != nil {
		writeAPIError(w, err)
		return
	}
	h.syncResourceEnv(nil, inst)
	httphelper.JSON(w, 200, inst)
}

func (h *handler) attach(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	var body struct {
		App string `json:"app"`
		As  string `json:"as"`
	}
	if err := decode(r, &body); err != nil {
		writeAPIError(w, err)
		return
	}
	env, err := h.store.Attach(p.ByName("id"), body.App, body.As)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	httphelper.JSON(w, 200, map[string]any{"env": env})
}

func (h *handler) detach(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	var body struct {
		App string `json:"app"`
	}
	if err := decode(r, &body); err != nil {
		writeAPIError(w, err)
		return
	}
	if err := h.store.Detach(p.ByName("id"), body.App); err != nil {
		writeAPIError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *handler) envSet(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	var body struct {
		App  string            `json:"app"`
		Vars map[string]string `json:"vars"`
	}
	if err := decode(r, &body); err != nil {
		writeAPIError(w, err)
		return
	}
	updates := make(map[string]*string, len(body.Vars))
	for k, v := range body.Vars {
		val := v
		updates[k] = &val
	}
	if err := h.store.CheckEnvSet(p.ByName("id"), body.App, updates); err != nil {
		writeAPIError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// libpqCredentialKeys are split PG* pieces that belong on neither the tenant
// resource nor the isolated instance (the instance uses POSTGRES_USER/DB).
var libpqCredentialKeys = []string{
	"PGHOST", "PGPORT", "PGUSER", "PGPASSWORD", "PGDATABASE", "PGSSLMODE",
}

// tenantPostgresCredentialKeys are extra POSTGRES_* login pieces the attached
// app does not need. The isolated instance release still has POSTGRES_USER/DB.
var tenantPostgresCredentialKeys = []string{
	"POSTGRES_USER", "POSTGRES_PASSWORD", "POSTGRES_DB", "POSTGRES_HOST", "POSTGRES_PORT",
}

// applyPostgresResourceEnv is the controller resource env the dashboard lists.
// Connection strings stay on DATABASE_URL or FLYNN_POSTGRESQL_<COLOR>_URL
// (or --as NAME_URL). POSTGRES_URL is an interpolation alias for pg:psql, not
// a stored env var. POSTGRES_ROLE/POSTGRES_LEADER mark --follow replicas so
// the Followers tab can find them after the API restarts.
func applyPostgresResourceEnv(inst *postgres.Instance, env map[string]string, leader *postgres.Instance) {
	if inst == nil || env == nil {
		return
	}
	env["FLYNN_POSTGRES"] = inst.App
	for _, k := range libpqCredentialKeys {
		delete(env, k)
	}
	delete(env, "POSTGRES_URL")
	if inst.Role == postgres.RoleFollower {
		env["POSTGRES_ROLE"] = "follower"
		leaderApp := ""
		if leader != nil {
			leaderApp = strings.TrimSpace(leader.App)
			if u := strings.TrimSpace(leader.ConnectionURL()); u != "" {
				env["POSTGRES_PRIMARY_URL"] = u
			}
		}
		if leaderApp == "" && postgres.IsolatedInstanceApp(inst.LeaderID) {
			leaderApp = strings.TrimSpace(inst.LeaderID)
		}
		if leaderApp != "" {
			env["POSTGRES_LEADER"] = leaderApp
		}
		if inst.AutoFailover {
			env[postgres.EnvAutoFailover] = "true"
		}
		return
	}
	if inst.Role == postgres.RoleDeposed {
		env["POSTGRES_ROLE"] = "deposed"
		if leader != nil && strings.TrimSpace(leader.App) != "" {
			env["POSTGRES_LEADER"] = leader.App
		} else if strings.TrimSpace(inst.LeaderID) != "" {
			env["POSTGRES_LEADER"] = inst.LeaderID
		}
		delete(env, "POSTGRES_PRIMARY_URL")
		delete(env, postgres.EnvAutoFailover)
		return
	}
	env["POSTGRES_ROLE"] = "primary"
	delete(env, "POSTGRES_LEADER")
	delete(env, "POSTGRES_PRIMARY_URL")
	delete(env, postgres.EnvAutoFailover)
	if inst.ReplicaPending {
		env["REPLICA_PENDING"] = "true"
	} else {
		delete(env, "REPLICA_PENDING")
	}
}

func stripTenantPostgresCredentials(env map[string]string) {
	if env == nil {
		return
	}
	for _, k := range libpqCredentialKeys {
		delete(env, k)
	}
	for _, k := range tenantPostgresCredentialKeys {
		delete(env, k)
	}
	delete(env, "POSTGRES_URL")
}

func writeAPIError(w http.ResponseWriter, err error) {
	httphelper.ValidationError(w, "postgres", err.Error())
}

func decode(r *http.Request, dest any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(dest); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
