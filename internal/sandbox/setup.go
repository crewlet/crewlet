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

// DefaultSetupTimeout is the budget for one setup command: a cold dependency
// install over a network takes one to five minutes, a large clone or an image
// pull up to ten, and a wedged command fails as a named setup failure at its
// own budget. Provisioning is not a control-plane call, so it never takes the
// backend's control timeout, sized for a mkdir.
//
// It bounds one command, never the provisioning: a launch's steps together may
// outrun the orphan reaper's cutoff ([DefaultHostGrace]), since the launch
// keeps the box's stamp touched while it provisions ([keepProvisioning]) and
// only this budget ends a slow step.
const DefaultSetupTimeout = 10 * time.Minute

// SetupStep is one declarative provisioning unit applied to a fresh sandbox.
//
// A launch's steps come from company config, in order: providers.sandbox.setup,
// then role.sandbox.setup. The engine ships none, git auth included: it gives
// only what config cannot know, as generic run env (the LLM credentials,
// CREWLET_AGENT_HANDLE, CREWLET_AGENT_EMAIL; tokens come from role.sandbox.env),
// and commands run WITH that env, so a recipe maps it into its tool's shape.
//
// The exception is Gitlet, whose repositories a run is granted: the
// `code_checkout` granting one hands the run the engine's [CodingSteps] to run
// itself, never at launch, with [BoxGit]'s environment (ADR-0093) and the
// run's credential in the file it names ([GitAccessStep]).
type SetupStep struct {
	// Name is a short identifier, used in logs and setup-failure errors.
	Name string `yaml:"name" json:"name"`

	// Files are written into the box (path → content) before Commands run,
	// byte for byte: a body that is wholly one `${VAR}` was resolved at
	// launch, and any other `${…}` is left for whatever reads the file.
	Files map[string]string `yaml:"files,omitempty" json:"files,omitempty"`

	// Commands run in order after the files land. A non-zero exit fails the
	// sandbox acquisition: no silent half-provisioning.
	Commands []string `yaml:"commands,omitempty" json:"commands,omitempty"`

	// Env is merged into the harness's run env. ${VAR} references are
	// resolved exactly ONCE, with the rest of the sandbox env at launch.
	Env map[string]string `yaml:"env,omitempty" json:"env,omitempty"`

	// OptionalKeys names the keys of Env and paths of Files that may resolve
	// to nothing at launch: such a variable is left out and such a file not
	// written. Any other reference resolving to nothing is the run's gap,
	// which the engine reports.
	OptionalKeys []string `yaml:"optional_keys,omitempty" json:"optional_keys,omitempty"`

	// Brief is the environment-context paragraph for the harness: what this
	// step made true about the box. Empty means nothing to tell.
	Brief string `yaml:"brief,omitempty" json:"brief,omitempty"`

	// TimeoutSeconds is how long each of this step's commands may run; zero
	// takes [DefaultSetupTimeout]. Lower it for a step that should be
	// instant, so a hung command fails setup rather than eating the turn.
	TimeoutSeconds float64 `yaml:"timeout_seconds,omitempty" json:"timeout_seconds,omitempty"`

	// EveryBox runs the step in every box, never in the build a container
	// box's home is cached from ([Local.CreateProvisioned]): for a step that
	// needs a credential the cache's rules cannot see it read.
	EveryBox bool `yaml:"every_box,omitempty" json:"every_box,omitempty"`

	// Credentials marks the launch's own step of the integrations' box files,
	// which hold the seat's credentials: written into every box first, never
	// cached, and never ending the cached prefix of the steps after it.
	Credentials bool `yaml:"-" json:"-"`
}

func (s SetupStep) timeout() time.Duration {
	if s.TimeoutSeconds > 0 {
		return time.Duration(s.TimeoutSeconds * float64(time.Second))
	}
	return DefaultSetupTimeout
}

