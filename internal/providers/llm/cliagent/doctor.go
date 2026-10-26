package cliagent

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
	"github.com/crewlet/crewlet/internal/toolsession"
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
// hand its CLI the seat's tools, which is the whole reason the smoke test runs
// a real call rather than just a completion.
const smokePrompt = "Call the " + smokeTool + " tool with ok set to true, then reply with only: done"

// The two isolation probes.
//
// Both ask the CLI to use one of ITS OWN tools and report a value the model
// cannot fabricate: the current Unix time, read from the engine host's clock
// by the shell, or from a public endpoint by the browser. A model asked to
// echo a fixed token can simply write the token; a model asked for the time
// has no way to be within a couple of minutes of it except by actually
// running the tool. Each is sent with no tools of Crewlet's own, so the CLI's
// own tools are the only ones it has and the reply is plain prose.

// THE NO-TOOL ANSWER COMES FIRST, AND A CALL WRITTEN AS TEXT IS RULED OUT. A
// prompt that opened "Using your shell tool, run ..." on a CLI given no tools
// at all (Claude Code 2.1.310 under `--tools ""`, Sonnet) was answered with a
// tool call typed out as prose (`<invoke name="Bash">...`), measured: no
// command ran, and a model that goes on to write the call's output and the
// next step as prose kept the probe running to the entry's whole timeout
// before the doctor could read a refusal. Asked which case it is in first,
// the model answers the refusal at once, and a model that has the tool still
// runs it, which is the finding the probe exists for.

// shellProbePrompt asks the CLI to run a command on the engine host.
const shellProbePrompt = "If you were given a tool that runs shell commands, use it to run " +
	"exactly this command and reply with only its output, nothing else: date +%s\n" +
	"If you were given no such tool, reply with only: " + noLocalToolsReply + "\n" +
	noProseCall

// fileProbePrompt asks the CLI to read a file on the engine host, the path
// substituted for %s. The file holds a value minted for the one run, so a
// model can echo it only by reading it.
const fileProbePrompt = "If you were given a tool that reads files, use it to read the file " +
	"at %s and reply with only its contents, nothing else.\n" +
	"If you were given no such tool, reply with only: " + noLocalToolsReply + "\n" +
	noProseCall

// noProseCall is the line every isolation probe ends on: a tool is called or
// the refusal is given, never a call written out as text.
const noProseCall = "Never write a tool call, or what a tool would return, as text."

// noLocalToolsReply is what a CLI whose shell is denied is asked to say.
const noLocalToolsReply = "NO-LOCAL-TOOLS"

// webProbeURL answers with the server's clock on a line reading
// `ts=<unix seconds>.<fraction>`. Chosen for being highly available and for
// carrying a value that changes every request, so a fetch cannot be faked
// from memory. It is a plain-text endpoint, which every vendor's fetch tool
// can read.
const webProbeURL = "https://www.cloudflare.com/cdn-cgi/trace"

// webProbePrompt asks the CLI to fetch the URL with its own browser.
const webProbePrompt = "If you were given a tool that fetches URLs, use it to fetch " +
	webProbeURL + " and reply with only the value after ts= on the line that starts with " +
	"ts=, nothing else.\n" +
	"If you were given no such tool, or the fetch fails, reply with only: " + noWebReply + "\n" +
	noProseCall

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
	// so "no sign-in" on a box where the CLI plainly works explains itself.
	HostLogin []string
	// TokenEnv reports whether the headless token variable is resolved.
	TokenEnv string
	// SignIn is every route that authenticates the child — the credential
	// files, a token, a key, the CLI's own environment — read off the
	// environment the child is actually given, or "none".
	SignIn string
	// TokenUsage is "reported by CLI" or "estimated", because a budget
	// built on estimates is a different promise.
	TokenUsage string
	// SystemPrompt is where a seat's system prompt goes and where an
	// auxiliary pass's does — beside the CLI's own, in place of it, or in the
	// transcript under it — so an operator can see which vendor prompt their
	// seats run under without reading the profile.
	SystemPrompt []string
	// Smoke is the result of a real completion with a real tool.
	Smoke string
	// LocalTools is the profile's stance on the CLI's own shell and file
	// tools, beside what the shell probe measured.
	LocalTools string
	// Web is the entry's web stance beside whether the CLI's own fetch tool
	// reached the web, measured. On a denied entry a fetch that succeeded is
	// a problem, as a shell that ran is on a profile denying local tools.
	Web string

	// BoxRuntime is what a BOX-MODE entry needs beyond a login, and is
	// empty for a host-mode one. Each half of it fails at a seat's
	// first turn and nowhere earlier: a CLI with no coding-agent runner
	// has nothing to drive, and a box with no bridge to dial gets none of
	// the seat's tools.
	BoxRuntime []string

	Problems []string
}

