package config

import (
	"slices"
	"strings"
)

// Placement is WHERE a seat's code work runs.
//
// # One axis, four cells
//
// This replaces a `type:` on the provider block, and the reason is that the
// old shape could express only ONE answer for a whole company. A company has
// exactly one `providers.sandbox`, so choosing `local` for the seat that needs
// the operator's own subscription login meant choosing it for the seat whose
// work must never touch the engine host. There was no way to say "this seat
// remotely, that seat here" — and the two are different security decisions
// about different work.
//
// So the provider block becomes a CATALOGUE — configure `e2b:`, `local:`, or
// both — and the choice moves to the seat, as `role.sandbox.run_in`.
type Placement string

const (
	// PlacementDirect runs a process tree on the ENGINE HOST in a per-box
	// directory, with HOME and the XDG variables pointed at it and an
	// allowlisted environment. That isolates STATE — no box sees another's
	// checkout, memory or credentials — but NOT THE HOST: the coding agent
	// runs as the engine user, so it reads what that user reads and
	// reaches what its credentials reach. Right for a workstation or a
	// dedicated VM; wrong for a shared host.
	PlacementDirect Placement = "direct"

	// PlacementContainer runs each box in its own container on the engine
	// host, with the box directory bind-mounted at /home/user — real host
	// isolation, and the same in-box paths a remote box uses.
	PlacementContainer Placement = "container"

	// PlacementE2B runs the work in a remote box: a fresh machine per run
	// that is not the engine's, which is what makes it the right choice
	// for anything shared.
	PlacementE2B Placement = "e2b"

	// PlacementSelf runs code work INSIDE the seat's own executor run,
	// which is only possible when that executor is a coding CLI in agent
	// mode: it already holds a shell, an editor and a checkout, so
	// provisioning a second box beside it would give the seat two
	// filesystems and make the one doing the work invisible to the other.
	//
	// It is the one value that needs no backend, because it IS the
	// executor's box. A seat that names it is simply not offered
	// run_sandbox — there is nothing to launch.
	PlacementSelf Placement = "self"
)

// Placements is the closed set — every value a `run_in` may take.
//
// The three that need a BACKEND are walked against the switch that builds them
// by a test, because the list and the switch have disagreed before: `e2b` was
// once the default with no case behind it, so a company that wrote
// `providers.sandbox:` and no type validated cleanly, reported a configured
// sandbox on the dashboard, and failed at its first coding run.
var Placements = []Placement{PlacementDirect, PlacementContainer, PlacementE2B, PlacementSelf}

// NeedsBackend reports whether a cell has a provider behind it.
//
// Only `self` does not, and it is worth a method rather than an equality test
// at each site: three separate walks decide what to build, what to validate
// and what to report, and each one silently does the wrong thing for a cell
// that is the executor's own box rather than a box the engine mints.
func (p Placement) NeedsBackend() bool { return p != PlacementSelf }

// backend names the catalogue entry a placement needs, for the message a
// refusal carries: an operator who wrote `run_in: container` with no `local:`
// block needs to be told which block to add, not which value to change.
func (p Placement) backend() string {
	if p == PlacementE2B {
		return "`e2b:`"
	}
	return "`local:`"
}

// BackendPlacements are the cells a provider is built for, in closed-set
// order — every value of [Placements] but `self`.
//
// Exported because three surfaces outside this file need exactly this list and
// must not each filter it themselves: the engine's build switch, the schema
// enum for the two fields that name a BACKEND cell (a company default and an
// agent-mode runtime, neither of which can be the executor's own box), and the
// test that holds those two together.
func BackendPlacements() []Placement {
	out := make([]Placement, 0, len(Placements))
	for _, p := range Placements {
		if p.NeedsBackend() {
			out = append(out, p)
		}
	}
	return out
}

// CodingAgent is which coding CLI runs inside a box.
type CodingAgent string

// The coding agents a sandbox can run.
const (
	CodingAgentClaudeCode CodingAgent = "claude-code"
	CodingAgentOpenCode   CodingAgent = "opencode"
)

// CodingAgents is the closed set.
var CodingAgents = []CodingAgent{CodingAgentClaudeCode, CodingAgentOpenCode}

