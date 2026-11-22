// Command notehelper hands a person's notes to a harness that has neither a
// hook route nor a plugin to take them: a small static program the engine
// writes into the box beside the harness at each launch, and the run's script
// starts before the harness and stops with it.
//
//	notehelper -adapter codex-app-server -socket $RUN/codex.sock \
//	    -thread-file $RUN/thread.id -notes-env CREWLET_NOTES_URL \
//	    -step-cap 200 -cap-marker $RUN/capped \
//	    -skills-env CREWLET_SKILLS_URL -skills-dir $CODEX_HOME/skills
//	notehelper -adapter codex-app-server -socket $RUN/codex.sock \
//	    -thread-file $RUN/thread.id -interrupt-once
//
// Beside the notes it bounds a turn and reports skill loads, for a harness
// that does neither itself; each part runs only where its flags ask.
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
// -step-cap N ends a turn at its Nth model response, the unit of Claude
// Code's --max-turns, which a context compaction's request is not: the
// adapter counts the responses ([events]), and at N
// the helper writes -cap-marker, by a rename, and then interrupts the turn
// ([stepCap]), so the run's collection tells the helper's cap from any other
// interrupt by the marker alone. An interrupt that fails is asked again at
// the turn's next response. -interrupt-once stands in for a helper that ended
// before its turn did: it joins, interrupts the turn in flight and exits, so
// killing the helper never leaves a turn without its bound.
//
// # Skill loads
//
// With -skills-env and -skills-dir the helper reports each load of one of the
// run's skills to the skill route, as OpenCode's plugin does ([reporter]): a
// load is what the adapter can prove the model read whole (for Codex, a
// command whose output is the skill's SKILL.md byte for byte,
// [appServer.completed]), and nothing else is one, so the bridge's guard
// fails closed on everything the helper cannot prove.
//
// # Exit
//
// 0 on SIGTERM or SIGINT, and once the harness's connection closes (the run
// is over); 1 when the notes route refuses the helper (401 or 404: its token
// no longer resolves, or the run has no notes route) or the harness cannot be
// reached; 2 for a command line or environment it cannot run with.
// -interrupt-once exits 0 once no turn runs (interrupted, or none was) and 1
// where it cannot tell. Every error goes to stderr, which the run's script
// sends to the run's error stream.
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
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stderr, defaultPacing)
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
	case (f.skillsEnv == "") != (f.skillsDir == ""):
		logger.Print("-skills-env and -skills-dir go together: the skill route, and where the skills are")
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
	if poll == nil {
		// No notes: the helper caps and reports until the run is over.
		select {
		case <-h.gone():
		case <-ctx.Done():
		}
		return exitDone
	}
	err = (&loop{poll: poll, h: h, log: logger, pace: p}).run(ctx)
	switch {
	case ctx.Err() != nil, errors.Is(err, errGone):
		return exitDone
	default:
		logger.Print(err)
		return exitError
	}
}

// interruptOnce is -interrupt-once: join, interrupt the turn in flight and
// exit, 0 where no turn runs any more (interrupted, or none was), 1 where
// that cannot be known. The run's script runs it once the long-running
// helper has ended before the turn did, so the turn is never left without
// its cap.
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
	case err == nil, errors.Is(err, errNotNow):
		return exitDone
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
