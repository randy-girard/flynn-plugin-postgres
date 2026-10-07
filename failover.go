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
