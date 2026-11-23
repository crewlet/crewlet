package config

import (
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/words"
)

// Placement is WHERE an executor run happens: one cell of the
// providers.sandbox catalogue.
//
// # Two cells, named by the entry that launches
//
// `local:` serves both cells, and they are different security decisions about
// one runtime's work: `direct` hands the harness the engine user's reach (the
// operator's own subscription login among it), `container` none of the host's
// files. A company has one `providers.sandbox`, so a company-wide choice could
// not say both; the choice sits on the providers.llm entry the executor
// launches on (`run_in`, else `default_run_in`), a property of that RUNTIME.
type Placement string

const (
	// PlacementDirect runs a process tree on the ENGINE HOST in a per-box
	// directory, with HOME and the XDG variables pointed at it and an
	// allowlisted environment. That isolates STATE — no box sees another's
	// checkout, memory or credentials — but NOT THE HOST: the harness
	// runs as the engine user, so it reads what that user reads and
	// reaches what its credentials reach. Right for a workstation or a
	// dedicated VM; wrong for a shared host.
	PlacementDirect Placement = "direct"

	// PlacementContainer runs each box in its own container on the engine
	// host, with the box directory bind-mounted at [hostbox.ContainerHome] —
	// real host isolation, and a home path a setup step and an image can name.
	PlacementContainer Placement = "container"
)

// Placements is the closed set — every value a `run_in` may take, each of
// which needs a backend.
//
// Walked against the switch that builds them by a test: a value the list
// names and the switch does not build validates cleanly, reports a configured
// sandbox on the dashboard, and fails at its first run.
var Placements = []Placement{PlacementDirect, PlacementContainer}

