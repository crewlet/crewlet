package config

import (
	"slices"
	"strings"
)

// SandboxType is the code-runtime backend.
type SandboxType string

const (
	// SandboxE2B is a remote box — the vendor cloud, or a self-hosted
	// cluster named by `domain`. A fresh machine per run that is not the
	// engine's, which is what makes it the right choice for anything
	// shared.
	SandboxE2B SandboxType = "e2b"
	// SandboxLocal is the ENGINE HOST as a backend, so code work can use
	// the subscription CLI login `crewlet llm login` already established,
	// with no remote account and no API key.
	SandboxLocal SandboxType = "local"
	// SandboxFake is in-process stubs for tests. It runs no real coding
	// agent and no real MCP.
	SandboxFake SandboxType = "fake"
	// SandboxNone disables code work entirely.
	SandboxNone SandboxType = "none"
)

// SandboxTypes is the closed set — THE ONES THIS ENGINE CAN BUILD.
//
// `e2b` was once in this set, was the DEFAULT, and had nothing behind it:
// buildSandboxProvider had a case that refused it by name. So a company that
// wrote `providers.sandbox:` and no type validated cleanly, reported a
// configured sandbox on the dashboard, and failed at its first coding run —
// an operator surface saying yes and a runtime saying no, which is the
// distance this closed set exists to close. It is back because the backend
// is, not because the value was restored.
//
// A value here is a backend `buildSandboxProvider` constructs, and a test
// walks this list against that switch.
var SandboxTypes = []SandboxType{SandboxE2B, SandboxLocal, SandboxFake, SandboxNone}

// CodingAgent is which coding CLI runs inside a box.
type CodingAgent string

// The coding agents a sandbox can run.
const (
	CodingAgentClaudeCode CodingAgent = "claude-code"
	CodingAgentOpenCode   CodingAgent = "opencode"
)

// CodingAgents is the closed set.
var CodingAgents = []CodingAgent{CodingAgentClaudeCode, CodingAgentOpenCode}

