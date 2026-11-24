package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
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

// MCPCallCeiling is none: `tool_timeout_sec` carries the bridge's own bound
// ([codexConfigTOML]), and Codex cuts a call at it whatever progress flows. Unset,
// it cuts one at 300 seconds ("timed out awaiting tools/call after 300s");
// set to 1050, a 1040-second call was answered whole (measured on 0.171.0).
func (Codex) MCPCallCeiling() time.Duration { return 0 }

// resumesOnLaunchRoute: measured on 0.171.0, the one release supported
// ([launchRouted]): created on a config provider at stub A, resumed through
// `app-server` and `exec --remote resume` with its base_url at stub B; the
// resumed request (40767 bytes, the first run's prompt in it) reached B. A
// resume under another provider id exits 1 with no request, which no launch
// meets: the run's config.toml is its own and names no provider
// ([codexConfigTOML]), so every entry runs on Codex's built-in one.
func (Codex) resumesOnLaunchRoute() bool { return true }

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
	return "sh -c " + shellword.Quote(codexScript(req, session, paths, codexRunTimes()))
}

// codexReleaseRead reads the box's `codex --version` once, as v.
const codexReleaseRead = "v=$(codex --version < /dev/null 2> /dev/null)\n"

// codexReleaseParse reads v as [releaseOf] does ([bareReleaseShell]): n is
// the bare release, "" for a pre-release or none; its numbers are a, b and c;
// and `at`, [claudeRelease]'s, is false for a release that does not read.
// Built when a script is, never at init: the hook init budget counts what
// the package allocates before main.
func codexReleaseParse() string {
	return bareReleaseShell("n") + `a=${n%%.*}; r=${n#*.}; b=${r%%.*}; c=${r##*.}
at() { [ -n "$n" ] && { [ "$a" -gt "$1" ] || { [ "$a" -eq "$1" ] && { [ "$b" -gt "$2" ] || { [ "$b" -eq "$2" ] && [ "$c" -ge "$3" ]; }; }; }; }; }
`
}

// codexReleaseRefusal ends the run before anything starts on a box whose
// Codex names no release ([codexReleaseParse]) or one below [codexFloor],
// saying which it printed and which it needs.
func codexReleaseRefusal() string {
	floor := codexFloor()
	return fmt.Sprintf(`[ -n "$n" ] || { echo "crewlet: this box's codex --version printed '$v', which names no release the codex runner reads (a bare MAJOR.MINOR.PATCH, never a pre-release); it drives %[1]s or later — install a release of it in the box image" >&2; exit 1; }
at %[2]s || { echo "crewlet: this box runs Codex $v; the codex runner drives %[1]s or later — install it in the box image" >&2; exit 1; }
`, floor, floorArgs(floor))
}

