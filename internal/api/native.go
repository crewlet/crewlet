package api

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/httpjson"
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
// Nothing here is captured when the API is built: a node's own Worklet, Doclet
// and Chatlet come up with its FIRST COMPANY, at an apply after the API serves,
// so a source taken once would stay absent until a restart. Each asks the
// engine per request, as does [NativeOperatorHalves]; the identity estate and
// org chart are CORE, up from boot, wired once. A half is UP, NOT UP YET (no
// company met: the engine's 503, a waiting client is served) or NOT THIS
// COMPANY'S (a vendor's tracker or wiki: question unregistered, route absent,
// since no wait brings it). [engine.Engine.NativeStarted] is read FIRST and the
// half AFTER: the first is monotonic, so a half absent after "started" is
// absent by configuration, never merely unpublished.

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
// [engine.WorkSearcher]): this node's copy while it serves, else another data
// node's. Page links ([estate.Pages.LinkedFrom]) derive from both Doclet and
// Worklet copies, so a node answers only while both serve (ADR-0079). Read
// cursors are the fleet's coordination record, read directly.
type NativeReads struct {
	Work       queries.WorkReader
	Pages      queries.PageReader
	Backlinks  queries.PageBacklinks
	WorkSearch queries.WorkSearcher
	Files      queries.FileReader
	Chat       queries.ChatReader
	ChatSearch queries.ChatSearcher
	ChatReads  queries.ChatReads

	// ChatDraft and ChatMentions answer `chat_reach`: a post's dry run, and
	// the post tool's reading of its words.
	ChatDraft    queries.ChatDrafter
	ChatMentions builtin.MentionResolver

	// PullRequests reads a pull request for a chat link's preview through
	// the company's GitHub wiring.
	PullRequests queries.PullRequestReader
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
		// THE POST'S DRY RUN AND ITS READING OF THE WORDS, the same two
		// halves the post tool is handed, so a composer is told what the
		// send will do.
		ChatDraft: liveChat{engine: e}, ChatMentions: engine.LiveMentions(e),
		// THE FLEET'S OWN RECORD, which every node reaches whatever its
		// company runs, and which a chat question reads only after it has
		// read rooms — a company with no Chatlet never asks it.
		ChatReads: e.Backends().Fleet,
		// THE ENGINE'S OWN GITHUB WIRING, the one a thread's participants
		// are read through, rebuilt on every apply.
		PullRequests: e,
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
	s.ChatDraft, s.ChatMentions = n.ChatDraft, n.ChatMentions
	s.PullRequests = n.PullRequests
}

// nativeAbsent is why a half is not here: no company met yet, the half turned
// on while this runtime ran without it, or the company does not run it
// (Worklet, Doclet or Chatlet, named by half); the file comment tells the first
// and last apart. A half turned on live is not one the company does not run:
// the native runtime is built once per process, so it is served only after a
// restart ([engine.Engine.AwaitingRestart]). "Does not run it" would not say a
// restart brings it, so it is `unavailable` under
// `native_backend_needs_restart` with no wait, as Gitlet's routes answer.
func nativeAbsent(e *engine.Engine, started bool, half string) error {
	return halfAbsent(started, started && awaitsRestart(e, half), half)
}

// halfAbsent is [nativeAbsent] once the node's posture has been read: whether
// it has met its company, and whether that company turned half on over a
// runtime built without it.
func halfAbsent(started, restart bool, half string) error {
	if !started {
		return errNoCompanyYet
	}
	if restart {
		return fmt.Errorf("%w: %w", queries.ErrUnavailable, &stream.Unserved{
			Code:   httpjson.CodeNativeBackendNeedsRestart,
			Detail: httpjson.NeedsRestart(half),
		})
	}
	return fmt.Errorf("%w: this company does not run %s", queries.ErrUnknown, half)
}

// halfServices is each native half by the name the org projection's
// `services` gives its service, which is how the engine names the ones
// awaiting a restart.
var halfServices = map[string]string{
	"Worklet": "tracker", "Doclet": "knowledge", "Chatlet": "chat",
}

// awaitsRestart reports whether the company this node serves runs half and
// this node's runtime was built without it.
func awaitsRestart(e *engine.Engine, half string) bool {
	c := e.Company()
	if c == nil {
		return false
	}
	return slices.Contains(e.AwaitingRestart(c.Config), halfServices[half])
}

// liveWork is the tracker's read side, resolved per question.
type liveWork struct{ engine *engine.Engine }

// A NODE'S WORK READER IS THE WHOLE CONTRACT the questions read through: a read
// it left out would fail to compile here rather than degrade on a real node.
var _ queries.WorkReader = liveWork{}

