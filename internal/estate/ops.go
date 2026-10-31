package estate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
	"github.com/crewlet/crewlet/internal/usage"
)

// The serving half: what a data node answers with.
//
// Declared here, by the consumer, and kept to exactly what the operations
// below and in codeops.go call — the seat surface's tools, the code host, and
// the operator's own surfaces: the dashboard and REST routes and the
// operator's MCP. Those reach the estate through this node's router as a
// seat's tools do, so a node whose copy of the operation's domain is out of
// service answers its operator from a peer's copy rather than its own, and a
// node holding no data serves them at all. And the identity directory's three
// reads, which a node holding no data asks for its seats, its party registry
// and the requests it serves.

// TrackerReader is the tracker's read side the surfaces ask of.
type TrackerReader interface {
	Tasks(ctx context.Context, q tracker.Query, now time.Time) (tracker.Answer, error)
	Task(ctx context.Context, idOrKey string, want tracker.DetailWants,
		fresh statelog.Freshness) (tracker.TaskDetail, error)
	Views(ctx context.Context, q tracker.ViewQuery) (tracker.ViewListing, error)
	ExpandedQuery(ctx context.Context, params map[string]any,
		viewer tracker.Viewer, now time.Time, loc *time.Location) (tracker.Query, error)
	Catalogue(ctx context.Context, q tracker.CatalogueQuery) (tracker.CatalogueAnswer, error)
	Person(ctx context.Context, q tracker.PersonQuery, now time.Time) (tracker.PersonState, error)
	Thread(ctx context.Context, q tracker.ThreadQuery,
		fresh statelog.Freshness) (tracker.ResolvedThread, error)
	Activity(ctx context.Context, q tracker.ActivityQuery, now time.Time) (
		tracker.ActivityAnswer, error)
	MyWork(ctx context.Context, q tracker.MyWorkQuery, now time.Time,
		loc *time.Location) (tracker.MyWork, error)
	Projects(ctx context.Context, q tracker.ProjectQuery) (tracker.ProjectListing, error)
	Project(ctx context.Context, q tracker.ProjectDetailQuery) (tracker.ProjectDetail, error)
	Files(ctx context.Context, q tracker.FileQuery) (tracker.FileListing, error)
	File(ctx context.Context, project, path string, fresh statelog.Freshness) (tracker.FileDetail, error)

	// The operator's reads: a unit's workload, a person's inbox and the
	// routing a change took.
	Workload(ctx context.Context, q tracker.WorkloadQuery, now time.Time,
		loc *time.Location) (tracker.WorkloadAnswer, error)
	Inbox(ctx context.Context, q tracker.InboxQuery, now time.Time) (tracker.InboxAnswer, error)
	Routing(ctx context.Context, q tracker.RoutingQuery, now time.Time) (tracker.RoutingAnswer, error)
	TurnsOf(ctx context.Context, idOrKey, cursor string, limit int,
		fresh statelog.Freshness) (tracker.TaskTurns, error)
	EveryView(ctx context.Context, q tracker.EveryViewQuery) (tracker.ViewListing, error)
	Flow(ctx context.Context, q tracker.FlowQuery, now time.Time,
		loc *time.Location) (tracker.FlowAnswer, error)
	CompanyFeed(ctx context.Context, q tracker.FeedQuery) (tracker.FeedPage, error)
	Decisions(ctx context.Context, q tracker.DecisionsQuery, now time.Time,
		loc *time.Location) (tracker.DecisionsAnswer, error)
	TurnPlaces(ctx context.Context, runs []string,
		fresh statelog.Freshness) (map[string]tracker.TurnPlace, error)
}

// TrackerWriter is the tracker's write side, as ONE actor — see [Actor].
type TrackerWriter interface {
	CreateTask(ctx context.Context, opID string, task tracker.Task,
		notify *tracker.Notify) (tracker.WriteResult, error)
	CreateTaskAsking(ctx context.Context, opID string, task tracker.Task,
		ask tracker.Comment, notify *tracker.Notify) (tracker.WriteResult, error)
	PlaceTask(ctx context.Context, opID string, place tracker.Place,
		notify *tracker.Notify) (tracker.PlaceResult, error)
	UpdateTask(ctx context.Context, opID, id, project string, ifMatch uint64,
		patch tracker.TaskPatch, kind tracker.ChangeKind,
		notify *tracker.Notify) (tracker.WriteResult, error)
	Depend(ctx context.Context, opID string, change tracker.DependencyChange,
		leads tracker.Leads) (tracker.DependencyResult, error)
	MergeDuplicates(ctx context.Context, opID, duplicate, into string,
		reparent bool, notify *tracker.Notify) (tracker.WriteResult, error)
	MoveTaskToProject(ctx context.Context, opID, taskID, target string,
		notify *tracker.Notify) (tracker.WriteResult, error)
	WriteProject(ctx context.Context, opID, key string, edit tracker.ProjectEdit,
		authority tracker.ProjectAuthority) (tracker.WriteResult, error)
	WriteTags(ctx context.Context, opID, project string, edit tracker.TagEdit,
		authority tracker.TagAuthority) (tracker.WriteResult, error)
	EnsureTags(ctx context.Context, opID, project string, tags []string) (
		[]string, []string, error)
	PutFile(ctx context.Context, opID string, put tracker.FilePut) (tracker.WriteResult, error)
	RemoveFile(ctx context.Context, opID, project, path string, ifMatch uint64) (tracker.WriteResult, error)
	RecordTurn(ctx context.Context, opID string, turn tracker.TurnRecord) (tracker.WriteResult, error)

	// The operator's writes, which no seat is handed: saved views, the
	// catalogue, a person's own state, rewriting one's own remark, the
	// trash and the purge.
	//
	// ViewPrior is the READ a save is decided on — a save replaces a view
	// whole by id, so who may make one is a question about the view it
	// overwrites as well as the one it writes — and it is on the writer
	// because the save that follows it is: the caller asks one seam both
	// questions, and the write refuses a prior that no longer holds.
	ViewPrior(ctx context.Context, id string) (tracker.ViewPrior, error)
	WriteView(ctx context.Context, opID string, view tracker.View,
		prior tracker.ViewPrior) (tracker.WriteResult, error)
	WriteTypes(ctx context.Context, opID string, types []tracker.TaskType) (tracker.WriteResult, error)
	WriteFields(ctx context.Context, opID string, fields []tracker.FieldDef) (tracker.WriteResult, error)
	MarkInbox(ctx context.Context, opID, handle string, gesture inbox.Gesture[tracker.Reason],
		authority tracker.PersonAuthority) (tracker.WriteResult, error)
	WritePins(ctx context.Context, opID, handle string, gesture tracker.PinGesture,
		authority tracker.PersonAuthority) (tracker.WriteResult, error)
	EditComment(ctx context.Context, opID, taskID, project, commentID, body string,
		notify *tracker.Notify) (tracker.WriteResult, error)
	WritePriorities(ctx context.Context, opID, handle string, priorities []string,
		ifMatch *uint64, authority tracker.PersonAuthority) (tracker.WriteResult, error)
	RemoveTask(ctx context.Context, opID, id, project string, subtree bool,
		notify *tracker.Notify) (tracker.WriteResult, error)
	RestoreTask(ctx context.Context, opID, id, project string, alone bool,
		notify *tracker.Notify) (tracker.WriteResult, error)
	PurgeTask(ctx context.Context, opID, id, project, reason string) (tracker.WriteResult, error)
}

