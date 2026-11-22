package executor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/sandbox"
)

func init() { Register(Codex{}) }

// CodexName is this runner's config name.
const CodexName = "codex"

// Codex drives OpenAI's Codex CLI headless: one `codex app-server` per run on
// a unix socket of the run's own, holding all of the run's configuration, and
// `codex exec --remote` against it with the brief ([Codex.Command]). Through
// the server and not `exec` alone, because the server is what takes a note
// into the running turn (`turn/steer`), which the note helper the script
// starts beside it sends ([Paths.CodexNoteHelper]). Codex mints its own
// session id ([sessionMinter]), which the script reports as `exec --json`
// prints it. docs/concepts/code-sandbox.md, "Codex", holds the measurements.
type Codex struct{}

var _ Harness = Codex{}

// NewCodex returns the runner.
func NewCodex() *Runner { return New(Codex{}) }

// Name is the harness's key in the runner registry.
func (Codex) Name() string { return CodexName }

// mintsSession reports that Codex mints its session's id as it starts: the
// thread id `exec --json` prints first (`thread.started`).
func (Codex) mintsSession() bool { return true }

// HandsSkills reports that a Codex run takes skills of its own, listed from
// [Paths.CodexSkills].
func (Codex) HandsSkills() bool { return true }

// Session is the session a Codex run works in ([Harness.Session]): none named
// for a new one, which Codex mints, or the one it continues, by the id Codex
// gave it. A continued run whose box can take up neither its copy nor a
// carried one starts a session of its own, again minted by Codex, so it names
// no Fresh id.
func (Codex) Session(req sandbox.RunRequest) Session {
	if req.Resume != "" {
		return Session{ID: req.Resume, Continues: true, Replaces: req.Carried != nil && req.Carried.Replace}
	}
	return Session{}
}

// codexFloor is the oldest Codex release the runner drives, its built-in
// profile's executor floor (harnessprofile.Executor.Floor), which the doctor
// reads too: the release every figure the runner rests on was measured on.
func codexFloor() string {
	runner, _ := harnessprofile.ExecutorOf(CodexName)
	return runner.Floor
}

// codexListenWait bounds the wait for a run's app-server to listen on its
// socket: SIXTY SECONDS, OpenCode's bound for its own server on the same busy
// box ([openCodeListenWait]). Codex's static binary listened at once when
// measured; the bound is for one that never does.
const codexListenWait = 60 * time.Second

// codexTimes is the waits a run's script runs at: a value, so a case about a
// wait runs it at a length it can tell apart ([openCodeTimes]).
type codexTimes struct {
	listen, stop time.Duration
	clock        string
}

// codexRunTimes is the waits every run's script runs at.
func codexRunTimes() codexTimes {
	return codexTimes{listen: codexListenWait, stop: sandbox.JobStopGrace, clock: openCodeClock}
}

// Command is the run's script ([codexScript]), run under sh as every runner's
// is, so the wrapper's redirections reach every step.
func (Codex) Command(req sandbox.RunRequest, session Session, paths Paths, _ string) string {
	return "sh -c " + sandbox.ShellQuote(codexScript(req, session, paths, codexRunTimes()))
}

// codexRelease reads the box's `codex --version` once, as v, its numbers as
// n, and defines [claudeRelease]'s `at`. Codex prints `codex-cli X.Y.Z`, so the
// numbers are the version's last run of digits and dots; anything else is taken
// for a current release, as Claude Code's unreadable version is.
const codexRelease = `v=$(codex --version < /dev/null 2> /dev/null); n=${v##*[!0-9.]}
a=${n%%.*}; r=${n#*.}; b=${r%%.*}; c=${r#*.}; c=${c%%.*}
case "$n" in [0-9]*.[0-9]*.[0-9]*) ;; *) a= ;; esac
at() { [ -z "$a" ] || [ "$a" -gt "$1" ] || { [ "$a" -eq "$1" ] && { [ "$b" -gt "$2" ] || { [ "$b" -eq "$2" ] && [ "$c" -ge "$3" ]; }; }; }; }
`

