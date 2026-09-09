package cliagent

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
)

// PromptMode is how a CLI receives the prompt.
type PromptMode string

const (
	// PromptStdin writes the prompt to the child's stdin. The default, and
	// the only mode with no length ceiling.
	PromptStdin PromptMode = "stdin"
	// PromptArgv appends the prompt as the last argument. Bounded by
	// ARG_MAX — around 2 MB on Linux, 256 KB on macOS — so a long
	// transcript on such a CLI fails at exec rather than at the model.
	PromptArgv PromptMode = "argv"
)

// Valid reports whether m is a mode this package knows.
func (m PromptMode) Valid() bool { return m == PromptStdin || m == PromptArgv }

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
// resolves to a number wins; when none does, the count is estimated and
// [Completion.Estimated] says so.
type UsagePaths struct {
	Input      []Path `yaml:"input,omitempty"`
	Output     []Path `yaml:"output,omitempty"`
	CacheRead  []Path `yaml:"cache_read,omitempty"`
	CacheWrite []Path `yaml:"cache_write,omitempty"`
}

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
	// into a keyword.
	Sentinel string `yaml:"sentinel,omitempty"`

	// ResetSeparator splits the reset value off the sentinel line. Claude
	// Code emits "Claude AI usage limit reached|1719849600" — a pipe, then
	// a Unix epoch — so the retry-after is a datum rather than a guess.
	ResetSeparator string `yaml:"reset_separator,omitempty"`

	// ResetUnit is how to read the value after the separator: "epoch" for
	// Unix seconds, "seconds" for a delta.
	ResetUnit string `yaml:"reset_unit,omitempty"`
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

// seedValidationRoot is the stand-in a seed path's shape is checked against
// at load time. Any ordinary directory does; what is proved is that the path
// is relative and climbs out of nothing.
var seedValidationRoot = filepath.Join(string(filepath.Separator), "seat")

// scope is the seed scope with its default applied.
func (f SeedFile) scope() SeedScope {
	if f.In == "" {
		return SeedHome
	}
	return f.In
}

// AuthMarker recognises a login the CLI has stopped honouring.
//
// Same reasoning as [LimitMarker]: an expired OAuth login exits non-zero with
// prose on stderr, and the chain needs AUTH rather than the FATAL an
// unrecognised failure would get, so that a role keeps working off its
// metered fallback while the operator re-runs `crewlet llm login`.
type AuthMarker struct {
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

	// Vendor is the model FAMILY this CLI addresses — anthropic, openai or
	// google.
	//
	// Needed because every cli-agent entry shares one providers.llm type,
	// so a coding agent that resolves "<family>/<model>" against a
	// catalogue would address a Claude subscription's "sonnet" as an
	// OpenAI model. The provider type names the family for an API entry;
	// this names it for a subscription one.
	Vendor string `yaml:"vendor,omitempty"`

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

	// ModelArgs carries the model, with {model} substituted. Empty means
	// the CLI takes no model flag, and an entry naming a model gets a
	// validation error rather than a silently ignored setting.
	ModelArgs []string `yaml:"model_args,omitempty"`

	// PromptMode is stdin (the default) or argv.
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
	// the CLI its system prompt twice — and [Profile.validate] refuses it.
	SystemPromptEnv string `yaml:"system_prompt_env,omitempty"`

	// PromptArgs introduces the prompt in argv mode, for a CLI that takes it
	// as a FLAG'S VALUE rather than as a positional argument. Empty appends
	// the prompt bare, which is what every other argv profile wants.
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

	// ErrorPaths locates a boolean the CLI sets when it failed despite
	// exiting zero.
	ErrorPaths []Path `yaml:"error_paths,omitempty"`

	// Usage locates the token counts.
	Usage UsagePaths `yaml:"usage,omitempty"`

	// ConfigEnv maps a vendor's own relocation variable to a directory
	// under the seat's home. Without it a CLI reads the engine user's real
	// dotfiles and every seat shares one set of sessions.
	ConfigEnv map[string]string `yaml:"config_env,omitempty"`

	// Env is fixed child environment the CLI needs. Never a credential:
	// see [Profile.validate].
	Env map[string]string `yaml:"env,omitempty"`

	// PassthroughEnv names engine environment variables forwarded to the
	// child. It MAY NOT name a credential, and the engine refuses a
	// profile that does — everything here is forwarded before auth.mode is
	// consulted, so a key listed here would reach every seat whatever the
	// mode said, which is exactly the metered-bill-on-a-flat-rate-plan
	// failure auth.mode exists to prevent.
	PassthroughEnv []string `yaml:"passthrough_env,omitempty"`

	// TokenEnv is the variable carrying a long-lived headless
	// subscription token.
	TokenEnv string `yaml:"token_env,omitempty"`

	// APIKeyEnv is the variable carrying a metered API key, set only
	// under auth.mode api-key or inherit-env.
	APIKeyEnv string `yaml:"api_key_env,omitempty"`

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

	// LimitMarkers recognise a spent subscription.
	LimitMarkers []LimitMarker `yaml:"limit_markers,omitempty"`

	// AuthMarkers recognise an expired login.
	AuthMarkers []AuthMarker `yaml:"auth_markers,omitempty"`

	// HostCredentialPaths are where this CLI keeps its login in a human's
	// own home directory, for `crewlet llm login --from-host` to adopt.
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

// credentialish matches an environment variable name that carries a secret.
//
// Substrings rather than an exact list because passthrough_env is
// operator-supplied and the set of vendor key names is open: GOOGLE_API_KEY,
// GH_TOKEN and OPENAI_API_KEY have nothing in common but the shape of the
// name. A false positive costs an operator one explicit override; a false
// negative bills them for a plan they thought was flat-rate.
var credentialish = []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "AUTH"}

