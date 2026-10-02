package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/randy-girard/flynn-plugin-postgres"
)

func TestHTTPProvisionDoesNotTargetAppliance(t *testing.T) {
	h := newHandler(postgres.NewStore())
	body := []byte(`{"app":"shop","as":"ANALYTICS"}`)
	req := httptest.NewRequest(http.MethodPost, "/databases", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "postgres-api.discoverd") {
		t.Fatalf("response targets appliance: %s", rec.Body.String())
	}
	var out struct {
		Env  map[string]string `json:"env"`
		Plan struct {
			Processes map[string]int `json:"Processes"`
			Sirenia   bool           `json:"Sirenia"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Env["ANALYTICS_URL"] == "" || out.Env["DATABASE_URL"] == "" || out.Env["FLYNN_POSTGRES"] == "" {
		t.Fatalf("env %#v", out.Env)
	}
	if colorURL(out.Env) != "" {
		t.Fatalf("--as ANALYTICS must not also set a color URL: %#v", out.Env)
	}
	if out.Env["POSTGRES_URL"] != "" {
		t.Fatalf("POSTGRES_URL must not be stored: %#v", out.Env)
	}
	if out.Env["POSTGRES_ROLE"] != "primary" {
		t.Fatalf("role %#v", out.Env)
	}
	if out.Env["ANALYTICS_URL"] == "" {
		t.Fatalf("psql URL %#v", out.Env)
	}
	for _, k := range []string{"PGHOST", "PGUSER", "PGPASSWORD", "PGDATABASE", "POSTGRES_USER", "POSTGRES_PASSWORD", "POSTGRES_DB", "POSTGRES_URL"} {
		if out.Env[k] != "" {
			t.Fatalf("resource env must not include %s: %#v", k, out.Env)
		}
	}
	if out.Plan.Sirenia || out.Plan.Processes["postgres"] != 1 || len(out.Plan.Processes) != 1 {
		t.Fatalf("plan %+v", out.Plan)
	}
	for _, c := range h.store.Contacts() {
		if strings.Contains(c, postgres.PlatformApplianceHost) {
			t.Fatal(c)
		}
	}
}

func TestHTTPProvisionWithoutAppReturnsDatabaseURL(t *testing.T) {
	h := newHandler(postgres.NewStore())
	req := httptest.NewRequest(http.MethodPost, "/databases", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Env["DATABASE_URL"] == "" || out.Env["FLYNN_POSTGRES"] == "" || out.Env["POSTGRES_URL"] != "" {
		t.Fatalf("env %#v", out.Env)
	}
	if colorURL(out.Env) == "" {
		t.Fatalf("provision must also set a color URL: %#v", out.Env)
	}
	if strings.Contains(out.Env["DATABASE_URL"], "postgres-api.discoverd") {
		t.Fatal(out.Env["DATABASE_URL"])
	}
	assertRandomDatabaseInURL(t, out.Env["DATABASE_URL"])
}

func TestHTTPProvisionFollowStampsRole(t *testing.T) {
	h := newHandler(postgres.NewStore())
	req := httptest.NewRequest(http.MethodPost, "/databases", strings.NewReader(`{"app":"shop"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("leader status %d %s", rec.Code, rec.Body.String())
	}
	var leader struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &leader); err != nil {
		t.Fatal(err)
	}
	name := leader.Env["FLYNN_POSTGRES"]
	if name == "" {
		t.Fatalf("leader env %#v", leader.Env)
	}
	body, err := json.Marshal(map[string]string{"app": "shop", "follow": name})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/databases", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("follower status %d %s", rec.Code, rec.Body.String())
	}
	var fol struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &fol); err != nil {
		t.Fatal(err)
	}
	if fol.Env["POSTGRES_ROLE"] != "follower" || fol.Env["POSTGRES_LEADER"] != name {
		t.Fatalf("follower env %#v", fol.Env)
	}
	if fol.Env["FLYNN_POSTGRES"] == "" || fol.Env["FLYNN_POSTGRES"] == name {
		t.Fatalf("follower instance %#v", fol.Env)
	}
	if fol.Env["POSTGRES_URL"] != "" || leader.Env["POSTGRES_URL"] != "" {
		t.Fatalf("POSTGRES_URL must not be stored leader=%q follower=%q", leader.Env["POSTGRES_URL"], fol.Env["POSTGRES_URL"])
	}
	if leader.Env["DATABASE_URL"] == "" {
		t.Fatalf("leader missing DATABASE_URL %#v", leader.Env)
	}
	assertRandomDatabaseInURL(t, leader.Env["DATABASE_URL"])
	if colorURL(leader.Env) == "" {
		t.Fatalf("leader missing color URL %#v", leader.Env)
	}
	if colorURL(fol.Env) == "" {
		t.Fatalf("follower missing color URL %#v", fol.Env)
	}
	if fol.Env["DATABASE_URL"] != "" {
		t.Fatalf("follower must not steal DATABASE_URL %#v", fol.Env)
	}
	if fol.Env["PGDATABASE"] != "" || fol.Env["PGUSER"] != "" || fol.Env["PGPASSWORD"] != "" {
		t.Fatalf("follower must not include split PG keys: %#v", fol.Env)
	}
}