// WorkSearcher is the tracker's ranked search over this node's corpus.
type WorkSearcher interface {
	Search(ctx context.Context, q tracker.SearchQuery) (tracker.SearchAnswer, error)
}

// PageReader is the knowledge base's read side.
type PageReader interface {
	List(ctx context.Context, f pages.Filter, fresh statelog.Freshness) (pages.Listing, error)
	Get(ctx context.Context, ref string, fresh statelog.Freshness) (pages.Detail, error)
	SkillPages(ctx context.Context, container string, fresh statelog.Freshness) ([]pages.Page, error)

	// The operator's reads: every container, what happened to a page or
	// a container, and one revision of a page's body.
	Containers(ctx context.Context, fresh statelog.Freshness) ([]pages.ContainerListing, error)
	Activity(ctx context.Context, q pages.PageActivityQuery) (pages.PageActivity, error)
	Revision(ctx context.Context, pageID string, version int,
		fresh statelog.Freshness) (pages.Revision, bool, error)
}

// PageWriter is the knowledge base's write side. The author is an argument
// of every call rather than a derived writer, which is the pages store's own
// shape — and so is the caller's operation key ([pages.Actor.OpKey]), which
// rides on that author.
type PageWriter interface {
	Create(ctx context.Context, actor pages.Actor, in pages.NewPage) (pages.Written, error)
	SavePage(ctx context.Context, actor pages.Actor, pageID string, save pages.Save) (pages.Written, error)
	Rename(ctx context.Context, actor pages.Actor, pageID, title string,
		quiet bool) (pages.Written, error)
	Comment(ctx context.Context, actor pages.Actor, pageID string,
		in pages.NewComment) (pages.Comment, pages.Written, error)
	EditComment(ctx context.Context, actor pages.Actor, pageID, commentID,
		body string) (pages.Comment, pages.Written, error)

	// The operator's writes, which no seat is handed: the trash, the purge
	// and a remark's take-down.
	Trash(ctx context.Context, actor pages.Actor, pageID string) (pages.Written, error)
	Restore(ctx context.Context, actor pages.Actor, pageID string) (pages.Written, error)
	Purge(ctx context.Context, actor pages.Actor, pageID, reason string) (pages.Written, error)
	RemoveComment(ctx context.Context, actor pages.Actor, pageID, commentID string,
		authority pages.CommentAuthority) (pages.Written, error)
}

// KnowledgeSearcher is the native knowledge search over this node's corpus,
// with the one question a caller asks of an empty answer.
//
// ANSWER, NOT THE BEST-EFFORT SEARCH: a search that could not run is an error
// here ([pages.Searcher.Answer]), because the asking node has to tell "the
// knowledge base could not be searched" from "nothing matched" — and an empty
// answer carried back over the wire reads as the second.
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

// UsageReader is the usage domain's read side (ADR-0020): what each node's
// seats, schedules and people did each company day — the spend, activity and
// skill-reader answers the operator's surfaces draw.
//
// EVERY READ THAT GROWS WITH A WINDOW IS ASKED A PAGE AT A TIME, resuming
// after the group the page before it ended on and cut by [usage.Fill] at a
// budget — [usagePageBytes] for an answer that crosses the wire, none for one
// this node gives itself ([Backend.pageBytes]): a ninety-day window of a
// company's spend is tens of megabytes of rows, and a reply crosses one
// message. The schedule runs are already a page the caller sizes, and the
// elided count is a number.
type UsageReader interface {
	SpendPage(ctx context.Context, q usage.SpendQuery, after usage.Group, budget int) (usage.Page[usage.SpendRow], error)
	SeatTurnsPage(ctx context.Context, q usage.SpendQuery, after usage.Group, budget int) (usage.Page[usage.TurnsRow], error)
	PersonSpendPage(ctx context.Context, q usage.PersonQuery, after usage.Group, budget int) (usage.Page[usage.PersonRow], error)
	SeatDaysPage(ctx context.Context, q usage.SpendQuery, after usage.Group, budget int) (usage.Page[usage.SeatDay], error)
	PageReadsPage(ctx context.Context, q usage.PageReadsQuery, after usage.Group, budget int) (usage.Page[usage.PageRead], error)
	ScheduleRuns(ctx context.Context, q usage.ScheduleRunsQuery) ([]usage.ScheduleRun, bool, error)
	ReadsElided(ctx context.Context, from, to string) (int64, error)
	RepoReadsPage(ctx context.Context, q usage.RepoReadsQuery, after usage.Group, budget int) (usage.Page[usage.RepoReader], error)
	RepoReadsElided(ctx context.Context, q usage.RepoReadsQuery) (int64, error)
}

