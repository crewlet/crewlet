package e2e

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/secretsapi"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/backup"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// listenForBridge binds the listener a node's API and seat tool bridge are
// served on, for the engine to name as the address a box on this host dials
// ([listenerURL], [engine.Options.BoxHostURL]).
//
// BOUND BEFORE THE ENGINE, and held until the server that serves on it: the
// engine takes the address once, building its bridge ([mcpbridge.Build]), and
// an httptest server picks its port only when it starts; a port released in
// between could be anybody's. Every node serves its bridge, as `crewlet run`
// does a seats node's. The address is the host's, not [mcpbridge.BaseURLVar]
// (every box here is `direct`). ON addr, or any port where empty: a restarted
// node binds its predecessor's address, which the URL a box holds names.
func listenForBridge(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", cmp.Or(addr, "127.0.0.1:0"))
	if err != nil {
		return nil, fmt.Errorf("binding the bridge's listener: %w", err)
	}
	return ln, nil
}

// listenerURL is where this host dials ln: where a `direct` box dials the
// bridge served on it, as `crewlet run` derives it for a seats node that binds
// a listener, and where a box anywhere dials it as the fleet's address.
func listenerURL(ln net.Listener) string { return "http://" + ln.Addr().String() }

// bridgeListener is [listenForBridge] for a case on the test's own goroutine,
// closing the listener when the test ends if no server took it over.
func bridgeListener(t *testing.T, addr string) net.Listener {
	t.Helper()
	ln, err := listenForBridge(addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// serveAPI is the API half of a node, wired to the engine beside it the way
// cmd/crewlet wires it, served on ln and torn down when the test ends. See
// [wireAPI].
//
// For a case that owns its node for the whole test, on the test's own
// goroutine. A fleet member is built on a goroutine of its own and may have to
// be torn down before the test ends, so it calls [wireAPI] directly.
func serveAPI(
	t *testing.T, e *engine.Engine, boot *config.Bootstrap, chatLive *stream.ChatHub,
	ln net.Listener, amend func(*api.Options),
) (*api.App, *httptest.Server) {
	t.Helper()
	app, srv, stops, err := wireAPI(t.Context(), e, boot, chatLive, ln, amend)
	// REGISTERED BEFORE THE ERROR IS RAISED, so a wiring that failed halfway
	// still stops the half that came up.
	t.Cleanup(func() { stopInReverse(stops) })
	if err != nil {
		t.Fatalf("api: %v", err)
	}
	return app, srv
}

// wireAPI is the API half of a node, wired to the engine beside it the way
// cmd/crewlet wires it, and served on ln ([listenForBridge]).
//
// # IT IS A SECOND COPY OF THE COMPOSITION ROOT, and that has a cost
//
// The two serve different subsets (test listener, sped-up tick, amend hook)
// and share every required seam, and nothing compares the lists. A required
// option added to cmd/crewlet and missed here is not a compile error but
// [api.New] or [setupapi.New] refusing by name at run time, in THIS package,
// which declares internal/solo: only `make test-solo`, a separate CI job,
// reaches it. Add a required seam in both in the same change, run BOTH suites.
// A surface built by a function the command also calls cannot drift; a copy can.
func wireAPI(
	ctx context.Context, e *engine.Engine, boot *config.Bootstrap,
	chatLive *stream.ChatHub, ln net.Listener, amend func(*api.Options),
) (*api.App, *httptest.Server, []func(), error) {
	// THE TEARDOWN IS RETURNED rather than registered, in the order the pieces
	// came up, so the caller runs it in reverse (listener, projector, app, then
	// the engine whose queue the projector reads), and on failure too. The error
	// is returned, not raised: a fleet member is built off the test's goroutine,
	// where t.Fatalf would end only that goroutine.
	var stops []func()
	fail := func(what string, err error) (*api.App, *httptest.Server, []func(), error) {
		return nil, nil, stops, fmt.Errorf("%s: %w", what, err)
	}

	backends := e.Backends()
	// The name the engine runs under and the document its live epoch holds,
	// as cmd/crewlet reads them: an apply replaces the epoch, so a document
	// captured here would describe a company the node no longer runs.
	nodeID := e.Node().ID()
	company := func() (*config.Company, *org.Organization) {
		if current := e.Company(); current != nil {
			return current.Config, current.Org
		}
		return nil, nil
	}
	// ONE KEYRING FOR THE PROCESS, the engine's, handed to every surface
	// that seals or opens, as cmd/crewlet hands it over: every node in this
	// suite carries one (withKeyring). The setup surface was the only one
	// given it here, so the reconciler trusted a plaintext revision, the
	// config surface stored one and /secrets could seal nothing — three
	// shapes `crewlet run` never produces. The reconciler reads the
	// engine's own now, and /config and /secrets refuse to be built
	// without one.
	cipher := e.Cipher()
	// THE RECONCILER, because the health surface reports this node's config
	// posture and its applied epoch, and the reconciler is what knows both.
	reconciler, err := e.NewReconciler(ctx, engine.ReconcilerOptions{
		Store: backends.Store, Fleet: backends.Fleet, Queue: backends.Queue,
		NodeID: nodeID,
	})
	if err != nil {
		return fail("reconciler", err)
	}
	runtime, err := api.NewEngineRuntime(e, reconciler)
	if err != nil {
		return fail("engine runtime", err)
	}
	configSurface, err := configapi.New(configapi.Options{
		Store: backends.Store, Cipher: cipher, Plane: backends.Fleet, Queue: backends.Queue,
		Secrets:        fleetsecrets.New(backends.Fleet, cipher),
		Grants:         e,
		Holders:        configapi.DirectoryHolders(e.IAM()),
		CompanyWriters: boot.API.Auth.CompanyWriters,
		SignInLimits:   boot.API.Auth.Limits,
		Node:           nodeID,
	})
	if err != nil {
		return fail("config surface", err)
	}
	// AND THE IDENTITY ESTATE'S HALF of a rotation, as cmd/crewlet passes it:
	// the engine's core, which every node holds from boot, company or none —
	// so an engine without one is refused, never handed over as a typed nil.
	keyring := e.IdentityKeyring()
	if keyring == nil {
		return fail("secret surface", errors.New("the engine opened no identity estate"))
	}
	secretSurface, err := secretsapi.New(secretsapi.Options{
		Fleet: backends.Fleet, Cipher: cipher, ActiveKeyID: boot.Secrets.ActiveKeyID,
		Identity: keyring, Config: configSurface,
	})
	if err != nil {
		return fail("secret surface", err)
	}
	// THE SETUP SURFACE AS cmd/crewlet BUILDS IT, through the one
	// constructor both call, so a seam it requires cannot be missed here.
	installs, err := e.SlackInstalls(boot.Secrets.TokenMaterial())
	if err != nil {
		return fail("slack installs", err)
	}
	setupSurface, err := api.NewSetupSurface(boot, e, configSurface, cipher, installs)
	if err != nil {
		return fail("setup surface", err)
	}
	// AS cmd/crewlet WIRES IT: the engine's own options, every registered log
	// and the object stream included.
	copier, err := backup.New(e.BackupOptions())
	if err != nil {
		return fail("backup", err)
	}

	// THE HUMAN SURFACES ARE NOT COPIED, the exception that shows the cure: the
	// sign-in surface, the identity directory, the human write surface and the
	// two credential arms beside them come from [api.NewHumanSurfaces], which
	// `crewlet run` calls too. Built in package main they were unreachable here,
	// and a fleet whose sessions did not cross between members stayed green.
	humans, err := api.NewHumanSurfaces(boot, e)
	if err != nil {
		return fail("human surfaces", err)
	}
	if humans.SignIn != nil {
		// A SIGN-IN'S AFTERMATH IS THE SURFACE'S OWN TO STOP — a stale
		// password's rewrite — and it runs after the listener closes
		// and before the engine it writes to, which is where the
		// reversed teardown puts it: after the entries appended below,
		// and before the engine, which every caller stops after this
		// list.
		stops = append(stops, func() { humans.SignIn.Stop(context.WithoutCancel(ctx)) })
	}

	opts := api.Options{
		Bootstrap:    boot,
		SeatBindings: auth.SeatBindings{Directory: e, Chart: engine.SeatViewOf(e)},
		Runtime:      runtime,
		Inbox:        e,
		Credentials:  e,
		// chatLive IS THE HUB THE NODE'S BOOT ATTACHED to its engine
		// ([newEngine]), the process's rather than this surface's, so it is
		// neither built nor stopped here: its teardown is the engine's half.
		ChatLive:     chatLive,
		Objectives:   e.Objectives(),
		QueueBackend: backends.Queue.Backend(),
		Sources: queries.Sources{
			// THE ENGINE'S OWN fleet reader, as cmd/crewlet wires it, so
			// the node the harness serves answers history the way a real
			// node does — from every live member.
			Events: e.History(),
			// EVERY NODE'S company days, through the estate's router as
			// cmd/crewlet reads them, so a member whose usage copy is out
			// of service answers from a peer's.
			Usage:   e.UsageReads(),
			Company: company,
			// WHICH SERVICES THIS NODE SERVES ONLY ONCE RESTARTED, as
			// cmd/crewlet hands it to the org projection.
			AwaitingRestart: e.AwaitingRestart,
			NodeID:          nodeID,
			Chart:           engine.ChartAuthorityOf(e),
			// WHOSE RECORD SOMEBODY ELSE'S LOGIN NAMES, as cmd/crewlet
			// hands it: api.New refuses a missing one by name.
			Holders: e,
			// AND WHOSE CONTACT IDENTITIES THE REGISTRY WITHHOLDS, which
			// api.New requires too.
			WithheldContacts: e.WithheldContacts,
			// AND WHAT A `${VAR}` IN A SEAT'S CONTACT IDS RESOLVES TO,
			// through the node's own chain as cmd/crewlet wires it: a
			// node here is HANDED its environment
			// ([engine.Options.Environment]), so a read surface left on
			// the process environment finds nobody by an id the node's
			// routing resolves.
			Env: e.LookupSecret,
			// THE FLEET VIEW, over the lease table, the control plane and
			// the object collector's recorded report, as cmd/crewlet reads
			// it: the seat states the dashboard is served read placement
			// from the leases, and a member that ran no pass reads the
			// collector's report off the store every member reads, as a
			// report on the object store it agreed with the fleet — through
			// the reader the collector's duty resumes from, so a report
			// left behind by a move of the files is none here too.
			Coord:   backends.Coord,
			Plane:   backends.Fleet,
			Objects: backends,
			// EACH NODE'S MCP, MODEL AND RUNNER ROWS, on its status record
			// beside its presence, as cmd/crewlet hands them.
			NodeStatuses: backends.Fleet,
			// EACH PERSON'S HOME BOARD, the fleet's record, as cmd/crewlet
			// hands it.
			HomeBoards: backends.Fleet,
			// THE FLEET'S TOKEN COUNTERS, which the `budgets` question
			// and every seat's live meter read.
			Budget: backends.Fleet,
			// AND WHAT THIS NODE CAN SAY ABOUT THE LOG'S OWN HISTORY,
			// through the constructor cmd/crewlet wires it with: nil on a
			// member in a maintenance mode, which arms no trim.
			Retention: api.RetentionSource(ctx, e),
			// THE FLEET'S RUNS, whose held calls the MCP answer lists, and
			// what those calls would send, opened under this node's
			// keyring, as cmd/crewlet hands both.
			Sandbox:    sandbox.NewCoordStore(backends.Fleet),
			RunAnswers: e.AnswerSeal(),
			HeldCalls:  sandbox.NewCallSeal(e.Cipher()),
		},
		Config:    configSurface,
		Secrets:   secretSurface,
		Setup:     setupSurface,
		Retention: backends.Fleet,
		Capacity:  e,
		// THE FLEET BROKER'S MEMBERSHIP, through the engine's own surface,
		// as `crewlet run` wires it.
		FleetBroker: e.FleetBroker(),
		// THE WAKE FEEDS' DEAD LETTERS, through the engine's one ledger, as
		// cmd/crewlet hands them: api.New requires them.
		DeadLetters: feedDeadLetters(e),
		Backup:      copier,
		// THE MOVE OF THE COMPANY'S FILES to an S3 bucket, which only the
		// engine can make, as cmd/crewlet hands it: api.New requires it.
		Objects: e,
		// THE RUNTIME AUDIT, onto this node's own queue as cmd/crewlet
		// hands it: api.New requires it.
		Audit: backends.Queue,
		// THE EVICTION GATE, which every node holding the estate runs
		// from boot — and NONE on a node that holds none, as cmd/crewlet
		// hands it: a nil *engine.NodeGate in the interface would be a gate
		// mounted over nothing on a stateless member.
		Nodes: nodeGate(e),
		// THE ENGINE'S OWN TRAIL, as cmd/crewlet hands it: the guard
		// reports every refused bearer and every token use through it.
		AuthEvents: e.AuthEvents(),
		// GITLET AND THE CODE READS, through [api.EngineCode], asked per
		// request: Gitlet is built with the native runtime, which a company
		// applied after boot brings up after this wiring ran.
		Code: api.EngineCode(e),
		// THE ENGINE'S OWN BRIDGE, as cmd/crewlet hands it: a run's
		// session lives in the object that opened it, and an executor
		// launch is refused on a node whose bridge nothing serves.
		Bridge: e.Bridge(),
		Inbound: api.Inbound{
			Secrets:   func() webhooks.Secrets { return e.WebhookSecrets() },
			Publisher: backends.Queue,
			Claims:    backends.Fleet,
			AppFlow:   setupSurface.AppFlow(),
			Recheck:   e,
			CloudIDs:  e.AtlassianCloudIDs(),
		},
		// The shared tick, sped up. It owns the spend rollup (deliberately,
		// so aggregating never runs on the engine's own goroutine
		// mid-turn), and at the production five seconds a test whose turn
		// finishes in 300ms would never see one, and would then "pass" on
		// the snapshot alone while the push path went unexercised.
		HealthInterval: tickInterval,
	}
	// THE SAME HAND-OVER cmd/crewlet makes, which is the one place an
	// absent surface stays absent rather than being mounted over a nil.
	humans.Mount(&opts)
	// THE NATIVE SOURCES ARE NOT COPIED EITHER (board, pages, backlinks, search,
	// file listing, the operator surface): [api.NativeSources] builds each over
	// the engine, resolved per request, since a node that booted with no company
	// meets its tracker and knowledge base only at its first, after its API is
	// serving. So `/operator/act` is the route a person's dashboard writes
	// through, and the file byte routes are IN, served by the human write
	// surface: files crossing between members only a fleet here can exercise.
	api.NativeSources(e).Into(&opts.Sources)
	// AND SETTINGS › MODELS, through the one constructor cmd/crewlet calls.
	modelSurface, err := api.NewModelSurface(e, configSurface, cipher, backends.Queue)
	if err != nil {
		return fail("model surface", err)
	}
	opts.Models = modelSurface
	api.ModelSources(&opts.Sources, e)
	if amend != nil {
		amend(&opts)
	}
	// The contextcheck exemption is cmd/crewlet's, for the same reason: the
	// two push ticks this registers make a bounded context of their own per
	// tick, because a tick is a timer rather than a request.
	app, err := api.New(opts) //nolint:contextcheck // see the paragraph above
	if err != nil {
		return fail("api.New", err)
	}
	app.Start(ctx)
	stops = append(stops, func() { app.Stop(context.WithoutCancel(ctx)) })

	// The other half of the observability pipeline: the engine persists what
	// THIS node publishes, and this is what puts the whole company's events
	// on this node's dashboard.
	projector := observe.NewProjector(backends.Queue, app.Stream())
	if err := projector.Start(ctx); err != nil {
		return fail("projector", err)
	}
	// WithoutCancel: a teardown runs after the context it was built under
	// has ended, which is exactly when a cancelled stop would do nothing.
	stops = append(stops, func() { projector.Stop(context.WithoutCancel(ctx)) })

	// AND THE HISTORY BEHIND IT, seeded as cmd/crewlet seeds it: after the
	// subscription so nothing falls into the gap, before the listener so the
	// first socket sees the seeded snapshot. A harness without it served a
	// node `crewlet run` never produces — one whose envelope carries no
	// `seeded_from` and whose feed starts at boot — and every case reading
	// either proved the harness rather than the node. FAILED here rather than
	// read again until it lands, as cmd/crewlet reads it: a seed that cannot
	// read the fleet it was just built beside is a broken case, not a
	// degraded node.
	seedCtx, cancelSeed := context.WithTimeout(ctx, seedBudget)
	_, composed := company()
	_, err = observe.Seed(seedCtx, e.History(), composed, app.Stream().State())
	cancelSeed()
	if err != nil {
		return fail("seed the live projection", err)
	}

	// ON THE LISTENER THE ENGINE WAS TOLD ABOUT, so the bridge URL its runs
	// are handed is this server's.
	srv := httptest.NewUnstartedServer(app)
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	stops = append(stops, srv.Close)
	// AND THE TOOL BRIDGE SHUT DOWN BEFORE THE LISTENER CLOSES, as
	// `crewlet run`'s surface does ([mcpbridge.Bridge.Shutdown]): a box's
	// call waiting for a seat holder, or a poll for its notes, is a request
	// the close would otherwise wait out. A box whose node stopped goes on
	// calling the fleet's address, here this node, and the close waited for
	// the call's whole holder wait — two seat-lease lifetimes, ninety seconds
	// of every stateless case's cleanup.
	stops = append(stops, e.Bridge().Shutdown)

	// AND THE RECONCILE LOOP, as `crewlet run` starts it once the node is
	// serving: what applies a configuration write. Without it a PUT /config
	// stored a revision and moved the fleet's pointer, and no node here —
	// the one that took the write included — ever applied it, so a harness
	// node was one whose company no write could change. With no pointer
	// published, which is every case here that writes no configuration, a
	// tick reads the pointer, finds none and changes nothing.
	//
	// STOPPED FIRST, before the listener (a teardown runs in reverse), as
	// the signal that ends `crewlet run` ends the loop before its drain:
	// an apply racing the engine's stop would be work with nowhere to go.
	// The goroutine is waited for, so nothing it started outlives the stop.
	loopCtx, endLoop := context.WithCancel(ctx)
	looped := make(chan struct{})
	go func() {
		defer close(looped)
		reconciler.Run(loopCtx)
	}()
	stops = append(stops, func() {
		endLoop()
		<-looped
	})
	return app, srv, stops, nil
}

// seedBudget bounds the harness's boot seed, at cmd/crewlet's
// projectionSeedBudget, which is how long a node waits for it before serving:
// the same read, over the same fan-out, against a fleet that is booting just as
// a real one is.
const seedBudget = 5 * time.Second

// stopInReverse runs a teardown in the reverse of the order it was built.
func stopInReverse(stops []func()) {
	for i := len(stops) - 1; i >= 0; i-- {
		stops[i]()
	}
}

// nodeGate is the engine's eviction gate as the API's seam, or nil on a node
// holding no estate — cmd/crewlet's own rule, so the harness serves a
// stateless member exactly as `crewlet run` does.
func nodeGate(e *engine.Engine) api.NodeGate {
	if g := e.NodeGate(); g != nil {
		return g
	}
	return nil
}

// feedDeadLetters is the engine's operator surface over the wake feeds' dead
// letters as the API's seam, or nil on an engine keeping none — cmd/crewlet's
// own rule, so the harness wires the dead letters exactly as `crewlet run`
// does.
func feedDeadLetters(e *engine.Engine) api.FeedDeadLetters {
	if d := e.FeedDeadLetters(); d != nil {
		return d
	}
	return nil
}
