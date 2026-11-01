package harnessprofile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/words"
)

// MCP is how a harness is handed one run's tools.
//
// A phase's tools reach its harness as ONE MCP server, named `crewlet`, which the
// harness starts as a stdio command — `crewlet mcp-relay <socket>`, a relay into a
// unix socket this engine serves for that one run (internal/toolsession) — and
// calls natively, in its own loop. What differs from harness to harness is only how it
// is TOLD about that server and its limits, and every one of those is here:
// arguments, a configuration file written into the call's working directory,
// environment, the allowlist that lets the model call the server's tools
// without a person to approve them, and the harness's own per-call timeout.
//
// NIL ON A PROFILE THAT CANNOT BE HANDED TOOLS. Such an entry still answers a
// text function — a judge, a classifier, a summary — and a seat's phase on it
// is refused naming this field, rather than run with no tools.
//
// # Placeholders
//
// Every string in args, allow_args, complete_args, usage_args, seed content,
// env values and the serve lifecycle's arguments may carry these, substituted
// per run:
//
//	{relay_command}         the relay's executable, as a path
//	{relay_command_json}    the same as a JSON string, quotes included
//	{relay_args_json}       the relay's arguments as a JSON array
//	{relay_argv_json}       the executable and its arguments as one JSON array
//	{socket}                the run's socket path
//	{tool_timeout_seconds}  ToolTimeoutSeconds
//	{tool_timeout_ms}       the same in milliseconds
//	{work}                  the call's working directory, where seeds are written
//	{<as>}                  a serve lifecycle's ready value ([Serve.Ready]),
//	                        everywhere but seeds and serve.args
//
// The JSON forms are encoded without HTML escaping, so each is also a valid TOML
// value — a string or an array — for a harness configured in TOML.
//
// PER RUN, NEVER PER SEAT. The socket is the run's own and goes when the run
// ends, so it may be written only where the run alone reads it: argv, the
// environment, and a file in the call's own working directory. A seed in the
// seat's home is shared by every call of the seat at once and seeded once per
// generation, so it would hand one run another's socket — which is why a seed
// here is written into the working directory and nowhere else.
type MCP struct {
	// Args are added to the argv of a run with tools, after the model and
	// session arguments and before the system prompt and the prompt.
	Args []string `yaml:"args,omitempty"`

	// AllowArgs are the harness's allowlist for the server's tools, added after
	// Args: a harness run headless asks nobody, so a tool its permission policy
	// does not allow is refused rather than run, and a seat that was offered
	// its tools would find it could call none of them. Prefer a wildcard over
	// naming each tool, because a tool activated mid-run is not on an argv
	// written before it.
	AllowArgs []string `yaml:"allow_args,omitempty"`

	// Seed are files written into the call's working directory for a run
	// with tools — `in: work` on each, which is the only scope allowed (see
	// above) — after the profile's own seed_files.
	Seed []SeedFile `yaml:"seed,omitempty"`

	// Env is set on a run with tools, over every other layer of the child's
	// environment, so the harness's tool timeout the host is told of is the one
	// the harness applies. Never a credential and never an isolation variable.
	Env map[string]string `yaml:"env,omitempty"`

	// ToolTimeoutSeconds is how long the harness waits for one call of the
	// server's before it gives up on it and tells its model the call failed:
	// the value the profile sets through {tool_timeout_seconds} or
	// {tool_timeout_ms}, or the harness's own fixed timeout where it has no knob.
	// Required. The engine answers a call still running near it itself and
	// ends the run, handing the next run the call's result, because a harness
	// that gives up on a call the engine is still running sends its model to
	// make it again.
	ToolTimeoutSeconds int `yaml:"tool_timeout_seconds,omitempty"`

	// Relists declares that the harness re-reads the server's tool list when the
	// server says it changed, mid-run. One that does not never sees a tool
	// activated during the run, so the engine ends the run after an
	// activation and starts the next with the new list.
	Relists bool `yaml:"relists,omitempty"`

	// Concurrency is which of one response's calls the harness makes without
	// waiting for the one before it to be answered. Required, because no
	// value is safe for every harness: the engine holds a call that ends a phase
	// until the calls beside it in its response have run, and on a harness that
	// makes them in turn those calls never come until it is answered.
	Concurrency Concurrency `yaml:"concurrency,omitempty"`

	// FailureEvents are the harness's own failure reports in what it prints, and
	// the fields of each a failed run's error shows. A run with tools prints
	// every call's arguments and result, so its output is never quoted in an
	// error or a log line: what a failure says is stderr and these fields.
	FailureEvents []FailureEvent `yaml:"failure_events,omitempty"`

	// CompleteArgs, where set, is the argv of a run with tools in place of
	// the profile's complete_args — for a harness whose run against a server of
	// its own takes another subcommand, or a flag that excludes one a text
	// function passes (OpenCode's `run --server <url>` beside `run
	// --standalone`). Rendered like Args.
	CompleteArgs []string `yaml:"complete_args,omitempty"`

	// UsageArgs, where set, is the usage command of a run with tools in place
	// of the profile's usage_args, which it requires — for a harness whose run's
	// session is asked about through the server that held it. Rendered like
	// Args, with {session} as usage_args takes it, and given the run's
	// environment, mcp.env included.
	UsageArgs []string `yaml:"usage_args,omitempty"`

	// Output, where set, is what a run with tools prints in place of the
	// profile's output — for a harness whose text function is read best as the
	// plain answer it prints, while the record of a run with tools is the
	// JSON stream another flag of its prints instead (Grok's
	// --output-format streaming-json, Pi's --mode json), which mcp.args or
	// mcp.complete_args then pass. A run with tools reads its answer from
	// that record ([Requests]), never from the profile's text_paths.
	Output OutputMode `yaml:"output,omitempty"`

	// Lifecycle is how a run with tools is shaped: run, the default — the
	// harness is one process that starts the crewlet server itself — or serve.
	Lifecycle Lifecycle `yaml:"lifecycle,omitempty"`

	// Serve is the serve lifecycle: required with it, refused without it.
	Serve *Serve `yaml:"serve,omitempty"`
}

// Concurrency is which of one response's calls a harness makes without waiting for
// the one before it to be answered — measured per harness, by a response asking
// for several calls at once.
type Concurrency string

