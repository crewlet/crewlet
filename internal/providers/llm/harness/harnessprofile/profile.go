package harnessprofile

import (
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/crewlet/crewlet/internal/words"
)

// PromptMode is how a CLI receives the prompt.
type PromptMode string

const (
	// PromptStdin writes the prompt to the child's stdin. The default, and
	// one of the two modes with no length ceiling.
	PromptStdin PromptMode = "stdin"
	// PromptArgv appends the prompt as the last argument. Bounded by
	// ARG_MAX — around 2 MB on Linux, 256 KB on macOS — so a long
	// transcript on such a CLI fails at exec rather than at the model.
	PromptArgv PromptMode = "argv"
	// PromptFile writes the prompt to a private file in the per-call
	// working directory and passes the PATH, through the `{file}`
	// placeholder in [Profile.PromptArgs].
	//
	// The same trade this package already makes for the system prompt, for
	// the same two reasons: argv is bounded by ARG_MAX, and
	// /proc/<pid>/cmdline makes it readable by every account on the
	// machine. A rendered prompt is the whole flattened transcript — the
	// seat's identity, the conversation and its tool results — so it is
	// the LARGEST and the most sensitive thing this
	// backend hands a CLI. Prefer this over argv wherever a vendor offers
	// a prompt-file flag; `muse exec --prompt-file` is the first that does.
	PromptFile PromptMode = "file"
)

// Valid reports whether m is a mode this package knows.
func (m PromptMode) Valid() bool {
	return m == PromptStdin || m == PromptArgv || m == PromptFile
}

// OutputMode is how a CLI's answer is encoded on stdout.
type OutputMode string

const (
	// OutputJSON is one JSON object.
	OutputJSON OutputMode = "json"
	// OutputJSONL is a stream of JSON objects, one per line, whose
	// interesting fields are spread across several of them. Codex streams
	// its events this way, so every line is scanned and the LAST value
	// found at a path wins.
	OutputJSONL OutputMode = "jsonl"
	// OutputText is the CLI's own prose on stdout, taken verbatim. No
	// usage figures exist in this mode, so tokens are estimated.
	OutputText OutputMode = "text"
)

// Valid reports whether m is a mode this package knows.
func (m OutputMode) Valid() bool {
	return m == OutputJSON || m == OutputJSONL || m == OutputText
}

// Path is a route to a value inside a decoded JSON document: successive
// object keys, or a decimal index into an array.
//
// A USAGE path may also take [PathEvery] as a step, for every member of the
// object or array there — and the count it reads is then the SUM of the count
// under each member that has one. It exists because a CLI can key its counts
// by something no profile can know in advance: Gemini CLI reports
// `stats.models.<model>.tokens`, one entry for every model a call used, and its
// web tools ask a second model on the side. Only a usage path may carry it
// ([Profile.Validate]): a count adds up, while an answer, a failure flag or an
// event's kind is one value, and which member's would be a guess.
type Path []string

// PathEvery is the [Path] step that stands for every member of the object or
// array at that point. A key spelled `*` in a CLI's own document is therefore
// out of a path's reach, which no CLI measured has.
const PathEvery = "*"

// HasEvery reports whether the path takes [PathEvery] anywhere.
func (p Path) HasEvery() bool { return slices.Contains(p, PathEvery) }

// String renders a path the way profiles.yaml would be read aloud —
// `usage.input_tokens` for [][]string{{"usage", "input_tokens"}}.
//
// It exists for the message an operator reads when a profile stops matching
// its CLI: naming the field to change is the whole point of that message, and
// `[]harness.Path{{"result"}}` printed with %v names nothing.
func (p Path) String() string { return strings.Join(p, ".") }

// PathList renders a set of paths for the same message, in declaration order.
//
// Declaration order because that is the order they are TRIED, so an operator
// comparing this against their CLI's real output reads the two in step.
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

// UsagePaths locates the four token counts in a CLI's own usage report.
//
// Each is a LIST of paths rather than one, because a vendor moves these
// between releases and an operator overriding a single field should not have
// to know which release the engine was written against. The first path that
// resolves to a number wins; when none does, the count is estimated (four
// characters a token), and `crewlet llm doctor` reports the profile's token
// usage as estimated, because a budget built on estimates is a different
// promise.
//
// Reasoning is the one figure that is ADDED rather than reported as itself:
// every provider bills the tokens a model spends reasoning as output, and a CLI
// that reports them APART from its output count — OpenCode 2.0's session totals
// carry `output` as the visible text alone, beside `reasoning` — would otherwise
// have every thinking model charged for its answer and not its thought. Declared
// only where the vendor's output count excludes them: OpenAI's own usage, and
// every CLI that passes it through, counts reasoning INSIDE output already, and
// adding it there would charge it twice.
//
// ToolPrompt is the same on the input side: the prompt a MODEL-SIDE tool put in
// front of the model — the pages a URL-context fetch retrieved, the results a
// search grounding returned — which Gemini reports as toolUsePromptTokenCount,
// apart from the prompt count, and bills as input. A web fetch puts a whole
// page there, so leaving it out would charge a fetching seat for its question
// and not for what it read.
type UsagePaths struct {
	Input      []Path `yaml:"input,omitempty"`
	Output     []Path `yaml:"output,omitempty"`
	CacheRead  []Path `yaml:"cache_read,omitempty"`
	CacheWrite []Path `yaml:"cache_write,omitempty"`
	Reasoning  []Path `yaml:"reasoning,omitempty"`
	ToolPrompt []Path `yaml:"tool_prompt,omitempty"`

	// InputIncludesCache declares that the input count is ALREADY the whole
	// prompt, the cached tokens inside it, so the cache figures only break it
	// down. False, the default, reads it as the uncached part alone, which is
	// how Anthropic's usage — and every CLI passing that through — reports
	// it, and adds the cache figures to it ([UsagePaths.PromptTokens]).
	//
	// A DECLARATION OF THE VENDOR'S ARITHMETIC, because the two shapes name
	// their fields alike and differ only in what the first one counts:
	// OpenAI's `input_tokens` holds its `cached_tokens`, and codex's
	// turn.completed passes that through (its own `non_cached_input` is the
	// difference). Read the other way, every cached token was charged twice.
	InputIncludesCache bool `yaml:"input_includes_cache,omitempty"`
}

// PromptTokens is the whole prompt's token count out of the three input
// figures a CLI reported, by the arithmetic [UsagePaths.InputIncludesCache]
// declares: the budget figure a completion's InputTokens carries, which counts
// every cached token once.
func (u UsagePaths) PromptTokens(input, cacheRead, cacheWrite int) int {
	if u.InputIncludesCache {
		return input
	}
	return input + cacheRead + cacheWrite
}

// CredentialStore is a login a CLI keeps in a DATABASE OF ITS OWN rather than
// in files — and so a login that cannot travel the way [Profile.CredentialPaths]
// do. It travels as the DOCUMENT the CLI's own export command prints, and
// reaches a home through the CLI's own import command.
//
// THE FILE CANNOT BE THE CREDENTIAL, because it is also the transcript. OpenCode
// 2.0 keeps its logins in the `credential` table of the same SQLite file it
// keeps every session in (`opencode.db`), and reads them nowhere else: a v1
// `auth.json` copied into a fresh home is ignored outright, and imported only
// once, while a v1 database is being upgraded. Copying the database would carry
// one seat's conversations into every other seat, syncing it back would carry
// them into the shared directory, and pruning it after a call — which is what
// keeps a turn from reaching the next — would delete the login.
//
// So the document is the login of record: the shared credential directory keeps
// it under [CredentialStoreDocument], a seat's home starts every generation from
// an empty database with the document imported into it, and the generation ends
// by exporting what the CLI then holds — a token it refreshed mid-call included
// — and carrying it back only where it differs from the document the home was
// seeded with, and only over that same document. A compare-and-swap, under the
// shared login's lock (hostbox.LockCredentials), and never "replace when it
// changed": where the shared document has moved on since the home's seed —
// another seat's refresh, an operator's new login, a logout — the shared
// directory keeps what it now holds and the home's export is dropped, and a
// home that exports what it was seeded with writes nothing at all, whatever
// the shared document holds (the backend's Workspace.syncCredentialsOut).
//
// The database itself is then volatile like any other session store. Measured
// against OpenCode 2.0.32: an import followed by an export reproduces the
// document byte for byte, and an import skips a credential id the database
// already holds rather than updating it, which is why a refresh comes back as a
// new document and never as an import into an older database.
type CredentialStore struct {
	// ExportArgs print every login the CLI holds as one document on stdout.
	ExportArgs []string `yaml:"export_args,omitempty"`

	// ImportArgs read such a document on STDIN and store every login in it.
	// Never on argv and never a path the engine invents: the document is a
	// credential, and stdin is the one channel no other account on the
	// machine can read.
	ImportArgs []string `yaml:"import_args,omitempty"`
}

// CredentialStoreDocument is the name the shared credential directory, a
// credential bundle and a coding box keep a [CredentialStore]'s document under.
//
// ONE NAME FOR EVERY PROFILE, and the engine's rather than the vendor's: no CLI
// reads this file — it reads the document on stdin — so where it is kept is a
// fact about this engine, and a profile declaring one would be a second place
// to keep the same answer. No extension, because the format is the CLI's own.
const CredentialStoreDocument = "exported-credentials"

// StdinLogin is a credential login a CLI genuinely accepts.
//
// A pointer field on Profile, and unset on every vendor whose login is
// browser OAuth: the distinction between "this CLI has no password login" and
// "this CLI has one that takes no arguments" is the difference between an
// error a person can act on and a command that hangs on a prompt.
type StdinLogin struct {
	// Args is the login argv, with {username} substituted.
	Args []string `yaml:"args,omitempty"`
	// StdinTemplate is written to the child's stdin, with {password} and
	// {username} substituted. Never argv: a password there is visible in
	// ps output and lands in the operator's shell history.
	StdinTemplate string `yaml:"stdin_template,omitempty"`
	// PasswordEnv is an alternative to stdin for a CLI that reads its
	// credential from the environment instead.
	PasswordEnv string `yaml:"password_env,omitempty"`
}

// LimitMarker recognises a spent subscription in a reply the CLI reported as
// a SUCCESS.
//
// A spent plan is not an HTTP status here: the process exits 0 and the answer
// is prose. Matching that prose by keyword gets it wrong twice over: "usage
// limit" appears in a model's own answer about rate limits, and a vendor's
// wording changes under it. So a marker matches by
// STRUCTURE: a literal sentinel the vendor emits verbatim, plus the field
// that carries the reset instant — and, in what a run that did not fail
// says, only where the sentinel opens it ([MarkerScope]). A marker that finds
// its sentinel but no reset value still classifies; one that finds neither
// does not fire.
type LimitMarker struct {
	// Sentinel is the exact substring the vendor emits. It is compared
	// case-sensitively against the extracted text: the vendors write these
	// as fixed strings, and folding case is what turns a sentinel back
	// into a keyword. In the [MarkerScopeEvents] scope it is the failure
	// CLASS instead, compared whole and case-sensitively with the string at
	// [Profile.MarkerPath] in the CLI's own failure event.
	Sentinel string `yaml:"sentinel,omitempty"`

	// Reset reads the reset instant out of the line the sentinel matched
	// on, where the vendor names one.
	Reset ResetReading `yaml:",inline"`
}

// ResetReading says where a vendor's limit report names the instant the limit
// lifts, and how to read it: the text after Separator, as Unit.
//
// A READING THAT FINDS NOTHING IS NOT AN ERROR. The report still classifies as
// a limit, and the entry sits out a bounded, growing window instead
// (internal/providers/llm/harness), which is the honest answer when the vendor
// did not say: inventing an instant would hold a seat off a plan that has
// already reset, or send it back into one that has not.
type ResetReading struct {
	// Separator splits the reset value off the report: "|" before an epoch,
	// "resets " before a time of day.
	Separator string `yaml:"reset_separator,omitempty"`

	// Unit is how to read the value after the separator; see [ResetUnit].
	Unit ResetUnit `yaml:"reset_unit,omitempty"`
}

// ResetUnit is how a [ResetReading] reads the value after its separator.
type ResetUnit string

