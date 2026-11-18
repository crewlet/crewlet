package estate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/usage"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The serving half: what a data node answers with, declared here by the
// consumer and kept to what the operations below and in codeops.go call. The
// operator's own surfaces (dashboard, REST, the operator's MCP) reach the
// estate through this node's router as a seat's tools do, so a node whose copy
// of a domain is out of service answers them from a peer's copy, and a node
// holding no data serves them at all.

// TrackerReader is the tracker's read side the surfaces ask of.
type TrackerReader interface {
	Tasks(ctx context.Context, q worklet.Query, now time.Time) (worklet.Answer, error)
	Task(ctx context.Context, idOrKey string, want worklet.DetailWants,
		fresh statelog.Freshness) (worklet.TaskDetail, error)
	Views(ctx context.Context, q worklet.ViewQuery) (worklet.ViewListing, error)
	ExpandedQuery(ctx context.Context, params map[string]any,
		viewer worklet.Viewer, now time.Time, loc *time.Location) (worklet.Query, error)
	Catalogue(ctx context.Context, q worklet.CatalogueQuery) (worklet.CatalogueAnswer, error)
	Person(ctx context.Context, q worklet.PersonQuery, now time.Time) (worklet.PersonState, error)
	Thread(ctx context.Context, q worklet.ThreadQuery,
		fresh statelog.Freshness) (worklet.ResolvedThread, error)
	WorkPatterns(ctx context.Context, q worklet.PatternsQuery) (worklet.WorkPatterns, error)
	TurnsMoved(ctx context.Context, q worklet.TurnsMovedQuery) (map[string]bool, error)
	Activity(ctx context.Context, q worklet.ActivityQuery, now time.Time) (
		worklet.ActivityAnswer, error)
	MyWork(ctx context.Context, q worklet.MyWorkQuery, now time.Time,
		loc *time.Location) (worklet.MyWork, error)
	ReadyWork(ctx context.Context, q worklet.ReadyWorkQuery, now time.Time) (
		worklet.ReadyWork, error)
	PickupAnswer(ctx context.Context, q worklet.PickupAnswerQuery) (types.PickupAnswer, error)
	OwnAsks(ctx context.Context, q worklet.OwnAsksQuery, now time.Time) (worklet.OwnAsks, error)
	EntryMarks(ctx context.Context, level statelog.ReadLevel) (worklet.EntryMarks, error)
	EntryWork(ctx context.Context, q worklet.EntryWorkQuery, now time.Time) (worklet.EntryWork, error)
	Projects(ctx context.Context, q worklet.ProjectQuery) (worklet.ProjectListing, error)
	Project(ctx context.Context, q worklet.ProjectDetailQuery) (worklet.ProjectDetail, error)
	Files(ctx context.Context, q worklet.FileQuery) (worklet.FileListing, error)
	File(ctx context.Context, project, path string, fresh statelog.Freshness) (worklet.FileDetail, error)

	// The operator's reads: a unit's workload, a person's inbox and the
	// routing a change took.
	Workload(ctx context.Context, q worklet.WorkloadQuery, now time.Time,
		loc *time.Location) (worklet.WorkloadAnswer, error)
	Inbox(ctx context.Context, q worklet.InboxQuery, now time.Time) (worklet.InboxAnswer, error)
	Routing(ctx context.Context, q worklet.RoutingQuery, now time.Time) (worklet.RoutingAnswer, error)
	TurnsOf(ctx context.Context, idOrKey, cursor string, limit int,
		fresh statelog.Freshness) (worklet.TaskTurns, error)
	EveryView(ctx context.Context, q worklet.EveryViewQuery) (worklet.ViewListing, error)
	Flow(ctx context.Context, q worklet.FlowQuery, now time.Time,
		loc *time.Location) (worklet.FlowAnswer, error)
	CompanyFeed(ctx context.Context, q worklet.FeedQuery) (worklet.FeedPage, error)
	Decisions(ctx context.Context, q worklet.DecisionsQuery, now time.Time,
		loc *time.Location) (worklet.DecisionsAnswer, error)
	AnsweredAsks(ctx context.Context, q worklet.DecisionsQuery, now time.Time,
		loc *time.Location) (worklet.AnsweredAnswer, error)
	TurnPlaces(ctx context.Context, runs []string,
		fresh statelog.Freshness) (map[string]worklet.TurnPlace, error)
}

// TrackerWriter is the tracker's write side, as ONE actor — see [Actor].
type TrackerWriter interface {
	CreateTask(ctx context.Context, opID string, task worklet.Task,
		notify *worklet.Notify) (worklet.WriteResult, error)
	CreateTaskAsking(ctx context.Context, opID string, task worklet.Task,
		ask worklet.Comment, notify *worklet.Notify) (worklet.WriteResult, error)
	PlaceTask(ctx context.Context, opID string, place worklet.Place,
		notify *worklet.Notify) (worklet.PlaceResult, error)
	UpdateTask(ctx context.Context, opID, id, project string, ifMatch uint64,
		patch worklet.TaskPatch, kind worklet.ChangeKind,
		notify *worklet.Notify) (worklet.WriteResult, error)
	Depend(ctx context.Context, opID string, change worklet.DependencyChange,
		leads worklet.Leads) (worklet.DependencyResult, error)
	MergeDuplicates(ctx context.Context, opID, duplicate, into string,
		reparent bool, notify *worklet.Notify) (worklet.WriteResult, error)
	MoveTaskToProject(ctx context.Context, opID, taskID, target string,
		notify *worklet.Notify) (worklet.WriteResult, error)
	WriteProject(ctx context.Context, opID, key string, edit worklet.ProjectEdit,
		authority worklet.ProjectAuthority) (worklet.WriteResult, error)
	WriteTags(ctx context.Context, opID, project string, edit worklet.TagEdit,
		authority worklet.TagAuthority) (worklet.WriteResult, error)
	EnsureTags(ctx context.Context, opID, project string, tags []string) (
		[]string, []caveat.Caveat, error)
	PutFile(ctx context.Context, opID string, put worklet.FilePut) (worklet.WriteResult, error)
	RemoveFile(ctx context.Context, opID, project, path string, ifMatch uint64) (worklet.WriteResult, error)
	RecordTurn(ctx context.Context, opID string, turn worklet.TurnRecord) (worklet.WriteResult, error)

	// The operator's writes, which no seat is handed.
	//
	// ViewPrior is the READ a save is decided on: a save replaces a view
	// whole by id, so who may make it depends on the view it overwrites too.
	// It is on the writer because the save is, and the save refuses a prior
	// that no longer holds.
	ViewPrior(ctx context.Context, id string) (worklet.ViewPrior, error)
	WriteView(ctx context.Context, opID string, view worklet.View,
		prior worklet.ViewPrior) (worklet.WriteResult, error)
	DeleteView(ctx context.Context, opID, id string,
		prior worklet.ViewPrior) (worklet.WriteResult, error)
	WriteTypes(ctx context.Context, opID string, types []worklet.TaskType) (worklet.WriteResult, error)
	WriteFields(ctx context.Context, opID string, fields []worklet.FieldDef) (worklet.WriteResult, error)
	MarkInbox(ctx context.Context, opID, handle string, seen statelog.Position,
		gesture inbox.Gesture[worklet.Reason], authority worklet.PersonAuthority) (worklet.WriteResult, error)
	WritePins(ctx context.Context, opID, handle string, gesture worklet.PinGesture,
		authority worklet.PersonAuthority) (worklet.WriteResult, error)
	EditComment(ctx context.Context, opID, taskID, project, commentID, body string,
		notify *worklet.Notify) (worklet.WriteResult, error)
	ResolveComment(ctx context.Context, opID, taskID, project, commentID string,
		withdrawal *worklet.Withdrawal, notify *worklet.Notify) (worklet.WriteResult, error)
	RemoveComment(ctx context.Context, opID, taskID, project, commentID string,
		authority worklet.CommentAuthority) (worklet.WriteResult, error)
	WritePriorities(ctx context.Context, opID, handle string, priorities []string,
		ifMatch *uint64, authority worklet.PersonAuthority) (worklet.WriteResult, error)
	RemoveTask(ctx context.Context, opID, id, project string, subtree bool,
		notify *worklet.Notify) (worklet.WriteResult, error)
	RestoreTask(ctx context.Context, opID, id, project string, alone bool,
		notify *worklet.Notify) (worklet.WriteResult, error)
	PurgeTask(ctx context.Context, opID, id, project, reason string) (worklet.WriteResult, error)
}

// WorkSearcher is the tracker's ranked search over this node's corpus.
type WorkSearcher interface {
	Search(ctx context.Context, q worklet.SearchQuery) (worklet.SearchAnswer, error)
}

// PageReader is the knowledge base's read side.
type PageReader interface {
	List(ctx context.Context, f doclet.Filter, fresh statelog.Freshness) (doclet.Listing, error)
	Get(ctx context.Context, ref string, fresh statelog.Freshness) (doclet.Detail, error)
	SkillPages(ctx context.Context, container string, fresh statelog.Freshness) ([]doclet.Page, error)

	// The operator's reads: every container, what happened to a page or
	// a container, and one revision of a page's body.
	Containers(ctx context.Context, fresh statelog.Freshness) ([]doclet.ContainerListing, error)
	Activity(ctx context.Context, q doclet.PageActivityQuery) (doclet.PageActivity, error)
	Revision(ctx context.Context, pageID string, version int,
		fresh statelog.Freshness) (doclet.Revision, bool, error)

	// TurnsMovedPages is which of a set of turns moved work on the pages.
	TurnsMovedPages(ctx context.Context, q doclet.TurnsMovedQuery) (map[string]bool, error)
}

// PageWriter is the knowledge base's write side. The author, carrying the
// caller's operation key ([doclet.Actor.OpKey]), is an argument of every call,
// as Doclet's store takes it.
type PageWriter interface {
	Create(ctx context.Context, actor doclet.Actor, in doclet.NewPage) (doclet.Written, error)
	SavePage(ctx context.Context, actor doclet.Actor, pageID string, save doclet.Save) (doclet.Written, error)
	Rename(ctx context.Context, actor doclet.Actor, pageID, title string,
		quiet bool) (doclet.Written, error)
	Comment(ctx context.Context, actor doclet.Actor, pageID string,
		in doclet.NewComment) (doclet.Comment, doclet.Written, error)
	EditComment(ctx context.Context, actor doclet.Actor, pageID, commentID,
		body string) (doclet.Comment, doclet.Written, error)

	// The operator's writes, which no seat is handed: the trash, the purge
	// and a remark's take-down.
	Trash(ctx context.Context, actor doclet.Actor, pageID string) (doclet.Written, error)
	Restore(ctx context.Context, actor doclet.Actor, pageID string) (doclet.Written, error)
	Purge(ctx context.Context, actor doclet.Actor, pageID, reason string) (doclet.Written, error)
	RemoveComment(ctx context.Context, actor doclet.Actor, pageID, commentID string,
		authority doclet.CommentAuthority) (doclet.Written, error)
}

// KnowledgeSearcher is the Doclet knowledge search over this node's corpus,
// with Building, which a caller asks of an empty answer.
//
// ANSWER, NOT THE BEST-EFFORT SEARCH ([doclet.Searcher.Answer]): a search that
// could not run must be an error, since an empty answer carried back over the
// wire reads as "nothing matched".
type KnowledgeSearcher interface {
	Answer(ctx context.Context, q knowledge.Query) (knowledge.Result, error)
	Building(ctx context.Context) bool
}

// BacklinkReader is a page's "linked from": the pages and tasks whose bodies
// link to it, from a node's own index over both corpora and named from the
// rows of each ([search.Backlinks]).
type BacklinkReader interface {
	LinkedFrom(ctx context.Context, pageID string) (search.Backlinks, error)
}

// UsageReader is the usage domain's read side (ADR-0020): the spend, activity
// and skill-reader answers the operator's surfaces draw.
//
// A READ THAT GROWS WITH ITS WINDOW IS ASKED A PAGE AT A TIME, resuming after
// the group the previous page ended on and cut by [usage.Fill] at a budget
// ([Backend.pageBytes]): a ninety-day window is tens of megabytes of rows, and
// a reply is one message. The schedule runs are a page the caller sizes, the
// elided counts are numbers, and [opUsageServerToolCalls] does not grow with
// the window.
type UsageReader interface {
	SpendPage(ctx context.Context, q usage.SpendQuery, after usage.Group, budget int) (usage.Page[usage.SpendRow], error)
	SeatTurnsPage(ctx context.Context, q usage.SpendQuery, after usage.Group, budget int) (usage.Page[usage.TurnsRow], error)
	PersonSpendPage(ctx context.Context, q usage.PersonQuery, after usage.Group, budget int) (usage.Page[usage.PersonRow], error)
	SeatDaysPage(ctx context.Context, q usage.SpendQuery, after usage.Group, budget int) (usage.Page[usage.SeatDay], error)
	PageReadsPage(ctx context.Context, q usage.PageReadsQuery, after usage.Group, budget int) (usage.Page[usage.PageRead], error)
	ScheduleRuns(ctx context.Context, q usage.ScheduleRunsQuery) ([]usage.ScheduleRun, bool, error)
	ReadsElided(ctx context.Context, from, to string) (int64, error)
	ServerToolCalls(ctx context.Context, q usage.ServerToolCallsQuery) ([]usage.ServerToolCallsRow, error)
	ToolsElided(ctx context.Context, from, to string) (int64, error)
	TurnLedger(ctx context.Context, q usage.SpendQuery) ([]usage.TurnLedgerRow, error)
	RepoReadsPage(ctx context.Context, q usage.RepoReadsQuery, after usage.Group, budget int) (usage.Page[usage.RepoReader], error)
	RepoReadsElided(ctx context.Context, q usage.RepoReadsQuery) (int64, error)
}

