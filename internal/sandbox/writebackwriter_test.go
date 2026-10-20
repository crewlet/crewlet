package sandbox_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// writeBackWriterEnv makes this test binary a run of local boxes writing a
// refreshed login back over and over, rather than the suite: see
// [runWriteBackWriter]. Its value is the shared credential directory.
const writeBackWriterEnv = "CREWLET_SANDBOX_WRITEBACK_WRITER"

// writeBackStateEnv is where that run keeps its boxes.
const writeBackStateEnv = "CREWLET_SANDBOX_WRITEBACK_STATE"

// writeBackLogins are the two logins the run alternates between: two files
// each, every file different, so any mix of the two is told apart from both.
var writeBackLogins = []map[string]string{
	{"auth.json": `{"token":"first"}`, "config.toml": `provider = "first"`},
	{"auth.json": `{"token":"second"}`, "config.toml": `provider = "second"`},
}

// runWriteBackWriter is an engine's local boxes, one after another, each
// seeded with the shared login in dir, its run rewriting both files to the
// other login, and its teardown writing them back — for
// TestABoxTeardownKilledPartWayNeverLeavesHalfALogin to kill at any instant. It
// says "ready" once its first write-back has landed.
func runWriteBackWriter(dir string) {
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ctx := context.Background()
	local, err := sandbox.NewLocal(sandbox.LocalOptions{
		Placement: sandbox.Direct, StateDir: os.Getenv(writeBackStateEnv),
	})
	if err != nil {
		fail(err)
	}
	files := map[string]string{
		".fake/auth.json":   filepath.Join(dir, "auth.json"),
		".fake/config.toml": filepath.Join(dir, "config.toml"),
	}
	for i := 0; ; i++ {
		box, err := local.Create(ctx, sandbox.Spec{CredentialFiles: files})
		if err != nil {
			fail(err)
		}
		for name, body := range writeBackLogins[i%2] {
			if err := os.WriteFile(filepath.Join(box.Home(), ".fake", name), []byte(body), 0o600); err != nil {
				fail(err)
			}
		}
		if err := box.Close(ctx); err != nil {
			fail(err)
		}
		if i == 0 {
			fmt.Println("ready")
		}
	}
}
