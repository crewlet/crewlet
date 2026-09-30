package api

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/opsmcp"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE NATIVE HALVES, AS EACH REQUEST FINDS THEM.
//
// # Why nothing here is captured when the API is built
//
// A node's own tracker and knowledge base come up with its FIRST COMPANY, and
// a node that booted with none meets that at an apply — after its API has been
// serving for as long as it took somebody to import one. The API used to take
// every native source from the engine ONCE, while it was being wired: the read
// surface's work and page questions, the human write surface, the operator's
// assistant. On such a node every one of them was absent, and stayed absent
// until a restart — so a company bootstrapped live, the quickstart's own
// route, had no board, no pages and no assistant until somebody restarted the
// node it was bootstrapped on.
//
// So each source here asks the engine per request. The identity estate and
// the org chart need none of this: they are the engine's CORE, running from
// boot on every node, and are wired once like everything else.
//
// # Three answers, and why a half that is not up is not a half that is absent
//
// A native half is UP, NOT UP YET, or NOT THIS COMPANY'S. Not up yet is a node
// that has met no company, and the answer is the engine's 503 — the halves are
// coming with its first revision, and a client that waits is served. Not this
// company's is a tracker or a wiki that is a vendor's, and the answer is the
// one a missing source always had — the question unregistered, the route
// absent — because no wait brings it: switching a backend takes a restart.
// [engine.Engine.NativeStarted] is read FIRST and the half AFTER: the first is
// monotonic, so a half read as absent after it said "started" is absent by
// configuration, never merely not published yet.

// errNoCompanyYet is a native question asked of a node that has not been handed
// a company yet. It is [queries.ErrUnavailable] — a question this node
// understood and cannot answer HERE, yet — and [stream.ErrNoCompany], the one
// reading both transports take of it ([stream.UnavailableOf]): the REST surface
// answers `503 no_active_revision` and the socket an `unavailable` frame whose
// refusal is that code, each with the reconcile poll as its wait and
// [httpjson.NativeHalvesNotUp] as its words.
var errNoCompanyYet = fmt.Errorf("%w: %w", queries.ErrUnavailable, stream.ErrNoCompany)

// NativeSources are the read surface's three native sources — the board, the
// knowledge base and ranked item search — each resolved per question.
//
// ONE CONSTRUCTOR for `crewlet run` and the end-to-end suite, for
// [NewHumanSurfaces]' reason: a copy of the rule in either would be a harness
// agreeing with itself.
func NativeSources(e *engine.Engine) (queries.WorkReader, queries.PageReader,
	queries.WorkSearcher) {

	return liveWork{engine: e}, livePages{engine: e}, liveWorkSearch{engine: e}
}

// nativeAbsent is why a half is not here: the node has not met its company
// yet, or the company does not keep this half in the engine. See the file
// comment for why the two are different answers.
func nativeAbsent(started bool, half string) error {
	if !started {
		return errNoCompanyYet
	}
	return fmt.Errorf("%w: this company's %s is not the engine's own",
		queries.ErrUnknown, half)
}

// liveWork is the tracker's read side, resolved per question.
type liveWork struct{ engine *engine.Engine }

func (l liveWork) reader() (queries.WorkReader, error) {
	started := l.engine.NativeStarted()
	if r := l.engine.Tracker(); r != nil {
		return r, nil
	}
	return nil, nativeAbsent(started, "tracker")
}

func (l liveWork) Tasks(ctx context.Context, q tracker.Query, now time.Time) (tracker.Answer, error) {
	r, err := l.reader()
	if err != nil {
		return tracker.Answer{}, err
	}
	return r.Tasks(ctx, q, now)
}

func (l liveWork) Task(ctx context.Context, idOrKey string, want tracker.DetailWants,
	fresh statelog.Freshness) (tracker.TaskDetail, error) {
	r, err := l.reader()
	if err != nil {
		return tracker.TaskDetail{}, err
	}
	return r.Task(ctx, idOrKey, want, fresh)
}

func (l liveWork) Views(ctx context.Context, q tracker.ViewQuery) (tracker.ViewListing, error) {
	r, err := l.reader()
	if err != nil {
		return tracker.ViewListing{}, err
	}
	return r.Views(ctx, q)
}

func (l liveWork) ExpandedQuery(ctx context.Context, params map[string]any,
	viewer tracker.Viewer, now time.Time, loc *time.Location) (tracker.Query, error) {
	r, err := l.reader()
	if err != nil {
		return tracker.Query{}, err
	}
	return r.ExpandedQuery(ctx, params, viewer, now, loc)
}