// SandboxProvider is the engine-wide code-runtime CATALOGUE.
//
// A sandbox-enabled seat runs real code work as a coding agent inside a box,
// through the run_sandbox tool. This block says WHICH BOXES EXIST; the per-seat
// gate is role.sandbox, and `role.sandbox.run_in` picks the cell. A seat with
// no role.sandbox never sees the tool at all.
//
// BOTH BACKENDS MAY BE CONFIGURED AT ONCE, which is the whole point of the
// reshape: the seat that should use the operator's own subscription login and
// the seat whose work must never touch the engine host are different seats,
// and a company-wide `type:` could only ever answer for both.
type SandboxProvider struct {
	// E2B configures the remote backend. Absent means no seat may name
	// `run_in: e2b`.
	E2B *E2BSandbox `yaml:"e2b,omitempty" json:"e2b,omitempty" desc:"The remote backend. Absent = run_in: e2b is refused."`

	// Local configures the engine host as a backend, so code work can use
	// the subscription CLI login `crewlet llm login` already established,
	// with no remote account and no API key. Absent means no seat may name
	// `run_in: direct` or `run_in: container`.
	//
	// It carries no containment of its own any more — `run_in` IS the
	// containment, per seat. A block-wide one could only say the same
	// thing for every seat, which is the limitation this reshape removes.
	Local *LocalSandbox `yaml:"local,omitempty" json:"local,omitempty" desc:"The engine host as a backend. Absent = run_in: direct|container is refused."`

	// Fake wires the in-process double: no real box, no real coding agent,
	// no real MCP. For a deployment demonstrating the flow, and named in
	// config rather than inferred so nobody runs one by accident.
	//
	// It answers EVERY placement, because it is not a placement — it is
	// the absence of one, and a double that answered only `direct` would
	// make a demo config differ from a real one in a second place.
	Fake bool `yaml:"fake,omitempty" json:"fake,omitempty" desc:"Use the in-process double for every placement. Demonstrations only."`

	// DefaultRunIn is where a sandbox-enabled seat that names none runs.
	//
	// THERE IS NO IMPLICIT DEFAULT WHENEVER MORE THAN ONE CELL IS
	// CONFIGURED, and the absence of one is the point: `direct` runs the
	// coding agent as the engine user with the engine's filesystem access
	// and `e2b` bills a remote account, so neither may be chosen for an
	// operator who did not say which they meant. `local:` alone is two
	// cells, not one — it serves both `direct` and `container` — so it
	// needs this field as much as a catalogue with both backends does.
	// Only `e2b:` alone, and the double, resolve on their own.
	//
	// REQUIRED ONLY WHERE SOMETHING WOULD READ IT. A catalogue whose every
	// sandbox-enabled seat, and every agent-mode entry a seat's executor
	// runs on, names its own cell has nothing left to default — that is
	// the shape the catalogue exists for — so the refusal is written at
	// the seat or entry that named none, never on this field alone. See
	// [Company.validateSandboxPlacement].
	DefaultRunIn Placement `yaml:"default_run_in,omitempty" json:"default_run_in,omitempty" js:"enum=direct|container|e2b" desc:"Where a sandbox-enabled seat that names none runs. Required unless the catalogue names exactly one cell, or every sandbox-enabled seat and agent-mode entry names its own run_in."`

	// DefaultCodingAgent is what a seat that names none runs.
	DefaultCodingAgent CodingAgent `yaml:"default_coding_agent,omitempty" json:"default_coding_agent,omitempty" js:"enum=claude-code|opencode" desc:"Coding agent for seats that name none."`

	// DefaultTimeoutSeconds is the box TTL and keepalive window. NOT a run
	// limit: a coding job is never force-stopped on a timer, and the
	// waiter refreshes a running box's TTL every tick so the clock never
	// kills it. Effectively the orphan-reclaim grace — how long a box
	// outlives an engine that stopped heart-beating.
	DefaultTimeoutSeconds float64 `yaml:"default_timeout_seconds,omitempty" json:"default_timeout_seconds,omitempty" js:"min=0" desc:"Box TTL/keepalive. Not a run cap — the orphan-reclaim grace."`

	// DefaultPauseTTLSeconds is how long a sandbox blocked on a human's
	// answer stays paused before it is reaped and the work re-seeds from
	// the pushed branch.
	//
	// A paused box is held indefinitely by the provider and BILLED for the
	// snapshot, so expiring it is the engine's job. 0 means never pause:
	// the box is torn down as soon as it blocks and the work always
	// re-seeds, for zero snapshot cost. A negative value is refused rather
	// than read as "no expiry" — an unbounded pause is exactly the leak
	// this knob exists to prevent, and a seat that wants the provider
	// default says so with role.sandbox.pause_ttl_seconds: -1.
	// A POINTER, because 0 is a valid SETTING here rather than an absent
	// field: unset means "take the 1800s default" and 0 means "never
	// pause". A plain float64 cannot hold both, and read as one it mapped
	// every operator who wrote 0 onto the default — silently keeping and
	// billing for the snapshots the setting exists to refuse.
	DefaultPauseTTLSeconds *float64 `yaml:"default_pause_ttl_seconds,omitempty" json:"default_pause_ttl_seconds,omitempty" js:"min=0" desc:"Paused-box TTL before reap and re-seed. Unset = 1800s; 0 = never pause."`

	// DefaultMaxTurns caps how many agentic rounds a coding run may take,
	// for seats that name none.
	//
	// THE ONLY ENGINE-SIDE BOUND ON A RUNAWAY CODING AGENT. A coding job is
	// deliberately never force-stopped on a clock — see
	// default_timeout_seconds — so nothing else stops one that is thrashing
	// rather than working, and the box's own TTL is refreshed on every
	// waiter tick precisely so that it cannot. 0 means uncapped, which is
	// the default: a cap that is too low truncates real work mid-task, and
	// the right number depends on the tasks a company gives its agents.
	DefaultMaxTurns int `yaml:"default_max_turns,omitempty" json:"default_max_turns,omitempty" js:"min=0" desc:"Agentic-round cap for coding runs on seats that name none. 0 = uncapped."`

	// Setup is the engine-wide provisioning applied to every box before
	// each seat's own extras. The engine ships NO steps of its own — git
	// auth included: the documented recipe is config, so an operator can
	// see and change every command that touches their box.
	Setup []SandboxSetupStep `yaml:"setup,omitempty" json:"setup,omitempty" desc:"Provisioning steps applied to every box before per-seat extras."`
}