// IdentityReader is the identity directory's read side as a node holding no
// data asks it: whose record a login names, who holds the seats (every one for
// the party registry, ONE for a turn composing the person it acts for), and
// the row a presented machine token is checked against.
//
// FACTS, NOT VERDICTS, as [iamdomain.Reader] answers them. What a stalled
// applier or a retained record means, the chart a binding resolves against,
// the declared Tier A tokens and whether a token is good are the ASKING
// node's: its request path decides on them, and a verdict formed elsewhere
// would be a second opinion about whose seat a login or a credential is.
type IdentityReader interface {
	PersonByLoginVouched(ctx context.Context, login string) (iamdomain.Sighting,
		iamdomain.Vouch, error)
	SeatHolders(ctx context.Context) ([]iamdomain.SeatHolder, error)
	Requester(ctx context.Context, seat string) (iamdomain.Requester, bool, error)
	MachineToken(ctx context.Context, id string) (credential.TokenRow, error)
}

// ChatReader is the engine's own chat's read side.
//
// THE VIEWER IS AN ARGUMENT OF EVERY READ, the domain's own shape: every read
// is answered for one seat, and a room that seat may not read is answered
// exactly as one nobody holds, on whichever node answers ([chatlet.Visible],
// judged against that snapshot's membership rows). The viewer is the seat the
// asking node resolved its caller to; the reader refuses an empty one.
type ChatReader interface {
	Channels(ctx context.Context, viewer string, q chatlet.ChannelsQuery,
		fresh statelog.Freshness) (chatlet.ChannelListing, error)
	Channel(ctx context.Context, viewer, channelID string,
		fresh statelog.Freshness) (chatlet.ChannelDetail, error)
	ChannelNamed(ctx context.Context, viewer, name string,
		fresh statelog.Freshness) (chatlet.ChannelDetail, error)
	Messages(ctx context.Context, viewer string, q chatlet.TranscriptQuery,
		fresh statelog.Freshness) (chatlet.Transcript, error)
	Thread(ctx context.Context, viewer string, q chatlet.ThreadQuery,
		fresh statelog.Freshness) (chatlet.Thread, error)
	// TurnThread is Thread's one read with whom each of its messages was
	// written for ([chatlet.TurnThread]): the engine's turn-start read alone
	// ([TurnThreads]), which no reader's surface asks.
	TurnThread(ctx context.Context, viewer string, q chatlet.ThreadQuery,
		fresh statelog.Freshness) (chatlet.TurnThread, error)
	RoomNames(ctx context.Context, viewer string,
		fresh statelog.Freshness) (chatlet.RoomNames, error)
	Directory(ctx context.Context, viewer string,
		fresh statelog.Freshness) (chatlet.Directory, error)

	// The reader's own views ([ChatReaderViews], chatreader.go).
	ChatReaderViews

	// Readable is every room the viewer may READ, wider than the rooms it is
	// in: no operation of its own, but the authorization half of
	// [opChatSearch], resolved on the node whose index the search reads, so
	// the set, the hits and the names they are shown under are one copy's.
	Readable(ctx context.Context, viewer string,
		fresh statelog.Freshness) ([]chatlet.ReadableRoom, error)

	// At is this copy's applied position on the chat log.
	At() statelog.Position
}

// ChatWriter is the engine's own chat's write side. The author, carrying the
// caller's operation key ([chatlet.Actor.OpKey]), is an argument of every
// call, since one node's chat serves every seat through one store.
//
// An admission crosses as a VALUE, never the grants, for [removeCommentArgs]'
// reason: a take-down ([chatlet.MessageAuthority]) or an archive
// ([chatlet.ChannelAuthority]) is decided on the asking node and judged in the
// serving node's snapshot. Retention is an admin's own gesture
// ([ChatWriter.SetRetention]), never a field a member's patch could carry. No
// operation sets a unit room's MEMBERSHIP: the chart's apply on each data node
// is its sole writer, and an operation would be one the chart cannot see.
type ChatWriter interface {
	CreateChannel(ctx context.Context, actor chatlet.Actor, in chatlet.NewChannel) (chatlet.Written, error)
	OpenDirect(ctx context.Context, actor chatlet.Actor, participants []string) (chatlet.Written, bool, error)
	PatchChannel(ctx context.Context, actor chatlet.Actor, channelID string, patch chatlet.ChannelPatch,
		authority chatlet.ChannelAuthority) (chatlet.Written, error)
	SetMembers(ctx context.Context, actor chatlet.Actor, channelID string,
		members []chatlet.Member, expected *[]chatlet.Member) (chatlet.Written, error)
	AddMembers(ctx context.Context, actor chatlet.Actor, channelID string,
		handles []string) (chatlet.Written, []string, error)
	Join(ctx context.Context, actor chatlet.Actor, channelID string) (chatlet.Written, error)
	Leave(ctx context.Context, actor chatlet.Actor, channelID string) (chatlet.Written, error)
	Post(ctx context.Context, actor chatlet.Actor, channelID string,
		in chatlet.NewMessage) (chatlet.Written, error)
	Reply(ctx context.Context, actor chatlet.Actor, channelID, replyTo string,
		in chatlet.NewMessage) (chatlet.Written, error)
	// Reach is a post's DRY RUN: whom it would reach if it were sent now,
	// decided by the post's own decision against the serving node's
	// snapshot and roster, and published nowhere ([chatlet.Store.Reach]).
	Reach(ctx context.Context, actor chatlet.Actor, channelID, replyTo string,
		in chatlet.NewMessage) (chatlet.Draft, error)
	Edit(ctx context.Context, actor chatlet.Actor, channelID, messageID string,
		in chatlet.EditMessage) (chatlet.Written, error)
	Delete(ctx context.Context, actor chatlet.Actor, channelID, messageID string,
		authority chatlet.MessageAuthority) (chatlet.Written, error)
	React(ctx context.Context, actor chatlet.Actor, channelID, messageID,
		emoji string) (chatlet.Written, error)
	Unreact(ctx context.Context, actor chatlet.Actor, channelID, messageID,
		emoji string) (chatlet.Written, error)

	// The admin's writes, which no seat is handed; the erase and the prune
	// take the operation id the caller derived, as a purge does.
	SetRetention(ctx context.Context, actor chatlet.Actor, channelID string,
		days *int) (chatlet.Written, error)
	Erase(ctx context.Context, actor chatlet.Actor, opID, channelID string,
		messageIDs []string, reason string) (chatlet.Written, error)
	Prune(ctx context.Context, actor chatlet.Actor, opID, channelID string,
		cutoff time.Time) (chatlet.Written, error)
}

// ChatSearcher is the chat keyword index over this node's corpus, not a search
// for a viewer: it takes the visible rooms on the query and refuses a query
// naming none ([search.ErrNoViewer]). [opChatSearch] resolves those rooms from
// the viewer, and asks the index's readiness ([search.ChatIndexer.Ready]), on
// the same node.
type ChatSearcher interface {
	SearchMessages(ctx context.Context, q search.ChatQuery) ([]search.ChatHit, error)
	Ready(ctx context.Context) (bool, error)
}

// Backend is one data node's answer to every operation, over its copy of the
// estate, resolved PER REQUEST: a node that booted with no company brings its
// native runtime up at its first apply, and a request before then is answered
// "not here" rather than refused for ever.
//
// A nil half is one this node does not run, answered [unservedNoBackend].
type Backend struct {
	Tracker    TrackerReader
	Writer     func(Actor) TrackerWriter
	WorkSearch WorkSearcher
	Pages      PageReader
	PageWriter PageWriter
	Knowledge  KnowledgeSearcher
	Backlinks  BacklinkReader
	Usage      UsageReader
	Chat       ChatReader
	ChatWriter ChatWriter
	ChatSearch ChatSearcher

	// Code is this node's code half — its committer and its reader
	// ([gitlet.Served]) — or nil where the company hosts no code: every code
	// operation is then answered "no backend here".
	Code CodeServer

	// Git is this node's git query server, set by the dispatcher for a GIT
	// operation alone ([GitCopy]) and nil on every backend a copy hands out:
	// a git operation reads no domain's copy.
	Git GitServer

	// Identity is the identity directory, held from boot by every data node;
	// nil only until the core runtime is up, answered like any half it does
	// not run.
	Identity IdentityReader

	// Artifacts is the artifacts estate, run from boot by every data node
	// ([ArtifactsHalf]): each side nil until the core runtime is up, and the
	// writer nil where the node publishes nothing.
	Artifacts ArtifactsHalf

	// ArtifactData is the artifact datastore, which every data node runs
	// from boot beside the artifacts estate ([ArtifactDataHalf]).
	ArtifactData ArtifactDataHalf

	// Executor is the executor log, run from boot by every data node
	// ([ExecutorHalf]): its writer only while the node publishes, each side
	// nil until the core runtime is up.
	Executor ExecutorHalf

	// Access is the access log, which every data node runs from boot, on the
	// executor log's terms ([AccessHalf]).
	Access AccessHalf

	// Atlas is the atlas log, which every data node runs from boot
	// ([AtlasHalf]).
	Atlas AtlasHalf

	// Reached waits until this node's applier of the floor's log has reached
	// a position, every request's floor the node's own included: every
	// row-writing record through it committed and every barrier decoded
	// ([statelog.Runner.WaitReached]). A floor may be a barrier (a read
	// answers with the position its rows are current through), and a
	// barrier's own commit is nothing the request reads.
	Reached func(ctx context.Context, at statelog.Position) error

	// Answers is whether this copy OF THE OPERATION'S DOMAIN may answer a
	// request now ([statelog.Health.Answers]): serving (drained since its
	// applier started and within the snapshot slack of its log's end) or
	// level this instant. A copy that does not LAGS: a worse node, asked only
	// once every copy that does has run nothing ([Router.route]; package doc,
	// "A domain's copy that is WRONG is out of service; one that LAGS is a
	// worse node"). Another domain's lag never holds it back (ADR-0079, [ready]).
	//
	// NOT A SEAT'S ADMISSION ([Backend.Admits]), which refuses at a lag of
	// one: a busy company nearly always has a record in flight, and such a
	// gate would send every read to whichever peer happened to be level, or
	// to none. What a read must see of its node's writes, the floors carry.
	Answers func(ctx context.Context) bool

	// Admits is whether a SEAT may attach to this copy now: at a lag of zero
	// on every domain that gates seat admission ([Router.Serves]). Only
	// admission's ping reads it.
	Admits func(ctx context.Context) bool

	// Current is whether this node serves the company's configuration: false
	// exactly while the control plane has CONFIRMED it behind the fleet's
	// activated epoch (shed or stuck), never on lag alone. Nil is current.
	//
	// A node behind it is a WORSE NODE TO ASK, never no node (package doc, "A
	// node behind the company's configuration is a worse node too"): every
	// other data node is asked first, and an asker that heard it once
	// remembers it ([unservedBehindConfig], [Router.markBehindConfig]). The
	// confirmation comes in about a minute: configplane.LagGrace after the
	// node first reads the new pointer, or sooner where its own apply fails
	// while a peer has applied, and moves at its next posture decision.
	Current func() bool

	// ServerSeams are the node's own: the dispatcher sets them on every
	// backend it hands an operation.
	ServerSeams

	// verifiers are the keys a machine token's narrowed answer is MAC'd
	// under ([TokenFacts]), from the node's envelopes: a NODE'S, never its
	// copy's, so the dispatcher (the server, or this node's router
	// in-process) sets them on every backend it hands an operation, as
	// [ServerSeams]; a backend is resolved per request, the keyring once.
	verifiers verifierKeys

	// pageBytes is the most one answer of a PAGED read carries before it is
	// cut ([usage.Fill]): [usagePageBytes] when the server answers a request
	// off the wire, and zero (the whole read, one page) when this node's
	// router runs it in-process: nothing carries that answer, and paging it
	// costs N transactions and snapshots plus an encoding of every row. Only
	// the server sets it.
	pageBytes int
}

// ServerSeams are what an operation reads of the SERVING node rather than of
// its copy: the chart seams a query or a dependency consults and the org a
// knowledge search is scoped by — the node's current epoch, which is what the
// package doc says crosses no wire — and the node's own event log.
type ServerSeams struct {
	// Units is attached to every tracker read that renders a project's
	// unit, and Leads is handed to a dependency.
	Units worklet.Units
	Leads worklet.Leads

	// Seat resolves a searching seat's handle to its role and the org its
	// read scope comes from, against this node's current epoch.
	Seat func(handle string) (*org.Role, *org.Organization)
}

// errNoHalf is an operation on a half this node does not run.
var errNoHalf = errors.New("estate: this node runs no native backend for this operation")

// declined is a half that RAN NOTHING for a reason of its own: a code committer
// that refused before appending ([ranNothing]), or a node holding no key to MAC
// a machine token's answer under ([opIdentityMachineToken]). The router and
// the server read it as errNoHalf, so every class moves on from it alike, and
// the walk's account keeps its wording ([ranNothingDetail]).
type declined struct{ cause error }

func (d declined) Error() string   { return d.cause.Error() }
func (d declined) Unwrap() []error { return []error{errNoHalf, d.cause} }

// ranNothingDetail is what a node's answer says of a half that ran nothing:
// that the node runs none, or a declined half's own reason, so an estate
// nobody served names why each node passed.
func ranNothingDetail(self, op string, err error) string {
	var why declined
	if errors.As(err, &why) {
		return fmt.Sprintf("%s ran nothing for %s: %v", self, op, why.cause)
	}
	return fmt.Sprintf("%s runs no native backend for %s", self, op)
}

// errNotAdmitting is admission's ping on a copy that admits no seat yet
// ([Backend.Admits]): nothing ran, and the asker moves on.
var errNotAdmitting = errors.New("estate: this copy admits no seat yet")

// SeatCopy is the domain an operation names when it asks about the copy a SEAT
// is served by: every domain that gates seat admission
// ([statelog.Domain.GatesSeatAdmission]) in service at once (ADR-0079).
// [opPing] and [Router.Serves] name it.
//
// No operation that reads a domain's rows may name it
// (TestEveryOperationNamesTheCopyItReads).
const SeatCopy = ""

