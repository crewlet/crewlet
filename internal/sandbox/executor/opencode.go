package executor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/activityline"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

func init() { Register(OpenCode{}) }

// OpenCodeName is this runner's config name.
const OpenCodeName = "opencode"

// transcriptDetailLimit caps one transcript line's echoed text: the grammar's
// own preview bound.
const transcriptDetailLimit = activityline.DetailLimit

// openCodeUsageEvent is the type of the one line the run's script adds to the
// event stream: the session's own usage report, asked for once the run has
// ended. OpenCode never prints this type, so nothing it emits is read as it.
const openCodeUsageEvent = "crewlet.usage"

// openCodeUsageBefore is the type of the line a run that CONTINUES a session
// adds before its `opencode run`: the same report, asked for before the run
// takes the session up. OpenCode reports the session's lifetime totals, an
// import's restored ones included, so a continued run's own spend is the
// report after less the report before. Never printed by OpenCode, as
// [openCodeUsageEvent].
const openCodeUsageBefore = "crewlet.usage_before"

// openCodeContextEvent is the type of the lines a run's script adds once its
// session is exported: each a page of the session's message list as its server
// answered it, or a marker that a page was past what the pages may put into
// the stream ([openCodeContext]). Never printed by OpenCode, as
// [openCodeUsageEvent].
const openCodeContextEvent = "crewlet.context"

// openCodeContextPages is the most pages, one assistant message each, a run's
// script asks for to measure its session's newest request ([openCodeContext]).
// A page past the first is asked only where the one before held a failed
// message. A job that ended by itself ends on a step's error at most once, but
// one that made no request pages back into the session it took up, which an
// end request may have left with two interrupted steps: the real exports
// (testdata/opencode/2.3.2/export.json) hold the measuring step
// fourth newest, behind two interrupted and one refused. SIX holds that and
// two failures more. Past it, the job's record keeps the measure carried along
// its copy's lineage.
const openCodeContextPages = 6

// openCodeContextBytes is the most the measure's pages may put into the event
// stream in all ([openCodeContext]), which the read collecting the job carries
// back within the poll's share of its budget ([sandbox.BoxReadBudget]).
//
// ONE MEBIBYTE: OpenCode cuts a tool's text output at 50 KiB, so a step's
// message holds about twenty, while a step that read an image carries it as a
// data URI of up to about 27 MiB, which no page should carry. Past it, a
// marker says the page's size and no page follows, never a page cut short.
// Each line stays far under the stream's line bound ([maxLineBytes]).
const openCodeContextBytes = 1 << 20

// openCodeListenWait bounds the wait for the run's private server to say where
// it listens: 0.4 to 0.5 s on 2.3.2 (six starts, a 2-core container), so
// sixty is room for a contended box, short of holding a seat for a server
// that is never coming up.
const openCodeListenWait = 60 * time.Second

// openCodeClock is the command a run's script reads the time from, in whole
// seconds, to bound its waits for its servers.
const openCodeClock = "date +%s"

// openCodeMCPWait bounds the wait for the run's declared MCP servers to
// connect before the brief is sent. OpenCode gives one server thirty seconds
// to list its tools by default, and they connect concurrently, so one still
// pending past twice that is stuck: the run starts without it, as OpenCode
// would, and says so on stderr.
const openCodeMCPWait = 60 * time.Second

// openCodeCallWait bounds one call a run's script makes to its own server
// whose answer is a few KiB at most: the readiness `mcp.list`, `session.get`,
// the leftover steers' inbox list and cancels, the end's `session.interrupt`,
// the login's export and import, and the stored logins' list and removal. Past
// it the call is ended and the script goes on as where the server is gone, or,
// before the client, ends the run ([openCodeBounded], [openCodeForgetLogins]).
//
// TEN SECONDS, about three times the slowest such call measured on 2.3.2: a
// readiness `mcp.list` answers in 0.4 s, and in 3.5 s where the server's
// OTLP endpoint refuses connections (a 2-core container; the other kinds
// untested on 2.3.2). Past it a call costs what a gone server does. OpenCode
// bounds nothing itself: its client waits as long as an answering server takes.
const openCodeCallWait = 10 * time.Second

// openCodeSessionCallWait bounds one call a run's script makes to its own
// server whose answer holds the session or one message of it, the export
// ([openCodeCapture]) and a page of the message list ([openCodeContext]):
// bounded in count, not bytes, since a message carries a file a tool read as a
// data URI.
//
// THIRTY SECONDS, about five times the slowest measured: 6.4 s to export a 256
// MiB session (the most a kept session's export holds unpacked, the scrub's
// plaintext bound, [scrubPlainMax]) on a busy 2-core box, an earlier
// OpenCode 2 release. Past it only the wait is lost: an export it ended is packed on demand
// once the job is over ([OpenCode.Pack]), and a page it ended leaves the
// figure carried along the copy's lineage.
const openCodeSessionCallWait = 30 * time.Second

// openCodeLoginSync is how often a run given a login exports it over the box's
// copy while it works ([OpenCode.Command]).
//
// THE INTERVAL IS THE LOSS WINDOW: a refreshed login lives only in OpenCode's
// database until exported, and every production teardown of a run in flight
// kills it outright ([sandbox.Local.Kill], `docker rm -f`). An OAuth token
// lives an hour or more, so thirty seconds leaves under one refresh in a
// hundred in the window, at one export per interval (0.14 s on an earlier
// OpenCode 2 release, under half a percent of a core); ten seconds would triple that for a
// window a mid-run kill already makes rare.
const openCodeLoginSync = 30 * time.Second

// OpenCode drives OpenCode 2 headless (`@opencode/cli`); the script refuses
// 1.x, which cannot run it, by name. The seat's model is a custom provider
// with an explicit base URL and model id: OpenCode otherwise resolves
// "<provider>/<model>" against a catalogue and the vendor's endpoint, failing
// a gateway's unlisted model or silently hitting the wrong host.
//
// A PERSON'S NOTES REACH THE RUN AT ITS NEXT STEP: the run's plugin
// (opencode_steer.js, [OpenCode.WriteConfig]) polls the bridge's notes route
// ([sandbox.SteerWiring.URL]) and admits each as a steer with `resume: false`,
// handed the model after the step in flight; one after the session settled
// starts nothing. A policy denying the plugin (`integration.use`) leaves a run
// that never asked, which the collection says (steer_reader_never_ran).
type OpenCode struct{}

var _ Harness = OpenCode{}

// NewOpenCode returns the runner.
func NewOpenCode() *Runner { return New(OpenCode{}) }

// Name is the harness's key in the runner registry.
func (OpenCode) Name() string { return OpenCodeName }

// resumesOnLaunchRoute: measured on 2.3.2, the one release supported
// ([launchRouted]): created on a
// provider whose baseURL was stub A, `run --session` with it at stub B, with
// another provider id at B, and with both configured and `--model` naming B's;
// each resumed request (25187 bytes, the first run's prompt in it) reached B.
func (OpenCode) resumesOnLaunchRoute() bool { return true }

// Resumes says OpenCode continues a session it reported: naming a session its
// database holds is the whole of continuing it ([OpenCode.Session],
// [openCodeSessionGate]).
func (OpenCode) Resumes() bool { return true }

// NoteInstruction is the standing instruction about notes an OpenCode run is
// given: it takes a person's notes between its steps, through the plugin its
// configuration loads — see [OpenCode].
func (OpenCode) NoteInstruction() string { return OpenCodeSteerInstruction }

// Command is the run: a short shell script ([openCodeScript]), since OpenCode
// 2 is driven through a server of the run's own. `opencode run` alone either
// attaches to the host's shared background service, holding the credentials of
// whichever run started it, or (`--standalone`) sends the brief before the MCP
// servers connect, and 2.0 fixes a step's tools as it starts, so the first
// step lacked the seat's bridged tools. So the script starts `opencode serve`
// on a loopback port of its own, waits for every declared MCP server to leave
// `pending`, runs the brief against it and stops it. Between them: the session
// settled ([openCodeSessionGate]), the login kept ([openCodeLoginSync]),
// leftover steers cancelled ([openCodeLeftoverSteers]), the session exported
// ([openCodeCapture]) and measured ([openCodeContext]), every call bounded
// ([openCodeBounded]), an end on request honoured (end.go).
func (OpenCode) Command(req sandbox.RunRequest, session Session, paths Paths, _ string) string {
	return "sh -c " + shellword.Quote(openCodeScript(req, session, paths, openCodeRunTimes()))
}

// openCodeRunTimes is the waits every run's script runs at ([openCodeTimes]).
func openCodeRunTimes() openCodeTimes {
	return openCodeTimes{
		listen: openCodeListenWait, mcp: openCodeMCPWait, sync: openCodeLoginSync, stop: sandbox.JobStopGrace,
		call: openCodeCallWait, sessionCall: openCodeSessionCallWait,
		clock: openCodeClock,
	}
}

// openCodeTimes is how long a run's script waits for its server to listen, its
// MCP servers to connect and each kind of call, how often it exports a login,
// the grace it gives a process of its own to go, and the clock it reads them
// on: a value rather than the constants inline ([openCodeRunTimes]), so a case
// about a wait runs it at a length it can tell apart, and decides when a bound
// runs out rather than racing the wall clock.
type openCodeTimes struct {
	listen, mcp, sync, stop time.Duration
	call, sessionCall       time.Duration
	clock                   string
}

// secs is a wait as the script reads one: whole seconds, never under one, since
// the script's clock reads whole seconds ([openCodeClock]) and a bound of zero
// would end every call as it starts.
func secs(d time.Duration) int { return max(1, int(d/time.Second)) }

