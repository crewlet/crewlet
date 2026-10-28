package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/node"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	llmchain "github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
	"github.com/crewlet/crewlet/internal/schedule"
	seatpkg "github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/tracing"
)

// The sandbox's engine-side wiring: which concrete thing satisfies which seam.
//
// The manager and its provider are built from providers.sandbox and swapped on
// an apply, like the LLM providers beside them. The coordinator and the waiter
// are NOT: they hold the busy set and the poll loop, which are facts about
// this PROCESS rather than about a config revision — rebuilding them on an
// apply would forget which seats are mid-run and start a second poll loop
// against the same rows. So they are built ONCE, by the first company that
// reaches a sandbox cell — at boot, or at whichever apply brings one — and
// every later revision only swaps the manager under them.

// buildSandbox constructs the manager for a company, or nil when no seat can
// run code.
//
// Nil is an ordinary outcome, not a failure: providers.sandbox is absent in
// most deployments, and a build with none simply never offers run_sandbox.
// What is NOT ordinary is a configured provider that cannot be constructed —
// that fails the apply, because the alternative publishes a company whose
// sandbox-enabled seats plan around a box they will never get.
func buildSandbox(c *config.Company, env *config.Resolver, otel *sandbox.OtelReceiver) (*sandbox.Manager, error) {
	spec := c.Providers.Sandbox
	if spec == nil || !spec.Enabled() {
		return nil, nil
	}
	// ONLY THE CELLS THE COMPANY ACTUALLY REACHES, from the same computation
	// validation reads. Building the whole catalogue eagerly constructed a
	// container backend for a company whose seats all run direct, and failed
	// the apply demanding an image the validator had just refused as a field
	// nothing would read.
	providers, err := buildSandboxProviders(spec, env, c.SandboxPlacements())
	if err != nil {
		return nil, err
	}
	if len(providers) == 0 {
		return nil, nil
	}
	return sandbox.NewManager(sandbox.ManagerOptions{
		Providers:          providers,
		DefaultPlacement:   sandbox.Placement(spec.RunIn()),
		Runners:            sandboxRunners(),
		DefaultCodingAgent: string(spec.DefaultCodingAgent),
		DefaultTimeout:     seconds(spec.Timeout()),
		DefaultPauseTTL:    secondsPtr(spec.PauseTTL()),
		DefaultMaxTurns:    spec.DefaultMaxTurns,
		DefaultSetup:       setupSteps(spec.Setup),
		Telemetry:          otel,
	})
}

// sandboxRunners is every coding agent this build can drive.
//
// A FUNCTION rather than a literal at the one call site, because a second
// reader needs the same list: `crewlet llm doctor` refuses a box-mode entry
// whose CLI has no runner, and it checks against codingagent.Names(). A test
// holds the two together — a drift would have the doctor pass an entry the
// engine then refuses at the seat's first turn.
func sandboxRunners() map[string]sandbox.Runner {
	return map[string]sandbox.Runner{
		codingagent.ClaudeCodeName: codingagent.NewClaudeCode(),
		codingagent.OpenCodeName:   codingagent.NewOpenCode(),
	}
}

// buildSandboxProviders turns the catalogue into one backend per placement it
// configures.
//
// THE CLOSED SET AND THIS FUNCTION ARE THE SAME LIST, asserted by a test,
// because nothing else connects them: the last time they disagreed the
// config's default named a backend with no case here, so a company validated
// cleanly, reported a configured sandbox on the dashboard, and failed at its
// first coding run.
//
// ONE LOCAL BACKEND PER LOCAL PLACEMENT, not one shared between them: the two
// differ in exactly one option, and a single instance would have to be told
// which cell it was serving on every call — which is the block-wide mode this
// whole reshape removed, reintroduced one layer down.
func buildSandboxProviders(spec *config.SandboxProvider, env *config.Resolver, reached map[config.Placement]string) (map[sandbox.Placement]sandbox.Provider, error) {
	built := make(map[sandbox.Placement]sandbox.Provider, len(reached))
	// WALKED IN THE CLOSED SET'S ORDER, not the map's: a map iterates
	// randomly, and an error naming whichever backend happened to come
	// first would differ between two runs of the same broken config.
	for _, placement := range config.BackendPlacements() {
		if _, want := reached[placement]; !want {
			continue
		}
		if !spec.Configured(placement) {
			// Refused by validation, so this is the belt to that brace: an
			// embedder building a company by hand reaches it, and a
			// silently missing backend would offer run_sandbox to a seat
			// whose runs can never start.
			return nil, fmt.Errorf("providers.sandbox: %q is reached by %s and "+
				"has no backend configured", placement, reached[placement])
		}
		provider, err := buildSandboxProvider(spec, env, placement)
		if err != nil {
			return nil, err
		}
		built[sandbox.Placement(placement)] = provider
	}
	return built, nil
}

func buildSandboxProvider(spec *config.SandboxProvider, env *config.Resolver, placement config.Placement) (sandbox.Provider, error) {
	if spec.Fake {
		// The in-process double, for a deployment demonstrating the flow
		// without a real box. Named in config rather than inferred, so
		// nobody runs one by accident — and it answers every placement,
		// so a demonstration config differs from a real one in exactly
		// one line.
		return sandbox.NewFakeProvider(), nil
	}
	switch placement {
	case config.PlacementE2B:
		e2b := spec.E2B
		// RESOLVED HERE, at the moment the provider is built, which is
		// the only place the key's value exists in this process. Tier B
		// stores its references verbatim — that is what keeps an exported
		// revision free of resolved secrets — so a backend handed
		// e2b.APIKey directly would authenticate with the literal
		// "${E2B_API_KEY}" and get a 401 naming the vendor rather than
		// the misconfiguration.
		//
		// The DOMAIN is resolved on the same terms and for a reason of its
		// own: a staging cluster and a production one are the same config
		// with a different variable, and passing the reference through
		// would point every box at a host called "${E2B_DOMAIN}".
		return sandbox.NewE2B(sandbox.E2BOptions{
			APIKey:   resolvedOr(env, e2b.APIKey),
			Domain:   resolvedOr(env, e2b.Domain),
			Template: e2b.Template,
		})
	case config.PlacementDirect, config.PlacementContainer:
		return sandbox.NewLocal(localOptions(spec.Local, env, placement))
	default:
		// UNREACHABLE THROUGH A PARSED CONFIG, because the closed set and
		// this switch are the same list.
		//
		// Answered rather than panicked: an embedder building a spec by
		// hand is a caller, not an operator to be crashed at.
		return nil, fmt.Errorf("providers.sandbox: %q is not one of %v",
			placement, config.Placements)
	}
}

// localOptions maps the local block onto the backend's options for one cell.
//
// runtime_env IS RESOLVED HERE, at the edge, as an MCP server's `env:` is
// ([resolveServer]): Tier B stores its references verbatim, so a backend handed
// the block's map would put the literal "${SSH_AUTH_SOCK}" into the runtime's
// environment, and the sandbox package must not be able to read the secret
// store to do better.
func localOptions(local *config.LocalSandbox, env *config.Resolver, placement config.Placement) sandbox.LocalOptions {
	return sandbox.LocalOptions{
		Placement:  sandbox.Placement(placement),
		StateDir:   local.StateDir,
		Image:      local.Image,
		Runtime:    string(local.Runtime),
		Network:    local.Network,
		RunArgs:    local.RunArgs,
		RuntimeEnv: resolveMap(env, local.RuntimeEnv),
	}
}

// resolveMap expands every ${VAR} in a config map.
func resolveMap(env *config.Resolver, in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = env.Value(v)
	}
	return out
}

// resolvedOr reads a config value through this node's chain, falling back to
// the literal when there is no resolver.
//
// A NIL RESOLVER IS A CALLER INSIDE THE PROCESS — a test, an embedder — and
// handing it the literal is right: it wrote the literal. A running engine
// always has one.
func resolvedOr(env *config.Resolver, value string) string {
	if env == nil {
		return strings.TrimSpace(value)
	}
	return strings.TrimSpace(env.Value(value))
}

// setupSteps converts the PROVIDER-WIDE steps a `providers.sandbox` block
// declares, which are settings and therefore config's.
//
// A translation rather than a shared type, so the sandbox package does not
// import the config package: a setup step is a runtime instruction, and
// keeping the two apart is what lets the sandbox layer be tested with a step
// built in a test rather than a YAML document parsed into one. The values are
// carried VERBATIM — what a step's file and env become is decided at launch,
// by [Engine.runEnvironment].
func setupSteps(steps []config.SandboxSetupStep) []sandbox.SetupStep {
	if len(steps) == 0 {
		return nil
	}
	out := make([]sandbox.SetupStep, 0, len(steps))
	for _, s := range steps {
		out = append(out, sandbox.SetupStep{
			Name: s.Name, Files: s.Files, Commands: s.Commands,
			Env: s.Env, OptionalKeys: s.OptionalKeys, Brief: s.Brief,
			TimeoutSeconds: s.Timeout(),
		})
	}
	return out
}

// seatSetupSteps converts a SEAT's own steps, which ride the org chart.
//
// TWO CONVERTERS BECAUSE THERE ARE TWO SOURCES, and only one thing about them
// could drift — what an unset timeout means — which is why that rule is
// [org.SetupTimeout] and neither of these states it. Everything else here is
// field-for-field and the compiler checks it.
func seatSetupSteps(steps []org.SandboxSetupStep) []sandbox.SetupStep {
	if len(steps) == 0 {
		return nil
	}
	out := make([]sandbox.SetupStep, 0, len(steps))
	for _, s := range steps {
		out = append(out, sandbox.SetupStep{
			Name: s.Name, Files: s.Files, Commands: s.Commands,
			Env: s.Env, OptionalKeys: s.OptionalKeys, Brief: s.Brief,
			TimeoutSeconds: org.SetupTimeout(s.TimeoutSeconds),
		})
	}
	return out
}

// runEnv is one coding run's environment and provisioning, read through this
// node's resolver by the rule every tool server's values are
// ([resolveServer]): every reference resolved, or why the run lacks it.
type runEnv struct {
	// env is the run environment the box's setup commands and its coding
	// agent are handed — the engine's own facts and the configuration's —
	// before the LLM credentials and the telemetry are laid over it.
	env map[string]string

	// setup is the provider-wide steps then the seat's own, every file's
	// body read as the content it is.
	setup []sandbox.SetupStep

	// gap is what of either resolved to nothing and is not declared
	// optional, judged against the integrations minting it — nil when
	// nothing. keys and files name where it sits, for a log line.
	gap         *credentialMissing
	keys, files []string

	// facts is what of the engine's own facts — the seat's email, a
	// reference that resolved to nothing — the run starts without, judged
	// as gap is; facted names them. Never part of gap: no layer can declare
	// a fact optional, and code work does not stop for a commit address.
	facts  *credentialMissing
	facted []string
}

// runEnvironment is one coding run's environment and provisioning: the
// provider-wide setup steps, then the seat's own, and the run environment
// they, the seat's sandbox block and the engine assemble. ONE PLACE FOR BOTH
// LAUNCH PATHS — the run_sandbox tool and a box-mode executor — because a
// box is provisioned the same way whichever of them opens it. Nil-safe on the
// gate, since a box-mode executor runs in a box whether or not the seat
// has a sandbox block of its own, and on c, a box launched with no company to
// read a provisioning plan from — every variable is then the operator's.
//
// # The environment
//
// THE ENGINE CONTRIBUTES ONLY TOOL-AGNOSTIC FACTS: the agent's identity, as
// CREWLET_AGENT_HANDLE and CREWLET_AGENT_EMAIL — per-launch values static
// config cannot know, which a setup recipe maps into whatever shape its tool
// needs. Every tool-specific variable comes from config: an external token is
// DECLARED in role.sandbox.env or a setup step's env, and the engine never
// names one of its own. Precedence, later winning: identity, then the setup
// steps' contributions, then the seat's own env — so an operator's explicit
// value always beats a step's default.
//
// RESOLVED EXACTLY ONCE, here, at launch — not at load. These values are
// where an operator declares a code-host token, and Tier B stores its
// references verbatim so an exported revision carries no resolved secret.
// Resolving at load as well would double-resolve and mangle any secret whose
// real value contains a literal ${...}.
//
// # Every reference resolves, or the key is declared optional
//
// By the rule a tool server's values are held to (toolcredentials.go), and
// for the reason it is: a run handed an empty code-host token clones nothing
// and pushes nothing, after spending its box, its install and its model's
// tokens, and nothing in a value can say that a setting — a test
// DATABASE_URL, a proxy — may be absent. So a key whose value does not
// resolve, under the declaration of whichever layer wrote the value that
// won, is LEFT OUT whole where that layer lists it under `optional_keys`, and
// is otherwise the run's gap: one a provisioning pass mints for the seat,
// one that pass cannot mint until a person acts, or one nothing will — and a
// value a pass mints for the seat is waited for whatever the declaration
// says, as a tool server's is ([refGap.hold]). The engine's own identity
// facts are never a gap, since no layer can declare them: each is contributed
// when it resolves, and one that does not — a seat's `email:` naming a
// variable nobody set — is left out and SAID, because a setup recipe's
// `git config user.email "$CREWLET_AGENT_EMAIL"` otherwise commits as nobody
// with nothing to say why.
//
// # The files
//
// A file is CONTENT rather than a setting ([secrets.ReadContent]): a body
// that is exactly one `${VAR}` becomes that variable's value, byte for byte,
// and any other body is written as it is. The env is the engine's setting
// and the run environment gets the value; a `${HOME}` in a script and a
// `${NPM_TOKEN}` in an .npmrc are the box's own, expanded there from the run
// environment the step's env just filled. A file whose whole reference does
// not resolve is not written where its step lists the path under
// `optional_keys`, and is otherwise the run's gap like an env value.
//
// NEW MAPS, never the steps' own: the provider-wide steps are the manager's,
// shared by every launch on this node, and a body resolved into them would be
// a credential cached past the snapshot it came from.
func (e *Engine) runEnvironment(ctx context.Context, c *Company, seat *org.Role,
	gate *org.RoleSandbox, defaults []sandbox.SetupStep) runEnv {

	// THE SEAT'S OWN RESOLUTION, so its GitHub App token is answered here
	// and nowhere else, issued good for [github.RunTokenLife]: a run's
	// environment is fixed when its box starts. See githubtoken.go.
	resolver, token := e.seatResolver(ctx, c, seat, github.RunTokenLife)
	setup := boxSetup(defaults, gate)

	// THE LAYERS, each value with what declared it.
	type layered struct {
		value, path, where string
		optional, fact     bool
	}
	merged := map[string]layered{}
	if handle := seat.Handle(); handle != "" {
		merged["CREWLET_AGENT_HANDLE"] = layered{value: handle, path: "role.handle", fact: true}
	}
	if email := seat.Email; email != "" {
		merged["CREWLET_AGENT_EMAIL"] = layered{value: email, path: "role.email", fact: true}
	}
	for i, step := range setup {
		where := stepOptionalKeys(step.Name, i < len(defaults))
		for key, value := range step.Env {
			merged[key] = layered{value: value, path: step.Name + ".env." + key, where: where,
				optional: slices.Contains(step.OptionalKeys, key)}
		}
	}
	if gate != nil {
		for key, value := range gate.Env {
			merged[key] = layered{value: value, path: "role.sandbox.env." + key,
				where: "role.sandbox.optional_keys", optional: slices.Contains(gate.OptionalKeys, key)}
		}
	}

	var mint *seatMint
	if c != nil {
		mint = &seatMint{kinds: e.mintedFor(c, seat.Handle()), token: token}
	}
	gap := refGap{env: resolver, mint: mint}
	facts := refGap{env: resolver, mint: mint}
	out := runEnv{env: make(map[string]string, len(merged))}
	for _, key := range slices.Sorted(maps.Keys(merged)) {
		l := merged[key]
		missing := resolver.Missing(l.value)
		switch {
		case len(missing) == 0:
			out.env[key] = resolver.Value(l.value)
		case l.fact:
			facts.hold(key, missing, false, "", false)
			out.facted = append(out.facted, l.path)
		case gap.hold(key, missing, l.optional, l.where, false):
			out.keys = append(out.keys, l.path)
		}
	}

	out.setup = make([]sandbox.SetupStep, len(setup))
	for i, step := range setup {
		out.setup[i] = step
		if len(step.Files) == 0 {
			continue
		}
		where := stepOptionalKeys(step.Name, i < len(defaults))
		files := make(map[string]string, len(step.Files))
		for _, path := range slices.Sorted(maps.Keys(step.Files)) {
			body := step.Files[path]
			content, unresolved := secrets.ReadContent(body, resolver.LookupOK)
			if name, whole := envref.Whole(body); whole && content == "" {
				// SET TO NOTHING READS AS NOTHING, as an env value does:
				// an empty credential file is no credential.
				unresolved = name
			}
			if unresolved == "" {
				files[path] = content
				continue
			}
			if gap.hold(path, []string{unresolved}, slices.Contains(step.OptionalKeys, path), where, false) {
				out.files = append(out.files, step.Name+":"+path)
			}
		}
		out.setup[i].Files = files
	}

	out.gap = gap.missing(seat.Handle(), "the run")
	out.facts = facts.missing(seat.Handle(), "the run")
	var held []*credentialMissing
	for _, m := range []*credentialMissing{out.gap, out.facts} {
		if m != nil {
			held = append(held, m)
		}
	}
	states := e.mintingStates(ctx, held)
	out.gap, out.facts = out.gap.judged(states), out.facts.judged(states)
	return out
}

// boxSetup is the setup steps one seat's box is provisioned with: the
// provider-wide ones, then the seat's own. ONE COMPOSITION for the launch and
// for the poll that keeps a running box's issued credentials current
// ([Engine.refreshRunTokens]), so the files one provisions are the files the
// other keeps.
func boxSetup(defaults []sandbox.SetupStep, gate *org.RoleSandbox) []sandbox.SetupStep {
	setup := slices.Clone(defaults)
	if gate != nil {
		setup = append(setup, seatSetupSteps(gate.Setup)...)
	}
	return setup
}

// stepOptionalKeys is where a setup step's optional keys are declared, for a
// sentence: a provider-wide step's or the seat's own.
func stepOptionalKeys(step string, providerWide bool) string {
	if providerWide {
		return fmt.Sprintf("the optional_keys of providers.sandbox.setup step %q", step)
	}
	return fmt.Sprintf("the optional_keys of role.sandbox.setup step %q", step)
}

