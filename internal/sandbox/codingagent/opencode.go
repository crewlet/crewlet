package codingagent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
// ended. A type OpenCode never prints, so nothing the CLI emits is read as it.
const openCodeUsageEvent = "crewlet.usage"

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
type OpenCode struct{}

var _ CLI = OpenCode{}

// NewOpenCode returns the runner.
func NewOpenCode() *Runner { return New(OpenCode{}) }

// Name is the coding agent's key in the runner registry.
func (OpenCode) Name() string { return OpenCodeName }

// Command is the run: a short shell script, because OpenCode 2 has to be
// driven through a server of the run's own rather than in one command.
//
// A PRIVATE SERVER, STARTED FIRST AND WAITED ON. `opencode run` alone either
// attaches to a background service — detached from the run, on a port compiled
// into the binary and shared by every box on a host, which outlives the run
// and keeps the environment, and so the credentials, of whichever run started
// it — or, with `--standalone`, starts a server inside the run and sends the
// brief at once. That second shape is what text calls use and it is wrong
// here: it sends the brief before the run's MCP servers have connected, and
// 2.0 fixes a step's tools as the step starts, so the first step of every
// run — measured on 2.0.32, the first step of every run with a server
// declared — was offered none of the seat's bridged tools, which only
// appeared from the step after. So the script starts `opencode serve` on a
// loopback port of its own, waits until every MCP server the run declares
// has left `pending` (`api mcp.list`, bounded by [openCodeMCPWait]), runs the
// brief against that server, and stops it.
//
// THE BRIEF ON STDIN ([Output.BriefOnStdin]), because OpenCode re-quotes an
// argv prompt; the session NAMED, by an id minted here, so the usage report
// asked for afterwards is this run's; a TITLE given, which stops 2.0 making a
// second billed model call to write one.
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
func (OpenCode) Command(req sandbox.RunRequest, paths Paths, _ string) string {
	return "sh -c " + shellQuote(openCodeScript(req, paths, newOpenCodeSession(), openCodeTimes{
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
// hexadecimal characters from crypto/rand, which the script prefixes with the
// `ses_` 2.0 requires. Random rather than derived from the run, because
// OpenCode CONTINUES a session it already holds, and a reused box holds the
// last run's.
func newOpenCodeSession() string {
	var raw [16]byte
	// crypto/rand.Read does not fail on the platforms this builds for: it
	// crashes the process rather than return an error since Go 1.24.
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

// openCodeScript is the run's script for one session id, exporting a login the
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
func openCodeScript(req sandbox.RunRequest, paths Paths, session string, times openCodeTimes) string {
	doc := shellQuote(paths.CredentialDocument())
	next := shellQuote(paths.CredentialDocument() + ".next")
	loop := shellQuote(paths.CredentialDocument() + ".sync")
	srv := shellQuote(paths.WorkDir() + "/opencode-server")
	sid := "ses_" + session

	run := []string{"opencode", "run", "--server", `"$url"`, "--format", "json",
		"--title", "crewlet", "--session", shellQuote(sid)}
	// THE ENTRY'S MODEL AS WRITTEN. It is the string every text call on
	// the same providers.llm entry hands `opencode run --model`, which
	// OpenCode splits at its first slash into a provider and a model — so
	// `anthropic/claude-sonnet-5` or `openrouter/anthropic/claude-sonnet-5`
	// means one thing in both places, signed in by the login or the key of
	// the provider it names. The engine refuses every other CLI's entry here.
	if req.Model != "" {
		run = append(run, "--model", shellQuote(req.Model))
	}

	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	// 1.x prints a bare version, 2.x `opencode v2.…`.
	line(`v=$(opencode --version < /dev/null 2> /dev/null) || { echo 'crewlet: no opencode on the box PATH' >&2; exit 127; }`)
	line(`case "$v" in [0-9]*) echo "crewlet: this box runs OpenCode $v; the opencode runner drives OpenCode 2 (the @opencode/cli package) — install it in the box image" >&2; exit 1;; esac`)
	line(`unset OPENCODE_PASSWORD OPENCODE_SERVER_PASSWORD`)
	line(`imported= spid= xpid=`)
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

	line(`opencode serve --hostname 127.0.0.1 --port 0 < /dev/null > %s 2>&1 &`, srv)
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

	line(`%s`, strings.Join(run, " "))
	line(`code=$?`)
	line(`report=$(opencode api --server "$url" session.get --param sessionID=%s < /dev/null 2> /dev/null) && case "$report" in '{'*) printf '{"type":"%s","report":%%s}\n' "$report";; esac`,
		shellQuote(sid), openCodeUsageEvent)
	line(`unsync`)
	line(`[ -z "$imported" ] || keep %s --server "$url"`, next)
	line(`stop; trap - EXIT TERM INT HUP`)
	line(`exit "$code"`)
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
// THE SERVER IS TOLD ONCE. It is stopped long after it starts on every path
// but a signal in its first instant, where a lost SIGTERM costs the bound and
// then the kill, and a repeat would tell a server that is already shutting
// down, stopping its own MCP servers, to stop again.
func openCodeStops(stop time.Duration) string {
	return fmt.Sprintf(`end() { [ -n "$1" ] || return 0; kill "$1" 2> /dev/null || return 0; i=0; while kill -0 "$1" 2> /dev/null && [ "$i" -lt %d ]; do sleep 0.1; i=$((i+1)); [ -z "$2" ] || { kill -0 "$1" 2> /dev/null && kill "$1" 2> /dev/null; }; done; kill -0 "$1" 2> /dev/null && kill -9 "$1" 2> /dev/null; return 0; }
stop() { end "$spid"; spid=; }
unsync() { end "$xpid" again; xpid=; }
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
func openCodeExporter(sync time.Duration, file string) string {
	return fmt.Sprintf(`( z= w= s=; trap 'if [ -n "$w" ]; then s=1; else [ -z "$z" ] || { kill -9 "$z" 2> /dev/null; wait "$z"; }; exit 0; fi' TERM; while :; do w=1; sleep %s & z=$!; w=; [ -z "$s" ] || { kill -9 "$z" 2> /dev/null; wait "$z"; exit 0; }; wait "$z"; z=; keep %s --server "$url"; done ) < /dev/null > /dev/null 2>&1`,
		strconv.FormatFloat(sync.Seconds(), 'f', -1, 64), file)
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

// WriteConfig renders opencode.json in OpenCode 2's own format: the run's
// posture, the scoped MCP surface and the instruction file its system text is
// written to.
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
// IT IS WRITTEN ON EVERY RUN, including one that names no server. The posture
// keys below are not decoration a minimal run can go without — and because
// this CLI takes no config flag, the file's LOCATION is
// the wiring, so a run that writes nothing does not run on the vendor's
// defaults: it runs on whatever the PREVIOUS run left in the checkout, which
// on a reused box is that run's MCP block with its dead per-run bridge URL in
// it.
func (OpenCode) WriteConfig(ctx context.Context, box sandbox.Sandbox, req sandbox.RunRequest, paths Paths) (string, error) {
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
		// internal/providers/llm/cliagent is built on.
		//
		// `question` is the one tool denied: it asks a PERSON, through a form
		// a headless run cancels, and a run that has something to ask has
		// `crewlet-ask`, which reaches the seat that started it.
		"permissions": []map[string]string{
			{"action": "*", "resource": "*", "effect": "allow"},
			{"action": "question", "resource": "*", "effect": "deny"},
		},
		// A CLI that updates itself mid-run swaps the tool under the brief
		// between launch and result, needs network a box may not have, and
		// does both while a seat's turn is suspended waiting on it.
		"update": "disable",
		// 2.0 keeps a git snapshot of the working directory at every step,
		// for an undo a headless run never offers — on a large checkout a
		// copy of its tree per step, for nothing.
		"snapshots": false,
	}
	if len(req.MCPServers) > 0 {
		cfg["mcp"] = map[string]any{"servers": openCodeMCP(req.MCPServers)}
	}
	// THE SYSTEM TEXT AS AN INSTRUCTION FILE, named by its absolute path.
	// OpenCode has no flag that adds to its system prompt; it adds every
	// instruction file it finds — a global AGENTS.md in its config
	// directory, a project's AGENTS.md, and each entry of a config's
	// `instructions` — after its default agent's own prompt, as
	// "Instructions from: <path>" and the file's text (measured on 2.0.32
	// and 2.1.4, off the request the CLI sent: the default prompt whole, the
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
	if req.System != "" {
		if err := box.WriteFile(ctx, paths.System(), []byte(req.System)); err != nil {
			return "", fmt.Errorf("codingagent: writing the system prompt: %w", err)
		}
		cfg["instructions"] = []string{paths.System()}
	}
	blob, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	// opencode.json in the WORKING DIRECTORY, which is the checkout: the CLI
	// takes no --config flag, so the file's location is the wiring.
	path := paths.Home + "/" + sandbox.WorkspaceSubdir + "/opencode.json"
	if err := box.WriteFile(ctx, path, blob); err != nil {
		return "", err
	}
	return path, nil
}

// openCodeMCP translates the generic launch specs into OpenCode 2's schema.
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
func openCodeMCP(servers map[string]sandbox.MCPServer) map[string]any {
	out := make(map[string]any, len(servers))
	for name, s := range servers {
		entry := map[string]any{"codemode": false}
		if s.Transport == sandbox.TransportHTTP {
			entry["type"] = "remote"
			entry["url"] = s.URL
			entry["oauth"] = false
			if len(s.Headers) > 0 {
				entry["headers"] = s.Headers
			}
			out[name] = entry
			continue
		}
		// ONE ARGV, not a command and its arguments: this CLI takes the
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
			entry["environment"] = s.Env
		}
		out[name] = entry
	}
	return out
}

// Output is where OpenCode writes: its stdout IS its event stream and its
// result is derived from it, the stream being the result file the clear
// removes, so a reused box never shows a job another's. Its brief arrives on
// stdin ([Output.BriefOnStdin]).
//
// The done marker is the end of a run, as it is for every CLI: 2.0 run
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
// DECODED LOOSELY. A field whose type a CLI version changed is skipped rather
// than losing the event — see [decodeOpenCodeEvent] — because the nesting has
// moved across versions and a box runs whatever the operator's image has.
type openCodeEvent struct {
	Type  string          `json:"type"`
	Tool  string          `json:"tool"`
	Name  string          `json:"name"`
	Error json.RawMessage `json:"error"`
	Part  struct {
		Tool   string         `json:"tool"`
		Text   string         `json:"text"`
		Reason string         `json:"reason"`
		Input  *openCodeInput `json:"input"`
		State  struct {
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
type openCodeEvents struct {
	answers    []string
	transcript transcriptLines
	errText    string
	sawEvent   bool
	sawLine    bool

	// usage is the session's usage report, when the script printed one.
	usage *openCodeReport

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
				d.answers = append(d.answers, chunk)
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
	}
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
		return sandbox.Result{Error: "the coding agent produced no output"}
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
	}
	if !success {
		res.Error = d.errText
		if res.Error == "" {
			res.Error = "the coding agent produced no answer"
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