const (
	// ResetEpoch is Unix seconds.
	ResetEpoch ResetUnit = "epoch"
	// ResetSeconds is a delay in seconds from the report.
	ResetSeconds ResetUnit = "seconds"
	// ResetClock is a time of day followed by the IANA zone it is in, the
	// way Claude Code 2.1 writes it: "4:30pm (America/Vancouver)",
	// "4pm (Europe/Berlin)" or "16:30 (Asia/Tokyo)". The instant is the
	// occurrence of that time nearest the report, so a reset named past
	// midnight is tomorrow's.
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

// LimitEvent recognises a spent plan or a rate limit by the CLI's own
// STRUCTURED report: a stdout event whose fields the CLI writes, such as
// Claude Code's result event with `is_error: true` and `api_error_status: 429`.
//
// IT IS READ BEFORE ANY SENTINEL, because a status code is the one signal a
// vendor's rewording leaves alone. Claude Code 2.1 replaced "Usage limit
// reached" with "You've hit your 5-hour limit · resets 4:30pm" while still
// writing 429 beside it, and a profile that knew only the sentence classified
// every refusal as a server failure: nothing sat the entry out, and every round
// of every seat launched a CLI to hear the same refusal.
//
// SAFE TO BENCH ON for the reason [MarkerScopeEvents] is: a model chooses the
// words inside an event's strings, never the keys or the types of the event the
// CLI writes them in. That holds only where every stdout line is a JSON event,
// which is why a profile may declare these only with jsonl output and a
// discriminator, and why every match names the discriminator and at least one
// other field.
type LimitEvent struct {
	// Match picks the report out; see [Match].
	Match []Match `yaml:"match,omitempty"`

	// SaidPath is where in the event the vendor's own sentence is: what the
	// operator is shown, and what Reset reads the instant from. Empty shows
	// the whole line and reads no instant.
	SaidPath Path `yaml:"said_path,omitempty"`

	// Reset reads the reset instant out of the sentence at SaidPath.
	Reset ResetReading `yaml:",inline"`
}

// MarkerScope says WHERE a CLI's failure prose can appear, and therefore
// where [LimitMarker] and [AuthMarker] sentinels may be matched.
//
// The default searches the model's ANSWER as well as stderr, and it has to:
// Claude Code reports a spent plan on a ZERO exit with the vendor's sentence
// about the plan standing where the answer should be, so a marker that
// searched stderr alone would never fire and the entry would never sit out.
//
// THE ANSWER IS SEARCHED ON TERMS, because on a healthy run it is the model's
// own words — its prose and the arguments of every tool call it makes. On a run
// the CLI did not report failed, a sentinel counts only where it BEGINS the
// answer's first non-empty line, so that the answer IS the vendor's report; it
// counts anywhere only in a run the CLI reported failed, by a non-zero exit or
// [Profile.ErrorPaths]. (Stderr is matched anywhere: the answer does not reach
// it.) Matched anywhere on every run, a seat ASKED about rate limits — "what
// is our quota?" — answered in prose that tripped a generic sentinel, the
// answer was thrown away as a spent plan, and the entry sat out for every seat
// on the node. The anchoring is what makes the shipped GENERIC sentinels safe
// to keep — "rate limit", "quota", "usage limit" are not measured vendor
// sentences, and a vendor's report opening with one is the only place they
// count on a clean run. Nothing about it saves a sentinel that is not words:
// this file lost a "429" to the same false positive, and an answer can open
// with digits as readily as mention them.
//
// A CLI that reports its failures on STDERR and nowhere else narrows it
// further, and its markers stop being matched against anything the model said,
// in a failed run or not. That is not
// a guess per profile: kimi-code writes `provider.rate_limit: …` to stderr
// with the answer stream empty, and pi writes `<status>: <the provider's JSON>`
// to stderr with stdout empty — both measured. A profile only narrows this when
// the vendor's own behaviour makes the answer an impossible place for the
// report, and hermes is the warning: only its failures to START reach stderr,
// while a refused key or a spent plan is printed on stdout with exit 2
// (measured on 0.21.5), so the stderr scope it once had matched none of them.
//
// THE THIRD SCOPE MATCHES STRUCTURE, NOT TEXT, for a CLI that reports a failure
// as an EVENT OF ITS OWN in a JSON stream. OpenCode 2's `--format json` prints
// `{"type":"error",…,"error":{"type":"provider.quota",…}}` on stdout and nothing
// on stderr, so the report is neither the answer nor stderr. And it can follow
// an answer — a call that wrote a sentence, fetched a page and was then refused
// on its next step has a located answer that does not contain the refusal — so
// searching the answer finds nothing exactly when a spent plan most needs the
// chain to carry the seat on.
//
// A substring search of stdout cannot stand in for it, and the first shape of
// this scope was exactly that, defended on the grounds that the model's words
// reach a JSON stream only as escaped strings. They do not: OpenCode prints
// every tool call as a `tool_use` event whose `state.input` is the model's
// arguments as a raw JSON OBJECT, so a model that put `"type":"provider.quota"`
// into a webfetch call — which text injected into a page or a message it read
// can ask it to — printed the sentinel verbatim on a call that exited 0 with a
// correct answer, and the credential was benched (measured on 2.0.32). So the
// scope decodes each stdout line, looks only at the events the profile names
// as the CLI's own failure reports ([Profile.MarkerEvents], told apart by
// [Profile.EventTypePath]), and compares every sentinel by EQUALITY with the
// string at [Profile.MarkerPath] in such an event. Whatever bytes a model puts
// in its text or a tool's arguments, they arrive inside an event of another
// type, or nested where the path does not reach.
//
// STDERR IS NOT SEARCHED in this scope. The one CLI that takes it writes none
// of these failures there — zero bytes, measured on 2.0.32 with
// `--print-logs=false` for a refused key, a spent plan and a rate limit — and a
// byte search of a channel with nothing to find is only a way for something
// other than the CLI's own event to classify a call. [Profile.Validate] refuses
// the scope without a jsonl stream, its discriminator, the failure events and
// the path.
type MarkerScope string

const (
	// MarkerScopeAnswerAndStderr searches the model's answer and stderr.
	// The default, and what a CLI that reports a spent plan AS its answer
	// requires.
	MarkerScopeAnswerAndStderr MarkerScope = "answer-and-stderr"
	// MarkerScopeStderr searches stderr only, for a CLI whose failures
	// never reach the answer.
	MarkerScopeStderr MarkerScope = "stderr"
	// MarkerScopeEvents compares sentinels with the failure class in the
	// CLI's own failure events and looks nowhere else — for a CLI that
	// reports a failure as an event of its own in a JSON stream.
	MarkerScopeEvents MarkerScope = "events"
)

// Valid reports whether s is a scope this package knows.
func (s MarkerScope) Valid() bool {
	return s == MarkerScopeAnswerAndStderr || s == MarkerScopeStderr || s == MarkerScopeEvents
}

// LocalTools says what a profile does about the CLI's OWN tools — its shell,
// its file editor, its browser.
//
// On this host the engine uses none of them — every phase but the executor: a
// seat's tools reach the CLI as the engine's own MCP server ([MCP]), and a CLI
// editing files in its own sandbox is invisible to the tool registry, the
// permission model and the event stream. A seat's executor is the other case:
// the same CLI's own agentic run in a box, fully permissioned there by its
// in-box runner (internal/sandbox/executor), which this stance does not
// reach. The
// shell is the one that matters: hostbox isolates a seat's HOME and
// environment, not the filesystem, so a CLI with a shell on the engine host
// reads whatever the engine user can read. A profile therefore DENIES local
// tools wherever the vendor offers a way to, and says so here, so that
// `crewlet llm doctor` can print the claim next to what its probe measured.
//
// Web is the deliberate exception and is not one of these: a seat on a
// subscription must not have less reach than the same CLI at a terminal, and
// a fetch is a read: it never gates delivery. An entry turns web off on its
// own stance, [WebTools], never through this one.
type LocalTools string

const (
	// LocalToolsDenied means the profile's flags or seeded files turn the
	// CLI's shell and file tools off. The doctor's shell probe is expected
	// to be refused.
	LocalToolsDenied LocalTools = "denied"
	// LocalToolsVendorDefault means the vendor offers no way to deny
	// them, so the CLI runs with whatever its non-interactive default
	// admits. The profile's local_tools_note says why, and the doctor
	// reports the probe's measurement as a finding rather than a pass.
	LocalToolsVendorDefault LocalTools = "vendor-default"
)

// Valid reports whether t is a stance this package knows.
func (t LocalTools) Valid() bool {
	return t == LocalToolsDenied || t == LocalToolsVendorDefault
}

// WebTools says whether a profile's run keeps the CLI's OWN web tools (its
// fetch and its search) or turns them off.
//
// A SECOND STANCE BESIDE [LocalTools] rather than a value of it, because the
// two answer different questions for different readers. Local tools are
// denied wherever a vendor allows it, for every entry, since a shell on the
// engine host reads whatever the engine user can. Web stays on for a seat by
// default (see [LocalTools]) and is turned off PER ENTRY, by
// `overrides: {web_tools: denied}`, for a caller whose whole security model
// is that every byte it reads arrives through the engine's own guard: the
// Dashboard Assistant (ADR-0050) refuses any member of `assistant.llm` whose
// merged profile is not denied here, because a page the model fetched for
// itself is text no authority decision ever saw, and a fetch is also a way
// OUT for whatever the person's data put in the prompt.
//
// The empty value reads as [WebToolsAllowed], so every profile and every
// entry that says nothing runs exactly as it did before the field existed.
type WebTools string

const (
	// WebToolsAllowed keeps the CLI's web tools on, the seats' default.
	WebToolsAllowed WebTools = "allowed"
	// WebToolsDenied runs the CLI with its web tools off, through the
	// profile's [WebDenial]. A profile with no web_denial cannot be denied,
	// and Load refuses the stance on it rather than run a CLI that still
	// fetches under a configuration that says it does not.
	WebToolsDenied WebTools = "denied"
)

// Valid reports whether w is a stance this package knows. The empty value is
// not one: it is the absence of a stance, which [Profile.EffectiveWebTools]
// reads as allowed.
func (w WebTools) Valid() bool { return w == WebToolsAllowed || w == WebToolsDenied }

// WebDenial is how one CLI turns its web tools off: what its run uses INSTEAD
// when the entry is `web_tools: denied`.
//
// REPLACEMENTS, NOT EDITS. Each field, when set, replaces the profile field
// of the same name whole, for the reason [applyOverrides] replaces lists
// whole: position matters in an argv, and the denied command line is one a
// person wrote and measured as a unit (an allow list removed and a deny list
// extended are two edits that only mean something together). A field left
// unset leaves its profile field as it is, which is how a CLI that takes its
// tool policy from a file (OpenCode) denies web without restating its argv.
// A denial that sets neither field changes nothing and is refused.
//
// A RUN WITH TOOLS IS COVERED TOO. A profile whose mcp block runs its own argv
// or writes its own settings (mcp.complete_args, mcp.seed) hands a run with
// tools those in place of the profile's, so a denial that replaced only the
// profile's fields would leave the web on exactly where a seat or the
// assistant works. Such a profile's denial says what that run uses instead
// ([WebDenial.MCP]) for each field whose counterpart it replaces, and one that
// does not is refused.
//
// Every built-in denial was measured against the installed CLI with the
// doctor's own web probe; the profile's web_tools_note says on which release.
type WebDenial struct {
	// CompleteArgs replaces complete_args when web is denied.
	CompleteArgs []string `yaml:"complete_args,omitempty"`
	// SeedFiles replaces seed_files when web is denied.
	SeedFiles []SeedFile `yaml:"seed_files,omitempty"`
	// MCP replaces the mcp block's own argv and seeds when web is denied.
	MCP *WebDenialMCP `yaml:"mcp,omitempty"`
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
		(d.MCP != nil && (d.MCP.CompleteArgs != nil || d.MCP.Seed != nil)))
}

// mcpUncovered names the mcp block's own fields a denial leaves in place
// although it replaces their counterpart for a text function, which a run with
// tools would then use with its web on; empty when it covers them all.
//
// THE COUNTERPART DECIDES: a denial that turns the web off in the argv must
// turn it off in a run's own argv too, and one that does it in a settings file
// must do it in the run's own settings. A field the denial does not touch
// carries no web stance it could leave behind.
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
	// SeedHome writes the file under the seat's HOME, once per seat
	// generation, beside the credentials. Settings a CLI reads from its
	// user directory go here.
	SeedHome SeedScope = "home"
	// SeedWork writes the file into the per-call working directory, on
	// every call. Settings a CLI reads from the project it is run in go
	// here — the scratch directory is that project.
	SeedWork SeedScope = "work"
)

// Valid reports whether s is a scope this package knows.
func (s SeedScope) Valid() bool { return s == SeedHome || s == SeedWork }

// SeedFile is a settings file the engine writes for the CLI before a call.
//
// Some vendors take their tool policy from a file rather than a flag —
// Gemini CLI's policy files, OpenCode's opencode.json, Cursor's cli.json — and
// the seat HOME and the per-call working directory are both places the engine
// controls, so the file is simply put where the CLI will look. Content is
// written verbatim at 0600.
type SeedFile struct {
	// Path is relative to the scope's root. Never absolute, never
	// escaping it.
	Path string `yaml:"path,omitempty"`
	// In is the scope: home (default) or work.
	In SeedScope `yaml:"in,omitempty"`
	// Content is the file's bytes.
	Content string `yaml:"content,omitempty"`
}

// staysInside reports, at load time, whether a profile-declared relative path
// stays inside whatever directory it is later joined onto: relative, not
// empty, and climbing out of nothing once cleaned ([filepath.IsLocal]).
//
// The load-time half of [UnderRoot], for a path whose real root does not exist
// yet. It used to be UnderRoot against a stand-in root named `/seat`, which
// proved less than it claimed: `../seat/settings.json` climbs out and back in
// by that very name, so it passed at load and was refused by the real
// UnderRoot on every call.
func staysInside(rel string) bool { return filepath.IsLocal(rel) }

// Scope is the seed scope with its default applied.
func (f SeedFile) Scope() SeedScope {
	if f.In == "" {
		return SeedHome
	}
	return f.In
}

// Channel is one way a CLI takes a system prompt: as an argument, as a file
// named by an environment variable, or as a file it finds by its location.
//
// Three shapes, because vendors ship all three, and `--help` shows only the
// first: Gemini CLI and its Qwen fork take their prompt from a file a VARIABLE
// names and have no flag for it at all, which is how both were first recorded
// here as having no channel; OpenCode takes no path at all and DISCOVERS the
// file where its vendor says to look.
type Channel struct {
	// Args is the argv the channel adds, with one of two placeholders, and
	// the difference is not cosmetic:
	//
	//   {file}    the text is written to a private file in the per-call
	//             working directory and the PATH is substituted. Prefer
	//             this always. A seat's system prompt carries the org
	//             chart, its policies, its backstory, its roster and its
	//             personal-memory and knowledge prefetches.
	//   {system}  the text is substituted INTO ARGV, where /proc/<pid>/cmdline
	//             makes it readable by every account on the machine and
	//             ARG_MAX bounds it (256 KB on macOS). Only for a CLI that
	//             offers no file variant.
	//
	// With a discovered [Channel.File] it carries neither: the argv only
	// selects what the CLI found, or is empty where the CLI reads the file
	// on every run.
	Args []string `yaml:"args,omitempty"`

	// Env is an environment variable naming a file the CLI reads its
	// system prompt from — `GEMINI_SYSTEM_MD`, `QWEN_SYSTEM_MD`. Preferred
	// over Args wherever both exist, because it is a file and the text
	// never reaches argv. A channel declares one or the other: both would
	// hand the CLI the same text twice, and which copy wins is the vendor's
	// business.
	Env string `yaml:"env,omitempty"`

	// File shapes the file a `{file}`, Env or discovered channel writes —
	// see [SystemPromptFile]. Nil writes the prompt alone, under a name no
	// CLI reads on its own.
	File *SystemPromptFile `yaml:"file,omitempty"`
}

// Declared reports whether the profile offers this channel at all.
func (c Channel) Declared() bool {
	return len(c.Args) > 0 || c.Env != "" || c.File.ReadAlways()
}

// WritesFile reports whether the channel puts the text in a FILE — `{file}`
// on argv, the variable, which names a path by construction, or a file the CLI
// discovers by its location.
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

