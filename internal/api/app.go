package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/chartapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/api/operator"
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
	"github.com/crewlet/crewlet/internal/tracker"
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

	// now is the clock, shared with the stream service so a hydration
	// window and a health tick cannot disagree about what time it is.
	now func() time.Time

	// queries is the read surface both transports answer from.
	queries *queries.Registry

	// backup takes a copy of this node's durable state.
	backup backupTaker

	// audit publishes the runtime audit record every backup leaves; see
	// [operator.Audit].
	audit operator.AuditPublisher

	// retention is the fleet's own record of what the log may delete, for
	// the one gesture that WRITES to it: an operator's backup
	// acknowledgement.
	retention retentionWriter

	// nodes installs and lifts the eviction gate. A RECORD on every
	// identity-claiming log rather than a coordination write, which is why
	// it is a different seam from the one above. Never nil: see
	// [Options.Nodes].
	nodes NodeGate

	// capacity drives a stream's byte ceiling through the maintenance
	// window. On a node that is publishing the verb refuses rather than
	// the route being absent, because "you are in the wrong mode" is the
	// answer an operator needs.
	capacity capacityRunner

	// company reads the engine's CURRENT epoch, which is what
	// [App.Configured] asks.
	company  func() (*config.Company, *org.Organization)
	seatHeld chartapi.Held
	resolve  chartapi.Resolve

	// estate answers whether the replicated estate can be read at the
	// log's floor, for /ready. See [EstateFloor].
	estate EstateFloor

	// identity answers whether anybody is enrolled, for /health. See
	// [Identity].
	identity Identity

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
// (`engine.Engine.StateLogHydrated`, which is the strict form of
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

// Identity reports whether anybody is enrolled in this company's identity
// directory yet — what /health says as `identity`.
//
// # Why /health carries it
//
// A fresh install has nobody in it, and the way the first person gets in is an
// invitation issued under a Tier A token (`crewlet iam invite`). An operator
// who has just started a node, and a dashboard whose sign-in form nobody can
// use yet, both need to know that this is the state they are in rather than a
// sign-in that is failing — and /health is the one surface an install with
// nobody in it can reach: it is a probe, exempt from the guard, and before the
// first person there is no credential but the deployment's own to present
// anywhere else. It is one EXISTS over the node's own rows, and one read of
// the identity log's end beside it, neither of which a probe notices.
//
// A CONTEXT and a live read, for [EstateFloor]'s reason: the answer changes
// the moment the first person lands, and a cached one would go on telling an
// operator to invite somebody who has already signed in.
//
// THREE-VALUED: an error is this node unable to read its identity estate, or
// holding rows that have not applied the whole identity log — every node
// applies it from boot, so a node that has just joined a fleet holds none for
// a moment — which is `unknown` and never `unclaimed`: told there is nobody, a
// dashboard would offer an empty company's guidance to one that has started.
type Identity func(ctx context.Context) (enrolled bool, err error)

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

// guardedMounter is what the API needs of a surface it mounts and never calls
// otherwise: its routes, each mounted through [authz.Router] with the verb it
// is decided by — and an error where one is not.
//
// EVERY SURFACE THE API MOUNTS IS ONE, /auth aside. /config, /secrets and
// /setup were a plain `Routes(*http.ServeMux)` and decided no grant at all:
// the guard in front of them answers only whether somebody resolved, which
// meant "the operator" while an operator token was the only credential and
// meant "anybody" the day a person could sign in holding `state:read`. A
// surface that cannot state its policy where it mounts has no type to be
// mounted as here.
//
// The error is real rather than stylistic: a route mounted with no policy —
// or naming a verb the authority table has no rule for — is a hole that ships
// looking correct. Refusing at mount is what turns it into a boot failure, and
// the boot is the one moment every mistake can be named at once.
//
// Declared here, by the consumer, so a test can mount an inert one without
// standing up the store, the plane or the keyring the real surface is built
// from.
type guardedMounter interface {
	Routes(mux authz.Mux) error
}

