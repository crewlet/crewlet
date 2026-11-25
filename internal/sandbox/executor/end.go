package executor

import (
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/browser/boxhelper"
	"github.com/crewlet/crewlet/internal/shellword"
)

// A RUN ENDS ON REQUEST (package doc, "A run ends on request"), because a
// session packed from a job still working holds a half-recorded step, in files
// its harness may still append to.
//
// SIGUSR1, NOT SIGTERM. SIGTERM is a teardown's: it reaches every process of
// the job at once, and an OpenCode script answers it by exporting its login
// beside a server going away ([OpenCode.Command]). An end needs that server
// alive, so its signal goes only to the script (internal/sandbox's endjob.go),
// and exec resets the trap, so the harness never inherits it. A script of an
// older build records no pid and is never asked: the read kills its job.

// jobScriptProtocol is the version of the end protocol a run's script records
// beside its pid ([Paths.JobScript]): a reader that does not know the version
// asks nothing of the script.
const jobScriptProtocol = 1

// endFunction defines the script's `end PID [again]`: SIGTERM, up to stop for
// PID to go, then SIGKILL; 1 where it had to kill, 0 where PID went or was none.
// Every stop goes through it ([launchHarness], [openCodeStops]).
//
// An empty PID is tested, not left to `kill`: busybox ash, Alpine's sh, answers
// `kill ""` and `kill -0 ""` with success, so an end with no process would wait
// out the whole bound.
//
// `again` re-sends SIGTERM each probe while `kill -0` still finds PID, since the
// first can be lost on a process forked while its parent trapped SIGTERM
// ([openCodeStops]' exporter); a pid reaped and reused within one 0.1 s probe
// is the one residual.
func endFunction(stop time.Duration) string {
	return fmt.Sprintf(`end() { [ -n "$1" ] || return 0; kill "$1" 2> /dev/null || return 0; i=0; while kill -0 "$1" 2> /dev/null && [ "$i" -lt %d ]; do sleep 0.1; i=$((i+1)); [ -z "$2" ] || { kill -0 "$1" 2> /dev/null && kill "$1" 2> /dev/null; }; done; if kill -0 "$1" 2> /dev/null; then kill -9 "$1" 2> /dev/null; return 1; fi; return 0; }
`, int(stop/(100*time.Millisecond)))
}

// endPrologue opens every run's script: the protocol's variables — endasked (a
// request came), endhalted (the harness's end is under way), harness (its pid
// while it runs), endedas (how the job ended, for [Paths.JobEnded]) — and its
// browser relay's ([relayPrologue]), then [endFunction], the handler endask
// (records the request and, while the harness runs, calls the script's
// `endharness`), the trap, and only then the pid published as `1 <pid>`
// ([Paths.JobScript]), written beside and moved into place whole. Published
// after the trap, a request sent through the file always meets the handler,
// never SIGUSR1's default, which kills.
//
// Every name is the protocol's own, apart from the one-letter variables the
// scripts already keep (s, k, w, h, d, a, b, c, i, z, v).
func endPrologue(paths Paths, stop time.Duration) string {
	next := shellword.Quote(paths.JobScript() + ".next")
	return "endasked= endhalted= harness= endedas=\n" + relayPrologue() + endFunction(stop) +
		`endask() { endasked=1; [ -z "$harness" ] || endharness; }
trap endask USR1
` + fmt.Sprintf(`printf '%d %%s\n' "$$" > %s && mv -f %s %s
`, jobScriptProtocol, next, next, shellword.Quote(paths.JobScript()))
}

// endHarness is the `endharness` a script's handler runs to end its harness
// while it runs: SIGUSR1 ignored from here on, so a second request interrupts
// neither the wait nor the pack; the end recorded as asked; before anything
// else, the job's browser relay, while it is still the relay, sent SIGTERM,
// so it goes within its own bound while the harness ends, never after it
// ([relayStep]); then, where a script has one (OpenCode's interrupt through
// its own server, Hermes's gateway stop), the step that settles what the
// harness is doing; then the harness ended within the grace, and recorded as
// killed where it had to be.
func endHarness(first string) string {
	if first != "" {
		first += "; "
	}
	return `endharness() { trap '' USR1; endhalted=1 endedas=asked; relaying && kill "$relay" 2> /dev/null; ` +
		first + `end "$harness" || endedas=killed; }
`
}

