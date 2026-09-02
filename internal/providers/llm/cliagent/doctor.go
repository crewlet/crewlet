package cliagent

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// probeTimeout caps a version or status probe.
//
// Far shorter than a completion's cap because these do no model work: they
// print a string and exit. Ten seconds still covers a cold Node runtime's
// first start on a loaded host, and a probe that hung for the completion
// timeout would make `doctor` look wedged when the answer is "the binary is
// broken".
const probeTimeout = 10 * time.Second

// smokePrompt is what the smoke test asks for. Short, unambiguous, and
// answerable with one tool call — a profile can look perfect and still not
// produce a parseable tool call, which is the whole reason the smoke test
// runs a real one rather than just a completion.
const smokePrompt = "Call the crewlet_smoke tool with ok set to true. Reply with only the JSON block."

// The two isolation probes.
//
// Both ask the CLI to use one of ITS OWN tools and report a value the model
// cannot fabricate: the current Unix time, read from the engine host's clock
// by the shell, or from a public endpoint by the browser. A model asked to
// echo a fixed token can simply write the token; a model asked for the time
// has no way to be within a couple of minutes of it except by actually
// running the tool. Each is sent with no tools of Crewlet's own, so no
// envelope contract is rendered and the reply is plain prose.

// shellProbePrompt asks the CLI to run a command on the engine host.
const shellProbePrompt = "Using your shell or command-execution tool, run exactly this " +
	"command and reply with only its output, nothing else: date +%s\n" +
	"If you have no tool that can run commands, reply with only: " + noLocalToolsReply

// noLocalToolsReply is what a CLI whose shell is denied is asked to say.
const noLocalToolsReply = "NO-LOCAL-TOOLS"

// webProbeURL answers with the server's clock on a line reading
// `ts=<unix seconds>.<fraction>`. Chosen for being highly available and for
// carrying a value that changes every request, so a fetch cannot be faked
// from memory. It is a plain-text endpoint, which every vendor's fetch tool
// can read.
const webProbeURL = "https://www.cloudflare.com/cdn-cgi/trace"

// webProbePrompt asks the CLI to fetch the URL with its own browser.
const webProbePrompt = "Using your web fetch tool, fetch " + webProbeURL + " and reply with " +
	"only the value after ts= on the line that starts with ts=, nothing else.\n" +
	"If you have no tool that can fetch a URL, or the fetch fails, reply with only: " +
	noWebReply

// noWebReply is what a CLI with no reachable web tool is asked to say.
const noWebReply = "NO-WEB"

// probeSkew is how far a reported clock may sit from the engine's before the
// probe stops believing a tool ran.
//
// A shell answers in the same second. A fetch through a vendor's tool can
// take tens of seconds on a loaded host, and the endpoint's own clock is not
// the engine's, so the window is generous — but it is still a window a
// guessed epoch cannot land in: a model that does not know the current time
// misses it by hours, not seconds.
const probeSkew = 5 * time.Minute

// Diagnosis is what `crewlet llm doctor` reports about one provider.
//
// A struct rather than printed text so the command can render it and a test
// can assert on it — a doctor whose output only exists as fmt calls is one
// nobody writes a regression test for.
type Diagnosis struct {
	Provider   string
	Agent      string
	Mode       string
	Model      string
	Binary     string
	BinaryPath string
	Version    string
	WrittenFor string
	StateDir   string

	// Credentials is what the shared directory holds.
	Credentials string
	// HostLogin names a login on this machine that has NOT been adopted,
	// so "no login" on a box where the CLI plainly works explains itself.
	HostLogin []string
	// TokenEnv reports whether the headless token variable is resolved.
	TokenEnv string
	// TokenUsage is "reported by CLI" or "estimated", because a budget
	// built on estimates is a different promise.
	TokenUsage string
	// Smoke is the result of a real completion with a real tool.
	Smoke string
	// LocalTools is the profile's stance on the CLI's own shell and file
	// tools, beside what the shell probe measured.
	LocalTools string
	// Web is whether the CLI's own fetch tool reached the web, measured.
	Web string

	// AgentRuntime is what an AGENT-MODE entry needs beyond a login, and
	// is empty for a text-mode one. Each half of it fails at a seat's
	// first turn and nowhere earlier: a CLI with no coding-agent runner
	// has nothing to drive, and a box with no bridge to dial gets none of
	// the seat's tools.
	AgentRuntime []string

	Problems []string
}

