// Package cliagent drives a coding CLI the operator already pays a
// subscription for as a headless text model behind [llm.Provider].
//
// WHICH CLIs is the profile table's answer, not this comment's —
// [cliprofile.BuiltinNames] reads the shipped table and config's tests hold it
// to the agent names a document may write. A
// prose list here would be a second copy of that answer with nothing checking
// it, and it already went stale by three.
//
// The CLI holds the operator's OAuth login, so Crewlet never sees a password
// and never re-implements a vendor's auth. What makes this more than "shell
// out to a CLI" is three problems it has to solve first, and each is a file
// here:
//
//   - SHARED MEMORY. A CLI keeps sessions, history, todos and project notes
//     under one home, so seven seats on one subscription would read each
//     other's transcripts. Every call runs in its own seat home with HOME,
//     the XDG variables and the vendor's own relocation variable pointed
//     inside it, in an empty per-call working directory, with an ALLOWLISTED
//     environment rather than the engine's — see workspace.go and env.go.
//
//   - NO TOOL CHANNEL. The tool loop needs tool calls back, and a CLI prints
//     prose. The catalogue and a response contract ride in the prompt, and
//     the reply is parsed by a deliberately forgiving parser that never
//     errors — see prompt.go and envelope.go.
//
//   - VENDOR DRIFT. These flags and JSON shapes change between releases, so
//     every profile field is data an operator can replace from YAML rather
//     than a Go literal that needs a Crewlet release — see [cliprofile].
//
// The backend obeys the same two rules as every other member of this
// contract: it does not retry, and it classifies a failure no further than a
// coarse kind. The one classification that is genuinely its own is a SPENT
// SUBSCRIPTION, which arrives as prose on a SUCCESSFUL exit and is recognised
// by a vendor's verbatim sentinel plus the reset instant it carries — never
// by keyword, which is how the same recognition was got wrong twice before,
// and on such an exit only where the sentinel OPENS what the CLI said, which
// is otherwise the model's own words (see [classifyMarkers]).
package cliagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

var log = logging.Get("llm.cliagent")

// providerName is what a classified failure calls this backend.
const providerName = "cli-agent"

// Config is one cli-agent provider entry, fully resolved.
//
// Resolved, not referenced: Token and APIKey hold values, never "${VAR}".
// Tier B stores its references verbatim so an exported revision carries no
// secrets, and resolution happens once, here, where the provider is built.
type Config struct {
	// Key is the providers.llm key, for log lines and error messages.
	Key string
	// Model is passed to the CLI's model flag. Empty means the CLI's own
	// default, and a profile with no model flag requires it to be empty.
	Model string
	// Agent names the built-in profile.
	Agent string
	// AgentMode runs a seat's executor as the CLI's own agentic run
	// rather than as a text completion. See [Provider.AgentMode].
	AgentMode bool
	// StateDir is the resolved state directory. The CLI keeps its login and
	// its per-seat homes under <StateDir>/<Agent> ([Shared]), so entries on
	// one directory share a login only when they drive one CLI.
	StateDir string
	// Overrides are the operator's profile edits.
	Overrides map[string]any
	// Timeout caps one invocation, wall clock.
	Timeout time.Duration
	// MaxConcurrent caps this provider's simultaneous processes.
	MaxConcurrent int
	// Env is extra child environment, already ${VAR}-resolved.
	Env map[string]string
	// Auth is the resolved credential material and the mode that decides
	// which of it reaches the child.
	Auth Auth
	// CredentialBundle is a resolved bundle from `crewlet llm export`,
	// restored into an EMPTY credential directory so a container rebuilt on
	// every deploy comes up already authenticated. Kept, not restored, by
	// [New]: [Provider.RestoreConfiguredBundle] is the restore, made by
	// whoever builds the entry to run it.
	CredentialBundle string
	// CredentialBundleConfigured is whether CredentialBundle came from
	// auth.credential_bundle rather than the conventional
	// CREWLET_LLM_CLI_<KEY>_CREDENTIALS ([cliprofile.BundleVarName]), for the lines that
	// tell an operator what retires it ([Provider.LogoutNotice]) — the same
	// split [Auth.TokenConfigured] makes for the token.
	CredentialBundleConfigured bool
}

// Provider is a coding CLI behind the language-model contract.
type Provider struct {
	key       string
	model     string
	agent     string
	agentMode bool
	profile   cliprofile.Profile
	ws        *Workspace
	env       map[string]string
	auth      Auth
	timeout   time.Duration

	// termGrace is how long every child this provider starts has to exit once
	// signalled — always [termGrace]. A field rather than the constant at each
	// exec so that one value reaches all of them, and so the suite can watch a
	// stubborn child be killed at a grace it can afford to wait out.
	termGrace time.Duration

	// bundle is [Config.CredentialBundle], which
	// [Provider.RestoreConfiguredBundle] restores, and bundleConfigured
	// [Config.CredentialBundleConfigured].
	bundle           string
	bundleConfigured bool

	// slots caps concurrent child processes. A buffered channel rather
	// than a semaphore type because acquisition has to be selectable
	// against the caller's context: a seat whose turn was cancelled while
	// queueing must not go on to launch a process nobody is waiting for.
	slots chan struct{}

	// sitOut is the window this entry sits out after its CLI reported a
	// spent plan ([Provider.bench]).
	sitOut bench

	// now is the clock the bench is read against — time.Now, and a fixed
	// clock in the suite, which has to watch a window end without waiting
	// a minute for it.
	now func() time.Time
}

var _ llm.Provider = (*Provider)(nil)

