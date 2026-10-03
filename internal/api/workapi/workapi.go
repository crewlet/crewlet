// Package workapi is the HUMAN WRITE SURFACE over the company's own tracker
// and knowledge base: filing, editing, commenting on, arranging and taking
// work and pages out of circulation, from a browser or a script, as the person
// the request resolved to.
//
// # It is the SAME TOOLS a seat and the operator's assistant hold
//
// Every route that has a tool behind it is an ADAPTER over that tool — the one
// internal/agent/builtin registers into a seat's turn — reached through the
// operator surface's ONE DISPATCH (internal/api/operator), the same path the
// dashboard's act route and an operator's assistant take: the same catalogue,
// the same authority decision, the same refusal mapping and the same runtime
// audit record. What a route adds is only the HTTP shape: the path names the
// object, the body is the tool's own arguments, and the answer is the tool's
// own receipt under a status code.
//
// The alternative this replaces was the obvious one: a handler per route
// calling the tracker's writer directly. It would have been the THIRD
// implementation of "file an item" — the third place a field is trimmed, a
// default applied, a mention resolved and a refusal worded — and the first
// one a person uses. Two copies had already drifted on exactly those parts
// before this surface existed; the third would have been the one nobody's
// tests reached, because the tools' suites exercise the tools.
//
// A FEW ROUTES HAVE NO TOOL, and are the ones a seat is never given: rewriting
// one's own remark on a work item, purging a work item, and the knowledge
// base's four destructive verbs and its rename. Those call the domain's writer
// themselves, decide first through the SAME table —
// [authz.ActionWorkCommentEdit], [authz.ActionWorkPurge],
// [authz.ActionPageRename], [authz.ActionPageTrash], [authz.ActionPageRestore],
// [authz.ActionPagePurge] and [authz.ActionPageCommentRemove] — answer through
// the same refusal mapping ([operator.Fail]), and publish the same
// `operator_acted` record the dispatch publishes for a tool
// ([operator.AuditGesture]), so a verb with no tool is no less recorded. The
// four page verbs had rules and no caller at all until this surface; the work
// purge had a route of its own under /work/{id}/purge with an operator check
// written beside it, which this replaces. Placing a card on a board used to be
// one of them; it is `place_work_item` now, and its route an adapter over it.
//
// # Authority is decided ONCE, and read identically everywhere
//
// Every route is mounted through [authz.Router] with a policy. Where the
// object is knowable from the PATH — the project in /work/projects/{key} —
// the route decides the verb itself. Where it needs a STORED ROW — which
// project a work item is filed under, which container a page is in, who wrote
// a comment, whose record the name in /work/people/{handle} is (a login is its
// holder's, which only the identity directory can say) — the route admits on
// the weakest honest precondition and the verb is decided once the row is
// read: by the tool's own ask, or by this package for a verb that has no tool.
//
// A refusal is WORDED by [builtin.Refusal] from the error [builtin.DecisionError]
// makes of the decision — the same two functions the tools' own gate uses — so
// "you may not" reads identically on a seat's turn, in the operator's
// assistant and here, and it is ANSWERED in the engine's envelope with the
// rule's reason and the grants that would have admitted the caller
// ([operator.RefuseDecision]). A route that composed its own sentence would be
// where the three first disagreed.
//
// # The status codes, and why each is its own
//
// They are the operator surface's one refusal mapping ([operator.Fail]), so a
// refusal reads the same here as on the act route; the `error` of a refused
// call is its refusal CLASS's own spelling.
//
//   - 400 — the request is the caller's to change: a body the verb does not
//     take, or an `Idempotency-Key` that is not an operation id.
//   - 401 — nobody presented a credential.
//   - 403 `unauthorized` — a credential this node knows, refused by the
//     authority table, carrying the rule's `reason` and the `grants` that
//     would have admitted it (`step_up_required` where only a fresher proof
//     is missing); `forbidden` — a rule of the domain's own.
//   - 503 with Retry-After — this node could not decide (the identity estate
//     or the chart could not be read), or could not establish the outcome.
//   - 503 `no_active_revision` — this node has not been handed a company yet,
//     so neither half is up; the Retry-After is the reconcile poll that
//     brings it one, and the halves come up with its first revision, with no
//     restart ([Options.Halves]).
//   - 404 — the item, page or comment does not exist, or a purge destroyed it;
//     and `no_route`, in the mux's own bytes, for a route of a half this
//     company does not run — its tracker or its wiki is a vendor's.
//   - 409 — somebody changed it since it was read (`stale_version`), a race
//     lost (`conflict`), a title taken (`exists`), or a state the request
//     collided with (`reassignment_budget`, `inbox_full`, …).
//   - 422 `invalid` — the domain refused an argument on its own rules, or a
//     destructive verb's `?confirm=` does not name what it would destroy; the
//     detail is its own sentence.
//   - 200 / 202 / 503 — a write that was made answers the tool's own outcome:
//     applied, pending with the position to read at, or unknown carrying the
//     operation key a retry must reuse — and, where this node's operation
//     ledger cannot vouch for it, `unvouched` and no Retry-After, because the
//     same request asked here answers the same way.
//
// # An argument the tool does not read is refused, never dropped
//
// A route's body IS the tool's arguments, and the tool reads what its schema
// declares and nothing else — so an argument it does not declare would be
// dropped, and a request answered as though it had asked for less. The tools'
// own gate refuses one by name ([builtin.ErrUndeclaredArgument]) on every
// surface a builtin is called through, and a tool-backed route answers that
// refusal 400; a facet route narrows the list further ([only]). This surface
// used to hold its own copy of the check, which left a seat's turn and the
// operator's assistant dropping what the route refused.
//
// # A retry is idempotent under the caller's own key
//
// An `Idempotency-Key` header becomes the operation SEED every write the
// request makes derives its id from — the tracker's through the actor's work
// key, the knowledge base's through the actor's op key — so a request retried
// after `unknown` under the same key lands once. Without one, a fresh key is
// minted per request and handed back as `op_id`. Either way the key is an
// operation id in the engine's grammar, because every id derived from it
// carries its instant and the ledger vouches for a retry by that instant — see
// [operationKey].
//
// EVERY ID IS ALSO BOUND TO WHAT THE REQUEST ASKS, so the same key is the same
// operation only for the same request: sent with another one, it derives
// operations of its own, which land as asked. Bound to the key alone, the
// ledger answered the second request with the first one's outcome — `applied`,
// with nothing of it written — see [keyedOp], builtin's own binding for the
// tools, and the knowledge base's ([pages.Store], which binds what each write
// says).
//
// # What is NOT here
//
// Sprints and goals. The design this surface was drawn from listed
// `PUT /work/projects/{key}/sprints` and `PUT /work/goals/{id}`, and the
// tracker has neither: both were dropped from the domain (the replicated
// estate's migrations 0013 and 0015), so there is nothing to route, and a
// route answering for a verb the domain does not have would be a promise the
// engine cannot keep.
package workapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

