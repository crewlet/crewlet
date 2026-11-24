// Command notehelper hands a person's notes to a harness that has neither a
// hook route nor a plugin to take them: a small static program the engine
// writes into the box beside the harness at each launch, and the run's script
// starts before the harness and stops with it.
//
//	notehelper -adapter codex-app-server -socket $RUN/codex.sock \
//	    -thread-file $RUN/thread.id -notes-env CREWLET_NOTES_URL \
//	    -step-cap 200 -cap-marker $RUN/capped \
//	    -skills-env CREWLET_SKILLS_URL -skills-dir $CODEX_HOME/skills \
//	    -skill-digest crewlet:deploy=<sha256 hex>
//	notehelper -adapter codex-app-server -socket $RUN/codex.sock \
//	    -thread-file $RUN/thread.id -interrupt-once
//
// Beside the notes it bounds a turn and reports skill loads, for a harness
// that does neither itself; each part runs only where its flags ask.
//
// or, for a harness whose hook runs it once per firing (once.go):
//
//	notehelper -adapter muse-hook -config $RUN/muse-take.json -slot 1
//	notehelper -adapter grok-hook -config $RUN/grok-take.json
//
// # What it does
//
// It polls the bridge's notes route OUTBOUND, as OpenCode's plugin does
// (internal/sandbox/executor's opencode_steer.js; internal/api/mcpbridge's
// SteerPattern), and hands each note it is served, oldest first, to the
// harness through the ADAPTER -adapter names. The loop, its pauses and its
// shutdown are the helper's and name no harness ([harness]); an adapter is
// one harness's way in. A later harness with neither route adds an adapter,
// never a second program.
//
// # It confirms nothing
//
// The helper never posts the route's read. The engine confirms a note from
// the harness's own record of the run (internal/sandbox/executor's
// recordConfirmer), never from the helper's word, so there is one confirm
// path. A note the harness refused is logged and handed over again on a later
// poll (the bridge serves it again until the run's record shows it); one the
// harness took is not asked for again, since the cursor passes it.
//
// # The routes are secrets
//
// The notes route and the skill route carry the run's bridge token in their
// paths (mcpbridge.SteerURL, mcpbridge.SkillURL), so each is read from the
// environment variable -notes-env or -skills-env names, never from the
// command line, which every process in the box can read, and never printed:
// an error names "the notes route" or "the skill route", never its address
// ([route.scrub]).
//
// # When it polls
//
// Only while the harness takes a note now ([harness.ready]: for Codex, a turn
// in flight), as OpenCode's plugin polls only while its session's execution
// runs: a note served after the run's last turn waits on the run's record for
// its review, never in the helper. The pauses are the plugin's: a failed poll
// or refused note waits a second, doubling to thirty, reset by the next
// success, and a poll that handed nothing over starts the next no sooner than
// a second after it began, so no answer can drive the helper into a loop of
// requests.
//
// # The step cap
//
// -step-cap N ends a turn at its Nth model response, counted per turn, the
// unit of Claude Code's --max-turns, which a context compaction's request is
// not: the adapter counts the responses ([events]), and at N the helper
// writes -cap-marker, by a rename, and then interrupts the turn ([stepCap]).
// The marker says the cap fired, never that a turn was cut short: a turn
// whose Nth response was its final answer completes, and the run reads its
// cap only where its record of the turn says the interrupt aborted it. An
// interrupt that fails is asked again at the turn's next response.
// -interrupt-once stands in for a helper that ended before its turn did: it
// joins, interrupts the turn in flight and exits, saying whether it cut one
// short, and the run's guard stops the harness where it did not, so killing
// the helper never leaves a turn without its bound.
//
// # Skill loads
//
// With -skills-env, -skills-dir and a -skill-digest per skill the helper
// reports each load of one of the run's skills to the skill route, as
// OpenCode's plugin does ([reporter]): a load is what the adapter can prove the
// model read whole as the runner installed it (for Codex, a command whose
// output's SHA-256 is the one -skill-digest names for that SKILL.md, on the
// command line the runner fixed before the model ran, [appServer.completed]),
// and nothing else is one, so the bridge's guard fails closed on everything
// the helper cannot prove.
//
// # Once per firing
//
// A harness whose hooks run a program (Muse Code's, Grok Build's) runs the
// helper once per firing instead (once.go): it takes the run's notes over the
// bridge's steer_take, as a hook that calls an MCP tool would, hands the
// answer over through the adapter, and exits 0 whatever happened, so a firing
// never stops the harness; a take the harness never got is told again. A run
// that continues a Codex session runs it first as codex-renew, which rewrites
// the standing instructions its rollout replays to the run's own
// (codexrenew.go).
//
// # Checkouts below the working directory
//
// A harness started in a run's working directory W does not load what a
// session started in a repository checked out below it would, so the helper
// hands each checkout directly under W (a directory or a link to one, holding
// .git, its name with no leading dot or control character) its instructions
// and skills once its clone has finished (checkoutclone.go; checkout.go), and
// once its skills fit what is left of the firing's bound, else at a later
// firing. Its logic names no harness; a runner gives it a spec:
//   - `instructions`: slots, in order, within `limit`, the harness's own
//     bound in bytes on what a session started in the checkout reads of them
//     (Codex: project_doc_max_bytes), omitted for a harness with none, a file
//     the limit cuts spending all of it. A list of file names relative to a
//     checkout's root hands its first regular file (Codex:
//     [["AGENTS.override.md","AGENTS.md"]]). A tree, {"tree": <dir>,
//     "suffix": <s>, "scoped": {"keys": [...], "whole": [...]}}, hands each
//     file below dir whose name ends in s in any case, whole, apart by a blank
//     line, walked as Copilot 1.0.114 walks .claude/rules (measured): depth
//     first, a directory's entries by name in byte order, dot entries
//     skipped, a file link followed while it stays in the checkout, a
//     directory link never. `scoped` leaves out a file Copilot loads only
//     lazily, on a path its frontmatter names, read as its line reader reads
//     it: line 1, after an optional BOM, is `---`, a `---` line closes it,
//     and between them the first of keys with a case-exact `<key>:` line,
//     indentation trimmed, has a value that, split on `,` or read as
//     `[a, b]`, quotes trimmed, holds an entry outside whole. A tree past 8
//     directories deep, 1,024 entries or 32 MiB is left out whole, said:
//     Copilot walks 9 levels (measured), so a deeper tree is the gap;
//   - `skillDirs`: the checkout's skill directories, an earlier one winning a
//     name (Codex: [".codex/skills",".agents/skills"]), each skill a
//     directory directly in one. {"dir": <d>, "recursive": true} walks d as
//     Pi 1.6.0 does (measured): a directory holding SKILL.md is a skill and
//     ends the descent, dot directories and node_modules are skipped, and a
//     skill reached twice, by real path, is copied once, and of two of one
//     name, the first, the other left out, said; past 8 directories
//     deep or 1,024 entries it is left out whole, said. Pi also follows a
//     link out of the checkout, lists a loose root `.md` skill, and honours
//     .gitignore, .ignore and .fdignore; the walk does none of these;
//   - `skills`: the run's skills directory each skill is copied into whole,
//     links followed while they stay in the checkout, within one firing's
//     bound, as `<tag>:<skill>` (at most 64 characters) with its
//     frontmatter's name rewritten to that, never over one already there;
//     `staging`: the directory on that filesystem a copy is built in, which
//     the harness must not load; `copied`: the file each copy is listed in
//     before it is written, for the runner's next launch to remove with the
//     staging directory (checkoutcopy.go); `reserved`: the tags no checkout
//     takes (the seats' namespace). A tag is the checkout's name lower-cased,
//     a byte outside [a-z0-9] a hyphen, `-2`, `-3`... where taken.
//
// checkout-launch runs once, in W, before the harness: its -config is the spec
// with `state` (the run's checkout state, remade per run), `agents` (the file
// it replaces with a section per checkout, "for `<name>` with root <path>"
// and its instructions, which the runner places where its harness reads them)
// and `first` (the repositories the run pushes to, handed first; the rest by
// name). It writes state/nested.json, the -config of the harness's nested
// adapter, which the run's post-tool hook runs after each of the main loop's
// tool calls: it claims each checkout that appeared since, once, under state's
// lock, copies its skills, and writes in its harness's shape what the harness
// does not load itself: codex-nested the instructions and the skills' listing
// (name, description, the copy's SKILL.md) as its hook's context; pi-nested
// the same with each file in Pi's `<project_instructions path=...>`, on
// stdout for the run's extension to add to the tool's result (Pi lists no
// skill it did not start with and loads instructions only as it starts,
// measured on 1.6.0). A harness taking checkouts adds a
// nested adapter, never a second core; its runner holds the home file and the
// copies (internal/sandbox/executor's checkoutHome).
//
// # A continued Codex session's standing instructions
//
// Codex records the client's developer_instructions once, in the developer
// message its first turn's context opens with, and a resume replays that
// message from the rollout: the resumed thread's configuration never reaches
// the model again, whether config.toml's or thread/resume's
// developerInstructions (both measured on 0.171.0 through the app-server; core
// restores the rollout's reference context and records only world-state
// changes, and the client's instructions are none). turn/start sets only a
// collaboration mode's instructions, another section, and `exec --remote`,
// which drives the run, resumes with the thread id alone. So
// codex-renew, run by the session gate before the app-server starts, rewrites
// each item the rollout records them in (content kind
// generic.developer_instructions, at any depth: a compaction's replacement
// history too) to the run's text, every other line byte for byte, its line
// count kept, by a rename over the file. A rollout recording none, which
// resumed would hand the model no standing instructions at all, is refused,
// and the run starts a session of its own, as Kimi Code's, Grok Build's and
// Hermes's runners renew theirs or start fresh.
//
// # Exit
//
// 0 on SIGTERM or SIGINT, and once the harness's connection closes (the run
// is over); 1 when the harness cannot be reached; 2 for a command line or
// environment it cannot run with; checkout-launch exits 1 where it could not
// write the instructions, and codex-renew where it could not renew them. A
// route that refuses the helper (401 or 404: its token no longer resolves, or
// the run has no such route) ends only what that route is for, logged: the
// notes route its polls, the skill route its reports, the cap and the other
// route going on. -interrupt-once exits 0 once
// it interrupted the turn in flight, 3 where no turn was in flight to
// interrupt, and 1 where it cannot tell. Every error goes to stderr, which the
// run's script sends to the run's error stream.
//
// # Its builds
//
// The engine embeds a gzipped build per box arch
// (internal/sandbox/executor/notehelper/*.gz, notehelper_embed.go): committed
// generated files, as static/dashboard is, rebuilt by `make notehelper` at
// go.mod's toolchain with -trimpath, no VCS stamp and an empty build id, and
// gzipped by ./gzipbuild, so a rebuild is byte for byte the same; `make
// notehelper-check` and ci.yml's build job rebuild and diff them. It links the
// standard library and golang.org/x/net/websocket alone: the one module
// beyond std, kept because dropping it means hand-rolling RFC 6455, which
// #1818's ruling forbids. Its version is in the build's module info, so a bump
// of it changes the builds, and .github/workflows/dependabot-notehelper.yml
// rebuilds them on Dependabot's pull request.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// The exit statuses (the package doc's "Exit").
const (
	exitDone  = 0
	exitError = 1
	exitUsage = 2
	// exitNothing is -interrupt-once finding no turn in flight: nothing was
	// cut short, so the next turn would start with no helper bounding it.
	exitNothing = 3
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	code, once := runOnce(ctx, os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr)
	if !once {
		code = run(ctx, os.Args[1:], os.Getenv, os.Stderr, defaultPacing)
	}
	stop()
	os.Exit(code)
}

