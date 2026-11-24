package sandbox

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/words"
)

// DefaultHostGrace is how long the node holding a run's box may be absent from
// the roster, or its store silent, before the run is carried on without that
// box (recorded lost, or its parked box released); and how long a box that
// records neither a run nor a check, its job gone, sits untouched before the
// orphan reaper on its node's box root may take it ([LocalOptions.HostGrace]).
//
// NOT a run-time or provisioning limit, nor a timer on any box: a job is never
// stopped on a clock. 900s outlasts a node's restart or rolling deploy (drain,
// boot, broker catch-up), so its runs are kept; it is the longest a seat's
// round waits on a node gone for good; and it is 60 of the waiter's stamp
// touches ([DefaultPollInterval]).
const DefaultHostGrace = 900 * time.Second

// provisionRefresh is how often a box being provisioned has its stamp touched
// ([keepProvisioning]): the waiter's cadence, so it is kept alive as a running
// box is, and a failed touch leaves fifty-nine more inside [DefaultHostGrace].
const provisionRefresh = DefaultPollInterval

// DefaultPauseTTL bounds a box PAUSED on a clarification.
//
// A paused box holds its directory on its launching node's box root and its
// RAM (a stopped process tree, a paused container), and the keepalive skips
// it, so without this one unanswered question strands it forever. 1800s is
// when a person is evidently not answering promptly; expiry loses nothing,
// since the work re-seeds from the pushed branch.
const DefaultPauseTTL = 1800 * time.Second

// ConfigError reports providers.sandbox being misconfigured — a placement
// with no backend behind it, or a harness with no registered runner.
type ConfigError struct{ msg string }

func (e *ConfigError) Error() string { return e.msg }

// why classes it as what it is: a catalogue a person writes.
func (e *ConfigError) why() (Why, bool) { return WhyConfiguration, true }

// ManagerOptions configures a [Manager].
type ManagerOptions struct {
	// Providers is the CATALOGUE: one backend per placement a company
	// configured. A placement absent from it is refused where the seat is
	// resolved, not where the box is created, after a turn spent its rounds.
	Providers map[Placement]Provider

	// DefaultPlacement is where a seat that names none runs. It MUST be a
	// key of Providers, checked here rather than at the first launch.
	DefaultPlacement Placement

	// Runners are the harnesses a box can run, by harness name. A launch
	// names its own (its executor-chain member's), so none is a default.
	Runners map[string]Runner

	// HostGrace is [Manager.HostGrace]. Zero takes [DefaultHostGrace].
	HostGrace time.Duration

	// DefaultPauseTTL bounds a paused box. Nil takes [DefaultPauseTTL];
	// an explicit zero disables pausing engine-wide: a pointer for the same
	// reason [SpecInput.PauseTTL] is one.
	DefaultPauseTTL *time.Duration

	// DefaultMaxTurns caps the agentic rounds of a run on a seat that names
	// none: providers.sandbox.default_max_turns, zero for uncapped as in the
	// config. Passed through, nothing filled in.
	DefaultMaxTurns int

	// AttributeCommits is whether every run's own commits credit Crewlet
	// (providers.sandbox.commit_attribution; see [Manager.CommitAttribution]).
	AttributeCommits bool

	// DefaultSetup are the engine-wide steps (providers.sandbox.setup)
	// applied to every box before a role's own extras. The engine ships
	// none, so these two config lists are a box's ONLY provisioning.
	DefaultSetup []SetupStep

	// Telemetry mints each run's OTel environment. Nil exports nothing
	// from inside the box, an ordinary configuration: the run's lifecycle
	// events and transcript remain the observability surface.
	Telemetry *OtelReceiver
}

// Manager is the engine-held sandbox lifecycle.
//
// Sandboxes are per-turn ephemeral, unlike the MCP bridge's persistent
// servers. Teardown is the caller's (a failed turn still reclaims its box),
// which is why Acquire hands ownership over rather than tracking it.
type Manager struct {
	providers map[Placement]Provider
	placement Placement
	runners   map[string]Runner

	// retired are the cells an EARLIER catalogue configured and this one
	// does not, with that catalogue's backend: reached only by boxes already
	// on them ([Manager.Provider], [Manager.carrying]), never provisioned
	// through.
	retired map[Placement]Provider

	hostGrace time.Duration
	pauseTTL  time.Duration
	maxTurns  int
	setup     []SetupStep
	telemetry *OtelReceiver

	// attributes is [ManagerOptions.AttributeCommits].
	attributes bool
}

