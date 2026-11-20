package postgres

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

func urlLogin(t *testing.T, raw string) (string, string, string) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		t.Fatalf("url %q: %v", raw, err)
	}
	pass, _ := u.User.Password()
	return u.User.Username(), pass, u.Hostname()
}

// NEW-06: a follower's tenant login is its own role, not the leader's app role.
func TestFollowerGetsItsOwnCredentials(t *testing.T) {
	s := NewStore()
	leader, leaderEnv, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	fol, folEnv, err := s.Provision(ProvisionRequest{App: "shop", As: "REPLICA", Follow: leader.ID})
	if err != nil {
		t.Fatal(err)
	}
	if fol.ReaderUser == "" || fol.ReaderPassword == "" {
		t.Fatalf("follower has no reader login: %+v", fol)
	}
	if fol.ReaderUser == leader.AppUser || fol.ReaderPassword == leader.AppPassword {
		t.Fatal("follower reader login must differ from the leader's app role")
	}
	if err := ValidUserName(fol.ReaderUser); err != nil {
		t.Fatalf("reader user %q: %v", fol.ReaderUser, err)
	}
	raw := folEnv["REPLICA_URL"]
	if raw == "" {
		t.Fatalf("follower env %#v", folEnv)
	}
	user, pass, host := urlLogin(t, raw)
	if user != fol.ReaderUser || pass != fol.ReaderPassword {
		t.Fatalf("follower URL logs in as %q, want the follower's own reader", user)
	}
	if strings.Contains(raw, leader.AppPassword) {
		t.Fatal("follower URL carries the leader's password")
	}
	if !strings.Contains(host, fol.App) {
		t.Fatalf("follower URL host %q does not target %s", host, fol.App)
	}
	if fol.ConnectionURL() != raw {
		t.Fatalf("ConnectionURL %q != attachment %q", fol.ConnectionURL(), raw)
	}
	// Replication still authenticates as the leader's app role, which the
	// follower carries in its own (plugin-owned) release only.
	ruser, _, _ := urlLogin(t, leader.ReplicationURL())
	if ruser != leader.AppUser {
		t.Fatalf("replication URL user %q, want leader app role", ruser)
	}
	if fol.AppUser != leader.AppUser || fol.AppPassword != leader.AppPassword {
		t.Fatal("follower instance login must stay the replicated leader app role")
	}
	if !fol.HasCredential(fol.ReaderPassword) {
		t.Fatal("HasCredential must know the reader password")
	}
	// The leader keeps its own URL.
	luser, _, _ := urlLogin(t, leaderEnv["DATABASE_URL"])
	if luser != leader.AppUser {
		t.Fatalf("leader URL user %q", luser)
	}
	// Two followers never share a reader login.
	other, _, err := s.Provision(ProvisionRequest{App: "shop", As: "REPLICA2", Follow: leader.ID})
	if err != nil {
		t.Fatal(err)
	}
	if other.ReaderUser == fol.ReaderUser || other.ReaderPassword == fol.ReaderPassword {
		t.Fatal("two followers share a reader login")
	}
}

// An upgrade copy is promoted with an attachment rewrite; it needs no reader.
func TestUpgradeFollowerHasNoReader(t *testing.T) {
	s := NewStore()
	leader, _, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	fol, _, err := s.ProvisionTrusted(ProvisionRequest{Follow: leader.ID, ForUpgrade: true})
	if err != nil {
		t.Fatal(err)
	}
	if fol.ReaderUser != "" || fol.ReaderPassword != "" {
		t.Fatalf("upgrade copy got a reader login: %+v", fol)
	}
}

