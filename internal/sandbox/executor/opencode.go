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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/textcut"
)

// OpenCodeName is this runner's config name.
const OpenCodeName = "opencode"

// transcriptDetailLimit caps one transcript line's echoed command or path.
//
// A bare "[tool] bash" is useless when a run fails — you cannot see WHAT ran —
// but a heredoc echoed whole would blow up the phase event. 160 characters is
// a readable command line.
const transcriptDetailLimit = 160

// openCodeUsageEvent is the type of the one line the run's script adds to the
// event stream: the session's own usage report, asked for once the run has
// ended. A type OpenCode never prints, so nothing the harness emits is read as it.
const openCodeUsageEvent = "crewlet.usage"

// openCodeUsageBefore is the type of the line a run that CONTINUES a session
// adds before its `opencode run`: the same report, asked for before the run
// takes the session up. OpenCode's report is the session's lifetime totals —
// every step it ever ran, and every total an import restored — so a continued
// run's own spend is the report after it less the report before it, both read
// off one session in one box by the run's own script. A type OpenCode never
// prints, for [openCodeUsageEvent]'s reason.
const openCodeUsageBefore = "crewlet.usage_before"

// openCodeListenWait bounds how long the run waits for its private server to
// say where it listens. Measured on OpenCode 2.0.32 it does so in about a
// second, most of it the runtime starting; sixty is room for a box whose disk
// or CPU is contended, and short of holding a seat for a server that is never
// coming up.
const openCodeListenWait = 60 * time.Second

// openCodeMCPWait bounds how long the run waits for the MCP servers it declares
// to finish connecting before it sends the brief. OpenCode's own catalog
// timeout — how long it gives one server to list its tools — defaults to thirty
// seconds, and the servers connect concurrently, so a server still pending past
// twice that is stuck rather than slow: the run starts without it, as OpenCode
// would, and says so on stderr.
const openCodeMCPWait = 60 * time.Second

// openCodeLoginSync is how often a run that was given a login exports it over
// the box's copy while it works — see [OpenCode.Command].
//
// THE INTERVAL IS THE LOSS WINDOW. A login OpenCode refreshed exists only in
// its database until it is exported, and every production teardown of a run
// still in flight is a SIGKILL of the job's group ([sandbox.Local.Kill]) or a
// `docker rm -f`, in which nothing in the box runs again — so a refresh is
// lost exactly when the run is killed less than one interval after it. An
// OAuth access token lives an hour or more, so a refresh — the only thing that
// changes the login — comes at most about hourly, and thirty seconds leaves
// under one refresh in a hundred inside the window. What it costs is one
// export through the run's own server per interval, measured at 0.14 s on
// OpenCode 2.0.32: under half a percent of one core for as long as the run
// lasts, where ten seconds would triple that to narrow a window a kill
// mid-run already makes rare.
const openCodeLoginSync = 30 * time.Second

// openCodeStopWait bounds how long the script waits for a process of its own
// — its server, its login exporter — to go once told to, before it kills it.
// Bounded, because the done marker waits on the script: a server that ignored
// SIGTERM would otherwise hold the run open for ever. Measured on 2.0.32, the
// server goes in a tenth of a second, and in three with a stdio MCP server
// still connecting, which it stops itself; five is room above that.
const openCodeStopWait = 5 * time.Second

// OpenCode drives OpenCode 2 headless — the `@opencode/cli` package. 1.x cannot
// run this script: it has no `serve` it can be pointed at, refuses the 2.0
// config format, and keeps its login where 2.0 does not look; the script says
// so rather than leaving its usage text as the failure.
//
// Provider-agnostic, which is why it configures its own LLM rather than
// reading the env: pointing it at a seat's model means declaring a custom
// provider with an explicit base URL and the exact model id, because
// OpenCode otherwise resolves a bare "<provider>/<model>" against a catalogue
// AND the vendor's default endpoint — so a custom gateway plus an unlisted
// model either fails to resolve or silently hits the wrong host.
//
// A PERSON'S NOTES REACH THE RUN AT ITS NEXT STEP, through a plugin of the
// run's own that its server loads (opencode_steer.js, written by
// [OpenCode.WriteConfig]): while the run's session works, it polls the bridge's
// notes route ([sandbox.SteerWiring.URL]) and admits each note into the
// session's inbox as a steer, which OpenCode hands the model after the tool
// results of the step in flight — or after a final step that called no tool —
// as a user message, and reports delivered, which the plugin confirms to the
// engine. Each with `resume: false`, so a note that arrives once the session
// has settled starts nothing the run would not see, and waits for the run's
// review. Measured against a stub model API: on OpenCode 2.0.32 and 2.1.4, a
// note admitted during a tool call or during the final model call is read in
// the same execution; on 2.1.4, one admitted after it settles starts nothing.
// What switches it off — a managed policy that denies the plugin
// (`integration.use`) — leaves a run that succeeds without asking for its notes
// once, which the collection says (internal/sandbox, steer_reader_never_ran).
type OpenCode struct{}

var _ Harness = OpenCode{}

// NewOpenCode returns the runner.
func NewOpenCode() *Runner { return New(OpenCode{}) }

// Name is the harness's key in the runner registry.
func (OpenCode) Name() string { return OpenCodeName }

// Resumes says OpenCode continues a session it reported: naming a session its
// database holds is the whole of continuing it ([OpenCode.Session],
// [openCodeSessionGate]).
func (OpenCode) Resumes() bool { return true }

// NoteInstruction is the standing instruction about notes an OpenCode run is
// given: it takes a person's notes between its steps, through the plugin its
// configuration loads — see [OpenCode].
func (OpenCode) NoteInstruction() string { return OpenCodeSteerInstruction }

// Command is the run: a short shell script, because OpenCode 2 has to be
// driven through a server of the run's own rather than in one command.
//
// A PRIVATE SERVER, STARTED FIRST AND WAITED ON. `opencode run` alone either
// attaches to a background service — detached from the run, on a port compiled
// into the binary and shared by every box on a host, which outlives the run
// and keeps the environment, and so the credentials, of whichever run started
// it — or, with `--standalone`, starts a server inside the run and sends the
// brief at once. That second shape is what the engine's runs on its own host
// use, and it is wrong here: it sends the brief before the run's MCP servers
// have connected, and
// 2.0 fixes a step's tools as the step starts, so the first step of every
// run — measured on 2.0.32, the first step of every run with a server
// declared — was offered none of the seat's bridged tools, which only
// appeared from the step after. So the script starts `opencode serve` on a
// loopback port of its own, waits until every MCP server the run declares
// has left `pending` (`api mcp.list`, bounded by [openCodeMCPWait]), runs the
// brief against that server, and stops it.
//
// THE RUN'S CONFIGURATION, ON ITS SERVER ALONE. The script reads the file
// [OpenCode.WriteConfig] wrote ([Paths.OpenCodeConfig]) and hands its content
// to `opencode serve` as OPENCODE_CONFIG_CONTENT, which OpenCode ranks above a
// repository's own opencode.json and .opencode/; a run with no such file is
// refused before a login is imported or a server started. A run wired to its
// turn's notes ([sandbox.RunRequest.Steer]) hands the server
// CREWLET_STEER_SESSION too — the session the run works in, which the notes
// plugin admits notes to and follows ([OpenCode]) — on the same command, so
// nothing else the script runs is handed it.
//
// THE BRIEF ON STDIN ([Output.BriefOnStdin]), because OpenCode re-quotes an
// argv prompt; the session NAMED, by the id the runner minted or the one the
// run continues ([OpenCode.Session]), so the usage report asked for afterwards
// is this run's; a TITLE given, which stops 2.0 making a second billed model
// call to write one.
//
// THE SESSION, SETTLED BEFORE THE SERVER STARTS ([openCodeSessionGate]): a run
// that continues one continues the copy the box holds, imports an export of it
// carried into the box where the box holds no copy, or one that does not hold
// that export, and starts a new one in its place, saying why, where neither — and
// records which ([Paths.SessionRecord]). Once the run has ended, the session
// is exported to be carried to another box ([openCodeCapture]).
//
// THE SERVER'S PASSWORD IS ITS OWN. `opencode serve` takes OPENCODE_PASSWORD
// or OPENCODE_SERVER_PASSWORD from its environment when either is set, and
// then prints no password at all — so a box whose environment carried one
// (a seat's role.sandbox.env, the image's own ENV, a profile the login shell
// sources) waited out the whole listen bound for a line that never came, and
// failed saying the server had not listened. The script unsets both before
// anything starts, waits for the server to print where it listens AND its
// password, and hands that password to the run's own clients as
// OPENCODE_PASSWORD — measured on 2.0.32, the variable a client reads first,
// OPENCODE_SERVER_PASSWORD only where it is absent. The server was started
// without it, so nothing the agent runs inherits it.
//
// THE LOGIN, WHERE THE BOX WAS GIVEN ONE: a local box is seeded with the
// entry's exported login ([Paths.CredentialDocument]), and the script imports
// it before the server starts. A token the run refreshes then lives only in
// OpenCode's database, and the box's teardown writes back only what the
// document holds — so the script exports the database over the document
// WHILE THE RUN WORKS, every [openCodeLoginSync], as well as once more when it
// ends: every production teardown of a run in flight kills the job's group
// outright, and an export left for the end would be lost with it. A SIGTERM,
// SIGINT or SIGHUP to the script exports once more before it goes, with the
// conventional status. No export holding no login ever replaces the
// document. A box given none — a remote one — has nothing exported, and signs
// in with the run environment's key.
//
// THE USAGE, from the session rather than the stream: `run --format json`
// drops the final step's counts, while the session the run worked in holds
// the whole total, so the script prints it as one last line of its own type
// ([openCodeUsageEvent]) once the run has ended.
//
// AND, FOR A RUN WIRED TO ITS NOTES, NO STEER LEFT PENDING: once the run has
// ended, every note the plugin admitted and OpenCode never delivered is
// cancelled in the session, before the server stops ([openCodeLeftoverSteers]).
func (OpenCode) Command(req sandbox.RunRequest, session Session, paths Paths, _ string) string {
	return "sh -c " + shellQuote(openCodeScript(req, session, paths, openCodeTimes{
		listen: openCodeListenWait, mcp: openCodeMCPWait, sync: openCodeLoginSync, stop: openCodeStopWait,
	}))
}