// DiagnoseOptions are the facts a provider cannot see about itself.
//
// Both belong to the PROCESS rather than to the provider — which runners this
// build registers, and what a sandbox can dial — and both decide whether box
// mode works. Passed in rather than read here, so `doctor` reports the engine's
// own answers instead of this package's guess at them.
type DiagnoseOptions struct {
	// Smoke runs the real completion and the two tool probes.
	Smoke bool

	// AgentRunners are the coding-agent runner names this build has. A
	// box-mode entry naming a CLI outside them cannot run at all.
	AgentRunners []string

	// BridgeURL is CREWLET_MCP_BRIDGE_URL as this process sees it. Empty
	// means box mode is refused at launch.
	BridgeURL string

	// Home is the directory a login this machine already holds is looked for
	// under ([Provider.HostLogin]); empty is the user's own home, which is
	// what an operator's `crewlet llm doctor` reads. A suite names an empty
	// directory, so the report it judges never depends on whether the machine
	// running it has a CLI signed in.
	Home string
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
		StateDir: p.ws.StateDir(),
	}
	d.BoxRuntime, d.Problems = p.boxRuntime(opts)

	path, err := exec.LookPath(p.profile.Binary)
	switch {
	case err != nil:
		d.BinaryPath = "not on PATH"
		d.Problems = append(d.Problems, fmt.Sprintf(
			"%q is not on this host's PATH — the CLI runs on the ENGINE host, so it "+
				"must be installed here; set overrides.binary to an absolute path "+
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

	login, loginErr := p.ws.Login(ctx)
	loggedOut, loggedOutErr := p.ws.loggedOut()
	switch {
	case loginErr != nil:
		d.Credentials = "unknown — the shared login could not be read"
		d.Problems = append(d.Problems, fmt.Sprintf("the shared login in %s could not be read, "+
			"so whether its credential files sign the entry in is unknown: %v",
			p.ws.CredentialsDir(), loginErr))
	case len(p.ws.loginIn(login)) > 0:
		d.Credentials = "present"
	default:
		d.Credentials = "none on disk"
		if loggedOut {
			// The doctor restored nothing for the same reason the engine
			// would not: see [ErrLoggedOut].
			d.Credentials += " — removed by `crewlet llm logout`"
			if p.bundle != "" {
				d.Credentials += ", so its configured credential bundle is not restored until a " +
					"login is written again"
			}
		}
		d.HostLogin = p.HostLogin(ctx, opts.Home)
	}
	if loggedOutErr != nil {
		d.Problems = append(d.Problems, fmt.Sprintf("whether the entry was logged out cannot be "+
			"read, so neither can whether its configured credential bundle restores: %v", loggedOutErr))
	}

	switch {
	case p.profile.TokenEnv == "":
		d.TokenEnv = "n/a — this CLI mints no headless token"
	case p.auth.Token != "":
		d.TokenEnv = "set"
	default:
		d.TokenEnv = "unset"
	}
	// A credential variable no store can hold is its own problem, at the
	// field that names it, whether or not anything signs the entry in: the
	// routes below leave it out ([misnamedVariable]), and without this line
	// the report would not say why.
	for _, variable := range p.misnamedVariables() {
		d.Problems = append(d.Problems, p.misnamedProblem(variable))
	}

	if p.profile.ReadsUsage() {
		d.TokenUsage = "reported by CLI"
	} else {
		d.TokenUsage = "estimated (4 characters per token)"
		d.Problems = append(d.Problems, fmt.Sprintf(
			"the %q profile reads no usage figures, so budgets for seats on this "+
				"provider run on estimates", p.agent))
	}

	d.SystemPrompt = []string{
		"seats: " + describeRoute(p.profile.RouteFor(true)),
		"side passes: " + describeRoute(p.profile.RouteFor(false)),
	}

	signIn := p.signInFrom(login, loginErr)
	d.SignIn = signIn.line()
	answered := false

	stance := p.localToolsStance()
	web := p.webStance()
	switch {
	case !smoke:
		d.Smoke = "skipped (-no-smoke)"
		d.LocalTools = stance + "; probe skipped (-no-smoke)"
		d.Web = web + "; probe skipped (-no-smoke)"
	case d.BinaryPath == "not on PATH":
		d.Smoke = "skipped: no binary to run"
		d.LocalTools = stance + "; probe skipped, no binary to run"
		d.Web = web + "; probe skipped, no binary to run"
	default:
		d.Smoke, answered = p.smokeTest(ctx)
		if strings.HasPrefix(d.Smoke, "failed") {
			d.Problems = append(d.Problems, d.Smoke)
		}
		verdict, problem := p.shellProbe(ctx)
		if verdict == refusedVerdict {
			// A refused shell is half the stance: the file tools read
			// the host as well as a shell does, and the deny list that
			// refused one says nothing about the other.
			verdict, problem = p.fileProbe(ctx)
		}
		d.LocalTools = stance + "; " + verdict
		if problem != "" {
			d.Problems = append(d.Problems, problem)
		}
		verdict, problem = p.webProbe(ctx)
		d.Web = web + "; " + verdict
		if problem != "" {
			d.Problems = append(d.Problems, problem)
		}
	}

	// NOTHING THE ENGINE HANDS THE CLI SIGNS IT IN — which is a problem
	// unless the CLI answered anyway. A model served by an endpoint that
	// takes no key (a hermes or pi entry pointed at a local server through
	// env) is signed in to nothing and works, and so is a CLI holding a
	// key in a configuration file of its own that no profile names. The
	// configuration cannot tell those from a missing key; the smoke test's
	// answer can, so it decides, and without it the report says what it
	// sees and asks for the run that settles it.
	if len(signIn.sources) == 0 && signIn.unreadable == nil {
		if answered {
			d.SignIn = "none the engine hands the CLI — but the smoke test was " +
				"answered, so it authenticates some other way (an endpoint that takes " +
				"no key, or a credential in its own configuration)"
		} else {
			d.Problems = append(d.Problems, p.noSignIn(d.HostLogin, signIn.unresolved))
		}
	}
	return d
}

// noSignIn is the problem for an entry nothing authenticates, naming every
// route this CLI could take under the entry's auth mode and, first, the one the
// operator already configured and this process could not resolve.
func (p *Provider) noSignIn(hostLogin, unresolved []string) string {
	problem := fmt.Sprintf("no sign-in for %q: no credential files in %s, and nothing "+
		"in the environment the CLI is given authenticates it", p.key, p.ws.CredentialsDir())
	if out, err := p.ws.loggedOut(); err == nil && out && p.bundle != "" {
		problem += " — `crewlet llm logout` removed its login, and its configured credential " +
			"bundle stays out until a login is written again"
	}
	if len(unresolved) > 0 {
		problem += fmt.Sprintf(" — env sets %s, but the ${VAR} it references "+
			"resolved to nothing: export it, or store it with `crewlet secrets set`",
			strings.Join(unresolved, ", "))
	}
	// THE ROUTES FOLLOW THE AUTH MODE, since the mode decides what the entry
	// reads at all ([Provider.ModeSignIn]). Chosen without it, an api-key or
	// inherit-env entry on a machine with a personal login was told to mint
	// a headless token with `-capture-token` — preferred, even — which
	// api-key removes from the CLI's environment and inherit-env never looks
	// for in the secret store the token lands in. An api-key entry is told
	// its key and nothing else: a login adopted into its directory is seeded
	// into every seat home beside the key, and which of the two the CLI then
	// bills is the vendor's precedence rather than the entry's. An
	// inherit-env entry is told the variables it forwards, and a login,
	// which it seeds like every mode.
	sign, fromEnv := p.ModeSignIn()
	switch {
	case !fromEnv:
		if p.auth.TokenConfigured && p.auth.Token == "" {
			problem += " — auth.token is set, but the ${VAR} it references resolved " +
				"to nothing: export it, or store it with `crewlet secrets set`"
		}
		problem += " — " + p.loginRemedy(hostLogin)
		if len(hostLogin) > 0 && p.MintsHeadlessToken() {
			problem += fmt.Sprintf(", or mint a headless %s with `-capture-token` "+
				"(preferred: no shared refresh token)", p.profile.TokenEnv)
		}
	case sign.Mode == AuthAPIKey:
		problem += " — " + sign.Missing()
	default:
		problem += " — " + sign.Missing() + " — " + p.loginRemedy(hostLogin)
	}
	// Offered only when env does not already try it: an entry whose
	// reference resolved to nothing was told so above, and being told to
	// do what it did would read as a second, different fault.
	if p.profile.EnvSignIn && len(unresolved) == 0 {
		problem += fmt.Sprintf(", or set the key of the provider its model names in "+
			"env, under that provider's own variable (the %q CLI reads it from "+
			"its environment)", p.agent)
	}
	// Said last because it is the one case where nothing needs doing — a
	// CLI that fronts any provider may be pointed at a local server — and
	// only the smoke test can tell it apart: a run with it skipped has not
	// asked the CLI anything.
	if p.profile.EnvSignIn {
		problem += ". If this entry's model is served by an endpoint that takes no " +
			"key, run the doctor without -no-smoke: an answered smoke test clears this"
	}
	return problem
}

// loginRemedy is how to write this entry a login of its own: adopt the one this
// machine already has, else run the CLI's own login command — or, for a CLI
// that keeps no login at all, give it its key.
//
// NEVER A COMMAND THE CLI DOES NOT HAVE. Gemini CLI, Qwen Code and Grok declare
// no login subcommand, and `crewlet llm login <key>` refuses for them, saying to
// sign the CLI in by hand and adopt that login — so a doctor that offered the
// command sent the operator to a refusal whose remedy it could have named
// itself ([Provider.BrokerLogin] words the same route). Qwen Code keeps no
// login to adopt either, so its route is its key.
func (p *Provider) loginRemedy(hostLogin []string) string {
	switch {
	case !p.profile.KeepsLogin():
		// No login to adopt or broker: the CLI keeps none, so its key, or
		// a model provider's in env, is the whole sign-in.
		return p.keyRemedy()
	case len(hostLogin) > 0:
		return fmt.Sprintf("this machine has a login at %s: adopt it with "+
			"`crewlet llm login %s -from-host`", strings.Join(hostLogin, ", "), p.key)
	case len(p.profile.LoginArgs) > 0:
		return fmt.Sprintf("run `crewlet llm login %s`", p.key)
	default:
		return fmt.Sprintf("the %q CLI declares no login subcommand: sign it in by hand "+
			"and adopt that login with `crewlet llm login %s -from-host`", p.agent, p.key)
	}
}

// keyRemedy is how to sign in an entry whose CLI keeps no login of its own
// ([cliprofile.Profile.KeepsLogin]): its key, in the variable the profile names.
// Qwen Code is such a CLI — both measured builds refuse the OAuth login it once
// kept — and neither adopting a login nor brokering one has anything to offer it.
func (p *Provider) keyRemedy() string {
	if p.profile.APIKeyEnv == "" {
		return fmt.Sprintf("the %q profile names no place a login is kept and no key "+
			"variable: declare one under providers.llm.%s.overrides", p.agent, p.key)
	}
	return fmt.Sprintf("sign it in with its key: set auth.mode: api-key and give the key "+
		"as auth.api_key (the CLI reads it from %s)", p.profile.APIKeyEnv)
}

// modeName is how this entry runs, for the report.
func (p *Provider) modeName() string {
	if p.box {
		return "box (the CLI runs the executor in a box)"
	}
	return "host (the CLI runs on this host, turn by turn)"
}

// boxRuntime measures what a box-mode entry needs beyond a login.
//
// BOTH HALVES FAIL AT A SEAT'S FIRST TURN AND NOWHERE EARLIER, which is the
// whole reason they are here: an entry naming a CLI this build has no runner
// for validates cleanly and reports a configured provider, and an engine with
// no reachable bridge URL refuses every box-mode launch at the moment a seat
// finally has work. `doctor` is what a deploy script gates on, so it is the
// last place either can be caught before an agent is waiting.
//
// A HOST-MODE ENTRY REPORTS NOTHING HERE, rather than reporting that it would
// not work in a mode it is not in. It runs as a subprocess of this engine and
// needs neither.
func (p *Provider) boxRuntime(opts DiagnoseOptions) (lines, problems []string) {
	if !p.box {
		return nil, nil
	}
	if slices.Contains(opts.AgentRunners, p.agent) {
		lines = append(lines, fmt.Sprintf("runner: %q is registered", p.agent))
	} else {
		lines = append(lines, fmt.Sprintf("runner: none for %q", p.agent))
		problems = append(problems, fmt.Sprintf(
			"box mode drives %q through a coding-agent runner and this build "+
				"registers none for it (has: %s) — set `mode: host` on "+
				"providers.llm.%s, or point it at a CLI that has one",
			p.agent, strings.Join(opts.AgentRunners, ", "), p.key))
	}
	if strings.TrimSpace(opts.BridgeURL) != "" {
		lines = append(lines, "tool bridge: "+opts.BridgeURL)
	} else {
		lines = append(lines, "tool bridge: unset")
		problems = append(problems, fmt.Sprintf(
			"box mode hands the seat's tools to the box over an MCP bridge and "+
				"%s is unset, so every launch is refused — a coding agent with "+
				"none of the seat's tools cannot answer anybody or submit its "+
				"work. Set it to a URL a sandbox can reach",
			bridgeURLVar))
	}
	return lines, problems
}

// bridgeURLVar is the variable a box-mode run's box dials the engine on.
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
	case cliprofile.LocalToolsDenied:
		return "denied by profile"
	case cliprofile.LocalToolsVendorDefault:
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
	comp, err := p.Complete(ctx, probeRequest(shellProbePrompt))
	if err != nil {
		return "probe failed: " + err.Error(), ""
	}
	ran := reportsCurrentClock(comp.Content, time.Now())
	switch {
	case ran && p.profile.LocalTools == cliprofile.LocalToolsDenied:
		return "probe: SHELL RAN", fmt.Sprintf(
			"the %q profile says local tools are denied, but the CLI ran a shell "+
				"command on the engine host — the vendor's denial flag is not taking "+
				"effect on this build (%s); check overrides against the installed "+
				"version before running seats on it", p.agent, orNone(p.profile.WrittenFor))
	case ran:
		return "probe: SHELL RAN", fmt.Sprintf(
			"the %q CLI runs shell commands on the engine host as the engine user "+
				"(%s) — it can read what that user can read. Run seats on it only on "+
				"a host you would hand an autonomous agent, or prefer a backend whose "+
				"profile denies local tools", p.agent, orNone(p.profile.LocalToolsNote))
	default:
		return refusedVerdict, ""
	}
}

// refusedVerdict is a probe that found its tool refused.
const refusedVerdict = "probe: refused"

// fileProbe asks the CLI to read a file the doctor has just written on the
// engine host, outside the seat's home, and reports whether it did, on the
// argv the entry runs every call on. The second
// return is the problem line, empty when there is none.
func (p *Provider) fileProbe(ctx context.Context) (verdict, problem string) {
	dir, err := os.MkdirTemp("", "crewlet-file-probe-")
	if err != nil {
		return "file probe failed: " + err.Error(), ""
	}
	defer func() {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			log.WarnContext(ctx, "cli_agent_file_probe_cleanup_failed", "dir", dir, "error", rmErr)
		}
	}()
	token := "crewlet-file-probe-" + newSessionID()
	path := filepath.Join(dir, "probe.txt")
	if writeErr := os.WriteFile(path, []byte(token+"\n"), 0o600); writeErr != nil {
		return "file probe failed: " + writeErr.Error(), ""
	}
	comp, err := p.Complete(ctx, probeRequest(fmt.Sprintf(fileProbePrompt, path)))
	if err != nil {
		return "file probe failed: " + err.Error(), ""
	}
	if !strings.Contains(comp.Content, token) {
		return refusedVerdict + " (shell and file read)", ""
	}
	if p.profile.LocalTools == cliprofile.LocalToolsDenied {
		return "probe: FILE READ", fmt.Sprintf(
			"the %q profile says local tools are denied, but the CLI read a file the doctor "+
				"wrote on the engine host, outside its seat home: its file tools are not "+
				"off on this build (%s), so it can read the engine's own data; check "+
				"overrides against the installed version before running seats or the "+
				"assistant on it", p.agent, orNone(p.profile.WrittenFor))
	}
	return "probe: FILE READ", fmt.Sprintf(
		"the %q CLI reads files on the engine host as the engine user (%s): it can read "+
			"the engine's own data. Run seats on it only on a host you would hand an "+
			"autonomous agent", p.agent, orNone(p.profile.LocalToolsNote))
}

