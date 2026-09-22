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
	"github.com/crewlet/crewlet/internal/textcut"
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

// saidShown is how much of the CLI's own answer a failed probe quotes back.
//
// ONE CONSTANT FOR BOTH PROBES, because it is one gesture asked twice: the
// smoke test and the web probe each failed, and each is showing the operator
// what the CLI said INSTEAD, so they can tell which failure they have. The two
// sites carried 120 and 200 as bare literals, in a file that names every other
// value it turns on — probeTimeout, probeSkew, noWebReply — and neither number
// said anything the other did not. Two undeclared answers to one question is
// how the pair drifts apart and how the next reader inherits a mystery instead
// of a reason.
//
// FOUR HUNDRED BYTES, reached on the measured evidence below and then found to
// be the answer [github.com/crewlet/crewlet/internal/httpx.RefusalDetail]
// already gives to the same question — what does a reader need from a party
// that refused?
//
// THAT IS RefusalDetail AND NOT
// [github.com/crewlet/crewlet/internal/httpx.RefusalBytes], which is 2 KiB and
// answers a different question: how much of a refused body is worth READING at
// all, before anything is rendered from it. Naming the wrong one of the two
// would send a reader to a number this constant has no relationship with.
//
// The agreement is CORROBORATION, not a dependency: the constant is
// deliberately not imported, because this is a child process's stdout rather
// than an HTTP response body, and a provider that spawns a process has no
// business reaching into the outbound transport for one integer. But a comment
// asserting two constants match with nothing enforcing it is the precise drift
// httpx's own package doc was written about, so
// TestSaidShownAgreesWithTheRefusalDetailItCites holds them together and fails
// the build if either moves — the same guard [bridgeURLVar] takes for the same
// reason, and for none of the coupling.
//
// The number is anchored to the one finding each quote exists to produce, and
// both are measured rather than guessed — see
// TestTheProbeQuoteHoldsTheFindingItExistsToShow, which holds this constant
// against the first of them:
//
//   - The SMOKE finding is a CLI that answered with an envelope of the WRONG
//     SHAPE — a JSON block naming a tool and its arguments under keys this
//     build does not look under — which is what sends an operator to
//     providers.llm.<key>.cli.overrides rather than to a bigger model. The
//     shape is not hypothetical: [renderPriorCalls] renders exactly it, and
//     for the smoke tool's own one-argument call the fenced block is 129
//     bytes. The old 120 could not hold the block AT ALL, and the old 200 cut
//     it the moment a model put one sentence in front of it — 202 bytes
//     measured, which is what a model does more often than not. With a
//     sentence either side it is 260, and that is the figure the budget has
//     to clear rather than the bare block's.
//   - The WEB finding is a CLI explaining why it could not fetch, in prose,
//     instead of answering noWebReply. A realistic one ("I attempted to fetch
//     the URL but the request failed: the sandbox in this environment blocks
//     outbound network access, so no web fetch tool is available to me here.")
//     is 162 bytes — past 120, which is why that site's own budget was the
//     tighter of the two for the looser finding.
//
// 400 carries both whole with room for the sentence that frames them, and
// still keeps the quote inside the five 80-column lines a fixed-width report
// can spend on one finding.
//
// THE WHOLE ANSWER IS RECOVERABLE FROM THE RUN THAT CUT IT, on the same page
// the quote is on: [probeQuote] hands the unabridged reply back as a
// [ProbeAnswer], [Diagnose] carries it, and [Diagnosis.Render] prints it under
// `probe replies` at the foot of the report. NOT A DEBUG LOG LINE, which is
// the obvious destination and the wrong one, because its lever is PROSPECTIVE
// ONLY: `doctor` takes no `-log-level` flag and reads no `logging:` block, and
// operatorLogLevel in cmd/crewlet leaves every non-`run` command at warn
// unless $CREWLET_LOG_LEVEL was exported BEFORE the run — which asks an
// operator to have predicted this failure. A lever that turns up the NEXT run
// cannot reach the operator already reading this one's `…`, and re-running
// spends three more real completions off their subscription on a NEW reply a
// nondeterministic model would not repeat. The rule is that the value cut is
// reachable, not that an equivalent is re-purchasable.
//
// It is carried UNBOUNDED, deliberately: a recovery path that is itself cut
// recovers nothing. It is bounded upstream anyway, by [maxOutput] and by
// [Provider.completion] refusing a stdout that reached it.
const saidShown = 400

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

	// Answers holds, WHOLE, every probe reply a problem line above quoted
	// only the opening of. Empty when nothing was cut, which is the usual
	// case: a probe that passed quotes nothing at all.
	//
	// It is on the struct rather than in a log line because that is what
	// makes the cut recoverable from THIS run — see [saidShown] for what
	// the log route could not deliver — and because a rendered report is
	// assertable where a process-wide log sink is not (see [probeQuote]).
	Answers []ProbeAnswer
}

