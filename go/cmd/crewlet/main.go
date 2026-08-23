// Command crewlet is the engine binary.
//
// One binary is the whole product: it runs agents, serves the API and
// dashboard, and — in the default solo topology — embeds its own broker and
// database, so a company runs with no external services at all. What a node
// does is a config value (node.roles), not a different command.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/version"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "crewlet:", err)
		os.Exit(1)
	}
}

// run is main's testable body: it takes its arguments and streams rather
// than reaching for globals, so the CLI surface can be exercised in tests.
func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		usage(stderr)
		return errors.New("no command given")
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "crewlet %s", version.String())
		if rev := version.Revision(); rev != "" {
			fmt.Fprintf(stdout, " (%s)", rev)
		}
		fmt.Fprintln(stdout)
		return nil
	case "help", "--help", "-h":
		usage(stdout)
		return flag.ErrHelp
	case "run":
		return runEngine(rest, stderr)
	case "validate":
		return validateConfigs(rest, stdout, stderr)
	default:
		usage(stderr)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `crewlet %s — an engine for hierarchically organized AI agent companies

Usage:
  crewlet run [flags]         Run the engine (API, agents and workers by default)
  crewlet validate [flags]    Check both config tiers without starting anything
  crewlet version             Print the version
  crewlet help                Show this message

Config:
  -config   Tier A, this NODE: where its broker, store and API are (default %q)
  -company  Tier B, the COMPANY: its org, providers and integrations (default %q)
`, version.String(), defaultBootstrapPath, defaultCompanyPath)
}

// The two tiers are separate files because they answer to separate people. A
// node's broker address and store path are an operator's; the org chart and
// the model behind each seat are the company's.
//
// Tier B is read from a FILE here, which is not where it will finally live: a
// company config is versioned in the store and activated by epoch, and the
// engine is meant to read the active revision rather than a path. That needs
// the config plane — revisions, the activation pointer, per-node apply status
// — which is a separate piece of work. Until it lands, the file IS the active
// revision, and -company becomes the import path rather than the run path when
// it does.
const (
	defaultBootstrapPath = "crewlet.yaml"
	defaultCompanyPath   = "company.yaml"
)

// configFlags is the pair every config-reading command takes.
type configFlags struct {
	bootstrap *string
	company   *string
}

func addConfigFlags(fs *flag.FlagSet) configFlags {
	return configFlags{
		bootstrap: fs.String("config", defaultBootstrapPath,
			"Tier A config: this node's broker, store and API"),
		company: fs.String("company", defaultCompanyPath,
			"Tier B config: the company's org, providers and integrations"),
	}
}

// load reads both tiers, reporting EVERY problem it can rather than the first.
//
// Both, not just the first to fail: an operator fixing a broker URL only to be
// told about their org chart on the next boot has been made to pay twice for
// one edit. It is the same rule each tier's own validator follows internally.
func (c configFlags) load() (*config.Bootstrap, *config.Company, error) {
	// Environment-only resolution for Tier A, which is not a default but a
	// rule: Tier A carries the store's address and the keys that open it,
	// so a resolver reaching the secret store would have Tier A reading
	// from the store it is describing.
	boot, bootErr := config.LoadBootstrap(*c.bootstrap, config.EnvOnly())
	company, companyErr := config.LoadCompany(*c.company)
	if err := errors.Join(bootErr, companyErr); err != nil {
		return nil, nil, err
	}
	return boot, company, nil
}

