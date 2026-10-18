package worklet

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

// FileTaskAt runs a create's task step on the number n, as a create whose mint
// took n does — for the case where the task lands between the mint and the
// task's own append, which no ordering of whole creates reproduces.
func (w *Writer) FileTaskAt(ctx context.Context, opID string, task Task,
	n uint64) (WriteResult, error) {

	return w.fileTask(ctx, opID, task, nil, n, settledCreate{}, nil)
}
