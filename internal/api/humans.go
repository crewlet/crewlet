package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/colleague"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/api/iamapi"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/api/workapi"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE SURFACES A PERSON REACHES, built over the engine running beside them:
// the way in, the identity directory, the operator surface (the dashboard's
// act route and an operator's own assistant), the human write surface, and the
// two credential arms the guard resolves a browser's cookie and a machine token
// through.
//
// # Here, beside the options they fill, and not in the command that serves them
//
// For [engineRuntime]'s reason, with its failure measured. They were built in
// cmd/crewlet, which is package main and reachable from no other package, so
// the end-to-end suite's own wiring could not call them and mounted none of
// them: every fixture there presented a Tier A bearer to a node serving no
// /auth, no /iam and no /worklet. A fleet whose sessions did not cross between
// its members — a cookie minted on one node refused on the next, a revocation
// one node never heard — left the fleet suite green, because nothing in it had
// ever signed in. One constructor that `crewlet run` and the suite both call is
// what makes the suite's sign-in the one a deployment serves; a copy in the
// suite would be a harness agreeing with itself.

// HumanSurfaces are the surfaces a node serves a person, and a person's
// credentials, through. Built by [NewHumanSurfaces] and handed to an API's
// options by [HumanSurfaces.Mount].
type HumanSurfaces struct {
	// SignIn serves /auth — how a person BECOMES a principal. Every node
	// serving the API serves it, one that has met no company included:
	// there the
	// deployment's Tier A token exchanges for a session (`POST
	// /auth/token`), which is how a company is created, and the company's
	// first person is invited or created onto a human seat it declares —
	// `409 no_active_revision` until it does.
	//
	// HELD BY WHOEVER SERVES IT as well as mounted, for
	// [authapi.Service.Stop]: the work a sign-in runs after its answer — a
	// stale password's rewrite — is the surface's own to stop, after the
	// listener closes and before the engine it writes to does.
	SignIn *authapi.Service

	// Sessions is the other end of the cookie SignIn mints, built from the
	// same signer.
	Sessions *auth.Sessions

	// Tokens resolves the machine tokens /iam/credentials mints — `crewlet
	// iam token`'s value, presented as CREWLET_API_TOKEN.
	Tokens *auth.Tokens

	// Directory serves /iam, and Work the human write surface over the
	// Worklet and Doclet, which reads the halves it serves
	// per request, since a node meets them at its first company and may
	// meet that long after this surface was built.
	Directory guardedMounter
	Work      guardedMounter

	// Operator is the operator surface — `/operator/act`, the dashboard's
	// write transport, and `/operator/mcp`, an operator's own assistant —
	// and the ONE DISPATCH Work's tool-backed routes call through. One
	// server per node, because its answer cache is per node and a second
	// one beside it would be a second catalogue for one surface.
	Operator *operator.Server

	// Listens tells every MCP client listening on Operator's transport that
	// its tools changed — when this node's catalogue moves and when another
	// node's does ([OperatorListens]).
	Listens *OperatorListens
}

// NewHumanSurfaces builds a node's human surfaces over its engine and the Tier
// A it runs under.
//
// EVERY NODE SERVING THE API SERVES ALL OF THEM, with a company or without
// one. Such a node holds the estate — ingress requires the `data` role
// (ADR-0025) — and the identity estate is the engine's CORE, running from boot
// on every node that holds the estate, so a node
// nobody has configured yet still exchanges the deployment's Tier A token for
// a session — the session its company is created through — holds the
// directory its first person is invited or created into once that company
// declares a human seat, and serves the write surface — whose
// routes answer `503 no_active_revision` until the first company brings the
// tracker and the knowledge base up, and are served from then on with no
// restart. They used to be ABSENT on such a node, each logged, so the node a
// company is bootstrapped on was the one node nobody could sign in to until
// it was restarted.
func NewHumanSurfaces(boot *config.Bootstrap, e *engine.Engine) (HumanSurfaces, error) {
	signIn, sessions, err := signInSurface(boot, e)
	if err != nil {
		return HumanSurfaces{}, err
	}
	// AND THE THIRD CREDENTIAL, a machine token the directory minted.
	tokens, err := auth.NewTokens(auth.TokensDeps{
		Directory: e, Chart: engine.SeatViewOf(e),
	})
	if err != nil {
		return HumanSurfaces{}, fmt.Errorf("api: the machine-token arm: %w", err)
	}
	// AND THE DIRECTORY.
	directory, err := directorySurface(boot, e)
	if err != nil {
		return HumanSurfaces{}, err
	}
	// AND THE OPERATOR SURFACE, whose dispatch the write surface's tools
	// are reached through — both deciding on ONE chart value, so a route
	// and the tool behind it cannot be handed two charts and answer one
	// question two ways.
	authority := engine.ChartAuthorityOf(e)
	// ONE REGISTRY OF LISTEN STREAMS for the node, which the server keeps
	// every stream in and the telling announces through.
	listeners := operator.NewListeners()
	operators, err := operatorSurface(e, authority, listeners)
	if err != nil {
		return HumanSurfaces{}, err
	}
	listens := NewOperatorListens(operators, listeners, e.Backends().Queue, e.Node().ID())
	// AND THE WRITE SURFACE, over the halves each request finds.
	work, err := workSurface(e, operators, authority)
	if err != nil {
		return HumanSurfaces{}, err
	}
	return HumanSurfaces{
		SignIn: signIn, Sessions: sessions, Tokens: tokens,
		Directory: directory, Work: work, Operator: operators,
		Listens: listens,
	}, nil
}

