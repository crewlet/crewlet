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

	// Executor is what an entry a seat's EXECUTOR can launch on needs beyond
	// a login, and is empty for an entry no executor chain holds. Each half
	// of it fails at a seat's first launch and nowhere earlier: a CLI with no
	// in-box runner is skipped by every launch, and a box with no bridge to
	// dial gets none of the seat's tools.
	Executor []string

	// Checks is what the report measured, one per [CheckName] in the order
	// the report prints them, and the one account of it: [Diagnosis.Render]
	// prints the measured lines from it, Problems is derived from it, and
	// BinaryPath, Version, SignIn, TokenUsage, Smoke, LocalTools, Web and
	// Executor are its details as the fields have always carried them.
	// Structured because the API answers with it, and a client reading the
	// text would be parsing a report written for people.
	Checks []Check

	// Problems is every problem a check raised, in the order of Checks: what
	// `crewlet llm doctor` exits non-zero on.
	Problems []string
}

// CheckName is one thing the doctor measures.
type CheckName string

// The checks, in the order the report prints them.
const (
	// CheckBinary is whether the CLI is on this host's PATH.
	CheckBinary CheckName = "binary"
	// CheckVersion is whether the CLI prints the version its profile reads.
	CheckVersion CheckName = "version"
	// CheckSignIn is what signs the CLI in, read off the environment it is
	// handed and the login on disk.
	CheckSignIn CheckName = "sign-in"
	// CheckUsage is whether the profile reads the CLI's own token counts or
	// a budget runs on estimates.
	CheckUsage CheckName = "usage"
	// CheckSmoke is a real completion with a real tool, served as a seat's.
	CheckSmoke CheckName = "smoke"
	// CheckShell is whether the CLI ran its own shell, or read a file, on
	// the engine host, beside the profile's stance.
	CheckShell CheckName = "shell"
	// CheckWeb is whether the CLI fetched from the web with its own tool,
	// beside the entry's stance.
	CheckWeb CheckName = "web"
	// CheckExecutor is what a seat's executor launch on the entry needs
	// beyond a login, for an entry an executor chain holds.
	CheckExecutor CheckName = "executor"
)

// checkOrder is the order of the report's lines, and so of
// [Diagnosis.Checks] and [Diagnosis.Problems].
var checkOrder = []CheckName{
	CheckBinary, CheckVersion, CheckSignIn, CheckUsage, CheckSmoke, CheckShell, CheckWeb, CheckExecutor,
}

// Valid reports whether n is a check the doctor makes.
func (n CheckName) Valid() bool { return slices.Contains(checkOrder, n) }

// CheckKindConfig is the [Check.Kind] of a problem the entry's configuration
// or its CLI's build causes, which no later call settles: a remedy to apply,
// not a failure to wait out. The other kinds are a failed call's
// ([llm.ErrorKind]).
const CheckKindConfig = "config"

// Check is one measured line of a [Diagnosis].
type Check struct {
	Name CheckName
	// OK is that the check raised no problem. A probe that was skipped, or
	// that could not run because an earlier check failed, is OK: the check
	// that stopped it carries the problem, once.
	OK bool
	// Detail is what the check found, as the report's line prints it.
	Detail string
	// Kind is the first problem's kind: an [llm.ErrorKind]'s word (auth,
	// rate_limit, timeout, server, fatal) for a call that failed, or
	// [CheckKindConfig]. Empty when OK.
	Kind string
	// Remedy is the check's problems, each saying what is wrong and what
	// to change, one per line. Empty when OK.
	Remedy string

	// problems are the sentences Remedy joins, kept apart for Problems,
	// where each is an item of its own.
	problems []string
}

// checks gathers a diagnosis's checks as it measures them.
type checks map[CheckName]*Check

// at is the named check, created on first use.
func (c checks) at(name CheckName) *Check {
	check, ok := c[name]
	if !ok {
		check = &Check{Name: name}
		c[name] = check
	}
	return check
}