const (
	// ConcurrencyAtOnce is any call beside any other (Gemini CLI, OpenCode,
	// Copilot, Grok, Pi).
	ConcurrencyAtOnce Concurrency = "at_once"
	// ConcurrencyReadOnly is only calls to tools annotated read-only beside
	// each other, and any other call alone (Claude Code, Qwen Code).
	ConcurrencyReadOnly Concurrency = "read_only"
	// ConcurrencyInTurn is one call at a time, each once the last is
	// answered (Muse, Kimi, Hermes).
	ConcurrencyInTurn Concurrency = "in_turn"
)

// ConcurrencyValues are the values [MCP.Concurrency] takes, in order.
var ConcurrencyValues = []Concurrency{ConcurrencyAtOnce, ConcurrencyReadOnly, ConcurrencyInTurn}

// Valid reports whether c is one of [ConcurrencyValues]; the empty one is not.
func (c Concurrency) Valid() bool { return slices.Contains(ConcurrencyValues, c) }

// Serves reports whether a run with tools on this route runs against a server
// of the harness's own ([LifecycleServe]).
func (m *MCP) Serves() bool { return m != nil && m.Lifecycle == LifecycleServe }

// RouteOutput is what a run with tools prints: the route's [MCP.Output]
// where it sets one, the profile's own output otherwise.
func (p *Profile) RouteOutput() OutputMode {
	if p.MCP != nil && p.MCP.Output != "" {
		return p.MCP.Output
	}
	return p.EffectiveOutput()
}

// Routed is the profile as a run with tools reads what its harness printed: its
// output the route's ([Profile.RouteOutput]).
func (p Profile) Routed() Profile {
	p.Output = p.RouteOutput()
	return p
}

// Lifecycle is how a run with tools is shaped.
type Lifecycle string

const (
	// LifecycleRun is one harness process for the run, which starts the
	// crewlet server as its MCP server and drives its loop.
	LifecycleRun Lifecycle = "run"

	// LifecycleServe is a harness whose loop runs in a SERVER of its own: the
	// server is started first and starts the crewlet server; once it reports
	// itself up and lists the crewlet server as started, the run is a client
	// of it; and the server is stopped after the run — and with it, the
	// moment the engine ends the run, since its loop is the server's.
	// OpenCode 2.1 is the harness that needs it: its one-shot run mode does not
	// wait for a configured MCP server to start before asking its model.
	LifecycleServe Lifecycle = "serve"
)

// Valid reports whether l is a lifecycle this package knows, the empty one
// (run) included.
func (l Lifecycle) Valid() bool { return l == "" || l == LifecycleRun || l == LifecycleServe }

// Serve is how a serve-lifecycle harness's own server is started and waited for.
//
// The server is the profile's binary run with Args in the call's working
// directory and the run's environment — the tool route's seeds already
// written, the system prompt's channel too — and with its stdin closed. It is
// up once it has printed every Ready value and, where Poll is set, the poll
// shows the crewlet server started; both within timeout_seconds, which bounds
// its start as one stretch of its own. It is stopped — its group signalled, a
// grace, then killed — the moment the run is cut short (the host ending it, a
// stretch running out, a failure marker on either's stderr), and otherwise
// once the run's usage has been asked for.
type Serve struct {
	// Args start the server, with the tool route's placeholders but none of
	// the Ready ones, which it has not printed yet.
	Args []string `yaml:"args,omitempty"`

	// Ready are the values the server prints on stdout once it is up, each
	// the rest of a line beginning with its prefix, spaces trimmed — its
	// address, a password it minted. Each is the placeholder `{<as>}` in
	// mcp.complete_args, mcp.args, mcp.allow_args, mcp.env, mcp.usage_args
	// and poll.args. Never logged: a server's password is printed here.
	Ready []ReadyValue `yaml:"ready,omitempty"`

	// Poll is asked until the server lists the crewlet server as started —
	// for a harness whose run asks its model before a configured MCP server has
	// connected, and so offers it none of that server's tools.
	Poll *ServePoll `yaml:"poll,omitempty"`
}

// mintedByServer reports whether value is wholly one of the values the harness's
// server prints as it starts — `{<as>}` of a [Serve.Ready] entry — and so
// carries nothing the profile itself holds.
func (m *MCP) mintedByServer(value string) bool {
	if m == nil || m.Serve == nil {
		return false
	}
	for _, r := range m.Serve.Ready {
		if r.As != "" && value == "{"+r.As+"}" {
			return true
		}
	}
	return false
}

// ReadyValue is one value a server prints when it is up.
type ReadyValue struct {
	// Prefix is what the line begins with.
	Prefix string `yaml:"prefix,omitempty"`
	// As names the placeholder, without its braces: a lower-case word.
	As string `yaml:"as,omitempty"`
}

// ServePoll is a command that prints one JSON document listing the server's MCP
// servers, asked every half second until the crewlet server's entry is
// there and no longer starting. A poll that fails or prints something else is
// asked again: a server that has printed its address may not answer yet.
type ServePoll struct {
	// Args are the command's, run like usage_args: the profile's binary, in
	// the call's working directory and the run's environment.
	Args []string `yaml:"args,omitempty"`
	// Items is the array of MCP servers in the document.
	Items Path `yaml:"items,omitempty"`
	// Match picks the crewlet server's entry out of it.
	Match []Match `yaml:"match,omitempty"`
	// Starting holds of the entry while it is still starting. Empty: the
	// entry is started once it is listed.
	Starting []Match `yaml:"starting,omitempty"`
}

// readyNames is the placeholders a serve lifecycle's ready values add.
func (m *MCP) readyNames() []string {
	if m.Serve == nil {
		return nil
	}
	var out []string
	for _, r := range m.Serve.Ready {
		out = append(out, "{"+r.As+"}")
	}
	return out
}

// ReadyPlaceholder reports whether s carries a placeholder only a serve
// lifecycle's server can fill — so it cannot be rendered before the server is
// up.
func (m *MCP) ReadyPlaceholder(s string) bool { return carriesAny(s, m.readyNames()) }

// FailureEvent is one kind of failure report a harness prints as an event of its
// own, and what of it to show.
type FailureEvent struct {
	// Match picks the event out — see [Match].
	Match []Match `yaml:"match,omitempty"`
	// Fields are shown, by path, each that resolves in the event.
	Fields []Path `yaml:"fields,omitempty"`
}