// ProbeAnswer is one probe's whole reply, kept beside the report line that
// quoted its opening.
//
// A named type rather than a bare string so the report can say WHICH probe it
// belongs to and how much of it was already shown: a `doctor` run can fail
// both the smoke test and the web probe, and two unlabelled blocks of prose at
// the foot of a report would be two things a reader has to match back to the
// lines above by eye.
type ProbeAnswer struct {
	// Probe is "smoke" or "web" — the probe whose problem line carries the
	// quote this is the whole of.
	Probe string
	// Shown is how many bytes of Answer that line quoted, so the report
	// states the same figure the problem line did rather than a second
	// one derived somewhere else.
	Shown int
	// Answer is the reply, unabridged. Unbounded on purpose: a recovery
	// path that is itself cut recovers nothing.
	Answer string
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
		version, problem := p.probeVersion(ctx)
		d.Version = version
		switch {
		case problem != "":
			// THE PROBE'S OWN REFUSAL WINS over "printed no version",
			// which would be the wrong sentence for a CLI that printed
			// 32 MiB of it: the two failures send an operator to
			// different fields.
			d.Problems = append(d.Problems, problem)
		case d.Version == "":
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

	if p.profile.ReadsUsage() {
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
		var kept *ProbeAnswer
		d.Smoke, kept = p.smokeTest(ctx)
		d.Answers = appendAnswer(d.Answers, kept)
		if strings.HasPrefix(d.Smoke, "failed") {
			d.Problems = append(d.Problems, d.Smoke)
		}
		verdict, problem := p.shellProbe(ctx)
		d.LocalTools = stance + " — " + verdict
		if problem != "" {
			d.Problems = append(d.Problems, problem)
		}
		d.Web, kept = p.webProbe(ctx)
		d.Answers = appendAnswer(d.Answers, kept)
		if strings.HasPrefix(d.Web, "failed") {
			d.Problems = append(d.Problems, d.Web)
		}
	}
	return d
}

// appendAnswer collects a probe's kept reply, and drops the nil that means
// nothing was cut.
//
// A helper rather than an `if kept != nil` at each of the two call sites,
// because the pairing is the rule this whole path exists for: a probe hands
// back its line and whatever that line had to shorten TOGETHER, and the one
// thing a later edit must not be able to do is keep the first and quietly drop
// the second.
func appendAnswer(into []ProbeAnswer, kept *ProbeAnswer) []ProbeAnswer {
	if kept == nil {
		return into
	}
	return append(into, *kept)
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
// The second return is the CLI's whole reply where the verdict quoted only its
// opening, and nil where it quoted all of it — see [probeQuote].
func (p *Provider) webProbe(ctx context.Context) (string, *ProbeAnswer) {
	comp, err := p.Complete(ctx, llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: webProbePrompt}},
	})
	if err != nil {
		return "failed — " + err.Error(), nil
	}
	if reportsCurrentClock(comp.Content, time.Now()) {
		return "ok — fetched " + webProbeURL, nil
	}
	// Same split as smokeTest, and for the same reason: an empty answer is
	// a completion now, so "it said: \"\"" would send an operator to the
	// egress proxy for a model that never spoke.
	if strings.TrimSpace(comp.Content) == "" {
		return fmt.Sprintf(
			"failed — the %q CLI exited 0 and answered with nothing (%d output tokens "+
				"billed), so this probe says nothing about web access either way. "+
				"Point this entry at a stronger model and run the doctor again",
			p.agent, comp.OutputTokens), nil
	}
	said, rest, kept := probeQuote("web", comp.Content)
	return fmt.Sprintf(
		"failed — the %q CLI could not fetch %s with its own web tool (it said: %q).%s "+
			"Web is meant to stay on for every subscription seat: check that no vendor "+
			"sandbox flag cuts the network and that the egress proxy reaches the child "+
			"environment (cli.env / passthrough_env)",
		p.agent, webProbeURL, said, rest), kept
}

