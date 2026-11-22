// Command notehelper hands a person's notes to a harness that has neither a
// hook route nor a plugin to take them: a small static program the engine
// writes into the box beside the harness at each launch, and the run's script
// starts before the harness and stops with it.
//
//	notehelper -adapter codex-app-server -socket $RUN/codex.sock \
//	    -thread-file $RUN/thread.id -notes-env CREWLET_NOTES_URL
//
// or, for a harness whose hook runs it once per firing (once.go):
//
//	notehelper -adapter muse-hook -config $RUN/muse-take.json -slot 1
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
// # The route is a secret
//
// The notes route carries the run's bridge token in its path
// (mcpbridge.SteerURL), so it is read from the environment variable
// -notes-env names, never from the command line, which every process in the
// box can read, and it is never printed: an error names "the notes route",
// never its address ([scrub]).
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
// # Once per firing
//
// A harness whose hooks run a program (Muse Code's) runs the helper once per
// firing instead (once.go): it takes the run's notes over the bridge's
// steer_take, as a hook that calls an MCP tool would, hands the answer over
// through the adapter, and exits 0 whatever happened, so a firing never stops
// the harness; a take the harness never got is told again.
//
// # Exit
//
// 0 on SIGTERM or SIGINT, and once the harness's connection closes (the run
// is over); 1 when the route refuses the helper (401 or 404: its token no
// longer resolves, or the run has no notes route) or the harness cannot be
// reached; 2 for a command line or environment it cannot run with. Every
// error goes to stderr, which the run's script sends to the run's error
// stream.
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
	code, once := runOnce(ctx, os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr)
	if !once {
		code = run(ctx, os.Args[1:], os.Getenv, os.Stderr, defaultPacing)
	}
	stop()
	os.Exit(code)
}

// adapters is every harness the helper can hand a note to, by -adapter.
var adapters = map[string]func(ctx context.Context, f flags, p pacing) (harness, error){
	"codex-app-server": openAppServer,
}

// flags is the command line.
type flags struct {
	adapter, socket, threadFile, notesEnv string

	// A one-shot adapter's (once.go): the take config's file, whether the
	// hook reports skill loads and whether it takes no notes, and the hook's
	// place among the run's hooks, which keeps each hook's command its own.
	config                string
	skillLoads, loadsOnly bool
	slot                  int
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
	case f.notesEnv == "":
		logger.Print("-notes-env is required: the environment variable holding the notes route")
		return exitUsage
	}
	route := getenv(f.notesEnv)
	if route == "" {
		logger.Printf("the environment variable %s holds no notes route; set it to the run's notes route", f.notesEnv)
		return exitUsage
	}
	poll, err := newPoller(route)
	if err != nil {
		logger.Printf("%s: %v", f.notesEnv, err)
		return exitUsage
	}
	h, err := open(ctx, f, p)
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
	err = (&loop{poll: poll, h: h, log: logger, pace: p}).run(ctx)
	switch {
	case ctx.Err() != nil, errors.Is(err, errGone):
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
