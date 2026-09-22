package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/chartapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/api/opsmcp"
	"github.com/crewlet/crewlet/internal/api/pagepolicy"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/static"
)

// App is the HTTP surface: the dashboard, the live socket and the REST routes.
type App struct {
	guard *auth.Guard
	cors  *auth.CORS

	// csrf refuses a state change a cross-site page could have caused.
	// Separate from cors because the two answer different questions: CORS
	// decides whether an attacker's page may READ the answer, and on a
	// write that is the part they do not need.
	csrf *auth.CSRF

	// secure says a BROWSER reaches this deployment over https, which is
	// what the `Strict-Transport-Security` header turns on. Read from
	// `api.external_url` rather than from the bind address or `r.TLS`:
	// this engine ordinarily sits behind a TLS-terminating proxy, so the
	// request it receives is plain http on a loopback socket.
	secure bool
	state  *livestate.LiveState
	stream *stream.Service

	// runtime is the engine this process runs beside. See NodeRuntime.
	runtime NodeRuntime

	nodeID       string
	queueBackend string

	// events is the node's own log, for the ONE thing this package does
	// with it outside the read registry: seeding the live spend window at
	// boot. Required, like every other estate the engine beside this
	// process opens.
	events *store.EventLog

	// now is the clock, shared with the stream service so a hydration
	// window and a health tick cannot disagree about what time it is.
	now func() time.Time

	// queries is the read surface both transports answer from.
	queries *queries.Registry

	// budgets is the fleet's token counter, for the one route that WRITES
	// to it.
	budgets budgetResetter

	// backup takes a copy of this node's durable state.
	backup backupTaker

	// retention is the fleet's own record of what the log may delete, for
	// the one gesture that WRITES to it: an operator's backup
	// acknowledgement.
	retention retentionWriter

	// nodes installs and lifts the eviction gate. A RECORD on the log
	// rather than a coordination write, which is why it is a different
	// seam from the one above. Nil on a process with no native tracker.
	nodes NodeGate

	// purger destroys a task, as the operator on the request. Its own
	// field rather than a third method on the seam above, because it is
	// the one write here attributed to a PERSON — see [TaskPurger]. Nil
	// leaves the route absent, which is honest on a build that cannot
	// serve it: an operator who cannot purge must not be told they can.
	purger TaskPurger

	// capacity drives a stream's byte ceiling through the maintenance
	// window. On a node that is publishing the verb refuses rather than
	// the route being absent, because "you are in the wrong mode" is the
	// answer an operator needs.
	capacity capacityRunner

	// company reads the engine's CURRENT epoch, which is what
	// [App.Configured] asks.
	company  func() (*config.Company, *org.Organization)
	seatHeld chartapi.Held

	// estate answers whether the replicated estate can be read at the
	// log's floor, for /ready. See [EstateFloor].
	estate EstateFloor

	handler http.Handler
}

// EstateFloor reports whether this node's REPLICATED estate can answer at the
// state log's floor, and names the term that says it cannot.
//
// # Why /ready turns on it
//
// A node's SQL copy is derived from an ordered log, and the log is trimmed. A
// consumer whose position falls below the stream's first sequence is clamped
// UPWARD with no error at all and then reports itself caught up over a hole —
// so a node below the floor answers every question confidently, out of rows
// with records missing from them for good. That is not "briefly behind": it is
// a copy the fleet has abandoned, and a load balancer sending a webhook, a
// dashboard query or a seat's tool read to it gets an answer somebody acts on.
// "There is no such work item" is how a duplicate gets filed.
//
// The engine already decides this, twice and for two purposes — seat admission
// (`engine.Engine.NativeHydrated`, which is the strict form of
// `statelog.Health.Established`) and whether the seats it holds may STAY
// (`engine.Engine.SeatsServiceable`). This seam is the same question asked for
// TRAFFIC, so the node that will not admit a seat also stops being sent work
// to do with one.
//
// A CONTEXT, because the answer is read live: the terms it rests on — the
// published floor, the stream's own first and last sequence — move while the
// process runs, and a cached answer is a node that reports ready through the
// whole window in which it stopped being so.
//
// THE REFUSAL IS NAMED, not merely counted: "below the floor" and "the broker
// could not be reached" call for opposite responses, and a bare false is the
// three-valued answer collapsed into a bool that this codebase refuses
// everywhere else.
type EstateFloor func(ctx context.Context) (ok bool, refusal string)

// routeMounter is what the API needs of a surface it mounts and never calls
// otherwise: its routes.
//
// Declared here, by the consumer, so a test can mount an inert one without
// standing up the store, the plane or the keyring the real surface is built
// from.
type routeMounter interface {
	Routes(mux *http.ServeMux)
}

// authMounter is the /auth surface's own mount, over a mux it can NAME.
//
// [http.ServeMux] satisfies it, which is what the one caller passes. The
// narrower type exists so internal/api/authapi's own gate can walk what the
// surface registered and hold it against internal/api/auth's exemption list —
// the standard mux reports nothing about what was mounted on it, and the
// failure when those two drift is a credential surface behind no credential.
type authMounter interface {
	Routes(mux auth.Mux)
}