func (l liveWork) Catalogue(ctx context.Context, q tracker.CatalogueQuery) (
	tracker.CatalogueAnswer, error) {
	r, err := l.reader()
	if err != nil {
		return tracker.CatalogueAnswer{}, err
	}
	return r.Catalogue(ctx, q)
}

func (l liveWork) Projects(ctx context.Context, q tracker.ProjectQuery) (
	tracker.ProjectListing, error) {
	r, err := l.reader()
	if err != nil {
		return tracker.ProjectListing{}, err
	}
	return r.Projects(ctx, q)
}

func (l liveWork) Project(ctx context.Context, q tracker.ProjectDetailQuery) (
	tracker.ProjectDetail, error) {
	r, err := l.reader()
	if err != nil {
		return tracker.ProjectDetail{}, err
	}
	return r.Project(ctx, q)
}

func (l liveWork) Workload(ctx context.Context, q tracker.WorkloadQuery, now time.Time) (
	tracker.WorkloadAnswer, error) {
	r, err := l.reader()
	if err != nil {
		return tracker.WorkloadAnswer{}, err
	}
	return r.Workload(ctx, q, now)
}

func (l liveWork) Activity(ctx context.Context, q tracker.ActivityQuery, now time.Time) (
	tracker.ActivityAnswer, error) {
	r, err := l.reader()
	if err != nil {
		return tracker.ActivityAnswer{}, err
	}
	return r.Activity(ctx, q, now)
}

func (l liveWork) MyWork(ctx context.Context, q tracker.MyWorkQuery, now time.Time) (
	tracker.MyWork, error) {
	r, err := l.reader()
	if err != nil {
		return tracker.MyWork{}, err
	}
	return r.MyWork(ctx, q, now)
}

func (l liveWork) Person(ctx context.Context, q tracker.PersonQuery, now time.Time) (
	tracker.PersonState, error) {
	r, err := l.reader()
	if err != nil {
		return tracker.PersonState{}, err
	}
	return r.Person(ctx, q, now)
}

func (l liveWork) Inbox(ctx context.Context, q tracker.InboxQuery, now time.Time) (
	tracker.InboxAnswer, error) {
	r, err := l.reader()
	if err != nil {
		return tracker.InboxAnswer{}, err
	}
	return r.Inbox(ctx, q, now)
}

func (l liveWork) Routing(ctx context.Context, q tracker.RoutingQuery, now time.Time) (
	tracker.RoutingAnswer, error) {
	r, err := l.reader()
	if err != nil {
		return tracker.RoutingAnswer{}, err
	}
	return r.Routing(ctx, q, now)
}

// livePages is the knowledge base's read side, resolved per question.
type livePages struct{ engine *engine.Engine }

func (l livePages) reader() (queries.PageReader, error) {
	started := l.engine.NativeStarted()
	if r := l.engine.Pages(); r != nil {
		return r, nil
	}
	return nil, nativeAbsent(started, "knowledge base")
}

func (l livePages) List(ctx context.Context, f pages.Filter, fresh statelog.Freshness) (
	pages.Listing, error) {
	r, err := l.reader()
	if err != nil {
		return pages.Listing{}, err
	}
	return r.List(ctx, f, fresh)
}

func (l livePages) Get(ctx context.Context, ref string, fresh statelog.Freshness) (
	pages.Detail, error) {
	r, err := l.reader()
	if err != nil {
		return pages.Detail{}, err
	}
	return r.Get(ctx, ref, fresh)
}

func (l livePages) Containers(ctx context.Context, fresh statelog.Freshness) (
	[]pages.ContainerListing, error) {
	r, err := l.reader()
	if err != nil {
		return nil, err
	}
	return r.Containers(ctx, fresh)
}

func (l livePages) Activity(ctx context.Context, q pages.PageActivityQuery) (
	pages.PageActivity, error) {
	r, err := l.reader()
	if err != nil {
		return pages.PageActivity{}, err
	}
	return r.Activity(ctx, q)
}

func (l livePages) Revision(ctx context.Context, pageID string, version int,
	fresh statelog.Freshness) (pages.Revision, bool, error) {
	r, err := l.reader()
	if err != nil {
		return pages.Revision{}, false, err
	}
	return r.Revision(ctx, pageID, version, fresh)
}

// liveWorkSearch is ranked item search, resolved per question.
//
// ITS OWN SOURCE, as [queries.Sources.WorkSearch] is: the index is this node's
// and the rows are the fleet's, so the two are absent independently.
type liveWorkSearch struct{ engine *engine.Engine }