// Options configure the app.
//
// # What is required, and why a nil is refused rather than served around
//
// Runtime, Inbox, EventLog, Sources.Company, Sources.Chart, Sources.Holders,
// Sources.WithheldContacts, Sources.Events, Sources.NodeID, Sources.Coord, the
// Inbound edge's Publisher, Claims, Secrets and AppFlow, Config, Secrets,
// Setup, Chart, Retention, Capacity, Backup, Nodes, AuthEvents and Audit are
// REQUIRED, and [New] refuses a missing one by name.
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
	// SeatBindings is where a Tier A token's seat binding is read — the
	// identity directory's row for its login — and resolved, through the
	// SAME chart seam a signed-in person's binding is. See
	// [auth.SeatBindings].
	//
	// THE COMPANY'S HALF OF A PRINCIPAL: it is what makes somebody acting
	// through a token act as THEMSELVES rather than as the credential —
	// without it every authority rule asking "do you lead this" falls
	// through to the admin grant. The binding used to be a field on a
	// seat's contact block naming the token id; it is a directory row now,
	// arbitrated on `iam.seat.<identity>` so two holders cannot claim one
	// seat.
	//
	// THE ZERO VALUE BINDS NOTHING, which is what an API stood up in a
	// suite has: every credential then acts as itself. `crewlet run`
	// always wires both halves over its engine.
	SeatBindings auth.SeatBindings

	// SeatHeld reports whether a seat is one somebody in the identity
	// directory is bound to, or nil where there is no directory to ask.
	//
	// NIL SKIPS THE QUESTION rather than answering it, which is what a
	// suite has: reading the absence as "nobody holds any seat" would
	// report every human seat in the company as unheld on /health — see
	// [chartapi.Held]. `crewlet run` always wires it: every node runs the
	// identity estate from boot, one that has met no company included.
	SeatHeld chartapi.Held

	// Resolve is this node's own `${VAR}` resolution, which the continuous
	// report compares seats' addresses and contact identities through —
	// the same answer /chart/check is given ([chartapi.Resolve]). NIL SKIPS
	// that one finding rather than answering it: every address on the
	// chart's rows is a sealed reference, and references never collide.
	Resolve chartapi.Resolve

	// Sessions turns a browser's cookie into the person holding it.
	//
	// OPTIONAL, and nil is what a suite about something else has: Tier A
	// tokens are then the whole of authentication. `crewlet run` always
	// wires it, beside [Options.Auth], on every node — the identity estate
	// is the engine's core and runs from boot, so a node with no company
	// signs its first person in like any other.
	Sessions *auth.Sessions

	// Tokens turns a machine token — a person's own access token or a
	// service account's, minted at `/iam/credentials` — into its owner.
	// See [auth.Tokens].
	//
	// NIL IS AN API THAT RESOLVES NO MACHINE TOKEN, which is what a suite
	// has. `crewlet run` ALWAYS builds one over its engine's directory,
	// which every node holds from boot.
	Tokens *auth.Tokens

	// AuthEvents is where the guard counts a refused credential and
	// records a Tier A token's use and overreach. REQUIRED: the engine
	// running beside this API holds the node's audit trail, and a guard
	// built without it would refuse a spray of wrong tokens and leave no
	// count of it anywhere.
	AuthEvents auth.Audit

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

	// Inbox is where this node hears whose inbox a committed tracker
	// batch moved, which [New] turns into `inbox_changed` frames for the
	// sockets watching each seat — see [InboxFeed].
	//
	// REQUIRED, like Runtime: every node that serves the API applies the
	// tracker's log, so a node that heard nothing would be a wiring
	// mistake rather than a narrower node, and the dashboard would go back
	// to learning about somebody's work a poll interval late.
	Inbox InboxFeed

	// Credentials is where this node hears whose credentials a committed
	// identity batch moved, which [New] turns into a fresh decision of
	// every open socket opened with one — see [CredentialFeed].
	//
	// REQUIRED, for Inbox's reason: every node applies the identity log
	// whatever its roles, and a socket authenticated at its handshake is
	// ended by the record that ends its credential — a node that heard
	// nothing would leave every revoked session's open tab receiving the
	// company's state until it closed on its own.
	Credentials CredentialFeed

	// State is the projection to serve. Nil builds an empty one.
	State *livestate.LiveState

	// EventLog is this node's OWN event store, which the webhook edge writes
	// every delivery it accepts into. Not the read surface's history, which
	// is the fleet's (Sources.Events): a write goes to this node and nowhere
	// else, and a read of one node's store is a third of a three-node
	// fleet's.
	EventLog *store.EventLog

	// Sources are what the read surface answers from. Company, Events,
	// NodeID, Chart, Holders, WithheldContacts and Coord are required; see
	// above. NodeID is the node's RESOLVED id, and it names this node on the
	// health body as well as in the fleet answer, so the two cannot disagree
	// about who answered. Chart is who leads whom, which every personal
	// question a lead asks about a report is decided by — left nil it
	// answered UNKNOWN to every one of them for the life of the process,
	// which is a 503 that never clears rather than a narrower node. Holders
	// is whose record somebody else's login names, which every personal
	// question naming one resolves through — left nil, every such question
	// is the same 503 for ever, and read literally, as it was, a bound
	// person's login named a record nothing of theirs is kept under.
	// WithheldContacts is whose contact identities the identity directory
	// withholds from what a lead and `colleague` are shown — left nil, a
	// suspended person's chat ids stayed readable. Coord is the fleet's
	// lease table, which the placement push and the fleet answer read. Any
	// other source left
	// nil makes its questions UNREGISTERED rather than failing, which is the
	// honest answer for a node that does not have that surface at all (no
	// knowledge backend, no native tracker) and distinct from an empty one.
	Sources queries.Sources

	// QueueBackend names the broker, for the health body.
	QueueBackend string

	// Now is injectable so a test can pin the timestamps. It is the
	// questions' clock too unless [queries.Sources.Now] pins one of its own.
	Now func() time.Time

	// HealthInterval overrides the shared tick's cadence.
	HealthInterval time.Duration

	// Inbound wires the webhook edge. See [Inbound].
	Inbound Inbound

	// Config serves /config, normally a configapi.Service.
	Config guardedMounter

	// Setup serves /setup, normally a setupapi.Service: collecting what
	// an integration still needs and writing it, half into the sealed store
	// and half into the company document.
	Setup guardedMounter

	// Auth serves /auth, normally an authapi.Service: how a person
	// BECOMES a principal. It is the one surface here whose routes are
	// not all guarded — the ones somebody obtains a credential through
	// cannot require one, or it is a deployment nobody can enter — and
	// which are which is named in internal/api/auth's exemption list,
	// held against the registration by a gate in authapi.
	//
	// ITS OWN MOUNTER TYPE, which takes a mux it can NAME rather than
	// *http.ServeMux: the standard mux does not report what was
	// registered on it, so the gate holding the exemption list against
	// the registration could not read one half of what it is about.
	//
	// OPTIONAL, unlike the four above, for a suite about something else:
	// `crewlet run` mounts one on every node, since every node holds the
	// identity directory from boot, company or none — it used to be absent
	// on a node that had met no company, which was the node its first
	// person had to sign in on. An API with none serves no /auth at all
	// rather than one that answers 503 to every attempt. ABSENT MEANS A
	// NIL INTERFACE, never a nil *authapi.Service inside one, which [New]
	// cannot tell from a surface that is there — [HumanSurfaces.Mount] is
	// the one place the conversion happens.
	Auth authMounter

	// Chart serves /chart and /company/export, normally a
	// chartapi.Service: the company's org chart, which is a state-log
	// domain of its own rather than part of the stored revision /config
	// writes.
	//
	// REQUIRED, like Config: a node that served the settings and not the
	// chart would answer `chart_not_writable_here` on one surface and 404
	// on the one that refusal points at, which is worse than either alone.
	Chart guardedMounter

	// IAM serves /iam, normally an iamapi.Service: the company's identity
	// directory.
	//
	// OPTIONAL, and nil — the routes ABSENT — is what a suite about
	// something else has. `crewlet run` always mounts it: every node holds
	// the directory from boot, and a node with nobody in its company is
	// exactly where the first person is invited.
	IAM guardedMounter

	// Work serves the human write surface over the company's own tracker
	// and knowledge base, normally a workapi.Service: the same tools a seat
	// and the operator's assistant hold, as routes a person reaches from a
	// browser or a script.
	//
	// OPTIONAL, and nil — the routes ABSENT — is what a suite about
	// something else has. `crewlet run` always mounts it, and the surface
	// reads the halves it serves PER REQUEST: a node's tracker and
	// knowledge base come up with its first company, which it may meet
	// long after this API started serving, and a route of a half the
	// company does not run answers as its absence would. It is also where
	// the one operation nothing undoes, destroying a work item, lives: it
	// had a route of its own beside the retention gestures, with an
	// operator check written there, and that check was the second answer
	// to a question the authority table already answers.
	Work guardedMounter

	// Secrets serves /secrets, normally a secretsapi.Service: the fleet's
	// credential store.
	Secrets guardedMounter

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

	// Operator is the company's own tracker and knowledge base as ONE tool
	// catalogue, served to the people who run it — over MCP for an
	// operator's own AI assistant, and over the act route for a person at
	// the dashboard (ADR-0024) — and the one dispatch the human write
	// surface's tool-backed routes call. Nil serves none and both routes are
	// ABSENT, which is what a suite about something else has.
	//
	// ONE SERVER, BUILT ONCE, WHOSE CATALOGUE IS READ PER CALL: the
	// catalogue is the native halves', which a node meets at its first
	// company. A request before that answers `503 no_active_revision`, and
	// one to a node whose catalogue is empty answers as the route's absence
	// would. [HumanSurfaces.Mount] is what hands it over.
	//
	// GUARDED, like every route the exemption list does not name. It
	// writes to the company, and the request's principal is what lands on
	// each record as the author — so a request with no principal has
	// nobody to attribute the write to.
	Operator *operator.Server

	// Retention is the fleet's record of what the log may delete, for the
	// operator's backup acknowledgement.
	Retention retentionWriter

	// Nodes installs and lifts the eviction gate.
	//
	// REQUIRED. It was optional, and nil answered `503 no_state_log`, for
	// a node that had met no company and so ran no state log. There is no
	// such node: the state log is the engine's core, started on every node
	// at boot, so every engine this API runs beside holds a gate — and a
	// nil is a wiring mistake, not a narrower node.
	Nodes NodeGate

	// Capacity drives a stream's byte ceiling.
	Capacity capacityRunner

	// Backup copies this node's durable state to a path an operator
	// names.
	Backup backupTaker

	// Identity says whether anybody is enrolled, for /health. See
	// [Identity].
	//
	// Optional, and nil is what a suite about something else has: with
	// nothing to ask about who is in the company, the health body leaves
	// the field out rather than calling the company unclaimed. `crewlet
	// run` always wires it, over the identity estate every node runs from
	// boot.
	Identity Identity

	// Estate answers whether this node's replicated estate can be read at
	// the log's floor, for /ready. See [EstateFloor].
	//
	// Optional, and nil is a real configuration rather than a missing
	// wire: a company on Jira and Confluence runs no state-log domain
	// whose health gates anything, which the engine itself reports as
	// trivially established. What nil must NOT be read as is "assume the
	// worst" — a node with nothing replicated would then never be ready.
	Estate EstateFloor

	// Audit is where a backup publishes its runtime audit record — this
	// node's own queue, whose publish listener writes the event store. The
	// operator surface is handed the same one by whoever builds it
	// ([operator.Options.Audit]). REQUIRED: copying every credential the
	// company holds to a directory somebody named is the one call an audit
	// is most often opened to find.
	Audit operator.AuditPublisher

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
		guard: auth.New(opts.Bootstrap).BindSeats(opts.SeatBindings).
			WithDevPrincipal(opts.DevPrincipal).WithSessions(opts.Sessions).
			WithTokens(opts.Tokens).
			WithAudit(opts.AuthEvents),
		csrf:         auth.NewCSRF(opts.Bootstrap),
		secure:       servedOverHTTPS(opts.Bootstrap),
		state:        state,
		runtime:      opts.Runtime,
		nodeID:       opts.Sources.NodeID,
		queueBackend: opts.QueueBackend,
		now:          now,
		// DERIVED FROM THE SOURCE THAT ALREADY EXISTS, rather than a
		// second field an embedder could set inconsistently with it:
		// Sources.Company reads the CURRENT epoch, and "is there one" is
		// the whole question [App.Configured] asks.
		company:  opts.Sources.Company,
		seatHeld: opts.SeatHeld,
		resolve:  opts.Resolve,
		estate:   opts.Estate,
		identity: opts.Identity,
	}
	var err error
	a.stream, err = stream.NewService(state, stream.Options{
		Health: a.streamHealth,
		// The SAME set /ready refuses on, read from the one place it is
		// declared — see [framePosture].
		Posture: framePosture,
		// Read through the SOURCES rather than captured, for the same
		// reason every other read here is: a config apply replaces the
		// company, and a directory captured at boot would keep linking a
		// renamed seat to the handle it used to have.
		Seats: opts.Sources.Seats,
		// The three config-derived surfaces, read live for the same
		// reason Seats is: an apply replaces the company. The roster is
		// the CONFIGURATION alone: what each seat is doing comes from the
		// live projection over the placement below.
		Roster: func() []map[string]any { return roster(opts.Sources.Company) },
		Org:    func() any { return orgProjection(opts.Sources.Company) },
		Tools:  func() []map[string]any { return toolRows(opts.Runtime) },
		// The CONFIGURED rows only. The dispatch ledger is a store read
		// and the snapshot makes none; the screen fetches that half
		// itself through the `schedules` question.
		Schedules: func() any { return opts.Sources.ConfiguredSchedules() },
		// WHICH SEATS SOME NODE HOLDS, from the fleet's lease table, for
		// the seat-state vocabulary's `unplaced`.
		Placement: func() (map[string]bool, error) {
			return placementTick(opts.Sources.Company, opts.Sources.Coord, opts.Runtime)
		},

		// WHO LEADS WHOM, which a `watch` frame is decided by — the SAME
		// seam the `work_inbox` question about the same seat asks.
		Chart: opts.Sources.Chart,
		// AND WHOSE RECORD A LOGIN NAMES, which a `watch` frame naming
		// one is resolved through — the SAME seam that question resolves
		// the same name through, so a watch is installed on the record
		// the frames are pushed to.
		Holders: opts.Sources.Holders,

		Now:            now,
		HealthInterval: opts.HealthInterval,
	})
	if err != nil {
		return nil, fmt.Errorf("api: %w", err)
	}
	// AND WHOSE INBOX MOVED, pushed to the sockets watching each seat.
	opts.Inbox.SetOnInboxMoved(a.pushInboxMoved)
	// AND WHOSE CREDENTIALS MOVED, deciding the sockets opened with them
	// again.
	opts.Credentials.SetOnIdentityMoved(a.pushIdentityMoved)

	tree := opts.Assets
	if tree == nil {
		tree = static.FS()
	}
	files := newAssets(tree)

	// ONE registry, and both transports go through it. Two surfaces
	// answering one question from two implementations is how they end up
	// disagreeing with nobody noticing.
	sources := opts.Sources
	if sources.State == nil {
		sources.State = state
	}
	// ONE CLOCK PER APP. The questions read [queries.Sources.Now] while the
	// stream service and the webhook edge read [Options.Now], so a caller
	// that pinned the app's clock and not the sources' had its pushes and
	// deliveries at the pinned instant and its answers on the wall clock —
	// a question stamping "now" on its answer, the live spend window's
	// edges, answered a REST call and a socket call a second apart with two
	// different windows, and a test that pinned the app to compare one
	// question across both transports compared two wall-clock readings
	// instead. A caller that pinned the sources' own clock keeps it.
	if sources.Now == nil {
		sources.Now = now
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
	// WHAT THIS PRINCIPAL MAY DO, from the SAME catalogue the act route
	// serves and the same authority every one of its tools is decided by:
	// a list computed anywhere else would be a second opinion about which
	// buttons work. A caller that supplied its own keeps it.
	if sources.Acts == nil && opts.Operator != nil {
		sources.Acts = opts.Operator.Acts
	}
	a.queries = queries.NewRegistry()
	queries.Register(a.queries, sources)
	a.backup, a.audit = opts.Backup, opts.Audit
	a.retention, a.nodes = opts.Retention, opts.Nodes
	a.capacity = opts.Capacity

	mux := http.NewServeMux()
	mux.Handle("GET /health", http.HandlerFunc(a.serveHealth))
	mux.Handle("GET /ready", http.HandlerFunc(a.serveReady))
	mux.Handle("GET /query/{what}", http.HandlerFunc(a.serveQuery))
	// The NAMED read routes — the public REST API. Adapters over the same
	// registry the generic form above reaches; see rest.go.
	a.mountReads(mux)
	// THE DEPLOYMENT'S OWN CONTROLS — the backup, the retention gestures
	// and the capacity window — each mounted with the grant it takes, and
	// failing the boot where a policy is missing.
	if err := a.mountDeployment(mux); err != nil {
		return nil, fmt.Errorf("api: mount the deployment's controls: %w", err)
	}
	mux.Handle(auth.SocketPath, stream.Handler(a.guard, a.csrf, a.stream, a.answer))
	// The OPERATOR surface: the same tracker and knowledge tools a seat
	// holds, offered to a person's own assistant over MCP and to the person
	// themself over the act route. Under its own
	// prefix rather than under /mcp/, which is exempt from the guard
	// wholesale for the sandbox bridge — see operator.MCPPath.
	a.mountOperator(mux, opts.Operator)
	// The dashboard shell and its assets. All four paths are exempt from
	// the guard: the page that prompts for a token cannot itself require
	// one, and it ships no data — every byte it renders comes from an
	// authenticated fetch.
	mux.Handle("GET /{$}", http.RedirectHandler(auth.PathDashboard, http.StatusFound))
	mux.Handle("GET "+auth.PathDashboard, http.HandlerFunc(files.serveIndex))
	mux.Handle("GET /favicon.ico", http.HandlerFunc(files.serveFavicon))
	mux.Handle("GET /static/", http.HandlerFunc(files.serveStatic))
	// The inbound edge. Exempt from the guard by prefix (see the auth
	// package) because each route authenticates by provider credential,
	// which is why every one of them verifies before it does anything.
	if err := a.mountWebhooks(mux, opts.Inbound, opts.EventLog, now); err != nil {
		return nil, err
	}
	// The SANDBOX TELEMETRY edge, exempt by the same prefix rule and for
	// the same reason: the exporter inside a box holds no API token, and
	// giving it one would hand a sandbox the credential that reads the
	// whole company. Its per-run token is in the path instead.
	a.mountOTLP(mux, opts.OtelReceiver)
	mountBridge(mux, opts.Bridge)
	// The config surface, every route on a grant, reads included:
	// reading it exposes the whole company document and writing it
	// changes the company.
	//
	// These three, like the chart below, fail the boot on a route mounted
	// with no policy — see [guardedMounter] for what they decided before.
	for _, surface := range []struct {
		name string
		m    guardedMounter
	}{
		{"config", opts.Config},
		// /secrets is how a rotation reaches a fleet at all — the
		// coordination broker is inside the engine's process on the
		// default topology, so no second process can write the store —
		// and its listing alone says which credentials a company holds.
		{"secrets", opts.Secrets},
		// Connecting an integration without a shell, reads included —
		// the list of which credentials a company has NOT configured is
		// worth as much to an attacker as the ones it has.
		{"setup", opts.Setup},
	} {
		if err := surface.m.Routes(mux); err != nil {
			return nil, fmt.Errorf("api: mount the %s surface: %w", surface.name, err)
		}
	}
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
	// AND THE IDENTITY DIRECTORY, which fails the same way for the same
	// reason: every /iam route states its authority where it is mounted.
	if opts.IAM != nil {
		if err := opts.IAM.Routes(mux); err != nil {
			return nil, fmt.Errorf("api: mount the identity surface: %w", err)
		}
	}
	// AND THE HUMAN WRITE SURFACE, which fails the same way for the same
	// reason: every route states its authority where it is mounted.
	if opts.Work != nil {
		if err := opts.Work.Routes(mux); err != nil {
			return nil, fmt.Errorf("api: mount the work surface: %w", err)
		}
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
	// which [httpjson.Mux] answers, and the redirect from `/`, which has an
	// HTML body) are covered without each needing to remember. A handler
	// serving a page replaces the policy with its own.
	//
	// And the drain gate sits inside all three, next to the routes it
	// refuses: see [App.drainGate].
	a.cors = auth.NewCORS(opts.Bootstrap)
	// THE ORDER IS THE ARGUMENT. Outermost is the page policy, so a
	// response no handler thought about still carries its headers. Then
	// the CANONICAL-PATH refusal, before anything reads the path: the
	// guard decides from `r.URL.Path` which routes are exempt, and a path
	// like `/webhooks/../config` reads to it as an exempt prefix and to
	// ServeMux as another route entirely — the mux answered it with a
	// redirect, so nothing was served, but the guard and the router were
	// deciding about two different paths and only the redirect kept that
	// harmless. Then CORS, which must answer a preflight before the guard
	// sees it — a browser attaches no credential to one. Then the guard,
	// which resolves the principal. Then the CSRF check, INSIDE the guard
	// because the credential's SHAPE is what decides whether a missing
	// Origin is a refusal, and nothing before the guard knows which shape
	// arrived. Innermost, the mux's OWN 404 and 405 are answered in the
	// engine's envelope ([httpjson.Mux]), like every other answer this
	// surface writes: a client reads a refusal with no code as something in
	// front of the node, so a route this node simply does not serve — a
	// work purge on a node serving no human write surface, any route a
	// newer client asks an older node for — read as a write whose outcome
	// was unknown.
	a.handler = pagepolicy.Apply(authz.CanonicalPath(
		a.cors.Middleware(a.guard.Middleware(a.csrf.Middleware(
			a.drainGate(httpjson.Mux(mux)))))),
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
		{"Inbox", o.Inbox == nil},
		{"Credentials", o.Credentials == nil},
		{"Sources.Company", o.Sources.Company == nil},
		{"EventLog", o.EventLog == nil},
		{"Sources.Events", o.Sources.Events == nil},
		{"Sources.NodeID", strings.TrimSpace(o.Sources.NodeID) == ""},
		{"Sources.Chart", o.Sources.Chart == nil},
		{"Sources.Holders", o.Sources.Holders == nil},
		{"Sources.WithheldContacts", o.Sources.WithheldContacts == nil},
		{"Sources.Coord", o.Sources.Coord == nil},
		{"Inbound.Publisher", o.Inbound.Publisher == nil},
		{"Inbound.Claims", o.Inbound.Claims == nil},
		{"Inbound.Secrets", o.Inbound.Secrets == nil},
		{"Inbound.AppFlow", o.Inbound.AppFlow == nil},
		{"Config", o.Config == nil},
		{"Secrets", o.Secrets == nil},
		{"Setup", o.Setup == nil},
		{"Chart", o.Chart == nil},
		{"Retention", o.Retention == nil},
		{"Capacity", o.Capacity == nil},
		{"Backup", o.Backup == nil},
		{"Nodes", o.Nodes == nil},
		{"AuthEvents", o.AuthEvents == nil},
		{"Audit", o.Audit == nil},
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

// mountDeployment registers the routes that operate the DEPLOYMENT rather
// than the company: the backup, the retention gestures and the capacity
// window. Every one takes [iam.GrantFleetOperate], through the
// authority table's [authz.ActionFleetOperate] for a write and
// [authz.ActionFleetRead] for a read, stated where it is mounted.
//
// # They decided nothing, and the guard in front of them decides nothing either
//
// Each handler resolved its CALLER — for the audit line — and asked no grant,
// because each was written while the only credential this engine had was an
// operator token and "somebody resolved" meant "the operator". The guard in
// front of every route answers the same question and no other: is there a
// principal. So once a person could sign in holding `state:read` alone, or a
// pipeline hold `work:write`, every one of them could clear a spend ceiling,
// copy the node's whole durable state to a directory of their choosing,
// move the floor the trim deletes against, and evict a machine from the
// fleet. Through [authz.Router] each route is refused `403` naming the
// grant, and a route mounted without a policy is a boot failure rather than
// an open door.
//
// THE READS TOO — the maintenance status and the reanchor confirmation value
// — because `/work/retention*` is the deployment's surface whole, which is
// what the `retention` question already declares: a map of which machine
// holds what, and the value a reanchor must echo, are not the company's
// working state.
//
// # A write asks for a recent proof of identity, and a read does not
//
// The two verbs carry the same grant and differ in the step-up alone: every
// write here changes what the deployment does for everybody on it, so it asks
// for a proof inside `api.auth.session.step_up`, while the two reads — the
// maintenance window's status and the value a reanchor must echo — change
// nothing, and a status an operator cannot see without re-proving who they
// are is one they stop checking. The METHOD picks the verb, because that is
// the one fact about a route this surface already treats as its read/write
// line (the session table's columns are keyed on it too), and a second list
// of which patterns read would be the thing that drifts.
func (a *App) mountDeployment(mux *http.ServeMux) error {
	router := authz.NewRouter(mux, authz.ContextGuard(authz.NoChart{}))
	var failures []error
	a.deploymentRoutes(func(pattern string, h http.HandlerFunc) {
		if err := router.Handle(pattern, deploymentPolicy(pattern), h); err != nil {
			failures = append(failures, err)
		}
	})
	return errors.Join(failures...)
}

// deploymentRoutes names every route [App.mountDeployment] mounts, through the
// mount it is handed: separate from the router, so a walk can read the whole
// list without standing the deployment up.
func (a *App) deploymentRoutes(mount func(string, http.HandlerFunc)) {
	// A POST, and a read posture of any kind never opens it: copying every
	// credential and every seat's memory to a path the caller names is not
	// a read. There is no budget reset beside it — a window's turnover IS
	// the reset (ADR-0019), and a raw reset would re-arm a company mid-window.
	mount("POST /backup", a.serveBackup)
	// The three retention gestures that write. See retention.go.
	a.mountRetention(mount)
	// The capacity window's own control surface. It is the one thing a
	// maintenance-mode node serves that a publishing one does not need,
	// and it is why the verb can run at all on a topology whose broker
	// binds no socket. See retention.go.
	a.mountCapacity(mount)
}

// deploymentPolicy is the verb one deployment route is decided under: a read
// is [authz.ActionFleetRead] and everything else [authz.ActionFleetOperate].
// See [App.mountDeployment] for why the method is what picks it.
func deploymentPolicy(pattern string) authz.Policy {
	if strings.HasPrefix(pattern, http.MethodGet+" ") {
		return authz.Policy{Action: authz.ActionFleetRead}
	}
	return authz.Policy{Action: authz.ActionFleetOperate}
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
func (a *App) mountWebhooks(mux *http.ServeMux, in Inbound, events *store.EventLog, now func() time.Time) error {
	receiver, err := webhooks.New(webhooks.Options{
		Secrets:    in.Secrets,
		Publisher:  in.Publisher,
		Claims:     in.Claims,
		Keys:       in.Keys,
		AppFlow:    in.AppFlow,
		Recheck:    in.Recheck,
		Events:     events,
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

// InboxFeed is where this node hears whose inbox moved: the engine, whose
// tracker applier says so after every committed batch.
//
// CONSUMER-DEFINED and one method wide. The engine satisfies it with
// [engine.Engine.SetOnInboxMoved]; a test hands in a feed it fires by hand.
type InboxFeed interface {
	SetOnInboxMoved(func([]tracker.InboxMovement))
}

// pushInboxMoved turns one committed batch's inbox movements into
// `inbox_changed` frames, each reaching only the sockets watching that seat.
//
// NO FORWARDING BETWEEN NODES, and none is missing: every node applies every
// tracker record, so every node hears every movement from its own applier and
// pushes to the sockets it holds.
func (a *App) pushInboxMoved(moved []tracker.InboxMovement) {
	for _, m := range moved {
		a.stream.InboxChanged(stream.InboxChange{
			Handle: m.Handle, UnreadDelta: m.UnreadDelta,
			Subject: m.Subject, Reason: string(m.Reason),
		})
	}
}

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
	if _, diverged := divergedPostures[h.NodeStatus()]; diverged {
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
func (a *App) answer(ctx context.Context, what string, params map[string]any) (any, error) {
	data, err := a.queries.Answer(ctx, what, params)
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
		//
		// AND A REFUSAL ON AUTHORITY SAYS WHY, as it does over REST: the
		// rule's reason and the grants that would have admitted the
		// caller travel on the frame. It was reduced to the question's
		// name here, so the one channel the dashboard reads was the one
		// on which "you may not" could not say what would change that.
		var refusal *queries.Refusal
		if errors.As(err, &refusal) {
			return nil, &stream.RefusedError{What: what,
				Refused: *stream.NewRefused(refusal.Reason, refusal.Grants)}
		}
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
		// and the frame carries it as `detail`. The others are
		// deliberately reduced to the query name — a failure's own text
		// can carry a database path, and none of them has a reader.
		return nil, &stream.BadParamsError{What: what, Detail: queries.RefusalDetail(err)}
	case errors.Is(err, queries.ErrUnavailable):
		// THE CAUSE IS KEPT, and only its STRUCTURE reaches the frame:
		// [stream.UnavailableOf] reads the state log's refusal and its
		// hint off it and never renders the error's text. Reduced to the
		// question's name, a node that will refuse this read until an
		// operator acts and one catching up were the same `unavailable`,
		// and the dashboard polled both.
		return nil, fmt.Errorf("%w: %s: %w", stream.ErrUnavailable, what, err)
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
	// Params come from the query string, read through the same accessors a
	// socket frame's JSON object goes through — which is what stops a
	// filter being honoured on one transport and ignored on the other.
	data, err := a.queries.AnswerWith(r.Context(), what,
		queries.FromQuery(r.URL.Query()))
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
//
// EVERY ARM IS THE ENVELOPE — `error`, and the `message` a person reads — as
// every other refusal on this API is. They were bare `{"error": code}`
// objects, so the one family of routes a dashboard reads most often was the
// one whose refusals carried no sentence, and the code a client branched on
// was the only thing it had to show.
func writeQueryError(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, queries.ErrUnknown):
		httpjson.Fail(w, http.StatusNotFound, httpjson.CodeUnknownQuery)
	case errors.Is(err, queries.ErrUnauthenticated):
		// 401: nobody is asking, so the remedy IS to present something —
		// and the code is the one the guard gives the same caller, so a
		// client never sees one absence of a credential two ways.
		// Unreachable through the wired guard, which answers this a layer
		// up — stated because the mapping is this function's to get right
		// whatever happens to run in front of it.
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
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
		//
		// AND IT SAYS WHY: the rule's reason and the grants that would
		// have admitted the caller, carried on the registry's refusal
		// rather than rewritten here — the same two fields every other
		// refusal on authority answers with.
		var refusal *queries.Refusal
		if !errors.As(err, &refusal) {
			httpjson.Fail(w, http.StatusForbidden, httpjson.CodeUnauthorized)
			return
		}
		httpjson.FailWithFields(w, http.StatusForbidden, httpjson.CodeUnauthorized,
			authz.RefusalDetail(refusal.Reason, refusal.Grants))
	case errors.Is(err, queries.ErrBadParams):
		// 400 AND ITS OWN CODE. The status was already right; the code
		// said `query_failed`, which names a fault of this node for a
		// request the caller has to change. AND ITS SENTENCE, as the
		// socket's frame carries it: the refusal names the parameter to
		// change, which is the whole of the fix, and the envelope's
		// `message` cannot say which one.
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeBadParams,
			httpjson.Detail{"detail": queries.RefusalDetail(err)})
	case errors.Is(err, queries.ErrNotFound):
		httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound)
	case errors.Is(err, stream.ErrNoCompany):
		// A NATIVE QUESTION ON A NODE WITH NO COMPANY YET, answered as
		// every surface answers that node — the webhook edge, the human
		// write surface, the operator's assistant — with the reconcile
		// poll as the wait, rather than as the state log's `unavailable`,
		// whose hint is the scale of a node catching up. See native.go.
		//
		// THE HINT AND THE WORDS ARE [stream.UnavailableOf]'s, the one
		// reading the socket's frame is built from too: decided here
		// alone, the REST route said fifteen seconds and why while the
		// socket said five and nothing.
		u := stream.UnavailableOf(err)
		httpjson.UnavailableWith(w, httpjson.CodeNoActiveRevision, u.RetryAfter,
			httpjson.Detail{"detail": u.Detail})
	case errors.Is(err, queries.ErrUnavailable):
		// 503, because this node could not serve the question and nothing
		// about the request was wrong: it is behind the log and draining,
		// its coordination store was briefly unreachable, or its state log
		// refused the read. A 500 would tell a client the server broke —
		// which is what a refusal waiting cannot clear used to be answered
		// with, its remedy sent only to the log — and an empty 200 would
		// tell a person the company has no work.
		//
		// THE HINT IS THE REFUSAL'S OWN where it has one — derived from
		// how far behind this node is over how fast it is actually
		// draining — ZERO, and so no Retry-After, for a refusal waiting
		// cannot clear (a full log, a record this node cannot decode, a
		// refusal the broker named), and five seconds otherwise, by
		// [statelog.RetryAfter]'s rule, which every surface answering a
		// refusal reads. A flat hint is wrong in both directions on one
		// fleet. The fallback is what an unreachable coordination store
		// gets, since there is no drain to derive from, and it is the
		// shared health tick's own cadence ([stream.HealthInterval]): a
		// client that waits it out asks again having seen at most one
		// newer health frame, which is the soonest it could learn the
		// store is back.
		//
		// AND THE REFUSAL SAYS WHAT IT IS — its code and its words, the
		// remedy — through [stream.UnavailableOf], the same reading the
		// socket's error frame is built from.
		u := stream.UnavailableOf(err)
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, u.RetryAfter, u.Fields())
	default:
		// The reason reaches the LOG, not the caller: it can carry a
		// database path or a driver's own message, and nothing about
		// holding a grant makes a caller somebody that path is for.
		log.Warn("api_query_failed", "what", what, "error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeQueryFailed)
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
