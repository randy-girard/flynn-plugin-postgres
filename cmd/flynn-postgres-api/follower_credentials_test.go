package main

import (
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/randy-girard/flynn-plugin-postgres"
	"github.com/randy-girard/flynn-plugin-postgres/internal/dashui"
	ct "github.com/randy-girard/flynn/controller/types"
)

type sqlCall struct {
	user, db, query string
	releases        int
}

func urlUserDB(raw string) (string, string) {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return "", ""
	}
	return u.User.Username(), strings.Trim(u.Path, "/")
}

func readerFollowerPair() (*postgres.Instance, *postgres.Instance) {
	leader := &postgres.Instance{
		App:         "postgresql-lead-11111",
		AppUser:     "app_lead",
		AppPassword: "apppw",
		Databases:   []postgres.Database{{Name: "db_shop"}, {Name: "db_extra"}},
		ServiceHost: "leader.postgresql-lead-11111.discoverd",
	}
	fol := &postgres.Instance{
		App:            "postgresql-follow-22222",
		AppUser:        "app_lead",
		AppPassword:    "apppw",
		ReaderUser:     "ro_fol",
		ReaderPassword: "ropw_fol",
		Role:           postgres.RoleFollower,
		Databases:      []postgres.Database{{Name: "db_shop"}},
	}
	return leader, fol
}

