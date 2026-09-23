// Package workapi is the HUMAN WRITE SURFACE over the company's own tracker
// and knowledge base: filing, editing, commenting on, arranging and taking
// work and pages out of circulation, from a browser or a script, as the person
// the request resolved to.
//
// # It is the SAME TOOLS a seat and the operator's assistant hold
//
// Every route that has a tool behind it is an ADAPTER over that tool — the one
// internal/agent/builtin registers into a seat's turn and internal/api/opsmcp
// serves to an operator's assistant — built from the same deps and the same
// authority decision. What a route adds is only the HTTP shape: the path names
// the object, the body is the tool's own arguments, and the answer is the
// tool's own receipt under a status code.
//
// The alternative this replaces was the obvious one: a handler per route
// calling the tracker's writer directly. It would have been the THIRD
// implementation of "file an item" — the third place a field is trimmed, a
// default applied, a mention resolved and a refusal worded — and the first
// one a person uses. Two copies had already drifted on exactly those parts
// before this surface existed; the third would have been the one nobody's
// tests reached, because the tools' suites exercise the tools.
//
// A FEW ROUTES HAVE NO TOOL, and are the ones a seat is never given: placing a
// card between two neighbours on a board, rewriting one's own remark on a work
// item, and the knowledge base's four destructive verbs and its rename. Those
// call the domain's writer themselves, and decide first through the SAME table
// — [authz.ActionWorkRank], [authz.ActionWorkCommentEdit],
// [authz.ActionPageRename], [authz.ActionPageTrash], [authz.ActionPageRestore],
// [authz.ActionPagePurge] and [authz.ActionPageCommentRemove]. The four page
// verbs had rules and no caller at all until this surface; the work purge had
// a route of its own under /work/{id}/purge with an operator check written
// beside it, which this replaces.
//
// # Authority is decided ONCE, and read identically everywhere
//
// Every route is mounted through [authz.Router] with a policy. Where the
// object is knowable from the PATH — the project in /work/projects/{key}, the
// person in /work/people/{handle} — the route decides the verb itself. Where
// it needs a STORED ROW — which project a work item is filed under, which
// container a page is in, who wrote a comment — the route admits on the
// weakest honest precondition and the verb is decided once the row is read:
// by the tool's own ask, or by this package for a verb that has no tool.
//
// A refusal is WORDED by [builtin.Refusal] from the error [builtin.DecisionError]
// makes of the decision — the same two functions the tools' own gate uses — so
// "you may not" reads identically on a seat's turn, in the operator's
// assistant and here. A route that composed its own sentence would be where
// the three first disagreed.
//
// # The status codes, and why each is its own
//
//   - 401 — nobody presented a credential.
//   - 403 — a credential this node knows, refused by the authority table.
//   - 503 with Retry-After — this node could not decide (the identity estate
//     or the chart could not be read), or could not establish the outcome.
//   - 404 — the item, page or comment does not exist.
//   - 409 — somebody changed it since it was read: a stale version, a title
//     taken, a race lost.
//   - 422 — the domain refused the write on its own rules; the detail is its
//     own sentence.
//   - 200 / 202 / 503 — a write that was made answers the tool's own outcome:
//     applied, pending with the position to read at, or unknown carrying the
//     operation id a retry must reuse.
//
// # A retry is idempotent under the caller's own key
//
// An `Idempotency-Key` header becomes the operation SEED every write the
// request makes derives its id from — the tracker's through the actor's work
// key, the knowledge base's through the actor's op key — so a request retried
// after `unknown` under the same key lands once. Without one, a fresh key is
// minted per request and handed back as `op_id`.
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

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

var log = logging.Get("api.workapi")

// IdempotencyHeader carries a caller's operation key.
//
// THE SAME SPELLING /chart AND /iam USE, because a script retrying any write
// on this engine is one script: the answer to an `unknown` carries the key
// and the route accepts it back.
const IdempotencyHeader = "Idempotency-Key"

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

// retryAfter is the Retry-After on this surface's 503s, in seconds.
//
// TWO, the identity surface's own value and for its reason: what a caller is
// waiting for is this node's applier committing one more batch, or its chart
// view catching up — both on the scale of one apply, not of an outage.
const retryAfter = 2

// decisionRead is the level a route reads a row at before deciding on it.
//
// THE LEVEL THE TOOLS THIS SURFACE SERVES READ AT, so a decision taken here
// and the tool's own read behind it see the same rows: a route that decided
// on a staler copy than the write then acts on would be deciding about a
// different object.
var decisionRead = statelog.Freshness{
	Level: statelog.DefaultReadLevel(statelog.SurfaceSeat),
}

