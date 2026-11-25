package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/steer"
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
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/node"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/panics"
	"github.com/crewlet/crewlet/internal/providers/llm"
	llmchain "github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/schedule"
	seatpkg "github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/tracing"
	"github.com/crewlet/crewlet/internal/words"
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

// buildSandbox constructs the manager for a company, or nil when no seat's
// executor reaches a box.
//
// Nil is an ordinary outcome only for a company with no agent seat that can
// launch one — config refuses an agent seat with an executor and no
// providers.sandbox — and, until its first entry, for one with no models. What
// is NOT ordinary is a configured provider that cannot be constructed: that
// fails the apply, because the alternative publishes a company whose seats
// cannot run a single turn.
//
// play is what a `providers.sandbox.fake` company's runs do ([fakeRunners]);
// nil plays every run as the double's own demonstration.
func buildSandbox(c *config.Company, env *config.Resolver, otel *sandbox.OtelReceiver,
	play fakePlayer) (*sandbox.Manager, error) {
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
	runners := sandboxRunners()
	if spec.Fake {
		runners = fakeRunners(play)
	}
	return sandbox.NewManager(sandbox.ManagerOptions{
		Providers:        providers,
		DefaultPlacement: sandbox.Placement(spec.RunIn()),
		Runners:          runners,
		HostGrace:        spec.HostGrace(),
		DefaultPauseTTL:  secondsPtr(spec.PauseTTL()),
		DefaultMaxTurns:  spec.MaxTurns(),
		AttributeCommits: spec.AttributesCommits(),
		DefaultSetup:     setupSteps(spec.Setup),
		Telemetry:        otel,
	})
}

// sandboxRunners is every harness this build can drive in a box: the runners
// registered ([executor.Registered]), the one list `crewlet llm doctor` and
// config read too (executor.Names, config.ExecutorHarnesses), held together by
// the executor's own test, so config never passes a chain the engine refuses at
// the seat's first launch.
func sandboxRunners() map[string]sandbox.Runner {
	runners := map[string]sandbox.Runner{}
	for _, h := range executor.Registered() {
		runners[h.Name()] = executor.New(h)
	}
	return runners
}

// Every runner [sandboxRunners] registers is offered for a save as its job
// works; whether its harness takes one is its own Saves.
var _ sandbox.SessionSaver = (*executor.Runner)(nil)

// fakePlayer is what a `providers.sandbox.fake` run does: called once per run
// with the request it was started with, answering its result. See
// [sandbox.FakeRunner.Player].
type fakePlayer = func(ctx context.Context, req sandbox.RunRequest) sandbox.Result

// fakeRunners is every runner [sandboxRunners] registers, as the in-process
// double, each playing its runs with play.
//
// THE SAME NAMES, because the double stands in for the box and not for the
// choice of harness: a fake company's executor chains pick their member exactly as
// a real one's do, and only what the run does differs. And A RUN THAT
// FINISHES, because a double whose job never ended would park every turn of a
// demonstration company for good — the double's own runner is what completes
// it ([demoRun], unless a test scripts the run).
func fakeRunners(play fakePlayer) map[string]sandbox.Runner {
	if play == nil {
		play = demoRun
	}
	real := sandboxRunners()
	runners := make(map[string]sandbox.Runner, len(executor.Names()))
	for _, name := range executor.Names() {
		runner := sandbox.NewFakeRunner(name)
		runner.Player = play
		// WHAT THE HARNESS'S RUNNER STATES, so a fake run is read as its harness's is.
		runner.Thread = sandbox.ThreadTraitsOf(real[name])
		if r, ok := real[name].(*executor.Runner); ok {
			runner.Pin, runner.NotesOff, runner.Fresh = r.PinnedWindow, r.NotesOffWhy(), r.StartsFresh()
			runner.DeferAt = r.DefersMCPAt()
		}
		runners[name] = runner
	}
	return runners
}

// demoRun is what a demonstration company's run does: nothing, and its report
// says so. The turn then collects a run that submitted nothing, and is judged
// on that record like any run that ended without saying what it did.
func demoRun(_ context.Context, _ sandbox.RunRequest) sandbox.Result {
	return sandbox.Result{
		Success: true,
		Text: "This run was played by providers.sandbox.fake, the in-process " +
			"double: no harness ran and no tool was called.",
	}
}