// Match is one condition an event must meet: the value at Path equals Equals,
// compared as JSON values — a string with a string, a boolean with a boolean, a
// number with a number — or, where Prefix is set in its place, is a string
// that begins with Prefix.
type Match struct {
	Path   Path `yaml:"path,omitempty"`
	Equals any  `yaml:"equals,omitempty"`
	// Prefix is for a discriminator a harness writes with what it names
	// appended to it: Muse Code files a side effect under an operation of
	// `tool:<the tool's name>` beside the `model.meta.response` of a model
	// request, and no one value tells every tool's from the model's.
	Prefix string `yaml:"prefix,omitempty"`
}

// ToolTimeout is [MCP.ToolTimeoutSeconds] as a duration.
func (m *MCP) ToolTimeout() time.Duration {
	return time.Duration(m.ToolTimeoutSeconds) * time.Second
}

// MCPValues are what a run's placeholders stand for.
type MCPValues struct {
	RelayCommand string
	RelayArgs    []string
	Socket       string
	Work         string
	// Ready is a serve lifecycle's ready values, by their `as`, once its
	// server has printed them.
	Ready map[string]string
}

// Render substitutes a run's values into one profile string.
func (m *MCP) Render(s string, v MCPValues) string {
	if !strings.Contains(s, "{") {
		return s
	}
	argv := append([]string{v.RelayCommand}, v.RelayArgs...)
	pairs := []string{
		"{relay_command_json}", jsonValue(v.RelayCommand),
		"{relay_args_json}", jsonValue(v.RelayArgs),
		"{relay_argv_json}", jsonValue(argv),
		"{relay_command}", v.RelayCommand,
		"{socket}", v.Socket,
		"{tool_timeout_seconds}", strconv.Itoa(m.ToolTimeoutSeconds),
		"{tool_timeout_ms}", strconv.Itoa(m.ToolTimeoutSeconds * 1000),
		"{work}", v.Work,
	}
	for _, name := range slices.Sorted(maps.Keys(v.Ready)) {
		pairs = append(pairs, "{"+name+"}", v.Ready[name])
	}
	return strings.NewReplacer(pairs...).Replace(s)
}

// jsonValue encodes v WITHOUT HTML escaping: `<`, `>` and `&` stay as they are,
// so the result is a valid TOML value too, which `<` would also be — but
// it is not what a path looks like in a log line.
func jsonValue(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	// A string or a slice of strings always encodes.
	_ = enc.Encode(v)
	return strings.TrimSuffix(b.String(), "\n")
}

// relayPlaceholders are the placeholders that carry the run's server,
// commandPlaceholders the ones among them that name what to start, and
// mcpPlaceholders every one the tool route substitutes.
var (
	relayPlaceholders = []string{
		"{relay_command}", "{relay_command_json}", "{relay_args_json}", "{relay_argv_json}", "{socket}",
	}
	commandPlaceholders = []string{"{relay_command}", "{relay_command_json}", "{relay_argv_json}"}
	mcpPlaceholders     = append(slices.Clone(relayPlaceholders),
		"{tool_timeout_seconds}", "{tool_timeout_ms}", "{work}")
)

// carriesAny reports whether s carries one of the placeholders.
func carriesAny(s string, placeholders []string) bool {
	return slices.ContainsFunc(placeholders, func(ph string) bool { return strings.Contains(s, ph) })
}

// rendered is every string of the block that can tell the harness its server, for
// the check that one does.
func (m *MCP) rendered() []string {
	out := slices.Concat(m.CompleteArgs, m.Args, m.AllowArgs)
	if m.Serve != nil {
		out = append(out, m.Serve.Args...)
	}
	for _, f := range m.Seed {
		out = append(out, f.Content)
	}
	for _, name := range slices.Sorted(maps.Keys(m.Env)) {
		out = append(out, m.Env[name])
	}
	return out
}

// clone deep-copies the block, because overrides replace its lists in place.
func (m *MCP) clone() *MCP {
	if m == nil {
		return nil
	}
	out := *m
	out.Args = slices.Clone(m.Args)
	out.AllowArgs = slices.Clone(m.AllowArgs)
	out.CompleteArgs = slices.Clone(m.CompleteArgs)
	out.UsageArgs = slices.Clone(m.UsageArgs)
	out.Seed = slices.Clone(m.Seed)
	out.Env = cloneMap(m.Env)
	if m.Serve != nil {
		srv := Serve{Args: slices.Clone(m.Serve.Args), Ready: slices.Clone(m.Serve.Ready)}
		if poll := m.Serve.Poll; poll != nil {
			srv.Poll = &ServePoll{
				Args: slices.Clone(poll.Args), Items: slices.Clone(poll.Items),
				Match: cloneMatches(poll.Match), Starting: cloneMatches(poll.Starting),
			}
		}
		out.Serve = &srv
	}
	out.FailureEvents = make([]FailureEvent, len(m.FailureEvents))
	for i, e := range m.FailureEvents {
		out.FailureEvents[i] = FailureEvent{Match: cloneMatches(e.Match), Fields: clonePaths(e.Fields)}
	}
	if m.FailureEvents == nil {
		out.FailureEvents = nil
	}
	return &out
}

func cloneMatches(in []Match) []Match {
	if in == nil {
		return nil
	}
	out := make([]Match, len(in))
	for i, m := range in {
		out[i] = Match{Path: slices.Clone(m.Path), Equals: m.Equals, Prefix: m.Prefix}
	}
	return out
}