// logGap writes what a run's environment lacks, by the story a tool server's
// gap is told by: waiting for provisioning is info, and the rest a warning.
// what says what becomes of the run. PATHS, KEYS AND VARIABLES ONLY, never a
// value, which may embed a partial secret, or a file's body, which is a
// credential more often than not.
func (r runEnv) logGap(ctx context.Context, seat, what string) {
	if r.gap == nil {
		return
	}
	attrs := []any{"seat", seat}
	if len(r.keys) > 0 {
		attrs = append(attrs, "keys", r.keys)
	}
	if len(r.files) > 0 {
		attrs = append(attrs, "files", r.files)
	}
	attrs = append(attrs, r.gap.logAttrs()...)
	attrs = append(attrs, "detail", what+": "+r.gap.reason())
	if r.gap.waiting() {
		log.InfoContext(ctx, "sandbox_env_unresolved", attrs...)
		return
	}
	log.WarnContext(ctx, "sandbox_env_unresolved", attrs...)
}

// logFacts writes what of the engine's own facts a run that starts goes
// without, on the line and at the level [runEnv.logGap] writes the gap at,
// so a setup recipe that read an empty commit address has a line saying why.
func (r runEnv) logFacts(ctx context.Context, seat string) {
	if r.facts == nil {
		return
	}
	attrs := append([]any{"seat", seat, "keys", r.facted}, r.facts.logAttrs()...)
	attrs = append(attrs, "detail", "the run starts without the engine's own "+
		plural(r.facted, "fact", "facts")+" from "+joined(r.facted)+": "+r.facts.reason())
	if r.facts.waiting() {
		log.InfoContext(ctx, "sandbox_env_unresolved", attrs...)
		return
	}
	log.WarnContext(ctx, "sandbox_env_unresolved", attrs...)
}

func seconds(v float64) time.Duration { return time.Duration(v * float64(time.Second)) }

// secondsPtr carries an OPTIONAL number of seconds through as a duration,
// keeping nil distinct from zero.
//
// The distinction is the whole point at the two call sites that need it: a
// pause TTL of 0 means "never pause" and an absent one means "take the
// engine default", and every layer that collapsed the two re-applied a
// default over a setting an operator had deliberately chosen.
func secondsPtr(v *float64) *time.Duration {
	if v == nil {
		return nil
	}
	d := seconds(*v)
	return &d
}

// sandboxAccountant post-charges a collected coding run against the shared
// counters.
//
// The charge happens AFTER the spend, which is why it cannot refuse: a refusal
// cannot un-spend a run that already ran, and recording it anyway is the only
// way the meter stays true when the cap is binding. So it goes through
// [coord.Budgets.PostCharge], never the gate. Charged through
// [coord.Budgets.Charge] when a refused charge counted nothing, a run that did
// not fit was recorded not at all, which under-stated the company's spend by
// the whole run at exactly the moment the cap bound. Charge counts what it
// refuses now, but its verdict is still the wrong thing to ask for: nothing
// waits on it — the work the run bought is done — and a refusal would stamp
// the seat's window as the gate turning a round away, and an admission clear
// every stamp, for a charge nobody was deciding about.
//
// The run is counted in the windows of COLLECTION — the day, week and month
// it was collected in, on the company's clock — because that is the only
// instant the counter is told about: a run that started yesterday and ended
// today is today's spend as far as any cap can know. The caps only decide
// whether to SAY the run went over.
//
// Charging it once per launch, however often its completion is retried, is the
// coordinator's side: see [sandbox.PendingRun.Charged]. So is finishing a charge
// that reached the company and not the seat ([coord.SeatUncountedError]): the
// coordinator records the partial on the run, and the retry it hands here is
// told so and records the seat's share alone ([coord.Budgets.PostChargeSeat]),
// rather than counting the company a second time.
type sandboxAccountant struct {
	budgets postCharger

	// basis reads, off the epoch LIVE at charge time rather than the one
	// the turn was pinned to, the ceilings the run is measured against and
	// the clock its windows are cut on: this charge lands after a run that
	// may have taken hours, and the caps the company is running under now
	// are the ones it should be measured against.
	basis func(agentID string) budgetBasis

	now func() time.Time
}

// postCharger is the slice of the fleet's counters the accountant calls.
type postCharger interface {
	PostCharge(ctx context.Context, seat string, tokens int, windows coord.Windows) (coord.Spend, error)
	PostChargeSeat(ctx context.Context, seat string, tokens int, windows coord.Windows) (coord.Usage, error)
}

// Charge post-charges a collected run, and reports whether it took a counter
// past a ceiling. The error is the counter's own, a [coord.SeatUncountedError]
// included, for the coordinator to record.
//
// A run whose company share an earlier attempt already recorded is charged to
// the seat alone, and only the seat's ceilings can then say it went over: the
// company's counter was not read, and reading it to answer a log line would
// be a second round trip for a figure nothing acts on.
func (a sandboxAccountant) Charge(ctx context.Context, agentID, _ string, tokens int, companyCharged bool) (bool, error) {
	if a.budgets == nil || tokens <= 0 {
		return false, nil
	}
	basis := a.basis(agentID)
	scope, windows := coord.AgentScope(agentID), coord.WindowsAt(a.now(), basis.zone)
	if companyCharged {
		seat, err := a.budgets.PostChargeSeat(ctx, scope, tokens, windows)
		if err != nil {
			return false, err
		}
		return overAnyCap(seat, basis.seat), nil
	}
	spend, err := a.budgets.PostCharge(ctx, scope, tokens, windows)
	if err != nil {
		return false, err
	}
	return overAnyCap(spend.Org, basis.org) || overAnyCap(spend.Agent, basis.seat), nil
}

// overAnyCap reports whether a counter has spent past any window's ceiling.
func overAnyCap(u coord.Usage, caps coord.Caps) bool {
	for p, ceiling := range caps {
		if u.In(p).Used > ceiling {
			return true
		}
	}
	return false
}

// resumer re-enters a suspended turn on this node.
//
// It is the coordinator's one seam into the agent layer, and the direction
// matters: the sandbox package holds the state as an OPAQUE blob and this
// decodes it, so the wire format's shape stays in the agent layer that
// understands it.
type resumer struct{ engine *Engine }

var _ sandbox.Resumer = (*resumer)(nil)

// Resume re-enters the suspended turn a completion or an answer names.
//
// NO PANIC LEAVES THIS FRAME, for the reason [Dispatcher.Dispatch] gives on the
// other path. A phase that panics is recovered inside the turn loop; anything
// else that panics while re-entering a turn is recovered here and abandoned.
// Unrecovered, it skipped the coordinator's revert as well as its settle, so
// the run row was stranded in resumed while the queue redelivered a completion
// the claim then refused.
func (r *resumer) Resume(ctx context.Context, req sandbox.ResumeRequest) error {
	return r.engine.guardResume(ctx, req.Run, func() error { return r.resume(ctx, req) })
}

// resume is [resumer.Resume]'s body, separated so the recovery around it is
// one deferred call.
func (r *resumer) resume(ctx context.Context, req sandbox.ResumeRequest) error {
	state, ok, err := execstate.Decode(req.Run.ExecuteState)
	if err != nil {
		// A state this build cannot read is a ROUTING failure, not a run
		// failure: a peer on the version that wrote it can resume this, so
		// the completion must go back rather than be settled here.
		return fmt.Errorf("%w: %w", sandbox.ErrResumeUnavailable, err)
	}
	if !ok {
		return fmt.Errorf("%w: run %s has no suspended conversation",
			sandbox.ErrResumeUnavailable, req.Run.TurnID)
	}
	// ONE EPOCH for the whole resume: the seat checked here, the organization
	// it belongs to, and the company the resumed turn then runs in (see
	// [resumeInput.Company]). A second read of the engine's company is a
	// different epoch once an apply lands in between, and a seat this check
	// found could be gone from it, which fails the run with "not an agent seat"
	// instead of routing the completion to a node that has the seat.
	company := r.engine.Company()
	if company == nil {
		// A node with no applied revision has no seat to resume into, and
		// a peer that has one can. Read through a nil company this was a
		// nil dereference.
		return fmt.Errorf("%w: this node has no applied company to resume run %s into",
			sandbox.ErrResumeUnavailable, req.Run.TurnID)
	}
	seat := company.Org.AgentSeatByHandle(req.Run.AgentHandle)
	if seat == nil {
		// The seat is gone from this epoch — decommissioned, or this node
		// is on a revision that never had it. Either way the resume belongs
		// somewhere else.
		return fmt.Errorf("%w: seat %q is not in this node's company",
			sandbox.ErrResumeUnavailable, req.Run.AgentHandle)
	}
	return r.engine.resumeTurn(ctx, resumeInput{
		Company:       company,
		Run:           req.Run,
		State:         state,
		Turn:          resumedTurn(req.Run, seat, company.Org),
		Answer:        req.Answer,
		Success:       req.Success,
		Trigger:       req.Trigger,
		DeliveredRefs: req.DeliveredRefs,
		RefsElided:    req.DeliveredRefsElided,
		InputTokens:   req.InputTokens,
		OutputTokens:  req.OutputTokens,
		Begin:         req.Begin,
		Engine:        req.Engine,
	})
}

// resumedTurn is the turn a parked run is re-entered as.
//
// THE SAME RUN AND THE SAME UNIT OF WORK the suspended turn had, both read off
// the row: the resume re-enters that run, and its writes stay idempotent
// against the trigger the run was dispatched for, from the instant the first
// half derived its ids with.
//
// THE START, NOT WHERE THE RESUMED HALF MINTS. That is decided afresh by every
// attempt at the resume, against its own clock, when the telemetry that hands
// the runner its turn is assembled ([Engine.describeResume], [rebaseFor]) — a
// resume that comes longer after this start than the operation ledger
// remembers mints its writes at the attempt, or at the instant an earlier
// attempt or an earlier half of the turn recorded, never here.
func resumedTurn(run sandbox.PendingRun, seat *org.Role, organization *org.Organization) *turnctx.Turn {
	return &turnctx.Turn{
		RunID: run.TurnID, WorkKey: run.WorkKey, WorkSince: run.WorkSince,
		Seat: seat, Org: organization,
		Depth: run.DelegationDepth, Chain: run.DelegationChain,
	}
}

// parkedRunContext is the context a parked run's turn runs under on this node:
// the resume that collects it, and a box-mode run taken over while it works
// ([Engine.reopenAgentRun]).
//
// ITS TRACE, continued from the ids on the run's row — the suspended span
// reconstructed as a remote parent, since no span survives a suspend — and
// ITS SEAT, bound for every model call the turn makes ([llm.WithSeat]) exactly
// as the dispatch path binds it for a fresh turn. A coding CLI keeps a home of
// its own per seat — its transcripts, its sign-in — and a call that names no
// seat runs in the one home every unbound call shares: a resumed turn's
// executor, its reviewer and a worker a box delegated to all did, beside each
// other's transcripts and on whichever login that home last held.
func parkedRunContext(ctx context.Context, run sandbox.PendingRun) context.Context {
	ctx = tracing.WithRemote(ctx, events.TraceContext{TraceID: run.TraceID, SpanID: run.SpanID})
	return llm.WithSeat(ctx, run.AgentHandle)
}

// guardResume runs one resume, abandoning it if it panics.
//
// It publishes the unhandled-exception guard itself because the frame that
// would have, the resumed turn's own telemetry, is what did not run: without
// it the seat renders as whatever it was last doing rather than its failure. The run's
// own row names the seat.
func (e *Engine) guardResume(ctx context.Context, run sandbox.PendingRun, resume func() error) (err error) {
	defer func() {
		if panicked := turn.Recovered(recover()); panicked != nil {
			err = e.resumePanicked(ctx, run, panicked)
		}
	}()
	return resume()
}

// resumePanicked is [Engine.guardResume]'s recovery.
func (e *Engine) resumePanicked(ctx context.Context, run sandbox.PendingRun, panicked *turn.PanicError) error {
	log.ErrorContext(ctx, "sandbox_resume_panicked", "turn_id", run.TurnID,
		"seat", run.AgentHandle, "panic", panicked.Value, "stack", panicked.Stack)
	role, agentID := run.Role, run.AgentID
	if live, id := seatIdentity(e.Company(), run.AgentHandle); live != "" {
		role = live
		if id != "" {
			agentID = id
		}
	}
	trace := events.TraceContext{TraceID: run.TraceID, SpanID: run.SpanID}
	if breach := panicBreach(role, agentID, run.TurnID, run.WorkKey, trace, panicked); breach != nil {
		e.observe(ctx, breach)
	}
	return fmt.Errorf("%w (%s): %w", sandbox.ErrResumeAbandoned, turn.AbandonedPanicked, panicked)
}

// resumeInput is one re-entry, assembled.
type resumeInput struct {
	// Company is the epoch the resume was admitted under: the one whose
	// organization holds Turn's seat. The resumed turn runs in it rather than
	// reading the engine's company again, because that read is the NEXT epoch
	// once an apply lands between the two, and the seat the admission found
	// may not be in it.
	Company *Company
	Run     sandbox.PendingRun
	State   execstate.State
	Turn    *turnctx.Turn
	Answer  string
	Success bool
	Trigger *events.Event

	// DeliveredRefs are what the collected run reported, for the resumed
	// phase's own event. Empty when a person's answer resumed a parked
	// clarification: nothing was collected. The run's cost is on its own
	// `sandbox` phase record — see [sandbox.ResumeRequest.DeliveredRefs].
	DeliveredRefs []string

	// RefsElided is how many more the run reported than DeliveredRefs
	// lists — see [sandbox.ResumeRequest.DeliveredRefsElided].
	RefsElided int

	// InputTokens and OutputTokens are the resumed job's tokens, which this
	// segment's charge to the turn's work item includes — see
	// [sandbox.ResumeRequest.InputTokens].
	InputTokens  int
	OutputTokens int

	// Begin is the coordinator's commit, called once the segment is certain
	// to run and before any of it does — see [sandbox.ResumeRequest.Begin].
	// Nil commits nothing.
	Begin func(ctx context.Context) error

	// Engine is what the engine spent on the job between segments — its
	// bridged calls and the condensation of its collection — which this
	// segment's charge includes too: see [sandbox.ResumeRequest.Engine].
	Engine sandbox.EngineSpend
}

