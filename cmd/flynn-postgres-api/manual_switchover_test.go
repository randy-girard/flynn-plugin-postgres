package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
)

// NEW-51 / DATA-54: a manual promote is a switchover. The leader's apps
// move to the promoted host with the leader's login, the old primary is
// stopped and deposed (never a second writable primary), and the API
// result lists exactly the attachments the controller now has.

// withTenant adds the tenant app shop-a with DATABASE_URL on the leader and
// a follower attachment, and the two controller resource rows.
func withTenant(t *testing.T, rig *promoteRig, leader, fol *postgres.Instance) {
	t.Helper()
	leaderURL := leader.ConnectionURL()
	folURL := fol.ConnectionURL()
	if u, _ := url.Parse(folURL); u == nil || u.User.Username() == leader.AppUser {
		t.Fatalf("fixture: follower URL %q should use the follower's own login", folURL)
	}
	rig.ctl.addApp("shop-a", map[string]string{
		"DATABASE_URL":            leaderURL,
		"FLYNN_POSTGRES":          leader.App,
		"FLYNN_POSTGRES_ROSE_URL": folURL,
		"APP_SETTING":             "kept",
	})
	rig.ctl.resources = []*ct.Resource{
		{ID: "res-leader", ProviderID: "pg", ExternalID: leader.ID, Apps: []string{"shop-a"}, Env: map[string]string{
			"DATABASE_URL": leaderURL, "FLYNN_POSTGRES": leader.App, "FLYNN_POSTGRES_ROLE": "primary",
		}},
		{ID: "res-follower", ProviderID: "pg", ExternalID: fol.ID, Apps: []string{"shop-a"}, Env: map[string]string{
			"FLYNN_POSTGRES_ROSE_URL": folURL, "FLYNN_POSTGRES": fol.App, "FLYNN_POSTGRES_ROLE": "follower", "FLYNN_POSTGRES_LEADER": leader.App,
		}},
	}
}

func urlParts(t *testing.T, raw string) (user, pass, host string) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		t.Fatalf("bad URL %q: %v", raw, err)
	}
	p, _ := u.User.Password()
	return u.User.Username(), p, u.Hostname()
}