// DiagnoseOptions are the facts a provider cannot see about itself.
//
// Both belong to the PROCESS rather than to the provider — which runners this
// build registers, and what a sandbox can dial — and both decide whether agent
// mode works. Passed in rather than read here, so `doctor` reports the engine's
// own answers instead of this package's guess at them.
type DiagnoseOptions struct {
	// Smoke runs the real completion and the two tool probes.
	Smoke bool

	// AgentRunners are the coding-agent runner names this build has. An
	// agent-mode entry naming a CLI outside them cannot run at all.
	AgentRunners []string

	// BridgeURL is CREWLET_MCP_BRIDGE_URL as this process sees it. Empty
	// means agent mode is refused at launch.
	BridgeURL string
}

// Diagnose measures one provider end to end.
//
// It never returns an error: every failure it can find is a LINE in the
// report, because an operator running `doctor` wants the whole picture, and a
// command that stopped at the first problem would hide the three behind it.
func (p *Provider) Diagnose(ctx context.Context, opts DiagnoseOptions) Diagnosis {
	smoke := opts.Smoke
	d := Diagnosis{
		Provider: p.key, Agent: p.agent, Mode: p.modeName(), Model: p.model,
		Binary: p.profile.Binary, WrittenFor: p.profile.WrittenFor,
		StateDir: p.ws.Root(),
	}
	d.AgentRuntime, d.Problems = p.agentRuntime(opts)

	path, err := exec.LookPath(p.profile.Binary)
	switch {
	case err != nil:
		d.BinaryPath = "not on PATH"
		d.Problems = append(d.Problems, fmt.Sprintf(
			"%q is not on this host's PATH — the CLI runs on the ENGINE host, so it "+
				"must be installed here; set cli.overrides.binary to an absolute path "+
				"if it lives somewhere unusual", p.profile.Binary))
	default:
		d.BinaryPath = path
		d.Version = p.probeVersion(ctx)
		if d.Version == "" {
			d.Problems = append(d.Problems, fmt.Sprintf(
				"%s %s printed no version — the profile may not match this build; "+
					"it was written for %s", path, strings.Join(p.profile.VersionArgs, " "),
				orNone(p.profile.WrittenFor)))
		}
	}

	files := p.ws.LoginFiles()
	switch {
	case len(files) > 0:
		d.Credentials = "present"
	default:
		d.Credentials = "none on disk"
		d.HostLogin = p.HostLogin("")
	}

	switch {
	case p.profile.TokenEnv == "":
		d.TokenEnv = "n/a — this CLI mints no headless token"
	case p.auth.Token != "":
		d.TokenEnv = "set"
	default:
		d.TokenEnv = "unset"
	}

	if len(p.profile.Usage.Input) > 0 || len(p.profile.Usage.Output) > 0 {
		d.TokenUsage = "reported by CLI"
	} else {
		d.TokenUsage = "estimated (4 characters per token)"
		d.Problems = append(d.Problems, fmt.Sprintf(
			"the %q profile reads no usage figures, so budgets for seats on this "+
				"provider run on estimates", p.agent))
	}

	if d.Credentials == "none on disk" && d.TokenEnv != "set" {
		problem := fmt.Sprintf("no login of its own for %q", p.key)
		if len(d.HostLogin) > 0 {
			problem += fmt.Sprintf(
				", but this machine has one at %s — adopt it with "+
					"`crewlet llm login %s --from-host`", strings.Join(d.HostLogin, ", "), p.key)
			if len(p.profile.CaptureTokenArgs) > 0 {
				problem += fmt.Sprintf(", or mint a headless %s with `--capture-token` "+
					"(preferred: no shared refresh token)", p.profile.TokenEnv)
			}
		} else {
			problem += fmt.Sprintf(" — run `crewlet llm login %s`", p.key)
		}
		d.Problems = append(d.Problems, problem)
	}

	stance := p.localToolsStance()
	switch {
	case !smoke:
		d.Smoke = "skipped (-no-smoke)"
		d.LocalTools = stance + " — probe skipped (-no-smoke)"
		d.Web = "probe skipped (-no-smoke)"
	case d.BinaryPath == "not on PATH":
		d.Smoke = "skipped — no binary to run"
		d.LocalTools = stance + " — probe skipped, no binary to run"
		d.Web = "probe skipped — no binary to run"
	default:
		d.Smoke = p.smokeTest(ctx)
		if strings.HasPrefix(d.Smoke, "failed") {
			d.Problems = append(d.Problems, d.Smoke)
		}
		verdict, problem := p.shellProbe(ctx)
		d.LocalTools = stance + " — " + verdict
		if problem != "" {
			d.Problems = append(d.Problems, problem)
		}
		d.Web = p.webProbe(ctx)
		if strings.HasPrefix(d.Web, "failed") {
			d.Problems = append(d.Problems, d.Web)
		}
	}
	return d
}

