package compact

import (
	"context"
	"errors"

	"golang.org/x/sync/errgroup"

	"github.com/crewlet/crewlet/internal/objstore"
)

// uploadAll runs every upload AT ONCE — one goroutine and one put each, the
// first failure cancelling the rest — and answers the first failure as it
// came, an *objstore.Full among them (objstore.ErrFull: the store has no room,
// which its caller answers for that room rather than as a blip to retry). On
// return nothing it started is still running.
//
// AT ONCE, NEVER ONE AFTER ANOTHER: an object's key is minted as its upload
// begins, and the record naming it is refused once the key is older than
// objstore.RecordWithin (twelve hours). Uploaded in sequence, every later
// upload ages the FIRST key: at the store's pacing floor (objstore.PaceFor,
// thirty seconds a mebibyte) two outputs of a gibibyte take about seventeen
// hours, and the compaction would be refused upload_expired at every plan,
// for ever. At once, the last lands at most objstore.ReadBase +
// objstore.PaceFor(code.MaxPackBytes) — 10 h 41 min, every output a bundle
// of at most that — after the first began, however many there are up to
// code.MaxCompactOutputs. On S3 each upload's own parts still run as many in
// flight as the store allows within its shared allowance, which bounds the
// memory; sixteen at once hold one first buffer of an S3 part each, 128 MiB.
//
// THE ONE FUNCTION every writer of a repository's rewritten objects uploads
// through: a compaction's outputs, and a publication's parts and repack
// outputs beside them.
func uploadAll(ctx context.Context, uploads []func(context.Context) error) error {
	g, ctx := errgroup.WithContext(ctx)
	for _, up := range uploads {
		g.Go(func() error { return up(ctx) })
	}
	return g.Wait()
}

// storeFull is the refusal an upload failed with when the object store had no
// room for it (objstore.ErrFull), with the room an operator makes
// (objstore.Full.Remedy): the ONE reading of [uploadAll]'s failure that every
// loop uploading through it takes, so a full store is refused `store_full` and
// held back until the store has the room, never retried as a blip
// ([Compactor.releaseHeld]).
func storeFull(err error) (*objstore.Full, bool) {
	var full *objstore.Full
	return full, errors.As(err, &full)
}
