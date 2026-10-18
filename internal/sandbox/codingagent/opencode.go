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
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/textcut"
)

// OpenCodeName is this runner's config name.
const OpenCodeName = "opencode"

// OpenCodeProviderID is the provider a run's config declares.
//
// Its own id rather than one of the vendor names, because it points at the
// SEAT's endpoint and model, which may be neither vendor's default.
const OpenCodeProviderID = "crewlet"

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
// THE LOGIN, WHERE THE BOX WAS GIVEN ONE: a local box is seeded with the
// entry's exported login ([Paths.CredentialDocument]); the script imports it
// before the server starts and exports the database back over it when the run
// has ended, so a token the run refreshed is what the box's teardown writes
// back. A box given none — a remote one, or an API entry's — signs in with the
// run environment's key, as before.
//
// THE USAGE, from the session rather than the stream: `run --format json`
// drops the final step's counts, while the session the run worked in holds
// the whole total, so the script prints it as one last line of its own type
// ([openCodeUsageEvent]) once the run has ended.
func (OpenCode) Command(req sandbox.RunRequest, paths Paths, _ string) string {
	return "sh -c " + shellQuote(openCodeScript(req, paths, newOpenCodeSession()))
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

// openCodeScript is the run's script for one session id — see [OpenCode.Command].
//
// Every command but the run itself reads /dev/null rather than the brief the
// script's stdin is, and writes nowhere but a file or /dev/null: its stdout is
// the event stream.
func openCodeScript(req sandbox.RunRequest, paths Paths, session string) string {
	doc := shellQuote(paths.CredentialDocument())
	next := shellQuote(paths.CredentialDocument() + ".next")
	srv := shellQuote(paths.WorkDir() + "/opencode-server")
	sid := "ses_" + session

	run := []string{"opencode", "run", "--server", `"$url"`, "--format", "json",
		"--title", "crewlet", "--session", shellQuote(sid)}
	if model := openCodeModelArg(req.LLM); model != "" {
		run = append(run, "--model", shellQuote(model))
	}

	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	// 1.x prints a bare version, 2.x `opencode v2.…`.
	line(`v=$(opencode --version < /dev/null 2> /dev/null) || { echo 'crewlet: no opencode on the box PATH' >&2; exit 127; }`)
	line(`case "$v" in [0-9]*) echo "crewlet: this box runs OpenCode $v; the opencode runner drives OpenCode 2 (the @opencode/cli package) — install it in the box image" >&2; exit 1;; esac`)
	line(`if [ -s %s ]; then`, doc)
	line(`  opencode auth import --standalone < %s > /dev/null || { echo 'crewlet: OpenCode refused the login this box was given — sign the engine in again with crewlet llm login' >&2; exit 1; }`, doc)
	line(`fi`)

	line(`opencode serve --hostname 127.0.0.1 --port 0 < /dev/null > %s 2>&1 &`, srv)
	line(`spid=$!`)
	// Bounded, because the done marker waits on this script: a server that
	// ignored SIGTERM would otherwise hold the run open for ever.
	line(`stop() { kill "$spid" 2> /dev/null || return 0; i=0; while kill -0 "$spid" 2> /dev/null && [ "$i" -lt 50 ]; do sleep 0.1; i=$((i+1)); done; kill -9 "$spid" 2> /dev/null; return 0; }`)
	line(`trap stop EXIT`)
	line(`deadline=$(( $(date +%%s) + %d ))`, int(openCodeListenWait.Seconds()))
	line(`until grep -q '^server password ' %s 2> /dev/null; do`, srv)
	line(`  if ! kill -0 "$spid" 2> /dev/null; then echo 'crewlet: opencode serve exited before it listened:' >&2; cat %s >&2; exit 1; fi`, srv)
	line(`  if [ "$(date +%%s)" -ge "$deadline" ]; then echo 'crewlet: opencode serve did not listen within %s' >&2; exit 1; fi`, openCodeListenWait)
	line(`  sleep 0.2`)
	line(`done`)
	line(`url=$(sed -n 's/^server listening on //p' %s)`, srv)
	line(`OPENCODE_SERVER_PASSWORD=$(sed -n 's/^server password //p' %s); export OPENCODE_SERVER_PASSWORD`, srv)
	line(`rm -f %s`, srv)

	if checks := openCodeReadiness(req.MCPServers); checks != "" {
		line(`deadline=$(( $(date +%%s) + %d ))`, int(openCodeMCPWait.Seconds()))
		line(`while :; do`)
		line(`  s=$(opencode api --server "$url" mcp.list < /dev/null 2> /dev/null)`)
		line(`  ready=1`)
		b.WriteString(checks)
		line(`  [ "$ready" = 1 ] && break`)
		line(`  if [ "$(date +%%s)" -ge "$deadline" ]; then echo 'crewlet: an MCP server was still connecting after %s; the run starts without it' >&2; break; fi`, openCodeMCPWait)
		line(`  sleep 0.5`)
		line(`done`)
	}

	line(`%s`, strings.Join(run, " "))
	line(`code=$?`)
	line(`report=$(opencode api --server "$url" session.get --param sessionID=%s < /dev/null 2> /dev/null) && case "$report" in '{'*) printf '{"type":"%s","report":%%s}\n' "$report";; esac`,
		shellQuote(sid), openCodeUsageEvent)
	// Only over a login the box was given, and never with an export of
	// nothing: a database that dropped a credential it could not refresh is
	// not a reason to replace the one the box came with.
	line(`if [ -s %s ]; then`, doc)
	line(`  if opencode auth export --server "$url" < /dev/null > %s 2> /dev/null && [ -s %s ] && [ "$(tr -d '[:space:]' < %s)" != '[]' ]; then mv %s %s; fi`,
		next, next, next, next, doc)
	line(`  rm -f %s`, next)
	line(`fi`)
	line(`stop; trap - EXIT`)
	line(`exit "$code"`)
	return b.String()
}

// openCodeReadiness is the script's test of one `mcp.list` answer, `$s`, against
// every server the run declares: present, and not `pending`. Empty when the run
// declares none, which waits for nothing.
//
// Matched as literal text, each name spelled as JSON spells it, because the
// answer is JSON a box may have no parser for: `"name":"<server>"` is present,
// and `"name":"<server>","status":{"status":"pending"` is not yet done. An
// absent name is still connecting too — measured, the first answer after the
// server starts lists nothing, because the workspace's servers start as it is
// first asked about.
func openCodeReadiness(servers map[string]sandbox.MCPServer) string {
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	slices.Sort(names)
	var b strings.Builder
	for _, name := range names {
		quoted, _ := json.Marshal(name)
		present := `"name":` + string(quoted)
		pending := present + `,"status":{"status":"pending"`
		fmt.Fprintf(&b, "  case \"$s\" in *%s*) case \"$s\" in *%s*) ready=0;; esac;; *) ready=0;; esac\n",
			shellQuote(present), shellQuote(pending))
	}
	return b.String()
}

// openCodeModelArg is the fully-formed --model value.
//
// AN OPENCODE ENTRY'S MODEL IS PASSED AS WRITTEN. It is the string every text
// call on the same providers.llm entry hands `opencode run --model`, which
// OpenCode splits at its first slash into a provider and a model — so
// `anthropic/claude-sonnet-5` or `openrouter/anthropic/claude-sonnet-5` means
// one thing in both places, signed in by the login or the key of the provider
// it names. Prefixing a family here addressed `anthropic/openrouter/…` and
// left the entry's own key unread: no value of `model` worked for both the box
// and the text calls.
//
// Otherwise the entry is an API one (the engine refuses another CLI's entry
// before a run starts): a custom base URL means the run's own declared
// provider, and with none the model is addressed under the OpenCode provider
// of the entry's wire, whose built-in key variable the run environment
// carries.
func openCodeModelArg(llm *sandbox.AgentLLM) string {
	if llm == nil || llm.Model == "" {
		return ""
	}
	if llm.CLI == OpenCodeName {
		return llm.Model
	}
	if llm.BaseURL != "" {
		return OpenCodeProviderID + "/" + llm.Model
	}
	return openCodeFamily(llm.ProviderType) + "/" + llm.Model
}

// openCodeFamily is the OpenCode provider an API entry's wire is served by:
// Anthropic's for an `anthropic` entry, OpenAI's for an `openai` or
// `openai-compatible` one — the two wires the engine's own providers speak,
// and the two key variables the run environment carries.
func openCodeFamily(providerType string) string {
	if providerType == "anthropic" {
		return "anthropic"
	}
	return "openai"
}

// WriteConfig renders opencode.json in OpenCode 2's own format: the run's
// posture, the custom provider and the scoped MCP surface.
//
// 2.0's FORMAT, NOT 1.x's, though 2.0 still reads 1.x's by translating it
// (its migration guide: servers move under `mcp.servers`, `enabled` becomes
// the inverse `disabled`, `npm` becomes `package`, a permission map becomes
// an ordered rule list). What this file has to say — a server's tools offered
// directly, a remote server that never starts OAuth, a rule list whose last
// match wins — exists only in the new shape, and a file written in the old one
// would be a translation of somebody else's to keep right.
//
// THE API KEY IS NEVER IN THE PAYLOAD. It rides the run env and the provider
// names the VARIABLE it is read from (`env`), so the secret is not written
// into a file inside the box where the agent could read it back and echo it
// into its report.
//
// IT IS WRITTEN ON EVERY RUN, including one that names no provider and no
// server. The posture keys below are not decoration a minimal run can go
// without — and because this CLI takes no config flag, the file's LOCATION is
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
	if req.LLM != nil && req.LLM.BaseURL != "" && req.LLM.Model != "" {
		cfg["providers"] = openCodeProvider(*req.LLM)
	}
	if len(req.MCPServers) > 0 {
		cfg["mcp"] = map[string]any{"servers": openCodeMCP(req.MCPServers)}
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

// openCodeProvider is the `crewlet` provider declared for an API entry with a
// base URL: the native package for the entry's wire, pointed at the entry's
// endpoint, signed in by the variable the run environment carries the entry's
// key under.
//
// THE ENDPOINT IS TRANSLATED FOR THE ANTHROPIC PACKAGE, because the two
// conventions disagree about where /v1 lives. An `anthropic` entry's base_url
// is the host ROOT — the convention of Anthropic's own SDK, which the engine's
// provider is built on and which appends /v1/messages, and of Claude Code's
// ANTHROPIC_BASE_URL — while OpenCode's anthropic-compatible package takes a
// prefix that already ends in /v1 and posts to `<baseURL>/messages` (measured
// on 2.0.32: the root posted to /messages, the root plus /v1 to /v1/messages).
// An `openai` or `openai-compatible` entry's base_url already ends where both
// its readers expect, so it passes through as written.
func openCodeProvider(llm sandbox.AgentLLM) map[string]any {
	pkg := "@opencode/ai/providers/openai-compatible"
	keyEnv := "OPENAI_API_KEY"
	baseURL := llm.BaseURL
	if openCodeFamily(llm.ProviderType) == "anthropic" {
		pkg = "@opencode/ai/providers/anthropic-compatible"
		keyEnv = "ANTHROPIC_API_KEY"
		baseURL = strings.TrimRight(baseURL, "/") + "/v1"
	}
	return map[string]any{
		OpenCodeProviderID: map[string]any{
			"package":  pkg,
			"name":     "Crewlet role LLM",
			"env":      []string{keyEnv},
			"settings": map[string]any{"baseURL": baseURL},
			"models":   map[string]any{llm.Model: map[string]any{}},
		},
	}
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
// complete it — the bridge authenticates with the run's token, in its headers.
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
		// A price only where OpenCode named one: a provider it has no
		// pricing for reports zero, which is "nobody said", not "free".
		if u.Data.Cost > 0 {
			res.CostUSD = u.Data.Cost
		}
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