// openCodeTimes is how long a run's script waits for its server to listen
// ([openCodeListenWait]) and for its MCP servers to connect
// ([openCodeMCPWait]), how often it exports a login ([openCodeLoginSync]), and
// how long it gives a process of its own to go ([openCodeStopWait]): a value
// rather than the constants read inline, so a case about a wait runs it at a
// length that case can tell apart — a bound run out at one a test can afford,
// or a stop stretched to one no prompt end could be mistaken for.
type openCodeTimes struct {
	listen, mcp, sync, stop time.Duration
}

// newOpenCodeSession mints the id a run's session is created under: 32 lowercase
// hexadecimal characters from crypto/rand, which [OpenCode.Session] prefixes
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
// ([sandbox.RunRequest.Resume], `ses_` added where the id came without it), or
// a new one — and, for a run that continues one, the new one it starts instead
// where its box holds that one neither way. OpenCode CONTINUES a session it
// already holds, and `run --session` on an id it does not hold CREATES one
// under that id, so a run never names an id it was not handed or minted.
func (OpenCode) Session(req sandbox.RunRequest) Session {
	if req.Resume != "" {
		return Session{ID: "ses_" + strings.TrimPrefix(req.Resume, "ses_"), Continues: true,
			Fresh: "ses_" + newOpenCodeSession()}
	}
	return Session{ID: "ses_" + newOpenCodeSession()}
}

// openCodeCarryFloor is the oldest OpenCode release a box continues a session
// carried from another box on, and the oldest whose session it continues:
// 2.0.32, the release the run's script, its session export and import, and the
// import's refusal of an id it holds were measured and read on — the import
// validates an export written by an earlier 2.x release, and nothing older is
// a release this runner drives.
const openCodeCarryFloor = "2.0.32"

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

// Continues reports whether a session written by OpenCode release writer can
// be continued in a box whose OpenCode is release box ([carrier]): both at or
// after [openCodeCarryFloor], and never a box older than the writer.
func (OpenCode) Continues(box, writer string) (bool, string) {
	switch {
	case !releaseAtLeast(box, openCodeCarryFloor):
		return false, fmt.Sprintf("this box runs OpenCode %s, older than %s, the oldest that "+
			"continues a session carried from another box", box, openCodeCarryFloor)
	case !releaseAtLeast(writer, openCodeCarryFloor):
		return false, fmt.Sprintf("the session was written by OpenCode %s, older than %s, the "+
			"oldest whose session another box continues", writer, openCodeCarryFloor)
	case !releaseAtLeast(box, writer):
		return false, carriedOlder("OpenCode", box, writer)
	}
	return true, ""
}

