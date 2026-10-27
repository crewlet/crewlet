package assistant

import (
	"log/slog"

	"github.com/google/uuid"
)

// Running reports whether this node runs turn id now, for a suite.
func (s *Service) Running(id uuid.UUID) bool { return s.runningHere(id) }

// SetLogger replaces the service's logger before any turn runs, for a suite
// that reads what the engine logged.
func (s *Service) SetLogger(l *slog.Logger) { s.log = l }
