package assistant

import "github.com/google/uuid"

// Running reports whether this node runs turn id now, for a suite.
func (s *Service) Running(id uuid.UUID) bool { return s.runningHere(id) }
