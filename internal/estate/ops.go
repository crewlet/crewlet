package estate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The serving half: what a data node answers with.
//
// Declared here, by the consumer, and kept to exactly what the operations
// below call — the seat surface's tools, and the operator's own surfaces: the
// dashboard and REST routes and the operator's MCP. Those reach the estate
// through this node's router as a seat's tools do, so a node whose copy is out
// of service answers its operator from a peer's copy rather than its own, and a
// node holding no data serves them at all. And the identity directory's two
// reads, which a node holding no data asks for its seats and its party
// registry; and the engine's own chat, whose every read is answered FOR a
// viewer it names.

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
	Activity(ctx context.Context, q worklet.ActivityQuery, now time.Time) (
		worklet.ActivityAnswer, error)
	MyWork(ctx context.Context, q worklet.MyWorkQuery, now time.Time,
		loc *time.Location) (worklet.MyWork, error)
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

	// The operator's writes, which no seat is handed: saved views, the
	// catalogue, a person's own state, rewriting one's own remark, the
	// trash and the purge.
	//
	// ViewPrior is the READ a save is decided on — a save replaces a view
	// whole by id, so who may make one is a question about the view it
	// overwrites as well as the one it writes — and it is on the writer
	// because the save that follows it is: the caller asks one seam both
	// questions, and the write refuses a prior that no longer holds.
	ViewPrior(ctx context.Context, id string) (worklet.ViewPrior, error)
	WriteView(ctx context.Context, opID string, view worklet.View,
		prior worklet.ViewPrior) (worklet.WriteResult, error)
	WriteTypes(ctx context.Context, opID string, types []worklet.TaskType) (worklet.WriteResult, error)
	WriteFields(ctx context.Context, opID string, fields []worklet.FieldDef) (worklet.WriteResult, error)
	MarkInbox(ctx context.Context, opID, handle string, gesture worklet.InboxGesture,
		authority worklet.PersonAuthority) (worklet.WriteResult, error)
	WritePins(ctx context.Context, opID, handle string, gesture worklet.PinGesture,
		authority worklet.PersonAuthority) (worklet.WriteResult, error)
	EditComment(ctx context.Context, opID, taskID, project, commentID, body string,
		notify *worklet.Notify) (worklet.WriteResult, error)
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
}

// PageWriter is the knowledge base's write side. The author is an argument
// of every call rather than a derived writer, which is Doclet's store's own
// shape, and so is the caller's operation key ([doclet.Actor.OpKey]), which
// rides on that author.
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
// with the one question a caller asks of an empty answer.
//
// ANSWER, NOT THE BEST-EFFORT SEARCH: a search that could not run is an error
// here ([doclet.Searcher.Answer]), because the asking node has to tell "the
// knowledge base could not be searched" from "nothing matched" — and an empty
// answer carried back over the wire reads as the second.
type KnowledgeSearcher interface {
	Answer(ctx context.Context, q knowledge.Query) (knowledge.Result, error)
	Building(ctx context.Context) bool
}

// IdentityReader is the identity directory's read side, as a node holding no
// data asks it: the two questions its seats and its party registry put to the
// directory, and no others.
//
// FACTS AND NOT VERDICTS, exactly as [iamdomain.Reader] answers them: the row a
// login binds and how far the serving node can vouch for it, and the seats the
// directory binds. What a stalled applier or a retained record MEANS — and the
// chart a binding is resolved against, and the Tier A tokens a node declares —
// are the ASKING node's, because the request path that decides on them is
// there and a verdict formed on another node would be a second opinion about
// whose seat a login is.
type IdentityReader interface {
	PersonByLoginVouched(ctx context.Context, login string) (iamdomain.Sighting,
		iamdomain.Vouch, error)
	SeatHolders(ctx context.Context) ([]iamdomain.SeatHolder, error)
}

