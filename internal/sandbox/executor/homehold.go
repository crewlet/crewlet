package executor

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// heldNone marks a home file held as none: the home had no such file, so
// putting it back removes the run's. An empty file is held as an empty copy,
// and comes back empty.
const heldNone = ".none"

// holdHomeShell is the shell holding live, a file of the home's, at held before
// a run writes its own in its place, printing `h` and the home's text, or `n`
// where the home has none: a copy where it has the file, the marker
// held+[heldNone] where it has none. Where either is held already, an earlier
// run's script ended before it put the home's back, so the held copy is the
// home's own and the live file an earlier run's.
func holdHomeShell(live, held string) string {
	q := shellword.Quote
	h, none, l := q(held), q(held+heldNone), q(live)
	return fmt.Sprintf(`set -e; if [ -e %[1]s ]; then printf h; cat %[1]s; elif [ -e %[2]s ]; then printf n; `+
		`elif [ -e %[3]s ]; then cp %[3]s %[1]s; printf h; cat %[1]s; else : > %[2]s; printf n; fi`, h, none, l)
}

// holdHome runs [holdHomeShell] in box, returning the home's own text, "" for
// none.
func holdHome(ctx context.Context, box sandbox.Sandbox, live, held string) (string, error) {
	res, err := box.Exec(ctx, holdHomeShell(live, held), sandbox.ExecOptions{})
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("exit %d: %s", res.ExitCode, firstLine(res.Stderr, transcriptDetailLimit))
	}
	if err != nil {
		return "", fmt.Errorf("executor: holding the box home's own %s: %w", live, err)
	}
	own, had := strings.CutPrefix(res.Stdout, "h")
	if !had {
		return "", nil
	}
	return own, nil
}

// putBackHome is the shell putting a held home file back once the run has
// stopped, by its script's end or the next run's clear ([homeKeeper]): the
// held copy written over the run's, through a link as the home keeps it, or
// the run's removed where the home had none. Nothing where none is held, so it
// runs any number of times; a copy that will not go back says which file on
// the error stream and fails, leaving the held copy for the next try.
func putBackHome(live, held string) string { return putBackHomeAt(shellword.Quote(live), held) }

// putBackHomeAt is [putBackHome] for the live file the shell word live names,
// which may be a variable's expansion.
func putBackHomeAt(live, held string) string {
	q := shellword.Quote
	h, none, l := q(held), q(held+heldNone), live
	return fmt.Sprintf(`if [ -e %[2]s ]; then rm -f %[3]s %[2]s; elif [ -e %[1]s ]; then `+
		`{ { cat %[1]s > %[3]s; } 2> /dev/null && rm -f %[1]s; } || `+
		`{ echo "crewlet: "%[3]s" was not put back from "%[1]s >&2; false; }; fi`, h, none, l)
}

// writeHome writes body over live, a file of the home's, as the home keeps
// it: through the link where live is one, by the box's own shell from a
// staged copy in the run's work directory (the box writes no file through a
// link from outside it); otherwise as a file.
func writeHome(ctx context.Context, box sandbox.Sandbox, paths Paths, live string, body []byte) error {
	q := shellword.Quote
	res, err := box.Exec(ctx, "[ ! -L "+q(live)+" ] || echo link", sandbox.ExecOptions{})
	if err != nil {
		return fmt.Errorf("executor: looking at %s: %w", live, err)
	}
	if strings.TrimSpace(res.Stdout) != "link" {
		return box.WriteFile(ctx, live, body)
	}
	staged := paths.WorkDir() + "/home-staged"
	if err = box.WriteFile(ctx, staged, body); err != nil {
		return err
	}
	return execOK(ctx, box, "cat "+q(staged)+" > "+q(live)+" && rm -f "+q(staged), "writing "+live+" through its link")
}