// newOpenCodeSession mints the id a run's session is created under: 32
// lowercase hex characters from crypto/rand, which [OpenCode.Session] prefixes
// with the `ses_` 2.0 requires. Random rather than derived from the run,
// because OpenCode CONTINUES a session it already holds, and a reused box
// holds the last run's.
func newOpenCodeSession() string {
	var raw [16]byte
	// crypto/rand.Read does not fail on the platforms this builds for: it
	// crashes the process rather than return an error since Go 1.24.
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

// Session is the session an OpenCode run works in ([Harness.Session]), each id
// `ses_` and a minted [newOpenCodeSession]: the one it continues
// ([sandbox.RunRequest.Resume]), or a new one, plus, for a continued run, the
// new one it starts instead where its box holds that one neither way. `run
// --session` on an id OpenCode does not hold CREATES it, so a run never names
// an id it was not handed or minted.
func (OpenCode) Session(req sandbox.RunRequest) Session {
	if req.Resume != "" {
		return Session{ID: "ses_" + strings.TrimPrefix(req.Resume, "ses_"), Continues: true,
			Fresh:    "ses_" + newOpenCodeSession(),
			Replaces: req.Carried != nil && req.Carried.Replace}
	}
	return Session{ID: "ses_" + newOpenCodeSession()}
}

// openCodeRelease is the one OpenCode release this runner supports, its
// newest: a box on an older one is refused at the run's start, naming its
// release ([openCodeScript]); one on a newer runs, a session carried in from
// another box starting fresh there while the box's own copy continues in
// place; and a carried session continues only where the box and the writer
// are both exactly it ([OpenCode.Continues]).
const openCodeRelease = "2.3.2"

// openCodeArchiveFormat is the layout an OpenCode run packs its session in: the
// document `opencode session export` prints — the session and its settled
// messages — gzipped, which `opencode session import` reads back
// ([openCodeScript]). A change to what is packed is a new format, which a box
// of this build refuses rather than reads.
const openCodeArchiveFormat = 1

// ArchiveFormat is the layout an OpenCode run packs its session in
// ([carrier]).
func (OpenCode) ArchiveFormat() int { return openCodeArchiveFormat }

// VersionCommand prints a box's OpenCode release ([carrier]).
func (OpenCode) VersionCommand() string { return "opencode --version" }

// Pack exports session now ([carrier]), as a run's script does once its run
// ends ([openCodeCapture]), but through a private server (`--standalone`),
// since the run's server, whose address and password only its script held, may
// be gone: into staged, its own staging directory, and over dest
// ([Paths.SessionPackHeld]), its digest kept ([openCodePackNow]). Never held to
// a room, as a save is: a refused final pack would lose the job's newest state
// on an estimate that counts every session in the box.
func (OpenCode) Pack(paths Paths, session, staged, dest string, _ bool) string {
	// Whether the job may still work (the ignored flag) changes nothing: the
	// export holds only settled messages either way.
	return openCodePackNow(paths, session, staged, dest, false)
}

// PackSave packs a save of rec's session beside its running server ([saver]),
// by the pack on demand's own export ([openCodePackNow]), into staged and over
// dest, waiting where its box lacks the room the export takes
// ([openCodeSaveRoom]). A record naming a release outside [openCodeCountRead]
// is refused before any script runs: a save beside a running server is made
// only on a release a probe measured it on (the package doc's "An OpenCode
// session saved beside its running server").
func (OpenCode) PackSave(paths Paths, rec sessionRecord, staged, dest string) (string, error) {
	if release := releaseOf(rec.release); !openCodeCountRead(release) {
		return "", fmt.Errorf("executor: OpenCode %q's save beside its running server was never measured, so "+
			"its session is not saved as it works", release)
	}
	return openCodePackNow(paths, rec.id, staged, dest, true), nil
}

// openCodePackNow is ONE PACK ON DEMAND, [OpenCode.Pack]'s and
// [OpenCode.PackSave]'s: the pins every standalone call takes
// ([openCodeStandalonePins]), the room where room (a save's alone,
// [openCodeSaveRoom]), the probe, and [openCodePack]'s bounded export through
// a private server, beside a server that may still be writing (the package
// doc's "An OpenCode session saved beside its running server").
//
// It ends [packNothing] ONLY ON OPENCODE'S OWN ANSWER that it holds no such
// session ([openCodeNotHeld]), [packNoRoom] where a save's box lacks the room,
// and [packFailed] wherever else no archive was made ([Runner.PackCapture]): a
// probe that could not say is no "no session".
func openCodePackNow(paths Paths, session, staged, dest string, room bool) string {
	// A probe read as "no session" recorded a session the box held as none,
	// and the turn's end named an older capture as its last.
	q := shellword.Quote
	var b strings.Builder
	b.WriteString("sid=" + q(session) + " packed=\n" + openCodeStandalonePins)
	// A CONFIGURATION DIRECTORY INSIDE ITS OWN STAGING ONE, which the pack
	// makes anew and removes ([openCodePack]): a pack loads nothing a run left
	// in the box. OpenCode makes the directory where it is missing.
	fmt.Fprintf(&b, "OPENCODE_CONFIG_DIR=%s; export OPENCODE_CONFIG_DIR\n", q(staged+"/opencode-config"))
	if room {
		// THE ROOM BEFORE ANY OPENCODE PROCESS, the probe's included: the
		// probe is a standalone OpenCode too. No database yet is no session
		// yet, which the probe says; one it holds all the same is in a
		// database the room never read, which the export is not run on.
		b.WriteString(sandbox.MemoryDomainScript + "\n" + openCodeSaveRoom + "\n")
		fmt.Fprintf(&b, `db="${XDG_DATA_HOME:-$HOME/.local/share}/opencode/opencode.db" bounded=`+"\n"+
			`if [ -f "$db" ]; then crewlet_memory 2> /dev/null | crewlet_save_fits "$(crewlet_size "$db")" "$(crewlet_size "$db-wal")" || exit %d; bounded=1; fi`+"\n",
			packNoRoom)
	}
	b.WriteString(`if probe=$(opencode api --standalone session.get --param sessionID="$sid" < /dev/null 2>&1); then held=$probe; else held=; fi` + "\n")
	fmt.Fprintf(&b, `case "$held" in *%s*) ;; *) case "$probe" in %s) exit %d;; esac; `+
		`printf '%%s\n' "${probe:-OpenCode answered nothing about the session}" >&2; exit %d;; esac`+"\n",
		shellword.Quote(`"id":"`+session+`"`), openCodeNotHeld, packNothing, packFailed)
	if room {
		fmt.Fprintf(&b, `[ -n "$bounded" ] || { echo "crewlet: OpenCode's database is not at $db, so the memory its export takes cannot be bounded" >&2; exit %d; }`+"\n",
			packFailed)
	}
	b.WriteString(openCodePack(paths, staged, dest, "--standalone", "", true))
	fmt.Fprintf(&b, "[ -n \"$packed\" ] || exit %d\n", packFailed)
	return b.String()
}

// openCodeStandalonePins is what every standalone call of a pack on demand runs
// under: the models fetch and the updater off, so a save, which every running
// job takes every interval, reaches nothing off the box (2.3.2 refreshes its
// models.dev catalogue where the fetch is not switched off, seen in a run's
// events). No proxy pin: a proxy that never answers held up no 2.3.2
// standalone export (measured).
const openCodeStandalonePins = `export OPENCODE_DISABLE_MODELS_FETCH=1 OPENCODE_DISABLE_AUTOUPDATE=1` + "\n"

// openCodeSaveNeedFactor and openCodeSaveNeedBase are the memory an OpenCode
// save's export is held to need: the factor times its database's bytes and
// its WAL's, plus the base. Twelve and 128 MiB hold every point measured on
// 2.3.2 with a fifth's room for a shape not measured at the bound: the
// steepest a slope of 8.6, the highest intercept 125 MiB, a character past
// Latin-1 in every string (16 MiB peaked at 253 MiB, held to 328; 66 MiB at
// 693, held to 917); an all-ASCII session, 3.4 to 4.7, is over-counted. The
// package doc has the measurements.
const (
	openCodeSaveNeedFactor = 12
	openCodeSaveNeedBase   = 128 << 20
)

// openCodeSaveRoom defines crewlet_size (a file's bytes, 0 where it is none),
// crewlet_save_room, which prints the room a save's export may take off
// crewlet_memory's lines on its input ([sandbox.MemoryDomainScript]): the
// domain's limit less what its kernel cannot reclaim, capped by the machine's
// MemAvailable, else MemAvailable alone, else nothing; and crewlet_save_fits
// DB WAL, which fails, saying both figures, where the export's need exceeds a
// room read. AN UNREAD ROOM REFUSES NOTHING: the node's one pack slot still
// serializes such a pack where its box shares the node's memory.
var openCodeSaveRoom = `crewlet_size() { if [ -f "$1" ]; then ls -Lln "$1" | awk '{print $5}'; else echo 0; fi; }
crewlet_save_room() {
awk -F= '$1=="limit"{l=$2} $1=="held"{h=$2} $1=="avail_kb"{a=$2}
END{n=""; if (l ~ /^[0-9]+$/ && h ~ /^-?[0-9]+$/) n=l-h; if (a ~ /^[0-9]+$/) {v=a*1024; if (n=="" || v<n) n=v}; if (n!="") {if (n<0) n=0; printf "%.0f\n", n}}'
}
crewlet_save_fits() {
room=$(crewlet_save_room); need=$((` + strconv.Itoa(openCodeSaveNeedFactor) + ` * (${1:-0} + ${2:-0}) + ` + strconv.Itoa(openCodeSaveNeedBase) + `))
[ -z "$room" ] || [ "$need" -le "$room" ] || { echo "crewlet: the save's export needs about $need bytes of memory, and its box has $room" >&2; return 1; }
}`

// openCodeNotHeld matches OpenCode's answer to `session.get` on an id its
// database does not hold: its NotFoundError, printed as "Session not found:
// <id>", the one answer that says the box holds no such session.
const openCodeNotHeld = `*'Session not found'*|*NotFoundError*`

// layout is an OpenCode archive's: one gzipped JSON document
// ([openCodeArchiveFormat]).
func (OpenCode) layout() archiveLayout { return archiveDocument }

// charges is true of the export, the one document its meter counts whole
// ([carrier.charges]).
func (OpenCode) charges(string, string) bool { return true }

// Continues reports whether a session written by OpenCode release writer can
// be continued in a box whose OpenCode is release box ([carrier]): only where
// both are exactly [openCodeRelease]; a box whose release does not read
// refuses ([boxNamedNoRelease]), and any other pair starts fresh.
func (o OpenCode) Continues(box, writer string) (bool, string) {
	switch {
	case !releaseReads(box):
		return false, boxNamedNoRelease("OpenCode")
	case box != openCodeRelease:
		return false, fmt.Sprintf("this box runs OpenCode %s, and a carried session continues only on "+
			"OpenCode %s", box, openCodeRelease)
	}
	return o.ContinuesWriter(writer)
}

// ContinuesWriter reports whether a session OpenCode release writer wrote can
// be continued at all ([carrier]): exactly [openCodeRelease]. A writer that
// does not read continues nowhere ([writerNamedNoRelease]).
func (OpenCode) ContinuesWriter(writer string) (bool, string) {
	if !releaseReads(writer) {
		return false, writerNamedNoRelease("OpenCode")
	}
	if writer != openCodeRelease {
		return false, fmt.Sprintf("the session was written by OpenCode %s, and only a session %s wrote "+
			"continues in another box", writer, openCodeRelease)
	}
	return true, ""
}

// openCodeOlderCheck is the script's refusal of a box whose OpenCode 2 is
// older than [openCodeRelease], its release read off `$v` as [releaseOf]
// reads one ([bareReleaseShell]: a pre-release reads as none, and is let
// run), each number compared as a number.
func openCodeOlderCheck() string {
	var maj, mnr, pat int
	if _, err := fmt.Sscanf(openCodeRelease, "%d.%d.%d", &maj, &mnr, &pat); err != nil {
		panic("executor: openCodeRelease is no release: " + openCodeRelease)
	}
	return bareReleaseShell("r") + fmt.Sprintf(`older() { o=$IFS; IFS=.; set -- $1; IFS=$o; `+
		`[ "$1" -lt %[1]d ] && return 0; [ "$1" -gt %[1]d ] && return 1; `+
		`[ "$2" -lt %[2]d ] && return 0; [ "$2" -gt %[2]d ] && return 1; `+
		`[ "$3" -lt %[3]d ]; }; `+
		`if [ -n "$r" ] && older "$r"; then echo "crewlet: this box runs OpenCode $r; the opencode runner `+
		`supports OpenCode %[4]s, its newest — install it in the box image" >&2; exit 1; fi`,
		maj, mnr, pat, openCodeRelease)
}

// openCodeScript is the run's script for one session ([OpenCode.Command]).
// Every command but the run reads /dev/null, not the brief on stdin, and
// writes only to a file, a command substitution or /dev/null: stdout is the
// event stream.
//
// AN ANSWER GOES THROUGH A FILE WHERE THE FILE DOES WORK: a bounded call's
// ([openCodeAnswerFile]), the export, measured or bounded ([openCodePack]),
// and the message list's pages, each measured before it is printed
// ([openCodeContext]). An unbounded call's answer is read through `$(…)`.
func openCodeScript(req sandbox.RunRequest, session Session, paths Paths, times openCodeTimes) string {
	doc := shellword.Quote(paths.CredentialDocument())
	next := shellword.Quote(paths.CredentialDocument() + ".next")
	loop := shellword.Quote(paths.CredentialDocument() + ".sync")
	srv := shellword.Quote(paths.WorkDir() + "/opencode-server")
	vrr := shellword.Quote(paths.WorkDir() + "/opencode-version")
	ans := openCodeAnswerFile(paths)
	cfg := shellword.Quote(paths.OpenCodeConfig())

	// The brief goes on stdin ([Output.BriefOnStdin]), since OpenCode
	// re-quotes an argv prompt; the session is named ([OpenCode.Session]) so
	// the usage report is this run's; a title stops 2.0 making a second billed
	// call to write one.
	run := []string{"opencode", "run", "--server", `"$url"`, "--format", "json",
		"--title", "crewlet", "--session", `"$sid"`}
	// THE ENTRY'S MODEL AS WRITTEN, as every run on the host on the same
	// providers.llm entry hands `--model`: OpenCode splits it at its first
	// slash into provider and model, signed in by that provider's login or
	// key. A launch passes over every other harness's entry. THE EFFORT RIDES
	// THE MODEL as its variant, `provider/model#variant`: OpenCode 2 has no
	// flag for it.
	if req.Model != "" {
		model := req.Model
		if req.Effort != "" {
			model += "#" + req.Effort
		}
		run = append(run, "--model", shellword.Quote(model))
	}

	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	// THE END PROTOCOL BEFORE ANYTHING ELSE (end.go). The run's own end of its
	// harness interrupts the session through its server first, which settles
	// the step in flight so the export holds it, then ends the client within
	// the grace. Ended alone, the client leaves the server working the step,
	// its tool call running to the end (measured on 2.3.2; the API names it
	// `session.interrupt`, not `session.abort`). On
	// 2.3.2 the interrupted client is gone within 0.2 s of its SIGTERM
	// (measured), so the end is recorded as asked. The interrupt is
	// BOUNDED ([openCodeBounded]), never by the request it serves: a server that never answers leaves the step
	// unsettled, as a gone one does, and no end waits on the server.
	b.WriteString(endPrologue(paths, times.stop))
	b.WriteString(endHarness(fmt.Sprintf(`[ -z "$url" ] || bounded interrupt %d - opencode api --server "$url" session.interrupt --param sessionID="$sid" > /dev/null 2>&1`,
		secs(times.call))))

	// ABSENT AND FAILING ARE TWO ANSWERS: an opencode that is installed but
	// cannot start is not told to install, and the words it failed with are
	// kept, since that stderr is the transcript when nothing else was written.
	line(`command -v opencode > /dev/null || { echo 'crewlet: no opencode on the box PATH' >&2; exit 127; }`)
	// 1.x prints a bare version, 2.x `opencode v2.…`.
	line(`v=$(opencode --version < /dev/null 2> %s) || { st=$?; echo "crewlet: opencode is on the box PATH, but opencode --version failed with exit $st:" >&2; cat %s >&2; rm -f %s; exit 1; }`,
		vrr, vrr, vrr)
	line(`rm -f %s`, vrr)
	line(`case "$v" in [0-9]*) echo "crewlet: this box runs OpenCode $v; the opencode runner drives OpenCode 2 (the @opencode/cli package) — install it in the box image" >&2; exit 1;; esac`)
	// AN OLDER 2.x IS REFUSED BY NAME ([openCodeRelease]); a newer runs, and
	// one whose release does not read (a pre-release among them) is let run,
	// as a release the engine cannot read is taken for a current one.
	line(`%s`, openCodeOlderCheck())
	// THE RUN LOADS ONLY THE CONFIGURATION ITS LAUNCH WRITES, before any
	// command that reads one: OpenCode's global directory is the run's own,
	// empty ([Paths.OpenCodeConfigDir]), and what it reads from the home and
	// the workspace's parent is removed ([openCodeHomeConfig]).
	line(`rm -rf %[1]s && mkdir -p %[1]s || { echo 'crewlet: the run could not make its OpenCode configuration directory' >&2; exit 1; }`,
		shellword.Quote(paths.OpenCodeConfigDir()))
	line(`OPENCODE_CONFIG_DIR=%s; export OPENCODE_CONFIG_DIR`, shellword.Quote(paths.OpenCodeConfigDir()))
	line(`rm -rf %s`, strings.Join(openCodeHomeConfig(paths), " "))
	// THE SERVER'S PASSWORD IS ITS OWN: `opencode serve` given
	// OPENCODE_PASSWORD or OPENCODE_SERVER_PASSWORD prints none, and the
	// listen wait would run out. So both are unset, the wait asks for the
	// password line too, and the run's clients are handed it as
	// OPENCODE_PASSWORD, which a client reads first; the server is
	// started without it, so nothing the agent runs inherits it.
	line(`unset OPENCODE_PASSWORD OPENCODE_SERVER_PASSWORD`)
	// THE RUN'S CONFIGURATION, refused where there is none: the server would
	// otherwise run on the image's and checkout's documents, without the run's
	// posture or servers. A shell variable, never exported: it is handed to
	// the server alone.
	line(`config=$(cat %s) && [ -n "$config" ] || { echo 'crewlet: the run has no OpenCode configuration at '%s >&2; exit 1; }`,
		cfg, cfg)
	line(`imported= spid= xpid= xout=%s url=`, loop)
	b.WriteString(openCodeStops())
	b.WriteString(openCodeBounded(paths, times))
	b.WriteString(openCodeCursor())
	// holds: the file is a whole, non-empty JSON array. A database that
	// dropped a credential it could not refresh exports `[]`, which
	// must never replace the box's login: the teardown would write it back
	// over the shared one.
	line(`holds() { t=$(tr -d '[:space:]' < "$1") || return 1; case "$t" in '['*']') [ "$t" != '[]' ];; *) return 1;; esac; }`)
	// keep TAG FILE FLAGS...: one export into FILE, moved over the document
	// only where it holds a login and differs from it, bounded as TAG
	// ([openCodeBounded]). The three exports (the loop's, the last, a
	// signal's) each write their own FILE and never overlap: the loop is
	// stopped and waited for before either other starts, so no older export
	// lands last.
	line(`keep() { kt=$1 f=$2; shift 2; if bounded "$kt" %d - opencode auth export "$@" > "$f" 2> /dev/null && holds "$f" && [ "$(cat "$f")" != "$(cat %s)" ]; then mv -f "$f" %s; else rm -f "$f"; fi; }`,
		secs(times.call), doc, doc)
	// quit: a teardown signal's end, never an end request's: the end request
	// ignored, a call in flight ended and reaped (unbound), the run's client
	// ended (it runs in the background, so the group's signal did not stop the
	// script's wait on it), the loop stopped, the login exported once more,
	// the server stopped. The EXIT trap is cleared too, since quit did all it
	// would: left set, `exit` ran its stops again over stopped processes.
	line(`quit() { trap '' USR1; trap - EXIT TERM INT HUP; unbound; end "$harness"; harness=; unsync; [ -z "$imported" ] || keep quit %s --standalone; stop; exit "$1"; }`, next)
	line(`trap 'unsync; stop' EXIT`)
	line(`trap 'quit 143' TERM; trap 'quit 130' INT; trap 'quit 129' HUP`)
	b.WriteString(openCodeSessionGate(session, paths, req.FreshWiring != nil))
	// THE RECORD, before the server and the run, so a run killed or ended
	// before it reported anything still says which session it took up and why
	// it began again. The release last, as the harness printed it
	// ([parseSessionRecord]).
	line(`printf 'id %%s\ncontinued %%s\nwhy %%s\nrelease %%s\n' "$sid" "$k" "$w" "$v" > %s`,
		shellword.Quote(paths.SessionRecord()))
	// A REQUEST BEFORE THE SERVER LISTENS starts no server and no run: the job
	// is recorded as ended before its harness started, and the EXIT trap stops
	// the server. A session the box holds is packed by the read that asked
	// ([OpenCode.Pack]).
	line(`endbefore() { endedas=before; %s; exit 143; }`, strings.TrimSuffix(recordEnded(paths), "\n"))
	line(`[ -z "$endasked" ] || endbefore`)

	// ON THE SERVER ALONE: it runs the session, so only it reads the run's
	// configuration, as OPENCODE_CONFIG_CONTENT, which OpenCode ranks above a
	// repository's opencode.json and .opencode/, and, for a run wired to its
	// notes, the session its notes plugin admits them to. The client and the
	// `api` calls, the login's import among them, are answered by it.
	steered := ""
	if req.Steer.Set() {
		steered = `CREWLET_STEER_SESSION="$sid" `
	}
	line(`OPENCODE_CONFIG_CONTENT="$config" %sopencode serve --hostname 127.0.0.1 --port 0 < /dev/null > %s 2>&1 &`,
		steered, srv)
	line(`spid=$!`)
	line(`deadline=$(( $(%s) + %d ))`, times.clock, int(times.listen.Seconds()))
	line(`until grep -q '^server listening on ' %s 2> /dev/null && grep -q '^server password ' %s 2> /dev/null; do`, srv, srv)
	line(`  [ -z "$endasked" ] || endbefore`)
	line(`  if ! kill -0 "$spid" 2> /dev/null; then echo 'crewlet: opencode serve exited before it listened:' >&2; cat %s >&2; exit 1; fi`, srv)
	line(`  if [ "$(%s)" -ge "$deadline" ]; then`, times.clock)
	line(`    if grep -q '^server listening on ' %s 2> /dev/null; then echo 'crewlet: opencode serve listened but printed no server password within %s' >&2; else echo 'crewlet: opencode serve did not say where it listens within %s' >&2; fi`,
		srv, times.listen, times.listen)
	line(`    exit 1`)
	line(`  fi`)
	line(`  sleep 0.2`)
	line(`done`)
	line(`url=$(sed -n 's/^server listening on //p' %s)`, srv)
	line(`OPENCODE_PASSWORD=$(sed -n 's/^server password //p' %s); export OPENCODE_PASSWORD`, srv)
	line(`rm -f %s`, srv)
	// THE LOGIN, where the box was given one ([Paths.CredentialDocument], a
	// local box's): imported through the run's own server, once every login
	// its database held is removed ([openCodeForgetLogins]), and exported over
	// the document while the run works and as it ends ([openCodeLoginSync]).
	// A remote box is given none and signs in with the run environment's key.
	// Through the server, so no second OpenCode runtime opens the database.
	// Before the client, so an end request ends each call and starts no run.
	b.WriteString(openCodeForgetLogins(ans, secs(times.call)))
	line(`if [ -z "$endasked" ] && [ -s %s ]; then`, doc)
	line(`  if bounded login %d e opencode auth import --server "$url" %s > /dev/null 2>&1; then imported=1; elif [ -z "$endasked" ]; then echo 'crewlet: OpenCode refused the login this box was given — sign the engine in again with crewlet llm login' >&2; exit 1; fi`,
		secs(times.call), doc)
	line(`fi`)
	line(`if [ -n "$imported" ]; then`)
	line(`  %s &`, openCodeExporter(times.sync, loop))
	line(`  xpid=$!`)
	line(`fi`)

	if checks := openCodeReadiness(req.MCPServers); checks != "" {
		line(`deadline=$(( $(%s) + %d ))`, times.clock, int(times.mcp.Seconds()))
		line(`while :; do`)
		// A REQUEST HERE starts no run, leaving the tail to export the session
		// through the server that is up, and ends a call it finds in flight.
		line(`  [ -z "$endasked" ] || break`)
		// THE ANSWER FROM A FILE, as every bounded call's is
		// ([openCodeAnswerFile]). EACH CALL BOUNDED BY WHAT IS LEFT OF THE LOOP'S WAIT,
		// and by [openCodeCallWait], since the deadline is read only between
		// calls.
		line(`  mr=$(( deadline - $(%s) )); [ "$mr" -le %d ] || mr=%d; [ "$mr" -ge 1 ] || mr=1`,
			times.clock, secs(times.call), secs(times.call))
		line(`  bounded mcp "$mr" e opencode api --server "$url" mcp.list > %s 2> /dev/null; s=$(cat %s 2> /dev/null); rm -f %s`, ans, ans, ans)
		b.WriteString(openCodeShadowCheck(req.MCPServers))
		line(`  ready=1`)
		b.WriteString(checks)
		line(`  [ "$ready" = 1 ] && break`)
		line(`  if [ "$(%s)" -ge "$deadline" ]; then echo 'crewlet: an MCP server was still connecting after %s; the run starts without it' >&2; break; fi`, times.clock, times.mcp)
		line(`  sleep 0.5`)
		line(`done`)
	}

	// WHERE THE RUN BEGAN IN THE SESSION IT CONTINUES, recorded before the
	// run takes it up: see [openCodeRunStart].
	if session.Continues {
		b.WriteString(openCodeRunStart(paths, times, ans))
	}

	// WHAT THE CONTINUED SESSION HAD SPENT ([openCodeUsageBefore]), asked just
	// before the run takes it up and after an import, so the totals it
	// restored are taken off too. A new session's totals begin at the run.
	if session.Continues {
		line(`if [ "$k" = yes ] && bounded before %d e opencode api --server "$url" session.get --param sessionID="$sid" > %s 2> /dev/null; then before=$(cat %s) && case "$before" in '{'*) printf '{"type":"%s","report":%%s}\n' "$before";; esac; fi; rm -f %s`,
			secs(times.call), ans, ans, openCodeUsageBefore, ans)
	}
	b.WriteString(launchHarness(strings.Join(run, " "), paths))
	// NOTHING THE PLUGIN ADMITTED IS LEFT IN THE SESSION once the run has
	// ended: see [openCodeLeftoverSteers].
	if req.Steer.Set() {
		line(`%s`, openCodeLeftoverSteers(ans, times))
	}
	// THE USAGE from the session, not the stream: `run --format json` drops
	// the final step's counts ([openCodeUsageEvent]).
	line(`if bounded report %d - opencode api --server "$url" session.get --param sessionID="$sid" > %s 2> /dev/null; then report=$(cat %s) && case "$report" in '{'*) printf '{"type":"%s","report":%%s}\n' "$report";; esac; fi; rm -f %s`,
		secs(times.call), ans, ans, openCodeUsageEvent, ans)
	// NOTHING TO EXPORT of a session the run never started: an end before its
	// client started leaves a minted session OpenCode does not hold.
	line(`if [ "$endedas" != before ] || [ "$k" = yes ]; then`)
	b.WriteString(openCodeCapture(paths, times))
	line(`fi`)
	// WHAT THE SESSION'S NEWEST REQUEST CARRIED, asked of its server once it is
	// exported, and never on an end request: see [openCodeContext].
	b.WriteString(openCodeContext(paths, times))
	line(`unsync`)
	line(`[ -z "$imported" ] || keep last %s --server "$url"`, next)
	line(`stop; trap - EXIT TERM INT HUP`)
	line(`exit "$code"`)
	return b.String()
}

// openCodeRunStart records, for a run that continues its session (`continued
// yes`), WHERE THE RUN BEGAN IN IT: the newest message the session holds
// before the run takes it up, asked of the run's server once its MCP servers
// are ready, never on an end request ([openCodeBounded]), and written to the
// record as a `from` line ([Paths.SessionRecord], [parseSessionRecord],
// [sandbox.RunStart]). Every message after it is the run's own. The ask names
// `order=desc` and `limit=1` rather than rest on a default, and reads only the
// answer's cursor ([openCodeCursor]), never the message. The record is written
// anew, `from` before `release`, and moved over the old whole, since its
// reader stops at the release. Anything that fails writes no `from`, and the
// capture names no start: "not known", never "the whole session".
func openCodeRunStart(paths Paths, times openCodeTimes, ans string) string {
	rec := shellword.Quote(paths.SessionRecord())
	next := shellword.Quote(paths.SessionRecord() + ".next")
	return fmt.Sprintf(`if [ "$k" = yes ] && bounded start %[1]d e opencode api --server "$url" session.message.list --param sessionID="$sid" --param order=desc --param limit=1 > %[2]s 2> /dev/null; then rs=$(listcursor %[2]s); [ -z "$rs" ] || { printf 'id %%s\ncontinued %%s\nwhy %%s\nfrom %%s\nrelease %%s\n' "$sid" "$k" "$w" "$rs" "$v" > %[4]s && mv -f %[4]s %[3]s; }; fi; rm -f %[2]s %[4]s
`, secs(times.sessionCall), ans, rec, next)
}

// openCodeAnswerFile is the file a run's script has a BOUNDED call to its
// server ([openCodeBounded]) write its answer to, quoted, removed once read:
// `bounded` runs the call in the background so an end request can stop it,
// and inside a command substitution it would run in a subshell, which an end
// request never reaches. One file serves because the script makes one
// OpenCode call at a time and nothing it starts beside itself writes here.
// The export and the message list's pages have their own ([openCodePack],
// [openCodeContext]).
func openCodeAnswerFile(paths Paths) string {
	return shellword.Quote(paths.WorkDir() + "/opencode-answer")
}

// openCodeHomeConfig is every path under a box's home OpenCode reads as
// configuration or instructions besides its global directory, shell-quoted:
// the skill roots it reads from any home, Claude Code's instructions and
// skills, and what the project walk finds in the home, the workspace's parent.
// Measured on 2.3.2: each loaded from a box's home, and none once removed.
// OPENCODE_DISABLE_CLAUDE_CODE is no substitute: it drops the repository's
// CLAUDE.md and .claude/skills as well.
func openCodeHomeConfig(paths Paths) []string {
	rel := []string{".agents/skills", ".claude/CLAUDE.md", ".claude/skills", ".opencode", "opencode.json",
		"opencode.jsonc", "AGENTS.md", "CLAUDE.md", "CONTEXT.md"}
	out := make([]string, len(rel))
	for i, r := range rel {
		out[i] = shellword.Quote(paths.Home + "/" + r)
	}
	return out
}

// openCodeForgetLogins removes, through the run's server and ans, every login
// the box's database holds: an earlier run's import, and whatever else was
// stored since. The run imports its own after, so only the login it was handed
// signs it in. A database still holding one ends the run before its client
// starts. Each call is bounded by wait seconds and ended by an end request,
// after which nothing more is asked: no run starts.
func openCodeForgetLogins(ans string, wait int) string {
	list := fmt.Sprintf(`bounded forget %[2]d e opencode api --server "$url" credential.list > %[1]s 2>&1 || [ -n "$endasked" ] || { echo "crewlet: OpenCode could not list the logins its database holds: $(cat %[1]s)" >&2; exit 1; }`, ans, wait)
	return `if [ -z "$endasked" ]; then` + "\n" +
		"  " + list + "\n" +
		`  if [ -z "$endasked" ] && grep -q '"id":' ` + ans + `; then` + "\n" +
		`    grep -o '"id":"[^"]*","integrationID":' ` + ans + ` | sed 's/^"id":"\([^"]*\)".*/\1/' > ` + ans + `.ids` + "\n" +
		fmt.Sprintf(`    while IFS= read -r id; do bounded forget %d e opencode api --server "$url" credential.remove --param credentialID="$id" > /dev/null 2>&1; done < %s.ids; rm -f %s.ids`, wait, ans, ans) + "\n" +
		"    " + list + "\n" +
		`    if [ -z "$endasked" ] && grep -q '"id":' ` + ans + `; then echo 'crewlet: OpenCode kept a login the run could not remove, so the run does not start' >&2; exit 1; fi` + "\n" +
		"  fi\n  rm -f " + ans + "\nfi\n"
}

// openCodeSessionGate decides, before the run's server starts (it is handed
// the session), the session the run works in as sid; as k, whether it is the
// one it was asked to continue; and where not, why, as w.
//
// A continued run takes up the box's own copy unless an export carried in
// ([Paths.SessionCarried]) is one the copy does not hold
// ([Paths.SessionDigests], carry.go); that export, or one carried into a box
// with no copy, is imported in the copy's place. Neither held nor imported,
// the run starts its minted session instead and says why. An import writes no
// instruction state (core/src/session/transfer.ts), so its first request reads
// the run's instructions as initial, in the system prompt. Children are never
// imported: a subagent's work reaches the parent as its task's result.
func openCodeSessionGate(session Session, paths Paths, rewired bool) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	q := shellword.Quote
	if !session.Continues {
		line(`sid=%s k=no w=`, q(session.ID))
		return b.String()
	}
	staged := paths.SessionStaging()
	carried := q(paths.SessionCarried())
	digests := q(paths.SessionDigests())
	export := q(staged + "/export.json")
	// Every ask goes to a private server (`--standalone`): the run's has not
	// started, and a bare command reaches the host's shared service
	// ([OpenCode.Command]). These calls are not bounded: the job's end and the
	// turn's cap bound them from outside, OpenCode's WAL database never makes
	// a reader wait on a writer, and a writer meeting a lock fails past
	// OpenCode's busy_timeout (5 s on every release read), read as a session
	// not imported.
	line(`sid=%s k=yes w= h= d= own=`, q(session.ID))
	line(`held=$(opencode api --standalone session.get --param sessionID="$sid" < /dev/null 2> /dev/null) || held=`)
	line(`case "$held" in *%s*) h=1;; esac`, q(`"id":"`+session.ID+`"`))
	// THE EXPORT'S DIGEST, where one was carried in and the box can take it:
	// the box's copy is kept unless the export is one it does not hold.
	line(`if [ -s %s ]; then d=$(cksum < %s 2> /dev/null) || d=; fi`, carried, carried)
	if session.Replaces {
		// A COPY ITS RUN WAS CUT OFF IN is replaced by the export whatever it
		// holds: it may hold a step that never settled, which an export leaves
		// out (transfer.ts).
		line(`if [ -n "$h" ] && [ -s %s ]; then h= own=1; fi`, carried)
	} else {
		line(`if [ -n "$h" ] && [ -n "$d" ] && ! grep -qxF "$sid $d" %s 2> /dev/null; then h= own=1; fi`, digests)
	}
	line(`if [ -z "$h" ]; then`)
	line(`  if [ ! -s %s ]; then w='it is not in this box, and no export of it was carried here'`, carried)
	// A BOX RELEASE THAT DOES NOT READ continues nothing carried, as the
	// engine's check holds it ([boxNamedNoRelease]): no bare MAJOR.MINOR.PATCH
	// in what `--version` printed, as [releaseOf] reads one.
	line(`  elif ! { %s[ -n "$rv" ]; }; then w=%s`, bareReleaseShell("rv"), q(boxNamedNoRelease("OpenCode")))
	line(`  elif ! command -v gzip > /dev/null 2>&1; then w='this box has no gzip to unpack its export with'`)
	line(`  else`)
	line(`    rm -rf %s`, q(staged))
	line(`    if ! { mkdir -p %s && gzip -dc < %s > %s 2> /dev/null; }; then w='its export could not be unpacked'`,
		q(staged), carried, export)
	line(`    elif [ -n "$own" ] && ! out=$(opencode session delete --standalone "$sid" < /dev/null 2>&1); then w="OpenCode did not delete the box's own copy, which the export carried here replaces: $(printf '%%s' "$out" | tr '\n' ' ')"`)
	line(`    else`)
	// IMPORTED under `$PWD`, where `opencode run` works, keeping its id and
	// restoring its totals, the box's copy deleted (with its children) only
	// once the export unpacked. The import must SAY `Imported session: <id>`:
	// on an id it holds it answers "Session already exists" with success and
	// imports nothing, and `run --session` on an id not held makes an empty
	// session answered as if it remembered.
	line(`      out=$(opencode session import --standalone --directory "$PWD" %s < /dev/null 2>&1)`, export)
	// AN IMPORT IS CONTINUED ONLY ONCE ITS DIGEST IS RECORDED and read back
	// ([recordDigest]): unrecorded, the copy would go on past an export the
	// box does not know it holds, which carried in again would be imported
	// over it. Recorded before the import, a failed import would leave the
	// line naming a copy that does not hold it.
	line(`      case "$out" in *%s*) %s || w=%s;; *) w="OpenCode did not import it: $(printf '%%s' "$out" | tr '\n' ' ')";; esac`,
		q("Imported session: "+session.ID), recordDigest(paths, "sid"), q(restoreDigestUnrecorded))
	line(`    fi`)
	line(`    rm -rf %s`, q(staged))
	line(`  fi`)
	line(`fi`)
	b.WriteString(openCodeRenew(session, paths))
	line(`rm -f %s`, carried)
	// A NEW SESSION IS HANDED THE BRIEF WRITTEN FOR ONE where the run was
	// given it ([sandbox.RunRequest.FreshBrief]): the brief on stdin continues
	// a session OpenCode does not hold.
	line(`if [ -n "$w" ]; then sid=%s k=no; printf '%%s%%s\n' %s "$w" >&2; [ ! -s %s ] || exec < %s; fi`,
		q(session.Fresh),
		q("crewlet: this run was asked to continue the OpenCode session "+session.ID+
			", and starts a new session, "+session.Fresh+", instead: "),
		q(paths.FreshBrief()), q(paths.FreshBrief()))
	line(`%s`, openCodeStaleSteers())
	if rewired {
		// THE RUN IS WIRED TO THE SESSION IT STARTS
		// ([sandbox.RunRequest.FreshWiring]): a fresh session's values
		// ([Paths.OpenCodeValuesFresh]) move over the continued one's, the
		// other set removed before the server reads them, so the token the run
		// dials tells the bridge which session it started
		// (internal/api/mcpbridge), unforgeable by the model. A configuration
		// naming no value has neither set; every other has both
		// ([OpenCode.WriteConfig]), so a lone set is an unfinished write, and
		// the run ends rather than start its own session on the continued
		// one's token and skill guard, or with no values at all.
		values, fresh := q(paths.OpenCodeValues()), q(paths.OpenCodeValuesFresh())
		line(`if [ -n "$w" ]; then { [ -e %s ] || [ ! -e %s ]; } && rm -rf %s && { [ ! -e %s ] || mv %s %s; } || { echo 'crewlet: the new session could not be wired to the tools of its seat, so the run does not start' >&2; exit 1; }; else rm -rf %s; fi`,
			fresh, values, values, fresh, fresh, values, fresh)
	}
	return b.String()
}