// buildSandboxProviders turns the catalogue into one backend per placement the
// company reaches (`local:` and the double each serve both cells).
//
// THE CLOSED SET AND THIS FUNCTION ARE THE SAME LIST, asserted by a test: a
// default naming a backend with no case here validates cleanly and fails at the
// first launch.
//
// ONE LOCAL BACKEND PER PLACEMENT, not one shared: a single instance would
// have to be told which cell it serves on every call, a block-wide mode in
// disguise.
func buildSandboxProviders(spec *config.SandboxProvider, env *config.Resolver, reached map[config.Placement]string) (map[sandbox.Placement]sandbox.Provider, error) {
	built := make(map[sandbox.Placement]sandbox.Provider, len(reached))
	// WALKED IN THE CLOSED SET'S ORDER, not the map's: a map iterates
	// randomly, and an error naming whichever backend happened to come
	// first would differ between two runs of the same broken config.
	for _, placement := range config.Placements {
		if _, want := reached[placement]; !want {
			continue
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
	case config.PlacementDirect, config.PlacementContainer:
		return sandbox.NewLocal(localOptions(spec, env, placement))
	default:
		// UNREACHABLE THROUGH A PARSED CONFIG, because the closed set and
		// this switch are the same list.
		//
		// Answered rather than panicked: an embedder building a spec by
		// hand is a caller, not an operator to be crashed at.
		return nil, fmt.Errorf("providers.sandbox: %q is not one of %s",
			placement, words.Or(config.Placements))
	}
}

// localOptions maps the local block onto the backend's options for one cell,
// with the manager's host grace as the orphan reaper's cutoff
// ([sandbox.LocalOptions.HostGrace]), so the two never disagree.
//
// runtime_env IS RESOLVED HERE, at the edge, as an MCP server's `env:` is
// ([resolveServer]): Tier B stores its references verbatim, so a backend handed
// the block's map would put the literal "${SSH_AUTH_SOCK}" into the runtime's
// environment, and the sandbox package must not be able to read the secret
// store to do better.
func localOptions(spec *config.SandboxProvider, env *config.Resolver, placement config.Placement) sandbox.LocalOptions {
	local := spec.Local
	return sandbox.LocalOptions{
		Placement:  sandbox.Placement(placement),
		StateDir:   local.StateDir,
		Image:      local.Image,
		Runtime:    string(local.Runtime),
		Network:    local.Network,
		RunArgs:    local.RunArgs,
		RuntimeEnv: resolveMap(env, local.RuntimeEnv),
		HostGrace:  spec.HostGrace(),
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

// runEnv is one launch's environment and provisioning, read through this
// node's resolver by the rule every tool server's values are
// ([resolveServer]): every reference resolved, or why the run lacks it.
type runEnv struct {
	// env is the run environment the box's setup commands and its harness
	// are handed — the engine's own facts and the configuration's —
	// before the LLM credentials and the telemetry are laid over it.
	env map[string]string

	// defaults is the integrations' box settings no layer above sets, each
	// resolved: laid beneath the entry's env too, and never judged as a
	// sign-in ([newExecutorRun]).
	defaults map[string]string

	// credentials is the seat's own integration credentials the engine sets
	// in its box ([seataccess.BoxSet.Credentials]), each resolved: laid over
	// every other layer, never judged as a sign-in, and never under a name
	// the harness signs in by ([newExecutorRun]).
	credentials map[string]string

	// homePaths is each variable naming one of the integrations' box files
	// ([seataccess.BoxFile.PathEnv]) no layer above sets, to the file's path
	// under the box's home, which the launch joins on lowest of all.
	homePaths map[string]string

	// tools is the names of env that hand a tool in the box one of the
	// seat's own vendor credentials ([config.Company.SeatCredentialVariables]),
	// doctl's DIGITALOCEAN_ACCESS_TOKEN set to the seat's sealed token: in the
	// box like the rest of env, and never judged as a sign-in
	// ([newExecutorRun]).
	tools []string

	// setup is the provider-wide steps then the seat's own, every file's
	// body read as the content it is.
	setup []sandbox.SetupStep

	// secrets is what of env and setup sits in a credential position by
	// where it came from: every value a reference resolved to — the
	// engine's own identity facts aside — and every setup file whose body
	// was one reference whole ([sandbox.LaunchRequest.Secrets],
	// [launchSecrets]).
	secrets []string

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

// runEnvironment is one launch's environment and provisioning: the setup steps
// and the run environment. Nil-safe on the seat's settings and on c.
//
// The engine adds no credential but the integrations' ([seataccess.BoxSet.Credentials]),
// which win a name over any layer ([BoxCredentialWarnings]). Precedence, later
// winning: identity, setup steps, the seat's env. Gitlet access (ADR-0093) joins
// the LAUNCH's environment once the store names the job.
//
// RESOLVED EXACTLY ONCE, here: Tier B keeps references verbatim, and resolving
// at load too would mangle a secret containing a literal ${...}. An unresolved
// reference is a gap unless `optional_keys` lists it (toolcredentials.go); an
// identity fact is left out and SAID. NEW MAPS, never the steps' own.
func (e *Engine) runEnvironment(ctx context.Context, c *Company, seat *org.Role,
	settings *org.RoleSandbox, defaults []sandbox.SetupStep, boxDefaults []seataccess.BoxDefault) runEnv {

	// THE SEAT'S OWN RESOLUTION, so what it is issued at use is answered
	// here and nowhere else, good for [runTokenLife]: a run's environment is
	// fixed when its box starts. See issued.go.
	resolver, token := e.seatResolver(ctx, c, seat, runTokenLife)
	setup := boxSetup(defaults, settings)

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
	if settings != nil {
		for key, value := range settings.Env {
			merged[key] = layered{value: value, path: "role.sandbox.env." + key,
				where: "role.sandbox.optional_keys", optional: slices.Contains(settings.OptionalKeys, key)}
		}
	}

	// THE INTEGRATIONS' CREDENTIALS TAKE THEIR NAMES from every layer above,
	// and their files their paths from every step.
	owned := map[string]bool{}
	for _, d := range boxDefaults {
		switch {
		case d.Credential:
			delete(merged, d.Env)
		case d.File != nil && d.File.Credential():
			owned[boxPath(d.File.Path)] = true
		}
	}

	var mint *seatMint
	if c != nil {
		mint = &seatMint{kinds: e.mintedFor(c, seat.Handle()), token: token}
	}
	gap := refGap{env: resolver, mint: mint}
	facts := refGap{env: resolver, mint: mint}
	var mine map[string]string
	if c != nil {
		mine = c.Config.SeatCredentialVariables(seat.Handle())
	}
	out := runEnv{env: make(map[string]string, len(merged))}
	for _, key := range slices.Sorted(maps.Keys(merged)) {
		l := merged[key]
		missing := resolver.Missing(l.value)
		switch {
		case len(missing) == 0:
			out.env[key] = resolver.Value(l.value)
			if !l.fact {
				out.secrets = append(out.secrets, referencedValues(resolver, l.value)...)
			}
			if slices.ContainsFunc(envref.Names(l.value), func(name string) bool { return mine[name] != "" }) {
				out.tools = append(out.tools, key)
			}
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
			if owned[boxPath(path)] {
				continue
			}
			body := step.Files[path]
			content, unresolved := secrets.ReadContent(body, resolver.LookupOK)
			if name, whole := envref.Whole(body); whole && content == "" {
				// SET TO NOTHING READS AS NOTHING, as an env value does:
				// an empty credential file is no credential.
				unresolved = name
			}
			if unresolved == "" {
				files[path] = content
				if _, whole := envref.Whole(body); whole {
					// THE REFERENCE'S VALUE, byte for byte: a deploy key, a
					// token file. A body written in the document is its
					// own words, expanded in the box from the environment.
					out.secrets = append(out.secrets, content)
				}
				continue
			}
			if gap.hold(path, []string{unresolved}, slices.Contains(step.OptionalKeys, path), where, false) {
				out.files = append(out.files, step.Name+":"+path)
			}
		}
		out.setup[i].Files = files
	}

	// THE SEAT'S OWN CREDENTIALS, kept apart for the launch to lay over the
	// rest, scrubbed, and a gap where one does not resolve, as the line a
	// person once wrote for it was: waiting while its integration mints it.
	for _, d := range boxDefaults {
		if !d.Credential {
			continue
		}
		if missing := resolver.Missing(d.Value); len(missing) > 0 {
			if gap.hold(d.Env, missing, false, "", false) {
				out.keys = append(out.keys, "integrations."+string(d.Kind)+"."+d.Env)
			}
			continue
		}
		if out.credentials == nil {
			out.credentials = map[string]string{}
		}
		out.credentials[d.Env] = resolver.Value(d.Value)
		out.secrets = append(out.secrets, referencedValues(resolver, d.Value)...)
	}

	// THE INTEGRATIONS' BOX DEFAULTS, kept apart from the run's own: a name any
	// layer here sets is that layer's, and one that does not resolve is left
	// out and said like an identity fact, never a gap.
	for _, d := range boxDefaults {
		if _, set := merged[d.Env]; set || d.File != nil || d.Credential {
			continue
		}
		if missing := resolver.Missing(d.Value); len(missing) > 0 {
			facts.hold(d.Env, missing, false, "", false)
			out.facted = append(out.facted, "integrations."+string(d.Kind))
			continue
		}
		if out.defaults == nil {
			out.defaults = map[string]string{}
		}
		out.defaults[d.Env] = resolver.Value(d.Value)
	}

	// AND THEIR FILES, written ahead of every step as one of their own, never
	// into the environment, so none is judged as a sign-in. A setting file at
	// a path a step writes is that step's, a credential file the
	// integration's ([suppliedFiles]), and a file whose reference does not resolve is
	// left out and said as a default is; the poll writes a rewritten one
	// once its credential is issued ([Engine.refreshTokenFiles]).
	var supplied map[string]string
	for _, d := range suppliedFiles(setup, boxDefaults) {
		content, unresolved := secrets.ReadContent(d.File.Ref, resolver.LookupOK)
		if name, whole := envref.Whole(d.File.Ref); whole && content == "" {
			unresolved = name
		}
		// ITS VARIABLE NAMES A FILE THE BOX HOLDS: one written here, or a
		// rewritten one the poll writes once issued. A name a layer sets is its.
		if _, set := merged[d.File.PathEnv]; d.File.PathEnv != "" && !set && (unresolved == "" || d.File.Rewritten) {
			if out.homePaths == nil {
				out.homePaths = map[string]string{}
			}
			out.homePaths[d.File.PathEnv] = d.File.Path
		}
		if unresolved != "" {
			facts.hold(d.File.Path, []string{unresolved}, false, "", false)
			out.facted = append(out.facted, "integrations."+string(d.Kind))
			continue
		}
		if supplied == nil {
			supplied = map[string]string{}
		}
		supplied[d.File.Path] = content
		out.secrets = append(out.secrets, content)
	}
	if supplied != nil {
		out.setup = append([]sandbox.SetupStep{{Name: integrationFilesStep, Files: supplied}}, out.setup...)
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

// integrationFilesStep names the setup step a launch writes the integrations'
// box files in ([seataccess.BoxFile]), ahead of every step a person writes.
const integrationFilesStep = "integration-files"

// suppliedFiles are the files among defaults ([seataccess.BoxDefault.File])
// the box is given: every credential file, which is the integration's, and
// every setting file whose path no step among steps writes, which is then the
// person's. ONE ANSWER for the launch that writes them and the poll that keeps
// the rewritten ones current.
func suppliedFiles(steps []sandbox.SetupStep, defaults []seataccess.BoxDefault) []seataccess.BoxDefault {
	written := map[string]bool{}
	for _, step := range steps {
		for path := range step.Files {
			written[boxPath(path)] = true
		}
	}
	var out []seataccess.BoxDefault
	for _, d := range defaults {
		if d.File != nil && (d.File.Credential() || !written[boxPath(d.File.Path)]) {
			out = append(out, d)
		}
	}
	return out
}

// boxPath is path as a box writes it, relative to its home: `~/` names the
// home in every cell, so `~/.npmrc` and `.npmrc` are one file.
func boxPath(path string) string { return strings.TrimPrefix(path, "~/") }

// boxSetup is the setup steps one seat's box is provisioned with: the
// provider-wide ones, then the seat's own. ONE COMPOSITION for the launch and
// for the poll that keeps a running box's issued credentials current
// ([Engine.refreshRunTokens]), so the files one provisions are the files the
// other keeps.
func boxSetup(defaults []sandbox.SetupStep, settings *org.RoleSandbox) []sandbox.SetupStep {
	setup := slices.Clone(defaults)
	if settings != nil {
		setup = append(setup, seatSetupSteps(settings.Setup)...)
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
	if r.gap.waiting() || r.gap.reading() {
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
		plural(r.facted, "fact", "facts")+" from "+asWritten(r.facted)+": "+r.facts.reason())
	if r.facts.waiting() || r.facts.reading() {
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

// sandboxAccountant post-charges a collected launch against the shared
// counters.
//
// The charge happens AFTER the spend, so it cannot refuse: a refusal cannot
// un-spend a run, and recording it keeps the meter true when the cap binds.
// Hence [coord.Budgets.PostCharge], never the gate, whose refusal would stamp
// the seat's window as turning a round away for a charge nobody was deciding.
//
// The run is counted in the windows of COLLECTION on the company's clock. Once
// per launch ([sandbox.PendingRun.Charged]) and a charge that reached the
// company but not the seat ([coord.SeatUncountedError]) are the coordinator's.
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
// NO PANIC LEAVES THIS FRAME, for the reason [Dispatcher.Dispatch] gives: an
// unrecovered one skips the coordinator's revert and settle, stranding the row
// in resumed while a completion is redelivered and refused.
//
// A PANIC AFTER THE TURN BEGAN ENDS THE TURN ON THE EXECUTOR LOG: the frame that
// owes the end is what panicked, so it is owed here, failed ([panickedEnding]).
// One BEFORE the turn began is the coordinator's to announce as a loss
// ([sandbox.CoordinatorOptions.TurnLost]).
func (r *resumer) Resume(ctx context.Context, req sandbox.ResumeRequest) error {
	return r.engine.resumeGuarded(ctx, req, r.resume)
}

// resumeGuarded is [resumer.Resume] over the resume body run: run guarded
// ([Engine.guardResume]), and a panic once its turn began ended on the executor
// log as failed ([panickedEnding]).
func (e *Engine) resumeGuarded(ctx context.Context, req sandbox.ResumeRequest,
	run func(context.Context, sandbox.ResumeRequest) error) error {

	var began atomic.Bool
	if begin := req.Begin; begin != nil {
		req.Begin = func(ctx context.Context, carry sandbox.ResumeCarry) error {
			if err := begin(ctx, carry); err != nil {
				return err
			}
			began.Store(true)
			return nil
		}
	}
	err := e.guardResume(ctx, req.Run, func() error { return run(ctx, req) })
	var panicked *turn.PanicError
	if began.Load() && errors.As(err, &panicked) {
		e.oweExecutorEnd(ctx, req.Run, panickedEnding(panicked))
	}
	return err
}

// panickedEnding is how a resumed turn that panicked once it had begun ended:
// failed, saying it panicked and with what — redacted, since a panic's value
// can quote what a tool or a box said, and fit as the log keeps it
// ([executorkeep.Excerpt]). The stack stays in the log.
func panickedEnding(panicked *turn.PanicError) executorkeep.Ending {
	return executorkeep.Ending{Outcome: executorkeep.OutcomeFailed,
		Failure: executorkeep.Excerpt(fmt.Sprintf("the resumed turn panicked and was abandoned: %v",
			panicked.Value), executorkeep.FailureMaxBytes)}
}

// resume is [resumer.Resume]'s body, separated so the recovery around it is
// one deferred call.
func (r *resumer) resume(ctx context.Context, req sandbox.ResumeRequest) error {
	state, ok, err := execstate.Decode(req.Run.ExecuteState)
	if err != nil {
		// A RETIRED STATE NO LIVE NODE READS IS SETTLED: no build ever will.
		unresumable, judgeErr := r.engine.retiredState(ctx, err)
		if judgeErr != nil {
			log.WarnContext(ctx, "sandbox_parked_run_unjudged", "turn_id", req.Run.TurnID,
				"error", judgeErr.Error(), "detail", "whether any build can resume this run "+
					"could not be read, so the resume goes back to be tried again")
		}
		if unresumable != nil {
			return unresumable
		}
		// A VERSION THIS BUILD DOES NOT READ is a ROUTING failure, not a run
		// failure: a peer on the version that wrote it can resume this, so
		// the completion must go back rather than be settled here. Bytes
		// that are no state at all are met again on every node.
		if errors.Is(err, execstate.ErrUnknownVersion) {
			return fmt.Errorf("%w: %w", sandbox.ErrResumeElsewhere, err)
		}
		return fmt.Errorf("%w: %w", sandbox.ErrResumeUnavailable, err)
	}
	if !ok {
		return fmt.Errorf("%w: run %s has no parked turn state",
			sandbox.ErrResumeUnavailable, req.Run.TurnID)
	}
	if req.Asked != nil {
		// AN ANSWER TO A QUESTION ABOUT A FINISHED ITEM IS MOOT: the run is
		// settled and the reply goes back to the seat as the message it is.
		finished, err := r.engine.itemFinished(ctx, req.Run)
		if err != nil {
			return fmt.Errorf("%w: %w", sandbox.ErrResumeUnavailable, err)
		}
		if finished != nil {
			return finished
		}
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
			sandbox.ErrResumeElsewhere, req.Run.TurnID)
	}
	seat := company.Org.AgentSeatByHandle(req.Run.AgentHandle)
	if seat == nil {
		// The seat is gone from this epoch — decommissioned, or this node
		// is on a revision that never had it. Either way the resume belongs
		// somewhere else.
		return fmt.Errorf("%w: seat %q is not in this node's company",
			sandbox.ErrResumeElsewhere, req.Run.AgentHandle)
	}
	return r.engine.resumeTurn(ctx, resumeInput{
		Company:      company,
		Run:          req.Run,
		Bridged:      req.Bridged,
		BridgedRead:  req.BridgedRead,
		Leads:        req.Leads,
		State:        state,
		Turn:         resumedTurn(req.Run, seat, company.Org, r.engine.rowHolds(req.Run)),
		Answer:       req.Answer,
		Success:      req.Success,
		Trigger:      req.Trigger,
		Account:      accountOf(req),
		InputTokens:  req.InputTokens,
		OutputTokens: req.OutputTokens,
		CacheRead:    req.CacheReadTokens,
		CacheWrite:   req.CacheWriteTokens,
		Begin:        req.Begin,
		Engine:       req.Engine,
		Ended:        req.Ended,
		Asked:        req.Asked,
		Lost:         req.Lost,
	})
}

// runAccount is what a resume carries of the collected run's delivery: the
// refs its report named and what its push record says it pushed — the
// resumed executor's phase record lists both ([runRecordOf]). Zero when a
// person's answer resumed a parked clarification: nothing was collected. The
// run's cost is on its launch's own `executor_collected` record instead — see
// [sandbox.ResumeRequest.DeliveredRefs].
type runAccount struct {
	// DeliveredRefs are what the run reported, and RefsElided how many more
	// than it lists — see [sandbox.ResumeRequest.DeliveredRefsElided].
	DeliveredRefs []string
	RefsElided    int
	// Pushed, PushedElided and PushedUnread are what the run's push record
	// says it moved on Gitlet, as the record carries them — see
	// [sandbox.ResumeRequest.Pushed].
	Pushed       []types.PushedRefRecord
	PushedElided int
	PushedUnread string
}

// accountOf is req's account of the run it collected.
func accountOf(req sandbox.ResumeRequest) runAccount {
	return runAccount{
		DeliveredRefs: req.DeliveredRefs, RefsElided: req.DeliveredRefsElided,
		Pushed: sandbox.PushedRecords(req.Pushed), PushedElided: req.PushedElided,
		PushedUnread: req.PushedUnread,
	}
}

// runRecordOf is the record the resumed executor's phase carries of the run
// it collected: which box did the work, under which entry — off the run's own
// row, because this process may not be the one that launched it, and it is
// the only thing that can say a phase ran in a sandbox rather than in this
// process's own tool loop — and its account of what it delivered.
//
// ONE FUNCTION FROM THE ROW AND THE ACCOUNT TO THE RECORD, so a field the
// account gains reaches the record or fails a test here
// (TestAResumedRecordCarriesTheRunsWholeAccount) rather than a reader of the
// dashboard.
func runRecordOf(run sandbox.PendingRun, account runAccount) runner.RunRecord {
	return runner.RunRecord{
		Harness:   run.Harness,
		SandboxID: run.SandboxID,
		// AND WHICH ENTRY RAN IT, as the launch recorded the member it
		// picked: the executor's record and its spend are charged to
		// that entry and model, which a fallback makes other than the
		// chain's head, and which the chain resolved now could name
		// differently again.
		ProviderKey:   run.Launch.ProviderKey,
		Model:         run.Launch.Model,
		LaunchID:      run.LaunchID,
		DeliveredRefs: account.DeliveredRefs,
		RefsElided:    account.RefsElided,
		Pushed:        account.Pushed,
		PushedElided:  account.PushedElided,
		PushedUnread:  account.PushedUnread,
		// AND THE REPLIES ITS ROUND'S QUESTIONS GOT THAT WERE REFUSED as too
		// large to hand the run, for the round's reviewer, which reads the
		// round from its calls and finds none of them there (ADR-0114).
		RefusedReplies: refusedRepliesOf(run.RoundRefusals),
	}
}

// refusedRepliesOf is the round's refused replies as the runner carries them to
// its reviewer: figures alone, as the row keeps them.
func refusedRepliesOf(refusals []sandbox.RoundRefusal) []turn.RefusedReply {
	if len(refusals) == 0 {
		return nil
	}
	out := make([]turn.RefusedReply, 0, len(refusals))
	for _, r := range refusals {
		out = append(out, turn.RefusedReply{Messages: r.Messages, Bytes: r.Bytes, MaxBytes: r.MaxBytes})
	}
	return out
}

// resumedTurn is the turn a parked run is re-entered as.
//
// THE SAME RUN AND UNIT OF WORK the suspended turn had, read off the row, so its
// writes stay idempotent against the first half's trigger. THE START, NOT WHERE
// THE RESUMED HALF MINTS: every attempt decides that afresh against its own
// clock ([Engine.describeResume], [rebaseFor]). The SAME HOLDS, read off the row
// with its writer attached ([Engine.rowHolds]), so a new hold is on the row
// before its request.
func resumedTurn(run sandbox.PendingRun, seat *org.Role, organization *org.Organization,
	holds *turnctx.Holds) *turnctx.Turn {
	return &turnctx.Turn{
		RunID: run.TurnID, WorkKey: run.WorkKey, WorkSince: run.WorkSince,
		Seat: seat, Org: organization,
		Depth: run.DelegationDepth, Chain: run.DelegationChain,
		Holds: holds,
	}
}

// parkedRunContext is the context a parked run's turn runs under on this node:
// the resume that collects it, and a run taken over while it works
// ([Engine.reopenAgentRun]).
//
// ITS TRACE, continued from the ids on the run's row — the suspended span
// reconstructed as a remote parent, since no span survives a suspend — and
// ITS SEAT, bound for every model call the turn makes ([llm.WithSeat]) exactly
// as the dispatch path binds it for a fresh turn. A harness keeps a home of
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
		if panicked := turn.Recovered(panics.ScopeTurn, recover()); panicked != nil {
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
	// Bridged is the run's bridged tool log as the resume read it
	// ([sandbox.ResumeRequest.Bridged]); empty on a close that judges no
	// calls.
	Bridged sandbox.BridgeLog
	// BridgedRead is that read ([sandbox.ResumeRequest.BridgedRead]).
	BridgedRead sandbox.LogRead
	// Leads is the turn's leads as the coordinator read them for the resume
	// ([sandbox.ResumeRequest.Leads]), or said it could not
	// ([sandbox.TurnLeads.Unread]); zero on a close or a reopening, which
	// tells no lead.
	Leads sandbox.TurnLeads

	State   execstate.State
	Turn    *turnctx.Turn
	Answer  string
	Success bool
	Trigger *events.Event

	// Account is what the collected run delivered, for the resumed phase's
	// own event — see [runAccount].
	Account runAccount

	// InputTokens and OutputTokens are the resumed job's tokens, which this
	// segment's charge to the turn's work item includes — see
	// [sandbox.ResumeRequest.InputTokens].
	InputTokens  int
	OutputTokens int
	// CacheRead and CacheWrite are InputTokens' cached share — see
	// [sandbox.ResumeRequest.CacheReadTokens].
	CacheRead  int
	CacheWrite int

	// Begin is the coordinator's commit, called once the segment is certain
	// to run and before any of it does, with what the segment does with the
	// round of the job it resumes from — see [sandbox.ResumeRequest.Begin].
	// Nil commits nothing.
	Begin func(ctx context.Context, carry sandbox.ResumeCarry) error

	// Engine is what the engine spent on the job between segments — its
	// bridged calls and the condensation of its collection — which this
	// segment's charge includes too: see [sandbox.ResumeRequest.Engine].
	Engine sandbox.EngineSpend

	// Ended is how the collected run's harness said it ended, verbatim — see
	// [sandbox.ResumeRequest.Ended] and [Engine.noteRunEnded].
	Ended sandbox.RunEnding

	// Asked is the question a person's answer resumes the run from, which
	// the turn carries the run on with — see [sandbox.ResumeRequest.Asked].
	Asked *sandbox.AskedRun

	// Lost is the loss a lost job's round is carried on from — see
	// [sandbox.ResumeRequest.Lost].
	Lost *sandbox.LostRun

	// Rotated is set by the resume itself, never by the collection: the
	// run ended refused on a sign-in replaced while it ran, so the turn
	// carries its round on once with the new one ([Engine.signInReplaced]).
	Rotated *runner.Rotated

	// Refused is set by the resume itself too: the run's provider refused
	// the request it made in a session it continued, so the turn carries its
	// round on once in a fresh session ([Engine.requestRefused]).
	Refused *runner.Refused
}

// resumeTurn re-enters a suspended turn.
//
// THE SAME TURN, not a new one: same id, trigger and ledger. Everything LIVE
// (budget meter, phase recorder, config pin) is rebuilt, since it belongs to the
// node doing the resuming.
//
// The turn pins the epoch live AT RESUME, not the one it suspended under: a run
// parked for days would otherwise re-enter a revision since deleted. The
// accepted cost, a turn seeing a config change across its suspension, is why
// suspension is a phase boundary rather than a mid-round pause.
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

	// THE INDICATOR A RESUMED TURN SHOWS comes from one of two places, never both
	// ([Engine.resumeWorkingStatus] owns the order). REJOIN FIRST: a BOX'S
	// COMPLETION resume has nothing to raise from, since the suspended half's
	// keepAlive left the hold up under this turn id and the row has no chat
	// metadata (partition keys are not channel addresses). BEGIN FROM THE TRIGGER
	// WHERE NO HOLD IS LEFT: a PERSON'S ANSWER, whose park released the hold and
	// whose answer is an ordinary chat message. Nil where neither holds: the old
	// indicator lapses on the backend's expiry; nothing here invents a thread.
	status, rejoined := e.resumeWorkingStatus(ctx, in.Turn.Handle(), in.Run.TurnID, in.Trigger)
	// TRUE UNTIL THE TURN ACTUALLY RUNS, BUT ONLY ON ONE OF THE TWO ROUTES.
	// Every early return is a RETRY, and the claim reverts to its source status:
	//   - A BOX'S COMPLETION comes round again and nothing else could re-raise the
	//     indicator, so it is KEPT.
	//   - A PERSON'S ANSWER goes back to owing its resume. Nothing is working, and
	//     an indicator would tell the one person who could move it that the run is
	//     busy with their reply. So it is CLEARED, only here: the coordinator's
	//     revert reports no stop since on the completion route the box still
	//     works ([sandbox.Coordinator.unclaim]).
	// THE ANSWER IS NOT LOST: it is recorded first and retried
	// ([sandbox.Coordinator.TryResumeFromAnswer]), never run as an ordinary turn.
	working := rejoined
	defer func() { endWorkingStatus(ctx, status, working) }()

	company := in.Company
	if company == nil {
		// Not a second read of the epoch, which is the one thing this
		// must not do (see [resumeInput.Company]). Handed back rather
		// than settled: a caller that assembled a resume without its
		// epoch is a defect here. A defect of this build, which every
		// node of it meets, so it is no resume another node can make
		// ([sandbox.ErrResumeElsewhere]), and the give-up counts it.
		return fmt.Errorf("%w: run %s was handed to resumeTurn without the "+
			"company its seat was resolved in", sandbox.ErrResumeUnavailable, in.Run.TurnID)
	}
	// THE REVISION THIS SEGMENT RELAUNCHES ON, for as long as it runs: a
	// reader asking this node what a relaunch of the turn is launched on is
	// answered from it, not from the revision an apply since put live
	// ([Engine.relaunchCompany]).
	e.resumedSegments.Store(in.Run.TurnID, company)
	defer e.resumedSegments.CompareAndDelete(in.Run.TurnID, company)
	// BEFORE ANYTHING THIS SEGMENT LAUNCHES: a run that ended on its
	// vendor's spent plan sits its entry out here, on the node whose launch
	// the turn's next run is — see [Engine.noteRunEnded].
	e.noteRunEnded(ctx, company, in.Run, in.Ended)
	in.Rotated = e.signInReplaced(ctx, company, in)
	in.Refused = requestRefused(ctx, in)
	// A COMPLETION'S MODELS ARE ASKED HERE, its job collected and charged and
	// its box asleep, and before anything of its turn runs: a limit its
	// review's chain already knows refuses it, which the coordinator holds on
	// this node, and parks its seat on that limit, a sit-out this very run's
	// ending may just have started. A RETRY, like every early return below.
	// An answer's were asked at its gate, before its claim, with nothing to
	// collect. See [Engine.collectedRefusal].
	if in.Asked == nil {
		if refusal, refused := e.collectedRefusal(ctx, company, in.Turn.Handle()); refused {
			return &sandbox.TurnRefused{Refusal: refusal}
		}
	}
	// THE TURN'S NOTES AS ITS RUN'S RECORD HOLDS THEM NOW, under the same
	// claim: the read that collected the job settled its hooks' serves after
	// the claim read the row ([runSteers.claimedNotes]). One place for every
	// road here: a completion, the cap's collection, a parked job's answer, a
	// lost carry. A RETRY, like every early return below.
	claimed, err := e.runSteers.claimedNotes(ctx, in.Run)
	if err != nil {
		return err
	}
	in.Run = claimed
	// THE TURN'S NOTES ARE ON ITS RUN'S RECORD, which this segment claimed:
	// taken there, pinned to the claim — the job it collects, the resumed
	// status and the lease the claim stamped — and carried from there, every
	// note the turn read before this segment included. A note sent while the
	// run worked or waited on its question was held there for exactly this.
	// Closed on every way out: each return below is a retry, and the view
	// it follows the record by must not outlive the attempt. Before the
	// runner's input, whose launcher seals a relaunch's brief from it. See
	// runsteer.go.
	source := e.runSteers.source(ctx, in.Run,
		sandbox.Fence{Owner: in.Run.Owner, Epoch: in.Run.OwnerEpoch})
	defer source.Close(ctx)
	input, tel, err := e.parkedTurnRunner(ctx, company, in, source)
	if err != nil {
		// A RETRY, like every early return here: nothing ran, and the
		// coordinator hands its claim back so the next attempt judges the
		// rebase again against its own clock.
		return err
	}
	resumedReply := input.Reply
	if source != nil {
		input.Steer = source
	}
	// The working indicator's phase updates, on the same terms as the
	// dispatch path: the resumed segment opens its phases like any other, and
	// a reader watching the thread should see them move when the box's answer
	// lands.
	input.OnPhase = func(ph phase.Phase) { status.Phase(ph.String()) }
	// NO onboarding on a resume. The pass is a seat's FIRST turn, and this
	// turn already ran its first phase — running it here would spend the
	// resumed turn's opening on orientation for a seat that is mid-task.
	input.Resume = runnerResume(in, e.runSteers.carried(in.Run), sandbox.NewCallSeal(e.cipher), e.leads)
	// AND EVERY WAKE A CONTINUATION OF THE RUN'S SESSION MAY HOLD, off the same
	// record: each one read, and each its job was served and never read.
	input.Resume.Wakes = e.runSteers.carriedWakes(in.Run)
	r, err := company.RunnerFor(ctx, in.Turn.Handle(), e.seatRegistry(company, in.Turn.Handle()), input)
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
		// AND WHAT THE TURN DOES WITH THE ROUND OF THE JOB IT RESUMES FROM, as
		// its runner decides it ([runner.Resume.CarriesOn]): for every reader
		// of whether a relaunch would continue the job's session while the
		// turn runs ([sandbox.LaunchRecord.ResumeCarry]).
		carry := sandbox.ResumeCollects
		if input.Resume.CarriesOn() {
			carry = sandbox.ResumeCarriesOn
		}
		if beginErr := in.Begin(ctx, carry); beginErr != nil {
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
	// AND WHAT IT HOLDS, answered for by this node while the segment runs,
	// as on the dispatch path — the row carries them too. See codeholds.go.
	defer e.turnHolds.running(tel.holds)()
	// THE TURN'S OWN DEADLINE, off the park: the cap bounds the turn a fire
	// started, so the round this segment collects always runs and no round
	// after it starts once that instant has passed ([turn.Settings.Deadline]).
	res, err := turn.Run(ctx, r, company.TurnSettings(tel.deadline),
		resumeInputFor(in, resumedReply))
	// A SEGMENT THAT ENDED THE TURN — finished, or stopped by a person — is
	// remembered here as ended, so a note that reaches this node after the
	// run's record is gone is told the turn is over rather than nothing. One
	// that suspended again leaves the record as the turn's notes' home, and
	// one that broke is a retry of this same segment.
	if !res.Suspended && (err == nil || turn.Stopped(err)) {
		e.steers.remember(in.Run.TurnID, steerSeat{agent: in.Run.AgentID, handle: in.Run.AgentHandle})
	}
	// THE TURN RAN, so from here the indicator follows what it concluded
	// rather than the retry rule above: a resumed turn that suspended AGAIN
	// keeps it, because the same box is still working.
	working = res.Suspended
	// A SEGMENT THAT WORKED ends the seat's run of failures that will
	// repeat, as a dispatched turn that worked does ([Engine.failureCleared]):
	// its models answered, so a refused credential before it and one after it
	// are not the same failure twice in a row.
	if err == nil {
		e.failureCleared(in.Turn.Handle())
	}
	// HOW A SEGMENT THAT BROKE GOES ON, decided once and BEFORE its completion
	// is published, because the completion says whether the turn ended: see
	// [resumeFailure].
	var fate resumeFailure
	if err != nil {
		fate = e.judgeResumeFailure(ctx, company, in, res, err)
	}
	// THE SEGMENT'S CHARGE, decided once for the suspension below and the
	// charge after the completion — see turnspend.go.
	spend := r.Spend()
	charge := tel.chargeFor(spend, res, err, time.Now().UTC())
	// A SEGMENT THAT BROKE WITHOUT ENDING ITS TURN is published as one that
	// did not end it ([types.AgentTurnCompleted.Suspended]): its run is
	// handed back, held or refused to wait, and the same turn id completes
	// again when its completion comes round. Published as an end, every
	// retry of one resume was a turn that ended and failed: a seat whose
	// review could not reach its model read "Not on a turn. Its last turn
	// failed" while its run still waited, and the Turns axis counted
	// hundreds of ended turns over a list of two.
	published := res
	if err != nil && !fate.endsTurn() {
		published.Suspended = true
	}
	e.endSegment(ctx, tel, spend, published, err, charge)
	// THE COLLEAGUE WHOSE ASK THE TURN TOOK UP IS ANSWERED BY THE SEGMENT
	// THAT ENDS IT — this one, wherever it ends the turn below — off the ask
	// its launch wrote onto the run's record: the dispatch frame that saw
	// the ask answers nothing for a turn that parked (a2a.go). After the
	// completion above, as on that frame, and only on a way out that ends
	// the turn: a retry, a hold and a park again leave it to a later segment.
	answer := func(res turn.Result, err error) colleagueAnswer {
		return e.answerColleague(ctx, company, answeringRun(in.Run), in.Run.ColleagueAsk, res, err)
	}
	// stopped settles the resumed turn as a person's stop ended it: said
	// (agent_turn_stopped, under the pause that stopped it), its colleague
	// answered, its end owed on the run's record for the run's ending to
	// record on the executor log, and the run settled rather than reverted
	// (below).
	//
	// UNLESS THE STOP WAS DECIDED ON THE RUN this segment's relaunch was
	// launching ([stoppedByRun]): that stop's own ending answers the
	// colleague and owes the turn's end, from the run's record, once this
	// frame has returned and the stop's read has kept the relaunched job's
	// session ([sandbox.CoordinatorOptions.CloseStopped]); answered here as
	// well, the colleague was answered twice.
	stopped := func(stop error, why string) error {
		pause, _ := stopOf(stop)
		role, agentID := seatIdentity(company, in.Turn.Handle())
		e.observe(ctx, turnStoppedEvent(in.Turn.Handle(), role, agentID,
			in.Run.TurnID, in.Run.WorkKey, pause, tracing.TraceOf(ctx)))
		if !stoppedByRun(stop) {
			answered := answer(res, stop)
			e.oweExecutorEnd(ctx, in.Run, stoppedEnding(res, resumedReply, stop, answered))
		}
		return fmt.Errorf("%w (%s): %w", sandbox.ErrResumeAbandoned, why, stop)
	}
	// A LAUNCH THIS SEGMENT'S FENCE REFUSED once its record was written is
	// this segment's to end, which the ways out below do: no other frame
	// takes it off ([Engine.abandonedLaunches]).
	defer e.abandonedLaunches.Delete(in.Run.TurnID)
	if err != nil {
		// A STOP DECIDES IT FIRST: a person stopped the resumed turn, or the
		// seat while the segment met a wait ([Engine.judgeResumeFailure]). It
		// is settled as stopped rather than reverted.
		if fate.stop != nil {
			return stopped(fate.stop, fate.stopWhy)
		}
		// A BUDGET THAT REFUSES THE SEGMENT HOLDS IT, before anything decides
		// it is lost ([Engine.judgeResumeFailure] says why it comes ahead of
		// the abandon).
		if fate.held != nil {
			working = false
			return fate.held
		}
		if fate.abandon != "" {
			// The same decision the dispatcher makes on the other path
			// (see (*Dispatcher).abandon), from the same rule, taken here
			// in this subsystem's own vocabulary: the coordinator reads
			// the sentinel and leaves the claim taken, so the completion
			// is not redelivered into a conversation a retry must not
			// re-enter.
			//
			// The indicator comes down with it: the run is settled,
			// the box reclaimed and the record deleted, so nothing is
			// coming back for this turn — and the colleague is told it
			// could not be answered, and the executor log how it ended.
			e.oweExecutorEnd(ctx, in.Run, abandonedEnding(res, resumedReply, answer(res, err), fate.abandon, err))
			return fmt.Errorf("%w (%s): %w", sandbox.ErrResumeAbandoned, fate.abandon, err)
		}
		// A RUN WHOSE ENDING IS DECIDED, or whose record is gone, ends the
		// turn here, as one abandoned, and settles rather than reverts
		// ([Engine.judgeResumeFailure]).
		if fate.runEnded {
			e.oweExecutorEnd(ctx, in.Run, abandonedEnding(res, resumedReply, answer(res, err),
				"its relaunch failed, and its run was ended with it", err))
			return fmt.Errorf("%w (the resumed turn's relaunch failed and its run was ended with it): %w",
				sandbox.ErrResumeAbandoned, err)
		}
		// Reverted, so the turn is not over, and BOTH KINDS OF RESUME COME BACK
		// rather than wait for a signal. A BOX'S COMPLETION is NAK'd by the
		// control-topic handler and redelivered on the broker's backoff until its
		// delivery budget is spent. A PERSON'S ANSWER is recorded on the run, so
		// the revert leaves it owed its resume and the coordinator retries it,
		// bounded by [sandbox.MaxAnswerAttempts] and pause_ttl_seconds, then lets
		// it go (a chat reply to the ordinary route, an answer by turn spent as
		// declined). The delivery that carried it is never handed back; it was
		// spent when the answer was recorded. Same retry rule as the seed above.
		working = rejoined
		// UNLESS WHAT BROKE IT IS A WAIT: every model at a usage limit
		// before this attempt wrote anything — the reviewer's, or every
		// executor member the relaunch could pick sitting out. The turn
		// gate's own condition ([Engine.seatCannotRun]), found a round
		// later, and handed back as its refusal is: a completion is
		// acknowledged and kept until the condition clears, and an answer
		// waits for it uncharged. As a failure, a completion was NAKed
		// into the same limit until its delivery budget ran out, and an
		// answer spent its attempts on a wait. (A spent window was held
		// above, before the abandon.)
		if refusal, refused := resumeRefusal(err); refused {
			// AND THE SEAT IS PARKED ON IT, as a dispatch that met it
			// parks it: its one steady condition and its chain's probe,
			// whose release re-fires this resume (providerpark.go).
			e.parkResumeOnLimit(ctx, in.Turn.Handle(), err)
			return &sandbox.TurnRefused{Cause: err, Refusal: refusal}
		}
		// AND A FAILURE A RETRY CANNOT CURE PAUSES THE SEAT on its second time in
		// a row, as on the dispatch path (ADR-0117), which shares the seat's run
		// of failures: every model of the review's chain refused its credential,
		// say. A resume is where this meets a seat most often (the run's review
		// is the turn's next model call); unpaused, the completion was
		// redelivered about a second apart for the half hour of the give-up
		// while every screen drew the seat as working. Paused, the seat says
		// why, and the completion is kept for the lift and held at the turn gate
		// meanwhile ([Engine.failurePauseRefusal]).
		reason, paused, holdErr := e.failureHold(ctx, in.Turn.Handle(), err)
		if holdErr != nil {
			log.WarnContext(ctx, "seat_failure_pause_not_taken", "seat", in.Turn.Handle(),
				"error", holdErr.Error(), "detail", "the resumed turn met a failure that will "+
					"repeat and the seat could not be paused on it; the resume is retried")
		}
		if paused {
			return &sandbox.TurnRefused{Cause: err, Refusal: sandbox.Refusal{Condition: waitPause, Reason: reason}}
		}
		return err
	}
	// A resumed turn that suspended AGAIN persists its new conversation the
	// same way the first one did — the coordinator sees the row back in
	// running and leaves the box for the next completion — and keeps its
	// indicator on the same terms, off the ROW rather than off the intent.
	if res.Suspended {
		parked, parkErr := e.persistSuspension(ctx, r, in.Run.TurnID, tel, charge)
		if turn.Stopped(parkErr) {
			// A PERSON STOPPED THE RUN IT RELAUNCHED, between the launch and
			// the park: closed by the park it refused, as on the dispatch
			// path, and settled as a stopped resumed turn is (above).
			working = false
			pause, _ := stopOf(parkErr)
			role, agentID := seatIdentity(company, in.Turn.Handle())
			e.observe(ctx, turnStoppedEvent(in.Turn.Handle(), role, agentID,
				in.Run.TurnID, in.Run.WorkKey, pause, tracing.TraceOf(ctx)))
			// Not parked after all: the run it relaunched is ended, and so
			// is the turn. Its colleague's answer and its end are that
			// stop's own ending's, which records them from the run's record
			// once its read has kept the relaunched job's session
			// ([sandbox.CoordinatorOptions.CloseStopped]); answered here
			// as well, the colleague would find the channel the first answer
			// closed.
			res.Suspended = false
			return fmt.Errorf("%w (a person stopped the run the resumed turn relaunched): %w",
				sandbox.ErrResumeAbandoned, parkErr)
		}
		working = stillWorking(parked, parkErr)
	}
	// A SEGMENT THAT PARKED AGAIN answers nothing here: answerColleague
	// leaves a suspended result to the segment that ends the turn.
	e.recordResume(ctx, in, res, resumedReply, answer(res, nil))
	return nil
}

// parkedTurnRunner is the runner a parked run's turn is rebuilt with on this
// node, for the resume ([Engine.resumeTurn]) and a run taken over while it works
// ([Engine.reopenAgentRun]).
//
// ONE CONSTRUCTION for both: a box's call after a recovery and the resumed
// turn's calls are the same turn's and must agree on everything a call reads;
// agreeing by copy drifted once. The results are named so a failure answers
// their zero values; every RunnerInput literal carries the seat's fence
// (internal/seat's TestEveryConfigThatCanCarryTheSeatFenceDoes). Every error is
// a RETRY: nothing has run. notes is the segment's notes on the run's record,
// which later iterations' launcher seals briefs from ([agentLauncher.notes]).
func (e *Engine) parkedTurnRunner(ctx context.Context, company *Company, in resumeInput, notes *rowSource,
) (input RunnerInput, tel turnTelemetry, err error) {
	// AND EVERY WAKE ITS RUN READ: the turn answers those too (wakeitem.go).
	resumedReply, err := e.runSteers.turnReply(in.Run)
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
	refusedIn := in.Run.Launch.RelaunchRefusedIn
	return RunnerInput{
		Task:    resumeTask(in),
		Compact: e.compactorFor(company),
		// THE RUNNER NEEDS IT TOO, not just the loop. This field reaches
		// runner.Config.Reply, which is what submit_work's own citation
		// check reads — so a resumed turn with this unset accepted
		// `no_action` on a turn somebody was waiting on, and accepted a
		// citation naming any surface at all, before the loop's later
		// checks ever ran.
		Reply: resumedReply,
		// AND THE READ OF A PICKUP'S ANSWER, from the turn's first launch,
		// which no write of the turn precedes: its row keeps that instant
		// across every relaunch (ADR-0122).
		PickupAnswer: e.pickupAnswerFor(in.Run.AgentHandle, in.Run.CreatedAt, resumedReply),
		Publisher:    e.backends.Queue,
		Turn:         turnIdentity,
		Sessions:     e.sessions,
		Metrics:      e.metrics,
		// THE LAUNCHER, for the turn's LATER iterations rather than this
		// one: this segment collects the run the turn parked on, and a turn
		// its reviewer sends back launches its next run from here — on
		// whichever member of the pinned chain can take it then. With the
		// window the run's last relaunch was refused below its floor in, off
		// its job's record, so this segment's relaunch waits for its brief's
		// room no longer than that window ([agentLauncher.continuedFloor]).
		Launcher: e.agentRunFor(company, in.Turn.Handle(), turnIdentity.Context, in.Run.ColleagueAsk, nil, nil,
			nil, notes, &refusedIn),
		Budget: tel.budget.budget(),
		// NO ROUND-CAP JUDGE: the only engine loop a resumed segment runs is
		// the reviewer's, whose budget is one submission and its correctives
		// rather than a cap to extend, and the onboarding pass — the one
		// phase with a cap — never runs on a resume.
		Remaining: e.remainingFor(company, in.Turn.Handle()),
		// THE SAME FENCE THE DISPATCH PATH GETS, and a parked run needs it
		// more than a fresh turn does: its turn was parked across a launch
		// that may have taken an hour, and the node that picks it up is
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

// heldByBudget is a resumed segment's error as the hold it is, where the seat's
// budget refused it ([toolloop.ErrBudgetExhausted]) and one of its windows is
// still refusing or starved ([Engine.budgetHold]); nil for every other error
// and for a window that has since turned over.
//
// HELD AGAINST THE FLOOR THE LAUNCH WAS REFUSED BELOW ([floorOf],
// [launchFloor.tokens]), not the static floor alone: a window with room for the
// static floor would release a resume into the same refusal below a larger
// one. The hold names the window it was refused in ([sandbox.Refusal.Window]),
// so the next relaunch waits for its floor no longer than that window.
func (e *Engine) heldByBudget(ctx context.Context, company *Company, handle string, err error) *sandbox.HeldError {
	if !errors.Is(err, toolloop.ErrBudgetExhausted) {
		return nil
	}
	refusal, held := e.budgetHold(ctx, company, handle, err)
	if !held {
		return nil
	}
	return &sandbox.HeldError{Refusal: refusal, Err: err}
}

// roundTellings is the tellings of bridged's round among tellings
// ([sandbox.BridgeLog.Told]), as the runner credits them.
func roundTellings(bridged sandbox.BridgeLog, tellings []sandbox.CallTelling) []runner.Telling {
	var out []runner.Telling
	for _, t := range bridged.Told(tellings) {
		out = append(out, runner.Telling{Appended: t.Appended, Place: t.Place, Hook: t.Hook})
	}
	return out
}

// runnerResume is the collection a resumed segment's runner takes, read off the
// run's row and the resume's own input: what the run did, where, under which
// entry, and what carries it on. steered is every note the turn has read
// ([runSteers.carried]).
//
// seal opens a held call's arguments and seals its result (ADR-0077): nil on a
// node without the fleet keyring's cipher, where a held call is not made. leads
// opens the run's leads the coordinator read beside its row ([resumeLeads]):
// nil reads each as a lead that cannot be read here.
func runnerResume(in resumeInput, steered []steer.Read, seal *sandbox.CallSeal,
	leads *sandbox.LeadSeal) *runner.Resume {

	return &runner.Resume{
		State: in.State, Answer: in.Answer,
		// WHICH SESSION THE JOB'S BOX SAID IT STARTED, off the job's own
		// record: every guard the resume rebuilds from State — the
		// collection's, a relaunch carrying the round on, a send-back that
		// continues the job's session — holds the skills its launch armed only
		// where the box took that session up ([runner.Resume.Continued]).
		Continued: in.Run.Launch.SessionStart == sandbox.SessionStartContinued,
		// HOW THE JOB'S HARNESS ENDED, off the job's own record, as it
		// says it: a relaunch carrying its round on in its box tells the
		// session there that it was cut off — or may have been, where the
		// record says nothing — every call of the round and every note the
		// turn has read, and arms its guard without the job's own loads,
		// unless its harness ended its own run ([runner.Resume.HarnessEnd]).
		HarnessEnd: in.Run.Launch.HarnessEnd,
		// AND THE READS THE BRIEF ITS RUN READ COUNTED WITHOUT LISTING THEM,
		// off the job's own record by that brief: the copy of its session in
		// its box holds them only as that count, so a relaunch carrying the
		// round on in that box tells them ([runner.Resume.Owed]).
		Owed: in.Run.Launch.OmittedCalls(),
		// AND THE LATE CALLS IT COUNTED SO, which that copy is credited no
		// telling of ([runner.Resume.LateOwed]).
		LateOwed: in.Run.Launch.LateOmittedCalls(),
		// AND THE LATE CALLS OF THE LOG'S ROUND A JOB'S SESSION TOOK, off the
		// row: a continuation of a session holding one is not told it again
		// ([runner.Resume.Tellings]).
		Tellings: roundTellings(in.Bridged, in.Run.Steer.Tellings),
		// AND WHAT THE COPY IN ITS BOX CARRIED, off the job's own record as the
		// read that collected it measured its newest request, or as the record
		// carried it along the copy's lineage: a relaunch carrying the round on
		// in that box is held to it with its brief, beside the job's newest
		// capture's measure ([runner.Resume.ContextTokens]).
		ContextTokens: in.Run.Launch.Context.ContextTokens(),
		// AND WHAT ITS HARNESS ESTIMATES THAT COPY'S NEXT REQUEST AT, off the
		// same record, never carried along the copy's lineage: what that
		// relaunch is held to in place of the measure where it holds a figure
		// ([runner.Resume.NextRequest]).
		NextRequest: in.Run.Launch.NextRequest(),
		// AND WHAT ITS HARNESS COUNTS THAT COPY AT, as the read that
		// collected the job found it, with the provider an OpenCode count's
		// reply was answered by, and the restart the job's launch handed it,
		// which the copy's own and a capture of the job are compared with: a
		// relaunch judges whether the session reaches its first request whole
		// by them ([runner.Resume.Count], [runner.Resume.BasisRestart]).
		Count: in.Run.Launch.ResumedCount, CountProvider: in.Run.Launch.CountProvider,
		BasisRestart: in.Run.Launch.RestartBasis(),
		// AND THE TURN'S LEADS AS THE RESUME READ THEM BESIDE ITS ROW, opened,
		// those its job's brief told marked: a relaunch continuing that job's
		// session where it may lack the brief tells them again
		// ([runner.Resume.Leads]), with how many earlier ones are not kept,
		// past the bound or because the store could not give them back; and
		// where it could not, the ones the job's brief told by their numbers,
		// for that relaunch to hand on unread.
		Leads: resumeLeads(in.Run, in.Leads, seal, leads), LeadsCut: in.Leads.Cut,
		LeadsUnread: unreadLeads(in.Run, in.Leads),
		// AND WHERE THE TURN BEGAN, the session its conversation's last turn
		// left as the turn's row froze it, which a task lead told again says.
		Base: frozenBase(in.Run),
		// WHICH BOX DID THE WORK, and what it delivered — see
		// [runRecordOf].
		Run: runRecordOf(in.Run, in.Account),
		// THE RUN'S OWN TOOL CALLS, off its durable log. The executor called
		// them over the bridge, possibly in another process, so this list is
		// the only record of what the phase did, its submission included.
		Bridged: bridgedCalls(in.Bridged.Calls),
		// AND THE CALLS IT HELD IN FLIGHT AS IT WAS READ, each its identity
		// and an outcome not known: counted by the turn's call log, listed in
		// the round's record and told to a run that carries the round on
		// ([runner.Resume.Unfinished]).
		Unfinished: unfinishedCalls(in.Bridged.InFlight),
		// AND WHAT THE LOG COUNTS WITHOUT LISTING, by identity, which the
		// turn's call log is seeded with beside them: the round's calls it
		// dropped, and the calls the log absorbed from their own work.
		Elided:   identitiesOf(in.Bridged.Unlisted()),
		Absorbed: identitiesOf(in.Bridged.Absorbed()),
		// AND HOW MANY OF THE DROPPED WERE LATE, which a continuation is
		// told past whatever its save point: no save point holds a late
		// call, and a dropped one keeps no mark.
		LateElided: in.Bridged.LateElided,
		// AND WHERE A PERSON'S ANSWER CUT THE ROUND, as the log recorded it
		// when the job carried on by that answer opened: no submission
		// made before it is the round's.
		Answered: in.Bridged.AnsweredThrough,
		// AND WHAT THE READ SAW, which every relaunch hands its launch: the
		// opening marks late every call the log took after it, or carries it
		// into a new round ([sandbox.LaunchRequest.BriefRead]).
		BridgedRead: in.BridgedRead,
		// AND A PERSON'S ANSWER, which carries the run on rather than
		// reporting it (see [runner.Asked]), or the loss of its job with its
		// host, which carries its round on ([runner.Lost]).
		Asked: runnerAsked(in.Asked, seal),
		Lost:  runnerLost(in.Lost),
		// A REFUSAL ON A REPLACED SIGN-IN CARRIES THE ROUND ON, once. See
		// [Engine.signInReplaced].
		Rotated: in.Rotated,
		// A CONTINUED SESSION ITS PROVIDER REFUSED CARRIES THE ROUND ON, once,
		// in a fresh one; a refusal past that is the round's ending. See
		// [requestRefused].
		Refused:        in.Refused,
		RequestRefused: refusedSaid(in),
		// AND EVERY NOTE THE TURN HAS READ, off the record every read is
		// written on: the briefs', the run's own and earlier reviews'.
		Steered: steered,
		// AND EVERY NOTE ITS BRIDGE SERVED A HARNESS, read since or waiting
		// still: a continued session may show one the record says it had not
		// read ([servedNotes]). Final for the segment: no poll serves a run
		// once its claim has moved it on.
		Served: servedNotes(in.Run),
	}
}

// resumeLeads is the turn's leads read beside run's row ([sandbox.TurnLeads]),
// opened in order ([runner.ResumeLead]): those the job's brief told marked, as
// the job's record says by their numbers ([sandbox.LaunchRecord.ToldLeads]),
// and a held lead's result opened from the seal its row put on it
// ([sandbox.CallSeal.OpenKept]). One that does not open
// here is kept as a lead that cannot be read, by its kind, job and round, and
// why: NEVER DROPPED, so a brief that tells it again says a message is missing
// rather than leave the run to think none was. One under a key this node lacks,
// or of a kind this build does not know, is this node's fact and logged at
// warning; one that does not open under a key it holds is the record's, and
// logged at error. A held lead whose result does not open is told as a call made
// whose result cannot be read here.
func resumeLeads(run sandbox.PendingRun, read sandbox.TurnLeads, seal *sandbox.CallSeal,
	leads *sandbox.LeadSeal) []runner.ResumeLead {

	if len(read.Kept) == 0 {
		return nil
	}
	told := map[int]bool{}
	for _, mark := range run.Launch.ToldLeads() {
		told[mark] = true
	}
	out := make([]runner.ResumeLead, 0, len(read.Kept))
	for _, kept := range read.Kept {
		lead, err := openLead(run.TurnID, kept, leads)
		if err != nil {
			lead = sandbox.Lead{Kind: kept.Kind, Job: kept.Job, Seq: kept.Seq, Round: kept.Round, Unread: err.Error()}
			if errors.Is(err, sandbox.ErrLeadUnopened) {
				log.Error("sandbox_lead_unopened", "turn_id", run.TurnID, "launch_id", kept.Job,
					"kind", string(kept.Kind), "error", err.Error())
			} else {
				log.Warn("sandbox_lead_unread", "turn_id", run.TurnID, "launch_id", kept.Job,
					"kind", string(kept.Kind), "error", err.Error())
			}
		}
		resumed := runner.ResumeLead{Lead: lead, Told: told[kept.Appended]}
		if answer := lead.Answer; answer != nil && answer.Held != nil && answer.Held.Result != nil {
			result, err := seal.OpenKept(run.TurnID, *answer.Held.Result)
			if err != nil {
				log.Error("held_call_result_unopened", "turn_id", run.TurnID, "held_call", answer.Held.HeldID,
					"tool", answer.Held.Tool, "error", err.Error())
			} else {
				resumed.Result = &result
			}
		}
		out = append(out, resumed)
	}
	return out
}

// unreadLeads is the leads the brief of run's job told, by their numbers
// ([sandbox.LaunchRecord.ToldLeads]), where the resume could not read the
// run's leads at all ([sandbox.TurnLeads.Unread]): none where it read them,
// whatever they held, since those it told are among them, marked.
//
// THE CHAIN OUTLIVES A READ THAT FAILED. A continuation of a session that may
// lack its brief tells those leads again and hands them on for the next job to
// mark, so a continuation of THAT session tells them in turn. Read, they ride
// the leads ([runner.ResumeLead.Told]); unread, the relaunch says them as not
// kept and hands their numbers on ([runner.Resume.LeadsUnread]), so the next
// read that answers tells them rather than leave a session told nothing.
func unreadLeads(run sandbox.PendingRun, read sandbox.TurnLeads) []int {
	if !read.Unread {
		return nil
	}
	return run.Launch.ToldLeads()
}

// openLead is kept opened under leads ([sandbox.LeadSeal.Open]), refused where
// this node holds no seal to open one with.
func openLead(turnID string, kept sandbox.KeptLead, leads *sandbox.LeadSeal) (sandbox.Lead, error) {
	if leads == nil {
		return sandbox.Lead{}, fmt.Errorf("%w: this node holds no seal to open a lead with",
			sandbox.ErrLeadKeyUnknown)
	}
	return leads.Open(turnID, kept)
}

// frozenBase is the session run's conversation's last turn left, as the turn's
// row froze it ([sandbox.PendingRun.Base]), in the runner's terms: nil where the
// turn continues none.
func frozenBase(run sandbox.PendingRun) *runner.BaseBasis {
	if run.Base == nil || run.Base.Session == nil {
		return nil
	}
	return baseBasis(*run.Base.Session, 0).Base
}

// runnerAsked is the coordinator's account of an answered question in the
// runner's terms, nil for a resume no answer drove: a held call's arguments
// opened from their seal where no attempt has marked it yet, and its mark and
// result read and written through the claim ([heldCommit]).
func runnerAsked(asked *sandbox.AskedRun, seal *sandbox.CallSeal) *runner.Asked {
	if asked == nil {
		return nil
	}
	out := &runner.Asked{
		Question: asked.Question, Answer: asked.Answer, By: asked.By,
		Branch: asked.Branch, SessionID: asked.SessionID, BoxKept: asked.BoxKept,
		Label: asked.Label, Withdrawn: asked.Withdrawn, Waited: asked.Waited, Called: asked.Called,
		CallsReturned: asked.CallsReturned,
	}
	if refused := asked.Refused; refused != nil {
		// A REFUSAL, NEVER AN EMPTY ANSWER: the run is told the reply did
		// not reach it, with its figures, in the engine's words (ADR-0114).
		out.Refused = &runner.RefusedReply{Messages: refused.Messages(), Bytes: refused.Bytes,
			MaxBytes: refused.MaxBytes}
	}
	if held := asked.Held; held != nil {
		call := held.Call
		out.Held = &runner.HeldCall{Tool: call.Tool, Server: call.Server, Digest: call.Digest,
			Choice: held.Choice, Approver: held.By, HeldID: call.ID, Fixed: call.Fixed, Binding: call.Binding,
			Commit: heldCommit{held: held, seal: seal}}
		// A CALL AN EARLIER ATTEMPT MARKED KEEPS ITS ARGUMENTS SEALED: it is
		// answered from its mark and never made again
		// ([runner.HeldCommit.Marked]), so nothing would run them, and a seal
		// this node cannot open is no reason to say anything about a call it
		// will not make.
		// A FIXED CALL'S DENIAL IS RECORDED by its tool too, from the same
		// arguments, so they are opened for a deny as well.
		if (sandbox.HeldMakes(held.Choice) || call.Fixed) && !call.Calling() {
			args, err := seal.OpenRawArguments(held.TurnID, call)
			if err != nil {
				// NEVER MADE WITH ARGUMENTS NOBODY APPROVED: a seal this
				// node cannot open leaves the call unmade, and the run is
				// told so.
				log.Error("held_call_arguments_unopened", "turn_id", held.TurnID,
					"held_call", call.ID, "tool", call.Tool, "error", err.Error())
				out.Held.Unopened = "its arguments could not be opened on the node carrying the " +
					"run on, so the engine could not make the call as it was approved; call it " +
					"again to ask afresh"
			}
			out.Held.Args = args
		}
	}
	return out
}

// heldCommit is [runner.HeldCommit] over the claim's read of the held call and
// its writes ([sandbox.HeldResume]), and the fleet keyring's seal, which the
// result is sealed under on the row and opened from for a later attempt.
type heldCommit struct {
	held *sandbox.HeldResume
	seal *sandbox.CallSeal
}

// Marked answers a held call an earlier attempt at the same answer marked, off
// the row as the claim read it ([sandbox.HeldResume.Call]); see
// [runner.HeldCommit].
func (h heldCommit) Marked(ctx context.Context) (runner.HeldPrior, bool) {
	if !h.held.Call.Calling() {
		return runner.HeldPrior{}, false
	}
	return h.prior(ctx, h.held.Call), true
}

// Begin marks the held call; see [runner.HeldCommit].
func (h heldCommit) Begin(ctx context.Context) (runner.HeldPrior, error) {
	call, first, err := h.held.Mark(ctx)
	switch {
	case err != nil:
		return runner.HeldPrior{}, err
	case first:
		return runner.HeldPrior{First: true}, nil
	}
	return h.prior(ctx, call), nil
}

// prior is how call stood, some earlier attempt having marked it: what it
// returned, opened from its seal, where that attempt recorded it, and that
// result as the row sealed it, which the answer's lead keeps whether or not it
// opens here ([runner.HeldPrior.Kept]).
func (h heldCommit) prior(ctx context.Context, call sandbox.HeldCall) runner.HeldPrior {
	if !call.Made() {
		return runner.HeldPrior{}
	}
	kept := call.Kept()
	result, err := h.seal.OpenResult(h.held.TurnID, call)
	if err != nil {
		// RECORDED, AND UNREADABLE HERE: the call was made, and what it
		// returned cannot be handed over. The run is told it was made, and
		// whether the tool reported a failure — off the call's entry the
		// result was recorded with, which is what the round's log lists of it
		// — never as a call whose result is unknown, which it is not: a later
		// attempt at the answer tells the call by this lead alone, the list of
		// the round's calls leaving it out (runner.HeldPrior.Unread). Never
		// made again.
		log.ErrorContext(ctx, "held_call_result_unopened", "turn_id", h.held.TurnID,
			"held_call", call.ID, "tool", call.Tool, "error", err.Error())
		return runner.HeldPrior{Made: true, Unread: true,
			Result: sandbox.HeldResult{Failed: call.Call != nil && call.Call.Failed}, Kept: kept}
	}
	return runner.HeldPrior{Made: true, Result: result, Kept: kept}
}

// Record seals and records what the held call returned, and the call itself as
// a call of its run's round, as the surface that made it answered it (its
// arguments and their digest withheld, as the held call's were), answering the
// result as it sealed it, which the answer's lead keeps whether or not the
// row's write lands; see [runner.HeldCommit].
func (h heldCommit) Record(ctx context.Context, result sandbox.HeldResult,
	answered toolloop.ToolResult) (sandbox.HeldKept, error) {

	sealed, err := h.seal.SealResult(h.held.TurnID, h.held.Call, result)
	if err != nil {
		return sandbox.HeldKept{}, err
	}
	return sandbox.KeptAs(h.held.Call, sealed), h.held.Record(ctx, sandbox.HeldMade{Result: sealed,
		Call: sandbox.BridgeCall{
			Name: h.held.Call.Tool, Failed: answered.Failed,
			Origin: answered.Origin, Server: answered.Server, Source: answered.Source,
			Refusal: string(answered.Refusal()),
		}})
}

// runnerLost is the coordinator's account of a lost job in the runner's terms,
// nil for a resume no loss drove.
func runnerLost(lost *sandbox.LostRun) *runner.Lost {
	if lost == nil {
		return nil
	}
	return &runner.Lost{Why: lost.Why, BoxKept: lost.BoxKept, Losses: lost.Losses, Limit: lost.Limit}
}

// noteRunEnded sits out the entry a collected run launched on, where the run
// ended on its vendor's report of a spent plan or a rate limit
// ([harness.Provider.RunEnded]), on this node in the epoch the resumed turn's
// launches pick their member from. HERE, BECAUSE NOTHING ELSE SEES IT: a box's
// run never passes the entry's runs on this host, the one path that benched an
// entry, so a send-back would relaunch into the plan just spent.
//
// THE ENDING IS THE SIGN-IN'S THE RUN LAUNCHED WITH, never the entry's current
// one: recorded on the entry, a refusal on a revoked token became the NEW
// sign-in's failure (a false `llm_sign_in_refused`). Such an ending records
// nothing ([Engine.signInReplaced] carries the run on).
func (e *Engine) noteRunEnded(ctx context.Context, c *Company, run sandbox.PendingRun, ended sandbox.RunEnding) {
	if c == nil || c.Models == nil || run.Launch.ProviderKey == "" ||
		(ended.Text == "" && ended.Error == "" && ended.Event == "" && ended.Limits == "") {
		return
	}
	provider, ok := c.Models.Provider(run.Launch.ProviderKey)
	if !ok {
		return
	}
	agent, isHarness := provider.(*harness.Provider)
	if !isHarness {
		return
	}
	if launched := run.Launch.SignInID; launched != "" && launched != string(agent.SignInID()) {
		log.InfoContext(ctx, "executor_run_ended_on_a_replaced_sign_in",
			"turn_id", run.TurnID, "seat", run.AgentHandle,
			"provider_key", run.Launch.ProviderKey,
			"detail", "the run ran on a sign-in its entry no longer uses; how it ended is "+
				"recorded on none the entry signs in with now")
		return
	}
	if agent.RunEnded(ctx, harness.RunEnd{Answer: ended.Text, Error: ended.Error,
		Failed: !ended.Success, Event: ended.Event, Limits: ended.Limits, NoLimit: ended.NoLimitExit,
		At: ended.At}) {
		log.InfoContext(ctx, "executor_run_spent_its_entry",
			"turn_id", run.TurnID, "seat", run.AgentHandle,
			"provider_key", run.Launch.ProviderKey,
			"detail", "the run ended on its vendor's limit report; the entry sits out on "+
				"this node, so the turn's next launch takes the next member that can run")
	}
}

// recordResume owes a finished resumed turn's end on its run's record, for the
// executor log once the captures settle ([Engine.oweExecutorEnd]), under the
// conversation the answer was matched on, never the partition beside it. How it
// ended ([endingOf]; not the reviewer's last word, which reads `done` after the
// loop overrode it) is what the conversation's next turn is told. A turn that
// suspended again owes nothing.
//
// A REPLY THE RECORD DOES NOT SHOW IS NOT KNOWN TO BE MISSING where a call has
// an unknown outcome ([sandbox.BridgeLog.InFlight], [ledger.Call.Unfinished],
// [ledger.Elided.Unfinished]): it may be the reply, so the end says delivery is
// unknown, never that the turn did not reply.
func (e *Engine) recordResume(ctx context.Context, in resumeInput, res turn.Result, reply turn.Reply,
	answered colleagueAnswer) {
	if !res.Suspended {
		e.oweExecutorEnd(ctx, in.Run, resumedEnding(in, res, reply, answered))
	}
}

// resumedEnding is how a finished resumed turn ended ([endingOf]), its delivery
// read as unknown where the record shows no reply beside a call whose outcome
// nobody knows ([unfinishedIn]).
func resumedEnding(in resumeInput, res turn.Result, reply turn.Reply, answered colleagueAnswer,
) executorkeep.Ending {
	ending := endingOf(res, reply, answered)
	if !ending.Delivered && unfinishedIn(in) {
		ending.DeliveryUnknown = true
	}
	return ending
}

// unfinishedIn reports whether a call of in's turn has an outcome nobody knows:
// one the log it resumes from held in flight as it was read, or one a closed
// round's record lists so ([ledger.Call.Unfinished]) or counts so in the tally
// of the calls it does not list ([ledger.Elided.Unfinished]) — where a park
// past its share folded the round's calls (internal/agent/execstate).
func unfinishedIn(in resumeInput) bool {
	if len(in.Bridged.InFlight) > 0 {
		return true
	}
	return slices.ContainsFunc(in.State.Iterations, func(it ledger.Iteration) bool {
		return slices.ContainsFunc(it.Calls, func(c ledger.Call) bool { return c.Unfinished }) ||
			slices.ContainsFunc(it.Elided, func(e ledger.Elided) bool { return e.Unfinished > 0 })
	})
}

// resumeTask is the brief the resumed turn re-enters with.
//
// Taken from the STATE rather than rebuilt from the trigger: the trigger may
// no longer be readable days later, and the task the turn was working on is a
// fact the suspension already captured, as the model read it, with somebody
// else's words in it fenced. EVERY PARKED RUN'S STATE CARRIES IT
// (runner.Runner.recordAgentSuspension), so the row's description is never
// read in its place: that is the turn's ask in a person's words, with no
// fence ([sandbox.PendingRun.TaskDescription]), and a model handed it as its
// brief would read somebody else's words bare.
func resumeTask(in resumeInput) string {
	return in.State.Task
}

// resumeInputFor renders a parked run coming back as the loop's own
// [turn.Input].
//
// A NAMED MAPPING, as [turnInputFor] is on the dispatch path: every field is one
// the resumed loop cannot derive and each degrades SILENTLY without. No History
// re-fires the deliveries of rounds closed before the box; no Reply skips every
// delivery gate; no Round hands the turn a fresh `max_iterations` and
// re-numbers rounds already published. Three come off the parked row, so
// nothing else can notice one missing.
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
// A value this build does not recognise is refused rather than defaulted, as a
// ROUTING failure a peer can get past ([sandbox.ErrResumeElsewhere]); an absent
// one, which no build writes, as one no peer gets past
// ([sandbox.ErrResumeUnavailable]). The package doc's "A resume another node or
// build can make" says why.
func resumeReply(run sandbox.PendingRun) (turn.Reply, error) {
	reply := turn.ParseReply(run.Reply)
	if !reply.Valid() {
		routed := sandbox.ErrResumeElsewhere
		if run.Reply == "" {
			routed = sandbox.ErrResumeUnavailable
		}
		return turn.Reply{}, fmt.Errorf(
			"%w: run %s carries reply %q, which is not one of %q, %q or %q",
			routed, run.TurnID, run.Reply,
			turn.ReplyNone, turn.ReplyTool, turn.ReplyEngine)
	}
	return reply, nil
}

// persistSuspension writes a turn's parked state to its row, which OPENS the run
// to the completion poll ([sandbox.StatusLaunching] is polled by nobody), and
// reports whether the turn is coming back.
//
// EVERY FAILURE FAILS THE RUN rather than dropping the suspension: a stateless
// row cannot be resumed, and failing with the box in hand beats a launching row
// holding it. THREE-VALUED, since [turn.Result] stays Suspended on every path:
// (true, nil) the poll will resume it; (false, nil) the run is settled; an error
// where nothing is established (a failed write MAY have landed, and
// [sandbox.Coordinator.FailRun] settles only a launching run), for deciding.
// The turn's written, uncharged and measured state and its ask ride along
// (execstate.State.Written, Uncharged, SoFar, Ask, AskProvenance, Interactions).
func (e *Engine) persistSuspension(ctx context.Context, r *runner.Runner, turnID string,
	tel turnTelemetry, charge segmentCharge,
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
	suspension.State.Written, suspension.State.WrittenMany = tel.written.Items()
	for _, c := range tel.written.Changes() {
		suspension.State.Committed = append(suspension.State.Committed, string(c))
	}
	suspension.State.Uncharged = charge.carry
	suspension.State.SoFar = charge.soFar
	suspension.State.Ask = tel.ask
	suspension.State.AskProvenance = tel.askLabel
	suspension.State.Interactions = tel.interactions
	suspension.State.Deadline = tel.deadline
	// HELD TO ITS SHARE OF THE RUN'S ROW ([sandbox.ExecuteStateBytes]): the
	// closed rounds condensed where the state is past it — down to one count
	// of calls per tool for the lot — and a state still past it refused by
	// name rather than by the store, which would refuse this write and every
	// write of the row after it: only the turn's own context is ever past it.
	blob, condensed, err := execstate.EncodeWithin(suspension.State, sandbox.ExecuteStateBytes)
	switch {
	case errors.Is(err, execstate.ErrTooLarge):
		e.failSuspension(ctx, rt, turnID, "sandbox_suspension_too_large",
			"the parked turn state is past its share of the run's row: the turn's own context (its ask, its "+
				"trigger's words, its turn-start context and how its conversation's earlier turns ended) is "+
				"past it with every closed round folded into one count of calls per tool, which is the least "+
				"a park keeps of them", err)
		return false, nil
	case err != nil:
		e.failSuspension(ctx, rt, turnID, "sandbox_suspension_unserializable",
			"the parked turn state could not be serialized", err)
		return false, nil
	case condensed.Any():
		log.InfoContext(ctx, "sandbox_suspension_condensed", "turn_id", turnID, "seat", tel.handle,
			"payloads_left_out", condensed.LeftOut, "texts_cut", condensed.Texts,
			"intents_cut", condensed.Intents, "tallies_folded", condensed.Identities,
			"rounds_calls_folded", condensed.Calls, "reviews_cut", condensed.Reviews,
			"rounds_dropped", condensed.Rounds, "bytes", len(blob),
			"detail", "the parked turn's closed rounds were condensed to fit its share of the run's "+
				"row; every call keeps its tool, its identity or its tool's count, and its outcome, and a "+
				"dropped round its calls by tool")
	}
	// The iteration rides BESIDE the blob: the coordinator files each
	// launch's own record under it and never decodes the state.
	suspended, err := rt.pending.MarkSuspended(ctx, turnID, sandbox.Suspension{
		State: blob, Iteration: suspension.State.Round,
	})
	if errors.Is(err, sandbox.ErrRunEnding) || (err == nil && !suspended) {
		// THE RUN WAS ENDED UNDER ITS LAUNCH. Where a person's stop ended
		// it, the turn is stopped, not a suspension that failed: the stop
		// has already reclaimed the box and announced the run, and the turn
		// is closed here, for whichever frame was parking it, from what
		// this segment's charge left unpaid ([Engine.closeStoppedAtPark]) —
		// see [Engine.stoppedAtPark].
		if stop := e.stoppedAtPark(ctx, rt, turnID, tel.handle); stop != nil {
			log.InfoContext(ctx, "sandbox_park_stopped", "turn_id", turnID, "seat", tel.handle,
				"detail", "a person stopped the seat's running work between the run's launch "+
					"and its park; the run is ended and its turn closed as stopped")
			e.closeStoppedAtPark(ctx, tel, charge, stop)
			return false, stop
		}
		// ANY OTHER ENDING decided under the launch is one the settle
		// below finishes, as it finishes a launch no longer launching.
		err, suspended = nil, false
	}
	if err != nil {
		e.failSuspension(ctx, rt, turnID, "sandbox_suspension_unwritable",
			"the parked turn state could not be written", err)
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

// stoppedAtPark is the stop a person's pause made of a launch whose park was
// refused, or nil where no stop explains it.
//
// FROM THE STOP'S OWN DECISION, NEVER FROM THE PAUSE. A stop finishes in the
// time a box takes to die, which is often less than a turn takes to unwind to
// its park, and deletes the row as it goes; and the person can resume the seat
// before the park comes. Read off the pause then, the refused park was taken
// for a suspension with nowhere to go, nothing closed the turn, and its park
// was its last record — parked for good while its run read stopped. So the
// coordinator keeps the decision for this frame ([sandbox.Coordinator.LaunchStopped]),
// and the row still says so where the stop's ending is on it — a stop another
// node decided — naming who made it ([sandbox.PendingRun.Stop]).
func (e *Engine) stoppedAtPark(ctx context.Context, rt *sandboxRuntime, turnID, handle string) error {
	if stop, ok := rt.coordinator.LaunchStopped(turnID); ok {
		return &stopError{pause: pauseOfStop(stop), handle: handle}
	}
	if run, found, err := rt.pending.Get(ctx, turnID); err == nil && found &&
		run.Ending != nil && run.Stop != nil {
		return &stopError{pause: pauseOfStop(*run.Stop), handle: handle}
	}
	return nil
}

// failSuspension settles a run whose suspension has nowhere to go, and says
// why, in the one voice all four failure paths share.
//
// SETTLED, NOT MARKED. The job is already executing in its box, and writing a
// failed status onto the record stranded that box: a record that is not
// active is read by no recovery pass and polled by no waiter, so the box ran
// on, holding its node's disk and memory, with nothing left to reclaim it. The
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
		types.ExecutorFailureSuspensionUnrecorded, detail); err != nil {
		log.WarnContext(ctx, "sandbox_suspension_settle_failed", "turn_id", turnID, "error", err,
			"detail", "the run's record could not be read, so its box was not reclaimed; the "+
				"seat's next recovery pass reaps a run left launching")
	}
}

// launchHold is one executor launch's hand-off of its turn's code holds to the
// run row it opens ([agentLauncher]).
//
// THE SET IS SEALED FROM ITS READ TO THE ROW: the launch reads the set
// ([turnctx.Holds.Seal]) and hands it to the row it opens
// ([sandbox.LaunchRequest.CodeHolds]); a hold another call takes in between
// waits, and the row's writer attaches once the row exists
// ([sandbox.LaunchRequest.Opened], before any box), after which every hold is
// on the row before its request. A RELAUNCH reads the set unsealed (its reset
// unions it with the row's). A launch failing before the row existed unseals
// the set; one failing after detaches the writer, the row is abandoned and the
// turn holds in memory.
type launchHold struct {
	set     *turnctx.Holds
	pending sandbox.PendingStore
	turnID  string
	// ids is the set as the launch read it, which the row is opened with.
	ids []string
	// rowOpened reports the row's writer attached. Set by the launch's
	// Opened, which the coordinator calls inline on the launching
	// goroutine, before the launch returns.
	rowOpened bool
}

// launchHolds seals set for a launch of turnID's run on pending.
func launchHolds(set *turnctx.Holds, pending sandbox.PendingStore, turnID string) *launchHold {
	return &launchHold{set: set, pending: pending, turnID: turnID, ids: set.Seal()}
}

// opened attaches the row's writer, ending the seal: the launch's
// [sandbox.LaunchRequest.Opened].
func (h *launchHold) opened(string) {
	h.set.Attach(holdWriter(h.pending, h.turnID))
	h.rowOpened = true
}

// settle ends the seal of a launch that failed: unsealed where no row
// existed, detached where one was opened and is being abandoned. A launch that
// succeeded leaves the writer attached.
func (h *launchHold) settle(err error) {
	switch {
	case err == nil:
	case h.rowOpened:
		h.set.Detach()
	default:
		h.set.Unseal()
	}
}

// launchFence is the seat lease this node writes on a seat's behalf under: the
// run's row is stamped with it and every later write carries it
// ([sandbox.CoordinatorOptions.Lease]). The seat's next holder fences the row to
// its newer lease, and this node's writes are then refused. The owner is
// [seat.Host.Owner], which the acquisition recovers its runs under.
//
// THREE ANSWERS: the lease, held; the zero fence, held (an unfenced write), on a
// node with no seat host; and NOT HELD where the host does not hold the seat,
// for which every write is refused ([sandbox.ErrSeatNotHeld]). Not-held must not
// be the zero fence, which constrains nothing: a node that noticed it lost the
// seat would take a person's answer past its successor's fence. A node that has
// NOT noticed answers its old lease, which the successor's fence outranks.
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
// that launched it: the resume, days later in another process, can recover none
// of it, and copies drift (one put the partition in both conversation fields,
// which compiles because both are strings).
//
// EVERY FACT OFF THE TURN'S OWN EPOCH, never the engine's current company: a
// company renamed by an apply mid-turn derives a DIFFERENT agent id for the same
// seat, splitting it in two on every surface grouping by id. The TRACE comes
// from the ACTIVE span so the box's spans nest under the call that started
// them; an empty trace id gets no telemetry token (RunEnv refuses to scope one).
func sandboxTurnRef(ctx context.Context, t *turnctx.Turn, role string,
	ask *sandbox.ColleagueAsk,
) sandbox.TurnRef {
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
		// turn had no way to say where its answer belonged, and its rows on
		// the executor log named no conversation, so the conversation's next
		// turn neither continued its session nor was told how it ended.
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
		WorkItem: t.WorkItem, Answered: slices.Clone(t.Answered),
		// AND WHO ASKED, which the park resolves a question addressed to
		// "the requester" against — see [sandbox.PendingRun.Requester].
		Requester: t.Requester, WorkingFor: t.WorkingFor,
		// OR THE CREDENTIAL that did, which a resumed segment's every call
		// is still bounded by — see [sandbox.PendingRun.RequesterLogin].
		RequesterLogin: t.RequesterLogin,
		RequesterApps:  slices.Clone(t.RequesterApps),
		// AND WHEN THE TURN'S CAP ENDS, for the poll to end a run that
		// outlasts it — see [sandbox.PendingRun.Deadline].
		Deadline: t.Deadline,
		// AND THE COLLEAGUE'S ASK the turn answers, for the frame that ends
		// it — see [sandbox.PendingRun.ColleagueAsk].
		ColleagueAsk: ask,
	}
}

// sandboxMCP renders the seat's SCOPED harness MCP surface.
//
// Only the servers role.sandbox.mcp.servers names, as a box has no per-tool
// control left; never one turned off ([config.MCPServer.IsEnabled]), not granted
// ([config.MCPServer.Grants]), at an address the engine would not dial
// ([config.MCPAddressProblem]) or with any tool held back
// ([config.MCPToolPolicy.AllowsEveryTool]): the box dials past the engine.
//
// RESOLVED HERE, for the scoped servers, with the seat's own inherited
// credentials, by the rule every server start passes ([resolveServer]): the
// harness cannot expand `${VAR}`. A SERVER WHOSE VALUE DOES NOT RESOLVE IS NOT
// OFFERED (an empty token acts as nobody); [Engine.boxServers] says it.
func sandboxMCP(
	env *config.Resolver, c *Company, seat *org.Role, settings *org.RoleSandbox,
	mint *seatMint,
) (map[string]sandbox.MCPServer, []*credentialMissing) {
	if len(settings.MCP.Servers) == 0 {
		return nil, nil
	}
	configured := make(map[string]*config.MCPServer, len(c.Config.MCPServers))
	for i := range c.Config.MCPServers {
		configured[c.Config.MCPServers[i].Name] = &c.Config.MCPServers[i]
	}
	var (
		servers  []sandbox.MCPServer
		scoped   = make([]string, 0, len(settings.MCP.Servers))
		withheld []*credentialMissing
		seen     = make(map[string]struct{}, len(settings.MCP.Servers))
	)
	for _, name := range settings.MCP.Servers {
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
		if !server.IsEnabled() {
			// OUT OF THE SCOPE TOO, unlike an unknown name: the renderer
			// would report one the company declares no server under, and
			// this one is declared and turned off. Not resolved either,
			// since a value it would need is nobody's concern while off.
			log.Info("sandbox_mcp_server_off", "seat", seat.Handle(), "server", name,
				"detail", "the executor is not given this server: the company has "+
					"turned it off, and an executor launched once it is turned back on "+
					"is given it")
			continue
		}
		if limit := seat.LimitOn(name); limit.Off {
			// OUT OF THE SCOPE, as one turned off on its entry is: a limit
			// on the way down to this seat turns it off, and keeps the
			// listing for the day it is lifted.
			log.Info("sandbox_mcp_server_limited", "seat", seat.Handle(), "server", name,
				"detail", "the executor is not given this server: a limit turns it off for "+
					"this seat (mcp_limits on the seat or a unit above it), and an executor "+
					"launched once the limit is lifted is given it")
			continue
		}
		if _, signed := server.SignedBinding(); signed {
			// OUT OF THE SCOPE: the engine signs each request to it with the
			// seat's session, which never enters the box. The box's own CLI
			// reaches the vendor through its credentials file instead.
			log.Info("sandbox_mcp_server_signed", "seat", seat.Handle(), "server", name,
				"detail", "the executor is not given this server: the engine signs each request "+
					"to it with the seat's own session, which never enters the box")
			continue
		}
		if !server.Grants(seat) {
			// OUT OF THE SCOPE, as one turned off is: declared, and not
			// this seat's. Admission refuses the listing, so only a stored
			// revision reaches here.
			log.Warn("sandbox_mcp_server_not_granted", "seat", seat.Handle(), "server", name,
				"detail", "the executor is not given this server: it is not granted to "+
					"this seat (mcp_servers[].access, or the integration lane for a server "+
					"reading each agent's own credential)")
			continue
		}
		if !server.Tools.AllowsEveryTool() || len(seat.LimitOn(name).Tools) > 0 {
			// OUT OF THE SCOPE, as one not granted is: a box reaches a
			// server whole, so a level that holds a tool back, on the
			// server's entry or in a limit on the way down to this seat,
			// could not hold there. Admission refuses the listing, so only
			// a stored revision reaches here.
			log.Warn("sandbox_mcp_server_levelled", "seat", seat.Handle(), "server", name,
				"detail", "the executor is not given this server in its box: some of its "+
					"tools are held back from this agent (mcp_servers[].tools, or mcp_limits on "+
					"the seat or a unit above it), and a box reaches a server whole; the seat "+
					"reaches its tools over the bridge, each at its level")
			continue
		}
		if server.OAuth != nil {
			// A BOX CANNOT RENEW A SIGN-IN: it is handed a server's values
			// once, at its launch, and admission refuses the listing; one a
			// stored revision still carries reaches the server through the
			// bridge instead.
			log.Warn("sandbox_mcp_server_refused", "seat", seat.Handle(), "server", name,
				"detail", "the executor is not given this server: it is signed in to with "+
					"OAuth, and the box reaches its tools through the bridge")
			continue
		}
		values, missing := resolveServer(server, seat.Handle(), "", seat.MCPEnv[name], env, mint)
		if missing != nil {
			withheld = append(withheld, missing)
			continue
		}
		if server.Kind() == config.TransportHTTP {
			// THE ADDRESS AS THE BOX WOULD DIAL IT, refused by the rule
			// that keeps the engine's own child of the server from dialling
			// it ([serverSpec]); out of the scope, like a server turned off.
			if problem := config.MCPAddressProblem(values.url); problem != "" {
				log.Warn("sandbox_mcp_server_refused", "seat", seat.Handle(), "server", name,
					"detail", "the executor is not given this server: its url "+problem)
				continue
			}
		}
		box := sandbox.MCPServer{
			Name: server.Name, Transport: sandbox.Transport(server.Kind()),
			Command: server.Command, Args: server.Args, URL: values.url,
			// For the run's brief to say: the box reaches this server past the
			// bridge, whose endpoint would say it as the server's instructions.
			Description: server.Description,
		}
		if server.Kind() == config.TransportHTTP {
			box.Headers = values.values
			// WHAT ITS URL'S REFERENCES RESOLVED TO, for the scrub of the
			// box's session: a secret kept in a URL's path is in the box's
			// MCP configuration as surely as a header is.
			box.Secrets = referencedValues(env, server.URL)
		} else {
			box.Env = values.values
		}
		servers = append(servers, box)
		scoped = append(scoped, name)
	}
	return sandbox.RenderMCP(servers, scoped), withheld
}

// boxServers is the scoped MCP surface one executor's box is given
// ([sandboxMCP]), with every server withheld for its seat credentials judged
// against the integrations minting them and said, at the level its story
// earns — the seat's own child's ([Engine.withheldForCredentials]): waiting
// for provisioning is info, and the rest a warning, since the run goes ahead
// without the server either way.
//
// WHAT STARTS IT DIFFERS from a seat's own child, and the sentence says so:
// nothing restarts a box's servers, so a server withheld from this run is
// offered to the first launch after what it waits for arrives
// ([credentialMissing.arrival]): the seal reaching this node, or the step an
// issuer waits on, such as the agent's GitHub App being installed.
func (e *Engine) boxServers(
	ctx context.Context, c *Company, seat *org.Role, settings *org.RoleSandbox,
) map[string]sandbox.MCPServer {
	// THE SAME TOKEN LIFE AS THE RUN'S OWN ENVIRONMENT: a server rendered
	// into the box is started there once, with whatever it is handed now.
	env, token := e.seatResolver(ctx, c, seat, runTokenLife)
	servers, withheld := sandboxMCP(env, c, seat, settings,
		&seatMint{kinds: e.mintedFor(c, seat.Handle()), token: token})
	// A SERVER A TOOL OF WHICH CHANGED is not the box's either (ADR-0078):
	// the run's facts at launch, not the configuration's, so outside
	// sandboxMCP, which decides from the revision alone.
	servers = e.withoutChangedServers(ctx, seat.Handle(), servers)
	states := e.mintingStates(ctx, withheld)
	for _, missing := range withheld {
		missing = missing.judged(states)
		attrs := append([]any{"seat", seat.Handle(), "server", missing.server}, missing.logAttrs()...)
		if missing.waiting() {
			log.InfoContext(ctx, "sandbox_mcp_server_withheld", append(attrs,
				"detail", "the harness is not given this server for this launch: "+
					missing.reason()+"; a launch made once "+missing.arrival()+
					" is given it")...)
			continue
		}
		level := slog.LevelWarn
		if missing.reading() {
			level = slog.LevelInfo
		}
		log.Log(ctx, level, "sandbox_mcp_server_withheld", append(attrs,
			"detail", "the harness is not given this server: "+missing.reason())...)
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
// NIL-SAFE, like [maxTurnsFor] and for the same seat: an executor is placed by
// its own providers.llm entry and runs in a box whether or not role.sandbox
// was ever written, and reading the override off a block that
// does not exist panicked that seat's first launch — after the bridge session
// was opened and before anything would have closed it.
func pauseTTL(settings *org.RoleSandbox) *time.Duration {
	if settings == nil || settings.PauseTTLSeconds == nil {
		return nil
	}
	d := seconds(*settings.PauseTTLSeconds)
	return &d
}

// seatSandbox is a seat's own box settings — `role.sandbox` — or nil for a seat
// with none, whose boxes take the catalogue's alone.
//
// KEYED ON THE HANDLE, which is unique by a runnable rule: two seats may share a
// display NAME, and matching on it would hand a seat its namesake's block (its
// setup steps, credentials and box).
//
// ONE LOOKUP, NOT A WALK OF THE DOCUMENT: the seat's [org.Role] holds the block,
// so this asks the running org through the lookup every layer resolves handles
// with. A stored revision carries no seats, so a document walk found nil for
// EVERY seat and silently disabled code work.
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

// sandboxCoordinator is this node's coordinator, or nil where no runtime has
// come up — read live, as [Engine.sandboxManager] is, for the answerer of box
// reads, which is armed before any runtime may exist.
func (e *Engine) sandboxCoordinator() *sandbox.Coordinator {
	if rt := e.sandbox.Load(); rt != nil {
		return rt.coordinator
	}
	return nil
}

// startSandboxFor is [Engine.startSandbox] for the company a node boots on:
// its catalogue built, and the runtime brought up where it reaches a cell.
// An apply builds the catalogue itself, earlier, so that a revision whose
// catalogue cannot be built is refused before anything else moves.
func (e *Engine) startSandboxFor(ctx context.Context, c *Company) error {
	manager, err := buildSandbox(c.Config, e.resolver(), e.sandboxOtel, e.fakePlay)
	if err != nil {
		return err
	}
	// Pinned on the epoch it was built for, as an apply pins its own — see
	// [Company.catalogue].
	c.catalogue = manager
	_, err = e.startSandbox(ctx, manager)
	return err
}

// startSandbox brings up this node's code-work machinery for the first company
// whose catalogue builds a manager, and reports whether this call did.
//
// ONCE PER PROCESS, from a boot or the first apply that reaches a sandbox cell,
// so an unconfigured node still gains code work. Afterwards a revision only
// swaps the manager ([Engine.Apply]'s `sandbox` stage). A NIL MANAGER IS NOT A
// START. SPLIT FROM THE POLL on boot: the coordinator must exist before any seat
// takes a turn, and the poll needs the node. On an apply the node exists, so
// this starts the poll too and PREPARES THE HELD SEATS, which took their leases
// before there was a control topic ([Engine.prepareHeldSeats]).
func (e *Engine) startSandbox(ctx context.Context, manager *sandbox.Manager) (bool, error) {
	if manager == nil {
		return false, nil
	}
	rt, err := e.publishSandbox(manager)
	if err != nil || rt == nil {
		return false, err
	}
	log.Info("sandbox_enabled", "placements", manager.Placements(),
		"default_run_in", string(manager.DefaultPlacement()))
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

// publishSandbox builds this process's runtime over manager and publishes it;
// nil where one is published already. PUBLISHED BEFORE THE SEATS ARE PREPARED
// ([Engine.startSandbox]), so a seat acquired from then on prepares itself.
//
// BUILT AND PUBLISHED UNDER ONE LOCK, never built to lose a race: a coordinator
// holds the process's taker lease from construction
// ([sandbox.CoordinatorOptions.Incarnation]), so a second beside the published
// one would claim the same lease and, stopped as the CAS loser, give it back
// under the published one's keeps, which waiting nodes would read as a taker gone.
func (e *Engine) publishSandbox(manager *sandbox.Manager) (*sandboxRuntime, error) {
	e.sandboxStart.Lock()
	defer e.sandboxStart.Unlock()
	if e.sandbox.Load() != nil {
		return nil, nil
	}
	rt, err := e.buildSandboxRuntime(manager)
	if err != nil {
		return nil, err
	}
	e.sandbox.Store(rt)
	return rt, nil
}

// buildSandboxRuntime builds the machinery [Engine.startSandbox] publishes.
func (e *Engine) buildSandboxRuntime(manager *sandbox.Manager) (*sandboxRuntime, error) {
	if e.backends == nil || e.backends.Fleet == nil {
		// A detached run's RECORD is what survives the turn that starts
		// it, so a node with no coordination store cannot offer code work
		// at all. Refused loudly rather than degraded: a sandbox whose
		// runs vanish is worse than no sandbox, because the box keeps
		// running and holding its node with nobody to collect it.
		//
		// The FLEET's store, not this node's: a run is recovered by
		// whichever node owns its seat next, and that node is not
		// reliably the one that launched it.
		return nil, fmt.Errorf("providers.sandbox needs a coordination store: a detached " +
			"run's state is a fleet record, and without one a seat handoff orphans every box")
	}
	return e.sandboxRuntimeOn(sandbox.NewCoordStore(e.backends.Fleet), manager)
}

// sandboxRuntimeOn is the machinery [Engine.buildSandboxRuntime] builds, over
// the given run store: the coordinator wired to this engine's every seam
// ([Engine.coordinatorOptions]).
func (e *Engine) sandboxRuntimeOn(pending sandbox.PendingStore, manager *sandbox.Manager) (*sandboxRuntime, error) {
	coordinator, err := sandbox.NewCoordinator(e.coordinatorOptions(pending, manager))
	if err != nil {
		return nil, err
	}
	return &sandboxRuntime{pending: pending, coordinator: coordinator}, nil
}

// coordinatorOptions is this engine's every seam as the sandbox coordinator is
// built on it ([Engine.sandboxRuntimeOn]), over the given run store and
// catalogue: a value of its own so what the coordinator is handed can be read
// whole, the taker lease it holds among it.
func (e *Engine) coordinatorOptions(pending sandbox.PendingStore, manager *sandbox.Manager) sandbox.CoordinatorOptions {
	opts := sandbox.CoordinatorOptions{
		Queue: e.backends.Queue, Pending: pending, Manager: manager,
		FailureKind: runner.ErrorKind,
		// A resume this node or its infrastructure failed is handed back
		// uncounted toward the give-up that ends a run, by the answer the
		// seat's deliveries are NAKed by ([nodesFailure]).
		NodesFailure:       nodesFailure,
		ResumableElsewhere: e.resumableElsewhere,
		// And one only an operator clears is said while the run waits for
		// one, by the one classification of conditions ([operatorsCondition]).
		OperatorsCondition: operatorsCondition,
		Resume:             &resumer{engine: e},
		Account:            e.sandboxAccountant(),
		// Who a parked question is put to, resolved against the live
		// chart at the park — see audience.go.
		Audience: audienceResolver{engine: e},
		// A person's answer is sealed onto the run it answers and opened
		// before a resume reads it — see [Engine.answers].
		Answers: e.answers,
		// And what carried each session of the run on, into the run's
		// leads, kept beside its record, for a later brief to tell again:
		// see [Engine.leads].
		Leads: e.leads,
		// A report, a failure or a question past what the run's record
		// carries is condensed by the seat's auxiliary model rather than cut.
		Condense: runCondenser{engine: e},
		// What a run given repositories pushed to Gitlet is read from
		// its push rows when it is collected — see runpushes.go.
		Pushes: e.codeRunPushes(),
		// The per-run tool bridge dies with the run — see
		// [sandbox.CoordinatorOptions.Ended]. Idempotent, and reached
		// from every settle path, so a run that failed before it ever
		// had a box closes its session too.
		Ended: e.bridge.Close,
		// AND IS SERVED AGAIN wherever the run is taken over while its box
		// works: this node after a restart, or the seat's next holder —
		// see [sandbox.CoordinatorOptions.Recovered].
		Recovered: e.reopenAgentRun,
		// AND A JOB'S BRIDGE CONNECTION THIS NODE HOLDS is settled before its
		// row is read for collection (connectmarks.go).
		ConnectSettled: e.connectMarks.settle,
		Unresumable:    e.parkedRunRecovered,
		// AND EVERY NOTE A PERSON SENT THE TURN THAT NO STEP REPORTED
		// READING is recorded by the ending, as the run's record knows it
		// (delivered where a later launch told it again, unconfirmed, missed
		// or expired), before the record goes. See runsteer.go.
		SteersEnded: e.runSteers.ended,
		// AND EVERY NOTE A HOOK'S SERVE HANDED OVER THAT ITS JOB'S SESSION
		// SHOWED IT TOOK is recorded delivered by every read of the job's box
		// that settles its serves. See runsteer.go.
		SteersConfirmed: e.runSteers.confirmed,
		// AND EVERY WAKE A JOB A RELAUNCH REPLACED NEVER READ is reported once
		// the relaunch's write removed it, its delivery left to a turn of its
		// own. See wakeitem.go.
		WakesEnded: e.runSteers.wakesEnded,
		// AND EVERY OTHER CONVERSATION WHOSE WAKE A STEP READ is named by the
		// turn's end, so its next turn continues the session the turn kept.
		// See wakeitem.go.
		Served: e.runSteers.served,
		// AND A SEAT HOLDS UP TO ITS CAP OF RUNS, one per conversation
		// (ADR-0169).
		SeatCap: e.seatRunCap,
		// AND WHILE A RUN HOLDS A SEAT, the seat's held inbox is read and a
		// wake from the run's requester about its work handed to it. See
		// wakerouter.go.
		Busy: e.seatRunsMoved,
		// AND A FOLD-IN A PERSON ANSWERED is read by the seat's router at
		// once. See foldin.go.
		FoldInAnswered: e.nudgeRouter,
		// A run that stops with a turn still suspended into it takes the
		// working indicator down with it, whichever way it stopped: the
		// agent is waiting on a person, or is never coming back at all,
		// and either way the turn does not return to say so. ONE
		// FUNCTION FOR EVERY REASON, because to the indicator they are
		// one fact — the frame that raised it has already returned. See
		// [sandbox.CoordinatorOptions.Stopped].
		Stopped: e.runStopped,
		// A HELD CALL PUT TO DELEGATES wakes them once it is parked, so
		// each finds it waiting (ADR-0125), and a run parked by
		// `wait_for_run` is watched until its run leaves (ADR-0176). See
		// delegated.go and runwaits.go.
		Parked:    e.runParked,
		WaitedFor: e.waitedFor,
		// A turn parked on a run a person stopped is closed on the record
		// as stopped, and its end recorded on the executor log, by
		// whichever node finishes the run's ending — see runstop.go.
		CloseStopped: e.closeStoppedTurn,
		// AND A TURN LOST WITH ITS RUN — the run's box gone, its
		// suspension never recorded, its tail abandoned — tells the
		// colleague whose ask it took up that it was lost, and the
		// executor log how it ended, by whichever node finishes the run's
		// ending: no frame of that turn is left to. See a2a.go.
		TurnLost: e.answerLostTurn,
		// EVERY LAUNCH'S HARNESS SESSION IS KEPT, and every turn's end
		// recorded, through the ESTATE ROUTER's executor writes — a seat's
		// box is wherever its seat runs, a seats-only node included, which
		// holds no log of its own — with the engine's policy, secrets,
		// minting and sealed upload beside them. See executorsession.go.
		Sessions: executorRecordsOf(e.executorLog),
		Keep:     executorKeep{engine: e},
		// AND A RELAUNCH CONTINUES THE TURN'S KEPT SESSION in whatever box it
		// gets, its row read through the same router's reads and its object
		// fetched through the same store. See executorrestore.go.
		Restore: executorRestore{engine: e},
		// AND WHETHER A MINTED SESSION'S ID IS ANY TURN'S, through the same reads.
		Holders: executorRestore{engine: e},
		// A seat's inbox is held while one of its runs holds it or an
		// answer's resume is owed, and a retried resume asks the seat's
		// conditions first — see [seatHold] and [Engine.mayResumeAnswer].
		Hold:  seatHold{engine: e},
		Admit: e.mayResumeAnswer,
		// AND EVERY RESUME, on either route, is a turn this node admits
		// as it admits a mailbox's: the seat owned fresh, a slot under
		// node.max_concurrent, and no drain. See [Engine.admitResume].
		Gate: e.turnGate(),
		// AND EVERY READ OF ITS RUNS' BOXES, the ones that gate does not admit
		// included, holds one of as many collections as it runs turns. See
		// [sandbox.CoordinatorOptions.Collections].
		Collections: e.turns,
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
		// AND WITH THIS NODE, whose host a direct or container box is on:
		// only this node touches such a box, a peer holding its seat asks
		// this one to read it, and this node tells the box's host when it
		// leaves one there — see sandboxhost.go.
		Node:  e.id,
		Boxes: e.boxReader(),
		Nudge: e.nudgeRun,
		// A job continued in its turn's kept box starts on current token
		// files: the hook the waiter runs on every poll, run once more
		// before the job, since no poll reached the box while it was
		// paused and no setup rewrites them (see
		// [sandbox.CoordinatorOptions.Refresh] and issued.go).
		Refresh: e.refreshRunTokens,
		// A final take's wait for spool room, and the room held, recorded in
		// the node's metrics. See spoolmeter.go.
		Spools: spoolMeter{metrics: e.metrics},
		// Each collected launch's verdict on its session, counted and judged
		// against session_carry. See sessionmeter.go.
		Carries: sessionMeter{metrics: e.metrics, objectives: e.objectives},
		// And each turn's conversation told what its runs' sessions lost, on the
		// engine's own chat. See conversationnotice.go.
		Conversations: conversationNotice{engine: e},
		// Every collection of a run's box holds its weight of the node's
		// answer budget, beside its peers' payload answers (answermeter.go).
		AnswerBudget: e.answerBudget,
	}
	// AND THIS PROCESS'S TAKER LEASE, in the node's lease store beside its seats
	// and presence, under the ENGINE'S OWN incarnation minted once in [New] (the
	// node is built after this coordinator on boot), at the seat lease TTL so a
	// stopped process reads as gone as fast as its seats move
	// ([sandbox.CoordinatorOptions.TakerTTL]). Every capture names it, and one
	// another process left begun is settled from the executor log once that
	// lease lapses ([sandbox.SessionCapture.Taker]). Held until the last keep
	// ends ([Engine.stopSandbox]) and given back on the claim's context, which no
	// stop reaches, so through [holdLeases]. With no lease store captures name no
	// taker and are waited for by age ([sandbox.CoordinatorOptions.Incarnation]).
	if e.backends.Coord != nil {
		opts.Incarnation, opts.TakerTTL = e.incarnation, e.leaseTTL
		opts.Takers = holdLeases{Backend: e.backends.Coord, engine: e}
	}
	// AND THE FLEET'S LEDGER OF EACH SEAT'S SAVES, which a save of a running
	// job is charged against before its stamp: the whole company agrees on a
	// seat's allowance, so the coordination store keeps it
	// ([sandbox.CoordinatorOptions.Saves]). With none, no save is taken.
	if e.backends.Fleet != nil {
		opts.Saves = e.backends.Fleet
	}
	return opts
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
	// resumed segment reads it as the executor's answer.
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
// its cause.
func runPartKind(part sandbox.RunPart) compact.Kind {
	switch part {
	case sandbox.PartFailure:
		return compact.KindToolError
	default:
		return compact.KindReport
	}
}

// startSandboxWaiter starts this node's completion poll, once the node exists
// and a runtime does, and at most once: it drives the boxes this node launched,
// which no other node reaches ([sandbox.Waiter]).
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
	opts := e.sandboxWaiterOptions(rt, interval)
	// THE LAUNCH-NODE WATCH, claimed each pass, with WHO IS STILL IN THE FLEET:
	// the one judge ends the runs of a node that left for good rather than
	// waiting on them for ever. The three are set together or not at all
	// ([sandbox.WaiterOptions.Roster]); none without a coordination store,
	// which is one node whose runs are all its own.
	if duty != nil {
		opts.ClaimDuty = sandbox.DutyFunc(duty)
		opts.DutyTTL = waiterDutyTTL(interval)
		opts.Roster = nodeRoster{coord: e.backends.Coord}
	}
	waiter, err := sandbox.NewWaiter(opts)
	if err != nil {
		return err
	}
	if e.sandboxWaiter.CompareAndSwap(nil, waiter) {
		waiter.Start(context.WithoutCancel(ctx))
	}
	return nil
}

// sandboxWaiterOptions is what this node's completion poll is built from,
// before the launch-node watch [Engine.startSandboxWaiter] adds. The refresh
// keeps a long run's issued tokens current in the files the run was
// provisioned with (issued.go), renewed by the poll that reaches the box and
// by nothing else.
func (e *Engine) sandboxWaiterOptions(rt *sandboxRuntime, interval time.Duration) sandbox.WaiterOptions {
	return sandbox.WaiterOptions{
		Queue: e.backends.Queue, Pending: rt.pending,
		// THE COORDINATOR'S CURRENT MANAGER, asked on every tick: an apply
		// swaps it, and the poll outlives every revision.
		Manager:  rt.coordinator.Manager,
		Node:     e.id,
		Interval: interval,
		Refresh:  e.refreshRunTokens,
		// AND WHERE THE REFRESH RECORDS EACH TOKEN BEFORE IT WRITES IT: the
		// job's sealed set beside its row, which every capture of the job's
		// session is scrubbed by (issued.go, "Recorded before it is written").
		Record: rt.coordinator.RecordHanded,
		// AND EVERY BOX IT FINDS RUNNING IS OFFERED FOR A SAVE of its job's
		// session as it works: decided from the coordinator's memory and taken
		// on its lifetime, never the poll's ([sandbox.WaiterOptions.Save]).
		Save: rt.coordinator.SaveRunning,
	}
}

// stopSandbox halts the poll loop and the coordinator's own retries. The rows
// and the boxes are untouched: a detached run belongs to its row — an answer
// still owed its resume included — and the next owner of its seat recovers it,
// while a box on this host waits for this node's next start.
//
// AFTER THE DRAIN, the order [Engine.Stop] and [Engine.teardown] keep: the drain
// gives presence up first while turns finish and captures upload, and the taker
// lease is held through it, since a peer settles a capture whose taker's lease
// is gone. The stop lets running keeps end and releases the lease after the
// last ([sandbox.Coordinator.Stop]), through [holdLeases], one step of the
// allowance every lease give-back shares ([Engine.stopping]).
func (e *Engine) stopSandbox() {
	if w := e.sandboxWaiter.Load(); w != nil {
		w.Stop()
	}
	if rt := e.sandbox.Load(); rt != nil {
		rt.coordinator.Stop()
	}
}

// sandboxSeatRuns is the inbox screening's two sandbox answers for a seat, read
// off the runtime live: whether its runs hold every slot of its cap, and
// whether one waits for an answer, in one read; no to both where there is no
// runtime, which is right for a node that has never run code.
func (e *Engine) sandboxSeatRuns(handle string) (atCap, awaitsAnswer bool) {
	rt := e.sandbox.Load()
	if rt == nil {
		return false, false
	}
	count := rt.coordinator.SeatCount(handle)
	return count.AtCap(), count.Awaiting > 0
}

// holdAside is the dispatcher's [Dispatcher.HoldAside], through the
// coordinator current when the delivery arrives.
func (e *Engine) holdAside(ctx context.Context, handle string) {
	if rt := e.sandbox.Load(); rt != nil {
		rt.coordinator.HoldForAside(ctx, handle)
	}
}

// answerParkedRun offers a delivery to a parked executor, through the
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

func (h seatHold) Hold(ctx context.Context, handle string, at queue.Attachment) error {
	subject, group, err := h.inboxOf(handle)
	if err != nil {
		return err
	}
	return h.engine.backends.Queue.PauseTopic(ctx, subject, group, at, string(inbox.HoldSandbox))
}

func (h seatHold) Release(ctx context.Context, handle string, at queue.Attachment) error {
	subject, group, err := h.inboxOf(handle)
	if err != nil {
		return err
	}
	return h.engine.backends.Queue.ResumeTopic(ctx, subject, group, at, string(inbox.HoldSandbox))
}

// Readmit hands back the seat's mail set aside under key, freed by turn's run
// leaving it ([queue.EventQueue.Readmit]): seat_wake_readmitted names both.
func (h seatHold) Readmit(ctx context.Context, handle, key, turn string) (int, error) {
	subject, group, err := h.inboxOf(handle)
	if err != nil {
		return 0, err
	}
	n, err := h.engine.backends.Queue.Readmit(ctx, subject, group, key)
	if n > 0 {
		log.InfoContext(ctx, "seat_wake_readmitted", "seat", handle, "key", key, "turn_id", turn,
			"deliveries", n, "detail", "the run they waited on left the seat; they run at the head of its inbox")
	}
	return n, err
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

	// waitSeatTurns: the seat runs as many turns on this node as its cap, or
	// one of the answer's conversation ([node.ErrSeatFull],
	// [node.ErrConversationBusy]). Signalled the moment one of them is
	// released ([node.Config.SeatTurnEnded]), so the answer is resumed as the
	// seat's next turn, ahead of its mail, which its owed hold keeps waiting.
	waitSeatTurns sandbox.Condition = "seat_turns"

	// waitProvider: every model of the seat's chain is at a usage limit it
	// already reported, or this node holds the seat parked on one. Signalled
	// by every release of a provider park and by every published company,
	// and otherwise lifted on the clock when the first is tried again, which
	// the refusal names (see providerpark.go).
	waitProvider sandbox.Condition = "provider"
)

// A draining node's refusal is [sandbox.Draining], which the sandbox package
// names because it decides whether a refusal leaves an acknowledgement owed:
// nothing in the engine undoes a drain ([Engine.Drain] latches), so the wait
// ends when the drain hands the seat back, which lets go of the answer for the
// seat's next holder to resume ([sandbox.Coordinator.ReleaseSeat]), or when
// the node stops.

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
// answer's retry waits on, and then the seat's conditions the route's turn
// would only fail on ([Engine.seatCannotRun]).
//
// The condition is what the control topic hands a refused completion back by
// ([controlOutcome]). AN ENGINE WITH NO NODE ADMITS UNDER THE CALLER'S CONTEXT
// (only a hand-built one lacks it). The node is asked HERE, not where the gate
// is built ([Engine.turnGate]): boot builds the coordinator before assigning
// the node, so choosing then would leave every node ungated.
func (e *Engine) admitResume(ctx context.Context, handle, conversation string, route sandbox.ResumeRoute) (context.Context, func(), error) {
	if e.node == nil {
		return ctx, func() {}, nil
	}
	ctx, release, err := e.node.AdmitTurn(ctx, handle, conversation)
	switch {
	case err == nil:
		if refusal, refused := e.seatCannotRun(ctx, handle, route); refused {
			release()
			return nil, nil, &sandbox.TurnRefused{Refusal: refusal}
		}
		return ctx, release, nil
	case errors.Is(err, node.ErrDraining):
		return nil, nil, &sandbox.TurnRefused{Cause: err, Refusal: sandbox.Refusal{
			Condition: sandbox.Draining, Reason: err.Error()}}
	case errors.Is(err, node.ErrSeatFull), errors.Is(err, node.ErrConversationBusy):
		return nil, nil, &sandbox.TurnRefused{Cause: err, Refusal: sandbox.Refusal{
			Condition: waitSeatTurns, Reason: err.Error()}}
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

// seatRunCap is how many of handle's runs hold its seat at once, and how many
// of its turns a node admits ([node.Config.SeatTurns], [sandbox.CoordinatorOptions.SeatCap]):
// the seat's sandbox.max_concurrent_runs in the current epoch, the default for
// a seat the epoch does not resolve (ADR-0169).
func (e *Engine) seatRunCap(handle string) int {
	var box *org.RoleSandbox
	if role := e.seatRole(handle); role != nil {
		box = role.Sandbox
	}
	return box.ConcurrentRuns()
}

// besideFloor is what handle's other runs holding its seat are counted to
// spend before their collections charge it ([launchFloor.beside]): the static
// floor for each run held besides turn's own, which a relaunch of it already
// holds. The static floor, because a running run's spend so far is nowhere
// until its collection, and its first request is at least that.
func (e *Engine) besideFloor(c *Company, handle, turn string) int {
	rt := e.sandbox.Load()
	if rt == nil {
		return 0
	}
	return rt.coordinator.HeldBeside(handle, turn) * staticFloor(c).static
}

// seatCannotRun reports whether the turn route resumes on handle's seat would
// only fail on a condition that ends at an instant, and what it waits on: the
// budget, a park for its models' limit ([Engine.providerParkRefusal]), or a
// limit every model of its first call reported ([Engine.providerRefusing]).
//
// BY ROUTE: a COMPLETION is asked no budget and no limit, its tokens being
// spent: it is collected first, then held on its row ([Engine.heldByBudget]) or
// on this node ([Engine.collectedRefusal]); refused before the claim, its spend
// landed in the next window. An ANSWER'S first call is the relaunch
// ([Engine.budgetHold], [executorRunners]). THE PARK AND THE FAILURE PAUSE
// ([Engine.failurePauseRefusal]) REFUSE A COMPLETION BEFORE THE CLAIM, its job
// being already collected; a refused one is acknowledged ([controlOutcome]).
func (e *Engine) seatCannotRun(ctx context.Context, handle string, route sandbox.ResumeRoute) (sandbox.Refusal, bool) {
	// A COMPLETION ON A SEAT A PERSON STOPPED is refused nothing here: the
	// turn it re-enters is closed as stopped at its first round by the seat's
	// fence, before any model call or launch, so neither the budget nor a
	// limit can refuse it, and held back for either it kept the work a person
	// asked to stop open until the condition cleared
	// ([sandbox.Coordinator.StopSeat] ends the wait it was held in).
	if route == sandbox.ResumeCompletion && e.seatStopping(handle) {
		return sandbox.Refusal{}, false
	}
	c := e.Company()
	// A COMPLETION ON A SEAT THE ENGINE PAUSED FOR A FAILURE THAT WILL
	// REPEAT waits for the pause to lift: see [Engine.failurePauseRefusal].
	// An answer's is refused on any pause already ([Engine.mayResumeAnswer]).
	if route == sandbox.ResumeCompletion {
		if refusal, held := e.failurePauseRefusal(handle); held {
			return refusal, true
		}
	}
	if route == sandbox.ResumeAnswer {
		// THE STATIC FLOOR: the gate is asked before the claim and names no
		// run, so it cannot know the brief the answer's relaunch is handed,
		// nor the session it continues. A relaunch whose own floor is above
		// it is refused at its launch and held on its window there
		// ([Engine.heldByBudget]), so the answer waits either way rather than
		// circling. And one held below it ([launchFloor.held]), at the room
		// its segment's own spend leaves, goes out in the windows the gate
		// reads only where each had that room and that spend left when the
		// gate asked: the ceiling of the window leaving the least room, which
		// config holds at or above the static floor. So in those windows the
		// gate holds back no answer its relaunch could go out on.
		if refusal, held := e.budgetHold(ctx, c, handle, nil); held {
			return refusal, true
		}
	}
	if refusal, parked := e.providerParkRefusal(handle); parked {
		return refusal, true
	}
	if route == sandbox.ResumeAnswer {
		if refusal, _, limited := e.providerRefusing(ctx, c, handle, route); limited {
			return refusal, true
		}
	}
	return sandbox.Refusal{}, false
}

// seatStopping reports whether a person's pause asks handle's seat to stop
// its running work ([Engine.stopFor]): what the seat's fence closes a turn
// on at its next round.
func (e *Engine) seatStopping(handle string) bool {
	return e.seatStop(handle) != nil
}

// seatStop is the stop a person's pause asks of handle's seat, as the seat's
// fence closes a turn on it ([Engine.stopFor]), nil where none does: a seat
// the running company does not name has no pause this node keeps either.
func (e *Engine) seatStop(handle string) error {
	if seat, err := e.seatID(handle); err == nil {
		return e.stopFor(seat, handle)
	}
	return nil
}

// resumeFailure is how a resumed segment that broke goes on, decided once by
// [Engine.judgeResumeFailure] before the segment's completion is published:
// that record says whether the turn ended, and a segment that is handed back,
// held or refused to wait did not end it. The zero value is a segment handed
// back for another attempt, or refused on a wait.
type resumeFailure struct {
	// stop is a person's stop that settles the segment as stopped, and
	// stopWhy the reason its settle names.
	stop    error
	stopWhy string
	// held is the seat's budget holding the segment until its window turns
	// over: a wait, which the same turn resumes from.
	held *sandbox.HeldError
	// abandon is why the turn is abandoned ([turn.Abandon]), empty for none.
	abandon string
	// runEnded is a run whose ending is decided or whose record is gone.
	runEnded bool
}

// endsTurn reports whether the segment ends its turn: settled as stopped or
// abandoned. A held segment, and one reverted for another attempt or a wait,
// is resumed again under the same turn id.
func (f resumeFailure) endsTurn() bool {
	return f.stop != nil || f.abandon != "" || f.runEnded
}

// judgeResumeFailure decides how a resumed segment that failed with err goes
// on ([resumeFailure]), in the order [Engine.resumeTurn] acts on it.
func (e *Engine) judgeResumeFailure(ctx context.Context, company *Company, in resumeInput,
	res turn.Result, err error,
) resumeFailure {
	handle := in.Turn.Handle()
	// A PERSON STOPPED THE RESUMED TURN: its seat was paused with a stop
	// after the run's completion was claimed, or while its collected job
	// waited on this resume, held or retried, both of which a stop leaves to
	// this turn (the run's own stop ends only a job still out:
	// [sandbox.Stoppable]), and the turn ended at its first round, its
	// charge above the job's only one. It is settled rather than reverted,
	// for the reason the dispatcher spends a stopped trigger: a revert brings
	// the completion straight back, and the resume would be stopped again on
	// every delivery until the broker dead-lettered it. The run's box is
	// reclaimed with it; stopping the turn is what was asked.
	if turn.Stopped(err) {
		return resumeFailure{stop: err, stopWhy: "a person stopped the resumed turn"}
	}
	// AND A WAIT THE SEGMENT MET WHILE A PERSON'S STOP LANDED is decided by
	// the stop: the seat was paused with a stop while one of the segment's
	// calls was out, and that call came back refused for a limit its models
	// report or the seat's budget. The turn's fence would have closed it as
	// stopped at its next round, and there is none: reverted on the wait, the
	// job was held on its node or its row until the limit's instant or the
	// window's turn, up to the hour an entry is benched for, the seat parked
	// and probed meanwhile, while the stop stood unanswered, since a stop
	// leaves a claimed run to its turn. Settled here as stopped instead, as
	// the fence settles it, and nothing is parked.
	if stop := e.seatStop(handle); stop != nil && e.resumeWaits(ctx, company, handle, err) {
		return resumeFailure{stop: stop, stopWhy: fmt.Sprintf("a person stopped the seat while "+
			"its resumed turn met a wait: %v", err)}
	}
	// A BUDGET THAT REFUSES THE SEGMENT HOLDS IT, before anything decides it is
	// lost: the relaunch or a call of its own was refused because the seat's
	// window is full or too short for a launch. A window turns over, so this is
	// a wait, not a failure: an ordinary retry re-collected the job and re-ran
	// its review into the same refusal. Held, the job waits collected in its
	// paused box until the window turns over or a revision raises the ceiling
	// ([sandbox.ErrResumeHeld]); an answer's resume is reverted and waits
	// uncharged on the same refusal ([Engine.seatCannotRun]). AHEAD OF THE
	// ABANDON, so a segment that wrote in its own attempt waits too rather than
	// lose the run's work; calls made before the segment are no such write
	// ([turn.Work.Inherited]).
	if held := e.heldByBudget(ctx, company, handle, err); held != nil {
		return resumeFailure{held: held}
	}
	// A RESUMED TURN QUALIFIES FOR THE ABANDON BY WHAT ITS OWN ATTEMPT DID,
	// as any turn does: what its run did over the bridge before this segment
	// collected it is an earlier attempt's ([turn.Work.Inherited]), and a
	// retry collects the same run rather than make any of it again. It
	// qualifies too when it panicked, and when it was refused for a reason
	// every retry would meet again ([turn.ErrPermanent]: its round lost with
	// its host past its limit): a refusal only a resume meets, so it reaches
	// [turn.Abandon] through this call alone.
	if reason, abandon := turn.Abandon(res, err); abandon {
		return resumeFailure{abandon: reason}
	}
	// A RETRY NEEDS THE RUN'S RECORD AS THE CLAIM LEFT IT, and a relaunch
	// that failed in its launch (no box, no attach, no start) abandoned
	// the run under it ([sandbox.Coordinator.Launch]): its ending is decided,
	// and its record waits for the resume's frame to say how the turn ended
	// before it goes ([sandbox.Coordinator.OweEnd]). Reverted, the claim
	// would find nothing to hand back, the completion nothing to claim again,
	// and the turn would end in silence, its colleague unanswered and its end
	// nowhere. So a segment that broke on a run whose ending is decided, or
	// whose record is gone, ends the turn, as one abandoned, and settles
	// rather than reverts.
	if e.runEnded(ctx, in.Run.TurnID) {
		return resumeFailure{runEnded: true}
	}
	return resumeFailure{}
}

// resumeWaits reports whether err, a resumed segment's failure, is a WAIT
// rather than a failure: a budget that holds it ([Engine.heldByBudget]) or a
// limit every model of its chain reported ([resumeRefusal]) — what a resume is
// reverted to wait on, and what a person's stop decides instead.
func (e *Engine) resumeWaits(ctx context.Context, company *Company, handle string, err error) bool {
	if _, refused := resumeRefusal(err); refused {
		return true
	}
	return e.heldByBudget(ctx, company, handle, err) != nil
}

// resumeRefusal is what a reverted resume's failure waits on, if it is the
// provider half of [Engine.seatCannotRun]'s conditions met mid-turn: a chain
// every member of which refused for a limit — the reviewer's, or the executor
// members a relaunch picks from, every one sitting out ([llmchain.Pick]) — until
// the first is tried again.
//
// NOT THE BUDGET HALF. A segment the seat's budget refused is held before the
// resume is judged at all ([Engine.heldByBudget], ahead of [turn.Abandon]): on
// a full window, and on one too short for the launch every relaunch makes, and
// whether or not the segment had acted. A spent window reaching this far is
// one that has turned over since, which an ordinary retry now runs.
func resumeRefusal(err error) (sandbox.Refusal, bool) {
	if limit, limited := llmchain.Limited(err); limited {
		return sandbox.Refusal{Condition: waitProvider, Reason: limitReason(limit.Keys, limit.RetryAt),
			Until: limit.RetryAt}, true
	}
	return sandbox.Refusal{}, false
}

// mayResumeAnswer is the coordinator's [sandbox.Admission]: whether a seat may
// run a RETRIED resume of a recorded answer now, and if not, what that waits on.
//
// THE SCREENING'S OWN CONDITIONS, as a delivery reads them: a retry runs outside
// any delivery and must not start a turn on a seat whose lease is stale, that a
// person paused, that has no model, or whose node refuses new work. Budget and
// model limit are the turn gate's ([Engine.seatCannotRun]); the seat's cap is
// the coordinator's count ([sandbox.SeatFull], [sandbox.ConversationBusy]).
//
// EACH REFUSAL NAMES WHAT IT WAITS ON, so the retry waits for that rather than
// polling ([sandbox.Coordinator.Readmit]). Ownership is an event
// (node.Config.SeatAdmitted) with one heartbeat behind it for a late renew.
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
// the node's admission refused, and lifts a collected run's resume held on it
// ([sandbox.PendingRun.HeldFor]); see [sandbox.Coordinator.Readmit]. It is THE
// hook for whatever ends a condition a refusal names: call it with that
// condition and the seat, and a refused completion resumes now rather than at
// the waiter's next announcement of it. Nothing to do on a node with no
// sandbox runtime, which owes no answer, kept no completion and holds no
// resume.
func (e *Engine) readmitResumes(cond sandbox.Condition, handles ...string) {
	if rt := e.sandbox.Load(); rt != nil {
		rt.coordinator.Readmit(cond, handles...)
	}
}

// AnswerSeal is the seal this node's coordinator records a person's answer to a
// parked run under, over the fleet keyring ([sandbox.CoordinatorOptions.Answers]):
// for the surfaces that name who gave a run's recorded answer, which only the
// seal says ([sandbox.OpenedAnswerer]). Nil on an engine assembled without one.
func (e *Engine) AnswerSeal() *sandbox.AnswerSeal { return e.answers }

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

// waiterDutyName is the fleet singleton this node's waiter claims each pass:
// the launch-node watch, the one judge of which launch nodes left the fleet
// for good ([sandbox.DutyFunc]).
const waiterDutyName = "launch-node-watch"

// waiterDuty is the claim of the launch-node watch for this node's waiter.
//
// Nil where there is no coordination backend, which is the single-node case:
// that node's runs are all its own, and no node can leave its fleet.
//
// An interval whose duty no backend will grant is refused HERE, at start,
// rather than on every pass, where a refused claim would leave no node judging
// gone launch nodes, with one warning per pass as the only sign. [New] asks the
// same question before anything is open, because the start can be an apply's
// (see [checkSandboxPollInterval]).
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

// checkSandboxPollInterval refuses a poll interval whose launch-node watch no
// backend will grant.
//
// ASKED AT BOOT whether or not this node runs a sandbox yet, as well as where
// the poll starts: a node that boots with none starts its poll at whichever
// apply brings the first, and refused THERE the interval would leave a runtime
// published with no poll — every run launched on it never collected — on a
// revision that was itself fine. A refused TTL would cost only the verdicts on
// gone launch nodes, since the node's own polls never depend on the claim, but
// an Option is the operator's, so it is refused where the operator is still
// looking.
func checkSandboxPollInterval(interval time.Duration) error {
	if err := coord.CheckLeaseTTL(coord.WorkerResource(waiterDutyName), waiterDutyTTL(interval)); err != nil {
		return fmt.Errorf("engine: a sandbox poll interval of %v needs a %v launch-node watch; "+
			"lower Options.SandboxPollInterval: %w", interval, waiterDutyTTL(interval), err)
	}
	return nil
}

// dutyTTLTicks is how many poll intervals the launch-node watch survives
// without a re-claim, and dutyTTLFloor the shortest it may ever be.
//
// Three intervals is the same "do not flap on a blip" rule the scheduler duty
// and the seat heartbeat follow: the holder re-claims every tick, so this
// rides out two consecutive slow or failed claims without the duty moving. The
// invariant is the RATIO, not any duration — a duty that cannot outlive three
// of its own ticks moves on ordinary jitter, and one that outlives many is
// time the fleet has no judge of gone launch nodes after a holder dies.
//
// The floor is for the other end: this code is driven at a sub-second cadence
// in tests, and three of those is a lease that lapses inside its own claim.
const (
	dutyTTLTicks = 3
	dutyTTLFloor = 30 * time.Second
)

// waiterDutyTTL is how long the launch-node watch survives without a re-claim.
//
// DERIVED FROM THE INTERVAL IT GUARDS, and from nothing else. It used to be
// `3 * sandbox.DefaultPollInterval`, a compile-time constant that ignored the
// configured interval entirely, and then it was capped at the seat lease TTL,
// because duties shared the seat lease bucket and the KV refused a lease
// longer than that bucket's age. The cap was wrong in its own right: a poll
// interval longer than the seat lease TTL got a duty that lapsed between two
// of its own ticks, so the watch moved to whichever peer ticked first on
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
// and start listening for their completions, BEFORE its mailbox opens. inbox is
// the attachment of the mailbox the node is about to make, which every hold of
// it is taken for until the seat's release (seatinbox.go).
func (e *Engine) prepareSeat(ctx context.Context, seat placement.Seat, epoch int64, owner string,
	inbox queue.Attachment) error {
	handle := seat.Handle
	// THE ATTACHMENT FIRST, before any step below can hold the seat's inbox,
	// and with it a person's pause hold where the seat is paused, taken
	// before the node attaches the inbox (seatpause.go).
	if err := e.acquireInbox(ctx, seat.ID, inbox); err != nil {
		return err
	}
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
	// FOR EVERY SEAT, not only a sandbox node's: this sat behind the
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
	// The subscription is attached BEFORE the recovery reads the seat's
	// runs, so a completion published in the window between the two is held
	// rather than dropped. A detached run outlives its node, so that window
	// is not hypothetical. And INSIDE the recovery's gate, which the pass
	// closes before it attaches: a delivery from here is refused its claim,
	// and handed back, until the pass is done, so none is claimed and
	// resumed while the pass reads the runs, which on a seat already held
	// (an apply bringing the runtime up) this node's admission alone did not
	// keep from happening (internal/sandbox's recoverygate.go).
	attach := func(ctx context.Context) error {
		if err := e.backends.Queue.Subscribe(ctx, control, group,
			func(ctx context.Context, ev *events.Event) queue.Result {
				return controlOutcome(ctx, rt.coordinator.OnEvent(ctx, ev))
			}); err != nil {
			return fmt.Errorf("attaching the sandbox control topic: %w", err)
		}
		// ATTACHED FROM HERE, whatever recovery says: a release has to
		// detach what this call attached, and a refused seat is released.
		e.markSandboxSeat(handle, true)
		return nil
	}
	if err := rt.coordinator.RecoverSeat(ctx, handle, owner, epoch, e.inboxOf(seat.ID), attach); err != nil {
		return err
	}
	// A SEAT TAKEN WHILE A PERSON'S STOP IS ON IT ends the runs its last
	// holder left running, before its mailbox opens: the stop was the
	// seat's, not that holder's, and a run recovered here would otherwise
	// go on to its end — the one thing the person asked not to happen. Its
	// recovery opened no session for them ([Engine.reopenAgentRun]), so its
	// box has had none of the seat's tools back in between.
	if p, paused, _ := e.pauseOf(seat.ID); paused {
		e.stopSeatRuns(ctx, seat.ID, p)
	}
	// EVERY RUNNING JOB'S SESSION IS OPEN AGAIN HERE NOW ([Engine.reopenAgentRun]),
	// so the bridge may say a job it holds no session for is over — and not
	// before, or a box calling in the moment between the seat's lease and
	// its recovery would be told its live run had ended. A run the stop
	// above ended, or could not end yet, is one of those: over, as far as
	// the seat's tools are concerned.
	e.bridge.SeatReady(seat.ID)
	return nil
}

// controlOutcome is what a seat's control-topic delivery leaves with the broker,
// given what the coordinator made of it under ctx, the delivery's context.
//
// A refusal is handed back by the condition it names ([sandbox.TurnRefused]),
// never by its error: one that clears by waiting is ACKED, the coordinator
// keeping the completion and the waiter announcing the run again; a draining
// node and a seat let go of DEFER. A refusal marked [sandbox.ErrUnannounced]
// is NAKED, never acked. Nil (a run settled elsewhere, a renewal) is ACKED.
// Everything else NAKs, as the node's ([queue.NakNode]) wherever [nodesFailure]
// or a routing failure says it is not the message's. The package doc's
// sections from "A refused completion is handed back by the condition it
// names" to "Everything else on the control topic NAKs" hold the why.
func controlOutcome(ctx context.Context, err error) queue.Result {
	if err == nil {
		return queue.Ack()
	}
	var refused *sandbox.TurnRefused
	waits := errors.As(err, &refused) && refused.Condition != ""
	switch {
	case waits && refused.Condition == sandbox.Draining:
		return queue.Defer(queue.DeferDraining, err.Error())
	case waits && refused.Condition == sandbox.SeatLost:
		return queue.Defer(queue.DeferSeatMoved, err.Error())
	case waits && !storeUnheard(err):
		return queue.Ack()
	case nodesFailure(ctx, err), errors.Is(err, sandbox.ErrResumeElsewhere),
		errors.Is(err, sandbox.ErrSeatNotHeld), errors.Is(err, sandbox.ErrSeatRecovering):
		return queue.NakNode(err)
	}
	return queue.Nak(err)
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
// recovered no run: its completions would go to a topic nothing here consumes
// and a run a peer left would be polled by nobody.
//
// UNDER EACH SEAT'S OWN LOCK ([seat.Host.WithHeldSeat]), so a seat claimed or
// handed on mid-walk is prepared exactly once or not at all. A SEAT THAT CANNOT
// BE PREPARED IS HANDED BACK voluntarily ([seat.ReasonUnprepared]), so its next
// acquisition runs the whole preparation again.
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
// LIVE-ONLY, and deliberately: placement moves seats on every rebalance, so a
// durable row per claim would fill the audit log with scheduling, not company,
// facts; hence neither type is in [events.Category]'s map.
//
// THE SEAT'S AGENT ID under `agent_id`, as every other seat-level event carries
// it and the projection keys a seat by it (ADR-0013): a handle there would put a
// seat's arrival and its first turn under two identities. Neither a handle nor
// a name can stand in for the id (a name is prose two seats may share); both
// ride along for a reader.
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
	// THE INBOX'S ATTACHMENT, which the node's detach has ended: a hold asked
	// from here names none, and the queue refuses it (seatinbox.go).
	e.detachInbox(seat.ID)
	// FIRST, while the queue is reachable and before the teardown: a seat gone
	// with no event stays "working" on every dashboard.
	//
	// BESIDE THE TEARDOWN rather than ahead of it: a stream that will not
	// acknowledge holds the event for [lifecyclePublishBudget], which the
	// teardown has no reason to wait out. JOINED BEFORE THIS RETURNS, which is
	// before the lease is given back, so a peer taking over announces
	// `agent_spawned` after this node's `agent_terminated`, never before, and
	// the live projection keyed on the role never shows a running seat terminated.
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
	// The seat's parks die with it too: a budget park's alarm and a
	// provider park's probe would only fire into a seat a peer now serves.
	// The detach that released the inbox dropped their holds and ended the
	// attachment each was taken for, so a park a turn asks for after it is
	// refused and records nothing, and this drops only the records
	// ([seatParks.forget]).
	e.forgetBudgetPark(seat.ID)
	e.forgetProviderPark(ctx, seat.ID)
	// AND THE RECORDS OF ITS PAUSE HOLDS, a person's and the no-model one,
	// for the same reason: the detach dropped the holds, and a record left
	// names the attachment that ended, which a later lift lifts nothing on
	// and reports lifted.
	e.forgetPauseHold(seat.ID)
	e.forgetModelHold(seat.ID)
	// AND WHAT IT READ OF THE SEAT'S HELD CONVERSATIONS: the next holder reads its own.
	e.conversations.forget(seat.ID)
	// AND THE TOOL SESSIONS OF ITS EXECUTOR RUNS, which the seat's next
	// holder serves from now on: left open here they would pin the seat's
	// surface in a process that no longer speaks for it, and every box
	// would go on talking to it — see [mcpbridge.Bridge.ReleaseSeat]. It
	// waits for no call still running on them, which is answered that it is
	// still running and runs on here, so the seat's lease is given back
	// without waiting out a call.
	e.bridge.ReleaseSeat(seat.ID)
	// AND ITS RUNS' CONNECTIONS NOT YET ON THEIR ROWS, before the lease goes:
	// the next holder collects them ([connectMarks.release]).
	e.connectMarks.release(ctx, handle, log)

	// AND THE SEAT'S WORKING INDICATORS: on a chat surface a left state is a
	// live re-assertion. A turn suspended into a detached executor leaves its
	// indicator up and resumes on whichever node holds the seat, so a node that
	// handed the seat on would keep saying "is thinking…" until the process died.
	// The clear goes out even on a cancelled drain, and a dead chat instance
	// costs seconds, not a client timeout per seat: the driver detaches and
	// bounds each request itself ([notify.StatusDriver.ClearFor]).
	e.Status().ClearFor(ctx, handle)
	e.waitsTold.forgetSeat(handle)

	// REFLECTION FIRST, so nothing new writes the seat's memory here after
	// the flush below.
	e.detachSeatReflection(ctx, seat)

	// A LAST TURN, then forget the seat ([releaseSeatMemory]). The holding turns
	// to releasing first, so the sync cycle stops publishing it and the flush
	// alone carries it; a seat whose hydration failed has no holding and flushes
	// nothing. The flush sends, from the cycles' marks and before the successor
	// hydrates, what the seat wrote or changed here, then, while the bound has
	// time, the rows this node held when it took the seat ([flushSeatMemory]).
	// It has [memoryFlushTimeout]; what it does not reach is lost as a crash
	// loses it.
	// Forgetting drops the marks and boundary, so the seat's next holding here
	// takes a new boundary and carries back everything this node holds for it, as
	// a restart does, which restores rows onto a changelog an operator recreated.
	if e.memory != nil {
		releaseSeatMemory(ctx, e.memory, handle)
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

// SeatHeldBySandbox reports whether detached executors hold every slot of a
// seat's cap, so it starts no new turn until one settles.
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

// executorRun is what one coding box is handed to reach a model: the model, the
// credential files the backend seeds, and the whole run environment.
//
// THREE things reach the box, differently on purpose: the MODEL, as the entry's
// runs on this host name it; the ENTRY'S ENVIRONMENT (its env and sign-in),
// beneath the seat's; and the credential FILES, as a host-path map the backend
// seeds and writes back, since a login's refresh token rotates in the box and
// the engine host's copy must follow it.
type executorRun struct {
	model string
	// effort is the entry's reasoning effort, passed beside the model.
	effort string
	// webDenied is the entry's `web_tools: denied`, which its executor honours
	// as its runs on the engine host do ([sandbox.RunRequest.WebDenied]).
	webDenied bool
	env       map[string]string

	// logins is, by its path under the box's home, each login file the box is
	// seeded with: its engine-host source and the rule it is seeded and written
	// back by, kept together so neither is ever handed without the other
	// ([executorRun.files], [executorRun.fileRules]).
	logins map[string]boxLogin

	// homePaths is the variables naming the integrations' box files, each to
	// its path under the box's home, which the launch joins onto the box's
	// own beneath env ([sandbox.LaunchRequest.HomePaths]). Never a sign-in.
	homePaths map[string]string

	// withheld is the seat's integration credentials kept out of env because
	// the harness signs in by their names, which the launch says.
	withheld []string
}

// newExecutorRun is the [executorRun] of a box run on agent, the executor-chain
// member the launch picked ([agentLauncher.member]), over seat, the environment
// the seat's config assembled ([Engine.runEnvironment]), with the integrations'
// box settings beneath everything and their credentials over it.
//
// THE PICKED MEMBER, never a second resolution of the chain: the runner IS
// that member's harness, so a launch that fell back past the head gets the
// fallback's model, env and login.
func newExecutorRun(agent *harness.Provider, seat runEnv) executorRun {
	// THE MODEL IS HANDED IN THE HARNESS'S OWN GRAMMAR (OpenCode's
	// `<provider>/<model>`, Claude Code's alias), as every other phase's run on
	// the entry gets it. No provider of the engine's declaring: a declared one
	// would shadow the vendor or base URL the entry's own calls reach.
	//
	// THE SEAT'S OWN VENDOR CREDENTIALS IN ITS BOX ARE A TOOL'S, NEVER A
	// SIGN-IN ([config.Company.SeatCredentialVariables]): they win a name over the
	// entry's but never decide its env or sign-in names, since a tool token
	// named like a sign-in would strip the entry's ([entryEnv]).
	own := maps.Clone(seat.env)
	for _, name := range seat.tools {
		delete(own, name)
	}
	entry := entryEnv(agent, own)
	// THE INTEGRATIONS' BOX DEFAULTS LOWEST OF ALL, beneath the entry's env
	// too, and out of every sign-in judgment: a cloud CLI's region or token
	// file must never override, strip or stand in for the harness's sign-in.
	// AND THE SEAT'S OWN INTEGRATION CREDENTIALS OVER EVERYTHING, out of the
	// sign-in too, and withheld under a name the harness signs in by, where
	// the agent's tool token would become its model's credential.
	credentials := integrationsOwn(agent, seat.credentials)
	env := underlay(underlay(maps.Clone(seat.env), entry), integrationsOwn(agent, seat.defaults))
	maps.Copy(env, credentials)
	profile := agent.Profile()
	run := executorRun{
		model:     agent.ModelArgument(),
		effort:    agent.Effort(),
		webDenied: profile.EffectiveWebTools() == harnessprofile.WebToolsDenied,
		env:       env,
		homePaths: integrationsOwn(agent, seat.homePaths),
	}
	rules := agent.SandboxCredentialRules()
	for rel, source := range agent.SandboxCredentials() {
		run.addLogin(rel, source, rules[rel])
	}
	for _, name := range slices.Sorted(maps.Keys(seat.credentials)) {
		if _, kept := credentials[name]; !kept {
			run.withheld = append(run.withheld, name)
		}
	}
	// A LOGIN THE HARNESS KEEPS IN ITS OWN DATABASE travels as the document its
	// export printed, mapped where the runner imports it from — the runner's
	// path, not the profile's, because the runner is what reads it
	// ([executor.Paths.CredentialDocument]).
	if doc := agent.SandboxCredentialDocument(); doc != "" {
		run.addLogin(executor.CredentialDocument, doc, agent.SandboxCredentialDocumentRule())
	}
	return run
}

// boxLogin is one login file of an [executorRun]: its engine-host source and
// its rule.
type boxLogin struct {
	source string
	rule   harnessprofile.FileRule
}

// addLogin hands the run's box the login file at rel under its home, read
// from source and seeded and written back by rule.
func (r *executorRun) addLogin(rel, source string, rule harnessprofile.FileRule) {
	if r.logins == nil {
		r.logins = map[string]boxLogin{}
	}
	r.logins[rel] = boxLogin{source: source, rule: rule}
}

// files is each login file's engine-host source, by its path under the box's
// home ([sandbox.Spec.CredentialFiles]); nil for none.
func (r executorRun) files() map[string]string {
	if len(r.logins) == 0 {
		return nil
	}
	out := make(map[string]string, len(r.logins))
	for rel, login := range r.logins {
		out[rel] = login.source
	}
	return out
}

// fileRules is each login file's rule, by the same path
// ([sandbox.Spec.CredentialRules]); nil for none.
func (r executorRun) fileRules() map[string]harnessprofile.FileRule {
	if len(r.logins) == 0 {
		return nil
	}
	out := make(map[string]harnessprofile.FileRule, len(r.logins))
	for rel, login := range r.logins {
		out[rel] = login.rule
	}
	return out
}

// integrationsOwn is what of vars, variables an integration puts in a box, the
// box is handed: every one but a name agent's harness signs in by, since a
// cloud toggle or a token under it would switch or replace the sign-in.
func integrationsOwn(agent *harness.Provider, vars map[string]string) map[string]string {
	out := make(map[string]string, len(vars))
	for name, value := range vars {
		if !slices.Contains(agent.CredentialEnvNames(), name) {
			out[name] = value
		}
	}
	return out
}

// launchSecrets is every value a launch's run environment hands its box out of
// a credential position, for the scrub of its session
// ([sandbox.LaunchRequest.Secrets]): what its references resolved to and its
// setup files that are one reference whole (runEnv's secrets), and the value of
// every variable of env — the run environment the box is handed, the entry's
// beneath the seat's — whose name says it carries a credential
// ([harnessprofile.IsCredentialName], the one predicate the backend asks of a
// name) or that signs the entry's harness in, written or referenced alike.
//
// BY WHERE IT CAME FROM AND WHAT IT IS NAMED, never the whole environment: the
// seat's handle and email, a base URL, a protocol's name are no secrets, and
// scrubbed by value each was lost from every kept transcript that named it.
func launchSecrets(agent *harness.Provider, run runEnv, env map[string]string) []string {
	secrets := slices.Clone(run.secrets)
	signIn := agent.CredentialEnvNames()
	for _, key := range slices.Sorted(maps.Keys(env)) {
		if harnessprofile.IsCredentialName(key) || slices.Contains(signIn, key) {
			secrets = append(secrets, env[key])
		}
	}
	return secrets
}

// referencedValues is what every reference in value resolved to under env —
// what the operator kept out of the document, a credential position whatever
// the variable is named — each non-empty value once.
func referencedValues(env *config.Resolver, value string) []string {
	var out []string
	for _, name := range envref.Names(value) {
		if v := env.Lookup(name); strings.TrimSpace(v) != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// entryEnv is what an entry contributes to a run's environment, beneath what
// the seat brought: its env and sign-in ([harness.Provider.SandboxEnv]),
// UNDERLAID BY NAME so what the seat names in role.sandbox.env or a step wins.
//
// ONE UNIT FOR THE SIGN-IN OF A HARNESS THAT HAS SEVERAL: when the profile lists
// sign-ins beyond token and key ([harnessprofile.Profile.SignInEnv]) and the
// seat's env already signs in ([harness.Provider.CredentialEnvNames]), the entry
// adds none of its own ([entrySignInName]). Such a harness ranks sign-ins by
// name (ANTHROPIC_API_KEY outranks a plan token), so underlaying by name let the
// company's metered key displace a seat's plan token.
// The rest of the entry's env (base URL, region, proxy) stays beneath: it says
// where requests go, not whom as.
func entryEnv(agent *harness.Provider, seatEnv map[string]string) map[string]string {
	env := agent.SandboxEnv()
	if len(agent.Profile().SignInEnv) == 0 || !signsIn(seatEnv, agent.CredentialEnvNames()) {
		return env
	}
	for name := range env {
		if entrySignInName(agent, name) {
			delete(env, name)
		}
	}
	return env
}

// entrySignInName reports whether name is part of an entry's sign-in, which a
// seat signing the harness in itself replaces as one unit ([entryEnv]): a name the
// harness signs in by, its profile's token and key variables (which an override may
// have renamed) and its sign_in_env, such as Claude Code's gateway bearer token
// and its cloud toggles ([harness.Provider.CredentialEnvNames]); and any
// credential-named variable
// ([harnessprofile.IsCredentialName]), such as a cloud sign-in's own key (Bedrock's
// AWS_BEARER_TOKEN_BEDROCK or the AWS key pair), which with the entry's toggle
// gone no request of the run reads, and which would sit in a box whose agent
// runs with every permission as a company credential the seat's run never
// uses.
func entrySignInName(agent *harness.Provider, name string) bool {
	return slices.Contains(agent.CredentialEnvNames(), name) || harnessprofile.IsCredentialName(name)
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

// underlay adds defaults to env WITHOUT overwriting what is already there.
//
// The direction is the decision: an operator who named a variable in
// role.sandbox.env meant that value, and the engine silently replacing it
// with a resolved subscription token would override a deliberate choice —
// including the deliberate choice to point one seat's executor at a
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

// requestRefused decides whether a collected run is carried on because its
// provider refused the request it made (sandbox.RunEnding.Refused) IN A SESSION
// IT CONTINUED: that session is retired at the collection, so the round is
// relaunched once in a session of its own, which holds nothing of what was
// refused.
//
// ALL OF IT MUST HOLD: a completion; a refused ending; a job that is not itself
// such a relaunch (once per round); and a session its box said it continued. A
// job refused in a session it started has nothing a fresh one would leave
// behind, and is collected ([refusedSaid]).
func requestRefused(ctx context.Context, in resumeInput) *runner.Refused {
	run := in.Run
	if in.Asked != nil || in.Lost != nil || !in.Ended.Refused ||
		run.Launch.FollowUp == sandbox.FollowUpRequestRefused ||
		run.Launch.SessionStart != sandbox.SessionStartContinued {
		return nil
	}
	log.InfoContext(ctx, "run_relaunched_fresh_session", "turn_id", run.TurnID, "seat", run.AgentHandle,
		"provider_key", run.Launch.ProviderKey, "launch_id", run.LaunchID, "session", run.SessionRan(),
		"detail", "the provider refused the request the run made in a session it continued; the "+
			"session is retired and the round is carried on once in a fresh one")
	return &runner.Refused{Said: refusedMessage(in.Ended), BoxKept: run.SandboxID != ""}
}

// refusedSaid is the provider's message where the collected run ended refused
// and its round is NOT carried on ([requestRefused]): what ends the turn.
func refusedSaid(in resumeInput) string {
	if in.Asked != nil || in.Lost != nil || !in.Ended.Refused || in.Refused != nil {
		return ""
	}
	return refusedMessage(in.Ended)
}

// refusedMessage is what the provider said of a refused request, as a
// diagnostic holds it ([events.Diagnostic]): the run's error, else its text,
// else that it said nothing.
func refusedMessage(ended sandbox.RunEnding) string {
	said := strings.TrimSpace(ended.Error)
	if said == "" {
		said = strings.TrimSpace(ended.Text)
	}
	if said == "" {
		return "the provider gave no message"
	}
	return events.Diagnostic(said)
}

// signInReplaced decides whether a collected run is carried on because a
// credential was replaced while it ran (M8 of the Models design), answering the
// carried round or nil.
//
// A running job keeps its launch credential; if the vendor revoked it the job
// ends refused while the entry already signs in anew. Every LAUNCH reads the
// current entry, so the round is relaunched once, continuing its session.
// ALL OF IT MUST HOLD: a completion; a job not itself such a relaunch (once per
// round, so a failing credential fails the turn); a sign-in recorded at launch;
// an auth-failure ending; and a sign-in that now differs.
func (e *Engine) signInReplaced(ctx context.Context, c *Company, in resumeInput) *runner.Rotated {
	run := in.Run
	if in.Asked != nil || in.Lost != nil || run.Launch.FollowUp == sandbox.FollowUpSignInReplaced ||
		run.Launch.SignInID == "" || c == nil || c.Models == nil {
		return nil
	}
	provider, ok := c.Models.Provider(run.Launch.ProviderKey)
	if !ok {
		return nil
	}
	agent, isHarness := provider.(*harness.Provider)
	if !isHarness {
		return nil
	}
	kind, classified := agent.EndedKind(harness.RunEnd{Answer: in.Ended.Text, Error: in.Ended.Error,
		Failed: !in.Ended.Success, Event: in.Ended.Event, At: in.Ended.At})
	if !classified || kind != llm.KindAuth {
		return nil
	}
	current := agent.SignInID()
	if current == "" || string(current) == run.Launch.SignInID {
		return nil
	}
	log.InfoContext(ctx, "run_relaunched_signed_in", "turn_id", run.TurnID, "seat", run.AgentHandle,
		"provider_key", run.Launch.ProviderKey, "launch_id", run.LaunchID,
		"detail", "the run was refused on a sign-in that was replaced while it ran; its round "+
			"is carried on once, in its box, on the entry's new sign-in")
	// THE SESSION THE JOB'S BOX RAN, never the one its launch chose: a box
	// that could not take that one up ran one of its own, and the round is
	// carried on in that — and in none in its box where the box's word never
	// reached the job's record ([sandbox.PendingRun.SessionRan]).
	return &runner.Rotated{SessionID: run.SessionRan(), BoxKept: run.SandboxID != ""}
}
