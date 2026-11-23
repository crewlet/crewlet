package executor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

func init() { Register(Muse{}) }

// MuseName is this runner's config name.
const MuseName = "muse-code"

// Muse drives Muse Code headless (`muse exec --json`), measured on 1.4.25.
//
// Its model is reached through the run ENVIRONMENT (META_API_KEY) or the login
// a local box is seeded with (`.config/muse/auth.json`). Its settings are
// written whole ([museSettings]); its session is a directory, packed and
// restored by its script ([museSessionGate], [musePack]). Every run is
// `--yolo`: approvals and the OS sandbox off and the workspace trusted for
// this run alone, since 1.4.25 loads no MCP server, the seat's bridge
// included, in an untrusted workspace (measured), so a repository's rules,
// skills and MCP servers load as they do under Claude Code. Its end on
// request is end.go's protocol: SIGTERM flushes the session and exits 143.
type Muse struct{}

var (
	_ Harness           = Muse{}
	_ carrier           = Muse{}
	_ saver             = Muse{}
	_ artifactHolder    = Muse{}
	_ artifactDirHolder = Muse{}
	_ inPlaceGate       = Muse{}
	_ sessionJudge      = Muse{}
	_ mcpDeferrer       = Muse{}
)

// NewMuse returns the runner.
func NewMuse() *Runner { return New(Muse{}) }

// Name is the harness's key in the runner registry.
func (Muse) Name() string { return MuseName }

// Resumes says Muse Code continues a session it ran: `muse exec --session-id`
// on an id its data directory holds resumes it (measured on 1.4.25).
func (Muse) Resumes() bool { return true }

// museSupported is the one Muse Code release this runner is written for, every
// behaviour here measured on it: the release the profile's written_for names
// ([harnessprofile.Release]), so the profile and the runner never name two. An
// older box is refused before anything runs; a newer one runs a session of its
// own and never continues one; and a session continues only where its writer
// and the box both run it exactly ([Muse.Continues], [museSessionGate]).
var museSupported = sync.OnceValue(func() string {
	p, _ := harnessprofile.Builtin(MuseName)
	return harnessprofile.Release(p.WrittenFor)
})

// museDeferral is how many tools, across a run's MCP servers, make Muse Code
// send none of them and offer each only through its tool_search, the bridge's
// included, and the release that was measured on. On 1.4.25 a run listing 96
// to 99 sent them all, and 77 with one of 200 KB among them; one listing 100
// deferred them all, one-word tools as much as the browser's, and with four of
// Muse's own tools fewer too: only MCP tools count, never their size. No
// setting moves the count or keeps a server loaded. A pin moves only once it
// is measured again (TestMuseDefersAtACountMeasuredOnTheReleaseItRuns).
var museDeferral = struct {
	at      int
	release string
}{at: 100, release: "1.4.25"}

// defersMCPAt is [museDeferral]'s count ([mcpDeferrer]).
func (Muse) defersMCPAt() int { return museDeferral.at }

// museSystemMax is the most bytes of standing instructions Muse Code takes in
// `run.system_prompt_append`: past it, 1.4.25 refuses the run before its model
// ("run.system_prompt_append is 300018 bytes; the limit is 65536 bytes",
// measured). A run whose system text is longer is refused at its launch,
// naming the limit, rather than started to fail in its box.
const museSystemMax = 65536

// Session is the session a Muse Code run works in ([Harness.Session]): the one
// it continues, or a new one under a UUID minted for it, which `--session-id`
// starts a session under; and, for a run that continues one, a second UUID for
// the session it starts instead where its box holds that one neither way.
func (Muse) Session(req sandbox.RunRequest) Session {
	if req.Resume != "" {
		return Session{ID: req.Resume, Continues: true, Fresh: uuid.NewString(),
			Replaces: req.Carried != nil && req.Carried.Replace}
	}
	return Session{ID: uuid.NewString()}
}

// Command is the run as ONE SCRIPT ([museScript]), so the wrapper's
// redirections (the stream, the error file) reach each step.
func (Muse) Command(req sandbox.RunRequest, session Session, paths Paths, _ string) string {
	return "sh -c " + shellword.Quote(museScript(req, session, paths, sandbox.JobStopGrace))
}