// Route is where one call's system prompt travels, and whether the CLI's own
// system prompt stays in force beside it — see [Profile.RouteFor].
type Route struct {
	// Channel carries the text; the zero channel leaves it in the transcript.
	Channel Channel

	// Field names the profile field the channel came from, `system_prompt`
	// or `append_system_prompt`, and is empty for the transcript. For a
	// message or a diagnosis to name what an operator would override.
	Field string

	// KeepsDefault says the CLI's own system prompt is in force for this
	// call: the append channel, or no channel at all, where the engine's
	// text rides the transcript under the vendor's prompt.
	KeepsDefault bool
}

// RouteFor picks the channel for one call: the append channel for a seat at
// work (agent) and the replace channel for a text function, each falling back
// to the other, and both to the transcript — see [Profile.SystemPrompt].
//
// A seat on a CLI that can only replace takes the replacement rather than the
// transcript: the vendor prompt it would keep is written for a coding agent at
// a terminal, and the seat's own instructions would arrive as a user message
// under it.
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

// SystemPromptFile shapes the file a channel writes, for the vendors whose
// channel takes a STRUCTURED file, or finds one by its location, rather than
// being handed a bare one.
//
// Without it the file is the system prompt and nothing else, which is what
// every other `{file}` profile wants. Kimi Code is the first that cannot take
// that: its only per-call prompt channel is `--agent-file`, an agent
// definition whose YAML frontmatter declares the agent's name, its
// description — REQUIRED — and its tool allowlist, with the BODY as the
// system prompt. Handed a bare prompt the CLI does not degrade: it "reports
// the error and exits", so every call fails.
//
// Which makes the frontmatter load-bearing twice over. It is also the only
// place that CLI can be told which of its own tools to keep, so the file is
// where this backend's tool denial lives for it — and that is why a channel
// declaring one is passed on EVERY call, including a request that carries no
// system prompt at all. Passing it only when there is text to put in it would
// hand the vendor's default agent, and every tool it has, to exactly the calls
// that would never notice.
//
// OpenCode 2 is the second, and it takes the file a different way: it is
// handed no path at all. It DISCOVERS the file by its location in its
// configuration directory, which its profile points inside the per-call
// working directory ([Profile.WorkConfigEnv]) — see [SystemPromptFile.Discovered].
type SystemPromptFile struct {
	// Name is the file to write in the per-call working directory. Empty
	// takes the default. A bare name for a file whose path the CLI is
	// handed: a vendor that keys on the extension (`--agent-file` wants
	// `.md`) is the only reason to set it. A relative path naming a FILE,
	// staying inside the working directory, for a discovered one, which the
	// CLI finds where its vendor says to look.
	Name string `yaml:"name,omitempty"`

	// Template is the file's content, with `{system}` substituted by the
	// seat's system prompt. Empty writes the prompt alone.
	Template string `yaml:"template,omitempty"`

	// Discovered says the CLI FINDS the file by its location in the working
	// directory rather than being handed its path — see [Discovery]. Empty
	// is a file handed over by path.
	Discovered Discovery `yaml:"discovered,omitempty"`
}

// Discovery is how a CLI comes to read a file it finds by its location.
type Discovery string

const (
	// DiscoverySelected is a file the CLI finds and the channel's argv then
	// selects by name: OpenCode's `agents/<name>.md`, run with
	// `--agent <name>`. Found but never selected, it changes nothing.
	DiscoverySelected Discovery = "selected"

	// DiscoveryAlways is a file the CLI reads on every run it makes in that
	// directory, with nothing on argv asking for it: OpenCode's global
	// `AGENTS.md` in its configuration directory. Its channel is declared by
	// the file alone.
	DiscoveryAlways Discovery = "always"
)

// Valid reports whether d is a known discovery, the empty one included.
func (d Discovery) Valid() bool {
	return d == "" || d == DiscoverySelected || d == DiscoveryAlways
}

// Found reports whether the CLI finds this file by its location rather than
// being handed its path. Nil-safe: no spec is a file handed over by path.
func (f *SystemPromptFile) Found() bool {
	return f != nil && f.Discovered != ""
}

// ReadAlways reports whether the CLI reads this file on every run, so the file
// alone is the channel. Nil-safe.
func (f *SystemPromptFile) ReadAlways() bool {
	return f != nil && f.Discovered == DiscoveryAlways
}

// CarriesPolicy reports whether the file says something of its own with no
// system prompt in it: a template beyond `{system}`, such as the frontmatter
// that names the agent OpenCode's `--agent` selects, or Kimi Code's tool
// allowlist. A call writes such a file whether or not the request carries a
// system prompt; a file that would hold the prompt alone is written only when
// there is one, because written empty it still says something — OpenCode puts
// an "Instructions from: <path>" line in front of an empty AGENTS.md. Nil-safe.
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
// directory to make it a project root of its own, and ProjectRootGitdir its
// content.
//
// A FILE, and one naming a gitdir, because those are the two shapes every CLI
// measured stops at: an empty `.git` DIRECTORY is ignored by git and by the
// CLIs that ask git where the root is (OpenCode, Qwen Code, Copilot), and Kimi
// Code accepts a `.git` file only when its first line is `gitdir:`. The gitdir
// is relative, so it names a path inside the call's own 0700 directory that
// never exists: git reports "not a git repository" there, and nobody else can
// create it.
//
// Reserved: no seed may claim the path ([Profile.claims]).
const (
	ProjectRootMarker = ".git"
	ProjectRootGitdir = "gitdir: .crewlet-not-a-repository\n"
)

// DefaultSystemPromptFile is what a {file} substitution writes, in the per-call
// working directory.
//
// That directory and not the seat home: it is created empty for one call and
// removed on release, so the text cannot outlive the call that needed it or
// reach the next one. The name is deliberately not one a coding CLI reads on
// its own (CLAUDE.md, AGENTS.md), because this is an argument to the CLI, not
// context for it to discover.
const DefaultSystemPromptFile = "crewlet-system-prompt.txt"

// AuthMarker recognises a login the CLI has stopped honouring.
//
// Same reasoning as [LimitMarker]: an expired OAuth login exits non-zero with
// prose on stderr. Unrecognised, that is the SERVER failure any failed run is,
// which moves the seat's chain on as AUTH does — so what the marker buys is
// the NAME: a seat working off its metered fallback says its login is what
// failed, and the operator knows that `crewlet llm login` is what mends it.
type AuthMarker struct {
	// Sentinel is matched exactly as a [LimitMarker]'s is, in the same scope.
	Sentinel string `yaml:"sentinel,omitempty"`

	// Meaning is what the failure says about the credential the CLI was
	// handed: [AuthRefused] (the default) when the vendor turned it down,
	// [AuthUnread] when the CLI never used it at all. Only the words a person
	// is shown depend on it; every auth marker fires the same failure.
	Meaning AuthMeaning `yaml:"meaning,omitempty"`
}

// AuthMeaning is what an [AuthMarker] says about the credential a run was
// handed.
type AuthMeaning string

const (
	// AuthRefused is a credential the vendor saw and turned down: wrong,
	// expired or revoked. The empty meaning reads as this one.
	AuthRefused AuthMeaning = "refused"
	// AuthUnread is a credential the CLI did not use, so the vendor never saw
	// it: the CLI reports itself signed out although a value was set. Claude
	// Code does this with a token that has a space inside it.
	AuthUnread AuthMeaning = "unread"
)

// AuthMeaning is the meaning of the first auth marker, in the profile's
// order, whose sentinel said contains: what a refused sign-in's own words
// ([llm.Error] Said) say about the credential. [AuthRefused] when none
// matches, since a sign-in failure is otherwise the vendor's refusal.
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

