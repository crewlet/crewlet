package api

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE NATIVE HALVES, AS EACH REQUEST FINDS THEM.
//
// # Why nothing here is captured when the API is built
//
// A node's own Worklet, Doclet and Chatlet come up with its FIRST COMPANY, and
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
// node that holds the estate — which a node serving this API always is — and
// are wired once like everything else.
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

// NativeReads are the read surface's native sources — the board, the knowledge
// base, the links between its pages, ranked item search, a project's file
// listing, the company's chat and its search, and a person's chat read state —
// each resolved per question. Built by [NativeSources] and handed to the
// read surface by [NativeReads.Into].
//
// THROUGH THIS NODE'S ESTATE ROUTER, as a seat's tools reach the same rows
// ([engine.OperatorWork], [engine.OperatorPages], [engine.OperatorChat],
// [engine.WorkSearcher]), so an operator and an agent looking at one item see
// one item: answered from this node's copy while it serves and from another
// data node's while it does not. Read off this node's copy directly, a data
// node whose copy was out of service — wrong rather than behind, or its file
// shut — went on drawing its operator's board from rows it would not vouch for
// to its own seats. The links between pages are the exception, and
// deliberately: they are this node's own lexical index, which no peer answers
// for. And a person's read cursors are the fleet's coordination record, which
// every node reads directly.
type NativeReads struct {
	Work       queries.WorkReader
	Pages      queries.PageReader
	Backlinks  queries.PageBacklinks
	WorkSearch queries.WorkSearcher
	Files      queries.FileReader
	Chat       queries.ChatReader
	ChatSearch queries.ChatSearcher
	ChatReads  queries.ChatReads
}

// NativeSources are this engine's native read sources.
//
// ONE CONSTRUCTOR for `crewlet run` and the end-to-end suite, for
// [NewHumanSurfaces]' reason: a copy of the rule in either would be a harness
// agreeing with itself.
func NativeSources(e *engine.Engine) NativeReads {
	return NativeReads{
		Work: liveWork{engine: e}, Pages: livePages{engine: e},
		Backlinks: liveBacklinks{engine: e}, WorkSearch: liveWorkSearch{engine: e},
		Files: liveFiles{engine: e},
		Chat:  liveChat{engine: e}, ChatSearch: liveChatSearch{engine: e},
		// THE FLEET'S OWN RECORD, which every node reaches whatever its
		// company runs, and which a chat question reads only after it has
		// read rooms — a company with no Chatlet never asks it.
		ChatReads: e.Backends().Fleet,
	}
}

// Into sets every native source on s — ALL OF THEM, so a caller cannot wire
// the board and forget the rooms beside it. A field set before the call is
// overwritten: these are the native sources, and there is one answer to what
// they are.
func (n NativeReads) Into(s *queries.Sources) {
	s.Work, s.Pages, s.Backlinks, s.WorkSearch, s.Files =
		n.Work, n.Pages, n.Backlinks, n.WorkSearch, n.Files
	s.Chat, s.ChatSearch, s.ChatReads = n.Chat, n.ChatSearch, n.ChatReads
}

// nativeAbsent is why a half is not here: the node has not met its company
// yet, or the company does not run this half (Worklet, Doclet or Chatlet,
// named by half). See the file comment for why the two are different
// answers.
func nativeAbsent(started bool, half string) error {
	if !started {
		return errNoCompanyYet
	}
	return fmt.Errorf("%w: this company does not run %s", queries.ErrUnknown, half)
}

// liveWork is the tracker's read side, resolved per question.
type liveWork struct{ engine *engine.Engine }

func (l liveWork) reader() (estate.Work, error) {
	started := l.engine.NativeStarted()
	if r, ok := engine.OperatorWork(l.engine); ok {
		return r, nil
	}
	return estate.Work{}, nativeAbsent(started, "Worklet")
}

// Kept answers whether this node serves the tracker half, by the same
// resolution every read below makes. See [queries.Keeper].
func (l liveWork) Kept() error {
	_, err := l.reader()
	return err
}

func (l liveWork) Tasks(ctx context.Context, q worklet.Query, now time.Time) (worklet.Answer, error) {
	r, err := l.reader()
	if err != nil {
		return worklet.Answer{}, err
	}
	return r.Tasks(ctx, q, now)
}

func (l liveWork) Task(ctx context.Context, idOrKey string, want worklet.DetailWants,
	fresh statelog.Freshness) (worklet.TaskDetail, error) {
	r, err := l.reader()
	if err != nil {
		return worklet.TaskDetail{}, err
	}
	return r.Task(ctx, idOrKey, want, fresh)
}