// E2BSandbox is the remote backend's block.
type E2BSandbox struct {
	// APIKey authenticates the remote provider, and is REQUIRED —
	// including against a self-hosted cluster, where Domain changes which
	// API is talked to and never whether it authenticates.
	APIKey string `secret:"true" yaml:"api_key,omitempty" json:"api_key,omitempty" desc:"Remote sandbox API key; required. ${VAR} supported."`

	// Domain points at a self-hosted cluster. Empty is the vendor cloud.
	//
	// ONE FIELD IS THE WHOLE CLOUD-TO-SELF-HOSTED SWITCH: the control-plane
	// address and every box's own hostname are both derived from it, so
	// there is no second address that can disagree with the first.
	Domain string `yaml:"domain,omitempty" json:"domain,omitempty" desc:"Self-hosted sandbox cluster domain; empty = vendor cloud."`

	// Template is the box image.
	//
	// It is also HOW A BOX IS SIZED. vCPU, RAM and disk are properties of
	// the template, fixed when it is built; the create API accepts no
	// resource arguments at all. To give agents bigger boxes, build a
	// template with the resources you want and name it here — there is
	// deliberately no engine-side limits knob, because the engine could
	// not honour one.
	Template string `yaml:"template,omitempty" json:"template,omitempty" desc:"Box template. This is where vCPU/RAM/disk are set — at template build time."`
}

func (e *E2BSandbox) validate(path string) error {
	var p problems
	if strings.TrimSpace(e.APIKey) == "" {
		// REQUIRED, and checked here rather than at construction so a
		// `crewlet validate` catches it: the API authenticates every call
		// on both the cloud and a self-hosted cluster, so a run without
		// one 401s at its first create — minutes into a turn that has
		// already spent its own rounds.
		p.add(at(path, "api_key"), ErrMissing,
			"required — the API authenticates every call, including against "+
				"a self-hosted cluster, where `domain` changes which API is "+
				"talked to and not whether it authenticates")
	}
	return p.err()
}