// Profile is everything the engine needs to drive one coding CLI.
//
// It is DATA, not code, and every field is replaceable from YAML
// (`overrides`), because these flags and JSON shapes belong to vendors
// who rename them between releases. A vendor renaming --output-format must be
// an operator's config edit, not a Crewlet release; a profile hard-coded in a
// switch statement makes it the latter.
type Profile struct {
	// Binary is the executable, resolved on PATH unless it is a path.
	Binary string `yaml:"binary,omitempty"`

	// WrittenFor is the CLI version this profile was written against,
	// printed by `crewlet llm doctor` beside the version actually
	// installed. A profile that silently stopped matching its CLI is the
	// failure mode this exists to make visible.
	WrittenFor string `yaml:"written_for,omitempty"`

	// VersionArgs runs the CLI's own version probe.
	VersionArgs []string `yaml:"version_args,omitempty"`

	// CompleteArgs is the argv of one completion, before ModelArgs and
	// before the prompt. Lists replace wholesale on override: position
	// matters in an argv, and merging two argvs element-wise produces a
	// command line neither side wrote.
	CompleteArgs []string `yaml:"complete_args,omitempty"`

	// ModelArgs carries the model, with {model} substituted. One with no
	// {model} — empty included — means the CLI takes no model, and an entry
	// naming a model gets a validation error rather than a silently ignored
	// setting ([Profile.TakesModel]). The placeholder may stand as the
	// flag's own element or after `=` in a joined `--model={model}`.
	ModelArgs []string `yaml:"model_args,omitempty"`

	// ModelNamesProvider declares that the CLI reads its model flag's value
	// as `<provider>/<model>` and splits it at the FIRST slash, so a value
	// with nothing before or after that slash names no model at all and
	// every call fails inside the CLI. Config refuses such a `model` on a
	// write, judged on the value the CLI is handed ([Profile.ModelArgument]).
	//
	// A DECLARATION OF A VENDOR FACT, measured per CLI and overridable like
	// every other: a release that starts accepting a bare id is an override,
	// not a Crewlet release. Only opencode's is measured — OpenCode 2's
	// `run --model` takes `provider/model[#variant]`, splits it at the first
	// slash, and refuses a bare id as "Invalid model reference: <id>" — so
	// only opencode declares it.
	ModelNamesProvider bool `yaml:"model_names_provider,omitempty"`

	// PromptMode is stdin (the default), argv or file.
	PromptMode PromptMode `yaml:"prompt_mode,omitempty"`

	// SystemPrompt is the channel on which the engine's system prompt
	// REPLACES the CLI's own, and AppendSystemPrompt the one on which it is
	// added to the CLI's own, which stays in force. A profile declares what
	// its vendor offers — either, both or neither — and each CALL takes the
	// one its kind of work wants ([Profile.RouteFor]):
	//
	//   - a seat at work (a phase of its turn: an Agent request) takes
	//     the APPEND channel: the vendor's harness prompt is written for
	//     an agent and moves with every model the vendor ships, and the
	//     engine's seat prompt follows it;
	//   - a text function the engine runs on the side (a filter, a
	//     classifier, a rewrite, a judge) takes the REPLACE channel: its
	//     system prompt is the whole instruction, its answer is parsed,
	//     and a coding agent's guidance in front of it would cost a few
	//     thousand tokens on every call and push it toward prose.
	//
	// Each falls back to the other when its own is not declared, and to
	// the transcript when neither is.
	//
	// ON A CHANNEL RATHER THAN IN THE TRANSCRIPT wherever the CLI has one: a
	// system prompt folded into the prompt text arrives as USER content, and
	// the model is asked to treat an ordinary message as its standing
	// instructions.
	SystemPrompt Channel `yaml:"system_prompt,omitempty"`

	// AppendSystemPrompt is the channel that keeps the CLI's own system
	// prompt and adds the engine's — see [Profile.SystemPrompt]. WHERE the
	// vendor puts the added text (after its own, in place of its identity
	// sentence, in an addendum block) is the vendor's; what the channel
	// promises is that the vendor's prompt is still in force.
	AppendSystemPrompt Channel `yaml:"append_system_prompt,omitempty"`

	// PromptArgs introduces the prompt in argv mode, for a CLI that takes it
	// as a FLAG'S VALUE rather than as a positional argument. Empty appends
	// the prompt bare, which is what every other argv profile wants.
	//
	// In [PromptFile] mode it carries the flag AND its `{file}`
	// placeholder — `["--prompt-file", "{file}"]` — and is required, since
	// a file-mode profile with nothing to substitute the path into would
	// hand the CLI no prompt at all.
	//
	// It exists because a CLI can take its prompt as a flag's VALUE, which
	// breaks the assumption the rest of this format is built on: with such a
	// flag sitting in complete_args, the model flag that follows becomes the
	// prompt and the real prompt becomes a stray positional. xAI's grok did
	// exactly that with `-p <PROMPT>` — `grok -p --model grok-4 "hi"` exits on
	// "a value is required for '--single <PROMPT>' but none was supplied" —
	// and every call died at argument parsing; its profile now takes the
	// file form, `--prompt-file {file}`, through this same field.
	//
	// Appended LAST, after the model and system-prompt arguments, because
	// that is the only position where the flag and its value stay adjacent.
	PromptArgs []string `yaml:"prompt_args,omitempty"`

	// Output is how stdout is encoded.
	Output OutputMode `yaml:"output,omitempty"`

	// TextPaths locates the assistant's text. First hit wins for json;
	// for jsonl every match is concatenated in stream order, which is how
	// an event stream spells one answer.
	TextPaths []Path `yaml:"text_paths,omitempty"`

	// EventTypePath locates the DISCRIMINATOR in one line of a `jsonl`
	// stream — the field naming what kind of event the line is. Empty
	// means the stream has none, or that every line's text counts.
	//
	// It exists because an ENVELOPED stream cannot be read by paths alone.
	// Muse Code wraps every event in one envelope shape and puts the kind
	// in `payload_type`, so `payload.text` is the assistant's text on a
	// `run.output.delta`, a tool's output on a `tool.result`, and the whole
	// answer AGAIN on `run.terminal.completed`. A path walk cannot tell the
	// three apart: it would splice tool output into the reply and then
	// repeat the reply. Naming the discriminator once is what makes the
	// stream readable.
	EventTypePath Path `yaml:"event_type_path,omitempty"`

	// TextEvents restricts TEXT extraction to lines whose discriminator is
	// one of these. Empty takes every line, which is what a stream with no
	// envelope wants.
	//
	// Text only, deliberately: usage and error paths are scanned across the
	// whole stream either way, because a stream reports those wherever it
	// likes and the last value wins. What must not be spliced together is
	// the ANSWER.
	TextEvents []string `yaml:"text_events,omitempty"`

	// ErrorPaths locates a boolean the CLI sets when it failed despite
	// exiting zero.
	ErrorPaths []Path `yaml:"error_paths,omitempty"`

	// Usage locates the token counts, in the CLI's stdout — or, where
	// [Profile.UsageFileArgs] or [Profile.UsageArgs] is set, in the report
	// that field names.
	Usage UsagePaths `yaml:"usage,omitempty"`

	// UsageFileArgs asks the CLI to write its token counts to a FILE,
	// carrying the path through the `{usage_file}` placeholder. Empty
	// reads them out of stdout, which is what every other profile does.
	//
	// It exists because a CLI can report usage honestly and still not put
	// it in the answer. Hermes's one-shot entry is `-z`, whose whole
	// contract is "single prompt in, final response text out, NOTHING else
	// on stdout or stderr" — so there is no envelope for a usage path to
	// walk, and the counts ride `--usage-file` instead. Estimating them
	// would be the alternative, and an estimate is what the budget cascade
	// then spends against.
	//
	// The file is written in the per-call working directory and read after
	// the process exits; when it is absent or unreadable the counts fall
	// back to whatever stdout said, because the vendor writes it "even
	// when the run fails" and a broken usage write must never mask the
	// run's own outcome. Absent is silent — the run did not get that far —
	// while a file that cannot be read or is empty logs
	// `cli_agent_usage_file_unreadable`, and one that does not decode, or in
	// which usage.input and usage.output do not both resolve, logs
	// `cli_agent_usage_report_unreadable` — for a run the CLI did not report
	// failed: a failed call is charged nothing, and the vendor's report of one
	// may hold no counts by design.
	UsageFileArgs []string `yaml:"usage_file_args,omitempty"`

	// SessionArgs name the session one call runs in, with `{session}`
	// substituted by an id the engine mints for that call alone: 32
	// lowercase hexadecimal characters, so a vendor that wants a prefix of
	// its own spells it around the placeholder (`ses_{session}`). Appended
	// with the model arguments, before the system prompt and the prompt.
	//
	// It exists for [Profile.UsageArgs], which has to ask the CLI about
	// THIS call afterwards — and a seat's home is shared by every call
	// running in it at once, so "the most recent session" would answer
	// with a sub-agent's.
	SessionArgs []string `yaml:"session_args,omitempty"`

	// UsageArgs is a command run AFTER the call, in the same home and
	// working directory and with the same environment, that prints the
	// call's token counts as one JSON document on stdout — which
	// [Profile.Usage] is then read out of, as it would be a usage file.
	// `{session}` is substituted with the id [Profile.SessionArgs] gave the
	// call.
	//
	// It exists because a CLI can record a call's usage exactly and still
	// not report it where the call prints. OpenCode 2.0's `run --format
	// json` emits a step's counts in a `step_finish` event, and drops the
	// FINAL step's: its formatter stops at the session's completion event,
	// and the last step's end does not reach it first. Measured on 2.0.32,
	// a text-only call printed no `step_finish` in six runs of six, and a
	// call that used a tool printed the tool step's counts and not the
	// answer's. Read from stdout, every call would have been charged for
	// nothing or for its first step. The session the call ran in holds the
	// whole total (`opencode api session.get`), so that is asked instead.
	//
	// A failed or unreadable report is not a failed call: the counts fall
	// back to whatever stdout said, as for a usage file — which, for a CLI
	// that reports no usage there, is the estimate — and a warning says
	// which: `cli_agent_usage_report_failed` for a command that failed or
	// printed nothing, `cli_agent_usage_report_unreadable` for a report that
	// does not decode or in which usage.input and usage.output do not both
	// resolve, on a run the CLI did not report failed.
	UsageArgs []string `yaml:"usage_args,omitempty"`

	// ConfigEnv maps a vendor's own relocation variable to a directory
	// under the seat's home. Without it a CLI reads the engine user's real
	// dotfiles and every seat shares one set of sessions.
	ConfigEnv map[string]string `yaml:"config_env,omitempty"`

	// WorkConfigEnv maps a vendor's relocation variable to a directory under
	// the PER-CALL WORKING directory, where [Profile.ConfigEnv] maps one under
	// the seat's home — for a CLI that reads configuration it must not share
	// with the seat's other calls from a directory a variable names.
	//
	// The seat's home cannot be that directory, because every call a seat runs
	// at once shares it and each call's system prompt is its own. OpenCode 2 is
	// the CLI that needs it, and the alternative it replaced was its PROJECT
	// configuration: OpenCode reads `opencode.json`, and `.opencode/` agents
	// and plugins, from the working directory and EVERY ANCESTOR of it, and a
	// project's permissions beat the seat's — so an `opencode.json` anywhere
	// above the state directory (the engine user's home, a repository
	// state_dir was put in) handed every seat a shell, and a plugin there
	// ran its code in every call (measured on 2.0.32). With that walk switched
	// off (OPENCODE_CONFIG_PROJECT_DISABLE, in [Profile.IsolationEnv]) the
	// only configuration OpenCode reads from a directory is the one
	// OPENCODE_CONFIG_DIR names — it REPLACES the one under XDG_CONFIG_HOME
	// rather than adding to it — so that variable points inside the call's own
	// directory, where the profile's `in: work` seed and its discovered agent
	// file are written. The one exception is a machine-wide MANAGED
	// configuration (`/etc/opencode` on Linux, read from the 2.0.32 bundle),
	// which only an administrator of the host can write and no variable
	// switches off; on a host that has one, it configures every seat.
	//
	// Set only for a command that runs AS A CALL: the completion, and the
	// usage command after it. A command with no call of its own — a credential
	// store's import and export, `crewlet llm login` and `status`, the version
	// probe — has no working directory to point inside, and runs without the
	// variable.
	WorkConfigEnv map[string]string `yaml:"work_config_env,omitempty"`

	// IsolationEnv is a vendor's own SWITCH that keeps the CLI from reading
	// configuration outside its seat's home and its call's directory — set,
	// with its value, on EVERY command this profile runs (a call, its usage
	// command, a credential store's import and export, a login, the version
	// probe), at the same layer as HOME and the relocations.
	//
	// A FIELD APART FROM [Profile.Env], whose values are settings, because a
	// switch here is part of the isolation and has to be protected like the
	// rest of it: the profile's env and passthrough_env, and an entry's
	// env, are all layered OVER the isolation, and each is refused naming
	// one of [Profile.IsolationVariables] — which an ordinary env entry is not.
	// Written in env, OpenCode's project switch was replaced by an entry's
	// `env: {OPENCODE_CONFIG_PROJECT_DISABLE: "0"}` — or by an EMPTY value,
	// which it reads as off — with nothing to say so, and every ancestor's
	// `opencode.json`, agents and plugins were configuration for every seat
	// again: measured on 2.0.32, the model's tools widened to shell, write and
	// execute, an ancestor agent of the seat's own name replaced the seat's
	// system prompt, and the ancestor's plugins ran.
	//
	// The NAME the vendor reads FIRST, never a fallback spelling: OpenCode 2
	// reads `OPENCODE_CONFIG_PROJECT_DISABLE ?? OPENCODE_DISABLE_PROJECT_CONFIG`
	// (read from the 2.0.0 and 2.0.32 bundles), so a profile setting only the
	// second is overruled by any value of the first, an empty one included.
	// With the first fixed here and refused everywhere else, the second is
	// never read, so nothing needs to refuse it.
	//
	// Never a credential, never the profile's token_env or api_key_env
	// ([Profile.ValidateCredentials]), never a variable the engine already
	// sets ([HomeIsolation], a relocation), and never an empty value, which
	// every switch these vendors read takes as off.
	IsolationEnv map[string]string `yaml:"isolation_env,omitempty"`

	// Env is fixed child environment the CLI needs. Never a credential,
	// and never the profile's own token_env or api_key_env, both refused
	// by [Profile.ValidateCredentials] — nor a variable the engine sets to
	// isolate the call ([Profile.IsolationVariables]), refused by
	// [Profile.Validate].
	Env map[string]string `yaml:"env,omitempty"`

	// SubscriptionEnv is child environment set under auth.mode subscription
	// alone, after the operator's env: what tells a CLI that picks its
	// sign-in from its environment to use the login the seat's home holds.
	//
	// A FIELD OF ITS OWN, because the same setting under api-key would
	// overrule the key. Gemini CLI chooses its sign-in from settings or,
	// failing those, its environment — GOOGLE_GENAI_USE_GCA=true first,
	// GEMINI_API_KEY after — and a copied login selects nothing, so every
	// subscription call exited 41 asking for an auth method (measured on
	// 0.58.0 and 0.65.0). In env, or as a settings seed, the variable would
	// have sent every api-key call to the login instead of the key.
	//
	// Never a credential, never the profile's token_env or api_key_env
	// ([Profile.ValidateCredentials]), never a variable the engine sets to
	// isolate the call or one env sets too, and never an empty value.
	SubscriptionEnv map[string]string `yaml:"subscription_env,omitempty"`

	// PassthroughEnv names engine environment variables forwarded to the
	// child. It MAY NOT name a credential, and config refuses a profile
	// that does ([Profile.ValidateCredentials]): everything here is
	// forwarded before auth.mode is consulted, so a key listed here would
	// reach every seat whatever the mode said, which is exactly the
	// metered-bill-on-a-flat-rate-plan failure auth.mode exists to prevent.
	// Nor may it name a variable the engine sets to isolate the call
	// ([Profile.IsolationVariables]): forwarding the engine's own HOME is
	// handing the CLI the engine user's dotfiles.
	PassthroughEnv []string `yaml:"passthrough_env,omitempty"`

	// TokenEnv is the variable carrying a long-lived headless
	// subscription token.
	TokenEnv string `yaml:"token_env,omitempty"`

	// APIKeyEnv is the variable carrying a metered API key, set only
	// under auth.mode api-key or inherit-env.
	APIKeyEnv string `yaml:"api_key_env,omitempty"`

	// APIKeyEnvShared declares that APIKeyEnv is a GENERIC name, one other
	// vendors' keys are kept under too, so an entry names its key
	// (auth.api_key) and the engine never reads that variable from its own
	// secret store or environment in its place — nor forwards the engine's
	// own under auth.mode inherit-env, which config refuses for such a CLI.
	//
	// The fallback exists for a vendor's own variable: ANTHROPIC_API_KEY
	// read for Claude Code is the key a person exported for exactly that
	// CLI. Qwen Code reads OPENAI_API_KEY and sends it to whatever endpoint
	// OPENAI_BASE_URL names — DashScope's when it names none — so falling
	// back to it handed an OpenAI key, kept on the host for embeddings or
	// for OpenAI itself, to another vendor.
	APIKeyEnvShared bool `yaml:"api_key_env_shared,omitempty"`

	// EnvSignIn declares that the CLI reads its model provider's credential
	// from its OWN environment, under that provider's own variable — so a
	// credential-named variable the operator sets in an entry's `env` signs
	// it in. False, the default, means a credential there is not a sign-in
	// this CLI reads, which is the right answer for a profile that says
	// nothing.
	//
	// A DECLARATION RATHER THAN A LIST OF NAMES, because the CLIs that need
	// it front dozens of providers each (hermes thirty-odd, pi twenty-odd,
	// OpenCode every provider on its catalogue) and every one has its own
	// variable: a list would be wrong for whichever provider it missed, and
	// a profile naming one variable would be wrong for every other entry —
	// which is why these profiles have no api_key_env at all.
	//
	// And a declaration at all, rather than counting any credential in an
	// entry's env, because only the profile knows whether the CLI reads one:
	// kimi-code is handed KIMI_API_KEY and ignores it, reading its key only
	// from config.toml.
	//
	// Read by `crewlet llm doctor` and `crewlet llm list` to decide whether
	// an entry is signed in, and by config to say where the key of an entry
	// that has no api_key_env goes instead.
	EnvSignIn bool `yaml:"env_sign_in,omitempty"`

	// CredentialPaths are the login files, relative to the seat home.
	// They are what a bundle may carry and what is synced back after a
	// refresh; anything else in the home is conversation state.
	CredentialPaths []string `yaml:"credential_paths,omitempty"`

	// LoginKeys names, by a credential file's NAME, the top-level key that
	// file holds only once the CLI has signed in — for a file the CLI also
	// keeps state of its own in. Copilot CLI writes `firstLaunchAt` into the
	// config.json its plaintext login lives in on its first start, and Cursor
	// Agent its settings into cli-config.json on every run, signed in or
	// not; carried out of a seat's home, either was a "login" that had the
	// doctor report a sign-in nobody made and kept a configured bundle from
	// being restored. Without its key such a file is not a login
	// ([Profile.HoldsLogin]). A credential file not named here is a login
	// whenever it exists.
	LoginKeys map[string]string `yaml:"login_keys,omitempty"`

	// VolatilePaths are sessions, transcripts, history and todo state,
	// relative to the seat home, deleted before and after every call. A
	// second invisible memory inside the CLI would make turns
	// non-reproducible and carry one task's context into the next.
	VolatilePaths []string `yaml:"volatile_paths,omitempty"`

	// LoginArgs brokers the vendor's own interactive login.
	LoginArgs []string `yaml:"login_args,omitempty"`

	// CaptureTokenArgs mints a headless token on stdout.
	CaptureTokenArgs []string `yaml:"capture_token_args,omitempty"`

	// StatusArgs asks the CLI who it is logged in as.
	StatusArgs []string `yaml:"status_args,omitempty"`

	// LogoutArgs revokes the login.
	LogoutArgs []string `yaml:"logout_args,omitempty"`

	// StdinLogin is a real credential login, where one exists.
	StdinLogin *StdinLogin `yaml:"stdin_login,omitempty"`

	// CredentialStore is set for a CLI that keeps its login in a database of
	// its own rather than in files — see [CredentialStore]. Nil, the default,
	// means the login is CredentialPaths, a token, a key, or nothing.
	CredentialStore *CredentialStore `yaml:"credential_store,omitempty"`

	// LimitEvents recognise a spent plan or a rate limit by the CLI's own
	// structured report, ahead of every sentinel; see [LimitEvent].
	LimitEvents []LimitEvent `yaml:"limit_events,omitempty"`

	// LimitMarkers recognise a spent subscription by the vendor's words.
	LimitMarkers []LimitMarker `yaml:"limit_markers,omitempty"`

	// AuthMarkers recognise an expired login.
	AuthMarkers []AuthMarker `yaml:"auth_markers,omitempty"`

	// AuthEvents recognise a refused sign-in by the CLI's own structured
	// report, ahead of every limit report and sentinel; see [AuthEvent].
	AuthEvents []AuthEvent `yaml:"auth_events,omitempty"`

	// LimitResetEvents date and name the limit a run reported, from the
	// CLI's own fields; see [LimitResetEvent].
	LimitResetEvents []LimitResetEvent `yaml:"limit_reset_events,omitempty"`

	// LimitWindows name the window of a limit from the vendor's sentence,
	// where no [LimitResetEvent] named one; see [LimitWindowWording].
	LimitWindows []LimitWindowWording `yaml:"limit_windows,omitempty"`

	// SignInEnv lists the variables that sign this CLI in when an entry's
	// env sets one: a cloud provider's switch (CLAUDE_CODE_USE_BEDROCK) or a
	// gateway's token (ANTHROPIC_AUTH_TOKEN). What auth.mode env requires
	// one of, and what `crewlet llm list` reports as the route.
	//
	// A LIST, unlike [Profile.EnvSignIn]'s declaration, because these CLIs
	// sign in by a handful of fixed names, and a switch such as
	// CLAUDE_CODE_USE_BEDROCK is no credential: no credential-name rule
	// would count it.
	SignInEnv []string `yaml:"sign_in_env,omitempty"`

	// IdentityEnv lists the variables, none of them a credential, whose
	// values say WHICH account or endpoint a call reaches: a cloud region and
	// profile, a project and location, a gateway's address. Folded into the
	// entry's sign-in identity beside its credentials, so two entries on one
	// cloud switch but different regions, projects or gateways are two
	// sign-ins, and a limit one meets does not sit the other out.
	IdentityEnv []string `yaml:"identity_env,omitempty"`

	// TokenPrefix and APIKeyPrefix are how the vendor's headless token and
	// its API key begin (`sk-ant-oat01-`, `sk-ant-api03-`): what a hint of
	// a stored credential shows, and what tells a key pasted where a token
	// belongs from the token. Empty where the vendor's credentials share no
	// prefix.
	TokenPrefix  string `yaml:"token_prefix,omitempty"`
	APIKeyPrefix string `yaml:"api_key_prefix,omitempty"`

	// TokenLifetimeDays is how long the headless token the CLI mints
	// ([Profile.CaptureTokenArgs]) lasts, as the vendor documents it. Zero
	// is unknown. The token is opaque, so its expiry is computed from when
	// it was stored, never read from it.
	TokenLifetimeDays int `yaml:"token_lifetime_days,omitempty"`

	// DeviceLoginArgs runs the vendor's device-code sign-in, which prints
	// an address and a one-time code a person enters elsewhere and then
	// waits for their approval; see [DeviceCodePatterns].
	DeviceLoginArgs []string `yaml:"device_login_args,omitempty"`

	// DeviceCodePatterns read the address and the code out of what
	// DeviceLoginArgs prints.
	DeviceCodePatterns *DeviceCodePatterns `yaml:"device_code_patterns,omitempty"`

	// StatusTextArgs asks the CLI who it is logged in as, in words, where
	// StatusArgs prints JSON: what `crewlet llm doctor` shows a person.
	// Empty shows StatusArgs' own output.
	StatusTextArgs []string `yaml:"status_text_args,omitempty"`

	// StatusFields locates the sign-in method and the account in the JSON
	// StatusArgs prints; see [StatusFields]. Nil reads only the exit code.
	StatusFields *StatusFields `yaml:"status_fields,omitempty"`

	// ModelAliases are model names the CLI resolves itself; see
	// [ModelAlias].
	ModelAliases []ModelAlias `yaml:"model_aliases,omitempty"`

	// ModelsDocs is the vendor's own page listing the models the CLI takes.
	ModelsDocs string `yaml:"models_docs,omitempty"`

	// MarkerScope is where the two marker sets above may be matched —
	// see [MarkerScope]. Empty searches the answer and stderr, which is
	// what a CLI reporting a spent plan as its answer needs.
	MarkerScope MarkerScope `yaml:"marker_scope,omitempty"`

	// MarkerEvents names the events — by the value at [Profile.EventTypePath]
	// — in which the CLI reports a failure of its own: the only lines the
	// [MarkerScopeEvents] scope looks at. Required by that scope, and refused
	// beside any other, which reads nothing here.
	MarkerEvents []string `yaml:"marker_events,omitempty"`

	// MarkerPath locates the failure CLASS inside one of
	// [Profile.MarkerEvents]: the string a sentinel must EQUAL, not merely
	// contain. Required by the [MarkerScopeEvents] scope, and refused beside
	// any other.
	MarkerPath Path `yaml:"marker_path,omitempty"`

	// HostCredentialPaths are where this CLI keeps its login in a human's
	// own home directory, for `crewlet llm login -from-host` to adopt.
	// Paths are relative to that home.
	HostCredentialPaths []string `yaml:"host_credential_paths,omitempty"`

	// LocalTools is the profile's stance on the CLI's own tools — see
	// [LocalTools]. Empty means the profile has not said, which the doctor
	// reports as such rather than assuming either way.
	LocalTools LocalTools `yaml:"local_tools,omitempty"`

	// LocalToolsNote explains a vendor-default stance: which flag is
	// missing, or which flag would also cut the web. Printed by the
	// doctor beside the probe result.
	LocalToolsNote string `yaml:"local_tools_note,omitempty"`

	// ToolsOff is how this CLI's run is left with NO tool of its own at
	// all, by construction in the argv it runs, or nil where no measured
	// way exists. See [ToolsOff].
	ToolsOff *ToolsOff `yaml:"tools_off,omitempty"`

	// SeedFiles are the settings files written for the CLI before a call
	// — the vendors that take their tool policy from a file rather than a
	// flag. See [SeedFile].
	SeedFiles []SeedFile `yaml:"seed_files,omitempty"`

	// WebTools is the profile's stance on the CLI's own web tools, see
	// [WebTools]. Empty reads as allowed. Set per entry through overrides;
	// no built-in profile sets it.
	WebTools WebTools `yaml:"web_tools,omitempty"`

	// WebToolsNote says how this CLI's web denial was measured, or why the
	// profile has none. Printed by the doctor beside the web probe.
	WebToolsNote string `yaml:"web_tools_note,omitempty"`

	// WebDenial is how this CLI turns its web tools off, or nil where it
	// has no measured way to. See [WebDenial].
	WebDenial *WebDenial `yaml:"web_denial,omitempty"`

	// KeySeedFiles are settings files written into a seat's KEY home alone —
	// the home its calls run in when they sign in with a key (auth.mode
	// api-key, or inherit-env handing it the engine's own) — after
	// seed_files, each over a seed_files entry at the same path.
	//
	// The counterpart of [Profile.SubscriptionEnv], for a CLI that picks
	// its sign-in from SETTINGS before its environment, so no variable can
	// tell it to use the key. Gemini CLI takes a GOOGLE_GEMINI_BASE_URL — an
	// operator's gateway in front of the API — as its gateway sign-in, which
	// it then refuses ("Invalid auth method selected", exit 41), unless its
	// settings name the key sign-in; and the same settings in a login home
	// would send every subscription call to a key it does not have (read
	// from the 0.65.0 bundle, and measured). A key home and a login home are
	// two directories ([Workspace.AcquireForKey] in the backend), so each
	// keeps its own.
	//
	// The seat's home only (`in` is home, its default): a call's working
	// directory is written per call and knows nothing of how it signs in.
	KeySeedFiles []SeedFile `yaml:"key_seed_files,omitempty"`

	// MCP is how the CLI is handed a phase's tools as an MCP server — see
	// [MCP]. Nil: the CLI cannot be handed any, and a seat's phase on it is
	// refused.
	MCP *MCP `yaml:"mcp,omitempty"`

	// Requests is how the CLI's own record shows each model request of a run
	// with tools — see [Requests]. Nil: the record shows none, and the whole
	// run counts as one round.
	Requests *Requests `yaml:"requests,omitempty"`

	// Cap is how a run the CLI's own turn cap ended is told from a failure —
	// see [Cap]. Nil: the CLI has no cap, or the profile cannot tell one.
	Cap *Cap `yaml:"cap,omitempty"`

	// FailureMarkers are sentinels the CLI writes on stderr when its own
	// confinement failed to load — a policy file it could not read — after
	// which it runs on its vendor's defaults. Matched as each stderr line
	// arrives, on every call, and the run is stopped at once and fails: a CLI
	// running with its tool policy unloaded must not be left to use the tools
	// that policy denies (Gemini CLI reports "Policy file error" and carries
	// on, measured on 0.58.0).
	FailureMarkers []string `yaml:"failure_markers,omitempty"`
}