// NewManager validates the options and returns the manager.
func NewManager(opts ManagerOptions) (*Manager, error) {
	if len(opts.Providers) == 0 {
		return nil, &ConfigError{msg: "providers.sandbox: no provider configured"}
	}
	for placement, provider := range opts.Providers {
		if !placement.Valid() {
			return nil, &ConfigError{msg: fmt.Sprintf(
				"providers.sandbox: %q is not a placement (have: %s)", placement, words.And(Placements))}
		}
		if provider == nil {
			return nil, &ConfigError{msg: fmt.Sprintf(
				"providers.sandbox: placement %q has no backend", placement)}
		}
	}
	if len(opts.Runners) == 0 {
		return nil, &ConfigError{msg: "providers.sandbox: no harness runners registered"}
	}
	m := &Manager{
		providers: maps.Clone(opts.Providers),
		placement: opts.DefaultPlacement,
		runners:   maps.Clone(opts.Runners),
		hostGrace: opts.HostGrace,
		maxTurns:  max(opts.DefaultMaxTurns, 0),
		setup:     slices.Clone(opts.DefaultSetup),
		telemetry: opts.Telemetry,

		attributes: opts.AttributeCommits,
	}
	if m.placement == "" && len(m.providers) == 1 {
		// One backend is not a choice. More than one without a default is a
		// catalogue whose EVERY CALLER NAMES ITS CELL (config validation
		// holds that), never resolved by map order, which is random across
		// restarts: [Manager.Provider] refuses a caller naming none, naming
		// the field to set.
		for placement := range m.providers {
			m.placement = placement
		}
	}
	if m.placement != "" {
		if _, ok := m.providers[m.placement]; !ok {
			return nil, &ConfigError{msg: fmt.Sprintf(
				"providers.sandbox.default_run_in %q has no backend configured (have: %s)",
				m.placement, words.And(slices.Sorted(maps.Keys(m.providers))))}
		}
	}
	if m.hostGrace == 0 {
		m.hostGrace = DefaultHostGrace
	}
	// NIL ALONE TAKES THE DEFAULT: an explicit zero is "never pause", and
	// read as unset it holds paused boxes a company asked not to keep.
	if opts.DefaultPauseTTL != nil {
		m.pauseTTL = *opts.DefaultPauseTTL
	} else {
		m.pauseTTL = DefaultPauseTTL
	}
	return m, nil
}

// Provider is the backend for one placement, for reconnect-by-id and for the
// pause reaper's kill.
//
// (Provider, error), not (Provider, bool): "this build has no such placement"
// and "this company did not configure it" are an operator's problem a log
// must name, not a nil the caller reads as "the box is gone".
//
// A CELL THE CATALOGUE HAS SINCE DROPPED is still answered, with its last
// backend, for this process's life ([Manager.carrying]): every caller holds a
// box that already EXISTS, and answered "not configured" a running job on it
// is settled failed with its box never reclaimed, and a paused one never
// killed. A NEW box goes through the current catalogue alone ([Manager.Acquire]).
func (m *Manager) Provider(placement Placement) (Provider, error) {
	provider, err := m.configured(placement)
	if err != nil && placement != "" {
		if retired, ok := m.retired[placement]; ok {
			return retired, nil
		}
	}
	return provider, err
}

// configured is the backend the CURRENT catalogue configured for a placement,
// the only one a new box may be provisioned through. An empty placement is
// the catalogue default, as in [Manager.BuildSpec]; only a hand-built spec or
// row arrives empty.
func (m *Manager) configured(placement Placement) (Provider, error) {
	if placement == "" {
		placement = m.placement
	}
	if placement == "" {
		// No cell against a catalogue with no default: validation refuses
		// that company, so this is a hand-built row or spec, and the error
		// names both fields that would settle it.
		return nil, &ConfigError{msg: fmt.Sprintf(
			"no cell named for this run and providers.sandbox names no "+
				"default_run_in (configured: %s): set one, or name the cell "+
				"on the executor entry's providers.llm.<key>.run_in",
			words.And(slices.Sorted(maps.Keys(m.providers))))}
	}
	provider, ok := m.providers[placement]
	if !ok {
		return nil, &ConfigError{msg: fmt.Sprintf(
			"no sandbox backend configured for placement %q (have: %s)",
			placement, words.And(slices.Sorted(maps.Keys(m.providers))))}
	}
	return provider, nil
}

