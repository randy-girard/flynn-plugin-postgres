package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestTwoResourcesAreIsolated(t *testing.T) {
	s := NewStore()
	a, envA, err := s.Provision(ProvisionRequest{App: "shop-a", Tenant: "tenant-a"})
	if err != nil {
		t.Fatal(err)
	}
	b, envB, err := s.Provision(ProvisionRequest{App: "shop-b", Tenant: "tenant-b"})
	if err != nil {
		t.Fatal(err)
	}
	if a.App == b.App || a.Volume == b.Volume || a.Superuser == b.Superuser || a.SuperuserPassword == b.SuperuserPassword {
		t.Fatalf("resources share identity: %+v %+v", a, b)
	}
	if a.AppUser == b.AppUser || a.AppPassword == b.AppPassword {
		t.Fatal("resources share app credentials")
	}
	if err := s.AddUser(a.ID, "ada", "secret-a", ""); err != nil {
		t.Fatal(err)
	}
	users, err := s.Users(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 0 {
		t.Fatalf("user on A visible on B: %+v", users)
	}
	got, err := s.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HasCredential(a.SuperuserPassword) || got.HasCredential(a.AppPassword) || got.HasCredential("secret-a") {
		t.Fatal("B holds a credential that can read A")
	}
	for _, env := range []map[string]string{envA, envB} {
		for k, v := range env {
			if strings.Contains(v, a.SuperuserPassword) || strings.Contains(v, b.SuperuserPassword) {
				t.Fatalf("tenant env %s contains a superuser password", k)
			}
			if strings.Contains(v, PlatformApplianceHost) {
				t.Fatalf("tenant env targets the platform appliance: %s", v)
			}
		}
	}
}

func TestProvisionNeverTargetsPlatformAppliance(t *testing.T) {
	if err := EndpointAllowed("http://postgres-api.discoverd/databases"); !errors.Is(err, ErrPlatformAppliance) {
		t.Fatalf("appliance URL: %v", err)
	}
	if err := EndpointAllowed("http://" + PlatformApplianceHost + ":3000/databases"); err == nil {
		t.Fatal("platform host with port must be rejected")
	}
	if err := EndpointAllowed(ProviderURL()); err != nil {
		t.Fatal(err)
	}
	if ProviderURL() == "http://postgres-api.discoverd/databases" || ProviderName == PlatformProviderName {
		t.Fatal("plugin provider must not be the appliance")
	}

	s := NewStore()
	s.providerURL = "http://postgres-api.discoverd/databases"
	if _, _, err := s.Provision(ProvisionRequest{App: "shop"}); !errors.Is(err, ErrPlatformAppliance) {
		t.Fatalf("provision: %v", err)
	}
	for _, c := range s.Contacts() {
		if strings.Contains(c, PlatformApplianceHost) {
			t.Fatalf("contacted appliance: %s", c)
		}
	}

	ok := NewStore()
	if _, _, err := ok.Provision(ProvisionRequest{App: "shop"}); err != nil {
		t.Fatal(err)
	}
	contacts := ok.Contacts()
	if len(contacts) != 1 || contacts[0] != ProviderURL() {
		t.Fatalf("contacts: %#v", contacts)
	}
	for _, c := range contacts {
		if strings.Contains(c, "postgres-api.discoverd") {
			t.Fatalf("tenant provision contacted %s", c)
		}
	}
}

func TestOneNodeByDefault(t *testing.T) {
	s := NewStore()
	inst, _, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	plan := inst.NodePlan()
	if plan.Sirenia {
		t.Fatal("sirenia must not start")
	}
	if len(plan.Processes) != 1 || plan.Processes[ProcessName] != 1 {
		t.Fatalf("processes: %#v", plan.Processes)
	}
	if plan.Volume == "" || plan.VolumePath != VolumePath || plan.App != inst.App {
		t.Fatalf("plan: %+v", plan)
	}
	if inst.Nodes != DefaultNodes {
		t.Fatalf("nodes: %d", inst.Nodes)
	}
}

func TestFollowerReadOnlyCannotFollowFollower(t *testing.T) {
	s := NewStore()
	leader, _, err := s.Provision(ProvisionRequest{App: "shop", Runtime: "standard-1"})
	if err != nil {
		t.Fatal(err)
	}
	fol, _, err := s.Provision(ProvisionRequest{App: "shop", Follow: leader.ID, Runtime: "perf-l", Mode: ModeStreaming})
	if err != nil {
		t.Fatal(err)
	}
	if !fol.ReadOnly || fol.Role != RoleFollower {
		t.Fatalf("follower: %+v", fol)
	}
	if fol.Runtime != "perf-l" || fol.Runtime == leader.Runtime {
		t.Fatalf("runtime leader=%s follower=%s", leader.Runtime, fol.Runtime)
	}
	if fol.Mode != ModeStreaming {
		t.Fatalf("mode: %s", fol.Mode)
	}
	if fol.App == leader.App || fol.Volume == leader.Volume || fol.Superuser == leader.Superuser {
		t.Fatal("follower shares app, volume, or superuser with the leader")
	}
	if err := s.Write(fol.ID, "db", "k", "v"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := s.Provision(ProvisionRequest{App: "shop", Follow: fol.ID}); !errors.Is(err, ErrFollowFollower) {
		t.Fatalf("follow follower: %v", err)
	}
	info, err := s.Info(leader.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Followers) != 1 || info.Followers[0] != fol.ID {
		t.Fatalf("info followers: %+v", info)
	}
	finfo, err := s.Info(fol.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finfo.LeaderID != leader.ID || finfo.LagBytes != 0 || !finfo.ReadOnly {
		t.Fatalf("follower info: %+v", finfo)
	}
}

func TestFollowLooksUpLeaderByAppName(t *testing.T) {
	s := NewStore()
	leader, _, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	fol, _, err := s.Provision(ProvisionRequest{App: "shop", Follow: leader.App, As: "FOLLOWER"})
	if err != nil {
		t.Fatal(err)
	}
	if fol.LeaderID != leader.ID || fol.AppUser != leader.AppUser {
		t.Fatalf("follow by app: %+v leader %+v", fol, leader)
	}
}

func TestFollowRejectsLogicalAndVersionMismatch(t *testing.T) {
	s := NewStore()
	leader, _, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Provision(ProvisionRequest{App: "shop", Follow: leader.ID, Mode: ModeLogical}); !errors.Is(err, ErrFollowLogical) {
		t.Fatalf("logical follow: %v", err)
	}
	if _, _, err := s.Provision(ProvisionRequest{App: "shop", Follow: leader.ID, Mode: ModeLogical, ForUpgrade: true}); err != nil {
		t.Fatalf("upgrade follow: %v", err)
	}

	s2 := NewStore()
	old, _, err := s2.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	s2.mu.Lock()
	s2.byID[old.ID].EngineVersion = "15"
	s2.mu.Unlock()
	t.Setenv("ENGINE_VERSION", "16")
	if _, _, err := s2.Provision(ProvisionRequest{App: "shop", Follow: old.ID}); !errors.Is(err, ErrFollowVersion) {
		t.Fatalf("version follow: %v", err)
	}
}

func TestFollowLoadsMissingLeaderFromLiveApp(t *testing.T) {
	s := NewStore()
	var lookedUp string
	s.LoadMissing = func(name string) *Instance {
		lookedUp = name
		if name != "pg-orchid-xkhthp" {
			return nil
		}
		return InstanceFromEnv("app-uuid", "pg-orchid-xkhthp", map[string]string{
			"FLYNN_POSTGRES":    "pg-orchid-xkhthp",
			"POSTGRES_USER":     "app_live",
			"POSTGRES_PASSWORD": "secret",
			"POSTGRES_DB":       "db_pg_orchid_xkhthp",
			"POSTGRES_URL":      "postgres://app_live:secret@leader.pg-orchid-xkhthp.discoverd:5432/db_pg_orchid_xkhthp?sslmode=require",
		})
	}
	fol, _, err := s.Provision(ProvisionRequest{App: "shop", Follow: "pg-orchid-xkhthp", As: "FOLLOWER"})
	if err != nil {
		t.Fatal(err)
	}
	if lookedUp != "pg-orchid-xkhthp" {
		t.Fatalf("lookup %q", lookedUp)
	}
	if fol.LeaderID != "app-uuid" || fol.AppUser != "app_live" || fol.Role != RoleFollower {
		t.Fatalf("follower %+v", fol)
	}
	if _, _, err := s.Provision(ProvisionRequest{App: "shop", Follow: "missing"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestInstanceFromEnv(t *testing.T) {
	inst := InstanceFromEnv("id", "pg-orchid-xkhthp", map[string]string{
		"FLYNN_POSTGRES":    "pg-orchid-xkhthp",
		ResourceIDEnv:       "res-abc",
		"POSTGRES_USER":     "app_live",
		"POSTGRES_PASSWORD": "secret",
		"POSTGRES_DB":       "db_pg_orchid_xkhthp",
	})
	if inst == nil || inst.App != "pg-orchid-xkhthp" || inst.AppUser != "app_live" || inst.Role != RolePrimary {
		t.Fatalf("%+v", inst)
	}
	if inst.ID != "res-abc" {
		t.Fatalf("resource id %q", inst.ID)
	}
	if len(inst.Databases) != 1 || inst.Databases[0].Name != "db_pg_orchid_xkhthp" {
		t.Fatalf("db %#v", inst.Databases)
	}
	fol := InstanceFromEnv("id", "pg-willow-abcdef", map[string]string{
		"FLYNN_POSTGRES":  "pg-willow-abcdef",
		"POSTGRES_ROLE":   "follower",
		"POSTGRES_LEADER": "pg-orchid-xkhthp",
		"POSTGRES_USER":   "app_live",
	})
	if fol == nil || fol.Role != RoleFollower || fol.LeaderID != "pg-orchid-xkhthp" {
		t.Fatalf("follower %+v", fol)
	}
	unfollowed := InstanceFromEnv("id", "pg-willow-abcdef", map[string]string{
		"FLYNN_POSTGRES":       "pg-willow-abcdef",
		"POSTGRES_ROLE":        "primary",
		"POSTGRES_LEADER":      "pg-orchid-xkhthp",
		"POSTGRES_PRIMARY_URL": "postgres://leader/db",
		"POSTGRES_USER":        "app_live",
	})
	if unfollowed == nil || unfollowed.Role != RolePrimary || unfollowed.LeaderID != "" || unfollowed.ReadOnly {
		t.Fatalf("unfollowed leftover markers %+v", unfollowed)
	}
	if InstanceFromEnv("id", "shop", map[string]string{"REDIS_URL": "redis://x"}) != nil {
		t.Fatal("non-postgres env")
	}
	if InstanceFromEnv("id", "app-one", map[string]string{
		"DATABASE_URL":   "postgres://u:p@leader.postgresql-harbor-12345.discoverd:5432/db?sslmode=require",
		"FLYNN_POSTGRES": "postgresql-harbor-12345",
	}) != nil {
		t.Fatal("tenant app env is not an isolated instance")
	}
	if InstanceFromEnv("id", "app-one", map[string]string{
		"DATABASE_URL": "postgres://u:p@leader.postgresql-harbor-12345.discoverd:5432/db?sslmode=require",
	}) != nil {
		t.Fatal("tenant DATABASE_URL must not hydrate a postgres instance")
	}
	fromPG := InstanceFromEnv("res-1", "", map[string]string{
		"FLYNN_POSTGRES": "pg-harbor-kxmnpq",
		"PGUSER":         "app_from_pg",
		"PGPASSWORD":     "secret",
		"PGDATABASE":     "db_pg_harbor_kxmnpq",
		"PGHOST":         "leader.pg-harbor-kxmnpq.discoverd",
	})
	if fromPG == nil || fromPG.AppUser != "app_from_pg" || fromPG.AppPassword != "secret" || fromPG.ServiceHost != "leader.pg-harbor-kxmnpq.discoverd" {
		t.Fatalf("pg env %+v", fromPG)
	}
}

func TestLookupLockedMergesLoadMissingDuplicateApp(t *testing.T) {
	s := NewStore()
	inst, _, err := s.Provision(ProvisionRequest{App: "shop-a", Tenant: "shop-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddUser(inst.ID, "alice", "secret", inst.Databases[0].Name); err != nil {
		t.Fatal(err)
	}
	s.LoadMissing = func(name string) *Instance {
		return InstanceFromEnv("live-uuid", inst.App, map[string]string{
			"FLYNN_POSTGRES":    inst.App,
			"POSTGRES_USER":     "app_live",
			"POSTGRES_PASSWORD": "secret",
			"POSTGRES_DB":       inst.Databases[0].Name,
		})
	}
	s.mu.Lock()
	got := s.lookupLocked("live-uuid")
	s.mu.Unlock()
	if got == nil || got.ID != inst.ID {
		t.Fatalf("expected provisioned instance, got %+v", got)
	}
	users, err := s.Users(inst.App)
	if err != nil || len(users) != 1 || users[0].Name != "alice" {
		t.Fatalf("users %+v %v", users, err)
	}
}

func TestMaintenanceURLUsesTenantDatabase(t *testing.T) {
	s := NewStore()
	inst, _, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if len(inst.Databases) == 0 {
		t.Fatal("expected tenant database")
	}
	db := inst.Databases[0].Name
	got := inst.MaintenanceURL()
	if strings.Contains(got, "/postgres?") || strings.HasSuffix(got, "/postgres") {
		t.Fatalf("admin URL must not use catalog database postgres: %s", got)
	}
	if !strings.Contains(got, "/"+db) {
		t.Fatalf("admin URL %s missing tenant database %s", got, db)
	}
}

func TestPromoteRewritesURLAndKeepsOldLeader(t *testing.T) {
	s := NewStore()
	leader, leaderEnv, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	beforeKey, before := firstAppURL(leaderEnv)
	if before == "" {
		t.Fatalf("leader env: %#v", leaderEnv)
	}
	fol, folEnv, err := s.Provision(ProvisionRequest{App: "shop", As: "ANALYTICS", Follow: leader.ID})
	if err != nil {
		t.Fatal(err)
	}
	if folEnv["ANALYTICS_URL"] == "" || folEnv["DATABASE_URL"] != "" {
		t.Fatalf("follower env: %#v", folEnv)
	}
	still, err := s.EnvForApp(leader.ID, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if still[beforeKey] != before {
		t.Fatal("follower attach replaced the leader URL")
	}
	res, err := s.Promote(fol.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Promoted.ReadOnly || res.Promoted.Role != RolePrimary || res.Promoted.LeaderID != "" {
		t.Fatalf("promoted: %+v", res.Promoted)
	}
	old, err := s.Get(leader.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.App != leader.App {
		t.Fatal("old leader resource was removed")
	}
	rewritten, err := s.EnvForApp(leader.ID, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if rewritten[beforeKey] == before || rewritten[beforeKey] == "" {
		t.Fatalf("primary URL not rewritten: %#v", rewritten)
	}
	if rewritten[beforeKey] != res.Promoted.Attachments[0].URL && !strings.Contains(rewritten[beforeKey], fol.App) {
		t.Fatalf("rewritten URL %s does not target promoted app %s", rewritten[beforeKey], fol.App)
	}
	analytics, err := s.EnvForApp(fol.ID, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if analytics["ANALYTICS_URL"] == "" {
		t.Fatalf("follower attachment: %#v", analytics)
	}
}

func TestUnfollowIsWritableAndStopsReceiving(t *testing.T) {
	s := NewStore()
	leader, _, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(leader.ID, "db", "before", "1"); err != nil {
		t.Fatal(err)
	}
	fol, _, err := s.Provision(ProvisionRequest{App: "shop", Follow: leader.ID, Mode: ModeStreaming})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.Rows(fol.ID)
	if err != nil || len(rows) != 1 || rows[0].Key != "before" {
		t.Fatalf("copy: %+v %v", rows, err)
	}
	got, err := s.Unfollow(fol.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReadOnly || got.Role != RoleStandalone || got.LeaderID != "" {
		t.Fatalf("unfollowed: %+v", got)
	}
	if err := s.Write(fol.ID, "db", "local", "2"); err != nil {
		t.Fatalf("writable: %v", err)
	}
	if err := s.Write(leader.ID, "db", "after", "3"); err != nil {
		t.Fatal(err)
	}
	rows, err = s.Rows(fol.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Key == "after" {
			t.Fatal("unfollowed instance received a leader write")
		}
	}
}

func TestAsSetsOneEnvVarAndDetachRemovesIt(t *testing.T) {
	s := NewStore()
	inst, env, err := s.Provision(ProvisionRequest{App: "shop", As: "ANALYTICS"})
	if err != nil {
		t.Fatal(err)
	}
	if env["ANALYTICS_URL"] == "" || env["DATABASE_URL"] == "" {
		t.Fatalf("env: %#v", env)
	}
	if colorURL(env) != "" {
		t.Fatalf("provision --as ANALYTICS must not also set a color URL: %#v", env)
	}
	locked := map[string]*string{"ANALYTICS_URL": strPtr("postgres://x")}
	for k := range env {
		if strings.HasSuffix(k, "_URL") {
			locked[k] = strPtr("postgres://z")
		}
	}
	if err := s.CheckEnvSet(inst.ID, "shop", locked); err == nil {
		t.Fatal("attached URLs must be locked")
	}
	other, err := s.Attach(inst.ID, "reports", "REPORTS")
	if err != nil {
		t.Fatal(err)
	}
	if other["REPORTS_URL"] == "" || other["ANALYTICS_URL"] != "" || other["DATABASE_URL"] != "" {
		t.Fatalf("second attach: %#v", other)
	}
	shop, err := s.EnvForApp(inst.ID, "shop")
	if err != nil || shop["ANALYTICS_URL"] == "" || shop["DATABASE_URL"] == "" {
		t.Fatalf("shop env: %#v %v", shop, err)
	}
	if err := s.Detach(inst.ID, "shop"); err != nil {
		t.Fatal(err)
	}
	shop, err = s.EnvForApp(inst.ID, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if len(shop) != 0 {
		t.Fatalf("detach left env: %#v", shop)
	}
	if _, err := s.EnvForApp(inst.ID, "reports"); err != nil {
		t.Fatal(err)
	}
}

func TestEnvSetAttachedURLRejectedUntilDetach(t *testing.T) {
	s := NewStore()
	inst, env, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if env["DATABASE_URL"] == "" || colorURL(env) == "" {
		t.Fatalf("env: %#v", env)
	}
	next := "postgres://elsewhere/db"
	err = s.CheckEnvSet(inst.ID, "shop", map[string]*string{"DATABASE_URL": &next})
	if !errors.Is(err, ErrAttachedEnv) {
		t.Fatalf("env set: %v", err)
	}
	cKey, _ := firstColorURL(env)
	if cKey == "" {
		t.Fatalf("missing color: %#v", env)
	}
	err = s.CheckEnvSet(inst.ID, "shop", map[string]*string{cKey: &next})
	if !errors.Is(err, ErrAttachedEnv) {
		t.Fatalf("color env set: %v", err)
	}
	foo := "ok"
	if err := s.CheckEnvSet(inst.ID, "shop", map[string]*string{"FOO": &foo}); err != nil {
		t.Fatal(err)
	}
	if err := s.Detach(inst.ID, "shop"); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckEnvSet(inst.ID, "shop", map[string]*string{cKey: &next}); err != nil {
		t.Fatalf("after detach: %v", err)
	}
}

func TestWaitUntilFakeLagReachesZero(t *testing.T) {
	s := NewStore()
	leader, _, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	fol, _, err := s.Provision(ProvisionRequest{App: "shop", Follow: leader.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetLag(fol.ID, 40); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(leader.ID, "db", "late", "1"); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Rows(fol.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Key == "late" {
			t.Fatal("lagging follower applied a write")
		}
	}
	done := make(chan error, 1)
	go func() {
		done <- s.Wait(context.Background(), fol.ID)
	}()
	time.Sleep(20 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("wait returned early: %v", err)
	default:
	}
	if err := s.SetLag(fol.ID, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("wait did not return when lag reached zero")
	}
	info, err := s.Info(fol.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.LagBytes != 0 {
		t.Fatalf("lag: %d", info.LagBytes)
	}
	rows, err = s.Rows(fol.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.Key == "late" {
			found = true
		}
	}
	if !found {
		t.Fatal("catch-up did not copy the leader write")
	}
}

func TestPsqlTargetsInstanceURLOnly(t *testing.T) {
	s := NewStore()
	inst, env, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	_, conn := firstAppURL(env)
	args, err := PsqlCommand(conn)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 2 || args[0] != "psql" || !strings.Contains(args[1], inst.ServiceHost) {
		t.Fatalf("args: %#v", args)
	}
	if _, err := PsqlCommand("http://postgres-api.discoverd/databases"); !errors.Is(err, ErrPlatformAppliance) {
		t.Fatalf("platform psql: %v", err)
	}
}

func TestSecondDatabaseOnAnAppUsesNamedURL(t *testing.T) {
	s := NewStore()
	_, envA, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if envA["DATABASE_URL"] == "" || colorURL(envA) == "" {
		t.Fatalf("first: %#v", envA)
	}
	_, envB, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	bKey, _ := firstColorURL(envB)
	if envB["DATABASE_URL"] != "" || bKey == "" {
		t.Fatalf("second: %#v", envB)
	}
}

func firstAppURL(env map[string]string) (key, val string) {
	if v := strings.TrimSpace(env["DATABASE_URL"]); v != "" {
		return "DATABASE_URL", v
	}
	return firstColorURL(env)
}

func firstColorURL(env map[string]string) (key, val string) {
	for k, v := range env {
		if postgresColorURLKey(k) && v != "" {
			return k, v
		}
	}
	return "", ""
}

func colorURL(env map[string]string) string {
	_, v := firstColorURL(env)
	return v
}

func hasNamedDatabaseURL(env map[string]string) bool {
	for k, v := range env {
		if v != "" && (postgresColorURLKey(k) || strings.HasSuffix(k, "_DATABASE_URL") && k != "DATABASE_URL") {
			return true
		}
	}
	return false
}

func strPtr(s string) *string { return &s }

func TestNoInPlaceResize(t *testing.T) {
	s := NewStore()
	if err := s.Resize("any", "perf-l"); !errors.Is(err, ErrNoResize) {
		t.Fatal(err)
	}
}

func TestDashboardHidesOtherTenants(t *testing.T) {
	s := NewStore()
	a, _, err := s.Provision(ProvisionRequest{App: "shop-a", Tenant: "shop-a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Provision(ProvisionRequest{App: "shop-b", Tenant: "shop-b"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddDatabase(a.ID, "only_a"); err != nil {
		t.Fatal(err)
	}
	visible := s.ForApp("shop-a")
	if len(visible) != 1 || visible[0].ID != a.ID {
		t.Fatalf("visible: %+v", visible)
	}
	for _, inst := range visible {
		for _, db := range inst.Databases {
			if strings.Contains(db.Name, "shop-b") {
				t.Fatal("other tenant leaked")
			}
		}
	}
}

func TestDestroyRejectedWhileFollowersLinked(t *testing.T) {
	s := NewStore()
	leader, _, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	fol, _, err := s.Provision(ProvisionRequest{App: "shop", Follow: leader.App})
	if err != nil {
		t.Fatal(err)
	}
	names := s.FollowerApps(leader.App)
	if len(names) == 0 {
		t.Fatal("leader must list the follower")
	}
	s.Forget(fol.ID)
	if names := s.FollowerApps(leader.ID); len(names) != 0 {
		t.Fatalf("unlinked %q", names)
	}
	s.Forget(leader.ID)
	if _, err := s.Get(leader.ID); err == nil {
		t.Fatal("forgotten leader still present")
	}
}

func TestFollowerAppsIgnoresGhostReplicas(t *testing.T) {
	s := NewStore()
	leader, _, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	s.byID[leader.ID].Followers = append(s.byID[leader.ID].Followers, "postgresql-orchid-80127")
	names := s.FollowerApps(leader.ID)
	if len(names) != 0 {
		t.Fatalf("deleted replica must not block: %q", names)
	}
	if err := CanDeleteResource(leader, names); err != nil {
		t.Fatalf("primary with a ghost follower: %v", err)
	}
	if got := s.byID[leader.ID].Followers; len(got) != 0 {
		t.Fatalf("stale Followers kept: %q", got)
	}
}

func TestReconcileFollowersDropsGoneReplicas(t *testing.T) {
	s := NewStore()
	leader, _, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	fol, _, err := s.Provision(ProvisionRequest{App: "shop", Follow: leader.App})
	if err != nil {
		t.Fatal(err)
	}
	s.ReconcileFollowers(leader.ID, nil)
	if _, err := s.Get(fol.ID); err == nil {
		t.Fatal("gone follower still in store")
	}
	if names := s.FollowerApps(leader.ID); len(names) != 0 {
		t.Fatalf("reconcile left %q", names)
	}
}

func TestCanDeleteResource(t *testing.T) {
	s := NewStore()
	leader, _, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if err := CanDeleteResource(leader, nil); err != nil {
		t.Fatalf("leader with no followers: %v", err)
	}
	fol, _, err := s.Provision(ProvisionRequest{App: "shop", Follow: leader.App})
	if err != nil {
		t.Fatal(err)
	}
	if err := CanDeleteResource(fol, s.FollowerApps(fol.ID)); err != nil {
		t.Fatalf("follower must be deletable: %v", err)
	}
	if err := CanDeleteResource(leader, s.FollowerApps(leader.App)); !errors.Is(err, ErrHasFollowers) {
		t.Fatalf("leader with follower: %v", err)
	}
	if _, err := s.Unfollow(fol.ID); err != nil {
		t.Fatal(err)
	}
	standalone, err := s.Get(fol.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := CanDeleteResource(standalone, s.FollowerApps(standalone.ID)); err != nil {
		t.Fatalf("unfollowed copy: %v", err)
	}
	if err := CanDeleteResource(nil, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("nil instance: %v", err)
	}
}

func TestAutoFailoverFollowRejectedWithoutFollow(t *testing.T) {
	s := NewStore()
	if _, _, err := s.Provision(ProvisionRequest{App: "shop", AutoFailover: true}); !errors.Is(err, ErrAutoFailoverFollow) {
		t.Fatalf("got %v", err)
	}
}

func TestAutoFailoverPromoteDeposesLeaderAndNeedsReplica(t *testing.T) {
	s := NewStore()
	leader, _, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	fol, _, err := s.Provision(ProvisionRequest{App: "shop", Follow: leader.ID, AutoFailover: true})
	if err != nil {
		t.Fatal(err)
	}
	if !fol.AutoFailover {
		t.Fatal("follower must record auto-failover")
	}
	if _, _, err := s.Provision(ProvisionRequest{App: "shop", Follow: leader.ID, AutoFailover: true}); !errors.Is(err, ErrAutoFailoverExists) {
		t.Fatalf("second auto follower: %v", err)
	}
	info, err := s.Info(fol.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Follows != leader.App || !info.AutoFailover {
		t.Fatalf("info: %+v", info)
	}
	res, err := s.Promote(fol.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Promoted.Role != RolePrimary || !res.Promoted.ReplicaPending || res.Promoted.AutoFailover {
		t.Fatalf("promoted: %+v", res.Promoted)
	}
	old, err := s.Get(leader.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.Role != RoleDeposed || !old.ReadOnly {
		t.Fatalf("deposed: %+v", old)
	}
	need := s.NeedsReplica()
	if len(need) != 1 || need[0].ID != fol.ID {
		t.Fatalf("needs replica: %+v", need)
	}
	converted, err := s.ConvertDeposedToFollower(old.ID, fol.ID)
	if err != nil {
		t.Fatal(err)
	}
	if converted.Role != RoleFollower || !converted.AutoFailover || converted.LeaderID != fol.ID {
		t.Fatalf("converted: %+v", converted)
	}
	primary, err := s.Get(fol.ID)
	if err != nil {
		t.Fatal(err)
	}
	if primary.ReplicaPending {
		t.Fatal("replica pending after convert")
	}
}

func TestAdoptLinksFollowerToLeader(t *testing.T) {
	s := NewStore()
	leader, _, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	got := s.Adopt(&Instance{
		ID:       "postgresql-valley-30620",
		App:      "postgresql-valley-30620",
		Role:     RoleFollower,
		LeaderID: leader.App,
		ReadOnly: true,
	})
	if got == nil || got.App != "postgresql-valley-30620" {
		t.Fatalf("adopt: %+v", got)
	}
	names := s.FollowerApps(leader.ID)
	if len(names) != 1 || names[0] != "postgresql-valley-30620" {
		t.Fatalf("followers=%v", names)
	}
	if err := s.SetAutoFailover(got.ID, false); err != nil {
		t.Fatal(err)
	}
	fol, err := s.Get(got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fol.AutoFailover {
		t.Fatal("SetAutoFailover(false) must stick")
	}
}