// IsCredentialName reports whether name looks like it carries a secret.
func IsCredentialName(name string) bool {
	upper := strings.ToUpper(name)
	for _, frag := range credentialish {
		if strings.Contains(upper, frag) {
			return true
		}
	}
	return false
}

// validate reports what is wrong with a profile, naming the override field an
// operator would edit rather than the Go field they cannot see.
func (p *Profile) validate(name string) error {
	var bad []string
	add := func(format string, args ...any) {
		bad = append(bad, fmt.Sprintf(format, args...))
	}

	if strings.TrimSpace(p.Binary) == "" {
		add("binary is empty — set cli.overrides.binary")
	}
	if len(p.CompleteArgs) == 0 && p.PromptMode != PromptArgv {
		add("complete_args is empty — set cli.overrides.complete_args")
	}
	if p.PromptMode != "" && !p.PromptMode.Valid() {
		add("prompt_mode %q (want stdin or argv)", p.PromptMode)
	}
	if p.Output != "" && !p.Output.Valid() {
		add("output %q (want json, jsonl or text)", p.Output)
	}
	if p.Output != OutputText && len(p.TextPaths) == 0 {
		add("text_paths is empty — a %s profile must say where the answer is", p.Output)
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
	for dir := range p.ConfigEnv {
		if dir == "HOME" {
			add("config_env may not name HOME — it is set from the seat home already")
		}
	}
	if len(p.PromptArgs) > 0 && p.PromptMode != PromptArgv {
		add("prompt_args is set but prompt_mode is %q — the flag introduces a prompt "+
			"on argv and there is none to introduce", p.PromptMode)
	}
	if len(p.SystemPromptArgs) > 0 && p.SystemPromptEnv != "" {
		// One channel or the other. Both would hand the CLI the same
		// system prompt twice, and which copy wins is the vendor's
		// business rather than something this profile can state.
		add("system_prompt_args and system_prompt_env are both set — a CLI takes " +
			"its system prompt on ONE channel; drop whichever this build does not use")
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
		// underRoot against a stand-in root proves the same two things
		// seeding will: relative, and not escaping the scope. No seat
		// home exists at load time, and the root directory itself will
		// not do — nothing can sit "inside" it by this test.
		if _, err := underRoot(seedValidationRoot, f.Path); err != nil {
			add("seed_files[%d].path %q must be relative and stay inside the %s directory",
				i, f.Path, f.scope())
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

// mode is the prompt mode with its default applied.
func (p *Profile) mode() PromptMode {
	if p.PromptMode == "" {
		return PromptStdin
	}
	return p.PromptMode
}

// output is the output mode with its default applied.
func (p *Profile) output() OutputMode {
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
// It exists so `crewlet llm doctor` and the extractor answer the same
// question. Doctor asked a narrower one — "are usage paths declared" — and so
// reported "reported by CLI" for a provider whose every call estimates, which
// is precisely the question that report exists to settle.
func (p *Profile) ReadsUsage() bool {
	if p.output() == OutputText {
		return false
	}
	return len(p.Usage.Input) > 0 || len(p.Usage.Output) > 0
}
