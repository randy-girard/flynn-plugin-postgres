package main

import (
	"encoding/json"
	"errors"
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
	if err := http.Serve(ln, h); err != nil && !errors.Is(err, http.ErrServerClosed) {
		shutdown.Fatal(err)
	}
}

type handler struct {
	store         *postgres.Store
	router        *httprouter.Router
	client        controller.Client
	imageID       string
	log           log15.Logger
	listResources func(app string) ([]*ct.Resource, error)
}

func newHandler(store *postgres.Store) *handler {
	h := &handler{store: store, router: httprouter.New()}
	h.router.GET("/ping", func(w http.ResponseWriter, _ *http.Request, _ httprouter.Params) {
		w.WriteHeader(http.StatusOK)
	})
	h.router.POST("/databases", h.provision)
	h.router.GET("/databases/:id", h.info)
	h.router.POST("/databases/:id/users", h.addUser)
	h.router.GET("/databases/:id/users", h.listUsers)
	h.router.POST("/databases/:id/databases", h.addDatabase)
	h.router.POST("/databases/:id/write", h.write)
	h.router.POST("/databases/:id/wait", h.wait)
	h.router.POST("/databases/:id/promote", h.promote)
	h.router.POST("/databases/:id/unfollow", h.unfollow)
	h.router.POST("/databases/:id/attach", h.attach)
	h.router.POST("/databases/:id/detach", h.detach)
	h.router.POST("/databases/:id/env-set", h.envSet)
	h.mountDashboard()
	return h
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.router.ServeHTTP(w, r)
}

type provisionBody struct {
	Platform    bool   `json:"platform"`
	App         string `json:"app"`
	Tenant      string `json:"tenant"`
	As          string `json:"as"`
	Follow      string `json:"follow"`
	Runtime     string `json:"runtime"`
	Replication string `json:"replication"`
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
		Tenant:  body.Tenant,
		App:     body.App,
		As:      body.As,
		Follow:  body.Follow,
		Mode:    postgres.ReplicationMode(body.Replication),
		Runtime: body.Runtime,
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
		env = postgres.AttachmentEnv(body.As, inst.ConnectionURL())
	}
	var leader *postgres.Instance
	if inst.LeaderID != "" {
		leader, _ = h.store.Get(inst.LeaderID)
	}
	applyPostgresResourceEnv(inst, env, leader)
	httphelper.JSON(w, 200, map[string]any{
		"id":   inst.ID,
		"env":  env,
		"plan": inst.NodePlan(),
		"host": inst.ServiceHost,
	})
}

func (h *handler) info(w http.ResponseWriter, _ *http.Request, p httprouter.Params) {
	info, err := h.store.Info(p.ByName("id"))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	httphelper.JSON(w, 200, info)
}

func (h *handler) addUser(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	var body struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if err := decode(r, &body); err != nil {
		writeAPIError(w, err)
		return
	}
	if err := h.store.AddUser(p.ByName("id"), body.Name, body.Password); err != nil {
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
	if err := h.store.AddDatabase(p.ByName("id"), body.Name); err != nil {
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
	ctx, cancel := timeoutCtx(r)
	defer cancel()
	if err := h.store.Wait(ctx, p.ByName("id")); err != nil {
		writeAPIError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *handler) promote(w http.ResponseWriter, _ *http.Request, p httprouter.Params) {
	res, err := h.store.Promote(p.ByName("id"))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	httphelper.JSON(w, 200, res)
}

func (h *handler) unfollow(w http.ResponseWriter, _ *http.Request, p httprouter.Params) {
	inst, err := h.store.Unfollow(p.ByName("id"))
	if err != nil {
		writeAPIError(w, err)
		return
	}
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

// applyPostgresResourceEnv is the controller resource env the dashboard lists.
// POSTGRES_URL is the connection string. DATABASE_URL is shared with other
// engines on the same app, so a later resource:add must not be what psql uses.
// POSTGRES_ROLE/POSTGRES_LEADER mark --follow replicas so the Followers tab
// can find them after the API restarts.
func applyPostgresResourceEnv(inst *postgres.Instance, env map[string]string, leader *postgres.Instance) {
	if inst == nil || env == nil {
		return
	}
	env["FLYNN_POSTGRES"] = inst.App
	env["POSTGRES_URL"] = inst.ConnectionURL()
	if len(inst.Databases) > 0 && inst.Databases[0].Name != "" {
		db := inst.Databases[0].Name
		env["PGDATABASE"] = db
		env["POSTGRES_DB"] = db
	}
	if inst.AppUser != "" {
		env["PGUSER"] = inst.AppUser
	}
	if inst.ServiceHost != "" {
		env["PGHOST"] = inst.ServiceHost
	}
	if inst.Role == postgres.RoleFollower {
		env["POSTGRES_ROLE"] = "follower"
		if leader != nil && strings.TrimSpace(leader.App) != "" {
			env["POSTGRES_LEADER"] = leader.App
		}
	} else {
		env["POSTGRES_ROLE"] = "primary"
	}
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