// validateMCP holds the tool route to the shapes in which it hands the harness its
// server: a server it is told how to start, a timeout the host can plan
// around, seeds only where the run alone reads them, and environment that
// neither carries a credential nor undoes the isolation.
func (p *Profile) validateMCP(add func(string, ...any)) {
	m := p.MCP
	if m == nil {
		return
	}
	all := m.rendered()
	if !slices.ContainsFunc(all, func(s string) bool { return carriesAny(s, commandPlaceholders) }) {
		// Without the command the harness is never told what to start, and every
		// run with tools is a run that never reached them.
		add("mcp names no relay command — put {relay_command}, {relay_command_json} or " +
			"{relay_argv_json} in mcp.args, mcp.seed or mcp.env, where the harness reads the " +
			"server it starts")
	}
	if !slices.ContainsFunc(all, func(s string) bool {
		return carriesAny(s, []string{"{relay_args_json}", "{relay_argv_json}", "{socket}"})
	}) {
		add("mcp names no relay arguments — the relay is told its socket on its argv, so " +
			"{relay_args_json}, {relay_argv_json} or {socket} has to reach the harness's server " +
			"configuration")
	}
	if !m.Concurrency.Valid() {
		add("mcp.concurrency is %q — say which of one response's calls the harness makes without "+
			"waiting for the one before: at_once, read_only (only calls to read-only tools, as "+
			"Claude Code) or in_turn (one at a time). The engine holds a call that ends a phase "+
			"until the calls beside it have run, and on a harness that makes them in turn those "+
			"never come until it is answered", m.Concurrency)
	}
	if m.ToolTimeoutSeconds <= 0 {
		add("mcp.tool_timeout_seconds is %d — say how long the harness waits for one call, the "+
			"value the profile sets or the harness's own fixed one: a call still running near it "+
			"is answered by the engine first, and with no value nothing is", m.ToolTimeoutSeconds)
	}
	// What the run itself is handed may carry a server's ready values; what
	// is written or started before the server is up may not.
	afterUp := slices.Concat(mcpPlaceholders, m.readyNames())
	for _, field := range []struct {
		name    string
		args    []string
		allowed []string
	}{
		{"mcp.complete_args", m.CompleteArgs, afterUp},
		{"mcp.args", m.Args, afterUp},
		{"mcp.allow_args", m.AllowArgs, afterUp},
		{"mcp.usage_args", m.UsageArgs, append(slices.Clone(afterUp), "{session}")},
	} {
		for i, arg := range field.args {
			if problem := unknownPlaceholder(arg, field.allowed); problem != "" {
				add("%s[%d] %s", field.name, i, problem)
			}
		}
	}
	if len(m.UsageArgs) > 0 {
		switch {
		case len(p.UsageArgs) == 0:
			add("mcp.usage_args is set but usage_args is not — it replaces the profile's usage " +
				"command on a run with tools, and a profile with none asks for no report")
		case !hasPlaceholder(m.UsageArgs, "{session}"):
			add("mcp.usage_args carries no {session} placeholder — the report has to be asked " +
				"about the run's own session")
		}
	}
	for i, f := range m.Seed {
		label := fmt.Sprintf("mcp.seed[%d]", i)
		switch {
		case f.In != SeedWork:
			// Written explicitly, because a seed_files entry with no `in`
			// lands in the home, and a reader comparing the two lists would
			// take this one for the same.
			add("%s.in is %q — a run's server is written into the call's own working "+
				"directory (in: work): a seat's home is shared by every call of the seat at "+
				"once and seeded once per generation, so a socket written there would hand "+
				"one run another's", label, f.In)
		case strings.TrimSpace(f.Path) == "":
			add("%s.path is empty", label)
		case !staysInside(f.Path):
			add("%s.path %q must be relative and stay inside the call's working directory", label, f.Path)
		case !namesAFile(f.Path):
			add("%s.path %q must name a FILE inside the call's working directory — not the "+
				"directory itself, not a path ending in a separator, and not one whose last "+
				"element is . or ..", label, f.Path)
		}
		if strings.TrimSpace(f.Content) == "" {
			add("%s.content is empty — a server configuration with nothing in it configures nothing", label)
		}
		if problem := m.beforeUpPlaceholder(f.Content); problem != "" {
			add("%s.content %s", label, problem)
		}
	}
	isolation := p.IsolationVariables()
	for _, name := range slices.Sorted(maps.Keys(m.Env)) {
		switch {
		case strings.TrimSpace(name) == "":
			add("mcp.env names an empty variable")
		case slices.Contains(isolation, name):
			add("mcp.env names %s, which the engine sets to keep the harness inside its seat's "+
				"home and its call's working directory — a value here would replace that "+
				"isolation on every run with tools", name)
		case strings.TrimSpace(m.Env[name]) == "":
			add("mcp.env.%s is empty — give the value the harness reads, or drop it", name)
		}
		if problem := unknownPlaceholder(m.Env[name], afterUp); problem != "" {
			add("mcp.env.%s %s", name, problem)
		}
	}
	p.validateServe(add)
	for i, e := range m.FailureEvents {
		label := fmt.Sprintf("mcp.failure_events[%d]", i)
		validateMatches(add, label+".match", e.Match)
		if len(e.Fields) == 0 {
			add("%s.fields is empty — name the fields of the harness's failure report a failed "+
				"run's error shows", label)
		}
		for j, f := range e.Fields {
			validatePath(add, fmt.Sprintf("%s.fields[%d]", label, j), f)
		}
	}
	if m.Output != "" && !m.Output.Valid() {
		add("mcp.output %q (want json, jsonl or text)", m.Output)
	}
	if len(m.FailureEvents) > 0 && p.RouteOutput() != OutputJSONL {
		add("mcp.failure_events is set but a run with tools prints %q — a failure event exists "+
			"only in a jsonl stream (mcp.output, or output)", p.RouteOutput())
	}
}

// beforeUpPlaceholder is [unknownPlaceholder] for a string rendered before a
// serve lifecycle's server is up — a seed, which it reads as it starts, and its
// own arguments — naming a ready value as what it is.
func (m *MCP) beforeUpPlaceholder(s string) string {
	for _, name := range m.readyNames() {
		if strings.Contains(s, name) {
			return fmt.Sprintf("carries %s, a value the server prints once it is up — this is "+
				"rendered before the server starts, so it would reach it unsubstituted", name)
		}
	}
	return unknownPlaceholder(s, mcpPlaceholders)
}