// ChatReader is the engine's own chat's read side.
//
// THE VIEWER IS AN ARGUMENT OF EVERY READ, which is the domain's own shape
// rather than a choice made here: a room's contents are not a fact about the
// company the way a page is, so every read is answered for one seat and answers
// a room that seat may not read exactly as one nobody holds — on whichever data
// node answers it, because the rule is the reader's own ([chatlet.Visible]),
// judged against the membership rows in the read's own snapshot there. The
// viewer is the SEAT the asking node resolved its caller to; the serving reader
// refuses an empty one rather than reading it as anybody.
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
	RoomNames(ctx context.Context, viewer string,
		fresh statelog.Freshness) (chatlet.RoomNames, error)
	Directory(ctx context.Context, viewer string,
		fresh statelog.Freshness) (chatlet.Directory, error)

	// The reader's own views ([ChatReaderViews], chatreader.go).
	ChatReaderViews

	// Readable is every room the viewer may READ, which is wider than the
	// rooms it is in. It is no operation of its own: it is the
	// authorization half of a search ([opChatSearch]), resolved on the node
	// that holds the index the search reads, so the set and the hits are
	// one copy's answer, and so are the names the hits are shown under.
	Readable(ctx context.Context, viewer string,
		fresh statelog.Freshness) ([]chatlet.ReadableRoom, error)

	// At is this copy's applied position on the chat log.
	At() statelog.Position
}

// ChatWriter is the engine's own chat's write side. The author is an argument
// of every call, as the knowledge base's is, because one node's chat serves
// every seat and every person through one store — and so is the caller's
// operation key ([chatlet.Actor.OpKey]), which rides on that author.
//
// WHAT A CALLER WAS ADMITTED TO DO CROSSES AS A VALUE, never as the grants it
// was decided from, for [removeCommentArgs]'s reason: a moderator's take-down
// ([chatlet.MessageAuthority]) and an archive ([chatlet.ChannelAuthority]) are
// decided on the asking node, which holds the grants and the chart, and judged
// against the message's author and the room's unit in the serving node's own
// snapshot. A room's retention is its own gesture ([ChatWriter.SetRetention]),
// an admin's on a surface that asks a fresh proof of who they are, rather than
// a field any member's patch could carry.
//
// # What it deliberately does not carry
//
// A UNIT ROOM'S MEMBERSHIP. The org chart is its sole writer, and the chart's
// apply runs where the estate is held, against that data node's own write
// side — the tracker's projects and the knowledge base's containers are
// written the same way — so no surface on any node has a unit room's members
// to set, and an operation for it would be a second writer the chart cannot
// see.
type ChatWriter interface {
	CreateChannel(ctx context.Context, actor chatlet.Actor, in chatlet.NewChannel) (chatlet.Written, error)
	OpenDirect(ctx context.Context, actor chatlet.Actor, participants []string) (chatlet.Written, bool, error)
	PatchChannel(ctx context.Context, actor chatlet.Actor, channelID string, patch chatlet.ChannelPatch,
		authority chatlet.ChannelAuthority) (chatlet.Written, error)
	SetMembers(ctx context.Context, actor chatlet.Actor, channelID string,
		members []chatlet.Member) (chatlet.Written, error)
	AddMembers(ctx context.Context, actor chatlet.Actor, channelID string,
		handles []string) (chatlet.Written, []string, error)
	Join(ctx context.Context, actor chatlet.Actor, channelID string) (chatlet.Written, error)
	Leave(ctx context.Context, actor chatlet.Actor, channelID string) (chatlet.Written, error)
	Post(ctx context.Context, actor chatlet.Actor, channelID string,
		in chatlet.NewMessage) (chatlet.Written, error)
	Reply(ctx context.Context, actor chatlet.Actor, channelID, replyTo string,
		in chatlet.NewMessage) (chatlet.Written, error)
	Edit(ctx context.Context, actor chatlet.Actor, channelID, messageID string,
		in chatlet.EditMessage) (chatlet.Written, error)
	Delete(ctx context.Context, actor chatlet.Actor, channelID, messageID string,
		authority chatlet.MessageAuthority) (chatlet.Written, error)
	React(ctx context.Context, actor chatlet.Actor, channelID, messageID,
		emoji string) (chatlet.Written, error)
	Unreact(ctx context.Context, actor chatlet.Actor, channelID, messageID,
		emoji string) (chatlet.Written, error)

	// The admin's writes, which no seat is handed: a room's retention, and
	// the destructions — the compliance erase and a room's prune, each under
	// the operation id the caller derived, as a purge is.
	SetRetention(ctx context.Context, actor chatlet.Actor, channelID string,
		days *int) (chatlet.Written, error)
	Erase(ctx context.Context, actor chatlet.Actor, opID, channelID string,
		messageIDs []string, reason string) (chatlet.Written, error)
	Prune(ctx context.Context, actor chatlet.Actor, opID, channelID string,
		cutoff time.Time) (chatlet.Written, error)
}