// SandboxProvider is the engine-wide CATALOGUE of cells every agent seat's
// executor runs in.
//
// Every agent seat's executor is its harness's own run, in a box: this block
// says HOW BOXES ARE MADE, the providers.llm entry picks the cell (`run_in`,
// else `default_run_in`), and a seat's role.sandbox adds what its box reads
// beyond the engine-wide settings. So a company with an agent seat and a
// model needs this block ([Company.validateSandboxPlacement]); one with
// neither does not. `local:` serves both cells at once; `fake: true` stands
// in for it.
type SandboxProvider struct {
	// Local configures the engine host's boxes, in both cells, so a run can
	// use the subscription harness login `crewlet llm login` already
	// established. Required unless Fake is set.
	//
	// It carries no containment of its own: `run_in` IS the containment, per
	// entry, and a block-wide one could only say the same thing for every run.
	Local *LocalSandbox `yaml:"local,omitempty" json:"local,omitempty" desc:"The engine host's boxes, for run_in direct and container. Required unless fake is true."`

	// Fake wires the in-process double: no real box and no real harness,
	// every run completed by the double's own runner. For a
	// deployment demonstrating the flow, and named in config rather than
	// inferred so nobody runs one by accident.
	//
	// It answers EVERY placement, because it is not a placement — it is
	// the absence of one, and a double that answered only `direct` would
	// make a demo config differ from a real one in a second place.
	Fake bool `yaml:"fake,omitempty" json:"fake,omitempty" desc:"Use the in-process double for every placement. Demonstrations only."`

	// DefaultRunIn is where a run launched on a providers.llm entry that names
	// no `run_in` happens.
	//
	// THERE IS NO IMPLICIT DEFAULT FOR `local:`: `direct` hands the harness
	// the engine user's reach, and `container` needs an image nobody named, so
	// neither may be chosen for an operator who did not say. Only the double
	// resolves alone.
	//
	// REQUIRED ONLY WHERE SOMETHING WOULD READ IT. A catalogue whose every
	// launchable entry names its cell has nothing to default, so the refusal is
	// written at the entry that named none. See [Company.validateSandboxPlacement].
	DefaultRunIn Placement `yaml:"default_run_in,omitempty" json:"default_run_in,omitempty" js:"enum=direct|container" desc:"Where a run launched on a providers.llm entry with no run_in happens. Required unless fake is true, or every entry a seat's executor can launch on names its own."`

	// HostGraceSeconds is how long the node that launched a run's box may be
	// absent from the fleet, or its store silent, before the run is recorded
	// lost or its parked box released; and the stamp cutoff of the orphan
	// reaper on a node's box root. NOT a run limit: an executor is never
	// stopped on a timer. The manager's, whichever cell a box is in.
	//
	// 900s by default: a restart or rolling deploy of a node comes back well
	// inside it and keeps its runs, and it is the longest a seat's round waits
	// on a node that left for good.
	HostGraceSeconds float64 `yaml:"host_grace_seconds,omitempty" json:"host_grace_seconds,omitempty" js:"min=0" desc:"How long a node that launched boxes may be absent or silent before its runs are recorded lost or released; also the orphan reaper's cutoff on its box root. Not a run cap. Unset = 900s."`

	// DefaultPauseTTLSeconds is how long a sandbox blocked on a human's answer
	// stays paused before it is reaped and the work re-seeds from the pushed
	// branch. A paused box holds its directory on the launching node's box
	// root and RAM (a stopped process tree, or a paused container), and
	// nothing else reclaims it while the run waits, so expiring it is the
	// engine's job. 0 means never pause: the box is torn down as soon as it
	// blocks and re-seeds. A negative value is refused, never "no expiry"; a
	// seat wanting the default omits role.sandbox.pause_ttl_seconds.
	//
	// A POINTER, because 0 is a valid SETTING, not an absent field: unset takes
	// the 1800s default, and a plain float64 cannot tell them apart.
	DefaultPauseTTLSeconds *float64 `yaml:"default_pause_ttl_seconds,omitempty" json:"default_pause_ttl_seconds,omitempty" js:"min=0" desc:"Paused-box TTL before reap and re-seed. Unset = 1800s; 0 = never pause."`

	// DefaultMaxTurns caps how many agentic rounds one executor run may take,
	// for seats that name none (role.sandbox.max_turns). Read it through
	// [SandboxProvider.MaxTurns].
	//
	// UNCAPPED UNLESS SET. How many rounds a task takes is the harness's own
	// loop to judge, as when a person runs it. A positive value becomes the
	// harness's turn cap (Claude Code's --max-turns, OpenCode's agent steps, a
	// Codex run's helper's count). Without one a run ends when it finishes, at a
	// scheduled turn's timeout_seconds, or when a person stops it, never on a
	// clock of its box's or part-way through a budget.
	//
	// A PLAIN INT: zero (left out or 0) is uncapped, every other value the cap.
	DefaultMaxTurns int `yaml:"default_max_turns,omitempty" json:"default_max_turns,omitempty" js:"min=0" desc:"Agentic-round cap for one executor run on seats that name none. Unset or 0 = uncapped."`

	// CommitAttribution says whether every commit an executor authors as itself
	// credits Crewlet: `Co-authored-by: Crewlet <noreply@crewlet.com>`,
	// `Crewlet-Seat:` the seat, `Crewlet-Run:` the run's turn and
	// `Crewlet-Work-Item:` the item it is charged to. Git adds it
	// through a hook the run's box is given (internal/sandbox/executor), so it
	// holds whatever the model remembers, and only on the run's own commits: a
	// person's commit the run amends or rebases keeps no credit.
	// Read it through [SandboxProvider.AttributesCommits].
	//
	// ON UNLESS TURNED OFF: a history reader needs where a commit came from, and
	// the credit changes nothing a commit does. A POINTER, because unset is
	// "on": a plain bool's zero would turn it off.
	CommitAttribution *bool `yaml:"commit_attribution,omitempty" json:"commit_attribution,omitempty" desc:"Credit Crewlet on every commit an executor authors as itself: Co-authored-by: Crewlet <noreply@crewlet.com>, Crewlet-Seat naming the seat, Crewlet-Run naming the run, and Crewlet-Work-Item naming the item its turn is charged to. Unset = true; false = credit nothing."`

	// Setup is the engine-wide provisioning applied to every box before
	// each seat's own extras. The engine ships NO steps of its own — git
	// auth included: the documented recipe is config, so an operator can
	// see and change every command that touches their box.
	Setup []SandboxSetupStep `yaml:"setup,omitempty" json:"setup,omitempty" desc:"Provisioning steps applied to every box before per-seat extras."`
}

