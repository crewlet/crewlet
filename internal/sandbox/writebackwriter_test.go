package sandbox_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/crewlet/crewlet/internal/hostbox/hostboxtest"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// writeBackWriterEnv makes this test binary a run of local boxes writing a
// refreshed login back, rather than the suite: see [runWriteBackWriter]. Its
// value is the shared credential directory.
const writeBackWriterEnv = "CREWLET_SANDBOX_WRITEBACK_WRITER"

// writeBackStateEnv is where that run keeps its boxes.
const writeBackStateEnv = "CREWLET_SANDBOX_WRITEBACK_STATE"

// writeBackLogins are the two logins the boxes alternate between: two files
// each, every file different, so any mix of the two is told apart from both.
var writeBackLogins = []map[string]string{
	{"auth.json": `{"token":"first"}`, "config.toml": `provider = "first"`},
	{"auth.json": `{"token":"second"}`, "config.toml": `provider = "second"`},
}

// writeBackScript is what the run of boxes writes back after its setup, as
// the kill matrix judges it: the login the setup's box writes back over the
// second login the case leaves in the shared directory, then the login each of
// the next two boxes writes back.
var writeBackScript = hostboxtest.Script{
	Names:  []string{"auth.json", "config.toml"},
	Logins: []map[string]string{writeBackLogins[0], writeBackLogins[1], writeBackLogins[0]},
}

// runWriteBackWriter is an engine's local boxes, one after another, each
// seeded with the shared login in dir, its run rewriting both files to the
// next login of writeBackScript, and its teardown writing them back, with the
// kill matrix's observer on every write-back ([hostboxtest.Writer]), for
// TestABoxTeardownKilledPartWayNeverLeavesHalfALogin to kill at each step.
// The first box is the setup.
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
	w := hostboxtest.NewWriter()
	sandbox.ObserveCredentialWrites(local, w.Steps())
	files := map[string]string{
		".fake/auth.json":   filepath.Join(dir, "auth.json"),
		".fake/config.toml": filepath.Join(dir, "config.toml"),
	}
	writeBack := func(login map[string]string) {
		box, err := local.Create(ctx, sandbox.Spec{CredentialFiles: files})
		if err != nil {
			fail(err)
		}
		for name, body := range login {
			if err := os.WriteFile(filepath.Join(box.Home(), ".fake", name), []byte(body), 0o600); err != nil {
				fail(err)
			}
		}
		if err := box.Close(ctx); err != nil {
			fail(err)
		}
	}
	writeBack(writeBackScript.Logins[0])
	w.Ready()
	for _, login := range writeBackScript.Logins[1:] {
		writeBack(login)
	}
	w.Done()
}
