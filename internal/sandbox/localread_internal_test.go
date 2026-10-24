package sandbox

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// A FILE THAT CANNOT BE OPENED IS AN ERROR, never an empty one — the open
// half of what TestALocalBoxAnswersAFileItCannotReadAsAnError holds for a
// read. A Unix socket stands in for a file the engine may not open, because
// opening one fails on every host and for every user, the root this suite may
// run as included (permissions would not: root reads past them).
func TestALocalFileThatCannotBeOpenedIsAnError(t *testing.T) {
	t.Parallel()
	// NOT UNDER t.TempDir, whose directory is named for this test: a socket's
	// path is bounded (104 bytes on macOS, 108 on Linux), and the test's name
	// under macOS's per-user temporary directory already runs past it, so
	// the listen failed there before the case was reached. A short directory
	// of its own and a one-letter name stay inside the bound on both.
	dir, err := os.MkdirTemp("", "sock") //nolint:usetesting // t.TempDir is named for the test, past a socket path's bound
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	if got, err := readHostFile(sock, "findings.md", MaxFileBytes); err == nil {
		t.Errorf("readHostFile = %q, nil; want the open's failure, not an empty file", got)
	}
	if r, err := openHostFile(sock, "stream.jsonl"); err == nil {
		_ = r.Close()
		t.Error("openHostFile = a reader, nil; want the open's failure, not an empty stream")
	}
	if tail, err := readHostTail(sock, "err.log", 64); err == nil {
		t.Errorf("readHostTail = %+v, nil; want the open's failure", tail)
	}
	// And absence is still empty, for all three.
	missing := filepath.Join(dir, "absent")
	if got, err := readHostFile(missing, "done", MaxFileBytes); err != nil || got != nil {
		t.Errorf("a missing file = %q, %v; want empty", got, err)
	}
	if tail, err := readHostTail(missing, "done", 64); err != nil || tail.Size != 0 {
		t.Errorf("a missing tail = %+v, %v; want empty", tail, err)
	}
}
