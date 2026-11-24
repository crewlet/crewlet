package harnessprofile

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/crewlet/crewlet/internal/words"
)

// PromptMode is how a harness receives the prompt.
type PromptMode string

const (
	// PromptStdin writes the prompt to the child's stdin. The default, and
	// one of the two modes with no length ceiling.
	PromptStdin PromptMode = "stdin"
	// PromptArgv appends the prompt as the last argument. Bounded by
	// ARG_MAX — around 2 MB on Linux, 256 KB on macOS — so a long
	// transcript on such a harness fails at exec rather than at the model.
	PromptArgv PromptMode = "argv"
	// PromptFile writes the prompt to a private file in the per-call
	// working directory and passes the PATH through the `{file}`
	// placeholder in [Profile.PromptArgs]. The prompt is the whole flattened
	// transcript, the largest and most sensitive thing handed to a harness,
	// and argv is ARG_MAX-bounded and readable via /proc/<pid>/cmdline: prefer
	// this over argv wherever the vendor has a prompt-file flag.
	PromptFile PromptMode = "file"
)

// Valid reports whether m is a mode this package knows.
func (m PromptMode) Valid() bool {
	return m == PromptStdin || m == PromptArgv || m == PromptFile
}

// OutputMode is how a harness's answer is encoded on stdout.
type OutputMode string

const (
	// OutputJSON is one JSON object.
	OutputJSON OutputMode = "json"
	// OutputJSONL is a stream of JSON objects, one per line, whose
	// interesting fields are spread across several of them. Codex streams
	// its events this way, so every line is scanned and the LAST value
	// found at a path wins.
	OutputJSONL OutputMode = "jsonl"
	// OutputText is the harness's own prose on stdout, taken verbatim. No
	// usage figures exist in this mode, so tokens are estimated.
	OutputText OutputMode = "text"
)

// Valid reports whether m is a mode this package knows.
func (m OutputMode) Valid() bool {
	return m == OutputJSON || m == OutputJSONL || m == OutputText
}

// Path is a route into a decoded JSON document: object keys, or a decimal
// array index.
//
// A usage path may take [PathEvery], summing the count under each member that
// has one (Gemini CLI keys counts by model, `stats.models.<model>.tokens`). Only
// usage paths may ([Profile.Validate]): for an answer or flag, which member's
// value would be a guess.
type Path []string

// PathEvery is the [Path] step standing for every member there; a literal `*`
// key is out of reach (no measured harness has one).
const PathEvery = "*"

// HasEvery reports whether the path takes [PathEvery] anywhere.
func (p Path) HasEvery() bool { return slices.Contains(p, PathEvery) }

// String renders a path dotted, `usage.input_tokens`, for operator messages.
func (p Path) String() string { return strings.Join(p, ".") }

// PathList renders paths for such a message, in the order they are TRIED.
func PathList(paths []Path) string {
	if len(paths) == 0 {
		return "(none declared)"
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, p.String())
	}
	return strings.Join(out, ", ")
}

// UsagePaths locates the four token counts in a harness's own usage report.
//
// Each is a LIST because vendors move these between releases: the first that
// resolves to a number wins; when none does, the count is estimated (four
// characters a token) and `crewlet llm doctor` says so.
//
// Reasoning is ADDED to output (billed as output): declare it only where the
// vendor's output excludes it (OpenCode 2.0); OpenAI's counts it inside, so
// adding it there charges it twice. ToolPrompt is the input-side twin: tokens a
// MODEL-SIDE tool put before the model (Gemini's toolUsePromptTokenCount).
type UsagePaths struct {
	Input      []Path `yaml:"input,omitempty"`
	Output     []Path `yaml:"output,omitempty"`
	CacheRead  []Path `yaml:"cache_read,omitempty"`
	CacheWrite []Path `yaml:"cache_write,omitempty"`
	Reasoning  []Path `yaml:"reasoning,omitempty"`
	ToolPrompt []Path `yaml:"tool_prompt,omitempty"`

	// InputIncludesCache declares that the input count is ALREADY the whole
	// prompt, cached tokens inside, so the cache figures only break it down.
	// False (Anthropic's shape, and every harness passing it through) reads
	// input as the uncached part and adds the cache figures
	// ([UsagePaths.PromptTokens]).
	//
	// A declaration of the vendor's arithmetic, since both shapes name fields
	// alike: OpenAI's `input_tokens` holds its `cached_tokens` (codex passes
	// that through). Read the other way, cached tokens are charged twice.
	InputIncludesCache bool `yaml:"input_includes_cache,omitempty"`
}

// PromptTokens is the whole prompt's token count (every cached token once) from
// the three input figures, by [UsagePaths.InputIncludesCache]'s arithmetic.
func (u UsagePaths) PromptTokens(input, cacheRead, cacheWrite int) int {
	if u.InputIncludesCache {
		return input
	}
	return input + cacheRead + cacheWrite
}

// CredentialStore is a login kept in a DATABASE OF THE HARNESS'S OWN, travelling
// as the DOCUMENT its export command prints, not as [Profile.CredentialPaths]:
// OpenCode 2.0 keeps logins in the same SQLite file as every session, so
// copying it would carry one seat's conversations to every other seat and
// pruning it would delete the login. The document is the login of record
// ([CredentialStoreDocument]): a JSON array of credentials, each an object
// with a string id. Each generation imports it into an empty database and
// exports at its end, carried back only as a compare-and-swap against the seed
// under hostbox.LockCredentials (Workspace.syncCredentialsOut), holding the
// handed logins alone ([KeepHandedCredentials]).
type CredentialStore struct {
	// ExportArgs print every login the harness holds as one document on stdout.
	ExportArgs []string `yaml:"export_args,omitempty"`

	// ImportArgs read such a document on STDIN (a credential: no other account
	// can read it there) and store every login in it.
	ImportArgs []string `yaml:"import_args,omitempty"`
}

// CredentialStoreDocument is the name the shared credential directory, bundles
// and coding boxes keep a [CredentialStore]'s document under: the engine's, one
// for every profile, since no harness reads the file. No extension (the format
// is the harness's own).
const CredentialStoreDocument = "exported-credentials"

// StdinLogin is a credential login a harness genuinely accepts.
//
// A pointer field on Profile, unset where login is browser OAuth: it separates
// "no password login" (an error a person can act on) from one taking no
// arguments (a command that hangs on a prompt).
type StdinLogin struct {
	// Args is the login argv, with {username} substituted.
	Args []string `yaml:"args,omitempty"`
	// StdinTemplate is written to the child's stdin, with {password} and
	// {username} substituted. Never argv: a password there is visible in
	// ps output and lands in the operator's shell history.
	StdinTemplate string `yaml:"stdin_template,omitempty"`
	// PasswordEnv is an alternative to stdin for a harness that reads its
	// credential from the environment instead.
	PasswordEnv string `yaml:"password_env,omitempty"`
}

// LimitMarker recognises a spent subscription in a reply the harness reported as
// a SUCCESS.
//
// The process exits 0 and the answer is prose. Keyword matching is wrong
// twice over ("usage limit" appears in a model's own answer, and vendors
// reword), so a marker matches by STRUCTURE: a literal sentinel the vendor
// emits verbatim, plus the field carrying the reset instant, and in a run that
// did not fail only where the sentinel opens the answer ([MarkerScope]). A
// sentinel with no reset value still classifies; no sentinel never fires.
type LimitMarker struct {
	// Sentinel is the exact substring the vendor emits, case-sensitive (folding
	// case turns it back into a keyword). In [MarkerScopeEvents] it is the
	// failure CLASS, compared whole with the string at [Profile.MarkerPath].
	Sentinel string `yaml:"sentinel,omitempty"`

	// Reset reads the reset instant out of the matched line, where named.
	Reset ResetReading `yaml:",inline"`
}

// ResetReading says where a vendor's limit report names the instant the limit
// lifts, and how to read it: the text after Separator, as Unit.
//
// A reading that finds nothing is not an error: the report still classifies as
// a limit and the entry sits out a bounded, growing window
// (internal/providers/llm/harness); inventing an instant would hold a seat off
// a reset plan or send it back into a spent one.
type ResetReading struct {
	// Separator splits the reset value off: "|" before an epoch, "resets "
	// before a time of day.
	Separator string `yaml:"reset_separator,omitempty"`

	// Unit is how to read the value after it ([ResetUnit]).
	Unit ResetUnit `yaml:"reset_unit,omitempty"`
}

// ResetUnit is how a [ResetReading] reads the value after its separator.
type ResetUnit string

const (
	// ResetEpoch is Unix seconds.
	ResetEpoch ResetUnit = "epoch"
	// ResetSeconds is a delay in seconds from the report.
	ResetSeconds ResetUnit = "seconds"
	// ResetClock is a time of day then its IANA zone, as Claude Code 2.1
	// writes it: "4:30pm (America/Vancouver)", "16:30 (Asia/Tokyo)". The
	// instant is the occurrence nearest the report, so one past midnight is
	// tomorrow's.
	ResetClock ResetUnit = "clock"
)

// Valid reports whether u is a unit a reading can be taken in.
func (u ResetUnit) Valid() bool {
	switch u {
	case ResetEpoch, ResetSeconds, ResetClock:
		return true
	}
	return false
}

// LimitEvent recognises a spent plan or rate limit by the harness's own
// STRUCTURED report, e.g. Claude Code's result event with `is_error: true` and
// `api_error_status: 429`. Read BEFORE any sentinel: a status code survives a
// vendor's rewording. Safe to bench on for [MarkerScopeEvents]'s reason (a
// model chooses an event's strings, never its keys or types), which holds only
// where every stdout line is a JSON event: these need jsonl output and a
// discriminator, and every match names both.
type LimitEvent struct {
	// Match picks the report out ([Match]).
	Match []Match `yaml:"match,omitempty"`

	// SaidPath locates the vendor's sentence: shown to the operator and read by
	// Reset. Empty shows the whole line, no instant.
	SaidPath Path `yaml:"said_path,omitempty"`

	// Reset reads the instant out of the sentence at SaidPath.
	Reset ResetReading `yaml:",inline"`

	// In is where the report is looked for ([EventSource]): stdout by default.
	In EventSource `yaml:"in,omitempty"`
}

// EventSource is where a [LimitEvent] or an [AuthEvent] is looked for.
type EventSource string

const (
	// EventsInStdout is the run's stdout, decoded a line at a time: the default.
	EventsInStdout EventSource = ""
	// EventsInRecord is the record the harness keeps of its session
	// ([Profile.Requests] with source file), followed for every run, a text
	// function's included, and read only for a run the harness failed, since a
	// process of the run can write the file. For a harness whose stdout and
	// stderr no longer say how a run ended: Kimi Code 2.7 prints a failed run's
	// class only on its session's `turn.ended` line.
	EventsInRecord EventSource = "record"
)

// Valid reports whether s is a source this package reads.
func (s EventSource) Valid() bool { return s == EventsInStdout || s == EventsInRecord }

// ReadsRecordEvents reports whether a limit or sign-in event is looked for in
// the harness's record ([EventsInRecord]), which every run then follows.
func (p *Profile) ReadsRecordEvents() bool {
	return slices.ContainsFunc(p.LimitEvents, func(e LimitEvent) bool { return e.In == EventsInRecord }) ||
		slices.ContainsFunc(p.AuthEvents, func(e AuthEvent) bool { return e.In == EventsInRecord })
}

// MarkerScope says WHERE a harness's failure prose can appear, and therefore
// where [LimitMarker] and [AuthMarker] sentinels may be matched.
type MarkerScope string

const (
	// MarkerScopeAnswerAndStderr searches the model's answer and stderr: the
	// default, required where a harness reports a spent plan AS its answer
	// (Claude Code, on a ZERO exit).
	//
	// On a run not reported failed (non-zero exit or [Profile.ErrorPaths]) the
	// answer is the model's own words, so a sentinel counts only where it BEGINS
	// the answer's first non-empty line; anywhere in a failed run. Stderr is
	// matched anywhere. Without the anchoring a seat asked about its quota
	// sat the entry out. A sentinel that is not words ("429") still false-positives.
	MarkerScopeAnswerAndStderr MarkerScope = "answer-and-stderr"
	// MarkerScopeStderr searches stderr only, for a harness whose failures
	// never reach the answer (pi; measured). Narrow only where the
	// answer is an impossible place for the report: hermes prints a refused
	// key on stdout with exit 2 (0.23.3).
	MarkerScopeStderr MarkerScope = "stderr"
	// MarkerScopeEvents matches STRUCTURE, not text, for a harness reporting a
	// failure as its own event in a JSON stream (OpenCode 2's `provider.quota`).
	// It decodes each stdout line, looks only at the events
	// [Profile.MarkerEvents] names (told apart by [Profile.EventTypePath]) and
	// compares each sentinel by EQUALITY with the string at
	// [Profile.MarkerPath]. A substring search is wrong: tool-call arguments
	// print as raw JSON, so injected text could bench the credential on a clean
	// exit 0. Stderr is not searched (empty, measured on OpenCode 2).
	// [Profile.Validate] requires jsonl, the discriminator, events and path.
	MarkerScopeEvents MarkerScope = "events"
)

// Valid reports whether s is a scope this package knows.
func (s MarkerScope) Valid() bool {
	return s == MarkerScopeAnswerAndStderr || s == MarkerScopeStderr || s == MarkerScopeEvents
}

// LocalTools says what a profile does about the harness's OWN tools (shell,
// file editor, browser).
//
// On the engine host every phase but the executor uses none: tools arrive as
// the engine's MCP server ([MCP]), and a harness editing files itself is
// invisible to the tool registry and permission model. The executor is fully
// permissioned in its box (internal/sandbox/executor). The shell matters most:
// hostbox isolates HOME and environment, not the filesystem. A profile DENIES
// local tools wherever the vendor allows; web is off per entry ([WebTools]).
type LocalTools string

const (
	// LocalToolsDenied means flags or seeded files turn the shell and file
	// tools off; the doctor's shell probe is expected to be refused.
	LocalToolsDenied LocalTools = "denied"
	// LocalToolsVendorDefault means the vendor offers no way to deny them. The
	// profile's local_tools_note says why, and the doctor reports the probe as
	// a finding, not a pass.
	LocalToolsVendorDefault LocalTools = "vendor-default"
)

// Valid reports whether t is a stance this package knows.
func (t LocalTools) Valid() bool {
	return t == LocalToolsDenied || t == LocalToolsVendorDefault
}

// WebTools says whether a run keeps the harness's OWN web tools (fetch and
// search), a second stance beside [LocalTools]: web is on for a seat by default
// and off per entry (`overrides: {web_tools: denied}`), and always for a
// helper's run whatever its entry says (ADR-0050): a page the model fetched
// itself passed no authority decision, and a fetch is a way OUT for the
// person's data. The empty value reads as [WebToolsAllowed].
type WebTools string

const (
	// WebToolsAllowed keeps the harness's web tools on, the seats' default.
	WebToolsAllowed WebTools = "allowed"
	// WebToolsDenied runs the harness with web off through the profile's
	// [WebDenial]; Load refuses it on a profile with none rather than run a
	// harness that still fetches.
	WebToolsDenied WebTools = "denied"
	// WebToolsSearch runs the harness with its vendor's web search and no
	// other tool, through the profile's [WebSearch]: Ask Crewlet's search run,
	// which is handed a query and nothing else.
	WebToolsSearch WebTools = "search"
)

// Valid reports whether w is a stance this package knows; the empty value is
// not ([Profile.EffectiveWebTools] reads it as allowed).
func (w WebTools) Valid() bool {
	return w == WebToolsAllowed || w == WebToolsDenied || w == WebToolsSearch
}

// WebSearch is how one harness runs a search and nothing else: the argv of
// that run (replacing complete_args whole), the runs it must carry and the
// flags it must not, the search tool its event stream names, and how the
// engine reads the results from that stream. Measured with the doctor's
// search probe; Note names the release.
type WebSearch struct {
	CompleteArgs []string     `yaml:"complete_args"`
	Args         [][]string   `yaml:"args"`
	Refuses      []string     `yaml:"refuses,omitempty"`
	Tool         string       `yaml:"tool"`
	Format       SearchFormat `yaml:"format"`
	Note         string       `yaml:"note"`
}