// ChatSearcher is the chat keyword index over this node's corpus.
//
// THE INDEX, NOT A SEARCH FOR A VIEWER: it takes the visible rooms on the
// query and refuses a query naming none ([search.ErrNoViewer]). Which rooms
// those are is resolved from the viewer by [opChatSearch], beside it, on the
// same node — and so is whether the index has caught up with the transcript
// ([search.ChatIndexer.Ready]), which is a fact about that node's index and
// that node's copy.
type ChatSearcher interface {
	SearchMessages(ctx context.Context, q search.ChatQuery) ([]search.ChatHit, error)
	Ready(ctx context.Context) (bool, error)
}

// Backend is one data node's answer to every operation, over its copy of the
// estate, resolved PER REQUEST: a node that booted with no company brings its
// native runtime up at its first apply, and a request that arrives before then
// is answered "not here" rather than refused for ever.
//
// A nil half is a half this node does not run, and an operation on it is
// answered [unservedNoBackend] — the company is on a vendor for it.
type Backend struct {
	Tracker    TrackerReader
	Writer     func(Actor) TrackerWriter
	WorkSearch WorkSearcher
	Pages      PageReader
	PageWriter PageWriter
	Knowledge  KnowledgeSearcher
	Chat       ChatReader
	ChatWriter ChatWriter
	ChatSearch ChatSearcher

	// Identity is the identity directory, which every data node holds from
	// boot whatever its company keeps — nil only where the node's core
	// runtime is not up yet, which is answered like any half it does not
	// run.
	Identity IdentityReader

	// Artifacts is the artifacts estate, which every data node runs from
	// boot whatever its company keeps: its reader and its writer, each nil
	// where the node's core runtime is not up yet, and the writer nil too
	// where the node publishes nothing ([ArtifactsHalf]).
	Artifacts ArtifactsHalf

	// ArtifactData is the artifact datastore, which every data node runs
	// from boot beside the artifacts estate ([ArtifactDataHalf]).
	ArtifactData ArtifactDataHalf

	// Executor is the executor log, which every data node runs from boot
	// whatever its company keeps: its reader, and its writer only while the
	// node publishes, each nil where the core runtime is not up yet
	// ([ExecutorHalf]).
	Executor ExecutorHalf

	// Committed waits until this node's applier of the floor's log holds a
	// position — the floor every request carries, the node's own included.
	Committed func(ctx context.Context, at statelog.Position) error

	// Answers is whether this copy may answer a request now
	// ([statelog.Health.Answers]): serving — drained since its appliers
	// started and within the snapshot slack of every log's end — or level
	// this instant. A copy that does not LAGS: it is asked only once every
	// data node whose copy does not has run nothing, by a remote asker and
	// by this node's own router alike — a worse node to ask, never no node
	// ([Router.route]).
	//
	// NOT A SEAT'S ADMISSION, which refuses at a lag of one: a busy
	// company has a record in flight on most instants, and a request gate
	// that refused then sent every read — this node's own seats' included —
	// to whichever peer happened to be level, or to none. What a read must
	// see of its own node's writes is what the floors carry.
	Answers func(ctx context.Context) bool

	// Admits is whether a SEAT may attach to this copy now: established at
	// a lag of zero — the gate every node's seat admission asks of the copy
	// that will serve it ([Router.Serves]). Only admission's ping reads it.
	Admits func(ctx context.Context) bool

	// ServerSeams are the node's own: the dispatcher sets them on every
	// backend it hands an operation.
	ServerSeams
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

// errNotAdmitting is admission's ping on a copy that admits no seat yet
// ([Backend.Admits]): nothing ran, and the asker moves on.
var errNotAdmitting = errors.New("estate: this copy admits no seat yet")

// The domains an operation's floor may be on, and each one's stream, from the
// domain's own declaration. An operation's floor is on its OWN domain's log and
// no other: see [ready].
//
// THE SEVEN DOMAINS AN OPERATION READS OR WRITES, and not the other three the
// estate carries: the vectors and the usage days are written by a fleet duty
// and by each data node's own publisher, never through an operation, and no
// operation reads their rows at a position — the searches that rank by the
// vectors read them as an index, which carries no floor at all — and the
// Dashboard Assistant's conversations are read and written only where the API
// runs, on a data node, from that node's own copy, so no request carries them
// across the router. The executor log is among the seven though no surface
// writes it: the engine records a seat's executor session wherever the seat's
// box ran, a node without `data` included, and only a data node publishes
// ([originEngine], executor.go).
var (
	trackerDomain      = worklet.Domain{}.Name()
	pagesDomain        = doclet.Domain{}.Name()
	identityDomain     = iamdomain.Domain{}.Name()
	artifactsDomain    = artifactsdomain.Domain{}.Name()
	artifactDataDomain = artifactdata.Domain{}.Name()
	chatDomain         = chatlet.Domain{}.Name()
	executorDomain     = executordomain.Domain{}.Name()

	floorStreams = map[string]string{
		trackerDomain:      worklet.Domain{}.Stream().Name,
		pagesDomain:        doclet.Domain{}.Stream().Name,
		identityDomain:     iamdomain.Domain{}.Stream().Name,
		artifactsDomain:    artifactsdomain.Domain{}.Stream().Name,
		artifactDataDomain: artifactdata.Domain{}.Stream().Name,
		chatDomain:         chatlet.Domain{}.Stream().Name,
		executorDomain:     executordomain.Domain{}.Stream().Name,
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

var opViews = define("tracker.views", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a viewsArgs) (worklet.ViewListing, error) {
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
	})

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
	})