// resumeTurn re-enters a suspended turn.
//
// THE SAME TURN, not a new one: the same id, the same trigger, and the ledger
// the suspended turn had. What is rebuilt rather than restored is everything
// LIVE — the budget meter, the phase recorder, the config pin — because those
// belong to the node doing the resuming, not to the one that suspended.
//
// The config pin is the interesting one: this turn pins the epoch live AT
// RESUME, not the one it suspended under. A run can be parked for days waiting
// on a person, and re-entering under a revision that has since been deleted
// would resume a turn into a company that no longer exists. The cost — a turn
// observing a config change across its own suspension — is real and accepted,
// and it is why suspension is a phase boundary rather than a mid-round pause.
func (e *Engine) resumeTurn(ctx context.Context, in resumeInput) error {
	// A SUSPEND IS A RETURN, AND A SPAN CANNOT SURVIVE IT. The suspending
	// phase's span ended when that phase returned; the process may have
	// exited, the seat may have moved node, and days may have passed. So the
	// resume does not continue a span — it RECONSTRUCTS the suspended one as
	// a remote parent from the ids on the run's own row, and opens a new
	// span beneath it. That is the honest shape: two spans in one trace,
	// with the wait between them visible as the gap it actually is. Every
	// run carries ids: the launch takes them from the active span, minting
	// a fresh trace when there is none. And the seat is bound, as the
	// dispatch path binds it — see [parkedRunContext].
	ctx = parkedRunContext(ctx, in.Run)
	ctx, span := tracing.Start(ctx, "engine", "agent.turn.resume",
		attribute.String("crewlet.seat", in.Turn.Handle()),
		attribute.String("crewlet.turn_id", in.Run.TurnID),
		// The unit of work beside the run, so the two halves of a
		// suspended turn answer the same trace query as the dispatch
		// span that started it.
		attribute.String("crewlet.work_key", in.Run.WorkKey))
	defer span.End()

	// THE INDICATOR A RESUMED TURN SHOWS, which comes from one of two places
	// and never from both. Ended below however the resumed turn goes.
	//
	// REJOIN FIRST, because a resume a BOX'S COMPLETION drove has nothing to
	// raise from: the suspended half ended with keepAlive, so the hold is
	// already up under this same turn id and the box's minutes are visible to
	// whoever is waiting — and a parked run's row carries no chat metadata by
	// design, the message that woke the suspended turn being days gone and
	// never this node's to keep, while the conversation keys the row does
	// carry are partition keys rather than addresses in a channel. A fresh
	// raise there would assert a conversation this turn cannot prove it is
	// in, over an indicator that is already up.
	//
	// BEGIN FROM THE TRIGGER WHERE NO HOLD IS LEFT, because that is a resume a
	// PERSON'S ANSWER drove: the park released the hold, and the answer is an
	// ordinary chat message carrying the conversation to raise in. Nil where
	// neither holds — a completion whose session is not on this node, the seat
	// having moved while the box ran or this process having restarted: the
	// indicator that node raised lapses on the backend's own expiry, and
	// nothing here invents a thread for it.
	//
	// See [Engine.resumeWorkingStatus], which owns that order and reports
	// which of the two answered.
	status, rejoined := e.resumeWorkingStatus(ctx, in.Turn.Handle(), in.Run.TurnID, in.Trigger)
	// TRUE UNTIL THE TURN ACTUALLY RUNS, BUT ONLY ON ONE OF THE TWO ROUTES.
	// Every early return below is a RETRY rather than an ending — a reply
	// this build cannot read and a runner that could not be built both leave
	// the coordinator to revert its claim — but the claim reverts to exactly
	// the status it was taken FROM, and the two routes came from opposite
	// facts:
	//
	//   - A BOX'S COMPLETION was claimed from a live run, so the revert puts
	//     it back and the completion comes round again. Nothing else could
	//     re-raise this indicator — the run's row carries no chat metadata,
	//     so a later resume has only the hold to take back — where on the
	//     dispatch path a redelivered trigger simply raises a fresh one. So
	//     it is KEPT.
	//   - A PERSON'S ANSWER was claimed from the answer recorded on the run
	//     — a chat reply or an answer by turn, which are both recorded before
	//     anything is done with them — so the revert puts the run back to
	//     owing that answer's resume. Nothing is working, and an
	//     indicator over that wait tells the one person who could move it
	//     that the run is busy with their reply when it is not — the same
	//     lie the park exists to stop. So it is CLEARED, and this is the
	//     only place that clear happens: the coordinator's revert reports no
	//     stop, deliberately, because the same revert on the completion
	//     route puts a run back to a box that is still working. See
	//     [sandbox.Coordinator.unclaim].
	//
	//     THE ANSWER ITSELF IS NOT LOST. It is recorded on the run before
	//     its resume is attempted, on either route, so the coordinator
	//     retries the resume on its own schedule and each attempt raises its
	//     own indicator off the recorded trigger. It never falls through to
	//     an ordinary turn, which answered the person with a turn rather
	//     than with the coding run they were replying to. See
	//     [sandbox.Coordinator.TryResumeFromAnswer] and
	//     [sandbox.Coordinator.AnswerByTurn].
	working := rejoined
	defer func() { endWorkingStatus(ctx, status, working) }()

	company := in.Company
	if company == nil {
		// Not a second read of the epoch, which is the one thing this
		// must not do (see [resumeInput.Company]). Handed back rather
		// than settled: a caller that assembled a resume without its
		// epoch is a defect here, and a peer can still resume the run.
		return fmt.Errorf("%w: run %s was handed to resumeTurn without the "+
			"company its seat was resolved in", sandbox.ErrResumeUnavailable, in.Run.TurnID)
	}
	input, tel, err := e.parkedTurnRunner(ctx, company, in)
	if err != nil {
		// A RETRY, like every early return here: nothing ran, and the
		// coordinator hands its claim back so the next attempt judges the
		// rebase again against its own clock.
		return err
	}
	resumedReply := input.Reply
	// The note box the runtime decides — see [steerBox]. A RESUMED SEGMENT
	// IS STEERABLE LIKE ANY OTHER: it is the same turn running again, and a
	// note sent while it parked was answered `closed` — the box that segment
	// had is gone.
	box := steerBox(input.AgentRun)
	input.Steer = box
	// The working indicator's phase updates, on the same terms as the
	// dispatch path: the resumed Execute loop opens a phase like any other,
	// and a reader watching the thread should see it move when the box's
	// answer lands.
	input.OnPhase = func(ph phase.Phase) { status.Phase(ph.String()) }
	// NO onboarding on a resume. The pass is a seat's FIRST turn, and this
	// turn already ran its first phase — running it here would spend the
	// resumed turn's opening on orientation for a seat that is mid-task.
	input.Resume = &runner.Resume{
		State: in.State, Answer: in.Answer,
		// WHICH BOX DID THE WORK. Off the run's own row, because this
		// process may not be the one that launched it — and it is the only
		// thing that can say a phase ran in a sandbox rather than in this
		// process's own tool loop.
		Run: runner.RunRecord{
			CodingAgent:   in.Run.CodingAgent,
			SandboxID:     in.Run.SandboxID,
			LaunchID:      in.Run.LaunchID,
			DeliveredRefs: in.DeliveredRefs,
			RefsElided:    in.RefsElided,
		},
		// THE RUN'S OWN TOOL CALLS, off its durable row. A box-mode
		// executor called them over the bridge, possibly in another
		// process, so this list is the only record of what the phase did —
		// its submission included.
		Bridged: bridgedCalls(in.Run.BridgeCalls),
	}
	r, err := company.RunnerFor(in.Turn.Handle(), e.seatRegistry(company, in.Turn.Handle()), input)
	if err != nil {
		return err
	}
	// THE SEGMENT IS NOW CERTAIN TO RUN, and the coordinator records that
	// before any of it does — for a person's recorded answer, that THIS TURN
	// TOOK IT, and that the delivery which carried it is worked
	// ([sandbox.ResumeRequest.Begin]). The row is all the seat's next holder
	// can read if this process stops from here on, and it is what tells a
	// reply a turn has used from one nobody ever got to, which goes back to
	// the seat; the completion ledger is what drops the delivery if it comes
	// round to that holder unacknowledged. A commit that cannot be made is a
	// RETRY like every return above: nothing has run, and a turn run without
	// it would be read after a crash as one that never got the answer — and
	// the reply handed back to the seat a second time.
	if in.Begin != nil {
		if beginErr := in.Begin(ctx); beginErr != nil {
			return beginErr
		}
	}
	// THE SEGMENT IS ON THE RECORD ONCE IT IS CERTAIN TO RUN, and not a
	// line earlier — which is where the dispatch path's start differs, and
	// has to. Every return above is a RETRY OF THIS SAME RUN: the
	// coordinator reverts its claim and the resume comes round again under
	// the same turn id. Announced before them, a runner that could not be
	// built would leave a start nothing closes, and closing it with a
	// completion would record as ended a run that is still parked. From
	// here every path publishes the completion below, so each start this
	// frame makes is paired. A resume gathers no context of its own, so
	// nothing slow is being skipped by waiting.
	e.publishTurnStarted(ctx, tel, in.Run.DelegationDepth, in.Run.DelegationChain, true)
	// Open once the segment is certain to run, for the reason its start is
	// announced only now; closed the moment it returns, as on the dispatch
	// path, with the defer as the backstop.
	closeSteer := e.openSteer(in.Run.TurnID, in.Turn.Handle(), box, r)
	defer closeSteer(ctx)
	// NO WALL-CLOCK CAP ON A RESUME. The cap bounds the turn a fire started;
	// a detached sandbox run can legitimately outlive it, and the resumed
	// half is finishing work the box already did rather than starting more.
	res, err := turn.Run(ctx, r, company.TurnSettings(0),
		resumeInputFor(in, resumedReply))
	closeSteer(ctx)
	// THE TURN RAN, so from here the indicator follows what it concluded
	// rather than the retry rule above: a resumed turn that suspended AGAIN
	// keeps it, because the same box is still working.
	working = res.Suspended
	// THE SEGMENT'S CHARGE, decided once for the suspension below and the
	// charge after the completion — see turnspend.go.
	spend := r.Spend()
	charge := tel.chargeFor(spend, res, err, time.Now().UTC())
	e.endSegment(ctx, tel, spend, res, err, charge)
	if err != nil {
		// A PERSON STOPPED THE RESUMED TURN — its seat was paused with a
		// stop while the run was out, and the turn ended at its first
		// round back. It is settled rather than reverted, for the reason
		// the dispatcher spends a stopped trigger: a revert brings the
		// completion straight back, and the resume would be stopped again
		// on every delivery until the broker dead-lettered it. The run's
		// box is reclaimed with it; stopping the turn is what was asked.
		if turn.Stopped(err) {
			pause, _ := stopOf(err)
			role, agentID := seatIdentity(company, in.Turn.Handle())
			e.observe(ctx, turnStoppedEvent(in.Turn.Handle(), role, agentID,
				in.Run.TurnID, in.Run.WorkKey, pause, tracing.TraceOf(ctx)))
			return fmt.Errorf("%w (a person stopped the resumed turn): %w",
				sandbox.ErrResumeAbandoned, err)
		}
		if reason, abandon := turn.Abandon(res, err); abandon {
			// The same decision the dispatcher makes on the other path
			// (see (*Dispatcher).abandon), from the same rule, taken here
			// in this subsystem's own vocabulary: the coordinator reads
			// the sentinel and leaves the claim taken, so the completion
			// is not redelivered into a conversation a retry must not
			// re-enter.
			//
			// A resumed turn is the one most likely to qualify. It
			// re-enters the executor's suspended loop with the whole
			// pre-suspend conversation, and the round that called
			// run_sandbox was never closed, so its writes are in no
			// ledger and a replay would repeat every one of them.
			//
			// The indicator comes down with it: the run is settled,
			// the box reclaimed and the record deleted, so nothing is
			// coming back for this turn.
			return fmt.Errorf("%w (%s): %w", sandbox.ErrResumeAbandoned, reason, err)
		}
		// Reverted, so the turn is not over — and BOTH KINDS OF RESUME COME
		// BACK rather than wait for a further signal.
		//
		// A BOX'S COMPLETION is NAK'd by the seat's control-topic handler
		// and redelivered on the broker's own backoff, to this node once
		// it recovers or to the seat's next owner, until its delivery
		// budget is spent.
		//
		// A PERSON'S ANSWER is recorded on the run — a chat reply and an
		// answer by turn alike — so the revert leaves it owed its resume
		// and the coordinator retries that on its own schedule, bounded by
		// [sandbox.MaxAnswerAttempts] and the run's own
		// pause_ttl_seconds, and lets the answer go past them: a chat
		// reply to the seat's ordinary route, an answer by turn spent as
		// declined. The delivery that carried it is never handed back for
		// this; it was spent when the answer was recorded.
		//
		// ON THE ROUTE'S OWN TERMS, exactly as the seed above: this is the
		// same retry rule reached one step later, over the same revert.
		working = rejoined
		// UNLESS WHAT BROKE IT IS A WAIT: every model at a usage limit, or
		// a capped window spent, before this attempt wrote anything. The
		// turn gate's own conditions ([Engine.seatCannotRun]), found a
		// round later, and handed back as its refusal is: a completion is
		// acknowledged and kept until the condition clears, and an answer
		// waits for it uncharged. As a failure, a completion was NAKed
		// into the same limit until its delivery budget ran out, and an
		// answer spent its attempts on a wait.
		if refusal, refused := e.resumeRefusal(ctx, in.Turn.Handle(), err); refused {
			return &sandbox.TurnRefused{Cause: err, Refusal: refusal}
		}
		return err
	}
	// A resumed turn that suspended AGAIN persists its new conversation the
	// same way the first one did — the coordinator sees the row back in
	// running and leaves the box for the next completion — and keeps its
	// indicator on the same terms, off the ROW rather than off the intent.
	if res.Suspended {
		working = stillWorking(e.persistSuspension(ctx, r, in.Run.TurnID, tel.written, charge, tel.ask, tel.senders))
	}
	e.recordResume(ctx, in, res)
	return nil
}

// parkedTurnRunner is the runner a parked run's turn is rebuilt with on this
// node, and the telemetry it was built from: the resume that collects the run
// ([Engine.resumeTurn]) adds what re-entering the turn takes, and a box-mode
// run taken over while it works ([Engine.reopenAgentRun]) uses it as it is.
//
// ONE CONSTRUCTION for both, because a call a box makes over its bridge after
// a recovery and a call the resumed turn makes are calls of the same turn, and
// the two must agree on everything a call reads: the turn's identity and unit
// of work, the rebase its operation ids are minted under, the counter its
// spend is held to, the seat's registry, skills and fence, and the reply it
// owes.
//
// The results are named so a failure answers their zero values without
// building a RunnerInput: every literal of one is held to carrying the seat's
// fence (internal/seat's TestEveryConfigThatCanCarryTheSeatFenceDoes).
func (e *Engine) parkedTurnRunner(ctx context.Context, company *Company, in resumeInput,
) (input RunnerInput, tel turnTelemetry, err error) {
	resumedReply, err := resumeReply(in.Run)
	if err != nil {
		return input, tel, err
	}
	tel, err = e.describeResume(ctx, company, in)
	if err != nil {
		return input, tel, err
	}
	// THE COUNTER AS THE RUN LEFT IT, read once before anything is sent:
	// the run was post-charged a moment ago, and a window it took past its
	// ceiling refuses every call this segment would make. Before the turn
	// identity, which carries it to every auxiliary call the segment makes
	// ([turnTelemetry.budget]).
	tel.budget = e.resumeMeterFor(ctx, company, in.Turn.Handle())
	turnIdentity := tel.runnerTurn(company, in.Run.DelegationDepth,
		in.Run.DelegationChain, resumeTask(in), resumedReply, e.WithheldContacts())
	return RunnerInput{
		Task:    resumeTask(in),
		Compact: e.compactorFor(company),
		// THE RUNNER NEEDS IT TOO, not just the loop. This field reaches
		// runner.Config.Reply, which is what submit_work's own citation
		// check reads — so a resumed turn with this unset accepted
		// `no_action` on a turn somebody was waiting on, and accepted a
		// citation naming any surface at all, before the loop's later
		// checks ever ran.
		Reply:     resumedReply,
		Publisher: e.backends.Queue,
		Turn:      turnIdentity,
		Sessions:  e.sessions,
		Metrics:   e.metrics,
		// THE SAME RUNTIME THE TURN SUSPENDED UNDER — supplied here for the
		// turn's LATER rounds, not for the one being re-entered: whether
		// that phase was agentic is the state's own answer (see
		// [execstate.State.AgentRun]), but a turn that loops to another
		// iteration must run that one the same way it ran the first.
		AgentRun: e.agentRunFor(company, in.Turn.Handle(), turnIdentity.Context),
		Budget:   tel.budget.budget(),
		// A resumed Execute loop can exhaust its rounds like any other, and
		// it is the phase most likely to: it comes back mid-task with its
		// budget already partly spent.
		Judge:     e.judgeFor(company, in.Turn.Handle(), tel.aux()),
		Remaining: e.remainingFor(company, in.Turn.Handle()),
		// THE SAME FENCE THE DISPATCH PATH GETS, and a parked run needs it
		// more than a fresh turn does: its turn was parked across a coding
		// run that may have taken an hour, and the node that picks it up is
		// not always the node that parked it. The grant it closes on is the
		// one THIS node holds now — which is the correct anchor, since that
		// is what this node is admitted for.
		Fence: e.seatFence(in.Turn.Handle()),
		// THE SKILL REGISTRY, which the resume's call site once omitted.
		// With nil Skills the runner's guardFor returns nil, so the
		// load-before-use gate was disarmed for every resumed turn: a seat
		// could call a tool whose required skill it had never loaded, on the
		// one path where nobody would notice — and the RunnerFor sites must
		// agree or a turn changes shape by being resumed.
		Skills: e.skills,
	}, tel, nil
}

// recordResume files a finished resumed turn against the conversation it was
// serving when it detached.
//
// ONLY THE DISPATCHER WROTE THIS, so a turn that ended here — which is every
// turn that did code work — left the conversation ledger untouched. The
// thread's history then stopped at the moment the run detached, and the
// seat's next turn on it read a conversation in which the coding work had
// never happened: no record of what was built, and nothing to stop it being
// planned again.
//
// A turn that suspended again records nothing — [Dispatcher.RecordSession]
// declines it, for both paths and for the same reason: it has not finished,
// so there is no reply to file. The completion that eventually lands comes
// back through this same frame and records then.
func (e *Engine) recordResume(ctx context.Context, in resumeInput, res turn.Result) {
	// THE RUN THE ROW NAMES, and the work key it rode in on: a resume
	// re-enters the run that suspended, so the entry names that one rather
	// than a fresh id, and it dedupes against the same trigger the dispatch
	// that launched it did.
	//
	// AND UNDER THE CONVERSATION IT REPORTS BACK TO, which is the
	// conversation its answer was matched on and never the partition beside
	// it: for a direct message those are different values, and filing here
	// under the batch would put the coding work in a row the seat's next
	// turn on that DM never looks up — the same silence this frame exists
	// to end.
	e.dispatch.RecordSession(ctx, in.Turn.Handle(), in.Run.ConversationKey,
		in.Run.TurnID, in.Run.WorkKey, resumeTask(in), res, e.dispatch.now())
}

// resumeTask is the brief the resumed turn re-enters with.
//
// Taken from the STATE rather than rebuilt from the trigger: the trigger may
// no longer be readable days later, and the task the turn was working on is a
// fact the suspension already captured.
func resumeTask(in resumeInput) string {
	if in.State.Task != "" {
		return in.State.Task
	}
	return in.Run.TaskDescription
}

// resumeInputFor renders a parked run coming back as the loop's own
// [turn.Input].
//
// A NAMED MAPPING for the reason [turnInputFor] is one on the dispatch path,
// and this literal proved it twice over: every field here is one the resumed
// loop cannot derive for itself, and each degrades SILENTLY without. A missing
// History forgets the rounds that closed before the box and re-fires their
// deliveries; a missing Reply skips every delivery gate; a missing Round hands
// the turn a whole fresh `max_iterations` and re-numbers rounds the suspended
// half had already published phase records under. Three of the five come off
// the parked row rather than off anything in this process, which is precisely
// why nothing else can notice one going missing.
func resumeInputFor(in resumeInput, reply turn.Reply) turn.Input {
	return turn.Input{
		RunID:   in.Run.TurnID,
		Depth:   in.Run.DelegationDepth,
		Reply:   reply,
		History: in.State.Iterations,
		Resume:  true,
		Round:   in.State.Round,
	}
}

// resumeReply is the delivery obligation a parked run comes back with.
//
// OFF THE ROW, and it cannot come from anywhere else. The resumed turn never
// sees the trigger that raised the obligation, and the event that carries the
// completion here is the run FINISHING rather than the ask, so [ReplyFor] over
// it would answer "nobody is waiting" for every turn somebody is waiting on.
//
// A value this build does not recognise is refused rather than defaulted: it
// was written by a build that knows a kind this one does not, and guessing at
// who is waiting is the half of the delivery question this engine exists to get
// right. The refusal is a ROUTING failure, like a state this build cannot
// decode, so the completion goes back for a peer that can read it. An absent
// value is no kind at all — every launch writes one, `none` included — and is
// refused the same way.
func resumeReply(run sandbox.PendingRun) (turn.Reply, error) {
	reply := turn.ParseReply(run.Reply)
	if !reply.Valid() {
		return turn.Reply{}, fmt.Errorf(
			"%w: run %s carries reply %q, which is not one of %q, %q or %q",
			sandbox.ErrResumeUnavailable, run.TurnID, run.Reply,
			turn.ReplyNone, turn.ReplyTool, turn.ReplyEngine)
	}
	return reply, nil
}