// validateServe holds the lifecycle to a server that is told how to start and
// how it is known to be up, and to values the rest of the route can be handed.
func (p *Profile) validateServe(add func(string, ...any)) {
	m := p.MCP
	switch {
	case !m.Lifecycle.Valid():
		add("mcp.lifecycle %q (want run or serve)", m.Lifecycle)
		return
	case m.Lifecycle == LifecycleServe && m.Serve == nil:
		add("mcp.lifecycle is serve but mcp.serve is not set — say how the harness's server is " +
			"started and how it is known to be up")
		return
	case m.Lifecycle != LifecycleServe && m.Serve != nil:
		add("mcp.serve is set but mcp.lifecycle is %q — set lifecycle: serve, or drop serve", m.Lifecycle)
		return
	case m.Serve == nil:
		return
	}
	s := m.Serve
	if len(s.Args) == 0 {
		add("mcp.serve.args is empty — give the arguments that start the harness's server")
	}
	for i, arg := range s.Args {
		if problem := m.beforeUpPlaceholder(arg); problem != "" {
			add("mcp.serve.args[%d] %s", i, problem)
		}
	}
	if len(s.Ready) == 0 && s.Poll == nil {
		add("mcp.serve names neither ready nor poll — nothing would say the server is up, and " +
			"the run would race its start")
	}
	reserved := slices.Concat(mcpPlaceholders, []string{"{session}"})
	seen := map[string]bool{}
	for i, r := range s.Ready {
		label := fmt.Sprintf("mcp.serve.ready[%d]", i)
		name := "{" + r.As + "}"
		if strings.TrimSpace(r.Prefix) == "" {
			add("%s.prefix is empty — every line would match it", label)
		}
		switch {
		case !isPlaceholderShaped(name):
			add("%s.as %q is not a lower-case word (letters and underscores)", label, r.As)
		case slices.Contains(reserved, name):
			add("%s.as %q is a placeholder the tool route already substitutes — name it something else", label, r.As)
		case seen[name]:
			add("%s.as %q names a value an earlier entry already does", label, r.As)
		}
		seen[name] = true
	}
	if poll := s.Poll; poll != nil {
		if len(poll.Args) == 0 {
			add("mcp.serve.poll.args is empty — give the command that lists the server's MCP servers")
		}
		afterUp := slices.Concat(mcpPlaceholders, m.readyNames())
		for i, arg := range poll.Args {
			if problem := unknownPlaceholder(arg, afterUp); problem != "" {
				add("mcp.serve.poll.args[%d] %s", i, problem)
			}
		}
		validatePath(add, "mcp.serve.poll.items", poll.Items)
		validateMatches(add, "mcp.serve.poll.match", poll.Match)
		if len(poll.Starting) > 0 {
			validateMatches(add, "mcp.serve.poll.starting", poll.Starting)
		}
	}
}

// unknownPlaceholder reports a `{name}` in s that is not one of allowed, or ""
// when there is none. A misspelt placeholder would reach the harness verbatim,
// which starts no server.
func unknownPlaceholder(s string, allowed []string) string {
	rest := s
	for {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			return ""
		}
		end := strings.IndexByte(rest[open:], '}')
		if end < 0 {
			return ""
		}
		name := rest[open : open+end+1]
		// A JSON or TOML document's own braces enclose quotes, colons or
		// spaces; a placeholder is a bare word.
		if isPlaceholderShaped(name) && !slices.Contains(allowed, name) {
			return fmt.Sprintf("carries %s, which is no placeholder the tool route substitutes "+
				"here (it has %s)", name, strings.Join(allowed, ", "))
		}
		rest = rest[open+1:]
	}
}

// isPlaceholderShaped reports whether a `{…}` is a bare lower-case word with
// underscores — the shape every placeholder has, and no JSON object does.
func isPlaceholderShaped(s string) bool {
	inner := strings.TrimSuffix(strings.TrimPrefix(s, "{"), "}")
	if inner == "" {
		return false
	}
	for _, r := range inner {
		if (r < 'a' || r > 'z') && r != '_' {
			return false
		}
	}
	return true
}

// validateMatches holds a list of conditions to the shape that picks an event
// out: at least one, each a path to a scalar.
func validateMatches(add func(string, ...any), label string, matches []Match) {
	if len(matches) == 0 {
		add("%s is empty — say which events these are, e.g. [{path: [type], equals: result}]", label)
	}
	for i, m := range matches {
		validatePath(add, fmt.Sprintf("%s[%d].path", label, i), m.Path)
		if m.Prefix != "" {
			if m.Equals != nil {
				add("%s[%d] sets both equals and prefix — a condition is one or the other", label, i)
			}
			continue
		}
		switch m.Equals.(type) {
		case string, bool, int, int64, uint64, float64:
		default:
			add("%s[%d].equals is %v — compare with a string, a boolean or a number, or "+
				"set prefix", label, i, m.Equals)
		}
	}
}

// validatePath holds one path that reads one value to its shape: non-empty,
// and without [PathEvery], which only a usage count may take.
func validatePath(add func(string, ...any), label string, path Path) {
	switch {
	case len(path) == 0:
		add("%s is empty", label)
	case path.HasEvery():
		add("%s %q takes %q, which only a usage path may: %s reads one value",
			label, path.String(), PathEvery, label)
	}
}

// Cap is the shape of a run the harness's OWN turn cap ended — the model still
// working, the harness no longer asking it — which is an answer of what the run
// did, not a failure: a Completion stopped at the cap.
//
// Read on a run with tools. A text function is one answer, and a capped one is
// the failure it always was.
//
// Both conditions where both are set: the exit status, and an event (or the
// document) matching every condition. A harness that reports nothing in its output
// at a cap is told by its exit status alone; one that exits as it does on any
// failure is told by what it printed.
type Cap struct {
	// ExitCode is the status a capped run exits with. Nil: any.
	ExitCode *int `yaml:"exit_code,omitempty"`
	// Match is what a capped run prints — see [Match]. Empty: nothing to
	// read, the exit status alone.
	Match []Match `yaml:"match,omitempty"`
}

func (c *Cap) clone() *Cap {
	if c == nil {
		return nil
	}
	out := Cap{Match: cloneMatches(c.Match)}
	if c.ExitCode != nil {
		code := *c.ExitCode
		out.ExitCode = &code
	}
	return &out
}

func (p *Profile) validateCap(add func(string, ...any)) {
	c := p.Cap
	if c == nil {
		return
	}
	if c.ExitCode == nil && len(c.Match) == 0 {
		add("cap names neither exit_code nor match — say how a run the harness's own turn cap " +
			"ended is told from any other")
	}
	if len(c.Match) > 0 {
		validateMatches(add, "cap.match", c.Match)
		if p.RouteOutput() == OutputText {
			add("cap.match is set but a run with tools prints text — there is no document to " +
				"match; use cap.exit_code")
		}
	}
}

// RequestSource is where a harness's record of its model requests is read from.
type RequestSource string

const (
	// SourceStdout reads the record from the run's stdout, line by line.
	SourceStdout RequestSource = "stdout"
	// SourceFile tails a file the harness writes as it runs.
	SourceFile RequestSource = "file"
)

// Valid reports whether s is a source this package knows.
func (s RequestSource) Valid() bool { return s == SourceStdout || s == SourceFile }