// openCodeScript is the run's script for one session, exporting a login the
// box was given every sync — see [OpenCode.Command].
//
// Every command but the run itself reads /dev/null rather than the brief the
// script's stdin is, and writes nowhere but a file or /dev/null: its stdout is
// the event stream.
//
// THE LOGIN'S EXPORTS ARE SERIALISED, because three of them can write the
// document — the loop while the run works, the last one when it ends, the one
// a signal asks for — and two renames racing would leave whichever landed
// last, which need not be the newest. Each writes a temporary file of its own
// and only the loop runs while the run does; the loop is stopped, and waited
// for, before either of the others starts. Stopped by SIGTERM, it finishes the
// command it is in and exits before the next — so an export it had begun is
// either moved over the document or dropped, never moved after the last one.
//
// AN EXPORT REPLACES THE DOCUMENT ONLY WHERE IT HOLDS A LOGIN AND DIFFERS:
// printed whole — OpenCode exited cleanly — a JSON array, and not the empty
// one, which is what an export of a database that dropped a credential it could
// not refresh prints (measured on 2.0.32: `[]`). Such an export is no reason to
// replace the login the box came with, and the teardown would otherwise write
// it back over the shared one.
//
// THE SESSION IS DECIDED BEFORE THE SERVER STARTS ([openCodeSessionGate]),
// because the server is handed it — a run wired to its notes names it to the
// notes plugin the server loads — and the record of it is written before the
// run ([Paths.SessionRecord]). Once the run has ended, and before its server
// stops, the session is exported to be carried to another box
// ([openCodeCapture]).
func openCodeScript(req sandbox.RunRequest, session Session, paths Paths, times openCodeTimes) string {
	doc := shellQuote(paths.CredentialDocument())
	next := shellQuote(paths.CredentialDocument() + ".next")
	loop := shellQuote(paths.CredentialDocument() + ".sync")
	srv := shellQuote(paths.WorkDir() + "/opencode-server")
	vrr := shellQuote(paths.WorkDir() + "/opencode-version")
	cfg := shellQuote(paths.OpenCodeConfig())

	run := []string{"opencode", "run", "--server", `"$url"`, "--format", "json",
		"--title", "crewlet", "--session", `"$sid"`}
	// THE ENTRY'S MODEL AS WRITTEN. It is the string every run on the host on
	// the same providers.llm entry hands `opencode run --model`, which
	// OpenCode splits at its first slash into a provider and a model — so
	// `anthropic/claude-sonnet-5` or `openrouter/anthropic/claude-sonnet-5`
	// means one thing in both places, signed in by the login or the key of
	// the provider it names. A launch passes over every other harness's entry,
	// so none reaches here.
	if req.Model != "" {
		run = append(run, "--model", shellQuote(req.Model))
	}

	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	// ABSENT AND FAILING ARE TWO ANSWERS. One sentence for any failure of
	// `--version` told a run whose opencode was installed but could not start
	// (not executable, busy, crashing) to install it, and dropped the words it
	// failed with, though that stderr is the run's transcript when nothing else
	// was written.
	line(`command -v opencode > /dev/null || { echo 'crewlet: no opencode on the box PATH' >&2; exit 127; }`)
	// 1.x prints a bare version, 2.x `opencode v2.…`.
	line(`v=$(opencode --version < /dev/null 2> %s) || { st=$?; echo "crewlet: opencode is on the box PATH, but opencode --version failed with exit $st:" >&2; cat %s >&2; rm -f %s; exit 1; }`, vrr, vrr, vrr)
	line(`rm -f %s`, vrr)
	line(`case "$v" in [0-9]*) echo "crewlet: this box runs OpenCode $v; the opencode runner drives OpenCode 2 (the @opencode/cli package) — install it in the box image" >&2; exit 1;; esac`)
	line(`unset OPENCODE_PASSWORD OPENCODE_SERVER_PASSWORD`)
	// THE RUN'S CONFIGURATION, read before anything starts, and refused where
	// there is none: handed nothing, the server would run on whatever
	// documents the image and the checkout hold, with neither the run's
	// posture nor its servers. A shell variable, never exported: it is handed
	// to the server alone.
	line(`config=$(cat %s) && [ -n "$config" ] || { echo 'crewlet: the run has no OpenCode configuration at '%s >&2; exit 1; }`,
		cfg, cfg)
	line(`imported= spid= xpid= xout=%s`, loop)
	b.WriteString(openCodeStops(times.stop))
	// holds: the file is a whole, non-empty JSON array.
	line(`holds() { t=$(tr -d '[:space:]' < "$1") || return 1; case "$t" in '['*']') [ "$t" != '[]' ];; *) return 1;; esac; }`)
	// keep FILE FLAGS...: one export into FILE, moved over the document only
	// where it holds a login and differs from what the document holds.
	line(`keep() { f=$1; shift; if opencode auth export "$@" < /dev/null > "$f" 2> /dev/null && holds "$f" && [ "$(cat "$f")" != "$(cat %s)" ]; then mv -f "$f" %s; else rm -f "$f"; fi; }`, doc, doc)
	// quit: a signal's end — the loop stopped, the login exported once more
	// beside a server that is going with the group, then the server stopped.
	// The EXIT trap goes with the others, because quit has done everything it
	// would: left set, the `exit` ran its stops a second time, over processes
	// quit had already stopped — under busybox ash, before end tested for an
	// empty pid, ten seconds of waiting for nothing.
	line(`quit() { trap - EXIT TERM INT HUP; unsync; [ -z "$imported" ] || keep %s --standalone; stop; exit "$1"; }`, next)
	line(`trap 'unsync; stop' EXIT`)
	line(`trap 'quit 143' TERM; trap 'quit 130' INT; trap 'quit 129' HUP`)
	line(`if [ -s %s ]; then`, doc)
	line(`  opencode auth import --standalone < %s > /dev/null || { echo 'crewlet: OpenCode refused the login this box was given — sign the engine in again with crewlet llm login' >&2; exit 1; }`, doc)
	line(`  imported=1`)
	line(`fi`)
	b.WriteString(openCodeSessionGate(session, paths))

	// ON THE SERVER ALONE: it runs the session, so it is the one process
	// that reads the run's configuration — and, for a run wired to its notes,
	// the session the notes plugin it loads admits them to. The run's client
	// and its `api` calls are answered by it, and the login's import and
	// export, which run before it or beside it, are handed none of it.
	steered := ""
	if req.Steer.Set() {
		steered = `CREWLET_STEER_SESSION="$sid" `
	}
	line(`OPENCODE_CONFIG_CONTENT="$config" %sopencode serve --hostname 127.0.0.1 --port 0 < /dev/null > %s 2>&1 &`,
		steered, srv)
	line(`spid=$!`)
	line(`deadline=$(( $(date +%%s) + %d ))`, int(times.listen.Seconds()))
	line(`until grep -q '^server listening on ' %s 2> /dev/null && grep -q '^server password ' %s 2> /dev/null; do`, srv, srv)
	line(`  if ! kill -0 "$spid" 2> /dev/null; then echo 'crewlet: opencode serve exited before it listened:' >&2; cat %s >&2; exit 1; fi`, srv)
	line(`  if [ "$(date +%%s)" -ge "$deadline" ]; then`)
	line(`    if grep -q '^server listening on ' %s 2> /dev/null; then echo 'crewlet: opencode serve listened but printed no server password within %s' >&2; else echo 'crewlet: opencode serve did not say where it listens within %s' >&2; fi`,
		srv, times.listen, times.listen)
	line(`    exit 1`)
	line(`  fi`)
	line(`  sleep 0.2`)
	line(`done`)
	line(`url=$(sed -n 's/^server listening on //p' %s)`, srv)
	line(`OPENCODE_PASSWORD=$(sed -n 's/^server password //p' %s); export OPENCODE_PASSWORD`, srv)
	line(`rm -f %s`, srv)
	line(`if [ -n "$imported" ]; then`)
	line(`  %s &`, openCodeExporter(times.sync, loop))
	line(`  xpid=$!`)
	line(`fi`)

	if checks := openCodeReadiness(req.MCPServers); checks != "" {
		line(`deadline=$(( $(date +%%s) + %d ))`, int(times.mcp.Seconds()))
		line(`while :; do`)
		line(`  s=$(opencode api --server "$url" mcp.list < /dev/null 2> /dev/null)`)
		line(`  ready=1`)
		b.WriteString(checks)
		line(`  [ "$ready" = 1 ] && break`)
		line(`  if [ "$(date +%%s)" -ge "$deadline" ]; then echo 'crewlet: an MCP server was still connecting after %s; the run starts without it' >&2; break; fi`, times.mcp)
		line(`  sleep 0.5`)
		line(`done`)
	}

	// WHAT THE CONTINUED SESSION HAD SPENT, asked of the server that runs it
	// just before the run takes it up ([openCodeUsageBefore]) — after an
	// import, so the totals it restored are among what is taken off. A run
	// that starts a session of its own needs none: its session's totals begin
	// at the run.
	if session.Continues {
		line(`if [ "$k" = yes ]; then before=$(opencode api --server "$url" session.get --param sessionID="$sid" < /dev/null 2> /dev/null) && case "$before" in '{'*) printf '{"type":"%s","report":%%s}\n' "$before";; esac; fi`,
			openCodeUsageBefore)
	}
	// THE RECORD, before the run, so a run killed before it reported anything
	// still says which session it took up, and why it began again where it
	// did. The release last, as the harness printed it ([parseSessionRecord]).
	line(`printf 'id %%s\ncontinued %%s\nwhy %%s\nrelease %%s\n' "$sid" "$k" "$w" "$v" > %s`,
		shellQuote(paths.SessionRecord()))
	line(`%s`, strings.Join(run, " "))
	line(`code=$?`)
	// NOTHING THE PLUGIN ADMITTED IS LEFT IN THE SESSION once the run has
	// ended: see [openCodeLeftoverSteers].
	if req.Steer.Set() {
		line(`%s`, openCodeLeftoverSteers())
	}
	line(`report=$(opencode api --server "$url" session.get --param sessionID="$sid" < /dev/null 2> /dev/null) && case "$report" in '{'*) printf '{"type":"%s","report":%%s}\n' "$report";; esac`,
		openCodeUsageEvent)
	b.WriteString(openCodeCapture(paths))
	line(`unsync`)
	line(`[ -z "$imported" ] || keep %s --server "$url"`, next)
	line(`stop; trap - EXIT TERM INT HUP`)
	line(`exit "$code"`)
	return b.String()
}