// persistSuspension writes a turn's suspended conversation to its row, which
// is also what OPENS the run to the completion poll, and reports whether the
// turn is actually coming back.
//
// Called the moment the turn returns Suspended, because the runner holds the
// conversation only until its frame unwinds. Until this lands the run sits in
// [sandbox.StatusLaunching] and nothing polls or claims it — the job is
// already executing, and a job that finishes first would otherwise be
// collected against a row with nothing to resume into.
//
// EVERY WAY THIS CAN FAIL FAILS THE RUN rather than dropping the suspension: a
// row with no state is one nothing can resume, and failing here, while the
// box is still in the engine's hands and the seat's owner is still this
// process, is far better than leaving a launching row to hold a box until its
// seat happens to move.
//
// # Why it answers, and why the answer is three-valued
//
// A caller that has to know whether anything is still working cannot read that
// off [turn.Result] — every path below leaves it Suspended while settling the
// run, so the turn's own intent says "coming back" on exactly the endings
// where nothing is. The working indicator is that caller: driven off the
// intent it stayed up for the life of the process on every failure here.
//
// The three answers are the ordinary three, so the middle one is not collapsed
// into the loss: (true, nil) the row is open and the poll will resume it;
// (false, nil) the run is settled and definitively will not; and a non-nil
// error for the one case nothing can establish — a write that failed MAY have
// landed, and [sandbox.Coordinator.FailRun] settles only a run still
// launching, so a row this reports as unwritten can be one the completion poll
// resumes. The error is for deciding, not for logging: every failure path here
// has already said what it did and why (see [Engine.failSuspension]), and no
// caller fails a turn over it — the run is settled either way.
//
// WHAT THE TURN HAS WRITTEN SO FAR rides the suspension too (see
// execstate.State.Written): the segment that finishes the turn may be charged
// by a sole write, and it can only judge "exactly one" over the whole turn if
// the half before the park travels with the conversation. So does what the
// segments so far spent and charged to nothing (execstate.State.Uncharged),
// for that same finishing segment to pay — see turnspend.go — and what the
// turn has measured of itself so far (execstate.State.SoFar), for the record
// that ends it to state the whole turn. And so does what
// the turn was ASKED (execstate.State.Ask), which the finishing segment's
// completed-turn event carries and is woken by something else entirely — and
// WHO asked it (execstate.State.Senders), which a refresh_memory call in any
// later segment tells the memory filter, since no later segment has the
// interactions to read them off.
func (e *Engine) persistSuspension(ctx context.Context, r *runner.Runner, turnID string,
	written *turnctx.Written, charge segmentCharge, ask string,
	senders []types.CanonicalIdentity,
) (bool, error) {
	rt := e.sandbox.Load()
	if rt == nil {
		// No store and no coordinator: nothing recorded the run, nothing
		// polls it, and nothing will ever resume this turn.
		return false, nil
	}
	suspension, ok := r.Suspended()
	if !ok {
		e.failSuspension(ctx, rt, turnID, "sandbox_suspension_missing",
			"the turn suspended but recorded no conversation", nil)
		return false, nil
	}
	suspension.State.Written, suspension.State.WrittenMany = written.Items()
	suspension.State.Uncharged = charge.carry
	suspension.State.SoFar = charge.soFar
	suspension.State.Ask = ask
	suspension.State.Senders = senders
	blob, err := execstate.Encode(suspension.State)
	if err != nil {
		e.failSuspension(ctx, rt, turnID, "sandbox_suspension_unserializable",
			"the suspended conversation could not be serialized", err)
		return false, nil
	}
	// The iteration rides BESIDE the blob: the coordinator files the run's
	// own phase record under it and never decodes the conversation.
	suspended, err := rt.pending.MarkSuspended(ctx, turnID, sandbox.Suspension{
		State: blob, Iteration: suspension.State.Round,
	})
	if err != nil {
		e.failSuspension(ctx, rt, turnID, "sandbox_suspension_unwritable",
			"the suspended conversation could not be written", err)
		// UNKNOWN, not lost: the write may have landed, and the settle
		// that follows it declines a row that is no longer launching.
		return false, fmt.Errorf("engine: recording the suspension of turn %s: %w", turnID, err)
	}
	if !suspended {
		// The row is not launching, so this suspension has nowhere to go:
		// either the launch never recorded it, or its tail has already
		// been claimed and settled by somebody else. Overwriting either
		// one is worse than failing.
		e.failSuspension(ctx, rt, turnID, "sandbox_suspension_not_launching",
			"the run was no longer launching when its conversation was written", nil)
		return false, nil
	}
	return true, nil
}

// failSuspension settles a run whose suspension has nowhere to go, and says
// why, in the one voice all four failure paths share.
//
// SETTLED, NOT MARKED. The job is already executing in its box, and writing a
// failed status onto the record stranded that box: a record that is not
// active is read by no recovery pass and polled by no waiter, so the box ran
// to its provider's TTL, billed, with nothing left to reclaim it. The
// coordinator settles it like every other lost turn, while this node still
// owns the seat, and the loss is announced rather than left as silence. It
// settles only a run still launching (see [sandbox.Coordinator.FailRun]): a
// write reported as failed can have landed, and a run it moved to running is
// one the completion poll resumes.
func (e *Engine) failSuspension(ctx context.Context, rt *sandboxRuntime, turnID, event, detail string,
	cause error) {
	args := []any{"turn_id", turnID,
		"detail", detail + "; a run still launching cannot be resumed, so its box is reclaimed and the run ended"}
	if cause != nil {
		args = append(args, "error", cause)
	}
	log.ErrorContext(ctx, event, args...)
	if err := rt.coordinator.FailRun(ctx, turnID,
		types.SandboxFailureSuspensionUnrecorded, detail); err != nil {
		log.WarnContext(ctx, "sandbox_suspension_settle_failed", "turn_id", turnID, "error", err,
			"detail", "the run's record could not be read, so its box was not reclaimed; the "+
				"seat's next recovery pass reaps a run left launching")
	}
}

// launcher is the run_sandbox tool's seam into the sandbox layer.
//
// It exists to assemble a launch from things only the engine holds — the
// seat's config block, the run environment, the box spec — so the tool itself
// stays a thin adapter that reads a brief and reports a suspension.
type launcher struct{ engine *Engine }

var _ builtin.SandboxLauncher = (*launcher)(nil)

func (l *launcher) Launch(ctx context.Context, t *turnctx.Turn, brief string) (sandbox.LaunchResult, error) {
	e := l.engine
	rt := e.sandbox.Load()
	if rt == nil {
		return sandbox.LaunchResult{}, fmt.Errorf("this engine has no sandbox backend configured")
	}
	manager, pending := rt.coordinator.Manager(), rt.pending
	seat, err := t.RequireSeat()
	if err != nil {
		return sandbox.LaunchResult{}, err
	}
	company := e.Company()
	gate := seatSandbox(company, seat.Handle())
	if gate == nil || !gate.Enabled {
		// Belt and braces: the tool is only on a sandbox-enabled seat's
		// surface, but the surface is built from a snapshot and a seat can
		// lose its gate across an apply mid-turn.
		return sandbox.LaunchResult{}, fmt.Errorf("this seat's sandbox is not enabled")
	}
	if config.Placement(gate.RunIn) == config.PlacementSelf {
		// `self` means this seat's code work rides its own executor run,
		// which is a coding CLI in box mode and already holds a shell,
		// an editor and a checkout. A second box beside it would give the
		// seat two filesystems with the work in the one the turn cannot
		// see — so the tool refuses and says which shell to use, rather
		// than provisioning a box whose output is invisible.
		return sandbox.LaunchResult{}, fmt.Errorf(
			"this seat runs its code work in its own agent session (role.sandbox.run_in: " +
				"self) — use the shell and editor you already have rather than " +
				"starting a second box")
	}

	// THE PRE-FLIGHT BUDGET FLOOR. turn_engine.sandbox_min_budget_tokens
	// was validated, schema'd and documented and read by nothing, so a
	// company that set it got a new revision and no behaviour.
	//
	// It is checked HERE rather than in the tool, because this is the frame
	// that holds the seat's counter — and refused as a launch error, which
	// the tool reports back to the model as a failed call. That is the
	// point of a floor: a coding run costs a box, a clone and a toolchain
	// install before it produces a token, so a seat with no headroom must
	// learn that now and fall back to its own tools rather than after the
	// job has died mid-run having delivered nothing.
	if err := sandboxHeadroom(ctx, e.remainingFor(company, seat.Handle()),
		company.Config.TurnEngine.SandboxMinBudgetTokens); err != nil {
		return sandbox.LaunchResult{}, err
	}

	// A box this turn already has, paused from an earlier call. An EMPTY id
	// on an existing row means that box is gone — reaped past its pause TTL,
	// or torn down under a zero TTL — so this call provisions a fresh one
	// and the work re-seeds from the pushed branch.
	reuse := ""
	if existing, found, err := pending.Get(ctx, t.RunID); err == nil && found {
		reuse = existing.SandboxID
	}

	// A RUN WHOSE ENVIRONMENT DOES NOT RESOLVE IS NOT STARTED, by the rule a
	// tool server is held to (toolcredentials.go): handed an empty code-host
	// token it clones nothing and pushes nothing, after spending its box. The
	// refusal is this call's result, classed unavailable by the tool, so it
	// costs the turn one round rather than the turn — a model told a token is
	// being minted can try later or do the work with its own tools.
	run := e.runEnvironment(ctx, company, seat, gate, manager.DefaultSetup())
	if run.gap != nil {
		run.logGap(ctx, seat.Handle(), "no box is started")
		return sandbox.LaunchResult{}, errors.New(run.gap.runRefusal())
	}
	run.logFacts(ctx, seat.Handle())
	setup := run.setup
	servers := l.engine.boxServers(ctx, company, seat, gate)
	// The seat's own model and login, resolved from llm_sandbox — which
	// falls back to `llm`, because sandboxed work IS this seat's own work
	// running somewhere else, and `llm` is what that work runs on — for the
	// runner that will read them, resolved here as BuildSpec resolves it, so
	// a pairing that runner cannot drive is refused before a box exists.
	runner := gate.CodingAgent
	if runner == "" {
		runner = manager.DefaultCodingAgent()
	}
	coding, refused := newCodingRun(company, seat, phase.Sandbox, runner, run.env)
	if refused != nil {
		return sandbox.LaunchResult{}, refused
	}
	model, env := coding.model, coding.env
	spec := manager.BuildSpec(sandbox.SpecInput{
		// The seat's cell, empty inheriting providers.sandbox's default.
		// Resolved at LAUNCH rather than at config time, for the same
		// reason coding_agent is: a catalogue change reaches every seat
		// that named nothing without rewriting their blocks.
		Placement:       sandbox.Placement(gate.RunIn),
		CodingAgent:     runner,
		PauseTTL:        pauseTTL(gate),
		MaxTurns:        gate.MaxTurns,
		Env:             env,
		CredentialFiles: coding.files,
	})
	if err := sandboxCredentials(company, seat, phase.Sandbox, spec.Placement, env); err != nil {
		return sandbox.LaunchResult{}, err
	}

	return rt.coordinator.Launch(ctx, manager, sandbox.LaunchRequest{
		Turn:  sandboxTurnRef(ctx, t, seat.Name),
		Brief: brief,
		Wider: t.Task,
		Task:  t.Task,
		// HOW TO ASK A PERSON, with the names the chart gives this seat —
		// without it the coding agent was never told the ask shim its box
		// carries exists, so no run could park on a question at all.
		Ask:        askBrief(company.Org, seat),
		Setup:      setup,
		Spec:       spec,
		Model:      model,
		MCPServers: servers,
		ReuseBox:   reuse,
	})
}

// launchFence is the seat lease this node writes on a seat's behalf under —
// a launch, the record of an answer, the claim of a completion or an answer —
// which the run's row is stamped with and every later write the node makes on
// it carries; see [sandbox.CoordinatorOptions.Lease]. The seat's next holder
// fences the row to its own, newer lease, and from then on this node's writes
// are refused rather than landing under it: a release that revives a claim the
// holder has already reaped, or a resumed turn taking an answer the holder has
// given back to the run or is handing back to the seat.
//
// The lease's OWN owner, the incarnation [seat.Host.Owner] names, which is the
// owner the seat's acquisition recovers its runs under — so a node fencing
// its own rows after a restart within one lease writes the same token.
//
// THREE ANSWERS. The seat's lease, held. The zero fence, held — an unfenced
// write — on a node with no seat host, which has no next holder to be fenced
// out by. And NOT HELD on a node whose seat host does not hold the seat, which
// the coordinator refuses every write for ([sandbox.ErrSeatNotHeld]). That last
// used to be the zero fence too, and the zero fence constrains nothing: a node
// that had noticed it lost the seat recorded, claimed and took a person's answer
// past the fence its successor had put on the run, and spent it on a turn the
// seat's lease no longer stood behind. A node that has NOT noticed answers its
// old lease, which the successor's fence outranks in the store.
func (e *Engine) launchFence(handle string) (sandbox.Fence, bool) {
	if e.node == nil {
		return sandbox.Fence{}, true
	}
	host := e.node.Host()
	epoch, held := host.EpochFor(handle)
	if !held {
		return sandbox.Fence{}, false
	}
	return sandbox.Fence{Owner: host.Owner(), Epoch: epoch}, true
}

// sandboxTurnRef is what a detached run's durable row records about the turn
// that launched it.
//
// ONE BUILDER FOR BOTH LAUNCH PATHS — the run_sandbox tool and a cli-agent
// executor handed its whole turn — because it is one row, the same facts and
// the same reasons: the resume happens in another process, days later, and can
// recover none of this from a trigger that is long gone. Written out twice it
// drifted inside a single commit: one copy put the partition in both
// conversation fields, which compiles, because both are strings.
//
// EVERY FACT OFF THE TURN'S OWN EPOCH, never the engine's current company. An
// apply landing mid-turn is the next epoch, and a company renamed there
// derives a DIFFERENT agent id for the same seat — so a row taking its id from
// the live company named an agent none of that turn's other events did, and
// its announcements split the seat in two on every surface that groups by id.
// One path already took the id from the turn and said why; the other did not,
// which is the whole hazard of writing the rule twice.
//
// The ROLE NAME is the caller's, because each already holds the seat it
// resolved and the turn's copy is the same pointer — nothing is gained by
// deriving it a second time here.
//
// The TRACE comes from the ACTIVE span, so the box's spans nest under the call
// that started them rather than appearing as unrelated work minutes later.
// TurnRef carried these two since the OTLP receiver was written and the
// run_sandbox path left both empty: PendingRun.TraceID was always "", RunEnv
// minted no telemetry token for any launch (it refuses to scope one to an
// empty trace, which is the one property that scoping has), and describeResume
// filed every resumed turn under no trace at all.
func sandboxTurnRef(ctx context.Context, t *turnctx.Turn, role string) sandbox.TurnRef {
	runTrace := tracing.TraceOf(ctx)
	return sandbox.TurnRef{
		// The id is derived from the turn's PINNED organization, like every
		// other fact about the seat here. The engine's current company is
		// the next epoch once an apply lands mid-turn, and a renamed
		// company derives a different id for the same seat.
		TurnID: t.RunID, WorkKey: t.WorkKey, WorkSince: t.WorkSince,
		AgentID: t.AgentID(), AgentHandle: t.Handle(), Role: role,
		Depth: t.Depth, Chain: t.Chain,
		TraceID: runTrace.TraceID, SpanID: runTrace.SpanID,
		// BOTH CONVERSATION VALUES, field for field with [turnctx.Turn] so
		// a swapped pair reads as one. The row has carried a conversation
		// since it was written and nothing set it: with it empty a resumed
		// turn had no way to say where its answer belonged, so it left no
		// conversation entry at all and the next turn on that thread read
		// history that stopped when the run detached.
		//
		// The conversation is where the resume reports and what admits a
		// person's answer; the partition states the batch this run was
		// launched from, which tells two runs parked on one direct message
		// apart ([sandbox.ConversationRef.Best]).
		PartitionKey:    t.PartitionKey,
		ConversationKey: t.ConversationKey,
		// The delivery obligation, so the resumed turn knows whether
		// anybody is waiting: it sees neither its trigger nor this frame,
		// and the row is the only place this can reach it from.
		Reply: t.Reply,
		// AND THE ITEM, for the same reason: the resumed turn is charged
		// to what this one was on, and has no trigger to resolve it from.
		WorkItem: t.WorkItem,
		// AND WHO ASKED, which the park resolves a question addressed to
		// "the requester" against — see [sandbox.PendingRun.Requester].
		Requester: t.Requester,
	}
}

// sandboxHeadroom refuses a launch below turn_engine.sandbox_min_budget_tokens.
//
// THREE-VALUED, like every other budget read in this engine: a seat with no
// counter is uncapped and passes, a seat below the floor is refused, and a
// read that FAILED is refused too — launching a box on an unknown budget is
// how a company discovers its ceiling by spending past it.
func sandboxHeadroom(ctx context.Context, remaining runner.Remaining, floor int) error {
	if floor <= 0 {
		return nil
	}
	if remaining == nil {
		// No counter anywhere in the epoch, which is a company with no
		// token budget rather than a read that could not be made.
		return nil
	}
	left, err := remaining.Remaining(ctx)
	if err != nil {
		return fmt.Errorf("this seat's remaining token budget could not be read, "+
			"and a coding run costs a box before it produces anything: %w", err)
	}
	if left < floor {
		return fmt.Errorf("this seat has %d tokens left and a coding run needs at "+
			"least %d (turn_engine.sandbox_min_budget_tokens): do the work with "+
			"your own tools, or report that the budget is exhausted", left, floor)
	}
	return nil
}

// sandboxMCP renders the seat's SCOPED coding-agent MCP surface.
//
// Only the servers role.sandbox.mcp.servers names — never the seat's whole MCP
// surface by default. A coding agent inside a box reaches whatever it is given
// with no per-tool control left, so what it is given is the decision, and it
// is made here rather than inherited.
//
// The credentials are the seat's OWN, inherited down the org chart at build
// time, so a seat gets the tokens it is entitled to and no others.
//
// RESOLVED HERE, like the run env and for the same reason: an in-box server
// has to authenticate, and Tier B stores its references verbatim. Only the
// scoped servers': a block the box is not given is nothing this run resolves
// or reports. And resolved by the rule every start of a server passes
// ([resolveServer]), from the same values the engine's own child of the
// server is started with: the server's own env, headers and url as well as the
// seat's block. They were once handed over verbatim, `${VAR}` and all, and
// written into the CLI's config that way — OpenCode reads `{env:VAR}` and not
// `${VAR}`, and Claude Code expands one from the run env, which holds none of
// the company's secrets.
//
// SO A SERVER WITH A VALUE THAT DOES NOT RESOLVE IS NOT OFFERED: an in-box
// server handed an empty token refuses to start or acts as nobody, and the
// agent would spend a round finding that out — the reason [sandbox.RenderMCP]
// skips a server it cannot render. It used to be offered with a warning that
// it "will not authenticate". What was withheld comes back beside the
// servers, for the caller to judge and say ([Engine.boxServers]); env is the
// seat's own resolution ([Engine.seatResolver]) and mint what is minted or
// issued for the seat ([seatMint]).
func sandboxMCP(
	env *config.Resolver, c *Company, seat *org.Role, gate *org.RoleSandbox,
	mint *seatMint,
) (map[string]sandbox.MCPServer, []*credentialMissing) {
	if len(gate.MCP.Servers) == 0 {
		return nil, nil
	}
	configured := make(map[string]*config.MCPServer, len(c.Config.MCPServers))
	for i := range c.Config.MCPServers {
		configured[c.Config.MCPServers[i].Name] = &c.Config.MCPServers[i]
	}
	var (
		servers  []sandbox.MCPServer
		scoped   = make([]string, 0, len(gate.MCP.Servers))
		withheld []*credentialMissing
		seen     = make(map[string]struct{}, len(gate.MCP.Servers))
	)
	for _, name := range gate.MCP.Servers {
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		server, known := configured[name]
		if !known {
			// LEFT IN THE SCOPE, so the renderer says what it says of a
			// name the company declares no server under.
			scoped = append(scoped, name)
			continue
		}
		values, missing := resolveServer(server, seat.Handle(), "", seat.MCPEnv[name], env, mint)
		if missing != nil {
			withheld = append(withheld, missing)
			continue
		}
		box := sandbox.MCPServer{
			Name: server.Name, Transport: sandbox.Transport(server.Kind()),
			Command: server.Command, Args: server.Args, URL: values.url,
		}
		if server.Kind() == config.TransportHTTP {
			box.Headers = values.values
		} else {
			box.Env = values.values
		}
		servers = append(servers, box)
		scoped = append(scoped, name)
	}
	return sandbox.RenderMCP(servers, scoped), withheld
}