func validateConfigs(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := addConfigFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	boot, company, err := cfg.load()
	if err != nil {
		return err
	}
	// Building the epoch is the rest of the check, and it reaches nothing:
	// no broker, no store, no provider is dialled. It is what catches the
	// problems a schema cannot — a seat whose llm names no configured
	// provider, a role reporting to a unit that does not exist.
	epoch, err := engine.NewCompany(company)
	if err != nil {
		return err
	}
	// Both slot types are read straight off the config: ParseBootstrap
	// decodes over DefaultBootstrap, so a file that names neither comes
	// back carrying the defaults rather than empty strings. A helper here
	// naming them again would be a second copy of the defaults, and the
	// two would eventually disagree about what an unset slot does.
	fmt.Fprintf(stdout, "%s: %d agent seats, %d LLM providers, stream %q, coordination %q\n",
		company.Name, len(epoch.Seats()), len(epoch.Models.Keys()),
		boot.Stream.Type, boot.Coordination.Type)
	return nil
}

func runEngine(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := addConfigFlags(fs)
	logLevel := fs.String("log-level", "info", "log level: debug, info, warn, error")
	logFormat := fs.String("log-format", "text", "log format: text or json")
	if err := fs.Parse(args); err != nil {
		return err
	}

	logging.Configure(logging.ParseLevel(*logLevel), logging.ParseFormat(*logFormat), stderr)
	log := logging.Get("cli")

	boot, company, err := cfg.load()
	if err != nil {
		return err
	}

	// The engine owns the process signals exclusively — one handler per
	// signal — because a graceful drain is the difference between a
	// restart that resumes cleanly and one that redelivers half-finished
	// turns. Nothing else in the process may install a handler.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("engine_starting", "version", version.String(), "company", company.Name)
	e, err := engine.New(ctx, engine.Options{Bootstrap: boot, Company: company})
	if err != nil {
		return err
	}
	if err := e.Start(ctx); err != nil {
		// Everything the engine opened comes down with it. Returning
		// without this leaves a broker, a store and a set of held seat
		// leases behind — and the leases are the expensive half, because
		// a peer cannot take those seats until they lapse at the TTL.
		e.Stop(context.WithoutCancel(ctx))
		return err
	}

	// MERGED: one process is both engine and API, sharing one broker and
	// one store. The API half is what makes the node reachable at all —
	// every inbound webhook arrives through it — so an engine that ran
	// without it would hold seats and hear nothing.
	surface, err := serveAPI(ctx, boot, e, log)
	if err != nil {
		e.Stop(context.WithoutCancel(ctx))
		return err
	}

	<-ctx.Done()
	if surface != nil {
		surface.stop(context.WithoutCancel(ctx), log)
	}

	// Detached from the signal context, which is already cancelled — that
	// is what woke us. The drain waits for in-flight turns, bounded only
	// by the context it is given, so handing it this one would abandon
	// every turn already running and make the shutdown the opposite of
	// graceful.
	//
	// The bound belongs to whatever supervises the process: a container
	// runtime's kill grace, an operator's second interrupt. Both already
	// have one and can see things this process cannot.
	log.Info("engine_draining")
	e.Stop(context.WithoutCancel(ctx))
	return nil
}

// httpSurface is the API half of a merged node.
type httpSurface struct {
	app    *api.App
	server *http.Server
}

// stop shuts the HTTP surface down before the engine drains.
//
// BEFORE, deliberately: the drain waits for in-flight turns, and a listener
// still accepting webhooks during it would keep minting new ones. Closing the
// door first is what makes the drain converge.
func (s *httpSurface) stop(ctx context.Context, log *slog.Logger) {
	shutdown, cancel := context.WithTimeout(ctx, apiShutdownGrace)
	defer cancel()
	if err := s.server.Shutdown(shutdown); err != nil {
		// A listener that would not close is not a reason to skip the
		// drain: the seats are the expensive thing to strand.
		log.Warn("api_shutdown_failed", "error", err)
	}
	s.app.Stop()
}

// apiShutdownGrace bounds how long the listener waits for in-flight REQUESTS.
//
// Requests, not turns. A REST call is a read against the local store and a
// webhook is a signature check plus a publish — both are milliseconds — so this
// only ever covers a client that stopped reading its own response. The turns
// are the engine's drain to wait for, and that one is bounded by the process
// supervisor rather than by a constant here.
const apiShutdownGrace = 5 * time.Second