var opCatalogue = define("tracker.catalogue", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q worklet.CatalogueQuery) (worklet.CatalogueAnswer, error) {
		if b.Tracker == nil {
			return worklet.CatalogueAnswer{}, errNoHalf
		}
		return b.Tracker.Catalogue(ctx, q)
	})

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

var opProjects = define("tracker.projects", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q worklet.ProjectQuery) (worklet.ProjectListing, error) {
		if b.Tracker == nil {
			return worklet.ProjectListing{}, errNoHalf
		}
		q.Units = b.Units
		return b.Tracker.Projects(ctx, q)
	})

var opProject = define("tracker.project", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q worklet.ProjectDetailQuery) (worklet.ProjectDetail, error) {
		if b.Tracker == nil {
			return worklet.ProjectDetail{}, errNoHalf
		}
		q.Units = b.Units
		return b.Tracker.Project(ctx, q)
	})

type workSearchArgs struct {
	Text  string
	Limit int

	// Mode is how to rank. Empty is hybrid — what a caller that names no
	// mode (a seat's search_work tool) sends.
	Mode knowledge.Mode
}

// THE RANKED SEARCH CARRIES NO FLOOR: it reads the lexical index, which each
// node's own walk maintains on its own schedule behind the applier, so a floor
// on the log would promise a visibility the index does not give.
var opWorkSearch = define("tracker.search", opRead, "", false,
	func(ctx context.Context, b Backend, _ *Actor, a workSearchArgs) (worklet.SearchAnswer, error) {
		if b.WorkSearch == nil {
			return worklet.SearchAnswer{}, errNoHalf
		}
		return b.WorkSearch.Search(ctx, worklet.SearchQuery{Text: a.Text, Limit: a.Limit, Mode: a.Mode})
	})

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

// A PROJECT'S FILES, as rows: the listing, one file with the object holding
// its bytes, and the two writes. The BYTES never cross here — every node,
// stateless or not, streams them to and from the one object store the whole
// fleet shares through its own client, and asks this service only for the row
// that names them.
//
// The two operations whose shape names the object are named for what they
// carry (`tracker.read_file`, `tracker.write_file`). An operation a node does
// not serve is answered `unserved`, which sends the router on to a node that
// does.
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

// A TURN'S SPEND, recorded by the node that ran the turn — which on a node
// without `data` is a node with no applier, so it crosses like any other
// write. It carries its operation id, so an unanswered one is asked again of
// the next node: the applier adds a turn's spend only for a new operation, and
// a repeat under the same id counts nothing twice.
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

var opCreateTask = define("tracker.create_task", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a createTaskArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.CreateTask(ctx, a.OpID, a.Task, a.Notify)
	})