// boxServers is the scoped MCP surface one coding run's box is given
// ([sandboxMCP]), with every server withheld for its seat credentials judged
// against the integrations minting them and said, at the level its story
// earns — the seat's own child's ([Engine.withheldForCredentials]): waiting
// for provisioning is info, and the rest a warning, since the run goes ahead
// without the server either way.
//
// WHAT STARTS IT DIFFERS from a seat's own child, and the sentence says so:
// nothing restarts a box's servers, so a server withheld from this run is
// offered to the first coding run launched once the seal reaches this node.
func (e *Engine) boxServers(
	ctx context.Context, c *Company, seat *org.Role, gate *org.RoleSandbox,
) map[string]sandbox.MCPServer {
	// THE SAME TOKEN LIFE AS THE RUN'S OWN ENVIRONMENT: a server rendered
	// into the box is started there once, with whatever it is handed now.
	env, token := e.seatResolver(ctx, c, seat, github.RunTokenLife)
	servers, withheld := sandboxMCP(env, c, seat, gate,
		&seatMint{kinds: e.mintedFor(c, seat.Handle()), token: token})
	states := e.mintingStates(ctx, withheld)
	for _, missing := range withheld {
		missing = missing.judged(states)
		attrs := append([]any{"seat", seat.Handle(), "server", missing.server}, missing.logAttrs()...)
		if missing.waiting() {
			log.InfoContext(ctx, "sandbox_mcp_server_withheld", append(attrs,
				"detail", "the coding agent is not given this server for this run: "+
					missing.reason()+"; a coding run launched once "+missing.arrival()+
					" is given it")...)
			continue
		}
		log.WarnContext(ctx, "sandbox_mcp_server_withheld", append(attrs,
			"detail", "the coding agent is not given this server: "+missing.reason())...)
	}
	return servers
}

// pauseTTL reads the seat's override, distinguishing "inherit" from "never".
//
// UNSET MEANS INHERIT and an explicit zero means never pause — two genuinely
// different instructions, which is why both the config field and the manager's
// input carry a pointer rather than a sentinel number. A negative value never
// reaches here: role validation refuses it.
//
// NIL-SAFE, like [maxTurnsFor] and for the same seat: a box-mode executor
// is placed by its own providers.llm entry and runs in a box whether or not
// role.sandbox was ever written, and reading the override off a block that
// does not exist panicked that seat's first launch — after the bridge session
// was opened and before anything would have closed it.
func pauseTTL(gate *org.RoleSandbox) *time.Duration {
	if gate == nil || gate.PauseTTLSeconds == nil {
		return nil
	}
	d := seconds(*gate.PauseTTLSeconds)
	return &d
}

// seatSandbox is a seat's sandbox gate, or nil.
//
// KEYED ON THE HANDLE, which is what makes the answer single-valued. Two seats
// may carry one display NAME — it is an admission rule, so a revision stored
// before that rule runs with duplicates and a write merely cannot add one —
// and a walk matching on it returned whichever came first. A seat with no
// sandbox block would then be handed its namesake's, which is a coding run on
// another seat's setup steps, its credentials and its box.
//
// A handle is unique by a RUNNABLE rule instead: a company carrying two is
// refused outright, so there is no document on which this can be ambiguous.
// ONE LOOKUP, NOT A WALK OF ITS OWN, because the seat is what carries the
// block now.
//
// It used to walk the company DOCUMENT — first only the top-level `roles:`,
// which answered nil for every seat in a unit and refused each of them with
// "this seat's sandbox is not enabled" on a seat whose block said otherwise;
// then every seat at any depth, breaking at the first match. Both were
// walking the wrong thing: a stored revision carries no seats at all, so the
// walk returned nil for EVERY seat and code work became silently unavailable
// to the whole company.
//
// The seat's own [org.Role] holds it, so this asks the org the company is
// running, through the one lookup every other layer resolves a handle with —
// aliases and all. The "two seats with one handle" hazard the walk had to
// reason about cannot arise there, because that lookup answers a live handle
// before any retired one and a company carrying two live ones is refused.
func seatSandbox(c *Company, handle string) *org.RoleSandbox {
	if c == nil || c.Org == nil {
		return nil
	}
	seat := c.Org.Role(handle)
	if seat == nil {
		return nil
	}
	return seat.Sandbox
}

// sandboxRuntime is this node's code-work machinery, published whole: the
// durable row store every detached run is recorded in, and the coordinator
// that drives those runs over it. See [Engine.sandbox].
type sandboxRuntime struct {
	pending     sandbox.PendingStore
	coordinator *sandbox.Coordinator
}

// sandboxManager is this node's current sandbox manager, or nil where no
// runtime has come up — read LIVE through the runtime, because the runtime
// may arrive by apply and every apply swaps the manager under its coordinator
// ([sandbox.Coordinator.SetManager]). The live-output reader and server take
// it as a function for that reason.
func (e *Engine) sandboxManager() *sandbox.Manager {
	rt := e.sandbox.Load()
	if rt == nil {
		return nil
	}
	return rt.coordinator.Manager()
}

// startSandboxFor is [Engine.startSandbox] for the company a node boots on:
// its catalogue built, and the runtime brought up where it reaches a cell.
// An apply builds the catalogue itself, earlier, so that a revision whose
// catalogue cannot be built is refused before anything else moves.
func (e *Engine) startSandboxFor(ctx context.Context, c *Company) error {
	manager, err := buildSandbox(c.Config, e.resolver(), e.sandboxOtel)
	if err != nil {
		return err
	}
	_, err = e.startSandbox(ctx, manager)
	return err
}

// startSandbox brings up this node's code-work machinery for the first company
// whose catalogue builds a manager, and reports whether this call brought it
// up.
//
// ONCE PER PROCESS, from whichever comes first: the boot of a company that
// reaches a sandbox cell, or the apply of the first revision that does. It is
// the second of those that had no path at all — the coordinator was built at
// boot or never, so a node started unconfigured (the quickstart's own route)
// or on a company with no providers.sandbox served every later revision's
// code-enabled seats with no run_sandbox, no box-mode executor and no way to
// finish a run a peer had left on one of its seats, until the process
// restarted. Once up, a revision only swaps the manager ([Engine.Apply]'s
// `sandbox` stage), because the busy set and the poll loop are this process's.
//
// A NIL MANAGER IS NOT A START: the company reaches no cell, and the runtime
// waits for one that does.
//
// SPLIT FROM THE POLL on the boot path, where the two need different things to
// exist: the coordinator must exist before equip, which registers run_sandbox
// only on a node that has one, and the poll needs the node, whose incarnation
// its fleet-singleton duty is claimed under. On an apply the node already
// exists, so this starts the poll too — and PREPARES THE SEATS this node
// already holds, which took their leases before there was a control topic to
// attach or a run to recover ([Engine.prepareHeldSeats]).
func (e *Engine) startSandbox(ctx context.Context, manager *sandbox.Manager) (bool, error) {
	if manager == nil || e.sandbox.Load() != nil {
		return false, nil
	}
	rt, err := e.buildSandboxRuntime(manager)
	if err != nil {
		return false, err
	}
	// PUBLISHED BEFORE THE SEATS ARE PREPARED, so a seat acquired from here
	// on prepares itself through its own hook, and the walk below finds it
	// already attached rather than racing it.
	if !e.sandbox.CompareAndSwap(nil, rt) {
		return false, nil
	}
	log.Info("sandbox_enabled", "placements", manager.Placements(),
		"default_run_in", string(manager.DefaultPlacement()),
		"coding_agent", manager.DefaultCodingAgent())
	if e.node == nil {
		// BOOT: the node, the poll and every seat come after this.
		return true, nil
	}
	if e.mode.Publishes() {
		if err := e.startSandboxWaiter(ctx); err != nil {
			return true, err
		}
	}
	e.prepareHeldSeats(ctx, rt)
	return true, nil
}

// buildSandboxRuntime builds the machinery [Engine.startSandbox] publishes.
func (e *Engine) buildSandboxRuntime(manager *sandbox.Manager) (*sandboxRuntime, error) {
	if e.backends == nil || e.backends.Fleet == nil {
		// A detached run's RECORD is what survives the turn that starts
		// it, so a node with no coordination store cannot offer code work
		// at all. Refused loudly rather than degraded: a sandbox whose
		// runs vanish is worse than no sandbox, because the box keeps
		// running and billing with nobody to collect it.
		//
		// The FLEET's store, not this node's: a run is recovered by
		// whichever node owns its seat next, and that node is not
		// reliably the one that launched it.
		return nil, fmt.Errorf("providers.sandbox needs a coordination store: a detached " +
			"run's state is a fleet record, and without one a seat handoff orphans every box")
	}
	pending := sandbox.NewCoordStore(e.backends.Fleet)

	coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
		Queue: e.backends.Queue, Pending: pending, Manager: manager,
		Resume:  &resumer{engine: e},
		Account: e.sandboxAccountant(),
		// Who a parked question is put to, resolved against the live
		// chart at the park — see audience.go.
		Audience: audienceResolver{engine: e},
		// A report, a failure or a question past what the run's record
		// carries is condensed by the seat's auxiliary model rather than cut.
		Condense: runCondenser{engine: e},
		// The per-run tool bridge dies with the run — see
		// [sandbox.CoordinatorOptions.Ended]. Idempotent, and reached
		// from every settle path, so a run that failed before it ever
		// had a box closes its session too.
		Ended: e.bridge.Close,
		// AND IS SERVED AGAIN wherever the run is taken over while its box
		// works: this node after a restart, or the seat's next holder —
		// see [sandbox.CoordinatorOptions.Recovered].
		Recovered: e.reopenAgentRun,
		// A run that stops with a turn still suspended into it takes the
		// working indicator down with it, whichever way it stopped: the
		// agent is waiting on a person, or is never coming back at all,
		// and either way the turn does not return to say so. ONE
		// FUNCTION FOR EVERY REASON, because to the indicator they are
		// one fact — the frame that raised it has already returned. See
		// [sandbox.CoordinatorOptions.Stopped].
		Stopped: e.releaseWorkingStatus,
		// A seat's inbox is held while one of its runs holds it or an
		// answer's resume is owed, and a retried resume asks the seat's
		// conditions first — see [seatHold] and [Engine.mayResumeAnswer].
		Hold:  seatHold{engine: e},
		Admit: e.mayResumeAnswer,
		// AND EVERY RESUME, on either route, is a turn this node admits
		// as it admits a mailbox's: the seat owned fresh, a slot under
		// node.max_concurrent, and no drain. See [Engine.admitResume].
		Gate: e.turnGate(),
		// A recorded answer that leaves its run — taken by a turn, on the
		// inline attempt as on a retry, or handed back — is recorded as
		// worked in the same completion ledger the dispatcher records a
		// consumed answer in, at that moment, so the original delivery
		// coming round afterwards is dropped rather than run as a second
		// message. See [sandbox.CoordinatorOptions.Spent].
		Spent: e.spendAnswer,
		// And an answer by turn that comes round on the control topic after
		// it was spent is acknowledged without reaching its run, off the same
		// ledger. See [sandbox.CoordinatorOptions.WasSpent].
		WasSpent: e.answerWorked,
		// Every launch is stamped with the seat lease this node runs the
		// seat under, so the seat's next holder can fence this node off
		// the run. See [sandbox.CoordinatorOptions.Lease].
		Lease: e.launchFence,
	})
	if err != nil {
		return nil, err
	}
	return &sandboxRuntime{pending: pending, coordinator: coordinator}, nil
}

// spendAnswer records the deliveries a parked run's recorded answer arrived in
// as worked, for the coordinator ([sandbox.CoordinatorOptions.Spent]).
//
// THE DISPATCHER'S OWN RECORD OF A CONSUMED ANSWER, written to the same fleet
// completion ledger under the same keys ([Dispatcher.SpendAnswer]), so a
// copy of the delivery reaching the seat afterwards is dropped by the ledger
// check its route makes — the ordinary route's for a chat reply, and the
// answer-by-turn route's before it asks the coordinator — the one check that
// does not depend on the run still being there to recognise it.
func (e *Engine) spendAnswer(ctx context.Context, handle string, evs []*events.Event) {
	if e.dispatch == nil {
		return
	}
	e.dispatch.SpendAnswer(ctx, handle, evs)
}

// answerWorked reads the ledger [Engine.spendAnswer] writes, for the
// coordinator ([sandbox.CoordinatorOptions.WasSpent]).
func (e *Engine) answerWorked(ctx context.Context, handle string, ev *events.Event) bool {
	if e.dispatch == nil {
		return false
	}
	return e.dispatch.AnswerWorked(ctx, handle, ev)
}

// runCondenser is the coordinator's [sandbox.Condenser]: the seat's own
// compactor, on the epoch CURRENT at the collection — the coordinator outlives
// every revision, and a run collected after an apply is condensed by the model
// the seat is configured with now.
//
// The label goes in front and is counted against the budget, because the
// budget is the record's and the label rides it.
type runCondenser struct{ engine *Engine }

func (r runCondenser) Condense(ctx context.Context, run sandbox.PendingRun, part sandbox.RunPart,
	text string, budget int) (string, sandbox.AuxTokens, error) {
	company := r.engine.Company()
	if company == nil {
		return "", sandbox.AuxTokens{}, compact.ErrUnavailable
	}
	note := compact.Result{Compacted: true, From: len(text)}.Note()
	// THE TURN'S OWN COST, filed under the run the report belongs to: the
	// resumed segment reads it as the coding run's answer.
	//
	// ITS OWN TALLY, handed back with the text: no segment's tally is open
	// while a run is collected — the condensation happens between two of
	// them, on whichever node collects — so the coordinator carries what it
	// cost to the segment that resumes from the collection, which pays it
	// to the turn's work item (ADR-0022). A rewrite that failed was paid
	// for too, so the tally is answered on every path.
	spent := auxspend.NewTally()
	use := auxspend.Use{Stage: types.AuxStageTurn, TurnID: run.TurnID, WorkKey: run.WorkKey,
		Tally: spent}
	res, err := r.engine.seatCompactor(company, run.AgentHandle, use).Fit(ctx, runPartKind(part), text,
		budget-len(note)-1)
	cost := auxTokensOf(spent.Total())
	if err != nil {
		return "", cost, err
	}
	return note + "\n" + res.Text, cost, nil
}

// auxTokensOf is an auxiliary tally in the shape a run's row carries.
func auxTokensOf(s auxspend.Spent) sandbox.AuxTokens {
	return sandbox.AuxTokens{Input: s.Input, Output: s.Output,
		CacheRead: s.CacheRead, CacheWrite: s.CacheWrite}
}

// runPartKind is what the compactor is told a piece of a run's account IS,
// which decides what its rewrite must keep: a report its findings, a failure
// its cause, a question the question and its options.
func runPartKind(part sandbox.RunPart) compact.Kind {
	switch part {
	case sandbox.PartFailure:
		return compact.KindToolError
	case sandbox.PartQuestion:
		return compact.KindQuestion
	default:
		return compact.KindReport
	}
}

// startSandboxWaiter starts the completion poll, once the node exists and a
// runtime does, and at most once.
func (e *Engine) startSandboxWaiter(ctx context.Context) error {
	rt := e.sandbox.Load()
	if rt == nil || e.sandboxWaiter.Load() != nil {
		return nil
	}
	interval := e.sandboxPollInterval
	duty, err := e.waiterDuty(interval)
	if err != nil {
		return err
	}
	waiter, err := sandbox.NewWaiter(sandbox.WaiterOptions{
		Queue: e.backends.Queue, Pending: rt.pending,
		// THE COORDINATOR'S CURRENT MANAGER, asked on every tick: an
		// apply swaps it, and the poll outlives every revision.
		Manager:  rt.coordinator.Manager,
		Interval: interval,
		// The duty is claimed per tick: the waiter polls EVERY active run
		// in the company, not just this node's seats, so N nodes running it
		// unclaimed means N reconnects per box per tick and N racing
		// reapers.
		ClaimDuty: sandbox.DutyFunc(duty),
		// The lease each claim takes, which bounds every poll the claim
		// authorises: a poll outlives the pass that started it, and must
		// not outlive the duty, past which a peer may poll the same box.
		DutyTTL: waiterDutyTTL(interval),
		// A LONG RUN'S GITHUB APP TOKEN, kept current in the files the
		// run was provisioned with: see githubtoken.go.
		Refresh: e.refreshRunTokens,
	})
	if err != nil {
		return err
	}
	if !e.sandboxWaiter.CompareAndSwap(nil, waiter) {
		return nil
	}
	waiter.Start(context.WithoutCancel(ctx))
	return nil
}

// stopSandbox halts the poll loop and the coordinator's own retries. The rows
// and the boxes are untouched: a detached run belongs to its row — an answer
// still owed its resume included — and the next owner of its seat recovers it.
func (e *Engine) stopSandbox() {
	if w := e.sandboxWaiter.Load(); w != nil {
		w.Stop()
	}
	if rt := e.sandbox.Load(); rt != nil {
		rt.coordinator.Stop()
	}
}

// sandboxSeatRuns is the inbox screening's two sandbox answers for a seat, read
// off the runtime live: no to both where there is none, which is right for a
// node that has never run code — a seat on it cannot be in either state.
func (e *Engine) sandboxSeatRuns(handle string) (held, awaitsAnswer bool) {
	rt := e.sandbox.Load()
	if rt == nil {
		return false, false
	}
	return rt.coordinator.SeatRuns(handle)
}