var log = logging.Get("api.workapi")

// MaxBodyBytes bounds one request body.
//
// DERIVED FROM THE LARGEST THING A BODY CARRIES: a page at the knowledge
// base's own cap ([pages.MaxBody]), doubled because JSON escaping a prose body
// heavy in quotes and backslashes can double it, plus 64 KiB for the rest of
// the envelope — labels, custom fields, a message. A body under it but over
// the domain's own cap is refused by the domain, naming the field, which is
// the more useful refusal; this bound exists so a body nothing could accept is
// not read into memory first.
const MaxBodyBytes = 2*pages.MaxBody + 64<<10

// decisionRead is the level a route reads a row at before deciding on it.
//
// THE LEVEL THE TOOLS THIS SURFACE SERVES READ AT, so a decision taken here
// and the tool's own read behind it see the same rows: a route that decided
// on a staler copy than the write then acts on would be deciding about a
// different object.
var decisionRead = statelog.Freshness{
	Level: statelog.DefaultReadLevel(statelog.SurfaceSeat),
}

// TrackerWriter is the tracker's write side for the two gestures no tool
// makes — rewriting one's own remark and a work item's purge — bound to one
// actor. A rank move is NOT one of them: the board drag is the catalogue's
// `place_work_item`, reached through the dispatch like every other tool.
type TrackerWriter interface {
	PurgeTask(ctx context.Context, opID, id, project, reason string) (
		tracker.WriteResult, error)
	EditComment(ctx context.Context, opID, taskID, project, commentID,
		body string, notify *tracker.Notify) (tracker.WriteResult, error)
}

// PageStore is the knowledge base's write side for the verbs no tool makes.
type PageStore interface {
	Rename(ctx context.Context, actor pages.Actor, pageID, title string,
		quiet bool) (pages.Written, error)
	Trash(ctx context.Context, actor pages.Actor, pageID string) (pages.Written, error)
	Restore(ctx context.Context, actor pages.Actor, pageID string) (pages.Written, error)
	Purge(ctx context.Context, actor pages.Actor, pageID, reason string) (
		pages.Written, error)
	RemoveComment(ctx context.Context, actor pages.Actor, pageID, commentID string,
		authority pages.CommentAuthority) (pages.Written, error)
}

// Halves are the two halves this surface serves, as one request finds them:
// the readers a route reads a row through before it decides on it — the SAME
// deps the operator surface builds its catalogue from — and the two writers
// for the gestures no tool makes. The tools themselves are reached through
// [Options.Operator], which reads its own halves from the same constructor.
type Halves struct {
	Work  builtin.WorkDeps
	Pages builtin.PageDeps

	// Tracker is the tracker's writer bound to one actor, for the gestures
	// no tool makes. Nil leaves the work half unserved.
	Tracker func(actor builtin.Actor) TrackerWriter

	// PageStore is the knowledge base's writer, for the verbs no tool makes.
	// Nil leaves the pages half unserved.
	PageStore PageStore
}

