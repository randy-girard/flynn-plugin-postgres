package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/randy-girard/flynn-plugin-postgres"
	controller "github.com/randy-girard/flynn/controller/client"
	ct "github.com/randy-girard/flynn/controller/types"
)

// DATA-03: manual promote and unfollow must restart (re-role) the job, in
// order: check the follower is caught up, fence (auto-failover only),
// restart, verify writable, and only then record the new role.

// pgNode is one fake running postgres.
type pgNode struct {
	up         bool
	recovery   bool
	wal        int64 // pg_current_wal_lsn on a primary
	replay     int64 // pg_last_wal_replay_lsn on a standby
	unreadable bool  // SQL fails (unreachable)
	// stuck keeps the node in recovery whatever role the release says.
	stuck bool
}

type promoteRig struct {
	t     *testing.T
	h     *handler
	store *postgres.Store
	ctl   *promoteCtl
	mu    sync.Mutex
	nodes map[string]*pgNode // by app
	ops   []string
	// onRestart runs when a job starts on a release (scale > 0).
	onRestart func(app string, rel *ct.Release)
}

func (r *promoteRig) op(format string, args ...any) {
	r.mu.Lock()
	r.ops = append(r.ops, fmt.Sprintf(format, args...))
	r.mu.Unlock()
}

func (r *promoteRig) node(app string) *pgNode {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.nodes[app]
	if n == nil {
		n = &pgNode{up: true}
		r.nodes[app] = n
	}
	return n
}

func (r *promoteRig) opsWith(prefix string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, o := range r.ops {
		if strings.HasPrefix(o, prefix) {
			out = append(out, o)
		}
	}
	return out
}

func (r *promoteRig) index(op string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, o := range r.ops {
		if strings.HasPrefix(o, op) {
			return i
		}
	}
	return -1
}

// promoteCtl is the controller: apps, releases, formations and jobs.
type promoteCtl struct {
	controller.Client
	rig      *promoteRig
	mu       sync.Mutex
	apps     map[string]*ct.App
	releases map[string]*ct.Release
	current  map[string]string
	scale    map[string]map[string]int // app -> release -> count
	n        int
}

func (c *promoteCtl) addApp(name string, env map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	app := &ct.App{ID: name, Name: name}
	rel := &ct.Release{ID: "rel-" + name + "-" + strconv.Itoa(c.n), AppID: name, Env: env, Processes: map[string]ct.ProcessType{postgres.ProcessName: {}}}
	c.apps[name] = app
	c.releases[rel.ID] = rel
	c.current[name] = rel.ID
	c.scale[name] = map[string]int{rel.ID: 1}
}

func (c *promoteCtl) GetApp(name string) (*ct.App, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a := c.apps[name]; a != nil {
		return a, nil
	}
	return nil, controller.ErrNotFound
}

func (c *promoteCtl) GetAppRelease(app string) (*ct.Release, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.current[app]
	if id == "" {
		return nil, controller.ErrNotFound
	}
	rel := *c.releases[id]
	env := map[string]string{}
	for k, v := range rel.Env {
		env[k] = v
	}
	rel.Env = env
	return &rel, nil
}

func (c *promoteCtl) CreateRelease(app string, rel *ct.Release) error {
	c.mu.Lock()
	c.n++
	rel.ID = "rel-" + app + "-" + strconv.Itoa(c.n)
	rel.AppID = app
	cp := *rel
	c.releases[rel.ID] = &cp
	c.mu.Unlock()
	c.rig.op("release %s role=%s alias=%s", app, rel.Env["FLYNN_POSTGRES_ROLE"], rel.Env[postgres.EnvServiceAlias])
	return nil
}

func (c *promoteCtl) SetAppRelease(app, id string) error {
	c.mu.Lock()
	c.current[app] = id
	c.mu.Unlock()
	return nil
}