// SandboxProvider is the engine-wide code-runtime provider.
//
// A sandbox-enabled seat runs real code work as a coding agent inside an
// isolated box, through the run_sandbox Execute tool. This block is the
// backend; the per-seat gate is role.sandbox, and a seat without one never
// sees the tool.
type SandboxProvider struct {
	// Type is the backend, and it is REQUIRED whenever a sandbox: block is
	// present.
	//
	// THERE IS NO DEFAULT, and the absence of one is the whole point.
	// Every candidate default is wrong in a way that is silent:
	//
	//   - `local` runs the coding agent on the ENGINE HOST. Its `direct`
	//     containment runs as the engine's user with the engine's
	//     filesystem access, which is a deliberate trade an operator makes
	//     for their own machine and must never be made for them.
	//   - `none` reads as "code work is on" in the config and off in the
	//     engine, which is the silence a whole class of bugs here comes
	//     from.
	//   - A REMOTE backend was the default, and the engine had no code to
	//     build one. `providers.sandbox: {}` therefore validated, reported
	//     a configured sandbox on every operator surface, and failed at the
	//     first coding run with an error naming a type nobody had written.
	//
	// A block with no type is an operator who has not decided, and the
	// honest answer is to ask rather than to pick.
	Type SandboxType `yaml:"type,omitempty" json:"type,omitempty" js:"enum=e2b|local|fake|none" desc:"Backend: e2b, local, fake, or none. Required — there is no default."`

	// Local is the local backend's block. Required when Type is local and
	// refused otherwise: type local without it would silently take the
	// `direct` containment, which runs the coding agent as the engine user
	// with no host isolation and must be a deliberate choice.
	Local *LocalSandbox `yaml:"local,omitempty" json:"local,omitempty" desc:"The local backend's block. Required for type local, refused otherwise."`

	// APIKey authenticates the remote provider, and is REQUIRED for it —
	// including against a self-hosted cluster, where Domain changes which
	// API is talked to and never whether it authenticates.
	APIKey string `secret:"true" yaml:"api_key,omitempty" json:"api_key,omitempty" desc:"Remote sandbox API key; required for type e2b. ${VAR} supported."`

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
func (s *SandboxProvider) Enabled() bool {
	return s != nil && s.Type != SandboxNone
}

func (s *SandboxProvider) validate(path string) error {
	var p problems
	switch {
	case s.Type == "":
		// REQUIRED, AND REPORTED AS A CHOICE rather than as a missing
		// field, because the three answers do materially different
		// things to the machine the engine runs on. See [SandboxProvider].
		p.add(at(path, "type"), ErrMissing,
			"a sandbox block has to name its backend (%s) — there is no "+
				"default, because `local` runs the coding agent on this host "+
				"and `none` turns code work off, and neither may be chosen "+
				"for an operator who has not said which they meant. Remove "+
				"the block entirely to leave the sandbox unconfigured",
			names(SandboxTypes))
		return p.err()
	case !slices.Contains(SandboxTypes, s.Type):
		p.add(at(path, "type"), ErrUnknownValue, "%q (want %s)", s.Type, names(SandboxTypes))
		return p.err()
	}
	kind := s.Type

	switch {
	case kind == SandboxLocal && s.Local == nil:
		p.add(at(path, "local"), ErrMissing,
			"type local needs a `local:` block choosing a containment mode, "+
				"e.g. `local: {containment: direct}`. `direct` runs the coding "+
				"agent as the engine user with no host isolation, so it is "+
				"never assumed")
	case kind != SandboxLocal && s.Local != nil:
		p.add(at(path, "local"), ErrConflict,
			"`local:` only applies to type local (this is type %q). Remove "+
				"the block, or change the type", kind)
	case s.Local != nil:
		p.wrap(s.Local.validate(at(path, "local")))
	}

	// THE REMOTE FIELDS ONLY MEAN ANYTHING TO THE REMOTE BACKEND, and a
	// value that means nothing where it is written is the silence this
	// package spends most of its rules on: `domain` beside `type: local`
	// reads as a cluster address and configures nothing.
	if kind != SandboxE2B {
		for _, unread := range []struct {
			field, value string
		}{
			{"api_key", s.APIKey},
			{"domain", s.Domain},
			{"template", s.Template},
		} {
			if strings.TrimSpace(unread.value) == "" {
				continue
			}
			p.add(at(path, unread.field), ErrConflict,
				"only applies to type e2b (this is type %q), so nothing "+
					"would read it. Remove it, or change the type", kind)
		}
	}
	if kind == SandboxE2B && strings.TrimSpace(s.APIKey) == "" {
		// REQUIRED, and checked here rather than at construction so a
		// `crewlet validate` catches it: the API authenticates every call
		// on both the cloud and a self-hosted cluster, so a run without
		// one 401s at its first create — minutes into a turn that already
		// spent a Plan phase.
		p.add(at(path, "api_key"), ErrMissing,
			"required for type e2b — the API authenticates every call, "+
				"including against a self-hosted cluster, where `domain` "+
				"changes which API is talked to and not whether it "+
				"authenticates")
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

// Containment is how far a local box is separated from the engine host.
type Containment string

const (
	// ContainmentDirect runs a process tree in a per-box directory with
	// HOME and the XDG variables pointed at it and an allowlisted
	// environment. That isolates STATE — no box sees another's checkout,
	// memory or credentials — but NOT THE HOST: the coding agent runs as
	// the engine user, so it can read what that user can read and reach
	// what its credentials reach. Right for a workstation or a dedicated
	// VM; wrong for a shared host.
	ContainmentDirect Containment = "direct"
	// ContainmentContainer runs each box in its own container with the box
	// directory bind-mounted at /home/user, giving real host isolation and
	// the same in-box paths a remote provider uses.
	ContainmentContainer Containment = "container"
)

// Containments is the closed set.
var Containments = []Containment{ContainmentDirect, ContainmentContainer}

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
type LocalSandbox struct {
	Containment Containment `yaml:"containment,omitempty" json:"containment,omitempty" js:"enum=direct|container" desc:"direct (state isolation only) or container (host isolation)."`

	// Image is the container image for containment: container. Required
	// there, with deliberately no default: a box whose image lacks the
	// coding-agent CLI fails only once an agent tries to use it.
	Image string `yaml:"image,omitempty" json:"image,omitempty" desc:"Container image with the coding-agent CLI installed."`

	Runtime ContainerRuntime `yaml:"runtime,omitempty" json:"runtime,omitempty" js:"enum=auto|docker|podman" desc:"Container CLI to drive."`

	// StateDir is the parent directory for box directories. Boxes are
	// removed at teardown, and orphans from a crashed engine are reaped on
	// the next create.
	StateDir string `yaml:"state_dir,omitempty" json:"state_dir,omitempty" desc:"Parent directory for box directories."`

	// Network is the container network. Empty uses the runtime's default.
	// Setting `none` cuts the box off entirely — which also cuts off the
	// coding agent's LLM, so only do it for purely local work.
	Network string `yaml:"network,omitempty" json:"network,omitempty" desc:"Container network; setting it to none also cuts off the coding agent's LLM."`

	// RunArgs are spliced into the container run command (--cpus,
	// --memory, extra mounts, --user). This is where a container box is
	// SIZED, mirroring how a remote box is sized by its template.
	RunArgs []string `yaml:"run_args,omitempty" json:"run_args,omitempty" desc:"Extra container run arguments. This is where a local box is sized."`
}

func (l *LocalSandbox) validate(path string) error {
	var p problems
	if l.Containment != "" && !slices.Contains(Containments, l.Containment) {
		p.add(at(path, "containment"), ErrUnknownValue, "%q (want %s)",
			l.Containment, names(Containments))
		return p.err()
	}
	mode := l.Containment
	if mode == "" {
		mode = ContainmentDirect
	}
	if mode == ContainmentContainer && strings.TrimSpace(l.Image) == "" {
		p.add(at(path, "image"), ErrMissing,
			"containment container needs an image with the coding-agent CLI installed")
	}
	if mode == ContainmentDirect && l.Image != "" {
		p.add(at(path, "image"), ErrConflict,
			"image only applies to containment container. Remove it, or switch containment")
	}
	if l.Runtime != "" && !slices.Contains(ContainerRuntimes, l.Runtime) {
		p.add(at(path, "runtime"), ErrUnknownValue, "%q (want %s)", l.Runtime, names(ContainerRuntimes))
	}
	if mode == ContainmentDirect {
		if l.Network != "" {
			p.add(at(path, "network"), ErrConflict,
				"network only applies to containment container; a direct box "+
					"shares the engine host's network")
		}
		if len(l.RunArgs) > 0 {
			p.add(at(path, "run_args"), ErrConflict,
				"run_args are container run arguments; a direct box launches no container")
		}
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