// New builds a cli-agent provider.
//
// Like every other backend, it builds even when no login is present: the call
// then fails with the vendor's own "not authenticated", which names the CLI
// and is exactly what `crewlet llm doctor` explains. A constructor that
// refused to exist would take the whole company down at boot over one
// provider's credentials.
func New(cfg Config) (*Provider, error) {
	profile, err := cliprofile.Load(cfg.Agent, cfg.Overrides)
	if err != nil {
		return nil, err
	}
	// Config refuses the same shape (on every write and at apply), so a
	// document that reaches here with it was never validated — a caller
	// building the provider from code.
	if cfg.Model != "" && !profile.TakesModel() {
		return nil, fmt.Errorf(
			"cli-agent %q: no model_args element of the %q CLI carries {model}, so model %q "+
				"would be ignored — remove it, or declare overrides.model_args with {model}",
			cfg.Key, cfg.Agent, cfg.Model)
	}
	if cfg.StateDir == "" {
		return nil, fmt.Errorf("cli-agent %q: state_dir is empty — it is derived per "+
			"provider key at config load, so an empty one means the derivation was skipped", cfg.Key)
	}
	ws, err := Shared(cfg.StateDir, cfg.Agent, profile)
	if err != nil {
		return nil, err
	}
	slots := cfg.MaxConcurrent
	if slots <= 0 {
		slots = 1
	}
	p := &Provider{
		key:              cfg.Key,
		bundle:           cfg.CredentialBundle,
		bundleConfigured: cfg.CredentialBundleConfigured,
		model:            cfg.Model,
		agent:            cfg.Agent,
		agentMode:        cfg.AgentMode,
		profile:          profile,
		ws:               ws,
		env:              cfg.Env,
		auth:             cfg.Auth,
		timeout:          cfg.Timeout,
		termGrace:        termGrace,
		slots:            make(chan struct{}, slots),
		now:              time.Now,
	}
	if p.timeout <= 0 {
		return nil, fmt.Errorf("cli-agent %q: timeout is zero — a CLI call with no "+
			"wall-clock cap holds a seat forever", cfg.Key)
	}
	// THE BUNDLE IS NOT RESTORED HERE. Building a provider is what
	// `crewlet validate` does to a document, and a constructor that wrote a
	// login into the state directory — under the shared login's lock, which
	// can wait out another writer — would make validating a document a write
	// to the host and a wait nobody's context bounds. The restore is
	// [Provider.RestoreConfiguredBundle], called with a context by the engine
	// as it builds an epoch to run and by the `crewlet llm` commands.
	return p, nil
}

// Model is the configured model identity.
func (p *Provider) Model() string { return p.model }

// Key is the providers.llm key the entry was built under.
func (p *Provider) Key() string { return p.key }

// ModelArgument is the model exactly as this entry's text calls hand it to the
// CLI ([cliprofile.Profile.ModelArgument]) — what a coding run of the same CLI
// is handed too, so a box and a text call on one entry name one model.
func (p *Provider) ModelArgument() string { return p.profile.ModelArgument(p.model) }

// AgentMode reports whether this entry runs a seat's executor as the CLI's own
// agentic run rather than as a text completion behind the engine's tool loop.
//
// The provider is still a full [llm.Provider] in agent mode, and deliberately:
// every OTHER phase — the reviewer, a worker, the summariser, the round-cap
// judge — is a text call, and a seat pointing `llm` at an agent-mode entry
// must not lose them. Only the executor branches, and only the engine knows
// where that branch is.
func (p *Provider) AgentMode() bool { return p.agentMode }

// CodingAgent is the sandbox runner that drives this CLI in agent mode.
//
// It is the CLI's own name, because the runner registry is keyed on exactly
// that: agent mode reuses the coding-agent runners rather than growing a
// second way to invoke the same binary with the same flags. A CLI with no
// registered runner is refused where the run is built, by a message that
// names what IS registered — see [sandbox.Manager.RunnerFor].
func (p *Provider) CodingAgent() string { return p.agent }

// Profile is the resolved profile, for `crewlet llm doctor`.
func (p *Provider) Profile() cliprofile.Profile { return p.profile }

// Workspace is this provider's CLI's share of its state directory, which every
// entry on the same directory and CLI shares.
func (p *Provider) Workspace() *Workspace { return p.ws }

// Agent is which CLI this provider drives.
func (p *Provider) Agent() string { return p.agent }

// AuthMode is how this entry signs its CLI in ([Auth.Mode]), for a caller that
// has to tell an operator which credential to supply: of the variables the
// engine hands the CLI, api-key reads only the key and inherit-env only the
// engine's own environment, so a remedy written for a subscription — mint a
// token into the secret store — is one the entry would never read
// ([Provider.ModeSignIn]). The credential files are seeded under every mode.
func (p *Provider) AuthMode() AuthMode {
	if p.auth.Mode == "" {
		return AuthSubscription
	}
	return p.auth.Mode
}

// String identifies the provider in a log line.
func (p *Provider) String() string {
	return fmt.Sprintf("%s/%s/%s", providerName, p.agent, p.model)
}