// HostGrace is [SandboxProvider.HostGraceSeconds] as a duration, zero when
// unset: the sandbox manager applies the default ([sandbox.DefaultHostGrace]),
// so it lives in one place, as [SandboxProvider.PauseTTL]'s does.
func (s *SandboxProvider) HostGrace() time.Duration {
	if s.HostGraceSeconds <= 0 {
		return 0
	}
	return time.Duration(s.HostGraceSeconds * float64(time.Second))
}

// MaxTurns is the agentic-round cap one executor run takes when its seat
// names none, zero for uncapped ([SandboxProvider.DefaultMaxTurns]).
// Nil-safe, so a company with no catalogue reads uncapped rather than
// panicking: such a company runs no box at all.
func (s *SandboxProvider) MaxTurns() int {
	if s == nil {
		return 0
	}
	return s.DefaultMaxTurns
}

// AttributesCommits reports whether an executor's own commits credit Crewlet
// ([SandboxProvider.CommitAttribution]): unless the company set false.
// Nil-safe, as [SandboxProvider.MaxTurns] is: a company with no catalogue
// runs no box, so nothing reads the answer.
func (s *SandboxProvider) AttributesCommits() bool {
	return s == nil || s.CommitAttribution == nil || *s.CommitAttribution
}

// PauseTTL is the paused-box TTL as configured, or nil when the field is
// absent and the default applies, resolved by the caller so it lives in ONE
// place ([sandbox.DefaultPauseTTL]). A POINTER OUT as well as in: 0 is "never
// pause", absent is "take the 1800s default", and collapsing the two holds
// every paused box for a company that asked never to pause.
//
// The TTL trades the disk and memory a paused box holds against an answer
// finding the run's working tree as it left it. The run's harness session is
// continued from the turn's kept capture whether or not the box is still
// there; where the turn kept none, the paused box is the session's one copy,
// and an answer after the pause ends carries the run on in a session of its own.
func (s *SandboxProvider) PauseTTL() *float64 {
	return s.DefaultPauseTTLSeconds
}

// Enabled reports whether the catalogue configures any box at all: `local:`
// serves every cell, and the double answers every placement.
//
// A catalogue with nothing in it is not enabled: the block was written and
// configures no box, so no executor run has anywhere to happen.
func (s *SandboxProvider) Enabled() bool {
	return s != nil && (s.Fake || s.Local != nil)
}

// RunIn is where a run launched on an entry that names no placement
// happens, or empty when the catalogue cannot answer that on its own.
//
// EMPTY IS A REAL ANSWER, not a failure to compute one: `direct` runs the
// harness as the engine user with the engine's own filesystem access, and
// `container` needs an image holding the harness. Guessing either for an
// operator who wrote neither is the kind of default that is discovered
// afterwards. Validation turns the empty answer into a message naming both
// fields that would fix it.
func (s *SandboxProvider) RunIn() Placement {
	switch {
	case s == nil:
		return ""
	case s.DefaultRunIn != "":
		return s.DefaultRunIn
	case s.Fake:
		// The double runs nowhere, so nothing is being chosen on the
		// operator's behalf and the cell only names itself in events.
		return PlacementDirect
	}
	// `local:` is two answers — direct and container are different security
	// decisions about the same host — so it does not resolve.
	return ""
}

// runInAt is the path of what names [SandboxProvider.RunIn]: its
// default_run_in where it is written, else the block itself — resolved from
// the catalogue's own shape rather than written, so a message points at the
// block, not at a field the operator will not find.
func (s *SandboxProvider) runInAt() string {
	if s != nil && s.DefaultRunIn != "" {
		return "providers.sandbox.default_run_in"
	}
	return "providers.sandbox"
}