// IdentityReader is the identity directory's read side, as a node holding no
// data asks it: the two questions its seats and its party registry put to the
// directory — whose record a login names, and who holds the seats, every one
// for the registry and ONE for a turn composing the person it acts for — the
// one a request presenting a machine token puts, and no others.
//
// FACTS AND NOT VERDICTS, exactly as [iamdomain.Reader] answers them: the row a
// login binds and how far the serving node can vouch for it, the seats the
// directory binds, and the row a machine token is checked against. What a
// stalled applier or a retained record MEANS — and the chart a binding is
// resolved against, the Tier A tokens a node declares, and whether a token is
// good — are the ASKING node's, because the request path that decides on them
// is there and a verdict formed on another node would be a second opinion
// about whose seat a login is, or about whose credential a bearer is.
type IdentityReader interface {
	PersonByLoginVouched(ctx context.Context, login string) (iamdomain.Sighting,
		iamdomain.Vouch, error)
	SeatHolders(ctx context.Context) ([]iamdomain.SeatHolder, error)
	SeatHolder(ctx context.Context, seat string) (iamdomain.SeatHolder, bool, error)
	MachineToken(ctx context.Context, id string) (credential.TokenRow, error)
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
	Backlinks  BacklinkReader
	Usage      UsageReader

	// Code is this node's code half — its committer and its reader
	// ([code.Served]) — or nil where the company hosts no code: every code
	// operation is then answered "no backend here".
	Code CodeServer

	// Git is this node's git half — its query server — set by the
	// dispatcher for a GIT operation alone ([GitCopy]: the router's
	// in-process run and the server's git arm) and nil on every backend a
	// copy hands out: a git operation reads no domain's copy.
	Git GitServer

	// Identity is the identity directory, which every data node holds from
	// boot whatever its company keeps — nil only where the node's core
	// runtime is not up yet, which is answered like any half it does not
	// run.
	Identity IdentityReader

	// Artifacts is the artifacts estate, which every data node runs from
	// boot whatever its company keeps: its reader and its writer, each nil
	// only where the node's core runtime is not up yet ([ArtifactsHalf]).
	Artifacts ArtifactsHalf

	// ArtifactData is the artifact datastore, which every data node runs
	// from boot beside the artifacts estate ([ArtifactDataHalf]).
	ArtifactData ArtifactDataHalf

	// Reached waits until this node's applier of the floor's log has reached
	// a position — the floor every request carries, the node's own included:
	// every record through it that writes a row committed, and every barrier
	// decoded ([statelog.Runner.WaitReached]). A floor may be a barrier — a
	// read answers with the position its rows are current through — and a
	// barrier's own commit is nothing the request reads.
	Reached func(ctx context.Context, at statelog.Position) error

	// Answers is whether this copy OF THE OPERATION'S DOMAIN may answer a
	// request now ([statelog.Health.Answers]): serving — drained since its
	// applier started and within the snapshot slack of its log's end — or
	// level this instant. A copy that does not LAGS: it is asked only once
	// every data node whose copy does not has run nothing, by a remote asker
	// and by this node's own router alike — a worse node to ask, never no
	// node ([Router.route]). A lagging knowledge base never holds back a
	// tracker read (ADR-0049), for the reason a floor on another domain's log
	// is not waited for ([ready]).
	//
	// NOT A SEAT'S ADMISSION, which refuses at a lag of one: a busy
	// company has a record in flight on most instants, and a request gate
	// that refused then sent every read — this node's own seats' included —
	// to whichever peer happened to be level, or to none. What a read must
	// see of its own node's writes is what the floors carry.
	Answers func(ctx context.Context) bool

	// Admits is whether a SEAT may attach to this copy now: established at
	// a lag of zero on every domain that gates seat admission — the gate
	// every node's seat admission asks of the copy that will serve it
	// ([Router.Serves]). Only admission's ping reads it.
	Admits func(ctx context.Context) bool

	// Current is whether this node serves the company's configuration —
	// false exactly while the control plane has CONFIRMED it behind the
	// epoch the fleet activated (shed or stuck), never on lag alone, which
	// every rollout produces on every node but the first to apply. Nil is
	// current.
	//
	// A node behind it is a WORSE NODE TO ASK, never no node, as a copy
	// that lags its log is ([ready]): what it supplies from its own epoch
	// — the chart a tracker write resolves a person and a lead by, a
	// unit a read renders, a search's scope, the code limits a push is
	// decided under — is the configuration the fleet replaced, so every
	// other data node is asked first and this one answers when none of
	// them does; and since that lasts, an asker that has heard it once
	// remembers it ([unservedBehindConfig], [Router.markBehindConfig])
	// rather than send the node a refusal's round trip on every request.
	// Refused instead, a fleet whose only data node failed an
	// apply would answer none of its tracker, pages or usage operations,
	// although the stale answer is one that arises anyway: every reader of
	// a handle, a lead or a unit already meets one a revision has changed.
	//
	// THE CONFIRMATION IS THE CONTROL PLANE'S, and its bound is about a
	// minute: confirmed configplane.LagGrace after the node first reads the
	// new pointer — or sooner, where its own apply fails while a peer has
	// applied — and moved at the node's next posture decision.
	Current func() bool

	// ServerSeams are the node's own: the dispatcher sets them on every
	// backend it hands an operation.
	ServerSeams

	// verifiers are the keys a machine token's narrowed answer is MAC'd
	// under ([TokenFacts]) — the node's own, from its envelopes, which the
	// dispatcher sets on every backend it hands an operation: the server for
	// a request off the wire, this node's router for one it runs in-process.
	// A NODE'S, never its copy's, as [ServerSeams] are, and set by the
	// dispatcher for the same reason: a backend is resolved per request, the
	// keyring once per process.
	verifiers verifierKeys

	// pageBytes is the most one answer of a PAGED read carries before it is
	// cut ([usage.Fill]): [usagePageBytes] where the serving half answers a
	// request off the wire, and zero — one page, the whole read — where
	// this node's own router runs the operation in-process. Only the server
	// sets it.
	//
	// THE IN-PROCESS ANSWER IS NOT PAGED because nothing carries it: paged,
	// this node's own spend screen was N read transactions instead of one, a
	// JSON encoding of every row to measure a page no message would hold,
	// and N snapshots of the estate where the whole read is one. A failover
	// gains nothing from it either — an in-process read that fails before it
	// lands is asked of a peer from its first page.
	pageBytes int
}

// ServerSeams are what an operation reads of the SERVING node rather than of
// its copy: the chart seams a query or a dependency consults and the org a
// knowledge search is scoped by — the node's current epoch, which is what the
// package doc says crosses no wire — and the node's own event log.
type ServerSeams struct {
	// Units is attached to every tracker read that renders a project's
	// unit, and Leads is handed to a dependency.
	Units tracker.Units
	Leads tracker.Leads

	// Seat resolves a searching seat's handle to its role and the org its
	// read scope comes from, against this node's current epoch.
	Seat func(handle string) (*org.Role, *org.Organization)
}

// errNoHalf is an operation on a half this node does not run.
var errNoHalf = errors.New("estate: this node runs no native backend for this operation")

// declined is a half that RAN NOTHING for a reason of its own — a code
// committer that refused before it appended anything ([ranNothing]), or a node
// holding no key a machine token's answer could be MAC'd under
// ([opIdentityMachineToken]): read as errNoHalf by the router and the server
// alike, so every class moves on from it with the same arguments, and worded
// as the half's own refusal, which the walk's account keeps
// ([ranNothingDetail]).
type declined struct{ cause error }

func (d declined) Error() string   { return d.cause.Error() }
func (d declined) Unwrap() []error { return []error{errNoHalf, d.cause} }

// ranNothingDetail is what a node's answer says of a half that ran nothing:
// that the node runs none, or — for a half that declined — the half's own
// reason, so an estate nobody served names why each node passed (a committer
// stopping, a node that publishes nothing) rather than claiming it has no
// backend.
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