type createTaskAskingArgs struct {
	OpID   string
	Task   worklet.Task
	Ask    worklet.Comment
	Notify *worklet.Notify
}

var opCreateTaskAsking = define("tracker.create_task_asking", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a createTaskAskingArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.CreateTaskAsking(ctx, a.OpID, a.Task, a.Ask, a.Notify)
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

// A DROP ON THE BOARD is a WALKING gesture on one task — its lane, then its
// place among its neighbours — each step under an operation id derived from
// the one the caller minted, so the whole of it is asked again of the next
// holder under the same id, and a step that holder's ledger answers is not
// written twice. A step one holder answered unvouched is not final
// ([unvouched]).
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
	})

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

// WHAT A SAVE WOULD REPLACE, read for the caller to decide its authority on
// and hand back to the save. A READ, so it is asked anywhere and repeated
// freely — and floored like every tracker read, since the asker may have
// written the view through another node a moment ago. The save that follows
// it refuses a prior that has moved in its own snapshot, wherever it runs, so
// a stale answer here is a refusal there rather than a wrong overwrite.
var opViewPrior = define("tracker.view_prior", opRead, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a viewPriorArgs) (worklet.ViewPrior, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.ViewPrior{}, err
		}
		return w.ViewPrior(ctx, a.ID)
	})

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
	OpID      string
	Handle    string
	Gesture   worklet.InboxGesture
	Authority worklet.PersonAuthority
}

// A GESTURE, never the lists: the tracker resolves it against the record its
// own decide reads, so a caller never sends back a list it read in another
// transaction — on whichever holder answers.
var opMarkInbox = define("tracker.mark_inbox", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a markInboxArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.MarkInbox(ctx, a.OpID, a.Handle, a.Gesture, a.Authority)
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

// A PURGE crosses like every other tracker write — its operation id is what a
// repeat after an unanswered one collapses on, and the purge's own ledger row
// answers that repeat with the first outcome — and its refusals keep their
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

// chatKeyed is [keyed] for a chat write: one whose actor carries the caller's
// own operation key ([chatlet.Actor.OpKey]), from which the chat store derives the
// operation id — and a message's, or a new room's — bound to what the gesture
// asks, so a repeat under it is the same operation.
func chatKeyed(actor chatlet.Actor) bool { return strings.TrimSpace(actor.OpKey) != "" }

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
	}).repeatableWhen(func(a createPageArgs) bool { return keyed(a.Actor) })

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
	}).repeatableWhen(func(a savePageArgs) bool { return keyed(a.Actor) })

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
	}).repeatableWhen(func(a renamePageArgs) bool { return keyed(a.Actor) })

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
})

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
	}).repeatableWhen(func(a editCommentArgs) bool { return keyed(a.Actor) })

// ---- the operator's page writes ------------------------------------------ //

// The knowledge base's writes no seat is handed — the trash, the purge and a
// remark's take-down — routed for the reason the operator's tracker writes are.
// Each is a once-write made repeatable by the caller's key, as every page write
// is: unkeyed, a second trash is a second record and a second purge a refusal
// of its own first copy.

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
	}).repeatableWhen(func(a pageRefArgs) bool { return keyed(a.Actor) })

var opRestorePage = define("pages.restore", opOnceWrite, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a pageRefArgs) (doclet.Written, error) {
		if b.PageWriter == nil {
			return doclet.Written{}, errNoHalf
		}
		return b.PageWriter.Restore(ctx, a.Actor, a.PageID)
	}).repeatableWhen(func(a pageRefArgs) bool { return keyed(a.Actor) })

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
	}).repeatableWhen(func(a purgePageArgs) bool { return keyed(a.Actor) })

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
	}).repeatableWhen(func(a removeCommentArgs) bool { return keyed(a.Actor) })

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
}