func (l liveWork) reader() (estate.Work, error) {
	started := l.engine.NativeStarted()
	if r, ok := engine.OperatorWork(l.engine); ok {
		return r, nil
	}
	return estate.Work{}, nativeAbsent(l.engine, started, "Worklet")
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

func (l liveWork) WorkPatterns(ctx context.Context, q worklet.PatternsQuery) (worklet.WorkPatterns, error) {
	r, err := l.reader()
	if err != nil {
		return worklet.WorkPatterns{}, err
	}
	return r.WorkPatterns(ctx, q)
}

func (l liveWork) EntryMarks(ctx context.Context, level statelog.ReadLevel) (worklet.EntryMarks, error) {
	r, err := l.reader()
	if err != nil {
		return worklet.EntryMarks{}, err
	}
	return r.EntryMarks(ctx, level)
}

func (l liveWork) EntryWork(ctx context.Context, q worklet.EntryWorkQuery, now time.Time) (worklet.EntryWork, error) {
	r, err := l.reader()
	if err != nil {
		return worklet.EntryWork{}, err
	}
	return r.EntryWork(ctx, q, now)
}

func (l liveWork) FlowHours(ctx context.Context, q worklet.FlowHoursQuery) ([]worklet.FlowHours, error) {
	r, err := l.reader()
	if err != nil {
		return nil, err
	}
	return r.FlowHours(ctx, q)
}

func (l liveWork) FlowItems(ctx context.Context, q worklet.FlowHoursQuery) (map[string]int, error) {
	r, err := l.reader()
	if err != nil {
		return nil, err
	}
	return r.FlowItems(ctx, q)
}

func (l liveWork) FlowPickups(ctx context.Context, q worklet.FlowHoursQuery) ([]time.Duration, error) {
	r, err := l.reader()
	if err != nil {
		return nil, err
	}
	return r.FlowPickups(ctx, q)
}

func (l liveWork) TurnsMoved(ctx context.Context, q worklet.TurnsMovedQuery) (map[string]bool, error) {
	r, err := l.reader()
	if err != nil {
		return nil, err
	}
	return r.TurnsMoved(ctx, q)
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
	return estate.Pages{}, nativeAbsent(l.engine, started, "Doclet")
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

func (l livePages) TurnsMovedPages(ctx context.Context, q doclet.TurnsMovedQuery) (map[string]bool, error) {
	r, err := l.reader()
	if err != nil {
		return nil, err
	}
	return r.TurnsMovedPages(ctx, q)
}

// liveBacklinks is which pages link to a page, resolved per question.
//
// ITS OWN SOURCE, and a native-half one: the links are an index the engine
// keeps over Doclet's pages, which comes up with the node's first company
// and is absent for a company whose wiki is a vendor's. Read through Doclet's
// routed reader, as [livePages] is, so the data node that answers is one whose
// copies of both domains the links are named from serve — see [NativeReads].
type liveBacklinks struct{ engine *engine.Engine }

func (l liveBacklinks) LinkedFrom(ctx context.Context, pageID string) (
	search.Backlinks, error) {
	started := l.engine.NativeStarted()
	p, ok := engine.OperatorPages(l.engine)
	if !ok {
		return search.Backlinks{}, nativeAbsent(l.engine, started, "Doclet")
	}
	return p.LinkedFrom(ctx, pageID)
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
		return worklet.SearchAnswer{}, nativeAbsent(l.engine, started, "Worklet")
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
	return estate.Chat{}, nativeAbsent(l.engine, started, "Chatlet")
}

// Reach is a post's dry run ([queries.ChatDrafter]), through the same router
// the post itself is sent through.
func (l liveChat) Reach(ctx context.Context, actor chatlet.Actor, channelID, replyTo string,
	in chatlet.NewMessage) (chatlet.Draft, error) {
	r, err := l.reader()
	if err != nil {
		return chatlet.Draft{}, err
	}
	return r.Reach(ctx, actor, channelID, replyTo, in)
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

func (l liveChat) RoomNames(ctx context.Context, viewer string,
	fresh statelog.Freshness) (chatlet.RoomNames, error) {
	r, err := l.reader()
	if err != nil {
		return chatlet.RoomNames{}, err
	}
	return r.RoomNames(ctx, viewer, fresh)
}

func (l liveChat) Directory(ctx context.Context, viewer string,
	fresh statelog.Freshness) (chatlet.Directory, error) {
	r, err := l.reader()
	if err != nil {
		return chatlet.Directory{}, err
	}
	return r.Directory(ctx, viewer, fresh)
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
		return estate.ChatSearchAnswer{}, nativeAbsent(l.engine, started, "Chatlet")
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
		// THE CODE READ TOOLS, where this node's running runtime serves
		// Gitlet NOW — read per call, as the other halves are, so the
		// call after a runtime starts serving code is offered them, and on
		// exactly the terms this node's `/gitlet/` reads are.
		halves.Code = engine.CodeToolDeps(e)
		// AND WHO READ A UNIT'S REPOSITORY, beside them and on their terms.
		halves.Readers = engine.CodeReaderDeps(e)
		// AND THE HEALTH TOOLS, read per call on the deps a seat's registry
		// takes: the fleet's presence and, where Gitlet is served, the
		// Gitlet nodes' status.
		halves.Health = engine.HealthToolDeps(e)
		// AND THE ATLAS, read per call from the settings this node runs, its
		// repositories through the code half above, as the caller.
		halves.Atlas = engine.AtlasToolDeps(e)
		// AND ITS WRITES, through the router, decided on that read.
		halves.AtlasWrite = engine.AtlasWriteToolDeps(e)
		// AND THE ASK THAT A SOURCE BE READ NOW, on the coordination store.
		halves.AtlasRefresh = engine.AtlasRefreshToolDeps(e)
		// AND A PERSON'S HOME BOARD, on the coordination store too.
		halves.HomeBoard = engine.HomeBoardToolDeps(e)
		// AND THEIR OWN ASK CREWLET INSTRUCTIONS, beside it.
		halves.Instructions = engine.InstructionsToolDeps(e)
		// AND A SCHEDULE RUN NOW, claimed on the fleet and sent from here.
		halves.ScheduleRuns = engine.ScheduleRunToolDeps(e)
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