// SeatCopy is the domain an operation names when what it asks about is the
// copy a SEAT is served by, rather than one domain's: every domain whose health
// gates seat admission ([statelog.Domain.GatesSeatAdmission]) in service at
// once (ADR-0049). Admission's ping names it ([opPing]), and so does
// [Router.Serves].
//
// EMPTY, and no operation that reads a domain's rows may name it: the gate over
// the registry holds every other operation to a domain this package knows
// (TestEveryOperationNamesTheCopyItReads).
const SeatCopy = ""

// The domains an operation may read, and each one's log — the one its floor is
// on unless it is floorless ([op.floorless]), from the domain's own
// declaration. An operation's floor is on its OWN domain's log and no other:
// see [ready].
var (
	trackerDomain      = tracker.Domain{}.Name()
	pagesDomain        = pages.Domain{}.Name()
	usageDomain        = usage.Domain{}.Name()
	codeDomain         = code.Domain{}.Name()
	identityDomain     = iamdomain.Domain{}.Name()
	artifactsDomain    = artifactsdomain.Domain{}.Name()
	artifactDataDomain = artifactdata.Domain{}.Name()

	floorStreams = map[string]string{
		trackerDomain:      tracker.Domain{}.Stream().Name,
		pagesDomain:        pages.Domain{}.Stream().Name,
		usageDomain:        usage.Domain{}.Stream().Name,
		codeDomain:         code.Domain{}.Stream().Name,
		identityDomain:     iamdomain.Domain{}.Stream().Name,
		artifactsDomain:    artifactsdomain.Domain{}.Stream().Name,
		artifactDataDomain: artifactdata.Domain{}.Stream().Name,
	}
)

// ---- the tracker's reads ------------------------------------------------ //

type tasksArgs struct {
	Query tracker.Query
	Now   time.Time
}

var opTasks = define("tracker.tasks", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a tasksArgs) (tracker.Answer, error) {
		if b.Tracker == nil {
			return tracker.Answer{}, errNoHalf
		}
		a.Query.Units = b.Units
		return b.Tracker.Tasks(ctx, a.Query, a.Now)
	})

type taskArgs struct {
	IDOrKey string
	Want    tracker.DetailWants
	Fresh   statelog.Freshness

	// Zone is the name of the zone the asker's day clock is on
	// ([tracker.DayClock.Zone], [zoneArg]).
	Zone string
}

var opTask = define("tracker.task", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a taskArgs) (tracker.TaskDetail, error) {
		if b.Tracker == nil {
			return tracker.TaskDetail{}, errNoHalf
		}
		if a.Want.Clock != nil {
			loc, err := zoneOf(a.Zone)
			if err != nil {
				return tracker.TaskDetail{}, err
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
	Query tracker.ViewQuery
	Zone  string
}

var opViews = define("tracker.views", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a viewsArgs) (tracker.ViewListing, error) {
		if b.Tracker == nil {
			return tracker.ViewListing{}, errNoHalf
		}
		loc, err := zoneOf(a.Zone)
		if err != nil {
			return tracker.ViewListing{}, err
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
	Viewer tracker.Viewer
	Now    time.Time

	// Zone is the location's name ([zoneArg]).
	Zone string
}

var opExpandedQuery = define("tracker.expanded_query", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a expandArgs) (tracker.Query, error) {
		if b.Tracker == nil {
			return tracker.Query{}, errNoHalf
		}
		loc, err := zoneOf(a.Zone)
		if err != nil {
			return tracker.Query{}, err
		}
		return b.Tracker.ExpandedQuery(ctx, a.Params, a.Viewer, a.Now, loc)
	})

var opCatalogue = define("tracker.catalogue", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q tracker.CatalogueQuery) (tracker.CatalogueAnswer, error) {
		if b.Tracker == nil {
			return tracker.CatalogueAnswer{}, errNoHalf
		}
		return b.Tracker.Catalogue(ctx, q)
	})

type personArgs struct {
	Query tracker.PersonQuery
	Now   time.Time
}

var opPerson = define("tracker.person", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a personArgs) (tracker.PersonState, error) {
		if b.Tracker == nil {
			return tracker.PersonState{}, errNoHalf
		}
		return b.Tracker.Person(ctx, a.Query, a.Now)
	})

type threadArgs struct {
	Query tracker.ThreadQuery
	Fresh statelog.Freshness
}

var opThread = define("tracker.thread", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a threadArgs) (tracker.ResolvedThread, error) {
		if b.Tracker == nil {
			return tracker.ResolvedThread{}, errNoHalf
		}
		return b.Tracker.Thread(ctx, a.Query, a.Fresh)
	})

type activityArgs struct {
	Query tracker.ActivityQuery
	Now   time.Time
}

var opActivity = define("tracker.activity", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a activityArgs) (tracker.ActivityAnswer, error) {
		if b.Tracker == nil {
			return tracker.ActivityAnswer{}, errNoHalf
		}
		return b.Tracker.Activity(ctx, a.Query, a.Now)
	})

type myWorkArgs struct {
	Query tracker.MyWorkQuery
	Now   time.Time

	// Zone is the company clock the asker cut "today" on ([zoneArg]).
	Zone string
}

var opMyWork = define("tracker.my_work", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a myWorkArgs) (tracker.MyWork, error) {
		if b.Tracker == nil {
			return tracker.MyWork{}, errNoHalf
		}
		loc, err := zoneOf(a.Zone)
		if err != nil {
			return tracker.MyWork{}, err
		}
		return b.Tracker.MyWork(ctx, a.Query, a.Now, loc)
	})

var opProjects = define("tracker.projects", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q tracker.ProjectQuery) (tracker.ProjectListing, error) {
		if b.Tracker == nil {
			return tracker.ProjectListing{}, errNoHalf
		}
		q.Units = b.Units
		return b.Tracker.Projects(ctx, q)
	})

var opProject = define("tracker.project", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q tracker.ProjectDetailQuery) (tracker.ProjectDetail, error) {
		if b.Tracker == nil {
			return tracker.ProjectDetail{}, errNoHalf
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

// THE RANKED SEARCH READS THE INDEX OVER THE TRACKER'S ROWS, so it runs only on
// a copy whose tracker is in service — and it CARRIES NO FLOOR: the lexical
// index is maintained by each node's own walk on its own schedule behind the
// applier, so a floor on the log would promise a visibility the index does not
// give.
var opWorkSearch = define("tracker.search", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a workSearchArgs) (tracker.SearchAnswer, error) {
		if b.WorkSearch == nil {
			return tracker.SearchAnswer{}, errNoHalf
		}
		return b.WorkSearch.Search(ctx, tracker.SearchQuery{Text: a.Text, Limit: a.Limit, Mode: a.Mode})
	}).floorless()

type workloadArgs struct {
	Query tracker.WorkloadQuery
	Now   time.Time

	// Zone is the company clock the asker cut its days on ([zoneArg]).
	Zone string
}

var opWorkload = define("tracker.workload", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a workloadArgs) (tracker.WorkloadAnswer, error) {
		if b.Tracker == nil {
			return tracker.WorkloadAnswer{}, errNoHalf
		}
		loc, err := zoneOf(a.Zone)
		if err != nil {
			return tracker.WorkloadAnswer{}, err
		}
		a.Query.Units = b.Units
		return b.Tracker.Workload(ctx, a.Query, a.Now, loc)
	})

