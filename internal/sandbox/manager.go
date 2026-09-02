package sandbox

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"
)

// DefaultBoxTimeout is a box's TTL and keepalive window.
//
// NOT a run-time limit: a coding job is never force-stopped on a timer, and
// the waiter refreshes a running box's TTL to this value every tick so the
// clock never kills it. It is effectively the ORPHAN-RECLAIM GRACE — how long
// a box outlives an engine that stops heart-beating before the provider
// reclaims it. 900s is 60 waiter ticks: ample slack for a node that pauses for
// GC, a deploy, or a slow reconcile, and short enough that a crashed engine's
// boxes do not linger for an operator to find.
const DefaultBoxTimeout = 900 * time.Second

// DefaultPauseTTL bounds a box PAUSED on a clarification.
//
// A paused box has no provider-side deadline — a remote provider holds the
// snapshot indefinitely and bills for it, and a local one holds RAM — and the
// keepalive deliberately does not touch it. Left alone, one unanswered
// question strands a box forever. 1800s is the wait after which a person is
// evidently not answering promptly, and the run is not lost when it expires:
// the work re-seeds from the pushed branch, which was always the durable half.
const DefaultPauseTTL = 1800 * time.Second

// DefaultCodingAgent is the runner a provider that names none resolves to.
const DefaultCodingAgent = "claude-code"

// ConfigError reports providers.sandbox being misconfigured — a placement
// with no backend behind it, or a coding agent with no registered runner.
type ConfigError struct{ msg string }

func (e *ConfigError) Error() string { return e.msg }

// ManagerOptions configures a [Manager].
type ManagerOptions struct {
	// Providers is the CATALOGUE: one backend per placement a company
	// configured. A placement absent from this map is one no seat may run
	// in, which is refused where the seat is resolved rather than where
	// the box is created — a turn that has already spent its rounds is the
	// wrong place to learn the config was incomplete.
	Providers map[Placement]Provider

	// DefaultPlacement is where a seat that names none runs. It MUST be a
	// key of Providers: a default naming a backend nobody configured is
	// the failure this whole reshape exists to make impossible, and it is
	// checked here rather than at the first coding run.
	DefaultPlacement Placement

	Runners map[string]Runner

	// DefaultCodingAgent is the effective agent when a role leaves
	// role.sandbox.coding_agent empty. Empty takes [DefaultCodingAgent].
	DefaultCodingAgent string

	// DefaultTimeout is the box TTL. Zero takes [DefaultBoxTimeout].
	DefaultTimeout time.Duration

	// DefaultPauseTTL bounds a paused box. Zero takes [DefaultPauseTTL];
	// pass a negative value to disable pausing engine-wide.
	DefaultPauseTTL time.Duration

	// DefaultMaxTurns caps the agentic rounds of a coding run on a seat
	// that names none. Zero is uncapped, which is the default: the right
	// number depends on the work a company gives its agents, and one that
	// is too low truncates a real task mid-flight.
	DefaultMaxTurns int

	// DefaultSetup are the engine-wide steps (providers.sandbox.setup)
	// applied to every box before a role's own extras. The engine ships
	// none of its own, so these two config lists are the ONLY provisioning
	// a box receives.
	DefaultSetup []SetupStep

	// Telemetry mints each run's OTel environment. Nil exports nothing
	// from inside the box, which is an ordinary configuration: the run's
	// engine-side lifecycle events and its published transcript are the
	// observability surface either way.
	Telemetry *OtelReceiver
}

// Manager is the engine-held sandbox lifecycle.
//
// Unlike the MCP bridge's per-role PERSISTENT servers, sandboxes are per-turn
// ephemeral. The manager holds the configured provider and the available
// runners, resolves an effective spec for a role, and mints and installs a box
// on demand. Teardown is the caller's — a turn that fails still reclaims its
// box, which is why Acquire hands ownership over rather than tracking it.
type Manager struct {
	providers map[Placement]Provider
	placement Placement
	runners   map[string]Runner

	codingAgent string
	timeout     time.Duration
	pauseTTL    time.Duration
	maxTurns    int
	setup       []SetupStep
	telemetry   *OtelReceiver
}