// set records what the check found.
func (c checks) set(name CheckName, detail string) { c.at(name).Detail = detail }

// fail raises one problem on the check. The kind is the first problem's:
// the one a caller acts on first.
func (c checks) fail(name CheckName, kind, problem string) {
	check := c.at(name)
	if len(check.problems) == 0 {
		check.Kind = kind
	}
	check.problems = append(check.problems, problem)
}

// finish puts the checks on d in the report's order, derives Problems from
// them, and fills the fields that carry their details.
func (c checks) finish(d *Diagnosis) {
	d.Checks, d.Problems = nil, nil
	for _, name := range checkOrder {
		check, ok := c[name]
		if !ok {
			continue
		}
		check.OK = len(check.problems) == 0
		if check.OK {
			check.Kind = ""
		}
		check.Remedy = strings.Join(check.problems, "\n")
		d.Checks = append(d.Checks, *check)
		d.Problems = append(d.Problems, check.problems...)
	}
	d.BinaryPath = d.detail(CheckBinary)
	d.Version = d.detail(CheckVersion)
	d.SignIn = d.detail(CheckSignIn)
	d.TokenUsage = d.detail(CheckUsage)
	d.Smoke = d.detail(CheckSmoke)
	d.LocalTools = d.detail(CheckShell)
	d.Web = d.detail(CheckWeb)
	d.Executor = d.executorLines()
}

// Check is the named check, and whether the diagnosis made it.
func (d Diagnosis) Check(name CheckName) (Check, bool) {
	for _, check := range d.Checks {
		if check.Name == name {
			return check, true
		}
	}
	return Check{}, false
}

// detail is the named check's Detail, "" for a check not made.
func (d Diagnosis) detail(name CheckName) string {
	check, _ := d.Check(name)
	return check.Detail
}

// executorLines are the executor check's lines, one per line of its Detail.
func (d Diagnosis) executorLines() []string {
	if detail := d.detail(CheckExecutor); detail != "" {
		return strings.Split(detail, "\n")
	}
	return nil
}

// DiagnoseOptions are the facts a provider cannot see about itself.
//
// They belong to the COMPANY and the PROCESS rather than to the provider —
// which seats launch their executor on this entry, which runners this build
// registers, and what a box can dial — and together they decide whether a
// seat's executor can run on it. Passed in rather than read here, so `doctor`
// reports the engine's own answers instead of this package's guess at them.
type DiagnoseOptions struct {
	// Smoke runs the real completion and the two tool probes.
	Smoke bool

	// ExecutorSeats are the agent seats whose executor chain holds this
	// entry, as the company resolves them. Empty means no seat's executor
	// can launch here, and the entry serves the phases on this host only.
	ExecutorSeats []string

	// AgentRunners are the in-box coding-agent runner names this build has.
	// An executor launch skips an entry whose CLI is outside them.
	AgentRunners []string

	// Cell is the cell a run launched on this entry is placed in — its own
	// run_in, else the company's default_run_in. A `direct` box on a node
	// that serves the API dials the node's own listener on loopback; every
	// other box dials BridgeURL.
	Cell string

	// BridgeURL is CREWLET_MCP_BRIDGE_URL as this process sees it. Empty
	// means a launch whose box cannot dial a listener of its own node's is
	// refused.
	BridgeURL string

	// Values are candidate credential values, by the variable the CLI is
	// handed each under: the profile's token_env or api_key_env, or a name
	// the entry's env sets. Each takes the place of what the entry resolved
	// for that variable, for this diagnosis alone, so a credential can be
	// tested before anything stores it. A name the entry does not hand its
	// CLI under its auth mode is a sign-in problem, never a value dropped
	// in silence: a test that passed without its candidate would vouch for
	// a credential nothing tried.
	//
	// A diagnosis with Values runs in Home, and in a home of its own made
	// and removed for it when Home is empty: a candidate tested beside the
	// entry's login would be tested with that login seeded next to it, and
	// whichever the CLI preferred would be what answered.
	Values map[string]string

	// Home is a state directory of the diagnosis's own, in place of the
	// entry's: its calls are seeded from no login but the one Home holds,
	// and whatever the CLI writes back lands there, never in the entry's
	// shared credentials. Whoever passes it owns it, and no other diagnosis
	// may use it at the same time.
	Home string
}

