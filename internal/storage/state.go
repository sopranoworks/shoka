package storage

// ProjectState is a project's health as tracked by the storage layer. It is held
// only in memory and recomputed at startup (and on operator demand); persisting
// it would risk diverging from reality.
type ProjectState string

const (
	// StateHealthy is the normal case: working tree matches git HEAD.
	StateHealthy ProjectState = "healthy"
	// StateCorrupted means the working tree drifted from HEAD outside the write
	// path (hand-edit, git pull, another tool). Writes are refused until recovery.
	StateCorrupted ProjectState = "corrupted"
	// StateDangerous means .git is unreadable or absent. All operations are refused.
	StateDangerous ProjectState = "dangerous"
)

// State returns the project's current state, defaulting to healthy.
func (s *FSGitStorage) State(namespace, projectName string) ProjectState {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	if st, ok := s.states[projectKey(namespace, projectName)]; ok {
		return st
	}
	return StateHealthy
}

// setState records a project's state, logging transitions to dangerous at ERROR.
func (s *FSGitStorage) setState(namespace, projectName string, st ProjectState) {
	key := projectKey(namespace, projectName)
	s.stateMu.Lock()
	prev, existed := s.states[key]
	s.states[key] = st
	s.stateMu.Unlock()
	if st != prev || !existed {
		switch st {
		case StateDangerous:
			s.log().Error("project state changed", "project", key, "state", st, "previous", prev)
		case StateCorrupted:
			s.log().Warn("project state changed", "project", key, "state", st, "previous", prev)
		default:
			s.log().Info("project state changed", "project", key, "state", st, "previous", prev)
		}
	}
}

// AllStates returns a snapshot of every tracked project's state.
func (s *FSGitStorage) AllStates() map[string]ProjectState {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	out := make(map[string]ProjectState, len(s.states))
	for k, v := range s.states {
		out[k] = v
	}
	return out
}