// chartMounter is the /chart surface's own mount, and it RETURNS AN ERROR
// where [routeMounter] does not.
//
// The difference is real rather than stylistic: every chart route carries an
// authority policy stated where it is mounted, and a route mounted with none
// — or naming a verb the authority table has no rule for — is a hole that
// ships looking correct. Refusing at mount is what turns it into a boot
// failure, and the boot is the one moment every mistake can be named at once.
type chartMounter interface {
	Routes(mux authz.Mux) error
}

// Options configure the app.
//
// # What is required, and why a nil is refused rather than served around
//
// Runtime, Sources.Company, Sources.Events, Sources.NodeID, the Inbound edge's
// Publisher, Claims, Secrets and AppFlow, Config, Secrets, Setup, Budgets,
// Retention, Capacity and Backup are REQUIRED, and [New] refuses a missing one
// by name.
//
// Every one of them is something the engine beside the API holds: `crewlet
// run` is the only thing that builds an App, it builds one over an engine that
// holds all of these, and a node that serves no API (api.port 0) builds none at
// all. So a nil here is a wiring mistake, and a narrower answer built around it
// (a health body missing the engine's fields, a 503, an unregistered /config)
// would hide that mistake behind something an operator reads as deliberate.
//
// What stays optional is what a real node can lack: a native tracker (a company
// on Jira), an operator MCP surface, a telemetry receiver or a tool bridge (an
// unset environment variable), and the defaults a test injects.
type Options struct {
	// BoundSeat maps a Tier A token id to the chart seat that claims it,
	// and answers empty for a credential nobody in the chart claims.
	//
	// THE COMPANY'S HALF OF A PRINCIPAL. `contact.crewlet_operator_id`
	// declares the binding, and it is what makes somebody at the dashboard
	// act as THEMSELVES rather than as the credential they hold — without
	// it every authority rule asking "do you lead this" falls through to
	// the admin grant, and a founder is indistinguishable from a CI
	// pipeline in every audit row and every refusal.
	//
	// OPTIONAL, and its absence is an ordinary wiring rather than a
	// mistake: an API built before any company is active has no chart to
	// ask, and every credential is then unbound — which is exactly what
	// an unbound one already means.
	BoundSeat func(operatorID string) string

	// SeatHeld reports whether a seat is one somebody in the identity
	// directory is bound to, or nil on a node that cannot tell.
	//
	// NIL SKIPS THE QUESTION rather than answering it. A node that runs no
	// identity domain has a legitimately empty copy of that estate, and
	// reading it as "nobody holds any seat" would report every human seat
	// in the company as unheld on /health — see [chartapi.Held].
	SeatHeld chartapi.Held

	// DevPrincipal is the identity an unauthenticated request resolves to
	// on a development run, or nil.
	//
	// BUILT BY THE CALLER, not from the bootstrap here, because it is a
	// FLAG rather than a config field and its refusals are boot-time: a
	// released binary and a bind other machines can reach are both refused
	// by [auth.NewDevPrincipal], which `crewlet run` calls before it ever
	// builds this. A field would be copied into an image, which is exactly
	// how `api.auth.disabled` reached production.
	DevPrincipal *auth.DevPrincipal

	// Bootstrap supplies the auth posture. Nil is permitted and is not the
	// same as absent config: the guard then refuses every write, because
	// nobody has said who may make one.
	//
	// It does NOT supply the node's name. The raw `node.id` is empty on a
	// node named through CREWLET_NODE_ID and may itself be a ${VAR}; the
	// name every surface reports is [queries.Sources.NodeID], the one
	// config.ResolveNodeID answered.
	Bootstrap *config.Bootstrap

	// Runtime is the engine this process runs beside.
	Runtime NodeRuntime

	// State is the projection to serve. Nil builds an empty one.
	State *livestate.LiveState

	// Sources are what the read surface answers from. Company, Events and
	// NodeID are required; see above. NodeID is the node's RESOLVED id, and
	// it names this node on the health body as well as in the fleet answer,
	// so the two cannot disagree about who answered. Any other source left
	// nil makes its questions UNREGISTERED rather than failing, which is the
	// honest answer for a node that does not have that surface at all (no
	// knowledge backend, no native tracker) and distinct from an empty one.
	Sources queries.Sources

	// QueueBackend names the broker, for the health body.
	QueueBackend string

	// Now is injectable so a test can pin the timestamps.
	Now func() time.Time

	// HealthInterval overrides the shared tick's cadence.
	HealthInterval time.Duration

	// Inbound wires the webhook edge. See [Inbound].
	Inbound Inbound

	// Config serves /config, normally a configapi.Service.
	Config routeMounter

	// Setup serves /setup, normally a setupapi.Service: collecting what
	// an integration still needs and writing it, half into the sealed store
	// and half into the company document.
	Setup routeMounter

	// Auth serves /auth, normally an authapi.Service: how a person
	// BECOMES a principal. It is the one surface here whose routes are
	// not all guarded — five of them are how somebody obtains a
	// credential, so requiring one would be a deployment nobody can
	// enter — and which of the twelve is which is named in
	// internal/api/auth's exemption list, held against the registration
	// by a gate in authapi.
	//
	// ITS OWN MOUNTER TYPE, which takes a mux it can NAME rather than
	// *http.ServeMux: the standard mux does not report what was
	// registered on it, so the gate holding the exemption list against
	// the registration could not read one half of what it is about.
	//
	// OPTIONAL, unlike the four above, and its absence is a real posture
	// rather than a wiring mistake: a deployment whose keyring cannot
	// sign for the fleet has no way to mint a session, and the honest
	// shape is a sign-in surface that is ABSENT rather than one that
	// answers 503 to every attempt. `crewlet validate` refuses that
	// keyring by name, so an operator learns it on a laptop.
	Auth authMounter

	// Chart serves /chart and /company/export, normally a
	// chartapi.Service: the company's org chart, which is a state-log
	// domain of its own rather than part of the stored revision /config
	// writes.
	//
	// REQUIRED, like Config: a node that served the settings and not the
	// chart would answer `chart_not_writable_here` on one surface and 404
	// on the one that refusal points at, which is worse than either alone.
	Chart chartMounter

	// Secrets serves /secrets, normally a secretsapi.Service: the fleet's
	// credential store.
	Secrets routeMounter

	// OtelReceiver serves the sandbox telemetry edge. Nil serves none, and
	// the route is then ABSENT rather than refusing — an endpoint that
	// exists and answers 503 to everything reads as broken, while one that
	// is not there matches what the config says.
	//
	// It belongs to the API rather than to the engine because the route is
	// served by whichever node the box can reach, which need not be the
	// node whose engine minted the endpoint. That is why the token is
	// signed rather than stored.
	OtelReceiver *sandbox.OtelReceiver

	// Bridge serves a running seat's tool surface to a coding agent over
	// MCP. Nil serves none, and the route is then ABSENT for the same
	// reason OtelReceiver's is.
	//
	// Unlike OtelReceiver it is NOT a split-deployment surface: a session
	// is a live tool surface in the process that opened it, so it must be
	// the ENGINE's own bridge, served by the node that runs the seat. A
	// node without the ingress role serves it alone, through [BridgeOnly].
	Bridge *mcpbridge.Bridge

	// Operator is the company's own tracker and knowledge base, served to
	// an operator's AI assistant over MCP. Nil serves none and the route
	// is ABSENT, which is the honest shape for a company on Jira and
	// Confluence: there is nothing here it could manage.
	//
	// GUARDED, like every route the exemption list does not name. It
	// writes to the company, and the credential's own name is what lands
	// on each record as the author — so a request with no principal has
	// nobody to attribute the write to.
	Operator *opsmcp.Server

	// Budgets is the fleet's token counter. Supplied separately from
	// Sources.Budget, which is the READ half: a reset is an operator
	// action against a spend ceiling, and giving the read surface a
	// method that clears one would put it a typo away from every screen
	// that renders spend.
	Budgets budgetResetter

	// Retention is the fleet's record of what the log may delete, for the
	// operator's backup acknowledgement.
	Retention retentionWriter

	// Nodes installs and lifts the eviction gate. Nil leaves the evict and
	// readmit routes answering 503.
	Nodes NodeGate

	// Purger destroys a task as the operator who asked. Nil leaves the
	// purge route unmounted.
	Purger TaskPurger

	// Capacity drives a stream's byte ceiling.
	Capacity capacityRunner

	// Backup copies this node's durable state to a path an operator
	// names.
	Backup backupTaker

	// Estate answers whether this node's replicated estate can be read at
	// the log's floor, for /ready. See [EstateFloor].
	//
	// Optional, and nil is a real configuration rather than a missing
	// wire: a company on Jira and Confluence runs no state-log domain
	// whose health gates anything, which the engine itself reports as
	// trivially established. What nil must NOT be read as is "assume the
	// worst" — a node with nothing replicated would then never be ready.
	Estate EstateFloor

	// Assets overrides the embedded dashboard tree. Nil serves the one
	// compiled into the binary, which is what every deployment does; a
	// test supplies its own to assert about serving rather than about the
	// dashboard's current contents.
	Assets fs.FS
}