// The domains an operation may read, and each one's log: the one its floor is
// on unless it is floorless ([op.floorless]). The floor is on its OWN domain's
// log and no other ([ready]).
//
// ELEVEN of the estate's thirteen domains: a fleet duty writes the vectors and
// the searches read them as an index, never at a position, and the Dashboard
// Assistant's conversations are read and written only on the data node the API
// runs on. The executor and access logs are here though no surface writes
// them: a seat's node records them, `data` or not, and only a data node
// publishes ([originEngine], executor.go, access.go).
var (
	trackerDomain      = worklet.Domain{}.Name()
	pagesDomain        = doclet.Domain{}.Name()
	usageDomain        = usage.Domain{}.Name()
	codeDomain         = gitlet.Domain{}.Name()
	identityDomain     = iamdomain.Domain{}.Name()
	artifactsDomain    = artifactsdomain.Domain{}.Name()
	artifactDataDomain = artifactdata.Domain{}.Name()
	chatDomain         = chatlet.Domain{}.Name()
	executorDomain     = executordomain.Domain{}.Name()
	accessDomain       = accessdomain.Domain{}.Name()
	atlasDomain        = atlasdomain.Domain{}.Name()

	floorStreams = map[string]string{
		trackerDomain:      worklet.Domain{}.Stream().Name,
		pagesDomain:        doclet.Domain{}.Stream().Name,
		usageDomain:        usage.Domain{}.Stream().Name,
		codeDomain:         gitlet.Domain{}.Stream().Name,
		identityDomain:     iamdomain.Domain{}.Stream().Name,
		artifactsDomain:    artifactsdomain.Domain{}.Stream().Name,
		artifactDataDomain: artifactdata.Domain{}.Stream().Name,
		chatDomain:         chatlet.Domain{}.Stream().Name,
		executorDomain:     executordomain.Domain{}.Stream().Name,
		accessDomain:       accessdomain.Domain{}.Stream().Name,
		atlasDomain:        atlasdomain.Domain{}.Stream().Name,
	}
)

// ---- the tracker's reads ------------------------------------------------ //

type tasksArgs struct {
	Query worklet.Query
	Now   time.Time
}

var opTasks = define("tracker.tasks", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a tasksArgs) (worklet.Answer, error) {
		if b.Tracker == nil {
			return worklet.Answer{}, errNoHalf
		}
		a.Query.Units = b.Units
		return b.Tracker.Tasks(ctx, a.Query, a.Now)
	})

type taskArgs struct {
	IDOrKey string
	Want    worklet.DetailWants
	Fresh   statelog.Freshness

	// Zone is the name of the zone the asker's day clock is on
	// ([worklet.DayClock.Zone], [zoneArg]).
	Zone string
}

var opTask = define("tracker.task", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a taskArgs) (worklet.TaskDetail, error) {
		if b.Tracker == nil {
			return worklet.TaskDetail{}, errNoHalf
		}
		if a.Want.Clock != nil {
			loc, err := zoneOf(a.Zone)
			if err != nil {
				return worklet.TaskDetail{}, err
			}
			clock := *a.Want.Clock
			clock.Zone = loc
			a.Want.Clock = &clock
		}
		a.Want.Units = b.Units
		return b.Tracker.Task(ctx, a.IDOrKey, a.Want, a.Fresh)
	})

// viewsArgs is a views query as it crosses, with the zone its pinned counts
// cut relative dates on carried by name ([zoneArg]).
type viewsArgs struct {
	Query worklet.ViewQuery
	Zone  string
}

// ONE CONTAINER'S STRIP OF VIEWS, as a screen shows it — the dashboard's tabs,
// the API's listing: read leniently.
var opViews = define("tracker.views", opRead, trackerDomain, false, views)

// THE STRIP A VIEW IS SAVED AGAIN FROM, ANSWERED WHOLE ([op.answeredWhole]):
// save_work_view replaces a view whole, so a call changing one thing re-sends
// the rest as list_work_views showed it (internal/agent/builtin), and a newer
// build's field this build would drop is refused here rather than saved away.
// Its own operation, so a screen that only shows the strip is not refused one
// ([opArtifactVersionCarried] says why).
//
// Not a save that keeps what it does not name: a field left at its zero
// crosses exactly as one an older build never sent ([define]), so a merge
// could not tell cleared from unknown ([worklet.Writer.WriteView]).
var opViewsCarried = define("tracker.views_carried", opRead, trackerDomain, false,
	views).answeredWhole()

// views is one container's strip of views, read from this node's copy.
func views(ctx context.Context, b Backend, _ *Actor, a viewsArgs) (worklet.ViewListing, error) {
	if b.Tracker == nil {
		return worklet.ViewListing{}, errNoHalf
	}
	loc, err := zoneOf(a.Zone)
	if err != nil {
		return worklet.ViewListing{}, err
	}
	q := a.Query
	q.Units, q.Zone = b.Units, loc
	return b.Tracker.Views(ctx, q)
}

// zoneArg is a location as it crosses: its NAME, because a *time.Location is a
// table of transitions a JSON encoder writes as nothing at all. Empty is a nil
// location.
func zoneArg(loc *time.Location) string {
	if loc == nil {
		return ""
	}
	return loc.String()
}

// zoneOf is the location a request named, loaded through the one loader the
// company's clock is read with ([period.LoadZone], ADR-0018) — which refuses
// `Local`, since the serving node's own clock is not the asker's — or nil for
// a request that named none.
func zoneOf(name string) (*time.Location, error) {
	if name == "" {
		return nil, nil
	}
	loc, err := period.LoadZone(name)
	if err != nil {
		return nil, fmt.Errorf("estate: the asking node's zone is not one this "+
			"node can load: %w", err)
	}
	return loc, nil
}

type expandArgs struct {
	Params map[string]any
	Viewer worklet.Viewer
	Now    time.Time

	// Zone is the location's name ([zoneArg]).
	Zone string
}

// A LIST'S PARAMETERS EXPANDED INTO THE QUERY ITS READ ASKS, ANSWERED WHOLE
// ([op.answeredWhole]): every caller hands the answer to tracker.tasks as that
// read's query — a seat's list_work_items, a screen's list of work items — so
// a filter a newer data node parsed and this build would drop is refused rather
// than asked without, which would answer more than the caller asked for.
var opExpandedQuery = define("tracker.expanded_query", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a expandArgs) (worklet.Query, error) {
		if b.Tracker == nil {
			return worklet.Query{}, errNoHalf
		}
		loc, err := zoneOf(a.Zone)
		if err != nil {
			return worklet.Query{}, err
		}
		return b.Tracker.ExpandedQuery(ctx, a.Params, a.Viewer, a.Now, loc)
	}).answeredWhole()

// THE WORKSPACE CATALOGUE, as a screen shows it: read leniently.
var opCatalogue = define("tracker.catalogue", opRead, trackerDomain, false, catalogue)

// THE CATALOGUE A CALLER SENDS BACK, ANSWERED WHOLE ([op.answeredWhole]): its
// writes replace every declaration whole, and write_work_catalogue tells a
// model to send back what get_work_catalogue gave (internal/agent/builtin), so
// a newer build's field this build would drop is refused rather than written
// away. Its own operation, so a screen that only shows the catalogue is not
// refused one ([opArtifactVersionCarried] says why).
var opCatalogueCarried = define("tracker.catalogue_carried", opRead, trackerDomain, false,
	catalogue).answeredWhole()

// catalogue is the workspace catalogue, read from this node's copy.
func catalogue(ctx context.Context, b Backend, _ *Actor, q worklet.CatalogueQuery) (worklet.CatalogueAnswer, error) {
	if b.Tracker == nil {
		return worklet.CatalogueAnswer{}, errNoHalf
	}
	return b.Tracker.Catalogue(ctx, q)
}

type personArgs struct {
	Query worklet.PersonQuery
	Now   time.Time
}

var opPerson = define("tracker.person", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a personArgs) (worklet.PersonState, error) {
		if b.Tracker == nil {
			return worklet.PersonState{}, errNoHalf
		}
		return b.Tracker.Person(ctx, a.Query, a.Now)
	})

type threadArgs struct {
	Query worklet.ThreadQuery
	Fresh statelog.Freshness
}

var opThread = define("tracker.thread", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a threadArgs) (worklet.ResolvedThread, error) {
		if b.Tracker == nil {
			return worklet.ResolvedThread{}, errNoHalf
		}
		return b.Tracker.Thread(ctx, a.Query, a.Fresh)
	})

var opWorkPatterns = define("tracker.work_patterns", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q worklet.PatternsQuery) (worklet.WorkPatterns, error) {
		if b.Tracker == nil {
			return worklet.WorkPatterns{}, errNoHalf
		}
		return b.Tracker.WorkPatterns(ctx, q)
	})

var opTurnsMoved = define("tracker.turns_moved", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q worklet.TurnsMovedQuery) (map[string]bool, error) {
		if b.Tracker == nil {
			return nil, errNoHalf
		}
		return b.Tracker.TurnsMoved(ctx, q)
	})

type activityArgs struct {
	Query worklet.ActivityQuery
	Now   time.Time
}

var opActivity = define("tracker.activity", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a activityArgs) (worklet.ActivityAnswer, error) {
		if b.Tracker == nil {
			return worklet.ActivityAnswer{}, errNoHalf
		}
		return b.Tracker.Activity(ctx, a.Query, a.Now)
	})

type myWorkArgs struct {
	Query worklet.MyWorkQuery
	Now   time.Time

	// Zone is the company clock the asker cut "today" on ([zoneArg]).
	Zone string
}

var opMyWork = define("tracker.my_work", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a myWorkArgs) (worklet.MyWork, error) {
		if b.Tracker == nil {
			return worklet.MyWork{}, errNoHalf
		}
		loc, err := zoneOf(a.Zone)
		if err != nil {
			return worklet.MyWork{}, err
		}
		return b.Tracker.MyWork(ctx, a.Query, a.Now, loc)
	})

type readyWorkArgs struct {
	Query worklet.ReadyWorkQuery
	Now   time.Time
}

var opReadyWork = define("tracker.ready_work", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a readyWorkArgs) (worklet.ReadyWork, error) {
		if b.Tracker == nil {
			return worklet.ReadyWork{}, errNoHalf
		}
		return b.Tracker.ReadyWork(ctx, a.Query, a.Now)
	})

var opPickupAnswer = define("tracker.pickup_answer", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q worklet.PickupAnswerQuery) (types.PickupAnswer, error) {
		if b.Tracker == nil {
			return "", errNoHalf
		}
		return b.Tracker.PickupAnswer(ctx, q)
	})

// ownAsksArgs is [opOwnAsks]' input.
type ownAsksArgs struct {
	Query worklet.OwnAsksQuery
	Now   time.Time
}

var opOwnAsks = define("tracker.own_asks", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a ownAsksArgs) (worklet.OwnAsks, error) {
		if b.Tracker == nil {
			return worklet.OwnAsks{}, errNoHalf
		}
		return b.Tracker.OwnAsks(ctx, a.Query, a.Now)
	})

var opEntryMarks = define("tracker.entry_marks", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, level statelog.ReadLevel) (worklet.EntryMarks, error) {
		if b.Tracker == nil {
			return worklet.EntryMarks{}, errNoHalf
		}
		return b.Tracker.EntryMarks(ctx, level)
	})

// entryWorkArgs is [opEntryWork]'s input.
type entryWorkArgs struct {
	Query worklet.EntryWorkQuery
	Now   time.Time
}

var opEntryWork = define("tracker.entry_work", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a entryWorkArgs) (worklet.EntryWork, error) {
		if b.Tracker == nil {
			return worklet.EntryWork{}, errNoHalf
		}
		return b.Tracker.EntryWork(ctx, a.Query, a.Now)
	})

var opProjects = define("tracker.projects", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q worklet.ProjectQuery) (worklet.ProjectListing, error) {
		if b.Tracker == nil {
			return worklet.ProjectListing{}, errNoHalf
		}
		q.Units = b.Units
		return b.Tracker.Projects(ctx, q)
	})

// ONE PROJECT, as a screen shows it: read leniently.
var opProject = define("tracker.project", opRead, trackerDomain, false, project)

// ONE PROJECT A CALLER SENDS BACK, ANSWERED WHOLE ([op.answeredWhole]):
// write_project replaces the field declarations whole, sent as describe_project
// read them (internal/agent/builtin), so a newer build's field this build would
// drop is refused rather than written away. Its own operation, so a screen
// that only shows the project is not refused one ([opArtifactVersionCarried]
// says why).
var opProjectCarried = define("tracker.project_carried", opRead, trackerDomain, false,
	project).answeredWhole()

// project is one project, read from this node's copy.
func project(ctx context.Context, b Backend, _ *Actor, q worklet.ProjectDetailQuery) (worklet.ProjectDetail, error) {
	if b.Tracker == nil {
		return worklet.ProjectDetail{}, errNoHalf
	}
	q.Units = b.Units
	return b.Tracker.Project(ctx, q)
}

type workSearchArgs struct {
	Text  string
	Limit int

	// Mode is how to rank. Empty is hybrid — what a caller that names no
	// mode (a seat's search_work tool) sends.
	Mode knowledge.Mode
}

// THE RANKED SEARCH READS THE INDEX OVER THE TRACKER'S ROWS, so it runs only on
// a copy whose tracker is in service, and CARRIES NO FLOOR: each node's own
// walk maintains the index behind its applier, so a floor would promise a
// visibility the index does not give.
var opWorkSearch = define("tracker.search", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a workSearchArgs) (worklet.SearchAnswer, error) {
		if b.WorkSearch == nil {
			return worklet.SearchAnswer{}, errNoHalf
		}
		return b.WorkSearch.Search(ctx, worklet.SearchQuery{Text: a.Text, Limit: a.Limit, Mode: a.Mode})
	}).floorless()

type workloadArgs struct {
	Query worklet.WorkloadQuery
	Now   time.Time

	// Zone is the company clock the asker cut its days on ([zoneArg]).
	Zone string
}

