package sandbox

import (
	"net"
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
	// A one-letter name, because a socket's path is bounded to about a
	// hundred bytes and the directory is already named for this test.
	dir := t.TempDir()
	sock := filepath.Join(dir, "s")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	if got, err := readHostFile(sock, "findings.md"); err == nil {
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
	if got, err := readHostFile(missing, "done"); err != nil || got != nil {
		t.Errorf("a missing file = %q, %v; want empty", got, err)
	}
	if tail, err := readHostTail(missing, "done", 64); err != nil || tail.Size != 0 {
		t.Errorf("a missing tail = %+v, %v; want empty", tail, err)
	}
}