// query is the search a request names, against the serving node's own chart:
// the seat's role and the org whose read scope applies.
func (a knowledgeArgs) query(b Backend) knowledge.Query {
	q := knowledge.Query{Text: a.Text, Limit: a.Limit, Mode: a.Mode}
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

// THE KNOWLEDGE SEARCH CARRIES NO FLOOR, for [opWorkSearch]'s reason: it
// reads the index, which no log position describes.
var opKnowledgeSearch = define("knowledge.search", opRead, "", false,
	func(ctx context.Context, b Backend, _ *Actor, a knowledgeArgs) (knowledge.Result, error) {
		if b.Knowledge == nil {
			return knowledge.Result{}, errNoHalf
		}
		return b.Knowledge.Answer(ctx, a.query(b))
	})

// WHETHER THE INDEX IS STILL BUILDING, which is what turns an empty search into
// "still indexing" — asked, like the search, of the index rather than the log.
var opKnowledgeBuilding = define("knowledge.building", opRead, "", false,
	func(ctx context.Context, b Backend, _ *Actor, _ struct{}) (bool, error) {
		if b.Knowledge == nil {
			return false, errNoHalf
		}
		return b.Knowledge.Building(ctx), nil
	})

// ---- the identity directory ---------------------------------------------- //

// A node holding no data runs seats, and its seats ask the identity directory
// two things: whose record a login names, which every person verb resolves
// before it is decided ([iam.OwnerOf]), and who holds each human seat, which
// the party registry routes inbound messages by and an agent's roster is cut
// by. ADR-0025 gives such a node no replicated estate and no applier, so both
// are asked of a data node, floored on the identity log like any read of rows
// a log derives.

// Binding is the part of a directory row a binding question reads: who holds
// the login, what kind of principal they are, how far through enrolment they
// got, and the seat the row binds.
//
// NOTHING ELSE CROSSES. [iamdomain.Sighting] also carries the row's credential
// verifiers and its grants; the asking node decides nothing on either — a
// binding is about a seat, and a credential is verified only where it is
// presented — and a verifier on the broker is a value the asker has no use for
// and an observer would. So the serving node narrows the row before it answers,
// and this type is what makes that narrowing a property of the wire rather than
// of every caller's restraint.
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

// ---- the engine's own chat ---------------------------------------------- //
//
// THE KNOWLEDGE BASE'S CLASSES, for the knowledge base's reason: a chat write
// names its author and its caller's key ([chatlet.Actor.OpKey]) on its own
// arguments, and the store derives the operation — and a message's or a new
// room's id — from that key and what the gesture asks, so a call that carries
// one is the same operation however often it is asked, and one that carries
// none is minted afresh by the store on whichever node runs it: a repeat would
// be a second message. So every gesture is a once-write that turns repeatable
// under the caller's key ([op.repeatableWhen]), and the two that take a whole
// operation id from their caller — the erase and the prune — are idempotent
// writes, as a purge is.
//
// EVERY READ IS ANSWERED FOR ITS VIEWER, and the viewer is in the SIGNED body
// ([Envelopes]): the serving reader decides which rooms that seat may read from
// the membership rows in its own snapshot, so a request naming a viewer is a
// request to be shown what that seat may see, and an unsigned one would have
// been a private room read as anybody.
//
// AND EVERY ONE CARRIES THE CHAT LOG'S FLOOR, the search included: what a seat
// may read is decided by membership ROWS, and a seat that has just joined a
// room — through whichever data node took the join — must find it among the
// rooms its next read and its next search cover.

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
// VIEWER whose rooms it searches, never the rooms.
//
// THE VISIBLE SET IS NOT A FIELD, which is the whole point of this type beside
// [search.ChatQuery]: which rooms somebody may read is an authorization fact,
// and a caller able to supply it is a caller able to supply the wrong one. The
// serving node resolves it from the viewer, on the copy whose index it then
// reads.
type ChatQuery struct {
	// Viewer is the seat the search is FOR. Required — the reader refuses a
	// read for nobody.
	Viewer string

	// Text is what somebody typed.
	Text string

	// Channel narrows to one room, empty for every room the viewer may
	// read. It is INTERSECTED with the visible set rather than trusted, so
	// naming a room the viewer may not read answers nothing rather than
	// refusing: a refusal confirms the room exists, and "there is no such
	// room" and "you may not see it" must read identically from outside.
	Channel string

	// Author narrows to one speaker's handle, empty for anybody.
	Author string

	// Limit bounds the hits; zero takes the index's own default.
	Limit int

	// Fresh is the level the VISIBLE SET is read at. The index itself has
	// none — each node maintains it behind its applier — so this is the
	// level of the only half that can be behind and that also decides what
	// may be seen.
	Fresh statelog.Freshness
}

// ChatSearchAnswer is one search's hits, and what they were drawn from.
type ChatSearchAnswer struct {
	// Hits is what matched, best first. Never nil.
	Hits []search.ChatHit

	// Rooms are the rooms the hits were said in, by id, each with the name
	// and kind a hit in it is shown under ([chatlet.ReadableRoom]), as a
	// mention carries its room's ([chatlet.Mention]): "#launch" rather than an
	// id nobody reads, and a direct conversation, which has no name, by its
	// kind. FROM THE SEARCH'S OWN VISIBLE SET ([ChatReader.Readable]), read
	// in the transaction that admitted each room, never from a read beside
	// it, so a hit is shown under the name of the room the search was
	// allowed to read.
	Rooms map[string]chatlet.ReadableRoom

	// Searched is how many rooms the search covered — zero for a viewer
	// who may read none, which is an answer rather than a refusal.
	Searched int

	// Building says the answering node's index was not caught up with the
	// transcript it holds for the whole of the search ([ChatSearcher.Ready]),
	// so the hits may be drawn from the part of it the walk had reached, or
	// from an index a rebuild is about to drop: "the index is still
	// building", which a seat and a screen must not read as "nobody said
	// that" — it would act on that by asking again, or by deciding nothing
	// was ever agreed.
	//
	// ON THE ANSWER rather than an operation of its own beside it, unlike
	// the knowledge base's ([opKnowledgeBuilding]): the readiness is the
	// ANSWERING node's walk's, and a second call could be answered by
	// another node, whose index says nothing about these hits.
	Building bool

	// Position is the answering copy's applied position on the chat log,
	// read BEFORE the visible set was resolved, so it is at or below the
	// read's own and never overstates what the answer saw.
	Position statelog.Position
}

// THE VISIBLE SET AND THE HITS ARE ONE NODE'S, which is why this is one
// operation rather than a read of the set followed by a search: two hops could
// be answered by two copies, and a set resolved on one and handed to another
// node's index would be an authorization fact crossing the wire — the very
// thing [ChatQuery] has no field for.
//
// A VIEWER WHO MAY READ NOTHING IS ANSWERED, NOT REFUSED. The index's own
// [search.ErrNoViewer] is for a caller that lost its viewer; handed an empty
// set here, it would turn an ordinary state into an error a seat reports as a
// broken tool. A viewer who is NOBODY is still refused — by the reader, whose
// visible set is the first thing read.
//
// THE INDEX IS ASKED WHETHER IT IS READY ON BOTH SIDES OF THE SEARCH, and the
// answer is building unless it was ready at both. Readiness moves in both
// directions while a search runs: a rebuild asked for meanwhile — an adoption
// replaced the estate — leaves hits drawn from the index of the copy it
// replaced, which only the ask after the search can see, and a walk that
// caught up meanwhile leaves hits drawn from the part it had reached, which
// only the ask before can see. Either one alone answers "caught up" for a
// list that was not.
//
// A READINESS THIS NODE COULD NOT READ IS BUILDING. The field promises "these
// hits may not cover every message"; a node that could not tell cannot make
// the opposite promise, and failing the search over it would answer a seat
// with a broken tool where hits it can show exist.
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
	}).repeatableWhen(func(a chatCreateChannelArgs) bool { return chatKeyed(a.Actor) })

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
	}).repeatableWhen(func(a chatOpenDirectArgs) bool { return chatKeyed(a.Actor) })