// codexScript is a Codex run's script ([Codex.Command]): the end protocol
// (end.go); the release, refused below [codexFloor]; the session settled and
// its configuration installed ([codexSessionGate], [codexFreshRoute]), and
// recorded; the app-server started on [Paths.CodexSocket], its
// output kept off the stream; the note helper beside it where the run is wired
// to its notes; a watcher writing the thread `exec --json` reports to
// [Paths.CodexThread] and reporting it ([reportSession]); `exec --remote`
// under the end protocol ([launchHarness]); the helper and server stopped;
// then the rollout lines the run appended, of the kinds the result reads
// ([codexRolloutEvent]), and the session packed ([codexCapture]). Codex is the
// script's child, never PID 1 (a container's is its `--init`): no reaper.
func codexScript(req sandbox.RunRequest, session Session, paths Paths, times codexTimes) string {
	q := sandbox.ShellQuote
	stream := q(paths.Result())
	sock := q(paths.CodexSocket())
	thread := q(paths.CodexThread())
	threadNext := q(paths.CodexThread() + ".next")
	serverLog := q(paths.WorkDir() + "/codex-server.log")
	steered := req.Steer.Set()

	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	b.WriteString(endPrologue(paths, times.stop))
	b.WriteString(endHarness(""))
	line(`command -v codex > /dev/null || { echo 'crewlet: no codex on the box PATH' >&2; exit 127; }`)
	b.WriteString(codexRelease)
	line(`at %s || { echo "crewlet: this box runs Codex $v; the codex runner drives %s or later — install it in the box image" >&2; exit 1; }`,
		floorArgs(codexFloor()), codexFloor())
	line(`mkdir -p %s %s && chmod 700 %s || exit 1`, q(paths.CodexHome()), q(paths.CodexSessions()),
		q(paths.CodexHome()))
	line(`apid= hpid= tpid= cpid= tid= before=0 r=`)
	// stop: the watcher, the helper, then the app-server, TERM again on each
	// probe: on TERM it finishes the running turn first, and a second TERM
	// ends it at once (measured on 0.171.0), all within the job's grace.
	line(`stop() { end "$tpid"; end "$cpid"; end "$hpid"; end "$apid" again; tpid= cpid= hpid= apid=; rm -f %s; }`, sock)
	// THE THREAD, off the stream's first line, the one `exec --json` prints as
	// the server starts the thread, before its turn: written for the helper
	// and reported for collection. Whole lines only: the pattern ends the line.
	line(`thread() { t=$(head -n 1 %s 2> /dev/null | sed -n 's/^{"type": *"thread.started", *"thread_id": *"\([0-9A-Za-z-]*\)"}$/\1/p'); [ -n "$t" ] || return 1; tid=$t; printf '%%s\n' "$tid" > %s && mv -f %s %s && %s; }`,
		stream, threadNext, threadNext, thread, reportSession(paths, "tid"))
	b.WriteString(codexFinish(paths))
	// quit: a teardown signal's end, as OpenCode's ([openCodeScript]): exec
	// first, then everything beside it, then the session packed.
	line(`quit() { trap '' USR1; trap - EXIT TERM INT HUP; end "$harness"; harness=; end "$tpid"; tpid=; thread || :; stop; finish; exit "$1"; }`)
	line(`trap 'stop' EXIT`)
	line(`trap 'quit 143' TERM; trap 'quit 130' INT; trap 'quit 129' HUP`)
	b.WriteString(codexSessionGate(session, paths, req.FreshWiring != nil))
	if session.Continues && req.FreshWiring != nil && req.Steer.Set() {
		b.WriteString(codexFreshRoute(paths))
	}
	line(`chmod 600 %s || exit 1`, q(paths.CodexLiveConfig()))
	// THE RECORD, before the server and the run: the session continued, else
	// none, since Codex has not minted the new one yet ([sessionMinter]).
	line(`printf 'id %%s\ncontinued %%s\nwhy %%s\nrelease %%s\n' "$sid" "$k" "$w" "$v" > %s`, q(paths.SessionRecord()))
	line(`endbefore() { endedas=before; %s; exit 143; }`, strings.TrimSuffix(recordEnded(paths), "\n"))
	line(`[ -z "$endasked" ] || endbefore`)
	// WHERE THE RUN BEGAN IN THE ROLLOUT IT CONTINUES: every line past it is
	// the run's own, which is all the result reads.
	if session.Continues {
		line(`if [ "$k" = yes ]; then r=$(%s); [ -z "$r" ] || { lc=$(wc -l < "$r" | tr -d " ") && [ -n "$lc" ] && before=$lc; } || before=0; fi`,
			codexRolloutPath(paths, session.ID))
	}

	line(`rm -f %s %s %s`, sock, thread, threadNext)
	// No --ignore-user-config: app-server has none, and the config.toml it
	// always loads is the run's own ([Paths.CodexLiveConfig]).
	line(`codex app-server --listen %s %s < /dev/null > %s 2>&1 &`,
		q("unix://"+paths.CodexSocket()), strings.Join(codexPosture(req), " "), serverLog)
	line(`apid=$!`)
	line(`deadline=$(( $(%s) + %d ))`, times.clock, secs(times.listen))
	line(`until [ -S %s ]; do`, sock)
	line(`  [ -z "$endasked" ] || endbefore`)
	line(`  if ! kill -0 "$apid" 2> /dev/null; then echo 'crewlet: codex app-server exited before it listened:' >&2; cat %s >&2; exit 1; fi`,
		serverLog)
	line(`  if [ "$(%s)" -ge "$deadline" ]; then echo 'crewlet: codex app-server did not listen on its socket within %s' >&2; exit 1; fi`,
		times.clock, times.listen)
	line(`  sleep 0.1`)
	line(`done`)

	line(`( while kill -0 $$ 2> /dev/null; do thread && exit 0; sleep 0.1; done ) &`)
	line(`tpid=$!`)

	if steered {
		// THE ROUTE IN THE HELPER'S ENVIRONMENT ALONE, never argv nor
		// Codex's: it carries the job's token. Its errors are the run's.
		line(`chmod 700 %s && %s=$(cat %s) %s -adapter codex-app-server -socket %s -thread-file %s -notes-env %s < /dev/null > /dev/null &`,
			q(paths.CodexNoteHelper()), codexNotesEnv, q(paths.CodexNotesURL()), q(paths.CodexNoteHelper()), sock, thread,
			codexNotesEnv)
		line(`hpid=$!`)
	}

	if session.Continues {
		line(`set --; [ "$k" != yes ] || set -- resume "$sid"`)
	} else {
		line(`set --`)
	}
	// THE BRIEF ON STDIN (`-`), as `exec --remote` reads it (measured on 0.171.0).
	harness := fmt.Sprintf(`codex exec --json --remote %s "$@" -`, q("unix://"+paths.CodexSocket()))
	if limit := req.Limits.MaxTurns; limit > 0 {
		harness = codexRoundCap(paths, harness)
		b.WriteString(codexCapWatch(paths, limit))
	}
	b.WriteString(launchHarness(harness, paths))
	if req.Limits.MaxTurns > 0 {
		line(`if [ -e %s ]; then printf '{"type":"%s","rounds":%d}\n'; code=1; fi`,
			q(paths.CodexCapped()), codexCappedEvent, req.Limits.MaxTurns)
	}
	// The watcher stopped before the last read, so no two writers race on the
	// report's staging file.
	line(`end "$tpid"; tpid=; thread || :`)
	line(`stop`)
	line(`finish`)
	line(`trap - EXIT TERM INT HUP`)
	line(`[ "$code" = 0 ] || [ ! -s %s ] || { echo 'crewlet: codex app-server said:' >&2; tail -c %d %s >&2; }`,
		serverLog, codexServerLogShown, serverLog)
	line(`exit "$code"`)
	return b.String()
}