func (c *promoteCtl) ScaleAppRelease(app, id string, opts ct.ScaleOptions) error {
	n := opts.Processes[postgres.ProcessName]
	c.mu.Lock()
	if c.scale[app] == nil {
		c.scale[app] = map[string]int{}
	}
	c.scale[app][id] = n
	rel := c.releases[id]
	c.mu.Unlock()
	c.rig.op("scale %s %s=%d", app, id, n)
	node := c.rig.node(app)
	c.rig.mu.Lock()
	if n == 0 {
		node.up = false
	} else {
		node.up = true
		if !node.stuck && rel != nil {
			node.recovery = rel.Env["FLYNN_POSTGRES_ROLE"] == "follower"
			if !node.recovery && node.wal < node.replay {
				// A promoted standby writes from where it replayed.
				node.wal = node.replay
			}
		}
	}
	c.rig.mu.Unlock()
	if n > 0 && c.rig.onRestart != nil && rel != nil {
		c.rig.onRestart(app, rel)
	}
	return nil
}

func (c *promoteCtl) JobList(app string) ([]*ct.Job, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*ct.Job
	for id, n := range c.scale[app] {
		if n > 0 {
			out = append(out, &ct.Job{ReleaseID: id, State: ct.JobStateUp})
		}
	}
	return out, nil
}

func (c *promoteCtl) AppResourceList(string) ([]*ct.Resource, error) { return nil, nil }
func (c *promoteCtl) ResourceListAll() ([]*ct.Resource, error)       { return nil, nil }
func (c *promoteCtl) PutResource(*ct.Resource) error                 { return nil }
func (c *promoteCtl) AppList() ([]*ct.App, error)                    { return nil, nil }
func (c *promoteCtl) GetProvider(string) (*ct.Provider, error)       { return nil, controller.ErrNotFound }

func (c *promoteCtl) runningRelease(app string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, n := range c.scale[app] {
		if n > 0 {
			return id
		}
	}
	return ""
}

func (c *promoteCtl) currentRelease(t *testing.T, app string) *ct.Release {
	t.Helper()
	rel, err := c.GetAppRelease(app)
	if err != nil {
		t.Fatal(err)
	}
	return rel
}

func appFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := strings.TrimSuffix(strings.TrimPrefix(u.Hostname(), "leader."), ".discoverd")
	return host
}