// servesWork reports whether these halves hold the tracker's whole surface.
func (h Halves) servesWork() bool {
	return h.Work.Reader != nil && h.Work.Writer != nil && h.Tracker != nil
}

// servesPages reports whether these halves hold the knowledge base's.
func (h Halves) servesPages() bool {
	return h.Pages.Reader != nil && h.Pages.Writer != nil && h.PageStore != nil
}

// Options configure the surface.
type Options struct {
	// Halves is what this surface serves NOW, read at the start of every
	// request — never once, when the surface is built.
	//
	// # Why a source and not the halves themselves
	//
	// A node's tracker and knowledge base come up with its FIRST COMPANY, and
	// a node may meet that at an apply, long after its API is serving: taken
	// once, the halves of a node that booted with no company were none, and
	// the surface served nothing until a restart — the one gap left in "a
	// company can be bootstrapped live". Read per request, the routes are
	// mounted once and serve the moment the halves exist.
	//
	// It answers FALSE for a node that has not been handed a company yet,
	// and every route then answers `503 no_active_revision` with the
	// reconcile poll as its Retry-After ([httpjson.NoActiveRevision]): the
	// halves are coming, and a client that waits is served. A half the
	// company does not run — its tracker or its wiki is a vendor's — is
	// answered as the route's absence would be ([httpjson.NoRoute]), since
	// no wait brings it: switching a backend takes a restart.
	//
	// REQUIRED.
	Halves func() (Halves, bool)

	// Chart is what every decision on this surface reads, routes and tools
	// alike.
	//
	// THE CHART THE OPERATOR'S DECISION IS BUILT OVER, and its builder
	// hands both the same value: the tools' decision is [builtin.Decide]
	// over it, so a route deciding on another chart and the tool behind it
	// would answer one question two ways. REQUIRED — [authz.NoChart] says
	// "decide on grants alone" out loud where a nil would read as an
	// omission.
	Chart authz.Chart

	// Operator is the operator surface's ONE DISPATCH, which every
	// tool-backed route calls — the same path the act route and an
	// operator's assistant take, so the catalogue, the authority decision,
	// the refusal and the audit record are one implementation whichever
	// surface a person used. REQUIRED.
	Operator Dispatcher

	// Audit is where a verb with no tool publishes the `operator_acted`
	// record the dispatch publishes for a tool ([operator.AuditGesture]):
	// the node's own queue. REQUIRED, for [operator.Options.Audit]'s reason.
	Audit operator.AuditPublisher
}

// Dispatcher is the operator surface's dispatch, as this surface calls it.
type Dispatcher interface {
	Dispatch(ctx context.Context, c operator.Call) (tools.Result, error)
	DispatchRecord(ctx context.Context, c operator.Call, name string,
		write operator.RecordWrite) (tools.Result, error)
}

// Service is the surface.
type Service struct {
	halves   func() (Halves, bool)
	chart    authz.Chart
	operator Dispatcher
	audit    operator.AuditPublisher
}

// New builds the surface.
//
// ALWAYS A SURFACE, never the nil an empty one used to be: which halves exist
// is a question a request asks ([Options.Halves]), and a route of a half the
// company does not run answers in the very bytes its absence would have.
func New(opts Options) (*Service, error) {
	if opts.Halves == nil {
		return nil, errors.New("workapi: no halves: every route reads the " +
			"tracker and knowledge base this node serves as the request finds them")
	}
	if opts.Chart == nil {
		return nil, errors.New("workapi: no chart: every decision here reads " +
			"one — pass authz.NoChart to decide on grants alone")
	}
	if opts.Operator == nil {
		return nil, errors.New("workapi: no operator dispatch: every " +
			"tool-backed route calls the one the act route and an operator's " +
			"assistant call")
	}
	if opts.Audit == nil {
		return nil, operator.ErrNoAudit
	}
	return &Service{
		halves: opts.Halves, chart: opts.Chart,
		operator: opts.Operator, audit: opts.Audit,
	}, nil
}

// Routes registers the surface on a mux.
//
// EVERY ROUTE CARRIES ITS OWN POLICY through [authz.Router], which is the only
// reader of the matched pattern — see the package doc for which routes decide
// the verb there and which admit on a precondition and decide once they have
// read the row.
//
// BOTH HALVES ARE MOUNTED, whatever this node serves when it is built: each
// route asks for its half when a request arrives ([Service.on]).
func (s *Service) Routes(mux authz.Mux) error {
	router := authz.NewRouter(mux, s.guard).Refusing(s.refuse)
	var failures []error
	mount := func(pattern string, p authz.Policy, h http.HandlerFunc) {
		if err := router.Handle(pattern, p, h); err != nil {
			failures = append(failures, err)
		}
	}
	s.workRoutes(mount)
	s.pageRoutes(mount)
	return errors.Join(failures...)
}

