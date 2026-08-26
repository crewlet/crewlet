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
	"strings"
	"syscall"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/schedule/sqlledger"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/secrets"
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

	// THE OPERATOR COMMANDS ARE QUIET BY DEFAULT. They open a store, which
	// logs a migration line per schema file and an open line per call —
	// noise on a one-shot command whose stdout is meant to be piped, read
	// or diffed. `run` configures its own level from its own flag, and
	// nothing here silences a WARNING.
	if cmd != "run" {
		logging.Configure(slog.LevelWarn, logging.FormatText, stderr)
	}

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
	case "schema":
		return emitSchema(rest, stdout, stderr)
	case "secrets":
		return runSecrets(rest, stdout, stderr)
	case "config":
		return runConfig(rest, stdout, stderr)
	case "migrate":
		return runMigrate(rest, stdout, stderr)
	case "budgets":
		return runBudgets(rest, stdout, stderr)
	case "llm":
		return runLLM(rest, stdout, stderr)
	case "gitlab", "plane", "jira", "slack", "confluence", "mattermost":
		return runIntegration(cmd, rest, stdout, stderr)
	default:
		usage(stderr)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `crewlet %s — an engine for hierarchically organized AI agent companies

Usage:
  crewlet run [flags]         Run the engine (API, agents and workers by default)
                              -roles ingress|seats|workers narrows what this node does
  crewlet validate [flags]    Check both config tiers without starting anything
  crewlet schema [tier]       Print a tier's JSON Schema (company by default)
  crewlet migrate [config]    Apply pending schema migrations (-check reports only)
  crewlet budgets <cmd>       Show or reset the durable token counters
  crewlet secrets <cmd>       Read and rotate the encrypted secret store
  crewlet config <cmd>        Import, inspect and activate company revisions
  crewlet llm <cmd>           Log in, verify and export the subscription CLI backends
  crewlet gitlab <cmd>        Reconcile the company's seats into a GitLab instance
  crewlet plane <cmd>         Reconcile Plane, and publish knowledge and skills
  crewlet jira <cmd>          Report a Jira instance's seat accounts and projects
  crewlet slack <cmd>         Create, update and install one Slack app per seat
  crewlet confluence <cmd>    Publish authored markdown and tool skills into spaces
  crewlet mattermost <cmd>    Reconcile a Mattermost team, and diagnose one
  crewlet version             Print the version
  crewlet help                Show this message

Config:
  -config   Tier A, this NODE: where its broker, store and API are (default %q)
  -company  Tier B, the COMPANY: its org, providers and integrations (default %q).
            A seed: it is imported into the store when the store does not
            already hold it, and a running node serves the store.
`, version.String(), defaultBootstrapPath, defaultCompanyPath)
}

