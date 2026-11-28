package estate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/usage"
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
	ViewsPage(ctx context.Context, q worklet.ViewQuery, after string) (worklet.ViewListing, string, error)
	ExpandedQuery(ctx context.Context, params map[string]any,
		viewer worklet.Viewer, now time.Time, loc *time.Location) (worklet.Query, error)
	Catalogue(ctx context.Context, q worklet.CatalogueQuery) (worklet.CatalogueAnswer, error)
	Person(ctx context.Context, q worklet.PersonQuery, now time.Time) (worklet.PersonState, error)
	Thread(ctx context.Context, q worklet.ThreadQuery,
		fresh statelog.Freshness) (worklet.ResolvedThread, error)
	WorkPatterns(ctx context.Context, q worklet.PatternsQuery) (worklet.WorkPatterns, error)
	FlowHours(ctx context.Context, q worklet.FlowHoursQuery) ([]worklet.FlowHours, error)
	FlowItems(ctx context.Context, q worklet.FlowHoursQuery) (map[string]int, error)
	FlowPickups(ctx context.Context, q worklet.FlowHoursQuery) ([]time.Duration, error)
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
	EveryViewPage(ctx context.Context, q worklet.EveryViewQuery, after string) (worklet.ViewListing, string, error)
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
	SkillPagesPage(ctx context.Context, container, after string, fresh statelog.Freshness) ([]doclet.Page, string, error)

	// The operator's reads: every container, what happened to a page or
	// a container, and one revision of a page's body.
	Containers(ctx context.Context, fresh statelog.Freshness) ([]doclet.ContainerListing, error)
	ContainersPage(ctx context.Context, after string, fresh statelog.Freshness) ([]doclet.ContainerListing, string, error)
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
	SeatHoldersHeld(ctx context.Context) ([]iamdomain.SeatHolder, error)
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
	// TakeReceipt is the engine's record on a seat's receipt for a message
	// that its running turn took it ([chatlet.Store.TakeReceipt]).
	TakeReceipt(ctx context.Context, actor chatlet.Actor, channelID, messageID, handle string,
		as chatlet.Taken) (chatlet.Written, error)

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

	// Advisor is the advisor log, which every data node runs from boot
	// ([AdvisorHalf]).
	Advisor AdvisorHalf

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
// TWELVE of the estate's fourteen domains: a fleet duty writes the vectors and
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
	advisorDomain      = advisordomain.Domain{}.Name()

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
		advisorDomain:      advisordomain.Domain{}.Stream().Name,
	}
)

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
	}).oneAnswer().floorless().ungated()

// served is which halves this backend runs — one answer for a local admission
// and a remote one, so the two cannot disagree about a half.
func (b Backend) served() Served {
	return Served{Tracker: b.Tracker != nil, Pages: b.Pages != nil, Chat: b.Chat != nil}
}