// TrackerWriter is the tracker's write side for the three gestures no tool
// makes, bound to one actor.
type TrackerWriter interface {
	MoveTask(ctx context.Context, opID, project, taskID string,
		after, before tracker.Rank) (tracker.WriteResult, error)
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

// Options configure the surface.
type Options struct {
	// Work and Pages are the deps the tools are built from — the SAME
	// values the operator's assistant is served from. Their Actor and
	// Authorize fields are this surface's to set and are overwritten: the
	// actor is the request's principal, and the decision is [Options.Chart]'s.
	Work  builtin.WorkDeps
	Pages builtin.PageDeps

	// Tracker is the tracker's writer bound to one actor, for the gestures
	// no tool makes. Nil leaves the work half unserved.
	Tracker func(actor builtin.Actor) TrackerWriter

	// PageStore is the knowledge base's writer, for the verbs no tool makes.
	// Nil leaves the pages half unserved.
	PageStore PageStore

	// Chart is what every decision on this surface reads, routes and tools
	// alike.
	//
	// ONE FIELD RATHER THAN A CHART AND AN AUTHORIZER, because the tools'
	// decision is [builtin.Decide] over this same chart: two fields could
	// be set to two charts, and a route and the tool behind it would then
	// answer one question two ways. REQUIRED — [authz.NoChart] says "decide
	// on grants alone" out loud where a nil would read as an omission.
	Chart authz.Chart
}

// Service is the surface.
type Service struct {
	workOn, pagesOn bool

	workDeps  builtin.WorkDeps
	pageDeps  builtin.PageDeps
	tracker   func(actor builtin.Actor) TrackerWriter
	store     PageStore
	chart     authz.Chart
	authorize builtin.Authorizer
}

// New builds the surface, or nil when there is nothing to serve.
//
// NIL RATHER THAN AN EMPTY SURFACE, on opsmcp's rule: a company on Jira and
// Confluence has no native tracker or knowledge base, and routes that exist
// to answer 503 read as an outage where absent ones match the configuration.
func New(opts Options) (*Service, error) {
	if opts.Chart == nil {
		return nil, errors.New("workapi: no chart: every decision here reads " +
			"one — pass authz.NoChart to decide on grants alone")
	}
	s := &Service{
		workDeps: opts.Work, pageDeps: opts.Pages,
		tracker: opts.Tracker, store: opts.PageStore, chart: opts.Chart,
		authorize: builtin.Decide(opts.Chart),
	}
	s.workOn = opts.Work.Reader != nil && opts.Work.Writer != nil &&
		opts.Tracker != nil
	s.pagesOn = opts.Pages.Reader != nil && opts.Pages.Writer != nil &&
		opts.PageStore != nil
	if !s.workOn && !s.pagesOn {
		return nil, nil
	}
	return s, nil
}

// Routes registers the surface on a mux.
//
// EVERY ROUTE CARRIES ITS OWN POLICY through [authz.Router], which is the only
// reader of the matched pattern — see the package doc for which routes decide
// the verb there and which admit on a precondition and decide once they have
// read the row.
func (s *Service) Routes(mux authz.Mux) error {
	router := authz.NewRouter(mux, s.guard).Refusing(s.refuse)
	var failures []error
	mount := func(pattern string, p authz.Policy, h http.HandlerFunc) {
		if err := router.Handle(pattern, p, h); err != nil {
			failures = append(failures, err)
		}
	}
	if s.workOn {
		s.workRoutes(mount)
	}
	if s.pagesOn {
		s.pageRoutes(mount)
	}
	return errors.Join(failures...)
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

// guard is what [authz.Router] asks before a handler runs.
func (s *Service) guard(r *http.Request, p authz.Policy) authz.Decision {
	principal, how := iam.From(r.Context())
	if how == iam.Unknown {
		// THIS NODE'S FAULT, answered as such: deciding it as the zero
		// principal would be a 403 naming a capability the caller may
		// well hold, for as long as the estate was unreadable.
		return authz.Decision{Err: iam.Reason(r.Context())}
	}
	var object authz.Object
	if p.Object != nil {
		object = p.Object(r)
	}
	return authz.Decide(r.Context(), principal, p.Action, object, s.chart)
}

// refuse renders what the router did not admit.
func (s *Service) refuse(w http.ResponseWriter, r *http.Request, p authz.Policy,
	d authz.Decision) {

	s.refuseDecision(w, r, p.Action, d)
}

// decide takes one decision this surface makes after reading a row, and
// renders the refusal when there is one. It reports whether to go on.
func (s *Service) decide(w http.ResponseWriter, r *http.Request,
	action authz.Action, object authz.Object) (authz.Decision, bool) {

	principal, how := iam.From(r.Context())
	var d authz.Decision
	if how == iam.Unknown {
		d = authz.Decision{Err: iam.Reason(r.Context())}
	} else {
		d = authz.Decide(r.Context(), principal, action, object, s.chart)
	}
	if d.Unknown() || !d.Allowed {
		s.refuseDecision(w, r, action, d)
		return d, false
	}
	return d, true
}

// refuseDecision is THE refusal this surface writes for an authority answer.
//
// WORDED BY THE TOOLS' OWN FUNCTIONS — [builtin.DecisionError] makes the error
// the tool's gate would have returned and [builtin.Refusal] the sentence the
// tool would have answered with — so a refusal here and a refusal in a turn
// or the operator's assistant are the same bytes.
func (s *Service) refuseDecision(w http.ResponseWriter, r *http.Request,
	action authz.Action, d authz.Decision) {

	switch _, how := iam.From(r.Context()); {
	case how == iam.Anonymous:
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
	case how == iam.Unknown:
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		httpjson.Fail(w, http.StatusServiceUnavailable,
			httpjson.CodeIdentityUnavailable)
	case d.Unknown():
		unavailable(w, builtin.Refusal(string(action),
			builtin.DecisionError(action, d)), nil)
	default:
		httpjson.FailWith(w, http.StatusForbidden, httpjson.CodeForbidden,
			map[string]string{"detail": builtin.Refusal(string(action),
				builtin.DecisionError(action, d))})
	}
}

// unavailable writes a 503 this caller should retry, with what to retry with.
func unavailable(w http.ResponseWriter, detail string, extra map[string]any) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	body := httpjson.Detail{"detail": detail}
	for k, v := range extra {
		body[k] = v
	}
	httpjson.FailWithFields(w, http.StatusServiceUnavailable,
		httpjson.CodeUnavailable, body)
}

// ---- calling a tool ---------------------------------------------------- //

// operationKey is the seed every write this request makes derives its
// operation id from: the caller's own where they sent one, and fresh where
// they did not — which is then handed back, so a retry can send it.
func operationKey(r *http.Request) string {
	if given := strings.TrimSpace(r.Header.Get(IdempotencyHeader)); given != "" {
		return given
	}
	return uuid.NewString()
}

// deps are this surface's deps for ONE request: the actor is the request's
// principal carrying the request's operation key, and the decision is the
// chart's.
//
// PER REQUEST because the key is: an actor built once would stamp every
// request with the first one's seed, and the ledger would collapse every
// write after it as a redelivery — the defect callKey's own doc records the
// operator surface having for a deployment's whole life.
func (s *Service) deps(key string) (builtin.WorkDeps, builtin.PageDeps) {
	work, kb := s.workDeps, s.pageDeps
	work.Actor = func(ctx context.Context, turn *turnctx.Turn) (builtin.Actor, error) {
		actor, err := builtin.PrincipalActor(ctx, turn)
		actor.WorkKey = key
		return actor, err
	}
	kb.Actor = func(ctx context.Context, turn *turnctx.Turn) (pages.Actor, error) {
		actor, err := builtin.PrincipalPageActor(ctx, turn)
		actor.OpKey = key
		return actor, err
	}
	work.Authorize, kb.Authorize = s.authorize, s.authorize
	return work, kb
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
		s.refuseDecision(w, r, "", authz.Decision{Err: err})
		return builtin.Actor{}, false
	}
	actor.WorkKey = key
	return actor, true
}

