package cliprofile

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
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
	// seat's identity, the tool catalogue, the conversation and its tool
	// results — so it is the LARGEST and the most sensitive thing this
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
type Path []string

// String renders a path the way profiles.yaml would be read aloud —
// `usage.input_tokens` for [][]string{{"usage", "input_tokens"}}.
//
// It exists for the message an operator reads when a profile stops matching
// its CLI: naming the field to change is the whole point of that message, and
// `[]cliagent.Path{{"result"}}` printed with %v names nothing.
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
type UsagePaths struct {
	Input      []Path `yaml:"input,omitempty"`
	Output     []Path `yaml:"output,omitempty"`
	CacheRead  []Path `yaml:"cache_read,omitempty"`
	CacheWrite []Path `yaml:"cache_write,omitempty"`
	Reasoning  []Path `yaml:"reasoning,omitempty"`
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
// — and replacing the document when that changed. The database itself is then
// volatile like any other session store. Measured against OpenCode 2.0.32: an
// import followed by an export reproduces the document byte for byte, and an
// import skips a credential id the database already holds rather than updating
// it, which is why a refresh comes back as a new document and never as an import
// into an older database.
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
// that carries the reset instant. A marker that finds its sentinel but no
// reset value still classifies; one that finds neither does not fire.
type LimitMarker struct {
	// Sentinel is the exact substring the vendor emits. It is compared
	// case-sensitively against the extracted text: the vendors write these
	// as fixed strings, and folding case is what turns a sentinel back
	// into a keyword. In the [MarkerScopeEvents] scope it is the failure
	// CLASS instead, compared whole and case-sensitively with the string at
	// [Profile.MarkerPath] in the CLI's own failure event.
	Sentinel string `yaml:"sentinel,omitempty"`

	// ResetSeparator splits the reset value off the sentinel line. Claude
	// Code emits "Claude AI usage limit reached|1719849600" — a pipe, then
	// a Unix epoch — so the retry-after is a datum rather than a guess.
	ResetSeparator string `yaml:"reset_separator,omitempty"`

	// ResetUnit is how to read the value after the separator: "epoch" for
	// Unix seconds, "seconds" for a delta.
	ResetUnit string `yaml:"reset_unit,omitempty"`
}

// MarkerScope says WHERE a CLI's failure prose can appear, and therefore
// where [LimitMarker] and [AuthMarker] sentinels may be matched.
//
// The default searches the model's ANSWER as well as stderr, and it has to:
// Claude Code reports a spent plan on a ZERO exit with the vendor's sentence
// about the plan standing where the answer should be, so a marker that
// searched stderr alone would never fire and the fallback chain would never
// carry the seat onto a metered key.
//
// It is also a false-positive surface, and a real one rather than a
// theoretical one: the haystack is the model's own words, so a seat ASKED
// about rate limits — "what is our quota?" — can answer in prose that trips a
// generic sentinel, and a KindRateLimit benches a perfectly good credential.
// This file already lost a "429" sentinel to exactly that.
//
// So a CLI that reports its failures on STDERR and nowhere else says so, and
// its markers stop being matched against anything the model said. That is not
// a guess per profile: kimi-code writes `provider.rate_limit: …` to stderr
// with the answer stream empty, pi writes `<status>: <the provider's JSON>`
// to stderr with stdout empty, and hermes writes `hermes -z: agent failed: …`
// there — all three measured. A profile only narrows this when the vendor's
// own behaviour makes the answer an impossible place for the report.
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
// In text mode the engine uses none of them: Crewlet's tools ride the prompt
// envelope, and a CLI editing files in its own sandbox is invisible to the
// tool registry, the permission model, redaction and the event stream. The
// shell is the one that matters: hostbox isolates a seat's HOME and
// environment, not the filesystem, so a CLI with a shell on the engine host
// reads whatever the engine user can read. A profile therefore DENIES local
// tools wherever the vendor offers a way to, and says so here, so that
// `crewlet llm doctor` can print the claim next to what its probe measured.
//
// Web is the deliberate exception and is never denied: a seat on a
// subscription must not have less reach than the same CLI at a terminal, and
// a fetch is a read — it never gates delivery.
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
// Gemini's settings.json, OpenCode's opencode.json, Cursor's cli.json — and
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

// SystemPromptFile shapes the file a `{file}` system-prompt channel writes,
// for the vendors whose prompt channel takes a STRUCTURED file rather than a
// bare one.
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
// where this backend's tool denial lives for it — and that is why a profile
// declaring one passes the channel on EVERY call, including a request that
// carries no system prompt at all (`crewlet llm doctor`'s isolation probes
// are two). Passing it only when there is text to put in it would hand the
// vendor's default agent, and every tool it has, to exactly the calls that
// exist to prove the tools are off.
//
// OpenCode 2 is the second, and it takes the file a different way: it is
// handed no path at all. It DISCOVERS agents by their location — `agents/<name>.md`
// in its configuration directory, with the body as the agent's system prompt —
// and `--agent <name>` selects one. Its profile points that directory inside
// the per-call working directory ([Profile.WorkConfigEnv]), so the file is
// written there and the channel's argv names the agent rather than carrying
// `{file}`: see [SystemPromptFile.Discovered].
type SystemPromptFile struct {
	// Name is the file to write in the per-call working directory. Empty
	// takes the default. A bare name for a file whose path the CLI is
	// handed: a vendor that keys on the extension (`--agent-file` wants
	// `.md`) is the only reason to set it. A relative path naming a FILE,
	// staying inside the working directory, for a
	// [SystemPromptFile.Discovered] one, which the CLI finds where its
	// vendor says to look.
	Name string `yaml:"name,omitempty"`

	// Template is the file's content, with `{system}` substituted by the
	// seat's system prompt. Empty writes the prompt alone.
	Template string `yaml:"template,omitempty"`

	// Discovered says the CLI FINDS the file by its location in the working
	// directory rather than being handed its path, so system_prompt_args
	// select what it found and carry no `{file}`. Written on every call, as
	// a `{file}` one is, and removed with the working directory on release.
	Discovered bool `yaml:"discovered,omitempty"`
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
// prose on stderr, and the chain needs AUTH rather than the FATAL an
// unrecognised failure would get, so that a role keeps working off its
// metered fallback while the operator re-runs `crewlet llm login`.
type AuthMarker struct {
	// Sentinel is matched exactly as a [LimitMarker]'s is, in the same scope.
	Sentinel string `yaml:"sentinel,omitempty"`
}

// Profile is everything the engine needs to drive one coding CLI.
//
// It is DATA, not code, and every field is replaceable from YAML
// (`cli.overrides`), because these flags and JSON shapes belong to vendors
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

	// SystemPromptArgs carries the system prompt on its OWN channel rather
	// than as the first section of the transcript, where a CLI has a flag
	// for it. Empty leaves it in the transcript, which is what a CLI with
	// no such flag can take.
	//
	// A system prompt folded into the prompt text arrives as USER content:
	// the model is asked to treat an ordinary message as its standing
	// instructions, and a vendor whose default prompt says what it is
	// ("I'm Claude Code, here to help with your software engineering
	// tasks") keeps saying so over the top of a seat's own identity.
	//
	// Two placeholders, and the difference is not cosmetic:
	//
	//   {file}    the text is written to a private file in the per-call
	//             working directory and the PATH is substituted. Prefer
	//             this always. A seat's system prompt carries the org
	//             chart, its policies, its backstory, its roster and its
	//             personal-memory and knowledge prefetches.
	//   {system}  the text is substituted INTO ARGV, where /proc/<pid>/cmdline
	//             makes it readable by every account on the machine and
	//             ARG_MAX bounds it (256 KB on macOS) — the same limit the
	//             copilot profile's argv prompt already lives under. Only
	//             for a CLI that offers no file variant.
	SystemPromptArgs []string `yaml:"system_prompt_args,omitempty"`

	// SystemPromptEnv is the THIRD channel, and the one `--help` does not
	// show: an environment variable naming a file the CLI reads its system
	// prompt from. Empty means the CLI has no such variable.
	//
	// Gemini CLI and its Qwen fork both work this way and neither has a
	// flag for a path — `GEMINI_SYSTEM_MD` / `QWEN_SYSTEM_MD`, each taking
	// a path and REPLACING the built-in prompt. Reading only `--help`
	// reports both as having no system-prompt channel at all, which is how
	// they were first recorded here.
	//
	// PREFERRED OVER SystemPromptArgs WHEREVER BOTH EXIST, because it is a
	// file: the text never reaches argv, where /proc/<pid>/cmdline makes it
	// readable by every account on the machine. Qwen Code has both and
	// takes this one for exactly that reason.
	//
	// The two are mutually exclusive — a profile declaring both would hand
	// the CLI its system prompt twice — and [Profile.Validate] refuses it.
	SystemPromptEnv string `yaml:"system_prompt_env,omitempty"`

	// SystemPromptFile shapes the file either `{file}` channel writes —
	// see [SystemPromptFile]. Nil writes the prompt alone, which is what
	// every vendor but Kimi Code takes.
	SystemPromptFile *SystemPromptFile `yaml:"system_prompt_file,omitempty"`

	// PromptArgs introduces the prompt in argv mode, for a CLI that takes it
	// as a FLAG'S VALUE rather than as a positional argument. Empty appends
	// the prompt bare, which is what every other argv profile wants.
	//
	// In [PromptFile] mode it carries the flag AND its `{file}`
	// placeholder — `["--prompt-file", "{file}"]` — and is required, since
	// a file-mode profile with nothing to substitute the path into would
	// hand the CLI no prompt at all.
	//
	// It exists because xAI's grok breaks the assumption the rest of this
	// format is built on. Its only headless trigger is `-p <PROMPT>`, and a
	// value is REQUIRED — so with `-p` sitting in complete_args, the model
	// flag that follows becomes the prompt and the real prompt becomes a
	// stray positional: `grok -p --model grok-4 "hi"` exits on
	// "a value is required for '--single <PROMPT>' but none was supplied".
	// Every call died at argument parsing.
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
	// `cli_agent_usage_report_unreadable`.
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
	// resolve.
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
	// cli.state_dir was put in) handed every seat a shell, and a plugin there
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
	// cli.env, are all layered OVER the isolation, and each is refused naming
	// one of [Profile.IsolationVariables] — which an ordinary env entry is not.
	// Written in env, OpenCode's project switch was replaced by
	// `cli.env: {OPENCODE_CONFIG_PROJECT_DISABLE: "0"}` — or by an EMPTY value,
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

	// EnvSignIn declares that the CLI reads its model provider's credential
	// from its OWN environment, under that provider's own variable — so a
	// credential-named variable the operator sets in `cli.env` signs it in.
	// False, the default, means a credential in cli.env is not a sign-in
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
	// And a declaration at all, rather than counting any credential in
	// cli.env, because only the profile knows whether the CLI reads one:
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

	// LimitMarkers recognise a spent subscription.
	LimitMarkers []LimitMarker `yaml:"limit_markers,omitempty"`

	// AuthMarkers recognise an expired login.
	AuthMarkers []AuthMarker `yaml:"auth_markers,omitempty"`

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

	// SeedFiles are the settings files written for the CLI before a call
	// — the vendors that take their tool policy from a file rather than a
	// flag. See [SeedFile].
	SeedFiles []SeedFile `yaml:"seed_files,omitempty"`
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
// passthrough_env and env entries are refused, which cli.env entries sign a
// CLI in, and which travel into a coding box — so the answers cannot drift.
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

// ModelArgument is the model as a text call hands it to the CLI: the VALUE of
// the model_args element carrying {model}, rendered with model — so an
// override such as ["--model", "openrouter/{model}"] is part of it. The model
// itself when no element carries the placeholder, which config and the
// backend both refuse beside a model ([Profile.TakesModel]).
//
// ONE RENDERING FOR EVERY PLACE THE MODEL GOES, because a cli-agent entry's
// `model` is written in its CLI's own grammar and every reader of it has to
// see the same string: a text call's argv, an agent-mode or code-sandbox run
// of the same CLI in a box, and the config rule that judges the value as that
// CLI reads it. A run that rebuilt the value its own way named one model to
// the box and another to every text call on the same entry.
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
// own env and passthrough_env ([Profile.Validate]) and an entry's cli.env
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
// it, which is what an operator overrides it with (cli.overrides.<field>).
const (
	// IsolationHome is [HomeIsolation]: set by the engine for every CLI,
	// whatever its profile says, and changed by no field at all.
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
// THE LAYER THE CHILD IS GIVEN, which is the last one applied: its environment
// sets [HomeIsolation], then config_env, then work_config_env, then
// isolation_env, so a variable two of them name is the later one's — and the
// one overlap [Profile.Validate] admits, config_env relocating an XDG home, is
// config_env's.
//
// For the refusals that have to say WHERE a variable's value is changed,
// rather than a second copy of the lists beside each of them: an entry's
// cli.env naming OPENCODE_CONFIG_DIR used to be sent to
// cli.overrides.config_env, which [Profile.Validate] refuses for that very
// variable as work_config_env's, and naming OPENCODE_CONFIG_PROJECT_DISABLE
// to the same field, refused there as an isolation_env switch.
func (p *Profile) IsolationLayerOf(name string) (IsolationLayer, bool) {
	if _, ok := p.IsolationEnv[name]; ok {
		return IsolationSwitch, true
	}
	if _, ok := p.WorkConfigEnv[name]; ok {
		return IsolationWorkConfigEnv, true
	}
	if _, ok := p.ConfigEnv[name]; ok {
		return IsolationConfigEnv, true
	}
	if slices.Contains(HomeIsolation, name) {
		return IsolationHome, true
	}
	return "", false
}

// HasSystemChannel reports whether this profile carries the system prompt on
// a channel of its own rather than leaving it in the transcript.
func (p *Profile) HasSystemChannel() bool {
	return len(p.SystemPromptArgs) > 0 || p.SystemPromptEnv != ""
}

// WritesSystemPromptFile reports whether this profile's system-prompt channel
// puts the text in a FILE — `{file}` on argv, the env-var channel, which is a
// path by construction, or a file the CLI discovers by its location.
func (p *Profile) WritesSystemPromptFile() bool {
	if p.SystemPromptEnv != "" {
		return true
	}
	if p.SystemPromptFile != nil && p.SystemPromptFile.Discovered {
		return true
	}
	return hasPlaceholder(p.SystemPromptArgs, "{file}")
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
	var bad []string
	add := func(format string, args ...any) {
		bad = append(bad, fmt.Sprintf(format, args...))
	}

	if strings.TrimSpace(p.Binary) == "" {
		add("binary is empty — set cli.overrides.binary")
	}
	if len(p.CompleteArgs) == 0 && p.EffectivePromptMode() == PromptStdin {
		// Only stdin mode can end up with NO argv at all. The other two
		// build one from prompt_args and the prompt itself, so a CLI
		// invoked as `mycli --prompt-file <path>` and nothing else is a
		// legitimate shape rather than a profile that forgot to say how
		// to run its binary.
		add("complete_args is empty — set cli.overrides.complete_args")
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
			`cli.overrides.prompt_args, e.g. ["--prompt-file", "{file}"]`)
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
	if len(p.SystemPromptArgs) > 0 && p.SystemPromptEnv != "" {
		// One channel or the other. Both would hand the CLI the same
		// system prompt twice, and which copy wins is the vendor's
		// business rather than something this profile can state.
		add("system_prompt_args and system_prompt_env are both set — a CLI takes " +
			"its system prompt on ONE channel; drop whichever this build does not use")
	}
	// AND ONE PLACEHOLDER OR THE OTHER WITHIN system_prompt_args, which is
	// the same rule one level down and was the hole: the renderer writes the
	// private file for `{file}` and then substitutes `{system}` into argv on
	// the SAME pass, so `["--agent-file", "{file}", "--system-prompt",
	// "{system}"]` wrote the seat's identity to a 0600 file and ALSO put
	// every byte of it in /proc/<pid>/cmdline, where any account on the
	// machine reads it. The shipped profiles are held to this by a test;
	// nothing held an operator's cli.overrides to it, and overrides are
	// exactly where a hand-written argv appears.
	if hasPlaceholder(p.SystemPromptArgs, "{file}") && hasPlaceholder(p.SystemPromptArgs, "{system}") {
		add("system_prompt_args names both {file} and {system} — the first writes the " +
			"seat's system prompt to a private file and the second puts the same text " +
			"on argv, where /proc/<pid>/cmdline exposes it to every account on the " +
			"machine; keep the {file} form and drop {system}")
	}
	if p.SystemPromptFile != nil {
		if !p.WritesSystemPromptFile() {
			// A template with no file to write is not a harmless
			// extra: on a `{system}` profile the seat's identity goes
			// on argv bare, and an operator who wrote frontmatter
			// meant it to reach the CLI.
			add("system_prompt_file is set but no system-prompt channel writes a " +
				"file — set system_prompt_args with a {file} placeholder, or " +
				"system_prompt_env")
		}
		if tpl := p.SystemPromptFile.Template; tpl != "" && !strings.Contains(tpl, "{system}") {
			add("system_prompt_file.template has no {system} placeholder — every " +
				"call would hand the CLI the same fixed file and no seat's identity")
		}
		// `fileName` rather than `name`, which is this function's own
		// parameter: the PROFILE's name. Two different names in one
		// validator whose every message is about the profile is worth a
		// second word.
		fileName := p.SystemPromptFile.Name
		switch {
		case p.SystemPromptFile.Discovered:
			p.validateDiscoveredPromptFile(add)
		case fileName != "" && !isBareFileName(fileName):
			// The path is joined onto the per-call working directory,
			// and this field is operator-overridable.
			add("system_prompt_file.name %q must be a plain file name, with no "+
				"directory separator — only a discovered file, which the CLI finds "+
				"where its vendor says to look, carries a directory", fileName)
		}
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
		if m.ResetSeparator != "" && m.ResetUnit != "epoch" && m.ResetUnit != "seconds" {
			add("limit_markers[%d].reset_unit %q (want epoch or seconds)", i, m.ResetUnit)
		}
	}
	// Checked at all, which it was not: an auth marker fires KindAuth, and
	// KindAuth exhausts the credential exactly as a spent plan does — so an
	// unusable sentinel costs the same here as it does above.
	for i, m := range p.AuthMarkers {
		if problem := sentinelProblem(m.Sentinel); problem != "" {
			add("auth_markers[%d].sentinel %s", i, problem)
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
	if p.LocalTools != "" && !p.LocalTools.Valid() {
		add("local_tools %q (want denied or vendor-default)", p.LocalTools)
	}
	if p.LocalTools == LocalToolsVendorDefault && strings.TrimSpace(p.LocalToolsNote) == "" {
		// A stance that admits the CLI's tools without saying why is
		// exactly the silent hole the field exists to make visible.
		add("local_tools is vendor-default but local_tools_note is empty — say which " +
			"denial the vendor lacks")
	}
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
			// under it fails ENOTDIR on every call. The rule
			// system_prompt_file.name is held to, for the same reason.
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
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("cli-agent profile %q: %s", name, strings.Join(bad, "; "))
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
		case name == "HOME":
			add("config_env may not name HOME — it is set from the seat home already")
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
		if m.ResetSeparator != "" {
			add("limit_markers[%d].reset_separator is set but marker_scope is %s — the "+
				"sentinel is compared with the whole failure class, so there is no text "+
				"after it to read a reset from", i, MarkerScopeEvents)
		}
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

// validateDiscoveredPromptFile holds a [SystemPromptFile.Discovered] channel to
// the shape that makes it one: a file the CLI finds in the working directory,
// and an argv that selects it.
func (p *Profile) validateDiscoveredPromptFile(add func(string, ...any)) {
	fileName := p.SystemPromptFile.Name
	switch {
	case fileName == "":
		// The default name is a bare prompt file no vendor looks for, so a
		// discovered channel without a name of its own writes a file nothing
		// will ever find — and the argv then selects an agent that does not
		// exist.
		add("system_prompt_file is discovered but its name is empty — say where the " +
			"CLI looks for it, e.g. .crewlet-config/agents/crewlet-seat.md")
	case !staysInside(fileName):
		add("system_prompt_file.name %q must be relative and stay inside the per-call "+
			"working directory", fileName)
	case !namesAFile(fileName):
		// The prompt is written to exactly this path, so a name that cleans
		// to a DIRECTORY fails every call: `.` and `x/..` are the working
		// directory itself (a write onto it is EISDIR), and
		// `.opencode/agents/` writes a regular FILE where the CLI looks for a
		// directory of agents, so it finds none. The bare-name rule beside
		// this one refuses `.` and `..` for the same reason.
		add("system_prompt_file.name %q must name a FILE inside the per-call working "+
			"directory — not the directory itself, not a path ending in a separator, and "+
			"not one whose last element is . or .. — e.g. "+
			".crewlet-config/agents/crewlet-seat.md", fileName)
	}
	switch {
	case len(p.SystemPromptArgs) == 0:
		// Found but never selected, the file changes nothing: the CLI runs its
		// own default agent, and the seat's identity is in neither place.
		add("system_prompt_file is discovered but system_prompt_args is empty — the " +
			"CLI finds the file, and nothing tells it to use what it found")
	case hasPlaceholder(p.SystemPromptArgs, "{file}") || hasPlaceholder(p.SystemPromptArgs, "{system}"):
		// A discovered file is found by where it is. Handing over its path
		// too is two channels for one prompt, and {system} would put the
		// text on argv beside the file.
		add("system_prompt_file is discovered but system_prompt_args carries {file} or " +
			"{system} — a discovered file is found by its location, and the argv only " +
			"selects it")
	}
	if p.SystemPromptEnv != "" {
		add("system_prompt_file is discovered but system_prompt_env is set — the " +
			"variable hands the CLI a path, and a discovered file needs none")
	}
}

// ValidateCredentials reports every place this profile routes a credential
// around cli.auth: a passthrough_env or env entry whose name says it carries a
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
				"stored unredacted; give the credential through cli.auth or cli.env", env)
		}
	}
	for _, env := range slices.Sorted(maps.Keys(p.Env)) {
		switch {
		case env != "" && (env == p.TokenEnv || env == p.APIKeyEnv):
			// Named apart from the shape rule below because an
			// overridden token_env or api_key_env need not LOOK like a
			// credential. cli.env and then cli.auth are layered over
			// this one, so whether a value here reaches the CLI turns on
			// the mode and on whether the secret store or the engine's
			// environment holds a credential of its own — which no
			// document can see.
			add("env names %q, which is this profile's token_env or api_key_env — "+
				"cli.auth owns that variable, and whether a value here reaches the CLI "+
				"depends on auth.mode and on whether the secret store or the engine's "+
				"environment holds one; give the credential through cli.auth "+
				"(auth.token, or api_keys with auth.mode api-key)", env)
		case IsCredentialName(env):
			// The same reason passthrough_env refuses one, and a second:
			// the profile's env is forwarded whatever auth.mode says,
			// and cli.overrides is neither ${VAR}-resolved nor marked
			// secret, so a key written here sits in the stored revision
			// in plain text and is shown unredacted on every read.
			add("env names %q, which looks like a credential — the profile's env "+
				"is forwarded whatever auth.mode says, and cli.overrides is stored "+
				"and shown unredacted; put it in cli.env, which is ${VAR}-resolved "+
				"and redacted, or give it through cli.auth", env)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("cli-agent profile %q: %s", name, strings.Join(bad, "; "))
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
// whatever the CLI printed — which on a healthy run is the model's own answer
// — so a sentinel that can occur inside ordinary text does not recognise a
// spent plan, it misclassifies arbitrary replies as one. And the cost is not
// a wrong log line: both kinds a marker produces, KindRateLimit and KindAuth,
// BENCH THE CREDENTIAL for a cooldown and hand the seat to the fallback chain
// (see [llm.ErrorKind.ExhaustsCredential]). A company degrades quietly.
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
	var where []string
	if len(p.CredentialPaths) > 0 {
		where = append(where, "its credential files ("+strings.Join(p.CredentialPaths, ", ")+")")
	}
	if p.CredentialStore != nil {
		where = append(where, "the login it keeps in its own database")
	}
	if len(where) == 0 {
		return "a login this profile names no place for"
	}
	return strings.Join(where, " and ")
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
		return "", fmt.Errorf("cli-agent: path %q must be relative to the seat home", rel)
	}
	joined := filepath.Join(root, rel)
	cleanRoot := filepath.Clean(root)
	if joined != cleanRoot && !strings.HasPrefix(joined, cleanRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("cli-agent: path %q escapes the seat home", rel)
	}
	return joined, nil
}