type inboxArgs struct {
	Query tracker.InboxQuery
	Now   time.Time
}

// opInbox is named `read_inbox`, beside `write_inbox`, never `tracker.inbox`:
// an operation's name is not a subject, but that one is spelled like a seat's
// inbox subject, and the guard that keeps every subject in internal/queue/topics
// rightly reads any literal ending `.inbox` as one written by hand.
var opInbox = define("tracker.read_inbox", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a inboxArgs) (tracker.InboxAnswer, error) {
		if b.Tracker == nil {
			return tracker.InboxAnswer{}, errNoHalf
		}
		return b.Tracker.Inbox(ctx, a.Query, a.Now)
	})

type routingArgs struct {
	Query tracker.RoutingQuery
	Now   time.Time
}

var opRouting = define("tracker.routing", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a routingArgs) (tracker.RoutingAnswer, error) {
		if b.Tracker == nil {
			return tracker.RoutingAnswer{}, errNoHalf
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
	func(ctx context.Context, b Backend, _ *Actor, a turnsOfArgs) (tracker.TaskTurns, error) {
		if b.Tracker == nil {
			return tracker.TaskTurns{}, errNoHalf
		}
		return b.Tracker.TurnsOf(ctx, a.IDOrKey, a.Cursor, a.Limit, a.Fresh)
	})

// everyViewArgs is [viewsArgs] for every saved view.
type everyViewArgs struct {
	Query tracker.EveryViewQuery
	Zone  string
}

var opEveryView = define("tracker.every_view", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a everyViewArgs) (tracker.ViewListing, error) {
		if b.Tracker == nil {
			return tracker.ViewListing{}, errNoHalf
		}
		loc, err := zoneOf(a.Zone)
		if err != nil {
			return tracker.ViewListing{}, err
		}
		q := a.Query
		q.Units, q.Zone = b.Units, loc
		return b.Tracker.EveryView(ctx, q)
	})

type flowArgs struct {
	Query tracker.FlowQuery
	Now   time.Time

	// Zone is the company clock the asker cut its days on ([zoneArg]).
	Zone string
}

var opFlow = define("tracker.flow", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a flowArgs) (tracker.FlowAnswer, error) {
		if b.Tracker == nil {
			return tracker.FlowAnswer{}, errNoHalf
		}
		loc, err := zoneOf(a.Zone)
		if err != nil {
			return tracker.FlowAnswer{}, err
		}
		return b.Tracker.Flow(ctx, a.Query, a.Now, loc)
	})

var opCompanyFeed = define("tracker.company_feed", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q tracker.FeedQuery) (tracker.FeedPage, error) {
		if b.Tracker == nil {
			return tracker.FeedPage{}, errNoHalf
		}
		return b.Tracker.CompanyFeed(ctx, q)
	})

type decisionsArgs struct {
	Query tracker.DecisionsQuery
	Now   time.Time

	// Zone is the company clock the asker cut "overdue" on ([zoneArg]).
	Zone string
}

var opDecisions = define("tracker.decisions", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a decisionsArgs) (tracker.DecisionsAnswer, error) {
		if b.Tracker == nil {
			return tracker.DecisionsAnswer{}, errNoHalf
		}
		loc, err := zoneOf(a.Zone)
		if err != nil {
			return tracker.DecisionsAnswer{}, err
		}
		return b.Tracker.Decisions(ctx, a.Query, a.Now, loc)
	})

type turnPlacesArgs struct {
	Runs  []string
	Fresh statelog.Freshness
}

var opTurnPlaces = define("tracker.turn_places", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a turnPlacesArgs) (map[string]tracker.TurnPlace, error) {
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
	func(ctx context.Context, b Backend, _ *Actor, q tracker.FileQuery) (tracker.FileListing, error) {
		if b.Tracker == nil {
			return tracker.FileListing{}, errNoHalf
		}
		return b.Tracker.Files(ctx, q)
	})

type fileArgs struct {
	Project string
	Path    string
	Fresh   statelog.Freshness
}

var opFile = define("tracker.read_file", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a fileArgs) (tracker.FileDetail, error) {
		if b.Tracker == nil {
			return tracker.FileDetail{}, errNoHalf
		}
		return b.Tracker.File(ctx, a.Project, a.Path, a.Fresh)
	})

type putFileArgs struct {
	OpID string
	Put  tracker.FilePut
}

var opPutFile = define("tracker.write_file", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a putFileArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
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
	func(ctx context.Context, b Backend, actor *Actor, a removeFileArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.RemoveFile(ctx, a.OpID, a.Project, a.Path, a.IfMatch)
	})

type recordTurnArgs struct {
	OpID string
	Turn tracker.TurnRecord
}

// A TURN'S SPEND, recorded by the node that ran the turn — which on a node
// without `data` is a node with no applier, so it crosses like any other
// write. It carries its operation id, so an unanswered one is asked again of
// the next node: the applier adds a turn's spend only for a new operation, and
// a repeat under the same id counts nothing twice.
var opRecordTurn = define("tracker.record_turn", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a recordTurnArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
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
	Task   tracker.Task
	Notify *tracker.Notify
}

var opCreateTask = define("tracker.create_task", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a createTaskArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.CreateTask(ctx, a.OpID, a.Task, a.Notify)
	})

type createTaskAskingArgs struct {
	OpID   string
	Task   tracker.Task
	Ask    tracker.Comment
	Notify *tracker.Notify
}

var opCreateTaskAsking = define("tracker.create_task_asking", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a createTaskAskingArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.CreateTaskAsking(ctx, a.OpID, a.Task, a.Ask, a.Notify)
	})

type placeTaskArgs struct {
	OpID   string
	Place  tracker.Place
	Notify *tracker.Notify
}