// openCodeSessionGate is the part of a run's script that decides, before the
// run's server starts, the session the run works in, as sid, whether that is
// the one it was asked to continue, as k, and — where it was asked and could
// not — why, as w.
//
// A RUN THAT STARTS A SESSION works in the id minted for it.
//
// A RUN THAT CONTINUES ONE continues the copy the box's database HOLDS — the
// session answers `session.get` under its id, as it does in the box its last
// run ran in — unless an export of the session was carried in
// ([Paths.SessionCarried]) that the copy does not hold: one whose digest is
// not among the box's for the session ([Paths.SessionDigests], and carry.go
// for why). That export, and one carried into a box that holds no copy, is
// IMPORTED: unpacked in the staging directory, the box's own copy deleted
// (`opencode session delete`, which takes its child sessions with it) only
// once the export unpacked, and handed to `opencode session import --directory
// "$PWD"`, which files it under the directory `opencode run` works in (its
// `$PWD`, as run reads it), keeps its id, and restores its totals; and its
// digest is kept. The import has to SAY it imported that session — `Imported
// session: <id>` — because it answers an id the database already holds with
// "Session already exists" and success, and imports nothing; and `run
// --session` on an id OpenCode does not hold creates an empty session under
// it, which the brief would then be answered in as if it remembered. Restored
// or not, the archive is removed: nothing reads it again. Where the session is
// neither held nor imported, the run starts the session minted for it in its
// place, and says why on its error stream and in its record, the words
// OpenCode answered with put on one line.
//
// ALL ASKED OF A PRIVATE SERVER OF THEIR OWN (`--standalone`), as the login's
// import is, before the run's server starts — never the background service a
// bare command reaches, which every box on a host shares and which keeps the
// environment of whichever run started it — because the run's server is
// handed the session it runs, which a run wired to its notes names to the
// notes plugin it loads, and so the session is settled before that server
// exists.
//
// ITS INSTRUCTION STATE STARTS AGAIN. OpenCode keeps, per session, the
// instructions it last told the model: the system prompt is rendered from the
// state's baseline, and a model whose instruction file changed since is handed
// an "instruction update" message in the conversation instead
// (instructions.mdx). The state is a row of its own, and a session with none
// has its instructions read as INITIAL — the baseline, rendered into the
// system prompt, with no update (core/src/session/instruction-state.ts at
// 2.0.32 and 2.2.5). An import writes the session and its messages and no
// such row (core/src/session/transfer.ts at the same two) — so the session's
// first request in the box it was carried into reads the run's instructions
// as its first. So the run's standing instructions stay its system prompt,
// written to the instruction file on every run as on a fresh one: a session
// carried into a new box each turn is told that turn's, and no turn's
// instructions pile up in the conversation as updates after the first's. In
// the box that already holds the session — the same turn's runs — the state
// is kept, and an instruction file that changed between them is handed over
// once, as an update, which is what OpenCode does for any session whose
// instructions change under it.
//
// ONLY THE SESSION ITSELF, never its children: a subagent's session is a child
// of the run's, and its work reaches the run's own conversation as the result
// of the task that ran it, which the parent's export carries. A child's import
// needs its parent first (transfer.ts), and a continued run starts its own
// tasks; a task that would take up a child the box no longer holds starts
// another.
func openCodeSessionGate(session Session, paths Paths) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	q := shellQuote
	if !session.Continues {
		line(`sid=%s k=no w=`, q(session.ID))
		return b.String()
	}
	staged := paths.SessionStaging()
	carried := q(paths.SessionCarried())
	digests := q(paths.SessionDigests())
	export := q(staged + "/export.json")
	line(`sid=%s k=yes w= h= d= own=`, q(session.ID))
	line(`held=$(opencode api --standalone session.get --param sessionID="$sid" < /dev/null 2> /dev/null) || held=`)
	line(`case "$held" in *%s*) h=1;; esac`, q(`"id":"`+session.ID+`"`))
	// THE EXPORT'S DIGEST, where one was carried in and the box can take it:
	// the box's copy is kept unless the export is one it does not hold, which
	// then replaces it.
	line(`if [ -s %s ]; then d=$(cksum < %s 2> /dev/null) || d=; fi`, carried, carried)
	line(`if [ -n "$h" ] && [ -n "$d" ] && ! grep -qxF "$sid $d" %s 2> /dev/null; then h= own=1; fi`, digests)
	line(`if [ -z "$h" ]; then`)
	line(`  if [ ! -s %s ]; then w='it is not in this box, and no export of it was carried here'`, carried)
	line(`  elif ! command -v gzip > /dev/null 2>&1; then w='this box has no gzip to unpack its export with'`)
	line(`  else`)
	line(`    rm -rf %s`, q(staged))
	line(`    if ! { mkdir -p %s && gzip -dc < %s > %s 2> /dev/null; }; then w='its export could not be unpacked'`,
		q(staged), carried, export)
	line(`    elif [ -n "$own" ] && ! out=$(opencode session delete --standalone "$sid" < /dev/null 2>&1); then w="OpenCode did not delete the box's own copy, which the export carried here replaces: $(printf '%%s' "$out" | tr '\n' ' ')"`)
	line(`    else`)
	line(`      out=$(opencode session import --standalone --directory "$PWD" %s < /dev/null 2>&1)`, export)
	line(`      case "$out" in *%s*) [ -z "$d" ] || printf '%%s %%s\n' "$sid" "$d" >> %s;; *) w="OpenCode did not import it: $(printf '%%s' "$out" | tr '\n' ' ')";; esac`,
		q("Imported session: "+session.ID), digests)
	line(`    fi`)
	line(`    rm -rf %s`, q(staged))
	line(`  fi`)
	line(`fi`)
	line(`rm -f %s`, carried)
	line(`if [ -n "$w" ]; then sid=%s k=no; printf '%%s%%s\n' %s "$w" >&2; fi`, q(session.Fresh),
		q("crewlet: this run was asked to continue the OpenCode session "+session.ID+
			", and starts a new session, "+session.Fresh+", instead: "))
	return b.String()
}

// openCodeCapture is the part of a run's script that exports the session the
// run worked in (sid) once the run has ended — after the steers it left are
// cancelled, and before its server stops — to be continued in another box:
// `opencode session export` through the run's own server, which holds the
// password the script read, gzipped in the staging directory and moved over
// [Paths.SessionCapture] whole, and only where the export said it succeeded;
// its digest kept as one the box's copy of the session holds
// ([Paths.SessionDigests]).
//
// NEVER THE DATABASE: OpenCode keeps its logins in the same file as its
// sessions, and a file carried to another box would carry the login with it —
// to a remote box, which is never handed one. The export is the session and its
// settled messages, and nothing of the login.
//
// A BOX WITHOUT GZIP, or an export that failed, is said on the error stream,
// and the run's own status is never the export's.
func openCodeCapture(paths Paths) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	q := shellQuote
	staged := paths.SessionStaging()
	line(`if command -v gzip > /dev/null 2>&1; then`)
	line(`  rm -rf %s; if mkdir -p %s && xs=$( { { opencode session export --server "$url" "$sid" < /dev/null 2> %s; echo $? >&4; } | gzip -c > %s; } 4>&1 ) && [ "$xs" = 0 ] && mv -f %s %s; then d=$(cksum < %s 2> /dev/null) && printf '%%s %%s\n' "$sid" "$d" >> %s; else echo "crewlet: the session $sid was not exported to be continued in another box: $(cat %s 2> /dev/null)" >&2; fi`,
		q(staged), q(staged), q(staged+"/error"), q(staged+"/capture"), q(staged+"/capture"),
		q(paths.SessionCapture()), q(paths.SessionCapture()), q(paths.SessionDigests()), q(staged+"/error"))
	line(`  rm -rf %s`, q(staged))
	line(`else`)
	line(`  echo 'crewlet: this box has no gzip, so the session was not exported to be continued in another box' >&2`)
	line(`fi`)
	return b.String()
}