var opWorkload = define("tracker.workload", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a workloadArgs) (worklet.WorkloadAnswer, error) {
		if b.Tracker == nil {
			return worklet.WorkloadAnswer{}, errNoHalf
		}
		loc, err := zoneOf(a.Zone)
		if err != nil {
			return worklet.WorkloadAnswer{}, err
		}
		a.Query.Units = b.Units
		return b.Tracker.Workload(ctx, a.Query, a.Now, loc)
	})

type inboxArgs struct {
	Query worklet.InboxQuery
	Now   time.Time
}

// opInbox is named `read_inbox`, beside `write_inbox`, never `tracker.inbox`:
// an operation's name is not a subject, but that one is spelled like a seat's
// inbox subject, and the guard that keeps every subject in internal/queue/topics
// rightly reads any literal ending `.inbox` as one written by hand.
var opInbox = define("tracker.read_inbox", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a inboxArgs) (worklet.InboxAnswer, error) {
		if b.Tracker == nil {
			return worklet.InboxAnswer{}, errNoHalf
		}
		return b.Tracker.Inbox(ctx, a.Query, a.Now)
	})

type routingArgs struct {
	Query worklet.RoutingQuery
	Now   time.Time
}

var opRouting = define("tracker.routing", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a routingArgs) (worklet.RoutingAnswer, error) {
		if b.Tracker == nil {
			return worklet.RoutingAnswer{}, errNoHalf
		}
		return b.Tracker.Routing(ctx, a.Query, a.Now)
	})

type turnsOfArgs struct {
	IDOrKey string
	Cursor  string
	Limit   int
	Fresh   statelog.Freshness
}

// A TASK'S TURNS are the tracker's: the turn records are charged to the task
// (ADR-0022) and live beside its rows.
var opTurnsOf = define("tracker.turns_of", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a turnsOfArgs) (worklet.TaskTurns, error) {
		if b.Tracker == nil {
			return worklet.TaskTurns{}, errNoHalf
		}
		return b.Tracker.TurnsOf(ctx, a.IDOrKey, a.Cursor, a.Limit, a.Fresh)
	})

// everyViewArgs is [viewsArgs] for every saved view.
type everyViewArgs struct {
	Query worklet.EveryViewQuery
	Zone  string
}

var opEveryView = define("tracker.every_view", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a everyViewArgs) (worklet.ViewListing, error) {
		if b.Tracker == nil {
			return worklet.ViewListing{}, errNoHalf
		}
		loc, err := zoneOf(a.Zone)
		if err != nil {
			return worklet.ViewListing{}, err
		}
		q := a.Query
		q.Units, q.Zone = b.Units, loc
		return b.Tracker.EveryView(ctx, q)
	})

type flowArgs struct {
	Query worklet.FlowQuery
	Now   time.Time

	// Zone is the company clock the asker cut its days on ([zoneArg]).
	Zone string
}

var opFlow = define("tracker.flow", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a flowArgs) (worklet.FlowAnswer, error) {
		if b.Tracker == nil {
			return worklet.FlowAnswer{}, errNoHalf
		}
		loc, err := zoneOf(a.Zone)
		if err != nil {
			return worklet.FlowAnswer{}, err
		}
		return b.Tracker.Flow(ctx, a.Query, a.Now, loc)
	})

var opCompanyFeed = define("tracker.company_feed", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q worklet.FeedQuery) (worklet.FeedPage, error) {
		if b.Tracker == nil {
			return worklet.FeedPage{}, errNoHalf
		}
		return b.Tracker.CompanyFeed(ctx, q)
	})

type decisionsArgs struct {
	Query worklet.DecisionsQuery
	Now   time.Time

	// Zone is the company clock the asker cut "overdue" on ([zoneArg]).
	Zone string
}

var opDecisions = define("tracker.decisions", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a decisionsArgs) (worklet.DecisionsAnswer, error) {
		if b.Tracker == nil {
			return worklet.DecisionsAnswer{}, errNoHalf
		}
		loc, err := zoneOf(a.Zone)
		if err != nil {
			return worklet.DecisionsAnswer{}, err
		}
		return b.Tracker.Decisions(ctx, a.Query, a.Now, loc)
	})

// opAnsweredAsks takes [decisionsArgs]: the same person, freshness and clock,
// asked for the asks closed rather than the ones still open.
var opAnsweredAsks = define("tracker.answered_asks", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a decisionsArgs) (worklet.AnsweredAnswer, error) {
		if b.Tracker == nil {
			return worklet.AnsweredAnswer{}, errNoHalf
		}
		loc, err := zoneOf(a.Zone)
		if err != nil {
			return worklet.AnsweredAnswer{}, err
		}
		return b.Tracker.AnsweredAsks(ctx, a.Query, a.Now, loc)
	})

type turnPlacesArgs struct {
	Runs  []string
	Fresh statelog.Freshness
}

var opTurnPlaces = define("tracker.turn_places", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a turnPlacesArgs) (map[string]worklet.TurnPlace, error) {
		if b.Tracker == nil {
			return nil, errNoHalf
		}
		return b.Tracker.TurnPlaces(ctx, a.Runs, a.Fresh)
	})

// ---- the tracker's writes ------------------------------------------------ //

// A PROJECT'S FILES, as rows. The BYTES never cross here: every node streams
// them to and from the fleet's one object store through its own client, and
// asks this service only for the row that names them.
var opFiles = define("tracker.files", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q worklet.FileQuery) (worklet.FileListing, error) {
		if b.Tracker == nil {
			return worklet.FileListing{}, errNoHalf
		}
		return b.Tracker.Files(ctx, q)
	})

type fileArgs struct {
	Project string
	Path    string
	Fresh   statelog.Freshness
}

var opFile = define("tracker.read_file", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a fileArgs) (worklet.FileDetail, error) {
		if b.Tracker == nil {
			return worklet.FileDetail{}, errNoHalf
		}
		return b.Tracker.File(ctx, a.Project, a.Path, a.Fresh)
	})

type putFileArgs struct {
	OpID string
	Put  worklet.FilePut
}

var opPutFile = define("tracker.write_file", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a putFileArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.PutFile(ctx, a.OpID, a.Put)
	})

type removeFileArgs struct {
	OpID    string
	Project string
	Path    string
	IfMatch uint64
}

var opRemoveFile = define("tracker.remove_file", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a removeFileArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.RemoveFile(ctx, a.OpID, a.Project, a.Path, a.IfMatch)
	})

type recordTurnArgs struct {
	OpID string
	Turn worklet.TurnRecord
}

// A TURN'S SPEND, recorded by the node that ran the turn, which may hold no
// applier. Its operation id makes asking the next node again safe: the
// applier adds a turn's spend only for a new operation.
var opRecordTurn = define("tracker.record_turn", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a recordTurnArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.RecordTurn(ctx, a.OpID, a.Turn)
	})

// writerFor is the tracker writer acting as the request's actor.
func writerFor(b Backend, actor *Actor) (TrackerWriter, error) {
	if b.Writer == nil {
		return nil, errNoHalf
	}
	w := b.Writer(*actor)
	if w == nil {
		return nil, fmt.Errorf("estate: the tracker refused to act as %q (%s)",
			actor.Handle, actor.Kind)
	}
	return w, nil
}

type createTaskArgs struct {
	OpID   string
	Task   worklet.Task
	Notify *worklet.Notify
}

// A TASK HOLDING THE ZERO INSTANT is refused before it is asked
// ([op.refusedBeforeAsking]): a pointer at it would cross as no instant at all,
// and the writer refuses it where it runs ([worklet.Task.CheckInstants]).
var opCreateTask = define("tracker.create_task", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a createTaskArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.CreateTask(ctx, a.OpID, a.Task, a.Notify)
	}).refusedBeforeAsking(func(a createTaskArgs) error { return a.Task.CheckInstants() })

type createTaskAskingArgs struct {
	OpID   string
	Task   worklet.Task
	Ask    worklet.Comment
	Notify *worklet.Notify
}

// The task and its ask are refused before they are asked for the zero instant
// as a create's task is ([opCreateTask]).
var opCreateTaskAsking = define("tracker.create_task_asking", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a createTaskAskingArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.CreateTaskAsking(ctx, a.OpID, a.Task, a.Ask, a.Notify)
	}).refusedBeforeAsking(func(a createTaskAskingArgs) error {
	if err := a.Task.CheckInstants(); err != nil {
		return err
	}
	return a.Ask.CheckInstants()
})

type placeTaskArgs struct {
	OpID   string
	Place  worklet.Place
	Notify *worklet.Notify
}

// placedTask is [TrackerWriter.PlaceTask]'s answer as it crosses: the result,
// with the reason the card changed lanes and did not take its place carried
// BESIDE it as a wire error — an error is an interface no encoder writes, and
// the tool that reads it classifies the refusal by its identity (a stale
// version, a conflict), which [encodeError] keeps.
type placedTask struct {
	Result   worklet.PlaceResult
	Unplaced *wireError
}

// A DROP ON THE BOARD WALKS one task (its lane, then its place among its
// neighbours), each step under an operation id derived from the caller's, so
// the whole is asked again of the next holder and a step its ledger answers is
// not written twice. A step answered unvouched is not final ([unvouched]).
var opPlaceTask = define("tracker.place_task", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a placeTaskArgs) (placedTask, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return placedTask{}, err
		}
		res, err := w.PlaceTask(ctx, a.OpID, a.Place, a.Notify)
		out := placedTask{Unplaced: encodeError(res.Unplaced)}
		res.Unplaced = nil
		out.Result = res
		return out, err
	})

// updateTaskArgs carries a patch's GESTURES beside it: they are resolved
// inside the decide and never encoded into a record, which is why the patch
// itself does not serialise them — and why they travel here, named.
type updateTaskArgs struct {
	OpID      string
	ID        string
	Project   string
	IfMatch   uint64
	Patch     worklet.TaskPatch
	Watch     *worklet.WatchIntent
	Relate    *worklet.RelationIntent
	Depend    *worklet.DependentIntent
	Checklist *worklet.ChecklistIntent
	Kind      worklet.ChangeKind
	Notify    *worklet.Notify
}

// A PATCH'S DATES are refused before they are asked where no task could hold
// them ([op.refusedBeforeAsking], [worklet.TaskPatch.CheckDates]): a date set to
// the zero instant would cross as no date named at all — a clear is its own
// flag, ClearStart or ClearDue, which crosses as itself.
var opUpdateTask = define("tracker.update_task", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a updateTaskArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		patch := a.Patch
		patch.Watch, patch.Relate, patch.Depend, patch.Checklist =
			a.Watch, a.Relate, a.Depend, a.Checklist
		return w.UpdateTask(ctx, a.OpID, a.ID, a.Project, a.IfMatch, patch, a.Kind, a.Notify)
	}).refusedBeforeAsking(func(a updateTaskArgs) error { return a.Patch.CheckDates() })

type dependArgs struct {
	OpID   string
	Change worklet.DependencyChange
}

var opDepend = define("tracker.depend", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a dependArgs) (worklet.DependencyResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.DependencyResult{}, err
		}
		return w.Depend(ctx, a.OpID, a.Change, b.Leads)
	})

type mergeArgs struct {
	OpID      string
	Duplicate string
	Into      string
	Reparent  bool
	Notify    *worklet.Notify
}

var opMergeDuplicates = define("tracker.merge_duplicates", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a mergeArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.MergeDuplicates(ctx, a.OpID, a.Duplicate, a.Into, a.Reparent, a.Notify)
	})

type moveArgs struct {
	OpID   string
	TaskID string
	Target string
	Notify *worklet.Notify
}

var opMoveTaskToProject = define("tracker.move_task_to_project", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a moveArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.MoveTaskToProject(ctx, a.OpID, a.TaskID, a.Target, a.Notify)
	})

type writeProjectArgs struct {
	OpID      string
	Key       string
	Edit      worklet.ProjectEdit
	Authority worklet.ProjectAuthority
}

var opWriteProject = define("tracker.write_project", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a writeProjectArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.WriteProject(ctx, a.OpID, a.Key, a.Edit, a.Authority)
	})

type writeTagsArgs struct {
	OpID      string
	Project   string
	Edit      worklet.TagEdit
	Authority worklet.TagAuthority
}

var opWriteTags = define("tracker.write_tags", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a writeTagsArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.WriteTags(ctx, a.OpID, a.Project, a.Edit, a.Authority)
	})

type ensureTagsArgs struct {
	OpID    string
	Project string
	Tags    []string
}

// ensuredTags is [TrackerWriter.EnsureTags]' two answers, named. Its caveats
// are rebuilt from the peer's answer at the router, as a write's are
// ([worklet.WriteResult.Caveats]).
type ensuredTags struct {
	Created []string
	Caveats []caveat.Caveat
}

var opEnsureTags = define("tracker.ensure_tags", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a ensureTagsArgs) (ensuredTags, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return ensuredTags{}, err
		}
		created, caveats, err := w.EnsureTags(ctx, a.OpID, a.Project, a.Tags)
		return ensuredTags{Created: created, Caveats: caveats}, err
	})

// ---- the operator's writes ------------------------------------------------ //

// The writes only the operator's surfaces make — no seat is handed a tool that
// reaches them — routed for the reason every other write is: the node the
// operator asked may hold no data, or a copy that is out of service.

type viewPriorArgs struct {
	ID string
}

// WHAT A SAVE WOULD REPLACE, read for the caller to decide its authority on and
// hand back to the save. A READ, floored like every tracker read; the save
// refuses a prior that has moved in its own snapshot, so a stale answer here
// is a refusal there, never a wrong overwrite. ANSWERED WHOLE
// ([op.answeredWhole]) because the save hands it back: a newer build's field
// dropped here would reach that build's decide as its zero.
var opViewPrior = define("tracker.view_prior", opRead, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a viewPriorArgs) (worklet.ViewPrior, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.ViewPrior{}, err
		}
		return w.ViewPrior(ctx, a.ID)
	}).answeredWhole()