// openCodeRenew is the part of a continuing run's gate that re-imports the
// box's own copy (h), so its first request renders this launch's standing
// instructions whole as the leading ones: the package doc's "A continued
// OpenCode session's standing instructions" says why, and what it costs. The
// copy is exported, held to a carried export's checks (the save room, non-empty,
// within [heldPlainMax], naming the session), deleted only then, and imported;
// any failure starts the minted session, saying why, and an import that failed
// keeps its export at [Paths.OpenCodeRenewExport]. The copy's child sessions
// are deleted with it and not imported, as on the carried path: nothing reads
// a finished run's children from the box (every read names sid alone).
func openCodeRenew(session Session, paths Paths) string {
	q := shellword.Quote
	return sandbox.MemoryDomainScript + "\n" + openCodeSaveRoom + "\n" + fmt.Sprintf(`if [ -n "$h" ] && [ -z "$w" ]; then
  rx=%[1]s re=%[2]s kept= db="${XDG_DATA_HOME:-$HOME/.local/share}/opencode/opencode.db"; rm -f "$rx"; mkdir -p %[3]s
  if [ ! -f "$db" ]; then w="its database is not at $db, so the memory re-importing it takes cannot be bounded"
  elif ! crewlet_memory 2> /dev/null | crewlet_save_fits "$(crewlet_size "$db")" "$(crewlet_size "$db-wal")" 2> "$re"; then w="its box lacks the room to re-import it: $(cat "$re" 2> /dev/null)"
  elif ! opencode session export --standalone "$sid" < /dev/null > "$rx" 2> "$re"; then w="OpenCode did not export it to renew its instructions: $(tr '\n' ' ' < "$re" 2> /dev/null)"
  elif ! n=$(($(wc -c < "$rx"))) || [ "$n" -le 0 ] || [ "$n" -gt %[4]d ] || ! grep -qF %[5]s "$rx"; then w="its export to renew its instructions is empty, past %[4]d bytes or names another session"
  elif ! out=$(opencode session delete --standalone "$sid" < /dev/null 2>&1); then w="OpenCode did not delete it to renew its instructions: $(printf '%%s' "$out" | tr '\n' ' ')"
  else
    out=$(opencode session import --standalone --directory "$PWD" "$rx" < /dev/null 2>&1)
    case "$out" in *%[6]s*) ;; *) kept=1 w="OpenCode did not re-import it to renew its instructions, its export kept at $rx: $(printf '%%s' "$out" | tr '\n' ' ')";; esac
  fi
  [ -n "$kept" ] || rm -f "$rx"; rm -f "$re"
fi
`, q(paths.OpenCodeRenewExport()), q(paths.SessionStaging()+"/renew-error"), q(paths.SessionStaging()), heldPlainMax,
		q(`"id":"`+session.ID+`"`), q("Imported session: "+session.ID))
}