// museScript is a Muse Code run's script, giving its harness stop to go once
// asked to end: the end protocol (end.go); the release read and held to
// [museSupported]; the session named, restored where it was carried, and recorded
// ([museSessionGate], [Paths.SessionRecord]); the harness run under the end
// protocol ([launchHarness]); its subagents' usage added to the stream
// ([museChildUsage]); and its session packed ([museCapture]), exiting with
// the harness's status.
//
// THE BRIEF IS A FILE: `--prompt-file` reads it byte for byte, and stdin is
// not read beside it (measured on 1.4.25).
func museScript(req sandbox.RunRequest, session Session, paths Paths, stop time.Duration) string {
	q := shellword.Quote
	parts := []string{
		"muse", "exec", "--json",
		// Approvals and the OS sandbox off, the workspace trusted for this
		// run alone (not saved): a headless run has nobody to answer a
		// prompt, and an untrusted workspace loads no MCP server at all.
		"--yolo",
		`--session-id "$s"`,
		// The goal-reminder observers are model requests of their own, which
		// a seat's run has no use for and would pay for.
		"--disable-reminders",
		// No Claude Code or Codex personal rules and skills from the box
		// home: the seat's are its own.
		"--no-foreign-personal-context",
		// Exec offers no ask-the-user tool; the flag holds that whatever a
		// later release does. ask_and_wait is on the bridge.
		"--disable-request-user-input",
		// A continued session resumes in the checkout this box has, wherever
		// the box that wrote it had its own (measured; harmless otherwise).
		"--allow-workspace-switch",
	}
	// VERBATIM, as the muse-code entry names it in Muse's own terms.
	if req.Model != "" {
		parts = append(parts, "--model", q(req.Model))
	}
	if req.Effort != "" {
		parts = append(parts, "--reasoning-effort", q(req.Effort))
	}
	// THE TURN CAP AS MUSE'S OWN: past it a run fails "model did not reach a
	// terminal state within N step(s)" ([museEvents.Result]); each of the
	// exec's runs is capped apart ([museCapMark]).
	if req.Limits.MaxTurns > 0 {
		parts = append(parts, "--max-model-steps", strconv.Itoa(req.Limits.MaxTurns))
	}
	// THE KEY ON STDIN, where the entry signs in by it (META_API_KEY): the
	// script reads it, and Muse's own environment, which its shell inherits
	// whole, never holds it (measured on 1.4.25).
	parts = append(parts, `--prompt-file "$p" $kf`)

	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	b.WriteString(endPrologue(paths, stop))
	b.WriteString(endHarness(""))
	b.WriteString(museRelease)
	line(`if ! at %s; then printf 'crewlet: this box runs Muse Code %%s, older than %s, the release this runner is written for\n' "$n" >&2; exit 1; fi`,
		floorArgs(museSupported()), museSupported())
	line(`p=%s`, q(paths.Brief()))
	// THE CAP NAMED IN THE STREAM, for the decoder to hold the exec's runs
	// to ([museCapMark]).
	if req.Limits.MaxTurns > 0 {
		line(`printf '%%s\n' %s`, q(fmt.Sprintf(`{"payload_type":%q,"turns":%d}`, museCapMark, req.Limits.MaxTurns)))
	}
	b.WriteString(museSessionGate(session, paths, req.FreshWiring != nil))
	line(`printf 'id %%s\ncontinued %%s\nwhy %%s\nrelease %%s\n' "$s" "$k" "$w" "$v" > %s`,
		q(paths.SessionRecord()))
	b.WriteString(museWiring(paths))
	b.WriteString(museShellEnvironment(paths, req.SignInEnv))
	before, after := museChildUsage(paths)
	b.WriteString(before)
	b.WriteString(launchHarness(strings.Join(parts, " "), paths))
	b.WriteString(after)
	b.WriteString(museCapture(paths))
	line(`exit "$code"`)
	return b.String()
}

// museRelease opens every Muse Code run's script, as [claudeRelease] opens
// Claude Code's: `muse --version` read once as v (`Muse Code 1.4.25
// (1.4.25-R9361.1)`), its first numbers as n, and `at MAJOR MINOR PATCH`. A
// VERSION IT CANNOT READ IS TAKEN FOR A CURRENT RELEASE, and a missing harness
// fails at the run.
const museRelease = `v=$(muse --version < /dev/null 2> /dev/null | head -n 1); n=${v#"${v%%[0-9]*}"}; n=${n%%[!0-9.]*}; n=${n%.}
a=${n%%.*}; r=${n#*.}; b=${r%%.*}; c=${r#*.}; c=${c%%.*}
case "$n" in [0-9]*.[0-9]*.[0-9]*) ;; *) a= ;; esac
at() { [ -z "$a" ] || [ "$a" -gt "$1" ] || { [ "$a" -eq "$1" ] && { [ "$b" -gt "$2" ] || { [ "$b" -eq "$2" ] && [ "$c" -ge "$3" ]; }; }; }; }
`

// museFind is the part of a script that sets x to the directory of the
// session the variable s names under [Paths.MuseSessions], at whatever day it
// is filed under, and e to that directory from the sessions directory down;
// both to nothing where the box holds none with a record.
func museFind(paths Paths) string {
	sessions := shellword.Quote(paths.MuseSessions())
	return `x= e=; for y in ` + sessions + `/*/*/*/"$s"; do [ -s "$y/session.jsonl" ] && [ ! -L "$y" ] && x=$y; done
[ -z "$x" ] || e=${x#` + shellword.Quote(paths.MuseSessions()+"/") + `}
`
}