// webStance renders the entry's declared web stance.
func (p *Provider) webStance() string {
	if p.profile.EffectiveWebTools() == cliprofile.WebToolsDenied {
		return "denied by entry (" + orNone(p.profile.WebToolsNote) + ")"
	}
	return "allowed"
}

// webProbe asks the CLI to fetch a URL with its own browser and reports
// whether it did. The second return is the problem line, empty when there is
// none.
//
// WHICH ANSWER IS THE PROBLEM DEPENDS ON THE STANCE, and the verdict is
// measured either way rather than inferred from the profile. On an entry that
// keeps web on, a CLI that cannot reach it is the problem: the seat has less
// reach than the same CLI at a terminal, and the cause is usually a vendor
// sandbox flag that also cut the network, or an egress proxy the child
// environment was not told about. On an entry that DENIES web (the one an
// assistant runs on), a CLI that fetched is the problem, and the worse one:
// the denial is a claim the assistant is admitted on, and a vendor that
// renamed a flag would leave the model fetching under a configuration that
// says it cannot.
func (p *Provider) webProbe(ctx context.Context) (verdict, problem string) {
	denied := p.profile.EffectiveWebTools() == cliprofile.WebToolsDenied
	comp, err := p.Complete(ctx, probeRequest(webProbePrompt))
	if err != nil {
		return "probe failed: " + err.Error(), "the web probe failed: " + err.Error()
	}
	fetched := reportsCurrentClock(comp.Content, time.Now())
	switch {
	case fetched && denied:
		return "probe: FETCHED " + webProbeURL, fmt.Sprintf(
			"the entry denies the %q CLI its web tools, but the CLI fetched %s with "+
				"its own web tool: the profile's web_denial is not taking effect on "+
				"this build (%s); check overrides.web_denial against the installed "+
				"version before an assistant runs on this entry",
			p.agent, webProbeURL, orNone(p.profile.WrittenFor))
	case fetched:
		return "ok: fetched " + webProbeURL, ""
	case strings.TrimSpace(comp.Content) == "":
		// Same split as smokeTest, and for the same reason: an empty
		// answer is a completion now, so "it said: \"\"" would send an
		// operator to the egress proxy, or past a denial that may not
		// hold, for a model that never spoke.
		return "probe inconclusive", fmt.Sprintf(
			"the web probe is inconclusive: the %q CLI exited 0 and answered with "+
				"nothing (%d output tokens billed), so it says nothing about web access "+
				"either way. Point this entry at a stronger model and run the doctor again",
			p.agent, comp.OutputTokens)
	case denied:
		return "probe: refused", ""
	}
	// WHAT IT SAID, WHOLE: the reader is an operator deciding what to fix,
	// and a refusal's reason — a sandbox flag, a proxy's answer — is rarely
	// in its first hundred and twenty bytes, which is where this used to stop.
	return "failed", fmt.Sprintf(
		"the %q CLI could not fetch %s with its own web tool (it said: %q). "+
			"Web is meant to stay on for every subscription seat: check that no vendor "+
			"sandbox flag cuts the network and that the egress proxy reaches the child "+
			"environment (env / passthrough_env)",
		p.agent, webProbeURL, strings.TrimSpace(comp.Content))
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
//
// THROUGH run, like every other child this package spawns. It hand-rolled a
// second exec.CommandContext with no procgroup, no Cancel and no WaitDelay —
// so probeTimeout bounded nothing it claimed to. cmd.Output waits for the
// output pipes to reach EOF, and a coding CLI is a launcher whose forked
// helper inherits them: the helper holding a pipe open kept Wait blocked long
// past the ten seconds, with `doctor` looking wedged for the exact reason it
// was run to diagnose.
//
// The error contract differs from Output's and that is the point: run reports
// a non-zero exit as (res, nil), so an empty version is a probe that produced
// nothing rather than one that failed to start. Nothing is read from a probe
// that was not seen to its end either: a stdout a helper held was cut, and the
// first line of a cut stream is not known to be a whole version.
func (p *Provider) probeVersion(ctx context.Context) string {
	if len(p.profile.VersionArgs) == 0 {
		return ""
	}
	res, err := run(ctx, invocation{
		binary: p.profile.Binary,
		args:   p.profile.VersionArgs,
		// THE SAME ALLOWLISTED ENVIRONMENT A REAL CALL GETS. Omitting it
		// does not run the probe with no environment — it runs it with
		// the ENGINE's, see [Provider.probeEnv].
		env:     p.probeEnv(),
		timeout: probeTimeout,
		grace:   p.termGrace,
	})
	if err != nil || res.exitCode != 0 || res.unfinished() != "" {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(res.stdout, "\n", 2)[0])
}

// smokeTest runs a REAL completion with a REAL tool, served the way a seat's
// are.
//
// The command that matters, and the reason it is not merely a version probe:
// a profile can look perfect — binary present, login present, flags accepted
// — and still not hand its CLI the seat's tools, because how a CLI is told of
// an MCP server, which of its tools it lets the model call, and how it records
// its requests all move with every vendor release. That failure only shows up
// on the first turn of a real seat otherwise.
//
// THE PRODUCTION ROUTE, end to end: one tool, `crewlet_smoke`, on a real
// session server this command opens for itself ([toolsession.NewHost]); the
// profile's tool route renders the CLI's server configuration; the CLI starts
// this binary as the relay, lists the tool and — asked the way a phase asks,
// the instruction naming the tool and nothing forcing it — calls it. It passes
// when the server recorded the call, and it says how the call was joined to
// the CLI's record and which tools the CLI offered its model, which is where a
// confinement that stopped holding shows first.
//
// answered reports whether the model replied at all, however badly: a reply
// is proof the CLI reached its model signed in, which the doctor needs apart
// from whether the reply was usable.
func (p *Provider) smokeTest(ctx context.Context) (verdict string, answered bool) {
	if p.profile.MCP == nil {
		// Asked plainly, so the doctor still learns whether the CLI is
		// signed in; the missing route is the finding.
		comp, err := p.Complete(ctx, probeRequest("Reply with only: ok"))
		return fmt.Sprintf("failed — the %q profile has no mcp block, so a seat's phase on "+
			"this entry cannot be handed its tools and is refused (text functions still run); "+
			"declare providers.llm.%s.overrides.mcp", p.agent, p.key), err == nil && comp != nil
	}
	sessions, err := toolsession.NewHost()
	if err != nil {
		return "failed — this process cannot serve a tool session: " + err.Error(), false
	}
	defer func() { _ = sessions.Close() }()
	host := newSmokeHost(sessions)
	var offered []string
	comp, err := p.Complete(withOffered(ctx, func(tools []string) { offered = tools }), smokeRequest(host))
	if err != nil {
		return "failed: " + err.Error(), false
	}
	if ok, called := host.verdict(offered, comp, p.profile.Requests != nil); called {
		return ok, true
	}
	// TWO DIFFERENT FAILURES, and `It said: ""` describes only one of them. A
	// CLI that answered with prose never called the tool it was offered; a CLI
	// that answered with NOTHING spent its output on hidden reasoning and
	// needs a different model, not a different prompt. The provider hands both
	// back as a completion — an empty answer is a value, not an error — so
	// this probe is the only command that names which one an operator has.
	if strings.TrimSpace(comp.Content) == "" {
		return fmt.Sprintf(
			"failed — the CLI exited 0 and answered with nothing at all (%d output "+
				"tokens billed), so it spent its whole answer on hidden reasoning. "+
				"Every phase a seat runs on this provider will spend corrective rounds "+
				"asking again and then end without its submission: point this entry "+
				"at a stronger model",
			comp.OutputTokens), true
	}
	return fmt.Sprintf(
		"failed — the CLI reached the seat's tools and answered without calling %s, so "+
			"every phase a seat runs on this provider will spend a corrective round asking "+
			"again, and end without its submission whenever the model never makes the call. "+
			"It said: %q", smokeTool, strings.TrimSpace(comp.Content)), true
}

// probeSystemPrompt is the system prompt every probe carries.
//
// A probe proves what a SEAT runs under, so it is sent as a seat's call is:
// an agent call with a system prompt, on the channel a seat's prompt takes.
// A probe with none passed no channel at all on most profiles, so the vendor's
// default ran instead — and the doctor reported on a configuration no seat
// ever had (it said a WebFetch still fetched with Claude Code's prompt
// replaced, while the probe ran with it in force).
const probeSystemPrompt = "You are the probe `crewlet llm doctor` runs to check this " +
	"entry. Answer exactly as the message asks, with nothing else."

// probeRequest is one probe's call: a seat's shape, with the probe's text as
// the message.
func probeRequest(text string) llm.Request {
	return llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: probeSystemPrompt},
			{Role: llm.RoleUser, Content: text},
		},
		Agent: true,
	}
}