// SearchFormat is how a search run's results are read from its event stream.
type SearchFormat string

// SearchClaudeStreamJSON reads each `user` event's `tool_use_result` that the
// search tool answered: its results' `content` entries, each a title and a URL.
const SearchClaudeStreamJSON SearchFormat = "claude-stream-json"

// Valid reports whether f is a format this build reads.
func (f SearchFormat) Valid() bool { return f == SearchClaudeStreamJSON }

// CanSearchWeb reports whether the profile has a measured search-only run.
func (p *Profile) CanSearchWeb() bool {
	return p.WebSearch != nil && len(p.WebSearch.CompleteArgs) > 0
}

// SearchProblem says why the search run's argv could reach anything but the
// vendor's search, or "" when it cannot: every run of [WebSearch.Args] present
// once and no flag of [WebSearch.Refuses].
func (p *Profile) SearchProblem() string {
	if !p.CanSearchWeb() {
		return "the profile has no measured search-only run"
	}
	w := p.WebSearch
	return (&ToolsOff{Args: w.Args, Refuses: w.Refuses}).problemIn(w.CompleteArgs)
}

// SearchRun is the profile as its search-only run uses it, or false where it
// has no measured one: what Load gives the entry with web_tools set to search.
func (p Profile) SearchRun() (Profile, bool) {
	if !p.CanSearchWeb() {
		return Profile{}, false
	}
	return p.withWebSearch(), true
}

// withWebSearch is the profile as its search run uses it: the stance set, the
// search argv in place of complete_args, and no mcp block or tool record,
// since a search run is handed no tool.
func (p Profile) withWebSearch() Profile {
	out := p.Clone()
	out.WebTools = WebToolsSearch
	if w := p.WebSearch; w != nil {
		out.CompleteArgs = append([]string(nil), w.CompleteArgs...)
	}
	out.MCP, out.Requests = nil, nil
	return out
}

// WebDenial is what a harness's run uses INSTEAD when the entry is
// `web_tools: denied`. Replacements, not edits: a set field replaces the
// profile field of the same name whole ([applyOverrides]), the denied argv
// having been measured as a unit. A denial setting nothing is refused.
//
// A run with tools is covered too: a profile whose mcp block has its own argv
// or settings would keep web on exactly where a seat works, so the denial says
// what that run uses ([WebDenial.MCP]) for each field it replaces, or is
// refused. Built-ins were measured with the doctor's web probe
// (web_tools_note names the release).
type WebDenial struct {
	// CompleteArgs replaces complete_args when web is denied.
	CompleteArgs []string `yaml:"complete_args,omitempty"`
	// SeedFiles replaces seed_files when web is denied.
	SeedFiles []SeedFile `yaml:"seed_files,omitempty"`
	// MCP replaces the mcp block's own argv and seeds when web is denied.
	MCP *WebDenialMCP `yaml:"mcp,omitempty"`
	// SystemPrompt and AppendSystemPrompt replace the channels of the same
	// name when web is denied: a harness whose tool allowlist rides the system
	// prompt's file (kimi-code's agent file) turns its web tools off there.
	// The channel carries a run with tools too, so no mcp field needs it.
	SystemPrompt       *Channel `yaml:"system_prompt,omitempty"`
	AppendSystemPrompt *Channel `yaml:"append_system_prompt,omitempty"`
}

// WebDenialMCP is what a run with tools uses instead when web is denied: each
// field, when set, replaces the mcp block's field of the same name whole.
type WebDenialMCP struct {
	// CompleteArgs replaces mcp.complete_args.
	CompleteArgs []string `yaml:"complete_args,omitempty"`
	// Seed replaces mcp.seed.
	Seed []SeedFile `yaml:"seed,omitempty"`
}

// changes reports whether the denial replaces anything at all.
func (d *WebDenial) changes() bool {
	return d != nil && (d.CompleteArgs != nil || d.SeedFiles != nil ||
		d.SystemPrompt != nil || d.AppendSystemPrompt != nil ||
		(d.MCP != nil && (d.MCP.CompleteArgs != nil || d.MCP.Seed != nil)))
}

// mcpUncovered names the mcp block's fields a denial leaves in place although
// it replaces their text-function counterpart, which a run with tools would use
// with web on. The counterpart decides: an argv denial must reach the run's
// argv, a settings one its settings.
func (d *WebDenial) mcpUncovered(m *MCP) []string {
	if m == nil {
		return nil
	}
	var out []string
	if d.CompleteArgs != nil && len(m.CompleteArgs) > 0 && (d.MCP == nil || d.MCP.CompleteArgs == nil) {
		out = append(out, "mcp.complete_args")
	}
	if d.SeedFiles != nil && len(m.Seed) > 0 && (d.MCP == nil || d.MCP.Seed == nil) {
		out = append(out, "mcp.seed")
	}
	return out
}

// SeedScope is where a seeded file lands.
type SeedScope string

const (
	// SeedHome writes under the seat's HOME once per seat generation, beside
	// the credentials: settings read from the user directory.
	SeedHome SeedScope = "home"
	// SeedWork writes into the per-call working directory on every call:
	// settings read from the project (the scratch directory is that project).
	SeedWork SeedScope = "work"
)

// Valid reports whether s is a scope this package knows.
func (s SeedScope) Valid() bool { return s == SeedHome || s == SeedWork }

// SeedFile is a settings file the engine writes for the harness before a call.
//
// For vendors taking tool policy from a file, not a flag (Gemini CLI's policy
// files, OpenCode's opencode.json, Cursor's cli.json). Content is written
// verbatim at 0600.
type SeedFile struct {
	// Path is relative to the scope's root. Never absolute, never
	// escaping it.
	Path string `yaml:"path,omitempty"`
	// In is the scope: home (default) or work.
	In SeedScope `yaml:"in,omitempty"`
	// Content is the file's bytes.
	Content string `yaml:"content,omitempty"`
}

// HomeDaemon is a process the harness starts for a call and keeps running in the
// background to serve the home's later calls, with the environment of the call
// that started it: Hermes Agent 0.23.3's `-z` runs its turn in a gateway it
// starts per HERMES_HOME in a session of its own, which outlives the call's
// process group and serves the next call with the first one's variables.
//
// Declared, a home runs one call at a time, and the engine runs StopArgs in it
// whenever PIDFile is there as a call releases the home and before a call
// opens it (one an engine that ended mid-call left behind), so every call
// starts its own daemon and none outlives the call that started it.
type HomeDaemon struct {
	// PIDFile is the file, relative to the seat home, the harness keeps while
	// its daemon runs and removes when it stops.
	PIDFile string `yaml:"pid_file,omitempty"`
	// StopArgs is the harness's own command that stops the daemon of the home
	// it runs in, a turn the daemon still runs included, and exits 0 when
	// there is none.
	StopArgs []string `yaml:"stop_args,omitempty"`
}

// staysInside is the load-time half of [UnderRoot] ([filepath.IsLocal]), for a
// path whose real root does not exist yet; a stand-in root would let
// `../seat/settings.json` pass here and fail on every call.
func staysInside(rel string) bool { return filepath.IsLocal(rel) }

// Scope is the seed scope with its default applied.
func (f SeedFile) Scope() SeedScope {
	if f.In == "" {
		return SeedHome
	}
	return f.In
}

// Channel is one way a harness takes a system prompt: as an argument, as a file
// named by an environment variable, or as a file it finds by its location.
//
// Vendors ship all three and `--help` shows only the first: Gemini CLI and Qwen
// take a file a VARIABLE names, with no flag; OpenCode takes no path and
// DISCOVERS the file where its vendor says to look.
type Channel struct {
	// Args is the argv the channel adds, with one of two placeholders:
	//
	//   {file}    the text goes to a private file in the per-call working
	//             directory and the PATH is substituted. Prefer always.
	//   {system}  the text goes INTO ARGV, readable via /proc/<pid>/cmdline and
	//             ARG_MAX-bounded (256 KB on macOS). Only where no file variant.
	//
	// With a discovered [Channel.File] it carries neither: it only selects what
	// the harness found, or is empty.
	Args []string `yaml:"args,omitempty"`

	// Env is an environment variable naming a file the harness reads its
	// system prompt from (`GEMINI_SYSTEM_MD`); preferred over Args, the text
	// never reaching argv. Not both: the harness would get the text twice.
	Env string `yaml:"env,omitempty"`

	// File shapes the file a `{file}`, Env or discovered channel writes
	// ([SystemPromptFile]). Nil writes the prompt alone, under a name no
	// harness reads on its own.
	File *SystemPromptFile `yaml:"file,omitempty"`
}

// Declared reports whether the profile offers this channel at all.
func (c Channel) Declared() bool {
	return len(c.Args) > 0 || c.Env != "" || c.File.ReadAlways()
}

// WritesFile reports whether the channel puts the text in a FILE: `{file}` on
// argv, the variable (it names a path), or a discovered file.
func (c Channel) WritesFile() bool {
	return c.Env != "" || c.File.Found() || hasPlaceholder(c.Args, "{file}")
}

// clone deep-copies the channel, because overrides replace its list in place.
func (c Channel) clone() Channel {
	out := Channel{Args: append([]string(nil), c.Args...), Env: c.Env}
	if c.File != nil {
		spec := *c.File
		out.File = &spec
	}
	return out
}

// clonePtr is a copy of the channel c points at, nil for nil.
func (c *Channel) clonePtr() *Channel {
	if c == nil {
		return nil
	}
	out := c.clone()
	return &out
}

// Route is where one call's system prompt travels ([Profile.RouteFor]).
type Route struct {
	// Channel carries the text; the zero channel leaves it in the transcript.
	Channel Channel

	// Field names the profile field the channel came from (`system_prompt` or
	// `append_system_prompt`), empty for the transcript, so a message can name
	// what to override.
	Field string

	// KeepsDefault says the harness's own system prompt is in force: the append
	// channel, or none (the text rides the transcript).
	KeepsDefault bool
}

// RouteFor picks a call's channel ([Profile.SystemPrompt]): append for a seat
// at work, replace for a text function, each falling back to the other, then
// the transcript. A seat on a replace-only harness takes the replacement: the
// vendor prompt it would keep is written for a terminal, and the seat's
// instructions would arrive as a user message under it.
func (p *Profile) RouteFor(agent bool) Route {
	keep := Route{Channel: p.AppendSystemPrompt, Field: "append_system_prompt", KeepsDefault: true}
	replace := Route{Channel: p.SystemPrompt, Field: "system_prompt"}
	first, second := replace, keep
	if agent {
		first, second = keep, replace
	}
	switch {
	case first.Channel.Declared():
		return first
	case second.Channel.Declared():
		return second
	default:
		return Route{KeepsDefault: true}
	}
}

// SystemPromptFile shapes the file a channel writes, for vendors whose channel
// takes a STRUCTURED file or finds one by location. Without it the file is the
// prompt alone.
//
// Kimi Code's only per-call channel is `--agent-file`: YAML frontmatter with a
// REQUIRED description and the tool allowlist, the BODY as the prompt; a bare
// prompt makes it exit. The frontmatter holds its tool denial, so the channel
// is passed on EVERY call, even with no system prompt. OpenCode 2 DISCOVERS the
// file in a configuration directory inside the call's working directory
// ([Profile.WorkConfigEnv], [SystemPromptFile.Discovered]).
type SystemPromptFile struct {
	// Name is the file to write in the per-call working directory; empty takes
	// the default. A bare name where the path is handed over (set only for an
	// extension the vendor keys on: `--agent-file` wants `.md`); a relative
	// path inside the working directory for a discovered file.
	Name string `yaml:"name,omitempty"`

	// Template is the file's content, `{system}` substituted by the system
	// prompt. Empty writes the prompt alone.
	Template string `yaml:"template,omitempty"`

	// Discovered says the harness FINDS the file by location instead of being
	// handed its path ([Discovery]).
	Discovered Discovery `yaml:"discovered,omitempty"`
}

// Discovery is how a harness comes to read a file it finds by location.
type Discovery string

const (
	// DiscoverySelected is a file the harness finds and the channel's argv
	// selects by name (OpenCode's `agents/<name>.md` with `--agent <name>`).
	// Never selected, it changes nothing.
	DiscoverySelected Discovery = "selected"

	// DiscoveryAlways is a file the harness reads on every run in that
	// directory with nothing on argv (OpenCode's global `AGENTS.md`); the file
	// alone declares the channel.
	DiscoveryAlways Discovery = "always"
)

// Valid reports whether d is a known discovery, the empty one included.
func (d Discovery) Valid() bool {
	return d == "" || d == DiscoverySelected || d == DiscoveryAlways
}

// Found reports whether the harness finds this file by location. Nil-safe.
func (f *SystemPromptFile) Found() bool {
	return f != nil && f.Discovered != ""
}

// ReadAlways reports whether the harness reads this file on every run (the file
// alone is the channel). Nil-safe.
func (f *SystemPromptFile) ReadAlways() bool {
	return f != nil && f.Discovered == DiscoveryAlways
}

// CarriesPolicy reports whether the file says something of its own with no
// system prompt in it: a template beyond `{system}` (OpenCode's `--agent`
// frontmatter, Kimi Code's tool allowlist). Such a file is written on every
// call; one holding the prompt alone only when there is one, because empty it
// still says something: OpenCode prefixes an empty AGENTS.md with an
// "Instructions from: <path>" line. Nil-safe.
func (f *SystemPromptFile) CarriesPolicy() bool {
	return f != nil && strings.TrimSpace(f.Render("")) != ""
}

// Render returns the bytes to write for one call's system prompt.
func (f *SystemPromptFile) Render(system string) string {
	if f == nil || f.Template == "" {
		return system
	}
	return strings.ReplaceAll(f.Template, "{system}", system)
}

// FileName returns the name to write under.
func (f *SystemPromptFile) FileName() string {
	if f == nil || f.Name == "" {
		return DefaultSystemPromptFile
	}
	return f.Name
}

// ProjectRootMarker is the file the engine writes into every call's working
// directory to make it a project root, and ProjectRootGitdir its content: a
// FILE naming a gitdir, which every harness measured stops at (an empty `.git`
// DIRECTORY is ignored by git and by harnesses asking git for the root; Kimi
// Code accepts a `.git` file only when its first line is `gitdir:`). The
// relative gitdir never exists in the call's own 0700 directory, so git reports
// "not a git repository" and nobody else can create it. Reserved: no seed may
// claim the path ([Profile.claims]).
const (
	ProjectRootMarker = ".git"
	ProjectRootGitdir = "gitdir: .crewlet-not-a-repository\n"
)

// DefaultSystemPromptFile is what a {file} substitution writes in the per-call
// directory, not the seat home: removed on release, the text cannot outlive its
// call. Deliberately not a name a harness reads itself (CLAUDE.md, AGENTS.md):
// this is an argument, not discoverable context.
const DefaultSystemPromptFile = "crewlet-system-prompt.txt"

// AuthMarker recognises a login the harness has stopped honouring.
//
// An expired OAuth login exits non-zero with prose on stderr; unrecognised it
// is a plain SERVER failure that moves the chain on as AUTH does, so the marker
// buys the NAME: the operator learns that `crewlet llm login` mends it.
type AuthMarker struct {
	// Sentinel is matched as a [LimitMarker]'s is, in the same scope.
	Sentinel string `yaml:"sentinel,omitempty"`

	// Meaning is what the failure says about the credential: [AuthRefused]
	// (default) when the vendor turned it down, [AuthUnread] when the harness
	// never used it. Only the words shown depend on it.
	Meaning AuthMeaning `yaml:"meaning,omitempty"`
}

// AuthMeaning is what an [AuthMarker] says about a run's credential.
type AuthMeaning string

const (
	// AuthRefused is a credential the vendor saw and turned down: wrong,
	// expired or revoked. The empty meaning reads as this one.
	AuthRefused AuthMeaning = "refused"
	// AuthUnread is a credential the harness did not use: it reports itself
	// signed out although a value was set (Claude Code, for a token containing
	// a space).
	AuthUnread AuthMeaning = "unread"
)

