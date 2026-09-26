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
	if err := s.AddUser(a.ID, "ada", "secret-a"); err != nil {
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
	fol, _, err := s.Provision(ProvisionRequest{App: "shop", Follow: leader.ID, Runtime: "perf-l", Mode: ModeLogical})
	if err != nil {
		t.Fatal(err)
	}
	if !fol.ReadOnly || fol.Role != RoleFollower {
		t.Fatalf("follower: %+v", fol)
	}
	if fol.Runtime != "perf-l" || fol.Runtime == leader.Runtime {
		t.Fatalf("runtime leader=%s follower=%s", leader.Runtime, fol.Runtime)
	}
	if fol.Mode != ModeLogical {
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

func TestPromoteRewritesURLAndKeepsOldLeader(t *testing.T) {
	s := NewStore()
	leader, leaderEnv, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	before := leaderEnv["DATABASE_URL"]
	fol, folEnv, err := s.Provision(ProvisionRequest{App: "shop", As: "ANALYTICS", Follow: leader.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(folEnv) != 1 || folEnv["ANALYTICS_URL"] == "" || folEnv["DATABASE_URL"] != "" {
		t.Fatalf("follower env: %#v", folEnv)
	}
	still, err := s.EnvForApp(leader.ID, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if still["DATABASE_URL"] != before {
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
	if rewritten["DATABASE_URL"] == before || rewritten["DATABASE_URL"] == "" {
		t.Fatalf("primary URL not rewritten: %#v", rewritten)
	}
	if rewritten["DATABASE_URL"] != res.Promoted.Attachments[0].URL && !strings.Contains(rewritten["DATABASE_URL"], fol.App) {
		t.Fatalf("rewritten URL %s does not target promoted app %s", rewritten["DATABASE_URL"], fol.App)
	}
	analytics, err := s.EnvForApp(fol.ID, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if len(analytics) != 1 || analytics["ANALYTICS_URL"] == "" {
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
	if len(env) != 1 || env["ANALYTICS_URL"] == "" {
		t.Fatalf("env: %#v", env)
	}
	other, err := s.Attach(inst.ID, "reports", "REPORTS")
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 1 || other["REPORTS_URL"] == "" || other["ANALYTICS_URL"] != "" {
		t.Fatalf("second attach: %#v", other)
	}
	shop, err := s.EnvForApp(inst.ID, "shop")
	if err != nil || len(shop) != 1 || shop["ANALYTICS_URL"] == "" {
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
	inst, _, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	next := "postgres://elsewhere/db"
	err = s.CheckEnvSet(inst.ID, "shop", map[string]*string{"DATABASE_URL": &next})
	if !errors.Is(err, ErrAttachedEnv) {
		t.Fatalf("env set: %v", err)
	}
	foo := "ok"
	if err := s.CheckEnvSet(inst.ID, "shop", map[string]*string{"FOO": &foo}); err != nil {
		t.Fatal(err)
	}
	if err := s.Detach(inst.ID, "shop"); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckEnvSet(inst.ID, "shop", map[string]*string{"DATABASE_URL": &next}); err != nil {
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
	args, err := PsqlCommand(env["DATABASE_URL"])
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
