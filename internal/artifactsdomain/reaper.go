package artifactsdomain

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
)

// THE TRASH REAPER'S SOURCE FOR ARTIFACTS: the hook the generic reaper duty
// (internal/trashreap) is handed, shaped as its Source contract states it, so
// the engine appends it to the reaper's sources beside the pages' with no
// adapter of its own.
//
// AN ARTIFACT TRASHED PAST [artifacts.TrashRetention] IS PURGED AS THE ENGINE
// (iam.ActorSystem), dependents first, exactly as an administrator's purge is.
// The reaper decides from a listing that may be a moment old, so the purge is
// GUARDED: an artifact restored since, or one trashed again more recently, is
// left alone, judged both before its dependents are purged and again in the
// artifact step's own snapshot.

// TrashSource is the artifacts source of the trash reaper.
type TrashSource struct {
	Reader *Reader
	Writer *Writer

	// Actor is who the reaper purges as: the engine, under the node's id.
	Actor iam.Actor

	// Now is the clock the term is judged by; nil is the wall clock.
	Now func() time.Time
}

// Name is the source's name in the reaper's report.
func (TrashSource) Name() string { return Domain{}.Name() }

// TrashedBefore lists up to limit artifacts trashed before cutoff, the longest
// trashed first.
func (s TrashSource) TrashedBefore(ctx context.Context, cutoff time.Time, limit int) ([]string, error) {
	if s.Reader == nil {
		return nil, errNoReader
	}
	ids, err := s.Reader.TrashedBefore(ctx, cutoff, limit)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out, nil
}

// PurgeTrashed purges one artifact the reaper listed, under the operation id
// the reaper derived for it, unless it is no longer trashed past the term.
func (s TrashSource) PurgeTrashed(ctx context.Context, id string, opID string) error {
	if s.Writer == nil {
		return errors.New("artifactsdomain: the trash source has no writer")
	}
	parsed, err := artifacts.ParseID(id)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	c := Call{
		Authority: artifacts.Authority{Actor: s.Actor, Role: artifacts.RoleOwner},
		OpKey:     opID,
	}
	_, err = s.Writer.purge(ctx, c, parsed, func(h Head) bool {
		return h.Status == StatusTrashed && !h.TrashedAt.IsZero() &&
			now().Sub(h.TrashedAt) >= artifacts.TrashRetention
	})
	if errors.Is(err, errNotPurgeable) {
		return nil
	}
	return err
}