// openCodeStops is the script's three stops: end, which stops one process of
// the script's own, bounded by stop ([openCodeStopWait]) and killing it only
// if it is still there at the end of the wait; and stop and unsync, which end
// its server and its login exporter ([openCodeExporter]).
//
// NO PROCESS IS NOTHING TO STOP, and it is TESTED rather than left to `kill`
// refusing an empty operand: dash and bash refuse it, but busybox ash — the
// sh of an Alpine image, which OpenCode 2's musl builds are for — answers both
// `kill ""` and `kill -0 ""` with success and signals nothing, so an end with
// no process waited out the whole bound: five seconds at the end of every run
// given no login, where the exporter never started, and ten after a signal.
//
// THE EXPORTER IS TOLD AGAIN until it goes (end's second operand), because
// the first SIGTERM can be lost. The exporter is a subshell forked while the
// script traps SIGTERM, so it starts with the script's handler for it; dash
// clears that handler as the subshell begins, and a SIGTERM the handler took
// before then is dropped as untrapped. So a stop sent before the subshell has
// run (a signal to the job as its server comes up, or a run that ends at once
// on a host too busy to have run the subshell) reached an exporter that could
// not hear it. Measured with a stop sent straight after the fork, one
// exporter at a time: all 100 of 100 lost under dash and 1 to 3 of 100 under
// bash, each waiting out the whole bound and then killing the exporter with
// its sleep still running, which left that sleep in the job's group for an
// interval. The loss is certain only while the fork has not run, so under
// load, where it often runs first, fewer are lost. A repeat goes only to a pid
// `kill -0` still finds, as every probe of end's does, so it shares end's one
// residual: a pid the script reaped during the tenth of a second before the
// probe, which the system could hand out again only by wrapping its whole pid
// space within that tenth.
//
// UNSYNC DROPS THE EXPORTER'S FILE ITSELF ($xout) once end has returned,
// whatever ended the exporter: one that heard the stop drops it as it goes
// ([openCodeExporter]), but one still in an export when end's bound ran out
// is killed with SIGKILL, which runs no trap, and its file stayed beside the
// document holding a second copy of the login.
//
// THE SERVER IS TOLD ONCE. It is stopped long after it starts on every path
// but a signal in its first instant, where a lost SIGTERM costs the bound and
// then the kill, and a repeat would tell a server that is already shutting
// down, stopping its own MCP servers, to stop again.
func openCodeStops(stop time.Duration) string {
	return fmt.Sprintf(`end() { [ -n "$1" ] || return 0; kill "$1" 2> /dev/null || return 0; i=0; while kill -0 "$1" 2> /dev/null && [ "$i" -lt %d ]; do sleep 0.1; i=$((i+1)); [ -z "$2" ] || { kill -0 "$1" 2> /dev/null && kill "$1" 2> /dev/null; }; done; kill -0 "$1" 2> /dev/null && kill -9 "$1" 2> /dev/null; return 0; }
stop() { end "$spid"; spid=; }
unsync() { end "$xpid" again; xpid=; [ -z "$xout" ] || rm -f "$xout"; }
`, int(stop/(100*time.Millisecond)))
}

// openCodeExporter is the login exporter: a subshell that exports the login
// into file and over the document every sync ([openCodeLoginSync]) while the
// run works, until [openCodeStops]' unsync stops it.
//
// A subshell of its own, with a SIGTERM trap of its own: a SIGTERM there ends
// its sleep at once, and an export it is in once that export has finished —
// see the serialisation in [openCodeScript]. The sleep is reaped before it
// goes, so a stopped exporter leaves nothing in the job's group. And its pid
// is FORGOTTEN the command after the sleep is reaped (`z=`, right after
// `wait`): a SIGTERM that arrives during an export, up to an interval later,
// would otherwise signal a pid the system is free to have handed to another
// process by then, and a `direct` box shares the host's pids. It shares end's
// residual ([openCodeStops]): a stop whose trap runs between the reap and
// `z=` signals that pid, which the system could have handed out again only by
// wrapping its whole pid space within those two commands.
//
// A SIGTERM BETWEEN STARTING THE SLEEP AND HOLDING ITS PID is kept rather than
// acted on: `w` is set across those two steps, and the trap only records the
// stop there (`s`), which the exporter acts on once `z` holds the sleep. Acted
// on, the trap found no pid, exited, and left the sleep running in the job's
// group for the rest of its interval.
//
// THE SLEEP IS ENDED WITH SIGKILL, never SIGTERM, for the reason unsync tells
// the exporter more than once ([openCodeStops]): the sleep is forked while the
// exporter traps SIGTERM, and until it has run far enough to drop that trap a
// SIGTERM lands on the inherited handler and is lost. A stop kept across the
// fork is acted on the instant after it, so under dash most such SIGTERMs
// were lost and the exporter waited out the whole interval on a sleep it had
// told to go. SIGKILL has no handler to land on, and a sleep has nothing to
// shut down.
//
// AN EXPORT IT WAS STOPPED IN IS DROPPED WHOLE: its file (`out`, a variable
// so that no path is quoted inside the quoted trap) is removed as the
// exporter goes, where it hears the stop. A stop is acted on between
// commands, so it could land after an export that keep had not yet moved
// over the document, which left a second copy of the login beside the
// document for the rest of the box's life, under no rule a teardown follows.
// After a move there is nothing to remove. An exporter still in an export
// when the stop's bound runs out is killed, and no trap runs: unsync drops
// the file for that one ([openCodeStops]).
func openCodeExporter(sync time.Duration, file string) string {
	return fmt.Sprintf(`( z= w= s= out=%[2]s; trap 'if [ -n "$w" ]; then s=1; else [ -z "$z" ] || { kill -9 "$z" 2> /dev/null; wait "$z"; }; rm -f "$out"; exit 0; fi' TERM; while :; do w=1; sleep %[1]s & z=$!; w=; [ -z "$s" ] || { kill -9 "$z" 2> /dev/null; wait "$z"; exit 0; }; wait "$z"; z=; keep "$out" --server "$url"; done ) < /dev/null > /dev/null 2>&1`,
		strconv.FormatFloat(sync.Seconds(), 'f', -1, 64), file)
}

// openCodeLeftoverSteers is the script's line that cancels every steer the run's
// notes plugin admitted to the run's session (the script's sid) and OpenCode
// never delivered, run once `opencode run` has exited and before the server is
// stopped.
//
// A NOTE ADMITTED AS THE SESSION SETTLED STAYS IN ITS INBOX. The plugin admits
// with `resume: false` and stops at the execution's settle, and a note whose
// admission lands after OpenCode's last look at the inbox is neither delivered
// nor confirmed — so it waits on the run's record, where the review or the next
// launch's brief reads it. But it also stays pending in the session's own
// inbox, which is durable and survives the server's restart: a run carried on
// in the same box and session, with a person's answer, had OpenCode put it in
// front of the next prompt, while the continuation's brief carried it too —
// the model read the note twice, and the record said once. Measured on 2.1.4
// with the server restarted between the two runs, as the script restarts it:
// the second run's request carried the pending note before its own prompt, and
// carried nothing once the item was cancelled (`session.inbox.cancel`), the
// inbox listing empty after.
//
// ONLY THE PLUGIN'S OWN: an id of the shape it admits under ([OpenCodeInboxID],
// `msg_` and 32 lowercase hexadecimal digits), as the `id` of an item — read
// out of the listing as text, since a box may have no JSON parser, and a note's
// own text cannot match it: its quotes are escaped in the listing. A failed
// listing or cancel costs nothing more than the duplicate it was there to
// prevent.
func openCodeLeftoverSteers() string {
	return `pending=$(opencode api --server "$url" session.inbox.list --param sessionID="$sid"` +
		` < /dev/null 2> /dev/null) && for id in $(printf '%s\n' "$pending" | grep -o '"id":"msg_[0-9a-f]\{32\}"' | sed 's/^"id":"//; s/"$//' | sort -u); do ` +
		`opencode api --server "$url" session.inbox.cancel --param sessionID="$sid"` +
		` --param inboxID="$id" < /dev/null > /dev/null 2>&1; done`
}

// openCodeReadiness is the script's test of one `mcp.list` answer, `$s`, against
// every server the run declares: present, and not `pending`. Empty when the run
// declares none, which waits for nothing.
//
// Matched as literal text, each name spelled as the answer spells it
// ([jsonStringify]), because the answer is JSON a box may have no parser for:
// `"name":"<server>"` is present, and `"name":"<server>","status":{"status":
// "pending"` is not yet done. An absent name is still connecting too —
// measured, the first answer after the server starts lists nothing, because
// the workspace's servers start as it is first asked about.
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
			shellQuote(present), shellQuote(pending))
	}
	return b.String()
}

