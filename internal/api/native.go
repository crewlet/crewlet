package api

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/search"
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
// So each source here asks the engine per request — the read surface's
// questions, and the halves the operator surface builds its catalogue from for
// every call ([NativeOperatorHalves]). The identity estate and the org chart
// need none of this: they are the engine's CORE, running from boot on every
// node, and are wired once like everything else.
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

// NativeSources are the read surface's four native sources — the board, the
// knowledge base, the links between its pages and ranked item search — each
// resolved per question.
//
// ONE CONSTRUCTOR for `crewlet run` and the end-to-end suite, for
// [NewHumanSurfaces]' reason: a copy of the rule in either would be a harness
// agreeing with itself.
func NativeSources(e *engine.Engine) (queries.WorkReader, queries.PageReader,
	queries.PageBacklinks, queries.WorkSearcher) {

	return liveWork{engine: e}, livePages{engine: e}, liveBacklinks{engine: e},
		liveWorkSearch{engine: e}
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

func (l liveWork) TurnsOf(ctx context.Context, idOrKey, cursor string, limit int,
	fresh statelog.Freshness) (tracker.TaskTurns, error) {
	r, err := l.reader()
	if err != nil {
		return tracker.TaskTurns{}, err
	}
	return r.TurnsOf(ctx, idOrKey, cursor, limit, fresh)
}

func (l liveWork) Views(ctx context.Context, q tracker.ViewQuery) (tracker.ViewListing, error) {
	r, err := l.reader()
	if err != nil {
		return tracker.ViewListing{}, err
	}
	return r.Views(ctx, q)
}

func (l liveWork) EveryView(ctx context.Context, q tracker.EveryViewQuery) (
	tracker.ViewListing, error) {
	r, err := l.reader()
	if err != nil {
		return tracker.ViewListing{}, err
	}
	return r.EveryView(ctx, q)
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

func (l liveWork) Workload(ctx context.Context, q tracker.WorkloadQuery, now time.Time,
	loc *time.Location) (tracker.WorkloadAnswer, error) {
	r, err := l.reader()
	if err != nil {
		return tracker.WorkloadAnswer{}, err
	}
	return r.Workload(ctx, q, now, loc)
}

func (l liveWork) Activity(ctx context.Context, q tracker.ActivityQuery, now time.Time) (
	tracker.ActivityAnswer, error) {
	r, err := l.reader()
	if err != nil {
		return tracker.ActivityAnswer{}, err
	}
	return r.Activity(ctx, q, now)
}

func (l liveWork) MyWork(ctx context.Context, q tracker.MyWorkQuery, now time.Time,
	loc *time.Location) (tracker.MyWork, error) {
	r, err := l.reader()
	if err != nil {
		return tracker.MyWork{}, err
	}
	return r.MyWork(ctx, q, now, loc)
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

func (l liveWork) Flow(ctx context.Context, q tracker.FlowQuery, now time.Time,
	loc *time.Location) (tracker.FlowAnswer, error) {
	r, err := l.reader()
	if err != nil {
		return tracker.FlowAnswer{}, err
	}
	return r.Flow(ctx, q, now, loc)
}

func (l liveWork) CompanyFeed(ctx context.Context, q tracker.FeedQuery) (
	tracker.FeedPage, error) {
	r, err := l.reader()
	if err != nil {
		return tracker.FeedPage{}, err
	}
	return r.CompanyFeed(ctx, q)
}

func (l liveWork) Decisions(ctx context.Context, q tracker.DecisionsQuery, now time.Time,
	loc *time.Location) (tracker.DecisionsAnswer, error) {
	r, err := l.reader()
	if err != nil {
		return tracker.DecisionsAnswer{}, err
	}
	return r.Decisions(ctx, q, now, loc)
}

func (l liveWork) TurnPlaces(ctx context.Context, runs []string,
	fresh statelog.Freshness) (map[string]tracker.TurnPlace, error) {
	r, err := l.reader()
	if err != nil {
		return nil, err
	}
	return r.TurnPlaces(ctx, runs, fresh)
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

// liveBacklinks is which pages link to a page, resolved per question.
//
// ITS OWN SOURCE, and a native-half one: the links are an index the engine
// keeps over its own knowledge base's pages, which comes up with the node's
// first company and is absent for a company whose wiki is a vendor's.
type liveBacklinks struct{ engine *engine.Engine }

func (l liveBacklinks) LinkedFrom(ctx context.Context, pageID string) (
	search.Backlinks, error) {
	started := l.engine.NativeStarted()
	x := l.engine.Backlinks()
	if x == nil {
		return search.Backlinks{}, nativeAbsent(started, "knowledge base")
	}
	return x.LinkedFrom(ctx, pageID)
}

// liveWorkSearch is ranked item search, resolved per question.
//
// ITS OWN SOURCE, as [queries.Sources.WorkSearch] is, and present exactly
// where the tracker is: the index is built for either native backend, but the
// search reads the tracker's corpus alone, so the engine builds it only for a
// company that keeps its tracker here ([engine.Engine.WorkSearch]).
type liveWorkSearch struct{ engine *engine.Engine }

func (l liveWorkSearch) Search(ctx context.Context, q tracker.SearchQuery) (
	tracker.SearchAnswer, error) {
	started := l.engine.NativeStarted()
	s := l.engine.WorkSearch()
	if s == nil {
		return tracker.SearchAnswer{}, nativeAbsent(started, "tracker")
	}
	return s.Search(ctx, q)
}

// NativeOperatorHalves are the halves the operator surface builds its
// catalogue from for every call, as the call finds them, and false where this
// node has not been handed a company yet ([operator.Options.Halves]).
//
// [engine.Engine.NativeStarted] FIRST, and the halves after: it is monotonic,
// so a half read as absent after it said "started" is one this company does
// not run rather than one not published yet — see the file comment.
//
// THE KNOWLEDGE SEARCH IS WHATEVER BACKEND THE COMPANY RUNS NOW, native or not:
// ranked search over the company's own wiki is exactly as useful to an
// operator's assistant on Confluence, and an apply can add one — so it is read
// per call too, and present exactly where the engine has a searcher.
func NativeOperatorHalves(e *engine.Engine) func() (operator.Halves, bool) {
	return func() (operator.Halves, bool) {
		if !e.NativeStarted() {
			return operator.Halves{}, false
		}
		var halves operator.Halves
		halves.Work, halves.Pages = NativeToolDeps(e)
		if e.Knowledge() != nil {
			halves.Knowledge = operatorKnowledge{engine: e}
		}
		if c := e.Company(); c != nil && c.Config != nil {
			halves.Company = c.Config.Name
		}
		return halves, true
	}
}

// operatorKnowledge resolves the node's searcher per call, for the reason the
// engine's own liveKnowledge does: an apply REPLACES it, and a value captured
// when the surface was built searches with a rotated credential's predecessor.
type operatorKnowledge struct{ engine *engine.Engine }

func (k operatorKnowledge) CanSearch(seat *org.Role, o *org.Organization) bool {
	s := k.engine.Knowledge()
	return s != nil && s.CanSearch(seat, o)
}

func (k operatorKnowledge) Search(ctx context.Context, q knowledge.Query) knowledge.Result {
	s := k.engine.Knowledge()
	if s == nil {
		return knowledge.Result{}
	}
	return s.Search(ctx, q)
}