// probeQuote splits a probe's reply into the bounded quote a problem line
// shows, the clause that says where the rest of it is, and the whole answer
// the report carries.
//
// ONE FUNCTION RETURNING ALL THREE, because the pairing IS the rule: the quote
// is allowed to be cut only because the whole of it goes somewhere reachable
// in the same breath, and written as separate steps a later edit can keep the
// cut and drop the recovery without anything looking wrong. Here it cannot —
// there is no way to get `shown` without also being handed what it was cut
// from and the sentence that points at it.
//
// THE DESTINATION IS THIS RUN'S OWN REPORT. [Diagnose] puts the returned
// [ProbeAnswer] on [Diagnosis] and [Diagnosis.Render] prints it under `probe
// replies`, so the operator reading the `…` has the rest on the same page.
// A DEBUG LOG LINE IS NOT SUCH A DESTINATION, which is the obvious
// alternative and the wrong one: `doctor` is not `run` — it takes no
// `-log-level` flag and reads no `logging:` block, and operatorLogLevel in
// cmd/crewlet leaves every non-`run` command at warn unless $CREWLET_LOG_LEVEL
// was exported BEFORE the invocation. That escape hatch turns up the NEXT run,
// never the one whose output is already on the screen, so it would leave a
// marker pointing at nothing an operator can reach on the run that took the
// cut — and getting an answer back would mean re-running and buying three more
// real completions off their subscription for a reply a nondeterministic model
// would not repeat. Rule: the value that was cut is reachable, not that an
// equivalent is re-purchasable.
//
// PURE, which that route could not be. The only way to assert on a log line is
// to point the process-wide sink at a test's own buffer, which
// [github.com/crewlet/crewlet/internal/logging.Configure]'s own doc records as
// a measured failure (29 parallel tests racing one global writer) and this
// package runs its cases in parallel — so the recovery half of this pairing
// would be the one no test could reach. A rendered report has no such problem.
//
// A REPORT LINE RATHER THAN A SECOND MODEL CALL, which is the obvious
// alternative and the wrong one: summarising a failed probe's reply would
// spend another completion off the operator's subscription, add a round trip
// to a command that already spends three, and ask a model to explain a failure
// at the exact moment the evidence says that model cannot be trusted to
// answer.
//
// The clause is returned SEPARATELY rather than folded into the quote because
// the quote is rendered with %q at both call sites: a sentence inside it would
// be escaped and read as part of what the CLI said, which is the one thing
// this value must not gain.
//
// The probes' prompts are the constants above and carry no company content, so
// what the report carries is a CLI's reply to a fixed question about the clock
// — there is no seat's work in it to leak into a report an operator pastes
// into an issue.
func probeQuote(probe, content string) (shown, rest string, kept *ProbeAnswer) {
	whole := strings.TrimSpace(content)
	shown = textcut.Ellipsis(whole, saidShown)
	if shown == whole {
		// NOTHING WAS CUT, so there is nothing to recover and nothing to
		// say about it. A standing sentence about a `probe replies`
		// block on every quoted failure is noise that trains a reader to
		// skip the line it is attached to.
		return shown, "", nil
	}
	// THE FIGURE THE READER IS GIVEN IS THE ONE THAT WAS QUOTED, rather
	// than [saidShown] restated: [textcut.Ellipsis] walks back to a rune
	// boundary, so a reply whose 400th byte sits inside a character is
	// quoted at 398, and "the first 400" would be a small lie in the one
	// place this package is asking to be believed about lengths.
	//
	// Asked of [textcut.Bytes], which is the walk Ellipsis itself makes, so
	// the count comes from the same decision rather than from measuring the
	// marked string — which would have meant spelling the marker here, and
	// a second spelling of "…" is the drift that package's own doc exists
	// to have ended.
	quoted := len(textcut.Bytes(whole, saidShown))
	return shown, fmt.Sprintf(
			" That quote is the first %d of %d bytes; the whole answer is printed "+
				"under `probe replies` at the foot of this report.", quoted, len(whole)),
		&ProbeAnswer{Probe: probe, Shown: quoted, Answer: whole}
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
// nothing rather than one that failed to start.
func (p *Provider) probeVersion(ctx context.Context) (version, problem string) {
	if len(p.profile.VersionArgs) == 0 {
		return "", ""
	}
	res, err := run(ctx, invocation{
		binary: p.profile.Binary,
		args:   p.profile.VersionArgs,
		// THE SAME ALLOWLISTED ENVIRONMENT A REAL CALL GETS. Omitting it
		// does not run the probe with no environment — it runs it with
		// the ENGINE's, see [Provider.probeEnv].
		env:     p.probeEnv(),
		timeout: probeTimeout,
	})
	if err != nil || res.exitCode != 0 {
		return "", ""
	}
	line, cut := res.stdoutFirstLine()
	if cut {
		// REFUSED NAMING THE FIELD, rather than printed as a version.
		// The engine's output cap fell inside the first line, so `line`
		// is a PREFIX — and a report showing `2.0` for a CLI that
		// printed `2.0.31` would be worse than showing nothing, because
		// `written for` is compared against it by eye. The whole of what
		// this CLI printed is not kept: the probe's streams are read
		// once and dropped, and there is nothing here worth keeping
		// anyway — a version command emitting megabytes without a
		// newline is the finding.
		return "", fmt.Sprintf(
			"%s %s wrote more than %d bytes with no newline in them, so the engine's "+
				"output cap fell inside the version line itself and what survived is "+
				"a prefix rather than a version — this CLI is streaming where a "+
				"version was asked for; set providers.llm.%s.cli.overrides."+
				"version_args to the flag that prints one",
			p.profile.Binary, strings.Join(p.profile.VersionArgs, " "), maxOutput, p.key)
	}
	return line, ""
}

// smokeTest runs a REAL completion with a REAL tool.
//
// The command that matters, and the reason it is not merely a version probe:
// a profile can look perfect — binary present, login present, flags accepted
// — and still not produce a parseable tool call, because the envelope
// contract is a request to a model rather than a schema the vendor enforces.
// That failure only shows up on the first turn of a real seat otherwise.
// The second return is the CLI's whole reply where the verdict quoted only its
// opening, and nil where it quoted all of it — see [probeQuote].
func (p *Provider) smokeTest(ctx context.Context) (string, *ProbeAnswer) {
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
		ToolChoice: llm.ToolChoiceRequired,
	})
	if err != nil {
		return "failed — " + err.Error(), nil
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
					"Seats on this provider will burn a corrective round and then "+
					"produce nothing: point this entry at a stronger model",
				comp.OutputTokens), nil
		}
		said, rest, kept := probeQuote("smoke", comp.Content)
		return fmt.Sprintf(
			"failed — the CLI answered but produced no parseable tool call, so seats on "+
				"this provider will burn a corrective round every turn. It said: %q%s",
			said, rest), kept
	}
	return fmt.Sprintf("ok — %d in / %d out", comp.InputTokens, comp.OutputTokens), nil
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
	} else {
		fmt.Fprintln(w, "problems:")
		for _, problem := range d.Problems {
			fmt.Fprintf(w, "  - %s\n", problem)
		}
	}
	d.renderAnswers(w)
}

