package gittest

import (
	"bufio"
	"io"
	"testing"
)

// FastImport runs one git fast-import session in dir, a repository, fed by
// what stream writes as fast-import reads it — so a history of gigabytes is
// never held in memory — and fails t when either side fails.
func FastImport(t testing.TB, c *Client, dir string, stream func(*bufio.Writer) error) {
	t.Helper()
	pr, pw := io.Pipe()
	wrote := make(chan error, 1)
	go func() {
		bw := bufio.NewWriterSize(pw, 1<<20)
		err := stream(bw)
		if err == nil {
			err = bw.Flush()
		}
		pw.CloseWithError(err)
		wrote <- err
	}()
	_, stderr, err := c.Try(t, dir, pr, "fast-import", "--quiet")
	// A session that ended early stops reading: closing the reader ends the
	// stream's next write, so its goroutine ends with the session.
	_ = pr.Close()
	werr := <-wrote
	switch {
	case err != nil:
		t.Fatalf("git fast-import in %s: %v\n%s", dir, err, stderr)
	case werr != nil:
		t.Fatalf("writing the fast-import stream for %s: %v", dir, werr)
	}
}