func TestManualPromoteIsASwitchover(t *testing.T) {
	rig, leader, fol := newPromoteRig(t, false)
	withTenant(t, rig, leader, fol)
	rec := rig.post("/databases/"+fol.ID+"/promote", "{}")
	if rec.Code != 200 {
		t.Fatalf("promote %d %s", rec.Code, rec.Body.String())
	}
	// The primary stops before the follower restarts writable, so no write
	// can land on it after the follower's catch-up point.
	fence := rig.index("scale " + leader.App)
	restart := rig.index("release " + fol.App + " role=primary")
	probe := rig.index("sql " + fol.App + " probe")
	deploy := rig.index("deploy shop-a")
	if fence < 0 || restart < 0 || fence > restart {
		t.Fatalf("want the old primary stopped before the follower restarts; ops %v", rig.ops)
	}
	if probe < 0 || deploy < 0 || probe > deploy {
		t.Fatalf("want the promoted job verified before the tenant deploy; ops %v", rig.ops)
	}
	if rig.node(leader.App).up {
		t.Fatal("old primary still running: two writable primaries")
	}
	if lrel := rig.ctl.currentRelease(t, leader.App); lrel.Env["FLYNN_POSTGRES_ROLE"] != "deposed" {
		t.Fatalf("old primary release role %q, want deposed", lrel.Env["FLYNN_POSTGRES_ROLE"])
	}
	if got, _ := rig.store.Get(leader.ID); got.Role != postgres.RoleDeposed {
		t.Fatalf("store old primary role %s, want deposed", got.Role)
	}
	if rel := rig.ctl.currentRelease(t, fol.App); rel.Env[postgres.EnvServiceAlias] != "" {
		t.Fatalf("a switchover rewrites URLs, it does not claim the old name: %v", rel.Env)
	}

	// The tenant's running release now points at the promoted host with
	// the leader's login; its own settings are kept.
	tenant := rig.ctl.currentRelease(t, "shop-a")
	user, pass, host := urlParts(t, tenant.Env["DATABASE_URL"])
	if host != fol.ServiceHost || user != leader.AppUser || pass != leader.AppPassword {
		t.Fatalf("tenant DATABASE_URL %s@%s, want %s@%s", user, host, leader.AppUser, fol.ServiceHost)
	}
	if tenant.Env["APP_SETTING"] != "kept" {
		t.Fatalf("tenant env lost its own key: %v", tenant.Env)
	}
	// The follower's own attachment gets the writable login too (DATA-53).
	user, _, host = urlParts(t, tenant.Env["FLYNN_POSTGRES_ROSE_URL"])
	if host != fol.ServiceHost || user != leader.AppUser {
		t.Fatalf("tenant FLYNN_POSTGRES_ROSE_URL %s@%s, want the writable login on %s", user, host, fol.ServiceHost)
	}
	if strings.Contains(tenant.Env["DATABASE_URL"], leader.ServiceHost) {
		t.Fatalf("tenant still on the old primary: %s", tenant.Env["DATABASE_URL"])
	}
	// The controller rows say the same thing.
	r := rig.ctl.resource("res-leader")
	if _, _, host := urlParts(t, r.Env["DATABASE_URL"]); host != fol.ServiceHost || r.Env["FLYNN_POSTGRES"] != fol.App || r.Env["FLYNN_POSTGRES_ROLE"] != "primary" {
		t.Fatalf("leader resource row %v", r.Env)
	}

	// The result lists what was moved, and only that.
	var res struct {
		Rewritten []struct {
			App string
			Env map[string]string
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Rewritten) == 0 {
		t.Fatalf("result lists no rewritten attachment: %s", rec.Body.String())
	}
	for _, a := range res.Rewritten {
		if a.App != "shop-a" {
			t.Fatalf("rewritten app %q", a.App)
		}
		for k, v := range a.Env {
			if v != tenant.Env[k] {
				t.Fatalf("result says %s=%s, controller has %s", k, v, tenant.Env[k])
			}
		}
	}
}

func TestManualSwitchoverRetriesTheTenantCutover(t *testing.T) {
	rig, leader, fol := newPromoteRig(t, false)
	withTenant(t, rig, leader, fol)
	rig.ctl.failDeploy = map[string]error{"shop-a": errors.New("deploy timed out")}
	rec := rig.post("/databases/"+fol.ID+"/promote", "{}")
	if rec.Code == 200 || !strings.Contains(rec.Body.String(), "pg:promote") || !strings.Contains(rec.Body.String(), "shop-a") {
		t.Fatalf("a failed tenant cutover must say which app and how to retry: %d %s", rec.Code, rec.Body.String())
	}
	if rig.node(leader.App).up {
		t.Fatal("old primary restarted after the follower was promoted")
	}
	rig.ctl.failDeploy = nil
	rec = rig.post("/databases/"+fol.ID+"/promote", "{}")
	if rec.Code != 200 {
		t.Fatalf("retry %d %s", rec.Code, rec.Body.String())
	}
	tenant := rig.ctl.currentRelease(t, "shop-a")
	if _, _, host := urlParts(t, tenant.Env["DATABASE_URL"]); host != fol.ServiceHost {
		t.Fatalf("retry did not move the tenant: %s", tenant.Env["DATABASE_URL"])
	}
}

// Other followers of the old primary replicate from the promoted one.
func TestManualSwitchoverRetargetsOtherFollowers(t *testing.T) {
	rig, leader, fol := newPromoteRig(t, false)
	other, _, err := rig.store.Provision(postgres.ProvisionRequest{App: "shop-a", Follow: leader.App})
	if err != nil {
		t.Fatal(err)
	}
	rig.ctl.addApp(other.App, map[string]string{"FLYNN_POSTGRES": other.App, "FLYNN_POSTGRES_ROLE": "follower", "FLYNN_POSTGRES_LEADER": leader.App, "FLYNN_POSTGRES_PRIMARY_URL": leader.ReplicationURL()})
	*rig.node(other.App) = pgNode{up: true, recovery: true, replay: 5000}
	if rec := rig.post("/databases/"+fol.ID+"/promote", "{}"); rec.Code != 200 {
		t.Fatalf("promote %d %s", rec.Code, rec.Body.String())
	}
	got, _ := rig.store.Get(other.ID)
	if got.LeaderID != fol.ID {
		t.Fatalf("other follower leader %q, want %q", got.LeaderID, fol.ID)
	}
	rel := rig.ctl.currentRelease(t, other.App)
	if _, _, host := urlParts(t, rel.Env["FLYNN_POSTGRES_PRIMARY_URL"]); host != fol.ServiceHost || rel.Env["FLYNN_POSTGRES_LEADER"] != fol.App {
		t.Fatalf("other follower release %v", rel.Env)
	}
}

// pg:promote prints which apps moved and which keys, never the URLs.
func TestPromoteTaskPrintsMovedAppsWithoutSecrets(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"Promoted":{"App":"postgresql-reef-55266"},"Rewritten":[{"App":"shop-a","Env":{"DATABASE_URL":"postgres://app_x:secretpw@leader.postgresql-reef-55266.discoverd:5432/db"}}]}`)
	}))
	t.Cleanup(ts.Close)
	t.Setenv("POSTGRES_PLUGIN_URL", ts.URL)
	r, w, _ := os.Pipe()
	orig := os.Stdout
	os.Stdout = w
	err := runPluginTask([]string{"promote", "postgresql-reef-55266"})
	os.Stdout = orig
	_ = w.Close()
	out, _ := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "shop-a") || !strings.Contains(string(out), "DATABASE_URL") || strings.Contains(string(out), "secretpw") {
		t.Fatalf("output %q", out)
	}
}