// renderAnswers prints, whole, every probe reply a problem line quoted only
// the opening of.
//
// LAST AND ONLY WHEN THERE IS ONE. A `doctor` report is read top to bottom and
// diffed by scripts, so a block that is usually absent belongs at the foot
// where it cannot push the fixed-width lines around — and a healthy run, which
// quotes nothing, renders byte for byte what it always did.
//
// AFTER the problems rather than inside them, because a problem line is one
// sentence an operator acts on and a multi-line transcript spliced into the
// middle of the list would break the one shape that makes the list scannable.
// The two are tied by the clause [probeQuote] puts on the line itself, which
// names this block.
//
// INDENTED RATHER THAN WRAPPED OR CUT: this is the recovery route for a value
// that was already shortened once, and shortening it again here would make the
// whole pairing pointless. The indent is what keeps a reply that happens to
// contain a line looking like `problems:` from reading as part of the report.
func (d Diagnosis) renderAnswers(w io.Writer) {
	if len(d.Answers) == 0 {
		return
	}
	fmt.Fprintln(w, "probe replies:")
	for _, a := range d.Answers {
		fmt.Fprintf(w, "  %s — the whole answer, %d bytes, quoted above to the first %d:\n",
			a.Probe, len(a.Answer), a.Shown)
		for reply := range strings.SplitSeq(a.Answer, "\n") {
			fmt.Fprintf(w, "    %s\n", reply)
		}
	}
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
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
