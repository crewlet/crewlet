package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/assistantapi"
	"github.com/crewlet/crewlet/internal/api/auth"
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
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/worklet"
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

	state  *livestate.LiveState
	stream *stream.Service

	// stopRooms unregisters the socket's rooms from the appliers they hear,
	// before the service stops.
	stopRooms []func()

	// chatLive is the process's chat hub, which the socket carries chat
	// through. The App's to mount and never to start or stop.
	chatLive *stream.ChatHub

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
	// objects moves the company's files to an S3 bucket (see objectsmigrate.go).
	objects objectsMigrator

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

	// fleetBroker reads and changes the fleet broker's membership. See
	// fleetbroker.go.
	fleetBroker FleetBrokerControl

	// company reads the engine's CURRENT epoch, which is what
	// [App.Configured] asks.
	company func() (*config.Company, *org.Organization)

	// estateFloor answers whether the replicated estate can be read at the
	// log's floor, for /ready. See [EstateFloor].
	//
	// Named for its type and never `estate`: internal/store's applier
	// gate matches a call by METHOD NAME across the tree (it parses, it
	// does not type-check), and `estate()` is the engine's accessor for
	// the replicated estate's write handle — so a call through a field of
	// that name reads to the gate as a write to the estate.
	estateFloor EstateFloor

	// identity answers whether anybody is enrolled, for /health. See
	// [Identity].
	identity Identity

	handler http.Handler

	// public is the handler of the dedicated public listener, or nil when
	// there is none and every route is [App.ServeHTTP]'s. See [App.Public].
	public http.Handler

	// routes is every pattern mounted, in mount order. See [routeTable].
	routes []string
	// policies are the guarded routes' policies. See [routeTable].
	policies map[string]authz.Policy
}

// routeTable is the one mux every surface mounts on, keeping the pattern of
// each route it is handed.
//
// KEPT because the listeners split ONE table by a predicate over paths
// ([auth.Public]), and a predicate cannot say which paths exist: a route
// mounted under a public prefix is published and unguarded whatever it was
// meant to be, and one an outside party calls mounted outside them is
// unreachable the day a deployment sets api.public. Only the table can be
// walked to catch either, and [net/http.ServeMux] does not list its own.
//
// EVERY MOUNT SIGNATURE IS ITS: [authz.Mux], which a guarded surface states
// each route's policy through, [auth.Mux], which /auth registers on, and
// [httpjson.Router] — so a route mounted through any of them is in the table.
type routeTable struct {
	mux      *http.ServeMux
	patterns []string
	// policies are the guarded routes' verbs, each reported by the
	// [authz.Router] that mounted it ([authz.PolicyMux]), so a walk can
	// classify a guarded route by the verb it is decided by.
	policies map[string]authz.Policy
}

// MountedWith keeps the policy a guarded route was mounted with.
func (t *routeTable) MountedWith(pattern string, p authz.Policy) {
	if t.policies == nil {
		t.policies = map[string]authz.Policy{}
	}
	t.policies[pattern] = p
}

func (t *routeTable) Handle(pattern string, handler http.Handler) {
	t.patterns = append(t.patterns, pattern)
	t.mux.Handle(pattern, handler)
}

func (t *routeTable) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	t.Handle(pattern, http.HandlerFunc(handler))
}

// EstateFloor reports whether this node can answer from the company's
// replicated estate at all, and names the term that says it cannot.
//
// # Why /ready turns on it
//
// A node's SQL copy is derived from an ordered log, and the log is trimmed. A
// consumer whose position falls below the stream's first sequence is clamped
// UPWARD with no error at all and then reports itself caught up over a hole —
// so a copy below the floor answers every question confidently, out of rows
// with records missing from them for good. "There is no such work item" is how
// a duplicate gets filed.
//
// WHICH IS WHY SUCH A COPY ANSWERS NOTHING, and why the node holding it is
// still sent traffic: a copy that is wrong — below the log, its applier
// halted, its node evicted — goes OUT OF SERVICE, and every read and write the
// node takes for the tracker and the knowledge base is routed to a data node
// whose copy is sound. What no copy of its own makes up for is a node that can
// no longer say WHERE to route: its own copy out of service and its view of
// the fleet too stale to name another. That is the question the engine asks
// before letting the seats a node holds STAY (`engine.Engine.SeatsServiceable`)
// — and `crewlet run` hands this seam exactly that, so the node that gives its
// seats back is the node that stops being sent work, and the two never
// disagree about one copy.
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
// A fresh install has nobody in it, and the way the first person gets in is
// onto a human seat the company declares — every person holds one for as long
// as they exist (ADR-0032): an invitation into it, or a person an
// administrator creates on it, either issued under a Tier A token
// (`crewlet iam invite -seat`, `crewlet iam create -seat`, or the seat's
// Invite and Create in the org chart). An operator who has just started a
// node, and a dashboard whose sign-in form nobody can use yet, both need to
// know that this is the state they are in rather than a sign-in that is
// failing — and /health is the one surface an install with nobody in it can
// reach: it is a probe, exempt from the guard, and before the first person
// there is no credential but the deployment's own to present anywhere else. It
// is one EXISTS over the node's own rows, and one read of the identity log's
// end beside it, neither of which a probe notices.
//
// SOMEBODY EXISTS, NOT SOMEBODY CAN SIGN IN: a person an administrator created
// is a row the moment the create lands, before they have spent the first
// password link it handed back, so `identity: ready` there says the company
// has its first person, who may not have chosen a password yet.
//
// A CONTEXT and a live read, for [EstateFloor]'s reason: the answer changes
// the moment the first person lands, and a cached one would go on telling an
// operator to invite somebody who has already signed in.
//
// THREE-VALUED: an error is this node unable to read its identity estate, or
// holding rows that have not applied the whole identity log — every node that
// serves this API holds the estate and applies the identity log from boot
// (ingress requires the `data` role, ADR-0025), so a node that has just joined
// a fleet holds none for a moment — which is `unknown` and never `unclaimed`:
// told there is nobody, a dashboard would offer an empty company's guidance to
// one that has started.
type Identity func(ctx context.Context) (enrolled bool, err error)

