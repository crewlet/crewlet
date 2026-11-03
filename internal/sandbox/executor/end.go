package executor

import (
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A RUN ENDS ON REQUEST, and packs its session as its own end does.
//
// A stopped or lost job's session is taken out of its box before the box goes,
// and taken from a job still working it is packed as it stands: a step in
// flight half recorded, a harness that may append to the very file being
// packed. So the read that takes it first asks the job to END
// (internal/sandbox's endjob.go): SIGUSR1 to the run's script, ONE process,
// by the pid the script recorded in its box ([Paths.JobScript]). The script
// ends its harness — SIGTERM, then SIGKILL after the grace — packs the session
// by the very code path its own exit takes, records that it was ended and how
// ([Paths.JobEnded]), and exits with its harness's own status; the wrapper then
// writes the done marker, as on every exit, and the job is over.
//
// SIGUSR1, NOT SIGTERM. SIGTERM is a teardown's: it reaches every process of
// the job at once, and an OpenCode run's script answers it by exporting its
// login beside a server that is going away ([OpenCode.Command]). An end needs
// the server alive to export the session through, so it is a signal of its own
// that only the engine sends and only to the script, which the harness never
// receives: a trap is reset by exec, so nothing the script starts inherits it.
//
// THE PID IS PUBLISHED ONLY ONCE THE TRAP IS SET ([endPrologue]), so a request
// sent through the file always meets the script's handler and never kills it by
// the signal's default. A script of an older build records no pid, and is
// never asked: the read kills its job instead.
//
// THE HARNESS RUNS IN THE BACKGROUND, waited for ([launchHarness]). A shell
// defers a trap until the foreground command it runs returns, and that command
// is the harness the trap has to stop; `wait` returns at once, above 128, when a
// trapped signal arrives, and a second `wait` then answers the harness's real
// status (measured under dash and bash --posix, end-job-scratch/exp/waits.sh).
//
// AN END UNDER WAY IGNORES A SECOND REQUEST: the handler's first act is to
// ignore SIGUSR1, so a second can interrupt neither the wait for the harness
// nor the pack — which moves its archive into place whole from a staging
// directory, so a pack killed part way leaves none where it is read.

// jobScriptProtocol is the version of the end protocol a run's script records
// beside its pid ([Paths.JobScript]): a reader that does not know the version
// asks nothing of the script.
const jobScriptProtocol = 1

// endFunction defines the script's `end PID [again]`: SIGTERM to PID, up to
// stop for it to go, then SIGKILL — answering 1 where it had to kill, and 0
// where PID went in time or was no process. Every process a run's script stops
// goes through it: its harness, as the end of a run asks ([launchHarness]); and
// an OpenCode run's server and login exporter ([openCodeStops]).
//
// NO PROCESS IS NOTHING TO STOP, and it is TESTED rather than left to `kill`
// refusing an empty operand: dash and bash refuse it, but busybox ash — the sh
// of an Alpine image, which OpenCode 2's musl builds are for — answers both
// `kill ""` and `kill -0 ""` with success and signals nothing, so an end with no
// process waited out the whole bound.
//
// A PROCESS TOLD AGAIN until it goes, where the second operand asks for it,
// because the first SIGTERM can be lost on a process forked while its parent
// trapped SIGTERM: see [openCodeStops]' exporter. A repeat goes only to a pid
// `kill -0` still finds, as every probe here does, so it shares the one
// residual: a pid the script reaped during the tenth of a second before the
// probe, which the system could hand out again only by wrapping its whole pid
// space within that tenth.
func endFunction(stop time.Duration) string {
	return fmt.Sprintf(`end() { [ -n "$1" ] || return 0; kill "$1" 2> /dev/null || return 0; i=0; while kill -0 "$1" 2> /dev/null && [ "$i" -lt %d ]; do sleep 0.1; i=$((i+1)); [ -z "$2" ] || { kill -0 "$1" 2> /dev/null && kill "$1" 2> /dev/null; }; done; if kill -0 "$1" 2> /dev/null; then kill -9 "$1" 2> /dev/null; return 1; fi; return 0; }
`, int(stop/(100*time.Millisecond)))
}

// endPrologue opens every run's script, before it does anything else: the
// variables the end protocol keeps — endasked (a request came), endhalted (the
// end of the harness is under way), harness (the harness's pid while it runs)
// and endedas (how the job was ended, for [Paths.JobEnded]) — the `end` it
// stops processes with ([endFunction]), and the handler the request runs,
// which records the request and, while the harness runs, ends it (the harness's
// own `endharness`, each script's); then the handler set; and only then the
// script's pid published in the box ([Paths.JobScript]), as `1 <pid>`, written
// beside and moved into place whole, so a reader finds the record or none.
//
// Every name is the protocol's own, apart from the one-letter variables the
// scripts already keep (s, k, w, h, d, a, b, c, i, z, v).
func endPrologue(paths Paths, stop time.Duration) string {
	next := sandbox.ShellQuote(paths.JobScript() + ".next")
	return `endasked= endhalted= harness= endedas=
` + endFunction(stop) + `endask() { endasked=1; [ -z "$harness" ] || endharness; }
trap endask USR1
` + fmt.Sprintf(`printf '%d %%s\n' "$$" > %s && mv -f %s %s
`, jobScriptProtocol, next, next, sandbox.ShellQuote(paths.JobScript()))
}

// endHarness is the `endharness` a script's handler runs to end its harness
// while it runs: SIGUSR1 ignored from here on, so a second request interrupts
// nothing of the end; the end recorded as asked; first, where a script has one
// (OpenCode's interrupt of its session through its own server), the step that
// settles what the harness is doing; then the harness ended within the grace,
// and recorded as killed where it had to be.
func endHarness(first string) string {
	if first != "" {
		first += "; "
	}
	return `endharness() { trap '' USR1; endhalted=1 endedas=asked; ` + first + `end "$harness" || endedas=killed; }
`
}

// launchHarness runs harness — the harness's command line — under the end
// protocol, and leaves its exit status in code ([endPrologue]):
//
//   - a request that came before it starts starts nothing: code is 143, as a
//     harness SIGTERM ended reports, and the job is recorded as ended before
//     its harness started;
//   - otherwise the harness runs in the background, its stdin the script's
//     own — the brief the wrapper, or the script's choice of a fresh brief,
//     points it at — handed it as fd 3, since an asynchronous command's stdin
//     is /dev/null unless it is redirected explicitly, and fd 0 by then is that
//     /dev/null;
//   - a request that came while it started ends it at once;
//   - its status is waited for, and — where a request ended it, which
//     interrupts the wait — waited for again, which answers its real status:
//     dash answers a job it already reported again, bash 127 for one it
//     already collected, and the first answer stands then. (Under bash a
//     harness whose real status is 127, an exec failure, and whose wait a
//     request interrupted in the same instant is reported 138.)
//
// AND THE END IS RECORDED AS SOON AS THE HARNESS IS OVER ([Paths.JobEnded]),
// before the pack, so it stands even where the pack is killed.
func launchHarness(harness string, paths Paths) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line(`if [ -n "$endasked" ]; then code=143 endedas=before`)
	line(`else`)
	line(`exec 3<&0`)
	line(`%s <&3 3<&- &`, harness)
	line(`harness=$!`)
	line(`exec 3<&-`)
	line(`[ -z "$endasked" ] || endharness`)
	line(`wait "$harness"; code=$?`)
	line(`if [ -n "$endhalted" ]; then wait "$harness" 2> /dev/null; again=$?; [ "$again" = 127 ] || code=$again; fi`)
	line(`harness=`)
	line(`fi`)
	b.WriteString(recordEnded(paths))
	return b.String()
}

// recordEnded is the line that records how the job was ended
// ([Paths.JobEnded]), where it was: written beside and moved into place whole.
func recordEnded(paths Paths) string {
	next := sandbox.ShellQuote(paths.JobEnded() + ".next")
	return fmt.Sprintf(`[ -z "$endedas" ] || { printf '%%s\n' "$endedas" > %s && mv -f %s %s; }
`, next, next, sandbox.ShellQuote(paths.JobEnded()))
}