// launchHarness runs harness (its command line) under the end protocol, leaving
// its exit status in code. A request before the launch starts nothing: code
// 143, as SIGTERM leaves, recorded as `before`. Otherwise the harness runs in
// the BACKGROUND, since a shell defers a trap until its foreground command
// returns, and is ended at once if a request came while it started. Its stdin,
// the script's own, is handed over as fd 3: an asynchronous command's is
// /dev/null unless redirected. A trapped signal makes `wait` return at once,
// above 128, so after an end it waits again for the real status (dash repeats
// it; bash answers 127 and the first stands, so bash reports 138 for a real 127
// interrupted in the same instant). The end is recorded ([recordEnded]) once
// the harness is over, before the pack, so it stands even if the pack is killed.
func launchHarness(harness string, paths Paths) string { return launchWatched(harness, "", paths) }

// launchWatched is [launchHarness] with watch, a command of the script's own,
// run once the harness has started and before it is waited for: a check of
// what the harness reported as it started, which may end it ([claudeGuard]).
// It runs in the script itself, so an end request still meets the handler.
// The job's browser relay starts just before the harness and is ended once
// the harness is over ([relayStep]); a request before the launch starts none.
func launchWatched(harness, watch string, paths Paths) string {
	start, stop := relayStep(paths)
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line(`if [ -n "$endasked" ]; then code=143 endedas=before`)
	line(`else`)
	b.WriteString(start)
	line(`exec 3<&0`)
	line(`%s <&3 3<&- &`, harness)
	line(`harness=$!`)
	line(`exec 3<&-`)
	line(`[ -z "$endasked" ] || endharness`)
	if watch != "" {
		line(`%s`, watch)
	}
	line(`wait "$harness"; code=$?`)
	line(`if [ -n "$endhalted" ]; then wait "$harness" 2> /dev/null; again=$?; [ "$again" = 127 ] || code=$again; fi`)
	line(`harness=`)
	b.WriteString(stop)
	line(`fi`)
	b.WriteString(recordEnded(paths))
	return b.String()
}

// relayStep is the job's browser relay beside its harness, start and stop
// (internal/browser/screen's package doc). start runs it in the background on
// the wiring the run's start wrote ([Paths.BrowserRelay]); a continuing run's
// on the one for the session its gate chose by k, which every gate sets, the
// other removed first, and none where that one is missing. The subshell execs
// node, so $relay is the relay's own pid, kept with its start time, and it
// holds neither the brief's stdin nor fd 3. stop ends it at the script's grace
// once the harness is over, while it is still the relay ([relayPrologue]): its
// own bound is under the grace, and an end request has TERMed it already
// ([endHarness]). A relay whose script went any other way leaves by itself.
func relayStep(paths Paths) (start, stop string) {
	q := shellword.Quote
	run := func(wiring string) string {
		return "( " + boxhelper.RelayScript(paths.Home, wiring) + " ) < /dev/null >> " + q(paths.BrowserRelayLog()) +
			" 2>&1 & relay=$!; relayat=$(relayborn)"
	}
	one, continued, fresh := paths.BrowserRelay(), paths.BrowserRelayContinued(), paths.BrowserRelayFresh()
	start = fmt.Sprintf(`if [ -s %[1]s ]; then rm -f %[2]s %[3]s; %[4]s
elif [ "$k" = yes ]; then rm -f %[3]s; [ ! -s %[2]s ] || { %[5]s; }
else rm -f %[2]s; [ ! -s %[3]s ] || { %[6]s; }
fi
`, q(one), q(continued), q(fresh), run(one), run(continued), run(fresh))
	return start, "relaying && end \"$relay\"; relay= relayat=\n"
}

// relayPrologue opens a script that starts its job's browser relay
// ([relayStep]): relay, the relay's pid while it runs, and relayat, the
// start time the kernel gave that pid, in clock ticks; relayborn, the start
// time it reports for $relay now; and relaying, whether $relay is still the
// relay, clearing both where it is not. A relay that left on its own is
// reaped at the script's next wait, and its pid is then any new process's to
// take, long after the relay's tick, so the script signals it only while
// relaying says so, as hermes.go's endgw signals its gateway. /proc answers
// it: a relay runs only beside a box's browser, in a container box, whose
// kernel is Linux.
func relayPrologue() string {
	return `relay= relayat=
relayborn() { { read -r relaystat < /proc/"$relay"/stat; } 2> /dev/null || return 1; set -- ${relaystat##*) }; printf '%s\n' "${20}"; }
relaying() { [ -n "$relay" ] && [ -n "$relayat" ] && [ "$(relayborn)" = "$relayat" ] || { relay= relayat=; return 1; }; }
`
}

// recordEnded is the line that records how the job was ended
// ([Paths.JobEnded]), where it was: written beside and moved into place whole.
func recordEnded(paths Paths) string {
	next := shellword.Quote(paths.JobEnded() + ".next")
	return fmt.Sprintf(`[ -z "$endedas" ] || { printf '%%s\n' "$endedas" > %s && mv -f %s %s; }
`, next, next, shellword.Quote(paths.JobEnded()))
}