// openCodeCapture exports the session the run worked in (sid) once the run has
// ended, after its leftover steers are cancelled and before its server stops,
// to be continued in another box: [openCodePack] through the run's own server,
// over [Paths.SessionCapture].
//
// NEVER THE DATABASE: OpenCode keeps its logins in the same file, and a remote
// box is never handed one. A box without gzip, or a failed export, is said on
// stderr; the run's status is never the export's. The export is BOUNDED
// ([openCodeSessionCallWait]): one past it never moves into place, and the
// read that takes the session packs it on demand ([OpenCode.Pack]), so a hung
// server costs the bound, never the session.
func openCodeCapture(paths Paths, times openCodeTimes) string {
	return openCodePack(paths, paths.SessionStaging(), paths.SessionCapture(), `--server "$url"`,
		fmt.Sprintf("bounded export %d - ", secs(times.sessionCall)), false)
}

// openCodePack exports sid through server, the run's own (`--server "$url"`)
// or a pack on demand's private one (`--standalone`), gzips it in staged and
// moves it over dest, its digest kept: ONE PACK for [openCodeCapture] and
// [openCodePackNow]. An export holds settled messages, never the note inbox;
// bounded, one empty or past [heldPlainMax] is refused before gzip.
//
// THE EXPORT GOES TO A FILE that gzip then reads: a pack on demand measures
// it there, and the run's script calls through `bounded`, which a pipeline's
// subshell would keep from an end request ([openCodeAnswerFile]). call
// prefixes the export: bounded in the run's script, bare in a pack on demand,
// under [sandbox.SessionPackWait], since OpenCode's WAL database lets it
// complete beside a live, stopped, killed or writing server.
func openCodePack(paths Paths, staged, dest, server, call string, bounded bool) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	q := shellword.Quote
	export, capture, failed := q(staged+"/export.json"), q(staged+"/capture"), q(staged+"/error")
	bound := ""
	if bounded {
		bound = fmt.Sprintf(`n=$(($(wc -c < %[1]s))) && { [ "$n" -gt 0 ] || { echo 'OpenCode exported it empty' > %[2]s; false; }; } && { [ "$n" -le %[3]d ] || { echo "its export is $n bytes, past the %[3]d a kept session's scrub reads" > %[2]s; false; }; } && `,
			export, failed, heldPlainMax)
	}
	line(`if command -v gzip > /dev/null 2>&1; then`)
	line(`  rm -rf %s; if mkdir -p %s && %sopencode session export %s "$sid" < /dev/null > %s 2> %s && %sgzip -c < %s > %s && %s && mv -f %s %s; then packed=1; else echo "crewlet: the session $sid was not exported to be continued in another box: $(cat %s 2> /dev/null)" >&2; fi`,
		q(staged), q(staged), call, server, export, failed, bound, export, capture, packDigest(paths, "sid", staged),
		capture, q(dest), failed)
	line(`  rm -rf %s`, q(staged))
	line(`else`)
	line(`  echo 'crewlet: this box has no gzip, so the session was not exported to be continued in another box' >&2`)
	line(`fi`)
	return b.String()
}

// openCodeContext measures the session the run worked in (sid) as its job
// ends, for a relaunch continuing the box's copy (internal/sandbox's
// LaunchRecord.Context): once the session is exported it asks the run's server
// for the newest assistant messages ONE A PAGE (`type=assistant`, `limit=1`),
// by each page's cursor ([openCodeCursor]), each page through a file and
// printed as one stream line ([openCodeContextEvent]), since a message's bytes
// are unbounded; one past what is left of [openCodeContextBytes] is said by
// its size and ends the asking. It stops at a page with no failed message or
// after [openCodeContextPages], skipping no page between two printed, so the
// engine ([measuredPrompt]) never takes an older figure for a newer request. A
// missing cursor, a page past its bound or an end request stops it: an end's
// wait is the export's ([sandbox.JobEndWait]).
func openCodeContext(paths Paths, times openCodeTimes) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line(`ctxfile=%s ctxcur= ctxn=0 ctxleft=%d`, shellword.Quote(paths.WorkDir()+"/opencode-context"),
		openCodeContextBytes)
	line(`while [ -z "$endasked" ] && [ "$ctxn" -lt %d ]; do`, openCodeContextPages)
	line(`  ctxn=$((ctxn + 1))`)
	line(`  bounded page %d e opencode api --server "$url" session.message.list --param sessionID="$sid" --param type=assistant --param limit=1 ${ctxcur:+--param cursor="$ctxcur"} > "$ctxfile" 2> /dev/null || break`,
		secs(times.sessionCall))
	line(`  ctxsize=$(wc -c < "$ctxfile") || break; ctxsize=$((ctxsize))`)
	line(`  [ "$ctxsize" -gt 0 ] || break`)
	line(`  if [ "$ctxsize" -gt "$ctxleft" ]; then printf '{"type":"%s","skipped":%%s}\n' "$ctxsize"; break; fi`,
		openCodeContextEvent)
	line(`  ctxleft=$((ctxleft - ctxsize))`)
	// Line breaks out, which JSON strings never hold raw, so a page is one
	// line. The test for a failed message is coarse in the safe direction
	// (any error object, a tool's too, costs a page more): the ENGINE decides
	// from the pages, never the script. The script measures because no pack
	// can: a company that keeps no session makes none.
	line(`  printf '{"type":"%s","page":'; tr -d '\n' < "$ctxfile"; printf '}\n'`, openCodeContextEvent)
	line(`  grep -q '"error"[[:space:]]*:[[:space:]]*{' "$ctxfile" || break`)
	line(`  ctxcur=$(listcursor "$ctxfile")`)
	line(`  [ -n "$ctxcur" ] || break`)
	line(`done`)
	line(`rm -f "$ctxfile"`)
	return b.String()
}