// museSessionGate is the part of a run's script that names the session its
// harness runs in (s), whether it continues the one asked (k) and, where it
// could not, why (w). As [claudeSessionGate]: the box's copy is continued
// unless a carried archive holds what it does not (a digest not in
// [Paths.SessionDigests]); that archive is restored once it holds the
// session's record, its digest recorded; neither held nor restored, the minted
// session starts and says why, on the brief a new session is handed.
//
// RESTORED UNDER THE DAY IT WAS FILED AT, every other copy removed first, so
// the paths its record names of its tool outputs still hold: Muse resumes a
// session under any day, from another home and workspace, and a torn last
// line as it is (measured on 1.4.25), so NO LINE IS CUT.
func museSessionGate(session Session, paths Paths, rewired bool) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	q := shellword.Quote
	if !session.Continues {
		line(`s=%s k=no w=`, q(session.ID))
		return b.String()
	}
	staged := paths.SessionStaging()
	carried := q(paths.SessionCarried())
	in := staged + "/in"
	sessions := q(paths.MuseSessions())
	restore := strings.Join([]string{
		"rm -rf " + q(staged),
		"mkdir -p " + q(in),
		"tar -xzf " + carried + " -C " + q(in) + " < /dev/null > /dev/null 2>&1",
		// THE ONE SESSION DIRECTORY the archive holds, under its day.
		`{ y=; for z in ` + q(in) + `/*/*/*/"$s"; do [ -d "$z" ] && [ ! -L "$z" ] && [ -f "$z/session.jsonl" ] && ` +
			`[ ! -L "$z/session.jsonl" ] && y=$z; done; [ -n "$y" ]; }`,
		`e=${y#` + q(in+"/") + `}`,
		// THE BOX'S OWN COPIES GO only now, once the archive is known to
		// hold a conversation.
		`for z in ` + sessions + `/*/*/*/"$s"; do rm -rf "$z"; done`,
		"mkdir -p " + sessions + `/"${e%/*}"`,
		`mv "$y" ` + sessions + `/"$e"`,
	}, " && ")
	line(`s=%s k=yes w= h= d=`, q(session.ID))
	b.WriteString(museFind(paths))
	line(`[ -n "$x" ] && h=1`)
	// A SESSION CONTINUES ON THE SUPPORTED RELEASE ALONE, read bare as the
	// engine reads it ([releaseOf]): any other, or none that reads, starts a
	// session of its own.
	line(`%s[ "$rv" = %s ] || w="this box runs Muse Code ${rv:-a release it could not read}, and a session continues only on %s, the release this runner is written for"`,
		bareReleaseShell("rv"), q(museSupported()), museSupported())
	line(`if [ -s %s ]; then d=$(cksum < %s 2> /dev/null) || d=; fi`, carried, carried)
	if session.Replaces {
		line(`if [ -n "$h" ] && [ -s %s ]; then h=; fi`, carried)
	} else {
		line(`if [ -n "$h" ] && [ -n "$d" ] && ! grep -qxF "$s $d" %s 2> /dev/null; then h=; fi`,
			q(paths.SessionDigests()))
	}
	line(`if [ -z "$w" ] && [ -z "$h" ]; then`)
	line(`  if [ ! -s %s ]; then w='it is not in this box, and no archive of it was carried here'`, carried)
	line(`  elif ! command -v tar > /dev/null 2>&1 || ! command -v gzip > /dev/null 2>&1; then w='this box has no tar and gzip to restore its archive with'`)
	line(`  elif ! { %s; }; then w='its archive could not be restored'`, restore)
	// Continued only once its digest is recorded and read back, for
	// [claudeSessionGate]'s reason.
	line(`  elif ! { %s; }; then w=%s`, recordDigest(paths, "s"), q(restoreDigestUnrecorded))
	line(`  fi`)
	line(`  rm -rf %s`, q(staged))
	line(`fi`)
	line(`rm -f %s`, carried)
	line(`if [ -z "$w" ]; then`)
	if rewired {
		line(`  rm -rf %s %s`, q(paths.MuseServersFresh()), q(paths.MuseTakeConfigFresh()))
	} else {
		line(`  :`)
	}
	line(`else`)
	line(`  s=%s k=no`, q(session.Fresh))
	line(`  printf '%%s%%s\n' %s "$w" >&2`, q("crewlet: this run was asked to continue the Muse Code session "+
		session.ID+", and starts a new session, "+session.Fresh+", instead: "))
	line(`  [ ! -s %s ] || p=%s`, q(paths.FreshBrief()), q(paths.FreshBrief()))
	if rewired {
		// THE NEW SESSION WIRED TO ITS OWN TOKEN, or not started:
		// [claudeSessionGate]'s reason.
		line(`  { rm -rf %s && mv -f %s %s; } || { echo 'crewlet: the new session could not be wired to the tools of its seat, so the run does not start' >&2; exit 1; }`,
			q(paths.MuseServers()), q(paths.MuseServersFresh()), q(paths.MuseServers()))
		line(`  if [ -f %s ]; then mv -f %s %s || { echo 'crewlet: the new session could not be wired to its notes, so the run does not start' >&2; exit 1; }; fi`,
			q(paths.MuseTakeConfigFresh()), q(paths.MuseTakeConfigFresh()), q(paths.MuseTakeConfig()))
	}
	line(`fi`)
	return b.String()
}