// authMounter is the /auth surface's own mount, over a mux it can NAME.
//
// The API's [routeTable] satisfies it, as [http.ServeMux] does. The narrower
// type exists so internal/api/authapi's own gate can walk what the surface
// registered and hold it against internal/api/auth's exemption list — the
// standard mux reports nothing about what was mounted on it, and the failure
// when those two drift is a credential surface behind no credential.
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
// meant "anybody" the day a person could sign in at all. A
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
// Runtime, Inbox, Credentials, Sources.Company, Sources.Chart, Sources.Holders,
// Sources.WithheldContacts, Sources.Events, Sources.NodeID, Sources.Coord, the
// Inbound edge's Publisher, Claims, Secrets and AppFlow, Config, Secrets,
// Setup, Retention, Capacity, FleetBroker, Backup, Nodes, AuthEvents and Audit
// are REQUIRED, and [New] refuses a missing one by name.
//
// Every one of them is something the engine beside the API holds: `crewlet
// run` is the only thing that builds an App, it builds one over an engine that
// holds all of these, and a node that serves no API (api.port 0) builds none at
// all. So a nil here is a wiring mistake, and a narrower answer built around it
// (a health body missing the engine's fields, a 503, an unregistered /config)
// would hide that mistake behind something an operator reads as deliberate.
//
// What stays optional is what a real node can lack: Worklet (a company
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
	// bound by one record on the directory subject so two holders cannot
	// hold one seat.
	//
	// THE ZERO VALUE BINDS NOTHING, which is what an API stood up in a
	// suite has: every credential then acts as itself. `crewlet run`
	// always wires both halves over its engine.
	SeatBindings auth.SeatBindings

	// Sessions turns a browser's cookie into the person holding it.
	//
	// OPTIONAL, and nil is what a suite about something else has: Tier A
	// tokens are then the whole of authentication. `crewlet run` always
	// wires it, beside [Options.Auth], on every node serving the API —
	// each holds the estate, since ingress requires the `data` role
	// (ADR-0025), and the identity estate is the core every data node
	// starts at boot, company or none — so a node with no company serves
	// the session a Tier A token exchanges for, which is how its company
	// is created, and signs its people in once that company has any.
	Sessions *auth.Sessions

	// Tokens turns a machine token — a person's own access token or a
	// service account's, minted at `/iam/credentials` — into its owner.
	// See [auth.Tokens].
	//
	// NIL IS AN API THAT RESOLVES NO MACHINE TOKEN, which is what a suite
	// has. `crewlet run` ALWAYS builds one over its engine's directory,
	// which every node serving the API holds from boot — ingress requires
	// the `data` role (ADR-0025).
	Tokens *auth.Tokens

	// --- the Dashboard Assistant's grants (WP1) ---

	// Delegations mints and checks the assistant's grants, and Dispatcher
	// sends the assistant's tool calls through this API's own handler with
	// them (ADR-0050). The two are ONE PAIR (the dispatcher sends what the
	// delegations mint), and New binds the delegations to this API's guard
	// and the dispatcher to this API's handler, so a grant is accepted only
	// by the chain it was minted for.
	//
	// NIL IS AN API WITH NO ASSISTANT: every grant-shaped bearer is refused,
	// over the wire and in process alike.
	Delegations *auth.Delegations
	Dispatcher  *auth.Dispatcher

	// --- the Dashboard Assistant's routes (WP8) ---

	// Assistant is the Dashboard Assistant's routes under /assistant/ and
	// the `assistant` member of `viewer` (internal/api/assistantapi). It
	// needs the grant pair above: a turn's every call carries a grant the
	// delegations mint and the dispatcher sends, so an Assistant without
	// them is refused at New.
	//
	// NIL IS AN API WITH NO ASSISTANT: no route under /assistant/, and the
	// viewer answers it `disabled`.
	Assistant *assistantapi.Surface

	// AuthEvents is where the guard counts a refused credential.
	// REQUIRED: the engine
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

	// Bootstrap supplies the auth posture, and whether the public routes
	// have a listener of their own (api.public — see [App.Public]). Nil is
	// permitted and is not the same as absent config: the guard then
	// refuses every write, because nobody has said who may make one, and
	// every route is served on the one listener.
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
	// tracker's log — and relays a peer's movements while its own copy is
	// out of service — so a node that heard nothing would be a wiring
	// mistake rather than a narrower node, and the dashboard would go back
	// to learning about somebody's work a poll interval late.
	Inbox InboxFeed

	// Credentials is where this node hears whose credentials a committed
	// identity batch moved, which [New] turns into a fresh decision of
	// every open socket opened with one — see [CredentialFeed].
	//
	// REQUIRED, for Inbox's reason: every node that serves the API holds
	// the estate and applies the identity log, and a socket authenticated
	// at its handshake is
	// ended by the record that ends its credential — a node that heard
	// nothing would leave every revoked session's open tab receiving the
	// company's state until it closed on its own.
	Credentials CredentialFeed

	// ChatLive is the company's own chat on this node's sockets: the
	// process's chat hub ([NewEngineChatLive]), whose committed frames, presence
	// and read-state pushes the socket carries for each viewer the room
	// admits. [New] mounts the socket over it and says so to it
	// ([stream.ChatHub.AttachSockets]) — which is what makes it follow a
	// peer's batches while this node's own copy is out of service, and
	// what starts its read-state push — and neither starts nor stops it:
	// the hub is the PROCESS's, attached to the engine on every node
	// whether or not that node serves an API.
	//
	// REQUIRED, for Inbox's reason: a hub exists on every node, so a nil
	// one here is a wiring mistake, and the screen it costs — a room that
	// never updates while a person is talking in it — looks exactly like
	// a quiet company. A company whose chat is not the engine's simply
	// gives it nothing to say.
	ChatLive *stream.ChatHub

	// State is the projection to serve. Nil builds an empty one on
	// [Options.Now].
	//
	// The projection ages and labels the live spend window on a clock of
	// its own ([livestate.WithClock]), and the live `tokens` answer's
	// window is that clock's rather than Now's — so an embedder that
	// supplies a projection and pins Now pins the projection to the same
	// clock, or the one surface answers on two.
	State *livestate.LiveState

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
	// knowledge backend, no Worklet) and distinct from an empty one.
	Sources queries.Sources

	// QueueBackend names the broker, for the health body.
	QueueBackend string

	// Now is injectable so a test can pin the timestamps. It is the
	// questions' clock too unless [queries.Sources.Now] pins one of its own,
	// and the clock of the projection a nil [Options.State] builds.
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
	// `crewlet run` mounts one on every node serving the API, since each
	// holds the identity directory from boot, company or none — ingress
	// requires the `data` role (ADR-0025) — and it used to be absent on a
	// node that had met no company, which was the node its first person
	// had to sign in on. An API with none serves no /auth at all
	// rather than one that answers 503 to every attempt. ABSENT MEANS A
	// NIL INTERFACE, never a nil *authapi.Service inside one, which [New]
	// cannot tell from a surface that is there — [HumanSurfaces.Mount] is
	// the one place the conversion happens.
	Auth authMounter

	// IAM serves /iam, normally an iamapi.Service: the company's identity
	// directory.
	//
	// OPTIONAL, and nil — the routes ABSENT — is what a suite about
	// something else has. `crewlet run` always mounts it: every node
	// serving the API holds the directory from boot (ingress requires the
	// `data` role, ADR-0025), and a node with nobody in its company is
	// exactly where the first person is invited.
	IAM guardedMounter

	// Work serves the human write surface over Worklet
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

	// Artifacts serves /artifacts on this listener, normally an
	// artifactsapi.Service: uploads, publishes, shares, trash and purge,
	// view tickets and the raw bytes of a version's files, each route
	// stating its authority where it is mounted. Nil serves none, which is
	// what a node built without the artifacts domain has. The artifact
	// BYTES are never rendered here: they are served by the separate
	// api.artifacts listener, inside a sandboxed frame.
	Artifacts guardedMounter

	// ArtifactData serves an artifact's datastore under
	// /artifacts/{id}/data, normally an artifactdataapi.Service. Nil serves
	// none.
	ArtifactData guardedMounter

	// Datasets is what a dashboard artifact's datasets are read from: the
	// artifacts surface's board reads and the datastore. The surface itself
	// (internal/api/datasetsapi) is built here, because an engine dataset
	// is answered by THIS app's question registry and its agents overview,
	// on the viewer's own request (adr/0043). Nil serves none.
	Datasets *DatasetsSource

	// Rooms decides and feeds the socket's artifact rooms: who may join one
	// (the datastore's own admission, asked with the read verb), where an
	// artifact's datastore stands, this node's presence hub, and what its
	// appliers commit. Nil serves no room, which is what a node built
	// without the artifacts domain has and what a suite about something
	// else leaves out; `crewlet run` always wires it.
	Rooms *Rooms

	// Objectives is the node's service objective meter, which every
	// committed change the socket pushes is timed into (change visibility,
	// [stream.Objectives]). Nil records nothing; `crewlet run` always wires
	// the engine's.
	Objectives stream.Objectives

	// Models serves the model routes under /llm/providers/ and
	// /llm/device-sign-ins/, normally an llmapi.Service: a model's
	// credential tested before it is stored, its checks, a retry of a
	// sit-out, a host login's status and a device sign-in. The reads beside
	// them are questions ([queries.Sources.Models]).
	Models guardedMounter

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
	// signed rather than stored. A node without the ingress role serves it
	// beside its probes, to its own host's boxes alone ([Probes]).
	OtelReceiver *sandbox.OtelReceiver

	// Bridge serves a running seat's tool surface to a coding agent over
	// MCP. Nil serves none, and the route is then ABSENT for the same
	// reason OtelReceiver's is.
	//
	// Unlike OtelReceiver it is the ENGINE's own: a session is a live tool
	// surface in the process holding the run's seat, and this bridge holds
	// this process's sessions and relays a call for any other seat to its
	// holder (internal/api/mcpbridge). A node without the ingress role
	// serves it beside its probes, to its own host's boxes alone
	// ([Probes]).
	Bridge *mcpbridge.Bridge

	// Operator is Worklet and Doclet as ONE tool
	// catalogue, served to the people who run it — over MCP for an
	// operator's own AI assistant, and over the act route for a person at
	// the dashboard (ADR-0024) — and the one dispatch the human write
	// surface's tool-backed routes call. Nil serves none and both routes are
	// ABSENT, which is what a suite about something else has.
	//
	// ONE SERVER, BUILT ONCE, WHOSE CATALOGUE IS READ PER CALL: the
	// catalogue is the native halves', which a node meets at its first
	// company, beside the seat controls every node serves. A request before
	// that answers `503 no_active_revision`. [HumanSurfaces.Mount] is what
	// hands it over.
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
	// such node here: the state log is the engine's core, started at boot
	// on every node that holds the estate, company or none, and the API is
	// served only on such a node — ingress requires the `data` role
	// (ADR-0025) — so every engine this API runs beside holds a gate, and a
	// nil is a wiring mistake, not a narrower node.
	Nodes NodeGate

	// Capacity drives a stream's byte ceiling.
	Capacity capacityRunner

	// FleetBroker reads and changes the fleet broker's membership — see
	// [FleetBrokerControl].
	FleetBroker FleetBrokerControl

	// Backup copies this node's durable state to a path an operator
	// names.
	Backup backupTaker

	// Objects moves the company's files from the fleet's object store to
	// an S3 bucket (`crewlet objects migrate --to s3`).
	Objects objectsMigrator

	// Identity says whether anybody is enrolled, for /health. See
	// [Identity].
	//
	// Optional, and nil is what a suite about something else has: with
	// nothing to ask about who is in the company, the health body leaves
	// the field out rather than calling the company unclaimed. `crewlet
	// run` always wires it, over the identity estate every data node runs
	// from boot — and only a data node serves the API (ingress requires
	// `data`, ADR-0025).
	Identity Identity

	// Estate answers whether this node can answer from the replicated
	// estate at all, for /ready. See [EstateFloor].
	//
	// Optional, and nil is what a suite about something else has: an API
	// with no estate to judge is ready on its other terms. `crewlet run`
	// always wires it ([engine.Engine.SeatsServiceable]), which itself
	// answers ready for a company whose tracker and knowledge base are
	// both a vendor's — there is nothing of theirs to route. What nil must
	// NOT be read as is "assume the worst": a node with nothing to judge
	// would then never be ready.
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
		// ON THE APP'S CLOCK: the live spend window is labelled by the
		// projection's, and one on the wall clock beside a pinned Now
		// would answer the surface's questions on two clocks.
		state = livestate.New(livestate.WithClock(now))
	}

	a := &App{
		guard: auth.New(opts.Bootstrap).BindSeats(opts.SeatBindings).
			WithDevPrincipal(opts.DevPrincipal).WithSessions(opts.Sessions).
			WithTokens(opts.Tokens).
			WithAudit(opts.AuthEvents),
		csrf:         auth.NewCSRF(opts.Bootstrap),
		state:        state,
		runtime:      opts.Runtime,
		nodeID:       opts.Sources.NodeID,
		queueBackend: opts.QueueBackend,
		now:          now,
		// DERIVED FROM THE SOURCE THAT ALREADY EXISTS, rather than a
		// second field an embedder could set inconsistently with it:
		// Sources.Company reads the CURRENT epoch, and "is there one" is
		// the whole question [App.Configured] asks.
		company:     opts.Sources.Company,
		estateFloor: opts.Estate,
		identity:    opts.Identity,
	}
	var err error
	// --- the Dashboard Assistant's grants (WP1) ---
	if opts.Dispatcher != nil && opts.Dispatcher.Delegations() != opts.Delegations {
		return nil, errors.New("api: Options.Dispatcher sends grants that " +
			"Options.Delegations did not mint; build it with auth.NewDispatcher " +
			"over the same delegations")
	}
	if err = a.guard.BindDelegations(opts.Delegations); err != nil {
		return nil, fmt.Errorf("api: %w", err)
	}
	a.stream, err = stream.NewService(state, stream.Options{
		Health: a.streamHealth,
		// The SAME set /ready refuses on, read from the one place it is
		// declared — see [framePosture].
		Posture: framePosture,
		// Read through the SOURCES rather than captured, for the same
		// reason every other read here is: a config apply replaces the
		// company, and a directory captured at boot would keep linking a
		// removed seat and miss every seat added since.
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
		// AND WHAT THE PUBLISHED COMPANY SAYS ABOUT A SEAT, which an open
		// socket acting as one is decided by when a company is published.
		SeatOf: func(name string) (stream.SeatState, bool) {
			return seatOf(opts.Sources.Company, name)
		},

		// THE ARTIFACT ROOMS, every half or none ([Rooms]).
		Rooms:   opts.Rooms.streamRooms(),
		Metrics: opts.Rooms.metrics(),
		// THE NODE'S OBJECTIVE METER, whatever the company keeps: every
		// committed change the socket pushes is timed into it, an inbox
		// movement as much as a room's, and a meter carried by the rooms
		// left the inbox untimed on every node that serves none.
		Objectives: opts.Objectives,

		Now:            now,
		HealthInterval: opts.HealthInterval,
	})
	if err != nil {
		return nil, fmt.Errorf("api: %w", err)
	}
	// AND WHAT EACH ARTIFACT'S APPLIERS COMMITTED, pushed to the sockets in
	// its room: this node's own appliers while its copy serves and a sound
	// peer's, relayed, while it does not, with every room told to re-read at
	// each switch (the engine's live relay).
	if r := opts.Rooms; r != nil && r.Feed != nil {
		a.stopRooms = append(a.stopRooms,
			r.Feed.OnArtifactsMoved(a.stream.ArtifactsMoved),
			r.Feed.OnArtifactDataMoved(a.stream.ArtifactDataMoved))
	}
	// AND WHOSE INBOX MOVED, pushed to the sockets watching each seat —
	// and when what this node hears has a gap, every watched inbox read
	// again.
	opts.Inbox.SetOnInboxMoved(a.pushInboxMoved)
	opts.Inbox.SetOnInboxGap(a.pushInboxGap)
	// AND WHOSE CREDENTIALS MOVED, deciding the sockets opened with them
	// again.
	opts.Credentials.SetOnIdentityMoved(a.pushIdentityMoved)
	// AND THE COMPANY'S OWN CHAT, on the socket mounted below — told so
	// once this app is built, not before: a build that failed past here
	// serves no socket, and a hub told otherwise would follow a peer's
	// batches for nobody.
	a.chatLive = opts.ChatLive

	tree := opts.Assets
	if tree == nil {
		tree = static.FS()
	}
	policy, err := dashboardPolicy(opts.Bootstrap)
	if err != nil {
		return nil, err
	}
	files := newAssets(tree, policy)

	// ONE registry, and both transports go through it. Two surfaces
	// answering one question from two implementations is how they end up
	// disagreeing with nobody noticing.
	sources := opts.Sources
	if sources.State == nil {
		sources.State = state
	}
	// THE BROKER'S MEMBERSHIP IS READ THROUGH THE SAME SEAM ITS REMOVAL IS
	// MADE THROUGH, so one wiring serves the question and the gesture.
	if sources.FleetBroker == nil {
		sources.FleetBroker = opts.FleetBroker
	}
	// ONE CLOCK PER APP. The questions read [queries.Sources.Now] while the
	// stream service and the webhook edge read [Options.Now], so a caller
	// that pinned the app's clock and not the sources' had its pushes and
	// deliveries at the pinned instant and its answers on the wall clock — a
	// question stamping "now" on its answer answered a REST call and a socket
	// call a second apart with two different instants, and a test that pinned
	// the app to compare one question across both transports compared two
	// wall-clock readings instead. A caller that pinned the sources' own clock
	// keeps it. (The live spend window is labelled by the projection, on the
	// clock it was aged on — which a nil State builds on this one: see
	// [Options.State].)
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
	// --- the Dashboard Assistant's routes (WP8) ---
	// WHETHER THE ASK PILL IS LIT, from the surface whose routes the pill
	// opens, so the viewer and a refused question cannot disagree.
	if sources.Assistant == nil && opts.Assistant != nil {
		sources.Assistant = opts.Assistant.Viewer
	}
	// WHO MAY CHANGE THE COMPANY DOCUMENT, from the same Tier A the config
	// surface enforces it from, so the viewer offers exactly the edits that
	// surface admits (ADR-0030). A caller that supplied its own keeps it.
	if sources.CompanyWriters == nil && opts.Bootstrap != nil {
		sources.CompanyWriters = slices.Clone(opts.Bootstrap.API.Auth.CompanyWriters)
	}
	// HOW A PERSON SIGNS IN HERE, from this node's own Tier A, through the
	// accessors the sign-in path enforces it by. A caller that supplied its
	// own keeps it.
	if sources.SignIn == nil && opts.Bootstrap != nil {
		policy := queries.SignInPolicyOf(opts.Bootstrap.API.Auth)
		sources.SignIn = &policy
	}
	a.queries = queries.NewRegistry()
	queries.Register(a.queries, sources)
	a.backup, a.audit = opts.Backup, opts.Audit
	a.objects = opts.Objects
	a.retention, a.nodes = opts.Retention, opts.Nodes
	a.capacity = opts.Capacity
	a.fleetBroker = opts.FleetBroker

	// ONE TABLE, keeping every pattern it is handed: see [routeTable].
	mux := &routeTable{mux: http.NewServeMux()}
	mux.Handle("GET /health", http.HandlerFunc(a.serveHealth))
	mux.Handle("GET /ready", http.HandlerFunc(a.serveReady))
	mux.Handle("GET /query/{what}", http.HandlerFunc(a.serveQuery))
	// The NAMED read routes — the public REST API. Adapters over the same
	// registry the generic form above reaches; see rest.go.
	a.mountReads(mux)
	// THE DEPLOYMENT'S OWN CONTROLS — the backup, the retention gestures,
	// the capacity window and the fleet broker's membership — each mounted
	// with the grant it takes, and failing the boot where a policy is
	// missing.
	if err := a.mountDeployment(mux); err != nil {
		return nil, fmt.Errorf("api: mount the deployment's controls: %w", err)
	}
	mux.Handle(auth.SocketPath, stream.Handler(a.guard, a.csrf, a.stream, a.answer, a.chatLive))
	// The OPERATOR surface: the same tracker and knowledge tools a seat
	// holds, offered to a person's own assistant over MCP and to the person
	// themself over the act route. Under its own
	// prefix rather than under /mcp/, which is exempt from the guard
	// wholesale for the sandbox bridge — see operator.MCPPath.
	a.mountOperator(mux, opts.Operator)
	// --- the Dashboard Assistant's routes (WP8) ---
	// THE ASSISTANT, every route on one of its two verbs. Its grants are
	// refused under this prefix whatever they carry (auth.AssistantPrefix),
	// so it can never be reached through itself.
	if opts.Assistant != nil {
		if opts.Delegations == nil || opts.Dispatcher == nil {
			return nil, errors.New("api: Options.Assistant needs Options.Delegations and " +
				"Options.Dispatcher: every call a turn makes carries a grant they mint " +
				"and send")
		}
		if err := opts.Assistant.Routes(mux); err != nil {
			return nil, fmt.Errorf("api: mount the assistant surface: %w", err)
		}
	}
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
	if err := a.mountWebhooks(mux, opts.Inbound, now); err != nil {
		return nil, err
	}
	// The SANDBOX TELEMETRY edge, exempt by the same prefix rule and for
	// the same reason: the exporter inside a box holds no API token, and
	// giving it one would hand a sandbox the credential that reads the
	// whole company. Its per-run token is in the path instead.
	mountOTLP(mux, opts.OtelReceiver)
	mountBridge(mux, opts.Bridge)
	// The config surface, every route on a grant, reads included:
	// reading it exposes the whole company document and writing it
	// changes the company.
	//
	// These three, like the chart below, fail the boot on a route mounted
	// with no policy — see [guardedMounter] for what they decided before.
	//
	// A REFUSED WRITE to the config, the secrets or a model logs one line
	// ([refusalMux]), under the surface's own logger; setup is the
	// first-run wizard's and logs its own outcomes.
	for _, surface := range []struct {
		name string
		m    guardedMounter
		log  *slog.Logger
	}{
		{"config", opts.Config, logging.Get("api.config")},
		// /secrets is how a rotation reaches a fleet at all — the
		// coordination broker is inside the engine's process on the
		// default topology, so no second process can write the store —
		// and its listing alone says which credentials a company holds.
		{"secrets", opts.Secrets, logging.Get("api.secrets")},
		// Connecting an integration without a shell, reads included —
		// the list of which credentials a company has NOT configured is
		// worth as much to an attacker as the ones it has.
		{"setup", opts.Setup, nil},
		// A MODEL'S CREDENTIAL, CHECKS AND SIGN-INS, each route on its
		// own verb (internal/api/llmapi).
		{"models", opts.Models, logging.Get("api.llm")},
	} {
		var mounted authz.Mux = mux
		if surface.log != nil {
			mounted = refusalMux{Mux: mux, log: surface.log}
		}
		if err := surface.m.Routes(mounted); err != nil {
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
	// THE IDENTITY DIRECTORY, a mount that can fail: its routes carry their
	// authority with their registration, so a policy that is missing or
	// names a verb the table has no rule for is refused now rather than
	// serving an ungated route for the life of the process.
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
	// AND THE ARTIFACTS SURFACES, for the same reason.
	if opts.Artifacts != nil {
		if err := opts.Artifacts.Routes(mux); err != nil {
			return nil, fmt.Errorf("api: mount the artifacts surface: %w", err)
		}
	}
	if opts.ArtifactData != nil {
		if err := opts.ArtifactData.Routes(mux); err != nil {
			return nil, fmt.Errorf("api: mount the artifact datastore surface: %w", err)
		}
	}
	if opts.Datasets != nil {
		if err := a.mountDatasets(mux, opts.Datasets, opts.Sources.Chart); err != nil {
			return nil, fmt.Errorf("api: mount the dashboard datasets surface: %w", err)
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
	// partition and the guard both decide from `r.URL.Path` — which
	// listener serves a route, and which routes are exempt — and a path
	// like `/webhooks/../config` reads to them as a public, exempt prefix
	// and to ServeMux as another route entirely. Then the PARTITION, before
	// the guard (see [partition]). Then CORS, which must answer a preflight
	// before the guard sees it — a browser attaches no credential to one.
	// Then the guard, which resolves the principal. Then the CSRF check,
	// INSIDE the guard because the credential's SHAPE is what decides
	// whether a missing Origin is a refusal, and nothing before the guard
	// knows which shape arrived. Innermost, the mux's OWN 404 and 405 are
	// answered in the engine's envelope ([httpjson.Mux]), like every other
	// answer this surface writes: a client reads a refusal with no code as
	// something in front of the node, so a route this node simply does not
	// serve — a work purge on a node serving no human write surface, any
	// route a newer client asks an older node for — read as a write whose
	// outcome was unknown.
	//
	// ONE ROUTE TABLE AND ONE CHAIN, whatever the listeners. With a dedicated
	// public listener (Tier A api.public) the two sockets serve the same
	// handler behind a partition — the public one only [auth.Public]'s
	// routes, this one everything else — so a route is mounted, guarded and
	// drained exactly once, and which socket answers it is decided by one
	// predicate rather than by which of two muxes somebody registered it on.
	a.routes = mux.patterns
	a.policies = mux.policies
	chain := a.cors.Middleware(a.guard.Middleware(a.csrf.Middleware(
		a.drainGate(httpjson.Mux(mux.mux)))))
	// EACH SOCKET SAYS WHETHER IT IS HTTPS BY ITS OWN ADDRESS
	// ([servedOverHTTPS]). The two are published apart — an admin side on a
	// private http address beside a public one behind TLS is the natural
	// shape of the split — and the public side serves pages a browser
	// renders (the GitHub App return, the Slack OAuth landing), so judged by
	// the admin side's scheme those went out with no Strict-Transport-Security
	// exactly where the deployment had published them over https.
	if opts.Bootstrap == nil || !opts.Bootstrap.API.Public.Enabled() {
		a.handler = servedOn(opts.Bootstrap, false, chain)
	} else {
		a.handler = servedOn(opts.Bootstrap, false, partition(false, chain))
		a.public = servedOn(opts.Bootstrap, true, partition(true, chain))
	}
	// --- the Dashboard Assistant's dispatcher (WP1) ---
	if err := bindDispatcher(opts.Dispatcher, a.handler); err != nil {
		return nil, err
	}
	a.chatLive.AttachSockets()
	return a, nil
}

// --- the Dashboard Assistant's dispatcher (WP1) ---

// bindDispatcher binds the assistant's dispatcher, when there is one, to the
// handler api.port serves: the whole chain a click meets, and never the public
// listener's, which serves none of the routes a grant may reach.
func bindDispatcher(d *auth.Dispatcher, h http.Handler) error {
	if d == nil {
		return nil
	}
	if err := d.Bind(h); err != nil {
		return fmt.Errorf("api: %w", err)
	}
	return nil
}

// servedOn is the outermost wrapping of everything one of a node's sockets
// serves: the security headers, judged by that socket's own scheme
// ([servedOverHTTPS]), and the canonical-path refusal before anything reads the
// path. See [New] for why each sits where it does.
//
// ONE WRAPPING for the full surface and the boot surface api.port and
// api.public serve before it ([Boot]), so a request a node refuses while it
// starts carries the headers the same request carries once it serves. Among
// them [httpjson.EngineHeader], set first, so every answer says it is the
// engine's, a refusal no handler thought about included.
func servedOn(bootstrap *config.Bootstrap, public bool, h http.Handler) http.Handler {
	return pagepolicy.Apply(markedAsEngine(authz.CanonicalPath(h)), servedOverHTTPS(bootstrap, public))
}

// markedAsEngine sets [httpjson.EngineHeader] on the answer before anything
// beneath it writes one: a header set after the status line is dropped.
func markedAsEngine(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(httpjson.EngineHeader, httpjson.EngineHeaderValue)
		next.ServeHTTP(w, r)
	})
}

// partition serves the requests on one side of [auth.Public] and answers every
// other with a 404.
//
// BEFORE THE GUARD, which is the point on the public side: a route this
// listener does not serve is absent there before any credential is asked for,
// so the socket a deployment publishes answers a missing token, a wrong one and
// the right one alike, and cannot be used to tell a valid operator token from an
// invalid one.
//
// THE PUBLIC SIDE'S 404 IS THE MUX'S OWN, byte for byte ([httpjson.NoRoute]):
// absent as on a node whose build lacks the route. Anything more — a hint naming
// api.port, a wording of its own — would tell whoever scans the published
// socket that a private admin port exists and which paths it serves.
//
// THE ADMIN SIDE'S 404 NAMES THE RIGHT LISTENER, for the reverse reason: the
// caller there is on the operator's own network, and the likeliest one is a
// vendor somebody configured with the wrong port, who must learn it from the
// first delivery rather than from one quietly accepted on the socket nobody
// published.
//
// Inside the security headers, so the 404 carries them like every other answer.
func partition(public bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch serves := auth.Public(r.URL.Path); {
		case serves == public:
			next.ServeHTTP(w, r)
		case public:
			httpjson.NoRoute(w, r)
		default:
			servedOnThePublicListener(w, r)
		}
	})
}