// Sandbox defaults. The TTL is the orphan-reclaim grace and the pause TTL
// trades a bounded snapshot bill against exact conversational resume.
const (
	defaultSandboxTimeoutSeconds  = 900.0
	defaultSandboxPauseTTLSeconds = 1800.0
)

// Timeout is the box TTL, applying the default.
func (s *SandboxProvider) Timeout() float64 {
	if s.DefaultTimeoutSeconds <= 0 {
		return defaultSandboxTimeoutSeconds
	}
	return s.DefaultTimeoutSeconds
}

// PauseTTL is the paused-box TTL as configured, or nil when the field is
// absent and the engine-wide default applies.
//
// A POINTER OUT as well as in, because collapsing the two here is what the
// bug was: 0 is "never pause — tear the box down the moment it blocks and
// always re-seed from the pushed branch", and absent is "take the 1800s
// default". The old accessor mapped 0 onto the default in the same breath as
// a comment claiming it did not, so a company that asked for zero snapshot
// cost was billed for every paused box for half an hour.
//
// The caller resolves nil, so the default lives in ONE place
// ([sandbox.DefaultPauseTTL]) rather than being re-applied by every layer
// that touches the value.
func (s *SandboxProvider) PauseTTL() *float64 {
	return s.DefaultPauseTTLSeconds
}

// Enabled reports whether any seat could run code at all.
//
// A catalogue with nothing in it is not enabled: the block was written and
// configures no box, so run_sandbox is never offered.
func (s *SandboxProvider) Enabled() bool {
	if s == nil {
		return false
	}
	return s.Fake || s.E2B != nil || s.Local != nil
}

// Configured reports whether this catalogue can actually run p.
//
// The FAKE answers every placement, because it is not a backend that happens
// to sit somewhere — it is the absence of one, and a double that answered only
// `direct` would make a demonstration config differ from the real one in a
// second place nobody would think to change back.
func (s *SandboxProvider) Configured(p Placement) bool {
	if s == nil {
		return false
	}
	if !p.NeedsBackend() {
		// `self` is the executor's own box. Nothing in the catalogue
		// serves it, and nothing has to — which is why the question
		// "does this company configure it" has no bearing on it.
		return false
	}
	if s.Fake {
		return slices.Contains(Placements, p)
	}
	switch p {
	case PlacementE2B:
		return s.E2B != nil
	case PlacementDirect, PlacementContainer:
		return s.Local != nil
	}
	return false
}

// RunIn is where a sandbox-enabled seat that names no placement runs, or
// empty when the catalogue cannot answer that on its own.
//
// EMPTY IS A REAL ANSWER, not a failure to compute one: `direct` runs the
// coding agent as the engine user with the engine's own filesystem access,
// and `e2b` bills a remote account. Guessing either for an operator who wrote
// neither is the kind of default that is discovered afterwards. Validation
// turns the empty answer into a message naming both fields that would fix it.
func (s *SandboxProvider) RunIn() Placement {
	if s == nil {
		return ""
	}
	if s.DefaultRunIn != "" {
		return s.DefaultRunIn
	}
	switch {
	case s.Fake:
		// The double runs nowhere, so nothing is being chosen on the
		// operator's behalf and the cell only names itself in events.
		return PlacementDirect
	case s.E2B != nil && s.Local == nil:
		return PlacementE2B
	}
	// `local:` alone is still two answers — direct and container are
	// different security decisions about the same host — so it does not
	// resolve. Neither does a catalogue with both backends.
	return ""
}