// EngineHost is the name the engine host answers to inside a box of placement
// ([EngineHost]), from its backend, before any such box exists: the
// environment a box is made with is written then, and an address of the
// engine's in it must already be the one the box dials. "" where the cell's
// boxes share the host's network, or no backend serves the cell (which no box
// is made in).
func (m *Manager) EngineHost(placement Placement) EngineHost {
	provider, err := m.Provider(placement)
	if err != nil {
		return ""
	}
	if aliased, ok := provider.(engineHostAlias); ok {
		return EngineHost(aliased.EngineHost())
	}
	return ""
}

// CellOf is the cell a run's box is in: the one its row names, else (only a
// hand-built row names none) this catalogue's default, as [Manager.Provider]
// reaches it.
func (m *Manager) CellOf(run PendingRun) Placement {
	if cell := Placement(run.Placement); cell != "" {
		return cell
	}
	return m.placement
}

// runBoxLister is a backend that keeps its boxes on this node's own host and
// can list them with the run each was made for: the local one ([Local]).
type runBoxLister interface {
	RunBoxes() ([]HostBox, error)
}

// checkBoxLister is a backend that keeps its boxes on this node's own host and
// can list them with the check attempt each was made for ([Local.CheckBoxes]).
type checkBoxLister interface {
	CheckBoxes() ([]CheckBox, error)
}

// HostBoxes is every box on this node's own host that records its run,
// through every backend this manager reaches (retired cells included, since
// their boxes are still here), each once however many backends share a root.
// An error is a host that could not be listed, which says nothing of any box
// on it: the caller reclaims nothing there.
func (m *Manager) HostBoxes() ([]HostBox, error) {
	seen := map[string]bool{}
	var out []HostBox
	for _, providers := range []map[Placement]Provider{m.providers, m.retired} {
		for _, placement := range slices.Sorted(maps.Keys(providers)) {
			lister, ok := providers[placement].(runBoxLister)
			if !ok {
				continue
			}
			boxes, err := lister.RunBoxes()
			if err != nil {
				return nil, err
			}
			for _, box := range boxes {
				if !seen[box.SandboxID] {
					seen[box.SandboxID] = true
					out = append(out, box)
				}
			}
		}
	}
	return out, nil
}

// CheckBoxes is every box on this node's own host that records its check
// attempt ([Spec.Check]), as [Manager.HostBoxes] lists those recording a run:
// through every backend this manager reaches, retired cells included, each
// once. An error is a host that could not be listed.
func (m *Manager) CheckBoxes() ([]CheckBox, error) {
	seen := map[string]bool{}
	var out []CheckBox
	for _, providers := range []map[Placement]Provider{m.providers, m.retired} {
		for _, placement := range slices.Sorted(maps.Keys(providers)) {
			lister, ok := providers[placement].(checkBoxLister)
			if !ok {
				continue
			}
			boxes, err := lister.CheckBoxes()
			if err != nil {
				return nil, err
			}
			for _, box := range boxes {
				if !seen[box.SandboxID] {
					seen[box.SandboxID] = true
					out = append(out, box)
				}
			}
		}
	}
	return out, nil
}

// DefaultPlacement is where a seat that names none runs. Empty when the
// catalogue names no default and holds several cells; [Manager.Provider] then
// refuses a run that names none rather than picking one.
func (m *Manager) DefaultPlacement() Placement { return m.placement }

// Placements are the cells this company configured, for the operator surface,
// in the closed set's order ([Placements]). A retired cell is not among them:
// nothing may be provisioned there.
func (m *Manager) Placements() []Placement {
	return slices.DeleteFunc(slices.Clone(Placements), func(p Placement) bool {
		_, configured := m.providers[p]
		return !configured
	})
}

// carrying is this manager with the cells previous reached and this one does
// not configure kept as retired backends: the manager a live reload installs
// ([Coordinator.SetManager]).
//
// A RUN'S PLACEMENT IS ON ITS ROW and a revision changes only where the NEXT
// run goes, so boxes still running or paused stay reachable through the newest
// backend that configured their cell. Held for this process's life: after a
// restart, a row naming an unconfigured cell is settled failed by its poller,
// naming the cell ([Waiter]).
//
// A COPY, never a write through the receiver: a caller already holding the
// manager must not read two revisions at once.
func (m *Manager) carrying(previous *Manager) *Manager {
	if previous == nil || previous == m {
		return m
	}
	carried := *m
	carried.retired = maps.Clone(m.retired)
	if carried.retired == nil {
		carried.retired = map[Placement]Provider{}
	}
	keep := func(placement Placement, provider Provider) {
		if _, configured := m.providers[placement]; !configured {
			carried.retired[placement] = provider
		}
	}
	// THE OLDER RETIREMENTS FIRST, so where previous both retired a cell
	// and configured it again, the backend it configured wins.
	for placement, provider := range previous.retired {
		keep(placement, provider)
	}
	for placement, provider := range previous.providers {
		keep(placement, provider)
	}
	if len(carried.retired) == 0 {
		carried.retired = nil
	}
	return &carried
}

