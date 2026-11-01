package config

import (
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/words"
)

// Placement is WHERE an executor run happens: one cell of the
// providers.sandbox catalogue.
//
// # One axis, three cells, named by the entry that launches
//
// This replaces a `type:` on the provider block, and the reason is that the
// old shape could express only ONE answer for a whole company. A company has
// exactly one `providers.sandbox`, so choosing `local` for the run that needs
// the operator's own subscription login meant choosing it for the run whose
// work must never touch the engine host. There was no way to say "this run
// remotely, that run here" — and the two are different security decisions
// about different work.
//
// So the provider block is a CATALOGUE — configure `e2b:`, `local:`, or
// both — and the choice sits on the providers.llm entry a seat's executor
// launches on (`run_in`), else on the catalogue's `default_run_in`. On the
// entry rather than the seat, because it is a property of that RUNTIME: a
// subscription login lives on the engine host, which `direct` and
// `container` reach and a remote cell does not.
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
)

// Placements is the closed set — every value a `run_in` may take, each of
// which needs a backend.
//
// Walked against the switch that builds them by a test, because the list and
// the switch have disagreed before: `e2b` was once the default with no case
// behind it, so a company that wrote `providers.sandbox:` and no type
// validated cleanly, reported a configured sandbox on the dashboard, and
// failed at its first run.
var Placements = []Placement{PlacementDirect, PlacementContainer, PlacementE2B}

// backend names the catalogue entry a placement needs, for the message a
// refusal carries: an operator who wrote `run_in: container` with no `local:`
// block needs to be told which block to add, not which value to change.
func (p Placement) backend() string {
	if p == PlacementE2B {
		return "`e2b:`"
	}
	return "`local:`"
}