// NEW-06: the follower's reader role is created on the leader, so the base
// backup and the WAL stream carry it, before the follower app exists.
func TestStartIsolatedFollowerCreatesReaderOnLeader(t *testing.T) {
	ctrl := &fakeInstanceControl{}
	leader, fol := readerFollowerPair()
	var calls []sqlCall
	orig := runSQL
	t.Cleanup(func() { runSQL = orig })
	runSQL = func(connURL, query string) (string, error) {
		user, db := urlUserDB(connURL)
		calls = append(calls, sqlCall{user: user, db: db, query: query, releases: len(ctrl.releases)})
		return "", nil
	}
	if err := startIsolatedInstance(ctrl, "img-1", fol, leader, nil, func(string, time.Duration) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(calls) == 0 {
		t.Fatal("no SQL ran on the leader")
	}
	joined := ""
	for _, c := range calls {
		if c.user != leader.AppUser {
			t.Fatalf("reader SQL ran as %q, want the leader's app role", c.user)
		}
		if c.releases != 0 {
			t.Fatal("reader role must exist before the follower release is created")
		}
		joined += "\n[" + c.db + "] " + c.query
	}
	for _, needle := range []string{
		"CREATE ROLE ro_fol LOGIN",
		"GRANT CONNECT ON DATABASE db_shop TO ro_fol",
		"GRANT CONNECT ON DATABASE db_extra TO ro_fol",
		"[db_shop] GRANT USAGE ON SCHEMA public TO ro_fol",
		"[db_extra] GRANT USAGE ON SCHEMA public TO ro_fol",
	} {
		if !strings.Contains(joined, needle) {
			t.Fatalf("missing %q in %s", needle, joined)
		}
	}
	env := ctrl.releases[0].Env
	if env[postgres.EnvReaderUser] != "ro_fol" || env[postgres.EnvReaderPassword] != "ropw_fol" {
		t.Fatal("follower release must record its reader login")
	}
	if env["POSTGRES_USER"] != leader.AppUser {
		t.Fatal("follower instance login must stay the replicated leader app role")
	}
	if env["FLYNN_POSTGRES_PRIMARY_URL"] == "" {
		t.Fatal("follower release lost its replication URL")
	}
}

func TestStartIsolatedFollowerReaderFailureCreatesNothing(t *testing.T) {
	ctrl := &fakeInstanceControl{}
	leader, fol := readerFollowerPair()
	orig := runSQL
	t.Cleanup(func() { runSQL = orig })
	runSQL = func(string, string) (string, error) { return "", errors.New("leader down") }
	if err := startIsolatedInstance(ctrl, "img-1", fol, leader, nil, func(string, time.Duration) error { return nil }); err == nil {
		t.Fatal("follow must fail when the reader role cannot be created")
	}
	if len(ctrl.apps) != 0 || len(ctrl.releases) != 0 {
		t.Fatal("a follower without its reader role must not be started")
	}
}

// The tenant resource env must never carry the leader's login.
func TestFollowerResourceEnvHasNoLeaderLogin(t *testing.T) {
	leader, fol := readerFollowerPair()
	env := map[string]string{
		"DATABASE_URL":               fol.ConnectionURL(),
		"FLYNN_POSTGRES_PRIMARY_URL": leader.ReplicationURL(),
		"POSTGRES_PRIMARY_URL":       leader.ReplicationURL(),
		postgres.EnvReaderUser:       "ro_fol",
		postgres.EnvReaderPassword:   "ropw_fol",
	}
	applyPostgresResourceEnv(fol, env, leader)
	stripTenantPostgresCredentials(env)
	for k, v := range env {
		if strings.Contains(v, leader.AppPassword) {
			t.Fatalf("tenant env %s carries the leader password", k)
		}
	}
	for _, k := range []string{"FLYNN_POSTGRES_PRIMARY_URL", "POSTGRES_PRIMARY_URL", postgres.EnvReaderUser, postgres.EnvReaderPassword} {
		if _, ok := env[k]; ok {
			t.Fatalf("tenant env kept %s", k)
		}
	}
	if env["FLYNN_POSTGRES_ROLE"] != "follower" || env["FLYNN_POSTGRES_LEADER"] != leader.App {
		t.Fatal("follower markers missing from tenant env")
	}
}

// Restamping the follower's own release keeps replication working.
func TestStampIsolatedFollowerKeepsReplicationURL(t *testing.T) {
	ctrl := &fakeInstanceControl{}
	leader, fol := readerFollowerPair()
	fol.ReaderUser = ""
	if err := startIsolatedInstance(ctrl, "img-1", fol, leader, nil, func(string, time.Duration) error { return nil }); err != nil {
		t.Fatal(err)
	}
	for _, l := range []*postgres.Instance{leader, nil} {
		if err := stampIsolatedRole(ctrl, fol, l); err != nil {
			t.Fatal(err)
		}
		rel, err := ctrl.GetAppRelease(fol.App)
		if err != nil {
			t.Fatal(err)
		}
		if rel.Env["FLYNN_POSTGRES_PRIMARY_URL"] != leader.ReplicationURL() {
			t.Fatalf("restamp (leader=%v) lost the replication URL", l != nil)
		}
	}
}

// HTTP follow: the tenant gets the follower's own login and nothing of the leader's.
func TestHTTPProvisionFollowerReturnsOwnLogin(t *testing.T) {
	ctl := newSizeCtl(t, true)
	origLog := writeResourceLog
	t.Cleanup(func() { writeResourceLog = origLog })
	writeResourceLog = func(*handler, string, string) {}
	var mu sync.Mutex
	var queries []string
	orig := runSQL
	t.Cleanup(func() { runSQL = orig })
	runSQL = func(_ string, query string) (string, error) {
		mu.Lock()
		queries = append(queries, query)
		mu.Unlock()
		return "", nil
	}
	store := postgres.NewStore()
	h := newHandler(store)
	h.client = ctl
	h.imageID = "00000000-0000-0000-0000-0000000000aa"
	w := httptest.NewRecorder()
	h.provision(w, httptest.NewRequest("POST", "/databases", strings.NewReader(`{"app":"web"}`)), nil)
	if w.Code != 200 {
		t.Fatalf("leader: %d %s", w.Code, w.Body.String())
	}
	leaders := store.Primaries()
	if len(leaders) != 1 {
		t.Fatalf("primaries %d", len(leaders))
	}
	leader := leaders[0]
	w = httptest.NewRecorder()
	h.provision(w, httptest.NewRequest("POST", "/databases", strings.NewReader(`{"app":"web","as":"REPLICA","follow":"`+leader.App+`"}`)), nil)
	if w.Code != 200 {
		t.Fatalf("follower: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, leader.AppPassword) {
		t.Fatal("follow response leaks the leader password")
	}
	var fol *postgres.Instance
	for _, inst := range store.All() {
		if inst.Role == postgres.RoleFollower {
			fol = inst
		}
	}
	if fol == nil || fol.ReaderUser == "" || !strings.Contains(body, fol.ReaderPassword) {
		t.Fatal("follow response does not carry the follower's own login")
	}
	mu.Lock()
	created := strings.Contains(strings.Join(queries, "\n"), "CREATE ROLE "+fol.ReaderUser)
	mu.Unlock()
	if !created {
		t.Fatal("reader role was not created on the leader")
	}
}

// Deleting a follower drops its reader role on the leader, best effort.
func TestDropFollowerReaderRunsOnLeader(t *testing.T) {
	leader, fol := readerFollowerPair()
	store := postgres.NewStore()
	h := newHandler(store)
	dropped := make(chan string, 8)
	orig := runSQL
	t.Cleanup(func() { runSQL = orig })
	runSQL = func(connURL, query string) (string, error) {
		user, _ := urlUserDB(connURL)
		if strings.Contains(query, "DROP ROLE IF EXISTS ro_fol") {
			dropped <- user
		}
		return "", nil
	}
	h.dropFollowerReader(fol, leader)
	select {
	case user := <-dropped:
		if user != leader.AppUser {
			t.Fatalf("reader dropped as %q, want the leader app role", user)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reader role was not dropped on the leader")
	}
	// No reader, or no leader: nothing to do.
	runSQL = func(string, string) (string, error) { t.Error("unexpected SQL"); return "", nil }
	legacy := *fol
	legacy.ReaderUser = ""
	h.dropFollowerReader(&legacy, leader)
	h.dropFollowerReader(fol, nil)
	time.Sleep(50 * time.Millisecond)
}

// A leader that cannot be reached never blocks deleting its follower.
func TestDropFollowerReaderDoesNotBlock(t *testing.T) {
	leader, fol := readerFollowerPair()
	h := newHandler(postgres.NewStore())
	orig := runSQL
	block := make(chan struct{})
	t.Cleanup(func() { close(block); runSQL = orig })
	runSQL = func(string, string) (string, error) { <-block; return "", errors.New("down") }
	done := make(chan struct{})
	go func() {
		h.dropFollowerReader(fol, leader)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dropping the reader blocked on the leader")
	}
}

// The dashboard must not drop a follower's login from the leader.
func TestDropUserRefusesFollowerReader(t *testing.T) {
	store := postgres.NewStore()
	h := newHandler(store)
	leader, _, err := store.Provision(postgres.ProvisionRequest{App: "web"})
	if err != nil {
		t.Fatal(err)
	}
	fol, _, err := store.Provision(postgres.ProvisionRequest{App: "web", As: "REPLICA", Follow: leader.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.dropUserOnInstance(leader.ID, fol.ReaderUser, ""); err == nil || !strings.Contains(err.Error(), "follower's login") {
		t.Fatalf("dropping a follower's reader login from the leader must be refused, got %v", err)
	}
	if err := store.DropUser(leader.ID, fol.ReaderUser); err == nil || !strings.Contains(err.Error(), "follower's login") {
		t.Fatalf("store must refuse too, got %v", err)
	}
}

type deleteCtl struct{ *sizeCtl }

func (d deleteCtl) AppList() ([]*ct.App, error)                    { return d.apps, nil }
func (d deleteCtl) ResourceListAll() ([]*ct.Resource, error)       { return nil, nil }
func (d deleteCtl) AppResourceList(string) ([]*ct.Resource, error) { return nil, nil }
func (d deleteCtl) ResourceList(string) ([]*ct.Resource, error) {
	return nil, errors.New("no resources")
}

// addons:destroy of a follower drops its login on the leader.
func TestDeprovisionFollowerDropsReader(t *testing.T) {
	ctl := deleteCtl{newSizeCtl(t, true)}
	origLog := writeResourceLog
	t.Cleanup(func() { writeResourceLog = origLog })
	writeResourceLog = func(*handler, string, string) {}
	dropped := make(chan struct{}, 1)
	orig := runSQL
	t.Cleanup(func() { runSQL = orig })
	store := postgres.NewStore()
	h := newHandler(store)
	h.client = ctl
	h.imageID = "00000000-0000-0000-0000-0000000000aa"
	leader, _, err := store.Provision(postgres.ProvisionRequest{App: "web"})
	if err != nil {
		t.Fatal(err)
	}
	fol, _, err := store.Provision(postgres.ProvisionRequest{App: "web", As: "REPLICA", Follow: leader.ID})
	if err != nil {
		t.Fatal(err)
	}
	runSQL = func(_ string, query string) (string, error) {
		if strings.Contains(query, "DROP ROLE IF EXISTS "+fol.ReaderUser) {
			select {
			case dropped <- struct{}{}:
			default:
			}
		}
		return "", nil
	}
	w := httptest.NewRecorder()
	h.deprovision(w, httptest.NewRequest("DELETE", "/databases?id="+fol.ID, nil), nil)
	if w.Code != 200 {
		t.Fatalf("deprovision: %d %s", w.Code, w.Body.String())
	}
	select {
	case <-dropped:
	case <-time.After(5 * time.Second):
		t.Fatal("follower delete did not drop its login on the leader")
	}
}

// The Users tab must not offer a follower's login (living on the leader) for deletion.
func TestCollectUsersHidesFollowerReader(t *testing.T) {
	store := postgres.NewStore()
	h := newHandler(store)
	h.client = deleteCtl{newSizeCtl(t, true)}
	h.imageID = "00000000-0000-0000-0000-0000000000aa"
	leader, _, err := store.Provision(postgres.ProvisionRequest{App: "web"})
	if err != nil {
		t.Fatal(err)
	}
	fol, _, err := store.Provision(postgres.ProvisionRequest{App: "web", As: "REPLICA", Follow: leader.ID})
	if err != nil {
		t.Fatal(err)
	}
	ctl := h.client.(deleteCtl)
	_ = ctl.CreateApp(&ct.App{Name: leader.App})
	_ = ctl.CreateApp(&ct.App{Name: fol.App})
	orig := runSQL
	t.Cleanup(func() { runSQL = orig })
	runSQL = func(string, string) (string, error) {
		return leader.AppUser + "\n" + fol.ReaderUser + "\nreporting\n", nil
	}
	users := h.collectUsers(&dashui.Session{AppID: "web", AppName: "web"})
	sawOther := false
	for _, u := range users {
		if strings.EqualFold(u.Name, fol.ReaderUser) {
			t.Fatal("Users tab lists the follower's own login")
		}
		if u.Name == "reporting" {
			sawOther = true
		}
	}
	if !sawOther {
		t.Fatalf("other users missing: %+v", users)
	}
}