// DefaultSetup are the engine-wide setup steps.
func (m *Manager) DefaultSetup() []SetupStep { return slices.Clone(m.setup) }

// HostGrace is how long a box's node may be away or its store silent before
// the box's run is carried on without it ([DefaultHostGrace]).
func (m *Manager) HostGrace() time.Duration { return m.hostGrace }

// RunnerFor returns the runner for a harness.
func (m *Manager) RunnerFor(harness string) (Runner, error) {
	runner, ok := m.runners[harness]
	if !ok {
		return nil, &ConfigError{msg: fmt.Sprintf(
			"no harness runner registered for %q (have: %s)",
			harness, words.And(slices.Sorted(maps.Keys(m.runners))))}
	}
	return runner, nil
}

// CallIDMeta is the `_meta` key harness's runner declares its calls' ids
// under ([CallIdentifier]), which the bridge serving its job reads
// ([OpenedJob.CallIDMeta]). "" with no such runner, none declared, or a nil
// manager (no sandbox runtime yet): a call is then recorded with no id, never
// refused.
func (m *Manager) CallIDMeta(harness string) string {
	if m == nil {
		return ""
	}
	if identifier, ok := m.runners[harness].(CallIdentifier); ok {
		return identifier.CallIDMeta()
	}
	return ""
}

// MCPCallCeiling is how long harness's runner states its harness waits for
// one bridged call ([CallCeilinged]), which the bridge serving its job answers
// a call still running before ([OpenedJob.CallCeiling]). Zero with no such
// runner, none stated, or a nil manager: no call is then answered early.
func (m *Manager) MCPCallCeiling(harness string) time.Duration {
	if m == nil {
		return 0
	}
	if ceilinged, ok := m.runners[harness].(CallCeilinged); ok {
		return ceilinged.MCPCallCeiling()
	}
	return 0
}

// SpecInput are the per-role values overlaid onto the provider defaults.
//
// PauseTTL is a pointer: nil inherits, and an explicit zero means "never
// pause, always re-seed from git", which one zero value cannot also carry.
type SpecInput struct {
	// Placement is the launching executor entry's run_in. Empty takes the
	// provider default.
	Placement Placement

	// Harness is the runner the box runs: the harness of the executor-chain
	// member the launch picked. REQUIRED and passed as given, since a runner
	// apart from the entry whose login and model the run carries could read
	// neither; an empty one is refused at the launch.
	Harness  string
	PauseTTL *time.Duration

	// MaxTurns is the seat's round cap. Nil inherits the provider default;
	// an explicit 0 is uncapped, how a seat escapes a company-wide cap (a
	// pointer as PauseTTL is).
	MaxTurns *int

	Env             map[string]string
	SignInEnv       []string
	CredentialFiles map[string]string
	CredentialRules map[string]harnessprofile.FileRule
}

// CommitAttribution is the credit a run's own commits carry under this
// manager's catalogue: a (Crewlet as co-author, plus the seat, its work item
// and the run where they exist), or nil where the company turned the credit
// off, which then names nothing at all, no trailer either. Every launch path
// asks it ([RunRequest.Attribution]).
//
// ASKED OF THE MANAGER, not carried on the [Spec]: a spec is written onto the
// run's records, and a revision that turns the credit off reaches the next
// launch, not a run already started.
func (m *Manager) CommitAttribution(a CommitAttribution) *CommitAttribution {
	if !m.attributes {
		return nil
	}
	return &a
}