// answerParkedRun offers a delivery to a parked coding run, through the
// runtime current when it arrives; with none, nothing is parked here to take
// it.
func (e *Engine) answerParkedRun(ctx context.Context, handle string,
	reply sandbox.Reply) (sandbox.AnswerDisposition, error) {

	rt := e.sandbox.Load()
	if rt == nil {
		return sandbox.AnswerNotMine, nil
	}
	return rt.coordinator.TryResumeFromAnswer(ctx, handle, reply)
}

// seatHold is the seat-inbox hold the sandbox coordinator keeps while one of a
// seat's runs holds the seat, or is owed the resume of a recorded answer — see
// [sandbox.SeatHold] and [inbox.HoldSandbox].
//
// A HOLD, NOT A PARK: the mail waits on the broker with its order and its
// delivery count intact, where a park republished it onto the inbox it had
// just been fetched from, in a loop, for as long as the run held the seat.
type seatHold struct{ engine *Engine }

var _ sandbox.SeatHold = seatHold{}

func (h seatHold) Hold(ctx context.Context, handle string) error {
	subject, group, err := h.inboxOf(handle)
	if err != nil {
		return err
	}
	return h.engine.backends.Queue.PauseTopic(ctx, subject, group, string(inbox.HoldSandbox))
}

func (h seatHold) Release(ctx context.Context, handle string) error {
	subject, group, err := h.inboxOf(handle)
	if err != nil {
		return err
	}
	return h.engine.backends.Queue.ResumeTopic(ctx, subject, group, string(inbox.HoldSandbox))
}

// inboxOf is the inbox a hold on handle sits on, named by the seat's id — the
// coordinator keeps its holds by handle, and the broker names every mailbox by
// the id derived from it (ADR-0013).
//
// DERIVED rather than looked up in the roster ([Engine.seatID]), because a hold
// outlives a revision: a run that ends after an apply removed its seat must
// still release the hold it took, or a seat re-added under the same handle
// would find its inbox held for a run nobody is running.
func (h seatHold) inboxOf(handle string) (subject, group string, err error) {
	c := h.engine.Company()
	if c == nil || c.Org == nil {
		return "", "", fmt.Errorf("engine: this node runs no company, so seat %q "+
			"has no inbox to hold", handle)
	}
	id, ok := org.DeriveAgentID(c.Org.Name, handle)
	if !ok {
		return "", "", fmt.Errorf("engine: seat %q has no inbox subject", handle)
	}
	return topics.AgentInbox(id), topics.AgentInboxGroup(id), nil
}

// holdSandboxSeat is the dispatcher's [Dispatcher.HoldSandbox]: it asks the
// coordinator current when the delivery arrives for the hold it keeps on a
// seat one of whose runs holds it. With no runtime nothing holds any seat.
func (e *Engine) holdSandboxSeat(ctx context.Context, handle string) {
	if rt := e.sandbox.Load(); rt != nil {
		rt.coordinator.HoldSeat(ctx, handle)
	}
}

// What a retried resume the seat's conditions refused waits on — the
// [sandbox.Condition] each refusal names, and the one each signal re-checks.
// See [Engine.mayResumeAnswer] for which event moves each, and
// [Engine.readmitResumes] for where the engine passes it on.
const (
	// waitOwnership — the seat's lease is not fresh, or the seat is still
	// being established. Signalled when the seat starts admitting turns:
	// the moment its acquisition is established, which is how an answer
	// inherited by the seat's own preparation resumes at once rather than
	// a heartbeat (15 s at the default TTL) later, and the renew that
	// re-proves a lease after a store blip. A lease merely stale from a
	// late renew has no such edge, so the refusal also names one heartbeat
	// on the clock.
	waitOwnership sandbox.Condition = "ownership"

	// waitPause — a person paused the seat, or this node has not read the
	// pauses yet. Moved by the pause watch: a resume, or its first complete
	// answer.
	waitPause sandbox.Condition = "pause"

	// waitTurnEngine — the company configures no model. Moved by the apply
	// that brings one.
	waitTurnEngine sandbox.Condition = "turn_engine"

	// waitPosture — this node's config posture refuses new work. Moved by
	// the reconcile loop's tick that finds it admitting again.
	waitPosture sandbox.Condition = "posture"

	// waitBudget — one of the seat's capped token windows is refusing.
	// Lifted on the clock when the window turns over, which the refusal
	// names, or at once by an apply that changes the ceilings.
	waitBudget sandbox.Condition = "budget"

	// waitSeatBusy: another of the seat's turns is running on this node, and
	// a seat runs one at a time ([node.ErrSeatBusy]). Signalled the moment
	// that turn is released ([node.Config.SeatTurnEnded]), so the answer is
	// resumed as the seat's next turn, ahead of its mail, which its owed hold
	// keeps waiting.
	waitSeatBusy sandbox.Condition = "seat_busy"

	// waitProvider: every model of the seat's chain is at a usage limit it
	// already reported. Lifted on the clock when the first is tried again,
	// which the refusal names (see providerpark.go).
	waitProvider sandbox.Condition = "provider"

	// waitDrain: this node is draining, and starts no turn. Nothing
	// signals it, because nothing in the engine undoes a drain
	// ([Engine.Drain] latches): the wait ends when the drain hands the seat
	// back, which lets go of the answer for the seat's next holder to
	// resume ([sandbox.Coordinator.ReleaseSeat]), or when the node stops.
	waitDrain sandbox.Condition = "drain"
)

// turnGate is the gate the coordinator resumes through: [Engine.admitResume],
// wrapped by [Options.resumeGate] where a case in this package set one.
func (e *Engine) turnGate() sandbox.TurnGate {
	if e.resumeGate != nil {
		return e.resumeGate(e.admitResume)
	}
	return e.admitResume
}

// admitResume is the coordinator's [sandbox.TurnGate]: the node's own
// admission ([node.Node.AdmitTurn]), with its refusal named as what an
// answer's retry waits on, and then the seat's conditions a turn would only
// fail on ([Engine.seatCannotRun]).
//
// The condition is what the control topic hands a refused completion back by
// ([controlOutcome]), and the cause stays reachable through the refusal for
// whoever logs or tests it.
func (e *Engine) admitResume(ctx context.Context, handle string) (context.Context, func(), error) {
	ctx, release, err := e.node.AdmitTurn(ctx, handle)
	switch {
	case err == nil:
		if refusal, refused := e.seatCannotRun(ctx, handle); refused {
			release()
			return nil, nil, &sandbox.TurnRefused{Refusal: refusal}
		}
		return ctx, release, nil
	case errors.Is(err, node.ErrDraining):
		return nil, nil, &sandbox.TurnRefused{Cause: err, Refusal: sandbox.Refusal{
			Condition: waitDrain, Reason: err.Error()}}
	case errors.Is(err, node.ErrSeatBusy):
		return nil, nil, &sandbox.TurnRefused{Cause: err, Refusal: sandbox.Refusal{
			Condition: waitSeatBusy, Reason: err.Error()}}
	case errors.Is(err, node.ErrSeatNotOwned):
		// THE SAME WAIT [Engine.mayResumeAnswer] names for a seat it
		// does not find owned, for the same edges.
		return nil, nil, &sandbox.TurnRefused{Cause: err, Refusal: sandbox.Refusal{
			Condition: waitOwnership, Reason: err.Error(),
			Until: e.now().Add(e.node.Host().HeartbeatInterval())}}
	default:
		return nil, nil, err
	}
}

// seatCannotRun reports whether a turn on handle's seat would only fail on a
// condition that ends at an instant, and if so what it waits on: one of its
// capped token windows refusing ([Engine.budgetRefusing]), or every model of
// its chain at a usage limit it already reported ([Engine.providerRefusing]).
//
// ASKED BY THE TURN GATE, so every resume asks it before it claims its run,
// whatever brought it: a box's completion, a person's answer on its inline
// attempt or its retry, an answer queued on the control topic. Asked of the
// counters and of what the entries already know, so it launches nothing.
// A completion it refuses is acknowledged ([controlOutcome]) and kept for the
// signal that ends the wait, and the waiter announces it again meanwhile, so
// it spends none of its deliveries; it used to reach the model, revert, and be
// NAKed through its whole delivery budget in about ten minutes.
func (e *Engine) seatCannotRun(ctx context.Context, handle string) (sandbox.Refusal, bool) {
	if reason, resets, refusing := e.budgetRefusing(ctx, handle); refusing {
		return sandbox.Refusal{Condition: waitBudget, Reason: reason, Until: resets}, true
	}
	if reason, retry, limited := e.providerRefusing(ctx, handle); limited {
		return sandbox.Refusal{Condition: waitProvider, Reason: reason, Until: retry}, true
	}
	return sandbox.Refusal{}, false
}

// resumeRefusal is what a reverted resume's failure waits on, if it is one of
// [Engine.seatCannotRun]'s conditions met mid-turn: a chain every member of
// which refused for a limit, until the first is tried again, or a capped window
// the turn's meter found spent, until it turns over.
func (e *Engine) resumeRefusal(ctx context.Context, handle string, err error) (sandbox.Refusal, bool) {
	if limit, limited := llmchain.Limited(err); limited {
		return sandbox.Refusal{Condition: waitProvider, Reason: limitReason(limit.Keys, limit.RetryAt),
			Until: limit.RetryAt}, true
	}
	if errors.Is(err, toolloop.ErrBudgetExhausted) {
		if reason, resets, refusing := e.budgetRefusing(ctx, handle); refusing {
			return sandbox.Refusal{Condition: waitBudget, Reason: reason, Until: resets}, true
		}
	}
	return sandbox.Refusal{}, false
}

// mayResumeAnswer is the coordinator's [sandbox.Admission]: whether a seat may
// run a RETRIED resume of a recorded answer now, and if not, what that waits
// on.
//
// THE SCREENING'S OWN CONDITIONS, read the way a delivery reads them, because
// the first attempt runs inside the delivery that carried the answer and so
// passed them; a retry runs outside any delivery and must not be the one way a
// turn starts on a seat whose lease is not fresh, that a person paused, that
// has no model, or whose node refuses new work. A spent budget and a model's
// limit are not asked here: the turn gate every resume passes asks them
// ([Engine.seatCannotRun]). Whether another of the seat's coding runs holds it
// is the coordinator's own count, so the coordinator asks that itself first
// ([sandbox.SeatCoding]), and whether another of its turns is running is the
// node's, asked at the same gate ([Engine.admitResume]): an answer by turn
// arrives on the seat's control topic at any moment of its day, and is
// resumed only once the seat is free of both.
//
// EACH REFUSAL NAMES WHAT IT WAITS ON, so the retry waits for that rather than
// re-checking on a timer (see [sandbox.Coordinator.Readmit]). Ownership is an
// event (the seat host reports the seat established, or its admission back
// after a blip, node.Config.SeatAdmitted) with one heartbeat on the clock
// behind it for a renew that was merely late. The rest are events this node
// observes and passes on in [Engine.readmitResumes]'s callers.
func (e *Engine) mayResumeAnswer(_ context.Context, handle string) (sandbox.Refusal, bool) {
	c := e.conditionsFor(nil)(handle)
	switch {
	case !c.Owned:
		return sandbox.Refusal{Condition: waitOwnership, Reason: "seat is not owned here",
			Until: e.now().Add(e.node.Host().HeartbeatInterval())}, true
	case c.PauseUnknown:
		return sandbox.Refusal{Condition: waitPause,
			Reason: "this node has not yet read whether the seat is paused"}, true
	case c.Paused:
		return sandbox.Refusal{Condition: waitPause, Reason: "a person paused this seat"}, true
	case !c.TurnEngineReady:
		return sandbox.Refusal{Condition: waitTurnEngine, Reason: "no turn engine"}, true
	case !c.AdmitsTriggers:
		return sandbox.Refusal{Condition: waitPosture, Reason: "config posture refuses new work"}, true
	}
	return sandbox.Refusal{}, false
}

// readmitResumes passes on that a condition a resume may be waiting on can have
// cleared, for the named seats or every seat: the coordinator re-checks at once
// the attempts waiting on exactly that, an owed answer's retry and a completion
// the node's admission refused ([sandbox.Coordinator.Readmit]). It is THE hook
// for whatever ends a condition a refusal names: call it with that condition
// and the seat, and a refused completion resumes now rather than at the
// waiter's next poll. Nothing to do on a node with no sandbox runtime, which
// owes no answer and kept no completion.
func (e *Engine) readmitResumes(cond sandbox.Condition, handles ...string) {
	if rt := e.sandbox.Load(); rt != nil {
		rt.coordinator.Readmit(cond, handles...)
	}
}

// sandboxAccountant charges collected runs, or nil where nothing counts them.
func (e *Engine) sandboxAccountant() sandbox.Accountant {
	if e.backends == nil || e.backends.Fleet == nil {
		return nil
	}
	return sandboxAccountant{
		budgets: e.backends.Fleet,
		basis: func(agentID string) budgetBasis {
			c := e.Company()
			if c == nil || c.Org == nil {
				// No epoch yet: nothing is capped, and the run is still
				// counted, on UTC — the clock a company that names none
				// has.
				return basisOf(c, nil)
			}
			id, err := uuid.Parse(agentID)
			if err != nil {
				return basisOf(c, nil)
			}
			return basisOf(c, c.Org.AgentSeatByID(id))
		},
		// THE ENGINE'S INSTANT, the one its gate charges a round at, so a
		// collected run is counted in the window a seat's next round is.
		now: e.now,
	}
}

// waiterDutyName is the fleet singleton the sandbox waiter claims.
const waiterDutyName = "sandbox-waiter"

// waiterDuty gates the poll tick on holding the fleet's waiter duty.
//
// Nil where there is no coordination backend, which is the single-node case:
// that node always holds it, and a wrapper that always said yes would make a
// single node report itself as a fleet singleton.
//
// An interval whose duty no backend will grant is refused HERE, at start,
// rather than on every tick: the waiter fails closed on a claim error, so a
// refused TTL would leave it never ticking, every detached run hanging and
// every box losing its keepalive, with one warning per tick as the only sign.
// [New] asks the same question before anything is open, because the start can
// be an apply's (see [checkSandboxPollInterval]).
func (e *Engine) waiterDuty(interval time.Duration) (schedule.DutyFunc, error) {
	if err := checkSandboxPollInterval(interval); err != nil {
		return nil, err
	}
	if e.backends == nil || e.backends.Coord == nil {
		return nil, nil
	}
	// The TTL expression is spelled as the duty-TTL guard test expects it;
	// see TestEveryDutyTTLFitsTheDutyCeiling.
	return e.workerDuty(waiterDutyName, waiterDutyTTL(interval)), nil
}

// checkSandboxPollInterval refuses a poll interval whose waiter duty no backend
// will grant.
//
// ASKED AT BOOT whether or not this node runs a sandbox yet, as well as where
// the poll starts: a node that boots with none starts its poll at whichever
// apply brings the first, and refused THERE the interval would leave a runtime
// published with no poll — every run launched on it never collected — on a
// revision that was itself fine. An Option is the operator's, so it is refused
// where the operator is still looking.
func checkSandboxPollInterval(interval time.Duration) error {
	if err := coord.CheckLeaseTTL(coord.WorkerResource(waiterDutyName), waiterDutyTTL(interval)); err != nil {
		return fmt.Errorf("engine: a sandbox poll interval of %v needs a %v waiter duty; "+
			"lower Options.SandboxPollInterval: %w", interval, waiterDutyTTL(interval), err)
	}
	return nil
}

// dutyTTLTicks is how many poll intervals the waiter duty survives without a
// re-claim, and dutyTTLFloor the shortest it may ever be.
//
// Three intervals is the same "do not flap on a blip" rule the scheduler duty
// and the seat heartbeat follow: the holder re-claims every tick, so this
// rides out two consecutive slow or failed claims without the duty moving. The
// invariant is the RATIO, not any duration — a duty that cannot outlive three
// of its own ticks moves on ordinary jitter, and one that outlives many is
// time the fleet has no waiter after a holder dies.
//
// The floor is for the other end: this code is driven at a sub-second cadence
// in tests, and three of those is a lease that lapses inside its own claim.
const (
	dutyTTLTicks = 3
	dutyTTLFloor = 30 * time.Second
)

// waiterDutyTTL is how long the waiter duty survives without a re-claim.
//
// DERIVED FROM THE INTERVAL IT GUARDS, and from nothing else. It used to be
// `3 * sandbox.DefaultPollInterval`, a compile-time constant that ignored the
// configured interval entirely, and then it was capped at the seat lease TTL,
// because duties shared the seat lease bucket and the KV refused a lease
// longer than that bucket's age. The cap was wrong in its own right: a poll
// interval longer than the seat lease TTL got a duty that lapsed between two
// of its own ticks, so the waiter moved to whichever peer ticked first on
// every tick. Duties now have a bucket of their own whose ceiling is
// [coord.MaxDutyTTL], so the ratio holds for every interval that ceiling
// admits, and [Engine.waiterDuty] refuses the ones it does not.
func waiterDutyTTL(interval time.Duration) time.Duration {
	if interval <= 0 {
		interval = sandbox.DefaultPollInterval
	}
	return max(dutyTTLTicks*interval, dutyTTLFloor)
}

