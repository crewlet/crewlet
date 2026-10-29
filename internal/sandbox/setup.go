package sandbox

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/redact"
)

// DefaultSetupTimeout is the budget for one setup command.
//
// Anchored to what provisioning actually is: a cold dependency install
// (apt-get install, npm ci, uv sync) over a network is typically one to five
// minutes, and a large monorepo clone or an image pull can reach ten. Ten
// minutes covers those while still failing well inside the box TTL
// (DefaultBoxTimeout, 900s), so a wedged command surfaces as a named setup
// failure rather than as a box the provider reaps out from under the run.
//
// It exists at all because provisioning is NOT a control-plane call: without
// its own budget these commands inherited the backend's control timeout —
// sized for a mkdir or a docker exec, not for work — so any real provisioning
// step was killed and failed the whole acquisition.
const DefaultSetupTimeout = 10 * time.Minute

// SetupStep is one declarative provisioning unit applied to a fresh sandbox.
//
// A coding agent's box needs environment wiring beyond the CLI itself: git
// auth, package-registry credentials, preinstalled toolchains, whatever the
// org's tasks demand. Rather than hardcoding each concern into a runner,
// provisioning is an ordered list of these — one unit each contributing files,
// commands, env, and a paragraph of brief.
//
// A launch's steps come from company config, applied in order:
// providers.sandbox.setup for every sandbox role, then role.sandbox.setup
// extras. FOR ANY OTHER HOST THE ENGINE SHIPS NONE OF ITS OWN — git auth
// included. What stays engine-side is only what static config cannot know,
// expressed generically: the run env carries the LLM credentials and the
// agent's identity as CREWLET_AGENT_HANDLE / CREWLET_AGENT_EMAIL, never a
// tool-specific variable. External tokens are declared in role.sandbox.env,
// and setup commands execute WITH the run env, so a recipe maps the generic
// facts into tool shape itself.
//
// The one exception is the engine's OWN code host, whose repositories a run
// is granted rather than told about: the steps that give a box those are the
// engine's ([GitVersionStep], [CloneRecipe]), carried apart from the
// operator's ([LaunchRequest.Clone]) and applied after them, and so is the
// git access they clone with — the run's credential and the box's git
// configuration ([BoxGit], ADR-0063), in the launch's own environment.
type SetupStep struct {
	// Name is a short identifier, used in logs and setup-failure errors.
	Name string `yaml:"name" json:"name"`

	// Files are written into the box (path → content) before Commands run,
	// byte for byte: each body arrives as the content it is, a whole
	// `${VAR}` already read by the engine at launch and any other `${…}`
	// left for whatever reads the file inside the box.
	Files map[string]string `yaml:"files,omitempty" json:"files,omitempty"`

	// Commands run in order after the files land. A non-zero exit fails the
	// sandbox acquisition — no silent half-provisioning.
	Commands []string `yaml:"commands,omitempty" json:"commands,omitempty"`

	// Env is merged into the coding agent's run env. ${VAR} references are
	// resolved exactly ONCE, with the rest of the sandbox env at launch.
	Env map[string]string `yaml:"env,omitempty" json:"env,omitempty"`

	// OptionalKeys names the keys of Env and the paths of Files whose value
	// may resolve to nothing at launch: the engine leaves such a variable
	// out and writes no such file, where any other reference that resolves
	// to nothing refuses a `run_sandbox` coding run (an agent-mode
	// executor's box starts without it, and the engine says so).
	OptionalKeys []string `yaml:"optional_keys,omitempty" json:"optional_keys,omitempty"`

	// Brief is the environment-context paragraph for the coding agent —
	// what this step made true about the box. Empty means nothing to tell.
	Brief string `yaml:"brief,omitempty" json:"brief,omitempty"`

	// TimeoutSeconds is how long each of this step's commands may run.
	// Zero takes [DefaultSetupTimeout]. Raise it for a step you know is
	// slow; lower it for one that should be instant, so a hung command
	// surfaces as a setup failure rather than eating the turn.
	TimeoutSeconds float64 `yaml:"timeout_seconds,omitempty" json:"timeout_seconds,omitempty"`
}

func (s SetupStep) timeout() time.Duration {
	if s.TimeoutSeconds > 0 {
		return time.Duration(s.TimeoutSeconds * float64(time.Second))
	}
	return DefaultSetupTimeout
}