func TestHTTPFollowRouteAttachesReplica(t *testing.T) {
	h := newHandler(postgres.NewStore())
	req := httptest.NewRequest(http.MethodPost, "/databases", strings.NewReader(`{"app":"shop"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("leader status %d %s", rec.Code, rec.Body.String())
	}
	var leader struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &leader); err != nil {
		t.Fatal(err)
	}
	name := leader.Env["FLYNN_POSTGRES"]
	if name == "" {
		t.Fatalf("leader env %#v", leader.Env)
	}
	req = httptest.NewRequest(http.MethodPost, "/databases/"+name+"/follow", strings.NewReader(`{"app":"shop"}`))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("follow status %d %s", rec.Code, rec.Body.String())
	}
	var fol struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &fol); err != nil {
		t.Fatal(err)
	}
	if fol.Env["POSTGRES_ROLE"] != "follower" || fol.Env["POSTGRES_LEADER"] != name {
		t.Fatalf("follower env %#v", fol.Env)
	}
	if fol.Env["FLYNN_POSTGRES"] == "" || fol.Env["FLYNN_POSTGRES"] == name {
		t.Fatalf("follower instance %#v", fol.Env)
	}
}

func TestHTTPEnvSetRejected(t *testing.T) {
	h := newHandler(postgres.NewStore())
	req := httptest.NewRequest(http.MethodPost, "/databases", strings.NewReader(`{"app":"shop"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var created struct {
		ID  string            `json:"id"`
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("create: %s %v", rec.Body.String(), err)
	}
	if created.Env["DATABASE_URL"] == "" {
		t.Fatalf("env %#v", created.Env)
	}
	body, err := json.Marshal(map[string]any{"app": "shop", "vars": map[string]string{"DATABASE_URL": "postgres://x"}})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/databases/"+created.ID+"/env-set", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == 200 {
		t.Fatalf("env set should fail: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "attached") {
		t.Fatalf("body: %s", rec.Body.String())
	}
}

func TestHTTPDeprovisionMissingIsOK(t *testing.T) {
	h := newHandler(postgres.NewStore())
	req := httptest.NewRequest(http.MethodDelete, "/databases?id=pg-missing-xxxxxx", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("missing deprovision should be ok, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPDestroyByNameAndBlocksFollowers(t *testing.T) {
	h := newHandler(postgres.NewStore())
	req := httptest.NewRequest(http.MethodPost, "/databases", strings.NewReader(`{"app":"shop"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("leader %d %s", rec.Code, rec.Body.String())
	}
	var leader struct {
		ID  string            `json:"id"`
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &leader); err != nil {
		t.Fatal(err)
	}
	name := leader.Env["FLYNN_POSTGRES"]
	body, err := json.Marshal(map[string]string{"app": "shop", "follow": name})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/databases", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("follower %d %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodDelete, "/databases?id="+name, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == 200 || !strings.Contains(rec.Body.String(), "followers") {
		t.Fatalf("leader with follower must not delete: %d %s", rec.Code, rec.Body.String())
	}
	var fol struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &fol); err == nil && fol.Env["FLYNN_POSTGRES"] != "" {
		t.Fatal("delete response should not be a provision body")
	}
	req = httptest.NewRequest(http.MethodDelete, "/databases?id="+leader.ID, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == 200 {
		t.Fatalf("id delete must also block: %s", rec.Body.String())
	}
}

func TestHTTPDestroyFollowerThenLeader(t *testing.T) {
	h := newHandler(postgres.NewStore())
	req := httptest.NewRequest(http.MethodPost, "/databases", strings.NewReader(`{"app":"shop"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("leader %d %s", rec.Code, rec.Body.String())
	}
	var leader struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &leader); err != nil {
		t.Fatal(err)
	}
	name := leader.Env["FLYNN_POSTGRES"]
	body, err := json.Marshal(map[string]string{"app": "shop", "follow": name})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/databases", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("follower %d %s", rec.Code, rec.Body.String())
	}
	var fol struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &fol); err != nil {
		t.Fatal(err)
	}
	follower := fol.Env["FLYNN_POSTGRES"]
	if follower == "" || follower == name {
		t.Fatalf("follower env %#v", fol.Env)
	}
	req = httptest.NewRequest(http.MethodDelete, "/databases?id="+follower, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("follower delete %d %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodDelete, "/databases?id="+name, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("leader delete after follower %d %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPDestroyHydratesMissingStoreByName(t *testing.T) {
	h := newHandler(postgres.NewStore())
	req := httptest.NewRequest(http.MethodPost, "/databases", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("provision %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		ID  string            `json:"id"`
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	name := out.Env["FLYNN_POSTGRES"]
	if name == "" {
		t.Fatalf("env %#v", out.Env)
	}
	h.store.Forget(out.ID)
	h.store.LoadMissing = func(idOrApp string) *postgres.Instance {
		if idOrApp != name {
			return nil
		}
		return postgres.InstanceFromEnv("app-id", name, map[string]string{
			"FLYNN_POSTGRES": name,
			"POSTGRES_URL":   "postgres://u:p@h/db",
		})
	}
	req = httptest.NewRequest(http.MethodDelete, "/databases?id="+name, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("hydrate delete %d %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPPlatformMarkerRejected(t *testing.T) {
	h := newHandler(postgres.NewStore())
	req := httptest.NewRequest(http.MethodPost, "/databases", strings.NewReader(`{"platform":true}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == 200 || !strings.Contains(rec.Body.String(), "postgres-api.discoverd") {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPUpgradeStartsBackgroundTask(t *testing.T) {
	h := newHandler(postgres.NewStore())
	req := httptest.NewRequest(http.MethodPost, "/databases", strings.NewReader(`{"app":"shop"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("create %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("create: %s %v", rec.Body.String(), err)
	}
	req = httptest.NewRequest(http.MethodPost, "/databases/"+created.ID+"/upgrade", strings.NewReader(`{}`))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 202 {
		t.Fatalf("upgrade %d %s", rec.Code, rec.Body.String())
	}
	var task postgres.Task
	if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil || task.ID == "" {
		t.Fatalf("task %s %v", rec.Body.String(), err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		req = httptest.NewRequest(http.MethodGet, "/tasks/"+task.ID, nil)
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("poll %d %s", rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil {
			t.Fatal(err)
		}
		if task.Status == postgres.TaskDone {
			if task.FollowerID == "" {
				t.Fatal("done task missing follower")
			}
			return
		}
		if task.Status == postgres.TaskFailed {
			t.Fatalf("upgrade failed: %s", task.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("upgrade task did not finish")
}

func TestHTTPClusterUpgradesStartsPrimaries(t *testing.T) {
	h := newHandler(postgres.NewStore())
	req := httptest.NewRequest(http.MethodPost, "/databases", strings.NewReader(`{"app":"shop"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("create %d %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "/cluster/upgrades", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 202 {
		t.Fatalf("cluster %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"tasks"`) {
		t.Fatalf("body %s", rec.Body.String())
	}
}

func TestBeginClusterUpgradesEmptyWhenNotLive(t *testing.T) {
	h := newHandler(postgres.NewStore())
	started, skipped := h.beginClusterUpgrades()
	if len(started) != 0 {
		t.Fatalf("started %#v", started)
	}
	_ = skipped
	h.autoStartClusterUpgrades()
}

func TestSkipClusterUpgradeIgnoresPluginImageID(t *testing.T) {
	cur := postgres.EngineVersion()
	if !skipClusterUpgrade("") || !skipClusterUpgrade(cur) {
		t.Fatalf("same or unknown engine must not cluster-upgrade (engine %q)", cur)
	}
	if cur != "15" && skipClusterUpgrade("15") {
		t.Fatal("older engine must cluster-upgrade")
	}
}

func TestAlreadyCurrentSkipsMatchingEngine(t *testing.T) {
	h := newHandler(postgres.NewStore())
	inst, _, err := h.store.Provision(postgres.ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if !h.alreadyCurrent(inst) {
		t.Fatalf("matching engine must skip: %#v", inst)
	}
	old := *inst
	old.EngineVersion = "15"
	if postgres.EngineVersion() != "15" && h.alreadyCurrent(&old) {
		t.Fatal("older engine must not skip")
	}
	started, skipped := h.beginClusterUpgrades()
	if len(started) != 0 {
		t.Fatalf("boot must not logical-upgrade matching engines: started %#v skipped %#v", started, skipped)
	}
}

func firstColorURL(env map[string]string) (key, val string) {
	const p = "FLYNN_POSTGRESQL_"
	for k, v := range env {
		if strings.HasPrefix(k, p) && strings.HasSuffix(k, "_URL") && v != "" {
			return k, v
		}
	}
	return "", ""
}

func colorURL(env map[string]string) string {
	_, v := firstColorURL(env)
	return v
}

var randomPostgresDB = regexp.MustCompile(`^[a-z][a-z0-9]{11}$`)

func assertRandomDatabaseInURL(t *testing.T, raw string) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url %q: %v", raw, err)
	}
	db := strings.Trim(u.Path, "/")
	if i := strings.IndexByte(db, '/'); i >= 0 {
		db = db[:i]
	}
	if !randomPostgresDB.MatchString(db) {
		t.Fatalf("first database name must be random alphanumeric, got %q in %s", db, raw)
	}
}
