// Package harness drives a harness the operator already pays a
// subscription for, headless, behind [llm.Provider]: a seat's phase as the
// harness's own loop over the seat's tools, and a text function as one answer.
//
// It is the backend of every phase but one: the reviewer, delegated workers,
// onboarding and the side passes call [Provider.Complete] on this host, with
// the harness's own shell and file tools denied by its profile. A seat's EXECUTOR
// is not a call through it — it is the same harness's own agentic run in a box
// (internal/sandbox/executor), which reads this entry's identity from here
// ([Provider.Harness], [Provider.ModelArgument], the Sandbox* environment and
// credentials) and, at launch, whether the entry sits out
// ([Provider.SittingOut]).
//
// WHICH HARNESSES is the profile table's answer, not this comment's —
// [harnessprofile.BuiltinNames] reads the shipped table and config's tests hold it
// to the agent names a document may write. A
// prose list here would be a second copy of that answer with nothing checking
// it, and it already went stale by three.
//
// The harness holds the operator's OAuth login, so Crewlet never sees a password
// and never re-implements a vendor's auth. What makes this more than "shell
// out to a harness" is three problems it has to solve first, and each is a file
// here:
//
//   - SHARED MEMORY. A harness keeps sessions, history, todos and project notes
//     under one home, so seven seats on one subscription would read each
//     other's transcripts. Every call runs in its own seat home with HOME,
//     the XDG variables and the vendor's own relocation variable pointed
//     inside it, in an empty per-call working directory, with an ALLOWLISTED
//     environment rather than the engine's — see workspace.go and env.go.
//
//   - A LOOP OF ITS OWN. A seat's phase hands the harness its tools as one MCP
//     server ([llm.ToolHost]), served for the run alone on a socket the
//     relay reaches, and the harness calls them natively, asking its model again
//     and again until it answers or the engine ends the run. So the engine
//     sees the calls and nothing between them: the harness's own record — its
//     stream, or a file it writes — is read as it goes for each model request,
//     its prose and its cost, and handed to the host; a refreshed login is
//     carried back at each call; and a model call made inside one of the
//     run's calls runs on the run's slot — see toolrun.go, record.go and
//     slots.go. A harness whose loop runs in a server of its own (OpenCode 2.1)
//     has that server started first and waited for until it has the tools,
//     and the run is its client — see serve.go. A text function — no tools —
//     is one prompt in and one answer out, as it always was.
//
//   - VENDOR DRIFT. These flags and JSON shapes change between releases, so
//     every profile field is data an operator can replace from YAML rather
//     than a Go literal that needs a Crewlet release — see [harnessprofile].
//
// WHAT THE VENDOR SAID IS THE ACCOUNT'S, NOT THE ENTRY'S (ADR-0069). A sit-out,
// the readiness verdict and the last answer and refusal are kept per sign-in
// ([SignInID]) in one node-wide registry ([Plans]) that outlives every apply,
// so a rebuilt entry on the same account keeps its window, a new credential
// starts clean, and entries on one credential pause together: see plans.go.
//
// The backend obeys the same two rules as every other member of this
// contract: it does not retry, and it classifies a failure no further than a
// coarse kind. The one classification that is genuinely its own is a SPENT
// SUBSCRIPTION, which arrives as prose on a SUCCESSFUL exit and is recognised
// by a vendor's verbatim sentinel plus the reset instant it carries — never
// by keyword, which is how the same recognition was got wrong twice before,
// and on such an exit only where the sentinel OPENS what the harness said, which
// is otherwise the model's own words (see [classifyMarkers]).
package harness

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

var log = logging.Get("llm.harness")

// providerName is what a classified failure calls this backend.
const providerName = "harness"

// Config is one harness provider entry, fully resolved.
//
// Resolved, not referenced: Token and APIKey hold values, never "${VAR}".
// Tier B stores its references verbatim so an exported revision carries no
// secrets, and resolution happens once, here, where the provider is built.
type Config struct {
	// Key is the providers.llm key, for log lines and error messages.
	Key string
	// Model is passed to the harness's model flag. Empty means the harness's own
	// default, and a profile with no model flag requires it to be empty.
	Model string
	// Harness names the built-in profile.
	Harness string
	// StateDir is the resolved state directory. The harness keeps its login and
	// its per-seat homes under <StateDir>/<Agent> ([Shared]), so entries on
	// one directory share a login only when they drive one harness.
	StateDir string
	// Overrides are the operator's profile edits.
	Overrides map[string]any
	// Timeout caps one stretch of the model's own work: a text function's
	// whole call, and on a run with tools each stretch with none of the
	// seat's tools in flight — see [stretch].
	Timeout time.Duration
	// MaxConcurrent caps this provider's simultaneous processes — see
	// slots.go for the calls made inside a run's own tool calls.
	MaxConcurrent int
	// Env is extra child environment, already ${VAR}-resolved.
	Env map[string]string
	// Auth is the resolved credential material and the mode that decides
	// which of it reaches the child.
	Auth Auth
	// CredentialBundle is a resolved bundle from `crewlet llm export`,
	// restored into a credential directory holding no login, or over a login
	// installed before it ([Provider.RestoreBundle]), so a container rebuilt
	// on every deploy comes up already authenticated. Kept, not restored, by
	// [New]: [Provider.RestoreConfiguredBundle] is the restore, made by
	// whoever builds the entry to run it.
	CredentialBundle string
	// Plans is the node's registry of sign-ins, which the entry's sit-outs
	// and readiness are kept in ([Plans]): the engine creates one for the
	// process and hands it to every build, so a rebuilt entry with the same
	// sign-in finds the windows its predecessor was sitting out. Nil gives
	// the entry a registry of its own, under a random key: what a `crewlet
	// llm` command, which builds one entry to report on it, needs.
	Plans *Plans
	// CredentialBundleConfigured is whether CredentialBundle came from
	// auth.credential_bundle rather than the conventional
	// CREWLET_LLM_CLI_<KEY>_CREDENTIALS ([harnessprofile.BundleVarName]), for the lines that
	// tell an operator what retires it ([Provider.LogoutNotice]) — the same
	// split [Auth.TokenConfigured] makes for the token.
	CredentialBundleConfigured bool
}