// BuildSpec overlays per-role inputs onto the provider defaults.
func (m *Manager) BuildSpec(in SpecInput) Spec {
	spec := Spec{
		Placement:       in.Placement,
		Harness:         in.Harness,
		PauseTTLSec:     m.pauseTTL.Seconds(),
		MaxTurns:        m.maxTurns,
		Env:             maps.Clone(in.Env),
		SignInEnv:       slices.Clone(in.SignInEnv),
		CredentialFiles: maps.Clone(in.CredentialFiles),
		CredentialRules: maps.Clone(in.CredentialRules),
	}
	if spec.Placement == "" {
		spec.Placement = m.placement
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
	// CLAMPED, not a second meaning: config refuses a negative, so only a
	// hand-built SpecInput has one, and an elapsed TTL reads "never pause".
	if spec.PauseTTLSec < 0 {
		spec.PauseTTLSec = 0
	}
	return spec
}

// Acquire provisions a box for spec, installs the harness, and applies
// the launch's setup steps.
//
// An install or setup failure tears the box down before the error returns,
// on a detached context ([Manager.discard]): a half-provisioned box must never
// reach a run whose brief promises the full environment. The box is kept
// alive until handed back or torn down ([keepProvisioning]), since the steps'
// summed timeouts are bounded by nothing the orphan reaper knows.
func (m *Manager) Acquire(ctx context.Context, spec Spec, setup []SetupStep) (Sandbox, Runner, error) {
	runner, err := m.RunnerFor(spec.Harness)
	if err != nil {
		return nil, nil, err
	}
	box, err := m.provision(ctx, spec, setup, runner)
	if err != nil {
		return nil, nil, err
	}
	return box, runner, nil
}

// AcquireBox provisions a box for spec with NO HARNESS (a check's box, running
// a repository's own command) and applies setup: [Manager.Acquire] less the
// install. It refuses, since a caller setting one meant an executor: a
// [Spec.Harness]; [Spec.CredentialFiles] (a model's login); [Spec.Repos] (a
// run's grants; its repository is its [CloneRecipe]'s); a [Spec.TurnID]; an
// empty [Spec.Check], whose box only the orphan reaper would judge, never
// while its container exists; an empty [Spec.Placement] (the default cell is a
// seat's); and [Direct], where the command would run as the engine's user.
//
// Setup is exactly setup, never [Manager.DefaultSetup]: the operator's steps
// provision coding boxes, often with credentials. The caller hands in its
// git floor and checkout ([GitVersionStep], [CloneRecipe]).
func (m *Manager) AcquireBox(ctx context.Context, spec Spec, setup []SetupStep) (Sandbox, error) {
	switch {
	case spec.Harness != "":
		return nil, fmt.Errorf("sandbox: a box with no harness names none, and this one names %q",
			spec.Harness)
	case len(spec.CredentialFiles) > 0:
		return nil, fmt.Errorf("sandbox: a box with no harness is seeded no login, and this one is "+
			"given %d credential files", len(spec.CredentialFiles))
	case len(spec.Repos) > 0:
		return nil, fmt.Errorf("sandbox: a box with no harness is no run, so it holds no run's grants, "+
			"and this one is given %d repositories: its repository is its clone step's", len(spec.Repos))
	case spec.TurnID != "":
		return nil, fmt.Errorf("sandbox: a box with no harness is no run, so it serves no turn, and this "+
			"one names turn %q", spec.TurnID)
	case spec.Check == "":
		return nil, fmt.Errorf("sandbox: a box with no harness names the check attempt it is made for, " +
			"which it records so its node's check runner can reclaim it once the attempt is over")
	case spec.Placement == "":
		return nil, fmt.Errorf("sandbox: a box with no harness names its cell: the catalogue's default " +
			"is where a seat that names none runs (providers.sandbox.default_run_in)")
	case spec.Placement == Direct:
		return nil, fmt.Errorf("sandbox: a box with no harness never runs %s: it runs a repository's own "+
			"command, which there would run as the engine's own user on the engine host, reading what the "+
			"engine reads", Direct)
	}
	return m.provision(ctx, spec, setup, nil)
}

// provision is a box for spec from the CURRENT catalogue (a retired cell
// reaches only its existing boxes), with runner's agent installed where there
// is one and setup applied WITH the spec's environment, so a recipe can read
// the engine's identity facts and its configured tokens. A failure of either
// tears the box down first ([Manager.discard]); the box is kept alive until
// both are done ([keepProvisioning]).
func (m *Manager) provision(ctx context.Context, spec Spec, setup []SetupStep, runner Runner) (Sandbox, error) {
	placement := spec.Placement
	if placement == "" {
		placement = m.placement
	}
	provider, err := m.configured(placement)
	if err != nil {
		return nil, err
	}
	box, err := provider.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	// STOPPED BEFORE ANY TEARDOWN below: a racing touch would rewrite the
	// keepalive stamp into a just-removed box directory.
	stopKeepalive := keepProvisioning(ctx, box)
	if runner != nil {
		if err := runner.Install(ctx, box); err != nil {
			stopKeepalive()
			log.ErrorContext(ctx, "sandbox_install_failed", "sandbox_id", box.ID(), "error", err.Error())
			m.discard(ctx, box)
			return nil, err
		}
	}
	if err := ApplySetup(ctx, box, setup, spec.Env); err != nil {
		stopKeepalive()
		// Apart from an install failure: this one is the operator's config,
		// or the box's git.
		log.ErrorContext(ctx, "sandbox_setup_failed", "sandbox_id", box.ID(), "error", err.Error())
		m.discard(ctx, box)
		return nil, err
	}
	stopKeepalive()
	names := make([]string, 0, len(setup))
	for _, step := range setup {
		names = append(names, step.Name)
	}
	log.DebugContext(ctx, "sandbox_acquired",
		"sandbox_id", box.ID(), "placement", string(placement),
		"harness", spec.Harness, "setup_steps", names)
	return box, nil
}

// keepProvisioning keeps box's stamp fresh while the engine provisions it:
// [Sandbox.Touch] at once and every [provisionRefresh] until the returned
// stop, which waits out a touch in flight. The caller stops it when
// provisioning ends, before any teardown.
//
// The waiter's keepalive takes over only once the row RUNS; a LAUNCHING row has
// no box while install, setup, the git check and a clone per repository
// ([MaxRunRepos]) run back to back, each to its own [DefaultSetupTimeout], a
// sum past the cutoff at which another launch's orphan sweep takes a jobless
// box whose record did not land. The first touch precedes the return: a
// reused box's stamp is whatever its last use left. A failed touch is left to
// the next; the launch's few calls after provisioning fit inside the last one.
func keepProvisioning(ctx context.Context, box Sandbox) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	refresh := func() {
		if err := box.Touch(ctx); err != nil && ctx.Err() == nil {
			log.DebugContext(ctx, "sandbox_provision_keepalive_failed",
				"sandbox_id", box.ID(), "error", err.Error())
		}
	}
	refresh()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(provisionRefresh)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// A tick that lands with the stop is the stop's: once asked
				// to stop, the box is not asked to stay.
				if ctx.Err() != nil {
					return
				}
				refresh()
			}
		}
	}()
	return sync.OnceFunc(func() {
		cancel()
		<-done
	})
}

