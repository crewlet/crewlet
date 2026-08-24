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

// ConfigError reports providers.sandbox being misconfigured — an unknown or
// unavailable type, or a coding agent with no registered runner.
type ConfigError struct{ msg string }

func (e *ConfigError) Error() string { return e.msg }

// ManagerOptions configures a [Manager].
type ManagerOptions struct {
	Provider Provider
	Runners  map[string]Runner

	// DefaultCodingAgent is the effective agent when a role leaves
	// role.sandbox.coding_agent empty. Empty takes [DefaultCodingAgent].
	DefaultCodingAgent string

	// DefaultTemplate is providers.sandbox's template, used when a role
	// names none.
	DefaultTemplate string

	// DefaultTimeout is the box TTL. Zero takes [DefaultBoxTimeout].
	DefaultTimeout time.Duration

	// DefaultPauseTTL bounds a paused box. Zero takes [DefaultPauseTTL];
	// pass a negative value to disable pausing engine-wide.
	DefaultPauseTTL time.Duration

	// DefaultSetup are the engine-wide steps (providers.sandbox.setup)
	// applied to every box before a role's own extras. The engine ships
	// none of its own, so these two config lists are the ONLY provisioning
	// a box receives.
	DefaultSetup []SetupStep
}

// Manager is the engine-held sandbox lifecycle.
//
// Unlike the MCP bridge's per-role PERSISTENT servers, sandboxes are per-turn
// ephemeral. The manager holds the configured provider and the available
// runners, resolves an effective spec for a role, and mints and installs a box
// on demand. Teardown is the caller's — a turn that fails still reclaims its
// box, which is why Acquire hands ownership over rather than tracking it.
type Manager struct {
	provider Provider
	runners  map[string]Runner

	codingAgent string
	template    string
	timeout     time.Duration
	pauseTTL    time.Duration
	setup       []SetupStep
}

// NewManager validates the options and returns the manager.
func NewManager(opts ManagerOptions) (*Manager, error) {
	if opts.Provider == nil {
		return nil, &ConfigError{msg: "providers.sandbox: no provider configured"}
	}
	if len(opts.Runners) == 0 {
		return nil, &ConfigError{msg: "providers.sandbox: no coding-agent runners registered"}
	}
	m := &Manager{
		provider:    opts.Provider,
		runners:     maps.Clone(opts.Runners),
		codingAgent: opts.DefaultCodingAgent,
		template:    opts.DefaultTemplate,
		timeout:     opts.DefaultTimeout,
		pauseTTL:    opts.DefaultPauseTTL,
		setup:       slices.Clone(opts.DefaultSetup),
	}
	if m.codingAgent == "" {
		m.codingAgent = DefaultCodingAgent
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

// Provider is the configured provider, for reconnect-by-id and for the pause
// reaper's kill.
func (m *Manager) Provider() Provider { return m.provider }

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
	CodingAgent     string
	Template        string
	Timeout         time.Duration
	PauseTTL        *time.Duration
	Env             map[string]string
	CredentialFiles map[string]string
}

// BuildSpec overlays per-role inputs onto the provider defaults.
func (m *Manager) BuildSpec(in SpecInput) Spec {
	spec := Spec{
		CodingAgent:     in.CodingAgent,
		Template:        in.Template,
		TimeoutSec:      m.timeout.Seconds(),
		PauseTTLSec:     m.pauseTTL.Seconds(),
		Env:             maps.Clone(in.Env),
		CredentialFiles: maps.Clone(in.CredentialFiles),
	}
	if spec.CodingAgent == "" {
		spec.CodingAgent = m.codingAgent
	}
	if spec.Template == "" {
		spec.Template = m.template
	}
	if in.Timeout > 0 {
		spec.TimeoutSec = in.Timeout.Seconds()
	}
	if in.PauseTTL != nil {
		spec.PauseTTLSec = in.PauseTTL.Seconds()
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
	box, err := m.provider.Create(ctx, spec)
	if err != nil {
		return nil, nil, err
	}
	if err := runner.Install(ctx, box); err != nil {
		log.Error("sandbox_install_failed", "sandbox_id", box.ID(), "error", err.Error())
		m.discard(box)
		return nil, nil, err
	}
	// Setup commands run WITH the run env so recipes can reference the
	// engine's identity facts and their own configured tokens at
	// provisioning time.
	if err := ApplySetup(ctx, box, setup, spec.Env); err != nil {
		// Logged distinctly from an install failure so the operator debugs
		// the right subsystem: this one is their config.
		log.Error("sandbox_setup_failed", "sandbox_id", box.ID(), "error", err.Error())
		m.discard(box)
		return nil, nil, err
	}
	names := make([]string, 0, len(setup))
	for _, step := range setup {
		names = append(names, step.Name)
	}
	log.Debug("sandbox_acquired",
		"sandbox_id", box.ID(), "coding_agent", spec.CodingAgent, "setup_steps", names)
	return box, runner, nil
}

// discardGrace bounds the teardown of a box that failed to provision. Short:
// the box is already unusable, and the caller is holding a turn open.
const discardGrace = 30 * time.Second

func (m *Manager) discard(box Sandbox) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), discardGrace)
	defer cancel()
	if err := box.Close(ctx); err != nil {
		log.Warn("sandbox_discard_failed", "sandbox_id", box.ID(), "error", err.Error())
	}
}

// Reconnect reattaches to an existing (possibly paused) box for reuse.
//
// Used when a follow-up run_sandbox call in a resumed Execute loop reuses the
// same box and checkout: Connect auto-resumes a paused box. No re-Install and
// no setup re-apply — the box already carries the ask shim, the work dir, and
// the launch's provisioning.
func (m *Manager) Reconnect(ctx context.Context, sandboxID, codingAgent string) (Sandbox, Runner, error) {
	runner, err := m.RunnerFor(codingAgent)
	if err != nil {
		return nil, nil, err
	}
	box, err := m.provider.Connect(ctx, sandboxID)
	if err != nil {
		return nil, nil, err
	}
	log.Debug("sandbox_reconnected", "sandbox_id", sandboxID, "coding_agent", codingAgent)
	return box, runner, nil
}