func (l liveWorkSearch) Search(ctx context.Context, text string, limit int) (
	[]tracker.Ranked, error) {
	started := l.engine.NativeStarted()
	s := l.engine.WorkSearch()
	if s == nil {
		return nil, nativeAbsent(started, "search index")
	}
	return s.Search(ctx, text, limit)
}

// OperatorSource is the operator's MCP surface as a request finds it: the
// server over the catalogue this node serves NOW, nil where it serves none,
// and false where it cannot say yet because it has not been handed a company.
// See [Options.Operator].
type OperatorSource func() (*opsmcp.Server, bool)

// NativeOperator is the operator's MCP surface over this engine, built per
// request.
//
// PER REQUEST, and for more than the halves. The catalogue is the native
// halves' — which a node meets at its first company — and the knowledge
// search beside them is whatever backend the company runs NOW, which an apply
// can add: a server built once served the knowledge search the company had
// when the API was wired, for the life of the process. Building one is the
// same catalogue [workapi] builds for every request it serves, and the
// transport is stateless, so there is no session a rebuilt server strands.
func NativeOperator(e *engine.Engine) OperatorSource {
	return func() (*opsmcp.Server, bool) {
		if !e.NativeStarted() {
			return nil, false
		}
		return operatorMCP(e), true
	}
}

// operatorMCP builds the operator's own MCP surface over what this node serves
// now, or nil where that is nothing.
//
// THE SAME DEPS A SEAT'S TOOLS GET, with one field different: the actor. That
// is what makes this one implementation of ten tools rather than two — see
// [builtin.WorkDeps.Actor].
//
// The DEFAULTS are deliberately absent. A seat files into its unit's project
// when it names none, because a seat HAS a unit; an operator does not, so the
// argument is required and the tool refuses naming it rather than guessing a
// project on a person's behalf.
//
// Which UNIT the work is filed into is not a default of this surface and no
// longer needs one: the tracker reads it off the project's own row at the
// write, so an operator's item belongs to the team that owns the project it
// named. It used to be stamped from the caller's own team, which an operator
// has not got — so every item filed here read "Filed into: no unit" beside a
// project page naming its unit.
func operatorMCP(e *engine.Engine) *opsmcp.Server {
	var opts opsmcp.Options
	if c := e.Company(); c != nil && c.Config != nil {
		opts.Company = c.Config.Name
	}
	opts.Work, opts.Pages = NativeToolDeps(e)
	// THE PRINCIPAL IS THE PARTY, and it comes from the request's context
	// rather than from the call: a tracker whose author field is chosen by
	// the writer is not an audit trail, and there is deliberately no way to
	// name a seat to act as. [builtin.PrincipalActor] is the one conversion
	// the HTTP write surface makes too.
	opts.Work.Actor = builtin.PrincipalActor
	opts.Pages.Actor = builtin.PrincipalPageActor
	// SEARCH IS OFFERED WHENEVER THE COMPANY HAS A BACKEND, native or not:
	// unlike the ten write tools, ranked search over the company's own
	// wiki is exactly as useful to an operator's assistant on Confluence.
	if e.Knowledge() != nil {
		opts.Knowledge = operatorKnowledge{engine: e}
		// AND THE CHART BESIDE IT. An operator has no turn, so the org
		// the search is scoped against comes from here; resolved per
		// call, because a config apply replaces it. Search is the only
		// tool that reads it: WHO the caller is comes from the identity
		// directory through the request's principal, never the chart.
		opts.Org = func() *org.Organization {
			c := e.Company()
			if c == nil {
				return nil
			}
			return c.Org
		}
	}
	// THE AUTHORITY DECISION, which is the SAME one every seat's registry
	// is built with — one table, one function, three surfaces. It reads
	// the chart per call, because an apply replaces the epoch under a
	// long-lived MCP session.
	opts.Authorize = builtin.Decide(engine.ChartAuthorityOf(e))
	return opsmcp.New(opts)
}

// operatorKnowledge resolves the node's searcher per call, for the reason the
// engine's own liveKnowledge does: an apply REPLACES it, and a value captured
// when the surface was built searches with a rotated credential's predecessor.
type operatorKnowledge struct{ engine *engine.Engine }

func (k operatorKnowledge) CanSearch(seat *org.Role, o *org.Organization) bool {
	s := k.engine.Knowledge()
	return s != nil && s.CanSearch(seat, o)
}

func (k operatorKnowledge) Search(ctx context.Context, q knowledge.Query) []knowledge.Hit {
	s := k.engine.Knowledge()
	if s == nil {
		return nil
	}
	return s.Search(ctx, q)
}