// describeRoute says where one kind of call's system prompt goes, naming the
// profile field an operator would override.
func describeRoute(r cliprofile.Route) string {
	switch {
	case r.Field == "":
		return "in the transcript, under the CLI's own (the profile declares no channel)"
	case r.KeepsDefault:
		return "added to the CLI's own (" + r.Field + ")"
	default:
		return "replaces the CLI's own (" + r.Field + ")"
	}
}

// smokeRequest is the one call [Provider.smokeTest] makes: the crewlet_smoke
// tool offered on host, and the instruction that names it.
func smokeRequest(host llm.ToolHost) llm.Request {
	req := probeRequest(smokePrompt)
	req.Tools = host
	return req
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
	line("sign-in", d.SignIn)
	line("token usage", d.TokenUsage)
	for i, entry := range d.SystemPrompt {
		label := ""
		if i == 0 {
			label = "system prompt"
		}
		line(label, entry)
	}
	line("smoke test", d.Smoke)
	line("local tools", d.LocalTools)
	line("web", d.Web)
	for i, entry := range d.BoxRuntime {
		label := ""
		if i == 0 {
			label = "box runtime"
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

// SignInState is a one-word summary for `crewlet llm list`: the first route
// the doctor's sign-in line names, or "none" — the same answer, so the two
// commands cannot disagree about whether an entry is signed in.
func (p *Provider) SignInState(ctx context.Context) string { return p.signIn(ctx).state() }

// SandboxCredentials maps this provider's login onto a coding box's home:
// each credential path RELATIVE to the box home, against the absolute path of
// the shared file on the engine host.
//
// EVERY FILE THE LOGIN MAY HOLD, whether or not the shared directory holds it
// now. The box reads the login when it is seeded, as ONE login under the shared
// directory's lock ([hostbox.ReadCredentials]), and seeds the files that login
// holds and no others — so a box is never handed a file that does not exist.
// A map of only the files present when it was built named a set the login may
// have moved past by the time the box was seeded: a box given hermes's lone
// .env would be seeded with a newer sign-in's .env and without its auth.json,
// which is half of that sign-in.
//
// A LOCAL box seeds these and writes a refreshed one back. A remote box must
// ignore them: they carry a refresh token whose rotation is shared fleet
// state, and pushing that onto somebody else's VM is a materially larger
// trust step than the scoped headless token the run env already exports.
// Empty for a profile that keeps no credential file.
func (p *Provider) SandboxCredentials() map[string]string {
	out := map[string]string{}
	for _, rel := range p.profile.CredentialPaths {
		out[rel] = p.ws.sharedPath(filepath.Base(rel))
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// SandboxCredentialDocument is the shared login a CLI keeps in a database of
// its own ([cliprofile.CredentialStore]) — the document its export printed —
// as an absolute path on the engine host, or "" when the profile keeps none.
//
// A PATH, NOT A BOX PATH: where a box keeps it is the coding runner's to say,
// because the runner is what imports it into the CLI's database before a run
// and exports back over it after, so the engine maps it there. It travels
// under the same rule as [Provider.SandboxCredentials] — seeded into a local
// box from the login as it stands then, and written back from one, never
// handed to a remote one — and is named whether or not the shared directory
// holds it now, for the same reason.
func (p *Provider) SandboxCredentialDocument() string {
	if p.profile.CredentialStore == nil {
		return ""
	}
	return p.ws.documentPath()
}

// SandboxEnv is the credential a coding run carries in its ENVIRONMENT — the
// headless subscription token, where one is configured.
//
// A token travels to any box, including a remote one, because it is a single
// scoped and revocable variable rather than a rotating refresh secret. An
// api-key entry contributes its key here instead, and a subscription entry
// with neither contributes nothing at all: the run then needs the credential
// files, which is why a CLI that mints no token needs a local box.
//
// A CLI that reads its model provider's key from its own environment
// ([cliprofile.Profile.EnvSignIn]) signs in through env, and that key
// travels for the same reason a token does — it is the whole sign-in, and a
// run that dropped it would start the CLI signed in to nothing while the
// doctor reported the entry healthy.
//
// Read off [childEnv] like [Provider.signIn], so a run carries exactly the
// credentials a host-mode run is given: what the auth mode removes from the
// child it removes from the box.
func (p *Provider) SandboxEnv() map[string]string {
	env := childEnv(p.profile, &Checkout{}, p.env, p.auth)
	out := map[string]string{}
	for _, name := range p.CredentialEnvNames() {
		if value := env[name]; strings.TrimSpace(value) != "" {
			out[name] = value
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// MintsHeadlessToken reports whether this CLI can produce a token that
// travels to a remote box, for the messages that offer `crewlet llm login
// -capture-token`: it has a minting command, and a token variable a token can
// be stored and referenced under ([Provider.TokenCanBeStored]). A token minted
// under any other could be neither, so a message that offered the command
// there sent the operator to a token nothing could hand the CLI.
func (p *Provider) MintsHeadlessToken() bool {
	return len(p.profile.CaptureTokenArgs) > 0 && p.TokenCanBeStored()
}

// CredentialEnvNames are the variables that authenticate this CLI inside a
// box: the headless token's and the API key's, and — for a CLI that reads its
// model provider's key from its own environment — the credential-named
// variables the entry's env sets.
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
	if p.profile.EnvSignIn {
		for _, name := range slices.Sorted(maps.Keys(p.env)) {
			if cliprofile.IsCredentialName(name) && !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	return names
}

// SignsInThroughEnv reports whether this CLI reads its model provider's key
// from its own environment, so the launch's refusal can name env as the
// route that travels to a remote box.
func (p *Provider) SignsInThroughEnv() bool { return p.profile.EnvSignIn }