// placedTask is [TrackerWriter.PlaceTask]'s answer as it crosses: the result,
// with the reason the card changed lanes and did not take its place carried
// BESIDE it as a wire error — an error is an interface no encoder writes, and
// the tool that reads it classifies the refusal by its identity (a stale
// version, a conflict), which [encodeError] keeps.
type placedTask struct {
	Result   tracker.PlaceResult
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
	Patch     tracker.TaskPatch
	Watch     *tracker.WatchIntent
	Relate    *tracker.RelationIntent
	Depend    *tracker.DependentIntent
	Checklist *tracker.ChecklistIntent
	Kind      tracker.ChangeKind
	Notify    *tracker.Notify
}

var opUpdateTask = define("tracker.update_task", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a updateTaskArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		patch := a.Patch
		patch.Watch, patch.Relate, patch.Depend, patch.Checklist =
			a.Watch, a.Relate, a.Depend, a.Checklist
		return w.UpdateTask(ctx, a.OpID, a.ID, a.Project, a.IfMatch, patch, a.Kind, a.Notify)
	})

type dependArgs struct {
	OpID   string
	Change tracker.DependencyChange
}

var opDepend = define("tracker.depend", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a dependArgs) (tracker.DependencyResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.DependencyResult{}, err
		}
		return w.Depend(ctx, a.OpID, a.Change, b.Leads)
	})

type mergeArgs struct {
	OpID      string
	Duplicate string
	Into      string
	Reparent  bool
	Notify    *tracker.Notify
}

var opMergeDuplicates = define("tracker.merge_duplicates", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a mergeArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.MergeDuplicates(ctx, a.OpID, a.Duplicate, a.Into, a.Reparent, a.Notify)
	})

type moveArgs struct {
	OpID   string
	TaskID string
	Target string
	Notify *tracker.Notify
}

var opMoveTaskToProject = define("tracker.move_task_to_project", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a moveArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.MoveTaskToProject(ctx, a.OpID, a.TaskID, a.Target, a.Notify)
	})

type writeProjectArgs struct {
	OpID      string
	Key       string
	Edit      tracker.ProjectEdit
	Authority tracker.ProjectAuthority
}

var opWriteProject = define("tracker.write_project", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a writeProjectArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.WriteProject(ctx, a.OpID, a.Key, a.Edit, a.Authority)
	})

type writeTagsArgs struct {
	OpID      string
	Project   string
	Edit      tracker.TagEdit
	Authority tracker.TagAuthority
}

var opWriteTags = define("tracker.write_tags", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a writeTagsArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.WriteTags(ctx, a.OpID, a.Project, a.Edit, a.Authority)
	})

type ensureTagsArgs struct {
	OpID    string
	Project string
	Tags    []string
}

// ensuredTags is [TrackerWriter.EnsureTags]' two answers, named.
type ensuredTags struct {
	Created  []string
	Warnings []string
}

var opEnsureTags = define("tracker.ensure_tags", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a ensureTagsArgs) (ensuredTags, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return ensuredTags{}, err
		}
		created, warnings, err := w.EnsureTags(ctx, a.OpID, a.Project, a.Tags)
		return ensuredTags{Created: created, Warnings: warnings}, err
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
	func(ctx context.Context, b Backend, actor *Actor, a viewPriorArgs) (tracker.ViewPrior, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.ViewPrior{}, err
		}
		return w.ViewPrior(ctx, a.ID)
	})

type writeViewArgs struct {
	OpID  string
	View  tracker.View
	Prior tracker.ViewPrior
}

var opWriteView = define("tracker.write_view", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a writeViewArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.WriteView(ctx, a.OpID, a.View, a.Prior)
	})

type writeTypesArgs struct {
	OpID  string
	Types []tracker.TaskType
}

var opWriteTypes = define("tracker.write_types", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a writeTypesArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.WriteTypes(ctx, a.OpID, a.Types)
	})

type writeFieldsArgs struct {
	OpID   string
	Fields []tracker.FieldDef
}

var opWriteFields = define("tracker.write_fields", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a writeFieldsArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.WriteFields(ctx, a.OpID, a.Fields)
	})

type markInboxArgs struct {
	OpID      string
	Handle    string
	Gesture   inbox.Gesture[tracker.Reason]
	Authority tracker.PersonAuthority
}

// A GESTURE, never the lists: the tracker resolves it against the record its
// own decide reads, so a caller never sends back a list it read in another
// transaction — on whichever holder answers.
var opMarkInbox = define("tracker.mark_inbox", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a markInboxArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.MarkInbox(ctx, a.OpID, a.Handle, a.Gesture, a.Authority)
	})

type writePinsArgs struct {
	OpID      string
	Handle    string
	Gesture   tracker.PinGesture
	Authority tracker.PersonAuthority
}

var opWritePins = define("tracker.write_pins", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a writePinsArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.WritePins(ctx, a.OpID, a.Handle, a.Gesture, a.Authority)
	})

type writePrioritiesArgs struct {
	OpID       string
	Handle     string
	Priorities []string
	IfMatch    *uint64
	Authority  tracker.PersonAuthority
}

var opWritePriorities = define("tracker.write_priorities", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a writePrioritiesArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.WritePriorities(ctx, a.OpID, a.Handle, a.Priorities, a.IfMatch, a.Authority)
	})

type editCommentOnTaskArgs struct {
	OpID      string
	TaskID    string
	Project   string
	CommentID string
	Body      string
	Notify    *tracker.Notify
}

// REWRITING ONE'S OWN REMARK on a work item. Its author is decided in the
// decide's own snapshot on the serving node ([tracker.ErrNotAuthor]), against
// the actor this request names — which is why the actor is the SIGNED one
// (see [Envelopes]): an edit whose author a request could choose would put
// words in somebody else's mouth.
var opEditTaskComment = define("tracker.edit_comment", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a editCommentOnTaskArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.EditComment(ctx, a.OpID, a.TaskID, a.Project, a.CommentID, a.Body, a.Notify)
	})

type removeTaskArgs struct {
	OpID    string
	ID      string
	Project string
	Subtree bool
	Notify  *tracker.Notify
}

var opRemoveTask = define("tracker.remove_task", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a removeTaskArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.RemoveTask(ctx, a.OpID, a.ID, a.Project, a.Subtree, a.Notify)
	})

type restoreTaskArgs struct {
	OpID    string
	ID      string
	Project string
	Alone   bool
	Notify  *tracker.Notify
}

var opRestoreTask = define("tracker.restore_task", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a restoreTaskArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
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
	func(ctx context.Context, b Backend, actor *Actor, a purgeTaskArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.PurgeTask(ctx, a.OpID, a.ID, a.Project, a.Reason)
	})

// ---- the knowledge base -------------------------------------------------- //

type listPagesArgs struct {
	Filter pages.Filter
	Fresh  statelog.Freshness
}