// modeName is how this entry runs, for the report.
func (p *Provider) modeName() string {
	if p.agentMode {
		return "agent (the CLI runs the executor)"
	}
	return "text (a model behind the engine's tool loop)"
}

// agentRuntime measures what an agent-mode entry needs beyond a login.
//
// BOTH HALVES FAIL AT A SEAT'S FIRST TURN AND NOWHERE EARLIER, which is the
// whole reason they are here: an entry naming a CLI this build has no runner
// for validates cleanly and reports a configured provider, and an engine with
// no reachable bridge URL refuses every agent-mode launch at the moment a seat
// finally has work. `doctor` is what a deploy script gates on, so it is the
// last place either can be caught before an agent is waiting.
//
// A TEXT-MODE ENTRY REPORTS NOTHING HERE, rather than reporting that it would
// not work in a mode it is not in. It runs as a subprocess of this engine and
// needs neither.
func (p *Provider) agentRuntime(opts DiagnoseOptions) (lines, problems []string) {
	if !p.agentMode {
		return nil, nil
	}
	if slices.Contains(opts.AgentRunners, p.agent) {
		lines = append(lines, fmt.Sprintf("runner: %q is registered", p.agent))
	} else {
		lines = append(lines, fmt.Sprintf("runner: none for %q", p.agent))
		problems = append(problems, fmt.Sprintf(
			"agent mode drives %q through a coding-agent runner and this build "+
				"registers none for it (has: %s) — set `mode: text` on "+
				"providers.llm.%s, or point it at a CLI that has one",
			p.agent, strings.Join(opts.AgentRunners, ", "), p.key))
	}
	if strings.TrimSpace(opts.BridgeURL) != "" {
		lines = append(lines, "tool bridge: "+opts.BridgeURL)
	} else {
		lines = append(lines, "tool bridge: unset")
		problems = append(problems, fmt.Sprintf(
			"agent mode hands the seat's tools to the box over an MCP bridge and "+
				"%s is unset, so every launch is refused — a coding agent with "+
				"none of the seat's tools cannot answer anybody or submit its "+
				"work. Set it to a URL a sandbox can reach",
			bridgeURLVar))
	}
	return lines, problems
}

// bridgeURLVar is the variable an agent-mode box dials the engine on.
//
// SPELLED HERE RATHER THAN IMPORTED, because importing the API package into a
// provider would be a dependency from a leaf onto an edge for one string —
// and a test asserts the two agree, which is the same guard for none of the
// cost.
const bridgeURLVar = "CREWLET_MCP_BRIDGE_URL"

// BridgeURLVar exposes that spelling, for the one test that holds it against
// the package which actually reads the variable.
func BridgeURLVar() string { return bridgeURLVar }

// localToolsStance renders the profile's declared stance.
func (p *Provider) localToolsStance() string {
	switch p.profile.LocalTools {
	case LocalToolsDenied:
		return "denied by profile"
	case LocalToolsVendorDefault:
		return "vendor default (" + p.profile.LocalToolsNote + ")"
	default:
		return "not declared by profile"
	}
}

// shellProbe asks the CLI to run a command with its own shell and reports
// whether it did.
//
// The verdict is measured, never inferred from the profile: a profile that
// says "denied" and a CLI that ran the command is precisely the finding an
// operator needs, and a profile that says "vendor default" and a CLI that
// refused is good news worth printing. The second return is the problem line,
// empty when there is none.
func (p *Provider) shellProbe(ctx context.Context) (verdict, problem string) {
	comp, err := p.Complete(ctx, llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: shellProbePrompt}},
	})
	if err != nil {
		return "probe failed — " + err.Error(), ""
	}
	ran := reportsCurrentClock(comp.Content, time.Now())
	switch {
	case ran && p.profile.LocalTools == LocalToolsDenied:
		return "probe: SHELL RAN", fmt.Sprintf(
			"the %q profile says local tools are denied, but the CLI ran a shell "+
				"command on the engine host — the vendor's denial flag is not taking "+
				"effect on this build (%s); check cli.overrides against the installed "+
				"version before running seats on it", p.agent, orNone(p.profile.WrittenFor))
	case ran:
		return "probe: SHELL RAN", fmt.Sprintf(
			"the %q CLI runs shell commands on the engine host as the engine user "+
				"(%s) — it can read what that user can read. Run seats on it only on "+
				"a host you would hand an autonomous agent, or prefer a backend whose "+
				"profile denies local tools", p.agent, orNone(p.profile.LocalToolsNote))
	default:
		return "probe: refused", ""
	}
}

