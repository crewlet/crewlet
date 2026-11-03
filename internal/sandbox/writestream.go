package sandbox

import (
	"context"
	"io"
)

// readUnder is content read only while ctx lasts: each piece is asked for
// after ctx is checked, so a streamed write ([Sandbox.WriteStream]) whose
// caller gave up stops at the next piece, on every backend, rather than
// reading its source to the end into a file nothing will keep.
//
// A READ ALREADY WAITING IS NOT INTERRUPTED: that is the source's own, which
// a reader handed out under the same context — the object store's stream of
// an archive — answers by ending. What this stops is the read after it.
func readUnder(ctx context.Context, content io.Reader) io.Reader {
	return contextReader{ctx: ctx, inner: content}
}

type contextReader struct {
	ctx   context.Context
	inner io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.inner.Read(p)
}