// Diagnose measures one provider end to end.
//
// It never returns an error: every failure it can find is a LINE in the
// report, because an operator running `doctor` wants the whole picture, and a
// command that stopped at the first problem would hide the three behind it.
//
// THE ENTRY'S RECORD LEARNS FROM IT ONLY WHEN IT RAN ON THE ENTRY'S SIGN-IN.
// Its probes are real calls, made through [Provider.Complete], so a diagnosis
// of the entry as it stands is recorded as any call is ([Provider.Plan]): an
// answer clears a refused sign-in, a refusal is remembered. One with Values or
// a Home runs on a sibling built for it ([Provider.candidate]), with a record
// of its own: what a candidate credential met says nothing about the
// credential the entry signs in with, and recorded on the entry it would clear
// a sign-in the vendor refuses, or sit out one that has quota left.
func (p *Provider) Diagnose(ctx context.Context, opts DiagnoseOptions) Diagnosis {
	if opts.Values == nil && opts.Home == "" {
		return p.diagnose(ctx, opts, nil)
	}
	home := opts.Home
	if home == "" {
		dir, err := os.MkdirTemp("", "crewlet-candidate-")
		if err != nil {
			return p.unisolated(fmt.Errorf("making a home of its own: %w", err))
		}
		defer func() {
			if rmErr := os.RemoveAll(dir); rmErr != nil {
				log.WarnContext(ctx, "cli_agent_candidate_cleanup_failed", "dir", dir, "error", rmErr)
			}
		}()
		home = dir
	}
	candidate, refused, err := p.candidate(home, opts.Values)
	if err != nil {
		return p.unisolated(err)
	}
	return candidate.diagnose(ctx, opts, refused)
}

// unisolated is the diagnosis of a candidate that could not be given a home of
// its own: nothing ran, since running it in the entry's would seed it beside
// the entry's login and write what the CLI refreshed back over it.
func (p *Provider) unisolated(err error) Diagnosis {
	d := Diagnosis{Provider: p.key, Agent: p.agent, Model: p.model,
		Binary: p.profile.Binary, WrittenFor: p.profile.WrittenFor}
	c := checks{}
	c.set(CheckSignIn, "not tested")
	c.fail(CheckSignIn, llm.KindFatal.String(), fmt.Sprintf("the candidate credentials for %q "+
		"were not tested: they run in a home of their own, and none could be made: %v", p.key, err))
	c.finish(&d)
	return d
}