// served is the surface for ONE request: the Service's decisions, over the
// halves this node served when the request arrived.
//
// AN ARGUMENT, not state the Service keeps: two requests arriving either side
// of a node's first company must each be served by the halves they found, and
// a handler that re-read them part of the way through could decide a verb on
// one half and write through another.
type served struct {
	*Service
	Halves
}

// servedHandler is one route's body, over the halves its request found.
type servedHandler func(*served, http.ResponseWriter, *http.Request)

// on is the handler a route is mounted with: it reads the halves this node
// serves now and hands the route's body the request's own, or answers why it
// cannot — `503 no_active_revision` where the halves are not up yet, and the
// route's absence where this company does not run the half at all.
func (s *Service) on(serves func(Halves) bool, h servedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		halves, up := s.halves()
		if !up {
			httpjson.NoActiveRevision(w, httpjson.Detail{
				"detail": httpjson.NativeHalvesNotUp})
			return
		}
		if !serves(halves) {
			httpjson.NoRoute(w, r)
			return
		}
		h(&served{Service: s, Halves: halves}, w, r)
	}
}

// onWork and onPages mount a route of one half.
func (s *Service) onWork(h servedHandler) http.HandlerFunc {
	return s.on(Halves.servesWork, h)
}

func (s *Service) onPages(h servedHandler) http.HandlerFunc {
	return s.on(Halves.servesPages, h)
}

// mounter is the one registration shape both halves use.
type mounter func(pattern string, p authz.Policy, h http.HandlerFunc)

// kinded is a policy naming only its object's KIND: the colleague writes,
// decided by capability, and the preconditions a row-decided route admits on.
func kinded(a authz.Action, kind authz.ObjectKind) authz.Policy {
	return authz.Policy{Action: a, Object: func(*http.Request) authz.Object {
		return authz.Object{Kind: kind}
	}}
}

// guard is what [authz.Router] asks before a handler runs: the shared
// [authz.ContextGuard] over this surface's chart, whose one clause worth
// getting right — a caller this node could not resolve is UNKNOWN, never the
// zero principal — is written once there rather than once per surface.
func (s *Service) guard(r *http.Request, p authz.Policy) authz.Decision {
	return authz.ContextGuard(s.chart)(r, p)
}

// refuse renders what the router did not admit, in the operator surface's
// envelope — the one a tool's own refusal is answered in.
//
// AND RECORDS IT, as [Service.decide] records a refusal it takes after a
// read: a route that decides the verb on its path refuses BEFORE the tool or
// the writer runs, so without this the same refusal was recorded when a
// person met it through the act route or their assistant — where the tool's
// own gate refuses it — and was not when they met it here. Nobody resolved
// is not recorded: there is nobody to name, and the guard's own trail counts
// a refused credential.
func (s *Service) refuse(w http.ResponseWriter, r *http.Request, p authz.Policy,
	d authz.Decision) {

	if _, how := iam.From(r.Context()); how == iam.Resolved {
		operator.AuditRefusal(r.Context(), s.audit, types.TransportWork,
			string(p.Action), "", d.Unknown())
	}
	operator.RefuseDecision(w, r, p.Action, d)
}

// decide takes one decision this surface makes after reading a row, and
// renders the refusal when there is one. It reports whether to go on.
//
// A REFUSAL HERE IS A CALL REFUSED, and it is recorded as the dispatch records
// a tool's gate refusing one ([operator.AuditRefusal]): the verb a person
// asked for and was not allowed is exactly what an audit is opened to find,
// whether the verb had a tool behind it or not.
func (s *Service) decide(w http.ResponseWriter, r *http.Request,
	action authz.Action, object authz.Object) (authz.Decision, bool) {

	principal, how := iam.From(r.Context())
	var d authz.Decision
	if how == iam.Unknown {
		d = authz.Decision{Err: iam.Reason(r.Context())}
	} else {
		d = authz.Decide(r.Context(), principal, action, object, s.chart, time.Now())
	}
	if d.Unknown() || !d.Allowed {
		if how == iam.Resolved {
			operator.AuditRefusal(r.Context(), s.audit, types.TransportWork,
				string(action), "", d.Unknown())
		}
		operator.RefuseDecision(w, r, action, d)
		return d, false
	}
	return d, true
}

// unavailableFor is a 503 for a READ this surface took before a decision, whose
// CAUSE says whether, and when, to come back — [statelog.RetryAfter]'s rule,
// which is every surface's.
//
// A REFUSAL WAITING CANNOT CLEAR CARRIES NO Retry-After: a read refused by a
// node holding a record it cannot decode answers the same however often it is
// asked. A refusal that derived a hint says that; anything else is
// [authz.RetryUndecidedSeconds], the scale of this node applying one more batch
// or its chart view catching up.
func unavailableFor(w http.ResponseWriter, cause error, detail string) {
	httpjson.UnavailableWith(w, httpjson.CodeUnavailable,
		httpjson.RetrySeconds(statelog.RetryAfter(cause,
			authz.RetryUndecidedSeconds*time.Second)),
		httpjson.Detail{"detail": detail})
}