// museChildUsage is the two parts of a run's script around its harness that
// add to its stream (the script's stdout) what its subagents spent: before
// the launch, the number of lines of every subagent's record the session
// already holds ([Paths.MuseChildLines]); after it, the mark
// ([museChildMark]) and every `model_completed` record past those lines, the
// requests this process's subagents made. Muse's `run.usage` counts the main
// thread alone and its stream carries nothing of a child (measured on 1.4.25),
// so these are the rest of the run's spend ([museEvents.runSpend]).
func museChildUsage(paths Paths) (before, after string) {
	q := shellword.Quote
	lines := q(paths.MuseChildLines())
	before = ": > " + lines + "\n" + museFind(paths) +
		`if [ -n "$x" ] && [ -d "$x/subagent" ]; then find "$x/subagent" -type f -name session.jsonl -exec wc -l {} + > ` +
		lines + " 2> /dev/null; fi\n"
	after = museFind(paths) + "printf '%s\\n' " + q(`{"payload_type":"`+museChildMark+`"}`) + "\n" +
		`if [ -n "$x" ] && [ -d "$x/subagent" ]; then find "$x/subagent" -type f -name session.jsonl 2> /dev/null | ` +
		`while IFS= read -r f; do n=$(sed -n "s|^ *\([0-9][0-9]*\) $f\$|\1|p" ` + lines + ` | head -n 1); ` +
		`tail -n +"$((${n:-0} + 1))" "$f" | grep -F '"kind":"model_completed"'; done; fi` + "\n"
	return before, after
}

// museArchiveFormat is the layout a Muse Code run packs its session in
// ([musePack]) and the one it restores ([museSessionGate]): one gzipped tar
// of members named from [Paths.MuseSessions] down,
// `<YYYY>/<MM>/<DD>/<session>/`: its session.jsonl, its subagents' records
// (subagent/) and its tool outputs (tool-outputs/), which a resume takes up
// whole from any home's data directory (measured on 1.4.25). Never cron.db,
// a binary SQLite file the archive's text scrub would corrupt, nor the cli
// log: Muse writes both afresh on a resume (measured). A change is a new
// format.
const museArchiveFormat = 1

// ArchiveFormat is the layout a Muse Code run packs its session in ([carrier]).
func (Muse) ArchiveFormat() int { return museArchiveFormat }

// VersionCommand prints a box's Muse Code release ([carrier]).
func (Muse) VersionCommand() string { return "muse --version" }

// layout is a Muse Code archive's: a gzipped tar of the session's files.
func (Muse) layout() archiveLayout { return archiveTar }

// charges is false: its meter counts nothing ([carrier.charges]).
func (Muse) charges(string, string) bool { return false }

// Continues reports whether a session written by Muse Code release writer can
// be continued in a box whose Muse Code is release box ([carrier]): only where
// the box continues any session ([Muse.continuesInPlace]) and the writer is
// as [Muse.ContinuesWriter] holds it. Any other pair starts a session of its
// own, saying which release it found.
func (m Muse) Continues(box, writer string) (bool, string) {
	if ok, why := m.continuesInPlace(box); !ok {
		return false, why
	}
	return m.ContinuesWriter(writer)
}

// continuesInPlace reports whether a box whose Muse Code is release box
// continues a session at all, the copy it holds as much as one carried to it
// ([inPlaceGate]): only on [museSupported], as the run's own gate holds it
// ([museSessionGate]), so the engine asks before a relaunch hands the brief
// that continues the box's copy.
func (Muse) continuesInPlace(box string) (bool, string) {
	if box == museSupported() {
		return true, ""
	}
	if box == "" {
		box = "a release it could not read"
	}
	return false, fmt.Sprintf(museBoxRefused, box, museSupported())
}

// museBoxRefused is why a box whose Muse Code is not [museSupported]
// continues no session, its own copy nor a carried one: the engine's check
// and the run's gate say it alike.
const museBoxRefused = "this box runs Muse Code %.64s, and a session continues only on %s, the release this " +
	"runner is written for"

// ContinuesWriter is [Muse.Continues] judged of the writer alone ([carrier]):
// only a session [museSupported] wrote; one written by any other release, or
// by one that named no release this build reads, is refused, saying so.
func (Muse) ContinuesWriter(writer string) (bool, string) {
	switch writer {
	case museSupported():
		return true, ""
	case "":
		return false, writerNamedNoRelease("Muse Code")
	}
	return false, fmt.Sprintf("the session was written by Muse Code %.64s, and only a session %s wrote "+
		"continues, the release this runner is written for", writer, museSupported())
}

// meter is the take's meter of a Muse Code archive ([carrier.meter]): no
// figure. This build reads no count or estimate of a Muse session's next
// request off its archive ([Muse.sessionTraits]). The archive is left to the
// caller, which drains it ([startMeter]).
func (Muse) meter(context.Context, io.Reader, string, string, *passBudget) (meterReading, error) {
	return meterReading{}, nil
}