type writeViewArgs struct {
	OpID  string
	View  worklet.View
	Prior worklet.ViewPrior
}

var opWriteView = define("tracker.write_view", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a writeViewArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.WriteView(ctx, a.OpID, a.View, a.Prior)
	})

type deleteViewArgs struct {
	OpID  string
	ID    string
	Prior worklet.ViewPrior
}

// A VIEW TAKEN AWAY, decided on the prior its caller read as a save is, and
// refused where the stored view moved, changed hands or went away since.
var opDeleteView = define("tracker.delete_view", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a deleteViewArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.DeleteView(ctx, a.OpID, a.ID, a.Prior)
	})

type writeTypesArgs struct {
	OpID  string
	Types []worklet.TaskType
}

var opWriteTypes = define("tracker.write_types", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a writeTypesArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.WriteTypes(ctx, a.OpID, a.Types)
	})

type writeFieldsArgs struct {
	OpID   string
	Fields []worklet.FieldDef
}

var opWriteFields = define("tracker.write_fields", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a writeFieldsArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.WriteFields(ctx, a.OpID, a.Fields)
	})

type markInboxArgs struct {
	OpID   string
	Handle string
	// Seen is how far the asker read the Worklet's log, which the decide
	// waits for ([worklet.Writer.MarkInbox]). ADDITIVE ON THE WIRE: an older
	// peer decodes past it, and an older sender's zero position waits for
	// nothing.
	Seen      statelog.Position
	Gesture   inbox.Gesture[worklet.Reason]
	Authority worklet.PersonAuthority
}

// A GESTURE, never the lists: the tracker resolves it against the record its
// own decide reads, on whichever holder answers, so a caller never sends back
// a list read in another transaction. The CALLER'S FLOOR rides with it, not in
// this node's session: it is one person's page's claim, held to this request.
var opMarkInbox = define("tracker.mark_inbox", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a markInboxArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.MarkInbox(ctx, a.OpID, a.Handle, a.Seen, a.Gesture, a.Authority)
	})

type writePinsArgs struct {
	OpID      string
	Handle    string
	Gesture   worklet.PinGesture
	Authority worklet.PersonAuthority
}

var opWritePins = define("tracker.write_pins", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a writePinsArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.WritePins(ctx, a.OpID, a.Handle, a.Gesture, a.Authority)
	})

type writePrioritiesArgs struct {
	OpID       string
	Handle     string
	Priorities []string
	IfMatch    *uint64
	Authority  worklet.PersonAuthority
}

var opWritePriorities = define("tracker.write_priorities", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a writePrioritiesArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.WritePriorities(ctx, a.OpID, a.Handle, a.Priorities, a.IfMatch, a.Authority)
	})

type editCommentOnTaskArgs struct {
	OpID      string
	TaskID    string
	Project   string
	CommentID string
	Body      string
	Notify    *worklet.Notify
}

// REWRITING ONE'S OWN REMARK on a work item. Its author is decided in the
// decide's own snapshot on the serving node ([worklet.ErrNotAuthor]), against
// the actor this request names — which is why the actor is the SIGNED one
// (see [Envelopes]): an edit whose author a request could choose would put
// words in somebody else's mouth.
var opEditTaskComment = define("tracker.edit_comment", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a editCommentOnTaskArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.EditComment(ctx, a.OpID, a.TaskID, a.Project, a.CommentID, a.Body, a.Notify)
	})

type resolveCommentOnTaskArgs struct {
	OpID      string
	TaskID    string
	Project   string
	CommentID string
	// Withdrawal is why its asker withdraws the question; nil resolves a
	// remark, which is what an older build does.
	Withdrawal *worklet.Withdrawal
	Notify     *worklet.Notify
}

// RESOLVING ONE'S OWN REMARK, a question its asker withdraws: the author is
// decided against the SIGNED actor ([worklet.ErrNotAuthor]), as an edit's is.
var opResolveTaskComment = define("tracker.resolve_comment", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a resolveCommentOnTaskArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.ResolveComment(ctx, a.OpID, a.TaskID, a.Project, a.CommentID, a.Withdrawal, a.Notify)
	})

type removeCommentOnTaskArgs struct {
	OpID      string
	TaskID    string
	Project   string
	CommentID string
	Authority worklet.CommentAuthority
}

// TAKING A REMARK DOWN on a work item: its author, or a moderator the caller's
// table admitted, decided against the SIGNED actor ([worklet.ErrNotAuthor]),
// as an edit's author is.
var opRemoveTaskComment = define("tracker.remove_comment", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a removeCommentOnTaskArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.RemoveComment(ctx, a.OpID, a.TaskID, a.Project, a.CommentID, a.Authority)
	})

type removeTaskArgs struct {
	OpID    string
	ID      string
	Project string
	Subtree bool
	Notify  *worklet.Notify
}

var opRemoveTask = define("tracker.remove_task", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a removeTaskArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.RemoveTask(ctx, a.OpID, a.ID, a.Project, a.Subtree, a.Notify)
	})

type restoreTaskArgs struct {
	OpID    string
	ID      string
	Project string
	Alone   bool
	Notify  *worklet.Notify
}

var opRestoreTask = define("tracker.restore_task", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a restoreTaskArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.RestoreTask(ctx, a.OpID, a.ID, a.Project, a.Alone, a.Notify)
	})

type purgeTaskArgs struct {
	OpID    string
	ID      string
	Project string
	Reason  string
}

// A PURGE's operation id is what a repeat after an unanswered one collapses
// on, its ledger row answering with the first outcome; its refusals keep their
// identity, so an operator re-running one is told it is purged, not behind.
var opPurgeTask = define("tracker.purge_task", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a purgeTaskArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.PurgeTask(ctx, a.OpID, a.ID, a.Project, a.Reason)
	})

// ---- the knowledge base -------------------------------------------------- //

type listPagesArgs struct {
	Filter doclet.Filter
	Fresh  statelog.Freshness
}

var opListPages = define("pages.list", opRead, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a listPagesArgs) (doclet.Listing, error) {
		if b.Pages == nil {
			return doclet.Listing{}, errNoHalf
		}
		return b.Pages.List(ctx, a.Filter, a.Fresh)
	})

type getPageArgs struct {
	Ref   string
	Fresh statelog.Freshness
}

var opGetPage = define("pages.get", opRead, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a getPageArgs) (doclet.Detail, error) {
		if b.Pages == nil {
			return doclet.Detail{}, errNoHalf
		}
		return b.Pages.Get(ctx, a.Ref, a.Fresh)
	})

type skillPagesArgs struct {
	Container string
	Fresh     statelog.Freshness
}

var opSkillPages = define("pages.skill_pages", opRead, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a skillPagesArgs) ([]doclet.Page, error) {
		if b.Pages == nil {
			return nil, errNoHalf
		}
		return b.Pages.SkillPages(ctx, a.Container, a.Fresh)
	})

type containersArgs struct {
	Fresh statelog.Freshness
}

var opContainers = define("pages.containers", opRead, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a containersArgs) ([]doclet.ContainerListing, error) {
		if b.Pages == nil {
			return nil, errNoHalf
		}
		return b.Pages.Containers(ctx, a.Fresh)
	})

// A PAGE'S ACTIVITY, a container's, or the whole knowledge base's.
var opPageActivity = define("pages.activity", opRead, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q doclet.PageActivityQuery) (doclet.PageActivity, error) {
		if b.Pages == nil {
			return doclet.PageActivity{}, errNoHalf
		}
		return b.Pages.Activity(ctx, q)
	})

var opPageTurnsMoved = define("pages.turns_moved", opRead, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q doclet.TurnsMovedQuery) (map[string]bool, error) {
		if b.Pages == nil {
			return nil, errNoHalf
		}
		return b.Pages.TurnsMovedPages(ctx, q)
	})

type revisionArgs struct {
	PageID  string
	Version int
	Fresh   statelog.Freshness
}

// aRevision is [PageReader.Revision]'s two answers, named.
type aRevision struct {
	Revision doclet.Revision
	Found    bool
}

var opRevision = define("pages.revision", opRead, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a revisionArgs) (aRevision, error) {
		if b.Pages == nil {
			return aRevision{}, errNoHalf
		}
		rev, found, err := b.Pages.Revision(ctx, a.PageID, a.Version, a.Fresh)
		return aRevision{Revision: rev, Found: found}, err
	})

// keyed reports a page write the caller made REPEATABLE: one whose actor
// carries the caller's own operation key ([doclet.Actor.OpKey]), from which the
// pages store derives the operation id — bound to what the write says — and a
// create its page's id, so a repeat under it is the same operation.
func keyed(actor doclet.Actor) bool { return strings.TrimSpace(actor.OpKey) != "" }

// chatKeyed is [keyed] for a chat write ([chatlet.Actor.OpKey]): the chat store
// derives the operation id, and a message's or a new room's, from the key and
// what the gesture asks.
func chatKeyed(actor chatlet.Actor) bool { return strings.TrimSpace(actor.OpKey) != "" }

// pageActorWrites is the check every page write is held to before it is asked
// anywhere ([op.refusedBeforeAsking]): that the actor its arguments hold may
// write ([actorWrites]).
func pageActorWrites(actor doclet.Actor) error {
	return actorWrites(actor.Handle, iam.ActorKind(actor.Kind), actor.WrittenFor)
}

// chatActorWrites is [pageActorWrites] for a chat write, every gesture whose
// arguments hold the actor it writes as. A reach writes nothing, and is held
// to nothing here.
func chatActorWrites(actor chatlet.Actor) error {
	return actorWrites(actor.Handle, iam.ActorKind(actor.Kind), actor.WrittenFor)
}

type createPageArgs struct {
	Actor doclet.Actor
	Page  doclet.NewPage
}

var opCreatePage = define("pages.create", opOnceWrite, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a createPageArgs) (doclet.Written, error) {
		if b.PageWriter == nil {
			return doclet.Written{}, errNoHalf
		}
		return b.PageWriter.Create(ctx, a.Actor, a.Page)
	}).repeatableWhen(func(a createPageArgs) bool { return keyed(a.Actor) }).
	refusedBeforeAsking(func(a createPageArgs) error { return pageActorWrites(a.Actor) })

type savePageArgs struct {
	Actor  doclet.Actor
	PageID string
	Save   doclet.Save
}

var opSavePage = define("pages.save", opOnceWrite, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a savePageArgs) (doclet.Written, error) {
		if b.PageWriter == nil {
			return doclet.Written{}, errNoHalf
		}
		return b.PageWriter.SavePage(ctx, a.Actor, a.PageID, a.Save)
	}).repeatableWhen(func(a savePageArgs) bool { return keyed(a.Actor) }).
	refusedBeforeAsking(func(a savePageArgs) error { return pageActorWrites(a.Actor) })

type renamePageArgs struct {
	Actor  doclet.Actor
	PageID string
	Title  string
	Quiet  bool
}

var opRenamePage = define("pages.rename", opOnceWrite, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a renamePageArgs) (doclet.Written, error) {
		if b.PageWriter == nil {
			return doclet.Written{}, errNoHalf
		}
		return b.PageWriter.Rename(ctx, a.Actor, a.PageID, a.Title, a.Quiet)
	}).repeatableWhen(func(a renamePageArgs) bool { return keyed(a.Actor) }).
	refusedBeforeAsking(func(a renamePageArgs) error { return pageActorWrites(a.Actor) })

type commentArgs struct {
	Actor   doclet.Actor
	PageID  string
	Comment doclet.NewComment
}

// commented is a comment write's two answers, named.
type commented struct {
	Comment doclet.Comment
	Written doclet.Written
}

// A COMMENT IS REPEATABLE UNDER ITS CALLER'S KEY ([doclet.Actor.OpKey]), as
// every page write is: a seat's turn hands its call's own, a person's request
// its Idempotency-Key, and the store derives the comment's id from it, so a
// re-run or a retry posts once.
var opCommentPage = define("pages.comment", opOnceWrite, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a commentArgs) (commented, error) {
		if b.PageWriter == nil {
			return commented{}, errNoHalf
		}
		c, w, err := b.PageWriter.Comment(ctx, a.Actor, a.PageID, a.Comment)
		return commented{Comment: c, Written: w}, err
	}).repeatableWhen(func(a commentArgs) bool {
	return keyed(a.Actor)
}).refusedBeforeAsking(func(a commentArgs) error { return pageActorWrites(a.Actor) })

type editCommentArgs struct {
	Actor     doclet.Actor
	PageID    string
	CommentID string
	Body      string
}

var opEditComment = define("pages.edit_comment", opOnceWrite, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a editCommentArgs) (commented, error) {
		if b.PageWriter == nil {
			return commented{}, errNoHalf
		}
		c, w, err := b.PageWriter.EditComment(ctx, a.Actor, a.PageID, a.CommentID, a.Body)
		return commented{Comment: c, Written: w}, err
	}).repeatableWhen(func(a editCommentArgs) bool { return keyed(a.Actor) }).
	refusedBeforeAsking(func(a editCommentArgs) error { return pageActorWrites(a.Actor) })

// ---- the operator's page writes ------------------------------------------ //

// The knowledge base's writes no seat is handed, routed as the operator's
// tracker writes are. Each is a once-write made repeatable by the caller's key:
// unkeyed, a second trash is a second record and a second purge a refusal of
// its own first copy.

type pageRefArgs struct {
	Actor  doclet.Actor
	PageID string
}

var opTrashPage = define("pages.trash", opOnceWrite, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a pageRefArgs) (doclet.Written, error) {
		if b.PageWriter == nil {
			return doclet.Written{}, errNoHalf
		}
		return b.PageWriter.Trash(ctx, a.Actor, a.PageID)
	}).repeatableWhen(func(a pageRefArgs) bool { return keyed(a.Actor) }).
	refusedBeforeAsking(func(a pageRefArgs) error { return pageActorWrites(a.Actor) })