// openCodeCursorTail is how much of a list answer's end the script reads its
// cursor from ([openCodeCursor]).
//
// ONE KIBIBYTE: the answer ends `"cursor":{"previous":"…","next":"…"}}`, at
// most 326 bytes for a 64-byte message id (236 for the 30-byte ids OpenCode
// mints, testdata/opencode/<version>/context), plus a pretty-printer's tens,
// and none of a message's data URI, which can run to tens of mebibytes.
const openCodeCursorTail = 1 << 10

// openCodeCursor is the script's one reading of a list answer's cursor,
// `listcursor FILE`: the `next` of the answer in FILE, or nothing.
//
// READ AS TEXT, since a box may have no JSON parser: `next` is the last key of
// the whole answer, so it is matched anchored at the end (base64url holds no
// quote), and a `next` inside a message is never it. FROM THE END ALONE
// ([openCodeCursorTail]), whose first byte may fall inside a multi-byte
// character, so UNDER LC_ALL=C: under UTF-8, GNU sed's `.` matches no such cut
// character, and the match would print it before the cursor.
func openCodeCursor() string {
	return fmt.Sprintf(`listcursor() { LC_ALL=C tail -c %d "$1" | LC_ALL=C tr -d '\n' | LC_ALL=C sed -n 's/.*"next"[[:space:]]*:[[:space:]]*"\([A-Za-z0-9_-]*\)"[[:space:]]*}[[:space:]]*}[[:space:]]*$/\1/p'; }
`, openCodeCursorTail)
}

// openCodeStops is the script's two stops beside end ([endFunction], defined
// first): stop ends its server, unsync its login exporter
// ([openCodeExporter]).
//
// THE EXPORTER IS TOLD AGAIN until it goes (end's second operand): it is
// forked while the script traps SIGTERM, and a SIGTERM before the subshell
// clears that inherited handler is dropped (a stop right after the fork: 100
// of 100 lost under dash, 1 to 3 under bash). UNSYNC DROPS $xout ITSELF, since
// an exporter killed past end's bound runs no trap, and its file is a second
// copy of the login. THE SERVER IS TOLD ONCE: it is stopped long after it
// starts but for a signal in its first instant, and a repeat would interrupt
// its own shutdown.
func openCodeStops() string {
	return `stop() { end "$spid"; spid=; }
unsync() { end "$xpid" again; xpid=; [ -z "$xout" ] || rm -f "$xout"; }
`
}

// openCodeBounded is the script's one way to call its own server: `bounded TAG
// WAIT ENDS CMD...` runs CMD on /dev/null, ends it past WAIT seconds and,
// where ENDS is `e`, at an end request (starting none once one came), and
// answers CMD's status, or 124; `unbound` ends and reaps the call in flight.
// Unbounded, a server that never answered held the script until the job was
// stopped (crewlet#1088). A subshell starts the call, writes its pid, then its
// status, so a stop signals the call itself and "over" is a written status,
// never `kill -0`, which reads an unreaped child as running (crewlet#1164).
// Ended SIGTERM, then SIGKILL past [sandbox.JobStopGrace], polled each tenth
// of a second. ONE CALL A TAG: the main flow's go one at a time, the end's
// interrupt runs only while it waits on the harness, and the login loop's tags
// are its own.
func openCodeBounded(paths Paths, times openCodeTimes) string {
	// The subshell's TERM and HUP trap writes the status as a teardown's
	// signal takes it, so quit's unbound finds the call over at once. The
	// clock is read once in ten polls, since it moves in whole seconds. Calls
	// beside no server go unbounded: see [openCodeSessionGate].
	grace := int(times.stop / (100 * time.Millisecond))
	return fmt.Sprintf(`bcur=
bounded() { bt=$1 bw=$2 bx=$3; shift 3; bf=%[1]s"$bt"; if [ "$bx" = e ] && [ -n "$endasked" ]; then return 124; fi; rm -f "$bf.pid" "$bf.done"; ( trap 'echo 143 > "$bf.done"; exit 143' TERM HUP; "$@" < /dev/null & echo $! > "$bf.pid"; wait $!; echo $? > "$bf.done" ) & bs=$! bcur=$bt bn=0; bd=$(( $(%[2]s) + bw )); until [ -s "$bf.done" ]; do if { [ "$bx" = e ] && [ -n "$endasked" ]; } || { [ $((bn %% 10)) = 0 ] && [ "$(%[2]s)" -ge "$bd" ]; }; then unbound; return 124; fi; sleep 0.1; bn=$((bn + 1)); done; wait "$bs" 2> /dev/null || wait "$bs" 2> /dev/null; bc=$(cat "$bf.done"); rm -f "$bf.pid" "$bf.done"; bcur=; return "$bc"; }
unbound() { [ -n "$bcur" ] || return 0; bf=%[1]s"$bcur" bi=0; until [ -s "$bf.pid" ] || [ "$bi" -ge %[3]d ]; do sleep 0.1; bi=$((bi + 1)); done; bp=$(cat "$bf.pid" 2> /dev/null); [ -z "$bp" ] || [ -s "$bf.done" ] || kill "$bp" 2> /dev/null; bi=0; until [ -s "$bf.done" ] || [ "$bi" -ge %[3]d ]; do sleep 0.1; bi=$((bi + 1)); done; if [ ! -s "$bf.done" ] && [ -n "$bp" ]; then kill -9 "$bp" 2> /dev/null; bi=0; until [ -s "$bf.done" ] || [ "$bi" -ge %[3]d ]; do sleep 0.1; bi=$((bi + 1)); done; fi; [ ! -s "$bf.done" ] || wait "$bs" 2> /dev/null || wait "$bs" 2> /dev/null; rm -f "$bf.pid" "$bf.done"; bcur=; }
`, shellword.Quote(paths.WorkDir()+"/opencode-call-"), times.clock, grace)
}

// openCodeEndCalls is every call a run's script makes to its own server that
// no end request ends (every `bounded` call whose ENDS is `-`), keyed by its
// tag, with its kind's bound: each runs on an end request's way out. The
// cancels share one bound, and keep's export runs under its caller's tag.
func openCodeEndCalls(times openCodeTimes) map[string]time.Duration {
	return map[string]time.Duration{
		"interrupt": times.call, "inbox": times.call, "cancel": times.call, "report": times.call,
		"export": times.sessionCall, `"$kt"`: times.call,
	}
}

// openCodeEndPath is the most an OpenCode run's script spends after an end
// request reaches it while its harness runs: every call on the way at its
// bound ([openCodeEndCalls]) plus a grace, and a grace each for the harness,
// the login loop and the server it stops. The engine waits
// [sandbox.JobEndWait] for an ended job before it kills it and packs its
// session on demand, so the path is held under it
// (TestAnEndedRunsPathFitsTheEnginesWait, which also holds openCodeEndCalls to
// the rendered script's calls).
func openCodeEndPath(times openCodeTimes) time.Duration {
	var path time.Duration
	for _, call := range openCodeEndCalls(times) {
		path += call + times.stop
	}
	return path + 3*times.stop
}

// openCodeExporter is the login exporter: a subshell exporting the login into
// file and over the document every sync ([openCodeLoginSync]) until unsync
// stops it ([openCodeStops]). Its SIGTERM trap leaves nothing in the job's
// group: it ends the export in flight (unbound, further stops ignored, since
// unsync repeats them) and removes its file (`out`), else a second copy of the
// login beside the document. The sleep is ended with SIGKILL, never SIGTERM,
// which a just-forked sleep can lose to the inherited trap; a stop between
// forking the sleep and holding its pid (`w` set) is recorded (`s`) and acted
// on once `z` holds it; and the pid is forgotten (`z=`) right after its
// `wait`, since a `direct` box shares the host's pids and a later stop could
// signal one reused.
func openCodeExporter(sync time.Duration, file string) string {
	return fmt.Sprintf(`( z= w= s= out=%[2]s; trap 'if [ -n "$w" ]; then s=1; else trap "" TERM; unbound; [ -z "$z" ] || { kill -9 "$z" 2> /dev/null; wait "$z"; }; rm -f "$out"; exit 0; fi' TERM; while :; do w=1; sleep %[1]s & z=$!; w=; [ -z "$s" ] || { kill -9 "$z" 2> /dev/null; wait "$z"; exit 0; }; wait "$z"; z=; keep sync "$out" --server "$url"; done ) < /dev/null > /dev/null 2>&1`,
		strconv.FormatFloat(sync.Seconds(), 'f', -1, 64), file)
}

// openCodeSteerIDs reads, from a `session.inbox.list` answer on stdin, the
// ids of the steers the notes plugin admitted ([OpenCode.inboxID]), one a
// line, as text, a group so a redirect feeds its grep (a note's quotes are escaped there): the listing is
// `{"data":[{"id":"msg_…",…}]}` (measured on 2.3.2,
// testdata/opencode/2.3.2/inbox-list.json), and an id OpenCode minted is
// never of the plugin's shape.
const openCodeSteerIDs = `{ grep -o '"id":"msg_[0-9a-f]\{32\}"' | sed 's/^"id":"//; s/"$//' | sort -u; }`

// openCodeLeftoverSteers is the script's line that cancels every steer the
// run's notes plugin admitted to the session (sid) and OpenCode never
// delivered, once `opencode run` has exited and before the server stops. Such
// a note already waits on the run's record for the review or the next brief,
// but the session's inbox is durable: left pending, OpenCode puts it before a
// continued run's next prompt too, and the model reads it twice (measured on
// 2.3.2, across a server's restart). The listing goes to the answer file, as
// every bounded call's does ([openCodeAnswerFile]), its ids read by
// [openCodeSteerIDs]. Each call is bounded ([openCodeCallWait]) and the
// cancels share one bound more, so the tail spends two bounds at most
// ([openCodeEndPath]). A failed listing or cancel costs only the duplicate.
func openCodeLeftoverSteers(ans string, times openCodeTimes) string {
	return fmt.Sprintf(`if bounded inbox %[2]d - opencode api --server "$url" session.inbox.list --param sessionID="$sid" > %[1]s 2> /dev/null; then `+
		`kd=$(( $(%[3]s) + %[2]d )); `+
		`for id in $(%[4]s < %[1]s); do `+
		`kr=$(( kd - $(%[3]s) )); [ "$kr" -ge 1 ] || break; `+
		`bounded cancel "$kr" - opencode api --server "$url" session.inbox.cancel --param sessionID="$sid"`+
		` --param inboxID="$id" > /dev/null 2>&1; done; fi; rm -f %[1]s`,
		ans, secs(times.call), times.clock, openCodeSteerIDs)
}

// openCodeStaleSteers is the session gate's line that cancels, in a session
// the run continues in its box's own copy, every steer an earlier run's
// notes plugin admitted and OpenCode never delivered: a run killed mid-way
// never reached its own cancels ([openCodeLeftoverSteers]), and its pending
// steers survive in the box's copy (measured on 2.3.2), which OpenCode would
// hand this run ahead of its brief while the brief retells them. Through
// private servers and unbounded, as every call of the gate is
// ([openCodeSessionGate]); this run's own plugin has admitted nothing yet.
func openCodeStaleSteers() string {
	return `if [ "$k" = yes ] && inbox=$(opencode api --standalone session.inbox.list --param sessionID="$sid" < /dev/null 2> /dev/null); then ` +
		`for id in $(printf '%s\n' "$inbox" | ` + openCodeSteerIDs + `); do ` +
		`opencode api --standalone session.inbox.cancel --param sessionID="$sid" --param inboxID="$id" < /dev/null > /dev/null 2>&1; done; fi`
}

// openCodeReadiness is the script's test of one `mcp.list` answer, `$s`,
// against every server the run declares: present, and not `pending`; empty
// when it declares none. Matched as literal text spelled as the answer spells
// it ([jsonStringify]), since a box may have no JSON parser. An absent name is
// still connecting: the workspace's servers start when first asked about.
func openCodeReadiness(servers map[string]sandbox.MCPServer) string {
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	slices.Sort(names)
	var b strings.Builder
	for _, name := range names {
		present := `"name":` + jsonStringify(name)
		pending := present + `,"status":{"status":"pending"`
		fmt.Fprintf(&b, "  case \"$s\" in *%s*) case \"$s\" in *%s*) ready=0;; esac;; *) ready=0;; esac\n",
			shellword.Quote(present), shellword.Quote(pending))
	}
	return b.String()
}

// openCodeShadowCheck ends a run before it starts where the `mcp.list` answer
// ($s) lists a server whose tools OpenCode would name like those of a server
// the run offers directly ([sandbox.MCPServer.AlwaysLoaded]): the engine's
// own. OpenCode names a tool `<server>_<tool>`, writing every character
// outside `A-Za-z0-9_-` as `_`, a later registration replacing the earlier,
// so a server `crewlet_x` or `crewlet.x` would answer or
// lose the engine's `x_y`. A seat's server cannot be named so, but the image's
// or repository's configuration can declare one ([OpenCode.WriteConfig]). The
// character after the name is listed rather than ranged, so no locale changes
// the match, and every occurrence is tried, since the run's own server is
// among them.
func openCodeShadowCheck(servers map[string]sandbox.MCPServer) string {
	const kept = `ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-`
	var b strings.Builder
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		if !servers[name].AlwaysLoaded {
			continue
		}
		prefix := shellword.Quote(`"name":` + strings.TrimSuffix(jsonStringify(name), `"`))
		fmt.Fprintf(&b, "  r=$s\n")
		fmt.Fprintf(&b, "  while :; do case \"$r\" in *%s*) r=${r#*%s};; *) break;; esac\n", prefix, prefix)
		fmt.Fprintf(&b, "    case \"$r\" in [!\\\"%s]*) echo \"crewlet: this box's OpenCode configuration declares an MCP "+
			"server named \"%s\"${r%%%%\\\"*}, whose tools OpenCode would name like those of the run's own server "+
			"\"%s\" (\"%s\"_<tool>); rename it in the image's or the repository's OpenCode configuration\" >&2; exit 1;; esac\n",
			kept, shellword.Quote(name), shellword.Quote(name), shellword.Quote(name))
		fmt.Fprintf(&b, "  done\n")
	}
	return b.String()
}