// The two tiers are separate files because they answer to separate people. A
// node's broker address and store path are an operator's; the org chart and
// the model behind each seat are the company's.
//
// Tier A is read from its file on every boot and never from anywhere else —
// it is what tells the process where its store and broker are, so it cannot
// come from them.
//
// Tier B is different: -company names a SEED. A running node serves the
// revision the activation pointer names, and the file is imported into the
// store when the store does not already hold it (see reconcile.go). That is
// what makes a PUT /config on one node reach every other, and what makes an
// operator's edit to the file still take effect.
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
	debug := fs.Bool("debug", false, "shorthand for -log-level debug")
	roles := fs.String("roles", "",
		"what this node runs, overriding node.roles: ingress, seats, workers")
	apiHost := fs.String("api-host", "", "bind address, overriding api.host")
	apiPort := fs.Int("api-port", -1,
		"bind port, overriding api.port; 0 serves no HTTP at all")
	if err := fs.Parse(args); err != nil {
		return err
	}

	level := logging.ParseLevel(*logLevel)
	if *debug {
		// The SHORTHAND WINS, because writing both is an operator asking
		// for debug in the loudest way available to them.
		level = slog.LevelDebug
	}
	logging.Configure(level, logging.ParseFormat(*logFormat), stderr)
	log := logging.Get("cli")

	boot, company, err := cfg.load()
	if err != nil {
		return err
	}
	// THE FLAGS OVERRIDE THE FILE, and are applied AFTER it loads so a
	// validation failure names the file's own value rather than one the
	// command line put there. Each is applied only when it was actually
	// given: an unset -api-port is not "port 0", which would serve no
	// HTTP at all and make every integration go deaf.
	if err = overrideNode(boot, fs, *roles, *apiHost, *apiPort); err != nil {
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

	// ONE keyring for the process. The reconciler opens revisions with it
	// and the config surface seals them with it; two ciphers over one
	// store would mean a revision written by this node is one it cannot
	// read back.
	cipher, err := boot.Secrets.Cipher()
	if err != nil {
		e.Stop(context.WithoutCancel(ctx))
		return fmt.Errorf("secrets keyring: %w", err)
	}

	// THE STORE IS AUTHORITATIVE AT RUNTIME; the file is a seed. Both
	// halves matter: without the seed a first run has nothing to activate
	// and the node serves a company no peer can see, and without the store
	// a PUT /config on one node would be invisible to every other.
	//
	// Converged BEFORE Start, so seats are claimed under the epoch this
	// node will actually serve rather than under the file's and then moved.
	reconciler, err := startReconciler(ctx, e, boot, company, cipher, log)
	if err != nil {
		e.Stop(context.WithoutCancel(ctx))
		return err
	}

	// THE POSTURE REACHES PEERS from here, because the reconciler owns it
	// and is only built once the engine exists. Set before Start, so the
	// first heartbeat already carries it.
	e.SetPosture(func(ctx context.Context) string {
		return string(reconciler.Posture(ctx))
	})

	// MERGED: one process is both engine and API, sharing one broker and
	// one store. The API half is what makes the node reachable at all —
	// every inbound webhook arrives through it — so an engine that ran
	// without it would hold seats and hear nothing.
	//
	// BOUND BEFORE THE ENGINE STARTS, and the order is a fix rather than a
	// preference. Starting the engine first means claiming seats first, and
	// a seat is not claimed until its per-role MCP children are up — one
	// subprocess per server per seat, each a spawn and a handshake and a
	// tools/list. On the Nimbus example that is 21 children, and the whole
	// inbound edge — dashboard, REST, every vendor's webhook — was dark for
	// as long as they took. Measured at 37 seconds with four seats and every
	// vendor failing FAST; a company whose vendors actually answer takes
	// minutes, and it scales with seats times servers.
	//
	// Nothing here needs a started engine: the node exists, /health and
	// /ready report honestly that it holds no seats yet, and a webhook that
	// arrives in the window is retained rather than dropped because the
	// mailboxes are created before any claiming — see Node.Start.
	surface, err := serveAPI(ctx, boot, e, reconciler, cipher, log)
	if err != nil {
		e.Stop(context.WithoutCancel(ctx))
		return err
	}

	if err = e.Start(ctx); err != nil {
		// Everything the engine opened comes down with it. Returning
		// without this leaves a broker, a store and a set of held seat
		// leases behind — and the leases are the expensive half, because
		// a peer cannot take those seats until they lapse at the TTL.
		//
		// The listener goes first, for the same reason it does in the
		// ordinary shutdown below: it is already accepting, and a node
		// whose engine failed to start must stop answering before it
		// gives up whatever it holds.
		if surface != nil {
			surface.stop(context.WithoutCancel(ctx), log)
		}
		e.Stop(context.WithoutCancel(ctx))
		return err
	}

	// The poll loop, started only once the node is serving: a reconcile
	// that landed mid-boot would apply an epoch to a node that has not
	// claimed anything yet, which is work with nowhere to go.
	go reconciler.Run(ctx)

	<-ctx.Done()

	// HAND THE SIGNALS BACK before draining, so a SECOND interrupt kills
	// the process. Until this runs, signal.NotifyContext is still the
	// installed handler and has already cancelled its context, so every
	// further SIGINT is swallowed — leaving an operator watching a drain
	// they cannot abort from the terminal they started it in, with SIGKILL
	// from somewhere else as the only way out.
	//
	// That escape hatch is what makes the unbounded drain below safe to
	// offer: the bound belongs to whoever supervises the process, and for
	// an operator at a terminal the second press IS their bound.
	stop()

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
	// runtime's kill grace, or the operator's second interrupt that the
	// stop() above just re-armed. Both already have one and can see
	// things this process cannot.
	log.Info("engine_draining")
	e.Stop(context.WithoutCancel(ctx))
	return nil
}