// Provider is a harness behind the language-model contract.
type Provider struct {
	key     string
	model   string
	harness string
	profile harnessprofile.Profile
	ws      *Workspace
	env     map[string]string
	auth    Auth
	timeout time.Duration

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

	// plans is the node's registry of sign-ins ([Config.Plans]), and plan
	// the [Plan] of the sign-in the entry last read itself to call as
	// (planOf), under planMu: what the entry's sit-outs and readiness are
	// kept in ([Provider.Plan]).
	plans  *Plans
	planMu sync.Mutex
	plan   *Plan
	planOf SignInID

	// zone is the time zone every child this provider starts is handed as
	// TZ ([childZone]), and so the zone a reset the harness names with no zone
	// of its own is read in.
	zone *time.Location

	// now is the clock the bench is read against — time.Now, and a fixed
	// clock in the suite, which has to watch a window end without waiting
	// a minute for it.
	now func() time.Time
}

var _ llm.Provider = (*Provider)(nil)

// New builds a harness provider.
//
// Like every other backend, it builds even when no login is present: the call
// then fails with the vendor's own "not authenticated", which names the harness
// and is exactly what `crewlet llm doctor` explains. A constructor that
// refused to exist would take the whole company down at boot over one
// provider's credentials.
func New(cfg Config) (*Provider, error) {
	profile, err := harnessprofile.Load(cfg.Harness, cfg.Overrides)
	if err != nil {
		return nil, err
	}
	// Config refuses the same shape (on every write and at apply), so a
	// document that reaches here with it was never validated — a caller
	// building the provider from code.
	if cfg.Model != "" && !profile.TakesModel() {
		return nil, fmt.Errorf(
			"harness %q: no model_args element of the %q harness carries {model}, so model %q "+
				"would be ignored — remove it, or declare overrides.model_args with {model}",
			cfg.Key, cfg.Harness, cfg.Model)
	}
	if cfg.StateDir == "" {
		return nil, fmt.Errorf("harness %q: state_dir is empty — it is derived per "+
			"provider key at config load, so an empty one means the derivation was skipped", cfg.Key)
	}
	ws, err := Shared(cfg.StateDir, cfg.Harness, profile)
	if err != nil {
		return nil, err
	}
	slots := cfg.MaxConcurrent
	if slots <= 0 {
		slots = 1
	}
	plans := cfg.Plans
	if plans == nil {
		plans = NewPlans(nil)
	}
	_, zone := childZone()
	p := &Provider{
		plans:            plans,
		zone:             zone,
		key:              cfg.Key,
		bundle:           cfg.CredentialBundle,
		bundleConfigured: cfg.CredentialBundleConfigured,
		model:            cfg.Model,
		harness:          cfg.Harness,
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
		return nil, fmt.Errorf("harness %q: timeout is zero — a harness call with no "+
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

// ModelArgument is the model exactly as this entry's runs on this host hand it
// to the harness ([harnessprofile.Profile.ModelArgument]) — what the executor's run of
// the same harness in a box is handed too, so the two name one model.
func (p *Provider) ModelArgument() string { return p.profile.ModelArgument(p.model) }

// Profile is the resolved profile, for `crewlet llm doctor`.
func (p *Provider) Profile() harnessprofile.Profile { return p.profile }

// Workspace is this provider's harness's share of its state directory, which every
// entry on the same directory and harness shares.
func (p *Provider) Workspace() *Workspace { return p.ws }

// Harness is which harness this provider drives — its profile name, which is also
// the key the in-box runner registry is keyed on ([sandbox.Manager.RunnerFor]):
// an executor run reuses the runner for the same binary rather than a second
// way to invoke it, so this one name answers both "which harness" and "which
// runner".
func (p *Provider) Harness() string { return p.harness }

// AuthMode is how this entry signs its harness in ([Auth.Mode]), for a caller that
// has to tell an operator which credential to supply: of the variables the
// engine hands the harness, api-key reads only the key and inherit-env only the
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
	return fmt.Sprintf("%s/%s/%s", providerName, p.harness, p.model)
}

// routeFor picks the channel one call's system prompt takes
// ([harnessprofile.Profile.RouteFor]), and a call that carries NO system prompt
// takes a seat's route whatever its kind: the appending channel wherever the
// profile declares one, so the harness's own prompt stays in force.
//
// A side pass takes the replacing channel because its system prompt is the
// whole instruction. With none, there is nothing to put in the vendor's place,
// and the replacing channel would put nothing there: harmless where a channel
// is passed only with text to carry, but a channel whose file carries policy is
// passed on every call ([harnessprofile.SystemPromptFile]), and its replacing form
// then renders an EMPTY system prompt. On Kimi Code that is an agent file with
// no body, which the harness refuses outright ("Missing prompt body", exit 1,
// measured on 0.42.0 and 2.3.1). The appending form with nothing to add is the
// vendor's own prompt under the same policy, which is what a call with no
// system prompt asks for. Every caller in the engine sends one today; this is
// what holds the next that does not.
func (p *Provider) routeFor(req llm.Request) harnessprofile.Route {
	if system, _ := SplitSystem(req); system == "" {
		return p.profile.RouteFor(true)
	}
	return p.profile.RouteFor(req.Agent)
}

// Complete runs one harness invocation and reads its answer.
//
// An entry its harness last reported spent is NOT called while it sits out
// ([Provider.SittingOut]): the call fails at once as a rate limit, naming the
// instant, so the seat's chain moves to its next model without a process
// launched to learn again what the last one said. Checked as the call arrives
// and again once it holds a slot, because a call that queued for one may have
// waited out the very call that sat the entry out.
//
// What the call met is remembered for [Provider.Unready].
//
// A CALL ITS CALLER CANCELLED FAILED AS A CANCELLATION, however it surfaced:
// a harness signalled with the call that exited 143 or held its pipes, a slot or
// a home the call stopped waiting for. Decided here, once, from the caller's
// context rather than from the process, which a cancellation and a crash
// end identically, so no path below can report a person's Stop as the
// vendor's outage ([llm.KindCancelled]).
func (p *Provider) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	comp, err := p.complete(ctx, req)
	if err != nil && errors.Is(ctx.Err(), context.Canceled) {
		comp, err = nil, p.fail(llm.KindCancelled, fmt.Errorf("the call was cancelled before "+
			"the %s harness answered: %w", p.harness, context.Cause(ctx)))
	}
	p.remember(err)
	if err == nil {
		p.answered(p.now())
	}
	return comp, err
}

// complete is one call, as [Provider.Complete] describes it.
func (p *Provider) complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	if err := p.SittingOut(); err != nil {
		return nil, err
	}
	if req.Tools != nil && p.profile.MCP == nil {
		// The operator's to fix, and the same on every attempt: the
		// profile says nothing about how this harness is handed an MCP server,
		// so a seat's phase on it would run with none of its tools.
		return nil, p.fail(llm.KindFatal, fmt.Errorf("the %q profile has no mcp block, so its "+
			"harness cannot be handed a seat's tools — declare providers.llm.%s.overrides.mcp, or "+
			"run seats on another entry and keep this one for text functions", p.harness, p.key))
	}
	seat := llm.SeatOf(ctx)
	callID := CallOf(ctx)

	// THE SYSTEM PROMPT ON THE CHANNEL THIS KIND OF CALL TAKES: beside the
	// harness's own for a seat at work, in place of it for a text function —
	// see [Provider.routeFor]. Lifted before the transcript is rendered, so
	// it is never both.
	route := p.routeFor(req)
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

	slot, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer slot.release()
	// AGAIN, NOW THAT THE SLOT IS HELD. A call that queued behind a full
	// entry passed the check above before the sit-out existed, so when the
	// calls ahead of it report the spent plan it would otherwise take its
	// slot, check out a home and start a whole harness only to hear the same
	// sentence — and every call behind it would wait for that too. Refused
	// here, the queue drains at once, each call handing its slot to the next
	// through the release deferred above. The check at the top stays: it
	// spares a call that arrives during a sit-out the render and the queue.
	if sitting := p.SittingOut(); sitting != nil {
		return nil, sitting
	}

	var checkout *Checkout
	if p.signsInWithKey() {
		checkout, err = p.ws.AcquireForKey(ctx, seat, callID)
	} else {
		checkout, err = p.ws.Acquire(ctx, seat, callID, p)
	}
	if err != nil {
		// CLASSIFIED, not fatal outright: a seat's generation runs the harness
		// itself now, so what failed may be a slow host or an unusable
		// login rather than this host's filesystem — see
		// [acquireFailureKind].
		return nil, p.fail(acquireFailureKind(err), err)
	}
	defer func() {
		// WITHOUT THE CALL'S CANCELLATION: a release is a teardown, and a
		// call that ended because its context was cancelled still has a
		// login the harness may have refreshed to write back. Bounded by the
		// export's own timeout rather than by nothing.
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err := checkout.Release(context.WithoutCancel(ctx)); err != nil {
			// Logged, not returned: the completion already happened
			// and failing it over a cleanup would throw away work the
			// operator paid for. A login that could not be carried out
			// is kept in the home, and the next generation to open —
			// this seat's next call, or any seat's first — carries it
			// out before it seeds — see [Workspace.Acquire].
			log.WarnContext(ctx, "harness_release_failed", "provider", p.key, "seat", seat, "error", err)
		}
	}()

	in := invocation{
		binary:  p.profile.Binary,
		args:    p.argv(req.Tools != nil),
		dir:     checkout.Work,
		env:     buildEnv(p.profile, checkout, p.env, p.auth),
		timeout: p.timeout,
		grace:   p.termGrace,
		failOn:  p.profile.FailureMarkers,
	}
	// Streamed only for a JSONL profile, where each event's text IS a
	// fragment of the answer. The other two output modes are extracted
	// from the WHOLE of stdout — a fenced block, one JSON document — so
	// there is no faithful incremental view of them, and forwarding raw
	// lines would stream a banner and a half-written fence as though they
	// were what the model said. A run with tools streams what its record
	// reader reads instead — see [Provider.serveTools].
	if req.Tools == nil && req.Streaming() && p.profile.EffectiveOutput() == harnessprofile.OutputJSONL {
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
	// about it afterwards — see [harnessprofile.Profile.SessionArgs]. After the
	// model arguments for the same reason they precede everything else: a
	// harness taking its prompt on argv reads the first non-flag argument.
	var session string
	if len(p.profile.SessionArgs) > 0 {
		session = newSessionID()
		for _, arg := range p.profile.SessionArgs {
			in.args = append(in.args, strings.ReplaceAll(arg, "{session}", session))
		}
	}
	// A USAGE REPORT THE HARNESS WRITES ITSELF, for a vendor whose one-shot
	// surface prints the answer and nothing else. Appended before the
	// system and prompt arguments for the same reason the model flag is:
	// a harness taking its prompt on argv reads the first non-flag argument,
	// so nothing may come after it.
	var usagePath string
	if len(p.profile.UsageFileArgs) > 0 {
		usagePath = filepath.Join(checkout.Work, usageFileName)
		for _, arg := range p.profile.UsageFileArgs {
			in.args = append(in.args, strings.ReplaceAll(arg, "{usage_file}", usagePath))
		}
	}
	// THE SEAT'S TOOLS, served for this run alone, and the harness told how to
	// reach them — before the system prompt and the prompt, as the model is.
	// Closed on every way out, BEFORE the checkout is released (deferred
	// earlier, so run later): the release waits for the calls still running,
	// and a refreshed login's carry works in the home.
	var tools *toolRun
	if req.Tools != nil {
		tools, err = p.serveTools(ctx, req, checkout, slot, &in, session, system+prompt)
		if err != nil {
			return nil, err
		}
		defer tools.close()
	}
	// EVERY CALL WHERE THE FILE CARRIES POLICY, not only the calls that
	// have something to put in it — see [harnessprofile.SystemPromptFile]. A
	// channel that is a vendor agent file declares that harness's tool denial in
	// the same file, so skipping it on a request with no system prompt
	// would hand the vendor's default agent, and every tool it has, to
	// exactly the calls that would never notice. A file that would hold the
	// system prompt and nothing else is written only when there is one
	// ([harnessprofile.SystemPromptFile.CarriesPolicy]): a harness that reads it on
	// every run still reads an empty one, and passing no channel is what an
	// empty system prompt means everywhere else ([SplitSystem]).
	if ch := route.Channel; system != "" || ch.File.CarriesPolicy() {
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
	// profile puts the model flag before it: a harness taking its prompt on
	// argv reads the first non-flag argument, so anything appended after it
	// is read as part of the prompt.
	switch p.profile.EffectivePromptMode() {
	case harnessprofile.PromptArgv:
		// PromptArgs and then the prompt, ADJACENT and last: a harness that
		// takes its prompt as a flag's value needs the two together, and
		// anything appended between them becomes the prompt instead.
		in.args = append(in.args, p.profile.PromptArgs...)
		in.args = append(in.args, prompt)
	case harnessprofile.PromptFile:
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

	// A SERVE LIFECYCLE'S SERVER, started now that everything the run writes
	// into its working directory is there — see [toolRun.up]. Its end ends
	// the run, through the context the run is made under.
	runCtx := ctx
	if tools != nil {
		var up bool
		runCtx, up, err = tools.up(ctx, p, &in)
		if err != nil {
			return nil, err
		}
		if !up {
			// The host ended the run before the harness's server was up:
			// nothing ran, and nothing was spent.
			return &llm.Completion{Model: p.model}, nil
		}
	}
	started := time.Now()
	res, err := run(runCtx, in)
	if err != nil {
		// NO HARNESS RAN, which is all run's error ever says: the binary is
		// missing or not executable here, or the host would not give it a
		// pipe or a process. That is this host's fault, the one exec failure
		// the next member is no answer to, so it is fatal. A harness that DID run
		// comes back as a result whatever happened to it, and [completion]
		// classifies it by what that was.
		return nil, p.fail(llm.KindFatal, fmt.Errorf("%w: %w", llm.ErrNotRunnable, err))
	}
	log.DebugContext(ctx, "harness_call", "provider", p.key, "agent", p.harness, "model", p.model,
		"seat", seat, "exit", res.exitCode, "elapsed_ms", time.Since(started).Milliseconds())
	connected := false
	if tools != nil {
		// THE RECORD WHOLE, THEN THE RUN RELEASED: every request the record
		// showed is told before the host's server for the run is retired,
		// which waits for every call still running on it.
		tools.ran(ctx, res)
		connected = tools.finish()
		reportOffered(ctx, tools.reader.offeredTools())
	}

	// READ BEFORE THE CHECKOUT IS RELEASED, which removes the directory.
	// A missing or unreadable report is not an error: the vendor writes it
	// "even when the run fails", so its absence says the run did not get
	// that far — and failing a completion that the model answered, over a
	// count, would throw away work the operator paid for. The counts then
	// fall back to whatever stdout said, which for such a harness is an
	// estimate.
	var usageDoc string
	if usagePath != "" {
		raw, readErr := os.ReadFile(usagePath) //nolint:gosec // rooted in the per-call dir
		switch {
		case os.IsNotExist(readErr):
		case readErr != nil:
			log.WarnContext(ctx, "harness_usage_file_unreadable", "provider", p.key,
				"agent", p.harness, "path", usagePath, "error", readErr)
		case strings.TrimSpace(string(raw)) == "":
			// Written and empty is not "the run did not get that far": the
			// vendor began its report and wrote nothing into it, and what
			// is read below could not tell this from no file at all.
			log.WarnContext(ctx, "harness_usage_file_unreadable", "provider", p.key,
				"agent", p.harness, "path", usagePath, "error", "the file is empty")
		default:
			usageDoc = string(raw)
		}
	}

	// A USAGE REPORT ASKED FOR AFTERWARDS, for a vendor that records the
	// call's counts and does not print all of them — see
	// [harnessprofile.Profile.UsageArgs]. Asked whatever the call's outcome,
	// because a refused call may still have spent tokens; and like a usage
	// file, a report that cannot be had leaves the counts to stdout rather
	// than failing an answer the operator paid for. In the working directory
	// the call ran in, before the checkout is released.
	//
	// A RUN WITH TOOLS ASKS ON EVERY OUTCOME, a timeout and a run the host
	// stopped included, and past the cancellation that may have ended it
	// (bounded by the command's own timeout): such a run is a phase's whole
	// loop, and its report is the one count of what the run spent before it
	// was cut.
	//
	// A RUN WITH TOOLS MAY ASK THROUGH ITS HARNESS'S SERVER ([harnessprofile.MCP.UsageArgs]),
	// which is then asked while it is still up — and not at all once a run cut
	// short took it down.
	if len(p.profile.UsageArgs) > 0 && (!res.timedOut || tools != nil) {
		args := make([]string, 0, len(p.profile.UsageArgs))
		for _, arg := range p.profile.UsageArgs {
			args = append(args, strings.ReplaceAll(arg, "{session}", session))
		}
		env := buildEnv(p.profile, checkout, p.env, p.auth)
		ask := true
		reportCtx := ctx
		if tools != nil {
			reportCtx = context.WithoutCancel(ctx)
			if len(p.profile.MCP.UsageArgs) > 0 {
				args, env, ask = tools.usageCommand(p, session, env)
			}
		}
		if ask {
			report, reportErr := p.runAuxiliary(reportCtx, args, checkout.Work, env, "")
			switch {
			case reportErr != nil:
				log.WarnContext(ctx, "harness_usage_report_failed", "provider", p.key,
					"agent", p.harness, "error", reportErr)
			case strings.TrimSpace(report) == "":
				// A command that succeeded and printed nothing reported
				// nothing, and the empty string handed on below reads as no
				// report at all — so this is the one place that can say so.
				log.WarnContext(ctx, "harness_usage_report_failed", "provider", p.key,
					"agent", p.harness, "error", fmt.Sprintf("%s %s exited 0 and printed nothing",
						p.profile.Binary, strings.Join(args, " ")))
			default:
				usageDoc = report
			}
		}
	}

	if tools != nil {
		return p.toolCompletion(ctx, res, usageDoc, tools, connected)
	}
	return p.completion(ctx, system+prompt, res, usageDoc)
}

// newSessionID mints the id [harnessprofile.Profile.SessionArgs] names a call's
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

// argv is the invocation's arguments: the profile's completion argv — its tool
// route's in place of it for a run with tools, where the route declares one,
// rendered by [Provider.serveTools] — then the model, then (in argv mode) the
// prompt, which the caller appends.
func (p *Provider) argv(tools bool) []string {
	complete := p.profile.CompleteArgs
	if m := p.profile.MCP; tools && m != nil && len(m.CompleteArgs) > 0 {
		complete = m.CompleteArgs
	}
	args := append([]string(nil), complete...)
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
// sent is everything the engine handed the harness — the system prompt on its
// channel and the rendered prompt — and is read only for the estimate a harness
// that reports no usage is charged. The system prompt is in it because a
// budget charged for the transcript alone undercounts every call by the
// largest stable thing in it; what is NOT in it is a vendor's own system
// prompt kept beside the engine's, which the engine never sees.
func (p *Provider) completion(
	ctx context.Context, sent string, res *rawResult, usageDoc string,
) (*llm.Completion, error) {
	if res.timedOut {
		return nil, p.fail(llm.KindTimeout, fmt.Errorf(
			"the harness did not answer within %s — raise timeout_seconds if this model "+
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
			"the harness wrote more than %d bytes to stdout and %d were dropped, so "+
				"the answer is incomplete. No completion comes near that size, so "+
				"the harness printed something besides one: run `crewlet llm doctor %s` "+
				"to see what, or point this provider at a model that streams less",
			maxOutput, res.droppedStdout, p.key))
	}

	out := extract(p.profile, res.stdout)
	// Whether the harness said the run FAILED — a non-zero exit, or its own flag
	// at the profile's error_paths. It decides where a marker may stand, and
	// that the call is refused below rather than answered.
	failed := res.exitCode != 0 || out.failed
	// The harness's own report wins over anything stdout happened to carry:
	// a profile only names one when its vendor puts the counts nowhere
	// else, so a hit here is the only real figure there is. A report that
	// ARRIVED and could not be read is said out loud, because the counts
	// then fall back to stdout's — an estimate, for a harness that reports
	// none there — on every call until somebody notices the budget
	// figures are made up.
	//
	// NOT FOR A RUN THE HARNESS SAID FAILED, which is refused below and charged
	// nothing, so no count falls back to anything. A vendor's report of a
	// failed run may hold no counts by design — Hermes writes them as null
	// for a refused key or a spent plan (measured on 0.21.5) — and warning
	// of drift on every such call names the profile as broken exactly when
	// the operator is reading the log for the refusal.
	if outcome := out.applyUsageReport(p.profile, usageDoc); outcome.unreadable() && !failed {
		fallback := "an estimate, four characters a token"
		if out.reported {
			fallback = "the counts stdout reported"
		}
		log.WarnContext(ctx, "harness_usage_report_unreadable", "provider", p.key,
			"agent", p.harness, "source", usageSource(p.profile), "reason", outcome.String(),
			"input_paths", harnessprofile.PathList(p.profile.Usage.Input),
			"output_paths", harnessprofile.PathList(p.profile.Usage.Output),
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
	// since the harness answers with prose where the envelope should be.
	//
	// Not stdout unconditionally, though, which was the first shape of
	// this: some shipped markers are short generic phrases rather than
	// vendor sentences, and scanning a healthy run's whole telemetry for
	// them adds false-positive surface for nothing. When the answer WAS
	// located it is the only thing the vendor said that matters.
	//
	// The events scope reads neither as text: it decodes the whole of
	// stdout and looks only at the harness's own failure events — see
	// [harnessprofile.MarkerScope] — which is why stdout itself is handed over
	// beside what was said. And whether the harness said the run FAILED is
	// handed over too, because it decides where in what was said a
	// sentinel may stand — see [classifyMarkers].
	said := nonEmpty(out.text, res.stdout)
	if hit, ok := classifyMarkers(p.profile, harnessOutput{
		said: said, stdout: res.stdout, stderr: res.stderr, failed: failed, at: p.now(), zone: p.zone,
	}); ok {
		return nil, p.classified(ctx, hit, nonEmpty(hit.Said, firstLine(said, res.stderr)))
	}

	// A RUN THE HARNESS SAYS FAILED, FOR NO REASON A PROFILE RECOGNISES, IS A
	// SERVER FAILURE — the harness or its vendor did not answer: an overload or
	// a 5xx it relayed once its own retries ran out, a crash, a reason in
	// words no marker knows. It used to be FATAL, on the reasoning that every
	// member of the seat's chain would refuse the call identically, and that
	// is true only of a call the engine cannot make at all ([llm.KindFatal]).
	// The next member is another login, another vendor or another binary,
	// which does not share this one's outage: trying it costs one process
	// launch, and stopping cost the seat every turn for as long as the outage
	// lasted, each redelivered against the same dead head. Nothing is
	// repeated by the walk, either: the calls a run made are on its
	// session's record, which the next member continues from rather than
	// runs again (internal/providers/llm/chain).
	//
	// NOT BENCHED: only a spent plan sits the entry out ([Provider.bench]),
	// because only a spent plan is known to last. A failure the next call
	// may not meet must not hold every seat on the node off a working entry.
	if failed {
		what := fmt.Sprintf("the %s harness exited %d", p.harness, res.exitCode)
		if res.exitCode == 0 {
			what = fmt.Sprintf("the %s harness exited 0 and reported that the run failed", p.harness)
		}
		return nil, p.fail(llm.KindServer, fmt.Errorf("%s:\n%s", what,
			tail(nonEmpty(res.stderr, out.text, res.stdout))))
	}
	// A RUN NOT SEEN TO ITS END IS A SERVER FAILURE TOO, whatever it
	// printed. A stdout that something the harness left running still held a
	// grace after the harness was done was cut there, so no reading of it can be
	// shown to be the whole answer: the refusal a clipped stdout gets above,
	// for the same reason. And a wait that failed is a harness that started and
	// did not finish as one. Neither is a call the engine could not make, so
	// the chain may carry it to the next member, and nothing says it lasts,
	// so nothing is sat out. Both used to come back from [run] as an error,
	// read as fatal: the walk stopped at the head with a whole answer on
	// stdout, the harness having exited 0.
	//
	// After the markers, which classify on what the vendor SAID: a spent plan
	// reported on a stdout a helper then held is still a spent plan.
	if why := res.unfinished(); why != "" {
		return nil, p.fail(llm.KindServer, fmt.Errorf("the %s harness exited 0, but %s:\n%s",
			p.harness, why, tail(nonEmpty(res.stderr, res.stdout))))
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
			"the %s harness exited 0 but printed nothing at all%s",
			p.harness, stderrDetail(res.stderr)))
	}
	if !out.located {
		// THE PROFILE HAS DRIFTED FROM THE HARNESS. Its output parsed, and
		// none of the paths that say where the answer lives resolved to
		// a string — so this build cannot tell which part of what the
		// harness printed is the model's reply, and the honest answer is
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
			"the %s harness's output parsed, but none of the text_paths this profile "+
				"looks in resolved (%s) — the profile no longer matches the "+
				"installed harness, so nothing it printed can be read as the model's "+
				"reply. Run `crewlet llm doctor %s` and set "+
				"providers.llm.%s.overrides.text_paths. It printed:\n%s",
			p.harness, harnessprofile.PathList(p.profile.TextPaths), p.key, p.key, tail(res.stdout)))
	}
	// LOCATED AND EMPTY IS AN ANSWER OF NOTHING, NOT A FAULT. The harness
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
	// from being reported as an outage of a harness that demonstrably answered.
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
	if out.reported {
		// InputTokens is ALWAYS the full prompt count, cache included —
		// the contract's rule, and what keeps it a correct budget figure
		// whatever the cache did. CacheRead and CacheWrite break the same
		// total down for cost reporting; adding them again double-counts,
		// which is why a vendor whose input count already holds them says
		// so ([harnessprofile.UsagePaths.InputIncludesCache]). A model-side
		// tool's prompt a harness reports apart from its input is billed as
		// input, so it is charged as input — see [harnessprofile.UsagePaths].
		comp.InputTokens = p.profile.Usage.PromptTokens(out.input, out.cacheRead, out.cacheWrite) + out.toolPrompt
		// Reasoning a harness reports apart from its output is billed as
		// output, so it is charged as output — see [harnessprofile.UsagePaths].
		comp.OutputTokens = out.output + out.reasoning
		comp.CacheRead = out.cacheRead
		comp.CacheWrite = out.cacheWrite
	} else {
		comp.InputTokens = EstimateTokens(sent)
		comp.OutputTokens = EstimateTokens(out.text)
	}

	if strings.TrimSpace(comp.Content) == "" {
		// Warned rather than returned, because nothing above this frame
		// can reconstruct WHY: the output token count is the evidence
		// that the model worked and said nothing (a thinking-only round
		// bills hundreds), and the text paths are the evidence a reader
		// needs if it turns out to be profile drift after all. Same
		// shape as agent/prefetch's own answered-nothing warning.
		log.WarnContext(ctx, "harness_answered_nothing", "provider", p.key,
			"agent", p.harness, "model", p.model, "text_paths", harnessprofile.PathList(p.profile.TextPaths),
			"output_tokens", comp.OutputTokens, "reported_usage", out.reported)
	}
	return comp, nil
}

// classifyMarkers recognises a spent subscription or an expired login in a
// harness's own words.
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
// to check a shipped profile's sentinels against the output its harness actually
// produces is to be able to ask without a running provider.
//
// # What the model said is not the vendor's report
//
// Verbatim is not enough on its own, because what the harness said on stdout is,
// on every healthy run, the MODEL's words — its prose, and the arguments of
// every tool call it makes — and a seat asked to add backoff for the GitHub
// API writes "rate limit" as readily as a vendor reports one. Matched anywhere
// in there, a sentence the model wrote threw its answer away as a spent plan
// and sat the entry out for every seat on the node, and the redelivered turn
// wrote it again a minute later. So in the default scope a sentinel in what
// was said counts in two places only:
//
//   - ANYWHERE, in a run the harness itself reported failed — a non-zero exit,
//     or its own failure flag ([harnessprofile.Profile.ErrorPaths]). The harness has
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
// Stderr is matched anywhere: it is the harness's own channel, and the answer
// does not reach it. So are the fields of a FAILED run with tools' own failure
// events ([harnessprofile.MCP.FailureEvents]): that run's answer is its record's
// last prose, which a failed run never wrote, and a harness whose stderr says only
// that it failed (Muse Code: "run ended with Failed") says why there alone.
// The events scope reads the harness's own failure events and nothing else, and
// the stderr scope stderr alone — see [harnessprofile.MarkerScope].
//
// # Structured reports first, then the words, then dated
//
// The harness's own structured reports of a limit and of a refused sign-in are
// read before any sentinel ([classifyEvents]), and THE RUN'S LAST WORD
// DECIDES among them: a refused token retried and then met by a spent plan
// ended on the plan, and reads as a limit. A refused sign-in is never read
// from a run that answered, by event or by sentinel. A limit, once
// recognised, is dated and named ([dateLimit]): the reset and the window from
// the harness's own structured event where it wrote one, else from the vendor's
// sentence.
func classifyMarkers(p harnessprofile.Profile, out harnessOutput) (markerHit, bool) {
	hit, ok := classifyEvents(p, out)
	if !ok {
		hit, ok = classifyReport(p, out)
	}
	if ok && hit.Kind == llm.KindRateLimit {
		hit = dateLimit(p, hit, out)
	}
	return hit, ok
}

// classifyReport is [classifyMarkers]' recognition of a limit or a login in
// the harness's structured limit report and its words.
func classifyReport(p harnessprofile.Profile, out harnessOutput) (markerHit, bool) {
	var haystacks []haystack
	switch p.EffectiveMarkerScope() {
	case harnessprofile.MarkerScopeEvents:
		return classifyFailureEvents(p, out.stdout)
	case harnessprofile.MarkerScopeStderr:
		haystacks = []haystack{{text: out.stderr, source: sourceStderr}}
	default:
		said := haystack{text: out.said, source: sourceReport, anchored: true}
		if out.failed {
			said = haystack{text: out.said, source: sourceFailedRun}
		}
		haystacks = []haystack{said, {text: out.stderr, source: sourceStderr}}
		if out.failed && out.events != "" {
			haystacks = append(haystacks, haystack{text: out.events, source: sourceEvent})
		}
	}
	for _, marker := range p.LimitMarkers {
		for _, hay := range haystacks {
			idx, found := hay.find(marker.Sentinel)
			if !found {
				continue
			}
			return markerHit{
				Kind:   llm.KindRateLimit,
				Reset:  resetAt(lineAt(hay.text, idx), marker.Reset, out.at, out.zone),
				Said:   lineAt(hay.text, idx),
				Source: hay.source,
			}, true
		}
	}
	for _, marker := range p.AuthMarkers {
		for _, hay := range haystacks {
			if hay.anchored {
				// NOT FROM A RUN THAT ANSWERED. A refused sign-in is a
				// failure the harness reports as one, and a clean answer that
				// opens with "Failed to authenticate" is the model's
				// account of somebody else's error; read as the vendor's,
				// it took the answer and set a verdict on every entry on
				// the sign-in.
				continue
			}
			if idx, found := hay.find(marker.Sentinel); found {
				return markerHit{Kind: llm.KindAuth, Said: lineAt(hay.text, idx), Source: hay.source}, true
			}
		}
	}
	return markerHit{}, false
}

// harnessOutput is what one finished run gives [classifyMarkers] to read.
type harnessOutput struct {
	// said is what the harness said: the located answer, or the whole of stdout
	// when none was located.
	said string
	// stdout is the whole of it, which only the events scope reads, and only
	// as decoded events.
	stdout string
	// stderr is the harness's own diagnostic channel.
	stderr string
	// events is a run with tools' own failure events, by the profile's
	// fields, one a line ([harnessprofile.MCP.FailureEvents]).
	events string
	// failed is the harness's own word that the run failed: a non-zero exit, or
	// the flag at the profile's error_paths.
	failed bool
	// at is when the run ended: what a reset the vendor names as a time of
	// day, or as a delay, is read against.
	at time.Time
	// zone is the zone the harness ran in, which a reset it names with no zone
	// of its own is read in ([childZone]); nil reads UTC.
	zone *time.Location
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
	// sourceFailedRun is what a run the harness reported failed said on stdout,
	// where a sentinel counts anywhere.
	sourceFailedRun markerSource = "failed-run"
	// sourceStderr is the harness's stderr.
	sourceStderr markerSource = "stderr"
	// sourceEvent is one of the harness's own failure events, under
	// [harnessprofile.MarkerScopeEvents] or, for a failed run with tools, by the
	// profile's [harnessprofile.MCP.FailureEvents].
	sourceEvent markerSource = "event"
)

// markerHit is what a matched sentinel tells the caller.
//
// Said is the load-bearing field and the reason this is a struct. The
// classification used to be reported with [firstLine] over the whole
// haystack, which is the vendor's own sentence for a harness that prints prose —
// and the FIRST EVENT of a stream for a harness that prints JSONL. A muse-code
// rate limit read `run.lifecycle.started`: correctly classified, and useless
// to the operator who has to act on it.
type markerHit struct {
	Kind llm.ErrorKind
	// Reset is when the vendor said the limit lifts, zero where it named no
	// instant ([resetAt]).
	Reset time.Time
	// Said is the line the sentinel matched on — the vendor's own words,
	// in the place the vendor put them.
	Said string
	// Source is where it matched, which is why it counts as the vendor's.
	Source markerSource
	// Window is which limit a rate limit is ([dateLimit]).
	Window llm.LimitWindow
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

// fail wraps an error as a classified provider failure.
func (p *Provider) fail(kind llm.ErrorKind, err error) error { return p.failure(kind, err) }

// failure is [Provider.fail] as its concrete type, for a caller that adds to
// it.
func (p *Provider) failure(kind llm.ErrorKind, err error) *llm.Error {
	return &llm.Error{Kind: kind, Provider: providerName + "/" + p.harness, Model: p.model,
		Key: p.key, Harness: p.harness, Subscription: p.AuthMode() == AuthSubscription, Err: err}
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

// stderrDetail appends a harness's stderr to a message, or nothing when it wrote
// none.
//
// A trailing empty ":" after a sentence that already said what went wrong is
// how a message stops reading like one — and stderr is genuinely absent on the
// paths that use this, because a harness that exits 0 usually says nothing there.
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
