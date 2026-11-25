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

// heldLink is a home file held as the link that stood in its place leading
// nowhere, moved aside whole for the run: written through, it would create
// the file it names, which putting back could not tell from the home's own.
// Putting it back moves the link back over the run's file, as it was.
const heldLink = ".link"

// heldPart is a home file's copy while it is made, beside held and moved
// over it whole: a copy cut short (a full disk, a SIGKILL mid-cp) is never
// held, so a put-back never writes part of the person's file over the whole.
// Every put-back removes one left behind.
const heldPart = ".tmp"

// holdHomeShell is the shell holding live, a file of the home's, at held before
// a run writes its own in its place, printing `h` and the home's text, or `n`
// where the home has none: a whole copy where it has the file ([heldPart]),
// the link itself ([heldLink]) where a link leading nowhere stands there, the
// marker held+[heldNone] where it has none. Where one is held already, an
// earlier run's script ended before it put the home's back, so what is held
// is the home's own and the live file an earlier run's.
func holdHomeShell(live, held string) string {
	q := shellword.Quote
	h, none, l, link, part := q(held), q(held+heldNone), q(live), q(held+heldLink), q(held+heldPart)
	return fmt.Sprintf(`set -e; if [ -e %[1]s ]; then printf h; cat %[1]s; elif [ -e %[2]s ] || [ -L %[4]s ]; then printf n; `+
		`elif [ -e %[3]s ]; then cp %[3]s %[5]s; mv -f %[5]s %[1]s; printf h; cat %[1]s; `+
		`elif [ -L %[3]s ]; then mv -f %[3]s %[4]s; printf n; else : > %[2]s; printf n; fi`, h, none, l, link, part)
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
// held copy written over the run's, through a link as the home keeps it, the
// held link moved back over it ([heldLink]), or the run's removed where the
// home had none, a copy cut short while it was held ([heldPart]) removed
// first. One command group, so its status is the put-back's wherever it is
// chained. Nothing where none is held, so it runs any number of times;
// what will not go back, the run's file that will not go included, says which
// file on the error stream and fails, leaving it held (the none marker too)
// for the next try.
func putBackHome(live, held string) string { return putBackHomeAt(shellword.Quote(live), held) }

// putBackHomeAt is [putBackHome] for the live file the shell word live names,
// which may be a variable's expansion.
func putBackHomeAt(live, held string) string {
	q := shellword.Quote
	h, none, l, link, part := q(held), q(held+heldNone), live, q(held+heldLink), q(held+heldPart)
	return fmt.Sprintf(`{ rm -f %[5]s; if [ -L %[4]s ]; then { rm -f %[3]s && mv -f %[4]s %[3]s; } 2> /dev/null || `+
		`{ echo "crewlet: "%[3]s" was not put back from "%[4]s >&2; false; }; `+
		`elif [ -e %[2]s ]; then { { rm -f %[3]s; } 2> /dev/null && rm -f %[2]s; } || `+
		`{ echo "crewlet: "%[3]s" was not put back" >&2; false; }; elif [ -e %[1]s ]; then `+
		`{ { cat %[1]s > %[3]s; } 2> /dev/null && rm -f %[1]s; } || `+
		`{ echo "crewlet: "%[3]s" was not put back from "%[1]s >&2; false; }; fi; }`, h, none, l, link, part)
}

// everyStep is the shell running each of steps whatever came of the ones
// before, failing where any failed, the first failure's words first on the
// error stream: a put-back that will not go back is reported and never keeps
// a later, independent step from running. A subshell, so a step's own
// variables stay its own.
func everyStep(steps ...string) string {
	var b strings.Builder
	b.WriteString("( r=0")
	for _, step := range steps {
		b.WriteString("; { " + step + "; } || r=1")
	}
	b.WriteString(`; [ "$r" = 0 ] )`)
	return b.String()
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