type chatPatchChannelArgs struct {
	Actor     chatlet.Actor
	ChannelID string
	Patch     chatlet.ChannelPatch

	// Authority is the caller's answer from the authority table — whether
	// it may move this room's archive, and the unit the room was recorded
	// under when that was decided — decided on the asking node, which holds
	// the grants and the chart, and judged against the room as the serving
	// node's own snapshot holds it: an archive decided on a unit the room
	// has since left is refused as a conflict there.
	Authority chatlet.ChannelAuthority
}

var opChatPatchChannel = define("chat.patch_channel", opOnceWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatPatchChannelArgs) (chatlet.Written, error) {
		if b.ChatWriter == nil {
			return chatlet.Written{}, errNoHalf
		}
		return b.ChatWriter.PatchChannel(ctx, a.Actor, a.ChannelID, a.Patch, a.Authority)
	}).repeatableWhen(func(a chatPatchChannelArgs) bool { return chatKeyed(a.Actor) })

type chatSetMembersArgs struct {
	Actor     chatlet.Actor
	ChannelID string
	Members   []chatlet.Member
}

var opChatSetMembers = define("chat.set_members", opOnceWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatSetMembersArgs) (chatlet.Written, error) {
		if b.ChatWriter == nil {
			return chatlet.Written{}, errNoHalf
		}
		return b.ChatWriter.SetMembers(ctx, a.Actor, a.ChannelID, a.Members)
	}).repeatableWhen(func(a chatSetMembersArgs) bool { return chatKeyed(a.Actor) })

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
	}).repeatableWhen(func(a chatAddMembersArgs) bool { return chatKeyed(a.Actor) })

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
	}).repeatableWhen(func(a chatMembershipArgs) bool { return chatKeyed(a.Actor) })