// webProbe asks the CLI to fetch a URL with its own browser and reports
// whether it did.
//
// Web is the one local tool a profile keeps ON, so a CLI that cannot reach
// it is a problem: the seat has less reach than the same CLI at a terminal,
// and the cause is usually a vendor sandbox flag that also cut the network,
// or an egress proxy the child environment was not told about.
func (p *Provider) webProbe(ctx context.Context) string {
	comp, err := p.Complete(ctx, llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: webProbePrompt}},
	})
	if err != nil {
		return "failed — " + err.Error()
	}
	if reportsCurrentClock(comp.Content, time.Now()) {
		return "ok — fetched " + webProbeURL
	}
	return fmt.Sprintf(
		"failed — the %q CLI could not fetch %s with its own web tool (it said: %q). "+
			"Web is meant to stay on for every subscription seat: check that no vendor "+
			"sandbox flag cuts the network and that the egress proxy reaches the child "+
			"environment (cli.env / passthrough_env)",
		p.agent, webProbeURL, truncate(comp.Content, 120))
}

// reportsCurrentClock reports whether text carries a Unix timestamp within
// probeSkew of now — the evidence both probes turn on.
//
// The reply is scanned for every run of digits rather than parsed as a
// number, because a CLI wraps its answer in whatever it wraps answers in: a
// code fence, a sentence, a trailing newline. A fractional part is ignored.
func reportsCurrentClock(text string, now time.Time) bool {
	epoch := now.Unix()
	for _, field := range strings.FieldsFunc(text, func(r rune) bool {
		return r < '0' || r > '9'
	}) {
		if len(field) < 9 || len(field) > 11 {
			// Fewer digits than a current epoch is a year or a byte
			// count; more is milliseconds, which the prompt did not ask
			// for and which would land a guess in range by accident.
			continue
		}
		n, err := strconv.ParseInt(field, 10, 64)
		if err != nil {
			continue
		}
		if delta := time.Duration(n-epoch) * time.Second; delta > -probeSkew && delta < probeSkew {
			return true
		}
	}
	return false
}

// probeVersion runs the CLI's own version command.
func (p *Provider) probeVersion(ctx context.Context) string {
	if len(p.profile.VersionArgs) == 0 {
		return ""
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, p.profile.Binary, p.profile.VersionArgs...) //nolint:gosec // args come from a validated profile
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
}

// smokeTest runs a REAL completion with a REAL tool.
//
// The command that matters, and the reason it is not merely a version probe:
// a profile can look perfect — binary present, login present, flags accepted
// — and still not produce a parseable tool call, because the envelope
// contract is a request to a model rather than a schema the vendor enforces.
// That failure only shows up on the first turn of a real seat otherwise.
func (p *Provider) smokeTest(ctx context.Context) string {
	comp, err := p.Complete(ctx, llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: smokePrompt}},
		Tools: []llm.ToolDef{{
			Name:        "crewlet_smoke",
			Description: "Confirm the tool channel works.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{"ok": map[string]any{"type": "boolean"}},
				"required":   []any{"ok"},
			},
		}},
		ToolChoice: "required",
	})
	if err != nil {
		return "failed — " + err.Error()
	}
	if len(comp.ToolCalls) == 0 {
		return fmt.Sprintf(
			"failed — the CLI answered but produced no parseable tool call, so seats on "+
				"this provider will burn a corrective round every turn. It said: %q",
			truncate(comp.Content, 200))
	}
	return fmt.Sprintf("ok — %d in / %d out", comp.InputTokens, comp.OutputTokens)
}

// Healthy reports whether the diagnosis found nothing wrong.
func (d Diagnosis) Healthy() bool { return len(d.Problems) == 0 }

