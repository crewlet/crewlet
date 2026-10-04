package tracker

import "context"

// The claim names, for the cases that hold one as another node would. A test
// spelling the name itself would be a second spelling of it — which is the
// thing the one function per claim exists to prevent.
var (
	MergeClaim = mergeClaim
	MoveClaim  = moveClaim
)

// Hold takes resource as one of w's walks does, for the cases that hold a
// claim the way a live walk on this very node holds it, and hands back its
// release.
func (w *Writer) Hold(ctx context.Context, resource string) (func(), error) {
	h, err := w.hold(ctx, resource)
	if err != nil {
		return nil, err
	}
	return func() { h.release(ctx) }, nil
}

// Admit takes the bulk admission one of w's bulk edits takes, for rows rows,
// and hands back its release.
func (w *Writer) Admit(ctx context.Context, rows int) (func(), error) {
	return w.admit(ctx, rows)
}

// The record version a rank order's and a purge's apply changed at, for the
// cases that hold each rule to its own version. Named rather than spelled as a
// number, because a merge that renumbers it leaves a literal asserting the old
// one.
const RewriteVersion = rewriteVersion