// New assembles the app, or refuses a missing required dependency by name.
// See [Options] for which are required and why.
//
// The auth guard is mounted UNCONDITIONALLY. Tier A supplies the posture, never
// the existence of a check — see the auth package for what the alternative
// costs.
func New(opts Options) (*App, error) {
	if err := opts.missing(); err != nil {
		return nil, err
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	state := opts.State
	if state == nil {
		state = livestate.New()
	}

	a := &App{
		guard: auth.New(opts.Bootstrap).BindSeats(opts.BoundSeat).
			WithDevPrincipal(opts.DevPrincipal),
		csrf:         auth.NewCSRF(opts.Bootstrap),
		secure:       servedOverHTTPS(opts.Bootstrap),
		state:        state,
		runtime:      opts.Runtime,
		nodeID:       opts.Sources.NodeID,
		queueBackend: opts.QueueBackend,
		events:       opts.Sources.Events,
		now:          now,
		// DERIVED FROM THE SOURCE THAT ALREADY EXISTS, rather than a
		// second field an embedder could set inconsistently with it:
		// Sources.Company reads the CURRENT epoch, and "is there one" is
		// the whole question [App.Configured] asks.
		company:  opts.Sources.Company,
		seatHeld: opts.SeatHeld,
		estate:   opts.Estate,
	}
	var err error
	a.stream, err = stream.NewService(state, stream.Options{
		Health: a.streamHealth,
		// The SAME set /ready refuses on, read from the one place it is
		// declared — see [framePosture].
		Posture: framePosture,
		// Read through the SOURCES rather than captured, for the same
		// reason every other read here is: a config apply replaces the
		// company, and a map captured at boot would keep cross-linking a
		// renamed seat to the handle it used to have.
		Handles: opts.Sources.RoleHandles,
		// The three config-derived surfaces, read live for the same
		// reason Handles is: an apply replaces the company.
		Roster: func() []map[string]any { return rosterTick(opts.Sources.Company, opts.Runtime) },
		Org:    func() any { return orgProjection(opts.Sources.Company) },
		Tools:  func() []map[string]any { return toolRows(opts.Runtime) },
		// The CONFIGURED rows only. The dispatch ledger is a store read
		// and the snapshot makes none; the screen fetches that half
		// itself through the `schedules` question.
		Schedules: func() any { return opts.Sources.ConfiguredSchedules() },

		Now:            now,
		HealthInterval: opts.HealthInterval,
	})
	if err != nil {
		return nil, fmt.Errorf("api: %w", err)
	}

	tree := opts.Assets
	if tree == nil {
		tree = static.FS()
	}
	files := newAssets(tree)

	// ONE registry, and both transports go through it. Two surfaces
	// answering one question from two implementations is how they end up
	// disagreeing with nobody noticing.
	sources := opts.Sources
	if sources.Health == nil {
		sources.Health = func(ctx context.Context) any { return a.health(ctx) }
	}
	if sources.State == nil {
		sources.State = state
	}
	// Only the engine knows which parsers registered and what its ${VAR}s
	// resolved to, so both are read off the runtime rather than taken from
	// the caller: one source for each, and the one that actually knows.
	sources.Routed = func(ctx context.Context) []string {
		return opts.Runtime.Snapshot(ctx).RoutedSources
	}
	sources.Verifiable = func(ctx context.Context) []string {
		return opts.Runtime.Snapshot(ctx).VerifiableSources
	}
	a.queries = queries.NewRegistry()
	queries.Register(a.queries, sources)
	a.budgets = opts.Budgets
	a.backup = opts.Backup
	a.retention, a.nodes, a.purger = opts.Retention, opts.Nodes, opts.Purger
	a.capacity = opts.Capacity

	mux := http.NewServeMux()
	mux.Handle("GET /health", http.HandlerFunc(a.serveHealth))
	mux.Handle("GET /ready", http.HandlerFunc(a.serveReady))
	mux.Handle("GET /query/{what}", http.HandlerFunc(a.serveQuery))
	// The NAMED read routes — the public REST API. Adapters over the same
	// registry the generic form above reaches; see rest.go.
	a.mountReads(mux)
	// The one WRITE outside /config and the webhook edge. A POST, so the
	// anonymous-read posture never opens it: clearing a company's spend
	// ceiling is not a read, whatever a laptop deployment allows.
	mux.Handle("POST /budgets/reset", http.HandlerFunc(a.serveBudgetReset))
	// Also a POST, and for the same reason: copying every credential and
	// every seat's memory to a path the caller names is not a read,
	// whatever the anonymous-read posture allows.
	mux.Handle("POST /backup", http.HandlerFunc(a.serveBackup))
	// The three retention gestures that write. POSTs for the same reason:
	// moving the floor the trim deletes against, stopping a machine
	// writing and letting it write again are not reads, whatever the
	// anonymous-read posture allows. See retention.go.
	a.mountRetention(mux)
	// The capacity window's own control surface. It is the one thing a
	// maintenance-mode node serves that a publishing one does not need,
	// and it is why the verb can run at all on a topology whose broker
	// binds no socket. See retention.go.
	a.mountCapacity(mux)
	mux.Handle(auth.SocketPath, stream.Handler(a.guard, a.stream, a.answer))
	// The OPERATOR MCP surface: the same tracker and knowledge tools a
	// seat holds, offered to a person's own assistant. Under its own
	// always-guarded prefix rather than under /mcp/, which is exempt
	// wholesale for the sandbox bridge — see opsmcp.Path.
	a.mountOperator(mux, opts.Operator)
	// The dashboard shell and its assets. All four paths are exempt from
	// the guard: the page that prompts for a token cannot itself require
	// one, and it ships no data — every byte it renders comes from an
	// authenticated fetch.
	mux.Handle("GET /{$}", http.RedirectHandler("/dashboard", http.StatusFound))
	mux.Handle("GET /dashboard", http.HandlerFunc(files.serveIndex))
	mux.Handle("GET /favicon.ico", http.HandlerFunc(files.serveFavicon))
	mux.Handle("GET /static/", http.HandlerFunc(files.serveStatic))
	// The inbound edge. Exempt from the guard by prefix (see the auth
	// package) because each route authenticates by provider credential,
	// which is why every one of them verifies before it does anything.
	if err := a.mountWebhooks(mux, opts.Inbound, sources, now); err != nil {
		return nil, err
	}
	// The SANDBOX TELEMETRY edge, exempt by the same prefix rule and for
	// the same reason: the exporter inside a box holds no API token, and
	// giving it one would hand a sandbox the credential that reads the
	// whole company. Its per-run token is in the path instead.
	a.mountOTLP(mux, opts.OtelReceiver)
	mountBridge(mux, opts.Bridge)
	// The config surface. GUARDED in full, reads included: the auth
	// package makes /config one of the two prefixes never eligible for
	// allow_anonymous_read, because reading it exposes the whole company
	// document and writing it changes the company.
	opts.Config.Routes(mux)
	// The other one. /secrets is how a rotation reaches a fleet at all —
	// the coordination broker is inside the engine's process on the
	// default topology, so no second process can write the store — and its
	// listing alone says which credentials a company holds.
	opts.Secrets.Routes(mux)
	// The third, and the newest: connecting an integration without a
	// shell. Guarded by the same prefix rule for the same reason, and
	// reads included — the list of which credentials a company has NOT
	// configured is worth as much to an attacker as the ones it has.
	opts.Setup.Routes(mux)
	// AND THE WAY IN. Mounted after the guarded surfaces and before the
	// chart's, which is a matter of reading order rather than routing:
	// Go's mux prefers the more specific pattern whichever way round they
	// are declared.
	if opts.Auth != nil {
		opts.Auth.Routes(mux)
	}
	// THE ORG CHART, and the only mount here that can fail: its routes
	// carry their authority with their registration, so a policy that is
	// missing or names a verb the table has no rule for is refused now
	// rather than serving an ungated route for the life of the process.
	if err := opts.Chart.Routes(mux); err != nil {
		return nil, fmt.Errorf("api: mount the chart surface: %w", err)
	}
	// THE BROWSER POSTURE WRAPS THE CREDENTIAL ONE, because a preflight
	// carries no credential: the browser sends it itself, before it will
	// attach an Authorization header to anything. Inside the guard every
	// preflight to a guarded route answers 401 and the real request is
	// never sent. See [auth.CORS.Middleware].
	//
	// The security headers go on outside both, so a refusal and a preflight
	// carry them as well as an answer does, and before routing, so the
	// responses no handler writes deliberately (the mux's own 404 and 405,
	// the redirect from `/`, which has an HTML body) are covered without
	// each needing to remember. A handler serving a page replaces the
	// policy with its own.
	//
	// And the drain gate sits inside all three, next to the routes it
	// refuses: see [App.drainGate].
	a.cors = auth.NewCORS(opts.Bootstrap)
	// THE ORDER IS THE ARGUMENT. Outermost is the page policy, so a
	// response no handler thought about still carries its headers. Then
	// CORS, which must answer a preflight before the guard sees it — a
	// browser attaches no credential to one. Then the guard, which
	// resolves the principal. Then the CSRF check, INSIDE the guard
	// because the credential's SHAPE is what decides whether a missing
	// Origin is a refusal, and nothing before the guard knows which shape
	// arrived.
	a.handler = pagepolicy.Apply(
		a.cors.Middleware(a.guard.Middleware(a.csrf.Middleware(a.drainGate(mux)))),
		a.secure)
	return a, nil
}

// missing names every required dependency these options leave nil, or reports
// nil when there is none. See [Options] for why each is required.
func (o Options) missing() error {
	var names []string
	for _, field := range []struct {
		name   string
		absent bool
	}{
		{"Runtime", o.Runtime == nil},
		{"Sources.Company", o.Sources.Company == nil},
		{"Sources.Events", o.Sources.Events == nil},
		{"Sources.NodeID", strings.TrimSpace(o.Sources.NodeID) == ""},
		{"Inbound.Publisher", o.Inbound.Publisher == nil},
		{"Inbound.Claims", o.Inbound.Claims == nil},
		{"Inbound.Secrets", o.Inbound.Secrets == nil},
		{"Inbound.AppFlow", o.Inbound.AppFlow == nil},
		{"Config", o.Config == nil},
		{"Secrets", o.Secrets == nil},
		{"Setup", o.Setup == nil},
		{"Chart", o.Chart == nil},
		{"Budgets", o.Budgets == nil},
		{"Retention", o.Retention == nil},
		{"Capacity", o.Capacity == nil},
		{"Backup", o.Backup == nil},
	} {
		if field.absent {
			names = append(names, "Options."+field.name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	return fmt.Errorf("api: %s required: every process that serves the API "+
		"runs the engine that supplies them, so a missing one is a wiring "+
		"mistake rather than a narrower node", strings.Join(names, ", "))
}

// Inbound is what the webhook edge needs that only the surrounding process
// has: somewhere to republish a delivery, the epoch's verification material,
// and the cross-process dedupe.
//
// The rest of what the edge needs (the event log, the live stream, whether a
// revision is active, the clock) comes from the app itself, so those cannot be
// wired differently here than they are everywhere else on this node.
//
// Publisher, Claims, Secrets and AppFlow are required; see [Options]. The edge
// is mounted on every node that serves the API, because every such node runs
// the queue a delivery is republished onto.
type Inbound struct {
	Secrets   func() webhooks.Secrets
	Publisher queue.Publisher
	Claims    coord.Claims

	// AppFlow finishes a GitHub App creation begun on the setup surface.
	//
	// Threaded from the caller rather than built here because it belongs to
	// the setup service (setupapi.Service.AppFlow), and the redirect URL
	// baked into every app this engine creates points at the webhook mux.
	AppFlow webhooks.AppCompleter

	// Recheck asks the reconcile loop to look at GitHub immediately, when
	// a person has just finished installing an agent's app there.
	//
	// Nil waits out the cadence. See [webhooks.Options.Recheck].
	Recheck webhooks.GitHubRechecker

	// Keys verifies Forge invocation tokens. Nil uses Atlassian's
	// published JWKS.
	Keys webhooks.KeySource
}

// mountWebhooks registers the inbound edge.
func (a *App) mountWebhooks(mux *http.ServeMux, in Inbound, sources queries.Sources, now func() time.Time) error {
	receiver, err := webhooks.New(webhooks.Options{
		Secrets:    in.Secrets,
		Publisher:  in.Publisher,
		Claims:     in.Claims,
		Keys:       in.Keys,
		AppFlow:    in.AppFlow,
		Recheck:    in.Recheck,
		Events:     sources.Events,
		Stream:     a.stream,
		Configured: a.Configured,
		Now:        now,
	})
	if err != nil {
		return fmt.Errorf("api: %w", err)
	}
	receiver.Routes(mux)
	return nil
}

// ServeHTTP makes the app the process's handler.
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.handler.ServeHTTP(w, r) }

// Stream exposes the live channel, for the engine to feed.
func (a *App) Stream() *stream.Service { return a.stream }

// State exposes the projection.
func (a *App) State() *livestate.LiveState { return a.state }

// Guard exposes the auth posture, for the startup line that states it.
func (a *App) Guard() *auth.Guard { return a.guard }

// CORS returns the browser-origin posture, for the startup line that states
// it beside the anonymous-read one.
func (a *App) CORS() *auth.CORS { return a.cors }

// Configured reports whether a company revision is active.
//
// READ THROUGH THE ENGINE'S LIVE EPOCH on every call, so it cannot go stale. A
// flag pushed at startup said "yes" from the first boot and was never
// corrected, which left the unconfigured posture below unreachable in the
// shipped binary. Load-bearing on readiness: an unconfigured node cannot
// verify a webhook signature, so it must leave rotation rather than answer
// deliveries it would only reject.
func (a *App) Configured() bool {
	settings, _ := a.company()
	return settings != nil
}

// Start brings up the shared health tick.
//
// IT DOES NOT SEED THE PROJECTION. The seed reads the node's event store for
// both halves — the feed and the spend window — and it has to run AFTER the
// broadcast subscription is attached, or an event published between the read
// and the subscribe falls into the gap. This runs before it, so the seed is
// the caller's (see observe.Seed, wired in cmd/crewlet).
func (a *App) Start(ctx context.Context) {
	a.stream.StartHealthTicks(ctx)
}

// Stop ends the tick and disconnects every client.
func (a *App) Stop() { a.stream.Stop() }

func (a *App) serveHealth(w http.ResponseWriter, r *http.Request) {
	// ALWAYS 200 while the process is alive, INCLUDING through a drain: an
	// orchestrator watching liveness must not SIGKILL a node that is
	// finishing its in-flight turns. /ready is what steers traffic. That
	// only holds because the listener outlives the drain; see drain.go.
	writeJSON(w, http.StatusOK, a.health(r.Context()))
}

// ReasonEstateBehind is what a refused /ready names when this node's
// replicated estate cannot be read at the log's floor.
//
// One code rather than the term that decided it, for the same reason
// [Readiness.Reason] carries a code at all: it is recorded by a load balancer
// on every failed probe, and a reason that changes as a node catches up
// (below_floor, then behind, then ready) makes one outage look like three.
// The term is logged — see [App.serveReady]. It belongs with [ReasonDraining]
// and [ReasonUnconfigured] in health.go and sits here only because the estate
// seam does.
const ReasonEstateBehind = "estate_behind"

// framePosture maps this node's own posture onto the one its sockets are
// served in.
//
// THREE DIFFERENT THINGS ARE CALLED A POSTURE and this function is the
// boundary between two of them: [Health.Status] carries the NODE posture — the
// config plane's conclusion about this node's config lag — and
// [stream.FramePosture] is how ONE SOCKET is being served. The third, a
// principal's enrolment stage, belongs to the identity work and never reaches
// here; see stream.FramePosture's own doc for why that type is not called
// `Posture`.
//
// The set is [divergedPostures], read rather than restated, because it is the
// same set /ready refuses on: a node whose copy of the company is wrong takes
// itself out of rotation AND stops pushing that copy at the tabs still
// watching. Written twice, a node could leave rotation while its dashboards
// carried on rendering what it held — which is the exact pair of facts an
// operator is trying to reconcile when they look.
//
// `isolated` and `wait` stay LIVE, for the reason /ready stays ready on them:
// wait is ordinary propagation during a rollout, and isolated means NO node
// applied the revision — degrading the socket there would blind every operator
// at the moment the fleet most needs watching.
func framePosture(h stream.Health) stream.FramePosture {
	if _, diverged := divergedPostures[h.Status]; diverged {
		return stream.FrameDegraded
	}
	return stream.FrameLive
}

func (a *App) serveReady(w http.ResponseWriter, r *http.Request) {
	body, status := a.readiness(r.Context())
	// THE ESTATE TERM, AFTER the ones readiness already decided, because
	// it is the narrowest: a draining or unconfigured node is out of
	// rotation for a reason an operator can act on directly, and reporting
	// the estate instead would name a consequence over a cause.
	if status == http.StatusOK {
		if ok, refusal := a.estateReady(r.Context()); !ok {
			body.Ready = false
			body.Reason = ReasonEstateBehind
			status = http.StatusServiceUnavailable
			// THE REFUSAL REACHES THE LOG, NOT THE BODY. /ready is
			// polled by an orchestrator every few seconds, so its
			// body is a stable code a load balancer records, and the
			// term that decided it — below the floor, behind, the
			// broker unreachable — is what somebody reading the node
			// wants. DEBUG, because a node catching up at boot would
			// otherwise write a line per probe for the whole of a
			// legitimate hydration.
			log.DebugContext(r.Context(), "ready_withheld_for_the_estate",
				"node", a.nodeID, "refusal", refusal)
		}
	}
	writeJSON(w, status, body)
}

// estateReady reports whether this node's replicated estate can answer at the
// log's floor, and names the term that says it cannot.
//
// A node with no seam declared has no replicated estate to be behind on, which
// is what a company on Jira and Confluence has — see [Options.Estate].
func (a *App) estateReady(ctx context.Context) (bool, string) {
	if a.estate == nil {
		return true, ""
	}
	return a.estate(ctx)
}

// writeJSON is [httpjson.Write] under this package's own name, kept so the
// route bodies read the same as they always did. The rule it used to state —
// status before body, or the header is already gone — lives with the writer.
func writeJSON(w http.ResponseWriter, status int, body any) {
	httpjson.Write(w, status, body)
}

// Queries exposes the read surface, for a caller that wants to know what this
// node can answer.
func (a *App) Queries() *queries.Registry { return a.queries }

// answer bridges the registry to the socket's error codes.
//
// The codes are the wire protocol's and the errors are the surface's, so the
// mapping lives at exactly one boundary — here. A query surface that returned
// wire codes would be a domain package encoding a transport's vocabulary, and
// a transport that classified errors itself would be a second place for the
// two to disagree about what "unauthorized" means.
func (a *App) answer(ctx context.Context, what string, params map[string]any, operatorID string) (any, error) {
	data, err := a.queries.Answer(ctx, what, params, operatorID)
	switch {
	case err == nil:
		return data, nil
	case errors.Is(err, queries.ErrUnknown):
		return nil, fmt.Errorf("%w: %s", stream.ErrUnknownQuery, what)
	case errors.Is(err, queries.ErrUnauthenticated),
		errors.Is(err, queries.ErrUnauthorized):
		// BOTH ONTO ONE SOCKET CODE, deliberately. The vocabulary here is
		// shared with the dashboard, and the distinction the REST mapping
		// draws — present something, versus the thing you presented does
		// not carry this — has no consumer over a socket: every one
		// authenticates at its handshake, so the unauthenticated arm is
		// unreachable and a second code would be a wire change with
		// nothing to read it.
		return nil, fmt.Errorf("%w: %s", stream.ErrUnauthorized, what)
	case errors.Is(err, queries.ErrNotFound):
		return nil, fmt.Errorf("%w: %s", stream.ErrNotFound, what)
	case errors.Is(err, queries.ErrBadParams):
		// TRANSLATED RATHER THAN LEFT TO THE DEFAULT, which is what it
		// was: an untranslated refusal reached the socket as an
		// unclassified error, so a caller that asked wrong was told the
		// query FAILED and the node warned about its own health. REST
		// already answered 400 here, and the two transports disagreeing
		// about whose fault a request is is exactly what this mapping
		// exists to prevent.
		//
		// THE ONLY ONE HERE THAT KEEPS THE ORIGINAL ERROR, because it is
		// the only one whose message is written FOR the caller: it names
		// the field that was missing and the values the field accepts,
		// and [stream] logs exactly that at debug. The others are
		// deliberately reduced to the query name — a failure's own text
		// can carry a database path, and none of them has a reader.
		return nil, fmt.Errorf("%w: %s: %w", stream.ErrBadParams, what, err)
	case errors.Is(err, queries.ErrUnavailable):
		return nil, fmt.Errorf("%w: %s", stream.ErrUnavailable, what)
	default:
		return nil, err
	}
}

// serveQuery answers a question over HTTP.
//
// The SAME registry the socket uses, reached with the same name — so a
// dashboard in degraded mode (no socket) and one with a socket are looking at
// one implementation, not two that agree today.
func (a *App) serveQuery(w http.ResponseWriter, r *http.Request) {
	what := r.PathValue("what")
	// See [App.answerHTTP] for why this is the total reader.
	operatorID := auth.OperatorOf(r.Context())

	// Params come from the query string, read through the same accessors a
	// socket frame's JSON object goes through — which is what stops a
	// filter being honoured on one transport and ignored on the other.
	data, err := a.queries.AnswerWith(r.Context(), what,
		queries.FromQuery(r.URL.Query()), operatorID)
	if err != nil {
		writeQueryError(w, what, err)
		return
	}
	writeJSON(w, http.StatusOK, data)
}

// writeQueryError maps one failed question onto a status.
//
// SHARED by the generic /query/{what} form and every named route, because a
// caller must not learn that a seat does not exist from one path and that the
// server broke from the other.
func writeQueryError(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, queries.ErrUnknown):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": stream.CodeUnknownQuery})
	case errors.Is(err, queries.ErrUnauthenticated):
		// 401: nobody is asking, so the remedy IS to present something.
		// Unreachable through the wired guard, which answers this a layer
		// up — stated because the mapping is this function's to get right
		// whatever happens to run in front of it.
		writeJSON(w, http.StatusUnauthorized,
			map[string]string{"error": stream.CodeUnauthorized})
	case errors.Is(err, queries.ErrUnauthorized):
		// 403 AND NOT 401, because the caller has already been
		// identified. This refusal is a GRANT one — the guard in front
		// of this resolved somebody, and the question needs authority
		// they do not carry — and 401 means "authenticate", which sends
		// a reader holding a perfectly good credential to go and get a
		// new one. It was 401 while authority here was "is there an
		// operator", where the two answers genuinely coincided; with ten
		// grants they do not, and a narrow reader asking a question
		// outside their grants is the ordinary case rather than the
		// exceptional one.
		//
		// The refusal a 401 is right for still happens, one layer up:
		// the guard answers it for a credential that is absent or
		// refused, before this function is reached at all.
		writeJSON(w, http.StatusForbidden, map[string]string{"error": stream.CodeUnauthorized})
	case errors.Is(err, queries.ErrBadParams):
		// 400 AND ITS OWN CODE. The status was already right; the code
		// said `query_failed`, which names a fault of this node for a
		// request the caller has to change.
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": stream.CodeBadParams})
	case errors.Is(err, queries.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": stream.CodeNotFound})
	case errors.Is(err, queries.ErrUnavailable):
		// 503 AND RETRY-AFTER, because this is the one failure here that
		// is expected to pass: this node is behind the log and is
		// draining, or its coordination store was briefly unreachable. A
		// 500 would tell a client to give up on a screen that will work in
		// a few seconds, and an empty 200 would tell a person the company
		// has no work.
		//
		// THE HINT IS THE REFUSAL'S OWN where it has one — derived from
		// how far behind this node is over how fast it is actually
		// draining — and five seconds otherwise. A flat hint is wrong in
		// both directions on one fleet. The fallback is what an
		// unreachable coordination store gets, since there is no drain to
		// derive from, and it is the shared health tick's own cadence
		// ([stream.HealthInterval]): a client that waits it out asks again
		// having seen at most one newer health frame, which is the soonest
		// it could learn the store is back.
		after := int(stream.HealthInterval / time.Second)
		if hint := queries.RetryAfter(err); hint > 0 {
			after = max(1, int(hint.Round(time.Second)/time.Second))
		}
		w.Header().Set("Retry-After", strconv.Itoa(after))
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]string{"error": stream.CodeUnavailable})
	default:
		// The reason reaches the LOG, not the caller: it can carry a
		// database path or a driver's own message, and these routes are
		// reachable under the anonymous read posture.
		log.Warn("api_query_failed", "what", what, "error", err)
		writeJSON(w, http.StatusInternalServerError,
			map[string]string{"error": stream.CodeQueryFailed})
	}
}

// servedOverHTTPS reports whether a browser reaches this deployment over TLS.
//
// THE EXTERNAL URL AND NOTHING ELSE, for the reason [App.secure] gives: the
// bind address and `r.TLS` both describe the hop between the proxy and this
// process, which on a hardened node is plain http over loopback — so either
// would withhold the header from exactly the deployments that need it.
func servedOverHTTPS(b *config.Bootstrap) bool {
	if b == nil {
		return false
	}
	return strings.HasPrefix(b.API.ExternalBase(), "https://")
}
