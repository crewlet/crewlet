package configapi

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/iamdomain"
)

// RevisionStore is the revision history the service writes through, exported
// to the test package so a case can wrap it: counting the writes a dry run
// must never make is the only honest proof that it makes none.
type RevisionStore = revisionStore

// WrapRevisions replaces the service's revision history with wrap applied to
// it. The wrapper forwards to the real store, so every other behaviour is the
// one production runs.
func (s *Service) WrapRevisions(wrap func(RevisionStore) RevisionStore) {
	s.configs = wrap(s.configs)
}

// WriteBackNamed is [writeBackNamed], for the cases that pin where a patch's
// restored encoding lands on trees no schema of this build holds yet.
func WriteBackNamed(merged, restored []byte, patch map[string]any) ([]byte, error) {
	return writeBackNamed(merged, restored, patch)
}

// ClaimHolders is [claimHolders]: the fold a node runs over its directory's
// one snapshot of the seat claims, over a case's own snapshot instead — so a
// case about what holds a seat drives the production fold rather than a fake
// of its answer.
func ClaimHolders(read func(context.Context, time.Time) (iamdomain.SeatClaims,
	error)) Holders {

	return claimHolders(read)
}