// SetupError reports a setup step failing — the box is not usable as promised,
// so the acquisition fails and the caller tears the box down.
//
// Distinct from an install failure on purpose: a failed setup step is an
// OPERATOR CONFIG problem (a broken role or provider setup command), not a
// coding-agent install problem, and the two want different debugging.
//
// THREE FAILURES, AND THE MESSAGE SAYS WHICH. A step fails writing one of its
// files, or running a command that then exits non-zero, or running a command
// that reports no exit status at all — one the box could not start, or one
// whose box went away under it, which the message cannot tell apart and so
// does not claim either of: [SetupError.Detail] carries the box's own account.
// One sentence for all three named a refused file "command #0 failed (exit
// 0)" — a command that does not exist, failing with the status that means
// success — and a command with no exit status "failed (exit 0)" too, which
// sent the operator to read a command for the reason it exited cleanly.
type SetupError struct {
	Step string
	// File is the path a write into the box failed for. Set only when a
	// file is what failed, and Command is zero then.
	File string
	// Command is the failed command's 1-based position within the step.
	Command int
	// Exited is whether the command reported an exit status, which Exit
	// then holds — a signal that ended it as a shell reports one, 128 plus
	// its number. A command the box could not start has none, and neither
	// does one whose box went away before it reported.
	Exited bool
	Exit   int
	Detail string
}

func (e *SetupError) Error() string {
	var msg string
	switch {
	case e.File != "":
		msg = fmt.Sprintf("setup step %q could not write %s", e.Step, e.File)
	case e.Exited:
		msg = fmt.Sprintf("setup step %q command #%d failed (exit %d)", e.Step, e.Command, e.Exit)
	default:
		msg = fmt.Sprintf("setup step %q command #%d reported no exit status", e.Step, e.Command)
	}
	if e.Detail != "" {
		msg += " — " + e.Detail
	}
	return msg
}

// ApplySetup applies every step to a box: write files, run commands, in order —
// a fresh box's whole provisioning, or the clone steps a reused one is given
// ([LaunchRequest.Clone]), each of which skips a repository already there.
//
// env is the run env the coding agent will get — commands run WITH it so a
// recipe can reference the engine's identity facts and its own configured
// tokens at provisioning time.
//
// Returns a [SetupError] on the FIRST failure, a file or a command, so the
// caller tears the box down: the coding agent's brief promises this environment, so a partial
// application must never reach a run.
func ApplySetup(ctx context.Context, box Sandbox, steps []SetupStep, env map[string]string) error {
	for _, step := range steps {
		// Deterministic order: a step writing several files may depend on
		// the directory another created, and map iteration is randomised.
		paths := slices.Sorted(maps.Keys(step.Files))
		for _, path := range paths {
			if err := box.WriteFile(ctx, path, []byte(step.Files[path])); err != nil {
				return &SetupError{Step: step.Name, File: path, Detail: err.Error()}
			}
		}
		for i, cmd := range step.Commands {
			result, err := box.Exec(ctx, cmd, ExecOptions{
				Env:        env,
				TimeoutSec: step.timeout().Seconds(),
			})
			if err != nil {
				return &SetupError{Step: step.Name, Command: i + 1, Detail: err.Error()}
			}
			if result.ExitCode != 0 {
				// NOT the command text. The engine resolves nothing in a
				// command — the box's shell expands its `$VAR`s from the
				// run env — but a recipe may still spell a credential out
				// literally, and this message is logged AND handed back to
				// the LLM.
				// The step name plus the command's position identifies it
				// precisely, and the operator has the config; stderr is
				// redacted for the same reason a transcript is.
				return &SetupError{
					Step:    step.Name,
					Command: i + 1,
					Exited:  true,
					Exit:    result.ExitCode,
					Detail:  redact.Secrets(strings.TrimSpace(result.Stderr)),
				}
			}
		}
		log.DebugContext(ctx, "sandbox_setup_step_applied",
			"step", step.Name, "files", len(step.Files), "commands", len(step.Commands))
	}
	return nil
}

// EnvironmentBrief is the "## Your environment" block for the coding agent.
//
// A generic sandbox intro, then each step's brief paragraph (what the
// configured provisioning made true about the box — this is where a
// config-authored git-auth step tells the agent which token to use), then the
// connected MCP servers. The agent should not have to rediscover — or fail
// against — what its environment already provides.
//
// Mechanism and hint together: the steps make the environment work no matter
// how the agent reasons, and the brief stops it wasting rounds finding out.
func EnvironmentBrief(steps []SetupStep, mcpServers []string) string {
	lines := []string{
		"\n## Your environment",
		"You are running autonomously inside an isolated sandbox with your " +
			"own shell, filesystem, and a full developer toolchain (git, " +
			"language runtimes, build tools). Work directly here — there is no " +
			"other machine to set up.",
	}
	for _, step := range steps {
		if step.Brief != "" {
			lines = append(lines, step.Brief)
		}
	}
	if len(mcpServers) > 0 {
		names := slices.Clone(mcpServers)
		slices.Sort(names)
		lines = append(lines, "You also have these MCP tool servers connected: "+
			strings.Join(names, ", ")+".")
	}
	return strings.Join(lines, "\n")
}
