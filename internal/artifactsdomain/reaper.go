package artifactsdomain

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
)

// THE TRASH TERM'S PURGE: what the trash reaper (internal/trashreap) asks of
// this domain once an artifact has been in the trash past
// [artifacts.TrashRetention]. The reaper's listing may be a moment old, so the
// purge is GUARDED: an artifact restored since, or trashed again more recently,
// is refused [ErrNotInTrash], judged both before its dependents are purged and
// again in the artifact step's own snapshot.

// ErrNotInTrash refuses a purge of the trash for an artifact that is not in the
// trash, or went in at or after the cutoff the purge was asked for.
var ErrNotInTrash = errors.New("artifactsdomain: the artifact is not in the trash past the term")

// PurgeTrashed purges an artifact in the trash since before cutoff, exactly as
// [Writer.Purge] does (its dependents first), and refuses [ErrNotInTrash] for
// one that is not.
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