// newPromoteRig is a live handler with a leader and one follower, both
// running, follower caught up.
func newPromoteRig(t *testing.T, autoFailover bool) (*promoteRig, *postgres.Instance, *postgres.Instance) {
	t.Helper()
	s := postgres.NewStore()
	leader, _, err := s.Provision(postgres.ProvisionRequest{App: "shop-a", Tenant: "shop-a"})
	if err != nil {
		t.Fatal(err)
	}
	fol, _, err := s.Provision(postgres.ProvisionRequest{App: "shop-a", Follow: leader.App, AutoFailover: autoFailover})
	if err != nil {
		t.Fatal(err)
	}
	rig := &promoteRig{t: t, store: s, nodes: map[string]*pgNode{}}
	ctl := &promoteCtl{rig: rig, apps: map[string]*ct.App{}, releases: map[string]*ct.Release{}, current: map[string]string{}, scale: map[string]map[string]int{}}
	rig.ctl = ctl
	ctl.addApp(leader.App, map[string]string{"FLYNN_POSTGRES": leader.App, "FLYNN_POSTGRES_ROLE": "primary"})
	folEnv := map[string]string{"FLYNN_POSTGRES": fol.App, "FLYNN_POSTGRES_ROLE": "follower", "FLYNN_POSTGRES_LEADER": leader.App, "FLYNN_POSTGRES_PRIMARY_URL": leader.ReplicationURL()}
	if autoFailover {
		folEnv[postgres.EnvAutoFailover] = "true"
	}
	ctl.addApp(fol.App, folEnv)
	*rig.node(leader.App) = pgNode{up: true, wal: 5000}
	*rig.node(fol.App) = pgNode{up: true, recovery: true, replay: 5000}

	h := newHandler(s)
	h.client = ctl
	h.imageID = "00000000-0000-0000-0000-0000000000aa"
	h.availability = func() (string, error) { return postgres.AvailabilityManual, nil }
	h.hostFits = func() ([]postgres.HostFit, error) { return nil, nil }
	rig.h = h

	events := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) }))
	t.Cleanup(events.Close)
	t.Setenv("DASHBOARD_EVENTS_URL", events.URL)
	origLog := writeResourceLog
	t.Cleanup(func() { writeResourceLog = origLog })
	writeResourceLog = func(*handler, string, string) {}
	origSQL, origWait := runSQL, waitInstanceReady
	origCatch, origVerify, origPoll := manualCatchupTimeout, manualVerifyTimeout, manualPoll
	t.Cleanup(func() {
		runSQL, waitInstanceReady = origSQL, origWait
		manualCatchupTimeout, manualVerifyTimeout, manualPoll = origCatch, origVerify, origPoll
	})
	manualCatchupTimeout = 150 * time.Millisecond
	manualVerifyTimeout = 150 * time.Millisecond
	manualPoll = 10 * time.Millisecond
	waitInstanceReady = func(string, time.Duration) error { return nil }
	runSQL = func(connURL, query string) (string, error) {
		app := appFromURL(connURL)
		n := rig.node(app)
		rig.mu.Lock()
		defer rig.mu.Unlock()
		if !n.up || n.unreadable {
			return "", errors.New("connection refused")
		}
		switch query {
		case sqlWalBytes:
			rig.ops = append(rig.ops, "sql "+app+" wal")
			if n.recovery {
				return "", errors.New("recovery is in progress")
			}
			return strconv.FormatInt(n.wal, 10), nil
		case sqlReplayBytes:
			rig.ops = append(rig.ops, "sql "+app+" replay")
			if n.recovery {
				return "1|" + strconv.FormatInt(n.replay, 10), nil
			}
			return "0|" + strconv.FormatInt(n.replay, 10), nil
		case sqlReceiveReplay:
			rig.ops = append(rig.ops, "sql "+app+" receive")
			return strconv.FormatInt(n.replay, 10) + "|" + strconv.FormatInt(n.replay, 10), nil
		case sqlWriteProbe:
			rig.ops = append(rig.ops, "sql "+app+" probe")
			if n.recovery {
				return "", errors.New("ERROR: cannot execute CREATE TABLE in a read-only transaction")
			}
			return "", nil
		}
		rig.ops = append(rig.ops, "sql "+app+" other")
		return "", nil
	}
	return rig, leader, fol
}

func (r *promoteRig) post(path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec
}

func TestManualPromoteBouncesFollowerJob(t *testing.T) {
	rig, leader, fol := newPromoteRig(t, false)
	// The store must still say follower while the job restarts: the new
	// role is recorded only after the job is verified writable.
	rig.onRestart = func(app string, rel *ct.Release) {
		if app != fol.App || rel.Env["FLYNN_POSTGRES_ROLE"] != "primary" {
			return
		}
		if got, _ := rig.store.Get(fol.ID); got.Role != postgres.RoleFollower {
			t.Errorf("store role %s before the promoted job was verified", got.Role)
		}
	}
	before := rig.ctl.runningRelease(fol.App)
	rec := rig.post("/databases/"+fol.ID+"/promote", "{}")
	if rec.Code != 200 {
		t.Fatalf("promote %d %s", rec.Code, rec.Body.String())
	}
	after := rig.ctl.runningRelease(fol.App)
	if after == "" || after == before {
		t.Fatalf("follower job was not restarted: running %q before, %q after; ops %v", before, after, rig.ops)
	}
	if rel := rig.ctl.currentRelease(t, fol.App); rel.Env["FLYNN_POSTGRES_ROLE"] != "primary" || rel.Env["FLYNN_POSTGRES_PRIMARY_URL"] != "" {
		t.Fatalf("follower release env %v", rel.Env)
	}
	// Order: caught-up check, restart, write probe.
	caught := rig.index("sql " + fol.App + " replay")
	stop := rig.index("scale " + fol.App + " " + before + "=0")
	probe := rig.index("sql " + fol.App + " probe")
	if caught < 0 || stop < 0 || probe < 0 || !(caught < stop && stop < probe) {
		t.Fatalf("want replay check < stop old job < write probe; ops %v", rig.ops)
	}
	if got := rig.opsWith("scale " + leader.App); len(got) != 0 {
		t.Fatalf("a plain follower's promote must not stop the primary: %v", got)
	}
	got, _ := rig.store.Get(fol.ID)
	if got.Role != postgres.RolePrimary {
		t.Fatalf("role %s", got.Role)
	}
	if rig.node(fol.App).recovery {
		t.Fatal("promoted node still in recovery")
	}
}