// ---- calling a tool ---------------------------------------------------- //

// operationKey is the seed every write this request makes derives its
// operation id from: the caller's own where they sent one, and fresh where
// they did not — which is then handed back, so a retry can send it. It answers
// false once it has refused a key that is not one ([opkey.Key]).
//
// Every id a write derives from the key carries the KEY'S instant — the
// tracker's through [builtin.Actor.WorkSince], the knowledge base's inside
// [pages.Store] — which is what the publisher vouches for a retry by; the
// knowledge base refuses a key outside the grammar itself ([pages.ErrInvalid]
// on `actor.op_key`), so without the check every page write through this
// surface was refused.
//
// BARE, with no name: the key is a SEED rather than the id of any one write —
// each write derives its own, named for its verb and its object — so a name
// here would label nothing in the ledger.
func operationKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	return opkey.Key(w, r, time.Now())
}

// keyedOp is the operation id of one write this surface makes ITSELF — a
// remark's edit, a purge — derived from the request's key, its verb,
// the object it is about and WHAT THE REQUEST ASKS of it.
//
// The verb and the object are in the identity, so the one key covers every
// record a request writes without two of them collapsing into one; the verb is
// the id's name, so a reader of the ledger finds it by what it did; and the id
// carries the KEY'S OWN INSTANT, so the ledger vouches for a retry of it
// exactly as long as it vouches for the key. It was `rank-<task>-<key>` and the
// like, which carried no instant at all — see [operationKey] for what that
// answered once the ledger had swept.
//
// # The request's arguments are in the operation
//
// Because the ledger answers an operation it already holds BEFORE the write is
// decided ([statelog.Result.Collapsed]), an id named by the verb and the object
// alone made the same key sent with another request — a remark rewritten
// again, a purge for another reason — the FIRST request's operation:
// answered `applied` from the ledger, with nothing of the second written. That
// is a change reported as made and silently dropped, the one answer a retry
// key must never produce. With the arguments in the identity the same request
// is the same operation, which is what a retry is, and any other request under
// the key is an operation of its own that lands as asked — the rule the tools
// behind the other routes already derive by ([builtin] puts a digest of a
// call's arguments in every id it derives from a key), so the two halves of
// this surface cannot answer one key two ways.
//
// The verb is a NAME, so it never holds a dot, which in the grammar begins a
// gesture's step ([statelog.StepOpID]).
func keyedOp(key, verb, object string, args map[string]any) string {
	at, _ := statelog.OpMintedAt(key)
	return statelog.DeriveOpID(at, verb, keyedOpNamespace, key, verb, object,
		turnctx.ArgsDigest(args))
}

// keyedOpNamespace keeps [keyedOp]'s ids apart from every other derivation
// over the same key — builtin's for the tool-backed routes, the knowledge
// base's own inside it. FIXED for the life of the format: a new one would make
// a retry that straddles the change a second write.
const keyedOpNamespace = "crewlet.workapi"

// seed puts the request's operation key on a work actor, with the instant it
// was minted at, for a gesture this surface makes without a tool.
func seed(actor *builtin.Actor, key string) {
	actor.WorkKey = key
	actor.WorkSince, _ = statelog.OpMintedAt(key)
}

// actor is the request's work actor, carrying its operation key.
func (s *Service) actor(w http.ResponseWriter, r *http.Request, key string) (
	builtin.Actor, bool) {

	actor, err := builtin.PrincipalActor(r.Context(), nil)
	if err != nil {
		// UNREACHABLE behind the router, which refuses an unresolved
		// principal before a handler runs; refused rather than trusted
		// if it somehow is not, because a write with no author is the
		// one thing this surface must never record.
		operator.RefuseDecision(w, r, "", authz.Decision{Err: err})
		return builtin.Actor{}, false
	}
	seed(&actor, key)
	return actor, true
}

// pageActor is [Service.actor] for the knowledge base, carrying the request's
// key, which the store binds to what each write says.
func (s *Service) pageActor(w http.ResponseWriter, r *http.Request,
	key string) (pages.Actor, bool) {

	actor, err := builtin.PrincipalPageActor(r.Context(), nil)
	if err != nil {
		operator.RefuseDecision(w, r, "", authz.Decision{Err: err})
		return pages.Actor{}, false
	}
	actor.OpKey = key
	return actor, true
}

