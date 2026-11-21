package assistant

import (
	"log/slog"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/live"
)

// Running reports whether this node runs turn id now, for a suite.
func (s *Service) Running(id uuid.UUID) bool { return s.runningHere(id) }

// SetLogger replaces the service's logger before any turn runs, for a suite
// that reads what the engine logged.
func (s *Service) SetLogger(l *slog.Logger) { s.log = l }

// FrameLog is the frame log this node holds for turn id, for a suite that
// shapes what it retains.
func (s *Service) FrameLog(id uuid.UUID) (*live.Log, bool) { return s.hub.Get(id) }