// AuthMeaning is the meaning of the first auth marker, in profile order, whose
// sentinel said contains ([llm.Error] Said); [AuthRefused] when none matches,
// a sign-in failure being otherwise the vendor's refusal.
func (p Profile) AuthMeaning(said string) AuthMeaning {
	for _, m := range p.AuthMarkers {
		if m.Sentinel != "" && strings.Contains(said, m.Sentinel) {
			if m.Meaning == "" {
				return AuthRefused
			}
			return m.Meaning
		}
	}
	return AuthRefused
}

// Binary is the executable a profile names for its harness: a name resolved on
// PATH, or a path.
//
// A type of its own so every harness start is found by what it runs: it keeps
// the type until the process is built. The harness package's
// TestOnlyStartChildStartsAHarness holds the module to one starter on the
// engine host, which captures output into files, never a pipe. A string on the
// wire.
type Binary string

// Profile is everything the engine needs to drive one harness.
//
// DATA, not code, every field replaceable from YAML (`overrides`): vendors
// rename these flags and JSON shapes between releases, and that must be an
// operator's config edit, not a Crewlet release.
type Profile struct {
	// Binary is the executable, resolved on PATH unless it is a path.
	Binary Binary `yaml:"binary,omitempty"`

	// WrittenFor is the harness version this profile was written against,
	// shown by `crewlet llm doctor` beside the installed one.
	WrittenFor string `yaml:"written_for,omitempty"`

	// VersionArgs runs the harness's own version probe.
	VersionArgs []string `yaml:"version_args,omitempty"`

	// CompleteArgs is the argv of one completion, before ModelArgs and
	// before the prompt. Lists replace wholesale on override: merging two
	// argvs element-wise yields a command line neither side wrote.
	CompleteArgs []string `yaml:"complete_args,omitempty"`

	// KeyArgs and SubscriptionArgs follow CompleteArgs on a call that signs
	// in with a key (api-key, env, an inherit-env key: the seat's key home,
	// [Profile.KeySeedFiles]'s) and on one that signs in with a login,
	// respectively: the flag choosing the sign-in, for a harness that picks
	// one on argv and runs another without it. Qwen Code's `--auth-type
	// openai` and `--auth-type qwen-subscription`: its OpenAI sign-in otherwise
	// needs OPENAI_MODEL set beside the key (0.28.4). Lists replace wholesale
	// on override, as CompleteArgs does.
	KeyArgs          []string `yaml:"key_args,omitempty"`
	SubscriptionArgs []string `yaml:"subscription_args,omitempty"`

	// ModelArgs carries the model, {model} substituted. None with {model}
	// (empty included) means the harness takes no model, and an entry naming
	// one is refused, not silently ignored ([Profile.TakesModel]). The
	// placeholder may be its own element or follow `=` in `--model={model}`.
	ModelArgs []string `yaml:"model_args,omitempty"`

	// ModelNamesProvider declares that the harness splits its model flag's value
	// `<provider>/<model>` at the FIRST slash, so a value with nothing on either
	// side names no model and every call fails inside the harness. Config
	// refuses such a `model` on a write ([Profile.ModelArgument]). A measured
	// vendor fact, overridable; only opencode declares it (OpenCode 2 refuses a
	// bare id as "Invalid model reference: <id>").
	ModelNamesProvider bool `yaml:"model_names_provider,omitempty"`

	// Effort is how the harness takes a reasoning effort and its levels
	// ([Effort]). Nil: none, and config refuses an entry's effort.
	Effort *Effort `yaml:"effort,omitempty"`

	// EffortNote says why a harness with no Effort has none, in that refusal.
	EffortNote string `yaml:"effort_note,omitempty"`

	// PromptMode is stdin (the default), argv or file.
	PromptMode PromptMode `yaml:"prompt_mode,omitempty"`

	// SystemPrompt is the channel on which the engine's system prompt REPLACES
	// the harness's own, AppendSystemPrompt the one that adds to it; each call
	// takes the one its work wants ([Profile.RouteFor]): a seat at work APPEND
	// (the vendor's prompt is written for an agent); a text function (filter,
	// classifier, judge) REPLACE (its answer is parsed, and harness guidance
	// would cost thousands of tokens and push it to prose). Each falls back to
	// the other, then to the transcript (USER content).
	SystemPrompt Channel `yaml:"system_prompt,omitempty"`

	// AppendSystemPrompt keeps the harness's own system prompt in force and adds
	// the engine's ([Profile.SystemPrompt]).
	AppendSystemPrompt Channel `yaml:"append_system_prompt,omitempty"`

	// PromptArgs introduces the prompt in argv mode for a harness taking it as a
	// FLAG'S VALUE; empty appends it bare. In [PromptFile] mode it carries the
	// flag AND its `{file}` placeholder (`["--prompt-file", "{file}"]`), required.
	// Appended LAST: a prompt flag in complete_args would put the model flag
	// after the prompt (`grok -p --model grok-4 "hi"` dies at argument parsing).
	PromptArgs []string `yaml:"prompt_args,omitempty"`

	// Output is how stdout is encoded.
	Output OutputMode `yaml:"output,omitempty"`

	// TextPaths locates the assistant's text. First hit wins for json; for
	// jsonl every match is concatenated in stream order.
	TextPaths []Path `yaml:"text_paths,omitempty"`

	// EventTypePath locates the DISCRIMINATOR in a `jsonl` line (the event's
	// kind). Empty: the stream has none, every line's text counts. An enveloped
	// stream cannot be read by paths alone: Muse Code's `payload.text` is the
	// answer on `run.output.delta`, a tool's output on `tool.result` and the
	// answer AGAIN on `run.terminal.completed`; a path walk would repeat it.
	EventTypePath Path `yaml:"event_type_path,omitempty"`

	// TextEvents restricts TEXT extraction to lines whose discriminator is one
	// of these; empty takes every line. Text only: usage and error paths are
	// scanned across the whole stream (last value wins); only the ANSWER must
	// not be spliced.
	TextEvents []string `yaml:"text_events,omitempty"`

	// ErrorPaths locates a boolean the harness sets when it failed despite
	// exiting zero.
	ErrorPaths []Path `yaml:"error_paths,omitempty"`

	// Usage locates the token counts in stdout, or in the report
	// [Profile.UsageFileArgs] or [Profile.UsageArgs] names.
	Usage UsagePaths `yaml:"usage,omitempty"`

	// UsageFileArgs asks the harness to write its token counts to a FILE through
	// the `{usage_file}` placeholder; empty reads stdout. For a harness with no
	// envelope for a usage path (Hermes's `-z` prints nothing else).
	//
	// Written in the call directory, read after exit; absent or unreadable,
	// counts fall back to stdout so a broken write never masks the outcome.
	// Absent is silent; unreadable or empty logs `harness_usage_file_unreadable`,
	// undecodable (or lacking usage.input or usage.output)
	// `harness_usage_report_unreadable`, for a run not reported failed (a failed
	// call is charged nothing).
	UsageFileArgs []string `yaml:"usage_file_args,omitempty"`

	// SessionArgs name the session one call runs in, `{session}` substituted by
	// an id minted for that call alone (32 lowercase hex characters; a vendor
	// prefix may wrap it, `ses_{session}`). Appended with the model arguments.
	// For [Profile.UsageArgs]: a seat's home is shared by concurrent calls, so
	// "the latest session" could be a sub-agent's.
	SessionArgs []string `yaml:"session_args,omitempty"`

	// UsageArgs is a command run AFTER the call, in the same home, directory and
	// environment, printing the token counts as one JSON document that
	// [Profile.Usage] reads; `{session}` is the id [Profile.SessionArgs] gave.
	// A release whose `run --format json` dropped the FINAL `step_finish`
	// would leave stdout alone undercharging (2.3.2 dropped none, measured).
	//
	// A failed or unreadable report is not a failed call: counts fall back to
	// stdout (the estimate, where none) with a warning:
	// `harness_usage_report_failed` for a command that failed or printed
	// nothing, `harness_usage_report_unreadable` for one undecodable or lacking
	// usage.input or usage.output, on a run not reported failed.
	UsageArgs []string `yaml:"usage_args,omitempty"`

	// ConfigEnv maps a vendor's relocation variable to a directory under the
	// seat's home. Without it a harness reads the engine user's real dotfiles
	// and every seat shares one set of sessions.
	ConfigEnv map[string]string `yaml:"config_env,omitempty"`

	// WorkConfigEnv maps a vendor's relocation variable to a directory under
	// the PER-CALL WORKING directory, not the seat's home ([Profile.ConfigEnv]),
	// which concurrent calls share while each system prompt is its own.
	// OpenCode 2 reads `opencode.json`, agents and plugins from the working
	// directory and EVERY ANCESTOR, where a project's permissions beat the
	// seat's; with that walk off ([Profile.IsolationEnv]) it reads only
	// OPENCODE_CONFIG_DIR, inside the call directory, where the `in: work` seed
	// and agent file go. A machine-wide MANAGED configuration (`/etc/opencode`)
	// cannot be switched off. Set only for a command run AS A CALL (login,
	// status and the version probe have no working directory).
	WorkConfigEnv map[string]string `yaml:"work_config_env,omitempty"`

	// IsolationEnv is a vendor's own SWITCH keeping the harness from reading
	// configuration outside its seat's home and call directory, set on EVERY
	// command, at the layer of HOME and the relocations. Apart from
	// [Profile.Env] because env, passthrough_env and an entry's env are layered
	// OVER the isolation and refused naming [Profile.IsolationVariables]: an
	// entry's `OPENCODE_CONFIG_PROJECT_DISABLE: "0"` (or empty) silently
	// restored every ancestor's configuration. The NAME the vendor
	// reads FIRST, never a fallback (OpenCode 2 reads
	// `OPENCODE_CONFIG_PROJECT_DISABLE ?? OPENCODE_DISABLE_PROJECT_CONFIG`).
	// Never a credential, a variable the engine sets ([HomeIsolation]) or empty.
	IsolationEnv map[string]string `yaml:"isolation_env,omitempty"`

	// Env is fixed child environment the harness needs. Never a credential or
	// the profile's token_env or api_key_env ([Profile.ValidateCredentials]),
	// nor a variable the engine sets to isolate the call
	// ([Profile.IsolationVariables], refused by [Profile.Validate]).
	Env map[string]string `yaml:"env,omitempty"`

	// SubscriptionEnv is child environment set under auth.mode subscription
	// alone, after the operator's env, telling a harness that picks its sign-in
	// from its environment to use the seat home's login. Its own field because
	// under api-key the same setting would overrule the key: Gemini CLI picks
	// its sign-in from settings or environment (GOOGLE_GENAI_USE_GCA=true
	// first, GEMINI_API_KEY after) and a copied login selects nothing
	// (subscription calls exited 41, 0.70.0). Never a credential, the
	// profile's token_env or api_key_env, a variable the engine sets to isolate
	// the call or one env sets too, or empty.
	SubscriptionEnv map[string]string `yaml:"subscription_env,omitempty"`

	// PassthroughEnv names engine environment variables forwarded to the child.
	// It MAY NOT name a credential ([Profile.ValidateCredentials]): it is
	// forwarded before auth.mode is consulted, so a key would reach every seat
	// whatever the mode (the metered-bill-on-a-flat-rate failure). Nor a
	// variable the engine sets to isolate the call
	// ([Profile.IsolationVariables]): forwarding the engine's HOME hands over
	// its user's dotfiles.
	PassthroughEnv []string `yaml:"passthrough_env,omitempty"`

	// TokenEnv is the variable carrying a long-lived headless subscription token.
	TokenEnv string `yaml:"token_env,omitempty"`

	// APIKeyEnv is the variable carrying a metered API key, set only under
	// auth.mode api-key or inherit-env.
	APIKeyEnv string `yaml:"api_key_env,omitempty"`

	// APIKeyEnvShared declares APIKeyEnv a GENERIC name other vendors' keys use
	// too, so an entry names its key (auth.api_key) and the engine never reads
	// the variable from its own store or environment, nor forwards it under
	// inherit-env (config refuses). Qwen Code sends OPENAI_API_KEY to whatever
	// OPENAI_BASE_URL names (DashScope's by default): the host's OpenAI key
	// would go to another vendor.
	APIKeyEnvShared bool `yaml:"api_key_env_shared,omitempty"`

	// APIKeyModelEnv is the variable the harness reads its model from when its
	// key variable signs it in, for a harness whose metered key is part of a
	// model of its own: Kimi Code's KIMI_MODEL_NAME, beside KIMI_MODEL_API_KEY,
	// adds a provider that becomes its default and refuses a --model naming
	// any other (2.7.3). Under auth.mode api-key the entry's model goes
	// here and model_args is not passed; under any other mode it is removed.
	// The engine owns it, as it does api_key_env: an entry's env may not set it.
	APIKeyModelEnv string `yaml:"api_key_model_env,omitempty"`

	// EnvSignIn declares that the harness reads its model provider's credential
	// from its OWN environment, under that provider's variable, so a
	// credential-named variable in an entry's `env` signs it in.
	//
	// A declaration, not a list of names: these harnesses front dozens of
	// providers (hermes, pi, OpenCode), hence no api_key_env. Not inferred from
	// env: kimi-code ignores KIMI_API_KEY and reads config.toml. Read by
	// `crewlet llm doctor`, `llm list` and config, to say where the key of an
	// entry with no api_key_env goes.
	EnvSignIn bool `yaml:"env_sign_in,omitempty"`

	// CredentialPaths are the login files, relative to the seat home: what a
	// bundle may carry and what is synced back after a refresh; anything else
	// in the home is conversation state.
	CredentialPaths []string `yaml:"credential_paths,omitempty"`

	// LoginKeys names, by a credential file's NAME, the key paths (dotted, into
	// a JSON object or a `.toml` file's tables) that are its login, for a MIXED
	// file the harness also reads its own settings from: Copilot CLI's
	// config.json, Cursor Agent's cli-config.json, Kimi Code's config.toml. A
	// seat home on the engine host is seeded with only these fields, a box
	// with the file whole, and a refresh carries back only these ([FileRule]);
	// holding none of them the file is not a login ([Profile.HoldsLogin]).
	// Every field the harness signs in from: one not named is never seeded on
	// the host. A file not named here is a login file, whole.
	LoginKeys map[string][]string `yaml:"login_keys,omitempty"`

	// CommentLines names the JSON credential files the harness writes lines
	// opening with `//` into, which are read with those lines set aside:
	// Copilot CLI's config.json opens with two. Any other JSON file holding one
	// does not read, so nothing is carried from it ([FileRule.Carry]).
	CommentLines []string `yaml:"comment_lines,omitempty"`

	// RefreshKeys names, by a credential file's NAME, the key paths (`*` for
	// every key of an object or index of an array there) a refresh carries back
	// from a seat's or a local box's copy: its tokens and their expiry. Every
	// other byte, an endpoint, a provider choice, a key a run minted, stays as
	// the shared login holds it ([FileRule.Carry]); a file not named here
	// carries nothing back. A login command takes the whole file.
	RefreshKeys map[string][]string `yaml:"refresh_keys,omitempty"`

	// IdentityKeys names, by a credential file's NAME, the key paths (`*` for
	// every key of the object there; a last `@subject` for a JWT's subject
	// claims) whose values say which login it is: whose account, at which
	// provider. Codex's auth.json names its ChatGPT account at tokens.account_id
	// and in its id_token. A refresh a seat's call or a local box's run carries
	// back is written only where each holds what the shared login's does
	// ([FileRule.SameAccount]): a refresh keeps its account. A file naming no
	// account (a token and its expiry alone) has none, and its profile says so.
	IdentityKeys map[string][]string `yaml:"identity_keys,omitempty"`

	// VolatilePaths are sessions, transcripts, history and todo state,
	// relative to the seat home, deleted before and after every call: a hidden
	// harness memory would make turns non-reproducible and carry one task's
	// context into the next.
	VolatilePaths []string `yaml:"volatile_paths,omitempty"`

	// HomeDaemon is the background process the harness keeps for a home's
	// calls, or nil for a harness whose calls end with their process group
	// ([HomeDaemon]).
	HomeDaemon *HomeDaemon `yaml:"home_daemon,omitempty"`

	// LoginArgs brokers the vendor's own interactive login.
	LoginArgs []string `yaml:"login_args,omitempty"`

	// CaptureTokenArgs mints a headless token on stdout.
	CaptureTokenArgs []string `yaml:"capture_token_args,omitempty"`

	// StatusArgs asks the harness who it is logged in as.
	StatusArgs []string `yaml:"status_args,omitempty"`

	// LogoutArgs revokes the login.
	LogoutArgs []string `yaml:"logout_args,omitempty"`

	// StdinLogin is a real credential login, where one exists.
	StdinLogin *StdinLogin `yaml:"stdin_login,omitempty"`

	// CredentialStore is set for a harness keeping its login in a database of
	// its own ([CredentialStore]). Nil: CredentialPaths, a token, a key or nothing.
	CredentialStore *CredentialStore `yaml:"credential_store,omitempty"`

	// LimitEvents recognise a spent plan or rate limit by the harness's own
	// structured report, ahead of every sentinel ([LimitEvent]).
	LimitEvents []LimitEvent `yaml:"limit_events,omitempty"`

	// LimitMarkers recognise a spent subscription by the vendor's words.
	LimitMarkers []LimitMarker `yaml:"limit_markers,omitempty"`

	// AuthMarkers recognise an expired login.
	AuthMarkers []AuthMarker `yaml:"auth_markers,omitempty"`

	// AuthEvents recognise a refused sign-in by the harness's own structured
	// report, ahead of every limit report and sentinel ([AuthEvent]).
	AuthEvents []AuthEvent `yaml:"auth_events,omitempty"`

	// LimitReports read where each of the vendor's windows stands from the
	// harness's own fields, and date the limit a run reported ([LimitReport]).
	LimitReports []LimitReport `yaml:"limit_reports,omitempty"`

	// LimitWindows name a limit's window from the vendor's sentence where no
	// [LimitReport] named one ([LimitWindowWording]).
	LimitWindows []LimitWindowWording `yaml:"limit_windows,omitempty"`

	// SignInEnv lists the variables that sign this harness in when an entry's
	// env sets one: a cloud provider's switch (CLAUDE_CODE_USE_BEDROCK) or a
	// gateway's token (ANTHROPIC_AUTH_TOKEN). auth.mode env requires one, and
	// `crewlet llm list` reports it as the route. A list, unlike
	// [Profile.EnvSignIn]: a switch is no credential, so no credential-name
	// rule would count it.
	SignInEnv []string `yaml:"sign_in_env,omitempty"`

	// IdentityEnv lists the non-credential variables whose values say WHICH
	// account or endpoint a call reaches (cloud region and profile, project and
	// location, gateway address). Folded into the entry's sign-in identity, so
	// entries on one cloud switch but different regions or gateways are two
	// sign-ins, and a limit one meets does not sit the other out.
	IdentityEnv []string `yaml:"identity_env,omitempty"`

	// TokenPrefix and APIKeyPrefix are how the vendor's headless token and
	// its API key begin (`sk-ant-oat01-`, `sk-ant-api03-`): shown in a
	// credential's hint, and telling a key pasted where a token belongs. Empty
	// where the vendor's credentials share no prefix.
	TokenPrefix  string `yaml:"token_prefix,omitempty"`
	APIKeyPrefix string `yaml:"api_key_prefix,omitempty"`

	// TokenLifetimeDays is how long the minted headless token
	// ([Profile.CaptureTokenArgs]) lasts per the vendor; zero is unknown. The
	// token is opaque: expiry is computed from when it was stored.
	TokenLifetimeDays int `yaml:"token_lifetime_days,omitempty"`

	// DeviceLoginArgs runs the vendor's device-code sign-in (it prints an address
	// and one-time code, then waits for approval; [DeviceCodePatterns]).
	DeviceLoginArgs []string `yaml:"device_login_args,omitempty"`

	// DeviceCodePatterns read the address and code out of DeviceLoginArgs' output.
	DeviceCodePatterns *DeviceCodePatterns `yaml:"device_code_patterns,omitempty"`

	// StatusTextArgs asks who the harness is logged in as, in words, where
	// StatusArgs prints JSON: what `crewlet llm doctor` shows. Empty shows
	// StatusArgs' own output.
	StatusTextArgs []string `yaml:"status_text_args,omitempty"`

	// StatusFields locates the sign-in method and account in StatusArgs' JSON
	// ([StatusFields]). Nil reads only the exit code.
	StatusFields *StatusFields `yaml:"status_fields,omitempty"`

	// ModelAliases are model names the harness resolves itself ([ModelAlias]).
	ModelAliases []ModelAlias `yaml:"model_aliases,omitempty"`

	// ModelsDocs is the vendor's own page listing the models the harness takes.
	ModelsDocs string `yaml:"models_docs,omitempty"`

	// MarkerScope is where the two marker sets above may be matched
	// ([MarkerScope]). Empty searches the answer and stderr.
	MarkerScope MarkerScope `yaml:"marker_scope,omitempty"`

	// MarkerEvents names the events (by the value at [Profile.EventTypePath])
	// reporting a failure: the only lines [MarkerScopeEvents] looks at.
	// Required by that scope, refused beside any other.
	MarkerEvents []string `yaml:"marker_events,omitempty"`

	// MarkerPath locates the failure CLASS inside one of [Profile.MarkerEvents]:
	// the string a sentinel must EQUAL. Required by [MarkerScopeEvents],
	// refused beside any other.
	MarkerPath Path `yaml:"marker_path,omitempty"`

	// HostCredentialPaths are where this harness keeps its login in a human's
	// own home directory, for `crewlet llm login -from-host` to adopt.
	// Paths are relative to that home.
	HostCredentialPaths []string `yaml:"host_credential_paths,omitempty"`

	// LocalTools is the profile's stance on the harness's own tools
	// ([LocalTools]). Empty means unsaid, which the doctor reports as such.
	LocalTools LocalTools `yaml:"local_tools,omitempty"`

	// LocalToolsNote explains a vendor-default stance: which flag is missing, or
	// would also cut the web. Printed by the doctor beside the probe.
	LocalToolsNote string `yaml:"local_tools_note,omitempty"`

	// ToolsOff is how the run is left with NO tool of its own, by construction
	// in its argv, or nil where no measured way exists ([ToolsOff]).
	ToolsOff *ToolsOff `yaml:"tools_off,omitempty"`

	// SeedFiles are settings files written before a call, for vendors taking
	// tool policy from a file ([SeedFile]).
	SeedFiles []SeedFile `yaml:"seed_files,omitempty"`

	// WebTools is the stance on the harness's own web tools ([WebTools]); empty
	// reads as allowed. Set per entry through overrides; no built-in sets it.
	WebTools WebTools `yaml:"web_tools,omitempty"`

	// WebToolsNote says how the web denial was measured, or why there is none.
	// Printed by the doctor beside the web probe.
	WebToolsNote string `yaml:"web_tools_note,omitempty"`

	// WebDenial is how the harness turns its web tools off, or nil where no
	// measured way exists ([WebDenial]).
	WebDenial *WebDenial `yaml:"web_denial,omitempty"`
	// WebSearch is this harness's search-only run, or nil where it has no
	// measured one. See [WebSearch].
	WebSearch *WebSearch `yaml:"web_search,omitempty"`

	// ImageInput is how a call hands the model images beside its prompt, or nil
	// where the harness has no way ([ImageInput]).
	ImageInput *ImageInput `yaml:"image_input,omitempty"`

	// KeySeedFiles are settings files written into a seat's KEY home alone (used
	// when signing in with a key: api-key, or inherit-env) after seed_files,
	// each over a seed_files entry at the same path. The counterpart of
	// [Profile.SubscriptionEnv], for a harness picking its sign-in from SETTINGS
	// before its environment: Gemini CLI takes an operator's
	// GOOGLE_GEMINI_BASE_URL gateway as a sign-in it then refuses (exit 41)
	// unless its settings name the key sign-in, and those settings in a login
	// home would send subscription calls to a key it lacks (0.70.0). Key and
	// login homes are two directories ([Workspace.AcquireForKey]). Home only:
	// a call's working directory knows nothing of how it signs in.
	KeySeedFiles []SeedFile `yaml:"key_seed_files,omitempty"`

	// MCP is how the harness is handed a phase's tools as an MCP server
	// ([MCP]). Nil: it cannot be, and a seat's phase on it is refused.
	MCP *MCP `yaml:"mcp,omitempty"`

	// Requests is how the harness's own record shows each model request of a run
	// with tools ([Requests]). Nil: none shown, the run counts as one round.
	Requests *Requests `yaml:"requests,omitempty"`

	// Cap is how a run ended by the harness's own turn cap is told from a
	// failure ([Cap]). Nil: no cap, or the profile cannot tell.
	Cap *Cap `yaml:"cap,omitempty"`

	// FailureMarkers are sentinels the harness writes on stderr when its own
	// confinement failed to load (a policy file it could not read), after which
	// it runs on vendor defaults. Matched as each stderr line arrives, on every
	// call; the run is stopped at once and fails, since a harness with its tool
	// policy unloaded must not use the tools that policy denies (Gemini CLI
	// reports "Policy file error" and carries on, measured on 0.70.0).
	FailureMarkers []string `yaml:"failure_markers,omitempty"`
}

