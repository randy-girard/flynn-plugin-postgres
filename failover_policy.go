package postgres

import "strings"

const (
	AvailabilitySystem = "system"
	AvailabilityManual = "manual"

	// FailoverLagBoundBytes is the most WAL a replica may still need before
	// it can be promoted. Async replication can drop commits inside this bound.
	FailoverLagBoundBytes int64 = 16 << 20

	// PlatformReplicaBatch is how many new platform replicas one 5s tick may start.
	PlatformReplicaBatch = 1

	EnvPlatformReplica = "FLYNN_POSTGRES_PLATFORM_REPLICA"
	EnvServiceAlias    = "FLYNN_POSTGRES_SERVICE_ALIAS"
)

// ReplicaHealth is what the watch knows about a failover target.
type ReplicaHealth struct {
	InRecovery bool
	LagBytes   int64
	LagKnown   bool
	Basebackup bool
}

// ReplicaPromotable is true only while the target is in recovery, not copying
// its base backup, and within FailoverLagBoundBytes. A failed lag read is not promotable.
func ReplicaPromotable(h ReplicaHealth) bool {
	if !h.LagKnown || !h.InRecovery || h.Basebackup {
		return false
	}
	return h.LagBytes >= 0 && h.LagBytes <= FailoverLagBoundBytes
}

// HostFit is one host's room for a database volume.
type HostFit struct {
	ID              string
	MemoryAvailable uint64
	DiskFree        uint64
	StatsKnown      bool
	ProcessCount    int
	MaxProcesses    int
	SingletonCount  int
	MaxSingletons   int
}

// HostCanTake reports whether this host can accept the volume. Unknown stats
// or an unknown runtime size do not count as room.
func HostCanTake(h HostFit, needMem, needDisk uint64) bool {
	if h.ID == "" || !h.StatsKnown || needMem == 0 || needDisk == 0 {
		return false
	}
	if h.MemoryAvailable < needMem || h.DiskFree < needDisk {
		return false
	}
	if h.MaxProcesses > 0 && h.ProcessCount >= h.MaxProcesses {
		return false
	}
	if h.MaxSingletons > 0 && h.SingletonCount >= h.MaxSingletons {
		return false
	}
	return true
}

// OtherHostCanTake returns another host that can take the volume.
func OtherHostCanTake(hosts []HostFit, avoid string, needMem, needDisk uint64) (string, bool) {
	avoid = strings.TrimSpace(avoid)
	for _, h := range hosts {
		if h.ID == "" || h.ID == avoid {
			continue
		}
		if HostCanTake(h, needMem, needDisk) {
			return h.ID, true
		}
	}
	return "", false
}

// LeaderServices are the discoverd services this process claims. A promoted
// primary also claims the original primary name. A follower claims only its own.
func LeaderServices(role, own, alias string) []string {
	own = strings.TrimSpace(own)
	alias = strings.TrimSpace(alias)
	if own == "" {
		return nil
	}
	out := []string{own}
	if strings.EqualFold(strings.TrimSpace(role), "primary") && alias != "" && alias != own {
		out = append(out, alias)
	}
	return out
}

// BatchIDs returns up to n sorted ids. n < 1 returns nothing.
func BatchIDs(ids []string, n int) []string {
	if n <= 0 || len(ids) == 0 {
		return nil
	}
	out := append([]string(nil), ids...)
	for i := 1; i < len(out); i++ {
		j := i
		for j > 0 && out[j] < out[j-1] {
			out[j], out[j-1] = out[j-1], out[j]
			j--
		}
	}
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// UserFollowerNames drops platform replica apps from the names that block delete.
func UserFollowerNames(names []string, platform map[string]bool) []string {
	var out []string
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || platform[name] {
			continue
		}
		out = append(out, name)
	}
	return out
}