// servedOnThePublicListener is api.port's answer for a route outside parties
// call, on a node whose public listener serves it: the 404 that names the right
// listener, for the reason [partition] gives.
//
// ITS OWN FUNCTION because two surfaces answer it: the partition, and the boot
// surface api.port serves before the node hands it this one ([Boot]) — so a
// vendor configured with the wrong port learns it from its first delivery
// whichever of the two that delivery meets.
func servedOnThePublicListener(w http.ResponseWriter, r *http.Request) {
	httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNoRoute, map[string]string{
		"detail": fmt.Sprintf("this listener serves nothing at %s %s",
			r.Method, r.URL.Path),
		"hint": "This node has a public listener (api.public): the " +
			"webhooks, the vendor app landings and the sandbox " +
			"endpoints are served there and only there. Point the " +
			"sender at that address.",
	})
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
		{"ChatLive", o.ChatLive == nil},
		{"Sources.Company", o.Sources.Company == nil},
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
		{"Models", o.Models == nil},
		{"Retention", o.Retention == nil},
		{"Capacity", o.Capacity == nil},
		{"FleetBroker", o.FleetBroker == nil},
		{"Backup", o.Backup == nil},
		{"Objects", o.Objects == nil},
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
// than the company: the backup, the retention gestures, the capacity window
// and the fleet broker's membership. Every one is admin's ([iam.GrantAdmin]),
// through the authority table's [authz.ActionFleetOperate] for a write and
// [authz.ActionFleetRead] for a read, stated where it is mounted.
//
// # They decided nothing, and the guard in front of them decides nothing either
//
// Each handler resolved its CALLER — for the audit line — and asked no grant,
// because each was written while the only credential this engine had was an
// operator token and "somebody resolved" meant "the operator". The guard in
// front of every route answers the same question and no other: is there a
// principal. So once a person could sign in holding nothing but the company's
// overview, or a pipeline hold Worklet alone, every one of them could
// clear a spend ceiling, copy the node's whole durable state to a directory of
// their choosing, move the floor the trim deletes against, and evict a machine
// from the fleet. Through [authz.Router] each route is refused `403` naming
// admin, and a route mounted without a policy is a boot failure rather than an
// open door.
//
// THE READS TOO (the maintenance status and the reanchor confirmation value),
// because `/worklet/retention*` is the deployment's surface whole, which is
// what the `retention` question already declares: a map of which machine
// holds what, and the value a reanchor must echo, are not the company's
// working state.
//
// # A write asks for a recent proof of identity, and a read does not
//
// The two verbs ask the same permission and differ in the step-up alone: every
// write here changes what the deployment does for everybody on it, so it asks
// for a proof inside `api.auth.session.step_up`, while the two reads — the
// maintenance window's status and the value a reanchor must echo — change
// nothing, and a status an operator cannot see without re-proving who they
// are is one they stop checking. The METHOD picks the verb, because that is
// the one fact about a route this surface already treats as its read/write
// line (the session table's columns are keyed on it too), and a second list
// of which patterns read would be the thing that drifts.
func (a *App) mountDeployment(mux authz.Mux) error {
	router := authz.NewRouter(mux, authz.ContextGuard(authz.NoChart{}))
	var failures []error
	a.deploymentRoutes(func(pattern string, does authz.Effect, h http.HandlerFunc) {
		policy := deploymentPolicy(pattern)
		policy.Effect = does
		if err := router.Handle(pattern, policy, h); err != nil {
			failures = append(failures, err)
		}
	})
	return errors.Join(failures...)
}