// ToolsOff is the proof, IN THE ARGV, that a run offers its model no tool of
// its OWN (no shell, file tool, web, or server but the engine's).
// [LocalToolsDenied] is a claim about a profile; a caller that must not be
// reached around (the Dashboard Assistant) checks this against each call's argv
// (ADR-0049): the call's arguments turn every built-in off and load no server,
// and the mcp block allows only the engine's tools. A deny list is not this: on
// Claude Code 2.1.310 --disallowedTools left thirteen tools where `--tools ""`
// left none, and a deny list is always a release behind.
type ToolsOff struct {
	// Args are the argument runs that leave the harness with no built-in tool
	// and no server but the mcp block's. Each must appear in the call's argv,
	// elements adjacent and in order, and its flag exactly once: a harness takes
	// the last value of a repeated flag, so a second `--tools default` undoes it.
	Args [][]string `yaml:"args,omitempty"`
	// Refuses are flags whose presence in the call's own arguments brings a
	// tool back (a server's config, an allow list, a plugin). The mcp block's
	// arguments are judged apart: they may allow only the engine's tools.
	Refuses []string `yaml:"refuses,omitempty"`
	// Note says how the construction was measured.
	Note string `yaml:"note,omitempty"`
}

// ToolsOffProblem says why the argv this profile runs with tools is NOT left
// with no tool of its own, or "" when it is.
func (p *Profile) ToolsOffProblem() string {
	if p.ToolsOff == nil {
		return "the profile has no measured way to run the harness with no tool of its own"
	}
	if p.MCP == nil {
		return "the profile has no mcp block, so the engine's tools cannot reach its harness"
	}
	complete := p.CompleteArgs
	if len(p.MCP.CompleteArgs) > 0 {
		complete = p.MCP.CompleteArgs
	}
	if problem := p.ToolsOff.problemIn(complete); problem != "" {
		return problem
	}
	if in := p.ImageInput; in != nil {
		if problem := p.ToolsOff.problemIn(append(slices.Clone(complete), in.Args...)); problem != "" {
			return "with images: " + problem
		}
	}
	return p.mcpAllowProblem()
}

// mcpAllowProblem says why the mcp block's allow list admits a tool that is
// not the engine's, or "" when every tool it names is one, spelled with the
// prefix the harness gives the engine's server ([Requests.HostPrefix]). A block
// allowing nothing by name admits nothing of its own either.
func (p *Profile) mcpAllowProblem() string {
	prefix := ""
	if p.Requests != nil {
		prefix = p.Requests.HostPrefix
	}
	for _, arg := range p.MCP.AllowArgs {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		if prefix == "" {
			return fmt.Sprintf("mcp.allow_args admits %q, and the profile names no "+
				"requests.host_prefix to tell the engine's tools from another's", arg)
		}
		if !strings.HasPrefix(arg, prefix) {
			return fmt.Sprintf("mcp.allow_args admits %q, which is not one of the engine's tools", arg)
		}
	}
	return ""
}

// problemIn judges one argv.
func (t *ToolsOff) problemIn(argv []string) string {
	for _, run := range t.Args {
		if len(run) == 0 {
			return "tools_off.args holds an empty run"
		}
		if !containsRun(argv, run) {
			return fmt.Sprintf("the argv does not carry %q", strings.Join(run, " "))
		}
		if n := countOf(argv, run[0]); n != 1 {
			return fmt.Sprintf("the argv carries %s %d times, and the harness takes the last", run[0], n)
		}
	}
	for _, flag := range t.Refuses {
		if slices.Contains(argv, flag) {
			return fmt.Sprintf("the argv carries %s, which brings a tool back", flag)
		}
	}
	return ""
}

// containsRun reports whether run appears in argv, adjacent and in order.
func containsRun(argv, run []string) bool {
	for i := 0; i+len(run) <= len(argv); i++ {
		if slices.Equal(argv[i:i+len(run)], run) {
			return true
		}
	}
	return false
}

// countOf counts the arguments equal to arg.
func countOf(argv []string, arg string) int {
	n := 0
	for _, a := range argv {
		if a == arg {
			n++
		}
	}
	return n
}

// EffectiveWebTools is the web stance with its default applied: empty reads
// as [WebToolsAllowed].
func (p *Profile) EffectiveWebTools() WebTools {
	if p.WebTools == "" {
		return WebToolsAllowed
	}
	return p.WebTools
}

// CanDenyWeb reports whether the profile has a way to turn its web tools off.
func (p *Profile) CanDenyWeb() bool { return p.WebDenial.changes() }

// withWebDenied is the profile run with web off: stance set, every field the
// denial names replaced. The denial is kept (the doctor prints it), so applying
// it twice is applying it once.
func (p Profile) withWebDenied() Profile {
	out := p.Clone()
	out.WebTools = WebToolsDenied
	if d := p.WebDenial; d != nil {
		if d.CompleteArgs != nil {
			out.CompleteArgs = append([]string(nil), d.CompleteArgs...)
		}
		if d.SeedFiles != nil {
			out.SeedFiles = append([]SeedFile(nil), d.SeedFiles...)
		}
		if d.SystemPrompt != nil {
			out.SystemPrompt = d.SystemPrompt.clone()
		}
		if d.AppendSystemPrompt != nil {
			out.AppendSystemPrompt = d.AppendSystemPrompt.clone()
		}
		if dm := d.MCP; dm != nil && out.MCP != nil {
			if dm.CompleteArgs != nil {
				out.MCP.CompleteArgs = append([]string(nil), dm.CompleteArgs...)
			}
			if dm.Seed != nil {
				out.MCP.Seed = append([]SeedFile(nil), dm.Seed...)
			}
		}
	}
	return out
}

// credentialWords are the name components marking a secret. Whole components,
// not substrings: a substring match read CLAUDE_CODE_MAX_OUTPUT_TOKENS as a
// credential. TOKENS is deliberately absent, being how every harness spells a
// count of model tokens.
var credentialWords = []string{
	"KEY", "KEYS", "APIKEY", "ACCESSKEY", "SECRETKEY", "TOKEN", "SECRET",
	"PASSWORD", "PASSWD", "CREDENTIAL", "CREDENTIALS", "AUTH", "OAUTH", "PAT",
}