func (l liveWork) TurnsOf(ctx context.Context, idOrKey, cursor string, limit int,
	fresh statelog.Freshness) (worklet.TaskTurns, error) {
	r, err := l.reader()
	if err != nil {
		return worklet.TaskTurns{}, err
	}
	return r.TurnsOf(ctx, idOrKey, cursor, limit, fresh)
}

func (l liveWork) Views(ctx context.Context, q worklet.ViewQuery) (worklet.ViewListing, error) {
	r, err := l.reader()
	if err != nil {
		return worklet.ViewListing{}, err
	}
	return r.Views(ctx, q)
}

func (l liveWork) EveryView(ctx context.Context, q worklet.EveryViewQuery) (
	worklet.ViewListing, error) {
	r, err := l.reader()
	if err != nil {
		return worklet.ViewListing{}, err
	}
	return r.EveryView(ctx, q)
}

func (l liveWork) ExpandedQuery(ctx context.Context, params map[string]any,
	viewer worklet.Viewer, now time.Time, loc *time.Location) (worklet.Query, error) {
	r, err := l.reader()
	if err != nil {
		return worklet.Query{}, err
	}
	return r.ExpandedQuery(ctx, params, viewer, now, loc)
}

func (l liveWork) Catalogue(ctx context.Context, q worklet.CatalogueQuery) (
	worklet.CatalogueAnswer, error) {
	r, err := l.reader()
	if err != nil {
		return worklet.CatalogueAnswer{}, err
	}
	return r.Catalogue(ctx, q)
}

func (l liveWork) Projects(ctx context.Context, q worklet.ProjectQuery) (
	worklet.ProjectListing, error) {
	r, err := l.reader()
	if err != nil {
		return worklet.ProjectListing{}, err
	}
	return r.Projects(ctx, q)
}

func (l liveWork) Project(ctx context.Context, q worklet.ProjectDetailQuery) (
	worklet.ProjectDetail, error) {
	r, err := l.reader()
	if err != nil {
		return worklet.ProjectDetail{}, err
	}
	return r.Project(ctx, q)
}

func (l liveWork) Workload(ctx context.Context, q worklet.WorkloadQuery, now time.Time,
	loc *time.Location) (worklet.WorkloadAnswer, error) {
	r, err := l.reader()
	if err != nil {
		return worklet.WorkloadAnswer{}, err
	}
	return r.Workload(ctx, q, now, loc)
}

func (l liveWork) Activity(ctx context.Context, q worklet.ActivityQuery, now time.Time) (
	worklet.ActivityAnswer, error) {
	r, err := l.reader()
	if err != nil {
		return worklet.ActivityAnswer{}, err
	}
	return r.Activity(ctx, q, now)
}

func (l liveWork) MyWork(ctx context.Context, q worklet.MyWorkQuery, now time.Time,
	loc *time.Location) (worklet.MyWork, error) {
	r, err := l.reader()
	if err != nil {
		return worklet.MyWork{}, err
	}
	return r.MyWork(ctx, q, now, loc)
}

func (l liveWork) Person(ctx context.Context, q worklet.PersonQuery, now time.Time) (
	worklet.PersonState, error) {
	r, err := l.reader()
	if err != nil {
		return worklet.PersonState{}, err
	}
	return r.Person(ctx, q, now)
}

func (l liveWork) Inbox(ctx context.Context, q worklet.InboxQuery, now time.Time) (
	worklet.InboxAnswer, error) {
	r, err := l.reader()
	if err != nil {
		return worklet.InboxAnswer{}, err
	}
	return r.Inbox(ctx, q, now)
}

func (l liveWork) Routing(ctx context.Context, q worklet.RoutingQuery, now time.Time) (
	worklet.RoutingAnswer, error) {
	r, err := l.reader()
	if err != nil {
		return worklet.RoutingAnswer{}, err
	}
	return r.Routing(ctx, q, now)
}

func (l liveWork) Flow(ctx context.Context, q worklet.FlowQuery, now time.Time,
	loc *time.Location) (worklet.FlowAnswer, error) {
	r, err := l.reader()
	if err != nil {
		return worklet.FlowAnswer{}, err
	}
	return r.Flow(ctx, q, now, loc)
}

func (l liveWork) CompanyFeed(ctx context.Context, q worklet.FeedQuery) (
	worklet.FeedPage, error) {
	r, err := l.reader()
	if err != nil {
		return worklet.FeedPage{}, err
	}
	return r.CompanyFeed(ctx, q)
}

func (l liveWork) Decisions(ctx context.Context, q worklet.DecisionsQuery, now time.Time,
	loc *time.Location) (worklet.DecisionsAnswer, error) {
	r, err := l.reader()
	if err != nil {
		return worklet.DecisionsAnswer{}, err
	}
	return r.Decisions(ctx, q, now, loc)
}