// httpSurface is the API half of a merged node.
type httpSurface struct {
	app       *api.App
	server    *http.Server
	projector *observe.Projector
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
	// After the listener, so no socket can be reading the projection while
	// its feed is torn down, and before the engine drains, so the drain's
	// own turns are not projected onto a page nobody can reach.
	s.projector.Stop(shutdown)
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
// companySecrets reads the verification material out of the engine's CURRENT
// epoch, on every request.
//
// Not captured once: a config reload replaces the epoch, and a receiver holding
// the old one would keep rejecting deliveries signed with a rotated secret —
// a failure that looks exactly like an attack and resolves only on restart.
// companyConfig is the engine's CURRENT company document, or nil.
func companyConfig(e *engine.Engine) *config.Company {
	if company := e.Company(); company != nil {
		return company.Config
	}
	return nil
}

func companySecrets(e *engine.Engine) webhooks.Secrets { return e.WebhookSecrets() }

func serveAPI(ctx context.Context, boot *config.Bootstrap, e *engine.Engine,
	reconciler *engine.Reconciler, cipher secrets.Cipher, log *slog.Logger,
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

	nodeID, err := config.ResolveNodeID(boot, nil)
	if err != nil {
		return nil, fmt.Errorf("api: node identity: %w", err)
	}
	// ONE config surface, shared by the REST routes and the socket
	// queries. Sealing and opening with the SAME keyring the reconciler
	// applies through: two ciphers over one store would mean a revision
	// written here is one no node can read.
	configSurface := configapi.New(configapi.Options{
		Store: e.Backends().Store, Cipher: cipher,
	})

	app := api.New(api.Options{
		Bootstrap:    boot,
		Runtime:      engineRuntime{engine: e, reconciler: reconciler},
		QueueBackend: e.Backends().Queue.Backend(),
		// The read surface answers from this node's OWN store. A
		// question it has no source for comes back unknown rather than
		// empty, which is the difference between "this node has no
		// event log" and "the company has done nothing".
		Sources: queries.Sources{
			Events: e.Backends().Store.Events(),
			// Read through the ENGINE's epoch rather than a captured
			// company: an apply replaces it, and a screen bound to the
			// one this process booted on would describe a company that
			// is no longer running.
			Company:       func() *config.Company { return companyConfig(e) },
			Coord:         e.Backends().Coord,
			Plane:         e.Backends().Fleet,
			Runs:          sqlledger.New(e.Backends().Store.SQL()),
			Conversations: ledgerstore.NewConversations(e.Backends().Store),
			Diary:         learning.NewDiary(e.Backends().Store),
			Episodes:      learning.NewEpisodes(e.Backends().Store),
			Config:        configSurface,
			Budget:        e.Backends().Fleet,
			// The DURABLE record of detached coding runs. Read rather
			// than projected: a run parked on a person's question can
			// wait days, and the live projection sweeps long before
			// that — so the states that most need somebody were the
			// ones least likely to be on screen.
			Sandbox: sandbox.NewSQLStore(e.Backends().Store),
			NodeID:  nodeID,
		},
		// The inbound edge. It republishes onto THIS node's queue and
		// dedupes through the FLEET'S coordination store, which is what
		// makes a delivery that lands on any node wake the seat's owner
		// exactly once — a vendor retrying reaches whichever node the
		// load balancer picks, so a claim only this node could see would
		// suppress nothing.
		// The WRITE half of the counter, for POST /budgets/reset. On the
		// default topology the coordination store is this engine's own
		// embedded broker, so a node that is running is the only thing
		// that can reach it — which is why the reset is a route and not
		// only a CLI subcommand.
		Budgets: e.Backends().Fleet,
		Config:  configSurface,
		Inbound: api.Inbound{
			Secrets:   func() webhooks.Secrets { return companySecrets(e) },
			Publisher: e.Backends().Queue,
			Claims:    e.Backends().Fleet,
		},
	})
	// CONFIGURED by construction. The engine only exists because a company
	// config parsed, validated and built an epoch, so by the time this
	// runs a company is active — and a node that never said so would be
	// permanently unready, which takes every working node out of a load
	// balancer's rotation.
	//
	// It stays true for the life of the process: an apply that FAILS
	// leaves the node serving the previous epoch, which is a configured
	// node. What a failed apply changes is the POSTURE, and /ready reads
	// that — the two answer different questions, and collapsing them would
	// take a correctly-serving node out of rotation for being behind.
	app.SetConfigured(true)
	app.Start(ctx)

	// The other half of the observability pipeline. The engine already
	// persists what THIS node publishes (see engine.Backends); this is what
	// puts the whole company's events on this node's dashboard, and it is a
	// broadcast subscription because a browser attached here must see turns
	// that ran anywhere — see observe.Projector.
	//
	// Started before the listener binds, so a socket cannot open onto a
	// projection that is not yet being fed.
	projector := observe.NewProjector(e.Backends().Queue, app.Stream())
	if err = projector.Start(ctx); err != nil {
		app.Stop()
		return nil, err
	}

	addr := net.JoinHostPort(boot.API.Host, strconv.Itoa(boot.API.Port))
	// Through a ListenConfig so a shutdown signal arriving while the bind
	// is in flight aborts it, rather than leaving a listener nobody will
	// serve from — the bind can block on a DNS lookup for the host.
	var listenCfg net.ListenConfig
	listener, err := listenCfg.Listen(ctx, "tcp", addr)
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
	// THE CONFIG-DERIVED SURFACES, re-sent whenever an apply changes them.
	//
	// The roster, the org tree and the tool catalogue all come from the
	// company document, so no event will ever correct them: a revision
	// that adds, renames or removes a role produces nothing a projection
	// could learn from, and an overlay merge cannot express a deletion at
	// all. Without this an open dashboard renders the company it connected
	// to until someone reloads.
	//
	// Registered after the app exists, which is the whole reason it is a
	// setter — see Engine.SetOnApplied.
	e.SetOnApplied(func(context.Context) {
		app.Stream().Broadcast("seats", app.Stream().Roster())
		app.Stream().Broadcast("org", app.Stream().Org())
		app.Stream().Broadcast("tools", app.Stream().Tools())
		app.Stream().Broadcast("schedules", app.Stream().Schedules())
	})

	return &httpSurface{app: app, server: server, projector: projector}, nil
}

// apiReadHeaderTimeout bounds how long a client may take to send its request
// line and headers.
//
// Ten seconds is generous for a header on any real network and short enough
// that a connection opened and left silent — the cheapest denial there is
// against a listener — costs one slot for ten seconds rather than for ever.
const apiReadHeaderTimeout = 10 * time.Second

// engineRuntime answers the questions only a co-located engine can.
type engineRuntime struct {
	engine     *engine.Engine
	reconciler *engine.Reconciler
}

// Tools is the catalogue this node serves, for the dashboard's tool screen.
//
// THE EPOCH'S SHARED CATALOGUE, not a seat's. A per-role MCP server gives each
// seat its own child and its own registry, so there is no single "the tools" a
// company has — and picking one seat's would render a catalogue that is right
// for one row of the agent screen and wrong for the rest. The shared surface
// is the one every seat has, which is the honest answer to "what does this
// company run".
func (r engineRuntime) Tools() []api.ToolInfo {
	company := r.engine.Company()
	if company == nil || company.Tools == nil {
		return nil
	}
	entries := company.Tools.List()
	out := make([]api.ToolInfo, 0, len(entries))
	for _, entry := range entries {
		source := "builtin"
		if server, ok := entry.FromMCP(); ok {
			source = server
		}
		out = append(out, api.ToolInfo{
			Name:        entry.Name(),
			Description: entry.Tool.Description(),
			Source:      source,
		})
	}
	return out
}

func (r engineRuntime) Snapshot() api.RuntimeState {
	host := r.engine.Node().Host()
	state := api.RuntimeState{
		InFlight:     r.engine.Backends().Queue.InFlightCount(),
		ShuttingDown: host.Draining(),
		Seats:        host.Held(),
		StartedAt:    r.engine.StartedAt().Format(time.RFC3339),
		// Which integrations have a PARSER, which is the only thing that
		// makes a verified delivery reach an agent. Read from the notify
		// service rather than from a list kept here: a hand-maintained
		// one is exactly what drifts, and it would drift towards
		// claiming more than the build does.
		RoutedSources: r.engine.RoutedSources(),
		// And which of them could actually verify a delivery, from the
		// RESOLVED secrets rather than from the config text. Same reason:
		// a list of what the document names would claim more than this
		// process can do.
		VerifiableSources: r.engine.VerifiableSources(),
	}
	if r.reconciler != nil {
		// Read live, on every probe, rather than cached: a cached
		// posture is a node that reports healthy through the whole
		// window in which it stopped being so — and this is the ONLY
		// place an operator can see why a node left rotation, since
		// /ready answers a bare 503 either way and "draining" and
		// "cannot apply epoch 41" call for opposite responses.
		state.Posture = string(r.reconciler.Posture(context.Background()))
		state.AppliedEpoch = r.reconciler.Applied()
	}
	return state
}

// emitSchema writes a tier's JSON Schema.
//
// # Why the CLI carries this at all
//
// The schema is not read by the engine — the Go types in `internal/config`
// are the validator, and the schema is a SUBSET of them. It exists for the
// people and tools that author a config without running one: the
// `# yaml-language-server: $schema=` modeline at the top of every shipped
// example, a CI linter, an assistant writing a config from scratch.
//
// It is generated rather than hand-written for the reason every generated
// artifact is: the two would diverge, and a schema that red-underlines a
// config the engine would happily run teaches authors to ignore it. The
// checked-in copies under `schema/` are regenerated by this command and
// compared in CI, so a config field added without one is a failing build
// rather than a stale file nobody opens.
func emitSchema(args []string, stdout, stderr io.Writer) error {
	// THE TIER COMES OFF THE FRONT, before the flags are parsed. Go's flag
	// package stops at the first non-flag argument, so parsing first would
	// leave `-o` in the positional tail on the natural spelling —
	// `crewlet schema company -o path` — and silently write to stdout.
	name, args := splitSubject(args)
	fs := flag.NewFlagSet("schema", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("o", "", "write to this file instead of stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// EXACTLY ONE tier, wherever it appeared. The tail after flag parsing
	// holds the trailing form (`schema -o path company`) and anything left
	// over in either form — a second tier, a typo — which must be an error
	// rather than a silently ignored argument.
	tail := fs.Args()
	if name == "" && len(tail) == 1 {
		name, tail = tail[0], nil
	}
	if len(tail) > 0 {
		fmt.Fprintf(stderr, "usage: crewlet schema [%s|%s] [-o path]\n",
			config.TierCompany, config.TierBootstrap)
		return errors.New("name at most one tier")
	}
	if name == "" {
		// THE COMPANY TIER IS THE DEFAULT because it is the one an author
		// edits: Tier A is a handful of URLs an operator writes once,
		// while Tier B is the org chart, the providers and every
		// integration — which is what an editor's completion is for.
		name = string(config.TierCompany)
	}

	tier := config.Tier(name)
	body, err := config.Schema(tier)
	if err != nil {
		return err
	}
	// A TRAILING NEWLINE, because the file is checked in and compared: a
	// generator that emits none leaves every editor and every `git diff`
	// reporting a change nobody made.
	body = append(body, '\n')

	if *out == "" {
		_, err = stdout.Write(body)
		return err
	}
	if err := os.WriteFile(*out, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", *out, err)
	}
	fmt.Fprintf(stdout, "wrote %s (%s tier, %d bytes)\n", *out, tier, len(body))
	return nil
}

// splitSubject peels a leading positional argument off a subcommand's args.
//
// Returns "" when the first argument is a flag, which is what lets both
// orders work: `schema company -o x` and `schema -o x company`.
func splitSubject(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

// overrideNode applies the run flags that override Tier A.
//
// # Why these three and not a general mechanism
//
// They are the fields whose right value depends on WHERE the process is
// running rather than on what the company is: which job this node does in a
// fleet, and where its HTTP surface binds. Everything else in Tier A is a
// property of the deployment that belongs in the file, where it can be
// reviewed — a flag for each would be a second configuration surface with no
// history and no validation story.
func overrideNode(boot *config.Bootstrap, fs *flag.FlagSet,
	roles, apiHost string, apiPort int,
) error {
	if isFlagSet(fs, "roles") {
		names := splitRoles(roles)
		if len(names) == 0 {
			return errors.New("-roles was given with no role names")
		}
		// THROUGH THE SAME PARSER the file goes through, which fails
		// CLOSED: an unknown name is an error rather than a skipped
		// entry, because a typo like `-roles seat` would otherwise
		// produce a node that runs nothing and reports itself healthy.
		// The flag is applied after the file has loaded, so this is the
		// only thing that validates it.
		if _, err := placement.ParseRoles(names); err != nil {
			return fmt.Errorf("-roles: %w", err)
		}
		boot.Node.Roles = names
	}
	if isFlagSet(fs, "api-host") {
		boot.API.Host = apiHost
	}
	if isFlagSet(fs, "api-port") {
		if apiPort < 0 || apiPort > 65535 {
			return fmt.Errorf("-api-port must be 0 (no HTTP surface) or "+
				"a port 1..65535, got %d", apiPort)
		}
		boot.API.Port = apiPort
	}
	return nil
}

// splitRoles reads a comma-separated role list, ignoring blank entries so a
// trailing comma is not a role named "".
func splitRoles(value string) []string {
	var out []string
	for _, name := range strings.Split(value, ",") {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}