// pageActor is [Service.actor] for the knowledge base.
func (s *Service) pageActor(w http.ResponseWriter, r *http.Request, key string) (
	pages.Actor, bool) {

	actor, err := builtin.PrincipalPageActor(r.Context(), nil)
	if err != nil {
		s.refuseDecision(w, r, "", authz.Decision{Err: err})
		return pages.Actor{}, false
	}
	actor.OpKey = key
	return actor, true
}

// call runs one tool as the request's principal and answers its receipt.
func (s *Service) call(w http.ResponseWriter, r *http.Request, verb string,
	args map[string]any) {

	key := operationKey(r)
	work, kb := s.deps(key)
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: work, Pages: kb, Authorize: s.authorize,
	}) {
		if tool.Name() != verb {
			continue
		}
		result, err := tool.Call(r.Context(), args)
		if err != nil {
			// THE CALLER'S CONTEXT ENDED — internal/mcp's own meaning
			// of a tool error. Nobody is waiting for an answer, and
			// whether the write landed is exactly what a retry under
			// the same key resolves.
			unavailable(w, "the request ended before "+verb+" answered: "+
				err.Error(), map[string]any{"op_id": key})
			return
		}
		answerTool(w, key, result)
		return
	}
	// A VERB THIS NODE DOES NOT SERVE — its backend is absent — is a 404
	// rather than a refusal: nothing here could ever make it succeed.
	httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound,
		map[string]string{"detail": verb + " is not served by this node"})
}