func (l liveWork) AnsweredAsks(ctx context.Context, q worklet.DecisionsQuery, now time.Time,
	loc *time.Location) (worklet.AnsweredAnswer, error) {
	r, err := l.reader()
	if err != nil {
		return worklet.AnsweredAnswer{}, err
	}
	return r.AnsweredAsks(ctx, q, now, loc)
}

func (l liveWork) TurnPlaces(ctx context.Context, runs []string,
	fresh statelog.Freshness) (map[string]worklet.TurnPlace, error) {
	r, err := l.reader()
	if err != nil {
		return nil, err
	}
	return r.TurnPlaces(ctx, runs, fresh)
}

// Thread is the read `work_item` asks [worklet.AskedOn] through (whether an
// open ask on an item names somebody without Worklet), routed like every
// other tracker read, so the probe and the comment that would answer the ask
// are resolved by one data node's rows under the same session floors.
func (l liveWork) Thread(ctx context.Context, q worklet.ThreadQuery,
	fresh statelog.Freshness) (worklet.ResolvedThread, error) {
	r, err := l.reader()
	if err != nil {
		return worklet.ResolvedThread{}, err
	}
	return r.Thread(ctx, q, fresh)
}

// livePages is the knowledge base's read side, resolved per question.
type livePages struct{ engine *engine.Engine }

// Both halves the merged questions read answer whether they are kept here, or
// those questions would read an absent half and refuse whole.
var (
	_ queries.Keeper = liveWork{}
	_ queries.Keeper = livePages{}
)

func (l livePages) reader() (estate.Pages, error) {
	started := l.engine.NativeStarted()
	if r, ok := engine.OperatorPages(l.engine); ok {
		return r, nil
	}
	return estate.Pages{}, nativeAbsent(started, "Doclet")
}

// Kept answers whether this node serves the knowledge half, by the same
// resolution every read below makes. See [queries.Keeper].
func (l livePages) Kept() error {
	_, err := l.reader()
	return err
}

func (l livePages) List(ctx context.Context, f doclet.Filter, fresh statelog.Freshness) (
	doclet.Listing, error) {
	r, err := l.reader()
	if err != nil {
		return doclet.Listing{}, err
	}
	return r.List(ctx, f, fresh)
}

func (l livePages) Get(ctx context.Context, ref string, fresh statelog.Freshness) (
	doclet.Detail, error) {
	r, err := l.reader()
	if err != nil {
		return doclet.Detail{}, err
	}
	return r.Get(ctx, ref, fresh)
}

func (l livePages) Containers(ctx context.Context, fresh statelog.Freshness) (
	[]doclet.ContainerListing, error) {
	r, err := l.reader()
	if err != nil {
		return nil, err
	}
	return r.Containers(ctx, fresh)
}

func (l livePages) Activity(ctx context.Context, q doclet.PageActivityQuery) (
	doclet.PageActivity, error) {
	r, err := l.reader()
	if err != nil {
		return doclet.PageActivity{}, err
	}
	return r.Activity(ctx, q)
}

func (l livePages) Revision(ctx context.Context, pageID string, version int,
	fresh statelog.Freshness) (doclet.Revision, bool, error) {
	r, err := l.reader()
	if err != nil {
		return doclet.Revision{}, false, err
	}
	return r.Revision(ctx, pageID, version, fresh)
}

// liveBacklinks is which pages link to a page, resolved per question.
//
// ITS OWN SOURCE, and a native-half one: the links are an index the engine
// keeps over Doclet's pages, which comes up with the node's first company
// and is absent for a company whose wiki is a vendor's.
type liveBacklinks struct{ engine *engine.Engine }

func (l liveBacklinks) LinkedFrom(ctx context.Context, pageID string) (
	search.Backlinks, error) {
	started := l.engine.NativeStarted()
	x := l.engine.Backlinks()
	if x == nil {
		return search.Backlinks{}, nativeAbsent(started, "Doclet")
	}
	return x.LinkedFrom(ctx, pageID)
}

// liveWorkSearch is ranked item search, resolved per question.
//
// ITS OWN SOURCE, as [queries.Sources.WorkSearch] is, and present exactly
// where the tracker is: the index is built for Worklet or Doclet, but the
// search reads the tracker's corpus alone, so the engine serves it only for a
// company that keeps its tracker here ([engine.WorkSearcher]) — through the
// router, where a node holding no index of its own is answered by one that
// does.
type liveWorkSearch struct{ engine *engine.Engine }