// Requests is how a harness's own record of a run shows each MODEL REQUEST it made
// — its prose, the calls it asked for, which model answered, why it stopped
// and what it cost — read while the run goes.
//
// The engine sees every call the harness makes to its server and nothing else: not
// the requests between them, not their prose, not what each cost. A round of a
// phase is one model request, the round budget counts them, and the budget is
// charged as each one's usage is read, so a phase is gated while it runs
// rather than once at its end. A harness whose record shows no requests — this
// block absent — is not refused: the whole run counts as one round, and its
// cost is charged when it ends.
//
// # How the record is read
//
// Each line of the record is one JSON event. Every rule in Events whose Match
// holds is applied to it, in order:
//
//   - opens begins a new request; an event with an id belongs to the request
//     of that id, opened by it if none is; any other event belongs to the
//     request open now;
//   - closes ends the request open now, once the rest of the rule is applied.
//     A record whose rules close requests marks where each one ENDS rather
//     than where it begins — Codex's rollout ends a response with its usage,
//     Qwen Code's stream ends one with the results of its calls — so in such a
//     record an event that reads into the request open now, while none is,
//     begins the next one;
//   - id, model, stop and usage set the request's (a usage figure takes the
//     LAST value read for that request, so a stream's running count works);
//   - text appends to the request's prose and delta streams text as it is
//     written; call reads one call; blocks reads an array of content blocks,
//     each by the first of its rules that matches;
//   - whole says the event carries its request WHOLE, as it stands now, so
//     what it reads replaces the prose and calls earlier events of the request
//     read rather than adding to them — for a harness that writes a response's
//     entry again as it grows (Gemini CLI, which adds a response's calls to its
//     entry once they have run);
//   - tools is the list of tools the harness offers its model, which the doctor
//     reports.
//
// # When a request is told to the host
//
// As soon as the record shows it saying something — its prose or a call — so
// the host has it before the calls it asked for arrive, and places each call
// on the request that asked for it. A record that shows a request's calls only
// once they have RUN (LateCalls) is told one request at a time once the record
// has moved past it instead: told as read, the host would hold a call it is
// about to run for a request it has already been told asked for none.
type Requests struct {
	// Source is stdout or file.
	Source RequestSource `yaml:"source,omitempty"`

	// File is a glob the harness's record is found by, for source file, relative
	// to In — `{session}` substituted with the run's session id
	// ([Profile.SessionArgs]). A glob under the seat's home must name the
	// run's own file through {session}: the home is shared by every call of
	// the seat at once.
	File string `yaml:"file,omitempty"`
	// In is the root File is relative to: home or work.
	In SeedScope `yaml:"in,omitempty"`

	// Events are the rules every line is read by.
	Events []RequestEvent `yaml:"events,omitempty"`

	// Stops maps the record's stop reasons to the engine's: end, a response
	// the model finished, and tool_use, one that ended to let its calls run.
	// A reason not named is reported as none.
	Stops map[string]string `yaml:"stops,omitempty"`

	// CallIDMeta is the key of a `tools/call` request's `_meta` the harness
	// hands the server its own id for the call under — the id the record's
	// call paths read — so the engine joins a call to the request that asked
	// for it by id: `claudecode/toolUseId` for Claude Code, `callId` for
	// Codex and Muse Code. Empty: the harness sends none, and a call is joined
	// by its name and arguments.
	CallIDMeta string `yaml:"call_id_meta,omitempty"`

	// HostPrefix is how the harness spells one of the server's tools to its
	// model — `mcp__crewlet__` for `mcp__crewlet__search`. A call so named is
	// the host's, reported by its own name; any other is one of the harness's own
	// tools (its web search, its fetch).
	HostPrefix string `yaml:"host_prefix,omitempty"`

	// HostNamespace is the namespace the harness files the server's tools under,
	// for a harness whose record keeps a call's namespace apart from its name
	// ([CallPaths.Namespace]) — Codex's rollout records `mcp__crewlet` beside
	// `search`. A call in it is the host's, reported by its name; any other
	// is one of the harness's own.
	HostNamespace string `yaml:"host_namespace,omitempty"`

	// Owner says which file is this run's record, for a file record under
	// the seat's home whose name the engine does not choose — see
	// [RecordOwner]. A glob there that names the run's own file through
	// {session} needs none.
	Owner *RecordOwner `yaml:"owner,omitempty"`

	// LateCalls declares that the record shows a request's calls only once
	// they have run — OpenCode prints a call as it completes, Gemini CLI
	// writes a response's calls into its entry after running them — so each
	// request is told to the host once the record has moved past it (a rule
	// closed it, another request began, or the run ended) rather than as it
	// is read. Its usage is still charged as it is read.
	LateCalls bool `yaml:"late_calls,omitempty"`
}

// RecordOwner is how a file record says whose it is: the value at Path in the
// file's FIRST line — or, where Match is set, in the first line that matches
// it. A file the glob finds whose line holds anything else there is another
// run's, and is passed over.
//
// For a harness that keeps every run's record side by side in the seat's home —
// which every concurrent call of the seat shares — under a name the engine does
// not choose: Codex names its rollout after a thread id it mints itself, and
// Gemini CLI names its chat after the first eight characters of the session
// id, which no glob can spell.
type RecordOwner struct {
	// Match picks the line the value is read from: the first line of the
	// file that matches every condition — for a harness whose record names its
	// run only in a later event (Kimi Code's wire log opens with its
	// protocol version and names the call's working directory in the
	// profile.bind event after it). Empty: the file's first line.
	Match []Match `yaml:"match,omitempty"`
	// Path is the value's place in that line.
	Path Path `yaml:"path,omitempty"`
	// Equals is what it holds in this run's file: {session}, the id
	// session_args handed the run, or {work}, the call's working directory.
	Equals string `yaml:"equals,omitempty"`
}

// RequestEvent is one rule a line of the record is read by — see [Requests].
type RequestEvent struct {
	Match  []Match    `yaml:"match,omitempty"`
	Opens  bool       `yaml:"opens,omitempty"`
	Closes bool       `yaml:"closes,omitempty"`
	Whole  bool       `yaml:"whole,omitempty"`
	ID     Path       `yaml:"id,omitempty"`
	Model  Path       `yaml:"model,omitempty"`
	Text   Path       `yaml:"text,omitempty"`
	Delta  Path       `yaml:"delta,omitempty"`
	Call   *CallPaths `yaml:"call,omitempty"`
	Blocks *Blocks    `yaml:"blocks,omitempty"`
	Stop   Path       `yaml:"stop,omitempty"`
	Usage  UsagePaths `yaml:"usage,omitempty"`
	Tools  Path       `yaml:"tools,omitempty"`
}