// museCapture is the part of a run's script that packs its session once its
// harness has exited ([musePack]).
func museCapture(paths Paths) string {
	return musePack(paths, packAt{staged: paths.SessionStaging(), dest: paths.SessionCapture()})
}

// musePack packs the session s names into at.staged and moves it over
// at.dest, its digest kept ([packDigest]): the members [museArchiveFormat]
// names, its record first. A pack beside a working harness is held to
// [claudeHeldTar]'s rule, whose lists catch a file rewritten as tar reads it
// and which reads the first member as the session's transcript.
func musePack(paths Paths, at packAt) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	q := shellword.Quote
	sessions := paths.MuseSessions()
	b.WriteString(museFind(paths))
	line(`if [ -n "$x" ]; then`)
	line(`  if command -v tar > /dev/null 2>&1 && command -v gzip > /dev/null 2>&1; then`)
	line(`    set -- "$e/session.jsonl"; for y in subagent tool-outputs; do [ ! -d "$x/$y" ] || [ -L "$x/$y" ] || set -- "$@" "$e/$y"; done`)
	tar := fmt.Sprintf(`{ tar -czf %s -C %s "$@" < /dev/null > /dev/null 2> %s; t=$?; [ "$t" = 0 ] || { [ "$t" = 1 ] && grep -q 'changed as we read it' %s; }; }`,
		q(at.staged+"/capture"), q(sessions), q(at.staged+"/error"), q(at.staged+"/error"))
	if at.working {
		tar = claudeHeldTar(sessions, at.staged)
	}
	line(`    rm -rf %s; if mkdir -p %s && %s && %s && mv -f %s %s; then packed=1; else echo "crewlet: the session $s was not packed to be continued in another box: $(cat %s 2> /dev/null)" >&2; fi`,
		q(at.staged), q(at.staged), tar, packDigest(paths, "s", at.staged), q(at.staged+"/capture"), q(at.dest),
		q(at.staged+"/error"))
	line(`    rm -rf %s`, q(at.staged))
	line(`  else`)
	line(`    echo 'crewlet: this box has no tar and gzip, so the session was not packed to be continued in another box' >&2`)
	line(`  fi`)
	line(`fi`)
	return b.String()
}

// Pack packs session now ([carrier]), as a run's script does once its harness
// exits, into staged and over dest: [packNothing] where the box holds no
// record of it, [packFailed] where no archive was made. Held to the rule of a
// pack beside a working harness where working says so.
func (Muse) Pack(paths Paths, session, staged, dest string, working bool) string {
	return museOnDemand(paths, session, packAt{staged: staged, dest: dest, working: working})
}

// PackSave packs a save of rec's session beside a harness that may still be
// working ([saver]), held to the rule of a pack beside a working harness: a
// copy of the directory made beside a running run resumes whole in another
// box (measured on 1.4.25), and a tool output written as it packs refuses
// that save, which the next one retries. The record refuses no save.
func (Muse) PackSave(paths Paths, rec sessionRecord, staged, dest string) (string, error) {
	return museOnDemand(paths, rec.id, packAt{staged: staged, dest: dest, working: true}), nil
}

// museOnDemand is a pack the engine makes of session outside the run's own
// script, at at.
func museOnDemand(paths Paths, session string, at packAt) string {
	return "s=" + shellword.Quote(session) + " packed=\n" + musePack(paths, at) +
		fmt.Sprintf("[ -n \"$x\" ] || exit %d\n[ -n \"$packed\" ] || exit %d\n", packNothing, packFailed)
}

// museSwitchesOff is every variable that moves what a Muse Code run reads or
// adds, removed whatever set it: XDG_CONFIG_HOME and XDG_DATA_HOME move its
// settings, login and sessions away from the paths this runner writes, packs
// and seeds ([Paths.MuseConfig], [Paths.MuseSessions]); and
// TBH_EVAL_APPEND_SYSTEM_PROMPT_FILE appends a file to its system prompt
// beside the seat's (measured on 1.4.25).
var museSwitchesOff = []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "TBH_EVAL_APPEND_SYSTEM_PROMPT_FILE"}

// Output is where a job writes under `--json`: its stream is its result,
// decoded whole or followed ([museEvents]).
func (Muse) Output(paths Paths) Output {
	return Output{Stdout: paths.Result(), Events: true, Unset: museSwitchesOff}
}

// routing answers [router] with nothing: a Muse Code run's calls go where its
// login says (auth.json's providers.meta.api_base_url), never where an entry's
// env does. Measured on 1.4.25 with the login at one stand-in and
// ANTHROPIC_BASE_URL, OPENAI_BASE_URL and OPENROUTER_BASE_URL, which its binary
// names, at another: for muse-stub-1, claude-sonnet-4-5 and gpt-5 every request
// reached the login's.
func (Muse) routing() []routeVar { return nil }

