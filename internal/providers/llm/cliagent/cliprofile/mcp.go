package cliprofile

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
)

// MCP is how a CLI is handed one run's tools.
//
// A phase's tools reach its CLI as ONE MCP server, named `crewlet`, which the
// CLI starts as a stdio command — `crewlet mcp-relay <socket>`, a relay into a
// unix socket this engine serves for that one run (internal/toolsession) — and
// calls natively, in its own loop. What differs from CLI to CLI is only how it
// is TOLD about that server and its limits, and every one of those is here:
// arguments, a configuration file written into the call's working directory,
// environment, the allowlist that lets the model call the server's tools
// without a person to approve them, and the CLI's own per-call timeout.
//
// NIL ON A PROFILE THAT CANNOT BE HANDED TOOLS. Such an entry still answers a
// text function — a judge, a classifier, a summary — and a seat's phase on it
// is refused naming this field, rather than run with no tools.
//
// # Placeholders
//
// Every string in args, allow_args, seed content and env values may carry
// these, substituted per run:
//
//	{relay_command}         the relay's executable, as a path
//	{relay_command_json}    the same as a JSON string, quotes included
//	{relay_args_json}       the relay's arguments as a JSON array
//	{relay_argv_json}       the executable and its arguments as one JSON array
//	{socket}                the run's socket path
//	{tool_timeout_seconds}  ToolTimeoutSeconds
//	{tool_timeout_ms}       the same in milliseconds
//	{work}                  the call's working directory, where seeds are written
//
// The JSON forms are encoded without HTML escaping, so each is also a valid TOML
// value — a string or an array — for a CLI configured in TOML.
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

	// AllowArgs are the CLI's allowlist for the server's tools, added after
	// Args: a CLI run headless asks nobody, so a tool its permission policy
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
	// environment, so the CLI's tool timeout the host is told of is the one
	// the CLI applies. Never a credential and never an isolation variable.
	Env map[string]string `yaml:"env,omitempty"`

	// ToolTimeoutSeconds is how long the CLI waits for one call of the
	// server's before it gives up on it and tells its model the call failed:
	// the value the profile sets through {tool_timeout_seconds} or
	// {tool_timeout_ms}, or the CLI's own fixed timeout where it has no knob.
	// Required. The engine answers a call still running near it itself and
	// ends the run, handing the next run the call's result, because a CLI
	// that gives up on a call the engine is still running sends its model to
	// make it again.
	ToolTimeoutSeconds int `yaml:"tool_timeout_seconds,omitempty"`

	// Relists declares that the CLI re-reads the server's tool list when the
	// server says it changed, mid-run. One that does not never sees a tool
	// activated during the run, so the engine ends the run after an
	// activation and starts the next with the new list.
	Relists bool `yaml:"relists,omitempty"`

	// FailureEvents are the CLI's own failure reports in what it prints, and
	// the fields of each a failed run's error shows. A run with tools prints
	// every call's arguments and result, so its output is never quoted in an
	// error or a log line: what a failure says is stderr and these fields.
	FailureEvents []FailureEvent `yaml:"failure_events,omitempty"`
}

// FailureEvent is one kind of failure report a CLI prints as an event of its
// own, and what of it to show.
type FailureEvent struct {
	// Match picks the event out — see [Match].
	Match []Match `yaml:"match,omitempty"`
	// Fields are shown, by path, each that resolves in the event.
	Fields []Path `yaml:"fields,omitempty"`
}

// Match is one condition an event must meet: the value at Path equals Equals,
// compared as JSON values — a string with a string, a boolean with a boolean, a
// number with a number.
type Match struct {
	Path   Path `yaml:"path,omitempty"`
	Equals any  `yaml:"equals,omitempty"`
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
}

// Render substitutes a run's values into one profile string.
func (m *MCP) Render(s string, v MCPValues) string {
	if !strings.Contains(s, "{") {
		return s
	}
	argv := append([]string{v.RelayCommand}, v.RelayArgs...)
	return strings.NewReplacer(
		"{relay_command_json}", jsonValue(v.RelayCommand),
		"{relay_args_json}", jsonValue(v.RelayArgs),
		"{relay_argv_json}", jsonValue(argv),
		"{relay_command}", v.RelayCommand,
		"{socket}", v.Socket,
		"{tool_timeout_seconds}", strconv.Itoa(m.ToolTimeoutSeconds),
		"{tool_timeout_ms}", strconv.Itoa(m.ToolTimeoutSeconds*1000),
		"{work}", v.Work,
	).Replace(s)
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

// rendered is every string of the block that is rendered, for a check over all
// of them.
func (m *MCP) rendered() []string {
	out := slices.Concat(m.Args, m.AllowArgs)
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
	out.Seed = slices.Clone(m.Seed)
	out.Env = cloneMap(m.Env)
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
		out[i] = Match{Path: slices.Clone(m.Path), Equals: m.Equals}
	}
	return out
}