func (s *SandboxProvider) validate(path string) error {
	var p problems
	switch {
	case !s.Enabled():
		// A block that names no backend is an unfinished edit, and it
		// validates and applies cleanly: every sandbox-enabled seat plans
		// around a box it never gets, and the only symptom is code work
		// that quietly never happens.
		p.add(path, ErrMissing,
			"a sandbox block configures nothing: give it `e2b:`, `local:`, or "+
				"`fake: true`. Remove the block entirely to leave code work off")
		return p.err()
	case s.Fake && (s.E2B != nil || s.Local != nil):
		// The double is not a third backend beside the real two — it
		// replaces every one of them, so a real block underneath it reads
		// as configuration and configures nothing.
		p.add(at(path, "fake"), ErrConflict,
			"the in-process double answers every placement, so `e2b:` and "+
				"`local:` beside it are never read. Remove one side")
		return p.err()
	}

	if s.E2B != nil {
		p.wrap(s.E2B.validate(at(path, "e2b")))
	}
	if s.Local != nil {
		p.wrap(s.Local.validate(at(path, "local")))
	}

	switch {
	case s.DefaultRunIn == "":
		// NOT REFUSED HERE, even when the catalogue is ambiguous: whether
		// a default is NEEDED is a question about the seats, and this
		// block cannot see them. A seat, or an agent-mode entry, that
		// names no cell against an ambiguous catalogue is refused where it
		// is written, with a message offering this field as the other
		// remedy — see [Company.validateSandboxPlacement]. Refused here
		// unconditionally, the remedy that message offered ("give every
		// seat its own run_in") could never pass.
	case !slices.Contains(BackendPlacements(), s.DefaultRunIn):
		// `self` is deliberately not offerable as a COMPANY default: it
		// is only meaningful for a seat whose executor is a coding CLI
		// in agent mode, and a company-wide default would silently turn
		// code work off for every seat that is not.
		p.add(at(path, "default_run_in"), ErrUnknownValue, "%q (want %s)",
			s.DefaultRunIn, names(BackendPlacements()))
	case !s.Configured(s.DefaultRunIn):
		p.add(at(path, "default_run_in"), ErrConflict,
			"%q needs the %s backend configured under providers.sandbox (this "+
				"catalogue has %s)",
			s.DefaultRunIn, s.DefaultRunIn.backend(), names(s.available()))
	}

	if s.DefaultCodingAgent != "" && !slices.Contains(CodingAgents, s.DefaultCodingAgent) {
		p.add(at(path, "default_coding_agent"), ErrUnknownValue, "%q (want %s)",
			s.DefaultCodingAgent, names(CodingAgents))
	}
	if s.DefaultTimeoutSeconds < 0 {
		p.add(at(path, "default_timeout_seconds"), ErrOutOfRange,
			"must be 0 (the %v s default) or positive, got %v",
			defaultSandboxTimeoutSeconds, s.DefaultTimeoutSeconds)
	}
	if s.DefaultMaxTurns < 0 {
		p.add(at(path, "default_max_turns"), ErrOutOfRange,
			"%d (a round cap cannot be negative; 0 means uncapped)", s.DefaultMaxTurns)
	}
	// The NEGATIVE refusal stays: an unbounded pause is the leak this knob
	// exists to prevent, and -1 is a SEAT's spelling of "inherit the
	// provider default", which is meaningless on the provider itself.
	if s.DefaultPauseTTLSeconds != nil && *s.DefaultPauseTTLSeconds < 0 {
		p.add(at(path, "default_pause_ttl_seconds"), ErrOutOfRange,
			"must not be negative: an unbounded pause is the snapshot leak "+
				"this knob exists to prevent. Use 0 to never pause, or omit "+
				"the field for the 1800s default; a SEAT asks for the "+
				"provider default with -1")
	}
	for i := range s.Setup {
		p.wrap(s.Setup[i].validate(idx(at(path, "setup"), i)))
	}
	return p.err()
}

// localBlock is the configured local backend, nil-safe on the catalogue so a
// company with no providers.sandbox at all reads as "no block" rather than
// panicking inside a cross-field rule that has to run either way.
func (s *SandboxProvider) localBlock() *LocalSandbox {
	if s == nil {
		return nil
	}
	return s.Local
}

// available is the placements this catalogue can run, in the canonical order,
// for the message a refusal carries.
func (s *SandboxProvider) available() []Placement {
	if s == nil {
		return nil
	}
	out := make([]Placement, 0, len(Placements))
	for _, p := range BackendPlacements() {
		if s.Configured(p) {
			out = append(out, p)
		}
	}
	return out
}