// adapters is every harness the helper can hand a note to, by -adapter.
var adapters = map[string]func(ctx context.Context, f flags, p pacing, ev events) (harness, error){
	"codex-app-server": openAppServer,
}

// flags is the command line.
type flags struct {
	adapter, socket, threadFile, notesEnv string
	stepCap                               int
	capMarker                             string
	interruptOnce                         bool
	skillsEnv, skillsDir                  string
	digests                               digestFlag
	// skills is the run's skills as -skills-dir and -skill-digest name them,
	// set before the adapter opens: no flag of its own.
	skills skillSet

	// A one-shot adapter's (once.go): the take config's file, whether the
	// hook reports skill loads and whether it takes no notes, and the hook's
	// place among the run's hooks, which keeps each hook's command its own.
	config                string
	skillLoads, loadsOnly bool
	slot                  int
	// codex-renew's: the rollout of the session the run continues.
	rollout string
}

// run is the whole program: the exit status for args, the environment
// getenv reads, and stderr; ctx ends with a signal.
func run(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer, p pacing) int {
	logger := log.New(stderr, "notehelper: ", log.LstdFlags|log.LUTC)
	var f flags
	set := flag.NewFlagSet("notehelper", flag.ContinueOnError)
	set.SetOutput(stderr)
	set.StringVar(&f.adapter, "adapter", "", "the harness to hand notes to: codex-app-server")
	set.StringVar(&f.socket, "socket", "", "the harness's unix socket")
	set.StringVar(&f.threadFile, "thread-file", "", "the file the run's script writes the harness's thread id to")
	set.StringVar(&f.notesEnv, "notes-env", "", "the environment variable holding the bridge's notes route")
	set.IntVar(&f.stepCap, "step-cap", 0, "end a turn at this many model responses (0: no cap)")
	set.StringVar(&f.capMarker, "cap-marker", "", "the file written, before the interrupt, when the step cap ends a turn")
	set.BoolVar(&f.interruptOnce, "interrupt-once", false, "interrupt the turn in flight, if any, and exit")
	set.StringVar(&f.skillsEnv, "skills-env", "", "the environment variable holding the bridge's skill route")
	set.StringVar(&f.skillsDir, "skills-dir", "", "the directory of the run's skills, one directory per skill holding its SKILL.md")
	f.digests = digestFlag{}
	set.Var(f.digests, "skill-digest", "name=sha256-hex: a skill's SKILL.md as the runner wrote it, once per skill")
	if err := set.Parse(args); err != nil {
		return exitUsage
	}
	open, ok := adapters[f.adapter]
	switch {
	case set.NArg() > 0:
		logger.Printf("unexpected arguments after the flags: %d", set.NArg())
		return exitUsage
	case !ok:
		logger.Printf("-adapter %q is not one the helper has; -help lists them", f.adapter)
		return exitUsage
	case f.interruptOnce:
		return interruptOnce(ctx, f, p, open, logger)
	case f.stepCap < 0:
		logger.Printf("-step-cap %d is below 0; 0 is no cap", f.stepCap)
		return exitUsage
	case f.capMarker != "" && f.stepCap == 0:
		logger.Print("-cap-marker is written only by a step cap: set -step-cap too")
		return exitUsage
	case (f.skillsEnv == "") != (f.skillsDir == "") || (f.skillsEnv == "") != (len(f.digests) == 0):
		logger.Print("-skills-env, -skills-dir and -skill-digest go together: the skill route, where the " +
			"skills are, and what the runner wrote there")
		return exitUsage
	case f.notesEnv == "" && f.stepCap == 0 && f.skillsEnv == "":
		logger.Print("nothing to do: set -notes-env, -step-cap or -skills-env")
		return exitUsage
	}
	var poll *poller
	var skills *reporter
	for _, want := range []struct {
		env, name string
		set       func(*route)
	}{
		{f.notesEnv, "the notes route", func(r *route) { poll = &poller{r} }},
		{f.skillsEnv, "the skill route", func(r *route) { skills = newReporter(r) }},
	} {
		if want.env == "" {
			continue
		}
		raw := getenv(want.env)
		if raw == "" {
			logger.Printf("the environment variable %s holds no route; set it to %s", want.env, want.name)
			return exitUsage
		}
		r, err := newRoute(raw, want.name)
		if err != nil {
			logger.Printf("%s: %v", want.env, err)
			return exitUsage
		}
		want.set(r)
	}
	if f.skillsDir != "" {
		f.skills = skillSet{dir: f.skillsDir, digests: f.digests}
	}
	var ev events
	var capped *stepCap
	if f.stepCap > 0 {
		capped = newStepCap(f.stepCap, f.capMarker)
		ev.step = capped.step
	}
	if skills != nil {
		ev.skill = skills.loaded
	}
	h, err := open(ctx, f, p, ev)
	switch {
	case ctx.Err() != nil:
		return exitDone
	case errors.Is(err, errUsage):
		logger.Print(err)
		return exitUsage
	case err != nil:
		logger.Print(err)
		return exitError
	}
	defer h.close()
	work, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	if capped != nil {
		wg.Go(func() { capped.run(work, h, logger) })
	}
	if skills != nil {
		wg.Go(func() { skills.run(work, logger) })
	}
	defer func() {
		stop()
		wg.Wait()
	}()
	if poll != nil {
		err = (&loop{poll: poll, h: h, log: logger, pace: p}).run(ctx)
		switch {
		case ctx.Err() != nil, errors.Is(err, errGone):
			return exitDone
		case !errors.Is(err, errRefused):
			logger.Print(err)
			return exitError
		}
		// THE NOTES ROUTE ALONE REFUSED THE HELPER: its polls end, and the cap
		// and the skill reports go on, since a helper that ended would leave
		// the turn unbounded (the run's guard would stop it). Said in the one
		// line that names the refusal, so a reader knows the polls are over.
		logger.Printf("%v: note polling has ended; the step cap and skill reports go on", err)
	}
	// The helper caps and reports until the run is over.
	select {
	case <-h.gone():
	case <-ctx.Done():
	}
	return exitDone
}

// interruptOnce is -interrupt-once: join, interrupt the turn in flight and
// exit, 0 where it interrupted one, [exitNothing] where none was in flight,
// 1 where that cannot be known. The run's script runs it once the
// long-running helper has ended before the turn did, and stops the harness
// on anything but 0: a turn not yet started when the helper ended would run
// with no cap at all.
func interruptOnce(ctx context.Context, f flags, p pacing,
	open func(context.Context, flags, pacing, events) (harness, error), logger *log.Logger) int {
	h, err := open(ctx, f, p, events{})
	switch {
	case errors.Is(err, errUsage):
		logger.Print(err)
		return exitUsage
	case err != nil:
		logger.Print(err)
		return exitError
	}
	defer h.close()
	err = h.interrupt(ctx, "")
	switch {
	case err == nil:
		return exitDone
	case errors.Is(err, errNotNow):
		logger.Print("no turn was in flight to interrupt")
		return exitNothing
	default:
		logger.Print(err)
		return exitError
	}
}

// errUsage marks an adapter's refusal of the command line.
var errUsage = errors.New("usage")

// usageError is a refusal of the command line, worded for a person.
func usageError(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{errUsage}, a...)...)
}