// codexNotesEnv is the variable the note helper reads the notes route from,
// set in its environment alone (internal/notehelper's -notes-env).
const codexNotesEnv = "CREWLET_NOTES_URL"

// codexServerLogShown is how much of the app-server's own output, from its
// end, a failed run's error stream is given: 4 KiB, a few of its lines, since
// the run's failure is read from that stream's end within
// [sandbox.MaxFailureBytes] beside exec's own words.
const codexServerLogShown = 4 << 10

// codexFreshRoute hands the note helper of a continued run that started a
// session of its own (k=no) the notes route under the job's other token, as
// the gate hands Codex that token's configuration ([codexSessionGate]), and
// removes the other route otherwise: the one place a run's notes route is
// chosen.
func codexFreshRoute(paths Paths) string {
	q := sandbox.ShellQuote
	url, fresh := q(paths.CodexNotesURL()), q(paths.CodexNotesURLFresh())
	return fmt.Sprintf("if [ \"$k\" = no ]; then mv -f %s %s || { echo 'crewlet: the new session could not be wired to its notes, so the run does not start' >&2; exit 1; }; else rm -f %s; fi\n",
		fresh, url, fresh)
}

// codexCappedEvent is the line a run's script adds to its stream where it
// ended its run at the round cap ([codexCapWatch]), read as
// [sandbox.Result.RoundCapped].
const codexCappedEvent = "crewlet.round_cap"

// codexCapPoll is how often a capped run's watcher counts its rounds: half a
// second, a few of a model's shortest responses, so a run overshoots its cap
// by the round already in flight at most.
const codexCapPoll = "0.5"