// jsonStringify spells s as JavaScript's JSON.stringify does, as OpenCode
// prints a server's name, not as encoding/json does: Go escapes `&`, `<`, `>`,
// U+2028 and U+2029, which JSON.stringify leaves literal, so such a
// name never matched and the run waited out [openCodeMCPWait]. JSON.stringify
// escapes exactly `"`, `\` and the control characters below U+0020, five in
// short form, the rest as `\u` and four lowercase hex digits. A byte that is
// not UTF-8 is U+FFFD, written as itself, as encoding/json put it in the
// config OpenCode read.
func jsonStringify(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// WriteConfig writes the run's OpenCode 2 configuration and returns its path
// ([Paths.OpenCodeConfig]). The script hands its content to the server as
// OPENCODE_CONFIG_CONTENT, which ranks above a repository's opencode.json and
// .opencode/ where OPENCODE_CONFIG ranks below (core/src/config.ts), so the
// repository's own loads beneath it. 2.0's format: a directly offered server,
// a remote one without OAuth and a last-match-wins rule list exist only there.
// No credential is written; a key rides the run env. Every entry the run owns
// states `disabled: false`, since a document beneath may disable one of the
// same name. Values are files ([openCodeValueFiles]); the plugins come last in
// `plugins`, after any `-<id>` removal beneath. Every path is under the work
// dir ([openCodeReferable]). Written every run: the script refuses to run
// without it.
func (o OpenCode) WriteConfig(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, _ Session,
	paths Paths) (string, error) {
	// THE COMMANDS ITS MODEL RUNS START WITHOUT WHAT THE HARNESS ALONE READS:
	// the names it signs in by and its own variables, on every run
	// (opencode_toolenv.js).
	omitted, refused := toolEnvOmitted(req.SignInEnv, openCodeHarnessEnv...)
	if refused != nil {
		return "", refused
	}
	// So every run's configuration names a path under the work dir.
	if err := openCodeReferable(paths.WorkDir()); err != nil {
		return "", fmt.Errorf("executor: this box's home %q cannot be named in an OpenCode "+
			"run's configuration: %w; a local box's home is under providers.sandbox.local.state_dir, "+
			"which needs a path without such characters", paths.Home, err)
	}
	cfg := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		// Sharing is disabled: a run's transcript is company work, and
		// OpenCode's share feature publishes it to a URL.
		"share": "disabled",
		// ALLOW, STATED RATHER THAN INHERITED, in 2.0's ordered rule list:
		// `opencode run` is headless, so a gate left at `ask` refuses the call
		// and the run carries on without it, reading as an agent that would
		// not do the work. The box is the boundary, as for claude-code's
		// bypassPermissions ([ClaudeCode.Command]), and a vendor default moves
		// between releases (internal/providers/llm/harness). `question` alone
		// is denied: it asks a PERSON through a form a headless run cancels,
		// where the bridge's `ask_and_wait` parks the run on an answer. Every
		// document's rules are added in OpenCode's order, so these, last,
		// decide among them; an agent's own rules, a repository's included,
		// are read after them (2.3.2).
		"permissions": []map[string]string{
			{"action": "*", "resource": "*", "effect": "allow"},
			{"action": "question", "resource": "*", "effect": "deny"},
		},
		// A harness that updates itself mid-run swaps the tool under the brief
		// between launch and result, needs network a box may not have, and
		// does both while a seat's turn is suspended waiting on it.
		"update": "disable",
		// 2.0 keeps a git snapshot of the working directory at every step,
		// for an undo a headless run never offers — on a large checkout a
		// copy of its tree per step, for nothing.
		"snapshots": false,
		// THE AGENT THE RUN IS, stated rather than inherited, for the
		// reason the permissions are: the turn cap below is set on an
		// agent, and an image whose own config named another default would
		// run the brief on an agent the cap never reached.
		"default_agent": openCodeAgent,
		// NO BACKGROUND SUMMARY. Within half the room left of its point (at
		// most 60,000 tokens), 2.3.2 sends a second model request beside the
		// step's, a compaction summary it keeps for later and writes nowhere
		// the export holds, so its tokens are spent and never counted. Off,
		// none goes out (measured); a compaction runs as it always does.
		"compaction": map[string]any{"background": false},
	}
	agent := map[string]any{}
	// THE RUN'S TURN CAP, as the agent's `steps` (Claude Code's --max-turns,
	// [ClaudeCode.Command]): the one bound on an executor's work, already
	// reconciled into Limits. Absent at zero, which is uncapped: OpenCode
	// refuses a step count below one.
	if req.Limits.MaxTurns > 0 {
		agent["steps"] = req.Limits.MaxTurns
		agent["disabled"] = false
	}
	// THE ENTRY'S WEB DENIED IN THE PERMISSIONS TOO, the run's and its agent's,
	// beside the web tools' plugins taken out below: Code Mode's `execute`
	// fetches under the webfetch permission, and on 2.3.2 a repository's agent
	// rule allowing it outranked the run's own rules until the agent's denied
	// it as well.
	if req.WebDenied {
		denied := []map[string]string{
			{"action": "webfetch", "resource": "*", "effect": "deny"},
			{"action": "websearch", "resource": "*", "effect": "deny"},
		}
		cfg["permissions"] = append(cfg["permissions"].([]map[string]string), denied...)
		agent["permissions"] = denied
	}
	if len(agent) > 0 {
		cfg["agents"] = map[string]any{openCodeAgent: agent}
	}
	values := &openCodeValueFiles{dir: paths.OpenCodeValues()}
	wired := openCodeWiring(sandbox.Wiring{MCPServers: req.MCPServers, Steer: req.Steer,
		SkillLoads: req.SkillLoads}, req.Steer.Set(), len(req.Skills) > 0, values)
	// AND THE SAME VALUES UNDER THE JOB'S OTHER BRIDGE TOKEN, for a session
	// the script starts in place of the continued one
	// ([sandbox.RunRequest.FreshWiring], [openCodeSessionGate]): the same
	// references in the one order, written beside the first set, so one
	// configuration serves either session, and a wiring that would need two is
	// refused.
	var fresh *openCodeValueFiles
	if f := req.FreshWiring; f != nil {
		fresh = &openCodeValueFiles{dir: paths.OpenCodeValues()}
		rewired := openCodeWiring(*f, req.Steer.Set(), len(req.Skills) > 0, fresh)
		if !reflect.DeepEqual(rewired, wired) || len(fresh.files) != len(values.files) ||
			f.Steer.Set() != req.Steer.Set() || f.SkillLoads.Set() != req.SkillLoads.Set() {
			return "", errors.New("executor: the run's two wirings, for the session it continues and for " +
				"one of its own, are not the same configuration, so the run cannot be wired to whichever " +
				"session its box starts")
		}
	}
	if wired.servers != nil {
		cfg["mcp"] = map[string]any{"servers": wired.servers}
	}
	var plugins []any
	if req.Steer.Set() {
		if err := box.WriteFile(ctx, paths.OpenCodeSteerPlugin()+"/"+openCodeSteerEntry, openCodeSteerPlugin); err != nil {
			return "", fmt.Errorf("executor: writing OpenCode's notes plugin: %w", err)
		}
		plugins = append(plugins, map[string]any{
			"package": paths.OpenCodeSteerPlugin(),
			"options": map[string]string{"url": wired.steer},
		})
	}
	// THE RUN'S OWN SKILLS, in a directory its `skills` names (an entry
	// OpenCode adds to every other document's), and the plugin reporting each
	// load to the bridge, with the skill route as its one option
	// ([writeOpenCodeSkills], opencode_skills.js).
	if len(req.Skills) > 0 {
		if err := writeOpenCodeSkills(ctx, box, req.Skills, paths); err != nil {
			return "", err
		}
		cfg["skills"] = []string{paths.OpenCodeSkills()}
		if err := box.WriteFile(ctx, paths.OpenCodeSkillsPlugin()+"/"+openCodeSteerEntry, openCodeSkillsPlugin); err != nil {
			return "", fmt.Errorf("executor: writing OpenCode's skill-load plugin: %w", err)
		}
		plugins = append(plugins, map[string]any{
			"package": paths.OpenCodeSkillsPlugin(),
			"options": map[string]string{"url": wired.skills},
		})
	}
	// THE ENTRY'S WEB TOOLS OFF where it denies them, as its runs on the engine
	// host have none: their plugins taken out, which on 2.3.2 left webfetch
	// and websearch unregistered for the run and every subagent, an agent of
	// the repository's own allowing them included. A repository's `plugins`
	// naming them again does not bring them back.
	if req.WebDenied {
		plugins = append(plugins, "-opencode.tool.webfetch", "-opencode.tool.websearch")
	}
	// AND ON EVERY RUN THE PLUGIN THAT KEEPS omitted OUT OF THE COMMANDS ITS
	// MODEL RUNS (opencode_toolenv.js), last.
	if err := box.WriteFile(ctx, paths.OpenCodeToolEnvPlugin()+"/"+openCodeSteerEntry, openCodeToolEnvPlugin); err != nil {
		return "", fmt.Errorf("executor: writing OpenCode's tool-environment plugin: %w", err)
	}
	cfg["plugins"] = append(plugins, map[string]any{
		"package": paths.OpenCodeToolEnvPlugin(),
		"options": map[string]any{"names": omitted},
	})
	for _, v := range values.files {
		if err := box.WriteFile(ctx, v.path, []byte(v.value)); err != nil {
			return "", fmt.Errorf("executor: writing a value of OpenCode's configuration: %w", err)
		}
	}
	if fresh != nil {
		for _, v := range fresh.files {
			at := paths.OpenCodeValuesFresh() + strings.TrimPrefix(v.path, paths.OpenCodeValues())
			if err := box.WriteFile(ctx, at, []byte(v.value)); err != nil {
				return "", fmt.Errorf("executor: writing a value of OpenCode's configuration for a "+
					"session of the run's own: %w", err)
			}
		}
	}
	// THE SYSTEM TEXT AS AN INSTRUCTION FILE: OpenCode has no flag adding to
	// its system prompt, but appends each instruction file (a global or
	// project AGENTS.md, each `instructions` entry) after its agent's own
	// prompt. An `instructions` entry because entries are ADDITIVE across
	// documents, where writing the global AGENTS.md would overwrite an
	// operator's, in a directory the image decides; rewritten every run, so a
	// reused box carries no old entry. ABSOLUTE, since a relative entry is
	// matched walking up from the working directory. TOLD OF NOTES on a run
	// wired to them ([withNoteInstruction]).
	system := req.System
	if req.Steer.Set() {
		system = withNoteInstruction(system, o.NoteInstruction())
	}
	if system != "" {
		if err := box.WriteFile(ctx, paths.System(), []byte(system)); err != nil {
			return "", fmt.Errorf("executor: writing the system prompt: %w", err)
		}
		cfg["instructions"] = []string{paths.System()}
	}
	blob, err := openCodeJSON(cfg)
	if err != nil {
		return "", err
	}
	if err := box.WriteFile(ctx, paths.OpenCodeConfig(), blob); err != nil {
		return "", fmt.Errorf("executor: writing OpenCode's configuration: %w", err)
	}
	return paths.OpenCodeConfig(), nil
}

// openCodeJSON is v as the configuration's text: indented, and with `&`, `<`
// and `>` written as themselves. OpenCode finds a `{file:<path>}` reference in
// the TEXT, before it parses it ([openCodeReferable]), so a path encoding/json
// escaped for HTML — `&` as `\u0026` — would name a file that is not there.
func openCodeJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("executor: rendering OpenCode's configuration: %w", err)
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// openCodeAgent is the OpenCode agent a run runs as: `build`, the harness's
// default primary agent, with every tool. Its config, `agents.<name>`, carries
// the run's turn cap as `steps`.
const openCodeAgent = "build"

// openCodeValueFiles is the values a run's configuration names by reference:
// each MCP server's url, header value and environment value and each plugin's
// route, one file apiece under [Paths.OpenCodeValues], named `v1`, `v2`, … in
// the configuration's order, so the same request writes the same files.
// OpenCode replaces `{file:<path>}` with the file's content, TRIMMED of edge
// whitespace and JSON-escaped (core/src/config/variable.ts), which only an
// environment value can lose. A missing file reads as empty, warned of only in
// OpenCode's own log, so the files are written before the configuration.
type openCodeValueFiles struct {
	dir   string
	files []openCodeValueFile
}

// openCodeValueFile is one value and the file it is written to.
type openCodeValueFile struct{ path, value string }

// ref is the reference naming value's file, which it records for writing.
// Every file is the directory and `v<n>` in it, and [OpenCode.WriteConfig]
// already held the work directory above it to what a reference can carry.
func (v *openCodeValueFiles) ref(value string) string {
	path := v.dir + "/v" + strconv.Itoa(len(v.files)+1)
	v.files = append(v.files, openCodeValueFile{path: path, value: value})
	return "{file:" + path + "}"
}

// openCodeWired is the part of a run's configuration that names the job's bridge
// endpoint, every value a reference into its value files: its MCP servers
// ([openCodeMCP]), nil for none, and the references to the notes route and the
// skill-load route its two plugins post to, "" for a plugin it has not.
type openCodeWired struct {
	servers       map[string]any
	steer, skills string
}

// openCodeWiring is w as a run's configuration names it ([openCodeWired]), its
// values recorded in values in the configuration's one order: servers by name,
// then the notes route where steer, then the skill-load route where skills.
// The same order for the run's own wiring and the one under the job's other
// bridge token ([sandbox.RunRequest.FreshWiring]), so a reference of either
// names the same file and the script can hand either set to one configuration.
func openCodeWiring(w sandbox.Wiring, steer, skills bool, values *openCodeValueFiles) openCodeWired {
	var out openCodeWired
	if len(w.MCPServers) > 0 {
		out.servers = openCodeMCP(w.MCPServers, values)
	}
	if steer {
		out.steer = values.ref(w.Steer.URL)
	}
	if skills {
		out.skills = values.ref(w.SkillLoads.URL)
	}
	return out
}