var opRestorePage = define("pages.restore", opOnceWrite, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a pageRefArgs) (doclet.Written, error) {
		if b.PageWriter == nil {
			return doclet.Written{}, errNoHalf
		}
		return b.PageWriter.Restore(ctx, a.Actor, a.PageID)
	}).repeatableWhen(func(a pageRefArgs) bool { return keyed(a.Actor) }).
	refusedBeforeAsking(func(a pageRefArgs) error { return pageActorWrites(a.Actor) })

type purgePageArgs struct {
	Actor  doclet.Actor
	PageID string
	Reason string
}

var opPurgePage = define("pages.purge", opOnceWrite, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a purgePageArgs) (doclet.Written, error) {
		if b.PageWriter == nil {
			return doclet.Written{}, errNoHalf
		}
		return b.PageWriter.Purge(ctx, a.Actor, a.PageID, a.Reason)
	}).repeatableWhen(func(a purgePageArgs) bool { return keyed(a.Actor) }).
	refusedBeforeAsking(func(a purgePageArgs) error { return pageActorWrites(a.Actor) })

type removeCommentArgs struct {
	Actor     doclet.Actor
	PageID    string
	CommentID string

	// Authority is the caller's answer from the authority table — whether
	// it may take down a remark it did not write — decided on the asking
	// node, which holds the grants, and judged against the remark's author
	// in the serving node's own snapshot.
	Authority doclet.CommentAuthority
}

var opRemoveComment = define("pages.remove_comment", opOnceWrite, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a removeCommentArgs) (doclet.Written, error) {
		if b.PageWriter == nil {
			return doclet.Written{}, errNoHalf
		}
		return b.PageWriter.RemoveComment(ctx, a.Actor, a.PageID, a.CommentID, a.Authority)
	}).repeatableWhen(func(a removeCommentArgs) bool { return keyed(a.Actor) }).
	refusedBeforeAsking(func(a removeCommentArgs) error { return pageActorWrites(a.Actor) })

// knowledgeArgs is a search as it crosses. The seat is its HANDLE and the
// org is the serving node's own, for the reason the package doc gives:
// neither is a value, both are the current chart.
type knowledgeArgs struct {
	Text  string
	Seat  string
	Limit int

	// Mode is how to rank. Empty is hybrid — what a caller that names no
	// mode (the prefetch, a seat's search_knowledge tool) sends.
	Mode knowledge.Mode

	// Scoped says the caller searched with an org, which is what supplies
	// the read scope; false searches as nobody.
	Scoped bool

	// ExcludeAncestors keeps nil and empty APART, which the query tells
	// apart on purpose: nil takes the default exclusion, and an empty list
	// is a caller deliberately searching drafts. Omitted-if-empty would
	// collapse the second into the first.
	ExcludeAncestors []string
	Exclusion        bool

	// Prefetch says the search is a turn's knowledge prefetch: the serving
	// node coordinates the scan, so it is the node that times it, and the
	// budget it times it against is the turn start's ([knowledge.Query]).
	Prefetch bool
}

// query is the search a request names, against the serving node's own chart:
// the seat's role and the org whose read scope applies.
func (a knowledgeArgs) query(b Backend) knowledge.Query {
	q := knowledge.Query{Text: a.Text, Limit: a.Limit, Mode: a.Mode, Prefetch: a.Prefetch}
	if a.Exclusion {
		q.ExcludeAncestors = a.ExcludeAncestors
		if q.ExcludeAncestors == nil {
			q.ExcludeAncestors = []string{}
		}
	}
	if b.Seat != nil {
		seat, o := b.Seat(a.Seat)
		if a.Seat != "" {
			q.Seat = seat
		}
		if a.Scoped {
			q.Org = o
		}
	}
	return q
}

// THE KNOWLEDGE SEARCH READS THE INDEX OVER THE KNOWLEDGE BASE'S ROWS, so it
// runs only on a copy whose knowledge base is in service — and it carries no
// floor, for [opWorkSearch]'s reason: the index is one no log position
// describes.
var opKnowledgeSearch = define("knowledge.search", opRead, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a knowledgeArgs) (knowledge.Result, error) {
		if b.Knowledge == nil {
			return knowledge.Result{}, errNoHalf
		}
		return b.Knowledge.Answer(ctx, a.query(b))
	}).floorless()

// WHETHER THE INDEX IS STILL BUILDING, which is what turns an empty search into
// "still indexing" — asked, like the search, of the index over the knowledge
// base's rows rather than of the log.
var opKnowledgeBuilding = define("knowledge.building", opRead, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, _ struct{}) (bool, error) {
		if b.Knowledge == nil {
			return false, errNoHalf
		}
		return b.Knowledge.Building(ctx), nil
	}).floorless()

// ---- the usage domain's reads ------------------------------------------- //

// THE USAGE DOMAIN'S READS cross like every other domain's, so a node whose
// usage copy is out of service is answered from a peer's (ADR-0079).
// FLOORLESS: no write this node makes lands on the usage log (every node
// publishes its own days from its own event log), and its rows are compacted,
// each replaced whole.
//
// FIVE ARE PAGED when a PEER answers ([UsageReader], [allPages]); this node's
// own answer is the whole read ([Backend.pageBytes]).

// usagePageBytes is the most a page of a routed usage read carries before it
// is cut at the next record's edge: A QUARTER OF THE MESSAGE
// ([queue.MaxPayloadBytes]). A page may run one group past it (a record's rows
// are never split, usage.Group); a record is one message, under 1 MiB at its
// caps (usage.TestAFullSeatDayRecordFitsOneMessage), and as read rows a few
// times that, which the other three quarters hold
// (TestAUsagePageFitsOneMessage). A reply past the message is refused, never
// dropped ([server.encodeReply]).
const usagePageBytes = queue.MaxPayloadBytes / 4

// usagePageArgs is one page of a paged usage read: the read's query, and the
// group the page before it ended on — empty for the first.
type usagePageArgs[Q any] struct {
	Query Q
	After usage.Group
}

var opUsageSpend = define("usage.spend", opRead, usageDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a usagePageArgs[usage.SpendQuery]) (usage.Page[usage.SpendRow], error) {
		if b.Usage == nil {
			return usage.Page[usage.SpendRow]{}, errNoHalf
		}
		return b.Usage.SpendPage(ctx, a.Query, a.After, b.pageBytes)
	}).floorless()

var opUsageSeatTurns = define("usage.seat_turns", opRead, usageDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a usagePageArgs[usage.SpendQuery]) (usage.Page[usage.TurnsRow], error) {
		if b.Usage == nil {
			return usage.Page[usage.TurnsRow]{}, errNoHalf
		}
		return b.Usage.SeatTurnsPage(ctx, a.Query, a.After, b.pageBytes)
	}).floorless()

var opUsagePersonSpend = define("usage.person_spend", opRead, usageDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a usagePageArgs[usage.PersonQuery]) (usage.Page[usage.PersonRow], error) {
		if b.Usage == nil {
			return usage.Page[usage.PersonRow]{}, errNoHalf
		}
		return b.Usage.PersonSpendPage(ctx, a.Query, a.After, b.pageBytes)
	}).floorless()

var opUsageSeatDays = define("usage.seat_days", opRead, usageDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a usagePageArgs[usage.SpendQuery]) (usage.Page[usage.SeatDay], error) {
		if b.Usage == nil {
			return usage.Page[usage.SeatDay]{}, errNoHalf
		}
		return b.Usage.SeatDaysPage(ctx, a.Query, a.After, b.pageBytes)
	}).floorless()

// scheduleRunsPage is [UsageReader.ScheduleRuns]'s two answers, named.
type scheduleRunsPage struct {
	Runs []usage.ScheduleRun
	More bool
}

var opUsageScheduleRuns = define("usage.schedule_runs", opRead, usageDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q usage.ScheduleRunsQuery) (scheduleRunsPage, error) {
		if b.Usage == nil {
			return scheduleRunsPage{}, errNoHalf
		}
		runs, more, err := b.Usage.ScheduleRuns(ctx, q)
		return scheduleRunsPage{Runs: runs, More: more}, err
	}).floorless()

var opUsagePageReads = define("usage.page_reads", opRead, usageDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a usagePageArgs[usage.PageReadsQuery]) (usage.Page[usage.PageRead], error) {
		if b.Usage == nil {
			return usage.Page[usage.PageRead]{}, errNoHalf
		}
		return b.Usage.PageReadsPage(ctx, a.Query, a.After, b.pageBytes)
	}).floorless()

// readsElidedArgs is a range of company days, both inclusive.
type readsElidedArgs struct {
	From, To string
}

var opUsageReadsElided = define("usage.reads_elided", opRead, usageDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a readsElidedArgs) (int64, error) {
		if b.Usage == nil {
			return 0, errNoHalf
		}
		return b.Usage.ReadsElided(ctx, a.From, a.To)
	}).floorless()

// A SERVER'S TOOL CALLS ARE ONE ANSWER, NOT PAGED: one row per (tool, seat)
// summed across nodes and days, so a longer window adds no row. At a couple of
// hundred bytes a row, one message ([queue.MaxPayloadBytes]) holds some forty
// thousand: a hundred tools called by four hundred seats. A reply past it is
// refused, never dropped ([server.encodeReply]).
var opUsageServerToolCalls = define("usage.server_tool_calls", opRead, usageDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q usage.ServerToolCallsQuery) ([]usage.ServerToolCallsRow, error) {
		if b.Usage == nil {
			return nil, errNoHalf
		}
		return b.Usage.ServerToolCalls(ctx, q)
	}).floorless()

// A SEAT'S TURNS BY WAKE AND OUTCOME are one answer, one row per (seat, wake,
// disposition, outcome) over the range, every node and day summed.
var opUsageTurnLedger = define("usage.turn_ledger", opRead, usageDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q usage.SpendQuery) ([]usage.TurnLedgerRow, error) {
		if b.Usage == nil {
			return nil, errNoHalf
		}
		return b.Usage.TurnLedger(ctx, q)
	}).floorless()

var opUsageToolsElided = define("usage.tools_elided", opRead, usageDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a readsElidedArgs) (int64, error) {
		if b.Usage == nil {
			return 0, errNoHalf
		}
		return b.Usage.ToolsElided(ctx, a.From, a.To)
	}).floorless()

// repoReadsArgs is one page of a repository's readers: the read's query, the
// reader the page before it ended on — empty for the first — and the bytes its
// ASKER cuts a page at.
type repoReadsArgs struct {
	Query  usage.RepoReadsQuery
	After  usage.Group
	Budget int
}

// A REPOSITORY'S READERS ARE PAGED BY THEIR ASKER: a person walks them a page
// per request (a tool answer, a REST page), so the page is the budget the
// asker names, bounded by the wire's ([usagePageBytes]) where that is smaller.
var opUsageRepoReads = define("usage.repo_reads", opRead, usageDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a repoReadsArgs) (usage.Page[usage.RepoReader], error) {
		if b.Usage == nil {
			return usage.Page[usage.RepoReader]{}, errNoHalf
		}
		budget := a.Budget
		if b.pageBytes > 0 && (budget <= 0 || budget > b.pageBytes) {
			budget = b.pageBytes
		}
		return b.Usage.RepoReadsPage(ctx, a.Query, a.After, budget)
	}).floorless()

var opUsageRepoReadsElided = define("usage.repo_reads_elided", opRead, usageDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q usage.RepoReadsQuery) (int64, error) {
		if b.Usage == nil {
			return 0, errNoHalf
		}
		return b.Usage.RepoReadsElided(ctx, q)
	}).floorless()

// linkedFromArgs names the page whose backlinks are asked for, by its id.
type linkedFromArgs struct {
	PageID string
}

// A PAGE'S BACKLINKS READ BOTH CORPORA, pages and tasks (a task's linked-page
// relations included), so they run only where this node's knowledge base AND
// tracker copies are in service ([op.alsoReads], ADR-0079): otherwise a peer's
// page would be merged with links from rows this node no longer answers for.
// No floor, for [opKnowledgeSearch]'s reason.
var opLinkedFrom = define("knowledge.linked_from", opRead, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a linkedFromArgs) (search.Backlinks, error) {
		if b.Backlinks == nil {
			return search.Backlinks{}, errNoHalf
		}
		return b.Backlinks.LinkedFrom(ctx, a.PageID)
	}).floorless().alsoReads(trackerDomain)

// ---- the identity directory ---------------------------------------------- //

// A node holding no data runs seats, which ask the directory whose record a
// login names ([iam.OwnerOf]) and who holds each human seat (the party
// registry's routing, an agent's roster); serving requests, it also asks for a
// presented machine token's row ([TokenFacts]). It holds no replicated estate
// (ADR-0025), so each is asked of a data node, floored on the identity log.

// Binding is the part of a directory row a binding question reads: who holds
// the login, their kind and enrolment stage, and the seat the row binds.
//
// NOTHING ELSE CROSSES. [iamdomain.Sighting] also carries credential verifiers
// and grants; the asking node decides nothing on either (a credential is
// verified only where it is presented), and a verifier on the broker is of use
// only to an observer. This type makes the narrowing a property of the wire,
// not of every caller's restraint. Only [opRequester]'s answer carries grants,
// since the asker decides a turn's ceiling on them, and never a verifier.
type Binding struct {
	ID    string
	Kind  iam.Kind
	Stage iam.Stage
	Login string
	Seat  string
}

// bindingOf is the binding a directory row holds.
func bindingOf(seen iamdomain.Sighting) Binding {
	return Binding{ID: seen.ID, Kind: seen.Kind, Stage: seen.Stage, Login: seen.Login,
		Seat: seen.Seat}
}

type bindingArgs struct {
	Login string
}

// bound is a binding question's answer: the row, and how far the serving node
// could vouch for it — read in ONE snapshot there, for
// [iamdomain.Reader.PersonByLoginVouched]'s reason.
type bound struct {
	Binding Binding
	Vouch   iamdomain.Vouch
}