// candidate is a sibling of this entry for a diagnosis that must not run on
// the entry's own sign-in: in a workspace at home, with values in place of
// what the entry resolved for their variables ([DiagnoseOptions.Values]), and
// with a [Plans] of its own, so nothing it meets is recorded on the entry.
// refused is a problem for each value that would reach nothing.
//
// BUILT FIELD BY FIELD, not copied: a provider holds the mutex its plan is
// read under, and a copy would share the entry's slots and plan. What is
// copied is what [New] made of the entry's configuration; what is fresh is
// what its calls change.
//
// THE WORKSPACE IS NOT [Shared]'s. That registry keeps one workspace per
// directory for the life of the process, so every entry on a directory locks
// one set of seats; a home made for one diagnosis and removed after it would
// stay in it for good. No other entry is on this directory, so there is no
// one to share it with.
func (p *Provider) candidate(home string, values map[string]string) (*Provider, []string, error) {
	key, err := keyOf(home, p.agent)
	if err != nil {
		return nil, nil, err
	}
	env := maps.Clone(p.env)
	if env == nil {
		env = map[string]string{}
	}
	auth := p.auth
	var refused []string
	placed := map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(values)) {
		value := values[name]
		switch {
		case name == p.profile.TokenEnv && name != "":
			auth.Token, auth.TokenConfigured = value, true
		case name == p.profile.APIKeyEnv && name != "":
			auth.APIKey = value
		default:
			if _, set := p.env[name]; !set {
				refused = append(refused, fmt.Sprintf("the candidate value for %s reaches nothing: "+
					"it is neither the %q CLI's token variable nor its key variable, and "+
					"providers.llm.%s.env does not set it", orNone(name), p.agent, p.key))
				continue
			}
			env[name] = value
		}
		placed[name] = true
	}
	c := &Provider{
		key: p.key, model: p.model, agent: p.agent, profile: p.profile,
		ws: &Workspace{
			stateDir: key.dir, root: filepath.Join(key.dir, key.agent), agent: key.agent,
			profile: p.profile, seats: map[string]*seatState{}, retries: map[string]carryRetry{},
		},
		env: env, auth: auth, timeout: p.timeout, termGrace: p.termGrace,
		slots: make(chan struct{}, cap(p.slots)),
		plans: NewPlans(nil), zone: p.zone, now: p.now,
	}
	// WHAT THE CHILD IS HANDED DECIDES, as it does for the entry's own
	// sign-in ([Provider.signIn]): a key given to a subscription entry, or
	// a token to an api-key one, is removed by the mode ([applyAuth]), and
	// a value inherit-env never reads stays where it was put.
	handed := childEnv(c.profile, &Checkout{}, c.env, c.auth)
	for _, name := range slices.Sorted(maps.Keys(placed)) {
		if handed[name] != values[name] {
			refused = append(refused, fmt.Sprintf("the candidate value for %s reaches nothing: "+
				"auth.mode %s does not hand the %q CLI that variable from the entry, so a test "+
				"with it would test the entry without it", name, c.AuthMode(), p.agent))
		}
	}
	return c, refused, nil
}