// call runs one tool as the request's principal, through the operator
// surface's dispatch, and answers its receipt.
func (s *served) call(w http.ResponseWriter, r *http.Request, verb string,
	args map[string]any) {

	if !operator.NoOperationArg(args) {
		operator.RefuseOperationArg(w, verb)
		return
	}
	key, ok := operationKey(w, r)
	if !ok {
		return
	}
	result, err := s.operator.Dispatch(r.Context(), operator.Call{
		Transport: types.TransportWork, Key: key, Tool: verb, Args: args,
	})
	s.answerDispatched(w, r, key, verb, operator.About(verb, args), result, err)
}

// answerDispatched renders what the dispatch answered one call with.
func (s *served) answerDispatched(w http.ResponseWriter, r *http.Request, key,
	verb string, about httpjson.Detail, result tools.Result, err error) {

	switch {
	case errors.Is(err, operator.ErrNotUp):
		// THE DISPATCH READS ITS HALVES PER CALL, as this surface does:
		// a node that met its company between the two reads is up for
		// the next request.
		httpjson.NoActiveRevision(w, httpjson.Detail{
			"detail": httpjson.NativeHalvesNotUp})
	case errors.Is(err, operator.ErrNotServed):
		// A VERB THIS NODE DOES NOT SERVE — its backend is absent — is a
		// 404 rather than a refusal: nothing here could ever make it
		// succeed.
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound,
			map[string]string{"detail": verb + " is not served by this node"})
	case err != nil:
		// THE CALLER'S CONTEXT ENDED — internal/mcp's own meaning of a
		// tool error — and a write may have landed before it did, so
		// this is an UNKNOWN outcome rather than a refusal: whether it
		// landed is exactly what a retry under the same key resolves,
		// and a client reading a refusal here would make the change
		// again under a fresh one.
		operator.Interrupted(w, r, key, verb, err)
	default:
		answerTool(w, key, about, result)
	}
}

// answerTool renders one tool's receipt, or its failure beside about.
func answerTool(w http.ResponseWriter, key string, about httpjson.Detail,
	result tools.Result) {

	if result.Failed {
		operator.Fail(w, result.Cause, result.Output, key, about)
		return
	}
	var receipt map[string]any
	if err := json.Unmarshal([]byte(result.Output), &receipt); err != nil {
		// EVERY WRITE TOOL ANSWERS JSON, so this is a tool that changed
		// shape rather than a caller's mistake.
		log.Warn("api_work_receipt_unreadable", "error", err)
		httpjson.FailWith(w, http.StatusInternalServerError,
			httpjson.CodeInternalError, map[string]string{"detail": result.Output})
		return
	}
	subtreeStoppedHere(receipt)
	moveStoppedHere(receipt)
	// A RECEIPT CARRIES NO LEDGER FACTS: a tool reports an outcome nobody
	// can establish as a FAILED result carrying [builtin.UnknownOutcome],
	// which [operator.Fail] answers, so an `unknown` still found here is a
	// nested half of a two-record tool, whose own detail says what landed.
	answer(w, key, outcomeOf(receipt), false, receipt)
}

// subtreeStoppedHere rewrites a stopped subtree's instruction for THIS
// surface.
//
// # Why the sentence is replaced rather than passed on
//
// remove_work_item and restore_work_item answer a subtree walk that stopped
// part of the way with the ROOT's receipt and a `subtree_stopped` sentence
// saying how to finish — written for the operator's assistant, where a new
// operation is made by leaving `op_id` out of the call. Here the operation is
// the request's `Idempotency-Key`, and a client told to call a tool "WITHOUT
// an op_id" is told about an argument this route refuses. What finishes the
// walk on this surface is the same request under a NEW key, or none: a new
// operation decides every task afresh — a task already where the gesture
// leaves it is nothing to do — while the same key finishes it only where this
// node's ledger can vouch for the step it stopped at, which nothing in the
// receipt says. So the one instruction right in both cases is given, and the
// counts it is formed from are the receipt's own fields, never words read out
// of the tool's sentence.
func subtreeStoppedHere(receipt map[string]any) {
	said, stopped := receipt["subtree_stopped"]
	if !stopped {
		return
	}
	// WHY IT STOPPED is in the tool's own sentence, beside the instruction
	// this replaces — kept for whoever reads this node's log, since the
	// answer's is the instruction a client can act on.
	log.Info("api_work_subtree_stopped", "item", receipt["key"], "tool_detail", said)
	done := "removed"
	if restored, _ := receipt["restored"].(bool); restored {
		done = "restored"
	}
	key, _ := receipt["key"].(string)
	followed, _ := receipt["subtree_followed"].(float64)
	total, _ := receipt["subtree_total"].(float64)
	receipt["subtree_stopped"] = fmt.Sprintf("%s is %s, but only %d of the %d "+
		"tasks that go with it followed before the walk stopped. Do not report "+
		"it as done. To finish it, send this request again under a NEW %s, or "+
		"none: a new operation decides each task afresh, so whatever has not "+
		"followed yet goes and what already has is left where it is.",
		key, done, int(followed), int(total), opkey.Header)
}