// NewManager validates the options and returns the manager.
func NewManager(opts ManagerOptions) (*Manager, error) {
	if len(opts.Providers) == 0 {
		return nil, &ConfigError{msg: "providers.sandbox: no provider configured"}
	}
	for placement, provider := range opts.Providers {
		if !placement.Valid() {
			return nil, &ConfigError{msg: fmt.Sprintf(
				"providers.sandbox: %q is not a placement (have: %v)", placement, Placements)}
		}
		if provider == nil {
			return nil, &ConfigError{msg: fmt.Sprintf(
				"providers.sandbox: placement %q has no backend", placement)}
		}
	}
	if len(opts.Runners) == 0 {
		return nil, &ConfigError{msg: "providers.sandbox: no coding-agent runners registered"}
	}
	m := &Manager{
		providers:   maps.Clone(opts.Providers),
		placement:   opts.DefaultPlacement,
		runners:     maps.Clone(opts.Runners),
		codingAgent: opts.DefaultCodingAgent,
		timeout:     opts.DefaultTimeout,
		pauseTTL:    opts.DefaultPauseTTL,
		maxTurns:    max(opts.DefaultMaxTurns, 0),
		setup:       slices.Clone(opts.DefaultSetup),
		telemetry:   opts.Telemetry,
	}
	if m.codingAgent == "" {
		m.codingAgent = DefaultCodingAgent
	}
	if m.placement == "" && len(m.providers) == 1 {
		// One backend is not a choice, so nothing is being decided for the
		// operator. More than one without a default is a catalogue whose
		// EVERY CALLER NAMES ITS CELL — config validation holds that for
		// each seat and each agent-mode entry — and it is never resolved
		// by map order here: that order is random, so the company would
		// run somewhere different on every restart. A caller that names
		// none against such a catalogue is refused by [Manager.Provider]
		// at its launch, naming the field to set.
		for placement := range m.providers {
			m.placement = placement
		}
	}
	if m.placement != "" {
		if _, ok := m.providers[m.placement]; !ok {
			return nil, &ConfigError{msg: fmt.Sprintf(
				"providers.sandbox.default_run_in %q has no backend configured (have: %v)",
				m.placement, slices.Sorted(maps.Keys(m.providers)))}
		}
	}
	if m.timeout == 0 {
		m.timeout = DefaultBoxTimeout
	}
	if m.pauseTTL == 0 {
		m.pauseTTL = DefaultPauseTTL
	}
	// Checked here rather than at the first turn: a default naming a runner
	// nobody registered fails only once an agent tries to do code work, which
	// is hours after the config was applied.
	if _, err := m.RunnerFor(m.codingAgent); err != nil {
		return nil, err
	}
	return m, nil
}

// Provider is the backend for one placement, for reconnect-by-id and for the
// pause reaper's kill.
//
// (Provider, error) RATHER THAN (Provider, bool), because the caller reaches
// it holding a run row that names a placement, and the three answers there are
// "this backend", "this build has no such placement" and "this company did not
// configure it" — the last two are an operator's problem and must reach a log
// saying which, not a nil the caller turns into "the box is gone".
func (m *Manager) Provider(placement Placement) (Provider, error) {
	if placement == "" {
		placement = m.placement
	}
	if placement == "" {
		// A caller that named no cell against a catalogue with no
		// default. Validation refuses this company, so reaching it means
		// a row or a spec built by hand — and the honest answer names
		// both fields that would have settled it, not "no backend for
		// the empty string".
		return nil, &ConfigError{msg: fmt.Sprintf(
			"no cell named for this run and providers.sandbox names no "+
				"default_run_in (configured: %v): set one, or name the cell "+
				"on the seat's role.sandbox.run_in or the entry's cli.run_in",
			slices.Sorted(maps.Keys(m.providers)))}
	}
	provider, ok := m.providers[placement]
	if !ok {
		return nil, &ConfigError{msg: fmt.Sprintf(
			"no sandbox backend configured for placement %q (have: %v)",
			placement, slices.Sorted(maps.Keys(m.providers)))}
	}
	return provider, nil
}

// DefaultPlacement is where a seat that names none runs. Empty when the
// catalogue names no default and holds more than one cell — a company whose
// every seat names its own — and then [Manager.Provider] refuses a run that
// names none rather than picking one.
func (m *Manager) DefaultPlacement() Placement { return m.placement }

// Placements are the cells this company configured, for the operator surface.
func (m *Manager) Placements() []Placement { return slices.Sorted(maps.Keys(m.providers)) }

// DefaultCodingAgent is the effective agent for a role that names none.
func (m *Manager) DefaultCodingAgent() string { return m.codingAgent }

// DefaultSetup are the engine-wide setup steps.
func (m *Manager) DefaultSetup() []SetupStep { return slices.Clone(m.setup) }

// BoxTimeout is the TTL the waiter refreshes a running box to on every tick.
func (m *Manager) BoxTimeout() time.Duration { return m.timeout }

// RunnerFor returns the runner for a coding agent.
func (m *Manager) RunnerFor(codingAgent string) (Runner, error) {
	runner, ok := m.runners[codingAgent]
	if !ok {
		return nil, &ConfigError{msg: fmt.Sprintf(
			"no coding-agent runner registered for %q (have: %v)",
			codingAgent, slices.Sorted(maps.Keys(m.runners)))}
	}
	return runner, nil
}

// SpecInput are the per-role values overlaid onto the provider defaults.
//
// A pointer PauseTTL rather than a sentinel duration: nil means "inherit", and
// an explicit zero means "never pause, always re-seed from git" — two
// genuinely different instructions that a single zero value cannot carry.
type SpecInput struct {
	// Placement is the seat's run_in. Empty takes the provider default.
	Placement Placement

	CodingAgent string
	Timeout     time.Duration
	PauseTTL    *time.Duration

	// MaxTurns is the seat's round cap. Nil inherits the provider default;
	// an explicit 0 is uncapped, which is how a seat escapes a company-wide
	// cap — the same three states, and the same reason for a pointer, as
	// PauseTTL above.
	MaxTurns *int

	Env             map[string]string
	CredentialFiles map[string]string
}

