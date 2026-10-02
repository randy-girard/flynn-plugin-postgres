package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Replica copy / catch-up phases shown by pg:wait and the dashboard.
const (
	PhaseStarting   = "starting"
	PhaseBasebackup = "basebackup"
	PhaseStreaming  = "streaming"
	PhaseReady      = "ready"
	PhaseFailed     = "failed"
)

// ReplicaProgress is one snapshot of follower (or upgrade replica) copy status.
type ReplicaProgress struct {
	Follower    string `json:"follower"`
	Leader      string `json:"leader,omitempty"`
	Phase       string `json:"phase"`
	Percent     int    `json:"percent"`
	BytesCopied int64  `json:"bytes_copied,omitempty"`
	BytesTotal  int64  `json:"bytes_total,omitempty"`
	LagBytes    int64  `json:"lag_bytes,omitempty"`
	Message     string `json:"message"`
	Ready       bool   `json:"ready"`
	Error       string `json:"error,omitempty"`
}

// LiveProgressFunc reads live copy status (pg_stat_progress_basebackup / lag).
type LiveProgressFunc func(inst *Instance) (*ReplicaProgress, error)

// SetLiveProgress installs a cluster-backed progress reader. Nil uses in-memory lag.
func (s *Store) SetLiveProgress(fn LiveProgressFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.liveProgress = fn
}

// Progress is the current copy/catch-up status for a follower (or ready primary).
func (s *Store) Progress(id string) (ReplicaProgress, error) {
	s.mu.Lock()
	inst := s.lookupLocked(id)
	live := s.liveProgress
	s.mu.Unlock()
	if inst == nil {
		return ReplicaProgress{}, ErrNotFound
	}
	if live != nil && inst.Role == RoleFollower {
		if got, err := live(inst); err == nil && got != nil {
			if got.Follower == "" {
				got.Follower = firstNonEmpty(inst.App, inst.ID)
			}
			got.Message = strings.TrimSpace(got.Message)
			if got.Message == "" {
				got.Message = FormatProgress(*got)
			}
			return *got, nil
		}
	}
	return storedProgress(inst), nil
}

// SetBackupProgress records basebackup bytes for tests and in-memory wait.
func (s *Store) SetBackupProgress(id string, copied, total int64) error {
	if copied < 0 || total < 0 {
		return fmt.Errorf("backup progress must be >= 0")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	inst := s.lookupLocked(id)
	if inst == nil {
		return ErrNotFound
	}
	if inst.Role != RoleFollower {
		return ErrNotFollower
	}
	inst.BackupCopied = copied
	inst.BackupTotal = total
	if total > 0 && copied < total {
		inst.CatchupMax = 0
	}
	return nil
}

func storedProgress(inst *Instance) ReplicaProgress {
	p := ReplicaProgress{
		Follower:    firstNonEmpty(inst.App, inst.ID),
		Leader:      inst.LeaderID,
		BytesCopied: inst.BackupCopied,
		BytesTotal:  inst.BackupTotal,
		LagBytes:    inst.LagBytes,
	}
	if inst.Role != RoleFollower {
		p.Phase = PhaseReady
		p.Percent = 100
		p.Ready = true
		p.Message = FormatProgress(p)
		return p
	}
	if inst.BackupTotal > 0 && inst.BackupCopied < inst.BackupTotal {
		p.Phase = PhaseBasebackup
		p.Percent = backupPercent(inst.BackupCopied, inst.BackupTotal)
		p.Message = FormatProgress(p)
		return p
	}
	if inst.LagBytes > 0 {
		p.Phase = PhaseStreaming
		max := inst.CatchupMax
		if max < inst.LagBytes {
			max = inst.LagBytes
		}
		p.Percent = streamingPercent(inst.LagBytes, max)
		p.Message = FormatProgress(p)
		return p
	}
	p.Phase = PhaseReady
	p.Percent = 100
	p.Ready = true
	p.Message = FormatProgress(p)
	return p
}

func backupPercent(copied, total int64) int {
	if total <= 0 {
		return 5
	}
	p := int(copied * 90 / total)
	if p < 1 && copied > 0 {
		p = 1
	}
	if p > 90 {
		p = 90
	}
	if p < 0 {
		p = 0
	}
	return p
}

func streamingPercent(lag, maxLag int64) int {
	if lag <= 0 {
		return 100
	}
	if maxLag <= 0 {
		return 95
	}
	caught := maxLag - lag
	if caught < 0 {
		caught = 0
	}
	p := 90 + int(caught*10/maxLag)
	if p > 99 {
		p = 99
	}
	if p < 90 {
		p = 90
	}
	return p
}

// FormatBytes is a short IEC size for CLI and UI copy.
func FormatBytes(n int64) string {
	if n < 0 {
		n = 0
	}
	u := []string{"B", "KB", "MB", "GB", "TB"}
	v := float64(n)
	i := 0
	for v >= 1024 && i < len(u)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	if v >= 10 {
		return fmt.Sprintf("%.0f %s", v, u[i])
	}
	return fmt.Sprintf("%.1f %s", v, u[i])
}

// FormatProgress is one CLI/UI line for a replica snapshot.
func FormatProgress(p ReplicaProgress) string {
	name := strings.TrimSpace(p.Follower)
	switch p.Phase {
	case PhaseFailed:
		err := strings.TrimSpace(p.Error)
		if err == "" {
			err = "replica failed"
		}
		if name != "" {
			return fmt.Sprintf("failed %s: %s", name, err)
		}
		return "failed: " + err
	case PhaseBasebackup:
		if p.BytesTotal > 0 {
			return fmt.Sprintf("basebackup %d%% (%s / %s)", p.Percent, FormatBytes(p.BytesCopied), FormatBytes(p.BytesTotal))
		}
		if name != "" {
			return fmt.Sprintf("basebackup %d%% %s", p.Percent, name)
		}
		return fmt.Sprintf("basebackup %d%%", p.Percent)
	case PhaseStreaming:
		if p.LagBytes > 0 {
			return fmt.Sprintf("streaming %d%% (lag %s)", p.Percent, FormatBytes(p.LagBytes))
		}
		return fmt.Sprintf("streaming %d%%", p.Percent)
	case PhaseStarting:
		if name != "" {
			return "starting " + name
		}
		return "starting replica"
	case PhaseReady:
		if name != "" {
			return "ready " + name
		}
		return "ready"
	default:
		if p.Message != "" {
			return p.Message
		}
		if name != "" {
			return name
		}
		return p.Phase
	}
}

// Wait blocks until follower copy is complete (lag zero and basebackup done).
func (s *Store) Wait(ctx context.Context, id string) error {
	interval := 5 * time.Millisecond
	s.mu.Lock()
	if s.liveProgress != nil {
		interval = time.Second
	}
	s.mu.Unlock()
	for {
		p, err := s.Progress(id)
		if err != nil {
			return err
		}
		if p.Phase == PhaseFailed {
			msg := strings.TrimSpace(p.Error)
			if msg == "" {
				msg = "replica failed"
			}
			return fmt.Errorf("%s", msg)
		}
		if p.Ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}
