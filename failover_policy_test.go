package postgres

import (
	"strings"
	"testing"
)

func TestReplicaPromotableRequiresRecoveryLagAndNoBasebackup(t *testing.T) {
	ok := ReplicaHealth{InRecovery: true, LagBytes: 0, LagKnown: true}
	if !ReplicaPromotable(ok) {
		t.Fatal("caught-up replica must be promotable")
	}
	if ReplicaPromotable(ReplicaHealth{InRecovery: true, LagBytes: FailoverLagBoundBytes + 1, LagKnown: true}) {
		t.Fatal("lag past the bound must wait")
	}
	if ReplicaPromotable(ReplicaHealth{InRecovery: false, LagKnown: true}) {
		t.Fatal("a writable replica is not a failover target")
	}
	if ReplicaPromotable(ReplicaHealth{InRecovery: true, LagKnown: true, Basebackup: true}) {
		t.Fatal("basebackup must finish first")
	}
	if ReplicaPromotable(ReplicaHealth{InRecovery: true}) {
		t.Fatal("an unknown lag must not promote")
	}
}

func TestOtherHostCanTakeChecksMemoryDiskAndCaps(t *testing.T) {
	hosts := []HostFit{
		{ID: "primary", StatsKnown: true, MemoryAvailable: 1 << 30, DiskFree: 1 << 40, ProcessCount: 1},
		{ID: "full", StatsKnown: true, MemoryAvailable: 1 << 20, DiskFree: 1 << 40},
		{ID: "capped", StatsKnown: true, MemoryAvailable: 8 << 30, DiskFree: 1 << 40, ProcessCount: 4, MaxProcesses: 4},
		{ID: "ok", StatsKnown: true, MemoryAvailable: 8 << 30, DiskFree: 50 << 30, ProcessCount: 1, MaxProcesses: 10, SingletonCount: 1, MaxSingletons: 4},
	}
	id, ok := OtherHostCanTake(hosts, "primary", 1<<30, 10<<30)
	if !ok || id != "ok" {
		t.Fatalf("host %q ok %v", id, ok)
	}
	if _, ok := OtherHostCanTake(hosts, "primary", 0, 10<<30); ok {
		t.Fatal("unknown memory must not count as room")
	}
	if _, ok := OtherHostCanTake([]HostFit{{ID: "b", MemoryAvailable: 8 << 30, DiskFree: 50 << 30}}, "a", 1<<30, 1<<30); ok {
		t.Fatal("missing stats must not count as room")
	}
}

func TestLeaderServicesClaimAliasOnlyForPrimary(t *testing.T) {
	if got := LeaderServices("follower", "postgresql-new", "postgresql-old"); len(got) != 1 || got[0] != "postgresql-new" {
		t.Fatalf("follower services %v", got)
	}
	got := LeaderServices("primary", "postgresql-new", "postgresql-old")
	if len(got) != 2 || got[0] != "postgresql-new" || got[1] != "postgresql-old" {
		t.Fatalf("promoted services %v", got)
	}
	if got := LeaderServices("primary", "postgresql-new", "postgresql-new"); len(got) != 1 {
		t.Fatalf("same name must be claimed once: %v", got)
	}
	if LeaderServices("deposed", "postgresql-old", "postgresql-old") != nil && LeaderServices("deposed", "postgresql-old", "postgresql-older")[0] != "postgresql-old" {
		t.Fatal("deposed must not claim the alias")
	}
	deposed := LeaderServices("deposed", "postgresql-old", "postgresql-older")
	if len(deposed) != 1 || deposed[0] != "postgresql-old" {
		t.Fatalf("deposed %v", deposed)
	}
}

func TestBatchIDsIsStableAndBounded(t *testing.T) {
	got := BatchIDs([]string{"c", "a", "b"}, PlatformReplicaBatch)
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("batch %v", got)
	}
	if BatchIDs([]string{"a"}, 0) != nil {
		t.Fatal("zero batch must be empty")
	}
}

func TestUserFollowerNamesSkipPlatformReplicas(t *testing.T) {
	got := UserFollowerNames([]string{"user-a", "platform-b", " "}, map[string]bool{"platform-b": true})
	if len(got) != 1 || got[0] != "user-a" {
		t.Fatalf("names %v", got)
	}
}

func TestPromoteKeepingServiceLeavesAttachmentURL(t *testing.T) {
	s := NewStore()
	leader, _, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.EnvForApp(leader.ID, "shop")
	if err != nil {
		t.Fatal(err)
	}
	fol, _, err := s.Provision(ProvisionRequest{Follow: leader.ID, PlatformReplica: true})
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.PromoteKeepingService(fol.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Promoted.ServiceAlias != leader.App {
		t.Fatalf("alias %q", res.Promoted.ServiceAlias)
	}
	if len(res.Rewritten) != 0 {
		t.Fatalf("rewrote %d attachments", len(res.Rewritten))
	}
	after, err := s.EnvForApp(leader.ID, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if after["DATABASE_URL"] != before["DATABASE_URL"] {
		t.Fatalf("DATABASE_URL changed\nbefore %s\nafter  %s", before["DATABASE_URL"], after["DATABASE_URL"])
	}
	if res.PreviousLeader.Role != RoleDeposed || !res.Promoted.ReplicaPending {
		t.Fatalf("promoted %+v previous %+v", res.Promoted.Role, res.PreviousLeader.Role)
	}
}

func TestPromoteKeepingServiceRetargetsUserFollowersAndChainsAlias(t *testing.T) {
	s := NewStore()
	leader, _, err := s.Provision(ProvisionRequest{App: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	user, _, err := s.Provision(ProvisionRequest{App: "shop", Follow: leader.ID})
	if err != nil {
		t.Fatal(err)
	}
	platform, _, err := s.Provision(ProvisionRequest{Follow: leader.ID, PlatformReplica: true})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.PromoteKeepingService(platform.ID)
	if err != nil {
		t.Fatal(err)
	}
	gotUser, err := s.Get(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotUser.Role != RoleFollower || !gotUser.ReadOnly || gotUser.LeaderID != first.Promoted.ID {
		t.Fatalf("user follower %+v should follow the new primary %s", gotUser, first.Promoted.ID)
	}
	if gotUser.AutoFailover || gotUser.PlatformReplica {
		t.Fatal("a retargeted user follower is not the failover target")
	}
	if first.Promoted.ServiceAlias != leader.App {
		t.Fatalf("first alias %q", first.Promoted.ServiceAlias)
	}
	primary, err := s.Get(first.Promoted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(primary.ReplicationURL(), "leader."+leader.App+".discoverd") {
		t.Fatalf("replication url %q", primary.ReplicationURL())
	}
	next, _, err := s.Provision(ProvisionRequest{Follow: first.Promoted.ID, PlatformReplica: true})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.PromoteKeepingService(next.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.Promoted.ServiceAlias != leader.App {
		t.Fatalf("second failover must keep the original discoverd name, alias %q", second.Promoted.ServiceAlias)
	}
	gotUser, err = s.Get(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotUser.Role != RoleFollower || !gotUser.ReadOnly || gotUser.LeaderID != second.Promoted.ID {
		t.Fatalf("user follower after second failover %+v want leader %s", gotUser, second.Promoted.ID)
	}
}