// moveStoppedHere rewrites a stopped move's instruction for THIS surface.
//
// # Why the remedy is the SAME key, where a subtree's is a new one
//
// move_work_item answers a move whose root landed in the target and whose walk
// over the subtree stopped with the root's receipt and a `move_stopped`
// sentence written for a seat or the operator's assistant — call again with
// the same arguments, or the same `op_id`, neither of which this route takes.
// What makes the same operation here is the same `Idempotency-Key`: every id
// the tool derives is a function of it, so the move's own ledger answers the
// root and the walk carries the rest. A NEW key is refused, which is where a
// move differs from a removal: the root is already in the target and that
// operation's ledger never put it there, which is exactly how somebody else's
// move looks. And the root stays marked mid-move while anything is left, so
// the tracker duty finishes the walk on its own — the remedy where this node
// cannot vouch for the step the walk stopped at, since the same key here stops
// there again. Formed from the receipt's own fields, never from words read out
// of the tool's sentence.
//
// THE KEY IS THE ANSWER'S `op_id` AS WELL AS THE REQUEST'S, and the sentence
// says so: a request that sent no key had one minted for it ([operationKey]),
// so "the SAME key" told that client to repeat a header it never held — and a
// resend without one is a new operation, which is exactly what is refused.
// The answer carries the key as `op_id` whichever way it was made, and
// [operator.UnknownOutcome] words its own retry the same way.
func moveStoppedHere(receipt map[string]any) {
	said, stopped := receipt["move_stopped"]
	if !stopped {
		return
	}
	log.Info("api_work_move_stopped", "item", receipt["moved_from"], "tool_detail", said)
	from, _ := receipt["moved_from"].(string)
	project, _ := receipt["project"].(string)
	as := ""
	if key, _ := receipt["key"].(string); key != "" {
		as = " as " + key
	}
	followed, _ := receipt["subtree_followed"].(float64)
	total, _ := receipt["subtree_total"].(float64)
	duty := "the tracker duty finishes the move on its own once nobody is " +
		"walking it"
	sameKey := "the SAME " + opkey.Header + " — send op_id back as that " +
		"header, which is this request's key whether it sent one or not"
	next := fmt.Sprintf("To finish it, send this request again under %s. A "+
		"new key is refused, since the item is already in %s. Or leave it: %s.",
		sameKey, project, duty)
	waits, _ := receipt["move_waits_for"].(string)
	unvouched, _ := receipt["move_unvouched"].(bool)
	switch {
	case unvouched:
		next = fmt.Sprintf("This node cannot vouch for the task the walk "+
			"stopped at, so the same request here stops there again: send it "+
			"to another node under %s, or leave it: %s.", sameKey, duty)
	case waits != "":
		next = fmt.Sprintf("%s is in the trash under it, and the move waits for "+
			"it: restore it (POST /work/items/%s/restore) or purge it, then send "+
			"this request again under %s. Or leave it: once it is restored, %s.",
			waits, waits, sameKey, duty)
	}
	receipt["move_stopped"] = fmt.Sprintf("%s moved to %s%s, but only %d of the "+
		"%d tasks under it followed before the walk stopped; the rest are still "+
		"in their old project. Do not report it as done. %s", from, project, as,
		int(followed), int(total), next)
}

// outcomeOf is a receipt's outcome: its own, or — for a tool that made two
// records, as write_project's tag and policy halves are — the LESS CERTAIN of
// the ones it nests ([statelog.LessCertain]), because the answer is about the
// whole request. A receipt that nests none appended nothing, and is applied.
func outcomeOf(receipt map[string]any) statelog.Outcome {
	if held, ok := receipt["outcome"].(string); ok && held != "" {
		return statelog.Outcome(held)
	}
	var least statelog.Outcome
	for _, v := range receipt {
		nested, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if held, ok := nested["outcome"].(string); ok {
			least = statelog.LessCertain(least, statelog.Outcome(held))
		}
	}
	if least == "" {
		return statelog.OutcomeApplied
	}
	return least
}

// answer writes a write that was MADE, under its outcome.
//
// THE THREE SUCCESSES ARE THREE ANSWERS, for chartapi's reason: only
// `applied` means the next read on this node sees the write, so only it is a
// 200. `pending` is durable and not yet here — 202 with the position to read
// at. `unknown` is a write this node cannot account for —
// [operator.UnknownOutcome].
func answer(w http.ResponseWriter, key string, outcome statelog.Outcome,
	unvouched bool, body map[string]any) {

	if body == nil {
		body = map[string]any{}
	}
	body["op_id"] = key
	switch outcome {
	case statelog.OutcomePending:
		body["detail"] = "this change is durable and every node will apply " +
			"it; this one has not yet. Read at the position above to see it."
		httpjson.Write(w, http.StatusAccepted, body)
	case statelog.OutcomeUnknown:
		operator.UnknownOutcome(w, key, unvouched, "", body)
	default:
		httpjson.Write(w, http.StatusOK, body)
	}
}