// IsCredentialName reports whether a variable name says it carries a secret:
// any component, cut at non-alphanumerics and compared case-insensitively, is
// one of [credentialWords]. One predicate for what passthrough_env and env
// refuse, which env sign a harness in, and what travels into a coding box.
func IsCredentialName(name string) bool {
	components := strings.FieldsFunc(strings.ToUpper(name), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for _, component := range components {
		if slices.Contains(credentialWords, component) {
			return true
		}
	}
	return false
}

// TakesModel reports whether an element of model_args carries {model}: the
// PLACEHOLDER, not merely a flag (`["--model", "fixed"]` passes `fixed`
// whatever the entry's `model`). Config and the backend's constructor share it.
func (p *Profile) TakesModel() bool {
	return slices.ContainsFunc(p.ModelArgs, func(arg string) bool {
		return strings.Contains(arg, modelPlaceholder)
	})
}

// modelPlaceholder is what a model_args element carries the entry's model as.
const modelPlaceholder = "{model}"

// ModelArgument is the model as a run hands it to the harness: the VALUE of
// the model_args element carrying {model}, rendered (an override such as
// "openrouter/{model}" is part of it); the model itself when none does. One
// rendering for host argv, executor box and config rule, or box and host name
// different models. The value, not the element: a box's runner writes its own
// flag, so `--model={model}` is read past its first `=`.
func (p *Profile) ModelArgument(model string) string {
	for _, arg := range p.ModelArgs {
		at := strings.Index(arg, modelPlaceholder)
		if at < 0 {
			continue
		}
		if strings.HasPrefix(arg, "-") {
			if eq := strings.IndexByte(arg, '='); eq >= 0 && eq < at {
				arg = arg[eq+1:]
			}
		}
		return strings.ReplaceAll(arg, modelPlaceholder, model)
	}
	return model
}

// EffectiveMarkerScope is the scope with its default applied.
func (p *Profile) EffectiveMarkerScope() MarkerScope {
	if p.MarkerScope == "" {
		return MarkerScopeAnswerAndStderr
	}
	return p.MarkerScope
}

// HomeIsolation is the variables the engine points inside a seat's home (beside
// it, for XDG_CACHE_HOME) for every harness, whatever its profile says.
// Declared here so config, which refuses an entry overriding one, can name them
// without importing the backend.
var HomeIsolation = []string{
	"HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "TMPDIR",
}

// IsolationVariables names every variable the engine sets to keep this harness
// inside its seat's home and call directory: [HomeIsolation], then
// [Profile.ConfigEnv], [Profile.WorkConfigEnv] and [Profile.IsolationEnv], each
// sorted. One list for the places that must not set one (the profile's env and
// passthrough_env, an entry's env): each is layered OVER the isolation, so a
// value there silently replaces the seat's home with the engine user's
// dotfiles, a directory no system prompt is written to, or a switch turned off.
func (p *Profile) IsolationVariables() []string {
	names := slices.Clone(HomeIsolation)
	names = append(names, slices.Sorted(maps.Keys(p.ConfigEnv))...)
	names = append(names, slices.Sorted(maps.Keys(p.WorkConfigEnv))...)
	return append(names, slices.Sorted(maps.Keys(p.IsolationEnv))...)
}

// IsolationLayer is which layer keeping a harness inside its seat's home and
// call directory sets one of [Profile.IsolationVariables], and so which field,
// if any, changes the child's value.
type IsolationLayer string

// The layers, each but the engine's own named by the profile field that sets
// it (overrides.<field>).
const (
	// IsolationHome is [HomeIsolation], set by the engine for every harness and
	// changed by no field: config_env, work_config_env and isolation_env each
	// refuse every one of them.
	IsolationHome IsolationLayer = "home"
	// IsolationConfigEnv is a [Profile.ConfigEnv] relocation under the
	// seat's home.
	IsolationConfigEnv IsolationLayer = "config_env"
	// IsolationWorkConfigEnv is a [Profile.WorkConfigEnv] relocation under
	// the call's working directory.
	IsolationWorkConfigEnv IsolationLayer = "work_config_env"
	// IsolationSwitch is a [Profile.IsolationEnv] switch.
	IsolationSwitch IsolationLayer = "isolation_env"
)

// IsolationLayerOf reports which layer sets name, false when it is not one of
// [Profile.IsolationVariables]. One layer per variable in a loaded profile,
// since each field refuses the others' names ([Profile.Validate]). For refusals
// that must say WHERE a value is changed: an entry's env naming
// OPENCODE_CONFIG_DIR would otherwise be sent to overrides.config_env, which
// refuses it as work_config_env's.
func (p *Profile) IsolationLayerOf(name string) (IsolationLayer, bool) {
	switch {
	case slices.Contains(HomeIsolation, name):
		return IsolationHome, true
	case hasKey(p.ConfigEnv, name):
		return IsolationConfigEnv, true
	case hasKey(p.WorkConfigEnv, name):
		return IsolationWorkConfigEnv, true
	case hasKey(p.IsolationEnv, name):
		return IsolationSwitch, true
	}
	return "", false
}

// hasKey reports whether a map names key, whatever its value.
func hasKey(m map[string]string, key string) bool {
	_, ok := m[key]
	return ok
}

// KeptUnder names the fields, once each in field order, whose paths this
// profile keeps inside the directory a relocation variable ([Profile.ConfigEnv]
// or [Profile.WorkConfigEnv]) points at; nil if none. They must move with the
// directory: the harness finds each path THROUGH the variable (CODEX_HOME's
// login and sessions, the policy and agent file under OPENCODE_CONFIG_DIR), so
// repointing strands a harness that never reads its login, never has sessions
// pruned or never sees the policy denying it a shell. A profile moving the
// directory without them does not load ([Load]).
func (p *Profile) KeptUnder(name string) []string {
	var dir string
	var scope SeedScope
	switch {
	case hasKey(p.ConfigEnv, name):
		dir, scope = p.ConfigEnv[name], SeedHome
	case hasKey(p.WorkConfigEnv, name):
		dir, scope = p.WorkConfigEnv[name], SeedWork
	default:
		return nil
	}
	var fields []string
	for _, k := range p.kept(scope) {
		if within(k.path, dir) && !slices.Contains(fields, k.field) {
			fields = append(fields, k.field)
		}
	}
	return fields
}

// keptPath is one path a profile keeps in a scope: its field, the entry as an
// operator would find it, and the path as written.
type keptPath struct {
	field, label, path string
}

// kept lists the paths a relocation of one scope can strand. Under the seat's
// home: login files, volatile paths and home seed files. Under the call's
// directory: its seed files and a discovered system prompt file (found by
// location; a `{file}` one is handed over by path, wherever it is).
func (p *Profile) kept(scope SeedScope) []keptPath {
	var out []keptPath
	add := func(field, label, path string) {
		out = append(out, keptPath{field: field, label: label, path: path})
	}
	if scope == SeedHome {
		for i, rel := range p.CredentialPaths {
			add("credential_paths", fmt.Sprintf("credential_paths[%d]", i), rel)
		}
		for i, rel := range p.VolatilePaths {
			add("volatile_paths", fmt.Sprintf("volatile_paths[%d]", i), rel)
		}
		if p.HomeDaemon != nil {
			add("home_daemon", "home_daemon.pid_file", p.HomeDaemon.PIDFile)
		}
	}
	for i, f := range p.SeedFiles {
		if f.Scope() == scope {
			add("seed_files", fmt.Sprintf("seed_files[%d].path", i), f.Path)
		}
	}
	if scope == SeedHome {
		for i, f := range p.KeySeedFiles {
			add("key_seed_files", fmt.Sprintf("key_seed_files[%d].path", i), f.Path)
		}
	}
	if scope == SeedWork {
		for _, ch := range p.channels() {
			if ch.File.Found() {
				add(ch.field+".file.name", ch.field+".file.name", ch.File.Name)
			}
		}
	}
	return out
}

// within reports whether rel is dir or lies under it, both relative to one
// root and compared clean: `.codex/auth.json` is within `.codex/`, and every
// path is within `.`, the root itself.
func within(rel, dir string) bool {
	rel, dir = filepath.Clean(rel), filepath.Clean(dir)
	return dir == "." || rel == dir || strings.HasPrefix(rel, dir+string(filepath.Separator))
}

// namedChannel is a system-prompt channel with its field, for messages.
type namedChannel struct {
	field string
	Channel
}

// channels lists the profile's two system-prompt channels, replace first.
func (p *Profile) channels() []namedChannel {
	return []namedChannel{
		{field: "system_prompt", Channel: p.SystemPrompt},
		{field: "append_system_prompt", Channel: p.AppendSystemPrompt},
	}
}

// hasPlaceholder reports whether any entry of an argv template carries one.
func hasPlaceholder(template []string, placeholder string) bool {
	return slices.ContainsFunc(template, func(arg string) bool {
		return strings.Contains(arg, placeholder)
	})
}

// isBareFileName reports whether name is a single path element that stays put
// when joined onto a directory.
func isBareFileName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsRune(name, '/') && !strings.ContainsRune(name, filepath.Separator)
}

// namesAFile reports whether a relative path that [staysInside] names a FILE
// under the directory it is joined onto, not that directory or one spelled with
// a trailing separator: its last element, as written, is a [isBareFileName],
// and it does not clean to the directory itself.
//
// As written, not cleaned: cleaning hides the shape (`x/..` cleans to `.`,
// `agents/` to `agents`, which reads as a file name though a directory was written).
func namesAFile(rel string) bool {
	if strings.HasSuffix(rel, "/") || strings.HasSuffix(rel, string(filepath.Separator)) {
		return false
	}
	last := rel[strings.LastIndexAny(rel, "/"+string(filepath.Separator))+1:]
	return isBareFileName(last) && filepath.Clean(rel) != "."
}

// Validate reports what is wrong with a profile, naming the override field an
// operator would edit rather than the Go field they cannot see.
func (p *Profile) Validate(name string) error {
	return profileError(name, p.problems())
}

// profileError is the one error a profile's problems are reported as, or nil
// for none.
func profileError(name string, bad []string) error {
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("harness profile %q: %s", name, strings.Join(bad, "; "))
}

// problems is what [Profile.Validate] reports, one sentence each.
func (p *Profile) problems() []string {
	var bad []string
	add := func(format string, args ...any) {
		bad = append(bad, fmt.Sprintf(format, args...))
	}

	p.validateEffort(add)
	if strings.TrimSpace(string(p.Binary)) == "" {
		add("binary is empty — set overrides.binary")
	}
	if len(p.CompleteArgs) == 0 && p.EffectivePromptMode() == PromptStdin {
		// Only stdin mode can have NO argv: the others build one from
		// prompt_args, so `mycli --prompt-file <path>` alone is legitimate.
		add("complete_args is empty — set overrides.complete_args")
	}
	if p.PromptMode != "" && !p.PromptMode.Valid() {
		add("prompt_mode %q (want stdin, argv or file)", p.PromptMode)
	}
	if p.Output != "" && !p.Output.Valid() {
		add("output %q (want json, jsonl or text)", p.Output)
	}
	if p.Output != OutputText && len(p.TextPaths) == 0 {
		// The defaulted mode, not the raw (possibly empty) field.
		add("text_paths is empty — a %s profile must say where the answer is", p.EffectiveOutput())
	}
	p.validateRelocations(add)
	if len(p.PromptArgs) > 0 && p.EffectivePromptMode() != PromptArgv && p.EffectivePromptMode() != PromptFile {
		add("prompt_args is set but prompt_mode is %q — the flag introduces a prompt "+
			"on argv and there is none to introduce", p.EffectivePromptMode())
	}
	if p.EffectivePromptMode() == PromptFile && !hasPlaceholder(p.PromptArgs, "{file}") {
		// Refused, not defaulted to a bare append: with no path in argv the
		// harness gets no prompt and opens an interactive session or prints
		// usage, neither of which looks like a configuration error.
		add("prompt_mode is file but no prompt_args entry contains {file} — set " +
			`overrides.prompt_args, e.g. ["--prompt-file", "{file}"]`)
	}
	switch {
	case len(p.EventTypePath) > 0 && len(p.TextEvents) == 0:
		add("event_type_path is set but text_events is empty — naming where the " +
			"event kind lives says nothing about which kinds carry the answer")
	case len(p.EventTypePath) == 0 && len(p.TextEvents) > 0:
		add("text_events is set but event_type_path is empty — there is nothing to " +
			"compare the event names against")
	case len(p.EventTypePath) > 0 && p.EffectiveOutput() != OutputJSONL:
		// Refused rather than ignored: a filter that silently does nothing
		// is a debugging session.
		add("event_type_path is set but output is %q — an event discriminator "+
			"only exists in a jsonl stream", p.EffectiveOutput())
	}
	p.validateEveryStep(add)
	for _, ch := range p.channels() {
		ch.validate(add)
	}
	if p.APIKeyEnvShared && p.APIKeyEnv == "" {
		add("api_key_env_shared is set but api_key_env is empty — it says the key " +
			"variable is one other vendors' keys go in too, and there is no variable")
	}
	p.validateUsageCommand(add)
	if len(p.UsageFileArgs) > 0 {
		if !hasPlaceholder(p.UsageFileArgs, "{usage_file}") {
			add("usage_file_args carries no {usage_file} placeholder — there is " +
				`nothing to substitute the path into, e.g. ["--usage-file", "{usage_file}"]`)
		}
		// Both counts, because [extracted.applyUsageReport] accepts a report
		// only when both resolve (a partial overlay would pair one source's
		// input with another's output, and the sum is charged). A profile
		// declaring one would estimate on EVERY call while
		// [Profile.ReadsUsage] and `crewlet llm doctor` call it the vendor's figures.
		if len(p.Usage.Input) == 0 || len(p.Usage.Output) == 0 {
			add("usage_file_args is set but usage.input and usage.output are not both " +
				"declared — the report is only read when both counts resolve, so every " +
				"call would fall back to an estimate while the doctor reports otherwise")
		}
	}
	switch {
	case p.MarkerScope != "" && !p.MarkerScope.Valid():
		add("marker_scope %q (want %s, %s or %s)", p.MarkerScope,
			MarkerScopeAnswerAndStderr, MarkerScopeStderr, MarkerScopeEvents)
	case p.MarkerScope == MarkerScopeEvents:
		p.validateEventMarkers(add)
	case len(p.MarkerEvents) > 0 || len(p.MarkerPath) > 0:
		// Refused rather than ignored: under any other scope markers would
		// be matched as substrings of the answer and stderr while the
		// profile appeared to say otherwise.
		add("marker_events or marker_path is set but marker_scope is %q — they say "+
			"where the %s scope looks, and no other scope reads them; set "+
			"marker_scope: %s, or drop them", p.EffectiveMarkerScope(), MarkerScopeEvents,
			MarkerScopeEvents)
	}
	for i, m := range p.LimitMarkers {
		if problem := sentinelProblem(m.Sentinel); problem != "" {
			add("limit_markers[%d].sentinel %s", i, problem)
		}
		m.Reset.validate(fmt.Sprintf("limit_markers[%d]", i), add)
	}
	p.validateLimitEvents(add)
	p.validateEventReports(add)
	p.validateAPIKeyModelEnv(add)
	p.validateSignInArgs(add)
	p.validateSignIn(add)
	// An auth marker fires KindAuth, which exhausts the credential as a
	// spent plan does, so an unusable sentinel costs the same as above.
	for i, m := range p.AuthMarkers {
		if problem := sentinelProblem(m.Sentinel); problem != "" {
			add("auth_markers[%d].sentinel %s", i, problem)
		}
		switch m.Meaning {
		case "", AuthRefused, AuthUnread:
		default:
			add("auth_markers[%d].meaning is %q (want %s or %s)", i, m.Meaning, AuthRefused, AuthUnread)
		}
	}
	if p.StdinLogin != nil && len(p.StdinLogin.Args) == 0 {
		add("stdin_login.args is empty")
	}
	if store := p.CredentialStore; store != nil {
		// Both halves or neither: no import leaves every seat home empty;
		// no export never writes back a refreshed token, logging the fleet
		// out at the next expiry.
		if len(store.ExportArgs) == 0 {
			add("credential_store.export_args is empty — a login held in the harness's " +
				"own database can only leave it through the harness's export command")
		}
		if len(store.ImportArgs) == 0 {
			add("credential_store.import_args is empty — a seat home starts from an " +
				"empty database, and only the harness's import command puts the login in it")
		}
		for _, args := range [][]string{store.ExportArgs, store.ImportArgs} {
			if hasPlaceholder(args, "{file}") {
				// A credential goes on stdin, which no other account can read;
				// a path would also be a file to remember to remove.
				add("credential_store args carry {file} — the document travels on " +
					"stdin and stdout, never through a path")
				break
			}
		}
		for _, rel := range p.CredentialPaths {
			if filepath.Base(rel) == CredentialStoreDocument {
				// Both kinds are kept under base names, so one would
				// overwrite the other in the shared directory and bundles.
				add("credential_paths names %q, whose file name is the one the "+
					"credential store's document is kept under (%s)", rel, CredentialStoreDocument)
			}
		}
	}
	p.validateHomePaths(add)
	if p.LocalTools != "" && !p.LocalTools.Valid() {
		add("local_tools %q (want denied or vendor-default)", p.LocalTools)
	}
	if p.LocalTools == LocalToolsVendorDefault && strings.TrimSpace(p.LocalToolsNote) == "" {
		// A stance admitting the harness's tools without a reason is the
		// silent hole this field exists to expose.
		add("local_tools is vendor-default but local_tools_note is empty — say which " +
			"denial the vendor lacks")
	}
	p.validateWebStance(add)
	p.validateToolsOff(add)
	p.validateImageInput(add)
	for i, f := range p.SeedFiles {
		if strings.TrimSpace(f.Path) == "" {
			add("seed_files[%d].path is empty", i)
			continue
		}
		if f.In != "" && !f.In.Valid() {
			add("seed_files[%d].in %q (want home or work)", i, f.In)
		}
		switch {
		case !staysInside(f.Path):
			// What seeding's UnderRoot proves (relative, not escaping the
			// scope), asked before any seat home exists.
			add("seed_files[%d].path %q must be relative and stay inside the %s directory",
				i, f.Path, f.Scope())
		case !namesAFile(f.Path):
			// UnderRoot accepts the scope directory itself, so `.` and `x/..`
			// would write onto the home or working directory (EISDIR on every
			// call), and `.crewlet-config/` writes a FILE where a directory is
			// needed (ENOTDIR on the system prompt's write). A channel's
			// file.name is held to the same rule. The same directory spelled
			// as a file name, `.crewlet-config`, is a conflict between two
			// fields, refused by [Profile.validateCollisions].
			add("seed_files[%d].path %q must name a FILE inside the %s directory — not "+
				"the directory itself, not a path ending in a separator, and not one "+
				"whose last element is . or .. — e.g. .vendor/settings.json",
				i, f.Path, f.Scope())
		}
		if f.Content == "" {
			add("seed_files[%d].content is empty — a settings file with nothing in it "+
				"configures nothing", i)
		}
	}
	for i, f := range p.KeySeedFiles {
		label := fmt.Sprintf("key_seed_files[%d]", i)
		switch {
		case f.Scope() != SeedHome:
			add("%s.in is %q — a key home's own settings are written into the seat's home "+
				"(in: home, the default); a call's working directory is written per call "+
				"and knows nothing of how the call signs in", label, f.In)
		case strings.TrimSpace(f.Path) == "":
			add("%s.path is empty", label)
		case !staysInside(f.Path):
			add("%s.path %q must be relative and stay inside the home directory", label, f.Path)
		case !namesAFile(f.Path):
			add("%s.path %q must name a FILE inside the home directory — not the directory "+
				"itself, not a path ending in a separator, and not one whose last element is "+
				". or .. — e.g. .vendor/settings.json", label, f.Path)
		}
		if f.Content == "" {
			add("%s.content is empty — a settings file with nothing in it configures nothing", label)
		}
	}
	p.validateMCP(add)
	p.validateRequests(add)
	p.validateCap(add)
	p.validateFailureMarkers(add)
	p.validateCollisions(add)
	return bad
}

