package compact

import (
	"context"
	"errors"

	"golang.org/x/sync/errgroup"

	"github.com/crewlet/crewlet/internal/objstore"
)

// uploadAll runs every upload AT ONCE, one goroutine each, the first failure
// cancelling the rest, and answers that failure as it came (an *objstore.Full
// among them: [storeFull]); nothing it started outlives it. Every writer of
// rewritten objects uploads through it: a compaction's outputs, a
// publication's parts and repack outputs.
//
// Never in sequence (package doc, "The uploads, and a store with no room"): a
// key minted as its upload begins is refused past objstore.RecordWithin (12 h),
// and two 1 GiB outputs in sequence at objstore.PaceFor's floor (30 s/MiB) take
// ~17 h, refused upload_expired at every plan. At once, gitlet.MaxCompactOutputs
// of gitlet.MaxPackBytes land within objstore.ReadBase + objstore.PaceFor(that),
// 10 h 41 min; S3 parts share the store's allowance (16 first buffers, 128 MiB).
func uploadAll(ctx context.Context, uploads []func(context.Context) error) error {
	g, ctx := errgroup.WithContext(ctx)
	for _, up := range uploads {
		g.Go(func() error { return up(ctx) })
	}
	return g.Wait()
}

// storeFull is the refusal an upload failed with when the object store had no
// room (objstore.ErrFull), with the room an operator makes
// (objstore.Full.Remedy): the ONE reading of [uploadAll]'s failure every
// uploading loop takes, so a full store is refused `store_full` and held back
// until it has room, never retried as a blip ([Compactor.releaseHeld]).
func storeFull(err error) (*objstore.Full, bool) {
	var full *objstore.Full
	return full, errors.As(err, &full)
}