// deploymentRoutes names every route [App.mountDeployment] mounts, through the
// mount it is handed: separate from the router, so a walk can read the whole
// list without standing the deployment up. A POST says what it does
// ([authz.Effect]); every other route passes none.
func (a *App) deploymentRoutes(mount func(string, authz.Effect, http.HandlerFunc)) {
	// A POST, and a read posture of any kind never opens it: copying every
	// credential and every seat's memory to a path the caller names is not
	// a read. There is no budget reset beside it — a window's turnover IS
	// the reset (ADR-0019), and a raw reset would re-arm a company mid-window.
	mount("POST /backup", authz.EffectCreates, a.serveBackup)
	// The move of the company's files to an S3 bucket: a copy every
	// object passes through, and on the final pass the fleet's record of
	// where its files are. See objectsmigrate.go.
	mount("POST /objects/migrate", authz.EffectRewrites, a.serveMigrateObjects)
	// The three retention gestures that write. See retention.go.
	a.mountRetention(mount)
	// The capacity window's own control surface. It is the one thing a
	// maintenance-mode node serves that a publishing one does not need,
	// and it is why the verb can run at all on a topology whose broker
	// binds no socket. See retention.go.
	a.mountCapacity(mount)
	// The removal of a member the fleet broker's metadata group still
	// counts after it is gone for good — a change to the quorum every
	// election runs on. The listing beside it is the `fleet_broker`
	// question, a named read. See fleetbroker.go.
	a.mountFleetBroker(mount)
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
// The rest of what the edge needs (whether a revision is active, the clock)
// comes from the app itself, so those cannot be wired differently here than
// they are everywhere else on this node. It needs no event log and no live
// stream: a delivery's row and its live envelope both come from the record it
// PUBLISHES, through the node's publish listener and every node's projection.
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

	// SlackInstalls finishes an install a seat's Slack install link began,
	// on the landing every agent's Slack app redirects to. Nil answers
	// such an arrival 503. See [webhooks.Options.SlackInstalls].
	SlackInstalls webhooks.SlackInstaller

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
func (a *App) mountWebhooks(mux httpjson.Router, in Inbound, now func() time.Time) error {
	receiver, err := webhooks.New(webhooks.Options{
		Secrets:    in.Secrets,
		Publisher:  in.Publisher,
		Claims:     in.Claims,
		Keys:       in.Keys,
		AppFlow:    in.AppFlow,
		Recheck:    in.Recheck,
		Configured: a.Configured,
		Now:        now,

		SlackInstalls: in.SlackInstalls,
	})
	if err != nil {
		return fmt.Errorf("api: %w", err)
	}
	receiver.Routes(mux)
	return nil
}

// ServeHTTP makes the app the handler of api.port: every route, or every route
// but the public ones when the node has a public listener.
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.handler.ServeHTTP(w, r) }

