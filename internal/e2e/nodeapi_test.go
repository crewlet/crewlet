package e2e

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/chartapi"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/secretsapi"
	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/backup"
	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/org"
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
// CI job. Measured: the chart surface and the per-seat write path were added
// to cmd/crewlet and missed here, and every cluster case failed four bring-up
// attempts with `setupapi: Options.Seats required` while the shared suite
// stayed green.
//
// When you add a required seam to cmd/crewlet, add it here in the same
// change, and run BOTH suites. CLAUDE.md says the same thing in one line: a
// run of `make test` by itself has not exercised the fleet.
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
	company := func() (*config.Company, *org.Organization) {
		if current := e.Company(); current != nil {
			return current.Config, current.Org
		}
		return nil, nil
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
	// THE NODE'S OWN KEYRING, as cmd/crewlet hands it over: every node in
	// this suite carries one (withKeyring), and a secret store built with
	// none — which this harness used to pass — refused every credential a
	// setup submission sealed, on a shape `crewlet run` never produces.
	cipher, err := boot.Secrets.Cipher()
	if err != nil {
		return fail("keyring", err)
	}
	setupSurface, err := setupapi.New(setupapi.Options{
		Company: company,
		Config:  configSurface,
		// THE OTHER HALF OF A COMPANY, exactly as cmd/crewlet passes it:
		// a seat's own document is the org chart's rather than the stored
		// revision's, so a per-seat submission writes through the engine.
		Seats:       e,
		Secrets:     fleetsecrets.New(backends.Fleet, cipher),
		Resolve:     e.LookupSecret,
		Passes:      e.SetupRunner(),
		Sink:        e.SetupSink,
		Status:      status,
		SlackApps:   e.SlackApps,
		StateCipher: cipher,
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

	// The chart surface, wired as cmd/crewlet wires it. It is REQUIRED by
	// api.New rather than optional, because a narrower answer built around
	// a nil — an absent route, a 503 — reads as deliberate and hides the
	// wiring mistake.
	chartSurface, err := chartapi.New(chartapi.Options{
		Reader: e.Chart(),
		Authority: func(actor string, kind chart.AuthorKind,
			grants []iam.Grant, provenance chart.Provenance) chartapi.Writer {
			return e.ChartWriter().As(actor, kind, grants, provenance)
		},
		// WHO IS ASKING, THREE-VALUED, straight from what the guard
		// resolved. It used to be a blunt translation beside the
		// guard — a recognised token became a machine principal
		// holding every grant — and that was a second security
		// decision about one request: the guard admitted a credential
		// and this told the authority table it could do anything. The
		// guard composes the principal now, from the token's own
		// declared grants intersected with this node's ceiling, and
		// the seat binding travels with it.
		Principal: func(r *http.Request) (iam.Principal, iam.Resolution) {
			return iam.From(r.Context())
		},
		Chart:   engine.ChartAuthorityOf(e),
		Fleet:   e,
		Company: company,
	})
	if err != nil {
		return fail("chart surface", err)
	}

	opts := api.Options{
		Bootstrap:    boot,
		SeatBindings: auth.SeatBindings{Directory: e, Chart: engine.SeatViewOf(e)},
		Runtime:      runtime,
		Inbox:        e,
		Chart:        chartSurface,
		QueueBackend: backends.Queue.Backend(),
		Sources: queries.Sources{
			Events:  backends.Store.Events(),
			Company: company,
			NodeID:  nodeID,
			Chart:   engine.ChartAuthorityOf(e),
			// WHOSE RECORD SOMEBODY ELSE'S LOGIN NAMES, as cmd/crewlet
			// hands it: api.New refuses a missing one by name.
			Holders: e,
		},
		Config:    configSurface,
		Secrets:   secretSurface,
		Setup:     setupSurface,
		Budgets:   backends.Fleet,
		Retention: backends.Fleet,
		Capacity:  e,
		Backup:    copier,
		// THE ENGINE'S OWN TRAIL, as cmd/crewlet hands it: the guard
		// reports every refused bearer and every token use through it.
		AuthEvents: e.AuthEvents(),
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