// ContainerRuntime is which container CLI to drive.
type ContainerRuntime string

// The container runtimes. `auto` prefers docker and falls back to podman,
// which is the rootless default on Fedora/RHEL and takes the same
// subcommands.
const (
	RuntimeAuto   ContainerRuntime = "auto"
	RuntimeDocker ContainerRuntime = "docker"
	RuntimePodman ContainerRuntime = "podman"
)

// ContainerRuntimes is the closed set.
var ContainerRuntimes = []ContainerRuntime{RuntimeAuto, RuntimeDocker, RuntimePodman}

// LocalSandbox is the local backend's block: the engine host as a code
// runtime, so code work can use the subscription login already established.
//
// IT CARRIES NO CONTAINMENT OF ITS OWN. `run_in: direct` and `run_in:
// container` are both served by this one block, per seat, because they are a
// choice about ONE SEAT'S WORK rather than about the host: the seat that needs
// the operator's own login and the seat whose generated code must not see the
// engine's filesystem are different seats on the same machine. A block-level
// mode could only ever answer for both.
type LocalSandbox struct {
	// Image is the container image, required for any seat that runs in a
	// container, with deliberately no default: a box whose image lacks the
	// coding-agent CLI fails only once an agent tries to use it.
	Image string `yaml:"image,omitempty" json:"image,omitempty" desc:"Container image with the coding-agent CLI installed. Required for run_in: container."`

	Runtime ContainerRuntime `yaml:"runtime,omitempty" json:"runtime,omitempty" js:"enum=auto|docker|podman" desc:"Container CLI to drive."`

	// StateDir is the parent directory for box directories. Boxes are
	// removed at teardown, and orphans from a crashed engine are reaped on
	// the next create. It applies to both placements — a direct box is a
	// directory under it too.
	StateDir string `yaml:"state_dir,omitempty" json:"state_dir,omitempty" desc:"Parent directory for box directories; used by both local placements."`

	// Network is the container network. Empty uses the runtime's default.
	// Setting `none` cuts the box off entirely — which also cuts off the
	// coding agent's LLM, so only do it for purely local work.
	Network string `yaml:"network,omitempty" json:"network,omitempty" desc:"Container network; setting it to none also cuts off the coding agent's LLM."`

	// RunArgs are spliced into the container run command (--cpus,
	// --memory, extra mounts, --user). This is where a container box is
	// SIZED, mirroring how a remote box is sized by its template.
	RunArgs []string `yaml:"run_args,omitempty" json:"run_args,omitempty" desc:"Extra container run arguments. This is where a local box is sized."`
}

// containerOnly are the fields no direct box reads, named as an operator
// wrote them. A value that means nothing where it is written is the silence
// this package spends most of its rules on, and the check that reports it
// needs the SEATS — see (*Company).validateSandboxPlacement.
func (l *LocalSandbox) containerOnly() []struct{ field, value string } {
	args := ""
	if len(l.RunArgs) > 0 {
		args = "set"
	}
	return []struct{ field, value string }{
		{"image", l.Image},
		{"runtime", string(l.Runtime)},
		{"network", l.Network},
		{"run_args", args},
	}
}

func (l *LocalSandbox) validate(path string) error {
	var p problems
	if l.Runtime != "" && !slices.Contains(ContainerRuntimes, l.Runtime) {
		p.add(at(path, "runtime"), ErrUnknownValue, "%q (want %s)", l.Runtime, names(ContainerRuntimes))
	}
	return p.err()
}