// WHOSE RECORD A LOGIN NAMES is decided on the asking node, against its own
// chart and its own Tier A tokens — this answers the directory's half: the row
// and the facts a verdict is formed from, never the verdict. A login nobody
// holds is the zero binding and no error, as the directory answers it.
var opIdentityBinding = define("identity.binding", opRead, identityDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a bindingArgs) (bound, error) {
		if b.Identity == nil {
			return bound{}, errNoHalf
		}
		seen, vouch, err := b.Identity.PersonByLoginVouched(ctx, a.Login)
		if err != nil {
			return bound{}, err
		}
		return bound{Binding: bindingOf(seen), Vouch: vouch}, nil
	})

// WHO HOLDS EACH HUMAN SEAT, read whole in one snapshot — the party registry is
// rebuilt whole from it, never patched. An error is UNKNOWN and crosses as one:
// an empty answer is a real reading ("nobody is bound to anything"), and an
// asker that read an outage as one would hand every suspended person's seat
// back to the chart.
var opSeatHolders = define("identity.seat_holders", opRead, identityDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, _ struct{}) ([]iamdomain.SeatHolder, error) {
		if b.Identity == nil {
			return nil, errNoHalf
		}
		return b.Identity.SeatHolders(ctx)
	})

// seatArgs names the one seat a holder question is about.
type seatArgs struct {
	Seat string
}

// heldRequester is who holds one seat, with the holder's grants, and whether
// anybody does: the directory's three-valued answer with its error crossing
// apart, as every error does.
type heldRequester struct {
	Requester iamdomain.Requester
	Held      bool
}

// WHO HOLDS ONE HUMAN SEAT, AND WITH WHAT GRANTS: [opSeatHolders]' answer for
// one seat, plus grants, in two seeks and one decode
// ([iamdomain.Reader.Requester]). A turn composes the person it acts for from
// it, up to once per call of a tool that decides for somebody (the requester
// ceiling), so its cost must not grow with the company's people. An error is
// UNKNOWN and crosses as one, never "nobody holds it", which a turn would read
// as a seat the chart alone speaks for.
//
// A NEW NAME, NEVER A RESHAPED ANSWER: a node that does not know it leaves it
// unserved and the asker moves on, so an answer without grants is never read
// as a person holding none or every app; a change to its meaning takes
// another name.
var opRequester = define("identity.requester", opRead, identityDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a seatArgs) (heldRequester, error) {
		if b.Identity == nil {
			return heldRequester{}, errNoHalf
		}
		requester, held, err := b.Identity.Requester(ctx, a.Seat)
		if err != nil {
			return heldRequester{}, err
		}
		return heldRequester{Requester: requester, Held: held}, nil
	})

// WHAT A PRESENTED MACHINE TOKEN IS CHECKED AGAINST, read in one snapshot (the
// credential, its owner, how far this node vouches for both) and answered
// NARROWED: the verifier withheld, a MAC over it and the asker's nonce in its
// place, under the key the asker named ([TokenFacts]). The asking node, which
// holds the presented secret, decides ([credential.CheckToken]). An error is
// UNKNOWN, never "no such token", which would 401 a sound credential.
//
// A KEY THIS NODE DOES NOT HOLD RUNS NOTHING, as a request signed under one
// does ([Envelopes]): the router asks a node that holds it, and the key is
// checked before the row is read.
var opIdentityMachineToken = define("identity.machine_token", opRead, identityDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a machineTokenArgs) (TokenFacts, error) {
		if b.Identity == nil {
			return TokenFacts{}, errNoHalf
		}
		key, held := b.verifiers.key(a.KeyID)
		if !held {
			return TokenFacts{}, declined{cause: fmt.Errorf("it holds no secrets.keys "+
				"entry %q to answer a machine token's verifier under (it holds %s)",
				a.KeyID, words.And(b.verifiers.held()))}
		}
		row, err := b.Identity.MachineToken(ctx, a.ID)
		if err != nil {
			return TokenFacts{}, err
		}
		return factsOf(row, key, a.Nonce), nil
	})

// ---- the engine's own chat ---------------------------------------------- //
//
// THE KNOWLEDGE BASE'S CLASSES: the store derives a chat write's operation id
// (and a message's or a new room's) from the caller's key on its actor
// ([chatlet.Actor.OpKey]) and the gesture; unkeyed, it mints one afresh, and a
// repeat would be a second message. So every gesture is a once-write
// repeatable under the key ([op.repeatableWhen]); the erase and the prune take
// a whole operation id and are idempotent writes, as a purge is.
//
// EVERY READ IS ANSWERED FOR ITS VIEWER, in the SIGNED body ([Envelopes]), or a
// private room could be read as anybody; and carries the chat log's FLOOR, the
// search included: a seat that just joined a room must find it.

type chatChannelsArgs struct {
	Viewer string
	Query  chatlet.ChannelsQuery
	Fresh  statelog.Freshness
}

var opChatChannels = define("chat.channels", opRead, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatChannelsArgs) (chatlet.ChannelListing, error) {
		if b.Chat == nil {
			return chatlet.ChannelListing{}, errNoHalf
		}
		return b.Chat.Channels(ctx, a.Viewer, a.Query, a.Fresh)
	})

type chatChannelArgs struct {
	Viewer    string
	ChannelID string
	Fresh     statelog.Freshness
}

var opChatChannel = define("chat.channel", opRead, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatChannelArgs) (chatlet.ChannelDetail, error) {
		if b.Chat == nil {
			return chatlet.ChannelDetail{}, errNoHalf
		}
		return b.Chat.Channel(ctx, a.Viewer, a.ChannelID, a.Fresh)
	})

type chatChannelNamedArgs struct {
	Viewer string
	Name   string
	Fresh  statelog.Freshness
}

// A ROOM BY ITS ADDRESS, answered for a room the viewer may not read exactly
// as for a name nobody holds — the reader's rule, kept on whichever node
// answers.
var opChatChannelNamed = define("chat.channel_named", opRead, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatChannelNamedArgs) (chatlet.ChannelDetail, error) {
		if b.Chat == nil {
			return chatlet.ChannelDetail{}, errNoHalf
		}
		return b.Chat.ChannelNamed(ctx, a.Viewer, a.Name, a.Fresh)
	})

type chatMessagesArgs struct {
	Viewer string
	Query  chatlet.TranscriptQuery
	Fresh  statelog.Freshness
}

var opChatMessages = define("chat.messages", opRead, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatMessagesArgs) (chatlet.Transcript, error) {
		if b.Chat == nil {
			return chatlet.Transcript{}, errNoHalf
		}
		return b.Chat.Messages(ctx, a.Viewer, a.Query, a.Fresh)
	})

type chatThreadArgs struct {
	Viewer string
	Query  chatlet.ThreadQuery
	Fresh  statelog.Freshness
}

var opChatThread = define("chat.thread", opRead, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatThreadArgs) (chatlet.Thread, error) {
		if b.Chat == nil {
			return chatlet.Thread{}, errNoHalf
		}
		return b.Chat.Thread(ctx, a.Viewer, a.Query, a.Fresh)
	})

// THE ENGINE'S TURN-START THREAD READ: chat.thread with whom each message was
// written for ([chatlet.TurnThread]), which no reader is handed. Its own
// operation, not a member on chat.thread's answer, which every reader's
// surface holds and an older data node would refuse; asked only through
// [TurnThreads], which the engine alone builds and which reads the plain
// thread where an older node answers it unserved.
var opChatTurnThread = define("chat.turn_thread", opRead, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatThreadArgs) (chatlet.TurnThread, error) {
		if b.Chat == nil {
			return chatlet.TurnThread{}, errNoHalf
		}
		return b.Chat.TurnThread(ctx, a.Viewer, a.Query, a.Fresh)
	})

type chatRoomNamesArgs struct {
	Viewer string
	Fresh  statelog.Freshness
}

// THE NAMES OF THE ROOMS A VIEWER MAY READ, resolved by the serving reader from
// the viewer that crossed, as every chat read's set is: never a set of rooms a
// caller could hand over.
var opChatRoomNames = define("chat.room_names", opRead, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatRoomNamesArgs) (chatlet.RoomNames, error) {
		if b.Chat == nil {
			return chatlet.RoomNames{}, errNoHalf
		}
		return b.Chat.RoomNames(ctx, a.Viewer, a.Fresh)
	})

// EVERY ROOM A VIEWER MAY BROWSE, resolved by the serving reader from the
// viewer that crossed, for [opChatRoomNames]' reason. Its arguments are that
// read's own.
var opChatDirectory = define("chat.directory", opRead, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatRoomNamesArgs) (chatlet.Directory, error) {
		if b.Chat == nil {
			return chatlet.Directory{}, errNoHalf
		}
		return b.Chat.Directory(ctx, a.Viewer, a.Fresh)
	})

// ChatQuery is one ranked search of the company's chat, as it crosses: the
// VIEWER whose rooms it searches, never the rooms. Unlike [search.ChatQuery],
// the visible set is not a field: it is an authorization fact a caller must
// not supply, resolved by the serving node from the viewer, on the copy whose
// index it reads.
type ChatQuery struct {
	// Viewer is the seat the search is FOR. Required — the reader refuses a
	// read for nobody.
	Viewer string

	// Text is what somebody typed.
	Text string

	// Channel narrows to one room, empty for every room the viewer may
	// read. INTERSECTED with the visible set, never trusted: a room the
	// viewer may not read answers nothing, since a refusal would confirm it
	// exists.
	Channel string

	// Author narrows to one speaker's handle, empty for anybody.
	Author string

	// Limit bounds the hits; zero takes the index's own default.
	Limit int

	// Fresh is the level the VISIBLE SET is read at; the index has none,
	// each node maintaining it behind its applier.
	Fresh statelog.Freshness
}

// ChatSearchAnswer is one search's hits, and what they were drawn from.
type ChatSearchAnswer struct {
	// Hits is what matched, best first. Never nil.
	Hits []search.ChatHit

	// Rooms are the rooms the hits were said in, by id, each with the name
	// and kind a hit is shown under ([chatlet.ReadableRoom]), as a mention
	// carries its room's ([chatlet.Mention]). FROM THE SEARCH'S OWN VISIBLE
	// SET ([ChatReader.Readable]), never a read beside it, so a hit is shown
	// under the name of the room the search was allowed to read.
	Rooms map[string]chatlet.ReadableRoom

	// Searched is how many rooms the search covered: zero for a viewer who
	// may read none, an answer rather than a refusal.
	Searched int

	// Building says the answering node's index was not caught up with its
	// transcript for the whole search ([ChatSearcher.Ready]): the hits may be
	// partial or from an index a rebuild will drop, which a seat must not
	// read as "nobody said that". On the answer, not an operation of its own
	// as [opKnowledgeBuilding] is: the readiness is the ANSWERING node's walk's,
	// and a second call could reach another node.
	Building bool

	// Position is the answering copy's applied position on the chat log,
	// read BEFORE the visible set was resolved, so it is at or below the
	// read's own and never overstates what the answer saw.
	Position statelog.Position
}

// THE VISIBLE SET AND THE HITS ARE ONE NODE'S, hence one operation: two hops
// could reach two copies, and a set handed to another node's index would be an
// authorization fact crossing the wire, the field [ChatQuery] lacks.
//
// A viewer who may read nothing is ANSWERED, not refused ([search.ErrNoViewer]
// is for a caller that lost its viewer); a viewer who is nobody is refused by
// the reader. Readiness is asked on BOTH SIDES of the search and must hold at
// both: a rebuild started meanwhile shows only after, a walk that caught up
// meanwhile only before. A readiness this node could not read is BUILDING: it
// cannot promise full coverage, and failing the search would break a tool
// whose hits exist.
var opChatSearch = define("chat.search", opRead, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q ChatQuery) (ChatSearchAnswer, error) {
		if b.Chat == nil || b.ChatSearch == nil {
			return ChatSearchAnswer{}, errNoHalf
		}
		at := b.Chat.At()
		ready := chatIndexReady(ctx, b.ChatSearch)
		readable, err := b.Chat.Readable(ctx, q.Viewer, q.Fresh)
		if err != nil {
			return ChatSearchAnswer{}, err
		}
		rooms := make(map[string]chatlet.ReadableRoom, len(readable))
		visible := make([]string, 0, len(readable))
		for _, room := range readable {
			if q.Channel == "" || room.ID == q.Channel {
				rooms[room.ID] = room
				visible = append(visible, room.ID)
			}
		}
		out := ChatSearchAnswer{Hits: []search.ChatHit{}, Searched: len(visible),
			Building: !ready, Position: at}
		if len(visible) == 0 {
			return out, nil
		}
		hits, err := b.ChatSearch.SearchMessages(ctx, search.ChatQuery{
			Text: q.Text, Channels: visible, Author: q.Author, Limit: q.Limit,
		})
		if err != nil {
			return ChatSearchAnswer{}, err
		}
		out.Building = !ready || !chatIndexReady(ctx, b.ChatSearch)
		if hits != nil {
			out.Hits = hits
		}
		for _, hit := range hits {
			if out.Rooms == nil {
				out.Rooms = make(map[string]chatlet.ReadableRoom)
			}
			out.Rooms[hit.ChannelID] = rooms[hit.ChannelID]
		}
		return out, nil
	})

// chatIndexReady is whether this node's chat index holds its whole transcript,
// with a read that failed taken as not — see [opChatSearch] — and said in the
// log, since the answer then carries no reason of its own.
func chatIndexReady(ctx context.Context, index ChatSearcher) bool {
	ready, err := index.Ready(ctx)
	if err != nil {
		log.WarnContext(ctx, "chat_index_readiness_unread", "error", err.Error(),
			"detail", "a chat search was answered as building because this node "+
				"could not tell whether its index covers the transcript")
		return false
	}
	return ready
}

type chatCreateChannelArgs struct {
	Actor   chatlet.Actor
	Channel chatlet.NewChannel
}