// failErr answers the error a writer this surface called itself returned
// through the operator surface's one refusal mapping, the writer's own refusal
// classified as a tool's would be and worded for whoever reads it
// ([operator.FailWrite]) — a fault in fixed words, its error in the log.
func failErr(w http.ResponseWriter, r *http.Request, err error, key string) {
	operator.FailWrite(r.Context(), w, err, key, nil)
}

// readFailed answers a read taken before a decision that could not be served.
//
// NOT FOUND IS A 404 AND EVERYTHING ELSE IS THIS NODE: a row that is absent
// and a row this node could not read are opposite answers, and a read failure
// rendered as "no such item" is how a caller comes to file a duplicate.
//
// AND "THIS NODE" IS TWO THINGS, told apart by the rule every tool classes a
// failure of its own by ([builtin.Condition]). A CONDITION waiting clears — a
// read the state log refused, a store closed while it adopts a peer's
// snapshot — is `503` in the condition's own words, with the refusal's hint:
// composed for a caller, and saying whether waiting helps. Anything else is a
// FAULT: `500 internal_error`, its words to the log at error, because nobody
// but whoever runs the node can act on it. Both were one 503 carrying the
// error's text, so a store this node could not read told a client to come
// back in two seconds, and handed it a database path to read while it waited.
func readFailed(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, tracker.ErrNoTask) || errors.Is(err, pages.ErrNotFound) {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound,
			map[string]string{"detail": err.Error()})
		return
	}
	if why, ok := builtin.Condition(err); ok {
		unavailableFor(w, err, "this node could not read what the decision is "+
			"about: "+why)
		return
	}
	log.ErrorContext(r.Context(), "api_work_read_failed", "error", err.Error())
	httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
}

// positionOf is a log position as a receipt states it: absent for a write that
// appended nothing.
func positionOf(at statelog.Position) any {
	if at.IsZero() {
		return nil
	}
	return at.String()
}

// ---- request bodies ---------------------------------------------------- //

// readArgs decodes a request body as a tool's arguments.
//
// AN EMPTY BODY IS NO ARGUMENTS, which is what a DELETE and a restore send.
// Anything else must be one JSON object: the arguments are the tool's own, by
// its own names, so there is nothing here to translate and nothing to drop.
func readArgs(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	raw, err := httpjson.ReadBody(w, r, MaxBodyBytes)
	if err != nil {
		httpjson.Refuse(w, err)
		return nil, false
	}
	args := map[string]any{}
	if strings.TrimSpace(string(raw)) == "" {
		return args, true
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": "the body must be one JSON object of " +
				"the verb's own arguments: " + err.Error()})
		return nil, false
	}
	return args, true
}

// fromPath puts the path's name for the object into the arguments, refusing a
// body that names a different one.
//
// THE PATH WINS AND A DISAGREEING BODY IS REFUSED rather than overwritten: a
// caller who sent `PATCH /work/items/ENG-1` with `"item": "ENG-2"` has a bug,
// and writing either one would be guessing which half of it they meant.
func fromPath(w http.ResponseWriter, args map[string]any, field, value string) bool {
	if held, ok := args[field]; ok {
		if s, _ := held.(string); strings.TrimSpace(s) != value {
			httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
				map[string]string{"detail": fmt.Sprintf("the body names %s %v "+
					"and the path names %q; the path is what this route acts on, "+
					"so leave %s out of the body", field, held, value, field)})
			return false
		}
	}
	args[field] = value
	return true
}

// only refuses a body carrying an argument this route does not take.
//
// A FACET ROUTE — the tags of a project, the dependencies of an item — is a
// narrower door onto a wider tool, and a body that reached past it would make
// the route's name a lie about what the request changed.
func only(w http.ResponseWriter, args map[string]any, allowed ...string) bool {
	for field := range args {
		if !contains(allowed, field) {
			httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
				map[string]string{"detail": fmt.Sprintf("this route takes %s; "+
					"%q is another route's", strings.Join(allowed, ", "), field)})
			return false
		}
	}
	return true
}

func contains(list []string, s string) bool {
	for _, held := range list {
		if held == s {
			return true
		}
	}
	return false
}

// ifMatch folds an `If-Match` header into the argument a tool takes for it,
// refusing a request that states two different versions.
func ifMatch(w http.ResponseWriter, r *http.Request, args map[string]any,
	field string) bool {

	header := strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), `"`)
	if header == "" {
		return true
	}
	version, err := strconv.ParseUint(header, 10, 64)
	if err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": "If-Match carries the version you read, " +
				"as a number — " + strconv.Quote(header) + " is not one"})
		return false
	}
	if held, ok := args[field].(float64); ok && uint64(held) != version {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": fmt.Sprintf("If-Match says %d and the "+
				"body's %s says %v; send one", version, field, held)})
		return false
	}
	args[field] = float64(version)
	return true
}