// ReadsRequest reports whether the rule reads anything INTO a request — as
// against only the tools on offer, or only where one ends — which is what
// begins the next request in a record whose rules close them.
func (e RequestEvent) ReadsRequest() bool {
	return len(e.Model) > 0 || len(e.Text) > 0 || len(e.Delta) > 0 || e.Call != nil ||
		e.Blocks != nil || len(e.Stop) > 0 || e.Usage.any()
}

// CallPaths locate one call: the harness's id for it, the tool's name, its
// arguments — an object, or one encoded as a JSON string — and, for a harness
// that keeps it apart from the name, the namespace the tool is filed under
// ([Requests.HostNamespace]).
type CallPaths struct {
	ID        Path `yaml:"id,omitempty"`
	Name      Path `yaml:"name,omitempty"`
	Args      Path `yaml:"args,omitempty"`
	Namespace Path `yaml:"namespace,omitempty"`
}

// Blocks is an array of content blocks in an event, and the rules each block is
// read by — the first whose Match holds.
type Blocks struct {
	Path Path        `yaml:"path,omitempty"`
	Each []BlockRule `yaml:"each,omitempty"`
}

// BlockRule reads one kind of content block: prose, or a call. An empty Match
// holds of every block — for an array that holds one kind alone, such as the
// list of calls Gemini CLI and Copilot record beside a response's prose.
type BlockRule struct {
	Match []Match    `yaml:"match,omitempty"`
	Text  Path       `yaml:"text,omitempty"`
	Call  *CallPaths `yaml:"call,omitempty"`
}

// StopValues are the engine's stop reasons a [Requests.Stops] entry may name,
// in its own spelling — the backend maps each onto the contract's, and its
// suite holds the two lists to each other.
var StopValues = []string{"end", "tool_use"}

func (r *Requests) clone() *Requests {
	if r == nil {
		return nil
	}
	out := *r
	out.Stops = cloneMap(r.Stops)
	if r.Owner != nil {
		owner := RecordOwner{Match: cloneMatches(r.Owner.Match), Path: slices.Clone(r.Owner.Path), Equals: r.Owner.Equals}
		out.Owner = &owner
	}
	if r.Events != nil {
		out.Events = make([]RequestEvent, len(r.Events))
		for i, e := range r.Events {
			out.Events[i] = e.clone()
		}
	}
	return &out
}

func (e RequestEvent) clone() RequestEvent {
	out := RequestEvent{
		Match: cloneMatches(e.Match), Opens: e.Opens, Closes: e.Closes, Whole: e.Whole,
		ID: slices.Clone(e.ID), Model: slices.Clone(e.Model), Text: slices.Clone(e.Text),
		Delta: slices.Clone(e.Delta), Stop: slices.Clone(e.Stop), Tools: slices.Clone(e.Tools),
		Usage: e.Usage.clone(), Call: e.Call.clone(),
	}
	if e.Blocks != nil {
		b := Blocks{Path: slices.Clone(e.Blocks.Path)}
		if e.Blocks.Each != nil {
			b.Each = make([]BlockRule, len(e.Blocks.Each))
			for i, rule := range e.Blocks.Each {
				b.Each[i] = BlockRule{Match: cloneMatches(rule.Match), Text: slices.Clone(rule.Text),
					Call: rule.Call.clone()}
			}
		}
		out.Blocks = &b
	}
	return out
}

func (c *CallPaths) clone() *CallPaths {
	if c == nil {
		return nil
	}
	return &CallPaths{ID: slices.Clone(c.ID), Name: slices.Clone(c.Name), Args: slices.Clone(c.Args),
		Namespace: slices.Clone(c.Namespace)}
}

func (u UsagePaths) clone() UsagePaths {
	return UsagePaths{
		Input: clonePaths(u.Input), Output: clonePaths(u.Output),
		CacheRead: clonePaths(u.CacheRead), CacheWrite: clonePaths(u.CacheWrite),
		Reasoning: clonePaths(u.Reasoning), ToolPrompt: clonePaths(u.ToolPrompt),
		InputIncludesCache: u.InputIncludesCache,
	}
}

// any reports whether the usage paths name any figure at all.
func (u UsagePaths) any() bool {
	return len(u.Input)+len(u.Output)+len(u.CacheRead)+len(u.CacheWrite)+
		len(u.Reasoning)+len(u.ToolPrompt) > 0
}

