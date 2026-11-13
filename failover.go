package postgres

// autoFailoverFollowerLocked returns the auto-failover replica of leader, if any.
func autoFailoverFollowerLocked(s *Store, leader *Instance) *Instance {
	if s == nil || leader == nil {
		return nil
	}
	for _, id := range leader.Followers {
		fol := s.lookupLocked(id)
		if fol != nil && fol.AutoFailover && fol.Role == RoleFollower {
			return fol
		}
	}
	for _, inst := range s.byID {
		if inst == nil || inst.Role != RoleFollower || !inst.AutoFailover {
			continue
		}
		if inst.LeaderID == leader.ID || inst.LeaderID == leader.App {
			return inst
		}
	}
	return nil
}

// ConvertDeposedToFollower turns a fenced old primary into an auto-failover
// replica of the new primary.
func (s *Store) ConvertDeposedToFollower(deposedID, primaryID string) (*Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.lookupLocked(deposedID)
	primary := s.lookupLocked(primaryID)
	if old == nil || primary == nil {
		return nil, ErrNotFound
	}
	if old.Role != RoleDeposed {
		return nil, ErrNotFollower
	}
	if autoFailoverFollowerLocked(s, primary) != nil {
		return nil, ErrAutoFailoverExists
	}
	old.Role = RoleFollower
	old.ReadOnly = true
	old.LeaderID = primary.ID
	old.Mode = ModeStreaming
	old.AutoFailover = true
	primary.Followers = append(primary.Followers, old.ID)
	primary.ReplicaPending = false
	return old.snapshot(), nil
}

// SetAutoFailover sets whether this instance is the HA replica of its leader.
func (s *Store) SetAutoFailover(id string, auto bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst := s.lookupLocked(id)
	if inst == nil {
		return ErrNotFound
	}
	inst.AutoFailover = auto
	return nil
}

// SetReplicaPending records that the primary still needs an auto-failover replica.
func (s *Store) SetReplicaPending(id string, pending bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst := s.lookupLocked(id)
	if inst == nil {
		return ErrNotFound
	}
	inst.ReplicaPending = pending
	return nil
}

// NeedsReplica lists primaries that auto-failover promoted and still need a replica.
func (s *Store) NeedsReplica() []*Instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Instance
	for _, inst := range s.byID {
		if inst == nil || inst.Role != RolePrimary || !inst.ReplicaPending {
			continue
		}
		if autoFailoverFollowerLocked(s, inst) != nil {
			inst.ReplicaPending = false
			continue
		}
		out = append(out, inst.snapshot())
	}
	return out
}

func platformReplicaLocked(s *Store, leader *Instance) *Instance {
	if s == nil || leader == nil {
		return nil
	}
	for _, id := range leader.Followers {
		fol := s.lookupLocked(id)
		if fol != nil && fol.PlatformReplica && fol.Role == RoleFollower {
			return fol
		}
	}
	for _, inst := range s.byID {
		if inst == nil || inst.Role != RoleFollower || !inst.PlatformReplica {
			continue
		}
		if inst.LeaderID == leader.ID || inst.LeaderID == leader.App {
			return inst
		}
	}
	return nil
}

// retargetUserFollowersLocked moves read-only user followers from the
// deposed primary onto the new primary. They stay followers. The platform
// replica is not included and is not marked for promotion.
func (s *Store) retargetUserFollowersLocked(from, to *Instance) {
	if s == nil || from == nil || to == nil {
		return
	}
	moved := map[string]bool{}
	take := func(fol *Instance) {
		if fol == nil || fol.ID == "" || fol.ID == to.ID || fol.ID == from.ID || moved[fol.ID] {
			return
		}
		if fol.Role != RoleFollower || fol.PlatformReplica {
			return
		}
		if fol.LeaderID != from.ID && fol.LeaderID != from.App {
			return
		}
		fol.LeaderID = to.ID
		fol.Role = RoleFollower
		fol.ReadOnly = true
		fol.AutoFailover = false
		moved[fol.ID] = true
		to.Followers = appendFollowerID(to.Followers, fol.ID)
	}
	for _, id := range append([]string(nil), from.Followers...) {
		take(s.lookupLocked(id))
	}
	for _, inst := range s.byID {
		take(inst)
	}
	kept := from.Followers[:0]
	for _, id := range from.Followers {
		fol := s.lookupLocked(id)
		if fol != nil && moved[fol.ID] {
			continue
		}
		if moved[id] {
			continue
		}
		kept = append(kept, id)
	}
	from.Followers = kept
}

func appendFollowerID(list []string, id string) []string {
	for _, cur := range list {
		if cur == id {
			return list
		}
	}
	return append(list, id)
}

// PlatformReplicas are the failover copies system mode promotes.
func (s *Store) PlatformReplicas() []*Instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Instance
	for _, inst := range s.byID {
		if inst != nil && inst.Role == RoleFollower && inst.PlatformReplica {
			out = append(out, inst.snapshot())
		}
	}
	return out
}

// MissingPlatformReplica lists primaries that do not have a platform replica.
func (s *Store) MissingPlatformReplica() []*Instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Instance
	for _, inst := range s.byID {
		if inst == nil || inst.Role != RolePrimary {
			continue
		}
		if platformReplicaLocked(s, inst) != nil {
			inst.ReplicaPending = false
			continue
		}
		out = append(out, inst.snapshot())
	}
	return out
}

// AutoFailoverFollowers are replicas that should promote if their primary job is gone.
func (s *Store) AutoFailoverFollowers() []*Instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Instance
	for _, inst := range s.byID {
		if inst != nil && inst.Role == RoleFollower && inst.AutoFailover {
			out = append(out, inst.snapshot())
		}
	}
	return out
}

// DeposedPrimaries are fenced old leaders waiting to become replicas or be deleted.
func (s *Store) DeposedPrimaries() []*Instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Instance
	for _, inst := range s.byID {
		if inst != nil && inst.Role == RoleDeposed {
			out = append(out, inst.snapshot())
		}
	}
	return out
}
