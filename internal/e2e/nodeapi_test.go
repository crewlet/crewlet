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
	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/backup"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/org"
)

// listenForBridge binds the listener a node's API — and with it the seat's
// tool bridge — is served on, for the engine the node is built with to name as
// the address a box on this host dials ([listenerURL],
// [engine.Options.BoxHostURL]).
//
// BOUND BEFORE THE ENGINE, and held until the server that serves on it: the
// engine takes the address once, when it builds its bridge
// ([mcpbridge.Build]), and an httptest server picks its port only when it
// starts — after the engine that has to name it. A port released in between
// could be anybody's by the time a box dials it. Every node here serves its
// bridge, as `crewlet run` does a seats node's: every agent seat's executor is
// a run in a box that calls the seat's tools back over it, so a node without
// one launches nothing.
//
// THE HOST ADDRESS AND NOT [mcpbridge.BaseURLVar], because that is what
// `crewlet run` gives a seats node that binds a listener, and every box here
// is in the `direct` cell: the harness runs the quickstart's shape, a node
// with no bridge variable at all, rather than a configuration the quickstart
// never asks for.
//
// ON addr, or on a port of the system's choosing where it is empty: a case
// restarting a node binds the address the node it replaces bound, as a node
// restarted on its own api.port does — the URL a box was handed addresses
// that socket and no other.
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
// cmd/crewlet's own wiring is the other one, and the two are deliberately not
// the same function: this one serves a subset of the surfaces, on a test
// listener, with a sped-up tick and an amend hook. What they DO share is
// every required seam, and nothing compares the two lists.
//
// So a required option added there and missed here is not a compile error —
// it is [api.New] or [setupapi.New] refusing by name, at run time, in THIS
// package. And this package declares internal/solo, so `make test` never
// reaches it: the refusal lands only in `make test-solo`, which is a separate
// CI job. Measured: a surface and a write path were added to cmd/crewlet and
// missed here, and every cluster case failed four bring-up attempts with a
// `setupapi: Options.… required` refusal while the shared suite stayed green.
//
// When you add a required seam to cmd/crewlet, add it here in the same
// change, and run BOTH suites. CLAUDE.md says the same thing in one line: a
// run of `make test` by itself has not exercised the fleet.
//
// THE HUMAN SURFACES ARE NOT COPIED, and they are the exception that shows the
// cure. The sign-in surface, the identity directory, the human write surface
// and the two credential arms beside them are built by
// [api.NewHumanSurfaces], which `crewlet run` calls too — they lived in
// package main, where nothing here could reach them, so this suite mounted
// none of them and a fleet whose sessions did not cross between its members
// left it green. A surface built here by a function the command also calls
// cannot drift from the one it serves; a copy can.
//
// ONE WIRING FOR EVERY CASE HERE, because there is one wiring: the API is built
// from the engine's own store, broker and coordination plane, and reads its
// company off the live epoch rather than being told once that one is active.
// The three harnesses in this package each assembled their own before, with no
// runtime and no surfaces, which made every one of them a shape `crewlet run`
// does not produce.
//
// THE TEARDOWN IS RETURNED rather than registered, in the order the pieces came
// up, so the caller runs it in reverse: the listener before the projector that
// feeds what it serves, the projector before the app, and all three before the
// engine whose queue the projector reads. It is returned on failure too,
// holding whatever did come up. The error is returned rather than raised
// because a fleet member is built off the test's goroutine, where t.Fatalf
// ends only that goroutine and leaves the test running with a nil member.
//
// THE NATIVE SOURCES ARE NOT COPIED EITHER — the read surface's board, pages,
// backlinks, search and file listing, and the operator surface both transports
// serve — and for the same cure: each is built by internal/api
// ([api.NativeSources], and the operator inside [api.NewHumanSurfaces]) over
// the engine, resolved per request, because a node meets its tracker and
// knowledge base at its first company, which a node that booted with none
// meets after its API is serving. So `/operator/act` here is the route a
// person's dashboard writes through, and the viewer names the same verbs. The
// file byte routes are IN too, served by the human write surface, because a
// file's bytes crossing between members is something only a fleet here can
// exercise.
//
// chatLive IS THE HUB THE NODE'S BOOT ATTACHED to its engine ([newEngine]),
// and the socket is mounted over it, as `crewlet run` hands its own to
// serveAPI. It is the process's rather than this surface's — built beside the
// engine on a node that serves no API as well — so it is neither built nor
// stopped here: its teardown is the engine's half, after this one.
func wireAPI(
	ctx context.Context, e *engine.Engine, boot *config.Bootstrap,
	chatLive *stream.ChatHub, ln net.Listener, amend func(*api.Options),
) (*api.App, *httptest.Server, []func(), error) {
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
		Holders:        configapi.DirectoryHolders(e.IAM()),
		CompanyWriters: boot.API.Auth.CompanyWriters,
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
		Identity: keyring,
	})
	if err != nil {
		return fail("secret surface", err)
	}
	status, err := e.IntegrationStore()
	if err != nil {
		return fail("integration status", err)
	}
	setupSurface, err := setupapi.New(setupapi.Options{
		Config:  configSurface,
		Secrets: fleetsecrets.New(backends.Fleet, cipher),
		Resolve: e.LookupSecret,
		// WHERE A VENDOR REACHES THIS NODE, from Tier A as cmd/crewlet
		// reads it, and the key it is read from.
		WebhookBase:    boot.API.WebhookBase(),
		WebhookBaseKey: boot.API.WebhookBaseKey(),
		Passes:         e.SetupRunner(),
		Probes:         engine.SetupProbes(),
		Sink:           e.SetupSink,
		Status:         status,
		SlackApps:      e.SlackApps,
		StateCipher:    cipher,
		StateClaims:    backends.Fleet,
	})
	if err != nil {
		return fail("setup surface", err)
	}
	copier, err := backup.New(backup.Options{
		Store: backends.Store, Estate: backup.HoldingFor(engine.HoldsEstate(boot)),
		Conn: backends.Conn(), API: backends.API(),
		Holds: backends.Fleet, Backups: backends.Fleet, NodeID: nodeID,
		// AS cmd/crewlet WIRES IT, the stream included: a backup of a fleet
		// whose files ride the broker's bucket snapshots that stream and
		// checks each object it names, rather than copying them beside it.
		Objects: &backup.Objects{Open: e.OpenObject, Stat: e.StatObject,
			Stream: e.ObjectsStream()},
	})
	if err != nil {
		return fail("backup", err)
	}

	// THE WAY IN, THE DIRECTORY AND THE HUMAN WRITE SURFACE, through the
	// constructor cmd/crewlet builds them with — see above.
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
		ChatLive:     chatLive,
		QueueBackend: backends.Queue.Backend(),
		Sources: queries.Sources{
			// THE ENGINE'S OWN fleet reader, as cmd/crewlet wires it, so
			// the node the harness serves answers history the way a real
			// node does — from every live member.
			Events:  e.History(),
			Usage:   e.UsageEstate(),
			Company: company,
			NodeID:  nodeID,
			Chart:   engine.ChartAuthorityOf(e),
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
			// collector's report off the store every member reads.
			Coord:   backends.Coord,
			Plane:   backends.Fleet,
			Objects: backends.Fleet,
			// THE FLEET'S TOKEN COUNTERS, which the `budgets` question
			// and every seat's live meter read.
			Budget: backends.Fleet,
		},
		Config:    configSurface,
		Secrets:   secretSurface,
		Setup:     setupSurface,
		Retention: backends.Fleet,
		Capacity:  e,
		// THE FLEET BROKER'S MEMBERSHIP, through the engine's own surface,
		// as `crewlet run` wires it.
		FleetBroker: e.FleetBroker(),
		Backup:      copier,
		// THE MOVE OF THE COMPANY'S FILES to an S3 bucket, which only the
		// engine can make, as cmd/crewlet hands it: api.New requires it.
		Objects: e,
		// THE RUNTIME AUDIT, onto this node's own queue as cmd/crewlet
		// hands it: api.New requires it.
		Audit: backends.Queue,
		// THE EVICTION GATE, which every node holding the estate runs
		// from boot and api.New requires.
		Nodes: e.NodeGate(),
		// THE ENGINE'S OWN TRAIL, as cmd/crewlet hands it: the guard
		// reports every refused bearer and every token use through it.
		AuthEvents: e.AuthEvents(),
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
	// AND THE NATIVE HALVES, per question, as cmd/crewlet hands them —
	// through the one constructor, which sets every one or none.
	api.NativeSources(e).Into(&opts.Sources)
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
	stops = append(stops, app.Stop)

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
	// warned about, as cmd/crewlet does: a seed that cannot read the fleet it
	// was just built beside is a broken case, not a degraded node.
	seedCtx, cancelSeed := context.WithTimeout(ctx, seedBudget)
	_, composed := company()
	err = observe.Seed(seedCtx, e.History(), composed, app.Stream().State())
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
	return app, srv, stops, nil
}

// seedBudget bounds the harness's boot seed, at cmd/crewlet's
// projectionSeedBudget: the same read, over the same fan-out, against a fleet
// that is booting just as a real one is.
const seedBudget = 5 * time.Second

// stopInReverse runs a teardown in the reverse of the order it was built.
func stopInReverse(stops []func()) {
	for i := len(stops) - 1; i >= 0; i-- {
		stops[i]()
	}
}
