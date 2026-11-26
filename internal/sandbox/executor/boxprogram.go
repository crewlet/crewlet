package executor

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// writeProgram puts content at path as a program the box executes: written
// beside it as path.source, then copied into place by the box's own process.
// A file the engine wrote can be refused when run (ETXTBSY): a fork the
// engine made while the write's descriptor was open keeps it until that child
// execs, and Linux will not execute a file anything holds open for writing.
// A copy made by a process that has exited is held open by nobody. It is moved
// over path, so a program an earlier run left running keeps its own file.
func writeProgram(ctx context.Context, box sandbox.Sandbox, path string, content []byte) error {
	q := shellword.Quote
	source, next := q(path+".source"), q(path+".next")
	if err := box.WriteFile(ctx, path+".source", content); err != nil {
		return err
	}
	install := "cp " + source + " " + next + " && mv -f " + next + " " + q(path) + "; r=$?; rm -f " + source +
		" " + next + "; exit $r"
	res, err := box.Exec(ctx, install, sandbox.ExecOptions{})
	if err != nil {
		return fmt.Errorf("copying %s into place: %w", path, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("copying %s into place exited %d: %s", path, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}
