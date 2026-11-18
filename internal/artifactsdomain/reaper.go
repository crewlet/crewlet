package artifactsdomain

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
)

// THE TRASH TERM'S PURGE, asked by internal/trashreap past
// [artifacts.TrashRetention] or to finish a stopped purge. The reaper's listing
// may be stale, so the purge is GUARDED ([ErrNotInTrash] for an artifact
// restored or re-trashed since), and the guard is judged in the snapshot of the
// first step ([OpPurgeBegin]), before any dependent is removed: from then on a
// restore is refused, so the guard cannot turn false. Judged after the
// dependents, a restore landing between them leaves a live artifact whose
// datastore is gone.

// ErrNotInTrash refuses a purge of the trash for an artifact that is not in the
// trash, or went in at or after the cutoff the purge was asked for.
var ErrNotInTrash = errors.New("artifactsdomain: the artifact is not in the trash past the term")

// PurgeTrashed purges an artifact in the trash since before cutoff, or one a
// purge has begun on, exactly as [Writer.Purge] does, and refuses
// [ErrNotInTrash] for one that is neither.
func (w *Writer) PurgeTrashed(ctx context.Context, c Call, id artifacts.ID, cutoff time.Time) (Written, error) {
	if cutoff.IsZero() {
		return Written{}, fmt.Errorf("%w: a purge of the trash names the instant the "+
			"artifact must have been trashed before", ErrInvalid)
	}
	written, err := w.purge(ctx, c, id, func(h Head) bool {
		return h.Status == StatusTrashed && !h.TrashedAt.IsZero() && h.TrashedAt.Before(cutoff)
	})
	if errors.Is(err, errNotPurgeable) {
		return written, fmt.Errorf("%w: artifact %s is not in the trash since before %s",
			ErrNotInTrash, id, cutoff.UTC().Format(time.RFC3339))
	}
	return written, err
}