// ToolsOff is the proof, IN THE ARGV, that a CLI's run offers its model no
// tool of its OWN: no shell, no file reader or writer, no web, and no tool
// server but the engine's. [LocalToolsDenied] is a claim about a profile; this
// is what a caller that must not be reached around (the Dashboard Assistant,
// whose every read goes through the person's own requests) checks instead,
// against the exact argv each of its calls runs.
//
// THE ENGINE'S SERVER IS THE ONE TOOL SOURCE LEFT. A run with tools is handed
// the phase's tools as one MCP server (ADR-0049, the profile's mcp block), and
// for the assistant those tools are the person's own requests. So the call's
// own arguments must turn every built-in tool off and load no server of their
// own, and the mcp block may allow nothing but the engine's tools.
//
// A DENY LIST IS NOT THIS. Measured on Claude Code 2.1.310 with no login (no
// model reached): the profile's --disallowedTools list still left thirteen
// tools on offer (Workflow, SendMessage, EnterWorktree and the cron tools
// among them), where `--tools ""` left none. A vendor adds tools in every
// release, and a list of the ones to refuse is always a release behind; an
// empty allow list is not.
type ToolsOff struct {
	// Args are the argument runs that leave the CLI with no built-in tool
	// and no server but the ones the mcp block names. Each must appear in
	// the call's own argv, its elements adjacent and in order, and a run's
	// flag exactly once, since a CLI takes the last value of a repeated flag
	// and a second `--tools default` would undo the first.
	Args [][]string `yaml:"args,omitempty"`
	// Refuses are flags whose presence in the call's own arguments brings a
	// tool back (a tool server's config, an allow list, a plugin), so an
	// argv carrying one is not off whatever else it says. The mcp block's
	// own arguments are judged apart: they may allow only the engine's tools.
	Refuses []string `yaml:"refuses,omitempty"`
	// Note says how the construction was measured.
	Note string `yaml:"note,omitempty"`
}

// ToolsOffProblem says why the argv this profile runs with tools is NOT left
// with no tool of its own, or "" when it is: every run of [ToolsOff.Args]
// present exactly once and no flag of [ToolsOff.Refuses] in the call's own
// arguments, and an mcp block whose allow list names the engine's tools alone.
func (p *Profile) ToolsOffProblem() string {
	if p.ToolsOff == nil {
		return "the profile has no measured way to run the CLI with no tool of its own"
	}
	if p.MCP == nil {
		return "the profile has no mcp block, so the engine's tools cannot reach its CLI"
	}
	complete := p.CompleteArgs
	if len(p.MCP.CompleteArgs) > 0 {
		complete = p.MCP.CompleteArgs
	}
	if problem := p.ToolsOff.problemIn(complete); problem != "" {
		return problem
	}
	return p.mcpAllowProblem()
}

// mcpAllowProblem says why the mcp block's allow list admits a tool that is
// not the engine's, or "" when every tool it names is one: spelled with the
// prefix the CLI gives the engine's server ([Requests.HostPrefix]). A block
// that allows nothing by name (a CLI whose server's tools need no allow list)
// admits nothing of its own either.
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
			return fmt.Sprintf("the argv carries %s %d times, and the CLI takes the last", run[0], n)
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

// withWebDenied is the profile as it runs with its web tools off: the stance
// set and every field the denial names replaced. The denial itself is kept,
// so the doctor can still print it, and applying it twice is applying it once.
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

// credentialWords are the name components that mark a variable as carrying a
// secret.
//
// WHOLE COMPONENTS, NOT SUBSTRINGS. The set of vendor key names is open —
// GOOGLE_API_KEY, GH_TOKEN and OPENROUTER_API_KEY have nothing in common but
// the shape of the name — so the rule reads that shape: a name is cut at every
// character that is not a letter or a digit, and it is a credential when one of
// its components is one of these words. A substring match read
// CLAUDE_CODE_MAX_OUTPUT_TOKENS and HERMES_MAX_TOKENS as credentials, which
// refused a real tuning variable in a profile's env and counted an entry with
// no key at all as signed in — a predicate that decides whether an entry is
// healthy cannot be that loose. The joined forms (APIKEY, ACCESSKEY,
// SECRETKEY) are listed because some vendors write their names that way, and
// KEYS because a variable holding several keys (GEMINI_API_KEYS) is still a
// credential — TOKENS is deliberately absent, being how every CLI spells a
// count of model tokens.
var credentialWords = []string{
	"KEY", "KEYS", "APIKEY", "ACCESSKEY", "SECRETKEY", "TOKEN", "SECRET",
	"PASSWORD", "PASSWD", "CREDENTIAL", "CREDENTIALS", "AUTH", "OAUTH", "PAT",
}

// IsCredentialName reports whether an environment variable's name says it
// carries a secret: whether any of its components, cut at every character
// that is not a letter or a digit and compared without regard to case, is
// one of [credentialWords].
//
// One predicate for every question this backend asks of a name — which
// passthrough_env and env entries are refused, which of an entry's env
// entries sign a CLI in, and which travel into a coding box — so the answers
// cannot drift.
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

// TakesModel reports whether this profile passes a model to its CLI — whether
// an element of its model_args carries {model}.
//
// One predicate for the two places that refuse a model the CLI would never
// see: config, which every write path runs, and the backend's constructor.
// The PLACEHOLDER, not merely a flag: `["--model", "fixed"]` passes `fixed`
// on every call whatever the entry's `model` says, which drops the entry's
// model exactly as silently as declaring no flag at all.
func (p *Profile) TakesModel() bool {
	return slices.ContainsFunc(p.ModelArgs, func(arg string) bool {
		return strings.Contains(arg, modelPlaceholder)
	})
}

