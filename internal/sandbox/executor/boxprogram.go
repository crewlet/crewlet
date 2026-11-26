package executor

import (
	"context"
	"crypto/rand"
	"fmt"
	"path"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// writeProgram puts content at at as a program the box executes: written into
// [Paths.ProgramStaging], then copied beside at and moved over it by the box's
// own process. A file the engine wrote can be refused when run (ETXTBSY): a
// fork the engine made while the write's descriptor was open keeps it until
// that child execs, and Linux will not execute a file anything holds open for
// writing. A copy made by a process that has exited is held open by nobody.
// Moved, so a failed copy leaves at as it was, and a program an earlier run
// left running keeps its own file.
func writeProgram(ctx context.Context, box sandbox.Sandbox, at string, content []byte) error {
	q := shellword.Quote
	source := PathsFor(box).ProgramStaging() + "/" + rand.Text()
	if err := box.WriteFile(ctx, source, content); err != nil {
		return err
	}
	next := q(at + ".next")
	install := "mkdir -p " + q(path.Dir(at)) + " && cp " + q(source) + " " + next + " && mv -f " + next + " " +
		q(at) + "; r=$?; rm -f " + q(source) + " " + next + "; exit $r"
	res, err := box.Exec(ctx, install, sandbox.ExecOptions{})
	if err != nil {
		return fmt.Errorf("copying %s into place: %w", at, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("copying %s into place exited %d: %s", at, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}