// Manual promote rewrites the leader's tenants to the promoted host with the
// leader's app role (replicated there), never the follower's read-only login.
func TestPromoteRewritesLeaderTenantsWithLeaderRole(t *testing.T) {
	s := NewStore()
	leader, _, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	fol, _, err := s.Provision(ProvisionRequest{App: "shop", As: "REPLICA", Follow: leader.ID})
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Promote(fol.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rewritten) == 0 {
		t.Fatal("no leader attachment rewritten")
	}
	for _, a := range res.Rewritten {
		user, pass, host := urlLogin(t, a.URL)
		if user != leader.AppUser || pass != leader.AppPassword {
			t.Fatalf("rewritten URL logs in as %q, want leader app role", user)
		}
		if !strings.Contains(host, fol.App) {
			t.Fatalf("rewritten host %q", host)
		}
	}
	// A new attachment to the promoted primary is a full login.
	env, err := s.Attach(fol.ID, "worker", "PRIMARY")
	if err != nil {
		t.Fatal(err)
	}
	user, _, _ := urlLogin(t, env["PRIMARY_URL"])
	if user != leader.AppUser {
		t.Fatalf("attach to promoted primary logs in as %q", user)
	}
}

// Keep-service failover leaves the leader's tenants on the leader's URL.
func TestPromoteKeepingServiceKeepsLeaderURL(t *testing.T) {
	s := NewStore()
	leader, leaderEnv, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	fol, _, err := s.ProvisionTrusted(ProvisionRequest{Follow: leader.ID, AutoFailover: true, Tenant: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PromoteKeepingService(fol.ID); err != nil {
		t.Fatal(err)
	}
	env, err := s.EnvForApp(leader.ID, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if env["DATABASE_URL"] != leaderEnv["DATABASE_URL"] {
		t.Fatal("keep-service promote rewrote the leader's tenant URL")
	}
}

// The reader login survives an API restart through the follower's release.
func TestInstanceFromEnvRestoresReader(t *testing.T) {
	env := map[string]string{
		"FLYNN_POSTGRES":        "postgresql-follow-12345",
		"POSTGRES_USER":         "app_leader",
		"POSTGRES_PASSWORD":     "adminpw",
		"POSTGRES_DB":           "db_shop",
		"FLYNN_POSTGRES_ROLE":   "follower",
		"FLYNN_POSTGRES_LEADER": "postgresql-lead-12345",
		EnvReaderUser:           "ro_abc",
		EnvReaderPassword:       "ropw_abc",
	}
	inst := InstanceFromEnv("", "postgresql-follow-12345", env)
	if inst == nil || inst.ReaderUser != "ro_abc" || inst.ReaderPassword != "ropw_abc" {
		t.Fatalf("reader not restored: %+v", inst)
	}
	user, _, _ := urlLogin(t, inst.ConnectionURL())
	if user != "ro_abc" {
		t.Fatalf("restored follower URL logs in as %q", user)
	}
	ruser, _, _ := urlLogin(t, inst.MaintenanceURL())
	if ruser != "app_leader" {
		t.Fatalf("maintenance URL logs in as %q, want the instance login", ruser)
	}
}

// A follower provisioned before NEW-06 has no reader and keeps working on the
// shared app role.
func TestLegacyFollowerKeepsSharedLogin(t *testing.T) {
	env := map[string]string{
		"FLYNN_POSTGRES":        "postgresql-follow-12345",
		"POSTGRES_USER":         "app_leader",
		"POSTGRES_PASSWORD":     "adminpw",
		"POSTGRES_DB":           "db_shop",
		"FLYNN_POSTGRES_ROLE":   "follower",
		"FLYNN_POSTGRES_LEADER": "postgresql-lead-12345",
	}
	inst := InstanceFromEnv("", "postgresql-follow-12345", env)
	if inst == nil {
		t.Fatal("nil")
	}
	user, _, _ := urlLogin(t, inst.ConnectionURL())
	if user != "app_leader" {
		t.Fatalf("legacy follower URL logs in as %q", user)
	}
}

func TestFollowerReaderSQL(t *testing.T) {
	got, err := FollowerReaderRoleSQL("ro_abc", "ropw_abc")
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{
		"CREATE ROLE ro_abc LOGIN PASSWORD 'ropw_abc'",
		"NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION",
		"ALTER ROLE ro_abc LOGIN PASSWORD 'ropw_abc'",
		"pg_roles WHERE rolname = 'ro_abc'",
	} {
		if !strings.Contains(got, needle) {
			t.Fatalf("missing %q in %s", needle, got)
		}
	}
	grant, err := FollowerReaderGrantSQL("ro_abc", "app_leader")
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{
		"GRANT USAGE ON SCHEMA public TO ro_abc",
		"GRANT SELECT ON ALL TABLES IN SCHEMA public TO ro_abc",
		"GRANT SELECT ON ALL SEQUENCES IN SCHEMA public TO ro_abc",
		"ALTER DEFAULT PRIVILEGES FOR ROLE app_leader IN SCHEMA public GRANT SELECT ON TABLES TO ro_abc",
		"ALTER DEFAULT PRIVILEGES FOR ROLE app_leader IN SCHEMA public GRANT SELECT ON SEQUENCES TO ro_abc",
	} {
		if !strings.Contains(got+grant, needle) {
			t.Fatalf("missing %q in %s", needle, grant)
		}
	}
	for _, bad := range []string{"GRANT ALL", "INSERT", "UPDATE", "DELETE", "CREATE ON SCHEMA"} {
		if strings.Contains(grant, bad) {
			t.Fatalf("reader grant must be read-only, found %q in %s", bad, grant)
		}
	}
	db, err := GrantReaderDatabaseSQL("ro_abc", "db_shop")
	if err != nil || db != "GRANT CONNECT ON DATABASE db_shop TO ro_abc" {
		t.Fatalf("%q %v", db, err)
	}
	for _, c := range []struct{ user, pass string }{
		{"ro;drop", "x"},
		{"ro_abc", ""},
		{"ro_abc", "a$$b"},
		{"ro_abc", "a'b"},
	} {
		if _, err := FollowerReaderRoleSQL(c.user, c.pass); err == nil {
			t.Fatalf("expected error for %q", c)
		}
	}
	if _, err := FollowerReaderGrantSQL("ro_abc", "app;x"); err == nil {
		t.Fatal("expected error for bad owner")
	}
}

// pg:upgrade replaces each follower on the new primary. The replacement keeps
// the old follower's reader login so its tenants' URLs (host-rewritten only)
// still authenticate.
func TestUpgradeReplacementFollowerKeepsReaderLogin(t *testing.T) {
	s := NewStore()
	leader, _, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	oldFol, _, err := s.Provision(ProvisionRequest{App: "shop", As: "REPLICA", Follow: leader.ID})
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Upgrade(context.Background(), leader.ID, UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ReplacedFollowers) != 1 {
		t.Fatalf("replaced %+v", res.ReplacedFollowers)
	}
	nf := res.ReplacedFollowers[0].New
	if nf.ReaderUser != oldFol.ReaderUser || nf.ReaderPassword != oldFol.ReaderPassword {
		t.Fatal("replacement follower must keep the old follower's reader login")
	}
}

func TestProvisionTrustedKeepsGivenReaderLogin(t *testing.T) {
	s := NewStore()
	leader, _, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	fol, _, err := s.ProvisionTrusted(ProvisionRequest{Follow: leader.ID, Tenant: "shop", ReaderUser: "ro_old", ReaderPassword: "ropw_old"})
	if err != nil {
		t.Fatal(err)
	}
	if fol.ReaderUser != "ro_old" || fol.ReaderPassword != "ropw_old" {
		t.Fatalf("reader login not kept: %q", fol.ReaderUser)
	}
}

// A platform replica is never attached to a tenant, so it needs no login.
func TestPlatformReplicaHasNoReader(t *testing.T) {
	s := NewStore()
	leader, _, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	rep, _, err := s.ProvisionTrusted(ProvisionRequest{Follow: leader.ID, PlatformReplica: true, Tenant: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.ReaderUser != "" {
		t.Fatalf("platform replica got a reader login %q", rep.ReaderUser)
	}
}
