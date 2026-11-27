package postgres

import "testing"

func TestRewriteAttachmentEnvSwapsHostAndApp(t *testing.T) {
	prev := &Instance{App: "postgresql-basin-73690", ServiceHost: "leader.postgresql-basin-73690.discoverd"}
	next := &Instance{App: "postgresql-orchid-80127", ServiceHost: "leader.postgresql-orchid-80127.discoverd"}
	env := map[string]string{
		"FLYNN_POSTGRES":             prev.App,
		"DATABASE_URL":               "postgres://u:p@leader.postgresql-basin-73690.discoverd:5432/db?sslmode=require",
		"FLYNN_POSTGRESQL_AMBER_URL": "postgres://u:p@leader.postgresql-basin-73690.discoverd:5432/db?sslmode=require",
		"FLYNN_POSTGRES_ROLE":        "primary",
	}
	RewriteAttachmentEnv(env, prev, next)
	if env["FLYNN_POSTGRES"] != next.App {
		t.Fatalf("FLYNN_POSTGRES=%s", env["FLYNN_POSTGRES"])
	}
	if env["DATABASE_URL"] != "postgres://u:p@leader.postgresql-orchid-80127.discoverd:5432/db?sslmode=require" {
		t.Fatalf("DATABASE_URL=%s", env["DATABASE_URL"])
	}
	if env["FLYNN_POSTGRESQL_AMBER_URL"] == env["DATABASE_URL"] && env["FLYNN_POSTGRESQL_AMBER_URL"] == "" {
		t.Fatal("color URL must also move")
	}
	if got := env["FLYNN_POSTGRESQL_AMBER_URL"]; got != env["DATABASE_URL"] {
		t.Fatalf("color URL=%s", got)
	}
}

func TestRewriteAttachmentEnvNilSafe(t *testing.T) {
	RewriteAttachmentEnv(nil, nil, nil)
	RewriteAttachmentEnv(map[string]string{}, nil, &Instance{App: "postgresql-basin-73690"})
}

// NEW-51: a switchover from a primary that answered for a stable name
// (an earlier failover) moves URLs on that name too.
func TestRewriteAttachmentEnvMovesStableNameURLs(t *testing.T) {
	prev := &Instance{App: "postgresql-b-2", ServiceHost: "leader.postgresql-b-2.discoverd", ServiceAlias: "postgresql-a-1"}
	next := &Instance{App: "postgresql-c-3", ServiceHost: "leader.postgresql-c-3.discoverd"}
	env := map[string]string{
		"DATABASE_URL": "postgres://u:p@leader.postgresql-a-1.discoverd:5432/db",
		"OTHER_URL":    "postgres://u:p@leader.postgresql-a-10.discoverd:5432/db",
	}
	RewriteAttachmentEnv(env, prev, next)
	if env["DATABASE_URL"] != "postgres://u:p@leader.postgresql-c-3.discoverd:5432/db" {
		t.Fatalf("DATABASE_URL %s", env["DATABASE_URL"])
	}
	if env["OTHER_URL"] != "postgres://u:p@leader.postgresql-a-10.discoverd:5432/db" {
		t.Fatalf("another instance's URL changed: %s", env["OTHER_URL"])
	}
}

func TestRewriteReaderLoginOnlyTouchesOwnReaderURLs(t *testing.T) {
	inst := &Instance{App: "postgresql-c-3", ServiceHost: "leader.postgresql-c-3.discoverd", Role: RolePrimary, AppUser: "app_1", AppPassword: "pw1", ReaderUser: "ro_3", ReaderPassword: "ropw"}
	env := map[string]string{
		"FLYNN_POSTGRES_ROSE_URL": "postgres://ro_3:ropw@leader.postgresql-c-3.discoverd:5432/db?sslmode=require",
		"ELSEWHERE_URL":           "postgres://ro_3:ropw@leader.postgresql-d-4.discoverd:5432/db",
		"MINE_URL":                "postgres://me:x@leader.postgresql-c-3.discoverd:5432/db",
	}
	RewriteReaderLogin(env, inst)
	if env["FLYNN_POSTGRES_ROSE_URL"] != "postgres://app_1:pw1@leader.postgresql-c-3.discoverd:5432/db?sslmode=require" {
		t.Fatalf("reader URL %s", env["FLYNN_POSTGRES_ROSE_URL"])
	}
	if env["ELSEWHERE_URL"] != "postgres://ro_3:ropw@leader.postgresql-d-4.discoverd:5432/db" || env["MINE_URL"] != "postgres://me:x@leader.postgresql-c-3.discoverd:5432/db" {
		t.Fatalf("other URLs changed: %v", env)
	}
	fol := *inst
	fol.Role = RoleFollower
	env = map[string]string{"X_URL": "postgres://ro_3:ropw@leader.postgresql-c-3.discoverd:5432/db"}
	RewriteReaderLogin(env, &fol)
	if env["X_URL"] != "postgres://ro_3:ropw@leader.postgresql-c-3.discoverd:5432/db" {
		t.Fatal("a follower's reader login was replaced")
	}
}

func TestSwitchoverDeposesLeaderAndMovesItsFollowers(t *testing.T) {
	s := NewStore()
	leader, _, _ := s.Provision(ProvisionRequest{App: "shop"})
	fol, _, _ := s.Provision(ProvisionRequest{App: "shop", Follow: leader.ID})
	other, _, _ := s.Provision(ProvisionRequest{App: "shop", Follow: leader.ID})
	res, err := s.Switchover(fol.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.PreviousLeader.Role != RoleDeposed || res.PreviousLeader.LeaderID != fol.ID {
		t.Fatalf("previous leader %+v", res.PreviousLeader)
	}
	got, _ := s.Get(other.ID)
	if got.LeaderID != fol.ID || got.Role != RoleFollower {
		t.Fatalf("other follower %+v", got)
	}
	if res.Promoted.ServiceAlias != "" || res.Promoted.ReplicaPending {
		t.Fatalf("promoted %+v", res.Promoted)
	}
	// pg:upgrade's Promote keeps the old leader and its followers as they are.
	s2 := NewStore()
	l2, _, _ := s2.Provision(ProvisionRequest{App: "shop"})
	f2, _, _ := s2.Provision(ProvisionRequest{App: "shop", Follow: l2.ID})
	if res, err := s2.Promote(f2.ID); err != nil || res.PreviousLeader.Role != RolePrimary {
		t.Fatalf("upgrade promote %+v %v", res, err)
	}
}