// answerTool renders one tool's receipt.
func answerTool(w http.ResponseWriter, key string, result tools.Result) {
	if result.Failed {
		fail(w, result.Cause, result.Output, key)
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
	answer(w, key, outcomeOf(receipt), receipt)
}

// outcomeOf is a receipt's outcome: its own, or — for a tool that made two
// records, as write_project's tag and policy halves are — the WEAKER of the
// ones it nests, because the answer is about the whole request.
func outcomeOf(receipt map[string]any) statelog.Outcome {
	if held, ok := receipt["outcome"].(string); ok && held != "" {
		return statelog.Outcome(held)
	}
	weakest := statelog.OutcomeApplied
	for _, v := range receipt {
		nested, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if held, ok := nested["outcome"].(string); ok {
			weakest = weaker(weakest, statelog.Outcome(held))
		}
	}
	return weakest
}

// weaker is the lesser of two outcomes: unknown below pending below applied.
func weaker(a, b statelog.Outcome) statelog.Outcome {
	rank := map[statelog.Outcome]int{
		statelog.OutcomeUnknown: 0, statelog.OutcomePending: 1,
	}
	ra, oka := rank[a]
	rb, okb := rank[b]
	switch {
	case !okb:
		return a
	case !oka || rb < ra:
		return b
	}
	return a
}

// answer writes a write that was MADE, under its outcome.
//
// THE THREE SUCCESSES ARE THREE ANSWERS, for chartapi's reason: only
// `applied` means the next read on this node sees the write, so only it is a
// 200. `pending` is durable and not yet here — 202 with the position to read
// at. `unknown` is a write this node cannot account for — 503, carrying the
// operation key, because the only safe retry is the same one.
func answer(w http.ResponseWriter, key string, outcome statelog.Outcome,
	body map[string]any) {

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
		unavailable(w, "this node cannot establish what happened to this "+
			"change. Retry it with the SAME key — send op_id back as the "+
			IdempotencyHeader+" header — because a fresh one would defeat "+
			"the ledger that makes the retry safe.", map[string]any{"op_id": key})
	default:
		httpjson.Write(w, http.StatusOK, body)
	}
}

// fail renders a write that was NOT made.
//
// FROM THE CAUSE, never from the sentence: a tool's refusal carries the error
// underneath it as [tools.Result.Cause], and reading a status back out of a
// sentence written for a model would make the wording of every refusal an API.
// The sentence is still the detail, because it is the only thing that says
// what to do.
func fail(w http.ResponseWriter, cause error, text, key string) {
	detail := map[string]string{"detail": text}
	switch {
	case errors.Is(cause, builtin.ErrUnauthenticated):
		httpjson.FailWith(w, http.StatusUnauthorized, httpjson.CodeInvalidToken, detail)
	case errors.Is(cause, builtin.ErrRefused), errors.Is(cause, tracker.ErrNotAuthor):
		httpjson.FailWith(w, http.StatusForbidden, httpjson.CodeForbidden, detail)
	case errors.Is(cause, builtin.ErrUndecidable):
		unavailable(w, text, nil)
	case errors.Is(cause, builtin.ErrNoAuthorizer):
		// A WIRING THAT DECIDED NOTHING, which no retry clears.
		httpjson.FailWith(w, http.StatusInternalServerError,
			httpjson.CodeInternalError, detail)
	case errors.Is(cause, tracker.ErrNoTask), errors.Is(cause, tracker.ErrNoComment),
		errors.Is(cause, tracker.ErrNoProject), errors.Is(cause, pages.ErrNotFound):
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound, detail)
	case errors.Is(cause, tracker.ErrStaleVersion), errors.Is(cause, pages.ErrStaleVersion),
		errors.Is(cause, pages.ErrConflict), errors.Is(cause, pages.ErrTitleTaken),
		errors.Is(cause, statelog.ErrConflict), errors.Is(cause, statelog.ErrExists):
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeStale, detail)
	case errors.Is(cause, statelog.ErrUnavailable):
		unavailable(w, text, map[string]any{"op_id": key})
	default:
		httpjson.FailWith(w, http.StatusUnprocessableEntity, httpjson.CodeRefused, detail)
	}
}

// failErr is [fail] for a writer this surface called itself, whose error is
// both the cause and the sentence.
func failErr(w http.ResponseWriter, err error, key string) {
	fail(w, err, err.Error(), key)
}

// readFailed answers a read taken before a decision that could not be served.
//
// NOT FOUND IS A 404 AND EVERYTHING ELSE IS THIS NODE: a row that is absent
// and a row this node could not read are opposite answers, and a read failure
// rendered as "no such item" is how a caller comes to file a duplicate.
func readFailed(w http.ResponseWriter, err error) {
	if errors.Is(err, tracker.ErrNoTask) || errors.Is(err, pages.ErrNotFound) {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound,
			map[string]string{"detail": err.Error()})
		return
	}
	log.Warn("api_work_read_failed", "error", err)
	unavailable(w, "this node could not read what the decision is about: "+
		err.Error(), nil)
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