// validateEveryStep refuses [PathEvery] in every path reading ONE value (the
// answer, failure flag, event kind, failure class); only a usage count may take
// it ([Path]). Not read as the first member: a decoded object's members are
// unordered, so the same stdout would answer differently call to call.
func (p *Profile) validateEveryStep(add func(string, ...any)) {
	for _, field := range []struct {
		name  string
		paths []Path
	}{
		{"text_paths", p.TextPaths},
		{"error_paths", p.ErrorPaths},
		{"event_type_path", []Path{p.EventTypePath}},
		{"marker_path", []Path{p.MarkerPath}},
	} {
		for _, path := range field.paths {
			if path.HasEvery() {
				add("%s %q takes %q, which only a usage path may: a count under every "+
					"member adds up, and %s reads one value", field.name, path.String(), PathEvery, field.name)
			}
		}
	}
}

// validateWebStance holds the web stance and its denial to their rules.
//
// A profile keeping web on but carrying a denial has it validated as the
// profile it would become, so a broken web_denial is refused where written, not
// on the first entry that turns web off.
func (p *Profile) validateWebStance(add func(string, ...any)) {
	if p.WebTools != "" && !p.WebTools.Valid() {
		add("web_tools %q (want allowed, denied or search)", p.WebTools)
		return
	}
	if w := p.WebSearch; w != nil {
		if len(w.CompleteArgs) == 0 || len(w.Args) == 0 || strings.TrimSpace(w.Tool) == "" {
			add("web_search needs complete_args, the args runs it must carry and its tool")
		}
		if !w.Format.Valid() {
			add("web_search.format %q (want %s)", w.Format, SearchClaudeStreamJSON)
		}
		if strings.TrimSpace(w.Note) == "" {
			add("web_search.note is empty: say how the search-only run was measured")
		}
		if problem := p.SearchProblem(); problem != "" && len(w.CompleteArgs) > 0 {
			add("web_search: %s", problem)
		}
	}
	if p.EffectiveWebTools() == WebToolsSearch && p.WebSearch == nil {
		add("web_tools is search but the profile has no web_search, so this harness has " +
			"no measured search-only run")
	}
	if p.WebDenial != nil && !p.WebDenial.changes() {
		add("web_denial sets none of complete_args, seed_files, system_prompt and " +
			"append_system_prompt, so it turns nothing off; say what the harness runs with " +
			"instead, or remove it")
		return
	}
	if p.WebDenial != nil && p.WebDenial.MCP != nil && p.MCP == nil {
		add("web_denial.mcp is set but the profile has no mcp block to replace it in")
	}
	switch {
	case p.EffectiveWebTools() == WebToolsDenied && p.WebDenial == nil:
		// Refused rather than run: a helper runs an entry on exactly this
		// claim, which the harness would not honour.
		add("web_tools is denied but the profile has no web_denial, so this harness "+
			"has no measured way to turn its web tools off (%s); set "+
			"overrides.web_denial, or keep web_tools allowed", orUnstated(p.WebToolsNote))
	case p.EffectiveWebTools() == WebToolsAllowed && p.WebDenial != nil:
		denied := p.withWebDenied()
		for _, problem := range denied.problems() {
			add("with web_tools denied, %s", problem)
		}
	}
}

// ImageInput is how a harness takes images beside its prompt: Args switch its
// headless run to structured input on stdin, written in Format, whose one user
// message carries the prompt and then each image. Args are judged with the
// rest of the argv by [Profile.ToolsOffProblem], so they can bring no tool back.
type ImageInput struct {
	Args   []string    `yaml:"args"`
	Format ImageFormat `yaml:"format"`
	// Note says what the input is and where it is documented.
	Note string `yaml:"note,omitempty"`
}

// ImageFormat is the structured input a harness reads its images in.
type ImageFormat string

// ImageClaudeStreamJSON is Claude Code's headless stream-json input: one JSON
// line per message, a user message's content a list of a text block and
// base64 image blocks.
const ImageClaudeStreamJSON ImageFormat = "claude-stream-json"

// Valid reports whether f is a format this build writes.
func (f ImageFormat) Valid() bool { return f == ImageClaudeStreamJSON }

// validateImageInput holds an image_input block to its shape: arguments, a
// format this build writes, a note, and a stdin prompt (where the message goes).
func (p *Profile) validateImageInput(add func(string, ...any)) {
	in := p.ImageInput
	if in == nil {
		return
	}
	if len(in.Args) == 0 {
		add("image_input.args is empty: name the arguments that switch the harness to its structured input")
	}
	if !in.Format.Valid() {
		add("image_input.format %q (want %s)", in.Format, ImageClaudeStreamJSON)
	}
	if strings.TrimSpace(in.Note) == "" {
		add("image_input.note is empty: say what the input is and where it is documented")
	}
	if p.EffectivePromptMode() != PromptStdin {
		add("image_input needs prompt_mode stdin: the message carrying the images is written there")
	}
}

// validateToolsOff holds a tools_off block to its shape. Whether the argv
// carries it is [Profile.ToolsOffProblem]'s question, asked only by the caller
// that requires it: a seat's argv deliberately keeps the web.
func (p *Profile) validateToolsOff(add func(string, ...any)) {
	t := p.ToolsOff
	if t == nil {
		return
	}
	if len(t.Args) == 0 {
		add("tools_off.args is empty: name the arguments that leave the harness with no tool")
	}
	for i, run := range t.Args {
		if len(run) == 0 {
			add("tools_off.args[%d] is empty", i)
		}
	}
	if p.LocalTools != LocalToolsDenied {
		add("tools_off is set but local_tools is %q: a harness run with no tool denies its local tools", p.LocalTools)
	}
	if strings.TrimSpace(t.Note) == "" {
		add("tools_off.note is empty: say how the construction was measured")
	}
}

// orUnstated renders an optional note inside a sentence.
func orUnstated(note string) string {
	if strings.TrimSpace(note) == "" {
		return "the profile does not say why"
	}
	return note
}

// pathClaim is one path a profile has the engine WRITE a file at, or the
// harness read a DIRECTORY at, in one scope: its naming field and clean path.
type pathClaim struct {
	label, path string
	dir         bool
}

// claims lists every path this profile claims in one scope whose own shape is
// valid (a refused shape is reported there, not again). Home: login and seed
// files are written, each config_env directory read. Call directory: seed
// files and the system prompt file written, each work_config_env directory read.
func (p *Profile) claims(scope SeedScope) []pathClaim {
	var out []pathClaim
	file := func(label, rel string) {
		out = append(out, pathClaim{label: label, path: filepath.Clean(rel)})
	}
	if scope == SeedHome {
		for i, rel := range p.CredentialPaths {
			if staysInside(rel) && namesAFile(rel) {
				file(fmt.Sprintf("credential_paths[%d]", i), rel)
			}
		}
	}
	if scope == SeedWork {
		// First, so a seed at this path is the one refused, as engine-owned.
		file("the call's project-root marker", ProjectRootMarker)
	}
	seeded := map[string]bool{}
	for i, f := range p.SeedFiles {
		if f.Scope() == scope && staysInside(f.Path) && namesAFile(f.Path) {
			file(fmt.Sprintf("seed_files[%d].path", i), f.Path)
			seeded[filepath.Clean(f.Path)] = true
		}
	}
	if scope == SeedHome {
		// One at a seed_files path is that file's key-home content, written
		// in its place: the same claim. Elsewhere it is held as a seed is.
		for i, f := range p.KeySeedFiles {
			if !staysInside(f.Path) || !namesAFile(f.Path) {
				continue
			}
			clean := filepath.Clean(f.Path)
			if seeded[clean] {
				// Replaces it once; a second key seed here is refused below.
				seeded[clean] = false
				continue
			}
			file(fmt.Sprintf("key_seed_files[%d].path", i), f.Path)
		}
	}
	if scope == SeedWork {
		// Written by every call taking a file channel, so a seed at this
		// path is overwritten before the harness reads it. One claim per
		// path across the two channels: a call takes one, so two naming one
		// file (Kimi's agent file) write it once per call.
		claimed := map[string]bool{}
		for _, ch := range p.channels() {
			if !ch.WritesFile() {
				continue
			}
			// Shapes the channel's own rules admit: a discovered file is a
			// path inside the call directory, a handed-over one a bare
			// name. A malformed one is that rule's to report.
			name := ch.File.FileName()
			claimable := isBareFileName(name)
			if ch.File.Found() {
				claimable = staysInside(name) && namesAFile(name)
			}
			if !claimable {
				continue
			}
			if clean := filepath.Clean(name); !claimed[clean] {
				claimed[clean] = true
				file(ch.field+".file.name", name)
			}
		}
	}
	if scope == SeedWork {
		// Written on runs with tools alone, after the seeds, replacing any
		// at the same path on exactly those runs.
		out = append(out, p.mcpClaims()...)
	}
	field, relocations := "config_env", p.ConfigEnv
	if scope == SeedWork {
		field, relocations = "work_config_env", p.WorkConfigEnv
	}
	for _, name := range slices.Sorted(maps.Keys(relocations)) {
		rel := relocations[name]
		if strings.TrimSpace(rel) != "" && staysInside(rel) {
			out = append(out, pathClaim{label: field + "." + name, path: filepath.Clean(rel), dir: true})
		}
	}
	return out
}

// validateCollisions holds the paths claimed in each scope ([Profile.claims])
// apart: no written file may be a path another claim needs as a DIRECTORY (a
// relocated directory, or an ancestor of any claim), and no two writes may
// name one file.
//
// [namesAFile] cannot see this: the conflict is between two fields.
// `.crewlet-config` is a file name, but opencode needs it as
// OPENCODE_CONFIG_DIR's directory (ENOTDIR on every call), as codex needs
// `.codex`. A seed at a login path replaces the seeded login and is carried
// back over the shared one. One refusal per file, naming the first collision.
func (p *Profile) validateCollisions(add func(string, ...any)) {
	for _, scope := range []SeedScope{SeedHome, SeedWork} {
		claims := p.claims(scope)
		for i, a := range claims {
			if a.dir {
				continue
			}
			if problem := collision(claims, i, scope); problem != "" {
				add("%s", problem)
			}
		}
	}
}

// collision is what is wrong with the file claims[i] beside its scope's other
// claims, or "": first a relocated directory at its path (the plainest
// conflict), then an earlier write of the same file, then the first claim
// needing it as a directory above itself.
func collision(claims []pathClaim, i int, scope SeedScope) string {
	a := claims[i]
	for _, b := range claims {
		if b.dir && b.path == a.path {
			return fmt.Sprintf("%s %q is the directory %s points the harness at, in the %s "+
				"directory — a file there leaves it no directory to read, and every call "+
				"fails; name a file inside it", a.label, a.path, b.label, scope)
		}
	}
	for _, b := range claims[:i] {
		if b.path == a.path {
			return fmt.Sprintf("%s %q is the file %s names too, in the %s directory — one "+
				"of the two writes replaces the other on every call; give each its own path",
				a.label, a.path, b.label, scope)
		}
	}
	for _, b := range claims {
		if strings.HasPrefix(b.path, a.path+string(filepath.Separator)) {
			return fmt.Sprintf("%s %q is a FILE where %s needs a directory (%q lies under "+
				"it, in the %s directory), so every call fails with ENOTDIR; name a file that "+
				"is no other path's directory", a.label, a.path, b.label, b.path, scope)
		}
	}
	return ""
}