// Public is the handler of the dedicated public listener (Tier A api.public):
// the routes outside parties call — see [auth.PublicPrefixes] — and nothing
// else. Nil when the node has none, and every route is then [App.ServeHTTP]'s.
func (a *App) Public() http.Handler { return a.public }

// InboxFeed is where this node hears whose inbox moved: the engine, whose
// tracker applier says so after every committed batch — and, while this
// node's own copy is out of service, a sound peer's applier relayed.
//
// CONSUMER-DEFINED and two methods wide: the movements, and the GAPS between
// them — the engine switching where its movements come from, after which what
// moved in between is not replayed. The engine satisfies it with
// [engine.Engine.SetOnInboxMoved] and [engine.Engine.SetOnInboxGap]; a test
// hands in a feed it fires by hand.
type InboxFeed interface {
	SetOnInboxMoved(func([]worklet.InboxMovement))
	SetOnInboxGap(func())
}

// pushInboxMoved turns one committed batch's inbox movements into
// `inbox_changed` frames, each reaching only the sockets watching that seat.
//
// FROM ONE SOURCE AT A TIME, which the engine chooses: every node that serves
// this API holds the estate — ingress requires the `data` role (ADR-0025) —
// and applies every tracker record, so while its copy serves it hears every
// movement from its own applier. A copy out of service keeps the node in
// rotation and its applier announces nothing it can trust, so for as long as
// it is out the engine relays a sound peer's movements instead, and says when
// it switched ([App.pushInboxGap]).
func (a *App) pushInboxMoved(moved []worklet.InboxMovement) {
	for _, m := range moved {
		a.stream.InboxChanged(stream.InboxChange{
			Handle: m.Handle, UnreadDelta: m.UnreadDelta,
			Subject: m.Subject, Reason: string(m.Reason), Stored: m.At,
		})
	}
}