// jsonStringify spells s as JavaScript's JSON.stringify does, which is how
// OpenCode prints a server's name in an answer — and not as encoding/json
// does. Go escapes `&`, `<` and `>` for HTML and U+2028 and U+2029 for
// JavaScript source, none of which JSON.stringify touches (measured on 2.0.32:
// a server named `R&D <x> tools` followed by U+2028 is listed with all four
// literal), so a name carrying any of them never matched and every run waited
// out the whole [openCodeMCPWait] for a server that had connected in a second.
// JSON.stringify escapes exactly `"`, `\` and the control characters below
// U+0020: the five with a short form by it, the rest as `\u` and four
// lowercase hexadecimal digits.
//
// A byte that is not UTF-8 is U+FFFD, written as itself: that is what
// encoding/json put in the config OpenCode read the name from (as `�`),
// so it is the name OpenCode holds.
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

// WriteConfig renders the run's configuration in OpenCode 2's own format — the
// run's posture, the scoped MCP surface and the instruction file its system
// text is written to — at [Paths.OpenCodeConfig], beside the run's other
// artefacts, and returns that path. The run's script hands its content to the
// run's server as OPENCODE_CONFIG_CONTENT ([OpenCode.Command]).
//
// 2.0's FORMAT, NOT 1.x's, though 2.0 still reads 1.x's by translating it
// (its migration guide: servers move under `mcp.servers`, `enabled` becomes
// the inverse `disabled`, `npm` becomes `package`, a permission map becomes
// an ordered rule list). What this file has to say — a server's tools offered
// directly, a remote server that never starts OAuth, a rule list whose last
// match wins — exists only in the new shape, and a file written in the old one
// would be a translation of somebody else's to keep right.
//
// NO CREDENTIAL IS IN THE PAYLOAD. A key rides the run env under the variable
// of the provider the model names, so the secret is not written into a file
// inside the box where the agent could read it back and echo it into its
// report.
//
// OUT OF THE CHECKOUT, AND ABOVE THE REPOSITORY'S OWN. OpenCode takes no config
// flag, and of the two variables that hand a process a configuration of its
// own, OPENCODE_CONFIG — a file's path — ranks BELOW a repository's
// opencode.json and .opencode/, while OPENCODE_CONFIG_CONTENT ranks above them.
// 2.x reads its configuration in this order, a later document winning: managed
// policies, well-known, global, OPENCODE_CONFIG, the project's opencode.json,
// its .opencode/, OPENCODE_CONFIG_CONTENT, managed settings (core/src/config.ts,
// the same at 2.0.0, 2.0.32 and 2.1.4). So the file is written here and its
// content handed over, where one written into the checkout would replace the
// repository's own opencode.json: that is left as it is and loads beneath the
// run's — the way Claude Code loads a repository's .claude/settings.json
// beneath --settings. Measured
// on 2.0.32 and 2.1.4 with a repository whose own opencode.json and
// .opencode/opencode.json disable the bridge and deny every permission: the
// bridge connected and the shell ran.
//
// EVERY ENTRY THE RUN OWNS SAYS IT IS ON (`disabled: false`): OpenCode
// combines the documents it reads, and a document beneath this one may
// disable a server or an agent under the name this one declares. How a release combines them differs by kind — 2.1.4
// replaces a server's entry whole and updates an agent's field by field — so
// the run states the field rather than rest on either.
//
// THE MCP SERVERS' VALUES ARE FILES ([openCodeValueFiles]), written before the
// configuration that names them, so the content holds each value's path and
// never the value — as the server's environment holds no password
// ([OpenCode.Command]). The notes plugin's route is one such value.
//
// THE NOTES PLUGIN, for a run wired to its turn's notes
// ([sandbox.RunRequest.Steer]): written into a directory of its own
// ([Paths.OpenCodeSteerPlugin]) before the server that loads it starts, and
// named in `plugins` with the route as its one option. A `plugins` list is
// combined across the documents OpenCode reads in their order — an entry is an
// addition, or a removal by id written `-<id>` — so this document's entry, read
// last, is added after any removal a document beneath it makes. Measured on
// 2.0.32 and 2.1.4 with a repository whose own opencode.json and .opencode/
// remove the plugin: it loaded, with its option read from its file.
//
// EVERY PATH THE CONFIGURATION NAMES IS UNDER THE BOX'S WORK DIRECTORY — the
// instruction file, the plugin, every value — and OpenCode replaces `{env:…}`
// and `{file:…}` in the configuration's text before it parses it, so a box
// whose home that text cannot name is refused before anything is written
// ([openCodeReferable]).
//
// IT IS WRITTEN ON EVERY RUN, including one that names no server. The posture
// keys below are not decoration a minimal run can go without, and a run that
// wrote none would not run on the vendor's defaults or on a previous run's
// file — the clear removed that ([Runner.ClearArtifacts]) — but not at all: the
// script refuses to start a server it has no configuration for.
func (o OpenCode) WriteConfig(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, paths Paths) (string, error) {
	if req.System != "" || len(req.MCPServers) > 0 || req.Steer.Set() {
		if err := openCodeReferable(paths.WorkDir()); err != nil {
			return "", fmt.Errorf("executor: this box's home %q cannot be named in an OpenCode "+
				"run's configuration: %w; a local box's home is under providers.sandbox.local.state_dir, "+
				"which needs a path without such characters", paths.Home, err)
		}
	}
	cfg := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		// Sharing is disabled: a run's transcript is company work, and
		// OpenCode's share feature publishes it to a URL.
		"share": "disabled",
		// ALLOW, STATED RATHER THAN INHERITED, in 2.0's ordered rule list
		// where the last match wins. `opencode run` is headless, so a gate
		// left at `ask` has nobody to answer it and is refused — the run
		// does not stop, it loses the call and carries on, which reads as an
		// agent that would not do the work.
		//
		// The box is the boundary here exactly as it is for claude-code's
		// --permission-mode bypassPermissions (see [ClaudeCode.Command]):
		// what the agent may reach was decided engine-side, before it
		// started, by which MCP servers and credentials the run carries. And
		// a vendor default is not a posture to rest on — it is a value that
		// moves between releases, which is the assumption every profile in
		// internal/providers/llm/harness is built on.
		//
		// `question` is the one tool denied: it asks a PERSON, through a form
		// a headless run cancels, and a run that has something to ask has
		// `ask_and_wait` on its bridge, which parks it on a person's answer.
		//
		// The rules of every document OpenCode reads are added together in
		// its order, so these come after a repository's own and, the last
		// match winning, decide.
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
	}
	// THE RUN'S TURN CAP, on that agent: `steps`, OpenCode's bound on its
	// agent's iterations, after which the agent answers in text and the run
	// ends — Claude Code's --max-turns ([ClaudeCode.Command]) in this harness's
	// own vocabulary. It is the one bound on an executor's work
	// (providers.sandbox.default_max_turns and a seat's role.sandbox.max_turns,
	// already reconciled into Limits), so a run on this harness that did not
	// carry it ran without one. Absent at zero, which is uncapped: OpenCode
	// refuses a step count below one.
	if req.Limits.MaxTurns > 0 {
		cfg["agents"] = map[string]any{
			openCodeAgent: map[string]any{"steps": req.Limits.MaxTurns, "disabled": false},
		}
	}
	values := &openCodeValueFiles{dir: paths.OpenCodeValues()}
	if len(req.MCPServers) > 0 {
		cfg["mcp"] = map[string]any{"servers": openCodeMCP(req.MCPServers, values)}
	}
	if req.Steer.Set() {
		if err := box.WriteFile(ctx, paths.OpenCodeSteerPlugin()+"/"+openCodeSteerEntry, openCodeSteerPlugin); err != nil {
			return "", fmt.Errorf("executor: writing OpenCode's notes plugin: %w", err)
		}
		cfg["plugins"] = []map[string]any{{
			"package": paths.OpenCodeSteerPlugin(),
			"options": map[string]string{"url": values.ref(req.Steer.URL)},
		}}
	}
	for _, v := range values.files {
		if err := box.WriteFile(ctx, v.path, []byte(v.value)); err != nil {
			return "", fmt.Errorf("executor: writing a value of OpenCode's configuration: %w", err)
		}
	}
	// THE SYSTEM TEXT AS AN INSTRUCTION FILE, named by its absolute path.
	// OpenCode has no flag that adds to its system prompt; it adds every
	// instruction file it finds — a global AGENTS.md in its config
	// directory, a project's AGENTS.md, and each entry of a config's
	// `instructions` — after its default agent's own prompt, as
	// "Instructions from: <path>" and the file's text (measured on 2.0.32
	// and 2.1.4, off the request the harness sent: the default prompt whole, the
	// text after it, the user message holding the brief alone).
	//
	// An `instructions` entry rather than the global AGENTS.md, because the
	// entry is ADDITIVE and the file is not ours: 2.x collects the entries of
	// every config document it reads (2.1.4's own source), so an image's
	// global config keeps its instructions, where writing AGENTS.md into the
	// global config directory would overwrite one an operator baked into the
	// box, and would have to find that directory first — this run sets no
	// OPENCODE_CONFIG_DIR, so it is wherever the image's environment puts it.
	// And this file is rewritten on every run, so a reused box never carries
	// a previous run's entry. ABSOLUTE, because a relative entry is matched
	// by walking up from the working directory.
	//
	// TOLD OF NOTES on a run wired to them ([withNoteInstruction]): the
	// plugin that reads them is named by this same configuration, which the
	// run's server is started with.
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

