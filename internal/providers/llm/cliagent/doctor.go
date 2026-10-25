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

// fileProbePrompt asks the CLI to read a file on the engine host, the path
// substituted for %s. The file holds a value minted for the one run, so a
// model can echo it only by reading it.
const fileProbePrompt = "Using your file-reading tool, read the file at %s and reply with " +
	"only its contents, nothing else.\n" +
	"If you have no tool that can read files, reply with only: " + noLocalToolsReply

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
	// StreamSmoke is the same completion through the profile's streamed
	// variant ([cliprofile.Stream]), the run a Dashboard Assistant answer
	// makes; empty where the profile has no variant.
	StreamSmoke string
	// LocalTools is the profile's stance on the CLI's own shell and file
	// tools, beside what the shell probe measured.
	LocalTools string
	// Web is the entry's web stance beside whether the CLI's own fetch tool
	// reached the web, measured. On a denied entry a fetch that succeeded is
	// a problem, as a shell that ran is on a profile denying local tools.
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
		StateDir: p.ws.StateDir(),
	}
	d.AgentRuntime, d.Problems = p.agentRuntime(opts)

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

	files := p.ws.LoginFiles()
	loggedOut, loggedOutErr := p.ws.loggedOut()
	switch {
	case len(files) > 0:
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
		d.HostLogin = p.HostLogin(ctx, "")
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

	signIn := p.signIn()
	d.SignIn = signIn.line()
	answered := false

	stance := p.localToolsStance()
	web := p.webStance()
	switch {
	case !smoke:
		d.Smoke = "skipped (-no-smoke)"
		d.LocalTools = stance + " — probe skipped (-no-smoke)"
		d.Web = web + "; probe skipped (-no-smoke)"
	case d.BinaryPath == "not on PATH":
		d.Smoke = "skipped — no binary to run"
		d.LocalTools = stance + " — probe skipped, no binary to run"
		d.Web = web + "; probe skipped, no binary to run"
	default:
		d.Smoke, answered = p.smokeTest(ctx, smokeRequest())
		if strings.HasPrefix(d.Smoke, "failed") {
			d.Problems = append(d.Problems, d.Smoke)
		}
		// THE STREAMED VARIANT, measured as a round of its own where the
		// profile has one: it is another argv and another output shape,
		// and the run that proves it reads back is what any caller beyond
		// the Dashboard Assistant waits on before it asks for the token
		// stream.
		if p.profile.CanStream() {
			var streamed bool
			d.StreamSmoke, streamed = p.smokeTest(ctx, tokenStreamed(smokeRequest()))
			answered = answered || streamed
			if strings.HasPrefix(d.StreamSmoke, "failed") {
				d.Problems = append(d.Problems, "streamed: "+d.StreamSmoke)
			}
		}
		verdict, problem := p.shellProbe(ctx)
		if verdict == refusedVerdict {
			// A refused shell is half the stance: the file tools read
			// the host as well as a shell does, and the deny list that
			// refused one says nothing about the other.
			verdict, problem = p.fileProbe(ctx)
		}
		d.LocalTools = stance + " — " + verdict
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
	if len(signIn.sources) == 0 {
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
	comp, err := p.Complete(ctx, tokenStreamed(probeRequest(shellProbePrompt)))
	if err != nil {
		return "probe failed — " + err.Error(), ""
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
// exact argv the Dashboard Assistant runs ([tokenStreamed]). The second
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
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "file probe failed: " + err.Error(), ""
	}
	comp, err := p.Complete(ctx, tokenStreamed(probeRequest(fmt.Sprintf(fileProbePrompt, path))))
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
	comp, err := p.Complete(ctx, tokenStreamed(probeRequest(webProbePrompt)))
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

// smokeTest runs a REAL completion with a REAL tool.
//
// The command that matters, and the reason it is not merely a version probe:
// a profile can look perfect — binary present, login present, flags accepted
// — and still not produce a parseable tool call, because the envelope
// contract is a request to a model rather than a schema the vendor enforces.
// That failure only shows up on the first turn of a real seat otherwise.
//
// THE PRODUCTION SHAPE, and nothing stronger: tools offered, no call forced,
// and the instruction naming the tool in the conversation — which is exactly
// what a phase that finishes by a call sends, its corrective round included.
// It used to force a call, which rendered the envelope's "you MUST request a
// tool call" contract that no phase ever receives; a CLI that only called a
// tool when told it must would have passed here and failed every seat.
//
// answered reports whether the model replied at all, however badly: a reply
// is proof the CLI reached its model signed in, which the doctor needs apart
// from whether the reply was usable.
func (p *Provider) smokeTest(ctx context.Context, req llm.Request) (verdict string, answered bool) {
	comp, err := p.Complete(ctx, req)
	if err != nil {
		return "failed — " + err.Error(), false
	}
	if len(comp.ToolCalls) == 0 {
		// TWO DIFFERENT FAILURES, and `It said: ""` describes only one of
		// them. A CLI that answered with prose misread the envelope
		// contract; a CLI that answered with NOTHING spent its output on
		// hidden reasoning and needs a different model, not a different
		// prompt. The provider hands both back as a completion now — an
		// empty answer is a value, not an error — so this probe is the
		// only command that names which one an operator has.
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
			"failed — the CLI answered but produced no parseable tool call, so every phase "+
				"a seat runs on this provider will spend a corrective round asking again, and "+
				"end without its submission whenever the model never manages one. It said: %q",
			strings.TrimSpace(comp.Content)), true
	}
	return fmt.Sprintf("ok — %d in / %d out", comp.InputTokens, comp.OutputTokens), true
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
// tool offered, and the instruction that names it.
func smokeRequest() llm.Request {
	req := probeRequest(smokePrompt)
	req.Tools = []llm.ToolDef{{
		Name:        "crewlet_smoke",
		Description: "Confirm the tool channel works.",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"ok": map[string]any{"type": "boolean"}},
			"required":   []any{"ok"},
		},
	}}
	return watched(req)
}

// watched is a request as a seat's round sends it: streamed for the live
// view, on the profile as written.
func watched(req llm.Request) llm.Request {
	req.OnDelta = func(llm.Delta) {}
	return req
}

// tokenStreamed is a request as a Dashboard Assistant answer sends it: watched
// and asking for the token stream, so it runs the profile's streamed variant
// where it has one ([Provider.profileFor]). The isolation probes send this,
// because the assistant is the caller whose admission rests on the web being
// denied, and the variant's argv only adds to the profile's own; a probe on
// the profile as written would prove the denial on an argv no assistant call
// runs.
func tokenStreamed(req llm.Request) llm.Request {
	req = watched(req)
	req.StreamTokens = true
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
	if d.StreamSmoke != "" {
		line("stream smoke", d.StreamSmoke)
	}
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

// SignInState is a one-word summary for `crewlet llm list`: the first route
// the doctor's sign-in line names, or "none" — the same answer, so the two
// commands cannot disagree about whether an entry is signed in.
func (p *Provider) SignInState() string { return p.signIn().state() }

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

// SandboxCredentialDocument is the shared login a CLI keeps in a database of
// its own ([cliprofile.CredentialStore]) — the document its export printed —
// as an absolute path on the engine host, or "" when the profile keeps none or
// nobody has signed the entry in.
//
// A PATH, NOT A BOX PATH: where a box keeps it is the coding runner's to say,
// because the runner is what imports it into the CLI's database before a run
// and exports back over it after, so the engine maps it there. It travels
// under the same rule as [Provider.SandboxCredentials] — seeded into a local
// box and written back from one, never handed to a remote one.
func (p *Provider) SandboxCredentialDocument() string {
	if p.profile.CredentialStore == nil {
		return ""
	}
	path := p.ws.documentPath()
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return path
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
// credentials a text-mode call is given: what the auth mode removes from the
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
// travels to a remote box, for the error that has to distinguish the two
// cases an operator can be in.
func (p *Provider) MintsHeadlessToken() bool {
	return p.profile.TokenEnv != "" && len(p.profile.CaptureTokenArgs) > 0
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