// Complete runs one CLI invocation and reads its answer.
//
// An entry its CLI last reported spent is NOT called while it sits out
// ([Provider.benched]): the call fails at once as a rate limit, naming the
// instant, so the seat's chain moves to its next model without a process
// launched to learn again what the last one said. Checked as the call arrives
// and again once it holds a slot, because a call that queued for one may have
// waited out the very call that sat the entry out.
func (p *Provider) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	if err := p.benched(); err != nil {
		return nil, err
	}
	seat := llm.SeatOf(ctx)
	callID := CallOf(ctx)

	// THE SYSTEM PROMPT ON THE CHANNEL THIS KIND OF CALL TAKES: beside the
	// CLI's own for a seat at work, in place of it for a text function —
	// see [cliprofile.Profile.RouteFor]. Lifted before the transcript is
	// rendered, so it is never both.
	route := p.profile.RouteFor(req.Agent)
	if route.KeepsDefault {
		req = withHarnessNote(req)
	}
	var system string
	if route.Channel.Declared() {
		system, req = SplitSystem(req)
	}
	prompt, err := RenderPrompt(req)
	if err != nil {
		return nil, p.fail(llm.KindFatal, err)
	}

	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	case <-ctx.Done():
		// Queued behind other seats and the caller gave up. Reported as
		// a timeout rather than fatal: nothing about the request was
		// wrong, and the chain should be free to try another member.
		return nil, p.fail(llm.KindTimeout, fmt.Errorf("waiting for a CLI slot: %w", ctx.Err()))
	}
	// AGAIN, NOW THAT THE SLOT IS HELD. A call that queued behind a full
	// entry passed the check above before the sit-out existed, so when the
	// calls ahead of it report the spent plan it would otherwise take its
	// slot, check out a home and start a whole CLI only to hear the same
	// sentence — and every call behind it would wait for that too. Refused
	// here, the queue drains at once, each call handing its slot to the next
	// through the release deferred above. The check at the top stays: it
	// spares a call that arrives during a sit-out the render and the queue.
	if sitting := p.benched(); sitting != nil {
		return nil, sitting
	}

	checkout, err := p.ws.Acquire(ctx, seat, callID, p)
	if err != nil {
		// CLASSIFIED, not fatal outright: a seat's generation runs the CLI
		// itself now, so what failed may be a slow host or an unusable
		// login rather than this host's filesystem — see
		// [acquireFailureKind].
		return nil, p.fail(acquireFailureKind(err), err)
	}
	defer func() {
		// WITHOUT THE CALL'S CANCELLATION: a release is a teardown, and a
		// call that ended because its context was cancelled still has a
		// login the CLI may have refreshed to write back. Bounded by the
		// export's own timeout rather than by nothing.
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err := checkout.Release(context.WithoutCancel(ctx)); err != nil {
			// Logged, not returned: the completion already happened
			// and failing it over a cleanup would throw away work the
			// operator paid for. A login that could not be carried out
			// is kept in the home, and the next generation to open —
			// this seat's next call, or any seat's first — carries it
			// out before it seeds — see [Workspace.Acquire].
			log.WarnContext(ctx, "cli_agent_release_failed", "provider", p.key, "seat", seat, "error", err)
		}
	}()

	in := invocation{
		binary:  p.profile.Binary,
		args:    p.argv(),
		dir:     checkout.Work,
		env:     buildEnv(p.profile, checkout, p.env, p.auth),
		timeout: p.timeout,
		grace:   p.termGrace,
	}
	// Streamed only for a JSONL profile, where each event's text IS a
	// fragment of the answer. The other two output modes are extracted
	// from the WHOLE of stdout — a fenced block, one JSON document — so
	// there is no faithful incremental view of them, and forwarding raw
	// lines would stream a banner and a half-written fence as though they
	// were what the model said.
	if req.Streaming() && p.profile.EffectiveOutput() == cliprofile.OutputJSONL {
		in.onLine = func(line string) {
			doc, ok := decodeObject(strings.TrimSpace(line))
			if !ok {
				return
			}
			// The bool is deliberately dropped HERE and only here:
			// a stream is many events and most of them carry no
			// text path at all, so "did this one event match" is
			// not the question. Whether the PROFILE matches is
			// decided once, over the whole stream, by extract.
			//
			// The SAME event filter the extractor applies, so a
			// stream is streamed as it will be read: a profile that
			// takes its answer from one terminal event delivers it
			// in one delta at the end rather than pushing a tool's
			// output through as though the model had said it.
			chunk, _ := textOf(p.profile, doc)
			req.Send(llm.Delta{Content: chunk})
		}
	}
	// THE CALL'S OWN SESSION, named so the usage command below can ask
	// about it afterwards — see [cliprofile.Profile.SessionArgs]. After the
	// model arguments for the same reason they precede everything else: a
	// CLI taking its prompt on argv reads the first non-flag argument.
	var session string
	if len(p.profile.SessionArgs) > 0 {
		session = newSessionID()
		for _, arg := range p.profile.SessionArgs {
			in.args = append(in.args, strings.ReplaceAll(arg, "{session}", session))
		}
	}
	// A USAGE REPORT THE CLI WRITES ITSELF, for a vendor whose one-shot
	// surface prints the answer and nothing else. Appended before the
	// system and prompt arguments for the same reason the model flag is:
	// a CLI taking its prompt on argv reads the first non-flag argument,
	// so nothing may come after it.
	var usagePath string
	if len(p.profile.UsageFileArgs) > 0 {
		usagePath = filepath.Join(checkout.Work, usageFileName)
		for _, arg := range p.profile.UsageFileArgs {
			in.args = append(in.args, strings.ReplaceAll(arg, "{usage_file}", usagePath))
		}
	}
	// EVERY CALL WHERE THE FILE CARRIES POLICY, not only the calls that
	// have something to put in it — see [cliprofile.SystemPromptFile]. A
	// channel that is a vendor agent file declares that CLI's tool denial in
	// the same file, so skipping it on a request with no system prompt
	// would hand the vendor's default agent, and every tool it has, to
	// exactly the calls that would never notice.
	if ch := route.Channel; system != "" || ch.File != nil {
		// One shape or the other — the profile validator refuses a
		// channel that declares both.
		switch {
		case ch.Env != "":
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			pair, err := systemEnv(ch, system, checkout.Work)
			if err != nil {
				return nil, p.fail(llm.KindFatal, err)
			}
			// APPENDED, so it wins: the child env is assembled from the
			// allowlist and the profile's own config_env above, and the
			// last assignment of a name is the one exec applies.
			in.env = append(in.env, pair)
		case len(ch.Args) > 0 || ch.File.ReadAlways():
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			args, err := systemArgs(ch, system, checkout.Work)
			if err != nil {
				return nil, p.fail(llm.KindFatal, err)
			}
			in.args = append(in.args, args...)
		}
	}
	// THE PROMPT LAST, and after the system args for the same reason the
	// profile puts the model flag before it: a CLI taking its prompt on
	// argv reads the first non-flag argument, so anything appended after it
	// is read as part of the prompt.
	switch p.profile.EffectivePromptMode() {
	case cliprofile.PromptArgv:
		// PromptArgs and then the prompt, ADJACENT and last: a CLI that
		// takes its prompt as a flag's value needs the two together, and
		// anything appended between them becomes the prompt instead.
		in.args = append(in.args, p.profile.PromptArgs...)
		in.args = append(in.args, prompt)
	case cliprofile.PromptFile:
		// Only the flag and the PATH go on argv; the transcript itself
		// stays in the per-call working directory at 0600.
		rendered, writeErr := promptArgs(p.profile.PromptArgs, prompt, checkout.Work)
		if writeErr != nil {
			return nil, p.fail(llm.KindFatal, writeErr)
		}
		in.args = append(in.args, rendered...)
	default:
		in.stdin = prompt
	}

	started := time.Now()
	res, err := run(ctx, in)
	if err != nil {
		// NO CLI RAN, which is all run's error ever says: the binary is
		// missing or not executable here, or the host would not give it a
		// pipe or a process. That is this host's fault, the one exec failure
		// the next member is no answer to, so it is fatal. A CLI that DID run
		// comes back as a result whatever happened to it, and [completion]
		// classifies it by what that was.
		return nil, p.fail(llm.KindFatal, err)
	}
	log.DebugContext(ctx, "cli_agent_call", "provider", p.key, "agent", p.agent, "model", p.model,
		"seat", seat, "exit", res.exitCode, "elapsed_ms", time.Since(started).Milliseconds())

	// READ BEFORE THE CHECKOUT IS RELEASED, which removes the directory.
	// A missing or unreadable report is not an error: the vendor writes it
	// "even when the run fails", so its absence says the run did not get
	// that far — and failing a completion that the model answered, over a
	// count, would throw away work the operator paid for. The counts then
	// fall back to whatever stdout said, which for such a CLI is an
	// estimate.
	var usageDoc string
	if usagePath != "" {
		raw, readErr := os.ReadFile(usagePath) //nolint:gosec // rooted in the per-call dir
		switch {
		case os.IsNotExist(readErr):
		case readErr != nil:
			log.WarnContext(ctx, "cli_agent_usage_file_unreadable", "provider", p.key,
				"agent", p.agent, "path", usagePath, "error", readErr)
		case strings.TrimSpace(string(raw)) == "":
			// Written and empty is not "the run did not get that far": the
			// vendor began its report and wrote nothing into it, and what
			// is read below could not tell this from no file at all.
			log.WarnContext(ctx, "cli_agent_usage_file_unreadable", "provider", p.key,
				"agent", p.agent, "path", usagePath, "error", "the file is empty")
		default:
			usageDoc = string(raw)
		}
	}

	// A USAGE REPORT ASKED FOR AFTERWARDS, for a vendor that records the
	// call's counts and does not print all of them — see
	// [cliprofile.Profile.UsageArgs]. Asked whatever the call's outcome,
	// because a refused call may still have spent tokens; and like a usage
	// file, a report that cannot be had leaves the counts to stdout rather
	// than failing an answer the operator paid for. In the working directory
	// the call ran in, before the checkout is released.
	if len(p.profile.UsageArgs) > 0 && !res.timedOut {
		args := make([]string, 0, len(p.profile.UsageArgs))
		for _, arg := range p.profile.UsageArgs {
			args = append(args, strings.ReplaceAll(arg, "{session}", session))
		}
		report, reportErr := p.runAuxiliary(ctx, args, checkout.Work,
			buildEnv(p.profile, checkout, p.env, p.auth), "")
		switch {
		case reportErr != nil:
			log.WarnContext(ctx, "cli_agent_usage_report_failed", "provider", p.key,
				"agent", p.agent, "error", reportErr)
		case strings.TrimSpace(report) == "":
			// A command that succeeded and printed nothing reported nothing,
			// and the empty string handed on below reads as no report at all
			// — so this is the one place that can say so.
			log.WarnContext(ctx, "cli_agent_usage_report_failed", "provider", p.key,
				"agent", p.agent, "error", fmt.Sprintf("%s %s exited 0 and printed nothing",
					p.profile.Binary, strings.Join(args, " ")))
		default:
			usageDoc = report
		}
	}

	return p.completion(ctx, system+prompt, len(req.Tools) > 0, res, usageDoc)
}