// openCodeReferable is nil where `{file:<path>}` names path, and any path
// below it, to OpenCode as itself, and otherwise says why not. OpenCode
// replaces every `{env:…}` and then `{file:…}` in the configuration's text
// before parsing it, and resolves a relative path against the checkout, so a
// path is named only where it is absolute, holds no brace, and is carried by
// the JSON text as itself: no `"`, `\`, control character, U+2028 or U+2029,
// which the text holds escaped, and nothing that is not UTF-8, which it holds
// as U+FFFD.
func openCodeReferable(path string) error {
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("the path %q is not absolute", path)
	}
	if strings.ContainsAny(path, "{}") {
		return fmt.Errorf("the path %q holds a brace, which opens or ends a reference", path)
	}
	ref := "{file:" + path + "}"
	if text, err := openCodeJSON(ref); err != nil || string(text) != `"`+ref+`"` {
		return fmt.Errorf("the path %q holds a character the configuration's text cannot "+
			"carry as itself", path)
	}
	return nil
}

// openCodeMCP translates the generic launch specs into OpenCode 2's schema,
// every value a reference to a value file ([openCodeValueFiles]). THE ENGINE'S
// OWN TOOLS ARE OFFERED DIRECTLY (`codemode: false`,
// [sandbox.MCPServer.AlwaysLoaded]); every other server is left to Code Mode,
// OpenCode's default, one `execute` tool searching the rest, each call still
// its own MCP call the bridge decides and records. So submit_work and
// ask_and_wait are `crewlet_<tool>` on every model. A REMOTE SERVER NEVER
// STARTS OAUTH: a headless run has nobody to complete it, and the bridge's
// token is in its URL path. EVERY SERVER IS ON ([OpenCode.WriteConfig]).
// `timeout.execution`, in milliseconds, ONLY WHERE THE ENGINE SETS A WAIT
// ([sandbox.MCPServer.CallTimeout]): OpenCode cancels a call past it whatever
// progress flows, and unset waits twelve hours.
func openCodeMCP(servers map[string]sandbox.MCPServer, values *openCodeValueFiles) map[string]any {
	out := make(map[string]any, len(servers))
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		s := servers[name]
		entry := map[string]any{"disabled": false}
		if s.AlwaysLoaded {
			entry["codemode"] = false
		}
		if ms := waitMillis(s.CallTimeout); ms > 0 {
			entry["timeout"] = map[string]any{"execution": ms}
		}
		if s.Transport == sandbox.TransportHTTP {
			entry["type"] = "remote"
			entry["url"] = values.ref(s.URL)
			entry["oauth"] = false
			if len(s.Headers) > 0 {
				entry["headers"] = openCodeRefs(s.Headers, values)
			}
			out[name] = entry
			continue
		}
		// ONE ARGV, not a command and its arguments: this harness takes the
		// whole invocation as a single array.
		cmd := make([]string, 0, len(s.Args)+1)
		if s.Command != "" {
			cmd = append(cmd, s.Command)
		}
		for _, a := range s.Args {
			if a != "" {
				cmd = append(cmd, a)
			}
		}
		entry["type"] = "local"
		entry["command"] = cmd
		if len(s.Env) > 0 {
			entry["environment"] = openCodeRefs(s.Env, values)
		}
		out[name] = entry
	}
	return out
}

// openCodeRefs is a header or environment map with every value a reference to
// its file, taken in key order.
func openCodeRefs(m map[string]string, values *openCodeValueFiles) map[string]string {
	out := make(map[string]string, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		out[k] = values.ref(m[k])
	}
	return out
}

// Output is where OpenCode writes: its stdout IS its event stream, from which
// its result is derived, and the stream is the result file the clear removes,
// so a reused box never shows a job another's. Its brief arrives on stdin
// ([Output.BriefOnStdin]). The done marker is the end of a run, as for every
// harness: 2.0 against the script's own server exits when its session does,
// and the script after it. A stream's end is never read as the end, which
// would collect a run before its script added the usage and wrote back the
// login.
func (OpenCode) Output(paths Paths) Output {
	return Output{Stdout: paths.Result(), Events: true, BriefOnStdin: true}
}

// routing answers [router] with nothing: an OpenCode run's calls go where its
// configuration's provider says, the provider its model names being the
// entry's route ([harnessprofile.Profile.ModelNamesProvider]), and any
// credential-named variable signs it in (env_sign_in). Measured on 2.3.2 with
// no network (its catalogue fetched first): ANTHROPIC_BASE_URL,
// OPENAI_BASE_URL and OPENROUTER_BASE_URL at a stand-in, its anthropic, openai
// and openrouter models still dialled api.anthropic.com, api.openai.com and
// openrouter.ai.
func (OpenCode) routing() []routeVar { return nil }

// Pins is nothing: OpenCode compacts near its model's own window, and its run's
// layout is written into its configuration ([OpenCode.WriteConfig]).
func (OpenCode) Pins(context.Context, sandbox.RunRequest, Paths) []Pinned { return nil }

// Events is a new decoder for one stream, read whole or followed ([Decoder]).
func (OpenCode) Events() Decoder { return &openCodeEvents{} }

// Parse decodes a whole stream already in hand: the result of a run whose
// result IS its stream.
//
// The run's tokens and price come from the session's usage report the script
// appends ([openCodeUsageEvent]), and stay ZERO where there is none, never
// estimated: a reader cannot tell an invented number in the spend rollup.
func (OpenCode) Parse(stdout []byte) sandbox.Result {
	return decodeAll(&openCodeEvents{}, stdout)
}

// openCodeEvent is the part of one stream event this runner reads, and no
// more: the event carries the tool's whole state, its output included, which a
// map of every field would hold several times over. DECODED LOOSELY, a field
// whose type a release changed skipped rather than the event lost
// ([decodeOpenCodeEvent]), since a box runs whatever the operator's image has.
type openCodeEvent struct {
	Type  string          `json:"type"`
	Tool  string          `json:"tool"`
	Name  string          `json:"name"`
	Error json.RawMessage `json:"error"`
	Part  struct {
		// MessageID is the message the part belongs to: each step of a
		// run is a message of its own.
		MessageID string          `json:"messageID"`
		Tool      string          `json:"tool"`
		Text      string          `json:"text"`
		Reason    string          `json:"reason"`
		Input     json.RawMessage `json:"input"`
		State     struct {
			Status string          `json:"status"`
			Error  string          `json:"error"`
			Output string          `json:"output"`
			Input  json.RawMessage `json:"input"`
			// Time is when the call started and ended, in Unix ms.
			Time struct {
				Start int64 `json:"start"`
				End   int64 `json:"end"`
			} `json:"time"`
		} `json:"state"`
	} `json:"part"`
	// Report is the session's usage report, on the one line of the
	// script's own type ([openCodeUsageEvent]).
	Report *openCodeReport `json:"report"`
}

// openCodeReport is the part of `opencode api session.get` the usage line
// carries that a result reads: the session's id, the price OpenCode put on it,
// and its token totals across every step.
//
// `output` is the VISIBLE text alone: 2.0 subtracts the reasoning tokens from
// the provider's output count and reports them beside it, and every provider
// bills them as output — so the result's output is the two added back. `input`
// is the prompt NOT served from the cache, the cache's two shares reported
// beside it, so the result's whole prompt count is the three added.
type openCodeReport struct {
	Data struct {
		ID     string  `json:"id"`
		Cost   float64 `json:"cost"`
		Tokens struct {
			Input     int `json:"input"`
			Output    int `json:"output"`
			Reasoning int `json:"reasoning"`
			Cache     struct {
				Read  int `json:"read"`
				Write int `json:"write"`
			} `json:"cache"`
		} `json:"tokens"`
	} `json:"data"`
}

// openCodeInput is the part of a tool call's input a transcript line names.
type openCodeInput struct {
	Command     string `json:"command"`
	FilePath    string `json:"filePath"`
	Path        string `json:"path"`
	Pattern     string `json:"pattern"`
	Description string `json:"description"`
}

// decodeOpenCodeEvent reads one line as an event, or reports that it is not
// one: a line that is not a JSON object, or a partial one — the stream is
// flushed live and a read can end mid-line, and the next read has it whole.
//
// A TYPE MISMATCH IS NOT A LOST EVENT: encoding/json skips a field whose value
// does not fit its Go type, decodes the rest and reports the first mismatch,
// so the mismatch is ignored and the event kept.
func decodeOpenCodeEvent(line []byte) (openCodeEvent, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return openCodeEvent{}, false
	}
	var ev openCodeEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		var mismatch *json.UnmarshalTypeError
		if !errors.As(err, &mismatch) {
			return openCodeEvent{}, false
		}
	}
	return ev, true
}

// openCodeEvents decodes one stream into the answer and a readable transcript,
// read whole or followed across reads ([Decoder]).
//
// THE ANSWER IS THE LAST MESSAGE'S TEXT, never every step's joined: each step
// is a message of its own (`part.messageID`), and a run handed a note after
// answering answers again in a later step, the stream showing no event for the
// note. Joined, the result held the superseded answer and any narration. The
// transcript keeps every step's text, and the result prefers the run's report
// anyway ([Paths.Findings]).
type openCodeEvents struct {
	// answers is the text of the latest message that produced any, in
	// order, and message that message's id. A text event that names no
	// message belongs to the latest one: only a message that names itself
	// and differs ends it, so a stream that names none is read whole.
	// answerBytes is what answers hold joined, and answerDropped what the
	// message wrote past [openCodeEvents.bound], counted rather than kept.
	answers       []string
	message       string
	answerBytes   int
	answerDropped int
	// readBound is the most of the answer, or of the plain output, kept:
	// [sandbox.MaxFileBytes], what a run's report is read to whatever the
	// harness wrote it as, where zero; a test lowers it.
	readBound  int
	transcript transcriptLines
	// seq numbers the run's tool calls ([activityline]).
	seq     int
	errText string
	// overflowed is whether the last error the stream carried is OpenCode's
	// own for a conversation past its model's context.
	overflowed bool
	sawEvent   bool
	sawLine    bool
	// ended is whether the newest step ended the run ([openCodeEvents.Ended]).
	ended bool

	// usage is the session's usage report, when the script printed one, and
	// usageBefore the one a continued run's script printed before the run
	// took the session up ([openCodeUsageBefore]).
	usage, usageBefore *openCodeReport

	// plain is the output while no event has been seen: a run that printed its
	// answer as text, not the event stream, whose whole output is then the
	// answer (its exit status, not this decoder, says whether it failed). Held
	// to [openCodeEvents.bound], what lies past it counted rather than kept.
	plain        strings.Builder
	plainDropped int64

	// following is a decoder read through Entries, which keeps neither
	// the answer nor the plain output a Result would need: a live reading
	// holds it for as long as somebody watches.
	following bool

	// named is every message the run's events name (`part.messageID`), so the
	// measure of the session's newest request says whether the run made it
	// ([openCodeEvents.contextLine]): kept only while no live reading follows the
	// stream, which never asks it and holds the decoder as long as somebody
	// watches.
	named map[string]bool

	// measured is the measure of the session's newest request as the pages
	// of its message list read it, and why there is none where there is none;
	// read whether the reading is over, a figure found or a reason to stop
	// met; and pages how many pages it has read ([openCodeEvents.contextLine]).
	measured contextRead
	read     bool
	pages    int
}

// Entries implements [Decoder].
func (d *openCodeEvents) Entries(end bool) []string {
	d.following = true
	d.answers = nil
	d.named = nil
	d.plain.Reset()
	return d.transcript.take(end)
}

func (d *openCodeEvents) Line(line []byte) {
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	d.sawLine = true
	ev, ok := decodeOpenCodeEvent(line)
	if !ok {
		if !d.sawEvent {
			d.keepPlain(line)
		}
		return
	}
	if !d.sawEvent {
		// The first event: whatever plain text came before it was a banner,
		// not an answer.
		d.sawEvent = true
		d.plain.Reset()
		d.plainDropped = 0
	}
	if id := ev.Part.MessageID; id != "" && !d.following {
		if d.named == nil {
			d.named = map[string]bool{}
		}
		d.named[id] = true
	}
	switch ev.Type {
	case "step_start":
		d.ended = false
	case "step_finish":
		d.ended = ev.Part.Reason != openCodeStepCalls
	case "text":
		if chunk := strings.TrimSpace(ev.Part.Text); chunk != "" {
			if !d.following {
				d.answer(ev.Part.MessageID, chunk)
			}
			d.transcript.add(chunk)
		}
	case "tool_use":
		d.seq++
		for _, l := range toolLines(d.seq, ev) {
			d.transcript.add(l)
		}
	case "error":
		d.ended = true
		d.errText = errorText(ev.Error)
		d.overflowed = openCodeContextOverflow(ev.Error)
		d.transcript.add(activityline.Words(activityline.TagError, d.errText))
	case openCodeUsageEvent:
		if ev.Report != nil {
			d.usage = ev.Report
		}
	case openCodeUsageBefore:
		if ev.Report != nil {
			d.usageBefore = ev.Report
		}
	case openCodeContextEvent:
		d.contextLine(line)
	}
}

// contextLine reads one of the lines a run's script adds of its session's message
// list ([openCodeContext]), in order: the first message of the pages that
// measured a prompt is the figure ([measuredPrompt]), the run's own where its
// events named it, and the reading is over; a marker of a page past the
// pages' budget, and a line or a page that does not decode, end it with none,
// since the page it stands for may hold the figure and a later page's would
// then be an older request's. A live reading reads none of it.
//
// A PAGE A MESSAGE AT A TIME ([eachElement]), each decoded by its keys exactly
// as written ([threadDecoding]) into the few values the measure reads
// ([openCodeMeasuredWire]), and never its content.
func (d *openCodeEvents) contextLine(line []byte) {
	if d.following || d.read {
		return
	}
	var w struct {
		Page    json.RawMessage `json:"page"`
		Skipped json.RawMessage `json:"skipped"`
	}
	if err := wireDecode(line, &w); err != nil {
		d.endContext(contextRead{why: "a line of the session's message list the run's script printed does " +
			"not decode: " + err.Error(), failed: true})
		return
	}
	if w.Skipped != nil {
		d.endContext(contextRead{why: fmt.Sprintf("a page of the session's message list of %s was past the %s "+
			"the pages may put into the run's stream, so it and every older one went unread",
			humanSize(looseInt(w.Skipped)), humanSize(openCodeContextBytes))})
		return
	}
	d.pages++
	n, own, found, err := d.page(w.Page)
	switch {
	case err != nil:
		d.endContext(contextRead{why: fmt.Sprintf("page %d of the session's message list does not decode: %v",
			d.pages, err), failed: true})
	case found:
		d.endContext(contextRead{measure: sandbox.RequestMeasure{Tokens: n, Own: own}})
	}
}