// discardGrace bounds a cleanup that has to outlive the context that asked for
// it: tearing down a box that failed to provision, a launch that could not
// finish or a finished run, and handing back a claim whose resume broke.
// Short: what it cleans is already unusable, and the caller holds a turn or a
// delivery open meanwhile.
const discardGrace = 30 * time.Second

func (m *Manager) discard(ctx context.Context, box Sandbox) {
	// Detached, not Background(): it survives the cancellation it undoes but
	// keeps the caller's VALUES, so the warning names the provisioning turn.
	closeCtx, cancel := detached(ctx)
	defer cancel()
	if err := box.Close(closeCtx); err != nil {
		log.WarnContext(ctx, "sandbox_discard_failed", "sandbox_id", box.ID(), "error", err.Error())
	}
}

// Reconnect reattaches to an existing (possibly paused) box: Connect
// auto-resumes a paused one. No re-Install and no setup re-apply: the box
// already carries both, so it is only ever reattached for the harness it was
// installed for. A Gitlet repository is never provisioned (the run checks each
// out through `code_checkout`), so the box keeps what earlier jobs cloned.
func (m *Manager) Reconnect(ctx context.Context, placement Placement, sandboxID, harness string) (Sandbox, Runner, error) {
	runner, err := m.RunnerFor(harness)
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
		"placement", string(placement), "harness", harness)
	return box, runner, nil
}

// Attach reattaches to an existing box for a READER, without resuming it — see
// [Provider.Attach]. A paused box its backend cannot read is [ErrBoxPaused].
func (m *Manager) Attach(ctx context.Context, placement Placement, sandboxID, harness string) (Sandbox, Runner, error) {
	runner, err := m.RunnerFor(harness)
	if err != nil {
		return nil, nil, err
	}
	provider, err := m.Provider(placement)
	if err != nil {
		return nil, nil, err
	}
	box, err := provider.Attach(ctx, sandboxID)
	if err != nil {
		return nil, nil, err
	}
	return box, runner, nil
}