var opChatCreateChannel = define("chat.create_channel", opOnceWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatCreateChannelArgs) (chatlet.Written, error) {
		if b.ChatWriter == nil {
			return chatlet.Written{}, errNoHalf
		}
		return b.ChatWriter.CreateChannel(ctx, a.Actor, a.Channel)
	}).repeatableWhen(func(a chatCreateChannelArgs) bool { return chatKeyed(a.Actor) }).
	refusedBeforeAsking(func(a chatCreateChannelArgs) error { return chatActorWrites(a.Actor) })

type chatOpenDirectArgs struct {
	Actor        chatlet.Actor
	Participants []string
}

// openedDirect is [ChatWriter.OpenDirect]'s two answers, named.
type openedDirect struct {
	Written chatlet.Written
	Created bool
}

var opChatOpenDirect = define("chat.open_direct", opOnceWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatOpenDirectArgs) (openedDirect, error) {
		if b.ChatWriter == nil {
			return openedDirect{}, errNoHalf
		}
		w, created, err := b.ChatWriter.OpenDirect(ctx, a.Actor, a.Participants)
		return openedDirect{Written: w, Created: created}, err
	}).repeatableWhen(func(a chatOpenDirectArgs) bool { return chatKeyed(a.Actor) }).
	refusedBeforeAsking(func(a chatOpenDirectArgs) error { return chatActorWrites(a.Actor) })

type chatPatchChannelArgs struct {
	Actor     chatlet.Actor
	ChannelID string
	Patch     chatlet.ChannelPatch

	// Authority is the caller's answer from the authority table: whether
	// it may move this room's archive, and the room's unit when decided.
	// Decided on the asking node, judged against the serving node's
	// snapshot: a room that has since left that unit is a conflict.
	Authority chatlet.ChannelAuthority
}

var opChatPatchChannel = define("chat.patch_channel", opOnceWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatPatchChannelArgs) (chatlet.Written, error) {
		if b.ChatWriter == nil {
			return chatlet.Written{}, errNoHalf
		}
		return b.ChatWriter.PatchChannel(ctx, a.Actor, a.ChannelID, a.Patch, a.Authority)
	}).repeatableWhen(func(a chatPatchChannelArgs) bool { return chatKeyed(a.Actor) }).
	refusedBeforeAsking(func(a chatPatchChannelArgs) error { return chatActorWrites(a.Actor) })

type chatSetMembersArgs struct {
	Actor     chatlet.Actor
	ChannelID string
	Members   []chatlet.Member
	// Expected is the membership the caller read, nil for none
	// ([chatlet.Store.SetMembers]). A pointer, so the empty room a caller
	// expects crosses the wire as the empty list rather than as no
	// expectation at all.
	Expected *[]chatlet.Member `json:",omitempty"`
}

var opChatSetMembers = define("chat.set_members", opOnceWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatSetMembersArgs) (chatlet.Written, error) {
		if b.ChatWriter == nil {
			return chatlet.Written{}, errNoHalf
		}
		return b.ChatWriter.SetMembers(ctx, a.Actor, a.ChannelID, a.Members, a.Expected)
	}).repeatableWhen(func(a chatSetMembersArgs) bool { return chatKeyed(a.Actor) }).
	refusedBeforeAsking(func(a chatSetMembersArgs) error { return chatActorWrites(a.Actor) })

type chatAddMembersArgs struct {
	Actor     chatlet.Actor
	ChannelID string
	Handles   []string
}

// addedMembers is [ChatWriter.AddMembers]' two answers, named.
type addedMembers struct {
	Written chatlet.Written
	Added   []string
}

var opChatAddMembers = define("chat.add_members", opOnceWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatAddMembersArgs) (addedMembers, error) {
		if b.ChatWriter == nil {
			return addedMembers{}, errNoHalf
		}
		w, added, err := b.ChatWriter.AddMembers(ctx, a.Actor, a.ChannelID, a.Handles)
		return addedMembers{Written: w, Added: added}, err
	}).repeatableWhen(func(a chatAddMembersArgs) bool { return chatKeyed(a.Actor) }).
	refusedBeforeAsking(func(a chatAddMembersArgs) error { return chatActorWrites(a.Actor) })

type chatMembershipArgs struct {
	Actor     chatlet.Actor
	ChannelID string
}

var opChatJoin = define("chat.join", opOnceWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatMembershipArgs) (chatlet.Written, error) {
		if b.ChatWriter == nil {
			return chatlet.Written{}, errNoHalf
		}
		return b.ChatWriter.Join(ctx, a.Actor, a.ChannelID)
	}).repeatableWhen(func(a chatMembershipArgs) bool { return chatKeyed(a.Actor) }).
	refusedBeforeAsking(func(a chatMembershipArgs) error { return chatActorWrites(a.Actor) })

var opChatLeave = define("chat.leave", opOnceWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatMembershipArgs) (chatlet.Written, error) {
		if b.ChatWriter == nil {
			return chatlet.Written{}, errNoHalf
		}
		return b.ChatWriter.Leave(ctx, a.Actor, a.ChannelID)
	}).repeatableWhen(func(a chatMembershipArgs) bool { return chatKeyed(a.Actor) }).
	refusedBeforeAsking(func(a chatMembershipArgs) error { return chatActorWrites(a.Actor) })

type chatPostArgs struct {
	Actor     chatlet.Actor
	ChannelID string
	Message   chatlet.NewMessage
}

var opChatPost = define("chat.post", opOnceWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatPostArgs) (chatlet.Written, error) {
		if b.ChatWriter == nil {
			return chatlet.Written{}, errNoHalf
		}
		return b.ChatWriter.Post(ctx, a.Actor, a.ChannelID, a.Message)
	}).repeatableWhen(func(a chatPostArgs) bool { return chatKeyed(a.Actor) }).
	refusedBeforeAsking(func(a chatPostArgs) error { return chatActorWrites(a.Actor) })

type chatReplyArgs struct {
	Actor     chatlet.Actor
	ChannelID string
	ReplyTo   string
	Message   chatlet.NewMessage
}

// A REPLY IS ITS OWN OPERATION rather than a post with an optional parent: the
// store refuses a reply that names no message, and one operation for both would
// turn that refusal into a post to the room.
var opChatReply = define("chat.reply", opOnceWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatReplyArgs) (chatlet.Written, error) {
		if b.ChatWriter == nil {
			return chatlet.Written{}, errNoHalf
		}
		return b.ChatWriter.Reply(ctx, a.Actor, a.ChannelID, a.ReplyTo, a.Message)
	}).repeatableWhen(func(a chatReplyArgs) bool { return chatKeyed(a.Actor) }).
	refusedBeforeAsking(func(a chatReplyArgs) error { return chatActorWrites(a.Actor) })

// chatReachArgs is a post's dry run as it crosses: the post's own arguments,
// with the thread it would answer (empty for a room post).
type chatReachArgs struct {
	Actor     chatlet.Actor
	ChannelID string
	ReplyTo   string
	Message   chatlet.NewMessage
}

// A POST'S DRY RUN IS A READ: it decides on the serving node's snapshot, with
// the roster that node's write path routes by, and publishes nothing, so
// asking it again is asking again ([chatlet.Store.Reach]).
var opChatReach = define("chat.reach", opRead, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatReachArgs) (chatlet.Draft, error) {
		if b.ChatWriter == nil {
			return chatlet.Draft{}, errNoHalf
		}
		return b.ChatWriter.Reach(ctx, a.Actor, a.ChannelID, a.ReplyTo, a.Message)
	})

type chatEditArgs struct {
	Actor     chatlet.Actor
	ChannelID string
	MessageID string
	Edit      chatlet.EditMessage
}

var opChatEdit = define("chat.edit", opOnceWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatEditArgs) (chatlet.Written, error) {
		if b.ChatWriter == nil {
			return chatlet.Written{}, errNoHalf
		}
		return b.ChatWriter.Edit(ctx, a.Actor, a.ChannelID, a.MessageID, a.Edit)
	}).repeatableWhen(func(a chatEditArgs) bool { return chatKeyed(a.Actor) }).
	refusedBeforeAsking(func(a chatEditArgs) error { return chatActorWrites(a.Actor) })

type chatDeleteArgs struct {
	Actor     chatlet.Actor
	ChannelID string
	MessageID string

	// Authority is the caller's answer from the authority table — whether
	// it may take down a message it did not write — decided on the asking
	// node, which holds the grants, and judged against the message's author
	// in the serving node's own snapshot.
	Authority chatlet.MessageAuthority
}

var opChatDelete = define("chat.delete", opOnceWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatDeleteArgs) (chatlet.Written, error) {
		if b.ChatWriter == nil {
			return chatlet.Written{}, errNoHalf
		}
		return b.ChatWriter.Delete(ctx, a.Actor, a.ChannelID, a.MessageID, a.Authority)
	}).repeatableWhen(func(a chatDeleteArgs) bool { return chatKeyed(a.Actor) }).
	refusedBeforeAsking(func(a chatDeleteArgs) error { return chatActorWrites(a.Actor) })

type chatReactArgs struct {
	Actor     chatlet.Actor
	ChannelID string
	MessageID string
	Emoji     string

	// Remove takes the reaction back rather than adding it — one operation
	// for both, because the store's two gestures are one decision with
	// opposite intents.
	Remove bool
}

var opChatReact = define("chat.react", opOnceWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatReactArgs) (chatlet.Written, error) {
		if b.ChatWriter == nil {
			return chatlet.Written{}, errNoHalf
		}
		if a.Remove {
			return b.ChatWriter.Unreact(ctx, a.Actor, a.ChannelID, a.MessageID, a.Emoji)
		}
		return b.ChatWriter.React(ctx, a.Actor, a.ChannelID, a.MessageID, a.Emoji)
	}).repeatableWhen(func(a chatReactArgs) bool { return chatKeyed(a.Actor) }).
	refusedBeforeAsking(func(a chatReactArgs) error { return chatActorWrites(a.Actor) })

// ---- the admin's chat writes --------------------------------------------- //

type chatSetRetentionArgs struct {
	Actor     chatlet.Actor
	ChannelID string

	// Days is the room's override, and ABSENT CLEARS IT. A pointer, because
	// [chatlet.RetentionForever] is zero and real: a present 0 keeps the room
	// for ever, and collapsed into absent it would take the company's horizon.
	Days *int
}

// A ROOM'S RETENTION IS ITS OWN WRITE, never a patch field: a shorter horizon
// is a delayed prune, an admin's to set on a surface that asks a fresh proof of
// who they are, never one any member's patch could schedule. Every surface
// taking one has a key to make it repeatable.
var opChatSetRetention = define("chat.set_retention", opOnceWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatSetRetentionArgs) (chatlet.Written, error) {
		if b.ChatWriter == nil {
			return chatlet.Written{}, errNoHalf
		}
		return b.ChatWriter.SetRetention(ctx, a.Actor, a.ChannelID, a.Days)
	}).repeatableWhen(func(a chatSetRetentionArgs) bool { return chatKeyed(a.Actor) }).
	refusedBeforeAsking(func(a chatSetRetentionArgs) error { return chatActorWrites(a.Actor) })

type chatEraseArgs struct {
	Actor      chatlet.Actor
	OpID       string
	ChannelID  string
	MessageIDs []string
	Reason     string
}

// AN ERASE crosses as a purge does: its operation id is the caller's, so a
// repeat after an unanswered one collapses on it, and the ledger answers that
// repeat with the first outcome.
var opChatErase = define("chat.erase", opIdempotentWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatEraseArgs) (chatlet.Written, error) {
		if b.ChatWriter == nil {
			return chatlet.Written{}, errNoHalf
		}
		return b.ChatWriter.Erase(ctx, a.Actor, a.OpID, a.ChannelID, a.MessageIDs, a.Reason)
	}).refusedBeforeAsking(func(a chatEraseArgs) error { return chatActorWrites(a.Actor) })

type chatPruneArgs struct {
	Actor     chatlet.Actor
	OpID      string
	ChannelID string
	Cutoff    time.Time
}

// A PRUNE crosses as an erase does, under the operation id its caller derived.
var opChatPrune = define("chat.prune", opIdempotentWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatPruneArgs) (chatlet.Written, error) {
		if b.ChatWriter == nil {
			return chatlet.Written{}, errNoHalf
		}
		return b.ChatWriter.Prune(ctx, a.Actor, a.OpID, a.ChannelID, a.Cutoff)
	}).refusedBeforeAsking(func(a chatPruneArgs) error { return chatActorWrites(a.Actor) })

// Served is which halves a data node's copy runs natively, as a seat's
// admission asks it ([Router.Serves]).
type Served struct {
	Tracker bool
	Pages   bool

	// Chat is the engine's own chat. FALSE ON A COPY WHOSE COMPANY KEEPS NO
	// CHATLET, which serves no chat operation either — so a seat whose
	// company does waits for a copy that answers true rather than attaching
	// to one whose every chat call would be answered "not here".
	Chat bool
}

// opPing is ADMISSION's question to a data node: whether its copy admits a seat
// now ([Backend.Admits]) and which halves it runs natively; a copy that does
// not is passed over ([Router.Serves]).
//
// It asks the SEAT COPY ([SeatCopy]), so one out-of-service seat-gating domain
// answers `out_of_service`; another domain's wrong copy withholds nothing. NO
// FLOOR, and past the serving gate on purpose: admission is its own stricter
// gate, not whether the copy reached this node's writes. NO ARGUMENTS: the one
// estate is all it can be about.
var opPing = define("estate.ping", opRead, SeatCopy, false,
	func(ctx context.Context, b Backend, _ *Actor, _ struct{}) (Served, error) {
		if b.Admits != nil && !b.Admits(ctx) {
			return Served{}, errNotAdmitting
		}
		return b.served(), nil
	}).floorless().ungated()

// served is which halves this backend runs — one answer for a local admission
// and a remote one, so the two cannot disagree about a half.
func (b Backend) served() Served {
	return Served{Tracker: b.Tracker != nil, Pages: b.Pages != nil, Chat: b.Chat != nil}
}