// codexRoundCap is harness run so its pid is recorded ([Paths.CodexExecPID]),
// the shell exec'ing Codex in its own place, so the recorded pid is Codex's,
// and the one the end protocol holds too. The file is an argument, never
// spliced into the quoted command.
func codexRoundCap(paths Paths, harness string) string {
	return `sh -c 'printf "%s\n" "$$" > "$1" && shift && exec "$@"' codex-exec ` +
		sandbox.ShellQuote(paths.CodexExecPID()) + " " + harness
}

// codexCapWatch starts the watcher that holds a run to its round cap,
// role.sandbox.max_turns: Codex has no setting for one (0.171.0), so the
// watcher counts the rounds the run's rollout records past where it began
// (one token_usage_record a model response), and at limit marks the run
// capped and ends Codex with SIGTERM, as a run ended on request is ended.
// The script reads the mark after Codex exits.
func codexCapWatch(paths Paths, limit int) string {
	q := sandbox.ShellQuote
	return fmt.Sprintf(`( while kill -0 $$ 2> /dev/null; do t=$(cat %[1]s 2> /dev/null); f=; [ -z "$t" ] || f=$(find %[2]s -type f -name "rollout-*-$t.jsonl" 2> /dev/null | LC_ALL=C sort | head -n 1); if [ -n "$f" ] && [ "$(tail -n +$((before + 1)) "$f" | grep -c '"type": *"token_usage_record"')" -ge %[3]d ] && [ -s %[4]s ]; then : > %[5]s; kill -TERM "$(cat %[4]s)" 2> /dev/null; exit 0; fi; sleep %[6]s; done ) &
cpid=$!
`, q(paths.CodexThread()), q(paths.CodexSessions()), limit, q(paths.CodexExecPID()), q(paths.CodexCapped()),
		codexCapPoll)
}

// codexFinish defines the script's `finish`, run once Codex and everything
// beside it have stopped, on the run's own end and a teardown's alike: the
// session is the thread Codex reported, else the one it continued, as
// collection reads it ([Runner.ReportedSession]); the rollout lines the run
// appended of the kinds the result reads go to the stream
// ([codexRolloutEvent]); and the session is packed ([codexCapture]).
func codexFinish(paths Paths) string {
	var b strings.Builder
	b.WriteString("finish() {\n")
	b.WriteString(`[ -z "$tid" ] || sid=$tid` + "\n")
	fmt.Fprintf(&b, `r=; [ -z "$sid" ] || r=$(find %s -type f -name "rollout-*-$sid.jsonl" 2> /dev/null | LC_ALL=C sort | head -n 1)`+"\n",
		sandbox.ShellQuote(paths.CodexSessions()))
	fmt.Fprintf(&b, `[ -z "$r" ] || tail -n +$((before + 1)) "$r" | grep -E '"type": *"(turn_context|token_usage_record|token_count)"' | sed 's/^/{"type":"%s","line":/; s/$/}/'`+"\n",
		codexRolloutEvent)
	b.WriteString(codexCapture(paths))
	b.WriteString("}\n")
	return b.String()
}

// Output is where a Codex run's stream lands, which is its result: its
// stdout, `exec --json`, with the rollout lines its script adds after it
// ([codexEvents]).
func (Codex) Output(paths Paths) Output {
	return Output{Stdout: paths.Result(), Events: true, BriefOnStdin: true}
}

// Pins is CODEX_HOME, the box's own [Paths.CodexHome], where the run's
// configuration, login, sessions and skills are, whatever a profile sets.
func (Codex) Pins(_ context.Context, _ sandbox.RunRequest, paths Paths) []Pinned {
	return []Pinned{{Name: "CODEX_HOME", Value: paths.CodexHome()}}
}

// Events is a new decoder for one stream, read whole or followed ([Decoder]).
func (Codex) Events() Decoder { return &codexEvents{} }

// Parse decodes a whole stream already in hand ([codexEvents]).
func (Codex) Parse(stdout []byte) sandbox.Result { return decodeAll(&codexEvents{}, stdout) }

// artifacts is what a Codex run leaves in its box beside the session it
// continues: the configuration with its tokens, the socket, the thread, the
// helper and its routes, the server's output.
func (Codex) artifacts(p Paths) (files, dirs []string) {
	return []string{p.CodexLiveConfig(), p.CodexConfig(), p.CodexConfigFresh(), p.CodexSocket(),
		p.CodexExecPID(), p.CodexCapped(),
		p.CodexThread(), p.CodexThread() + ".next", p.CodexNoteHelper(), p.CodexNotesURL(),
		p.CodexNotesURLFresh(), p.WorkDir() + "/codex-server.log"}, nil
}