func (l liveWorkSearch) Search(ctx context.Context, q worklet.SearchQuery) (
	worklet.SearchAnswer, error) {
	started := l.engine.NativeStarted()
	s := engine.WorkSearcher(l.engine)
	if s == nil {
		return worklet.SearchAnswer{}, nativeAbsent(started, "Worklet")
	}
	return s.Search(ctx, q)
}

// liveChat is chat's read side, resolved per question, from the router's rooms
// for [liveWork]'s reason.
type liveChat struct{ engine *engine.Engine }

func (l liveChat) reader() (estate.Chat, error) {
	started := l.engine.NativeStarted()
	if r, ok := engine.OperatorChat(l.engine); ok {
		return r, nil
	}
	return estate.Chat{}, nativeAbsent(started, "Chatlet")
}

func (l liveChat) Channels(ctx context.Context, viewer string, q chatlet.ChannelsQuery,
	fresh statelog.Freshness) (chatlet.ChannelListing, error) {
	r, err := l.reader()
	if err != nil {
		return chatlet.ChannelListing{}, err
	}
	return r.Channels(ctx, viewer, q, fresh)
}

func (l liveChat) Channel(ctx context.Context, viewer, channelID string,
	fresh statelog.Freshness) (chatlet.ChannelDetail, error) {
	r, err := l.reader()
	if err != nil {
		return chatlet.ChannelDetail{}, err
	}
	return r.Channel(ctx, viewer, channelID, fresh)
}

func (l liveChat) ChannelNamed(ctx context.Context, viewer, name string,
	fresh statelog.Freshness) (chatlet.ChannelDetail, error) {
	r, err := l.reader()
	if err != nil {
		return chatlet.ChannelDetail{}, err
	}
	return r.ChannelNamed(ctx, viewer, name, fresh)
}

func (l liveChat) Messages(ctx context.Context, viewer string, q chatlet.TranscriptQuery,
	fresh statelog.Freshness) (chatlet.Transcript, error) {
	r, err := l.reader()
	if err != nil {
		return chatlet.Transcript{}, err
	}
	return r.Messages(ctx, viewer, q, fresh)
}

func (l liveChat) Thread(ctx context.Context, viewer string, q chatlet.ThreadQuery,
	fresh statelog.Freshness) (chatlet.Thread, error) {
	r, err := l.reader()
	if err != nil {
		return chatlet.Thread{}, err
	}
	return r.Thread(ctx, viewer, q, fresh)
}

func (l liveChat) Mentions(ctx context.Context, viewer string, q chatlet.MentionQuery,
	fresh statelog.Freshness) (chatlet.MentionFeed, error) {
	r, err := l.reader()
	if err != nil {
		return chatlet.MentionFeed{}, err
	}
	return r.Mentions(ctx, viewer, q, fresh)
}

// liveChatSearch is chat's ranked search, resolved per question: the router's,
// whose serving data node resolves the viewer's rooms beside the index it
// reads.
type liveChatSearch struct{ engine *engine.Engine }

func (l liveChatSearch) Search(ctx context.Context, q estate.ChatQuery) (
	estate.ChatSearchAnswer, error) {
	started := l.engine.NativeStarted()
	s, ok := engine.OperatorChatSearch(l.engine)
	if !ok {
		return estate.ChatSearchAnswer{}, nativeAbsent(started, "Chatlet")
	}
	return s.Search(ctx, q)
}

// liveFiles is a project's file listing — the rows only, never the bytes,
// which the human write surface streams — resolved per question, from the
// tracker's own half for [liveWork]'s reason.
type liveFiles struct{ engine *engine.Engine }

func (l liveFiles) Files(ctx context.Context, q worklet.FileQuery) (worklet.FileListing, error) {
	r, err := liveWork(l).reader()
	if err != nil {
		return worklet.FileListing{}, err
	}
	return r.Files(ctx, q)
}

// NativeOperatorHalves are the halves the operator surface builds its
// catalogue from for every call, as the call finds them, and false where this
// node has not been handed a company yet ([operator.Options.Halves]).
//
// [engine.Engine.NativeStarted] FIRST, and the halves after: it is monotonic,
// so a half read as absent after it said "started" is one this company does
// not run rather than one not published yet — see the file comment.
//
// THE KNOWLEDGE SEARCH IS WHATEVER BACKEND THE COMPANY RUNS NOW, Doclet or not:
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
		halves.Chat = ChatletDeps(e)
		if e.Knowledge() != nil {
			halves.Knowledge = operatorKnowledge{engine: e}
		}
		halves.Artifacts = engine.ArtifactToolDeps(e)
		halves.ArtifactData = engine.ArtifactDataToolDeps(e)
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