// validateMCP holds the tool route to the shapes in which it hands the CLI its
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
		// Without the command the CLI is never told what to start, and every
		// run with tools is a run that never reached them.
		add("mcp names no relay command — put {relay_command}, {relay_command_json} or " +
			"{relay_argv_json} in mcp.args, mcp.seed or mcp.env, where the CLI reads the " +
			"server it starts")
	}
	if !slices.ContainsFunc(all, func(s string) bool {
		return carriesAny(s, []string{"{relay_args_json}", "{relay_argv_json}", "{socket}"})
	}) {
		add("mcp names no relay arguments — the relay is told its socket on its argv, so " +
			"{relay_args_json}, {relay_argv_json} or {socket} has to reach the CLI's server " +
			"configuration")
	}
	if m.ToolTimeoutSeconds <= 0 {
		add("mcp.tool_timeout_seconds is %d — say how long the CLI waits for one call, the "+
			"value the profile sets or the CLI's own fixed one: a call still running near it "+
			"is answered by the engine first, and with no value nothing is", m.ToolTimeoutSeconds)
	}
	for _, field := range []struct {
		name string
		args []string
	}{{"mcp.args", m.Args}, {"mcp.allow_args", m.AllowArgs}} {
		for i, arg := range field.args {
			if problem := unknownPlaceholder(arg); problem != "" {
				add("%s[%d] %s", field.name, i, problem)
			}
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
		if problem := unknownPlaceholder(f.Content); problem != "" {
			add("%s.content %s", label, problem)
		}
	}
	isolation := p.IsolationVariables()
	for _, name := range slices.Sorted(maps.Keys(m.Env)) {
		switch {
		case strings.TrimSpace(name) == "":
			add("mcp.env names an empty variable")
		case slices.Contains(isolation, name):
			add("mcp.env names %s, which the engine sets to keep the CLI inside its seat's "+
				"home and its call's working directory — a value here would replace that "+
				"isolation on every run with tools", name)
		case strings.TrimSpace(m.Env[name]) == "":
			add("mcp.env.%s is empty — give the value the CLI reads, or drop it", name)
		}
		if problem := unknownPlaceholder(m.Env[name]); problem != "" {
			add("mcp.env.%s %s", name, problem)
		}
	}
	for i, e := range m.FailureEvents {
		label := fmt.Sprintf("mcp.failure_events[%d]", i)
		validateMatches(add, label+".match", e.Match)
		if len(e.Fields) == 0 {
			add("%s.fields is empty — name the fields of the CLI's failure report a failed "+
				"run's error shows", label)
		}
		for j, f := range e.Fields {
			validatePath(add, fmt.Sprintf("%s.fields[%d]", label, j), f)
		}
	}
	if len(m.FailureEvents) > 0 && p.EffectiveOutput() != OutputJSONL {
		add("mcp.failure_events is set but output is %q — a failure event exists only in a "+
			"jsonl stream", p.EffectiveOutput())
	}
}