// pushInboxGap tells every watched inbox to read itself again: the engine
// switched where this node's movements come from, and what moved in the gap is
// not replayed — see [stream.Service.InboxReread].
func (a *App) pushInboxGap() { a.stream.InboxReread() }

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
func (a *App) Stop() {
	for _, stop := range a.stopRooms {
		stop()
	}
	a.stream.Stop()
}

func (a *App) serveHealth(w http.ResponseWriter, r *http.Request) {
	// ALWAYS 200 while the process is alive, INCLUDING through a drain: an
	// orchestrator watching liveness must not SIGKILL a node that is
	// finishing its in-flight turns. /ready is what steers traffic. That
	// only holds because the listener outlives the drain; see drain.go.
	writeJSON(w, http.StatusOK, a.health(r.Context()))
}

// ReasonEstateBehind is what a refused /ready names when the estate seam
// refused it ([EstateFloor]) — on a running node, that this node can no longer
// route the estate's reads and writes to a sound copy.
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

// estateReady reports whether this node can answer from the replicated estate,
// and names the term that says it cannot.
//
// A node with no seam declared has nothing to judge — see [Options.Estate].
func (a *App) estateReady(ctx context.Context) (bool, string) {
	if a.estateFloor == nil {
		return true, ""
	}
	return a.estateFloor(ctx)
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
	var refusal *queries.Refusal
	switch {
	case err == nil:
		return data, nil
	case errors.Is(err, queries.ErrUnknown):
		return nil, fmt.Errorf("%w: %s", stream.ErrUnknownQuery, what)
	case errors.As(err, &refusal):
		// A REFUSAL ON AUTHORITY SAYS WHY, as it does over REST, and in the
		// SAME WORDS: [stream.RefusedFor] answers it through the authority
		// table's one mapping, so the code, the rule's reason and the grants
		// that would have admitted the caller are the ones the REST twin
		// writes. It was reduced to the question's name here once, so the one
		// channel the dashboard reads was the one on which "you may not"
		// could not say what would change that — and then spelled
		// `unauthorized` here for itself, which a refusal the mapping words
		// otherwise would have reached the dashboard as.
		//
		// A QUESTION ASKED BY NOBODY ([queries.ErrUnauthenticated]) has no
		// arm here and is answered as the fault it is: every socket
		// authenticates at its handshake and closes when its credential
		// ends, so a question on one that reaches the registry unresolved
		// means this node lost who the socket is — which no refusal a client
		// acts on describes.
		return nil, stream.RefusedFor(what, refusal.Decision)
	case errors.Is(err, queries.ErrNotFound):
		return nil, fmt.Errorf("%w: %s", stream.ErrNotFound, what)
	case errors.Is(err, queries.ErrArtifactTrashed):
		return nil, fmt.Errorf("%w: %s", stream.ErrArtifactTrashed, what)
	case errors.Is(err, queries.ErrUnknownProvider):
		// A REQUEST TO CHANGE, as a refused parameter is: the socket has
		// one code for that, and the detail names the provider.
		return nil, &stream.BadParamsError{What: what, Detail: err.Error()}
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
		writeQueryError(w, r, what, err)
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
func writeQueryError(w http.ResponseWriter, r *http.Request, what string, err error) {
	var refusal *queries.Refusal
	var revision *stream.RevisionError
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
	case errors.As(err, &refusal):
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
		// AND IT SAYS WHY, in the AUTHORITY TABLE'S OWN MAPPING
		// ([authz.RefusalFields]): the code, the rule's reason and the
		// grants that would have admitted the caller, from the decision the
		// registry's refusal carries whole — the answer every other surface
		// gives the same refusal. It wrote `unauthorized` here for itself,
		// which agreed with the mapping only for as long as no question was
		// refused for a stale proof. A bare [queries.ErrUnauthorized] names
		// no decision, and is answered by the default arm as the fault it
		// is.
		status, code, detail := authz.RefusalFields(refusal.Decision)
		httpjson.FailWithFields(w, status, code, detail)
	case errors.Is(err, queries.ErrBadParams):
		// 400 AND ITS OWN CODE. The status was already right; the code
		// said `query_failed`, which names a fault of this node for a
		// request the caller has to change. AND ITS SENTENCE, as the
		// socket's frame carries it: the refusal names the parameter to
		// change, which is the whole of the fix, and the envelope's
		// `message` cannot say which one.
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeBadParams,
			httpjson.Detail{"detail": queries.RefusalDetail(err)})
	case errors.Is(err, queries.ErrUnknownProvider):
		// 422: the parameter is well formed and names nothing the
		// catalogue holds, which the page answers with free text.
		httpjson.FailWithFields(w, http.StatusUnprocessableEntity, httpjson.CodeUnknownProvider,
			httpjson.Detail{"detail": err.Error()})
	case errors.Is(err, queries.ErrNotFound):
		httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound)
	case errors.As(err, &revision):
		// A REVISION THIS NODE CANNOT OPEN, under the /config route's own
		// code and words, as the socket's frame carries them: answered by
		// the default arm, a missing key was a 500 nobody could act on.
		httpjson.FailWith(w, http.StatusConflict, revision.Code,
			map[string]string{"detail": revision.Detail})
	case errors.Is(err, queries.ErrArtifactTrashed):
		// THE ARTIFACTS ROUTES' OWN ANSWER for the same artifact, so a
		// question and the route beside it say one thing about it.
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeArtifactTrashed,
			map[string]string{"detail": "the artifact is in the trash; its data is not " +
				"served until it is restored"})
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
		//
		// THE REFUSAL'S OWN DETAIL GOES TO THE LOG ([stream.LogUnavailable]):
		// it names positions and settings for whoever runs the node, and
		// the answer's words are a person's sentence for the refusal.
		stream.LogUnavailable(r.Context(), log, "api_query_unavailable", err, "what", what)
		u := stream.UnavailableOf(err)
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, u.RetryAfter, u.Fields())
	default:
		// The reason reaches the LOG, not the caller: it can carry a
		// database path or a driver's own message, and nothing about
		// holding a grant makes a caller somebody that path is for.
		stream.LogFailed(r.Context(), log, "api_query_failed", err, "what", what)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeQueryFailed)
	}
}

// servedOverHTTPS reports whether a browser reaches one of this node's
// listeners over TLS, which is what turns on `Strict-Transport-Security`
// ([pagepolicy.Apply]): the public listener's when public is true, and
// api.port's otherwise.
//
// THE ADDRESS THAT LISTENER IS PUBLISHED AT AND NOTHING ELSE. The bind address
// and `r.TLS` both describe the hop between the proxy and this process, which
// on a hardened node is plain http over loopback — so either would withhold
// the header from exactly the deployments that need it. api.port is published
// at `api.external_url`; the public listener at `api.public.external_url`
// ([config.API.WebhookBase], which is that address whenever the listener is
// set) — two sockets, two addresses, and possibly two schemes, so one answer
// for both sent the header on neither or on the wrong one.
func servedOverHTTPS(b *config.Bootstrap, public bool) bool {
	switch {
	case b == nil:
		return false
	case public:
		return strings.HasPrefix(b.API.WebhookBase(), "https://")
	default:
		return strings.HasPrefix(b.API.ExternalBase(), "https://")
	}
}