// SandboxProvider is the engine-wide CATALOGUE of cells every agent seat's
// executor runs in.
//
// Every agent seat's executor is its coding CLI's own run, in a box: this
// block says WHICH BOXES EXIST, the providers.llm entry the run launches on
// picks the cell (`run_in`, else `default_run_in`), and a seat's own
// role.sandbox adds what that seat's box reads beyond the engine-wide
// settings. So a company with an agent seat and a model to run it on needs
// this block ([Company.validateSandboxPlacement]); one with no agent seat, or
// no providers.llm yet, does not.
//
// BOTH BACKENDS MAY BE CONFIGURED AT ONCE, which is the whole point of the
// reshape: the run that should use the operator's own subscription login and
// the run whose work must never touch the engine host are different runs,
// and a company-wide `type:` could only ever answer for both.
type SandboxProvider struct {
	// E2B configures the remote backend. Absent means no run may be
	// placed `run_in: e2b`.
	E2B *E2BSandbox `yaml:"e2b,omitempty" json:"e2b,omitempty" desc:"The remote backend. Absent = run_in: e2b is refused."`

	// Local configures the engine host as a backend, so a run can use the
	// subscription CLI login `crewlet llm login` already established, with
	// no remote account and no API key. Absent means no run may be placed
	// `run_in: direct` or `run_in: container`.
	//
	// It carries no containment of its own any more — `run_in` IS the
	// containment, per entry. A block-wide one could only say the same
	// thing for every run, which is the limitation this reshape removes.
	Local *LocalSandbox `yaml:"local,omitempty" json:"local,omitempty" desc:"The engine host as a backend. Absent = run_in: direct|container is refused."`

	// Fake wires the in-process double: no real box and no real coding
	// agent, every run completed by the double's own runner. For a
	// deployment demonstrating the flow, and named in config rather than
	// inferred so nobody runs one by accident.
	//
	// It answers EVERY placement, because it is not a placement — it is
	// the absence of one, and a double that answered only `direct` would
	// make a demo config differ from a real one in a second place.
	Fake bool `yaml:"fake,omitempty" json:"fake,omitempty" desc:"Use the in-process double for every placement. Demonstrations only."`

	// DefaultRunIn is where a run launched on a providers.llm entry that
	// names no `run_in` happens.
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
	// entry an executor can launch on names its own cell has nothing left to
	// default — that is the shape the catalogue exists for — so the refusal
	// is written at the entry that named none, never on this field alone.
	// See [Company.validateSandboxPlacement].
	DefaultRunIn Placement `yaml:"default_run_in,omitempty" json:"default_run_in,omitempty" js:"enum=direct|container|e2b" desc:"Where a run launched on a providers.llm entry with no run_in happens. Required unless the catalogue names exactly one cell, or every entry a seat's executor can launch on names its own."`

	// DefaultTimeoutSeconds is the box TTL and keepalive window. NOT a run
	// limit: a coding job is never force-stopped on a timer, and the
	// waiter refreshes a running box's TTL every tick so the clock never
	// kills it. Effectively the orphan-reclaim grace — how long a box
	// outlives an engine that stopped heart-beating.
	DefaultTimeoutSeconds float64 `yaml:"default_timeout_seconds,omitempty" json:"default_timeout_seconds,omitempty" js:"min=0" desc:"Box TTL/keepalive. Not a run cap: the orphan-reclaim grace."`

	// DefaultPauseTTLSeconds is how long a sandbox blocked on a human's
	// answer stays paused before it is reaped and the work re-seeds from
	// the pushed branch.
	//
	// A paused box is held indefinitely by the provider and BILLED for the
	// snapshot, so expiring it is the engine's job. 0 means never pause:
	// the box is torn down as soon as it blocks and the work always
	// re-seeds, for zero snapshot cost. A negative value is refused rather
	// than read as "no expiry" — an unbounded pause is exactly the leak
	// this knob exists to prevent — and a seat that wants the provider
	// default omits role.sandbox.pause_ttl_seconds.
	// A POINTER, because 0 is a valid SETTING here rather than an absent
	// field: unset means "take the 1800s default" and 0 means "never
	// pause". A plain float64 cannot hold both, and read as one it mapped
	// every operator who wrote 0 onto the default — silently keeping and
	// billing for the snapshots the setting exists to refuse.
	DefaultPauseTTLSeconds *float64 `yaml:"default_pause_ttl_seconds,omitempty" json:"default_pause_ttl_seconds,omitempty" js:"min=0" desc:"Paused-box TTL before reap and re-seed. Unset = 1800s; 0 = never pause."`

	// DefaultMaxTurns caps how many agentic rounds one executor run may
	// take, for seats that name none (role.sandbox.max_turns). Read it
	// through [SandboxProvider.MaxTurns].
	//
	// THE ONLY ROUND BOUND ON EVERY AGENT SEAT'S TURN. An executor run is
	// deliberately never force-stopped on a clock — see
	// default_timeout_seconds — so nothing else stops one that is
	// thrashing rather than working, and the box's own TTL is refreshed on
	// every waiter tick precisely so that it cannot. Unset takes
	// [defaultSandboxMaxTurns]; an explicit 0 is uncapped, which is how a
	// company opts out.
	//
	// A POINTER, because those are three states and an int has two: an
	// unset field's zero would read as "uncapped", so every company that
	// said nothing would run without the one bound its turns have.
	DefaultMaxTurns *int `yaml:"default_max_turns,omitempty" json:"default_max_turns,omitempty" js:"min=0" desc:"Agentic-round cap for one executor run on seats that name none. Unset = 96; 0 = uncapped."`

	// CommitAttribution says whether every commit a coding run authors as
	// itself credits Crewlet: `Co-authored-by: Crewlet <noreply@crewlet.ai>`,
	// and `Crewlet-Run:` naming the run's turn. Git adds it, through a hook
	// the run's box is given (internal/sandbox/codingagent), so it holds
	// whatever the model remembers, and only on the run's own commits: a
	// person's commit the run amends, cherry-picks or rebases keeps no
	// credit. Read it through [SandboxProvider.AttributesCommits].
	//
	// ON UNLESS TURNED OFF, because a commit that says where it came from is
	// what a reader of the history needs by default, and the credit changes
	// nothing a commit does. A company that does not want Crewlet named in
	// its repositories' history sets false.
	//
	// A POINTER, because unset is "on": a plain bool's zero would turn the
	// credit off for every company that said nothing.
	CommitAttribution *bool `yaml:"commit_attribution,omitempty" json:"commit_attribution,omitempty" desc:"Credit Crewlet on every commit a coding run authors as itself: Co-authored-by: Crewlet <noreply@crewlet.ai>, and Crewlet-Run naming the run. Unset = true; false = credit nothing."`

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
	Template string `yaml:"template,omitempty" json:"template,omitempty" desc:"Box template. This is where vCPU/RAM/disk are set: at template build time."`
}