// Mount hands the surfaces to an API's options.
//
// THE ONE PLACE A NIL SURFACE BECOMES A NIL INTERFACE, and it is a method so
// that no caller does the conversion for itself. A surface held as a nil *T is
// a NON-NIL interface once it reaches [Options], so [New]'s own "is there one"
// check passes and mounts every route over a nil service — which answers each
// request with a nil dereference instead of the 404 an absent surface is
// documented to be. [NewHumanSurfaces] builds every one on an engine [engine.New]
// made; a zero HumanSurfaces — a suite's — is what this keeps honest.
func (h HumanSurfaces) Mount(o *Options) {
	if h.SignIn != nil {
		o.Auth = h.SignIn
	}
	o.Sessions, o.Tokens = h.Sessions, h.Tokens
	o.IAM, o.Work = h.Directory, h.Work
	o.Operator, o.OperatorListens = h.Operator, h.Listens
}

// signInSurface builds /auth and the session arm beside it.
//
// ON EVERY NODE SERVING THE API. The identity estate is the engine's core, run
// wherever the estate is held — which ingress requires — so a node that has
// met no company holds its directory too — and is exactly where the
// deployment's Tier A token signs a session in to create the company, after
// which its first person is invited or created onto one of its human seats
// and signs in there. An engine with no identity estate is
// one [engine.New] did not build, which is a wiring mistake to refuse by
// name rather than a posture to serve around: a node holding no estate is one
// whose roles leave out `data`, and config refuses `ingress` on such a node.
//
// A keyring that cannot sign for the fleet USED TO BE a narrower posture too,
// and it is not any more: Tier A refuses a file without a usable keyring and
// the engine refuses to start without one, so a session signer this function
// cannot build is a fault it returns rather than a node it quietly narrows.
func signInSurface(boot *config.Bootstrap, e *engine.Engine) (
	*authapi.Service, *auth.Sessions, error) {

	reader, writer := e.IAM(), e.IAMWriter()
	if reader == nil || writer == nil {
		return nil, nil, errNoIdentityEstate
	}
	signer, err := session.New(session.Options{
		Material: boot.Secrets.TokenMaterial(),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("api: the session signer: %w", err)
	}
	// THIS NODE'S OWN CURVE, and nobody else's: a guessing run a load
	// balancer rotates across the fleet meets each node's separately, which
	// is the residual internal/iam/credential's throttle states and bounds.
	throttle := credential.NewThrottle(credential.ThrottleDeps{})
	// THE SEALER, checked here for the directory's reason: a typed nil in
	// the surface's interface would pass its own refusal.
	sealer := e.PersonSealer()
	if sealer == nil {
		return nil, nil, errors.New("api: sign-in needs a keyring. Set " +
			"`secrets.keys` in this node's config file")
	}
	surface, err := authapi.New(authapi.Options{
		Bootstrap: boot,
		// THE READER, with a named session's standing asked of the ENGINE,
		// which reads the identity log's end first so a lineage this
		// node's rows do not hold is answered as nobody's only where they
		// hold every record the log does — see [signInDirectory].
		Directory: signInDirectory{Reader: reader, engine: e},
		// THE ORGANISATION THIS NODE RUNS, which an invitation's page
		// names the seat it binds from.
		Seats: engine.SeatViewOf(e),
		// THE NODE'S OWN WRITER, which acts as the deployment. What the
		// routes do with it is create people and open sessions, both of
		// which are the deployment's to do on somebody's behalf — a
		// person cannot author their own enrolment, because they do not
		// exist until it lands. It is the AUTHOR only of what the engine
		// decides for itself: a password re-hashed at this build's cost.
		Writer: writer,
		// AND THAT WRITER WRITING FOR A PERSON, for every gesture a person
		// makes through these routes: the node only writes it down, and
		// the trail names who made it — see [iamdomain.Writer.For].
		Behalf:   func(person iam.Principal) authapi.Writer { return writer.For(person) },
		Signer:   signer,
		Hasher:   credential.NewHasher(credential.Default(), credential.VerifyCap()),
		Throttle: throttle,
		Blinder:  e.PersonBlinder(),
		Sealer:   sealer,
		Sessions: reader,
		// THE SAME PURE FUNCTION THE GUARD USES over the same Tier A,
		// which is one PARSER rather than one instance — see
		// [auth.Clients].
		Clients: auth.NewClients(boot),
		// THE NODE'S ONE AUDIT TRAIL, which the guard and the directory
		// hand what they saw to as well: a failed sign-in and a refused
		// bearer from one source are one count on the minute's row only
		// because both reach the same trail.
		Audit: e.AuthEvents(),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("api: the sign-in surface: %w", err)
	}
	// AND THE OTHER HALF, built from the SAME signer. A cookie minted
	// under one key and validated against another is a sign-in that
	// appears to work and then does not stick — and it would do so only
	// on the requests that landed on a node whose signer was built
	// separately, which is the shape nobody reproduces.
	sessions, err := auth.NewSessions(auth.SessionsDeps{
		Signer:    signer,
		Directory: reader,
		// THE SAME READER'S APPLIER, which a write presenting a session
		// this node has not applied yet waits on — the sign-in above
		// answers before it does.
		Applier: reader,
		// THE SEAT VIEW over the company this node runs — never nil, which
		// internal/iam/session reads as the seatless arm; a node running no
		// company yet answers UNKNOWN. See [engine.SeatViewOf].
		Chart:    engine.SeatViewOf(e),
		External: boot.API.ExternalBase(),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("api: the session arm: %w", err)
	}
	return surface, sessions, nil
}

// signInDirectory is the sign-in surface's [authapi.Directory]: this node's
// identity reader, with the reads that turn on a row being MISSING — whose is a
// named session, and whether a link's id names an invitation or a reset link —
// proved against the identity log's end by the engine
// ([engine.Engine.SessionStanding], [engine.Engine.ResetByID],
// [engine.Engine.InvitationByID]), because the reader cannot read the log's
// end itself and a missing row on a node behind the log is a session still
// running, or a link issued through a peer.
type signInDirectory struct {
	*iamdomain.Reader
	engine *engine.Engine
}

func (d signInDirectory) SessionStanding(ctx context.Context, lineage string,
	now time.Time) (string, bool, error) {

	return d.engine.SessionStanding(ctx, lineage, now)
}

func (d signInDirectory) ResetByID(ctx context.Context, id string) (
	iamdomain.ResetRow, error) {

	return d.engine.ResetByID(ctx, id)
}

func (d signInDirectory) InvitationByID(ctx context.Context, id string) (
	iamdomain.InvitationRow, error) {

	return d.engine.InvitationByID(ctx, id)
}

// directorySurface builds /iam, on every node serving the API for
// [signInSurface]'s reason.
func directorySurface(boot *config.Bootstrap, e *engine.Engine) (guardedMounter, error) {
	reader, writer := e.IAM(), e.IAMWriter()
	if reader == nil || writer == nil {
		return nil, errNoIdentityEstate
	}
	// THE KEYRING'S OPENER, checked here rather than handed in as a typed
	// nil: an interface holding a nil pointer is not nil, so the surface's
	// own refusal would never see it and the first name it opened would
	// panic.
	sealer := e.PersonSealer()
	if sealer == nil {
		return nil, errors.New("api: the people directory needs a keyring. " +
			"Set `secrets.keys` in this node's config file")
	}
	surface, err := iamapi.New(iamapi.Options{
		Directory: reader,
		// ONE WRITER PER CALLER. The node's own writer acts as the
		// DEPLOYMENT, which is right for a bootstrap and wrong for
		// everything here: a directory whose author field is the node
		// is not an audit trail. The party is the caller's principal,
		// whole — its name, the credential beside it, its grants and its
		// id, which a person's own mint is decided on.
		Authority: func(principal iam.Principal) iamapi.Writer {
			return writer.As(principal)
		},
		Opener:       sealer,
		ExternalBase: boot.API.ExternalBase(),
		// THIS NODE'S OWN ADMIN POSTURE, which the report reads to say
		// whose admin this node clamps away: it is applied at decision
		// time and never written, so a fleet whose nodes disagree is a
		// legal state and nothing else would say so.
		Admin: boot.API.Auth.Admin,
		// THE ENGINE'S RULE, not one written here: the request path's own
		// seat table applied to a person's row, which the report asks and
		// nothing restates. The seam it replaced asked only whether the
		// chart held a row by that handle, which is how a person bound to
		// an AGENT seat went unreported while every request they made was
		// refused. A node running no company yet answers the unknown arm
		// per row, which the report counts rather than guesses at.
		Bindings: e.DanglingBinding,
		// The company this node runs, whose human seats `/iam/seats`
		// lists beside who holds each.
		Seats: engine.SeatViewOf(e),
		// THIS NODE'S Tier A labels — never their values — for
		// `GET /iam/node-tokens`, read off a guard built from the same Tier
		// A the request guard is: which bearer authenticates is a pure
		// function of that document, and this surface is built before the
		// API's own guard exists.
		TokenIDs: auth.New(boot).TokenIDs,
		// AND WHICH OF THEM ALONE MAY CHANGE A MANAGED COMPANY DOCUMENT
		// (ADR-0030), from the same Tier A the config surface enforces it
		// from.
		CompanyWriters: boot.API.Auth.CompanyWriters,
		// What an administrator did — a token minted or revoked, a
		// session ended, a person removed — on the node's audit feed.
		Audit: e.AuthEvents(),
	})
	if err != nil {
		return nil, fmt.Errorf("api: the identity directory: %w", err)
	}
	return surface, nil
}

// errNoIdentityEstate is a human surface asked of an engine that holds no
// identity estate — one [engine.New] did not build, since every engine it
// builds runs the estate from boot.
var errNoIdentityEstate = errors.New("api: this node serves the API but has " +
	"no `data` role, so nobody can sign in. Add `data` to `node.roles`")

// operatorSurface builds the operator surface over this engine: ONE server per
// node, whose catalogue is built for every call from the halves the node
// serves at that moment ([NativeOperatorHalves]).
//
// THE SAME DEPS A SEAT'S TOOLS GET, with one field different: the actor, which
// is the request's principal ([operator] sets it per call). That is what makes
// this one implementation of every tool rather than two — see
// [builtin.WorkDeps.Actor].
//
// The seat controls — a parked coding run answered by its turn, a seat paused
// or resumed, a note to a running turn — are served on every node whatever the
// company's backends: each record is the fleet's, and each is carried out by
// the node holding the seat, which reads it off that seat's inbox or its own
// watched copy. A person's question answered from the company's knowledge runs
// on the auxiliary model of their own seat and is charged to the company's
// windows, with the answers cached on this node — once, here, since a cache a
// per-call catalogue built would remember nothing.
func operatorSurface(e *engine.Engine, authority authz.Chart,
	listeners *operator.Listeners) (*operator.Server, error) {
	chart := func() *org.Organization {
		c := e.Company()
		if c == nil {
			return nil
		}
		return c.Org
	}
	var counter tools.Counter
	if rec := e.Recorder(); rec != nil {
		counter = rec
	}
	server, err := operator.New(operator.Options{
		Halves:  NativeOperatorHalves(e),
		Counter: counter,
		Org:     chart,
		// THE AUTHORITY DECISION, which is the SAME one every seat's
		// registry is built with — one table, one function, every surface.
		// It reads the chart per call, because an apply replaces it.
		Authorize: builtin.Decide(authority),
		Runs: builtin.RunDeps{Desk: sandbox.AnswerDesk{
			Pending: sandbox.NewCoordStore(e.Backends().Fleet),
			Queue:   e.Backends().Queue,
			// WHO ANSWERED FIRST is said only by the answer's seal, which
			// the node's own fleet keyring opens.
			Seal: e.AnswerSeal(),
		}},
		Pauses: builtin.SeatPauseDeps{
			Pauses: e.Backends().Fleet, Announce: e.Backends().Queue, Org: chart,
		},
		// SIGNED WITH THE NODE'S OWN STEER CHANNEL, the one its desk
		// answers with: the running node offers a note as the person it
		// names on this node's decision, so neither half may be unsigned.
		Steer: builtin.SteerDeps{Asker: e.Backends().Queue, Wire: e.SteerWire(), Runs: e.RunSteers()},
		Answer: builtin.AnswerDeps{
			Models: engine.AnswerModels(e), Budget: engine.AnswerBudget(e),
			Corpus: e.KnowledgeCorpus,
			Cache:  builtin.NewAnswerCache(builtin.AnswerCacheEntries),
			// THE NODE'S REWRITE CACHE, the one every compaction this
			// node makes is kept in, so the same question at the same
			// corpus pays for each condensed source once.
			Rewrites: e.Rewrites(),
		},
		// EVERY CALL THAT MAY WRITE is audited onto this node's own queue,
		// so the event store here holds who did what through any of the
		// three transports.
		Audit: e.Backends().Queue,
		// THE NODE'S ONE REGISTRY of the streams clients listen on, which
		// [OperatorListens] announces through.
		Listeners: listeners,
	})
	if err != nil {
		return nil, fmt.Errorf("api: the operator surface: %w", err)
	}
	return server, nil
}

// workSurface builds the human write surface over the halves each request
// finds.
//
// ITS TOOLS THROUGH THE OPERATOR SURFACE'S DISPATCH — the one the act route and
// an operator's assistant call — and its own decisions on authority, the chart
// value that dispatch decides on, so a route and the tool behind it answer one
// question one way. What it adds is
// the two writers the tools never reach: the tracker's, bound to the caller,
// for a comment edit and the purge, and the knowledge base's store for its
// rename and its three destructive verbs.
//
// PER REQUEST, never captured here: the halves come up with the node's first
// company, which a node that booted with none meets at an apply after this
// surface is serving. See [workapi.Options.Halves].
func workSurface(e *engine.Engine, operators *operator.Server,
	authority authz.Chart) (guardedMounter, error) {

	surface, err := workapi.New(workapi.Options{
		Halves:   func() (workapi.Halves, bool) { return nativeHalves(e) },
		Chart:    authority,
		Operator: operators,
		// A VERB WITH NO TOOL is recorded onto the same queue the
		// dispatch records a tool's call onto.
		Audit: e.Backends().Queue,
	})
	if err != nil {
		return nil, fmt.Errorf("api: the work surface: %w", err)
	}
	return surface, nil
}

// nativeHalves are the tracker, knowledge-base and chat halves this engine
// serves now, and false where it has not been handed a company yet.
//
// [engine.Engine.NativeStarted] FIRST, and the halves after: it is monotonic,
// so a half read as absent after it said "started" is one this company does
// not run rather than one not published yet — see native.go.
//
// THROUGH THE NODE'S ESTATE ROUTER, every half, for [NativeToolDeps]' reason —
// the two writers the tools never reach and the object store the file bytes
// stream through included.
func nativeHalves(e *engine.Engine) (workapi.Halves, bool) {
	if !e.NativeStarted() {
		return workapi.Halves{}, false
	}
	work, kb := NativeToolDeps(e)
	halves := workapi.Halves{Work: work, Pages: kb}
	if as, ok := engine.OperatorWorkWriter(e); ok {
		halves.Tracker = func(actor builtin.Actor) workapi.TrackerWriter {
			return personWriter(as, actor)
		}
	}
	if store, ok := engine.OperatorPageWriter(e); ok {
		halves.PageStore = store
	}
	// CHAT'S WRITER THROUGH THE ROUTER, never this node's own store, for the
	// tracker's reason; a node in a maintenance mode hands out none, and its
	// chat routes answer as absent.
	if rooms, ok := engine.OperatorChatWriter(e); ok {
		halves.ChatStore = rooms
	}
	// THE CONVERSION IS THE POINT: a typed nil *objstore.Store inside the
	// interface would pass the surface's own check and panic on the first
	// upload.
	if objects := e.Objects(); objects != nil {
		halves.Objects = objects
	}
	return halves, true
}

// personWriter is the tracker's writer, through this node's estate router,
// acting as one person-facing actor: the author [builtin.PrincipalActor] made
// of the request's principal, of its kind, with the credential it acted
// through — the actor's own provenance, the one construction a seat's
// writers are bound by too, so the two surfaces cannot carry different trails
// for one actor.
//
// ONE HELPER FOR EVERY SEAM a person's surface writes the tracker through —
// the tool seams below and the work surface's own writer — because turning a
// tool-layer actor into a writer is a single rule, and hand-copied spellings
// of it are chances for one seam to record an identity the others do not.
func personWriter(as func(estate.Actor) estate.WorkWriter, actor builtin.Actor) estate.WorkWriter {
	return as(estate.Actor{Handle: actor.Handle, Kind: actor.Kind,
		Provenance: actor.Provenance()})
}

// NativeToolDeps are the deps the builtin tools are built from for a surface
// whose caller is a PERSON rather than a seat: the operator surface's
// catalogue, which the dashboard's act route, an operator's assistant over MCP
// and the human write surface's tool-backed routes all call through, and the
// rows the write surface reads before it decides.
//
// ONE CONSTRUCTOR FOR ALL OF THEM, because a field wired on one and forgotten
// on another is a tool that behaves differently depending on where it was
// called from — which is the drift each of the comments below records having
// happened once already. Actor and Authorize are left for the operator surface
// to set per call: the actor is the request's principal carrying the call's
// operation, and the decision is the chart's.
//
// THE CHART SEAMS ARE READ PER CALL — leads, units, mentions, the default
// project, the clock — because these deps are built for every request and an
// apply can move any of them between two.
//
// THROUGH THIS NODE'S ESTATE ROUTER, every read and every write, exactly as a
// seat's tools are ([engine.OperatorWork], [engine.OperatorWorkWriter],
// [engine.OperatorPages], [engine.OperatorPageWriter]) — never this node's own
// copy read directly. Read directly, a data node whose copy was out of service
// went on answering its operator from the copy it had stopped serving while
// its own seats were answered by a peer's; through the router a person is
// answered as the seats are, and a read after their own write waits for it
// on the router's session floor ([engine.Engine.AwaitEstate]) — never this
// node's own applier, which is not the one that answers once its copy is out
// of service.
//
// THE READS WHEREVER THIS NODE SERVES THEM, THE WRITES ONLY WHERE IT HANDS OUT
// A WRITER: two facts, so two gates. A node in a maintenance mode serves
// Worklet's and Doclet's reads and hands no writer to any surface
// ([engine.OperatorWorkWriter], [engine.OperatorPageWriter], behind the
// engine's one writer gate), so it is handed the read tools and no write tool
// — never write tools whose every write is refused. Gated on both at once,
// such a node's operator was offered no read at all for the whole of a
// capacity window: their assistant could not read a board the dashboard beside
// it was drawing, nor fetch a project's file the listing named. Every tool is
// gated on its own seam ([builtin.OperatorTools]), so a reader handed without
// a writer serves exactly the reads ([readsOnly]).
func NativeToolDeps(e *engine.Engine) (builtin.WorkDeps, builtin.PageDeps) {
	var work builtin.WorkDeps
	var kb builtin.PageDeps
	reader, readable := engine.OperatorWork(e)
	writer, writable := engine.OperatorWorkWriter(e)
	if readable {
		work = builtin.WorkDeps{
			Reader: reader,
			// THE OPERATOR'S OWN CREDENTIAL IS THE PARTY, and it comes
			// from the request's context rather than from the call: a
			// tracker whose author field is chosen by the writer is not
			// an audit trail, and there is deliberately no way to name a
			// seat to act as.
			Writer: func(actor builtin.Actor) builtin.WorkWriter {
				return personWriter(writer, actor)
			},
			// AND THE TWO SEQUENCES, which this surface went
			// without — so an operator's assistant was refused
			// `waiting_on` and `blocking` by name on a tool whose
			// own description offers them, and would not have been
			// served the fold at all. Both need the replicated
			// estate, which this writer has; nothing else about
			// them differs from a seat's.
			Dependencies: func(actor builtin.Actor) builtin.WorkDepender {
				return personWriter(writer, actor)
			},
			Merges: func(actor builtin.Actor) builtin.WorkMerger {
				return personWriter(writer, actor)
			},
			// AND THE CROSS-PROJECT MOVE, the third sequence: a subtree
			// walked record by record onto another project's keys, which
			// needs the replicated estate for the walk exactly as the
			// two above do. Without it `move_work_item` is not
			// registered here at all, and the only way to move a
			// person's item was to ask a seat to.
			Moves: func(actor builtin.Actor) builtin.WorkMover {
				return personWriter(writer, actor)
			},
			// AND THE RANKED SEARCH. It reads, so it takes no actor —
			// the corpus is the same for everybody and there is nothing
			// to attribute — and without it the operator catalogue
			// listed a verb this surface could never register.
			Search: engine.WorkSearcher(e),
			// THE SAVED-VIEW WRITER, which only this surface has: a
			// view is furniture a person arranges, and no seat is
			// given the tools that reach it.
			ViewWriter: func(actor builtin.Actor) builtin.ViewWriter {
				return personWriter(writer, actor)
			},
			// AND THE CATALOGUE WRITER: the company's own vocabulary is
			// a person's to set, never a seat's to widen so its own
			// create succeeds.
			CatalogueWriter: func(actor builtin.Actor) builtin.CatalogueWriter {
				return personWriter(writer, actor)
			},
			// AND THE PERSON WRITER. Who may write what is the
			// tracker's own rule; what this surface supplies is the
			// identity it is judged against.
			PersonWriter: func(actor builtin.Actor) builtin.PersonWriter {
				return personWriter(writer, actor)
			},
			// AND THE INBOX READ. It takes no actor for the reason
			// Search takes none — it reads, and whose inbox is an
			// argument rather than an identity — and it is this
			// surface's alone beside the person writer, because a
			// seat has a mailbox rather than an inbox.
			Inbox: reader,
			// AND THE TRASH. A removal takes an item off every board in
			// the company and a restore puts it back at any age; neither
			// destroys anything, which is what separates both from the
			// purge the CLI guards with a typed confirmation. No seat
			// holds either — see internal/agent/builtin/worktrash.go.
			TrashWriter: func(actor builtin.Actor) builtin.TrashWriter {
				return personWriter(writer, actor)
			},
			// AND THE BOARD DRAG. A card's place in its project's order
			// is a person's arrangement, so no seat holds it — see
			// internal/agent/builtin/workplace.go — and the write
			// surface's rank route is an adapter over it.
			Placer: func(actor builtin.Actor) builtin.WorkPlacer {
				return personWriter(writer, actor)
			},
			// AND A PROJECT'S OWN SETTINGS. Unlike the five above,
			// this one is on every surface — declaring a tag is open
			// to every seat — and what an operator adds here is the
			// credential the archive facet asks for.
			ProjectWriter: func(actor builtin.Actor) builtin.ProjectWriter {
				return personWriter(writer, actor)
			},
			// AND THE PROJECT'S FILES, as a seat holds them: the rows
			// through the tracker and the bytes through this node's own
			// object store — all three or no file tool, which the store's
			// absence decides ([builtin.WorkDeps.Files]). Without them a
			// person's assistant was offered no file tool at all while
			// every seat beside it kept reports in its projects.
			Files: reader,
			FileWriter: func(actor builtin.Actor) builtin.FileWriter {
				return personWriter(writer, actor)
			},
			Objects: e.ObjectStore(),
			// WHOSE RECORD A LOGIN NAMES, which every person verb
			// resolves its name through: a login is never a seat, and
			// read literally a bound person's named a record nothing of
			// theirs is kept under.
			Holders: e,
			// THE ROSTER, so an operator's assistant is refused a
			// handle nobody has rather than silently filing work for
			// one — the same check every seat's tools make.
			Seats: func() []colleague.Seat {
				c := e.Company()
				if c == nil {
					return nil
				}
				// Contact ids resolve through the node's own chain, as
				// every seat's roster resolves them.
				return builtin.Corpus(c.Org, e.LookupSecret, e.WithheldContacts())
			},
			// AND THE THREE CHART SEAMS THE SEAT SURFACE HAS AND THIS
			// ONE WENT WITHOUT. Their absence was invisible and not
			// harmless: with no Leads, an operator filing an unassigned
			// task woke nobody at all — the lead fallback is what
			// catches exactly that task — and with no Units every
			// project this surface listed read as belonging to no team.
			Leads: engine.LiveLeads(e),
			Units: engine.LiveUnits(e),
			// A PERSON'S CREATE THAT NAMES NO PROJECT lands where their
			// seat's work does — the derivation every seat's surface and
			// the dashboard's `viewer` use, read per call against the
			// current chart. Keyed on the actor's handle, which is a seat
			// only for a person the identity directory binds to one: a
			// credential nobody is bound through has no seat, so its
			// create names a project or is refused naming the argument,
			// rather than a project guessed on its behalf. It was a stub
			// answering "" for everybody, so a bound founder's create was
			// refused while the viewer said it defaulted.
			DefaultProject: engine.LiveDefaultProject(e),
			// THE MENTION RESOLVER, which this surface went without: a
			// comment's @-mention is turned into a wake by the tracker's
			// recipients only when the writer resolved it, so an
			// operator writing "@alice can you take this" reached her
			// watchers and never her — while the tool's own description,
			// which their assistant reads, promised it would.
			Mentions: engine.LiveMentions(e),
			// AND WHERE AN ASKER MAY PROMISE TO REPORT A DECISION —
			// the chart's chat surfaces and unit channels, read per
			// call. Without it a person's `decision.inform` was refused
			// while a seat's, on the same chart, was not.
			Channels: engine.LiveChannels(e),
			// AND THE COMPANY'S CLOCK (ADR-0018), read per call: every
			// `due=friday`, `due=overdue` and relative date a person
			// wrote resolved on UTC while a seat's own resolved on the
			// company's zone, and an apply can move the clock between
			// two requests.
			Zone: e.Zone,
			// THE ROUTER'S SESSION FLOOR, which every write above raises.
			Await: e.AwaitEstate,
		}
		if !writable {
			work = readsOnly(work)
		}
	}
	pageReader, pagesReadable := engine.OperatorPages(e)
	pageWriter, pagesWritable := engine.OperatorPageWriter(e)
	if pagesReadable {
		kb = builtin.PageDeps{
			Reader: pageReader, Writer: pageWriter,
			Mentions: engine.LiveMentions(e),
			// A PERSON'S PAGE THAT NAMES NO CONTAINER lands in their own
			// team's space, on the rule a seat's own writes follow
			// ([engine.LiveSeatSpace]), keyed on the actor's handle as
			// DefaultProject is above: a person writes as their seat, and a
			// credential nobody is bound through names a container or is
			// refused naming the argument. With none, the inbox's "Save as
			// a page" was refused for every person who used it.
			DefaultContainer: engine.LiveSeatSpace(e),
			// THE SKILLS CONTAINER, so a person's write into it is asked
			// for admin, which a tool skill takes, rather than admitted on
			// the knowledge base alone — the store exempts every person,
			// because capability is not its question.
			SkillsContainer: engine.LiveSkillsContainer(e),
			Await:           e.AwaitEstate,
		}
		if !pagesWritable {
			// The zero estate.Pages the engine answers beside false is a
			// value, so as an interface it is not nil and would register
			// every page write tool over a writer that writes nothing.
			kb.Writer = nil
		}
	}
	return work, kb
}

// readsOnly is d with every per-actor seam cleared — each is a family of WRITE
// tools ([TestAPersonsSurfacesAreHandedEveryWriterSeam] reads the same set off
// the struct) — leaving the reads a node with no writer still serves.
//
// A seam added to [builtin.WorkDeps] is cleared here or a test fails
// ([TestReadsOnlyClearsEveryWriterSeam]), because a seam left set on a node
// with no writer is a tool whose every call reaches a writer that is nil.
func readsOnly(d builtin.WorkDeps) builtin.WorkDeps {
	d.Writer = nil
	d.ProjectWriter = nil
	d.Dependencies = nil
	d.Merges = nil
	d.Moves = nil
	d.ViewWriter = nil
	d.CatalogueWriter = nil
	d.PersonWriter = nil
	d.TrashWriter = nil
	d.Placer = nil
	d.FileWriter = nil
	return d
}

// ChatletDeps are chat's tool deps for the same surfaces [NativeToolDeps]
// builds the tracker's and the knowledge base's for, on the same terms: Actor
// and Authorize are the operator surface's to set per call, and every read and
// every write goes through this node's estate router ([engine.OperatorChat],
// [engine.OperatorChatWriter], [engine.OperatorChatSearch]).
//
// # A person's read state, whenever the rooms are the engine's
//
// The one half no seat's surface has: how far a person has read in each room,
// what they muted and when their do-not-disturb ends, which `list_channels`
// counts its badges against ([builtin.ChatDeps.ReadState]) and `mark_chat_read`
// writes ([builtin.ChatDeps.Cursors]). It is the fleet's coordination record
// rather than a record on the chat log, so it is wired WHEREVER THE ROOMS ARE
// THE ENGINE'S, a maintenance mode included: the mode forbids a node to
// publish a record, and has nothing to say about a compare-and-set in the
// coordination store — and every gesture the dashboard makes is a tool here
// (ADR-0024), so withholding it would leave a person on such a node no way to
// clear a badge at all.
//
// # The rooms, both halves or neither
//
// Unlike Worklet's and Doclet's tools, whose reads such a node still serves
// ([NativeToolDeps]): a node that hands out no chat writer — one in a
// maintenance mode — serves none of the tools that read or write the rooms,
// rather than reads answered beside writes refused on every press. The search
// goes with them, since a node that searches the rooms but cannot post in them
// is a surface no seat has.
func ChatletDeps(e *engine.Engine) builtin.ChatDeps {
	reader, readable := engine.OperatorChat(e)
	if !readable {
		return builtin.ChatDeps{}
	}
	reads := chatReads{store: e.Backends().Fleet}
	writer, writable := engine.OperatorChatWriter(e)
	searcher, searchable := engine.OperatorChatSearch(e)
	if !writable || !searchable {
		return builtin.ChatDeps{Cursors: reads, ReadState: reads.ReadState}
	}
	return builtin.ChatDeps{
		Reader: reader, Writer: writer, Search: searcher,
		// THE CHART CURRENT WHEN THE MESSAGE IS WRITTEN, for the reason a
		// work comment's mentions are resolved against it: these deps are
		// built per call, over an engine an apply moves underneath them.
		Mentions: engine.LiveMentions(e),
		// THE ROUTER'S SESSION FLOOR, which every write above raises — a
		// person's own message is in the room their next read draws.
		Await:   e.AwaitEstate,
		Cursors: reads, ReadState: reads.ReadState,
	}
}

// chatReadStore is the coordination store's half of a person's chat read state
// these deps reach: one read and one merge, and nothing else of the fleet's
// shared state, so a seam serving a person's cursors is given no reason to
// reach the activation pointer or the sealed secrets.
type chatReadStore interface {
	ChatRead(ctx context.Context, handle string) (coord.ChatReadState, error)
	AdvanceChatRead(ctx context.Context, handle string, delta coord.ChatReadDelta) (
		coord.ChatReadState, error)
}

// chatReads is a person's read state as the chat tools read and write it — the
// [builtin.ChatCursors] seam, and [builtin.ChatDeps.ReadState] through its
// ReadState.
//
// THE PERSON'S STATE AND NOTHING OF THE RECONCILE'S, both ways: the absence
// stamp the maintenance sweep keeps on a record
// ([coord.ChatReadState.AbsentSince]) is its machinery, so it is neither
// answered to a listing nor handed back from a flush.
//
// THE STORE'S ERROR IS RETURNED AS IT IS, never re-worded or wrapped: the tool
// tells a flush the merge refused ([coord.ErrInvalidReadState], the caller's
// to change) from one whose answer was lost ([coord.ErrWriteUnknown], which
// may have landed) and from a store that could not take it by the sentinel
// alone, and says the first in the merge's own words — re-worded, a refusal
// that can never land would read "try again". And a read that failed is never
// a zero state in its place: a zero state is somebody who has opened nothing,
// to whom every room's tail is unread.
type chatReads struct{ store chatReadStore }

var (
	_ builtin.ChatCursors                                        = chatReads{}
	_ func(context.Context, string) (coord.ChatReadState, error) = chatReads{}.ReadState
)

// ReadState is one person's read state, as a listing counts its badges by.
func (r chatReads) ReadState(ctx context.Context, handle string) (coord.ChatReadState, error) {
	state, err := r.store.ChatRead(ctx, handle)
	if err != nil {
		return coord.ChatReadState{}, err
	}
	state.AbsentSince = time.Time{}
	return state, nil
}

// AdvanceChatRead merges one flush into a person's read state.
func (r chatReads) AdvanceChatRead(ctx context.Context, handle string,
	delta coord.ChatReadDelta) (coord.ChatReadState, error) {

	state, err := r.store.AdvanceChatRead(ctx, handle, delta)
	if err != nil {
		return coord.ChatReadState{}, err
	}
	state.AbsentSince = time.Time{}
	return state, nil
}
