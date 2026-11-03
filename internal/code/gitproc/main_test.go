package gitproc_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// escapeEnv, set to a path, makes this test binary the ESCAPEE of
// TestAnEscapedDescendantDoesNotHoldRun instead of a test run: it leaves the
// process group it was started in, says so by creating that path, and holds
// the stdout and stderr it inherited for longer than any case runs.
const escapeEnv = "GITPROC_TEST_ESCAPE"

func TestMain(m *testing.M) {
	if ready := os.Getenv(escapeEnv); ready != "" {
		escape(ready)
	}
	os.Exit(runSuite(m))
}

// runSuite is TestMain's body as a function, so the history the benchmarks
// built for the whole binary is removed once every one of them has run:
// os.Exit skips deferred calls.
func runSuite(m *testing.M) int {
	defer removeShared()
	return m.Run()
}

// escape never returns. Its own process group puts it out of the sweep's
// reach, which is the whole point; the case ends it with SIGKILL, so the race
// runtime's exit sleep never applies to it.
func escape(ready string) {
	if err := syscall.Setpgid(0, 0); err != nil {
		os.Exit(3)
	}
	if err := os.WriteFile(filepath.Clean(ready), nil, 0o600); err != nil {
		os.Exit(4)
	}
	time.Sleep(10 * time.Minute)
	os.Exit(0)
}