// codexScript is a Codex run's script ([Codex.Command]): the end protocol
// (end.go); the release, refused below [codexFloor]; the session settled and
// its configuration installed ([codexSessionGate], [codexFreshRoutes]), and
// recorded; the app-server started on [Paths.CodexSocket], its
// output kept off the stream; the note helper beside it where the run is wired
// to its notes; a watcher writing the thread `exec --json` reports to
// [Paths.CodexThread] and reporting it ([reportSession]); `exec --remote`
// under the end protocol ([launchHarness]); the helper and server stopped;
// then the rollout lines the run appended, of the kinds the result reads
// ([codexRolloutEvent]), and the session packed ([codexCapture]). Codex is the
// script's child, never PID 1 (a container's is its `--init`): no reaper.
func codexScript(req sandbox.RunRequest, session Session, paths Paths, times codexTimes) string {
	q := shellword.Quote
	stream := q(paths.Result())
	sock := q(paths.CodexSocket())
	thread := q(paths.CodexThread())
	threadNext := q(paths.CodexThread() + ".next")
	runStart, runStartNext := q(paths.CodexRunStart()), q(paths.CodexRunStart()+".next")
	serverLog := q(paths.WorkDir() + "/codex-server.log")
	helped := codexHelped(req)

	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	b.WriteString(endPrologue(paths, times.stop))
	b.WriteString(endHarness(""))
	line(`command -v codex > /dev/null || { echo 'crewlet: no codex on the box PATH' >&2; exit 127; }`)
	b.WriteString(codexReleaseRead)
	b.WriteString(codexReleaseParse())
	b.WriteString(codexReleaseRefusal())
	line(`mkdir -p %s %s && chmod 700 %s || exit 1`, q(paths.CodexHome()), q(paths.CodexSessions()),
		q(paths.CodexHome()))
	line(`apid= hpid= tpid= gpid= tid= before=0 r=`)
	// stop: the watcher, the helper, then the app-server, TERM again on each
	// probe: on TERM it finishes the running turn first, and a second TERM
	// ends it at once (measured on 0.171.0), all within the job's grace.
	// AND THE HOME'S OWN CONFIGURATION BACK once the server is gone ([codexRestoreHome]).
	line(`stop() { end "$tpid"; end "$gpid"; end "$hpid"; end "$apid" again; tpid= gpid= hpid= apid=; rm -f %s; %s; }`,
		sock, codexRestoreHome(paths))
	// THE THREAD, off the stream's first line, the one `exec --json` prints as
	// the server starts the thread, before its turn: written for the helper
	// and reported for collection, with where the run began in its rollout
	// ([Paths.CodexRunStart]). Whole lines only: the pattern ends the line.
	line(`thread() { t=$(head -n 1 %s 2> /dev/null | sed -n 's/^{"type": *"thread.started", *"thread_id": *"\([0-9A-Za-z-]*\)"}$/\1/p'); [ -n "$t" ] || return 1; tid=$t; printf '%%s %%s\n' "$tid" "$before" > %s && mv -f %s %s && printf '%%s\n' "$tid" > %s && mv -f %s %s && %s; }`,
		stream, runStartNext, runStartNext, runStart, threadNext, threadNext, thread, reportSession(paths, "tid"))
	b.WriteString(codexFinish(paths))
	b.WriteString(codexAborted(paths))
	// quit: a teardown signal's end, as OpenCode's ([openCodeScript]): exec
	// first, then everything beside it, then the session packed.
	line(`quit() { trap '' USR1; trap - EXIT TERM INT HUP; end "$harness"; harness=; end "$tpid"; tpid=; thread || :; stop; finish; exit "$1"; }`)
	line(`trap 'stop' EXIT`)
	line(`trap 'quit 143' TERM; trap 'quit 130' INT; trap 'quit 129' HUP`)
	b.WriteString(codexSessionGate(session, paths, req.FreshWiring != nil))
	if session.Continues && req.FreshWiring != nil {
		b.WriteString(codexFreshRoutes(req, paths))
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

	line(`rm -f %s %s %s %s`, sock, thread, threadNext, runStart)
	// THE REPOSITORIES' INSTRUCTIONS AND SKILLS, none of their configuration
	// ([codexRepoGate]), and the nested hook that hands a later checkout's
	// where the box holds the helper it runs ([codexNestedHookFlags]).
	b.WriteString(codexRepoGate(paths, req.ReportedRepos))
	hook, state := codexNestedHookFlags(paths)
	line(`hp=%s; hs=%s`, q(hook), q(state))
	// The config.toml it loads is the home's with the run's over it
	// ([codexConfigOver]), as a person's session loads the home's. Started in the
	// script's own directory, exec's: under --remote a relative path and an
	// item's cwd resolve against the SERVER's (CODEX-MEASURE §18).
	line(`codex app-server --listen %s %s ${hk:+-c "$hp" -c "$hs"} < /dev/null > %s 2>&1 &`,
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

	if len(crewletServers(req.MCPServers)) > 0 {
		b.WriteString(codexBridgeCheck(req, paths))
	}

	line(`( while kill -0 $$ 2> /dev/null; do thread && exit 0; sleep 0.1; done ) &`)
	line(`tpid=$!`)

	if helped {
		b.WriteString(codexStartHelper(req, paths))
	}

	if session.Continues {
		line(`set --; [ "$k" != yes ] || set -- resume "$sid"`)
	} else {
		line(`set --`)
	}
	// THE BRIEF ON STDIN (`-`), as `exec --remote` reads it (measured on 0.171.0).
	b.WriteString(launchHarness(fmt.Sprintf(`codex exec --json --remote %s "$@" -`,
		q("unix://"+paths.CodexSocket())), paths))
	if helped {
		// The guard first, so a helper stopped below is never taken for lost.
		line(`end "$gpid"; gpid=`)
		line(`if [ -e %s ]; then printf '{"type":"%s"}\n'; code=1; fi`, q(paths.CodexHelperLost()), codexHelperLostEvent)
	}
	// The watcher stopped before the last read, so no two writers race on the
	// report's staging file.
	line(`end "$tpid"; tpid=; thread || :`)
	if limit := req.Limits.MaxTurns; limit > 0 {
		// THE CAP ENDED THE RUN only where the helper marked it AND the turn
		// was aborted by an interrupt: a turn whose Nth response was its final
		// answer completed, marker or not, as Claude Code's --max-turns lets it.
		line(`if [ -s %s ] && aborted; then printf '{"type":"%s","rounds":%d}\n'; code=1; fi`,
			q(paths.CodexCapFired()), codexCappedEvent, limit)
	}
	line(`stop`)
	line(`finish`)
	line(`trap - EXIT TERM INT HUP`)
	line(`[ "$code" = 0 ] || [ ! -s %s ] || { echo 'crewlet: codex app-server said:' >&2; tail -c %d %s >&2; }`,
		serverLog, codexServerLogShown, serverLog)
	line(`exit "$code"`)
	return b.String()
}

// codexNotesEnv and codexSkillsEnv are the variables the note helper reads the
// notes and skill-load routes from, set in its environment alone
// (internal/notehelper's -notes-env, -skills-env).
const (
	codexNotesEnv  = "CREWLET_NOTES_URL"
	codexSkillsEnv = "CREWLET_SKILLS_URL"
)

// codexServerLogShown is how much of the app-server's own output, from its
// end, a failed run's error stream is given: 4 KiB, a few of its lines, since
// the run's failure is read from that stream's end within
// [sandbox.MaxFailureBytes] beside exec's own words.
const codexServerLogShown = 4 << 10

// codexFreshRoutes hands the note helper of a continued run that started a
// session of its own (k=no) the notes and skill-load routes under the job's
// other token, as the gate hands Codex that token's configuration
// ([codexSessionGate]), and removes the other routes otherwise: the one place
// a run's helper routes are chosen.
func codexFreshRoutes(req sandbox.RunRequest, paths Paths) string {
	q := shellword.Quote
	var b strings.Builder
	route := func(url, fresh string) {
		fmt.Fprintf(&b, "if [ \"$k\" = no ]; then mv -f %s %s || { echo 'crewlet: the new session could not be wired to its seat, so the run does not start' >&2; exit 1; }; else rm -f %s; fi\n",
			q(fresh), q(url), q(fresh))
	}
	if req.Steer.Set() {
		route(paths.CodexNotesURL(), paths.CodexNotesURLFresh())
	}
	if len(req.Skills) > 0 {
		route(paths.CodexSkillsURL(), paths.CodexSkillsURLFresh())
	}
	return b.String()
}

// codexCappedEvent is the line a run's script adds to its stream, exiting 1,
// where the note helper marked the run's round cap ([Paths.CodexCapFired])
// AND the rollout lines the run appended record its turn aborted as
// interrupted (the script's `aborted`): the cap is model responses per turn,
// and a turn whose last counted response was its final answer completed.
// Read as [sandbox.Result.RoundCapped] only beside that turn_aborted line
// too, so no other interrupt is taken for the cap.
const codexCappedEvent = "crewlet.round_cap"

// codexHelperLostEvent is the line a run's script adds to its stream where
// the note helper ended while Codex still worked ([codexStartHelper]): the run
// fails, never goes on past a cap or a skill guard no helper holds.
const codexHelperLostEvent = "crewlet.helper_lost"

// codexHelped reports whether req's run needs the note helper beside Codex:
// wired to its notes, held to a round cap (Codex has none of its own,
// CODEX-MEASURE §17.1), or handed skills whose loads only it reports.
func codexHelped(req sandbox.RunRequest) bool {
	return req.Steer.Set() || req.Limits.MaxTurns > 0 || len(req.Skills) > 0
}

// codexStartHelper starts the note helper (internal/notehelper) for req's
// run, and the guard that fails the run closed where the helper ends while
// Codex still works: the guard marks it lost ([Paths.CodexHelperLost]) and
// interrupts the turn in flight with the helper's -interrupt-once, or, where
// that interrupted none (exit 3: the next turn would run uncapped) or cannot
// tell (exit 1), stops the app-server. Every route reaches the helper's
// environment alone, never argv nor Codex's, since each carries the job's
// token; the helper's errors are the run's.
func codexStartHelper(req sandbox.RunRequest, paths Paths) string {
	q := shellword.Quote
	helper, sock, thread := q(paths.CodexNoteHelper()), q(paths.CodexSocket()), q(paths.CodexThread())
	base := fmt.Sprintf(`%s -adapter codex-app-server -socket %s -thread-file %s`, helper, sock, thread)
	env, flags := "", ""
	if req.Steer.Set() {
		env += fmt.Sprintf(`%s=$(cat %s) `, codexNotesEnv, q(paths.CodexNotesURL()))
		flags += " -notes-env " + codexNotesEnv
	}
	if len(req.Skills) > 0 {
		env += fmt.Sprintf(`%s=$(cat %s) `, codexSkillsEnv, q(paths.CodexSkillsURL()))
		flags += fmt.Sprintf(" -skills-env %s -skills-dir %s", codexSkillsEnv, q(paths.CodexSkills()))
		for _, digest := range skillDigests(req.Skills) {
			flags += " -skill-digest " + q(digest)
		}
	}
	if limit := req.Limits.MaxTurns; limit > 0 {
		flags += fmt.Sprintf(" -step-cap %d -cap-marker %s", limit, q(paths.CodexCapFired()))
	}
	// The helper is executable: the repository gate made it so ([codexRepoGate]).
	return fmt.Sprintf(`%[1]s%[2]s%[3]s < /dev/null > /dev/null &
hpid=$!
( while kill -0 $$ 2> /dev/null && kill -0 "$hpid" 2> /dev/null; do sleep %[4]s; done; kill -0 $$ 2> /dev/null || exit 0; : > %[5]s; %[2]s -interrupt-once < /dev/null > /dev/null || end "$apid" again ) &
gpid=$!
`, env, base, flags, codexGuardPoll, q(paths.CodexHelperLost()))
}

// codexBridgeCheck is the part of a Codex run's script that starts nothing
// unless the note helper's codex-bridge, asking the app-server before the
// thread starts, finds Codex would hand the model each bridge's tools under
// their own names (internal/notehelper, "The Codex bridge check"): run in the
// background, so an end request is taken while it waits, and its refusal,
// naming the server, on the run's error stream. Each bridge goes as the
// SHA-256 of every address the run wrote for it, both wirings' where it has two.
func codexBridgeCheck(req sandbox.RunRequest, paths Paths) string {
	type bridge struct {
		Name    string   `json:"name"`
		Digests []string `json:"url_sha256"`
	}
	var bridges []bridge
	for _, name := range crewletServers(req.MCPServers) {
		b := bridge{Name: name, Digests: []string{sha256Hex(req.MCPServers[name].URL)}}
		if f := req.FreshWiring; f != nil {
			if s, ok := f.MCPServers[name]; ok {
				b.Digests = append(b.Digests, sha256Hex(s.URL))
			}
		}
		bridges = append(bridges, b)
	}
	config, _ := json.Marshal(map[string]any{"socket": paths.CodexSocket(),
		"wait_seconds": waitSeconds(codexBridgeCheckWait), "bridges": bridges}) // strings and ints cannot fail to encode
	q := shellword.Quote
	return fmt.Sprintf(`printf '%%s\n' %s > "$rr/bridge.json" || exit 1
"$rh" -adapter codex-bridge -config "$rr/bridge.json" < /dev/null &
cpid=$!
while kill -0 "$cpid" 2> /dev/null; do [ -z "$endasked" ] || { end "$cpid"; endbefore; }; sleep 0.1; done
wait "$cpid" || { echo 'crewlet: the run does not start: Codex would not hand its model the engine'"'"'s own tools under their own names, or that could not be checked (above)' >&2; exit 1; }
`, q(string(config)))
}

// codexBridgeCheckWait bounds each answer the bridge check waits for: the
// listing starts every MCP server, the bridge under its own startup bound
// ([codexMCPStartupWait]), past which the run would fail anyway, so that and
// the helper's own thirty-second bound on an answer.
const codexBridgeCheckWait = codexMCPStartupWait + 30*time.Second

// sha256Hex is the SHA-256 of s in hex.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// codexGuardPoll is how often the guard looks for the helper: half a second,
// a few of a model's shortest responses, so a run outlives its helper by at
// most that before its turn is interrupted.
const codexGuardPoll = "0.5"

// codexAborted defines the script's `aborted`: true where the rollout lines
// the run appended (past before, of the thread reported, else the one
// continued) record a turn aborted as interrupted, as the result reads them
// ([codexEvents.keepRollout]).
func codexAborted(paths Paths) string {
	return fmt.Sprintf(`aborted() { as=${tid:-$sid}; [ -n "$as" ] || return 1; af=$(find %s -type f -name "rollout-*-$as.jsonl" 2> /dev/null | LC_ALL=C sort | head -n 1); [ -n "$af" ] && tail -n +$((before + 1)) "$af" | grep -E '"type": *"turn_aborted"' | grep -qE '"reason": *"interrupted"'; }`+"\n",
		shellword.Quote(paths.CodexSessions()))
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
		shellword.Quote(paths.CodexSessions()))
	fmt.Fprintf(&b, `[ -z "$r" ] || tail -n +$((before + 1)) "$r" | grep -E '"type": *"(turn_context|token_usage_record|token_count|turn_aborted)"' | sed 's/^/{"type":"%s","line":/; s/$/}/'`+"\n",
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

// routing answers [router] with the key alone: a Codex run's calls go to its
// built-in provider, and OPENAI_BASE_URL in its env moved no request (measured
// on 0.171.0, stand-ins at both routes: neither was reached).
func (Codex) routing() []routeVar { return routes(roleSigns, "CODEX_API_KEY") }

// Pins is CODEX_HOME, the box's own [Paths.CodexHome], where the run's
// configuration, login, sessions and skills are, whatever a profile sets.
func (Codex) Pins(_ context.Context, _ sandbox.RunRequest, paths Paths) []Pinned {
	return []Pinned{{Name: "CODEX_HOME", Value: paths.CodexHome()}}
}

// Events is a new decoder for one stream, read whole or followed ([Decoder]).
func (Codex) Events() Decoder { return &codexEvents{} }

// Parse decodes a whole stream already in hand ([codexEvents]).
func (Codex) Parse(stdout []byte) sandbox.Result { return decodeAll(&codexEvents{}, stdout) }

// artifactFiles is what a Codex run leaves in its box beside the session it
// continues: its staged configurations with their tokens (config.toml is the
// home's, put back as the run ends), the socket, the thread, the helper, its
// routes and its renew config, the server's output. No directory: its skills
// sit in CODEX_HOME's own skill root beside the home's, and the next Codex run
// removes the run's ([writeCodexSkills]).
func (Codex) artifactFiles(p Paths) []string {
	return []string{p.CodexConfig(), p.CodexConfigFresh(), p.CodexSocket(),
		p.CodexCapFired(), p.CodexHelperLost(), p.CodexSkillsURL(), p.CodexSkillsURLFresh(),
		p.CodexThread(), p.CodexThread() + ".next", p.CodexNoteHelper(), p.CodexNotesURL(),
		p.CodexNotesURLFresh(), p.CodexRenew(), p.CodexRepoAgents(), p.WorkDir() + "/codex-server.log"}
}