// BuildSpec overlays per-role inputs onto the provider defaults.
func (m *Manager) BuildSpec(in SpecInput) Spec {
	spec := Spec{
		Placement:       in.Placement,
		CodingAgent:     in.CodingAgent,
		TimeoutSec:      m.timeout.Seconds(),
		PauseTTLSec:     m.pauseTTL.Seconds(),
		MaxTurns:        m.maxTurns,
		Env:             maps.Clone(in.Env),
		CredentialFiles: maps.Clone(in.CredentialFiles),
	}
	if spec.Placement == "" {
		spec.Placement = m.placement
	}
	if spec.CodingAgent == "" {
		spec.CodingAgent = m.codingAgent
	}
	if in.Timeout > 0 {
		spec.TimeoutSec = in.Timeout.Seconds()
	}
	if in.PauseTTL != nil {
		spec.PauseTTLSec = in.PauseTTL.Seconds()
	}
	if in.MaxTurns != nil {
		spec.MaxTurns = *in.MaxTurns
	}
	if spec.MaxTurns < 0 {
		spec.MaxTurns = 0
	}
	if spec.PauseTTLSec < 0 {
		spec.PauseTTLSec = 0
	}
	return spec
}

// Acquire provisions a box for spec, installs the coding agent, and applies
// the launch's setup steps.
//
// On install OR setup failure the box is torn down before the error
// propagates: a half-provisioned box must never reach a run whose brief
// promises the full environment. Teardown uses a context of its own, because
// the failure that got us here is often the caller's context expiring — and a
// close that skips because the context is already dead leaks the box.
func (m *Manager) Acquire(ctx context.Context, spec Spec, setup []SetupStep) (Sandbox, Runner, error) {
	runner, err := m.RunnerFor(spec.CodingAgent)
	if err != nil {
		return nil, nil, err
	}
	provider, err := m.Provider(spec.Placement)
	if err != nil {
		return nil, nil, err
	}
	box, err := provider.Create(ctx, spec)
	if err != nil {
		return nil, nil, err
	}
	if err := runner.Install(ctx, box); err != nil {
		log.ErrorContext(ctx, "sandbox_install_failed", "sandbox_id", box.ID(), "error", err.Error())
		m.discard(ctx, box)
		return nil, nil, err
	}
	// Setup commands run WITH the run env so recipes can reference the
	// engine's identity facts and their own configured tokens at
	// provisioning time.
	if err := ApplySetup(ctx, box, setup, spec.Env); err != nil {
		// Logged distinctly from an install failure so the operator debugs
		// the right subsystem: this one is their config.
		log.ErrorContext(ctx, "sandbox_setup_failed", "sandbox_id", box.ID(), "error", err.Error())
		m.discard(ctx, box)
		return nil, nil, err
	}
	names := make([]string, 0, len(setup))
	for _, step := range setup {
		names = append(names, step.Name)
	}
	log.DebugContext(ctx, "sandbox_acquired",
		"sandbox_id", box.ID(), "placement", string(spec.Placement),
		"coding_agent", spec.CodingAgent, "setup_steps", names)
	return box, runner, nil
}

// discardGrace bounds the teardown of a box that failed to provision. Short:
// the box is already unusable, and the caller is holding a turn open.
const discardGrace = 30 * time.Second

func (m *Manager) discard(ctx context.Context, box Sandbox) {
	// WithoutCancel(ctx), not Background(): a teardown must survive the
	// cancellation it is undoing — often the very one that failed the
	// provision — but it should keep the caller's VALUES, so the warning
	// below still names the turn that was provisioning this box.
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), discardGrace)
	defer cancel()
	if err := box.Close(closeCtx); err != nil {
		log.WarnContext(ctx, "sandbox_discard_failed", "sandbox_id", box.ID(), "error", err.Error())
	}
}

// Reconnect reattaches to an existing (possibly paused) box for reuse.
//
// Used when a follow-up run_sandbox call in a resumed Execute loop reuses the
// same box and checkout: Connect auto-resumes a paused box. No re-Install and
// no setup re-apply — the box already carries the ask shim, the work dir, and
// the launch's provisioning.
func (m *Manager) Reconnect(ctx context.Context, placement Placement, sandboxID, codingAgent string) (Sandbox, Runner, error) {
	runner, err := m.RunnerFor(codingAgent)
	if err != nil {
		return nil, nil, err
	}
	provider, err := m.Provider(placement)
	if err != nil {
		return nil, nil, err
	}
	box, err := provider.Connect(ctx, sandboxID)
	if err != nil {
		return nil, nil, err
	}
	log.DebugContext(ctx, "sandbox_reconnected", "sandbox_id", sandboxID,
		"placement", string(placement), "coding_agent", codingAgent)
	return box, runner, nil
}
