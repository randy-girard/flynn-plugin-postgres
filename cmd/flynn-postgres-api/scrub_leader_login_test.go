package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/randy-girard/flynn-plugin-postgres"
	ct "github.com/randy-girard/flynn/controller/types"
)

// NEW-52: tenant apps attached to a follower made before NEW-06 still carry
// FLYNN_POSTGRES_PRIMARY_URL (the leader's app role login) and use the
// leader's login in their follower URL. Maintenance removes the key, gives
// the follower its own read-only login and moves its apps onto it; the
// follower's own instance release keeps its replication URL.
func legacyFollowerRig(t *testing.T) (*promoteRig, *postgres.Instance, *postgres.Instance) {
	t.Helper()
	rig, leader, fol := newPromoteRig(t, false)
	// A follower made before NEW-06: no login of its own.
	if err := rig.store.SetReaderLogin(fol.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	fol, _ = rig.store.Get(fol.ID)
	legacyURL := fol.ConnectionURL()
	if !strings.Contains(legacyURL, leader.AppUser+":") {
		t.Fatalf("fixture: legacy follower URL %s should use the leader login", legacyURL)
	}
	primaryURL := leader.ReplicationURL()
	rig.ctl.addApp("shop-a", map[string]string{
		"FLYNN_POSTGRES_ROSE_URL":    legacyURL,
		"FLYNN_POSTGRES_PRIMARY_URL": primaryURL,
		"MY_PRIMARY_URL":             "postgres://me:x@db.example.com:5432/x",
		"DATABASE_URL":               leader.ConnectionURL(),
	})
	rig.ctl.resources = []*ct.Resource{
		{ID: "res-leader", Apps: []string{"shop-a"}, Env: map[string]string{"DATABASE_URL": leader.ConnectionURL(), "FLYNN_POSTGRES": leader.App, "FLYNN_POSTGRES_ROLE": "primary"}},
		{ID: "res-follower", Apps: []string{"shop-a"}, Env: map[string]string{
			"FLYNN_POSTGRES_ROSE_URL": legacyURL, "FLYNN_POSTGRES_PRIMARY_URL": primaryURL,
			"FLYNN_POSTGRES": fol.App, "FLYNN_POSTGRES_ROLE": "follower", "FLYNN_POSTGRES_LEADER": leader.App,
		}},
	}
	return rig, leader, fol
}

func TestScrubRemovesLeaderLoginFromFollowerAttachments(t *testing.T) {
	rig, leader, fol := legacyFollowerRig(t)
	before := rig.ctl.currentRelease(t, fol.App)
	rig.h.scrubFollowerAttachments(nil)

	tenant := rig.ctl.currentRelease(t, "shop-a")
	if _, ok := tenant.Env["FLYNN_POSTGRES_PRIMARY_URL"]; ok {
		t.Fatalf("tenant release still has FLYNN_POSTGRES_PRIMARY_URL: %v", tenant.Env)
	}
	if strings.Contains(strings.Join(values(tenant.Env, "FLYNN_POSTGRES_ROSE_URL"), ""), leader.AppPassword) {
		t.Fatalf("tenant follower URL still has the leader's login: %s", tenant.Env["FLYNN_POSTGRES_ROSE_URL"])
	}
	user, pass, host := urlParts(t, tenant.Env["FLYNN_POSTGRES_ROSE_URL"])
	got, _ := rig.store.Get(fol.ID)
	if got.ReaderUser == "" || user != got.ReaderUser || pass != got.ReaderPassword || host != fol.ServiceHost {
		t.Fatalf("tenant follower URL %s@%s, want the follower's own login %q", user, host, got.ReaderUser)
	}
	// The follower's own login was created on the leader (replication
	// copies it to the follower).
	if len(rig.opsWith("sql "+leader.App+" other")) == 0 {
		t.Fatalf("no login created on the leader: %v", rig.ops)
	}
	// The tenant's own keys and its leader attachment are untouched.
	if tenant.Env["MY_PRIMARY_URL"] != "postgres://me:x@db.example.com:5432/x" || tenant.Env["DATABASE_URL"] != leader.ConnectionURL() {
		t.Fatalf("tenant keys changed: %v", tenant.Env)
	}
	if rig.index("deploy shop-a") < 0 {
		t.Fatalf("tenant not deployed: %v", rig.ops)
	}
	row := rig.ctl.resource("res-follower")
	if _, ok := row.Env["FLYNN_POSTGRES_PRIMARY_URL"]; ok || strings.Contains(row.Env["FLYNN_POSTGRES_ROSE_URL"], leader.AppPassword) {
		t.Fatalf("follower resource row still has the leader's login: %v", row.Env)
	}
	// The follower instance itself still replicates with the leader URL and
	// now records its own login.
	own := rig.ctl.currentRelease(t, fol.App)
	if own.Env["FLYNN_POSTGRES_PRIMARY_URL"] == "" || own.Env[postgres.EnvReaderUser] != got.ReaderUser {
		t.Fatalf("follower release %v (was %v)", own.Env, before.Env)
	}
	// Running it again changes nothing.
	n := len(rig.opsWith("deploy "))
	rig.h.scrubFollowerAttachments(nil)
	if len(rig.opsWith("deploy ")) != n {
		t.Fatalf("second scrub deployed again: %v", rig.ops)
	}
}

// Only the leader's URL is removed: a FLYNN_POSTGRES_PRIMARY_URL the tenant
// pointed somewhere else stays.
func TestScrubKeepsATenantOwnedPrimaryURL(t *testing.T) {
	rig, _, _ := legacyFollowerRig(t)
	rel := rig.ctl.currentRelease(t, "shop-a")
	rel.Env["FLYNN_POSTGRES_PRIMARY_URL"] = "postgres://me:x@db.example.com:5432/x"
	_ = rig.ctl.CreateRelease("shop-a", rel)
	_ = rig.ctl.SetAppRelease("shop-a", rel.ID)
	rig.h.scrubFollowerAttachments(nil)
	if got := rig.ctl.currentRelease(t, "shop-a").Env["FLYNN_POSTGRES_PRIMARY_URL"]; got != "postgres://me:x@db.example.com:5432/x" {
		t.Fatalf("tenant-owned value changed: %q", got)
	}
}

func TestMaintenanceRunsTheFollowerScrub(t *testing.T) {
	h := newHandler(postgres.NewStore())
	called := false
	orig := scrubFollowerAttachmentsFn
	t.Cleanup(func() { scrubFollowerAttachmentsFn = orig })
	scrubFollowerAttachmentsFn = func(*handler, map[string]struct{}) []string { called = true; return nil }
	req := httptest.NewRequest(http.MethodPost, "/cluster/maintenance", strings.NewReader(`{"all":true}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || !called {
		t.Fatalf("maintenance %d called=%v", rec.Code, called)
	}
}

func values(env map[string]string, keys ...string) []string {
	var out []string
	for _, k := range keys {
		out = append(out, env[k])
	}
	return out
}