// errFigureFound stops the walk of a page at its first message that measured a
// prompt ([openCodeEvents.page]).
var errFigureFound = errors.New("the page's figure is found")

// page is the first message of one page of the session's message list, raw,
// that measured a prompt: its figure, whether the run's events named it, and
// whether one did; an error where the page, or a message of it before that one,
// does not decode.
func (d *openCodeEvents) page(raw json.RawMessage) (int64, bool, bool, error) {
	if !wireObject(raw) {
		return 0, false, false, fmt.Errorf("the page is %s, not an object", wireKind(raw))
	}
	var page struct {
		Data json.RawMessage `json:"data"`
	}
	if err := wireDecode(raw, &page); err != nil {
		return 0, false, false, err
	}
	var (
		figure int64
		own    bool
	)
	err := eachElement(page.Data, func(element json.RawMessage, _ int) error {
		if !wireObject(element) {
			return fmt.Errorf("a message is %s, not an object", wireKind(element))
		}
		var w openCodeMeasuredWire
		if err := wireDecode(element, &w); err != nil {
			return err
		}
		m, id, err := w.message()
		if err != nil {
			return err
		}
		if n, measured := measuredPrompt(m); measured {
			figure, own = n, d.named[id]
			return errFigureFound
		}
		return nil
	})
	if errors.Is(err, errFigureFound) {
		return figure, own, true, nil
	}
	if err != nil {
		return 0, false, false, fmt.Errorf("data: %w", err)
	}
	return 0, false, false, nil
}

// endContext ends the reading of the session's message list with read.
func (d *openCodeEvents) endContext(read contextRead) { d.measured, d.read = read, true }

// request is the measure of the session's newest request as the pages its run's
// script printed read it, and why there is none where there is none
// ([contextReader]).
func (d *openCodeEvents) request() contextRead {
	switch {
	case d.read:
		return d.measured
	case d.pages == 0:
		return contextRead{why: "the run's stream holds no page of the session's message list: its job was " +
			"ended on request, its script could not ask its server, or the session holds no assistant message"}
	}
	return contextRead{why: fmt.Sprintf("none of the %d page(s) of the session's message list the run's "+
		"script asked holds a settled assistant message that measured a prompt: it asks one message a page, "+
		"newest first, at most %d pages, and no page past one whose message did not fail", d.pages,
		openCodeContextPages)}
}

// answer adds one text event's chunk to the answer: after the latest message's
// text where the event is that message's, or names none; in its place where it
// names a later one.
// A later message lets the earlier one's chunks go. A chunk that would take
// the message past the bound, its line break counted, is counted rather than
// kept, and so is every later chunk of the message.
func (d *openCodeEvents) answer(message, chunk string) {
	if message != "" {
		if d.message != "" && message != d.message {
			d.answers, d.answerBytes, d.answerDropped = nil, 0, 0
		}
		d.message = message
	}
	if d.answerDropped > 0 || d.answerBytes+len(chunk)+1 > d.bound() {
		d.answerDropped += len(chunk) + 1
		return
	}
	d.answers = append(d.answers, chunk)
	d.answerBytes += len(chunk) + 1
}

// bound is [openCodeEvents.readBound], or [sandbox.MaxFileBytes] where unset.
func (d *openCodeEvents) bound() int {
	if d.readBound > 0 {
		return d.readBound
	}
	return sandbox.MaxFileBytes
}

// settledHead is text, whole lines that a bound cut after, held to what its
// redaction settles ([redact.Settled]), so a reader that redacts it reads an
// opening of what the whole text redacts to; and the bytes it held back.
func settledHead(text string) (string, int) {
	kept := text[:redact.Settled(text)]
	return kept, len(text) - len(kept)
}

// withNote is text with note on a line after it, or note alone.
func withNote(text, note string) string {
	if text == "" {
		return note
	}
	return text + "\n" + note
}

func (d *openCodeEvents) keepPlain(line []byte) {
	if d.following {
		return
	}
	if d.plainDropped > 0 || d.plain.Len()+len(line)+1 > d.bound() {
		d.plainDropped += int64(len(line) + 1)
		return
	}
	d.plain.Write(line)
	d.plain.WriteByte('\n')
}

// Skipped counts a line past the stream's bound, and ends the reading of the
// session's message list where it has found no figure: the line may be a page
// of it, which may hold the figure, and a later page's would then be an older
// request's ([openCodeEvents.contextLine]).
func (d *openCodeEvents) Skipped(n int64, bound int) {
	d.sawLine = true
	if !d.read {
		d.endContext(contextRead{why: fmt.Sprintf("a line of %s, past the %s a line of the stream is read "+
			"within, came before any page that measured a prompt", humanSize(n), humanSize(int64(bound)))})
	}
	if !d.sawEvent {
		d.plainDropped += n
		return
	}
	d.transcript.skip(n, bound)
}

// openCodeStepCalls is the reason a step ends on that another follows: the
// model called tools, whose results the next step answers.
const openCodeStepCalls = "tool-calls"

// Ended implements [Decoder]: a step that ended for any reason but tool calls,
// with no step begun after it, or an `error` event, which `opencode run`
// prints as it gives up. Measured on 2.3.2: a finished run ends on a `stop`
// step, a refused request and an interrupted session on an `error`, and a
// client killed mid-turn on a `tool-calls` step or on nothing.
func (d *openCodeEvents) Ended() bool { return d.ended }

func (d *openCodeEvents) Result() sandbox.Result {
	if !d.sawLine {
		return sandbox.Result{Error: "the harness produced no output"}
	}
	if !d.sawEvent {
		// Output that is not the event stream: the whole of it is the
		// answer.
		text := d.plain.String()
		if d.plainDropped > 0 {
			kept, held := settledHead(text)
			text = withNote(strings.TrimSpace(kept), fmt.Sprintf("(%s more of the output not kept: past the %s "+
				"a run's plain output is read to)", humanSize(d.plainDropped+int64(held)), humanSize(int64(d.bound()))))
		}
		text = strings.TrimSpace(text)
		return sandbox.Result{Text: text, Success: true, DeliveredRefs: deliveredRefs(text)}
	}
	body := strings.TrimSpace(strings.Join(d.answers, "\n"))
	success := body != "" && d.errText == ""
	if d.answerDropped > 0 {
		var lines strings.Builder
		for _, chunk := range d.answers {
			lines.WriteString(chunk)
			lines.WriteByte('\n')
		}
		kept, held := settledHead(lines.String())
		body = withNote(strings.TrimSpace(kept), fmt.Sprintf("(%s more of the answer not kept: past the %s "+
			"a run's answer is read to)", humanSize(int64(d.answerDropped+held)), humanSize(int64(d.bound()))))
		success = d.errText == ""
	}
	res := sandbox.Result{
		Text:          body,
		Success:       success,
		DeliveredRefs: deliveredRefs(body),
	}
	res.Transcript, res.TranscriptElidedLines, res.TranscriptElidedBytes = d.transcript.result()
	if u := d.usage; u != nil {
		tokens := u.Data.Tokens
		res.InputTokens = tokens.Input + tokens.Cache.Read + tokens.Cache.Write
		res.OutputTokens = tokens.Output + tokens.Reasoning
		res.CacheReadTokens = tokens.Cache.Read
		res.CacheWriteTokens = tokens.Cache.Write
		// AS REPORTED, a negative price included, like the counts: a provider
		// OpenCode cannot price reports zero, which a result's zero already
		// means, and a negative one is the coordinator's to floor and SAY
		// ([sandbox.Result]'s floor, and its sandbox_usage_negative warning).
		res.CostUSD = u.Data.Cost
		res.SessionID = u.Data.ID
		// THE RUN'S OWN SPEND, for a run that continued a session: the
		// report is the session's lifetime, so what it said before the run
		// took the session up is taken off ([openCodeUsageBefore]). A
		// difference below zero is handed on as any negative is, for the
		// coordinator to floor and say.
		if b := d.usageBefore; b != nil {
			was := b.Data.Tokens
			res.InputTokens -= was.Input + was.Cache.Read + was.Cache.Write
			res.OutputTokens -= was.Output + was.Reasoning
			res.CacheReadTokens -= was.Cache.Read
			res.CacheWriteTokens -= was.Cache.Write
			res.CostUSD -= b.Data.Cost
		}
	}
	// NO TURN COUNT ([sandbox.Result.Turns]): the session report carries none,
	// and OpenCode 2 has printed `step_start` for the first step and rarely
	// for the later ones, so a count off them would be right only by luck. None
	// reported counts as the one call the engine asked for.
	if !success {
		res.Error = d.errText
		if res.Error == "" {
			res.Error = "the harness produced no answer"
		}
		res.ContextExhausted = d.overflowed
	}
	return res
}

// openCodeOverflowError is the name OpenCode gives the error of a conversation
// past its model's context (read from the 2.2.6 bundle).
const openCodeOverflowError = "ContextOverflowError"

// openCodeContextOverflow reports whether an error event's payload is that
// error, BY ITS NAME: the message beside it quotes the vendor's own words,
// which a run's tool output can repeat.
func openCodeContextOverflow(raw json.RawMessage) bool {
	var named struct {
		Name string `json:"name"`
	}
	return json.Unmarshal(raw, &named) == nil && named.Name == openCodeOverflowError
}

// toolLines is one tool event's lines ([activityline]): call, input and result
// at once, as OpenCode reports a call once it has ended. Never the tool's output,
// only a failed call's first line.
func toolLines(seq int, ev openCodeEvent) []string {
	name := firstNonBlank(ev.Part.Tool, ev.Tool, ev.Name, "tool")
	raw := ev.Part.State.Input
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		raw = ev.Part.Input
	}
	// Loose, as the event is: a retyped field leaves the others named.
	var input openCodeInput
	_ = json.Unmarshal(raw, &input)
	subject := firstNonBlank(input.Command, input.FilePath, input.Path, input.Pattern, input.Description)
	if subject == "" {
		subject = compactInput(raw)
	}
	lines := activityline.Call(seq, name, subject)

	state := ev.Part.State
	var took activityline.Took
	if t := state.Time; t.Start > 0 && t.End >= t.Start {
		took = activityline.Took{D: time.Duration(t.End-t.Start) * time.Millisecond, Known: true}
	}
	failure := firstNonBlank(state.Error)
	if failure == "" && state.Status == "error" {
		failure = firstNonBlank(state.Output, "failed")
	}
	switch {
	case failure != "":
		lines = append(lines, activityline.Failed(seq, failure, took))
	case state.Status == "completed":
		lines = append(lines, activityline.OK(seq, took))
	}
	return lines
}

// compactInput is a tool's input as compact JSON, for a tool with no subject
// field of its own; "" for an empty object, no object, or one past
// [compactInputLimit].
func compactInput(raw json.RawMessage) string {
	if !wireObject(raw) || len(raw) > compactInputLimit {
		return ""
	}
	var out bytes.Buffer
	if json.Compact(&out, raw) != nil || out.String() == "{}" {
		return ""
	}
	return out.String()
}

// compactInputLimit bounds the input compacted for a subject: the subject keeps
// [activityline.InputLimit] of it, so a larger input is a body, not an argument.
const compactInputLimit = 8 * activityline.InputLimit

func errorText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	// An object: its JSON, compacted.
	var compacted bytes.Buffer
	if json.Compact(&compacted, raw) == nil {
		return compacted.String()
	}
	return string(raw)
}

// firstNonBlank is the first value that is not blank.
func firstNonBlank(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// firstLine is s's first line as a preview, redacted whole before it is cut
// ([activityline.Preview]).
func firstLine(s string, limit int) string { return activityline.Preview(s, limit) }

// openCodeChargeBytes is the fewest bytes OpenCode's own estimate counts as a
// token: round(UTF-16 length / 4) of any text, and a string's UTF-8 bytes are
// never fewer than its UTF-16 units.
const openCodeChargeBytes sandbox.Charge = 4

// threadTraits is what OpenCode's main-thread read settles: its export, taken
// as its script ends, may lack what a cut-off copy holds, so such a copy is
// not counted; its plugin's serves confirm themselves, so it asks no receipts.
func (OpenCode) threadTraits() sandbox.ThreadTraits { return sandbox.ThreadTraits{} }

// sessionTraits is how a relaunch judges an OpenCode session: at the point its
// box says, its calls keyed by their place in the round's log, a brief at
// [openCodeChargeBytes].
func (OpenCode) sessionTraits() sandbox.SessionTraits {
	return sandbox.SessionTraits{FirstRequest: sandbox.FirstRequestBox, CallKeying: sandbox.CallKeyingByLogPlace,
		Charge: openCodeChargeBytes}
}

// notesOffWhy is what switches OpenCode's reading of notes off.
func (OpenCode) notesOffWhy() string {
	return "a managed policy that denies the run's notes plugin (integration.use), or a plugin OpenCode " +
		"failed to load, which its own log names"
}

// artifactFiles is the files an OpenCode run leaves in its box of its own.
func (OpenCode) artifactFiles(p Paths) []string {
	return []string{p.OpenCodeConfig(), p.OpenCodeRenewExport()}
}

// artifactDirs is the directories an OpenCode run leaves in its box of its
// own.
func (OpenCode) artifactDirs(p Paths) []string {
	return []string{p.OpenCodeValues(), p.OpenCodeValuesFresh(), p.OpenCodeSteerPlugin(), p.OpenCodeSkills(),
		p.OpenCodeSkillsPlugin(), p.OpenCodeToolEnvPlugin(), p.OpenCodeConfigDir()}
}
