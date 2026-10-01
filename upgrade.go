package postgres

import (
	"context"
	"strings"
	"time"
)

// TaskKindUpgrade is follow → wait → promote for the primary, then recreate each follower.
const TaskKindUpgrade = "upgrade"

// Task status values for a background upgrade.
const (
	TaskPending            = "pending"
	TaskFollowing          = "following"
	TaskWaiting            = "waiting"
	TaskPromoting          = "promoting"
	TaskReplacingFollowers = "replacing_followers"
	TaskDone               = "done"
	TaskFailed             = "failed"
)

// DefaultUpgradeTimeout bounds one automated upgrade step (basebackup + catch-up).
const DefaultUpgradeTimeout = 30 * time.Minute

// Task is one background plugin job. Upgrade walks the whole topology.
type Task struct {
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`
	Target     string          `json:"target"`
	FollowerID string          `json:"follower_id,omitempty"`
	Mode       ReplicationMode `json:"replication,omitempty"`
	Status     string          `json:"status"`
	Error      string          `json:"error,omitempty"`
	StartedAt  time.Time       `json:"started_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

// UpgradeOptions selects replication mode and optional hooks after a follower
// resource exists (live clusters start the isolated job) or an old follower is dropped.
type UpgradeOptions struct {
	Mode        ReplicationMode
	Runtime     string
	Timeout     time.Duration
	AfterFollow func(*Instance) error
	AfterDrop   func(*Instance) error
}

// FollowerReplace is one old follower recreated against the new primary.
type FollowerReplace struct {
	Old *Instance
	New *Instance
}

// UpgradeResult is a finished topology upgrade.
type UpgradeResult struct {
	Task              *Task
	Follower          *Instance
	Promoted          *Instance
	PreviousLeader    *Instance
	Rewritten         []Attachment
	ReplacedFollowers []FollowerReplace
	Dropped           []string
}

func (t *Task) snapshot() *Task {
	if t == nil {
		return nil
	}
	out := *t
	return &out
}

func (t *Task) done() bool {
	return t != nil && (t.Status == TaskDone || t.Status == TaskFailed)
}

func (s *Store) ensureTaskMaps() {
	if s.tasks == nil {
		s.tasks = map[string]*Task{}
	}
	if s.upgradeByLeader == nil {
		s.upgradeByLeader = map[string]string{}
	}
}

func (s *Store) activeUpgradeLocked(leaderID string) *Task {
	s.ensureTaskMaps()
	id := s.upgradeByLeader[leaderID]
	if id == "" {
		return nil
	}
	task := s.tasks[id]
	if task == nil || task.done() {
		return nil
	}
	return task
}

// GetTask returns a copy of one background task.
func (s *Store) GetTask(id string) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureTaskMaps()
	task := s.tasks[id]
	if task == nil {
		return nil, ErrNotFound
	}
	return task.snapshot(), nil
}

// ListTasks returns every background task, newest first.
func (s *Store) ListTasks() []*Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureTaskMaps()
	out := make([]*Task, 0, len(s.tasks))
	for _, task := range s.tasks {
		out = append(out, task.snapshot())
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].StartedAt.After(out[i].StartedAt) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// LatestUpgrade is the newest upgrade task for a primary (id or app name).
func (s *Store) LatestUpgrade(id string) *Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	leader := s.lookupLocked(id)
	if leader == nil {
		return nil
	}
	s.ensureTaskMaps()
	if tid := s.upgradeByLeader[leader.ID]; tid != "" {
		if task := s.tasks[tid]; task != nil {
			return task.snapshot()
		}
	}
	var latest *Task
	for _, task := range s.tasks {
		if task.Kind != TaskKindUpgrade || task.Target != leader.ID {
			continue
		}
		if latest == nil || task.StartedAt.After(latest.StartedAt) {
			latest = task
		}
	}
	return latest.snapshot()
}

func (s *Store) setTaskLocked(id, status, errMsg, followerID string) {
	s.ensureTaskMaps()
	task := s.tasks[id]
	if task == nil {
		return
	}
	task.Status = status
	task.Error = errMsg
	if followerID != "" {
		task.FollowerID = followerID
	}
	task.UpdatedAt = time.Now().UTC()
}

// StartUpgrade runs the topology upgrade in the background.
func (s *Store) StartUpgrade(id string, opts UpgradeOptions) (*Task, error) {
	s.mu.Lock()
	leader := s.lookupLocked(id)
	if leader == nil {
		s.mu.Unlock()
		return nil, ErrNotFound
	}
	if leader.Role == RoleFollower {
		s.mu.Unlock()
		return nil, ErrNotPrimary
	}
	if s.activeUpgradeLocked(leader.ID) != nil {
		s.mu.Unlock()
		return nil, ErrUpgradeInProgress
	}
	now := time.Now().UTC()
	task := &Task{
		ID:        "upg-" + newID(),
		Kind:      TaskKindUpgrade,
		Target:    leader.ID,
		Mode:      opts.Mode,
		Status:    TaskPending,
		StartedAt: now,
		UpdatedAt: now,
	}
	if task.Mode == "" {
		task.Mode = ModeLogical
	}
	s.ensureTaskMaps()
	s.tasks[task.ID] = task
	s.upgradeByLeader[leader.ID] = task.ID
	leaderID := leader.ID
	nFollowers := len(s.followerIDsLocked(leader))
	s.mu.Unlock()

	go s.runUpgrade(leaderID, task.ID, opts, nFollowers)
	return task.snapshot(), nil
}