var opChatLeave = define("chat.leave", opOnceWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatMembershipArgs) (chatlet.Written, error) {
		if b.ChatWriter == nil {
			return chatlet.Written{}, errNoHalf
		}
		return b.ChatWriter.Leave(ctx, a.Actor, a.ChannelID)
	}).repeatableWhen(func(a chatMembershipArgs) bool { return chatKeyed(a.Actor) })

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
	}).repeatableWhen(func(a chatPostArgs) bool { return chatKeyed(a.Actor) })

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
	}).repeatableWhen(func(a chatReplyArgs) bool { return chatKeyed(a.Actor) })

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
	}).repeatableWhen(func(a chatEditArgs) bool { return chatKeyed(a.Actor) })

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
	}).repeatableWhen(func(a chatDeleteArgs) bool { return chatKeyed(a.Actor) })

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
	}).repeatableWhen(func(a chatReactArgs) bool { return chatKeyed(a.Actor) })

// ---- the admin's chat writes --------------------------------------------- //

type chatSetRetentionArgs struct {
	Actor     chatlet.Actor
	ChannelID string

	// Days is the room's override, and ABSENT CLEARS IT — a POINTER that
	// crosses as one, because [chatlet.RetentionForever] is zero and is a real
	// setting: a present 0 keeps the room for ever, and a wire that
	// collapsed it into an absent one would hand the room the company's
	// horizon and the retention duty a year of somebody's decisions.
	Days *int
}

// A ROOM'S RETENTION IS ITS OWN WRITE, never a field of a patch: a shorter
// horizon is a prune on a delay, so who may set one is an admin's to decide on
// a surface that asks a fresh proof of who they are, and an operation every
// member's patch could reach would be a destruction any member could schedule.
// Repeatable under its caller's key like every chat gesture, and a surface that
// takes one always has a key to give it.
var opChatSetRetention = define("chat.set_retention", opOnceWrite, chatDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a chatSetRetentionArgs) (chatlet.Written, error) {
		if b.ChatWriter == nil {
			return chatlet.Written{}, errNoHalf
		}
		return b.ChatWriter.SetRetention(ctx, a.Actor, a.ChannelID, a.Days)
	}).repeatableWhen(func(a chatSetRetentionArgs) bool { return chatKeyed(a.Actor) })

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
	})

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
	})

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

// opPing is ADMISSION's question, asked of a data node: whether its copy
// admits a seat now ([Backend.Admits]), and which halves it runs natively. A
// copy that does not is passed over, like a node that ran nothing — see
// [Router.Serves].
//
// NO FLOOR, and past the serving gate on purpose: it asks whether the copy is
// ready for a seat, which is its own stricter gate, not whether it has
// reached this node's writes. And NO ARGUMENTS: the one estate is the only
// thing it can be about.
var opPing = define("estate.ping", opRead, "", false,
	func(ctx context.Context, b Backend, _ *Actor, _ struct{}) (Served, error) {
		if b.Admits != nil && !b.Admits(ctx) {
			return Served{}, errNotAdmitting
		}
		return b.served(), nil
	}).ungated()

// served is which halves this backend runs — one answer for a local admission
// and a remote one, so the two cannot disagree about a half.
func (b Backend) served() Served {
	return Served{Tracker: b.Tracker != nil, Pages: b.Pages != nil, Chat: b.Chat != nil}
}