func (s *SandboxProvider) validate(path Path) error {
	var p problems
	switch {
	case !s.Enabled():
		// A block that names no backend is an unfinished edit: every agent
		// seat's executor runs in one of its cells, and this one has none
		// to run in.
		p.add(path, ErrMissing,
			"a sandbox block configures nothing: give it `local:` or "+
				"`fake: true`. Every agent seat's executor runs in one of its "+
				"cells; only a company with no agent seat or no providers.llm entry "+
				"yet may leave the block out")
		return p.err()
	case s.Fake && s.Local != nil:
		// The double is not a backend beside the real one — it replaces
		// it, so a real block underneath it reads as configuration and
		// configures nothing.
		p.add(at(path, "fake"), ErrConflict,
			"the in-process double answers every placement, so `local:` "+
				"beside it is never read. Remove one of the two")
		return p.err()
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
	}

	if s.HostGraceSeconds < 0 {
		p.add(at(path, "host_grace_seconds"), ErrOutOfRange,
			"must be positive, or 0 for the default, got %v", s.HostGraceSeconds)
	}
	if s.DefaultMaxTurns < 0 {
		p.add(at(path, "default_max_turns"), ErrOutOfRange,
			"%d (a round cap cannot be negative; omit the field, or set 0, to "+
				"leave executor runs uncapped)", s.DefaultMaxTurns)
	}
	// A NEGATIVE value is refused: an unbounded pause is the leak this knob
	// exists to prevent.
	if s.DefaultPauseTTLSeconds != nil && *s.DefaultPauseTTLSeconds < 0 {
		p.add(at(path, "default_pause_ttl_seconds"), ErrOutOfRange,
			"must not be negative: an unbounded pause holds the box's disk and "+
				"memory on its node for good. Use 0 to never pause, or omit "+
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
	// placed in a container, with deliberately no default: a box whose image
	// lacks the harness fails only once an agent tries to use it. On a company
	// hosting code it is also what runs checks: each check is a container made
	// from it on a Gitlet node, so it holds git and whatever the checks build
	// with, and a company that sets none runs no check ([Company.ChecksRefusal]).
	Image string `yaml:"image,omitempty" json:"image,omitempty" desc:"Container image: with the harness installed for an executor run's box, and git 2.40 or newer and what its checks build with for a check's. Required where an executor run is placed in a container; on a company hosting code (code.backend: gitlet), setting it runs the repositories' checks in containers made from it, and leaving it out runs none."`

	Runtime ContainerRuntime `yaml:"runtime,omitempty" json:"runtime,omitempty" js:"enum=auto|docker|podman" desc:"Container CLI to drive."`

	// StateDir is the parent directory for box directories: the company's
	// BOX ROOT. Boxes are removed at teardown, and one a crashed engine left
	// is reclaimed by its node. It applies to both placements (a direct box is
	// a directory under it too), so every box the engine makes on its host
	// lands on the one volume sized for boxes.
	StateDir string `yaml:"state_dir,omitempty" json:"state_dir,omitempty" desc:"Parent directory for box directories; used by both placements."`

	// Network is the container network. Empty uses the runtime's default.
	// Setting `none` cuts the box off entirely — which also cuts off the
	// harness's LLM, so only do it for purely local work.
	Network string `yaml:"network,omitempty" json:"network,omitempty" desc:"Container network; setting it to none also cuts off the harness's LLM."`

	// RunArgs are spliced into the container run command (--cpus,
	// --memory, extra mounts, --user). This is where a container box is
	// SIZED.
	RunArgs []string `yaml:"run_args,omitempty" json:"run_args,omitempty" desc:"Extra container run arguments. This is where a container box is sized."`

	// RuntimeEnv is the environment DECLARED for the container runtime's own CLI
	// (every `docker`/`podman` call the backend makes), over the fixed rule it
	// always gets (hostbox.ContainerRuntime: the host allowlist, the engine
	// user's locations and the runtime's own DOCKER_/CONTAINER_/CONTAINERS_/
	// PODMAN_ settings).
	//
	// The channel the fixed rule cannot be: what a runtime's HELPERS read is
	// open-ended (the cloud role, profile or key file a registry credential
	// helper pulls a private image with). It is the MCP server's `env:` for the
	// runtime, resolved the same way at the edge, and never reaches a box: that
	// is `role.sandbox.env`. A credential field, for the reason an MCP server's
	// `env:` is one.
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
	// A MEMORY SIZE THE RUNTIME WOULD REFUSE fails every box at its create;
	// refused here, at the argument, it is said before any seat runs.
	if i, value := memoryArg(l.RunArgs); i >= 0 {
		if _, ok := ramInBytes(value); !ok {
			p.add(idx(at(path, "run_args"), i), ErrShape, "%s", memorySizeRefused(value))
		}
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
// Every provisioning concern — git auth for any other host included — goes
// through this one apply, env-merge and brief pipeline. Steps come only from
// providers.sandbox.setup and role.sandbox.setup, but for the one exception:
// a repository on Gitlet is a run's GRANT, which the
// engine clones into the box itself after these steps, with the run's own
// credential (ADR-0093) — and no step's env may set the variables that
// credential and its clones read ([ReservedSandboxEnv]).
type SandboxSetupStep struct {
	// Name identifies the step in logs and in setup-failure errors.
	Name string `yaml:"name" json:"name" js:"required" desc:"Short identifier used in logs and failure messages."`

	// Files are written into the box (path -> content) before Commands run.
	//
	// A CREDENTIAL FIELD, because provisioning is what these files are for: a
	// registry's auth file, a git credential store, a cloud CLI's config. A
	// file's content is shown on a config read only when it is exactly one
	// ${VAR} reference, and masked otherwise.
	//
	// CONTENT RATHER THAN A SETTING (`secret:"content"`, see internal/secrets): a
	// body that is exactly one ${VAR} is read at launch as the value that
	// variable holds, byte for byte; any other body is written as it is, so a
	// `${HOME}` in a script or `${NPM_TOKEN}` in an .npmrc is the box's own
	// syntax, expanded there from the run environment, never the engine host's.
	Files map[string]string `secret:"content" yaml:"files,omitempty" json:"files,omitempty" desc:"Files written into the box before commands run; a body that is exactly one ${VAR} is that variable's value."`

	// Commands run in order after the files land. A non-zero exit fails
	// the whole acquisition — the harness's brief promises this
	// environment, so a partially provisioned box must never reach a run.
	Commands []string `yaml:"commands,omitempty" json:"commands,omitempty" desc:"Shell commands run in order; a non-zero exit fails the box."`

	// Env is merged into the harness's run environment.
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

	// Brief is the paragraph handed to the harness: what this step
	// made TRUE about its box. Never resolved — it is agent-facing text,
	// and substituting engine-host environment into it would be surprising
	// at best.
	Brief string `yaml:"brief,omitempty" json:"brief,omitempty" desc:"What this step made true about the box, told to the harness."`

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

// ReservedSandboxEnv is every variable the ENGINE sets in a box's environment
// itself — an executor's or a check's — and so the names no env an operator
// writes for a box may carry: the box's access to Gitlet —
// its git credential and the base its clones and pushes reach, which an
// executor's box and a check's are both given — and what a check's box is told
// of its check ([ReservedCheckEnv]). A closed list, which the validator reads here and the
// engine's tests hold to the variables each launch sets: every executor
// launch on a node serving Gitlet sets every name but the check's own,
// whatever its run checks out (internal/engine,
// TestABoxIsConfiguredByEnvironment).
func ReservedSandboxEnv() []string {
	return append([]string{"CREWLET_GIT_ACCESS_FILE", "CREWLET_GIT_URL"}, ReservedCheckEnv()...)
}

// ReservedCheckEnv is every variable the engine sets in a CHECK'S box alone:
// which check it runs — the repository, the commit it measures and the base
// that commit is measured against, the check's name and its attempt — where
// its box reads that commit without a clone, and where its command writes the
// summary its result is reported with. Beside them a check's box is given
// Gitlet's credential and base, as an executor's is; every name of both is
// in [ReservedSandboxEnv].
func ReservedCheckEnv() []string {
	return []string{"CREWLET_CHECK_REPO", "CREWLET_CHECK_SHA", "CREWLET_CHECK_BASE", "CREWLET_CHECK_NAME",
		"CREWLET_CHECK_ATTEMPT", "CREWLET_CHECK_API", "CREWLET_CHECK_SUMMARY"}
}

// reservedSandboxEnvReasons is why each of [ReservedSandboxEnv] is the
// engine's, as a refusal of an operator's value for it says.
var reservedSandboxEnvReasons = map[string]string{
	"CREWLET_CHECK_REPO": "it is the repository a check's box checks out, which the engine names for the " +
		"check it runs, and a value here would tell the check's own command another",
	"CREWLET_CHECK_SHA": "it is the commit a check measures, checked out in its box, and a value here would " +
		"tell the check's own command another commit than the one it builds",
	"CREWLET_CHECK_BASE": "it is the commit a check's commit is measured against, the base its diff is read " +
		"from, and a value here would have the check compare against another",
	"CREWLET_CHECK_NAME": "it is the name of the check a box runs, and a value here would tell its command it " +
		"is another check",
	"CREWLET_CHECK_ATTEMPT": "it is the attempt of its check a box runs, and a value here would tell its " +
		"command another attempt than the one its result is recorded under",
	"CREWLET_CHECK_API": "it is where a check's box reads its commit's files and diff without a clone, with " +
		"its own credential, and a value here would send those reads, and that credential, elsewhere",
	"CREWLET_CHECK_SUMMARY": "it is the file a check's command writes its summary to, read back when the " +
		"check ends, and a value here would have the summary written where it is never read",
	"CREWLET_GIT_ACCESS_FILE": "it is the file in the box holding the run's own credential on Gitlet, " +
		"minted for each of its launches on a node that serves it, and a value here would break its clones " +
		"and pushes",
	"CREWLET_GIT_URL": "it is the base a run clones Gitlet's repositories from and " +
		"pushes them to (CREWLET_BOX_GIT_URL on the launching node), and a value here would send its git " +
		"elsewhere",
}

// validateReservedSandboxEnv refuses a [ReservedSandboxEnv] name as a key of
// any env a box's environment is assembled from: a seat's `sandbox.env`, a
// seat's setup steps' `env`, `providers.sandbox.setup`'s, and
// `code.checks.env`.
//
// AN ADMISSION RULE, for [Company.validateHumanSeatCode]'s reason: running
// does not depend on it, since the engine's own value wins over every other
// source for the names it sets (sandbox.LaunchRequest.RunEnv; a check's
// own), so a stored revision carrying one runs as it did, a later build may
// relax the rule, and this build applies whatever its newer peer admitted.
// What it refuses is a setting that READS as working: the engine overwrites
// it wherever it sets the name, and elsewhere nothing of the engine's reads it.
func (c *Company) validateReservedSandboxEnv() error {
	var p problems
	check := func(path Path, env map[string]string) {
		for _, name := range ReservedSandboxEnv() {
			if _, set := env[name]; set {
				p.add(entry(path, name), ErrConflict, "%s is the engine's: %s. Remove it",
					name, reservedSandboxEnvReasons[name])
			}
		}
	}
	steps := func(path Path, list []SandboxSetupStep) {
		for i := range list {
			check(at(idx(path, i), "env"), list[i].Env)
		}
	}
	if c.Providers.Sandbox != nil {
		steps(field("providers.sandbox.setup"), c.Providers.Sandbox.Setup)
	}
	check(field("code.checks.env"), c.Code.Checks.Env)
	for role, path := range c.EachRole() {
		if gate := role.Sandbox; gate != nil {
			check(at(path, "sandbox.env"), gate.Env)
			steps(at(path, "sandbox.setup"), gate.Setup)
		}
	}
	return p.err()
}