// SetupError reports a setup step failing: the box is not usable as promised,
// so the acquisition fails and the caller tears the box down. It is distinct
// from an install failure because a broken setup step is operator config, not
// the harness; [SetupError.why] classes it.
//
// The message names which of three failures it was: writing a file, a command
// exiting non-zero, or a command reporting no exit status (the box could not
// start it, or went away under it; the message claims neither, and
// [SetupError.Detail] carries the box's account). Never "failed (exit 0)" for
// a command that reported none or a file that was refused.
type SetupError struct {
	Step string
	// File is the path a write into the box failed for. Set only when a
	// file is what failed, and Command is zero then.
	File string
	// Command is the failed command's 1-based position within the step.
	Command int
	// Exited is whether the command reported an exit status, held in Exit
	// (a signal as a shell reports one, 128 plus its number). A command the
	// box could not start, or whose box went away, has none.
	Exited bool
	Exit   int
	Detail string

	// Err is the box's answer where it did not run the step to an exit
	// status (a write or a command it could not take), nil for a command
	// that exited; [WhyOf] reads it through [SetupError.Unwrap].
	Err error
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

// Unwrap is what the box answered, where it answered something.
func (e *SetupError) Unwrap() error { return e.Err }

// why classes a step a command of which EXITED non-zero as the recipe's to
// change, the step's own timeout (124) included: a command stopped by
// timeout_seconds is as likely an unreachable network as a slow one, and the
// budget is the operator's. A step the box never ran to an exit is not
// classed here; the box's own answer says why ([SetupError.Err]).
//
// Except [exitTempFail]: the recipe saying a download is failing on the far
// side after its own retries, which a wait clears ([WhySetupUnavailable]).
// Only the recipe can tell the two apart, and a step that downloads on every
// new box (the sandbox guide's stock-image recipe) would otherwise read a
// registry outage as a setup an administrator has to fix.
func (e *SetupError) why() (Why, bool) {
	if e.Exited && e.Exit == exitTempFail {
		return WhySetupUnavailable, true
	}
	return WhyConfiguration, e.Exited
}

// exitTempFail is the status a setup command exits with to say its failure
// is temporary: sysexits.h's EX_TEMPFAIL, so the convention is the shell's
// own, not this engine's. docs/concepts/code-sandbox.md tells a recipe when
// to use it.
const exitTempFail = 75

// ApplySetup applies every step to a box, files then commands, in order: a
// fresh box's provisioning, or a check box's git floor and checkout
// ([Manager.AcquireBox]). Commands run WITH env, the harness's run env, so a
// recipe can use the engine's identity facts and its own tokens.
//
// It returns a [SetupError] on the first failure so the caller tears the box
// down: the harness's brief promises this environment, so a partial
// application must never reach a run.
func ApplySetup(ctx context.Context, box Sandbox, steps []SetupStep, env map[string]string) error {
	for _, step := range steps {
		// Deterministic order: a step writing several files may depend on
		// the directory another created, and map iteration is randomised.
		paths := slices.Sorted(maps.Keys(step.Files))
		for _, path := range paths {
			if err := box.WriteFile(ctx, path, []byte(step.Files[path])); err != nil {
				return &SetupError{Step: step.Name, File: path, Detail: err.Error(), Err: err}
			}
		}
		for i, cmd := range step.Commands {
			result, err := box.Exec(ctx, cmd, ExecOptions{
				Env:        env,
				TimeoutSec: step.timeout().Seconds(),
			})
			if err != nil {
				return &SetupError{Step: step.Name, Command: i + 1, Detail: err.Error(), Err: err}
			}
			if result.ExitCode != 0 {
				// Not the command text: a recipe may spell a credential out
				// literally, and this message is logged AND handed to the
				// LLM. The step and position identify the command; stderr
				// is redacted for the same reason a transcript is.
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

// EnvironmentBrief is the "## Your environment" block for the harness: a
// generic sandbox intro, each step's brief (where a config-authored git-auth
// step names the token to use), then the connected MCP servers by name, and a
// line for each that says what it is for ([MCPServer.Description]), then a
// browser seat's browserBrief ([BrowserOutcome.Brief]). The steps make the
// environment work however the agent reasons; the brief saves it rounds
// rediscovering, or failing against, what is already provided.
func EnvironmentBrief(steps []SetupStep, servers map[string]MCPServer, browserBrief string) string {
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
	if len(servers) > 0 {
		names := slices.Sorted(maps.Keys(servers))
		lines = append(lines, "You also have these MCP tool servers connected: "+
			strings.Join(names, ", ")+".")
		for _, name := range names {
			if description := strings.TrimSpace(servers[name].Description); description != "" {
				lines = append(lines, "`"+name+"`: "+description)
			}
		}
	}
	if b := strings.TrimSpace(browserBrief); b != "" {
		lines = append(lines, b)
	}
	return strings.Join(lines, "\n")
}