// validateRequests holds the request record to the shapes in which it can be
// read: a source, rules that each read something, and calls the host can tell
// its own from.
func (p *Profile) validateRequests(add func(string, ...any)) {
	r := p.Requests
	if r == nil {
		return
	}
	if p.MCP == nil {
		// Requests are read on a run with tools, which a profile with no tool
		// route never makes.
		add("requests is set but mcp is not — the record is read on a run with tools, and " +
			"this profile cannot be handed any")
	}
	switch {
	case !r.Source.Valid():
		add("requests.source %q (want stdout or file)", r.Source)
	case r.Source == SourceStdout:
		if r.File != "" || r.In != "" {
			add("requests.file or requests.in is set but requests.source is stdout — they say " +
				"where a file source is found")
		}
		if p.RouteOutput() != OutputJSONL {
			add("requests.source is stdout but a run with tools prints %q — a record of requests "+
				"is a jsonl stream (mcp.output, or output)", p.RouteOutput())
		}
	case r.Source == SourceFile:
		switch {
		case strings.TrimSpace(r.File) == "":
			add("requests.source is file but requests.file is empty — give the glob the " +
				"record is found by")
		case !staysInside(strings.ReplaceAll(r.File, "{session}", "session")):
			add("requests.file %q must be relative and stay inside its root", r.File)
		}
		switch {
		case !r.In.Valid():
			add("requests.in %q (want home or work)", r.In)
		case r.In == SeedHome && !strings.Contains(r.File, "{session}") && r.Owner == nil:
			add("requests.file %q is under the seat's home and does not name the run's own "+
				"file through {session} — the home is shared by every call of the seat at once, "+
				"so the glob could find another run's record; name it, or say how the file "+
				"names its run (requests.owner)", r.File)
		}
		if strings.Contains(r.File, "{session}") && !hasPlaceholder(p.SessionArgs, "{session}") {
			add("requests.file names {session} but session_args does not give the run one — " +
				"declare session_args with {session}")
		}
	}
	p.validateOwner(add)
	if len(r.Events) == 0 {
		add("requests.events is empty — give the rules a line of the record is read by")
	}
	opens, closes, calls, namespaced := false, false, false, false
	for i, e := range r.Events {
		label := fmt.Sprintf("requests.events[%d]", i)
		validateMatches(add, label+".match", e.Match)
		if !e.Opens && !e.Closes && len(e.ID) == 0 && len(e.Tools) == 0 && !e.ReadsRequest() {
			add("%s reads nothing — give it opens, closes, id, model, text, delta, call, blocks, "+
				"stop, usage or tools", label)
		}
		if e.Whole && len(e.ID) == 0 {
			// What a whole event replaces is its OWN request's, which only an
			// id names: read into the request open now, it would wipe whichever
			// request that happened to be.
			add("%s is whole but names no id — a whole event replaces what its own request "+
				"read, and only an id says which request that is", label)
		}
		opens = opens || e.Opens || len(e.ID) > 0
		closes = closes || e.Closes
		namespaced = namespaced || e.Call.namespaced() || e.Blocks.namespaced()
		for _, f := range []struct {
			name string
			path Path
		}{{"id", e.ID}, {"model", e.Model}, {"text", e.Text}, {"delta", e.Delta}, {"stop", e.Stop}, {"tools", e.Tools}} {
			if len(f.path) > 0 {
				validatePath(add, label+"."+f.name, f.path)
			}
		}
		if e.Call != nil {
			calls = true
			e.Call.validate(add, label+".call")
		}
		if b := e.Blocks; b != nil {
			validatePath(add, label+".blocks.path", b.Path)
			if len(b.Each) == 0 {
				add("%s.blocks.each is empty — give the rules a content block is read by", label)
			}
			for j, rule := range b.Each {
				rl := fmt.Sprintf("%s.blocks.each[%d]", label, j)
				switch {
				case len(rule.Match) > 0:
					validateMatches(add, rl+".match", rule.Match)
				case j < len(b.Each)-1:
					// Every block is read by the first rule that holds, so
					// one that holds of every block leaves the rules after
					// it nothing to read.
					add("%s matches every block, so the rules after it never read one — give "+
						"it a match, or put it last", rl)
				}
				switch {
				case len(rule.Text) > 0 && rule.Call != nil:
					add("%s reads both text and a call — a block is one or the other", rl)
				case len(rule.Text) > 0:
					validatePath(add, rl+".text", rule.Text)
				case rule.Call != nil:
					calls = true
					rule.Call.validate(add, rl+".call")
				default:
					add("%s reads nothing — give it text or call", rl)
				}
			}
		}
	}
	if !opens && !closes {
		add("requests.events neither opens a request, names one by id, nor closes one — no " +
			"line of the record would ever begin a request")
	}
	switch {
	case calls && strings.TrimSpace(r.HostPrefix) == "" && strings.TrimSpace(r.HostNamespace) == "":
		add("requests reads calls but neither requests.host_prefix nor requests.host_namespace " +
			"is set — say how the harness spells one of the server's tools to its model, e.g. " +
			"mcp__crewlet__, so a call to it is told from one to the harness's own tools")
	case namespaced && strings.TrimSpace(r.HostNamespace) == "":
		add("requests reads a call's namespace but requests.host_namespace is empty — say " +
			"which namespace the harness files the server's tools under, e.g. mcp__crewlet")
	case !namespaced && strings.TrimSpace(r.HostNamespace) != "":
		add("requests.host_namespace is set but no call rule reads a namespace — give the " +
			"call its namespace path, or drop host_namespace")
	}
	for _, from := range slices.Sorted(maps.Keys(r.Stops)) {
		if !slices.Contains(StopValues, r.Stops[from]) {
			add("requests.stops.%s is %q, which is not one of %s", from, r.Stops[from], words.Or(StopValues))
		}
	}
}

// validateOwner holds a file record's owner to a value the run knows before
// the file exists: its session, where session_args hands it one, or its
// working directory.
func (p *Profile) validateOwner(add func(string, ...any)) {
	o := p.Requests.Owner
	if o == nil {
		return
	}
	if p.Requests.Source != SourceFile {
		add("requests.owner is set but requests.source is %q — it says which file is the "+
			"run's, and a stdout record is the run's own", p.Requests.Source)
	}
	if len(o.Match) > 0 {
		validateMatches(add, "requests.owner.match", o.Match)
	}
	validatePath(add, "requests.owner.path", o.Path)
	switch o.Equals {
	case "{work}":
	case "{session}":
		if !hasPlaceholder(p.SessionArgs, "{session}") {
			add("requests.owner.equals is {session} but session_args does not give the run " +
				"one — declare session_args with {session}")
		}
	default:
		add("requests.owner.equals is %q (want {session}, the id session_args hands the run, "+
			"or {work}, the call's working directory)", o.Equals)
	}
}

func (c *CallPaths) validate(add func(string, ...any), label string) {
	if len(c.Name) == 0 {
		add("%s.name is empty — a call is told by the tool it names", label)
	} else {
		validatePath(add, label+".name", c.Name)
	}
	if len(c.ID) > 0 {
		validatePath(add, label+".id", c.ID)
	}
	if len(c.Args) > 0 {
		validatePath(add, label+".args", c.Args)
	}
	if len(c.Namespace) > 0 {
		validatePath(add, label+".namespace", c.Namespace)
	}
}

// namespaced reports whether the call rule reads a namespace.
func (c *CallPaths) namespaced() bool { return c != nil && len(c.Namespace) > 0 }

// namespaced reports whether any of the block rules reads a call's namespace.
func (b *Blocks) namespaced() bool {
	if b == nil {
		return false
	}
	return slices.ContainsFunc(b.Each, func(rule BlockRule) bool { return rule.Call.namespaced() })
}

// validateFailureMarkers holds each failure marker to the shape of a vendor's
// words, as a limit or auth marker is.
func (p *Profile) validateFailureMarkers(add func(string, ...any)) {
	for i, m := range p.FailureMarkers {
		if problem := sentinelProblem(m); problem != "" {
			add("failure_markers[%d] %s", i, problem)
		}
	}
}

// mcpClaims are the files a run with tools writes into the call's working
// directory, for [Profile.claims].
func (p *Profile) mcpClaims() []pathClaim {
	if p.MCP == nil {
		return nil
	}
	var out []pathClaim
	for i, f := range p.MCP.Seed {
		if f.In == SeedWork && staysInside(f.Path) && namesAFile(f.Path) {
			out = append(out, pathClaim{label: fmt.Sprintf("mcp.seed[%d].path", i), path: filepath.Clean(f.Path)})
		}
	}
	return out
}