// diagnose is [Provider.Diagnose] on this provider as it stands, with refused
// the problems of candidate values that reach nothing.
func (p *Provider) diagnose(ctx context.Context, opts DiagnoseOptions, refused []string) Diagnosis {
	smoke := opts.Smoke
	d := Diagnosis{
		Provider: p.key, Agent: p.agent, Model: p.model,
		Binary: p.profile.Binary, WrittenFor: p.profile.WrittenFor,
		StateDir: p.ws.StateDir(),
	}
	c := checks{}
	if lines, problems := p.executor(opts); len(lines) > 0 {
		c.set(CheckExecutor, strings.Join(lines, "\n"))
		for _, problem := range problems {
			c.fail(CheckExecutor, CheckKindConfig, problem)
		}
	}

	path, err := exec.LookPath(p.profile.Binary)
	switch {
	case err != nil:
		c.set(CheckBinary, "not on PATH")
		c.set(CheckVersion, "")
		c.fail(CheckBinary, llm.KindFatal.String(), fmt.Sprintf(
			"%q is not on this host's PATH — the CLI runs on the ENGINE host, so it "+
				"must be installed here; set overrides.binary to an absolute path "+
				"if it lives somewhere unusual", p.profile.Binary))
	default:
		c.set(CheckBinary, path)
		version := p.probeVersion(ctx)
		c.set(CheckVersion, version)
		if version == "" {
			c.fail(CheckVersion, CheckKindConfig, fmt.Sprintf(
				"%s %s printed no version — the profile may not match this build; "+
					"it was written for %s", path, strings.Join(p.profile.VersionArgs, " "),
				orNone(p.profile.WrittenFor)))
		}
	}

	for _, problem := range refused {
		c.fail(CheckSignIn, CheckKindConfig, problem)
	}
	login, loginErr := p.ws.Login(ctx)
	loggedOut, loggedOutErr := p.ws.loggedOut()
	switch {
	case loginErr != nil:
		d.Credentials = "unknown — the shared login could not be read"
		c.fail(CheckSignIn, llm.KindFatal.String(), fmt.Sprintf("the shared login in %s could not be read, "+
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
		d.HostLogin = p.HostLogin(ctx, "")
	}
	if loggedOutErr != nil {
		c.fail(CheckSignIn, llm.KindFatal.String(), fmt.Sprintf("whether the entry was logged out cannot be "+
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
		c.fail(CheckSignIn, CheckKindConfig, p.misnamedProblem(variable))
	}

	if p.profile.ReadsUsage() {
		c.set(CheckUsage, "reported by CLI")
	} else {
		c.set(CheckUsage, "estimated (4 characters per token)")
		c.fail(CheckUsage, CheckKindConfig, fmt.Sprintf(
			"the %q profile reads no usage figures, so budgets for seats on this "+
				"provider run on estimates", p.agent))
	}

	d.SystemPrompt = []string{
		"seats: " + describeRoute(p.profile.RouteFor(true)),
		"side passes: " + describeRoute(p.profile.RouteFor(false)),
	}

	signIn := p.signInFrom(login, loginErr)
	c.set(CheckSignIn, signIn.line())
	answered := false

	stance := p.localToolsStance()
	web := p.webStance()
	switch {
	case !smoke:
		c.set(CheckSmoke, "skipped (-no-smoke)")
		c.set(CheckShell, stance+"; probe skipped (-no-smoke)")
		c.set(CheckWeb, web+"; probe skipped (-no-smoke)")
	case err != nil:
		c.set(CheckSmoke, "skipped: no binary to run")
		c.set(CheckShell, stance+"; probe skipped, no binary to run")
		c.set(CheckWeb, web+"; probe skipped, no binary to run")
	default:
		outcome := p.smoke(ctx)
		answered = outcome.answered
		c.set(CheckSmoke, outcome.verdict)
		if outcome.kind != "" {
			c.fail(CheckSmoke, outcome.kind, outcome.verdict)
		}
		verdict, problem := p.shellProbe(ctx)
		if verdict == refusedVerdict {
			// A refused shell is half the stance: the file tools read
			// the host as well as a shell does, and the deny list that
			// refused one says nothing about the other.
			verdict, problem = p.fileProbe(ctx)
		}
		c.set(CheckShell, stance+"; "+verdict)
		if problem != "" {
			c.fail(CheckShell, CheckKindConfig, problem)
		}
		verdict, problem, kind := p.webProbe(ctx)
		c.set(CheckWeb, web+"; "+verdict)
		if problem != "" {
			c.fail(CheckWeb, kind, problem)
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
			c.set(CheckSignIn, d.SignIn)
		} else {
			c.fail(CheckSignIn, CheckKindConfig, p.noSignIn(d.HostLogin, signIn.unresolved))
		}
	}
	c.finish(&d)
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

// executor measures what an entry a seat's executor launches on needs beyond
// a login.
//
// BOTH HALVES FAIL AT A SEAT'S FIRST LAUNCH AND NOWHERE EARLIER, which is the
// whole reason they are here: an engine with no reachable bridge URL refuses
// every launch at the moment a seat finally has work, and a CLI with no in-box
// runner is skipped by every launch while validating cleanly as a provider.
// `doctor` is what a deploy script gates on, so it is the last place either
// can be caught before an agent is waiting.
//
// A NON-RUNNER MEMBER IS A LINE, NOT A PROBLEM. The company refuses an agent
// seat whose executor chain holds no runner at all, so a CLI without one sits
// in a chain beside one that has it — skipped at launch, and still the seat's
// model for every phase on this host the chain serves. Faulting it would make `doctor`
// red on a configuration the engine runs.
//
// AN ENTRY NO EXECUTOR CHAIN HOLDS REPORTS NOTHING HERE: it serves the phases
// on this host, as a subprocess of this engine, and needs neither.
func (p *Provider) executor(opts DiagnoseOptions) (lines, problems []string) {
	if len(opts.ExecutorSeats) == 0 {
		return nil, nil
	}
	lines = append(lines, "seats: "+strings.Join(opts.ExecutorSeats, ", "))
	if !slices.Contains(opts.AgentRunners, p.agent) {
		lines = append(lines, fmt.Sprintf(
			"runner: none for %q — skipped at launch (runners: %s); serves "+
				"those seats' phases on this host only",
			p.agent, strings.Join(opts.AgentRunners, ", ")))
		return lines, nil
	}
	lines = append(lines, fmt.Sprintf("runner: %q is registered", p.agent))
	switch {
	case opts.Cell == directCell:
		// ON THE HOST, so on a node that binds a listener the box reaches
		// on loopback — api.port, on every interface or on loopback, which
		// a node without the ingress role serves its own host alone — the
		// node's own listener is the address, derived by the node that
		// binds it: the variable is not needed there, and a report calling
		// it missing would send an operator to set what the quickstart and
		// a seat pool never ask for. Whether this node is one is its Tier
		// A, which this command does not read; a seats node that is not
		// says so itself at apply.
		via := "a node on api.port 0 dials " + bridgeURLVar
		if url := strings.TrimSpace(opts.BridgeURL); url != "" {
			via = "a node on api.port 0 dials " + url
		}
		lines = append(lines, "tool bridge: this node's own listener, on loopback, "+
			"where the node binds api.port (on every interface or on loopback); "+via)
	case strings.TrimSpace(opts.BridgeURL) != "":
		lines = append(lines, "tool bridge: "+opts.BridgeURL)
	default:
		lines = append(lines, "tool bridge: unset")
		problems = append(problems, fmt.Sprintf(
			"a seat's executor reaches its tools over an MCP bridge, and a box in "+
				"the %s cell is not on the node's host, so it dials %s alone — which "+
				"is unset, so every launch on providers.llm.%s is refused: a coding "+
				"agent with none of the seat's tools cannot answer anybody or submit "+
				"its work. Set it to a URL that box reaches the node at",
			orNone(opts.Cell), bridgeURLVar, p.key))
	}
	return lines, problems
}

// directCell is the cell whose box is a process tree on the engine's own host
// — config.PlacementDirect, spelled here for [bridgeURLVar]'s reason.
const directCell = "direct"

// bridgeURLVar is the variable an executor's box dials the engine on.
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
//
// The third return is the problem's [Check.Kind]: the failed call's for a
// probe that could not run, else [CheckKindConfig]; empty with no problem.
func (p *Provider) webProbe(ctx context.Context) (verdict, problem, kind string) {
	denied := p.profile.EffectiveWebTools() == cliprofile.WebToolsDenied
	comp, err := p.Complete(ctx, probeRequest(webProbePrompt))
	if err != nil {
		return "probe failed: " + err.Error(), "the web probe failed: " + err.Error(), llm.KindOf(err).String()
	}
	fetched := reportsCurrentClock(comp.Content, time.Now())
	switch {
	case fetched && denied:
		return "probe: FETCHED " + webProbeURL, fmt.Sprintf(
			"the entry denies the %q CLI its web tools, but the CLI fetched %s with "+
				"its own web tool: the profile's web_denial is not taking effect on "+
				"this build (%s); check overrides.web_denial against the installed "+
				"version before an assistant runs on this entry",
			p.agent, webProbeURL, orNone(p.profile.WrittenFor)), CheckKindConfig
	case fetched:
		return "ok: fetched " + webProbeURL, "", ""
	case strings.TrimSpace(comp.Content) == "":
		// Same split as smokeTest, and for the same reason: an empty
		// answer is a completion now, so "it said: \"\"" would send an
		// operator to the egress proxy, or past a denial that may not
		// hold, for a model that never spoke.
		return "probe inconclusive", fmt.Sprintf(
			"the web probe is inconclusive: the %q CLI exited 0 and answered with "+
				"nothing (%d output tokens billed), so it says nothing about web access "+
				"either way. Point this entry at a stronger model and run the doctor again",
			p.agent, comp.OutputTokens), CheckKindConfig
	case denied:
		return "probe: refused", "", ""
	}
	// WHAT IT SAID, WHOLE: the reader is an operator deciding what to fix,
	// and a refusal's reason — a sandbox flag, a proxy's answer — is rarely
	// in its first hundred and twenty bytes, which is where this used to stop.
	return "failed", fmt.Sprintf(
		"the %q CLI could not fetch %s with its own web tool (it said: %q). "+
			"Web is meant to stay on for every subscription seat: check that no vendor "+
			"sandbox flag cuts the network and that the egress proxy reaches the child "+
			"environment (env / passthrough_env)",
		p.agent, webProbeURL, strings.TrimSpace(comp.Content)), CheckKindConfig
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
	outcome := p.smoke(ctx)
	return outcome.verdict, outcome.answered
}

// smokeOutcome is what [Provider.smokeTest] found, and the kind of its
// failure: empty for a pass, the failed call's where the call itself failed,
// and [CheckKindConfig] for a CLI that ran and fell short of the route.
type smokeOutcome struct {
	verdict  string
	answered bool
	kind     string
}

// smoke is [Provider.smokeTest] with its failure's kind.
func (p *Provider) smoke(ctx context.Context) smokeOutcome {
	var err error
	verdict, answered := p.smokeRun(ctx, &err)
	outcome := smokeOutcome{verdict: verdict, answered: answered}
	switch {
	case !strings.HasPrefix(verdict, "failed"):
	case err != nil:
		outcome.kind = llm.KindOf(err).String()
	default:
		outcome.kind = CheckKindConfig
	}
	return outcome
}

// smokeRun is [Provider.smokeTest]'s run. It sets failed to the error of a
// run that did not get as far as an answer.
func (p *Provider) smokeRun(ctx context.Context, failed *error) (verdict string, answered bool) {
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
		*failed = err
		return "failed — this process cannot serve a tool session: " + err.Error(), false
	}
	defer func() { _ = sessions.Close() }()
	host := newSmokeHost(sessions)
	var offered []string
	comp, err := p.Complete(withOffered(ctx, func(tools []string) { offered = tools }), smokeRequest(host))
	if err != nil {
		*failed = err
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
//
// The measured lines are the checks' ([Diagnosis.Checks]), and the rest the
// facts beside them: one account, printed for people here and answered as
// structure by the API.
func (d Diagnosis) Render(w io.Writer) {
	line := func(label, value string) {
		fmt.Fprintf(w, "%-14s: %s\n", label, value)
	}
	lines := func(label string, values []string) {
		for i, value := range values {
			if i > 0 {
				label = ""
			}
			line(label, value)
		}
	}
	line("provider", d.Provider)
	line("cli agent", d.Agent)
	if d.Model != "" {
		line("model", d.Model)
	}
	line("binary", orNone(d.detail(CheckBinary)))
	line("version", orNone(d.detail(CheckVersion)))
	line("written for", orNone(d.WrittenFor))
	line("state dir", d.StateDir)
	line("credentials", d.Credentials)
	if len(d.HostLogin) > 0 {
		line("host login", strings.Join(d.HostLogin, ", ")+" (not adopted)")
	}
	line("token env", d.TokenEnv)
	line("sign-in", d.detail(CheckSignIn))
	line("token usage", d.detail(CheckUsage))
	lines("system prompt", d.SystemPrompt)
	line("smoke test", d.detail(CheckSmoke))
	line("local tools", d.detail(CheckShell))
	line("web", d.detail(CheckWeb))
	lines("executor", d.executorLines())
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

// SandboxEnv is the environment a coding run on this entry carries into its
// box, in any cell: the entry's own env, and the credential its auth mode signs
// the CLI in with — the headless subscription token where one is configured,
// an api-key entry's key, the variables inherit-env forwards.
//
// THE WHOLE ENV, NOT ONLY ITS SIGN-IN. An entry's env is extra environment for
// every invocation of its CLI, and a seat's executor run is one: a gateway's
// ANTHROPIC_BASE_URL and the bearer token it takes, a Bedrock or Vertex AI
// switch and its region, a proxy. Cut to the credential names, a box run on
// an entry that reached its model through a gateway called the vendor directly
// with the gateway's key, and one on Bedrock called the Anthropic API signed
// in to nothing, while every other phase of the same entry worked.
//
// A token or a key travels to any box, including a remote one, because it is a
// single scoped and revocable variable rather than a rotating refresh secret.
// A subscription entry with neither contributes no credential at all: the run
// then needs the credential files, which is why a CLI that mints no token
// needs a local box.
//
// Read off [childEnv] like [Provider.signIn], so a run carries what a run on
// this host is given of the entry: what the auth mode removes from the child
// it removes from the box, and what the mode sets in the child — the
// credential, and [cliprofile.Profile.SubscriptionEnv] — it sets in the box.
// The rest of that environment is this host's and stays on it: the host
// allowlist (a direct box is given its own, and a remote one is another
// machine), the isolation variables (a box points HOME and the XDG
// directories at its own home, and an entry's env naming one is refused on
// every write but only warned about where it is applied), and the profile's
// fixed env and passthrough, which tune a call on this host — a run in a box
// is configured by its runner (internal/sandbox/codingagent).
//
// TZ travels with it: the zone the engine reads the run's reset in.
//
// A BLANK VALUE IS LEFT OUT, by the rule the rest of a run's environment is
// held to: a reference that resolved to nothing reaches no box as an empty
// string, and its build already warned (cli_agent_env_unresolved).
func (p *Provider) SandboxEnv() map[string]string {
	env := childEnv(p.profile, &Checkout{}, p.env, p.auth)
	names := slices.Collect(maps.Keys(p.env))
	names = append(names, p.profile.TokenEnv, p.profile.APIKeyEnv)
	names = append(names, slices.Collect(maps.Keys(p.profile.SubscriptionEnv))...)
	// THE ZONE TRAVELS TOO ([childZone]): a run in a box reports its limit
	// in its own zone, and the engine reads a reset the CLI names without
	// one in the zone it handed the run, never in whatever the box's image
	// defaults to.
	names = append(names, "TZ")
	isolation := p.profile.IsolationVariables()
	out := map[string]string{}
	for _, name := range names {
		if name == "" || slices.Contains(isolation, name) {
			continue
		}
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
// box: the headless token's and the API key's, the names it signs in by from an
// entry's env, and, for a CLI that reads its
// model provider's key from its own environment — the credential-named
// variables the entry's env sets.
//
// Exported because the launch has to answer a question only it can — whether
// ANYTHING in the run environment authenticates, including a value the
// OPERATOR declared in role.sandbox.env. The engine names no tool-specific
// variable of its own, so it cannot recognise a credential by inspection; what
// it can do is ask the profile which names count and look for those.
func (p *Provider) CredentialEnvNames() []string {
	names := make([]string, 0, 2+len(p.profile.SignInEnv))
	for _, name := range []string{p.profile.TokenEnv, p.profile.APIKeyEnv} {
		if name != "" {
			names = append(names, name)
		}
	}
	// The names the profile signs in by from an entry's env
	// ([cliprofile.Profile.SignInEnv]): a cloud provider's switch or a
	// gateway's token authenticates a box as surely as a key does.
	for _, name := range p.profile.SignInEnv {
		if !slices.Contains(names, name) {
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