func (s *Store) runUpgrade(leaderID, taskID string, opts UpgradeOptions, nFollowers int) {
	timeout := opts.Timeout
	if timeout <= 0 {
		n := 1 + nFollowers
		if n < 1 {
			n = 1
		}
		timeout = DefaultUpgradeTimeout * time.Duration(n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, err := s.Upgrade(ctx, leaderID, opts)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.setTaskLocked(taskID, TaskFailed, err.Error(), "")
		return
	}
	s.setTaskLocked(taskID, TaskDone, "", "")
}

func (s *Store) followerIDsLocked(leader *Instance) []string {
	if leader == nil {
		return nil
	}
	seen := map[string]bool{}
	var ids []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || id == leader.ID || seen[id] {
			return
		}
		fol := s.findLocked(id)
		if fol == nil {
			return
		}
		if fol.Role != RoleFollower {
			return
		}
		seen[fol.ID] = true
		ids = append(ids, fol.ID)
	}
	for _, fid := range leader.Followers {
		add(fid)
	}
	for _, other := range s.byID {
		if other == nil || other.ID == leader.ID {
			continue
		}
		if other.LeaderID == leader.ID || other.LeaderID == leader.App {
			add(other.ID)
		}
	}
	return ids
}

// Upgrade promotes a new primary on the current image, then recreates each
// existing follower so it replicates from the new primary. Followers are never
// promoted. The old primary remains as its own resource.
func (s *Store) Upgrade(ctx context.Context, id string, opts UpgradeOptions) (*UpgradeResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	leader := s.lookupLocked(id)
	if leader == nil {
		s.mu.Unlock()
		return nil, ErrNotFound
	}
	if leader.Role == RoleFollower {
		s.mu.Unlock()
		return nil, ErrNotPrimary
	}
	leaderID := leader.ID
	oldFollowers := append([]string(nil), s.followerIDsLocked(leader)...)
	mode := opts.Mode
	if mode == "" {
		mode = ModeLogical
	}
	runtime := opts.Runtime
	s.mu.Unlock()

	s.markUpgrade(leaderID, TaskFollowing, "", "")
	fol, _, err := s.Provision(ProvisionRequest{
		Follow:     leaderID,
		Mode:       mode,
		Runtime:    runtime,
		ForUpgrade: true,
	})
	if err != nil {
		return nil, err
	}
	s.markUpgrade(leaderID, TaskWaiting, "", fol.ID)
	if opts.AfterFollow != nil {
		if err := opts.AfterFollow(fol); err != nil {
			return nil, err
		}
	}
	if err := s.Wait(ctx, fol.ID); err != nil {
		return nil, err
	}
	s.markUpgrade(leaderID, TaskPromoting, "", fol.ID)
	promo, err := s.Promote(fol.ID)
	if err != nil {
		return nil, err
	}
	newPrimary := promo.Promoted
	res := &UpgradeResult{
		Follower:       fol,
		Promoted:       newPrimary,
		PreviousLeader: promo.PreviousLeader,
		Rewritten:      promo.Rewritten,
	}
	if len(oldFollowers) == 0 {
		s.markUpgrade(leaderID, TaskDone, "", fol.ID)
		return res, nil
	}
	s.markUpgrade(leaderID, TaskReplacingFollowers, "", fol.ID)
	for _, oldID := range oldFollowers {
		old, err := s.Get(oldID)
		if err != nil {
			return nil, err
		}
		followRuntime := old.Runtime
		if runtime != "" {
			followRuntime = runtime
		}
		nf, _, err := s.Provision(ProvisionRequest{
			Follow:  newPrimary.ID,
			Mode:    ModeStreaming,
			Runtime: followRuntime,
		})
		if err != nil {
			return nil, err
		}
		if opts.AfterFollow != nil {
			if err := opts.AfterFollow(nf); err != nil {
				return nil, err
			}
		}
		if err := s.Wait(ctx, nf.ID); err != nil {
			return nil, err
		}
		if opts.AfterDrop != nil {
			if err := opts.AfterDrop(old); err != nil {
				return nil, err
			}
		}
		s.Forget(old.ID)
		res.ReplacedFollowers = append(res.ReplacedFollowers, FollowerReplace{Old: old, New: nf})
		res.Dropped = append(res.Dropped, old.App)
	}
	s.markUpgrade(leaderID, TaskDone, "", fol.ID)
	return res, nil
}

func (s *Store) markUpgrade(leaderID, status, errMsg, followerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureTaskMaps()
	if tid := s.upgradeByLeader[leaderID]; tid != "" {
		s.setTaskLocked(tid, status, errMsg, followerID)
	}
}