// prepareSeat is the node's SeatReady hook: recover this seat's in-flight runs
// and start listening for their completions, BEFORE its mailbox opens.
func (e *Engine) prepareSeat(ctx context.Context, seat placement.Seat, epoch int64, owner string) error {
	handle := seat.Handle
	// THE SEAT'S OWN TOOLS FIRST, because OnAcquire's whole contract is
	// that everything the first turn needs is ready before the mailbox
	// opens. A seat that started consuming before its per-role children
	// were up would run its first turn with a surface missing exactly the
	// tools it acts through.
	// startSeatServers files the registry alongside the bridge, so the two
	// cannot disagree about which children this seat has — and returns only
	// once an apply that landed while they were starting has been folded in.
	e.startSeatServers(ctx, handle)

	// THE SEAT'S MEMORY, before it can take a turn. Its diary, episodes,
	// counterparties and skills were written on whichever nodes ran it
	// before, and placement moves seats — so without this the seat opens
	// its mailbox against a store that has never heard of it and runs its
	// first turn having forgotten everything it learned elsewhere.
	//
	// A failure REFUSES the seat, like every other step here. A peer that
	// takes it instead may hydrate cleanly, and a seat serving with
	// amnesia produces work its own history contradicts — which is worse
	// than the seat waiting for the next placement sweep.
	if e.memory != nil {
		if _, err := e.memory.Hydrate(ctx, handle); err != nil {
			return fmt.Errorf("carrying the seat's memory: %w", err)
		}
	}
	// AND WHAT IT HAS STILL TO LEARN: reflection runs where the seat is held,
	// on the memory just hydrated.
	if err := e.attachSeatReflection(ctx, seat); err != nil {
		return err
	}

	if rt := e.sandbox.Load(); rt != nil {
		if err := e.prepareSeatSandbox(ctx, rt, seat, epoch, owner); err != nil {
			return err
		}
	}
	// THE SEAT IS LIVE HERE, which is the fact the live projection has
	// always had a branch for and nothing ever published: an
	// `agent_spawned` is what clears a stale `terminated`, `offline` or
	// `afk` from a seat that has moved to this node, so without it a seat
	// whose last owner went away renders as broken until it happens to do
	// some work.
	//
	// FOR EVERY SEAT, not only a code-enabled node's: this sat behind the
	// sandbox step's early return, so on the ordinary node — one whose
	// company configures no providers.sandbox — no seat was ever announced
	// and every one of them stayed on whatever its last owner left behind.
	e.publishSeatLifecycle(ctx, handle, types.AgentSpawned{})
	return nil
}

// prepareSeatSandbox is the seat's half of the code-work machinery: its control
// topic attached, then its detached runs recovered, BEFORE anything can
// deliver to it. Exactly once per holding, whichever path reaches it first —
// the seat's own acquisition, or the apply that brought the runtime up while
// the seat was already held; the caller holds the seat's lock either way.
//
// THE CONTROL TOPIC IS NAMED BY THE SEAT'S ID, the agent id derived from its
// handle (ADR-0013), as its inbox is.
func (e *Engine) prepareSeatSandbox(ctx context.Context, rt *sandboxRuntime,
	seat placement.Seat, epoch int64, owner string) error {

	handle := seat.Handle
	if e.sandboxSeatAttached(handle) {
		return nil
	}
	control, group := topics.AgentControl(seat.ID), topics.AgentControlGroup(seat.ID)
	if control == "" {
		return nil
	}
	// The subscription is attached BEFORE recovery, so a completion
	// published in the window between the two is held rather than dropped.
	// A detached run outlives its node, so that window is not hypothetical.
	if err := e.backends.Queue.Subscribe(ctx, control, group,
		func(ctx context.Context, ev *events.Event) queue.Result {
			return e.seatControlOutcome(handle, rt.coordinator.OnEvent(ctx, ev))
		}); err != nil {
		return fmt.Errorf("attaching the sandbox control topic: %w", err)
	}
	// ATTACHED FROM HERE, whatever recovery says: a release has to detach
	// what this call attached, and a refused seat is released.
	e.markSandboxSeat(handle, true)
	if err := rt.coordinator.RecoverSeat(ctx, handle, owner, epoch); err != nil {
		return err
	}
	// EVERY RUNNING JOB'S SESSION IS OPEN AGAIN HERE NOW ([Engine.reopenAgentRun]),
	// so the bridge may say a job it holds no session for is over — and not
	// before, or a box calling in the moment between the seat's lease and
	// its recovery would be told its live run had ended.
	e.bridge.SeatReady(seat.ID)
	return nil
}

// controlOutcome is what a seat's control-topic delivery leaves with the broker,
// given what the coordinator made of it.
//
// # A refusal is handed back by the condition it names
//
// A completion the node's admission refused ([Engine.admitResume]) left its run
// unclaimed and running, so the delivery carries nothing that is lost with it:
// the waiter announces a finished run again on every poll for as long as its
// row is running ([sandbox.Waiter]). What a refusal does to the delivery is
// therefore keyed on the condition it names ([sandbox.TurnRefused]), never on
// which error caused it, so a condition a later change adds is handed back by
// the rule for its kind with nothing here to edit:
//
//   - A CONDITION THAT CLEARS BY WAITING (the seat's ownership not yet proven
//     again, and any limit a seat sits out) is ACKED. A Nak or a deferral each
//     spend one of the broker's deliveries ([queue.OutcomeDefer]), so handing a
//     healthy completion back on a wait would walk it to the dead-letter
//     boundary while nothing was wrong with it, and a deferral would also
//     quiesce the attachment, which nothing undoes. The coordinator keeps the
//     completion and re-fires it when the condition clears
//     ([Engine.readmitResumes]), and the poll's next announcement is the
//     backstop for a clearing nothing signals, one poll interval at most.
//   - A DRAINING NODE ([waitDrain]) DEFERS: the condition does not clear on this
//     node, which is leaving, so the attachment is quiesced for the rest of the
//     drain, as a mailbox's is, and the completion goes back to the head of the
//     topic for the seat's next holder. It costs one delivery, as every handoff
//     does, which the delivery budget is sized for.
//
// # Everything else NAKs
//
// A completion this node could not settle (the store failing, a refusal that
// names no condition, a seat the store says a newer lease owns while this
// node's host still holds it) comes back on the broker's backoff: to this node
// once its store recovers, or to the seat's next owner. Acking it would lose
// the turn. A seat this node's host no longer holds is the one exception, and
// it is [Engine.seatControlOutcome]'s, because only the host knows it.
func controlOutcome(err error) queue.Result {
	if err == nil {
		return queue.Ack()
	}
	var refused *sandbox.TurnRefused
	if !errors.As(err, &refused) || refused.Condition == "" {
		return queue.Nak(err)
	}
	if refused.Condition == waitDrain {
		return queue.Defer(err.Error())
	}
	return queue.Ack()
}

// seatControlOutcome is [controlOutcome] for one seat's control topic on this
// node, with the one rule only the seat host can apply.
//
// # A seat this node has let go of DEFERS
//
// A write refused because this node does not hold the seat
// ([sandbox.ErrSeatNotHeld]) is work this process has lost the right to do,
// which is what a deferral is for ([queue.OutcomeDefer]): the delivery goes back
// to the head of the topic for the seat's holder, and this node's attachment is
// quiesced, so it is not handed the same answer or completion again on a
// backoff. It costs ONE delivery, as every handoff does. A Nak instead spent one
// per redelivery for as long as the attachment outlived the lease, and a person's
// answer that kept coming back to a node refusing it walked to the dead-letter
// subject. The release that follows the host's letting go detaches the
// attachment, and a later claim attaches a fresh one.
//
// ONLY WHEN THE HOST AGREES. The same sentinel is what the run store answers a
// write whose row a newer lease has stamped, and the host still holding the seat
// then means the two disagree: the heartbeat settles which is right within a
// beat, by dropping the seat or by renewing it. Quiesced on the store's word
// alone, a seat this node does hold would hear no completion or answer again,
// since nothing resumes a control attachment, which is a far worse failure than
// the backoff of a Nak, whose dead letter at least says so.
func (e *Engine) seatControlOutcome(handle string, err error) queue.Result {
	if errors.Is(err, sandbox.ErrSeatNotHeld) {
		if _, held := e.launchFence(handle); !held {
			return queue.Defer(err.Error())
		}
	}
	return controlOutcome(err)
}

// sandboxSeatAttached reports whether this node attached the seat's sandbox
// control topic, and markSandboxSeat records that it did or no longer does.
func (e *Engine) sandboxSeatAttached(handle string) bool {
	e.sandboxSeatsMu.Lock()
	defer e.sandboxSeatsMu.Unlock()
	return e.sandboxSeats[handle]
}

func (e *Engine) markSandboxSeat(handle string, attached bool) {
	e.sandboxSeatsMu.Lock()
	defer e.sandboxSeatsMu.Unlock()
	if attached {
		e.sandboxSeats[handle] = true
		return
	}
	delete(e.sandboxSeats, handle)
}

// prepareHeldSeats gives every seat this node already holds the code-work half
// of its preparation, when the runtime comes up after they were taken.
//
// A SEAT TAKEN BEFORE THE RUNTIME EXISTED attached no control topic and
// recovered no run, because its acquisition had nothing to attach them to. Left
// so, the first run it launched under the revision that brought the runtime
// would publish its completion to a topic nothing on this node consumes, and a
// run a peer left on it would be polled by nobody here: the seat held the
// coding job's turn for ever and never resumed it.
//
// UNDER EACH SEAT'S OWN LOCK, the one its acquisition and release run under, so
// a seat claimed or handed on while this walks is prepared exactly once or not
// at all ([seat.Host.WithHeldSeat]).
//
// A SEAT THAT CANNOT BE PREPARED IS HANDED BACK, exactly as an acquisition that
// failed the same step is refused: its next acquisition, here or on a peer,
// runs the whole preparation again. Voluntarily — the lease is good and the turn
// in flight finishes — which is [seat.ReasonUnprepared].
func (e *Engine) prepareHeldSeats(ctx context.Context, rt *sandboxRuntime) {
	host := e.node.Host()
	// THE SEATS THEMSELVES, as the host recorded them at the claim: the
	// control topic is named by the seat's id, which the handle alone does
	// not carry.
	for _, held := range host.HeldSeats() {
		handle := held.Handle
		_, err := host.WithHeldSeat(handle, func(lease coord.Lease) error {
			return e.prepareSeatSandbox(ctx, rt, held, lease.Epoch, lease.Owner)
		})
		if err == nil {
			continue
		}
		log.ErrorContext(ctx, "sandbox_seat_unprepared", "seat", handle, "error", err,
			"detail", "the seat is handed back so its next acquisition prepares it "+
				"for code work whole")
		host.Release(ctx, handle, seatpkg.ReasonUnprepared)
	}
}

// publishSeatLifecycle announces a seat arriving on or leaving this node.
//
// LIVE-ONLY, and deliberately: placement moves seats between nodes on every
// rebalance, so a durable row per claim would fill the audit log with a fact
// about scheduling rather than about the company — which is why neither type
// is in [events.Category]'s map. What reads them is the live seat state, where
// "this seat is running here now" is exactly the question.
//
// THE SEAT'S AGENT ID under `agent_id`, because that is what every other
// seat-level event carries there and what the projection keys a seat by. It
// carried the HANDLE, so the one field the projection reads its key from meant
// a different thing on these two types than on every other: a seat's arrival
// and its first turn landed under two identities. Neither a handle nor a name
// can stand in for the id — a name is prose two seats may share, and the
// projection keys every other seat-level event on the id (ADR-0013). The
// handle and the name ride along for a reader.
func (e *Engine) publishSeatLifecycle(ctx context.Context, handle string,
	payload events.Payload) {

	if e.backends == nil || e.backends.Queue == nil {
		return
	}
	role := e.seatRole(handle)
	agentID := e.seatAgentID(role)
	if role == nil || agentID == "" {
		// A HANDLE THIS EPOCH NO LONGER HAS, which is the ordinary way a
		// seat is released: the revision that removed it is already
		// current. There is no seat to key the row on, and inventing an
		// identity from the handle would make a second row for the same
		// seat.
		return
	}
	var ev *events.Event
	switch p := payload.(type) {
	case types.AgentSpawned:
		p.Agent, p.AgentHandle, p.RoleName = agentID, role.Handle(), role.Name
		ev = events.New(p, tracing.TraceOf(ctx))
	case types.AgentTerminated:
		p.Agent, p.AgentHandle, p.RoleName = agentID, role.Handle(), role.Name
		ev = events.New(p, tracing.TraceOf(ctx))
	default:
		return
	}
	ev.Source = role.Name
	// ON A BOUND OF ITS OWN ([lifecycleContext]), never a step of a stop's
	// allowance when a drain released the seat: a live screen ages a seat out
	// whether or not this lands, and the allowance is the time the seat's
	// LEASE needs to be given back.
	publishCtx, cancel := lifecycleContext(ctx)
	defer cancel()
	if err := e.backends.Queue.Publish(publishCtx, topics.Event(ev.Type), ev); err != nil {
		log.WarnContext(ctx, "seat_lifecycle_not_published", "type", ev.Type,
			"seat", handle, "error", err.Error(),
			"detail", "the live seat state keeps whatever this seat last showed")
	}
}

// releaseSeat is the node's SeatDone hook.
//
// The control subscription is DETACHED, never deleted: a completion published
// while the seat is between owners must be held for its successor, and the box
// it refers to is still real.
func (e *Engine) releaseSeat(ctx context.Context, seat placement.Seat) {
	handle := seat.Handle
	// FIRST, while the queue is still reachable and before anything this
	// release tears down: a seat that went away with no event left its
	// last state standing on every dashboard — stuck "working" in a phase
	// that ended, because `terminated` was a state nothing could reach.
	//
	// BESIDE THE TEARDOWN BELOW rather than in front of it, because the
	// event travels on the stream and a stream that will not acknowledge
	// holds it for its whole bound ([lifecyclePublishBudget]), which the
	// seat's teardown — its last memory publish among it — has no reason to
	// wait out. And JOINED BEFORE THIS RETURNS, which is before the host
	// gives the seat's lease back: a peer that takes the seat over then
	// announces its `agent_spawned` after this node's `agent_terminated`,
	// never before it, and the live projection, which keys on the role,
	// would otherwise show a running seat as terminated.
	var terminated sync.WaitGroup
	defer terminated.Wait()
	terminated.Go(func() {
		e.publishSeatLifecycle(ctx, handle,
			types.AgentTerminated{Reason: "the seat was released by this node"})
	})
	// The seat's children die with its lease. The credentials in one ARE
	// that seat's identity, so a child left running would let this node go
	// on acting as a seat a peer has taken over — and the surface goes
	// with them, so nothing here can serve a turn through a dead client.
	// stopSeatServers drops the registry with the bridge, in one step.
	e.stopSeatServers(ctx, handle)
	// A budget park's alarm dies with the seat too. The detach that
	// released the inbox already dropped the hold; the alarm would only
	// fire into a seat a peer now serves.
	e.forgetBudgetPark(seat.ID)
	e.forgetProviderPark(ctx, seat.ID)
	// AND THE TOOL SESSIONS OF ITS BOX-MODE RUNS, which the seat's next
	// holder serves from now on: left open here they would pin the seat's
	// surface in a process that no longer speaks for it, and every box
	// would go on talking to it — see [mcpbridge.Bridge.ReleaseSeat].
	e.bridge.ReleaseSeat(seat.ID)

	// AND THE SEAT'S WORKING INDICATORS, for the reason the event above
	// exists: a seat that went away with no signal leaves its last state
	// standing, and on a chat surface that state is a live re-assertion
	// rather than a stale row. A turn that suspended into a detached coding
	// run deliberately leaves its indicator up, and the resume lands on
	// whichever node holds the seat when the box reports — so a node that has
	// handed the seat on would otherwise go on saying "is thinking…" every
	// refresh interval, for a turn it is not running, until the process died.
	//
	// The clear has to go out even when the release is a cancelled drain,
	// and a chat instance that has stopped answering must cost the drain
	// seconds rather than a client timeout per seat — both of which the
	// driver sees to itself, detaching and bounding each request it makes
	// (see [notify.StatusDriver.ClearFor]).
	e.Status().ClearFor(ctx, handle)

	// REFLECTION FIRST, so nothing new writes the seat's memory here after
	// the flush below.
	e.detachSeatReflection(ctx, seat)

	// A LAST PUBLISH, then forget the seat. The publish is what makes a
	// graceful handoff lossless: whatever this node learned since its last
	// cycle reaches the changelog before the successor hydrates. Forgetting
	// the watermarks is what makes a LATER re-acquisition correct — resuming
	// from the old marks would skip exactly the memory the other node made
	// in between.
	//
	// Best effort, and it must be: this runs on the release path, where the
	// seat is already gone. A failure here costs the successor whatever was
	// written since the last cycle, which is the same bounded loss a crash
	// costs — and refusing to release would strand the seat for a full TTL.
	if e.memory != nil {
		// BOUNDED, because this runs on the release path — including a
		// drain, where every seat passes through here in turn. A broker
		// that has stopped answering must cost the drain a few seconds
		// per seat, not the whole shutdown.
		flushCtx, stopFlush := context.WithTimeout(
			context.WithoutCancel(ctx), memoryFlushTimeout)
		if _, err := e.memory.Publish(flushCtx, handle); err != nil {
			log.WarnContext(ctx, "seat_memory_not_flushed", "seat", handle, "error", err)
		}
		stopFlush()
		e.memory.Forget(handle)
	}

	rt := e.sandbox.Load()
	if rt == nil {
		return
	}
	rt.coordinator.ReleaseSeat(handle)
	// ONLY WHAT THIS NODE ATTACHED: a seat taken before the runtime came up,
	// and handed on before its preparation reached it, has no consumer here
	// to detach.
	if !e.sandboxSeatAttached(handle) {
		return
	}
	// FORGOTTEN WHETHER OR NOT THE DETACH LANDS. A re-acquisition that
	// found the mark would skip its own attach, and if the detach did in
	// fact take the consumer down the seat would collect no completion at
	// all; attaching again beside one that survived is a second competing
	// consumer over the same coordinator, which settles each delivery once.
	e.markSandboxSeat(handle, false)
	control, group := topics.AgentControl(seat.ID), topics.AgentControlGroup(seat.ID)
	if control == "" {
		return
	}
	if _, err := e.backends.Queue.Detach(ctx, control, group); err != nil {
		log.WarnContext(ctx, "sandbox_control_detach_failed", "seat", handle, "error", err)
	}
}

// SeatHeldBySandbox reports whether a detached coding run HOLDS a seat, so it
// starts no new turn until the run settles.
//
// Exported for the operator surfaces and for a test: the inbox screening reads
// it internally through the dispatcher's conditions, but "is this seat busy on
// code work" is also a question a dashboard asks, and answering it from a
// second place would eventually answer it differently.
//
// It is NOT "does this seat have a run waiting for an answer" — a parked run
// frees its seat by design. See [sandbox.Coordinator.SeatRuns].
func (e *Engine) SeatHeldBySandbox(handle string) bool {
	held, _ := e.sandboxSeatRuns(handle)
	return held
}

// codingRun is what one coding box is handed to reach a model: the model, the
// credential files a local backend seeds, and the whole run environment.
//
// THREE things travel, and they travel differently on purpose:
//
//   - The MODEL, so the agent addresses the model the seat runs on rather
//     than its catalogue's default — the entry's model exactly as its text
//     calls name it, for its own CLI only.
//   - A TOKEN OR A KEY, in the run environment, which reaches any box
//     including a remote one: it is one scoped, revocable variable — a
//     subscription's headless token, an api-key entry's key, or a key the
//     entry's env names.
//   - The credential FILES, only as a host-path map the LOCAL backend seeds
//     and writes back. They carry a refresh token whose rotation is shared
//     fleet state, so pushing them onto somebody else's VM is a materially
//     larger trust step than the token — which is why the map is offered and
//     each backend decides, rather than being exported like the rest.
type codingRun struct {
	model string
	files map[string]string
	env   map[string]string
}