// openCodeAgent is the OpenCode agent a run runs as: `build`, the harness's own
// default primary agent — the one with every tool, which a run that writes
// code is. Its config is OpenCode 2's `agents.<name>`, whose `steps` carries the
// run's turn cap.
const openCodeAgent = "build"

// openCodeValueFiles is the values a run's configuration names by reference:
// each MCP server's url, header value and environment value, and the notes
// plugin's route, one file apiece under [Paths.OpenCodeValues], named `v1`,
// `v2`, … in the order the configuration asks for them — servers by name, a
// server's headers and environment by key, then the plugin's — so the same
// request writes the same files.
//
// OpenCode replaces `{file:<path>}` with the file's content, its surrounding
// whitespace TRIMMED and JSON-escaped (core/src/config/variable.ts, the same
// in 2.0.32 and 2.1.4), so a value arrives as written save for whitespace at
// either end — which a url or a header value carries nothing in (HTTP drops a
// header value's own), and which an environment value that has it arrives
// without. A reference to a file
// that is missing reads as empty, with a warning only in OpenCode's own log,
// so the files are written before the configuration that names them.
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

// openCodeReferable is nil where `{file:<path>}` names path to OpenCode — and
// where the path, and any below it, can stand in the configuration's text as
// itself — and otherwise says why it cannot. OpenCode replaces every
// `{env:[^}]+}` and then every `{file:[^}]+}` in the configuration's text before
// it parses it, and resolves a relative path against the working directory,
// which is the checkout — so a path is named only where it is absolute, holds
// no brace to open a reference of its own or end one early, and is carried by
// the JSON text as itself: no `"`, `\`, control character, U+2028 or U+2029,
// which the text holds escaped and OpenCode would read as part of the path, and
// nothing that is not UTF-8, which the text holds as U+FFFD.
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
// every value a server is configured with — its url, each header value, each
// environment value — a reference to a file of values ([openCodeValueFiles]).
//
// EVERY SERVER'S TOOLS ARE OFFERED DIRECTLY (`codemode: false`). 2.0's default
// is Code Mode, where a server's tools are reachable only from JavaScript the
// model writes for the `execute` tool: the transcript then records every call
// as `execute`, never the tool the agent reached, and a model that does not
// take to writing the script cannot reach the seat's tools at all. Offered
// directly, a call is `<server>_<tool>`, as it was on 1.x.
//
// A REMOTE SERVER NEVER STARTS OAUTH (`oauth: false`): 2.0 tries it by default
// on a remote server that refuses a request, and a headless run has nobody to
// complete it — the bridge authenticates with the run's token, which is a
// segment of the endpoint's URL path rather than a header.
//
// EVERY SERVER IS ON (`disabled: false`) — see [OpenCode.WriteConfig].
func openCodeMCP(servers map[string]sandbox.MCPServer, values *openCodeValueFiles) map[string]any {
	out := make(map[string]any, len(servers))
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		s := servers[name]
		entry := map[string]any{"codemode": false, "disabled": false}
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
		// whole invocation as a single array, which is the one place its
		// vocabulary differs structurally rather than in spelling.
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

// Output is where OpenCode writes: its stdout IS its event stream and its
// result is derived from it, the stream being the result file the clear
// removes, so a reused box never shows a job another's. Its brief arrives on
// stdin ([Output.BriefOnStdin]).
//
// The done marker is the end of a run, as it is for every harness: 2.0 run
// against the script's own server exits when its session does — every run
// measured on 2.0.32 did — and the script exits after it. 1.x finished its
// work and did not exit, and the runner once read a stream's end for a
// terminal event to cover that; 2.0 would not have honoured it anyway, since
// it prints the final step's `step_finish` in only some runs, and an end read
// early would collect a run before its script had added the usage report and
// written back the box's refreshed login.
func (OpenCode) Output(paths Paths) Output {
	return Output{Stdout: paths.Result(), Events: true, BriefOnStdin: true}
}

// Events is a new decoder for one stream, read whole or followed ([Decoder]).
func (OpenCode) Events() Decoder { return &openCodeEvents{} }

// Parse decodes a whole stream already in hand: the result of a run whose
// result IS its stream.
//
// The run's tokens and its price come from the session's own usage report the
// script appends ([openCodeUsageEvent]), and stay ZERO where there is none —
// never estimated: an invented number in the spend rollup is worse than a
// missing one, because a reader cannot tell it is invented.
func (OpenCode) Parse(stdout string) sandbox.Result {
	return decodeAll(&openCodeEvents{}, stdout)
}

// openCodeEvent is the part of one stream event this runner reads, and no
// more: the event carries the tool's whole state, its output included, and a
// generic map of every field was several times the line it came from.
//
// DECODED LOOSELY. A field whose type a harness version changed is skipped rather
// than losing the event — see [decodeOpenCodeEvent] — because the nesting has
// moved across versions and a box runs whatever the operator's image has.
type openCodeEvent struct {
	Type  string          `json:"type"`
	Tool  string          `json:"tool"`
	Name  string          `json:"name"`
	Error json.RawMessage `json:"error"`
	Part  struct {
		// MessageID is the message the part belongs to: each step of a
		// run is a message of its own.
		MessageID string         `json:"messageID"`
		Tool      string         `json:"tool"`
		Text      string         `json:"text"`
		Reason    string         `json:"reason"`
		Input     *openCodeInput `json:"input"`
		State     struct {
			Status string         `json:"status"`
			Error  string         `json:"error"`
			Output string         `json:"output"`
			Input  *openCodeInput `json:"input"`
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
// A TYPE MISMATCH IS NOT A LOST EVENT. encoding/json skips a field whose value
// does not fit its Go type, decodes the rest, and reports the first such
// mismatch — so the mismatch is ignored and the event kept, which is what the
// map lookups this replaced did for free.
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

// openCodeEvents decodes one stream into the answer and a readable
// transcript, read whole or followed across reads ([Decoder]).
//
// THE ANSWER IS THE LAST MESSAGE'S TEXT, never every step's joined. Each step
// of a run is a message of its own (`part.messageID`), and a run handed a
// person's note after it had written an answer goes on to a later step that
// answers again — `run --format json` prints no event for the note, so the
// stream reads as two answers in a row (measured on 2.0.32 and 2.1.4). Joined,
// the run's result was the answer the note superseded followed by the one it
// asked for; and a run that narrated its way through its steps ("running the
// tests first") reported the narration as part of its answer. The transcript
// keeps every step's text, and the report the run writes is what the result
// prefers anyway ([Paths.Findings]).
type openCodeEvents struct {
	// answers is the text of the latest message that produced any, in
	// order, and message that message's id. A text event that names no
	// message belongs to the latest one: only a message that names itself
	// and differs ends it, so a stream that names none is read whole.
	answers    []string
	message    string
	transcript transcriptLines
	errText    string
	sawEvent   bool
	sawLine    bool

	// usage is the session's usage report, when the script printed one, and
	// usageBefore the one a continued run's script printed before the run
	// took the session up ([openCodeUsageBefore]).
	usage, usageBefore *openCodeReport

	// plain is the output while no event has been seen: a run that printed
	// its answer as text rather than as the event stream it was asked for,
	// and the whole of it is the answer then — prose is still an answer, and
	// a run's exit status, not this decoder, says whether it failed. Held to [sandbox.MaxFileBytes],
	// the bound the whole read it replaced held it to, with what lies past
	// it counted rather than kept.
	plain        strings.Builder
	plainDropped int64

	// following is a decoder read through Entries, which keeps neither
	// the answer nor the plain output a Result would need: a live reading
	// holds it for as long as somebody watches.
	following bool
}

// Entries implements [Decoder].
func (d *openCodeEvents) Entries() []string {
	d.following = true
	d.answers = nil
	d.plain.Reset()
	return d.transcript.take()
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
	switch ev.Type {
	case "text":
		if chunk := strings.TrimSpace(ev.Part.Text); chunk != "" {
			if !d.following {
				d.answer(ev.Part.MessageID, chunk)
			}
			d.transcript.add(chunk)
		}
	case "tool_use":
		d.transcript.add(toolLine(ev))
	case "error":
		d.errText = errorText(ev.Error)
		d.transcript.add("[error] " + d.errText)
	case openCodeUsageEvent:
		if ev.Report != nil {
			d.usage = ev.Report
		}
	case openCodeUsageBefore:
		if ev.Report != nil {
			d.usageBefore = ev.Report
		}
	}
}

// answer adds one text event's chunk to the answer: after the latest message's
// text where the event is that message's, or names none; in its place where it
// names a later one.
func (d *openCodeEvents) answer(message, chunk string) {
	if message != "" {
		if d.message != "" && message != d.message {
			d.answers = d.answers[:0]
		}
		d.message = message
	}
	d.answers = append(d.answers, chunk)
}

func (d *openCodeEvents) keepPlain(line []byte) {
	if d.following {
		return
	}
	if d.plainDropped > 0 || d.plain.Len()+len(line)+1 > sandbox.MaxFileBytes {
		d.plainDropped += int64(len(line) + 1)
		return
	}
	d.plain.Write(line)
	d.plain.WriteByte('\n')
}

func (d *openCodeEvents) Skipped(n int64, bound int) {
	d.sawLine = true
	if !d.sawEvent {
		d.plainDropped += n
		return
	}
	d.transcript.skip(n, bound)
}

func (d *openCodeEvents) Result() sandbox.Result {
	if !d.sawLine {
		return sandbox.Result{Error: "the harness produced no output"}
	}
	if !d.sawEvent {
		// Output that is not the event stream: the whole of it is the
		// answer.
		text := strings.TrimSpace(d.plain.String())
		if d.plainDropped > 0 {
			text += fmt.Sprintf("\n(%s more of the output not read: past the %s a run's plain "+
				"output is read to)", humanSize(d.plainDropped), humanSize(sandbox.MaxFileBytes))
		}
		return sandbox.Result{Text: text, Success: true, DeliveredRefs: deliveredRefs(text)}
	}
	body := strings.TrimSpace(strings.Join(d.answers, "\n"))
	success := body != "" && d.errText == ""
	res := sandbox.Result{
		Text:          body,
		Success:       success,
		Transcript:    d.transcript.String(),
		DeliveredRefs: deliveredRefs(body),
	}
	if u := d.usage; u != nil {
		tokens := u.Data.Tokens
		res.InputTokens = tokens.Input + tokens.Cache.Read + tokens.Cache.Write
		res.OutputTokens = tokens.Output + tokens.Reasoning
		res.CacheReadTokens = tokens.Cache.Read
		res.CacheWriteTokens = tokens.Cache.Write
		// AS REPORTED, a negative price included, like the counts above. A
		// provider OpenCode has no pricing for reports zero, which is what a
		// result's zero already means — nobody said — and a negative one is
		// the coordinator's to floor and SAY ([sandbox.Result]'s floor, and
		// its sandbox_usage_negative warning): dropped here, it was floored
		// with nothing logged, while a negative token count beside it was not.
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
	// NO TURN COUNT ([sandbox.Result.Turns]): the session report carries
	// none, and the stream's step markers cannot be counted — 2.0.32 prints
	// `step_start` for a run's first step and, in nearly every multi-step
	// run measured, not for the steps after it — so a count read off them
	// would be one for most runs and right only by luck. None reported is
	// a run counted as the one call the engine asked for.
	if !success {
		res.Error = d.errText
		if res.Error == "" {
			res.Error = "the harness produced no answer"
		}
	}
	return res
}

// toolLine renders one tool event for the transcript.
//
// Enriched with the call's own input, because a bare tool name is useless when
// a run fails: what a reader needs is the command that ran. The nesting varies
// across versions, so every lookup falls back to the name. NEVER the tool's
// output: that is the file a read returned or everything a command printed,
// which is what the transcript is a summary of — only a failed call's first
// line, which says why it failed.
func toolLine(ev openCodeEvent) string {
	name := firstNonBlank(ev.Part.Tool, ev.Tool, ev.Name)
	if name == "" {
		name = "tool"
	}
	input := ev.Part.State.Input
	if input == nil {
		input = ev.Part.Input
	}
	if input == nil {
		input = &openCodeInput{}
	}

	line := "[tool] " + name
	if detail := firstNonBlank(
		input.Command, input.FilePath, input.Path, input.Pattern, input.Description,
	); detail != "" {
		line += ": " + firstLine(detail, transcriptDetailLimit)
	}

	state := ev.Part.State
	failure := firstNonBlank(state.Error)
	if failure == "" && state.Status == "error" {
		failure = firstNonBlank(state.Output)
	}
	if state.Status == "error" || failure != "" {
		if failure == "" {
			failure = "failed"
		}
		line += " → error: " + firstLine(failure, transcriptDetailLimit)
	}
	return line
}

func errorText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	// An object: its JSON, compacted, which is how the generic decode this
	// replaced rendered it.
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

// firstLine is one transcript line's echoed command or path, bounded — a
// PREVIEW, for a person scanning what the run did, and marked as one.
//
// The bound is real — a heredoc echoed whole would make one entry of a
// line-structured log read as many — and every cut says so: the line itself
// with an ellipsis, and the lines after it with a count. Only the first was
// ever marked, so a heredoc read as the one command on its first line. Two
// older defects: `line[:limit-1] + "…"` emitted limit+2 BYTES (limit-1 of
// content plus a three-byte ellipsis), so the constant bounded nothing it
// named; and the byte slice split whatever multi-byte character straddled the
// cut, which reaches the event store as invalid UTF-8.
//
// REDACTED WHOLE BEFORE IT IS CUT, and here because this is the one place
// every preview cut goes through — a tool's subject, a failed call's error and
// a run's ending, for both decoders and so for the record and the live view
// alike. The transcript is redacted again downstream, but by then the cut has
// already happened: a token whose start fell in the last few dozen bytes
// before the limit survived as a fragment shorter than its rule's length floor
// (`ghp_` and 28 of a GitHub token's 36 characters), which no pattern
// recognises and which identifies the token to anybody holding its checksum.
// The whole value, not just its first line, because a private key's block
// runs from a BEGIN line on the first line into the lines below it.
func firstLine(s string, limit int) string {
	line, rest, more := strings.Cut(strings.TrimSpace(redact.Secrets(s)), "\n")
	suffix := ""
	if more {
		suffix = fmt.Sprintf(" (+%d more line(s))", strings.Count(rest, "\n")+1)
	}
	if limit <= 0 || len(line)+len(suffix) <= limit {
		return line + suffix
	}
	// [textcut.Within] rather than Ellipsis: that one does not count its
	// marker against max, and this limit bounds what reaches the phase
	// event, markers included.
	return textcut.Within(line, max(limit-len(suffix), 0)) + suffix
}