// newSessionID mints the id [cliprofile.Profile.SessionArgs] names a call's
// session with: 32 lowercase hexadecimal characters from crypto/rand.
//
// RANDOM rather than derived from the seat and the call, because the id is the
// key a usage command looks the call up by in a database every concurrent
// call of the seat shares — and a retried call that reused its predecessor's
// id would continue that session, carrying the failed attempt's transcript
// into the retry and its counts into the retry's bill.
func newSessionID() string {
	var raw [16]byte
	// crypto/rand.Read never fails on the platforms this builds for (it
	// panics rather than return an error since Go 1.24), so there is no
	// error to handle.
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

// argv is the invocation's arguments: the profile's completion argv, then the
// model, then (in argv mode) the prompt, which the caller appends.
func (p *Provider) argv() []string {
	args := append([]string(nil), p.profile.CompleteArgs...)
	if p.model == "" {
		return args
	}
	for _, arg := range p.profile.ModelArgs {
		args = append(args, strings.ReplaceAll(arg, "{model}", p.model))
	}
	return args
}

// completion turns a finished invocation into an answer or a classified
// failure.
//
// It takes a context only to log with: an answer this backend hands back
// EMPTY is a real outcome rather than an error (see the fall-through below),
// and the one place that can say so is here, where the token counts that
// explain it are still in hand.
//
// sent is everything the engine handed the CLI — the system prompt on its
// channel and the rendered prompt — and is read only for the estimate a CLI
// that reports no usage is charged. The system prompt is in it because a
// budget charged for the transcript alone undercounts every call by the
// largest stable thing in it; what is NOT in it is a vendor's own system
// prompt kept beside the engine's, which the engine never sees.
//
// offeredTools says whether the prompt carried the response contract, which
// [RenderPrompt] appends exactly when the request offered tools. Only then is
// the reply read as an envelope: see the read below.
func (p *Provider) completion(
	ctx context.Context, sent string, offeredTools bool, res *rawResult, usageDoc string,
) (*llm.Completion, error) {
	if res.timedOut {
		return nil, p.fail(llm.KindTimeout, fmt.Errorf(
			"the CLI did not answer within %s — raise timeout_seconds if this model "+
				"legitimately reasons for longer:\n%s", p.timeout, tail(res.stderr)))
	}

	// A CLIPPED ANSWER IS NOT AN ANSWER. stdout past maxOutput is dropped by
	// the capped buffer, and everything below would then parse whatever
	// survived and hand it back as a completion — a half-written envelope, a
	// JSON body missing its close, a report cut mid-sentence — with nothing
	// downstream able to tell it from a model that simply stopped there.
	// Refused as a SERVER failure so the fallback chain may try another
	// member and the credential is not benched: nothing about the prompt was
	// rejected.
	if res.droppedStdout > 0 {
		return nil, p.fail(llm.KindServer, fmt.Errorf(
			"the CLI wrote more than %d bytes to stdout and %d were dropped, so "+
				"the answer is incomplete. No completion comes near that size, so "+
				"the CLI printed something besides one: run `crewlet llm doctor %s` "+
				"to see what, or point this provider at a model that streams less",
			maxOutput, res.droppedStdout, p.key))
	}

	out := extract(p.profile, res.stdout)
	// The CLI's own report wins over anything stdout happened to carry:
	// a profile only names one when its vendor puts the counts nowhere
	// else, so a hit here is the only real figure there is. A report that
	// ARRIVED and could not be read is said out loud, because the counts
	// then fall back to stdout's — an estimate, for a CLI that reports
	// none there — on every call until somebody notices the budget
	// figures are made up.
	if outcome := out.applyUsageReport(p.profile, usageDoc); outcome.unreadable() {
		fallback := "an estimate, four characters a token"
		if out.reported {
			fallback = "the counts stdout reported"
		}
		log.WarnContext(ctx, "cli_agent_usage_report_unreadable", "provider", p.key,
			"agent", p.agent, "source", usageSource(p.profile), "reason", outcome.String(),
			"input_paths", cliprofile.PathList(p.profile.Usage.Input),
			"output_paths", cliprofile.PathList(p.profile.Usage.Output),
			"counts_from", fallback)
	}

	// A spent subscription is checked BEFORE the exit code, because it
	// arrives on a successful one: the process exits 0 and the answer is
	// the vendor's own sentence about the plan.
	//
	// THE ANSWER IF THERE IS ONE, EVERYTHING PRINTED IF THERE IS NOT.
	// A marker is something the VENDOR said, so losing it because this
	// profile's text paths no longer resolve would classify a spent plan
	// as a server fault and burn the chain's next member on it — and a
	// spent plan is exactly when a drifted-looking run is most likely,
	// since the CLI answers with prose where the envelope should be.
	//
	// Not stdout unconditionally, though, which was the first shape of
	// this: some shipped markers are short generic phrases rather than
	// vendor sentences, and scanning a healthy run's whole telemetry for
	// them adds false-positive surface for nothing. When the answer WAS
	// located it is the only thing the vendor said that matters.
	//
	// The events scope reads neither as text: it decodes the whole of
	// stdout and looks only at the CLI's own failure events — see
	// [cliprofile.MarkerScope] — which is why stdout itself is handed over
	// beside what was said. And whether the CLI said the run FAILED is
	// handed over too, because it decides where in what was said a
	// sentinel may stand — see [classifyMarkers].
	said := nonEmpty(out.text, res.stdout)
	if hit, ok := classifyMarkers(p.profile, cliOutput{
		said: said, stdout: res.stdout, stderr: res.stderr,
		failed: res.exitCode != 0 || out.failed,
	}); ok {
		reason := nonEmpty(hit.Said, firstLine(said, res.stderr))
		if hit.Kind == llm.KindRateLimit {
			p.bench(hit.Retry, reason)
			// WHERE IT MATCHED, beside what it matched: a sit-out takes the
			// entry from every seat on this node, and the one question an
			// operator who doubts it has is whether the vendor said it.
			log.WarnContext(ctx, "cli_agent_limit_reported", "provider", p.key, "agent", p.agent,
				"source", string(hit.Source), "said", reason)
		}
		return nil, p.fail(hit.Kind, fmt.Errorf("%s", reason))
	}

	// A RUN THE CLI SAYS FAILED, FOR NO REASON A PROFILE RECOGNISES, IS A
	// SERVER FAILURE — the CLI or its vendor did not answer: an overload or
	// a 5xx it relayed once its own retries ran out, a crash, a reason in
	// words no marker knows. It used to be FATAL, on the reasoning that every
	// member of the seat's chain would refuse the call identically, and that
	// is true only of a call the engine cannot make at all ([llm.KindFatal]).
	// The next member is another login, another vendor or another binary,
	// which does not share this one's outage: trying it costs one process
	// launch, and stopping cost the seat every turn for as long as the outage
	// lasted, each redelivered against the same dead head. Nothing is
	// repeated by the walk, either — a text call runs none of the seat's
	// tools; the engine runs those once an answer is back.
	//
	// NOT BENCHED: only a spent plan sits the entry out ([Provider.bench]),
	// because only a spent plan is known to last. A failure the next call
	// may not meet must not hold every seat on the node off a working entry.
	if res.exitCode != 0 || out.failed {
		what := fmt.Sprintf("the %s CLI exited %d", p.agent, res.exitCode)
		if res.exitCode == 0 {
			what = fmt.Sprintf("the %s CLI exited 0 and reported that the run failed", p.agent)
		}
		return nil, p.fail(llm.KindServer, fmt.Errorf("%s:\n%s", what,
			tail(nonEmpty(res.stderr, out.text, res.stdout))))
	}
	// A RUN NOT SEEN TO ITS END IS A SERVER FAILURE TOO, whatever it
	// printed. A stdout that something the CLI left running still held a
	// grace after the CLI was done was cut there, so no reading of it can be
	// shown to be the whole answer: the refusal a clipped stdout gets above,
	// for the same reason. And a wait that failed is a CLI that started and
	// did not finish as one. Neither is a call the engine could not make, so
	// the chain may carry it to the next member, and nothing says it lasts,
	// so nothing is sat out. Both used to come back from [run] as an error,
	// read as fatal: the walk stopped at the head with a whole answer on
	// stdout, the CLI having exited 0.
	//
	// After the markers, which classify on what the vendor SAID: a spent plan
	// reported on a stdout a helper then held is still a spent plan.
	if why := res.unfinished(); why != "" {
		return nil, p.fail(llm.KindServer, fmt.Errorf("the %s CLI exited 0, but %s:\n%s",
			p.agent, why, tail(nonEmpty(res.stderr, res.stdout))))
	}
	if strings.TrimSpace(res.stdout) == "" {
		// Exit zero and nothing on stdout AT ALL — no envelope, no
		// banner, no prose. Checked before the two cases below because
		// neither can say anything true about output that does not
		// exist: there is no shape for a path to miss and no field to
		// find empty. Not a fatal request problem — nothing about the
		// prompt was refused — so the chain is free to try another
		// member, and the entry is not benched.
		return nil, p.fail(llm.KindServer, fmt.Errorf(
			"the %s CLI exited 0 but printed nothing at all%s",
			p.agent, stderrDetail(res.stderr)))
	}
	if !out.located {
		// THE PROFILE HAS DRIFTED FROM THE CLI. Its output parsed, and
		// none of the paths that say where the answer lives resolved to
		// a string — so this build cannot tell which part of what the
		// CLI printed is the model's reply, and the honest answer is
		// that it has none.
		//
		// It used to hand the whole of stdout back as the reply instead,
		// on the reasoning that an operator could then see the shape and
		// write an override. They could — but only after it had already
		// been spoken as an agent: the tool loop appends it to the
		// conversation, the reviewer judges the turn on it, and the
		// dashboard prints it as the sentence the seat said. The shape
		// belongs in THIS message, where the person who can fix it is
		// the only reader.
		//
		// KindServer, so the chain may try another member and the
		// credential is not benched: nothing about the prompt was
		// refused.
		return nil, p.fail(llm.KindServer, fmt.Errorf(
			"the %s CLI's output parsed, but none of the text_paths this profile "+
				"looks in resolved (%s) — the profile no longer matches the "+
				"installed CLI, so nothing it printed can be read as the model's "+
				"reply. Run `crewlet llm doctor %s` and set "+
				"providers.llm.%s.overrides.text_paths. It printed:\n%s",
			p.agent, cliprofile.PathList(p.profile.TextPaths), p.key, p.key, tail(res.stdout)))
	}
	// LOCATED AND EMPTY IS AN ANSWER OF NOTHING, NOT A FAULT. The CLI
	// exited 0, reported no error, and the path this profile looks in
	// resolved to an empty string — which is what a model that spent its
	// whole output budget on hidden reasoning does. That is a MODEL
	// outcome, and [llm.Provider] forbids a backend from deciding what a
	// failure means beyond a coarse kind, so it must not be dressed as one:
	// it is a Completion with empty Content, and the tool loop is the frame
	// that corrects it.
	//
	// It used to be a KindServer error, on the reasoning that there was no
	// reply for the tool loop to correct. That was true only because the
	// loop tested tool calls and never read Content; closing that gap is
	// what makes this fall-through safe, and what stops one empty round
	// from being reported as an outage of a CLI that demonstrably answered.
	//
	// Falls through rather than returning early so the round is charged:
	// Content becomes "" with the vendor's real usage attached below — an
	// empty answer costs tokens, and it was previously the one outcome that
	// spent them without ever reaching a budget.
	comp := &llm.Completion{
		Model:      p.model,
		Content:    out.text,
		StopReason: llm.StopEnd,
	}
	// AN ENVELOPE ONLY WHERE ONE WAS ASKED FOR. A call that offered no tools
	// sent no contract, so its reply is the answer verbatim — and reading it
	// as an envelope anyway is not harmless forgiveness: the envelope reader
	// accepts `content`, `text`, `message` and `response` as the message key,
	// so an auxiliary answer that IS a JSON object carrying one of them
	// (the persist decider's `{"kind": "LONG", "content": "..."}`) came back
	// as that one field, its caller's decode failed, and the decision was
	// silently dropped.
	var env Envelope
	if offeredTools {
		env = ParseEnvelope(out.text)
		if env.Parsed {
			comp.Content = env.Message
		}
	}
	for i, call := range env.ToolCalls {
		comp.ToolCalls = append(comp.ToolCalls, llm.ToolCall{
			// The CLI has no id of its own to give, and the tool loop
			// pairs results to calls by it. Derived from the position
			// so two calls in one answer stay distinguishable.
			ID:        fmt.Sprintf("cli_%d", i),
			Name:      call.Name,
			Arguments: call.Arguments,
			// Arguments the envelope could not read: the tool loop
			// answers the call with this instead of running it on an
			// empty map, as it does for every other backend.
			ArgumentsError: call.ArgumentsError,
		})
	}
	if len(comp.ToolCalls) > 0 {
		// The CLI reports no stop reason of its own in text mode: the
		// envelope it was asked for is either an answer or a set of
		// calls, so those are the only two this backend can honestly
		// name. A CLI that hit its own cap or refused returns that as
		// prose or a failure the exit path classifies.
		comp.StopReason = llm.StopToolUse
	}

	if out.reported {
		// InputTokens is ALWAYS the full prompt count, cache included —
		// the contract's rule, and what keeps it a correct budget figure
		// whatever the cache did. CacheRead and CacheWrite break the same
		// total down for cost reporting; adding them again double-counts,
		// which is why a vendor whose input count already holds them says
		// so ([cliprofile.UsagePaths.InputIncludesCache]).
		comp.InputTokens = p.profile.Usage.PromptTokens(out.input, out.cacheRead, out.cacheWrite)
		// Reasoning a CLI reports apart from its output is billed as
		// output, so it is charged as output — see [cliprofile.UsagePaths].
		comp.OutputTokens = out.output + out.reasoning
		comp.CacheRead = out.cacheRead
		comp.CacheWrite = out.cacheWrite
	} else {
		comp.InputTokens = EstimateTokens(sent)
		comp.OutputTokens = EstimateTokens(out.text)
	}

	if strings.TrimSpace(comp.Content) == "" && len(comp.ToolCalls) == 0 {
		// Warned rather than returned, because nothing above this frame
		// can reconstruct WHY: the output token count is the evidence
		// that the model worked and said nothing (a thinking-only round
		// bills hundreds), and the text paths are the evidence a reader
		// needs if it turns out to be profile drift after all. Same
		// shape as agent/prefetch's own answered-nothing warning.
		log.WarnContext(ctx, "cli_agent_answered_nothing", "provider", p.key,
			"agent", p.agent, "model", p.model, "text_paths", cliprofile.PathList(p.profile.TextPaths),
			"output_tokens", comp.OutputTokens, "reported_usage", out.reported)
	}
	return comp, nil
}

// classifyMarkers recognises a spent subscription or an expired login in a
// CLI's own words.
//
// Structure, not keywords. A marker is a sentinel the vendor emits VERBATIM,
// and where the vendor carries the reset instant alongside it, the
// classification yields a real Retry-After rather than a guess. Matching
// "usage limit" case-insensitively across a reply is what made this wrong
// before: a model asked about rate limits writes the phrase itself, and every
// such answer was thrown away as a spent plan.
//
// A FUNCTION OF THE PROFILE, not a method on the provider: what a vendor's
// spent plan looks like is a property of the profile table, and the only way
// to check a shipped profile's sentinels against the output its CLI actually
// produces is to be able to ask without a running provider.
//
// # What the model said is not the vendor's report
//
// Verbatim is not enough on its own, because what the CLI said on stdout is,
// on every healthy run, the MODEL's words — its prose, and the arguments of
// every tool call it makes — and a seat asked to add backoff for the GitHub
// API writes "rate limit" as readily as a vendor reports one. Matched anywhere
// in there, a sentence the model wrote threw its answer away as a spent plan
// and sat the entry out for every seat on the node, and the redelivered turn
// wrote it again a minute later. So in the default scope a sentinel in what
// was said counts in two places only:
//
//   - ANYWHERE, in a run the CLI itself reported failed — a non-zero exit,
//     or its own failure flag ([cliprofile.Profile.ErrorPaths]). The CLI has
//     said this was no answer; what it printed is its account of why.
//   - AT THE START, in a run it did not: the sentinel begins the first
//     non-empty line, so that what was said IS the vendor's report rather
//     than an answer that mentions one. That is the shape of the clean-exit
//     report this package has measured — Claude Code's spent plan, a line
//     beginning "Usage limit reached" standing where the answer should be —
//     and it is what makes the shipped profiles' generic, unmeasured
//     sentinels ("rate limit", "quota") safe to keep: a model's answer that
//     merely mentions one, or a tool call that carries one in its
//     arguments, is not classified at all, and comes back as the answer it
//     is.
//
// Stderr is matched anywhere: it is the CLI's own channel, and the answer
// does not reach it. The events scope reads the CLI's own failure
// events and nothing else, and the stderr scope stderr alone — see
// [cliprofile.MarkerScope].
func classifyMarkers(p cliprofile.Profile, out cliOutput) (markerHit, bool) {
	var haystacks []haystack
	switch p.EffectiveMarkerScope() {
	case cliprofile.MarkerScopeEvents:
		return classifyFailureEvents(p, out.stdout)
	case cliprofile.MarkerScopeStderr:
		haystacks = []haystack{{text: out.stderr, source: sourceStderr}}
	default:
		said := haystack{text: out.said, source: sourceReport, anchored: true}
		if out.failed {
			said = haystack{text: out.said, source: sourceFailedRun}
		}
		haystacks = []haystack{said, {text: out.stderr, source: sourceStderr}}
	}
	for _, marker := range p.LimitMarkers {
		for _, hay := range haystacks {
			idx, found := hay.find(marker.Sentinel)
			if !found {
				continue
			}
			return markerHit{
				Kind:   llm.KindRateLimit,
				Retry:  resetAfter(hay.text[idx:], marker),
				Said:   lineAt(hay.text, idx),
				Source: hay.source,
			}, true
		}
	}
	for _, marker := range p.AuthMarkers {
		for _, hay := range haystacks {
			if idx, found := hay.find(marker.Sentinel); found {
				return markerHit{Kind: llm.KindAuth, Said: lineAt(hay.text, idx), Source: hay.source}, true
			}
		}
	}
	return markerHit{}, false
}

// cliOutput is what one finished run gives [classifyMarkers] to read.
type cliOutput struct {
	// said is what the CLI said: the located answer, or the whole of stdout
	// when none was located.
	said string
	// stdout is the whole of it, which only the events scope reads, and only
	// as decoded events.
	stdout string
	// stderr is the CLI's own diagnostic channel.
	stderr string
	// failed is the CLI's own word that the run failed: a non-zero exit, or
	// the flag at the profile's error_paths.
	failed bool
}

// haystack is one text a sentinel may be found in, and on what terms.
type haystack struct {
	text   string
	source markerSource
	// anchored admits a sentinel only where it BEGINS the text's first
	// non-empty line — the rule for what a run that did not fail said on
	// stdout, which is the model's words unless it is the vendor's report.
	anchored bool
}

// find is the byte offset of sentinel in the haystack, on its terms.
func (h haystack) find(sentinel string) (int, bool) {
	if !h.anchored {
		idx := strings.Index(h.text, sentinel)
		return idx, idx >= 0
	}
	// The first non-space byte opens the first non-empty line, trimmed.
	start := strings.IndexFunc(h.text, func(r rune) bool { return !unicode.IsSpace(r) })
	if start < 0 || !strings.HasPrefix(h.text[start:], sentinel) {
		return -1, false
	}
	return start, true
}

// markerSource is where a sentinel matched — the evidence that the vendor,
// and not the model, said it.
type markerSource string

const (
	// sourceReport is what a run that did not fail said on stdout, which is
	// the vendor's report: the sentinel begins its first non-empty line.
	sourceReport markerSource = "report"
	// sourceFailedRun is what a run the CLI reported failed said on stdout,
	// where a sentinel counts anywhere.
	sourceFailedRun markerSource = "failed-run"
	// sourceStderr is the CLI's stderr.
	sourceStderr markerSource = "stderr"
	// sourceEvent is one of the CLI's own failure events, under
	// [cliprofile.MarkerScopeEvents].
	sourceEvent markerSource = "event"
)

// markerHit is what a matched sentinel tells the caller.
//
// Said is the load-bearing field and the reason this is a struct. The
// classification used to be reported with [firstLine] over the whole
// haystack, which is the vendor's own sentence for a CLI that prints prose —
// and the FIRST EVENT of a stream for a CLI that prints JSONL. A muse-code
// rate limit read `run.lifecycle.started`: correctly classified, and useless
// to the operator who has to act on it.
type markerHit struct {
	Kind  llm.ErrorKind
	Retry time.Duration
	// Said is the line the sentinel matched on — the vendor's own words,
	// in the place the vendor put them.
	Said string
	// Source is where it matched, which is why it counts as the vendor's.
	Source markerSource
}

// lineAt returns the line of s containing byte offset idx, trimmed.
func lineAt(s string, idx int) string {
	start := strings.LastIndexByte(s[:idx], '\n') + 1
	end := strings.IndexByte(s[idx:], '\n')
	if end < 0 {
		return strings.TrimSpace(s[start:])
	}
	return strings.TrimSpace(s[start : idx+end])
}

// resetAfter reads the reset instant a limit marker carries.
//
// Zero when the vendor gave none, which [Provider.bench] reads as "sit out
// the default" — the honest answer, rather than inventing a window.
func resetAfter(from string, marker cliprofile.LimitMarker) time.Duration {
	if marker.ResetSeparator == "" {
		return 0
	}
	_, rest, found := strings.Cut(from, marker.ResetSeparator)
	if !found {
		return 0
	}
	// The value ends at the first character that cannot be part of it, so
	// a sentinel embedded in a longer sentence still yields its number.
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	value, err := time.ParseDuration(rest[:end] + "s")
	if err != nil {
		return 0
	}
	if marker.ResetUnit == "epoch" {
		until := time.Until(time.Unix(int64(value.Seconds()), 0))
		if until <= 0 {
			// A reset already in the past means the window has turned
			// over; the caller should try again rather than wait.
			return 0
		}
		return until
	}
	return value
}

// fail wraps an error as a classified provider failure.
func (p *Provider) fail(kind llm.ErrorKind, err error) error {
	return &llm.Error{Kind: kind, Provider: providerName + "/" + p.agent, Model: p.model, Err: err}
}

// firstLine is the first non-empty line across the given texts, for a failure
// message that names the vendor's own sentence rather than its whole reply.
func firstLine(texts ...string) string {
	for _, text := range texts {
		for line := range strings.SplitSeq(text, "\n") {
			if trimmed := strings.TrimSpace(line); trimmed != "" {
				return trimmed
			}
		}
	}
	return "no output"
}

// stderrDetail appends a CLI's stderr to a message, or nothing when it wrote
// none.
//
// A trailing empty ":" after a sentence that already said what went wrong is
// how a message stops reading like one — and stderr is genuinely absent on the
// paths that use this, because a CLI that exits 0 usually says nothing there.
func stderrDetail(stderr string) string {
	if strings.TrimSpace(stderr) == "" {
		return ""
	}
	return " It wrote on stderr:\n" + tail(stderr)
}

// nonEmpty is the first of the given strings with content.
func nonEmpty(candidates ...string) string {
	for _, c := range candidates {
		if strings.TrimSpace(c) != "" {
			return c
		}
	}
	return ""
}