var opListPages = define("pages.list", opRead, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a listPagesArgs) (pages.Listing, error) {
		if b.Pages == nil {
			return pages.Listing{}, errNoHalf
		}
		return b.Pages.List(ctx, a.Filter, a.Fresh)
	})

type getPageArgs struct {
	Ref   string
	Fresh statelog.Freshness
}

var opGetPage = define("pages.get", opRead, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a getPageArgs) (pages.Detail, error) {
		if b.Pages == nil {
			return pages.Detail{}, errNoHalf
		}
		return b.Pages.Get(ctx, a.Ref, a.Fresh)
	})

type skillPagesArgs struct {
	Container string
	Fresh     statelog.Freshness
}

var opSkillPages = define("pages.skill_pages", opRead, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a skillPagesArgs) ([]pages.Page, error) {
		if b.Pages == nil {
			return nil, errNoHalf
		}
		return b.Pages.SkillPages(ctx, a.Container, a.Fresh)
	})

type containersArgs struct {
	Fresh statelog.Freshness
}

var opContainers = define("pages.containers", opRead, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a containersArgs) ([]pages.ContainerListing, error) {
		if b.Pages == nil {
			return nil, errNoHalf
		}
		return b.Pages.Containers(ctx, a.Fresh)
	})

// A PAGE'S ACTIVITY, a container's, or the whole knowledge base's.
var opPageActivity = define("pages.activity", opRead, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q pages.PageActivityQuery) (pages.PageActivity, error) {
		if b.Pages == nil {
			return pages.PageActivity{}, errNoHalf
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
	Revision pages.Revision
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
// carries the caller's own operation key ([pages.Actor.OpKey]), from which the
// pages store derives the operation id — bound to what the write says — and a
// create its page's id, so a repeat under it is the same operation.
func keyed(actor pages.Actor) bool { return strings.TrimSpace(actor.OpKey) != "" }

type createPageArgs struct {
	Actor pages.Actor
	Page  pages.NewPage
}

var opCreatePage = define("pages.create", opOnceWrite, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a createPageArgs) (pages.Written, error) {
		if b.PageWriter == nil {
			return pages.Written{}, errNoHalf
		}
		return b.PageWriter.Create(ctx, a.Actor, a.Page)
	}).repeatableWhen(func(a createPageArgs) bool { return keyed(a.Actor) })

type savePageArgs struct {
	Actor  pages.Actor
	PageID string
	Save   pages.Save
}

var opSavePage = define("pages.save", opOnceWrite, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a savePageArgs) (pages.Written, error) {
		if b.PageWriter == nil {
			return pages.Written{}, errNoHalf
		}
		return b.PageWriter.SavePage(ctx, a.Actor, a.PageID, a.Save)
	}).repeatableWhen(func(a savePageArgs) bool { return keyed(a.Actor) })

type renamePageArgs struct {
	Actor  pages.Actor
	PageID string
	Title  string
	Quiet  bool
}

var opRenamePage = define("pages.rename", opOnceWrite, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a renamePageArgs) (pages.Written, error) {
		if b.PageWriter == nil {
			return pages.Written{}, errNoHalf
		}
		return b.PageWriter.Rename(ctx, a.Actor, a.PageID, a.Title, a.Quiet)
	}).repeatableWhen(func(a renamePageArgs) bool { return keyed(a.Actor) })

type commentArgs struct {
	Actor   pages.Actor
	PageID  string
	Comment pages.NewComment
}

// commented is a comment write's two answers, named.
type commented struct {
	Comment pages.Comment
	Written pages.Written
}

// A COMMENT HAS TWO KEYS, and the turn's wins: a remark made from a turn
// derives its id from the turn's unit of work ([pages.NewComment.TurnKey]) so
// a re-run posts once, and one made by a person from the caller's own
// operation key — the order the pages store reads them in.
var opCommentPage = define("pages.comment", opOnceWrite, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a commentArgs) (commented, error) {
		if b.PageWriter == nil {
			return commented{}, errNoHalf
		}
		c, w, err := b.PageWriter.Comment(ctx, a.Actor, a.PageID, a.Comment)
		return commented{Comment: c, Written: w}, err
	}).repeatableWhen(func(a commentArgs) bool {
	return strings.TrimSpace(a.Comment.TurnKey) != "" || keyed(a.Actor)
})

type editCommentArgs struct {
	Actor     pages.Actor
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
	Actor  pages.Actor
	PageID string
}

var opTrashPage = define("pages.trash", opOnceWrite, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a pageRefArgs) (pages.Written, error) {
		if b.PageWriter == nil {
			return pages.Written{}, errNoHalf
		}
		return b.PageWriter.Trash(ctx, a.Actor, a.PageID)
	}).repeatableWhen(func(a pageRefArgs) bool { return keyed(a.Actor) })

var opRestorePage = define("pages.restore", opOnceWrite, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a pageRefArgs) (pages.Written, error) {
		if b.PageWriter == nil {
			return pages.Written{}, errNoHalf
		}
		return b.PageWriter.Restore(ctx, a.Actor, a.PageID)
	}).repeatableWhen(func(a pageRefArgs) bool { return keyed(a.Actor) })

type purgePageArgs struct {
	Actor  pages.Actor
	PageID string
	Reason string
}

var opPurgePage = define("pages.purge", opOnceWrite, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a purgePageArgs) (pages.Written, error) {
		if b.PageWriter == nil {
			return pages.Written{}, errNoHalf
		}
		return b.PageWriter.Purge(ctx, a.Actor, a.PageID, a.Reason)
	}).repeatableWhen(func(a purgePageArgs) bool { return keyed(a.Actor) })

type removeCommentArgs struct {
	Actor     pages.Actor
	PageID    string
	CommentID string

	// Authority is the caller's answer from the authority table — whether
	// it may take down a remark it did not write — decided on the asking
	// node, which holds the grants, and judged against the remark's author
	// in the serving node's own snapshot.
	Authority pages.CommentAuthority
}