// newCodingRun resolves the [codingRun] of a box driven by runner on the
// entry seat's phase resolves to, over seatEnv — the environment the seat's
// own config assembled ([Engine.runEnvironment]) — or refuses one that runner
// cannot drive with a [SandboxModelError], before a box exists.
//
// TWO CALLERS, TWO PHASES, and the difference is load-bearing. A run_sandbox
// call is CODE WORK the executor delegated, so it runs on llm_sandbox — a seat
// legitimately points that at a cheaper or more code-shaped model than the one
// it thinks with. A BOX-MODE run is the executor ITSELF, so it runs on the
// executor's own entry: sending it to llm_sandbox would run a seat's whole turn
// on the model it chose for a subordinate job, silently, on any seat that set
// both.
//
// THE RUNNER IS AN INPUT, because an entry's model is written in its own
// CLI's grammar and its sign-in is that CLI's. A run_sandbox launch takes its
// runner from role.sandbox.coding_agent and its model from llm_sandbox
// independently, so the pairing is the operator's, and one entry under ANOTHER
// CLI's runner cannot work: Claude Code was handed an OpenCode entry's
// `openrouter/anthropic/claude-sonnet-5`, and OpenCode a Claude Code entry's
// `sonnet` with a login it never reads, each starting a box that failed inside
// it. A box-mode run never meets this while its turn's epoch holds: its
// runner is its entry's own CLI. Config refuses the same pairing on a write
// ([config.Company.validateSandboxCodingAgents]) and both say one sentence
// ([config.CodingAgentMismatch]).
//
// A seat with no resolvable model is not an error here: a company with no
// models takes no turn, so launches no run (see nomodels.go), and the run
// carries the seat's own environment and nothing else. Neither is a provider
// that is not a CLI, which only a test's fake is: it names no model a coding
// agent could read, so the run carries the seat's environment alone.
func newCodingRun(c *Company, seat *org.Role, ph phase.Phase, runner string,
	seatEnv map[string]string) (codingRun, error) {
	run := codingRun{env: seatEnv}
	if c == nil || c.Models == nil {
		return run, nil
	}
	member, err := c.Models.Head(seat, ph)
	if err != nil {
		log.Warn("sandbox_llm_unresolved", "seat", seat.Handle(),
			"phase", ph.String(), "error", err)
		return run, nil
	}
	agent, isCLI := member.Provider.(*cliagent.Provider)
	if !isCLI {
		return run, nil
	}
	if agent.Agent() != runner {
		return codingRun{}, &SandboxModelError{msg: fmt.Sprintf("seat %q: %s",
			seat.Handle(), config.CodingAgentMismatch(runner, member.Key, agent.Agent()))}
	}
	return entryRun(agent, runner, seatEnv), nil
}

// entryRun is the run an entry's CLI works under in a box: its model, and its
// sign-in beneath what the caller's own environment brings. ONE CONSTRUCTION
// for a seat's coding run and the assistant's deep work alike, so a box signs
// in the same way whoever launched it.
func entryRun(agent *cliagent.Provider, runner string, seatEnv map[string]string) codingRun {
	run := codingRun{env: seatEnv}
	// THE MODEL AS THE ENTRY'S TEXT CALLS PASS IT, AND THE CLI IT IS WRITTEN
	// FOR. An entry's model is in that CLI's own grammar — OpenCode reads
	// `<provider>/<model>`, Claude Code an alias — so the run is handed the
	// exact string every text call on the entry gives the CLI, resolved and
	// rendered as they render it.
	//
	// No base URL: an entry has none of its own — the CLI talks to its
	// vendor, and declaring a custom provider for it would point a coding
	// agent at an endpoint nothing is serving.
	run.model = agent.ModelArgument()
	run.env = underlay(seatEnv, entrySignIn(agent, runner, seatEnv))
	run.files = agent.SandboxCredentials()
	// A LOGIN THE CLI KEEPS IN ITS OWN DATABASE travels as the document its
	// export printed, mapped where the runner imports it from — the runner's
	// path, not the profile's, because the runner is what reads it
	// ([codingagent.Paths.CredentialDocument]).
	if doc := agent.SandboxCredentialDocument(); doc != "" {
		if run.files == nil {
			run.files = map[string]string{}
		}
		run.files[codingagent.CredentialDocument] = doc
	}
	return run
}

// entrySignIn is the sign-in an entry contributes to a run's environment,
// beneath what the seat brought: its headless token, its API key, or the
// provider key its env names ([cliagent.Provider.SandboxEnv]).
//
// UNDERLAID BY NAME, so a variable the seat names in role.sandbox.env wins.
// For OpenCode that is the whole rule: it reads one key per model provider,
// and the provider the run's model names reads the one variable it reads.
//
// ONE UNIT FOR CLAUDE CODE: when the seat's own environment already signs
// Claude Code in ([codingagent.ClaudeCodeCredentialEnv]), the entry adds
// nothing. Claude Code has several sign-ins under different names and ranks
// them — a cloud toggle routes away from the Anthropic API altogether, and
// ANTHROPIC_API_KEY outranks a plan's CLAUDE_CODE_OAUTH_TOKEN — so underlaying
// by name cannot see that a seat is already signed in. An api-key entry's key
// underlaid beside a seat's own plan token silently moved that seat onto the
// company's metered key, and beside a Bedrock toggle it rode into a box that
// never reads it.
func entrySignIn(agent *cliagent.Provider, runner string, seatEnv map[string]string) map[string]string {
	if runner == codingagent.ClaudeCodeName && signsIn(seatEnv, codingagent.ClaudeCodeCredentialEnv) {
		return nil
	}
	return agent.SandboxEnv()
}

// signsIn reports whether env carries a non-blank value under any of names.
// Blank does not count: a value written blank authenticates nothing. (An
// unresolved `${VAR}` never arrives as one: the run environment leaves its
// key out, or the run is not started ([Engine.runEnvironment]).)
func signsIn(env map[string]string, names []string) bool {
	for _, name := range names {
		if strings.TrimSpace(env[name]) != "" {
			return true
		}
	}
	return false
}

// SandboxModelError reports a coding run whose coding agent cannot use the
// model and sign-in its providers.llm entry gives it, refused before the box
// is minted.
//
// Its own type for the reason [SandboxCredentialError] is one: the remedy is a
// config edit — another entry, another coding agent — and never a retry, since
// nothing about the vendor being up changes what a runner can read.
type SandboxModelError struct{ msg string }

func (e *SandboxModelError) Error() string { return e.msg }

// SandboxCredentialError reports a coding run whose box could never
// authenticate, refused before the box is minted.
//
// A distinct type because the remedies (a token that travels, an api-key
// entry's key, a provider key that travels, a variable in the engine's own
// environment, a local cell) are different config edits and an operator has
// to be told which one is theirs ([credentialRefusal]) — and because this is the one launch failure
// that is a CONFIGURATION mistake rather than a provider outage, so it must
// not be retried as if the vendor were down.
type SandboxCredentialError struct{ msg string }

func (e *SandboxCredentialError) Error() string { return e.msg }

// sandboxCredentials refuses a run whose coding agent has nothing to
// authenticate with inside its box.
//
// THE FAILURE THIS CLOSES IS SILENT AND EXPENSIVE. A subscription CLI that
// mints no headless token (Codex, Gemini CLI) authenticates from credential
// FILES, and those deliberately never leave the engine host: they carry a
// refresh token whose rotation is shared fleet state. So a seat on such a
// provider running in a remote cell provisions a box, installs the agent,
// applies every setup step, starts the job — and the agent fails at its first
// model call with the vendor's own "not authenticated", minutes in, naming
// nothing an operator could act on. The documented error existed in the docs
// and in no code at all.
//
// IT ASKS THE PROFILE WHICH NAMES COUNT rather than recognising a credential
// by inspection, because the engine names no tool-specific variable of its own
// and an operator legitimately declares their own key in role.sandbox.env.
// That value is in the merged run environment this is handed, so a seat that
// brought its own credential passes — which is why the check can be a refusal
// rather than a warning.
//
// THE PHASE IS THE CALLER'S, and it is the same phase the caller resolved the
// run's model with — see [runLLM]. A run_sandbox launch runs on llm_sandbox
// and a box-mode run on the executor's own entry, and a guard that always
// asked llm_sandbox inspected the wrong provider for every box-mode seat
// that set both: it waved through a remote run whose CLI had no token, and
// refused one whose executor entry would have minted one.
func sandboxCredentials(c *Company, seat *org.Role, ph phase.Phase, placement sandbox.Placement, env map[string]string) error {
	if placement.OnEngineHost() {
		// A local box seeds the credential files and writes a refreshed
		// one back, so files alone are a complete answer there.
		return nil
	}
	if c == nil || c.Models == nil {
		return nil
	}
	member, err := c.Models.Head(seat, ph)
	if err != nil {
		//nolint:nilerr // Deliberate: a seat with no resolvable model for
		// this phase is not this guard's question. A company with no
		// models takes no turn and so launches no run, and [runLLM],
		// which resolved the same seat and phase to pick the run's model
		// moments ago, has already logged it. Returning the error here
		// would refuse a run over a question this guard does not ask.
		return nil
	}
	agent, isCLI := member.Provider.(*cliagent.Provider)
	if !isCLI {
		// Only a test's fake is not a CLI: it signs nothing in, so there
		// is no host-bound half to strand.
		return nil
	}
	if entrySignsIn(agent, env) {
		return nil
	}
	return credentialRefusal(agent, member.Key, seat.Handle(), placement)
}

// entrySignsIn reports whether env signs agent's CLI in, in a box that is not on
// the engine host: the rule a seat's remote run and the assistant's deep work
// are both held to before a box is started.
func entrySignsIn(agent *cliagent.Provider, env map[string]string) bool {
	if signsIn(env, agent.CredentialEnvNames()) {
		return true
	}
	// CLAUDE CODE'S OTHER SIGN-INS COUNT TOO — a bearer token, a Bedrock,
	// Vertex AI or Foundry toggle — because a seat that brought one is handed
	// none of the entry's ([entrySignIn]). Judged by the profile's two names
	// alone, that seat's remote run was refused as signed in to nothing when
	// it was signed in to its own cloud.
	if agent.Agent() == codingagent.ClaudeCodeName && signsIn(env, codingagent.ClaudeCodeCredentialEnv) {
		return true
	}
	// A CLI THAT READS ITS PROVIDER'S KEY FROM ITS OWN ENVIRONMENT has no one
	// variable to ask about: which one it reads depends on the provider the
	// entry's model names, and an operator may give it in role.sandbox.env
	// rather than env. So any credential-named variable the run carries
	// passes. That is looser than the question — a code-host token the seat
	// declared for git passes too — and deliberately so: this guard refuses,
	// and a refusal of a run whose key it could not recognise would block a
	// seat that works, which is worse than the vendor's own "not
	// authenticated" it exists to pre-empt.
	//
	// IT JUDGES THE RIGHT CLI, and the key it lets through is one the run can
	// read: the box is handed the entry's model exactly as its text calls
	// name it, so its provider is the one the operator keyed for those calls,
	// and a run whose runner is another CLI never gets here ([newCodingRun]).
	// Narrowing it to the variable the model's provider segment "should" use
	// would need a table of that CLI's providers the engine does not ship and
	// would be wrong anyway: OpenCode's `zai` reads ZHIPU_API_KEY,
	// `moonshotai` MOONSHOT_API_KEY, `amazon-bedrock` the AWS pair.
	if agent.SignsInThroughEnv() {
		for name, value := range env {
			if cliprofile.IsCredentialName(name) && strings.TrimSpace(value) != "" {
				return true
			}
		}
	}
	return false
}

// credentialRefusal is the [SandboxCredentialError] for a remote run nothing
// in whose environment signs its CLI in, carrying the remedy for THIS entry.
//
// THE REMEDY FOLLOWS THE AUTH MODE BEFORE THE PROFILE, because the mode decides
// what the entry reads at all ([cliagent.Provider.ModeSignIn]). Chosen by the
// profile alone, an api-key Claude Code entry whose key resolved to nothing was
// told to mint a subscription token: `-capture-token` stored one the entry
// never reads (api-key gives the token no fallback, and the child has the token
// variable removed), and the next launch was refused with the same advice. An
// inherit-env entry reads the engine's process environment and never the secret
// store the token lands in; its credential files are seeded as under every
// mode, but those never leave the engine host. So:
//
//   - api-key: the key, worded once in cliagent for every message that names
//     it, or the seat's own in role.sandbox.env;
//   - inherit-env: one of the variables it forwards, in the engine's
//     environment or the seat's role.sandbox.env, or a local cell, which is
//     seeded with the entry's login;
//   - subscription: a token that travels where the CLI mints one, the
//     provider's key where the CLI reads it from its environment, and
//     otherwise a local cell, which is seeded with the login itself.
//
// A variable the profile names outside the reference grammar is never offered
// ([cliagent.ModeSignIn]'s Misnamed): no secret store, ${VAR}, export or token
// mint can carry a credential under it, so where it is the route the mode or
// the CLI would take, the remedy names the overrides field that names it —
// beside every route the entry still has, and alone only where it has none.
//
// Its own function rather than the tail of [sandboxCredentials] so that the
// guard's VERDICT — which runs are refused — reads apart from its wording.
func credentialRefusal(agent *cliagent.Provider, key, seat string, placement sandbox.Placement) *SandboxCredentialError {
	localCell := fmt.Sprintf("role.sandbox.run_in: %q or %q", sandbox.Direct, sandbox.Container)
	if sign, fromEnv := agent.ModeSignIn(); fromEnv {
		lead := fmt.Sprintf("seat %q runs code in %q, and nothing in this run's environment "+
			"authenticates %q: %s", seat, placement, key, sign.Missing())
		switch {
		case len(sign.Variables) == 0 && len(sign.Misnamed) == 0:
			// The mode hands the CLI nothing, so the remedy is the mode
			// — and the login it names reaches a box only in a local cell.
			return &SandboxCredentialError{msg: fmt.Sprintf(
				"%s; that login reaches this seat's box only in a local cell (%s)", lead, localCell)}
		case len(sign.Variables) == 0 && sign.Mode == cliagent.AuthAPIKey:
			// Its one variable is outside the grammar, and the lead names
			// that field as the remedy. Not role.sandbox.env: every write
			// refuses the entry until the field changes, and a name
			// carrying `=` sets a different variable wherever it is placed.
			return &SandboxCredentialError{msg: lead}
		case len(sign.Variables) == 0:
			// inherit-env whose every variable is such a name: the login
			// it seeds is the one sign-in left, and only a local cell has it.
			return &SandboxCredentialError{msg: fmt.Sprintf(
				"%s; or give this seat a local cell (%s), which is seeded with the entry's login",
				lead, localCell)}
		case sign.Mode == cliagent.AuthAPIKey:
			return &SandboxCredentialError{msg: fmt.Sprintf(
				"%s; or give this seat its own %s in role.sandbox.env", lead, sign.Variables[0])}
		default:
			return &SandboxCredentialError{msg: fmt.Sprintf(
				"%s; or set %s in this seat's role.sandbox.env, or give this seat a local cell "+
					"(%s), which is seeded with the entry's login",
				lead, strings.Join(sign.Variables, " or "), localCell)}
		}
	}
	// A token variable no store can hold: `-capture-token` refuses to mint
	// for it, so it is named as the field to change rather than offered.
	tokenVar := agent.Profile().TokenEnv
	misnamedToken := ""
	if tokenVar != "" && !agent.TokenCanBeStored() {
		misnamedToken = fmt.Sprintf("providers.llm.%s.overrides.token_env is %q, which is not "+
			"an environment variable name, so no secret store, ${VAR} or shell export can "+
			"carry one and `crewlet llm login -capture-token` refuses to mint it", key, tokenVar)
	}
	remedy := "Give this seat a local cell — " + localCell
	switch {
	case agent.MintsHeadlessToken():
		remedy = fmt.Sprintf("Mint a token that travels with `crewlet llm login %s "+
			"-capture-token`, or give this seat a local cell (%s)", key, localCell)
	case agent.SignsInThroughEnv():
		// This CLI's key travels the way a token does, so the one-line
		// fix is the key rather than a placement change — named by the
		// provider segment of the very value the box is handed, the one
		// fact here that says which provider it is. The VARIABLE stays
		// the CLI's to name (see [sandboxCredentials]), so it is not
		// guessed at.
		provider := "the provider its model names"
		if name, _, found := strings.Cut(agent.ModelArgument(), "/"); found && name != "" {
			provider = fmt.Sprintf("%q, the provider its model names", name)
		}
		remedy = fmt.Sprintf("Set the key of %s, under the variable %s reads for "+
			"that provider, in providers.llm.%s.env or in this seat's "+
			"role.sandbox.env — both are exported into the box — or give this seat a "+
			"local cell (%s)",
			provider, agent.Agent(), key, localCell)
		// The key travels whatever token_env says, so it stays the remedy,
		// and the field the doctor reports is named beside it.
		if misnamedToken != "" {
			remedy += ". No headless token can travel instead: " + misnamedToken +
				"; name the variable your build of the CLI reads there"
		}
	case misnamedToken != "":
		remedy = fmt.Sprintf("No headless token can travel: %s. Name the variable your build "+
			"of the CLI reads there, or give this seat a local cell (%s)", misnamedToken, localCell)
	}
	return &SandboxCredentialError{msg: fmt.Sprintf(
		"seat %q runs code in %q, where the %q subscription login cannot follow it: "+
			"the login stays on the engine host because it carries a refresh token whose "+
			"rotation is fleet state, and nothing in this run's environment "+
			"authenticates. %s",
		seat, placement, key, remedy)}
}

// underlay adds defaults to env WITHOUT overwriting what is already there.
//
// The direction is the decision: an operator who named a variable in
// role.sandbox.env meant that value, and the engine silently replacing it
// with a resolved subscription token would override a deliberate choice —
// including the deliberate choice to point one seat's coding runs at a
// different account.
func underlay(env, defaults map[string]string) map[string]string {
	if env == nil {
		env = map[string]string{}
	}
	for name, value := range defaults {
		if _, declared := env[name]; !declared {
			env[name] = value
		}
	}
	return env
}