// unknownPlaceholder reports a `{name}` in s that is not one the tool route
// substitutes, or "" when there is none. A misspelt placeholder would reach the
// CLI verbatim, which starts no server.
func unknownPlaceholder(s string) string {
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
		if isPlaceholderShaped(name) && !slices.Contains(mcpPlaceholders, name) {
			return fmt.Sprintf("carries %s, which is no placeholder the tool route substitutes "+
				"(it has %s)", name, strings.Join(mcpPlaceholders, ", "))
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
		switch m.Equals.(type) {
		case string, bool, int, int64, uint64, float64:
		default:
			add("%s[%d].equals is %v — compare with a string, a boolean or a number", label, i, m.Equals)
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

// Cap is the shape of a run the CLI's OWN turn cap ended — the model still
// working, the CLI no longer asking it — which is an answer of what the run
// did, not a failure: a Completion stopped at the cap.
//
// Read on a run with tools. A text function is one answer, and a capped one is
// the failure it always was.
//
// Both conditions where both are set: the exit status, and an event (or the
// document) matching every condition. A CLI that reports nothing in its output
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
		add("cap names neither exit_code nor match — say how a run the CLI's own turn cap " +
			"ended is told from any other")
	}
	if len(c.Match) > 0 {
		validateMatches(add, "cap.match", c.Match)
		if p.EffectiveOutput() == OutputText {
			add("cap.match is set but output is text — there is no document to match; use " +
				"cap.exit_code")
		}
	}
}

// RequestSource is where a CLI's record of its model requests is read from.
type RequestSource string

const (
	// SourceStdout reads the record from the run's stdout, line by line.
	SourceStdout RequestSource = "stdout"
	// SourceFile tails a file the CLI writes as it runs.
	SourceFile RequestSource = "file"
)

// Valid reports whether s is a source this package knows.
func (s RequestSource) Valid() bool { return s == SourceStdout || s == SourceFile }

// Requests is how a CLI's own record of a run shows each MODEL REQUEST it made
// — its prose, the calls it asked for, which model answered, why it stopped
// and what it cost — read while the run goes.
//
// The engine sees every call the CLI makes to its server and nothing else: not
// the requests between them, not their prose, not what each cost. A round of a
// phase is one model request, the round budget counts them, and the budget is
// charged as each one's usage is read, so a phase is gated while it runs
// rather than once at its end. A CLI whose record shows no requests — this
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
//   - id, model, stop and usage set the request's (a usage figure takes the
//     LAST value read for that request, so a stream's running count works);
//   - text appends to the request's prose and delta streams text as it is
//     written; call reads one call; blocks reads an array of content blocks,
//     each by the first of its rules that matches;
//   - tools is the list of tools the CLI offers its model, which the doctor
//     reports.
type Requests struct {
	// Source is stdout or file.
	Source RequestSource `yaml:"source,omitempty"`

	// File is a glob the CLI's record is found by, for source file, relative
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

	// HostPrefix is how the CLI spells one of the server's tools to its
	// model — `mcp__crewlet__` for `mcp__crewlet__search`. A call so named is
	// the host's, reported by its own name; any other is one of the CLI's own
	// tools (its web search, its fetch).
	HostPrefix string `yaml:"host_prefix,omitempty"`
}

// RequestEvent is one rule a line of the record is read by — see [Requests].
type RequestEvent struct {
	Match  []Match    `yaml:"match,omitempty"`
	Opens  bool       `yaml:"opens,omitempty"`
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

// CallPaths locate one call: the CLI's id for it, the tool's name and its
// arguments, an object.
type CallPaths struct {
	ID   Path `yaml:"id,omitempty"`
	Name Path `yaml:"name,omitempty"`
	Args Path `yaml:"args,omitempty"`
}

// Blocks is an array of content blocks in an event, and the rules each block is
// read by — the first whose Match holds.
type Blocks struct {
	Path Path        `yaml:"path,omitempty"`
	Each []BlockRule `yaml:"each,omitempty"`
}

// BlockRule reads one kind of content block: prose, or a call.
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
		Match: cloneMatches(e.Match), Opens: e.Opens,
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
	return &CallPaths{ID: slices.Clone(c.ID), Name: slices.Clone(c.Name), Args: slices.Clone(c.Args)}
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
		if p.EffectiveOutput() != OutputJSONL {
			add("requests.source is stdout but output is %q — a record of requests is a jsonl "+
				"stream", p.EffectiveOutput())
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
		case r.In == SeedHome && !strings.Contains(r.File, "{session}"):
			add("requests.file %q is under the seat's home and does not name the run's own "+
				"file through {session} — the home is shared by every call of the seat at once, "+
				"so the glob could find another run's record", r.File)
		}
		if strings.Contains(r.File, "{session}") && !hasPlaceholder(p.SessionArgs, "{session}") {
			add("requests.file names {session} but session_args does not give the run one — " +
				"declare session_args with {session}")
		}
	}
	if len(r.Events) == 0 {
		add("requests.events is empty — give the rules a line of the record is read by")
	}
	opens, calls := false, false
	for i, e := range r.Events {
		label := fmt.Sprintf("requests.events[%d]", i)
		validateMatches(add, label+".match", e.Match)
		if !e.Opens && len(e.ID) == 0 && len(e.Model) == 0 && len(e.Text) == 0 && len(e.Delta) == 0 &&
			e.Call == nil && e.Blocks == nil && len(e.Stop) == 0 && !e.Usage.any() && len(e.Tools) == 0 {
			add("%s reads nothing — give it opens, id, model, text, delta, call, blocks, stop, "+
				"usage or tools", label)
		}
		opens = opens || e.Opens || len(e.ID) > 0
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
				validateMatches(add, rl+".match", rule.Match)
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
	if !opens {
		add("requests.events neither opens a request nor names one by id — no line of the " +
			"record would ever begin a request")
	}
	if calls && strings.TrimSpace(r.HostPrefix) == "" {
		add("requests reads calls but requests.host_prefix is empty — say how the CLI spells " +
			"one of the server's tools to its model, e.g. mcp__crewlet__, so a call to it is " +
			"told from one to the CLI's own tools")
	}
	for _, from := range slices.Sorted(maps.Keys(r.Stops)) {
		if !slices.Contains(StopValues, r.Stops[from]) {
			add("requests.stops.%s is %q (want one of %s)", from, r.Stops[from], strings.Join(StopValues, ", "))
		}
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