func TestManualPromoteRefusesLaggingFollower(t *testing.T) {
	rig, leader, fol := newPromoteRig(t, true)
	rig.node(fol.App).replay = 1000 // 4000 bytes behind, never catches up
	before := rig.ctl.runningRelease(fol.App)
	rec := rig.post("/databases/"+fol.ID+"/promote", "{}")
	if rec.Code == 200 {
		t.Fatalf("promoted a lagging follower: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "behind") {
		t.Fatalf("error does not say the follower is behind: %s", rec.Body.String())
	}
	if got := rig.opsWith("scale "); len(got) != 0 {
		t.Fatalf("refused promote scaled jobs: %v", got)
	}
	if got := rig.opsWith("release "); len(got) != 0 {
		t.Fatalf("refused promote wrote releases: %v", got)
	}
	if rig.ctl.runningRelease(fol.App) != before || !rig.node(leader.App).up {
		t.Fatal("refused promote changed a running job")
	}
	if got, _ := rig.store.Get(fol.ID); got.Role != postgres.RoleFollower {
		t.Fatalf("follower role %s", got.Role)
	}
	if got, _ := rig.store.Get(leader.ID); got.Role != postgres.RolePrimary {
		t.Fatalf("leader role %s", got.Role)
	}
}

func TestManualPromoteRefusesUnreachablePrimaryUnlessForced(t *testing.T) {
	rig, _, fol := newPromoteRig(t, false)
	rig.node(rig.mustLeaderApp(fol)).unreadable = true
	rec := rig.post("/databases/"+fol.ID+"/promote", "{}")
	if rec.Code == 200 || !strings.Contains(rec.Body.String(), "--force") {
		t.Fatalf("unforced promote with an unreachable primary: %d %s", rec.Code, rec.Body.String())
	}
	if got := rig.opsWith("scale "); len(got) != 0 {
		t.Fatalf("refused promote scaled jobs: %v", got)
	}
	rec = rig.post("/databases/"+fol.ID+"/promote", `{"force":true}`)
	if rec.Code != 200 {
		t.Fatalf("forced promote %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := rig.store.Get(fol.ID); got.Role != postgres.RolePrimary {
		t.Fatalf("role %s", got.Role)
	}
}

func (r *promoteRig) mustLeaderApp(fol *postgres.Instance) string {
	l, err := r.store.Get(fol.LeaderID)
	if err != nil {
		r.t.Fatal(err)
	}
	return l.App
}

func TestManualPromoteOfAutoFailoverKeepsServiceAlias(t *testing.T) {
	rig, leader, fol := newPromoteRig(t, true)
	rec := rig.post("/databases/"+fol.ID+"/promote", "{}")
	if rec.Code != 200 {
		t.Fatalf("promote %d %s", rec.Code, rec.Body.String())
	}
	// The old primary is stopped before the follower restarts writable.
	fence := rig.index("scale " + leader.App)
	restart := rig.index("release " + fol.App + " role=primary")
	if fence < 0 || restart < 0 || fence > restart {
		t.Fatalf("want leader fenced before the follower restarts as primary; ops %v", rig.ops)
	}
	if rig.node(leader.App).up {
		t.Fatal("old primary still running")
	}
	rel := rig.ctl.currentRelease(t, fol.App)
	if rel.Env[postgres.EnvServiceAlias] != leader.App || rel.Env["FLYNN_POSTGRES_ROLE"] != "primary" {
		t.Fatalf("promoted release env %v, want alias %s", rel.Env, leader.App)
	}
	if lrel := rig.ctl.currentRelease(t, leader.App); lrel.Env["FLYNN_POSTGRES_ROLE"] != "deposed" {
		t.Fatalf("old primary release role %s, want deposed", lrel.Env["FLYNN_POSTGRES_ROLE"])
	}
	got, _ := rig.store.Get(fol.ID)
	if got.Role != postgres.RolePrimary || got.ServiceAlias != leader.App {
		t.Fatalf("promoted %s alias %q", got.Role, got.ServiceAlias)
	}
	if rig.node(fol.App).recovery {
		t.Fatal("promoted node still in recovery")
	}
}

func TestManualPromoteRollsBackWhenPromotedJobStaysReadOnly(t *testing.T) {
	rig, leader, fol := newPromoteRig(t, true)
	rig.node(fol.App).stuck = true
	prevFol := rig.ctl.currentRelease(t, fol.App).ID
	prevLeader := rig.ctl.currentRelease(t, leader.App).ID
	rec := rig.post("/databases/"+fol.ID+"/promote", "{}")
	if rec.Code == 200 {
		t.Fatalf("promote succeeded with a read-only job: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "rolled back") {
		t.Fatalf("error does not say it rolled back: %s", rec.Body.String())
	}
	if cur := rig.ctl.currentRelease(t, fol.App).ID; cur != prevFol || rig.ctl.runningRelease(fol.App) != prevFol {
		t.Fatalf("follower release %s running %s, want back on %s", cur, rig.ctl.runningRelease(fol.App), prevFol)
	}
	if cur := rig.ctl.currentRelease(t, leader.App).ID; cur != prevLeader || rig.ctl.runningRelease(leader.App) != prevLeader || !rig.node(leader.App).up {
		t.Fatalf("old primary not restored: release %s running %s up %v", cur, rig.ctl.runningRelease(leader.App), rig.node(leader.App).up)
	}
	if got, _ := rig.store.Get(fol.ID); got.Role != postgres.RoleFollower {
		t.Fatalf("follower role %s", got.Role)
	}
	if got, _ := rig.store.Get(leader.ID); got.Role != postgres.RolePrimary {
		t.Fatalf("leader role %s", got.Role)
	}
}

func TestManualPromoteRetryAfterSuccessIsANoOp(t *testing.T) {
	rig, _, fol := newPromoteRig(t, false)
	if rec := rig.post("/databases/"+fol.ID+"/promote", "{}"); rec.Code != 200 {
		t.Fatalf("promote %d %s", rec.Code, rec.Body.String())
	}
	n := len(rig.opsWith("scale "))
	if rec := rig.post("/databases/"+fol.ID+"/promote", "{}"); rec.Code != 200 {
		t.Fatalf("retry %d %s", rec.Code, rec.Body.String())
	}
	if got := len(rig.opsWith("scale ")); got != n {
		t.Fatalf("retry restarted jobs again: %v", rig.ops)
	}
}

func TestUnfollowBouncesJob(t *testing.T) {
	rig, leader, fol := newPromoteRig(t, false)
	rig.onRestart = func(app string, rel *ct.Release) {
		if app == fol.App && rel.Env["FLYNN_POSTGRES_ROLE"] == "primary" {
			if got, _ := rig.store.Get(fol.ID); got.Role != postgres.RoleFollower {
				t.Errorf("store role %s before the job was verified", got.Role)
			}
		}
	}
	before := rig.ctl.runningRelease(fol.App)
	rec := rig.post("/databases/"+fol.ID+"/unfollow", "{}")
	if rec.Code != 200 {
		t.Fatalf("unfollow %d %s", rec.Code, rec.Body.String())
	}
	if after := rig.ctl.runningRelease(fol.App); after == "" || after == before {
		t.Fatalf("job not restarted: %q -> %q; ops %v", before, after, rig.ops)
	}
	rel := rig.ctl.currentRelease(t, fol.App)
	if rel.Env["FLYNN_POSTGRES_ROLE"] != "primary" || rel.Env["FLYNN_POSTGRES_PRIMARY_URL"] != "" || rel.Env["FLYNN_POSTGRES_LEADER"] != "" {
		t.Fatalf("unfollowed release still names the leader: %v", rel.Env)
	}
	if rig.index("sql "+fol.App+" probe") < 0 {
		t.Fatalf("no write probe: %v", rig.ops)
	}
	if got := rig.opsWith("scale " + leader.App); len(got) != 0 {
		t.Fatalf("unfollow touched the upstream: %v", got)
	}
	if got, _ := rig.store.Get(fol.ID); got.Role != postgres.RoleStandalone {
		t.Fatalf("role %s", got.Role)
	}
	if rig.node(fol.App).recovery {
		t.Fatal("unfollowed node still in recovery")
	}
}

func TestUnfollowRollsBackWhenJobStaysReadOnly(t *testing.T) {
	rig, _, fol := newPromoteRig(t, false)
	rig.node(fol.App).stuck = true
	prev := rig.ctl.currentRelease(t, fol.App).ID
	rec := rig.post("/databases/"+fol.ID+"/unfollow", "{}")
	if rec.Code == 200 {
		t.Fatalf("unfollow succeeded with a read-only job: %s", rec.Body.String())
	}
	if cur := rig.ctl.currentRelease(t, fol.App).ID; cur != prev || rig.ctl.runningRelease(fol.App) != prev {
		t.Fatalf("follower not restored to %s: current %s running %s", prev, cur, rig.ctl.runningRelease(fol.App))
	}
	if got, _ := rig.store.Get(fol.ID); got.Role != postgres.RoleFollower {
		t.Fatalf("role %s", got.Role)
	}
}

func TestDashPromoteAndUnfollowUseTheVerifiedPath(t *testing.T) {
	t.Setenv("DASHBOARD_SSO_OPTIONAL", "1")
	rig, _, fol := newPromoteRig(t, false)
	rig.node(fol.App).replay = 10 // lagging
	dash := func(action string) string {
		form := strings.NewReader("action=" + action + "&instance=" + fol.App)
		req := httptest.NewRequest(http.MethodPost, "/dashboard/replication", form)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Flynn-Dashboard-App", "shop-a")
		req.Header.Set("X-Flynn-Dashboard-Permissions", "app:resources:write")
		rec := httptest.NewRecorder()
		rig.h.ServeHTTP(rec, req)
		return rec.Body.String()
	}
	if body := dash("promote"); strings.Contains(body, "Follower promoted") {
		t.Fatalf("dashboard promoted a lagging follower: %s", body)
	}
	if got, _ := rig.store.Get(fol.ID); got.Role != postgres.RoleFollower {
		t.Fatalf("role %s", got.Role)
	}
	before := rig.ctl.runningRelease(fol.App)
	if body := dash("unfollow"); !strings.Contains(body, "Replication stopped") {
		t.Fatalf("dashboard unfollow: %s", body)
	}
	if after := rig.ctl.runningRelease(fol.App); after == before {
		t.Fatalf("dashboard unfollow did not restart the job: %v", rig.ops)
	}
}

func TestPromoteTaskSendsForceOnlyWhenAsked(t *testing.T) {
	var bodies []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, r.URL.Path+" "+string(raw))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ts.Close)
	t.Setenv("POSTGRES_PLUGIN_URL", ts.URL)
	if err := runPluginTask([]string{"promote", "postgresql-reef-55266"}); err != nil {
		t.Fatal(err)
	}
	if err := runPluginTask([]string{"promote", "postgresql-reef-55266", "--force"}); err != nil {
		t.Fatal(err)
	}
	if err := runPluginTask([]string{"unfollow", "postgresql-reef-55266", "--force"}); err == nil {
		t.Fatal("unfollow accepted --force")
	}
	want := []string{"/databases/postgresql-reef-55266/promote {}", `/databases/postgresql-reef-55266/promote {"force":true}`}
	if strings.Join(bodies, "\n") != strings.Join(want, "\n") {
		t.Fatalf("bodies %q, want %q", bodies, want)
	}
}