// Render writes the report in the fixed-width form the docs show.
func (d Diagnosis) Render(w io.Writer) {
	line := func(label, value string) {
		fmt.Fprintf(w, "%-14s: %s\n", label, value)
	}
	line("provider", d.Provider)
	line("cli agent", d.Agent)
	line("mode", d.Mode)
	if d.Model != "" {
		line("model", d.Model)
	}
	line("binary", orNone(d.BinaryPath))
	line("version", orNone(d.Version))
	line("written for", orNone(d.WrittenFor))
	line("state dir", d.StateDir)
	line("credentials", d.Credentials)
	if len(d.HostLogin) > 0 {
		line("host login", strings.Join(d.HostLogin, ", ")+" (not adopted)")
	}
	line("token env", d.TokenEnv)
	line("token usage", d.TokenUsage)
	line("smoke test", d.Smoke)
	line("local tools", d.LocalTools)
	line("web", d.Web)
	for i, entry := range d.AgentRuntime {
		label := ""
		if i == 0 {
			label = "agent runtime"
		}
		line(label, entry)
	}
	if d.Healthy() {
		line("problems", "none")
		return
	}
	fmt.Fprintln(w, "problems:")
	for _, problem := range d.Problems {
		fmt.Fprintf(w, "  - %s\n", problem)
	}
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// LoginState is a one-word summary for `crewlet llm list`.
func (p *Provider) LoginState() string {
	switch {
	case p.ws.HasLogin():
		return "credentials"
	case p.auth.Token != "":
		return "token"
	case p.auth.Mode == AuthAPIKey && p.auth.APIKey != "":
		return "api key"
	case p.auth.Mode == AuthInheritEnv:
		if p.profile.TokenEnv != "" && os.Getenv(p.profile.TokenEnv) != "" {
			return "inherited token"
		}
		if p.profile.APIKeyEnv != "" && os.Getenv(p.profile.APIKeyEnv) != "" {
			return "inherited key"
		}
		return "none"
	default:
		return "none"
	}
}

// Vendor is the model FAMILY this provider's CLI addresses.
//
// Not the providers.llm type, which is "cli-agent" for every one of them: a
// coding agent that resolves "<family>/<model>" against a catalogue would
// otherwise address a Claude subscription's "sonnet" as an OpenAI model.
func (p *Provider) Vendor() string { return p.profile.Vendor }

// SandboxCredentials maps this provider's login onto a coding box's home:
// each credential path RELATIVE to the box home, against the absolute path of
// the shared file on the engine host.
//
// Empty when there is no login on disk, which is the correct answer rather
// than a set of paths that do not exist — a box seeded with missing files
// would report a puzzling failure inside the run instead of the plain "not
// authenticated" the CLI gives when it finds nothing.
//
// A LOCAL box seeds these and writes a refreshed one back. A remote box must
// ignore them: they carry a refresh token whose rotation is shared fleet
// state, and pushing that onto somebody else's VM is a materially larger
// trust step than the scoped headless token the run env already exports.
func (p *Provider) SandboxCredentials() map[string]string {
	shared := p.ws.CredentialsDir()
	out := map[string]string{}
	for _, rel := range p.profile.CredentialPaths {
		host := filepath.Join(shared, filepath.Base(rel))
		if info, err := os.Stat(host); err != nil || !info.Mode().IsRegular() {
			continue
		}
		out[rel] = host
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// SandboxEnv is the credential a coding run carries in its ENVIRONMENT — the
// headless subscription token, where one is configured.
//
// A token travels to any box, including a remote one, because it is a single
// scoped and revocable variable rather than a rotating refresh secret. An
// api-key entry contributes its key here instead, and a subscription entry
// with neither contributes nothing at all: the run then needs the credential
// files, which is why a CLI that mints no token needs a local box.
func (p *Provider) SandboxEnv() map[string]string {
	out := map[string]string{}
	switch p.auth.Mode {
	case AuthAPIKey:
		if p.profile.APIKeyEnv != "" && p.auth.APIKey != "" {
			out[p.profile.APIKeyEnv] = p.auth.APIKey
		}
	case AuthInheritEnv:
		for _, name := range []string{p.profile.TokenEnv, p.profile.APIKeyEnv} {
			if name == "" {
				continue
			}
			if value, ok := os.LookupEnv(name); ok {
				out[name] = value
			}
		}
	default:
		if p.profile.TokenEnv != "" && p.auth.Token != "" {
			out[p.profile.TokenEnv] = p.auth.Token
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// MintsHeadlessToken reports whether this CLI can produce a token that
// travels to a remote box, for the error that has to distinguish the two
// cases an operator can be in.
func (p *Provider) MintsHeadlessToken() bool {
	return p.profile.TokenEnv != "" && len(p.profile.CaptureTokenArgs) > 0
}

// CredentialEnvNames are the variables that authenticate this CLI inside a
// box: the headless token's and the API key's.
//
// Exported because the launch has to answer a question only it can — whether
// ANYTHING in the run environment authenticates, including a value the
// OPERATOR declared in role.sandbox.env. The engine names no tool-specific
// variable of its own, so it cannot recognise a credential by inspection; what
// it can do is ask the profile which names count and look for those.
func (p *Provider) CredentialEnvNames() []string {
	names := make([]string, 0, 2)
	for _, name := range []string{p.profile.TokenEnv, p.profile.APIKeyEnv} {
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}