// SandboxSetupStep is one declarative provisioning unit applied to a fresh
// box.
//
// Every provisioning concern — git auth included — goes through this one
// apply, env-merge and brief pipeline, with no engine special cases. Steps
// come only from providers.sandbox.setup and role.sandbox.setup.
type SandboxSetupStep struct {
	// Name identifies the step in logs and in setup-failure errors.
	Name string `yaml:"name" json:"name" js:"required" desc:"Short identifier used in logs and failure messages."`

	// Files are written into the box (path -> content) before Commands
	// run. ${VAR} references here are resolved when the step is loaded.
	Files map[string]string `yaml:"files,omitempty" json:"files,omitempty" desc:"Files written into the box before commands run."`

	// Commands run in order after the files land. A non-zero exit fails
	// the whole acquisition — the coding agent's brief promises this
	// environment, so a partially provisioned box must never reach a run.
	Commands []string `yaml:"commands,omitempty" json:"commands,omitempty" desc:"Shell commands run in order; a non-zero exit fails the box."`

	// Env is merged into the coding agent's run environment.
	//
	// It stays VERBATIM here and is resolved exactly once, with the rest
	// of the sandbox env at launch. Resolving it at load as well would
	// double-resolve and silently mangle any secret whose real value
	// contains a literal ${...}.
	Env map[string]string `yaml:"env,omitempty" json:"env,omitempty" desc:"Env merged into the run; resolved once at launch, not at load."`

	// Brief is the paragraph handed to the coding agent: what this step
	// made TRUE about its box. Never resolved — it is agent-facing text,
	// and substituting engine-host environment into it would be surprising
	// at best.
	Brief string `yaml:"brief,omitempty" json:"brief,omitempty" desc:"What this step made true about the box, told to the coding agent."`

	// TimeoutSeconds is how long each of this step's commands may run.
	//
	// Provisioning is not a control-plane call. Without its own budget
	// these commands inherited the backend's control timeout — sized for a
	// mkdir, not for work — so any real step (a dependency install, a cold
	// image pull, a large clone) was killed and failed the acquisition.
	TimeoutSeconds float64 `yaml:"timeout_seconds,omitempty" json:"timeout_seconds,omitempty" js:"min=0" desc:"Per-command wall-clock cap for this step."`
}

// defaultSetupTimeoutSeconds gives a provisioning command room for a
// dependency install or a cold image pull, which is what these steps
// actually do.
const defaultSetupTimeoutSeconds = 300.0

// Timeout is the per-command cap, applying the default.
func (s *SandboxSetupStep) Timeout() float64 {
	if s.TimeoutSeconds <= 0 {
		return defaultSetupTimeoutSeconds
	}
	return s.TimeoutSeconds
}

func (s *SandboxSetupStep) validate(path string) error {
	var p problems
	if strings.TrimSpace(s.Name) == "" {
		p.add(at(path, "name"), ErrMissing,
			"a setup step needs a name — it is what a failure message points at")
	}
	if len(s.Files) == 0 && len(s.Commands) == 0 && len(s.Env) == 0 && s.Brief == "" {
		// A step that provisions nothing and tells the agent nothing is
		// almost always an unfinished edit; it would otherwise apply
		// cleanly forever and look like it was working.
		p.add(path, ErrMissing,
			"step %q does nothing: give it files, commands, env or a brief", s.Name)
	}
	if s.TimeoutSeconds < 0 {
		p.add(at(path, "timeout_seconds"), ErrOutOfRange,
			"must be 0 (the %v s default) or positive, got %v",
			defaultSetupTimeoutSeconds, s.TimeoutSeconds)
	}
	for file := range s.Files {
		if strings.TrimSpace(file) == "" {
			p.add(at(path, "files"), ErrMissing, "a file path must not be empty")
		}
	}
	return p.err()
}

// Resolve returns a copy of the step with ${VAR} substituted in its FILES
// and COMMANDS only, reporting what went unresolved.
//
// Env is deliberately left verbatim: it is resolved exactly once, together
// with the rest of the sandbox env at launch, and resolving it here too
// would double-resolve a secret whose real value contains a literal
// ${...}. Brief and Name are never resolved — agent-facing text and an
// identifier.
func (s *SandboxSetupStep) Resolve(path string, r *Resolver) (SandboxSetupStep, []Unresolved) {
	out := *s
	var missing []Unresolved
	if len(s.Files) > 0 {
		files, m := r.Map(at(path, "files"), s.Files)
		out.Files = files
		missing = append(missing, m...)
	}
	if len(s.Commands) > 0 {
		out.Commands = make([]string, len(s.Commands))
		for i, cmd := range s.Commands {
			expanded, names := r.Expand(cmd)
			out.Commands[i] = expanded
			if len(names) > 0 {
				missing = append(missing, Unresolved{Path: idx(at(path, "commands"), i), Names: names})
			}
		}
	}
	return out, missing
}
