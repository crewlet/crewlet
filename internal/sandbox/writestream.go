package sandbox

import (
	"context"
	"io"
)

// readUnder is content read only while ctx lasts, so a streamed write
// ([Sandbox.WriteStream]) whose caller gave up stops at the next piece on
// every backend rather than reading its source to the end into a file nothing
// will keep. A read already waiting is not interrupted: that is the source's
// own to end (the object store's archive stream ends under the same context).
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