// modelPlaceholder is what a model_args element carries the entry's model as.
const modelPlaceholder = "{model}"

// ModelArgument is the model as a run hands it to the CLI: the VALUE of
// the model_args element carrying {model}, rendered with model — so an
// override such as ["--model", "openrouter/{model}"] is part of it. The model
// itself when no element carries the placeholder, which config and the
// backend both refuse beside a model ([Profile.TakesModel]).
//
// ONE RENDERING FOR EVERY PLACE THE MODEL GOES, because a cli-agent entry's
// `model` is written in its CLI's own grammar and every reader of it has to
// see the same string: the argv of a run on this host, the executor's run of
// the same CLI in a box, and the config rule that judges the value as that
// CLI reads it. A run that rebuilt the value its own way named one model to
// the box and another to every run on this host on the same entry.
//
// THE VALUE, NOT THE ELEMENT. A box's runner writes its own flag
// (`--model <value>`), so an element that spells flag and value as one —
// `--model={model}`, which every CLI here also accepts — is read past its
// first `=`: returned whole it reached the box as `--model
// '--model=anthropic/claude-sonnet-5'`, and the config rule judged
// `--model=anthropic` as the provider.
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

// HomeIsolation is the variables the engine points inside a seat's home — or,
// for XDG_CACHE_HOME, beside it — for every CLI, whatever its profile says.
//
// Declared here, beside [Profile.IsolationVariables], rather than only in the
// code that sets them, because config refuses an entry that would override
// one and has to be able to name them without importing the backend.
var HomeIsolation = []string{
	"HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "TMPDIR",
}

// IsolationVariables names every variable the engine sets to keep this CLI
// inside its seat's home and its call's working directory: [HomeIsolation],
// then the profile's [Profile.ConfigEnv], [Profile.WorkConfigEnv] and
// [Profile.IsolationEnv], each sorted.
//
// One list for the three places that must not set one of them — the profile's
// own env and passthrough_env ([Profile.Validate]) and an entry's env
// (config) — because every one of those is layered OVER the isolation when the
// child's environment is built, so a value there silently replaces the seat's
// home with whatever it names: the engine user's own dotfiles, a
// configuration directory no seat's system prompt is written to, or a switch
// turned back off.
func (p *Profile) IsolationVariables() []string {
	names := slices.Clone(HomeIsolation)
	names = append(names, slices.Sorted(maps.Keys(p.ConfigEnv))...)
	names = append(names, slices.Sorted(maps.Keys(p.WorkConfigEnv))...)
	return append(names, slices.Sorted(maps.Keys(p.IsolationEnv))...)
}

// IsolationLayer is which of the layers that keep a CLI inside its seat's home
// and its call's working directory sets one of [Profile.IsolationVariables] —
// and so which field, if any, changes the value the child is given.
type IsolationLayer string

// The layers, each but the engine's own named by the profile field that sets
// it, which is what an operator overrides it with (overrides.<field>).
const (
	// IsolationHome is [HomeIsolation]: HOME, the XDG homes and TMPDIR, set
	// by the engine for every CLI, whatever its profile says, and changed by
	// no field at all — config_env, work_config_env and isolation_env each
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

// IsolationLayerOf reports which layer sets name, and false when none does:
// when name is not one of [Profile.IsolationVariables].
//
// ONE LAYER PER VARIABLE in a profile that loaded, because each field refuses
// the others' names ([Profile.Validate]): config_env refuses [HomeIsolation],
// work_config_env refuses it and config_env's variables, and isolation_env
// refuses all three. So the answer is the layer whose value the child is
// given, with no precedence to decide.
//
// For the refusals that have to say WHERE a variable's value is changed,
// rather than a second copy of the lists beside each of them: an entry's
// env naming OPENCODE_CONFIG_DIR used to be sent to
// overrides.config_env, which [Profile.Validate] refuses for that very
// variable as work_config_env's, and naming OPENCODE_CONFIG_PROJECT_DISABLE
// to the same field, refused there as an isolation_env switch.
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

// KeptUnder names the fields whose paths this profile keeps inside the
// directory one of its relocation variables points at — a [Profile.ConfigEnv]
// directory under the seat's home, a [Profile.WorkConfigEnv] one under the
// call's working directory — each once, in field order; nil for a variable
// neither relocates, or one under which the profile keeps nothing.
//
// WHAT HAS TO MOVE WITH THE DIRECTORY. The CLI finds each of those paths
// THROUGH the variable — CODEX_HOME's login and sessions, the policy and the
// agent file under OPENCODE_CONFIG_DIR — so pointing the variable somewhere
// else and leaving them where they were is a CLI that never reads its login,
// never has its sessions pruned, or never sees the tool policy that denies it
// a shell. The refusal of an entry's env naming the variable says which
// fields move with it; a profile that moves the directory without them does
// not load ([Load]).
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

// keptPath is one path a profile keeps for its CLI in a scope: the field it
// is set with, the entry as an operator would find it, and the path as
// written.
type keptPath struct {
	field, label, path string
}

// kept lists the paths this profile keeps for its CLI in one scope — the ones
// a relocation of that scope can strand. Under the seat's home: the login
// files, the volatile paths and the home's seed files. Under the call's
// working directory: its seed files and a discovered system prompt file, which
// the CLI finds by where it is; a `{file}` one is handed to the CLI by its
// path, wherever it is.
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

// namedChannel is one of a profile's system-prompt channels with the field it
// is configured under, for a message to name what an operator would override.
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
// under the directory it is joined onto, rather than that directory or a
// directory spelled with a trailing separator: its last element, as written,
// is a [isBareFileName], and it does not clean to the directory itself.
//
// The last element AS WRITTEN, not after cleaning, because cleaning is what
// hides the shape: `x/..` cleans to `.` and `agents/` to `agents`, and the
// second of those reads as a file name while the operator wrote a directory.
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
	return fmt.Errorf("cli-agent profile %q: %s", name, strings.Join(bad, "; "))
}