// Pins is the run's posture, over whatever the seat's env, the image or a
// profile set: the launcher's update check and background download off
// (MUSE_NO_AUTO_UPDATE; the bare binary makes none).
func (Muse) Pins(context.Context, sandbox.RunRequest, Paths) []Pinned {
	return []Pinned{{Name: "MUSE_NO_AUTO_UPDATE", Value: "1"}}
}

// Events is a new decoder for one stream, read whole or followed ([Decoder]).
func (Muse) Events() Decoder { return &museEvents{} }

// Parse decodes a whole stream already in hand: the result of a run whose
// result IS its stream.
func (Muse) Parse(stdout []byte) sandbox.Result { return decodeAll(&museEvents{}, stdout) }

// museSettings is the run's own settings ([Paths.MuseSettings]), written whole:
// telemetry off (26 connections to api.meta.ai over two runs without it, none
// with it, measured) and the seat's standing instructions appended to Muse's
// own system prompt. Its MCP servers are added by the run's script
// ([museWiring]).
type museSettings struct {
	SchemaVersion int           `json:"schema_version"`
	Telemetry     museTelemetry `json:"telemetry"`
	Run           *museRun      `json:"run,omitempty"`
}

type museTelemetry struct {
	Enabled bool `json:"enabled"`
}

// museRun is the settings' `run` block: SystemPromptAppend lands at the very
// end of the request's instructions, re-read from the settings on every
// resume (measured on 1.4.25), so a continued run is told by its own launch.
type museRun struct {
	SystemPromptAppend string `json:"system_prompt_append,omitempty"`
}

// museMCPServer is one server as Muse's settings read it (1.4.25): an HTTP
// server by its url and headers, a command `type: stdio`; every one REQUIRED,
// so one that cannot be reached fails the run before its model ("Required MCP
// server `crewlet` failed during startup", rc 1, measured), where a server not
// required is silently left out; and the engine's call wait as
// `tool_timeout_sec`, past which a call answers "tool timed out" (measured),
// where Muse's own is 300 seconds. Both are settings-file fields alone: `-c`
// refuses tool_timeout_sec.
type museMCPServer struct {
	Type           string            `json:"type,omitempty"`
	URL            string            `json:"url,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	Command        string            `json:"command,omitempty"`
	Args           []string          `json:"args,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	Required       bool              `json:"required"`
	ToolTimeoutSec int64             `json:"tool_timeout_sec,omitempty"`
}

// museMCP is s in Muse's own vocabulary ([museMCPServer]).
func museMCP(s sandbox.MCPServer) museMCPServer {
	out := museMCPServer{Required: true}
	if s.Transport == sandbox.TransportHTTP {
		out.URL, out.Headers = s.URL, s.Headers
	} else {
		out.Type, out.Command, out.Args, out.Env = "stdio", s.Command, s.Args, s.Env
	}
	if s.CallTimeout > 0 {
		// WHOLE SECONDS, rounded up, so the harness never cancels a call
		// the engine still waits for.
		out.ToolTimeoutSec = int64((s.CallTimeout + time.Second - 1) / time.Second)
	}
	return out
}

// museNonce is a run's own suffix to its servers' names: six hex digits (24
// bits) from the system's random source, new every run, so the run's servers
// are under names no file of the repository's can declare beforehand (a
// trusted run loads the repository's `.mcp.json` beside them), and a name of
// up to 19 characters stays whole ([museServerNameShown]). A variable so a
// case can name the files it reads.
var museNonce = func() string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// writeMuseServers writes each of servers as dir/<name>-<nonce>.json, which
// the run's script adds to its settings under that name ([museWiring]), the
// model offered its tools as `mcp__<name>_<nonce>__<tool>`: refused for a
// name that is not a file's, which `muse mcp add-json` would be handed whole.
func writeMuseServers(ctx context.Context, box sandbox.Sandbox, dir string, servers map[string]sandbox.MCPServer,
	nonce string) error {

	for name, s := range servers {
		full := name + "-" + nonce
		if !museServerName(full) {
			return fmt.Errorf("executor: the MCP server %q is not named as Muse Code reads a server's name: "+
				"letters, digits, '-' and '_' (rename it in the seat's MCP servers)", name)
		}
		if err := writeJSON(ctx, box, dir+"/"+full+".json", museMCP(s)); err != nil {
			return fmt.Errorf("executor: writing the MCP server %q for Muse Code: %w", name, err)
		}
	}
	return nil
}

// museServerNameShown is the longest server name Muse Code shows the model
// as it is: a server's tools come under one entry, `mcp__<name>`, of at most
// 31 characters, and a longer name is shown as a hash of it, which runs all
// the same (measured on 1.4.25, names of up to 70 characters).
const museServerNameShown = 26