// validateHomePaths holds [Profile.CredentialPaths],
// [Profile.HostCredentialPaths] and [Profile.VolatilePaths] to the shape each
// names, at load as a seed path is.
//
// Each is operator-overridable and joined onto a directory only at run time,
// where [UnderRoot] alone would refuse one climbing out of the home, on EVERY
// call. A login path is also a FILE, carried by its base name: `.` and `x/..`
// fail with EISDIR, and two sharing a base name are ONE file in the shared
// directory and every bundle (one named [BundleManifest] collides with the
// manifest). A volatile path may name a directory: held only to staying inside.
func (p *Profile) validateHomePaths(add func(string, ...any)) {
	for _, list := range []struct {
		field, home string
		paths       []string
	}{
		{"credential_paths", "seat home", p.CredentialPaths},
		{"host_credential_paths", "person's home", p.HostCredentialPaths},
	} {
		for i, rel := range list.paths {
			switch {
			case !staysInside(rel):
				add("%s[%d] %q must be relative and stay inside the %s", list.field, i, rel, list.home)
			case !namesAFile(rel):
				add("%s[%d] %q must name a login FILE inside the %s — not the directory "+
					"itself, not a path ending in a separator, and not one whose last "+
					"element is . or ..", list.field, i, rel, list.home)
			}
		}
	}
	byName := map[string]string{}
	for i, rel := range p.CredentialPaths {
		name := filepath.Base(rel)
		if name == BundleManifest {
			add("credential_paths[%d] %q has the file name %s, which every bundle keeps its "+
				"manifest under — the entry naming the harness it was exported from — so a login "+
				"file by that name could not travel in one", i, rel, BundleManifest)
			continue
		}
		if name == LoginInstallFile {
			add("credential_paths[%d] %q has the file name %s, which the engine keeps a login's "+
				"install record under beside the login, so a login file by that name would be "+
				"read as the record", i, rel, LoginInstallFile)
			continue
		}
		if first, taken := byName[name]; taken {
			add("credential_paths names %q and %q, which share the file name %s — the "+
				"shared directory and every bundle keep a login file under its file name, "+
				"so one would overwrite the other", first, rel, name)
			continue
		}
		byName[name] = rel
	}
	for _, name := range slices.Sorted(maps.Keys(p.LoginKeys)) {
		switch {
		case byName[name] == "":
			add("login_keys names %q, which is the file name of no credential_paths entry — "+
				"a login key says which of the credential files holds a login only once it "+
				"carries that key", name)
		case len(p.LoginKeys[name]) == 0:
			add("login_keys.%s is empty — name the key paths the file holds its login under", name)
		}
		for i, key := range p.LoginKeys[name] {
			if slices.Contains(strings.Split(key, "."), "") {
				add("login_keys.%s[%d] %q is no key path — name the key, its parents' keys "+
					"before it, each joined by a dot", name, i, key)
			}
		}
	}
	for i, name := range p.CommentLines {
		if byName[name] == "" {
			add("comment_lines[%d] %q is the file name of no credential_paths entry — comment "+
				"lines say which credential files the harness writes `//` lines into", i, name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(p.RefreshKeys)) {
		if byName[name] == "" {
			add("refresh_keys names %q, which is the file name of no credential_paths entry — "+
				"refresh keys say which fields of a credential file a refresh carries back", name)
		}
		if len(p.RefreshKeys[name]) == 0 {
			add("refresh_keys.%s is empty — name the token fields a refresh carries back", name)
		}
		for i, key := range p.RefreshKeys[name] {
			if parts := strings.Split(key, "."); slices.Contains(parts, "") || slices.Contains(parts, subjectPart) ||
				parts[len(parts)-1] == "*" {
				add("refresh_keys.%s[%d] %q is no key path to a token — name the key, its parents' keys "+
					"before it, each joined by a dot, and a field last, never *", name, i, key)
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(p.IdentityKeys)) {
		if byName[name] == "" {
			add("identity_keys names %q, which is the file name of no credential_paths entry — "+
				"identity keys say which keys of a credential file name its login's account", name)
		}
		if len(p.IdentityKeys[name]) == 0 {
			add("identity_keys.%s is empty — name the keys that name the login's account", name)
		}
		for i, key := range p.IdentityKeys[name] {
			if parts := strings.Split(key, "."); slices.Contains(parts, "") ||
				slices.Index(parts, subjectPart) >= 0 && slices.Index(parts, subjectPart) != len(parts)-1 {
				add("identity_keys.%s[%d] %q is no key path — name the key, its parents' keys "+
					"before it, each joined by a dot, and %s only last", name, i, key, subjectPart)
			}
		}
	}
	// A host file is adopted under its file name and a seat is seeded only with
	// credential_paths' names, so a host path with another name is adopted,
	// reported as found, and read by no seat: copilot listed gh's
	// `.config/gh/hosts.yml`, adopted as `hosts.yml` beside `hosts.json`.
	for i, rel := range p.HostCredentialPaths {
		if name := filepath.Base(rel); staysInside(rel) && namesAFile(rel) && byName[name] == "" {
			add("host_credential_paths[%d] %q has a file name (%s) no credential_paths "+
				"entry has — `crewlet llm login -from-host` keeps an adopted file under its "+
				"file name and seeds a seat only with the files credential_paths names, so "+
				"it would adopt a login no seat reads; name where a seat reads it in "+
				"credential_paths, or drop it", i, rel, name)
		}
	}
	for i, rel := range p.VolatilePaths {
		if !staysInside(rel) {
			add("volatile_paths[%d] %q must be relative and stay inside the seat home", i, rel)
		}
	}
	if d := p.HomeDaemon; d != nil {
		if !staysInside(d.PIDFile) || !namesAFile(d.PIDFile) {
			add("home_daemon.pid_file %q must name a file inside the seat home", d.PIDFile)
		}
		if len(d.StopArgs) == 0 {
			add("home_daemon.stop_args is empty: name the harness's own command that stops " +
				"the daemon of the home it runs in")
		}
	}
}

// validateRelocations holds [Profile.ConfigEnv] and [Profile.WorkConfigEnv] to
// the one shape in which they relocate anything: each variable named once,
// none the engine already points at the seat's home, each directory relative
// and inside its root. It holds [Profile.IsolationEnv] to a vendor variable of
// its own with a value, and env and passthrough_env off every variable any of
// them or [HomeIsolation] names. Without the directory check
// `config_env: {CODEX_HOME: ../..}` would point the vendor's whole
// configuration, sessions and login included, outside the seat's home.
func (p *Profile) validateRelocations(add func(string, ...any)) {
	for _, name := range slices.Sorted(maps.Keys(p.ConfigEnv)) {
		rel := p.ConfigEnv[name]
		switch {
		case slices.Contains(HomeIsolation, name):
			// The engine's own, never a vendor's: the XDG homes and TMPDIR
			// already point inside the seat home (the cache beside it).
			// Relocating one strands whatever the profile keeps at the
			// engine's location: on muse-code `XDG_CONFIG_HOME: .x` left the
			// seeded tool policy in a `.config` the harness no longer read,
			// handing every seat its default tools. It was also the one
			// overlap between isolation layers ([Profile.IsolationLayerOf]).
			add("config_env may not name %s — the engine points it at or inside the "+
				"seat home for every harness already; config_env relocates a variable of "+
				"the vendor's own", name)
		case strings.TrimSpace(rel) == "":
			add("config_env.%s is empty — name the directory under the seat home it points at", name)
		case !staysInside(rel):
			add("config_env.%s %q must be relative and stay inside the seat home", name, rel)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(p.WorkConfigEnv)) {
		rel := p.WorkConfigEnv[name]
		_, alsoHome := p.ConfigEnv[name]
		switch {
		case slices.Contains(HomeIsolation, name):
			// Removed after every call, it would take the login and settings with it.
			add("work_config_env may not name %s — the engine points it at the seat's "+
				"home, which outlives the call", name)
		case alsoHome:
			add("config_env and work_config_env both name %s — one variable names one "+
				"directory; keep it in whichever the harness must not share across calls", name)
		case strings.TrimSpace(rel) == "":
			add("work_config_env.%s is empty — name the directory under the per-call "+
				"working directory it points at", name)
		case !staysInside(rel):
			add("work_config_env.%s %q must be relative and stay inside the per-call "+
				"working directory", name, rel)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(p.IsolationEnv)) {
		_, home := p.ConfigEnv[name]
		_, work := p.WorkConfigEnv[name]
		switch {
		case strings.TrimSpace(name) == "":
			add("isolation_env names an empty variable")
		case slices.Contains(HomeIsolation, name) || home || work:
			// Two values for one variable: the last layer applied wins.
			add("isolation_env names %s, which the engine already points at a directory — "+
				"a switch is a variable of the vendor's own, never a relocated one", name)
		case strings.TrimSpace(p.IsolationEnv[name]) == "":
			// A vendor reading `1` or `true` as on reads empty as OFF: the
			// switch would do the opposite of what it says.
			add("isolation_env.%s is empty — give the value that switches the vendor's "+
				"behaviour off, e.g. \"1\"; an empty one reads as off", name)
		}
	}
	// Layered over the isolation, hence refused here: the child's environment
	// applies env and passthrough after the home variables, so one named here
	// silently replaces the seat's home.
	isolation := p.IsolationVariables()
	for _, name := range slices.Sorted(maps.Keys(p.Env)) {
		if slices.Contains(isolation, name) {
			add("env names %s, which the engine sets to keep the harness inside its seat's "+
				"home and its call's working directory — a value here would replace that "+
				"isolation; relocate a vendor directory with config_env or work_config_env, "+
				"and set a vendor's isolating switch in isolation_env", name)
		}
	}
	for _, name := range p.PassthroughEnv {
		if slices.Contains(isolation, name) {
			add("passthrough_env names %s, which the engine sets to keep the harness inside "+
				"its seat's home and its call's working directory — forwarding the engine's "+
				"own would hand the harness the engine user's directories", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(p.SubscriptionEnv)) {
		_, inEnv := p.Env[name]
		switch {
		case strings.TrimSpace(name) == "":
			add("subscription_env names an empty variable")
		case slices.Contains(isolation, name):
			add("subscription_env names %s, which the engine sets to keep the harness inside "+
				"its seat's home and its call's working directory — a value here would "+
				"replace that isolation on every subscription call", name)
		case inEnv:
			// Two values for one variable: a sign-in choice made in two places.
			add("subscription_env and env both name %s — set it in subscription_env "+
				"alone, which is the only mode it may apply under", name)
		case strings.TrimSpace(p.SubscriptionEnv[name]) == "":
			add("subscription_env.%s is empty — give the value that selects the harness's "+
				"login, e.g. \"true\"", name)
		}
	}
}

// strandedBy reports every relocation an override MOVED that left behind a
// path the built-in kept under it ([Profile.kept] still under the old
// directory, unreached through the moved variable).
//
// Each variable on its own: another variable's directory excuses nothing, nor
// does an ANCESTOR (CODEX_HOME at `.` reads `auth.json`, the login sits at
// `.codex/auth.json`); a path is reached only where the new directory lies
// strictly inside the old and the path inside the new. Stranded, the deny-all
// policy and agent file go unread. Judged against the BUILT-IN, whose layout
// says which paths are read through which variable; a refused new value
// ([Profile.validateRelocations]) is left to that refusal.
func (p *Profile) strandedBy(builtin *Profile) []string {
	var bad []string
	for _, r := range []struct {
		field    string
		scope    SeedScope
		was, now map[string]string
	}{
		{"config_env", SeedHome, builtin.ConfigEnv, p.ConfigEnv},
		{"work_config_env", SeedWork, builtin.WorkConfigEnv, p.WorkConfigEnv},
	} {
		for _, name := range slices.Sorted(maps.Keys(r.was)) {
			// Overrides merge key by key, so the variable is still there;
			// empty (a YAML null) or escaping, it is refused on its own. The
			// same directory, however spelled, strands nothing.
			was, now := filepath.Clean(r.was[name]), r.now[name]
			if strings.TrimSpace(now) == "" || !staysInside(now) {
				continue
			}
			now = filepath.Clean(now)
			if now == was {
				continue
			}
			var left []string
			example := ""
			for _, k := range p.kept(r.scope) {
				if !within(k.path, was) || (within(k.path, now) && !within(was, now)) {
					continue
				}
				left = append(left, fmt.Sprintf("%s %q", k.label, k.path))
				if example == "" {
					if rel, err := filepath.Rel(was, filepath.Clean(k.path)); err == nil {
						example = fmt.Sprintf(" (%q becomes %q)", k.path, filepath.Join(now, rel))
					}
				}
			}
			if len(left) == 0 {
				continue
			}
			bad = append(bad, fmt.Sprintf("%s moves %s from %s to %s, and %s still lie "+
				"where the harness found them through %s, which it no longer reads them from — "+
				"re-root them from %s to %s in the same override, each at the same place "+
				"under the new directory%s", r.field, name, was, now,
				strings.Join(left, ", "), was, was, now, example))
		}
	}
	return bad
}

// validateEventMarkers holds a [MarkerScopeEvents] profile to the shape in
// which a sentinel can only be the harness's own: jsonl, a discriminator, the
// failure events, and where in them the class is.
func (p *Profile) validateEventMarkers(add func(string, ...any)) {
	if p.EffectiveOutput() != OutputJSONL {
		// Text stdout IS the model's reply; json is one document, no events.
		add("marker_scope is %s but output is %q — a failure event exists only in a "+
			"jsonl stream; use %s for a harness that reports a failure as its answer",
			MarkerScopeEvents, p.EffectiveOutput(), MarkerScopeAnswerAndStderr)
	}
	if len(p.EventTypePath) == 0 {
		add("marker_scope is %s but event_type_path is empty — without the "+
			"discriminator no line can be told to be the harness's failure event rather than "+
			"one carrying the model's words", MarkerScopeEvents)
	}
	if len(p.MarkerEvents) == 0 {
		add("marker_scope is %s but marker_events is empty — name the event types the "+
			`harness reports a failure in, e.g. ["error"]`, MarkerScopeEvents)
	}
	if len(p.MarkerPath) == 0 {
		add("marker_scope is %s but marker_path is empty — say where in that event the "+
			`failure class is, e.g. ["error", "type"]`, MarkerScopeEvents)
	}
	for _, kind := range p.MarkerEvents {
		if slices.Contains(p.TextEvents, kind) {
			// An answer-carrying event holds the model's words.
			add("marker_events and text_events both name %q — an event that carries "+
				"the model's answer is not the harness's failure report", kind)
		}
	}
	for i, m := range p.LimitMarkers {
		if m.Reset.Separator != "" {
			add("limit_markers[%d].reset_separator is set but marker_scope is %s — the "+
				"sentinel is compared with the whole failure class, so there is no text "+
				"after it to read a reset from", i, MarkerScopeEvents)
		}
	}
}

// validate holds a reset reading to a unit it can be taken in. what names the
// declaration, for the message.
func (r ResetReading) validate(what string, add func(string, ...any)) {
	switch {
	case r.Separator == "" && r.Unit != "":
		add("%s.reset_unit is set with no reset_separator: the unit says how to read "+
			"the text after the separator, so there is nothing to read", what)
	case r.Separator != "" && !r.Unit.Valid():
		add("%s.reset_unit %q (want %s, %s or %s)", what, r.Unit,
			ResetEpoch, ResetSeconds, ResetClock)
	}
}

// validateLimitEvents holds each [LimitEvent] to the one shape in which a
// model cannot write it: a JSON event stream with a discriminator, matched on
// that discriminator and on at least one field beside it.
func (p *Profile) validateLimitEvents(add func(string, ...any)) {
	onStdout := slices.ContainsFunc(p.LimitEvents, func(e LimitEvent) bool { return e.In == EventsInStdout })
	if onStdout && (p.EffectiveOutput() != OutputJSONL || len(p.EventTypePath) == 0) {
		add("limit_events needs output: %s and an event_type_path: on any other "+
			"output a line of the model's answer is a line of stdout, and it could spell "+
			"the report out", OutputJSONL)
	}
	for i, e := range p.LimitEvents {
		what := fmt.Sprintf("limit_events[%d]", i)
		if e.In != EventsInStdout {
			p.validateRecordEvent(what, e.In, e.Match, add)
		} else if discriminated := slices.ContainsFunc(e.Match, func(m Match) bool {
			return slices.Equal(m.Path, p.EventTypePath)
		}); !discriminated || len(e.Match) < 2 {
			add("%s.match must name the event type at event_type_path and at least one "+
				"field the harness writes beside it, e.g. an error flag or a status code", what)
		}
		if e.Reset.Separator != "" && len(e.SaidPath) == 0 {
			add("%s.reset_separator is set with no said_path: the reset is read from "+
				"the sentence at said_path", what)
		}
		e.Reset.validate(what, add)
	}
}

// validateAPIKeyModelEnv holds [Profile.APIKeyModelEnv] to a key it goes beside
// and a model it can carry: without api_key_env no mode puts it in place, and
// one sharing a name with the key or the token would overwrite it.
// validateSignInArgs holds [Profile.KeyArgs] and [Profile.SubscriptionArgs]
// to a sign-in the profile has: args for a key on a harness no key signs in,
// or for a login on one that keeps none and mints no token, would be passed
// on no call, a claim nothing runs.
func (p *Profile) validateSignInArgs(add func(string, ...any)) {
	if len(p.KeyArgs) > 0 && p.APIKeyEnv == "" && !p.EnvSignIn {
		add("key_args is set, but no key signs this harness in (no api_key_env, no env_sign_in), " +
			"so no call would pass it: drop it, or name the key variable")
	}
	if len(p.SubscriptionArgs) > 0 && !p.KeepsLogin() && p.TokenEnv == "" {
		add("subscription_args is set, but the harness keeps no login (no credential_paths, " +
			"credential_store) and mints no token (no token_env), so no call would pass it: drop it, " +
			"or name the login's files")
	}
	for _, list := range []struct {
		field string
		args  []string
	}{{"key_args", p.KeyArgs}, {"subscription_args", p.SubscriptionArgs}} {
		for i, arg := range list.args {
			if strings.TrimSpace(arg) == "" {
				add("%s[%d] is empty — an empty argument is one the harness reads as a value", list.field, i)
			}
		}
	}
}

func (p *Profile) validateAPIKeyModelEnv(add func(string, ...any)) {
	name := p.APIKeyModelEnv
	switch {
	case name == "":
	case p.APIKeyEnv == "":
		add("api_key_model_env %q is set with no api_key_env: the model goes beside the "+
			"key, under auth.mode api-key", name)
	case name == p.APIKeyEnv || name == p.TokenEnv || IsCredentialName(name):
		add("api_key_model_env %q names a credential variable: it carries the model, "+
			"beside the key in api_key_env", name)
	case !p.TakesModel():
		add("api_key_model_env %q is set, but model_args takes no {model}: the harness "+
			"takes no model to put there", name)
	}
}

// validateRecordEvent holds a limit or sign-in event looked for in the
// harness's record ([EventsInRecord]) to a record there is: a file the harness
// keeps, read by [Profile.Requests]. The record names no discriminator, so the
// match carries its own: at least two fields, the event's type and its class.
func (p *Profile) validateRecordEvent(what string, in EventSource, match []Match, add func(string, ...any)) {
	if !in.Valid() {
		add("%s.in %q (want %q, or nothing for stdout)", what, in, EventsInRecord)
		return
	}
	if p.Requests == nil || p.Requests.Source != SourceFile {
		add("%s.in is %s, but requests names no file the harness keeps its record in "+
			"(requests: source: %s)", what, in, SourceFile)
	}
	if len(match) < 2 {
		add("%s.match must name the record's event type and at least one field the "+
			"harness writes beside it, e.g. an error code", what)
	}
}

// validateUsageCommand holds [Profile.SessionArgs] and [Profile.UsageArgs] to
// the one shape in which a usage command reports the call it follows.
func (p *Profile) validateUsageCommand(add func(string, ...any)) {
	if len(p.SessionArgs) > 0 && !hasPlaceholder(p.SessionArgs, "{session}") {
		add("session_args carries no {session} placeholder — there is nothing to " +
			`substitute the call's id into, e.g. ["--session", "ses_{session}"]`)
	}
	if len(p.UsageArgs) == 0 {
		return
	}
	switch {
	case !hasPlaceholder(p.UsageArgs, "{session}"):
		// Without the id it asks about SOME session of a shared home.
		add("usage_args carries no {session} placeholder — the report has to be " +
			"about this call's session, and a seat's home holds every call running in it")
	case !hasPlaceholder(p.SessionArgs, "{session}"):
		add("usage_args asks about the call's session but session_args does not name " +
			"one — declare session_args with {session}, or the harness picks an id nobody " +
			"can ask about")
	}
	if len(p.UsageFileArgs) > 0 {
		// Two reports of one call: the second read would replace the first.
		add("usage_args and usage_file_args are both set — one call has one usage " +
			"report; declare the one this harness writes")
	}
	// As for a usage file: both counts or the report is never read.
	if len(p.Usage.Input) == 0 || len(p.Usage.Output) == 0 {
		add("usage_args is set but usage.input and usage.output are not both " +
			"declared — the report is only read when both counts resolve, so every " +
			"call would fall back to an estimate while the doctor reports otherwise")
	}
}

// validate holds one system-prompt channel to the shapes that make it one.
func (ch namedChannel) validate(add func(string, ...any)) {
	f := ch.field
	if len(ch.Args) > 0 && ch.Env != "" {
		// Both would hand over the prompt twice; the vendor picks the winner.
		add("%s.args and %s.env are both set — a harness takes its system prompt on ONE "+
			"channel; drop whichever this build does not use", f, f)
	}
	// One placeholder or the other within args: the renderer writes the private
	// file for `{file}` and substitutes `{system}` into argv on the SAME pass,
	// so `["--agent-file", "{file}", "--system-prompt", "{system}"]` also put
	// every byte of the seat's identity in /proc/<pid>/cmdline. A test holds
	// shipped profiles to this; overrides are where a hand-written argv appears.
	if hasPlaceholder(ch.Args, "{file}") && hasPlaceholder(ch.Args, "{system}") {
		add("%s.args names both {file} and {system} — the first writes the seat's "+
			"system prompt to a private file and the second puts the same text on argv, "+
			"where /proc/<pid>/cmdline exposes it to every account on the machine; keep "+
			"the {file} form and drop {system}", f)
	}
	if ch.File == nil {
		return
	}
	if !ch.File.Discovered.Valid() {
		add("%s.file.discovered %q (want selected or always, or unset for a file "+
			"handed over by path)", f, ch.File.Discovered)
	}
	if !ch.WritesFile() {
		// Not harmless: on a `{system}` channel the identity goes on argv
		// bare, and the operator meant the frontmatter to reach the harness.
		add("%s.file is set but the channel writes no file — give %s.args a {file} "+
			"placeholder, set %s.env, or make the file discovered", f, f, f)
	}
	if tpl := ch.File.Template; tpl != "" && !strings.Contains(tpl, "{system}") {
		add("%s.file.template has no {system} placeholder — every call would hand "+
			"the harness the same fixed file and no seat's identity", f)
	}
	switch name := ch.File.Name; {
	case ch.File.Found():
		ch.validateDiscovered(add)
	case name != "" && !isBareFileName(name):
		// Joined onto the per-call directory, and operator-overridable.
		add("%s.file.name %q must be a plain file name, with no directory separator "+
			"— only a discovered file, which the harness finds where its vendor says to "+
			"look, carries a directory", f, name)
	}
}

// validateDiscovered holds a discovered file to its shape: a file the harness
// finds in the working directory, and an argv selecting it, or none for one it
// reads on every run.
func (ch namedChannel) validateDiscovered(add func(string, ...any)) {
	f, name := ch.field, ch.File.Name
	switch {
	case name == "":
		// The default name is one no vendor looks for: nothing would find it.
		add("%s.file is discovered but its name is empty — say where the harness looks "+
			"for it, e.g. .crewlet-config/agents/crewlet-seat.md", f)
	case !staysInside(name):
		add("%s.file.name %q must be relative and stay inside the per-call working "+
			"directory", f, name)
	case !namesAFile(name):
		// A name cleaning to a DIRECTORY fails every call: `.` and `x/..` are
		// the working directory (EISDIR), and `.opencode/agents/` writes a
		// FILE where the harness looks for a directory of agents.
		add("%s.file.name %q must name a FILE inside the per-call working directory "+
			"— not the directory itself, not a path ending in a separator, and not one "+
			"whose last element is . or .. — e.g. .crewlet-config/agents/crewlet-seat.md", f, name)
	}
	switch {
	case ch.File.Discovered == DiscoverySelected && len(ch.Args) == 0:
		// Found but never selected, the file changes nothing: the harness runs
		// its default agent without the seat's identity.
		add("%s.file is discovered: selected but %s.args is empty — the harness finds the "+
			"file, and nothing tells it to use what it found; select it, or make it "+
			"discovered: always if the harness reads it on every run", f, f)
	case hasPlaceholder(ch.Args, "{file}") || hasPlaceholder(ch.Args, "{system}"):
		// Handing over the path too is two channels for one prompt, and
		// {system} would put the text on argv beside the file.
		add("%s.file is discovered but %s.args carries {file} or {system} — a "+
			"discovered file is found by its location, and the argv only selects it", f, f)
	}
	if ch.Env != "" {
		add("%s.file is discovered but %s.env is set — the variable hands the harness a "+
			"path, and a discovered file needs none", f, f)
	}
}

// ValidateCredentials reports every place this profile routes a credential
// around auth: a passthrough_env or env entry whose name says it carries a
// secret, and an env entry naming the profile's own token_env or api_key_env.
//
// Apart from [Profile.Validate], which [Load] runs: these are ADMISSION rules,
// that one is RUNNABLE. A profile breaking one still runs, only wrong about
// where a credential comes from. Config refuses such a document on every write
// but only warns on an apply, since refusing would take a node off the fleet's
// epoch over a revision that runs, and a later build is free to change this
// rule set.
func (p *Profile) ValidateCredentials(name string) error {
	var bad []string
	add := func(format string, args ...any) {
		bad = append(bad, fmt.Sprintf(format, args...))
	}
	for _, env := range p.PassthroughEnv {
		if IsCredentialName(env) {
			// Refused rather than silently dropped.
			add("passthrough_env names %q, which looks like a credential — "+
				"passthrough is forwarded before auth.mode is consulted, so it would "+
				"reach every seat whatever the mode says; use auth.mode api-key or "+
				"inherit-env instead", env)
		}
	}
	for _, env := range slices.Sorted(maps.Keys(p.IsolationEnv)) {
		// Forwarded on every command whatever auth.mode says: no place for a secret.
		if env != "" && (env == p.TokenEnv || env == p.APIKeyEnv || IsCredentialName(env)) {
			add("isolation_env names %q, which is a credential variable — a switch the "+
				"engine sets on every command is forwarded whatever auth.mode says and is "+
				"stored unredacted; give the credential through the entry's auth or its "+
				"own env", env)
		}
	}
	for _, env := range slices.Sorted(maps.Keys(p.SubscriptionEnv)) {
		// Set under subscription alone, where auth supplies the token: a
		// credential here would sign in with a value no auth field names,
		// stored unredacted.
		if env != "" && (env == p.TokenEnv || env == p.APIKeyEnv || IsCredentialName(env)) {
			add("subscription_env names %q, which is a credential variable — it selects "+
				"the login a subscription call signs in with and carries no secret; give "+
				"a credential through the entry's auth", env)
		}
	}
	if p.MCP != nil {
		for _, env := range slices.Sorted(maps.Keys(p.MCP.Env)) {
			if p.MCP.mintedByServer(p.MCP.Env[env]) {
				// A value the harness's own server mints (the password
				// OpenCode's `serve` prints), rendered per run and never
				// stored: the profile holds a placeholder, no secret.
				continue
			}
			// Set on every run with tools whatever auth.mode says, stored
			// unredacted like the rest of overrides.
			if env != "" && (env == p.TokenEnv || env == p.APIKeyEnv || IsCredentialName(env)) {
				add("mcp.env names %q, which is a credential variable — the tool route's "+
					"environment is set whatever auth.mode says and is stored unredacted; give "+
					"the credential through the entry's auth or its own env", env)
			}
		}
	}
	for _, env := range slices.Sorted(maps.Keys(p.Env)) {
		switch {
		case env != "" && (env == p.TokenEnv || env == p.APIKeyEnv):
			// Apart from the shape rule below: an overridden token_env or
			// api_key_env need not LOOK like a credential. The entry's env and
			// auth are layered over this, so whether a value reaches the
			// harness turns on the mode and on stores no document can see.
			add("env names %q, which is this profile's token_env or api_key_env — "+
				"auth owns that variable, and whether a value here reaches the harness "+
				"depends on auth.mode and on whether the secret store or the engine's "+
				"environment holds one; give the credential through auth "+
				"(auth.token, or auth.api_key with auth.mode api-key)", env)
		case IsCredentialName(env):
			// As passthrough_env, and overrides is neither ${VAR}-resolved nor
			// marked secret: a key here sits in the stored revision in plain
			// text and is shown unredacted on every read.
			add("env names %q, which looks like a credential — the profile's env "+
				"is forwarded whatever auth.mode says, and overrides is stored "+
				"and shown unredacted; put it in the entry's own env, which is "+
				"${VAR}-resolved and redacted, or give it through its auth", env)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("harness profile %q: %s", name, strings.Join(bad, "; "))
}

// EffectivePromptMode is the prompt mode with its default applied.
func (p *Profile) EffectivePromptMode() PromptMode {
	if p.PromptMode == "" {
		return PromptStdin
	}
	return p.PromptMode
}

// EffectiveOutput is the output mode with its default applied.
func (p *Profile) EffectiveOutput() OutputMode {
	if p.Output == "" {
		return OutputJSON
	}
	return p.Output
}

// sentinelProblem reports why a marker sentinel cannot be matched safely, or
// "" when it can.
//
// A sentinel is matched as a plain substring against stderr, a failed run's
// output and the opening of a healthy (model-written) answer ([MarkerScope]).
// One that can occur in ordinary text misclassifies replies: KindRateLimit and
// KindAuth hand the seat to the fallback chain, and a rate limit SITS THE ENTRY
// OUT (harness bench.go); `sentinel: "429"` benched a working subscription when
// a model quoted a status. The rule is "contains a letter", no minimum length:
// a length floor nobody can defend would refuse a legitimate short vendor
// string. See [LimitMarker].
func sentinelProblem(sentinel string) string {
	if sentinel == "" {
		return "is empty"
	}
	if !strings.ContainsFunc(sentinel, unicode.IsLetter) {
		return fmt.Sprintf("%q has no letters — a sentinel is the vendor's own wording, "+
			"matched as a substring against whatever the harness printed, so one made only of "+
			"digits or punctuation matches ordinary replies and benches a working "+
			"credential. Use the sentence the harness actually prints", sentinel)
	}
	return ""
}

// ReadsUsage reports whether this profile can take token counts from the harness
// rather than estimating them.
//
// Both halves, since either alone is a lie: no usage paths cannot, and a TEXT
// profile cannot since [extract] never decodes a document in that mode
// (`output: text` is a one-line override leaving inherited paths inert). A
// usage report is the exception: decoded whatever stdout carries, so a `text`
// profile declaring [Profile.UsageFileArgs] or [Profile.UsageArgs] reports real
// counts. `crewlet llm doctor` and the extractor share this answer.
func (p *Profile) ReadsUsage() bool {
	if len(p.Usage.Input) == 0 && len(p.Usage.Output) == 0 {
		return false
	}
	if p.ReadsUsageReport() {
		return true
	}
	return p.EffectiveOutput() != OutputText
}

// LoginLocation says where this harness keeps a login, for a message telling an
// operator to put one there: credential files, a [CredentialStore] database, or both.
func (p *Profile) LoginLocation() string {
	var where []words.Said
	if len(p.CredentialPaths) > 0 {
		paths := make([]words.Said, len(p.CredentialPaths))
		for i, path := range p.CredentialPaths {
			paths[i] = words.Said(path)
		}
		where = append(where, words.Said("its credential files ("+words.And(paths)+")"))
	}
	if p.CredentialStore != nil {
		where = append(where, "the login it keeps in its own database")
	}
	if len(where) == 0 {
		return "a login this profile names no place for"
	}
	return words.And(where)
}

// KeepsLogin reports whether the harness keeps a login the engine can hold:
// credential files or its own database. Otherwise it signs in only through a
// call's environment: token, key, or a provider's key ([Profile.EnvSignIn]).
func (p *Profile) KeepsLogin() bool {
	return len(p.CredentialPaths) > 0 || p.CredentialStore != nil
}

// ReadsUsageReport reports whether counts come from a report apart from stdout
// (a usage file or a usage command run after the call).
func (p *Profile) ReadsUsageReport() bool {
	return len(p.UsageFileArgs) > 0 || len(p.UsageArgs) > 0
}

// UnderRoot joins a profile-declared relative path onto a root and refuses
// one that escapes it.
//
// Profile paths are operator-overridable, and the backend's prune calls
// RemoveAll on what this returns, so "../../.ssh" must not pass.
func UnderRoot(root, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("harness: path %q must be relative to the seat home", rel)
	}
	joined := filepath.Join(root, rel)
	cleanRoot := filepath.Clean(root)
	if joined != cleanRoot && !strings.HasPrefix(joined, cleanRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("harness: path %q escapes the seat home", rel)
	}
	return joined, nil
}