// problems is what [Profile.Validate] reports, one sentence each.
func (p *Profile) problems() []string {
	var bad []string
	add := func(format string, args ...any) {
		bad = append(bad, fmt.Sprintf(format, args...))
	}

	if strings.TrimSpace(p.Binary) == "" {
		add("binary is empty — set overrides.binary")
	}
	if len(p.CompleteArgs) == 0 && p.EffectivePromptMode() == PromptStdin {
		// Only stdin mode can end up with NO argv at all. The other two
		// build one from prompt_args and the prompt itself, so a CLI
		// invoked as `mycli --prompt-file <path>` and nothing else is a
		// legitimate shape rather than a profile that forgot to say how
		// to run its binary.
		add("complete_args is empty — set overrides.complete_args")
	}
	if p.PromptMode != "" && !p.PromptMode.Valid() {
		add("prompt_mode %q (want stdin, argv or file)", p.PromptMode)
	}
	if p.Output != "" && !p.Output.Valid() {
		add("output %q (want json, jsonl or text)", p.Output)
	}
	if p.Output != OutputText && len(p.TextPaths) == 0 {
		// The DEFAULTED mode, not the field: a profile that names no
		// output is a json profile, and the raw field printed "a  profile".
		add("text_paths is empty — a %s profile must say where the answer is", p.EffectiveOutput())
	}
	p.validateRelocations(add)
	if len(p.PromptArgs) > 0 && p.EffectivePromptMode() != PromptArgv && p.EffectivePromptMode() != PromptFile {
		add("prompt_args is set but prompt_mode is %q — the flag introduces a prompt "+
			"on argv and there is none to introduce", p.EffectivePromptMode())
	}
	if p.EffectivePromptMode() == PromptFile && !hasPlaceholder(p.PromptArgs, "{file}") {
		// Refused rather than defaulted to a bare append: a file-mode
		// profile whose argv never carries the path runs the CLI with no
		// prompt, which a vendor answers by opening an interactive
		// session or by printing usage — neither of which looks like the
		// configuration error it is.
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
		// Refused rather than ignored: an operator who wrote it meant the
		// answer to be picked out of an event stream, and a filter that
		// silently did nothing is a debugging session.
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
		// BOTH PROMPT COUNTS, because [extracted.applyUsageReport] accepts a
		// report only when both resolve — a partial overlay would pair one
		// source's input with another's output, and the sum is what a
		// budget is charged. A profile declaring one of them (or only the
		// cache paths) therefore estimates on EVERY call while
		// [Profile.ReadsUsage] and `crewlet llm doctor` report it as using
		// the vendor's own figures, which is the one thing that report
		// exists to settle.
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
		// Refused rather than ignored: an operator who named the failure
		// event meant markers to be matched in it, and under any other scope
		// they would be matched as substrings of the answer and stderr while
		// the profile appeared to say otherwise.
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
	p.validateSignIn(add)
	// Checked at all, which it was not: an auth marker fires KindAuth, and
	// KindAuth exhausts the credential exactly as a spent plan does — so an
	// unusable sentinel costs the same here as it does above.
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
		// BOTH HALVES OR NEITHER: an export with no import leaves every seat
		// home empty, and an import with no export can never write back the
		// token a call refreshed, which is how a fleet is logged out at the
		// next expiry.
		if len(store.ExportArgs) == 0 {
			add("credential_store.export_args is empty — a login held in the CLI's " +
				"own database can only leave it through the CLI's export command")
		}
		if len(store.ImportArgs) == 0 {
			add("credential_store.import_args is empty — a seat home starts from an " +
				"empty database, and only the CLI's import command puts the login in it")
		}
		for _, args := range [][]string{store.ExportArgs, store.ImportArgs} {
			if hasPlaceholder(args, "{file}") {
				// The document is a credential, so it goes on stdin, which no
				// other account can read; a path would also have to be a
				// file the engine wrote and then had to remember to remove.
				add("credential_store args carry {file} — the document travels on " +
					"stdin and stdout, never through a path")
				break
			}
		}
		for _, rel := range p.CredentialPaths {
			if filepath.Base(rel) == CredentialStoreDocument {
				// The shared directory keeps both kinds under their base
				// names, so one would overwrite the other there and in every
				// bundle.
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
		// A stance that admits the CLI's tools without saying why is
		// exactly the silent hole the field exists to make visible.
		add("local_tools is vendor-default but local_tools_note is empty — say which " +
			"denial the vendor lacks")
	}
	p.validateWebStance(add)
	p.validateToolsOff(add)
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
			// The same two things seeding's UnderRoot proves — relative,
			// and not escaping the scope — asked before any seat home
			// exists.
			add("seed_files[%d].path %q must be relative and stay inside the %s directory",
				i, f.Path, f.Scope())
		case !namesAFile(f.Path):
			// The content is written to exactly this path, and UnderRoot
			// proves only that it stays inside — it accepts the scope
			// directory itself. So `.` and `x/..` are a write onto the
			// home or the working directory (EISDIR on every call), and
			// `.crewlet-config/` writes a regular FILE where the CLI and
			// the engine need a directory, so the system prompt's write
			// under it fails ENOTDIR on every call. The rule a channel's
			// file.name is held to, for the same reason.
			// The same directory spelled as a file name, `.crewlet-config`,
			// is a conflict between two fields rather than a shape, and
			// [Profile.validateCollisions] refuses it.
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

// validateEveryStep refuses [PathEvery] in every path that reads ONE value —
// the answer, the failure flag, an event's kind and its failure class — where
// only a usage count, which adds up, may take it ([Path]). Refused rather than
// read as the first member: the members of a decoded object have no order, so
// the same stdout would answer differently from one call to the next.
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
// A profile that keeps web on but CARRIES a denial has that denial validated
// as the profile it would become, so a broken web_denial is refused where it
// is written rather than on the first entry that turns web off.
func (p *Profile) validateWebStance(add func(string, ...any)) {
	if p.WebTools != "" && !p.WebTools.Valid() {
		add("web_tools %q (want allowed or denied)", p.WebTools)
		return
	}
	if p.WebDenial != nil && !p.WebDenial.changes() {
		add("web_denial sets neither complete_args nor seed_files, so it turns " +
			"nothing off; say what the CLI runs with instead, or remove it")
		return
	}
	if p.WebDenial != nil && p.WebDenial.MCP != nil && p.MCP == nil {
		add("web_denial.mcp is set but the profile has no mcp block to replace it in")
	}
	switch {
	case p.EffectiveWebTools() == WebToolsDenied && p.WebDenial == nil:
		// Refused rather than run: the stance would be a claim the CLI
		// does not honour, and the assistant admits an entry on exactly
		// this claim.
		add("web_tools is denied but the profile has no web_denial, so this CLI "+
			"has no measured way to turn its web tools off (%s); set "+
			"overrides.web_denial, or keep web_tools allowed", orUnstated(p.WebToolsNote))
	case p.EffectiveWebTools() == WebToolsAllowed && p.WebDenial != nil:
		denied := p.withWebDenied()
		for _, problem := range denied.problems() {
			add("with web_tools denied, %s", problem)
		}
	}
}

// validateToolsOff holds a tools_off block to its shape. Whether the argv
// carries it is [Profile.ToolsOffProblem]'s question, asked by the one caller
// that requires it, since a seat's argv deliberately keeps the web.
func (p *Profile) validateToolsOff(add func(string, ...any)) {
	t := p.ToolsOff
	if t == nil {
		return
	}
	if len(t.Args) == 0 {
		add("tools_off.args is empty: name the arguments that leave the CLI with no tool")
	}
	for i, run := range t.Args {
		if len(run) == 0 {
			add("tools_off.args[%d] is empty", i)
		}
	}
	if p.LocalTools != LocalToolsDenied {
		add("tools_off is set but local_tools is %q: a CLI run with no tool denies its local tools", p.LocalTools)
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

// pathClaim is one path a profile has the engine WRITE a file at, or has the
// CLI read a DIRECTORY at, inside one scope: the field that names it, and the
// path clean.
type pathClaim struct {
	label, path string
	dir         bool
}

// claims lists every path this profile claims in one scope whose own shape is
// valid — a path its shape check refuses is reported there and not again.
// Under the seat's home: the login files and the home's seed files are
// written, and each config_env directory is read. Under the call's working
// directory: its seed files and the system prompt file are written, and each
// work_config_env directory is read.
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
		// FIRST, so a seed at the same path is the one refused, under a
		// label that says the engine owns it.
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
		// A key home's own: one at a seed_files path is that file's key-home
		// content, written in its place, so it is the same claim rather than
		// a second write — and one anywhere else is held to every rule a
		// seed is.
		for i, f := range p.KeySeedFiles {
			if !staysInside(f.Path) || !namesAFile(f.Path) {
				continue
			}
			clean := filepath.Clean(f.Path)
			if seeded[clean] {
				// Replacing it, once: a second key seed at the path is a
				// second write, which the claim below is refused as.
				seeded[clean] = false
				continue
			}
			file(fmt.Sprintf("key_seed_files[%d].path", i), f.Path)
		}
	}
	if scope == SeedWork {
		// Written by every call that takes a file channel, discovered or
		// handed over by path, so a seed at the same path is overwritten
		// before the CLI ever reads it. ONE CLAIM PER PATH across the two
		// channels: a call takes one channel or the other, so two channels
		// naming one file (Kimi's agent file, with and without the vendor's
		// own prompt in it) write it once per call, never twice.
		claimed := map[string]bool{}
		for _, ch := range p.channels() {
			if !ch.WritesFile() {
				continue
			}
			// The shapes the channel's own rules admit: a file the CLI
			// finds is a path inside the call's directory, one handed
			// over by path a bare file name. A malformed one is that
			// rule's to report, and claims nothing here.
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
		// Written on a run with tools alone, after the seeds above, so one at
		// a path a seed or the system prompt file also names replaces it on
		// exactly those runs.
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

// validateCollisions holds the paths a profile claims in each scope
// ([Profile.claims]) apart: no file the engine writes may be a path another
// claim needs as a DIRECTORY — the same path as a relocated directory, or an
// ancestor of any other claim — and no two writes may name one file.
//
// The shape of one path ([namesAFile]) cannot see this, because the conflict
// is between two fields: `.crewlet-config` is a file name, and opencode needs
// that name as OPENCODE_CONFIG_DIR's directory, so seeding a file there failed
// the agent file's write under it with ENOTDIR on every call — as did
// `.crewlet-config/agents` and, in the seat's home, codex's `.codex`, which
// both CODEX_HOME and its login need as a directory. And a file written twice
// is one write the CLI never sees: a seed at a login path replaces the seeded
// login and is then carried back out over the shared one, and a seed at the
// system prompt's path is replaced before the CLI starts.
//
// One refusal per file, naming the first claim it collides with.
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

// collision is what is wrong with the file claims[i] beside the other claims
// of its scope, or "" for nothing: a relocated directory at its very path
// first, as the plainest statement of the conflict, then an earlier write of
// the same file, then the first claim that needs it as a directory above
// itself.
func collision(claims []pathClaim, i int, scope SeedScope) string {
	a := claims[i]
	for _, b := range claims {
		if b.dir && b.path == a.path {
			return fmt.Sprintf("%s %q is the directory %s points the CLI at, in the %s "+
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

// validateHomePaths holds the paths a login and a prune are made of —
// [Profile.CredentialPaths], [Profile.HostCredentialPaths] and
// [Profile.VolatilePaths] — to the shape in which each names what it says,
// asked at load as a seed path is.
//
// Every one of them is operator-overridable and joined onto a directory only
// at run time, where [UnderRoot] alone checks it — and refuses one climbing out
// of the home on EVERY call, so `credential_paths: [../auth.json]` loaded, was
// admitted by every write path, and failed each call's seed, and a volatile
// path like it each call's prune. A login path is a FILE besides: each is read,
// written and carried by its base name, so `.` and `x/..` — a directory, under
// a base name of `.` or `..` — fail with EISDIR wherever a seed or a write-back
// reads them, `.vendor/` writes a file where its operator wrote a directory,
// and in a human's home, where `-from-host` takes regular files only, each of
// them is a login nothing ever finds. And two credential paths sharing a base
// name are ONE file in the shared directory and in every bundle, so the second
// overwrote the first there, and every seat was seeded with one login in both
// places — as one named [BundleManifest] would be one entry in every bundle with
// the manifest saying which CLI the bundle was exported from.
//
// A volatile path may name a directory — most do — so it is held only to
// staying inside the home.
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
				"manifest under — the entry naming the CLI it was exported from — so a login "+
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
		case strings.TrimSpace(p.LoginKeys[name]) == "":
			add("login_keys.%s is empty — name the top-level key the file holds once the "+
				"CLI has signed in", name)
		}
	}
	// A HOST FILE IS ADOPTED UNDER ITS FILE NAME, and a seat is seeded only
	// with the names credential_paths gives — so a host path whose file name
	// none of them has is adopted, reported as the login found, and read by
	// no seat: copilot listed gh's `.config/gh/hosts.yml`, adopted as
	// `hosts.yml` beside the `hosts.json` its credential_paths then named.
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
}

// validateRelocations holds [Profile.ConfigEnv] and [Profile.WorkConfigEnv] to
// the one shape in which they relocate anything: each variable named once,
// none the engine already points at the seat's home, and each directory
// relative and inside its root — holds [Profile.IsolationEnv] to a vendor
// variable of its own with a value — and holds env and passthrough_env off
// every variable any of them, or [HomeIsolation], names.
//
// The directories were never checked before: `config_env: {CODEX_HOME: ../..}`
// loaded, and pointed the vendor's whole configuration — sessions and login
// included — outside the seat's home, which is the isolation this field exists
// to provide.
func (p *Profile) validateRelocations(add func(string, ...any)) {
	for _, name := range slices.Sorted(maps.Keys(p.ConfigEnv)) {
		rel := p.ConfigEnv[name]
		switch {
		case slices.Contains(HomeIsolation, name):
			// The engine's own, for every CLI, and never a vendor's: HOME
			// is the seat's home, and the XDG homes and TMPDIR already
			// point inside it (the cache beside it). Relocating one moved
			// nothing out of the home that was not in it, and stranded
			// whatever the profile keeps at the engine's location — on
			// muse-code, `XDG_CONFIG_HOME: .x` left the seeded tool policy
			// in a `.config` the CLI no longer read, handing every seat
			// its default tools. It was also the one overlap between the
			// isolation layers, so an entry's env naming XDG_CONFIG_HOME
			// could not be told that no field changes it.
			add("config_env may not name %s — the engine points it at or inside the "+
				"seat home for every CLI already; config_env relocates a variable of "+
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
			// A home variable pointed at a directory removed after every call
			// takes the login and every setting the home holds with it.
			add("work_config_env may not name %s — the engine points it at the seat's "+
				"home, which outlives the call", name)
		case alsoHome:
			add("config_env and work_config_env both name %s — one variable names one "+
				"directory; keep it in whichever the CLI must not share across calls", name)
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
			// Two values for one variable, of which the child would get
			// whichever layer happened to be applied last.
			add("isolation_env names %s, which the engine already points at a directory — "+
				"a switch is a variable of the vendor's own, never a relocated one", name)
		case strings.TrimSpace(p.IsolationEnv[name]) == "":
			// Not "unset": a vendor reading `1` or `true` as on reads an
			// empty value as OFF, so the switch would be declared and do the
			// opposite of what it says.
			add("isolation_env.%s is empty — give the value that switches the vendor's "+
				"behaviour off, e.g. \"1\"; an empty one reads as off", name)
		}
	}
	// LAYERED OVER THE ISOLATION, which is why they are refused here: the
	// child's environment applies the profile's env and passthrough after
	// the variables that point it at its own home, so one named here
	// replaces the seat's home with something else and says nothing.
	isolation := p.IsolationVariables()
	for _, name := range slices.Sorted(maps.Keys(p.Env)) {
		if slices.Contains(isolation, name) {
			add("env names %s, which the engine sets to keep the CLI inside its seat's "+
				"home and its call's working directory — a value here would replace that "+
				"isolation; relocate a vendor directory with config_env or work_config_env, "+
				"and set a vendor's isolating switch in isolation_env", name)
		}
	}
	for _, name := range p.PassthroughEnv {
		if slices.Contains(isolation, name) {
			add("passthrough_env names %s, which the engine sets to keep the CLI inside "+
				"its seat's home and its call's working directory — forwarding the engine's "+
				"own would hand the CLI the engine user's directories", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(p.SubscriptionEnv)) {
		_, inEnv := p.Env[name]
		switch {
		case strings.TrimSpace(name) == "":
			add("subscription_env names an empty variable")
		case slices.Contains(isolation, name):
			add("subscription_env names %s, which the engine sets to keep the CLI inside "+
				"its seat's home and its call's working directory — a value here would "+
				"replace that isolation on every subscription call", name)
		case inEnv:
			// Two values for one variable, of which the child gets the
			// subscription one under subscription and the other under
			// every other mode — a sign-in choice made in two places.
			add("subscription_env and env both name %s — set it in subscription_env "+
				"alone, which is the only mode it may apply under", name)
		case strings.TrimSpace(p.SubscriptionEnv[name]) == "":
			add("subscription_env.%s is empty — give the value that selects the CLI's "+
				"login, e.g. \"true\"", name)
		}
	}
}

// strandedBy reports every relocation an override MOVED that left behind a
// path the built-in kept under it: a [Profile.ConfigEnv] or
// [Profile.WorkConfigEnv] variable pointed at a different directory than
// builtin points it at, while a path this profile keeps in that scope
// ([Profile.kept]) still lies under the built-in's directory and is not
// reached through the moved variable — outside the new directory, or under it
// only because the new directory is the old one's ancestor.
//
// EACH VARIABLE ON ITS OWN. A path is reached through the variable that points
// at its directory, so another variable's directory excuses nothing (`{CODEX_HOME:
// ., CODEX_EXTRA: .codex}` left the login where only CODEX_HOME was read), and
// an ANCESTOR excuses nothing either: the CLI reads `$VAR/auth.json`, so with
// CODEX_HOME at `.` it reads `auth.json` while the login sits at
// `.codex/auth.json`, under the new directory and unreached. A path still under
// the old directory is reached only where the new directory lies strictly
// inside the old one and the path inside the new.
//
// The CLI finds those paths through the variable ([Profile.KeptUnder]), so a
// moved directory with the paths left where they were loaded and was admitted
// by every write path while every call ran without them:
// `work_config_env: {OPENCODE_CONFIG_DIR: .elsewhere}` left the deny-all
// policy and the seat's agent file in a `.crewlet-config` OpenCode no longer
// read — "Agent not found" on every run, and with the agent file alone moved,
// a model handed OpenCode's default tools — and `config_env: {CODEX_HOME:
// .codex-elsewhere}` left the login and every session path behind, so no seat
// was signed in and no session was ever pruned.
//
// Judged against the BUILT-IN, and only for a directory it relocates, because
// the built-in's own layout is the one statement of which paths the CLI reads
// through which variable: a path a `custom` profile keeps beside its relocation
// may be one its CLI reads from a fixed place, and nothing here can say
// otherwise. A variable whose new value is itself refused
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
			// An override merges into the built-in's map key by key, so the
			// variable is still there; empty (a YAML null) or escaping its
			// root, it is refused on its own. The same directory, however
			// spelled, strands nothing.
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
				"where the CLI found them through %s, which it no longer reads them from — "+
				"re-root them from %s to %s in the same override, each at the same place "+
				"under the new directory%s", r.field, name, was, now,
				strings.Join(left, ", "), was, was, now, example))
		}
	}
	return bad
}

// validateEventMarkers holds a [MarkerScopeEvents] profile to the shape in
// which a sentinel can only be the CLI's own: a jsonl stream, a discriminator
// that tells its events apart, the events that report a failure, and where in
// them the class is.
func (p *Profile) validateEventMarkers(add func(string, ...any)) {
	if p.EffectiveOutput() != OutputJSONL {
		// On a text profile stdout IS the model's reply, and a json one is a
		// single document with no events to tell apart.
		add("marker_scope is %s but output is %q — a failure event exists only in a "+
			"jsonl stream; use %s for a CLI that reports a failure as its answer",
			MarkerScopeEvents, p.EffectiveOutput(), MarkerScopeAnswerAndStderr)
	}
	if len(p.EventTypePath) == 0 {
		add("marker_scope is %s but event_type_path is empty — without the "+
			"discriminator no line can be told to be the CLI's failure event rather than "+
			"one carrying the model's words", MarkerScopeEvents)
	}
	if len(p.MarkerEvents) == 0 {
		add("marker_scope is %s but marker_events is empty — name the event types the "+
			`CLI reports a failure in, e.g. ["error"]`, MarkerScopeEvents)
	}
	if len(p.MarkerPath) == 0 {
		add("marker_scope is %s but marker_path is empty — say where in that event the "+
			`failure class is, e.g. ["error", "type"]`, MarkerScopeEvents)
	}
	for _, kind := range p.MarkerEvents {
		if slices.Contains(p.TextEvents, kind) {
			// An event that carries the answer carries the model's words, and
			// matching a sentinel in it is matching what the model wrote.
			add("marker_events and text_events both name %q — an event that carries "+
				"the model's answer is not the CLI's failure report", kind)
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
	if len(p.LimitEvents) == 0 {
		return
	}
	if p.EffectiveOutput() != OutputJSONL || len(p.EventTypePath) == 0 {
		add("limit_events needs output: %s and an event_type_path: on any other "+
			"output a line of the model's answer is a line of stdout, and it could spell "+
			"the report out", OutputJSONL)
	}
	for i, e := range p.LimitEvents {
		what := fmt.Sprintf("limit_events[%d]", i)
		discriminated := slices.ContainsFunc(e.Match, func(m Match) bool {
			return slices.Equal(m.Path, p.EventTypePath)
		})
		if !discriminated || len(e.Match) < 2 {
			add("%s.match must name the event type at event_type_path and at least one "+
				"field the CLI writes beside it, e.g. an error flag or a status code", what)
		}
		if e.Reset.Separator != "" && len(e.SaidPath) == 0 {
			add("%s.reset_separator is set with no said_path: the reset is read from "+
				"the sentence at said_path", what)
		}
		e.Reset.validate(what, add)
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
		// Without the id the command can only ask about SOME session, and a
		// seat's home holds every call running in it at once.
		add("usage_args carries no {session} placeholder — the report has to be " +
			"about this call's session, and a seat's home holds every call running in it")
	case !hasPlaceholder(p.SessionArgs, "{session}"):
		add("usage_args asks about the call's session but session_args does not name " +
			"one — declare session_args with {session}, or the CLI picks an id nobody " +
			"can ask about")
	}
	if len(p.UsageFileArgs) > 0 {
		// Two reports of one call, and whichever was read second would
		// silently replace the first.
		add("usage_args and usage_file_args are both set — one call has one usage " +
			"report; declare the one this CLI writes")
	}
	// The same rule, for the same reason, as a usage file's: both prompt
	// counts or the report is never read.
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
		// One shape or the other. Both would hand the CLI the same
		// system prompt twice, and which copy wins is the vendor's
		// business rather than something this profile can state.
		add("%s.args and %s.env are both set — a CLI takes its system prompt on ONE "+
			"channel; drop whichever this build does not use", f, f)
	}
	// AND ONE PLACEHOLDER OR THE OTHER WITHIN args, which is the same rule one
	// level down and was the hole: the renderer writes the private file for
	// `{file}` and then substitutes `{system}` into argv on the SAME pass, so
	// `["--agent-file", "{file}", "--system-prompt", "{system}"]` wrote the
	// seat's identity to a 0600 file and ALSO put every byte of it in
	// /proc/<pid>/cmdline, where any account on the machine reads it. The
	// shipped profiles are held to this by a test; nothing held an operator's
	// overrides to it, and overrides are exactly where a hand-written argv
	// appears.
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
		// A template with no file to write is not a harmless extra: on a
		// `{system}` channel the seat's identity goes on argv bare, and an
		// operator who wrote frontmatter meant it to reach the CLI.
		add("%s.file is set but the channel writes no file — give %s.args a {file} "+
			"placeholder, set %s.env, or make the file discovered", f, f, f)
	}
	if tpl := ch.File.Template; tpl != "" && !strings.Contains(tpl, "{system}") {
		add("%s.file.template has no {system} placeholder — every call would hand "+
			"the CLI the same fixed file and no seat's identity", f)
	}
	switch name := ch.File.Name; {
	case ch.File.Found():
		ch.validateDiscovered(add)
	case name != "" && !isBareFileName(name):
		// The path is joined onto the per-call working directory, and
		// this field is operator-overridable.
		add("%s.file.name %q must be a plain file name, with no directory separator "+
			"— only a discovered file, which the CLI finds where its vendor says to "+
			"look, carries a directory", f, name)
	}
}

// validateDiscovered holds a discovered file to the shape that makes it one: a
// file the CLI finds in the working directory, and an argv that selects it —
// or none at all, for a file the CLI reads on every run.
func (ch namedChannel) validateDiscovered(add func(string, ...any)) {
	f, name := ch.field, ch.File.Name
	switch {
	case name == "":
		// The default name is a bare prompt file no vendor looks for, so a
		// discovered channel without a name of its own writes a file
		// nothing will ever find.
		add("%s.file is discovered but its name is empty — say where the CLI looks "+
			"for it, e.g. .crewlet-config/agents/crewlet-seat.md", f)
	case !staysInside(name):
		add("%s.file.name %q must be relative and stay inside the per-call working "+
			"directory", f, name)
	case !namesAFile(name):
		// The prompt is written to exactly this path, so a name that
		// cleans to a DIRECTORY fails every call: `.` and `x/..` are the
		// working directory itself (a write onto it is EISDIR), and
		// `.opencode/agents/` writes a regular FILE where the CLI looks
		// for a directory of agents, so it finds none. The bare-name rule
		// beside this one refuses `.` and `..` for the same reason.
		add("%s.file.name %q must name a FILE inside the per-call working directory "+
			"— not the directory itself, not a path ending in a separator, and not one "+
			"whose last element is . or .. — e.g. .crewlet-config/agents/crewlet-seat.md", f, name)
	}
	switch {
	case ch.File.Discovered == DiscoverySelected && len(ch.Args) == 0:
		// Found but never selected, the file changes nothing: the CLI runs
		// its own default agent, and the seat's identity is in neither
		// place.
		add("%s.file is discovered: selected but %s.args is empty — the CLI finds the "+
			"file, and nothing tells it to use what it found; select it, or make it "+
			"discovered: always if the CLI reads it on every run", f, f)
	case hasPlaceholder(ch.Args, "{file}") || hasPlaceholder(ch.Args, "{system}"):
		// A discovered file is found by where it is. Handing over its
		// path too is two channels for one prompt, and {system} would put
		// the text on argv beside the file.
		add("%s.file is discovered but %s.args carries {file} or {system} — a "+
			"discovered file is found by its location, and the argv only selects it", f, f)
	}
	if ch.Env != "" {
		add("%s.file is discovered but %s.env is set — the variable hands the CLI a "+
			"path, and a discovered file needs none", f, f)
	}
}

// ValidateCredentials reports every place this profile routes a credential
// around auth: a passthrough_env or env entry whose name says it carries a
// secret, and an env entry naming the profile's own token_env or api_key_env.
//
// APART FROM [Profile.Validate], which [Load] runs, because these are
// ADMISSION rules and that one is RUNNABLE. A profile that breaks one still
// builds, starts and answers: what it gets wrong is where a credential comes
// from. Config refuses a document that breaks one on every write and only
// warns about a revision being applied, because an apply that refused it would
// take a node off the fleet's epoch over a revision that runs — and the rule
// set here is the one a later build is free to change, as this predicate
// itself changed from substrings to whole name components.
func (p *Profile) ValidateCredentials(name string) error {
	var bad []string
	add := func(format string, args ...any) {
		bad = append(bad, fmt.Sprintf(format, args...))
	}
	for _, env := range p.PassthroughEnv {
		if IsCredentialName(env) {
			// Refused rather than dropped: an operator who wrote it
			// meant it to arrive, and a variable that vanished
			// silently is a debugging session.
			add("passthrough_env names %q, which looks like a credential — "+
				"passthrough is forwarded before auth.mode is consulted, so it would "+
				"reach every seat whatever the mode says; use auth.mode api-key or "+
				"inherit-env instead", env)
		}
	}
	for _, env := range slices.Sorted(maps.Keys(p.IsolationEnv)) {
		// A switch is forwarded on every command whatever auth.mode says,
		// and is no place for a value anybody would have to keep secret.
		if env != "" && (env == p.TokenEnv || env == p.APIKeyEnv || IsCredentialName(env)) {
			add("isolation_env names %q, which is a credential variable — a switch the "+
				"engine sets on every command is forwarded whatever auth.mode says and is "+
				"stored unredacted; give the credential through the entry's auth or its "+
				"own env", env)
		}
	}
	for _, env := range slices.Sorted(maps.Keys(p.SubscriptionEnv)) {
		// Set under subscription alone, which removes the key variable and
		// sets the token's from auth: a credential here would sign the CLI
		// in with a value no auth field names, stored unredacted.
		if env != "" && (env == p.TokenEnv || env == p.APIKeyEnv || IsCredentialName(env)) {
			add("subscription_env names %q, which is a credential variable — it selects "+
				"the login a subscription call signs in with and carries no secret; give "+
				"a credential through the entry's auth", env)
		}
	}
	if p.MCP != nil {
		for _, env := range slices.Sorted(maps.Keys(p.MCP.Env)) {
			if p.MCP.mintedByServer(p.MCP.Env[env]) {
				// A VALUE THE CLI'S OWN SERVER MINTS — the password
				// OpenCode's `serve` prints as it starts — rendered into
				// each run and never written anywhere: the profile holds
				// a placeholder, which is no secret, and the rule below
				// exists for a secret the profile would hold.
				continue
			}
			// Set on every run with tools whatever auth.mode says, and
			// stored unredacted like the rest of overrides.
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
			// Named apart from the shape rule below because an
			// overridden token_env or api_key_env need not LOOK like a
			// credential. The entry's env and then its auth are layered over
			// this one, so whether a value here reaches the CLI turns on
			// the mode and on whether the secret store or the engine's
			// environment holds a credential of its own — which no
			// document can see.
			add("env names %q, which is this profile's token_env or api_key_env — "+
				"auth owns that variable, and whether a value here reaches the CLI "+
				"depends on auth.mode and on whether the secret store or the engine's "+
				"environment holds one; give the credential through auth "+
				"(auth.token, or auth.api_key with auth.mode api-key)", env)
		case IsCredentialName(env):
			// The same reason passthrough_env refuses one, and a second:
			// the profile's env is forwarded whatever auth.mode says,
			// and overrides is neither ${VAR}-resolved nor marked
			// secret, so a key written here sits in the stored revision
			// in plain text and is shown unredacted on every read.
			add("env names %q, which looks like a credential — the profile's env "+
				"is forwarded whatever auth.mode says, and overrides is stored "+
				"and shown unredacted; put it in the entry's own env, which is "+
				"${VAR}-resolved and redacted, or give it through its auth", env)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("cli-agent profile %q: %s", name, strings.Join(bad, "; "))
}

// HoldsLogin reports whether the credential file named name, holding content,
// is a login: one [Profile.LoginKeys] does not name, or one whose top-level
// key it names is set to something other than null or an empty string, object
// or array.
//
// Read as JSON once every line opening with `//` is set aside, because the
// file Copilot CLI keeps its plaintext login in opens with two such lines. A
// file that does not read that way IS taken for a login: guessing harder would
// mean parsing a vendor's format here, and a wrong guess in the other
// direction is a login the engine would not carry, export or keep.
func (p *Profile) HoldsLogin(name string, content []byte) bool {
	key, named := p.LoginKeys[name]
	if !named {
		return true
	}
	var lines []string
	for line := range strings.Lines(string(content)) {
		if !strings.HasPrefix(strings.TrimSpace(line), "//") {
			lines = append(lines, line)
		}
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.Join(lines, "")), &doc); err != nil {
		return true
	}
	var value any
	if err := json.Unmarshal(doc[key], &value); err != nil {
		return false
	}
	switch v := value.(type) {
	case nil:
		return false
	case string:
		return v != ""
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	}
	return true
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
// A SENTINEL IS THE VENDOR'S WORDS. It is matched as a plain substring against
// stderr and a failed run's output, and against the opening of a healthy
// run's answer — which is the model's own ([MarkerScope]) — so a sentinel that
// can occur inside ordinary text does not recognise a spent plan, it
// misclassifies arbitrary replies as one. And the cost is not a wrong log
// line: both kinds a marker produces, KindRateLimit and KindAuth, hand the
// seat to the fallback chain, and a rate limit SITS THE ENTRY OUT for a while
// besides (internal/providers/llm/harness, bench.go). A company degrades
// quietly.
//
// The shipped profiles carried `sentinel: "429"`, which is the failure in its
// purest form: three digits matched inside free text, so a model quoting an
// HTTP status, a stack trace's line number, a token count, or any of the ten-
// digit epochs this very package handles would bench a working subscription.
//
// The rule is "it has to contain a letter", and deliberately not also a
// minimum length. A number is not a sentence, which is derivable from what a
// sentinel IS; a length floor would be a constant nobody can defend, and it
// would refuse a legitimate short vendor string on a guess. See [LimitMarker].
func sentinelProblem(sentinel string) string {
	if sentinel == "" {
		return "is empty"
	}
	if !strings.ContainsFunc(sentinel, unicode.IsLetter) {
		return fmt.Sprintf("%q has no letters — a sentinel is the vendor's own wording, "+
			"matched as a substring against whatever the CLI printed, so one made only of "+
			"digits or punctuation matches ordinary replies and benches a working "+
			"credential. Use the sentence the CLI actually prints", sentinel)
	}
	return ""
}

// ReadsUsage reports whether this profile can take token counts from the CLI
// rather than estimating them.
//
// BOTH HALVES, because either alone is a lie. A profile with no usage paths
// obviously cannot; less obviously, a TEXT profile cannot either — [extract]
// never decodes a document in that mode, so declared paths are walked by
// nothing. That combination is not an operator error to refuse: `output: text`
// is a one-line override, and the JSON paths it inherits from the built-in
// profile are simply inert afterwards.
//
// A USAGE REPORT IS THE EXCEPTION TO THE TEXT RULE, and it is the whole reason
// those channels exist: a report the CLI writes to a path of its own, or prints
// to a usage command, is decoded whatever stdout carries, so a `text` profile
// declaring [Profile.UsageFileArgs] or [Profile.UsageArgs] reports real
// counts. Reading the text rule first would answer "estimated" for exactly the
// profile that went to the trouble.
//
// It exists so `crewlet llm doctor` and the extractor answer the same
// question. Doctor asked a narrower one — "are usage paths declared" — and so
// reported "reported by CLI" for a provider whose every call estimates, which
// is precisely the question that report exists to settle.
func (p *Profile) ReadsUsage() bool {
	if len(p.Usage.Input) == 0 && len(p.Usage.Output) == 0 {
		return false
	}
	if p.ReadsUsageReport() {
		return true
	}
	return p.EffectiveOutput() != OutputText
}

// LoginLocation says where this CLI keeps a login, for a message telling an
// operator to put one there: its credential files, the database a
// [CredentialStore] reads and writes, or both.
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

// KeepsLogin reports whether the CLI keeps a login of its own that the engine
// can hold for it: credential files, or a login in its own database. A profile
// that keeps none signs in only through what a call's environment carries — its
// token, its key, or a model provider's key ([Profile.EnvSignIn]).
func (p *Profile) KeepsLogin() bool {
	return len(p.CredentialPaths) > 0 || p.CredentialStore != nil
}

// ReadsUsageReport reports whether this profile takes its counts from a report
// apart from stdout — a usage file the CLI writes, or a usage command run after
// the call — rather than from what the call printed.
func (p *Profile) ReadsUsageReport() bool {
	return len(p.UsageFileArgs) > 0 || len(p.UsageArgs) > 0
}

// UnderRoot joins a profile-declared relative path onto a root and refuses
// one that escapes it.
//
// Profile paths are operator-overridable, so "../../.ssh" is reachable from
// config — and the backend's prune calls RemoveAll on whatever this returns.
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