// museServerName reports whether name, a server's name with the run's suffix,
// is one a file and a shell word carry as it is: its file, `<name>.json`,
// within the 255 bytes a file's name takes.
func museServerName(name string) bool {
	if name == "" || len(name)+len(".json") > 255 {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

// WriteConfig writes what a Muse Code run reads: its settings
// ([museSettings]), its MCP servers ([writeMuseServers]) with a fresh
// session's beside them ([sandbox.RunRequest.FreshWiring]), its skills, its
// notes plugin, take config and note helper where it is wired to its notes or
// its skills' loads, and its brief, and the brief a new session is handed in
// a continued one's place, which `--prompt-file` names. Its standing
// instructions, told of notes where it reads them, are refused past
// [museSystemMax].
func (m Muse) WriteConfig(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, _ Session,
	paths Paths) (string, error) {
	if _, err := toolEnvOmitted(req.SignInEnv); err != nil {
		return "", err
	}
	system := req.System
	if req.Steer.Set() {
		system = withNoteInstruction(system, m.NoteInstruction())
	}
	if n := len(system); n > museSystemMax {
		return "", fmt.Errorf("executor: this run's standing instructions are %d bytes, and Muse Code takes "+
			"at most %d (run.system_prompt_append): shorten the seat's system prompt, its policies or its "+
			"memory", n, museSystemMax)
	}
	settings := museSettings{SchemaVersion: 1}
	if system != "" {
		settings.Run = &museRun{SystemPromptAppend: system}
	}
	if err := writeJSON(ctx, box, paths.MuseSettings(), settings); err != nil {
		return "", fmt.Errorf("executor: writing Muse Code's settings: %w", err)
	}
	nonce := museNonce()
	if err := writeMuseServers(ctx, box, paths.MuseServers(), req.MCPServers, nonce); err != nil {
		return "", err
	}
	if err := writeMuseSkills(ctx, box, req.Skills, paths); err != nil {
		return "", err
	}
	notes, loads := req.Steer.Set(), req.SkillLoads.Set()
	if notes || loads {
		if err := writeMuseNotes(ctx, box, req, paths); err != nil {
			return "", err
		}
	}
	if f := req.FreshWiring; f != nil {
		if err := writeMuseServers(ctx, box, paths.MuseServersFresh(), f.MCPServers, nonce); err != nil {
			return "", err
		}
		if notes || loads {
			take, err := hookTakeConfig(f.MCPServers, f.Steer, f.SkillLoads, "")
			if err != nil {
				return "", err
			}
			if err = box.WriteFile(ctx, paths.MuseTakeConfigFresh(), take); err != nil {
				return "", fmt.Errorf("executor: writing the note helper's config for a new session: %w", err)
			}
		}
	}
	if err := box.WriteFile(ctx, paths.Brief(), []byte(req.Brief)); err != nil {
		return "", fmt.Errorf("executor: writing the brief: %w", err)
	}
	if req.Resume != "" && req.FreshBrief != "" {
		if err := box.WriteFile(ctx, paths.FreshBrief(), []byte(req.FreshBrief)); err != nil {
			return "", fmt.Errorf("executor: writing the brief a new session is handed: %w", err)
		}
	}
	return paths.MuseSettings(), nil
}

// writeMuseNotes writes a run's notes plugin ([musePluginManifest]), its take
// config ([hookTakeConfig]) and the note helper's build for the box's
// machine ([writeNoteHelper]).
func writeMuseNotes(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, paths Paths) error {
	take, err := hookTakeConfig(req.MCPServers, req.Steer, req.SkillLoads, "")
	if err != nil {
		return err
	}
	if err = box.WriteFile(ctx, paths.MuseTakeConfig(), take); err != nil {
		return fmt.Errorf("executor: writing the note helper's config: %w", err)
	}
	manifest, err := musePluginManifest(paths.MuseNoteHelper(), paths.MuseTakeConfig(), req.Steer.Set(),
		req.SkillLoads.Set())
	if err != nil {
		return err
	}
	if err = box.WriteFile(ctx, paths.MuseNotesPlugin()+"/.muse-plugin/plugin.json", manifest); err != nil {
		return fmt.Errorf("executor: writing the run's notes plugin: %w", err)
	}
	return writeNoteHelper(ctx, box, paths.MuseNoteHelper())
}

// museWiring is the part of a run's script that wires its settings before its
// harness starts: the run's notes plugin installed and approved where it has
// one, among no other, since the clear removed every installed plugin
// ([Paths.MusePlugins]); then each MCP server added
// whole. AN APPROVAL REWRITES THE SETTINGS, dropping every server's
// `required` and `tool_timeout_sec`, while `muse mcp add-json` keeps both
// (measured on 1.4.25), so the servers come after it. A plugin Muse would not
// take leaves the run without its notes, as its error stream says; a server
// it would not take fails the run, which would have no tools. The install's
// answer is kept whole ([Paths.MuseNotesInstall]) for the confirm to read the
// package's digest from, and removed where the approval failed.
func museWiring(paths Paths) string {
	q := shellword.Quote
	plugin := paths.MuseNotesPlugin()
	errs := q(paths.WorkDir() + "/muse-wiring-error")
	installed := q(paths.MuseNotesInstall())
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line(`if [ -f %s ]; then`, q(plugin+"/.muse-plugin/plugin.json"))
	line(`  chmod 0755 %s 2> /dev/null`, q(paths.MuseNoteHelper()))
	line(`  if ! { muse plugins install %s --scope user --json < /dev/null > %s 2> %s && muse plugins approve %s --json < /dev/null > /dev/null 2>> %s; }; then`,
		q(plugin), installed, errs, museNotesPlugin, errs)
	line(`    echo "crewlet: the run's notes plugin could not be installed, so the run takes no notes between its steps: $(head -c 400 %s 2> /dev/null)" >&2`, errs)
	line(`    rm -f %s`, installed)
	line(`  fi`)
	line(`fi`)
	line(`for f in %s/*.json; do`, q(paths.MuseServers()))
	line(`  [ -f "$f" ] || continue`)
	line(`  n=${f##*/}; n=${n%%.json}`)
	line(`  muse mcp add-json "$n" "$(cat "$f")" --user < /dev/null > /dev/null 2> %s || { echo "crewlet: the MCP server $n could not be added to the run's settings, so the run does not start: $(head -c 400 %s 2> /dev/null)" >&2; exit 1; }`,
		errs, errs)
	line(`done`)
	line(`rm -f %s`, errs)
	return b.String()
}

// museShellOff is every variable the run's model's shell runs without, since
// it inherits Muse's environment whole (measured on 1.4.25): the entry's key,
// which the script hands Muse on stdin instead ([museShellEnvironment]), and
// OTEL_EXPORTER_OTLP_*, an exporter's address and headers, of which Muse reads
// the endpoint alone.
const museShellOff = "OTEL_EXPORTER_OTLP_"

// museShellEnvironment is the part of a run's script that keeps the
// environment its harness, and so the model's shell, starts with to what it
// needs: the entry's META_API_KEY, where it has one, written to a file only
// the script's user reads, opened as the script's stdin, which the launch
// hands the harness, and removed at once, so Muse reads the key on stdin
// (`--api-key-stdin`, kf) and no file or variable keeps it; every other
// variable the run signs in by (signIn, [sandbox.RunRequest.SignInEnv]),
// which Muse never reads, and every OTEL_EXPORTER_OTLP_* variable unset.
// A sign-in name no shell can unset is refused by [Muse.WriteConfig].
func museShellEnvironment(paths Paths, signIn []string) string {
	key := shellword.Quote(paths.WorkDir() + "/muse-key")
	unset := "unset META_API_KEY"
	if names, err := toolEnvOmitted(signIn); err == nil {
		for _, name := range names {
			unset += " " + name
		}
	}
	return `kf=
if [ -n "${META_API_KEY-}" ]; then
  (umask 077 && printf '%s' "$META_API_KEY" > ` + key + `) && exec 0< ` + key + ` && kf=--api-key-stdin
  rm -f ` + key + `
fi
` + unset + `
for o in $(env | sed -n 's/^\(` + museShellOff + `[A-Za-z0-9_]*\)=.*/\1/p'); do unset "$o"; done
`
}

// artifactFiles is the files a Muse Code run leaves in its box of its own
// beside its sessions, which a later run must not find ([artifactHolder]):
// the note helper, its take configs, its answer to the notes plugin's install
// and the count of its subagents' lines its script kept, and the personal
// AGENTS.md Muse loads as the user's own instructions, whatever wrote it
// (measured on 1.4.25). Its settings are written whole by every run, and its
// sessions and login are kept.
func (Muse) artifactFiles(p Paths) []string {
	return []string{p.MuseChildLines(), p.MuseTakeConfig(), p.MuseTakeConfigFresh(), p.MuseNoteHelper(),
		p.MuseNotesInstall(), p.MuseInstructions()}
}

// artifactDirs is the directories a Muse Code run leaves of its own
// ([artifactDirHolder]): its MCP servers, a fresh session's, and its plugin;
// and the two skill directories and the installed plugins Muse loads as the
// user's own, whatever wrote them (measured on 1.4.25), so a run loads only
// the skills its launch writes.
func (Muse) artifactDirs(p Paths) []string {
	return []string{p.MuseServers(), p.MuseServersFresh(), p.MuseNotesPlugin(), p.MuseSkills(), p.MuseConfigSkills(),
		p.MusePlugins()}
}

// museChargeBytes is the fewest bytes a Muse Code session's brief is counted
// at a token: one, the most tokens any estimate can make of n bytes, since
// Muse's own estimate was not read.
const museChargeBytes sandbox.Charge = 1

// sessionTraits is how a relaunch judges a Muse Code session, each value the
// one that never under-counts its next request, since Muse compacts at its
// catalogue's window for the model (api.meta.ai's, behind a sign-in), which
// the session's record does not hold (measured on 1.4.25):
//   - FirstRequestBox, with no point its box can say: the relaunch always
//     hands the summarized telling;
//   - CallKeyingNone: no reading of the session thins a telling;
//   - [museChargeBytes].
func (Muse) sessionTraits() sandbox.SessionTraits {
	return sandbox.SessionTraits{FirstRequest: sandbox.FirstRequestBox, CallKeying: sandbox.CallKeyingNone,
		Charge: museChargeBytes}
}
