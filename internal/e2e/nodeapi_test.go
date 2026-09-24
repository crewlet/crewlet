package e2e

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/secretsapi"
	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/backup"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/observe"
)

// serveAPI is the API half of a node, wired to the engine beside it the way
// cmd/crewlet wires it, served on a test listener and torn down when the test
// ends. See [wireAPI].
//
// For a case that owns its node for the whole test, on the test's own
// goroutine. A fleet member is built on a goroutine of its own and may have to
// be torn down before the test ends, so it calls [wireAPI] directly.
func serveAPI(
	t *testing.T, e *engine.Engine, boot *config.Bootstrap, amend func(*api.Options),
) (*api.App, *httptest.Server) {
	t.Helper()
	app, srv, stops, err := wireAPI(t.Context(), e, boot, amend)
	// REGISTERED BEFORE THE ERROR IS RAISED, so a wiring that failed halfway
	// still stops the half that came up.
	t.Cleanup(func() { stopInReverse(stops) })
	if err != nil {
		t.Fatalf("api: %v", err)
	}
	return app, srv
}

// wireAPI is the API half of a node, wired to the engine beside it the way
// cmd/crewlet wires it, and served on a test listener.
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
// What it leaves out is what a case here never reaches: the operator MCP
// surface and the native tracker and knowledge readers, each of which has its
// own suite.
func wireAPI(
	ctx context.Context, e *engine.Engine, boot *config.Bootstrap, amend func(*api.Options),
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
	company := func() *config.Company {
		if current := e.Company(); current != nil {
			return current.Config
		}
		return nil
	}
	// THE RECONCILER, because the health surface reports this node's config
	// posture and its applied epoch, and the reconciler is what knows both.
	reconciler, err := e.NewReconciler(engine.ReconcilerOptions{
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
		Store: backends.Store, Plane: backends.Fleet, Queue: backends.Queue,
	})
	if err != nil {
		return fail("config surface", err)
	}
	secretSurface, err := secretsapi.New(secretsapi.Options{Fleet: backends.Fleet})
	if err != nil {
		return fail("secret surface", err)
	}
	status, err := e.IntegrationStore()
	if err != nil {
		return fail("integration status", err)
	}
	setupSurface, err := setupapi.New(setupapi.Options{
		Company:     company,
		Config:      configSurface,
		Secrets:     fleetsecrets.New(backends.Fleet, nil),
		Resolve:     e.LookupSecret,
		Passes:      e.SetupRunner(),
		Sink:        e.SetupSink,
		Status:      status,
		SlackApps:   e.SlackApps,
		StateClaims: backends.Fleet,
	})
	if err != nil {
		return fail("setup surface", err)
	}
	copier, err := backup.New(backup.Options{
		Store: backends.Store, Conn: backends.Conn(),
		Holds: backends.Fleet, Backups: backends.Fleet, NodeID: nodeID,
	})
	if err != nil {
		return fail("backup", err)
	}

	opts := api.Options{
		Bootstrap:    boot,
		Runtime:      runtime,
		QueueBackend: backends.Queue.Backend(),
		Sources: queries.Sources{
			Events:  backends.Store.Events(),
			Company: company,
			NodeID:  nodeID,
		},
		Config:    configSurface,
		Secrets:   secretSurface,
		Setup:     setupSurface,
		Retention: backends.Fleet,
		Capacity:  e,
		Backup:    copier,
		Audit:     backends.Queue,
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

	srv := httptest.NewServer(app)
	stops = append(stops, srv.Close)
	return app, srv, stops, nil
}

// stopInReverse runs a teardown in the reverse of the order it was built.
func stopInReverse(stops []func()) {
	for i := len(stops) - 1; i >= 0; i-- {
		stops[i]()
	}
}