func (e *E2BSandbox) validate(path Path) error {
	var p problems
	if strings.TrimSpace(e.APIKey) == "" {
		// REQUIRED, and checked here rather than at construction so a
		// `crewlet validate` catches it: the API authenticates every call
		// on both the cloud and a self-hosted cluster, so a run without
		// one 401s at its first create — minutes into a turn that has
		// already spent its own rounds.
		p.add(at(path, "api_key"), ErrMissing,
			"required: the API authenticates every call, including against "+
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

	// defaultSandboxMaxTurns is 96 because it is now the only round bound
	// on every agent seat's turn. The engine's own executor loop was capped
	// at 48 rounds once its extension judge had granted all it could
	// (turn_engine's execute ceiling, twice its 24-round base), and that was
	// for engine-tool work alone. One run now also does the shell, edit and
	// test steps code work adds, each a CLI turn of its own, so twice that
	// ceiling: room for a real task's reading, editing and checking, and
	// still a wall a run that is thrashing hits within one turn.
	defaultSandboxMaxTurns = 96
)

// Timeout is the box TTL, applying the default.
func (s *SandboxProvider) Timeout() float64 {
	if s.DefaultTimeoutSeconds <= 0 {
		return defaultSandboxTimeoutSeconds
	}
	return s.DefaultTimeoutSeconds
}

// MaxTurns is the agentic-round cap one executor run takes when its seat
// names none, applying the default: an explicit 0 is uncapped, and unset is
// [defaultSandboxMaxTurns]. Nil-safe, so a company with no catalogue reads
// the default rather than panicking: such a company runs no box at all.
func (s *SandboxProvider) MaxTurns() int {
	if s == nil || s.DefaultMaxTurns == nil {
		return defaultSandboxMaxTurns
	}
	return *s.DefaultMaxTurns
}

// AttributesCommits reports whether a coding run's own commits credit Crewlet
// ([SandboxProvider.CommitAttribution]): unless the company set false.
// Nil-safe, as [SandboxProvider.MaxTurns] is: a company with no catalogue
// runs no box, so nothing reads the answer.
func (s *SandboxProvider) AttributesCommits() bool {
	return s == nil || s.CommitAttribution == nil || *s.CommitAttribution
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

// Enabled reports whether the catalogue configures any cell at all.
//
// A catalogue with nothing in it is not enabled: the block was written and
// configures no box, so no executor run has anywhere to happen.
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

// RunIn is where a run launched on an entry that names no placement
// happens, or empty when the catalogue cannot answer that on its own.
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

func (s *SandboxProvider) validate(path Path) error {
	var p problems
	switch {
	case !s.Enabled():
		// A block that names no backend is an unfinished edit: every agent
		// seat's executor runs in one of its cells, and this one has none
		// to run in.
		p.add(path, ErrMissing,
			"a sandbox block configures nothing: give it `e2b:`, `local:`, or "+
				"`fake: true`. Every agent seat's executor runs in one of its "+
				"cells; only a company with no agent seat or no providers.llm entry "+
				"yet may leave the block out")
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
		// a default is NEEDED is a question about the entries the seats'
		// executors launch on, and this block cannot see them. An entry
		// that names no cell against an ambiguous catalogue is refused
		// where it is written, with a message offering this field as the
		// other remedy — see [Company.validateSandboxPlacement]. Refused
		// here unconditionally, the remedy that message offered ("give
		// every entry its own run_in") could never pass.
	case !slices.Contains(Placements, s.DefaultRunIn):
		p.add(at(path, "default_run_in"), ErrUnknownValue, "%q (want %s)",
			s.DefaultRunIn, words.Or(Placements))
	case !s.Configured(s.DefaultRunIn):
		p.add(at(path, "default_run_in"), ErrConflict,
			"%q needs the %s backend configured under providers.sandbox (this "+
				"catalogue has %s)",
			s.DefaultRunIn, s.DefaultRunIn.backend(), words.And(s.available()))
	}

	if s.DefaultTimeoutSeconds < 0 {
		p.add(at(path, "default_timeout_seconds"), ErrOutOfRange,
			"must be 0 (the %v s default) or positive, got %v",
			defaultSandboxTimeoutSeconds, s.DefaultTimeoutSeconds)
	}
	if s.DefaultMaxTurns != nil && *s.DefaultMaxTurns < 0 {
		p.add(at(path, "default_max_turns"), ErrOutOfRange,
			"%d (a round cap cannot be negative; omit the field for the default "+
				"of %d, or set 0 for uncapped)", *s.DefaultMaxTurns, defaultSandboxMaxTurns)
	}
	// A NEGATIVE value is refused: an unbounded pause is the leak this knob
	// exists to prevent.
	if s.DefaultPauseTTLSeconds != nil && *s.DefaultPauseTTLSeconds < 0 {
		p.add(at(path, "default_pause_ttl_seconds"), ErrOutOfRange,
			"must not be negative: an unbounded pause is the snapshot leak "+
				"this knob exists to prevent. Use 0 to never pause, or omit "+
				"the field for the 1800s default")
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
	for _, p := range Placements {
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
// runtime, so a run can use the subscription login already established.
//
// IT CARRIES NO CONTAINMENT OF ITS OWN. `run_in: direct` and `run_in:
// container` are both served by this one block, per entry, because they are a
// choice about ONE RUNTIME'S WORK rather than about the host: the run that
// needs the operator's own login and the run whose generated code must not see
// the engine's filesystem are different runs on the same machine. A
// block-level mode could only ever answer for both.
type LocalSandbox struct {
	// Image is the container image, required wherever an executor run is
	// placed in a container, with deliberately no default: a box whose
	// image lacks the coding-agent CLI fails only once an agent tries to use
	// it.
	Image string `yaml:"image,omitempty" json:"image,omitempty" desc:"Container image with the coding-agent CLI installed. Required for run_in: container."`

	Runtime ContainerRuntime `yaml:"runtime,omitempty" json:"runtime,omitempty" js:"enum=auto|docker|podman" desc:"Container CLI to drive."`

	// StateDir is the parent directory for box directories: the company's
	// BOX ROOT. Boxes are removed at teardown, and orphans from a crashed
	// engine are reaped on the next create. It applies to both placements —
	// a direct box is a directory under it too — and to the Dashboard
	// Assistant's deep work on the engine host, whose confined boxes the
	// engine keeps in a directory of their own beneath it, so every box the
	// engine makes on its host lands on the one volume sized for boxes.
	StateDir string `yaml:"state_dir,omitempty" json:"state_dir,omitempty" desc:"Parent directory for box directories; used by both local placements and by deep work on the engine host."`

	// Network is the container network. Empty uses the runtime's default.
	// Setting `none` cuts the box off entirely — which also cuts off the
	// coding agent's LLM, so only do it for purely local work.
	Network string `yaml:"network,omitempty" json:"network,omitempty" desc:"Container network; setting it to none also cuts off the coding agent's LLM."`

	// RunArgs are spliced into the container run command (--cpus,
	// --memory, extra mounts, --user). This is where a container box is
	// SIZED, mirroring how a remote box is sized by its template.
	RunArgs []string `yaml:"run_args,omitempty" json:"run_args,omitempty" desc:"Extra container run arguments. This is where a local box is sized."`

	// RuntimeEnv is the environment DECLARED for the container runtime's
	// own CLI — every `docker`/`podman` call the backend makes — over the
	// fixed rule it always gets (hostbox.ContainerRuntime: the host
	// allowlist, the engine user's locations and the runtime's own
	// DOCKER_/CONTAINER_/CONTAINERS_/PODMAN_ settings).
	//
	// The channel the fixed rule cannot be: what a runtime's HELPERS read
	// is open-ended — the cloud role, profile or key file a registry
	// credential helper pulls a private image with — and with no
	// way to add a name, a runtime that needed one simply stopped working
	// when the engine stopped handing the CLI its whole environment. It is
	// the MCP server's `env:` for the runtime, resolved the same way at the
	// edge, and it never reaches a box: what a box needs is
	// `role.sandbox.env`.
	//
	// A credential field, for the reason an MCP server's `env:` is one: a
	// helper's cloud credentials are exactly what gets declared here.
	RuntimeEnv map[string]string `secret:"true" yaml:"runtime_env,omitempty" json:"runtime_env,omitempty" desc:"Environment for the container runtime's own CLI (not the box); ${VAR} supported."`
}

// containerOnly are the fields no direct box reads, named as an operator
// wrote them. A value that means nothing where it is written is the silence
// this package spends most of its rules on, and the check that reports it
// needs the SEATS' executor chains — see (*Company).validateSandboxPlacement.
func (l *LocalSandbox) containerOnly() []struct{ field, value string } {
	set := func(n int) string {
		if n > 0 {
			return "set"
		}
		return ""
	}
	return []struct{ field, value string }{
		{"image", l.Image},
		{"runtime", string(l.Runtime)},
		{"network", l.Network},
		{"run_args", set(len(l.RunArgs))},
		{"runtime_env", set(len(l.RuntimeEnv))},
	}
}

// unsuppliedEnvArgs is every bare `-e NAME` in run_args — the index it was
// written at and the name — that the runtime's CLI has nothing for.
//
// A bare `-e NAME` (`--env NAME`, `--env=NAME`, `-eNAME`) COPIES NAME FROM
// THE RUNTIME CLI'S OWN ENVIRONMENT, and that environment is the fixed rule
// plus runtime_env — never the engine's. So a name neither carries arrives in
// the box as nothing at all, silently: the container starts, and whatever
// read the variable finds it unset. `-e NAME=value` names its own value and
// is not this question.
func (l *LocalSandbox) unsuppliedEnvArgs() []struct {
	at   int
	name string
} {
	var out []struct {
		at   int
		name string
	}
	for i := 0; i < len(l.RunArgs); i++ {
		arg, at := l.RunArgs[i], i
		var value string
		switch {
		case arg == "-e" || arg == "--env":
			if i+1 >= len(l.RunArgs) {
				continue
			}
			i++
			value, at = l.RunArgs[i], i
		case strings.HasPrefix(arg, "--env="):
			value = strings.TrimPrefix(arg, "--env=")
		case strings.HasPrefix(arg, "-e") && !strings.HasPrefix(arg, "--"):
			value = strings.TrimPrefix(strings.TrimPrefix(arg, "-e"), "=")
		default:
			continue
		}
		if value == "" || strings.Contains(value, "=") {
			continue
		}
		if _, declared := l.RuntimeEnv[value]; declared || hostbox.ContainerRuntimeCarries(value) {
			continue
		}
		out = append(out, struct {
			at   int
			name string
		}{at, value})
	}
	return out
}

func (l *LocalSandbox) validate(path Path) error {
	var p problems
	if l.Runtime != "" && !slices.Contains(ContainerRuntimes, l.Runtime) {
		p.add(at(path, "runtime"), ErrUnknownValue, "%q (want %s)", l.Runtime, words.Or(ContainerRuntimes))
	}
	// A NAME OUTSIDE THE ENVIRONMENT'S OWN GRAMMAR is refused rather than
	// handed on: `A=B` as a key would set a variable called A to `B=…`, and
	// a name with a space in it is one no process can read back.
	for _, name := range slices.Sorted(maps.Keys(l.RuntimeEnv)) {
		if !envref.ValidName(name) {
			p.add(entry(at(path, "runtime_env"), name), ErrShape,
				"%q is not an environment variable name (letters, digits and "+
					"underscores, not starting with a digit)", name)
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
	// run.
	//
	// A CREDENTIAL FIELD, because provisioning is what these files are for:
	// a registry's auth file, a git credential store, a cloud CLI's config.
	// A file's content is shown on a config read only when it is exactly one
	// ${VAR} reference, and masked otherwise.
	//
	// AND CONTENT RATHER THAN A SETTING (`secret:"content"`, see
	// internal/secrets): a body that is exactly one ${VAR} is read at launch
	// as the value that variable holds, byte for byte, and any other body is
	// written as it is — a `${HOME}` in a script or a `${NPM_TOKEN}` in an
	// .npmrc is the box's own syntax, expanded there from the run
	// environment and never from the engine host's.
	Files map[string]string `secret:"content" yaml:"files,omitempty" json:"files,omitempty" desc:"Files written into the box before commands run; a body that is exactly one ${VAR} is that variable's value."`

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
	//
	// Tagged as a credential like its sibling role.sandbox.env: both are
	// merged into the same run environment, which is where an external
	// service token is declared.
	Env map[string]string `secret:"true" yaml:"env,omitempty" json:"env,omitempty" desc:"Env merged into the run; resolved once at launch, not at load."`

	// OptionalKeys names the keys of Env, and the paths of Files, whose value
	// may resolve to nothing: such a variable is left out of the run and
	// such a file is not written, quietly. Any other reference that resolves
	// to nothing is left out too, and the run's start is logged naming it
	// (sandbox_env_unresolved) — see [RoleSandbox.OptionalKeys], the seat's
	// own block's half, for why an executor run starts either way.
	OptionalKeys []string `yaml:"optional_keys,omitempty" json:"optional_keys,omitempty" desc:"Keys of env, or paths of files, whose value may resolve to nothing; left out quietly rather than logged as a gap in the run's environment."`

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

// IdentityKey is the step's name: the handle a failure message and a log line
// already use for it, and now what its masked credentials are restored by.
//
// A step's env and files are credentials, so a list of steps is a collection
// whose masks must follow the member rather than the slot. Reordering two
// provisioning steps must never hand one step's registry token to the other.
//
// A VALUE receiver, because the redaction walker holds the prior document by
// value and cannot take an address inside it.
func (s SandboxSetupStep) IdentityKey() string { return s.Name }

// Timeout is the per-command cap, applying the default.
func (s *SandboxSetupStep) Timeout() float64 { return org.SetupTimeout(s.TimeoutSeconds) }

func (s *SandboxSetupStep) validate(path Path) error {
	var p problems
	if strings.TrimSpace(s.Name) == "" {
		p.add(at(path, "name"), ErrMissing,
			"a setup step needs a name: it is what a failure message points at")
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
			org.DefaultSetupTimeoutSeconds, s.TimeoutSeconds)
	}
	for file := range s.Files {
		if strings.TrimSpace(file) == "" {
			p.add(at(path, "files"), ErrMissing, "a file path must not be empty")
		}
	}
	return p.err()
}