var opRemoveComment = define("pages.remove_comment", opOnceWrite, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a removeCommentArgs) (pages.Written, error) {
		if b.PageWriter == nil {
			return pages.Written{}, errNoHalf
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
// usage copy is out of service answers its operator's spend and activity
// screens from a peer's copy rather than from the one it stopped serving
// (ADR-0049). FLOORLESS: no write this node makes lands on the usage log —
// every node publishes its own days from its own event log — so there is no
// position an operator's read must see, and the rows are compacted, one per
// object, each replaced whole.
//
// FIVE OF THEM ARE PAGED (see [UsageReader]) when a PEER answers them: each
// reply is one page of the read, and the asker asks again after the group it
// ended on ([allPages]). This node's own answer is the whole read, one page
// ([Backend.pageBytes]).

// usagePageBytes is the most a page of a routed usage read carries before it
// is cut at the next record's edge: A QUARTER OF THE MESSAGE
// ([queue.MaxPayloadBytes]). A page may run past it by one group — one
// record's rows, never split (usage.Group) — and a record is itself one
// message, measured under 1 MiB at its caps
// (usage.TestAFullSeatDayRecordFitsOneMessage); re-encoded as read rows each
// carrying its group's key it is at most a few times that, which the other
// three quarters hold (TestAUsagePageFitsOneMessage). A reply past the message
// is refused rather than dropped ([server.encodeReply]), so a pathological group
// fails loudly.
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

// repoReadsArgs is one page of a repository's readers: the read's query, the
// reader the page before it ended on — empty for the first — and the bytes its
// ASKER cuts a page at.
type repoReadsArgs struct {
	Query  usage.RepoReadsQuery
	After  usage.Group
	Budget int
}

// A REPOSITORY'S READERS ARE PAGED BY THEIR ASKER, unlike the reads above: a
// person walks them a page at a time through a surface that answers one page
// per request — a tool answer, a REST page — so the page is the asker's, at the
// budget it names, and the wire's own ([usagePageBytes]) bounds it only where
// it is the smaller.
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

// A PAGE'S BACKLINKS READ BOTH CORPORA — the pages and the tasks whose bodies
// link to it, named from the rows of each, a task's linked-page relations
// included — so they run only where this node's copies of the knowledge base
// AND the tracker are in service ([op.alsoReads], ADR-0049). Read straight off
// a node's own index, a node whose pages or tracker copy had stopped serving
// merged a peer's page with links derived from rows it no longer answered for,
// presented as complete. They carry no floor, for [opKnowledgeSearch]'s
// reason: the links are rows of an index no log position describes.
var opLinkedFrom = define("knowledge.linked_from", opRead, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a linkedFromArgs) (search.Backlinks, error) {
		if b.Backlinks == nil {
			return search.Backlinks{}, errNoHalf
		}
		return b.Backlinks.LinkedFrom(ctx, a.PageID)
	}).floorless().alsoReads(trackerDomain)

// ---- the identity directory ---------------------------------------------- //

// A node holding no data runs seats, and its seats ask the identity directory
// two things: whose record a login names, which every person verb resolves
// before it is decided ([iam.OwnerOf]), and who holds each human seat, which
// the party registry routes inbound messages by and an agent's roster is cut
// by. A node holding no data that serves requests asks a third: the row a
// presented machine token is checked against ([TokenFacts]). ADR-0025 gives
// such a node no replicated estate and no applier, so each is asked of a data
// node, floored on the identity log like any read of rows a log derives.

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

// seatArgs names the one seat a holder question is about.
type seatArgs struct {
	Seat string
}

// heldSeat is who holds one seat, and whether anybody does: the directory's
// three-valued answer with its error crossing apart, as every error does.
type heldSeat struct {
	Holder iamdomain.SeatHolder
	Held   bool
}

// WHO HOLDS ONE HUMAN SEAT — [opSeatHolders]' answer for that seat alone, by
// the same rule, read as two seeks on the serving node
// ([iamdomain.Reader.SeatHolder]). It is what a seat's turn composes the
// person it acts for from on every call of a tool that reads or writes for
// somebody, so it must cost the same however many people the company has hired
// or removed, where the whole set grows with both. An error is UNKNOWN and
// crosses as one, never "nobody holds it", which a turn would read as a seat
// the chart alone speaks for.
var opSeatHolder = define("identity.seat_holder", opRead, identityDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a seatArgs) (heldSeat, error) {
		if b.Identity == nil {
			return heldSeat{}, errNoHalf
		}
		holder, held, err := b.Identity.SeatHolder(ctx, a.Seat)
		if err != nil {
			return heldSeat{}, err
		}
		return heldSeat{Holder: holder, Held: held}, nil
	})

// WHAT A PRESENTED MACHINE TOKEN IS CHECKED AGAINST, read in one snapshot —
// the credential, its owner as this node holds them now, and how far this node
// can vouch for both — and answered NARROWED: the verifier withheld, and a MAC
// over it and the asker's nonce, under the key the asker named, in its place
// ([TokenFacts]). Whether the token is good is decided on the asking node,
// which holds the presented secret, by the one table every node decides a
// token by ([credential.CheckToken]). An error is UNKNOWN and crosses as one,
// never "no such token", which would be a 401 to a pipeline whose credential
// is fine.
//
// A KEY THIS NODE DOES NOT HOLD RUNS NOTHING, as a request signed under one
// does ([Envelopes]): it is a fact about this node, which an operator changes
// by adding the key, so the router asks a node that holds it — and it is
// checked before the row is read, so a node that could not MAC an answer
// reads nothing for it.
var opIdentityMachineToken = define("identity.machine_token", opRead, identityDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a machineTokenArgs) (TokenFacts, error) {
		if b.Identity == nil {
			return TokenFacts{}, errNoHalf
		}
		key, held := b.verifiers.key(a.KeyID)
		if !held {
			return TokenFacts{}, declined{cause: fmt.Errorf("it holds no secrets.keys "+
				"entry %q to answer a machine token's verifier under (it holds %v)",
				a.KeyID, b.verifiers.held())}
		}
		row, err := b.Identity.MachineToken(ctx, a.ID)
		if err != nil {
			return TokenFacts{}, err
		}
		return factsOf(row, key, a.Nonce), nil
	})

// served is what a node runs, as a seat's admission asks it.
type served struct {
	Tracker bool
	Pages   bool
}

// opPing is ADMISSION's question, asked of a data node: whether its copy
// admits a seat now ([Backend.Admits]), and which halves it runs natively. A
// copy that does not is passed over, like a node that ran nothing — see
// [Router.Serves].
//
// IT ASKS THE SEAT COPY ([SeatCopy]): a data node one of whose seat-gating
// domains is out of service answers `out_of_service`, and a wrong copy of any
// other domain withholds nothing. NO FLOOR, and past the serving gate on
// purpose: it asks whether the copy is ready for a seat, which is its own
// stricter gate, not whether it has reached this node's writes. And NO
// ARGUMENTS: the one estate is the only thing it can be about.
var opPing = define("estate.ping", opRead, SeatCopy, false,
	func(ctx context.Context, b Backend, _ *Actor, _ struct{}) (served, error) {
		if b.Admits != nil && !b.Admits(ctx) {
			return served{}, errNotAdmitting
		}
		return served{Tracker: b.Tracker != nil, Pages: b.Pages != nil}, nil
	}).floorless().ungated()