// serveAPI binds the HTTP surface, or reports that this node serves none.
func serveAPI(ctx context.Context, boot *config.Bootstrap, e *engine.Engine,
	log *slog.Logger,
) (*httpSurface, error) {
	if boot.API.Port == 0 {
		// A real posture: a worker-only node runs no dashboard, no REST
		// API and no webhook endpoint. Saying so is the point — an
		// operator who expected an integration to work should learn it
		// here rather than from a webhook that never arrives.
		log.Warn("api_disabled",
			"hint", "api.port is 0, so this node serves no dashboard, no REST "+
				"API and no webhook endpoint; every integration is deaf here")
		return nil, nil
	}

	app := api.New(api.Options{
		Bootstrap:    boot,
		Runtime:      engineRuntime{e},
		QueueBackend: e.Backends().Queue.Backend(),
		// The read surface answers from this node's OWN store. A
		// question it has no source for comes back unknown rather than
		// empty, which is the difference between "this node has no
		// event log" and "the company has done nothing".
		Sources: queries.Sources{Events: e.Backends().Store.Events()},
	})
	// CONFIGURED by construction. The engine only exists because a company
	// config parsed, validated and built an epoch, so by the time this
	// runs a company is active — and a node that never said so would be
	// permanently unready, which takes every working node out of a load
	// balancer's rotation.
	//
	// It is the config plane that will own this once revisions are
	// activated from the store rather than read from a file: the flag then
	// tracks whether THIS node applied the current epoch, which is a
	// question a file cannot ask.
	app.SetConfigured(true)
	app.Start(ctx)

	addr := net.JoinHostPort(boot.API.Host, strconv.Itoa(boot.API.Port))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		app.Stop()
		return nil, fmt.Errorf("api: bind %s: %w", addr, err)
	}
	server := &http.Server{
		Handler: app,
		// A read that never completes holds a connection open forever,
		// and the listener is the one surface an unauthenticated client
		// can reach.
		ReadHeaderTimeout: apiReadHeaderTimeout,
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("api_serve_failed", "error", err)
		}
	}()

	log.Info("api_listening", "addr", listener.Addr().String(),
		"anonymous_read", app.Guard().AnonymousRead(),
		"tokens", app.Guard().Tokens())
	if app.Guard().AnonymousRead() && !auth.BindIsLoopback(boot.API.Host) {
		// Stated rather than assumed. The read surface carries LLM
		// transcripts, diary entries and the whole event stream, and on a
		// bind anything else can reach that is a decision somebody may
		// not have made deliberately.
		log.Warn("api_anonymous_read_on_a_reachable_bind",
			"host", boot.API.Host,
			"hint", "reads serve without a token on an address other machines "+
				"can reach; set api.auth.allow_anonymous_read to false to close them")
	}
	return &httpSurface{app: app, server: server}, nil
}

// apiReadHeaderTimeout bounds how long a client may take to send its request
// line and headers.
//
// Ten seconds is generous for a header on any real network and short enough
// that a connection opened and left silent — the cheapest denial there is
// against a listener — costs one slot for ten seconds rather than for ever.
const apiReadHeaderTimeout = 10 * time.Second

// engineRuntime answers the questions only a co-located engine can.
type engineRuntime struct{ engine *engine.Engine }

func (r engineRuntime) Snapshot() api.RuntimeState {
	host := r.engine.Node().Host()
	return api.RuntimeState{
		InFlight:     r.engine.Backends().Queue.InFlightCount(),
		ShuttingDown: host.Draining(),
		// SERVE, and it is honest rather than optimistic: posture
		// describes the gap between the epoch this node applied and the
		// one its peers have, and with revisions read from a file there
		// is no such gap to be in. It becomes the config plane's answer
		// when there is a pointer for nodes to lag behind.
		Posture: "serve",
		Seats:   host.Held(),
	}
}
