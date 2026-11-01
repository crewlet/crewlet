package queries

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/usage"
	"github.com/crewlet/crewlet/internal/words"
)

var log = logging.Get("api.queries")

// Page sizes. Two numbers per listing, and both are load-bearing: the default
// is what a dashboard gets when it names none, and the ceiling is what stops
// one tab pulling the whole log through a process every other tab shares.
const (
	// DefaultEventPage matches what one screen of the activity feed shows.
	// Larger would spend a round trip on rows nobody scrolls to; smaller
	// would make the first scroll a second query.
	DefaultEventPage = 100

	// MaxEventPage bounds it. Chosen against the feed ring the projection
	// keeps — asking for more than the live buffer holds is a request the
	// store answers and the screen cannot use.
	MaxEventPage = livestate.EventFeedLimit
)

// Sources are what the answers read from.
//
// Every field is optional, and an absent one leaves its questions UNREGISTERED,
// so they come back as [ErrUnknown] (`unknown_query`, 404) rather than answered
// emptily: "there is no tracker here" and "the tracker is empty" are answers a
// screen must be able to tell apart, and "not here" is permanent: no wait and
// no other node's answer gives this node a source it was not configured with,
// so it is a question this node does not have rather than [ErrUnavailable]'s
// question it cannot answer here — whose `retry_after` says whether coming
// back helps, and a Retry-After here would send a client round a loop.
//
// Only a few are absent on a real node: Work and Pages on a company that runs
// the vendor tracker and wiki, Knowledge where no knowledge backend is
// configured, and Retention where no state log runs. The rest are held by every
// node, and the API wires every one of them. Leaving one of those out is what a
// caller that asks a subset of the questions does, which is how this package's
// own suite exercises one question at a time.
type Sources struct {
	State *livestate.LiveState

	// Events is the FLEET's turn-level history: every data node's own event
	// store, asked at query time, with a [eventfan.Coverage] beside every
	// answer saying which nodes it came from (ADR-0021).
	//
	// An interface every method of which answers a coverage, so a bare
	// *store.EventLog does not compile here: a read of this node's store
	// alone answered for a third of a three-node fleet and said nothing
	// about the other two, which is exactly the answer a screen must never
	// be handed without being told.
	Events FleetEvents

	// Usage is the replicated estate's usage domain: every data node's
	// company days, for every NAMED spend window and its series (ADR-0020).
	// Not this node's event log, which answered for this node alone and only
	// thirty days back. Nil leaves `token_series` unregistered and a named
	// `tokens` window empty.
	Usage usage.Estate

	// Company reads the CURRENT company, for the questions answered from
	// the company rather than from a store: the document this node applied
	// and the ORG it built from it.
	//
	// A function, not a value: a company is republished by every
	// activation, and an answer bound to the one this process booted on
	// would describe a company that is no longer running.
	//
	// ONE FUNCTION RETURNING BOTH, never two accessors: two reads can
	// straddle a publish — and a screen that took the integrations from
	// one and the roster from the next would describe a company that never
	// existed.
	Company func() (*config.Company, *org.Organization)

	// Chart answers the lead relations the personal questions are scoped
	// by, through the SAME seam internal/authz asks every other surface's
	// through — see [Sources.recordHandle]. Nil is a surface with no
	// chart, which [authz.NoChart] reports as UNKNOWN rather than as
	// "leads nobody": a 503 a caller retries, never a 403 that sends them
	// to ask for authority they already hold.
	//
	// internal/api REQUIRES it, because a node serving the API always has
	// a chart and the unknown answer never clears by waiting: `crewlet
	// run` built these sources without one, so every lead reading a
	// report's inbox, queue or pins was told "this node cannot say who
	// leads" for the life of the process, and the live socket's `watch`
	// decides by the same seam.
	Chart authz.Chart

	// Holders says whose record somebody else's LOGIN names — the identity
	// directory's half of [iam.OwnerOf], through which every personal
	// question resolves the `handle` it was asked about before it decides
	// or reads anything. See [Sources.recordHandle].
	//
	// internal/api REQUIRES it, for Chart's reason: a node serving the API
	// always runs the identity domain, and a nil here answers "this node
	// cannot say" to every question naming a login, for the life of the
	// process. Read literally instead — which is what these questions did
	// — a bound person's login named an empty record under the login,
	// while everything of theirs is kept under their seat.
	Holders iam.Holders

	// Acts names the operator catalogue's tools that WRITE which the
	// authority table can admit this principal to before any object is
	// named (internal/api/operator) — what `viewer` answers as `acts`, so
	// a screen enables exactly the controls a press of would be decided
	// for rather than discovering every refusal by pressing.
	//
	// ASKED OF THE PRINCIPAL, never of the transport: any principal the
	// guard resolved may call the act route, and what separates one
	// caller's buttons from another's is the table, not whether a seat is
	// bound. An ERROR is the table's UNKNOWN — this node could not decide
	// for this principal — and `viewer` refuses rather than answering an
	// empty list a screen would draw as "you may do nothing". Nil answers
	// none: it is a surface built with no operator catalogue, which mounts
	// no act route — never an engine node, whose catalogue serves the seat
	// controls whatever backends its company runs — so no press could
	// reach anything.
	Acts func(ctx context.Context, p iam.Principal) ([]string, error)

	// Assistant answers whether the Dashboard Assistant is available to this
	// principal on this node, and the first reason not
	// (internal/api/assistantapi's Surface.Viewer), which `viewer` answers as
	// `assistant`, so the Ask pill decides without a second request. NIL IS A
	// NODE THAT SERVES NO ASSISTANT, answered as `disabled`.
	Assistant func(ctx context.Context, p iam.Principal) wire.ViewerAssistant

	// WithheldContacts reads the identity directory's withholding as the
	// party registry holds it now — whether a human seat's contact
	// identities are withheld because the person bound to it may not be
	// reached — so `colleague` builds its corpus as a seat's own lookup
	// does ([builtin.Corpus]). A FUNCTION returning the check, read per
	// call for [Sources.Company]'s reason: the registry is rebuilt by a
	// directory change and by a publish.
	//
	// internal/api REQUIRES it, for Chart's reason: nil reads the chart
	// alone, which offers a suspended person as somebody to hand work to.
	WithheldContacts func() func(handle string) bool

	// Env resolves a `${VAR}` a seat's CONTACT IDS carry — a Slack user id,
	// a GitHub login — through the serving node's own chain: its secret
	// store, then the environment it was handed (the engine's
	// LookupSecret). `colleague` reports those ids, and a seat's own lookup
	// resolves them through the same chain ([builtin.Corpus]). Nil reads
	// the process environment.
	//
	// THE NODE'S CHAIN because the routing that mentions a person resolves
	// the same fields through it: an id sealed in the store, read here from
	// the process environment, was a person the screen showed no id for
	// while every delivery to them was routed by it. It resolves contact
	// ids and the browser addresses `link_hosts` reports, and nothing else:
	// who a caller IS is the identity directory's answer, never a field of
	// the chart.
	Env org.EnvLookup

	// CompanyWriters is Tier A's `api.auth.company_writers`: the ids of
	// the Tier A tokens that alone may change the company document while
	// it is managed (ADR-0030), or empty while it is not. `viewer` answers
	// from it whether this principal may change the document and who
	// manages it, through [config.APIAuth.MayWriteCompany] — the one
	// reading the config surface enforces it by — so a screen offers
	// exactly the edits that surface admits.
	CompanyWriters []string

	// SignIn is this node's sign-in posture, read from its Tier A
	// `api.auth` ([SignInPolicyOf]). Nil leaves `sign_in_policy`
	// unregistered; internal/api fills it from the Bootstrap it is handed.
	SignIn *SignInPolicy

	// Coord is the lease table: the fleet's one shared answer to "which
	// node holds what". Nil leaves the fleet question unregistered.
	Coord coord.Backend

	// Builtins is the engine's own tools as this node's seats are offered
	// them, as catalogue rows: `mcp_servers_status`'s built-in groups. Nil,
	// or a nil answer (a node with no active revision), sends no group.
	//
	// THIS NODE'S, and that is the fleet's answer: every node applies the
	// same revision, so the tools a seat is offered do not depend on the
	// node holding it.
	Builtins func() []ToolRow

	// Plane is the control plane, for the config columns of the fleet view.
	Plane coord.Plane

	// Objects is the fleet's record of the object store, for the fleet
	// view's file-storage card. Nil leaves the card out of the answer rather
	// than reporting a fleet whose collector never ran.
	Objects ObjectsReader

	// FleetBroker is the fleet broker's membership — what every live node
	// advertises against what the metadata group counts. Nil leaves the
	// question unregistered.
	FleetBroker BrokerLister

	// Runs is the schedule dispatch ledger. Nil still answers the
	// schedules question — the configured schedules are a projection of
	// the org — with an empty history, because "no ledger" and "nothing
	// has fired" are told apart by the answer's own shape.
	Runs ScheduleRuns

	// Memory is a seat's memory — its diary, episodes, the skills it drafted
	// and what it learned about the people it works with — and its
	// conversation ledger, each ANSWERED BY THE NODE HOLDING THE SEAT
	// (internal/learning/memread). Every node keeps a copy of every seat it
	// ever ran and only the holder keeps it current, so a read of whichever
	// node served the request described the seat as of the last time that
	// node ran it.
	//
	// HANDED THE SEAT WHOLE ([memread.Seat]: its agent id and its handle),
	// resolved here from the org the answer is made of — the reader never
	// resolves a handle itself, because the node that answers may hold a
	// different org from the node that asked. Nil leaves `agent_memory`,
	// `memory_overview` and `conversations` unregistered.
	Memory SeatMemory

	// Channels is the fleet's agent-to-agent authorization record. A
	// consumer-defined interface rather than the whole coord.Fleet: this
	// surface reads open channels and nothing else, and a source that could
	// reach the activation pointer would eventually be given a reason to.
	Channels interface {
		// TWO LISTINGS WITH OPPOSITE RULES — see the coordination
		// contract. The idle sweep's is open-only, because a closed
		// channel re-reported is a second close for one channel; a read
		// surface needs both, or the record the fleet keeps until the
		// purge horizon is one nothing can ever show.
		OpenChannels(ctx context.Context) ([]coord.Channel, error)
		AllChannels(ctx context.Context) ([]coord.Channel, error)
	}

	// Knowledge resolves the company's ONE knowledge backend, behind the
	// same seam a seat's own turn searches through, so an operator
	// asking "what would an agent find" gets the answer an agent would
	// get, rather than one from an index somebody has to keep fresh.
	//
	// A FUNCTION, not a value, for the reason [Sources.Company] is one: an
	// apply REPLACES the searcher. A credential rotation, a retired
	// integration block, a knowledge backend repaired after a failed boot
	// — each rebuilds it, and a value captured when the API was assembled
	// keeps searching with the credential that was revoked, against the
	// wiki that was removed, or answers "no backend" forever for a company
	// that has had one since its second minute. Nil, and a nil answer from
	// it, both mean the same thing and leave the question unregistered.
	Knowledge func() knowledge.Searcher

	// Budget is the DURABLE token counter — what the engine actually
	// enforces against, across every node and every restart. Nil answers
	// the budget surface with `durable: false` rather than zeros: "nobody
	// looked" and "nothing was spent" are different facts, and only one of
	// them is a measurement.
	//
	// A consumer-defined interface rather than the whole coord.Fleet: the
	// budget screen reads spend and nothing else, and a source that could
	// reach the activation pointer would eventually be given a reason to.
	Budget interface {
		Usage(ctx context.Context, windows coord.Windows) ([]coord.Usage, error)
	}

	// Sandbox is the durable record of detached executors, the fleet's
	// rather than this node's. Nil leaves the question unregistered.
	Sandbox PendingRuns

	// RunAnswers opens a run's recorded answer for who gave it, which only
	// the answer's seal says ([sandbox.AnswerSeal]). Nil, or a seal that
	// does not open here, leaves a row's `answered_by` empty.
	RunAnswers *sandbox.AnswerSeal

	// ExecutorTail is a running executor's live output, ANSWERED BY THE
	// NODE THAT OWNS THE RUN (internal/sandbox/livetail.go): the box is
	// reachable only through the node driving it, and what it has said so
	// far is on no record anywhere until the run is collected. Nil leaves
	// the question unregistered.
	ExecutorTail ExecutorTails

	// Config serves the config family, every one of which is admin's: the
	// document names every ${VAR} the company holds a credential under.
	Config *configapi.Service

	// Routed names the integrations whose deliveries can wake a seat, or
	// nil when this node cannot say: an engine mid-boot, or one with no
	// active revision, has not started its notification service yet. The
	// app populates it from its NodeRuntime; nil here is not an error and
	// not "none route", and the integrations answer keeps those three apart
	// rather than folding them into a boolean.
	Routed func(ctx context.Context) []string

	// Verifiable names the integrations whose resolved material could
	// accept a delivery, or nil when this process cannot say. Populated
	// from the same NodeRuntime as Routed, and kept apart from it for the
	// same reason: "would a delivery be verified" and "would a verified
	// delivery reach anyone" fail independently, and an operator staring at
	// a silent integration has to know which half broke.
	Verifiable func(ctx context.Context) []string

	// Reconciles is what the integration reconcile loop last found for
	// each surface, or nil when this process cannot say.
	//
	// The FLEET's record rather than this node's, and that is the whole
	// reason it is stored where it is: the loop is a worker duty, so on a
	// split-role deployment the node answering this request is never the
	// one that wrote it.
	//
	// Nil is "cannot say" and an empty slice is "nothing has been
	// reconciled", exactly as with Routed and Verifiable above. The
	// integrations answer keeps the two apart rather than folding a
	// coordination store that could not be read into a claim that every
	// surface is unchecked.
	Reconciles func(ctx context.Context) []integration.State

	// Converges reports whether a reconcile pass converges a surface in
	// this build, or is nil when this process cannot say.
	//
	// The one fact the tool roll-up needs that no row carries: a surface no
	// pass converges (Slack, whose apps are created by hand) never gets a
	// report, so without this its card would read "connecting" for as long
	// as it is configured. Nil is "cannot say", and the roll-up then reads
	// an unreported surface as connecting rather than guessing either way.
	Converges func(kind integration.Kind) bool

	// Work and Pages are Worklet and Doclet, read through the estate
	// router and resolved per question: a company whose tracker or wiki is
	// a vendor's has each of that half's questions answered unknown, as an
	// unregistered question is. Nil — a surface built without them —
	// leaves their questions unregistered.
	//
	// Consumer-defined interfaces rather than the concrete readers, like
	// every other seam here — and the READ side only. Nothing on this
	// surface writes: a board's edit goes through a seat's tools or the
	// operator surface (internal/api/operator), both of which are
	// attributed to somebody.
	Work  WorkReader
	Pages PageReader

	// Files is a project's files — the rows, never the bytes, which the
	// download route streams. SEPARATE from [Sources.Work] for the reason
	// [Sources.WorkSearch] is: a node can answer one and not the other,
	// and a screen needs to know which. Nil leaves `work_files`
	// unregistered.
	Files FileReader
	// Backlinks is which pages and tasks link to a page — this node's
	// lexical index, which derives the links from the bodies it reads
	// ([search.Backlinks]). Nil leaves the `page` answer without
	// `linked_from`, which is absent rather than empty: "nothing links
	// here" is a claim only a node holding the index can make.
	Backlinks PageBacklinks

	// WorkSearch is the ranked item search, and it is SEPARATE from
	// [Sources.Work] because the two fail independently: the rows are the
	// fleet's and the lexical index is this node's own, so a node still
	// building one answers every board question and cannot rank a word.
	// Nil leaves `work_search` unregistered, which is what a screen needs
	// in order to offer the board's filters instead of an empty ranking.
	WorkSearch WorkSearcher

	// Chat is the company's own conversation through the estate router,
	// the third native backend beside the tracker and the wiki and resolved
	// per question the same way: a company talking on Slack has each chat
	// question answered unknown. Nil leaves them unregistered.
	//
	// EVERY QUESTION IT SERVES IS ABOUT THE CALLER, as the seat the
	// request's principal acts as — see chat.go's file doc. There is no
	// handle parameter anywhere on this family and no arm that reads
	// somebody else's rooms.
	Chat ChatReader

	// ChatSearch is the ranked search over that conversation, through the
	// router: the data node that answers resolves the viewer's rooms beside
	// the index it reads. A seam of its own rather than a method of
	// [Sources.Chat] because it is a search FOR A VIEWER rather than a read
	// of rooms — see [ChatSearcher]. Nil leaves `chat_search`
	// unregistered.
	ChatSearch ChatSearcher

	// ChatReads is where each person's eye has reached, which rooms they
	// muted and whether they are in do-not-disturb: the ONE piece of chat
	// state that is not on the log, kept in coordination. Nil is "this
	// process cannot say", which the rail reports rather than badging
	// every room with its whole history.
	ChatReads ChatReads

	// WebhookBase is where a THIRD-PARTY APP reaches this deployment —
	// `config.API.WebhookBase`: the public listener's
	// `api.public.external_url` where `api.public` is set, and
	// `api.external_url` otherwise — or nil when this process cannot say.
	//
	// THE VENDOR'S ADDRESS, NEVER THE PERSON'S. What this is compared
	// against is the endpoint a surface was REGISTERED at, which the
	// reconcile loop and the dashboard's setup pass both stamp from that
	// same accessor ([integration.StampEndpoint]). Behind a public listener
	// the person's `api.external_url` is a different socket, one that
	// answers 404 for every webhook route — so compared against it, every
	// registration on a healthy deployment read as moved, for ever, and its
	// card as Action needed. Named as setupapi's Options name it, so the
	// wiring says which of the two addresses it hands in.
	//
	// A SEAM RATHER THAN A READ OF [Sources.Company], which is where the
	// address used to live. As a Tier B field it could be a whole `${VAR}`
	// the document stored verbatim, while what a surface REGISTERED was the
	// address that reference resolved to — so a comparison against the raw
	// document answered "the address moved" for every company that wrote
	// one, for ever. It is Tier A now and cannot be a reference, and the
	// seam stays because this package must not import the operator's own
	// config to answer a question about a company.
	//
	// Nil is "cannot say", exactly as with Routed, Verifiable and Reconciles
	// above: a node that cannot read the value must not be the reason a
	// screen calls a healthy registration stale.
	WebhookBase func() string
	// DashboardBase is where a PERSON's browser reaches this deployment,
	// `api.external_url`: the origin a link in chat must carry to be one of
	// this dashboard's own addresses, which is what lets a message's link
	// to a task, a page or an artifact be previewed as that record
	// (`chat_unfurls`). Nil, or empty, is "cannot say", and then no link is
	// taken for one of this dashboard's: a preview of the company's task
	// under somebody else's address would vouch for a link that does not
	// go there.
	DashboardBase func() string
	// PullRequests reads a pull request for a chat link's preview, through
	// the company's GitHub wiring (`chat_unfurls`). Nil previews none: a
	// pull request's link is then drawn by the link rules alone.
	PullRequests PullRequestReader
	// Retention is this node's answer about the state log's own history:
	// how far each domain's log may be trimmed, what is stopping it, and
	// what this node costs to replace.
	//
	// A FUNCTION rather than a value, for the reason [Sources.Company] is
	// one: the answer is assembled per call from coordination and from
	// this node's own loops, and a document captured when the API was
	// assembled would name a fleet from before every node it describes
	// had reported. Nil leaves the question unregistered, which is honest
	// for a node running no state log: it has no applier and no floor to
	// say anything about.
	Retention func(ctx context.Context) any

	// Backups is the fleet's backup register — each node's newest announced
	// point and the operator's newest acknowledgement of each log, which is
	// what each log's backup term reads. With [Sources.Events] it answers
	// `backups`; nil leaves that unregistered.
	Backups interface {
		BackupPoints(ctx context.Context) ([]coord.BackupPoint, error)
	}

	// BackupFloor is whose word the trim takes for what is backed up
	// (`stream.tracker_retention.backup_floor`, Tier A, fixed for the life
	// of the process), so `backups` marks exactly the points the trim
	// counts. Empty reads as the default, `engine`.
	BackupFloor config.BackupFloor

	// NodeID names this node in the fleet answer, so a reader can tell
	// which row is the one they are talking to. The RESOLVED id
	// (config.ResolveNodeID), which is also the name the node's presence
	// lease carries, never the raw `node.id` field.
	NodeID string

	// Now is the ONE clock every answer here reads, injectable so a caller
	// can pin it. Nil is the wall clock.
	//
	// EVERY ANSWER, not the few that happened to be written against it:
	// `tokens` and seven of the tracker's answers read the wall clock
	// beside it, so a caller that pinned this still had answers that moved
	// with the wall — and `tokens` labels its window with the instant it
	// was asked, so one question asked twice, a second apart, answered
	// twice differently. A source walk in clock_test.go holds it: nothing
	// in this package reads the wall clock but [Sources.clock].
	//
	// AND THE STORE IS HANDED IT where an answer here labels what the store
	// counts: `tokens` and `token_series` head their answer with a window
	// measured from this clock, and [store.EventLog.PhaseTokens] measures
	// its rows from the same instant, or the heading and the rows are two
	// windows. What the store measures AND labels itself stays on its own
	// clock — a listing's read floor, which has to agree with the clock the
	// retention sweep deletes by, and `event_series`, whose axis comes back
	// from the one evaluation its bars were counted over — so pinning this
	// does not pin those.
	Now func() time.Time

	// Models is the engine's half of the model questions (`llm_entries`):
	// what this node resolved for each entry and its latest alarms. Nil
	// leaves `llm_entries` unregistered, with `llm_runners` beside it.
	Models ModelSource

	// Secrets is the fleet's secret rows, metadata only, which a model's
	// credential is dated by (`stored_at`). Nil answers no `stored_at`.
	Secrets SecretRows

	// DeviceSignIns is the fleet's device sign-in records. Nil leaves
	// `llm_device_sign_in` unregistered and `device_sign_in` null.
	DeviceSignIns DeviceSignIns

	// Catalogue is the model catalogue a model step suggests from. Nil
	// leaves `llm_models` unregistered.
	Catalogue ModelCatalogue

	// Artifacts is the artifacts' admission and reads, for the artifacts'
	// questions. Nil leaves them unregistered: a node built with no
	// artifacts estate to route to.
	Artifacts *ArtifactsSource
	// ArtifactData is the artifact datastore's admission and reads, for the
	// datastore's questions. Nil leaves them unregistered: a node built with
	// no datastore.
	ArtifactData *ArtifactDataSource
}

// ScheduleRuns is the dispatch history a schedules answer reads.
//
// Declared here rather than imported so this package depends on the shape and
// not on the ledger. Satisfied by the SQL ledger and its memory twin alike.
type ScheduleRuns interface {
	Recent(ctx context.Context, limit int) ([]schedule.Run, error)

	// RecentFor is the same listing narrowed to ONE schedule — see
	// [schedule.Ledger]. The company-wide page is not a history of
	// anything: twenty schedules firing hourly fill fifty rows in two and
	// a half hours, so "did the standup fire this week" was unanswerable
	// while every row of the answer sat in the table.
	RecentFor(ctx context.Context, scope types.ScheduleScope,
		scopeID, name string, limit int) ([]schedule.Run, error)
}

// clock reads the injected time, or the wall clock, in UTC either way. It is
// the only read of the wall clock in this package — see [Sources.Now].
func (s Sources) clock() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

// zone is the company's ONE clock (ADR-0018) as the current epoch sets it, or
// UTC before one is running: what every day this surface cuts — a `due=`
// filter, a due band, an overdue mark, a person's day, a spend window —
// begins at.
//
// Read per call, like [Sources.Company] itself, because an apply can move it.
func (s Sources) zone() *time.Location {
	if s.Company == nil {
		return time.UTC
	}
	company, _ := s.Company()
	return company.Location()
}

// ErrUnavailable is a question this node understood and cannot answer HERE:
// its copy of the company's records is still catching up, the coordination
// store it reads could not be reached, or its state log refused the read — at
// the level asked, or at all while it holds a record it cannot decode, is
// evicted or its log is full.
//
// Distinct from an empty answer, and the distinction is the point: a dashboard
// that drew "there is no work" for "this node cannot read the work right now"
// would report a quiet company during an outage. Distinct from [ErrUnknown]
// too, which is the answer for a source this node does not have at all. Some
// of it clears by waiting and some does not, and that is said by
// [statelog.RetryAfter] on what it wraps — zero, and so no Retry-After, for a
// refusal no wait changes — rather than by leaving those out of this error and
// reporting them as a fault.
//
// Answers do not return it themselves. The registry classifies at one boundary
// (see classifyFailure), so an answer that reads a store which can be
// briefly unreachable cannot forget to.
var ErrUnavailable = errors.New("queries: not available on this node")

// ErrNotFound is a question this surface understood, about a record it does
// not hold.
//
// Distinct from [ErrUnavailable] and from a plain failure, because a client
// acts on all three differently: a dead link to show the person, a retry in a
// moment, and a bug to report. Folding the first into the third is how a
// mistyped item key reads to an operator as the server being broken.
var ErrNotFound = errors.New("queries: no such record")

// Register wires every question these sources can answer.
//
// A question whose source is missing is NOT registered, so it comes back as
// unknown rather than as a failure — the honest answer for a node that does not
// have that surface at all.
func Register(r *Registry, s Sources) {
	if s.State != nil {
		r.Register("agent", Everybody(), s.agent)
		// ONE SEAT'S CALL IN FLIGHT, WHOLE: what a tab asks when an `agents`
		// push names a version of a heavy field newer than the copy it
		// holds — the push that carried it was dropped. From the projection
		// alone, so it costs what a push does; `agent` reads the seat's
		// history from every data node as well. Everybody's, as the `agents`
		// push is, and masked as that push is: only a reader the seat's
		// trail is open to — its lead, or an admin — receives the body.
		r.Register("live_call", Everybody(), s.liveCall)
		r.Register("tokens", Everybody(), s.tokens)
	}
	if s.Events != nil {
		// THE COMPANY-WIDE AUDIT TRAIL is admin's, every question of it:
		// a phase record carries its prompts and its response, and a
		// company-wide listing is every seat's at once — which no lead's
		// relation reaches, since a lead reads their own seats' trail.
		r.Register("events", Requires(iam.GrantAdmin), s.events)
		// THE SAME ROWS WITH A TIME AXIS, which the listing has no
		// dimension for: a page of rows says what happened and nothing
		// about when the company was busy. A second question rather than
		// a flag on the first, because the two answers have different
		// shapes and one route returning either would make every caller
		// branch on what came back — the same split `tokens` and
		// `token_series` already carry.
		r.Register("event_series", Requires(iam.GrantAdmin), s.eventSeries)
		r.Register("event", Requires(iam.GrantAdmin), s.event)
		r.Register("trace", Requires(iam.GrantAdmin), s.trace)
		// A turn is its own question, not a slice of the trace: one trace
		// can span several turns and one turn several traces. See the
		// answer, and migration 0015 which made it askable at all.
		r.Register("turn", Requires(iam.GrantAdmin), s.turn)
		// AND THE LIST OF THEM, which did not exist: a turn is the unit
		// of work this engine does and every other surface is a
		// projection of one. The dashboard faked it by paging the raw
		// feed sixty-one times and folding in the browser.
		//
		// DECIDED PER SEAT rather than admin's whole: one seat's turns
		// (`seat=`) are that seat's trail, which its lead reads
		// (`audit.seat.read`), and the company's are admin's.
		r.Register("turns", DecidedBy(authz.ActionSeatTrailRead), s.turns)
		// The company's phase records, with their payloads. `events` cannot
		// serve this: its listing never selects the payload, and a phase
		// record without one has no prompts, no response and no decision.
		// Decided as `turns` is.
		r.Register("phases", DecidedBy(authz.ActionSeatTrailRead), s.phases)
	}
	if s.Usage != nil {
		// AND THE TIME AXIS. `tokens` is a breakdown whose every row is a
		// sum over the whole window, so it cannot say WHEN — which is the
		// question a cost explorer is for. Gated on the usage domain rather
		// than on the projection: its buckets are company days, which only
		// the replicated rows hold, and an axis that changed source when a
		// reader widened the range is a seam across the one comparison the
		// screen exists to make.
		r.Register("token_series", Everybody(), s.tokenSeries)
		// EVERY SEAT'S TURNS over a window of company days, from the same
		// replicated rows: counts, the first-pass rate over reviewed turns,
		// merged duration quantiles and a day-by-day series. A screen
		// counting the rows of a list it loaded was counting the list.
		r.Register("seat_activity", Everybody(), s.seatActivity)
		// WHO READ A PAGE, from every data node's days — see pagereads.go.
		// Gated on the USAGE domain rather than on Doclet's pages: a
		// read is recorded with its backend, so a company on Confluence
		// has readers too. Doclet's, as the pages are.
		r.Register("page_reads", Requires(iam.GrantDoclet), s.pageReads)
	}
	if s.Coord != nil {
		// THE DEPLOYMENT'S OWN SHAPE rather than the company's work: the
		// node ids, which node holds which seat, the lease epochs and how
		// far a config rollout has reached. So it is admin's, as every
		// other control of the deployment is — `retention` and `backups`
		// included — rather than everybody's overview.
		r.Register("fleet", Requires(iam.GrantAdmin), s.fleet)
		// WHAT EACH MCP SERVER DID ON EACH NODE, off the same lease
		// table: every node re-publishes its starts on its presence
		// heartbeat, so one listing answers for the fleet. Admin's, as
		// the company document is, because that is what it discloses —
		// every server's launch command and the first line of each
		// failure, which `/config` shows the same reader. See
		// [Sources.mcpServersStatus].
		r.Register("mcp_servers_status", Requires(iam.GrantAdmin), s.mcpServersStatus)
		// WHAT EACH MODEL IS DOING, from the same heartbeats: everybody's,
		// in two readings (see [Sources.llmEntries]), and the runners'
		// readiness the admin's, as the configuration is.
		if s.Models != nil {
			r.Register("llm_entries", Everybody(), s.llmEntries)
			r.Register("llm_runners", Requires(iam.GrantAdmin), s.llmRunners)
		}
	}
	if s.Backups != nil && s.Events != nil {
		// THE DEPLOYMENT'S, for retention's reason and more: every row
		// names a directory on a named host that holds the company's
		// sealed credentials. Gated on BOTH halves, since an answer with
		// the register and no history would read as a fleet nobody ever
		// asked for a backup.
		r.Register("backups", Requires(iam.GrantAdmin), s.backups)
	}
	if s.FleetBroker != nil {
		// ADMIN'S, for the fleet question's reason: node ids, their roles
		// and which member the broker counts are the deployment's shape,
		// not the company's work — and the removal it is read beside is an
		// admin's gesture too.
		r.Register("fleet_broker", Requires(iam.GrantAdmin), s.fleetBroker)
	}
	// WHO IS ASKING. Registered unconditionally: a process with no company
	// still has a credential presented to it, and "this token resolves to
	// no seat" is the answer a screen needs in order to say what to bind.
	// Everybody's, since the answer is about the asker and nobody else.
	r.Register("viewer", Everybody(), s.viewer)
	if s.Catalogue != nil {
		r.Register("llm_models", Requires(iam.GrantAdmin), s.llmModels)
	}
	// ONE DEVICE SIGN-IN WHILE ITS RECORD LINGERS, the admin's, as starting
	// one is.
	if s.DeviceSignIns != nil {
		r.Register("llm_device_sign_in", Requires(iam.GrantAdmin), s.llmDeviceSignIn)
	}
	if s.Runs != nil {
		// ONE SCHEDULE'S OWN HISTORY, gated on the LEDGER rather than on
		// the company: the configured rows are a projection of the org
		// and this is a store read, so a node with one and not the other
		// is a real shape.
		r.Register("schedule_runs", Everybody(), s.scheduleRuns)
	}
	if s.Company != nil {
		// Gated on the COMPANY, not on the durable counter: the caps are
		// what the screen is about, and a counter that cannot be read
		// answers "these are the ceilings, and nobody can read the usage",
		// which is a real state an operator needs to see and is not the
		// same as the question being unavailable here.
		r.Register("budgets", Everybody(), s.budgets)
		// THE CAPS A TURN IS HELD TO, as the engine enforces them, beside
		// its defaults: read from the company document, and the
		// configuration's, so an administrator's like the `config`
		// question it is read beside. See [LimitRow].
		r.Register("limits", Requires(iam.GrantAdmin), s.companyLimits)
		// Both are projections of the epoch: what the company DECLARES,
		// which is a different question from what it has done.
		r.Register("schedules", Everybody(), s.schedules)
		// ADMIN'S, alone among the projections, because of what it
		// projects. `/setup` is guarded in FULL — reads
		// included — for the reason its own package doc gives: "the list
		// of which credentials a company has NOT configured is a map of
		// what to attack." This answer is that same map, per surface:
		// which are configured, which hold a secret, which are
		// half-set-up, and the address each is registered against.
		r.Register("integrations", Requires(iam.GrantAdmin), s.integrations)
		// Gated on the COMPANY, not on the searcher, for the same reason
		// budgets is: "this company has no knowledge backend configured" is
		// a fact the company alone establishes, and it is a far more useful
		// answer than an unknown query. A nil searcher IS the answer here,
		// not the absence of one.
		//
		// EVERYBODY MAY ASK, and only the knowledge base's holder is
		// searched for: the answer says a caller without it was not
		// searched for, and refuses a caller with neither app — see
		// [Sources.knowledgeSearch].
		r.Register("knowledge", Everybody(), s.knowledgeSearch)
		// WHERE THE COMPANY'S APPS LIVE IN A BROWSER, the least of the
		// integrations a reader needs to draw a vendor's mark beside a link
		// and to link a bare reference: a kind and a base address per app.
		// Everybody's, unlike `integrations`, because it carries nothing
		// else; see [Sources.linkHosts].
		r.Register("link_hosts", Everybody(), s.linkHosts)
		// A NAME TO A SEAT, through the tiers an agent's own lookup uses —
		// the command palette's assign and ask pickers. A projection of
		// the chart, so it rides the company like the others here, and is
		// everybody's as the chart is — with the contact ids a vendor
		// knows the seat by kept to an admin (see [Sources.colleague]).
		r.Register("colleague", Everybody(), s.colleague)
	}
	if s.Sandbox != nil {
		// THE STATUS OF EVERY EXECUTOR is everybody's overview; what a
		// run was asked, what it asked and what it was answered are its
		// seat's trail, and its answer withholds them from a reader the
		// seat's trail is not open to.
		r.Register("executors", Everybody(), s.executorRows)
	}
	if s.ExecutorTail != nil {
		// ASKED ONLY WHILE SOMEBODY WATCHES: a trace polls it while a
		// running executor's span is open, and nothing else does. There
		// is no event and no row behind it — see [Sources.ExecutorTail].
		// DECIDED ON THE RUN'S SEAT, as that seat's trail: what a coding
		// agent prints is its transcript, as a phase record's prompt and
		// response are, so its lead reads it and an admin reads any.
		r.Register("executor_tail", DecidedBy(authz.ActionSeatTrailRead), s.executorTail)
	}
	if s.Retention != nil {
		// THE DEPLOYMENT'S. The answer names every node in the fleet, its
		// position, its disk and its snapshot repository — a map of which
		// machine to take out to lose the company's history — and it is
		// read by a person or their cron.
		r.Register("retention", Requires(iam.GrantAdmin), s.retention)
	}
	if s.SignIn != nil {
		// HOW A PERSON SIGNS IN HERE, for the administrators who answer
		// for it: posture rather than a secret, but the deployment's, read
		// beside the people and tokens it governs. See [SignInPolicy].
		r.Register("sign_in_policy", Requires(iam.GrantAdmin), s.signInPolicy)
	}
	// The NATIVE backends, each gated on its own reader: a company can run
	// Worklet on Confluence, or Doclet on
	// Jira, and registering the pair together would offer one screen a
	// question its half of the company cannot answer. Each is its app's —
	// the tracker's questions the tracker's, the pages' the knowledge
	// base's — with the overview's cross-app answers (`company_feed`,
	// `decisions`) everybody's and filtered per row.
	if s.Work != nil {
		r.Register("work_items", Requires(iam.GrantWorklet), s.workItems)
		// ONE ITEM, DECIDED ON THE ITEM: a tracker holder reads any, and
		// somebody an open ask on it names reads that one to answer it
		// (`get_work_item`'s asked arm) — which only the item says.
		r.Register("work_item", DecidedBy(authz.ActionWorkRead), s.workItem)
		// A TASK'S THREAD, PAGED — the one collection on a detail with
		// no bound of its own, so the detail returns its newest page and
		// this walks the rest. See [Sources.workComments].
		r.Register("work_comments", Requires(iam.GrantWorklet), s.workComments)
		// A TASK'S TURNS, PAGED, from the tracker's own rows — the
		// durable account its cost panel sums. See
		// [Sources.workItemTurns].
		r.Register("work_item_turns", Requires(iam.GrantWorklet), s.workItemTurns)
		// A SEPARATE QUESTION from `work_items`, for the reason
		// `containers` is separate from `pages`: a screen draws the tab
		// strip once and the rows in it on every filter change.
		r.Register("work_views", Requires(iam.GrantWorklet), s.workViews)
		// AND EVERY VIEW THE CALLER CAN SEE, across the containers: the
		// inventory and the sidebar's pins are about the caller's own
		// views, not one container's tabs. See [Sources.workSavedViews].
		r.Register("work_saved_views", Requires(iam.GrantWorklet), s.workSavedViews)
		// A SEPARATE QUESTION from `work_items` for the reason
		// `work_views` is: a home screen draws the project list once
		// and its rows' tasks on every navigation, and the counts here
		// are three MAINTAINED columns rather than an aggregate over
		// every task in the company.
		r.Register("work_projects", Requires(iam.GrantWorklet), s.workProjects)
		r.Register("work_project", Requires(iam.GrantWorklet), s.workProject)
		// AND WHO IS CARRYING HOW MUCH, across every project at once.
		// A caller that grouped it itself paid one round trip per
		// project and rewrote the arithmetic per surface.
		r.Register("work_workload", Requires(iam.GrantWorklet), s.workWorkload)
		// THE FEED IS ITS OWN QUESTION, because it is ordered by the
		// LOG rather than by anything a board sorts on: one durable
		// table at any age, with a cursor that is a position.
		r.Register("work_activity", Requires(iam.GrantWorklet), s.workActivity)
		// AND ONE PERSON'S DAY, plus the notices that reached them.
		//
		// SCOPED RATHER THAN ADMIN-ONLY — see [Sources.recordHandle].
		// A caller reads their own record, and naming anybody else's is
		// the owner-or-lead rule the tools that WRITE these records are
		// decided by: theirs, whoever leads them, or an admin — each with
		// the tracker. Registered admin-only, the landing screen becomes
		// the most-gated screen in the product and the human teammate —
		// one of the two readers this dashboard is for — is fictional.
		r.Register("work_my_work", Requires(iam.GrantWorklet), s.workMyWork)
		// THE READER HAS ALWAYS EXISTED and nothing asked it: eighteen
		// typed wake reasons, an addressed flag, a fallback flag and
		// this person's own read and snooze marks, swept on a 365-day
		// retention and reaching no screen.
		r.Register("work_inbox", Requires(iam.GrantWorklet), s.workInbox)
		// WHO ONE CHANGE WOKE. The applier has written the set
		// since the domain landed and its only trace on any surface
		// was `work_activity.notified`: a boolean saying that
		// somebody, somewhere, was told.
		r.Register("work_routing", Requires(iam.GrantWorklet), s.workRouting)
		r.Register("work_catalogue", Requires(iam.GrantWorklet), s.workCatalogue)
		r.Register("work_person", Requires(iam.GrantWorklet), s.workPerson)
		// THE LANDING SCREEN'S SERIES: the tracker's history replayed
		// backward from today's census. See home.go.
		r.Register("work_flow", Requires(iam.GrantWorklet), s.workFlow)
	}
	// THE LANDING SCREEN'S TWO MERGED ANSWERS, registered wherever ANY half
	// they merge is wired rather than behind the tracker alone: the feed
	// merges the tracker's completions, creates and hand-offs with the
	// pages' and the schedules' where this node keeps them, and what is
	// waiting on a person is their open asks beside the executors parked
	// on a question to them, scoped as `work_my_work` is. Each leaves out a
	// half that is not kept here ([keptHere]) and answers the rest, so a
	// company on Jira still sees its schedules' runs and the runs waiting on
	// a person. Both are the overview's and everybody's: each row is
	// filtered by the app it belongs to ([authz.Shows]), and a person's own
	// asks need no tracker to be listed, since answering them needs none
	// (section 5a of ADR-0033).
	if s.Work != nil || s.Pages != nil || s.Usage != nil {
		r.Register("company_feed", Everybody(), s.companyFeed)
	}
	if s.Work != nil || s.Sandbox != nil {
		r.Register("decisions", Everybody(), s.decisions)
	}
	// WHAT A PERSON DECIDED LATELY, the other half of `decisions` and
	// scoped as it is: the asks put to them and closed, by them or by
	// somebody else. The tracker's alone, since a parked run that was
	// answered is gone from the sandbox; and everybody's, since a person's
	// own asks need no tracker (section 5a of ADR-0033).
	if s.Work != nil {
		r.Register("answered_decisions", Everybody(), s.answeredDecisions)
	}
	if s.Pages != nil {
		r.Register("pages", Requires(iam.GrantDoclet), s.pageList)
		r.Register("page", Requires(iam.GrantDoclet), s.page)
		// A SEPARATE QUESTION from `pages`, not a facet of it: a browser
		// draws the container list once and the page list on every
		// navigation, and folding them together would ship every
		// container's record with every page listing.
		r.Register("containers", Requires(iam.GrantDoclet), s.containers)
		// WHAT HAPPENED TO THE PAGES, which `pages_history` has recorded
		// since the domain landed with two indexes naming readers nobody
		// wrote — and ONE REVISION'S BODY, which the detail's summaries
		// could say existed and never show.
		r.Register("page_activity", Requires(iam.GrantDoclet), s.pageActivity)
		r.Register("page_revision", Requires(iam.GrantDoclet), s.pageRevision)
	}
	if s.Chat != nil {
		// CHATLET, and then the SEAT: the registry asks for `chatlet`
		// as it asks every app's questions, and each answer decides its
		// own verb on the authority table before it reads a thing, so a
		// credential acting as itself is refused as the table refuses it
		// (`unseated`) — a room is read as a seat, and such a caller has
		// none to be shown it as. See chat.go.
		r.Register("chat_channels", Requires(iam.GrantChatlet), s.chatChannels)
		r.Register("chat_channel", Requires(iam.GrantChatlet), s.chatChannel)
		r.Register("chat_messages", Requires(iam.GrantChatlet), s.chatMessages)
		// A THREAD IS ITS OWN QUESTION, not a filter on the room: it is
		// opened beside the transcript and paged the other way, because a
		// conversation is read forwards.
		r.Register("chat_thread", Requires(iam.GrantChatlet), s.chatThread)
		// THE READER'S OWN VIEWS, each across the rooms they may read and
		// each read with their own record (chatreader.go): what waits for
		// them, the threads they follow, what they saved and sent, a
		// room's shared links, and one message read in place.
		r.Register("chat_activity", Requires(iam.GrantChatlet), s.chatActivity)
		r.Register("chat_threads", Requires(iam.GrantChatlet), s.chatThreads)
		r.Register("chat_saved", Requires(iam.GrantChatlet), s.chatSaved)
		r.Register("chat_sent", Requires(iam.GrantChatlet), s.chatSent)
		r.Register("chat_links", Requires(iam.GrantChatlet), s.chatLinks)
		r.Register("chat_message", Requires(iam.GrantChatlet), s.chatMessage)
		// A MESSAGE'S LINKS PREVIEWED, each asked of the question its
		// record's own screen asks, through this registry and so as the
		// caller (chatunfurls.go).
		r.Register("chat_unfurls", Requires(iam.GrantChatlet), s.chatUnfurls(r))
		// THE NAMES OF THE ROOMS A SEAT MAY READ, joined or not: how a
		// reader tells a room written in prose from a colour or an
		// issue's number spelled the same way.
		r.Register("chat_room_names", Requires(iam.GrantChatlet), s.chatRoomNames)
		r.Register("chat_rooms", Requires(iam.GrantChatlet), s.chatRooms)
		if s.ChatSearch != nil {
			r.Register("chat_search", Requires(iam.GrantChatlet), s.chatSearch)
		}
	}
	if s.Memory != nil {
		// A SEAT'S TRAIL — what it remembers and what it said — ANSWERED
		// BY THE HOLDER, and saying which node that was (see
		// [Sources.Memory]). Decided per seat by
		// [authz.ActionSeatTrailRead] — the seat, its lead, or an admin —
		// the one verb all four ask, so the halves of what a seat has said
		// and remembered cannot answer one reader differently.
		r.Register("agent_memory", DecidedBy(authz.ActionSeatTrailRead), s.agentMemory)
		// ONE EPISODE WHOLE, which the listing carries the openings of —
		// a page of fifty whole asks could be more than the transport
		// takes. Half of the same trail, so decided per seat by the same
		// verb.
		r.Register("agent_episode", DecidedBy(authz.ActionSeatTrailRead), s.agentEpisode)
		// EVERY AGENT SEAT'S TOTALS IN ONE ANSWER, each counted by its
		// holder in one scatter — what the diaries list draws, where a
		// read per seat would be a lease read and a scatter per row.
		// Decided per seat too, so the list holds the seats the caller's
		// trail reaches and no others.
		r.Register("memory_overview", DecidedBy(authz.ActionSeatTrailRead), s.memoryOverview)
		// An absent handle is the caller's own seat; see
		// [Sources.trailSeat].
		r.Register("conversations", DecidedBy(authz.ActionSeatTrailRead), s.conversations)
	}
	if s.Channels != nil {
		// EVERY SEAT'S ASKS OF EVERY OTHER: the company-wide trail, so
		// admin's, as `events` is.
		r.Register("a2a_channels", Requires(iam.GrantAdmin), s.a2aChannels)
	}
	if s.Files != nil {
		// A PROJECT'S FILES, a page at a time in path order. The bytes are
		// not an answer on this channel — a download streams from its own
		// route — so this is everything a file list draws and nothing a
		// reader opens. The tracker's, as every other work question.
		r.Register("work_files", Requires(iam.GrantWorklet), s.workFiles)
	}
	if s.WorkSearch != nil {
		// SEARCH IS A QUESTION, not a filter on the board, and it is
		// gated on its own index rather than on the tracker: the ranked
		// reader is what `search_work_items` gives a seat, and the operator
		// reading the same company had only `q=` — an escaped LIKE over
		// the excerpt, gated to a span of days. The tracker's, as the
		// board is.
		r.Register("work_search", Requires(iam.GrantWorklet), s.workSearch)
	}
	if s.Config != nil {
		// ADMIN'S, all four. Reading the config document exposes the
		// whole company — which integrations are wired, and every ${VAR}
		// reference by name.
		r.Register("config", Requires(iam.GrantAdmin), s.configDocument)
		r.Register("config_audit", Requires(iam.GrantAdmin), s.configAudit)
		r.Register("config_diff", Requires(iam.GrantAdmin), s.configDiff)
		r.Register("config_entities", Requires(iam.GrantAdmin), s.configEntities)
	}
	if s.Artifacts != nil {
		// THE ARTIFACTS, each decided per artifact by the artifacts' one
		// admission and shaped by the reader's role; the listing filtered
		// row by row rather than refused.
		registerArtifacts(r, *s.Artifacts, s.clock)
	}
	if s.ArtifactData != nil {
		// AN ARTIFACT'S DATA, decided per artifact by the datastore's one
		// admission and shaped by each collection's read rule.
		registerArtifactData(r, *s.ArtifactData, s.clock)
	}
}

// agent answers one seat's live state.
func (s Sources) agent(ctx context.Context, p Params) (any, error) {
	// BY HANDLE, the dashboard's one address for a seat — `query("agent",
	// {id})` from the seat page and /agents/{id} from the REST table — and
	// resolved to the seat's AGENT ID, which is what both halves of the
	// answer are keyed by: the projection holds a seat's live state under it,
	// and the event store promotes it on every phase row.
	//
	// It used to accept a ROLE NAME as well and answer by name, from a
	// projection keyed by name and a history matched on `agent_role`. A
	// name is prose two seats may share, so two "Engineer"s answered each
	// other's live call and read each other's transcript.
	handle := p.String("id")
	if handle == "" {
		return nil, fmt.Errorf("%w: agent needs the seat's handle as id", ErrBadParams)
	}
	seat, agentID := s.agentSeat(handle)
	if seat == nil {
		return nil, fmt.Errorf("%w: no agent seat answers to the handle %q", ErrNotFound, handle)
	}
	// LIVE STATE AND HISTORY, which are two different sources and always
	// were: the projection holds the call in flight, the event store holds
	// the ones that finished. The answer carried only the first, so a seat
	// page showed the round happening now and nothing before it — while the
	// spend chart beside it, which reads the store, reported every phase
	// the turn had already completed. Two panels on one screen disagreeing
	// about whether a seat had done anything.
	//
	// A seat the projection has never seen is NOT an error: a seat
	// configured and never spawned is exactly that, and a 404 there would
	// make a healthy new company look broken. Its history is answered the
	// same way.
	//
	// `history=false` ASKS FOR THE SEAT ALONE, its status without the calls
	// it finished: their payloads are whole transcripts, so a caller asking
	// what a seat is doing (the assistant's step, held to a step's read)
	// was handed a body too large to read and lost the status with it.
	history, next, coverage := []store.EventRecord{}, "", (*eventfan.Coverage)(nil)
	if p.Bool("history", true) {
		history, next, coverage = s.phaseHistory(ctx, agentID, p)
	}
	// WHAT THIS READER MAY SEE OF THE SEAT: its status, everybody's; its
	// body — the live call's transcript and every finished call's payload
	// — the seat trail's readers' ([Sources.reach]). See livestate's
	// mask.go for the split.
	reach := s.reach(ctx, seat.Handle())
	answer := map[string]any{
		"handle":   seat.Handle(),
		"agent_id": agentID,
		"role":     seat.Name,
		"live":     nil,
		// WHETHER THE BODY WAS WITHHELD, so a seat page draws "not shown
		// to you" over the transcript rather than a seat that said
		// nothing.
		"body_withheld": !reach.Body,
		// The rows are `store.EventRecord`s, PAYLOAD NESTED — the same
		// shape `event`, `trace` and `turn` answer with. They used to be
		// the payload flattened with an id and a timestamp merged in,
		// which meant one client had to carry two readers for one kind of
		// thing and a field added to the envelope reached three screens
		// and not the fourth.
		//
		// MASKED AS THE LIVE CALL IS: a finished call's payload is the
		// same transcript the live one was, so a reader without the
		// seat's body is shown each row's columns — when, which phase,
		// what it cost, its summary — and not its payload.
		"llm_history": maskHistory(history, reach),
		// The cursor the caller pages with, or "" at the end of the
		// record. Its absence is why a seat's transcript was a hard fifty
		// rows with no way past them, while the events it was made of sat
		// in the store addressable by id.
		"next": next,
		// WHICH NODES THE HISTORY CAME FROM. A seat moves between nodes,
		// so its history is on every data node it ran on or that kept its
		// events in custody; null when it could not be read at all, which
		// is not the same as a seat that has never run.
		"coverage": coverage,
	}
	// ASSIGNED ONLY WHEN THERE IS ONE, because a nil *Overlay stored in an
	// any is not nil: every `live != nil` check above this layer would read
	// a seat that has never run as one that has. It marshals to null either
	// way, so the client never saw it and only a Go caller would — which is
	// exactly the kind of trap that survives until something depends on it.
	if overlay := s.State.AgentOverlay(agentID); overlay != nil {
		answer["live"] = overlay.For(reach)
	}
	return answer, nil
}

// reach is what the asking principal may see of the live state of the seat
// with handle: its body where the authority table's `audit.seat.read` admits
// them to the seat — an admin, or its lead — through the SAME decision the
// seat's turns, memory and conversations are read under, never a second copy
// of who that is; the work item it is on where they hold Worklet; the
// company's configuration (which entries served a run, a failure's own text)
// where they are an admin; and their own record, to which an executor's
// question may be put.
//
// AN UNDECIDABLE DECISION IS NO BODY: a chart this node cannot read withholds
// the transcript and still shows the status, because what is withheld is the
// part a wrong answer would leak.
func (s Sources) reach(ctx context.Context, handle string) livestate.Reach {
	principal, how := iam.From(ctx)
	if how != iam.Resolved {
		// THE REGISTRY HAS ANSWERED ANY OTHER CALLER BEFORE THE ANSWER
		// RUNS; a reader nobody resolved is shown the status and nothing
		// else, never the zero principal read as somebody.
		return livestate.Reach{}
	}
	r := livestate.Reach{Tracker: principal.Can(iam.GrantWorklet),
		Record: iam.RecordOwner(principal)}
	r.Config = readsConfig(ctx)
	switch {
	case r.Config:
		r.Body = true
	case handle != "":
		r.Body = authz.Decide(ctx, principal, authz.ActionSeatTrailRead,
			authz.Object{Kind: authz.KindPerson, Owner: handle}, s.Chart,
			s.clock()).Allowed
	}
	return r
}

// maskHistory is a seat's finished calls as reader r receives them: each row's
// payload — the call's prompts and response — only with the seat's body, and
// the work item a row was charged to only with the tracker. A COPY wherever it
// changes anything, so the rows the store handed back are never edited.
//
// THE ITEM IS WITHHELD FROM THE PAYLOAD TOO, not only from the tags: a phase
// record states the item it was charged to as its own top-level `work_item`
// (types.AgentPhaseCompleted.WorkItem), so a lead reading their agent's body
// without Worklet would otherwise be handed exactly the field the live
// call's mask (livestate.MaskCall) and the turn listing withhold from them.
func maskHistory(rows []store.EventRecord, r livestate.Reach) []store.EventRecord {
	if r.Body && r.Tracker {
		return rows
	}
	out := make([]store.EventRecord, len(rows))
	for i, row := range rows {
		switch {
		case !r.Body:
			row.Payload = nil
		case !r.Tracker:
			row.Payload = withoutWorkItem(row.Payload)
		}
		if !r.Tracker && (row.Tags[historyWorkItemTag] != "" ||
			row.Tags[historyTaskTag] != "") {
			tags := maps.Clone(row.Tags)
			delete(tags, historyWorkItemTag)
			delete(tags, historyTaskTag)
			row.Tags = tags
		}
		out[i] = row
	}
	return out
}

// withoutWorkItem is a record's payload with its top-level `work_item`
// removed, as a FRESH encoding: the bytes the store handed back are shared by
// every reader of the page. A payload that names no item comes back as it is.
// One that is not a JSON object is withheld whole — a record this build cannot
// read is not one it can vouch carries no item, and a status read of it loses
// nothing a reader without the body or the tracker was owed.
func withoutWorkItem(payload json.RawMessage) json.RawMessage {
	if len(payload) == 0 {
		return payload
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil || fields == nil {
		return nil
	}
	if _, named := fields[historyWorkItemTag]; !named {
		return payload
	}
	delete(fields, historyWorkItemTag)
	out, err := json.Marshal(fields)
	if err != nil {
		return nil
	}
	return out
}

// The tags a phase row names the work it was charged to by: the tracker's,
// and withheld with it.
const (
	historyWorkItemTag = "work_item"
	historyTaskTag     = "task_id"
)

// agentSeat resolves a handle to the AGENT seat answering to it in the
// company this node runs, and that seat's
// agent id. Nil for a handle no agent seat answers to, and on a node running
// no company.
func (s Sources) agentSeat(handle string) (*org.Role, string) {
	if handle == "" || s.Company == nil {
		return nil, ""
	}
	company, roster := s.Company()
	// THE COMPANY'S OWN ORG, the one this node built when it applied the
	// epoch, rather than one re-derived from the document per read: both
	// come from ONE read of the epoch, so they never describe two revisions.
	if company == nil || roster == nil {
		return nil, ""
	}
	seat := roster.AgentSeatByHandle(handle)
	if seat == nil {
		return nil, ""
	}
	id, ok := roster.AgentIDFor(seat)
	if !ok {
		return nil, ""
	}
	return seat, id.String()
}

// liveCall answers `live_call{agent_id}`: the seat's in-flight call with every
// field, and the version of each, or null while it has none.
//
// The REPAIR for an `agents` push the socket dropped. A push leaves out a heavy
// field its seat's tabs already hold at the version it names (internal/api/
// stream's pushAgents), so a tab that missed the push which moved one is
// holding an older copy than every later push describes, and asks for the call
// whole here. A seat the projection has not seen is answered null like one
// between calls: either way there is nothing in flight.
//
// BY AGENT ID, the key the projection and every `agents` push name the seat
// by, so the tab pairs the answer with the row it asked for. It used to be the
// ROLE NAME, which two seats may share — two "Engineer"s answered each other's
// call. The handle rides along as a label, from the company this node runs, and
// is empty for an id that company does not hold: a call already in flight on a
// seat the latest revision removed is still a call, and nothing in the answer
// is keyed by the label.
//
// MASKED AS THE `agents` PUSH IS ([livestate.MaskCall]): the call whole to a
// reader the seat's trail is open to, its status and no body to anybody else,
// so the repair a status reader's tab never asks for anyway hands them nothing
// the push did not.
func (s Sources) liveCall(ctx context.Context, p Params) (any, error) {
	agentID := strings.TrimSpace(p.String("agent_id"))
	if agentID == "" {
		return nil, fmt.Errorf("%w: live_call needs the seat's agent_id", ErrBadParams)
	}
	handle := ""
	if id, err := uuid.Parse(agentID); err == nil {
		if organization := s.organization(); organization != nil {
			if seat := organization.AgentSeatByID(id); seat != nil {
				handle = seat.Handle()
			}
		}
	}
	// live_call_seq orders this answer against the `agents` pushes for the
	// same seat: the answer runs on its own goroutine and can be delivered
	// after a push generated later (a clear, a new call), so the tab drops
	// an answer whose sequence is behind what it has applied. Always carried,
	// a null call included — see [livestate.Overlay.LiveCallSeq].
	answer := map[string]any{"agent_id": agentID, "handle": handle,
		"live_call": nil, "live_call_seq": 0}
	if overlay := s.State.AgentOverlay(agentID); overlay != nil {
		answer["live_call_seq"] = overlay.LiveCallSeq
		if overlay.LiveCall != nil {
			answer["live_call"] = overlay.LiveCall.For(s.reach(ctx, handle))
		}
	}
	return answer, nil
}

// phaseHistory is the seat's finished calls, newest first, as the rows the
// dashboard renders beside the live one.
//
// BEST EFFORT. The live half is the answer's point and it comes from memory;
// refusing the whole seat page because the event log could not be read would
// turn a degraded history into no screen at all. An unreadable log and a seat
// that has not run yet both render as "no invocations", which is the same
// thing a reader can see for themselves from the phase chart above it.
//
// Each row is the event's PAYLOAD with the envelope's timestamp merged in: the
// payload's field names are already the client's — turn_id, phase, iteration,
// model, response, tool_executions, total_tokens — because the same shape
// drives the live row, and the timestamp is the one field that lives on the
// envelope rather than inside it. The payload's price rides along, since the
// row is the record as stored, and the dashboard reads none of it (rule 19 in
// docs/reference/dashboard-design.md).
//
// BY THE SEAT'S AGENT ID, the one identifier every phase row carries for its
// seat — never its role name, which a unit template stamps onto every seat it
// makes.
func (s Sources) phaseHistory(ctx context.Context, agentID string, p Params) ([]store.EventRecord, string, *eventfan.Coverage) {
	if s.Events == nil {
		return []store.EventRecord{}, "", nil
	}
	var before *store.Cursor
	if id := p.String("before_id"); id != "" {
		at, err := time.Parse(time.RFC3339Nano, p.String("before_time"))
		if err == nil {
			before = &store.Cursor{Time: at, ID: id}
		}
		// A malformed cursor falls back to the newest page rather than
		// failing: this is best effort, and refusing the whole seat page
		// over a bad query parameter would turn a paging bug into no
		// screen at all.
	}
	listing, coverage, err := s.Events.SeatPhases(ctx, agentID, before)
	if err != nil {
		log.WarnContext(ctx, "agent_history_unavailable", "agent_id", agentID, "error", err)
		return []store.EventRecord{}, "", nil
	}
	records := listing.Rows
	if len(records) == 0 {
		// EMPTINESS, not nil-ness, and the two are not interchangeable here:
		// a store answers a nil slice for a seat it cannot name, an
		// allocated empty one for a seat with no phases yet, and the index
		// below is out of range on BOTH. Testing for nil alone left a seat
		// whose history read came back empty indexing a slice of length zero.
		return []store.EventRecord{}, "", &coverage
	}
	// The cursor is the LAST row's key, echoed rather than left for a client
	// to assemble: (time, id) is the table's key, and a client rebuilding it
	// from a rendered timestamp would lose the sub-second precision the
	// tiebreak depends on. Offered only when the fleet holds MORE — a node
	// whose read found a row past its page, or a page the merge cut at the
	// newest point such a node stopped at — since a cursor past the end would
	// page for ever, and one on a page that merely filled pages onto nothing.
	next := ""
	if listing.More {
		last := records[len(records)-1]
		next = last.Time.UTC().Format(time.RFC3339Nano) + "|" + last.ID
	}
	return records, next, &coverage
}

// firstOf returns the first non-empty value.
func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// events answers a page of the log.
//
// The filters are the store's own, passed through rather than re-implemented:
// a listing this surface filtered itself would page differently from one the
// store filtered, and the difference shows up as rows that vanish when a reader
// scrolls.
func (s Sources) events(ctx context.Context, p Params) (any, error) {
	q, err := s.eventFilters(p)
	if err != nil {
		return nil, err
	}
	// THE RELATED FILTER IS KEYED ON AGENT IDS, and `agent` is the seat's
	// HANDLE, resolved through the org this node holds, exactly as the
	// event log resolved the handles each event
	// named when it was written (see store.DB.SetEventSeats). A handle no
	// agent seat answers to is refused rather than compared: there is no
	// id it could match, and an empty page reads as an agent that has done
	// nothing.
	if q.RelatedAgent != "" {
		id, ok := s.agentSeatID(q.RelatedAgent)
		if !ok {
			return nil, fmt.Errorf("%w: agent %q names no agent seat in this "+
				"company. The related filter is about an agent; a person's "+
				"events are the ones they acted in, under `actor`",
				ErrBadParams, q.RelatedAgent)
		}
		q.RelatedAgent = id
	}
	q.Limit = Clamp(p.Int("limit", 0), DefaultEventPage, MaxEventPage)
	if before := p.String("before_id"); before != "" {
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		at, err := cursorTime(p)
		if err != nil {
			return nil, err
		}
		q.Before = &store.Cursor{Time: at, ID: before}
	}

	listing, coverage, err := s.Events.List(ctx, q)
	if err != nil {
		return nil, err
	}
	rows := listing.Rows
	return map[string]any{
		"events": rows,
		// The cursor the caller pages with next, echoed rather than left
		// for a client to assemble: (time, id) is the table's key and a
		// client that built it from the last row's fields would be
		// reimplementing the one thing that must not drift.
		"next": cursorOf(rows),
		// A page shorter than the limit does NOT mean history is
		// exhausted when a related-agent filter is set: that filter
		// folds each page's trace siblings into it, so only a zero-row
		// page ends the walk. Saying so beats a client inferring it
		// wrongly.
		"exhausted": len(rows) == 0,
		// WHICH NODES THE PAGE WAS MERGED FROM. A node that did not answer
		// is named here, because its rows are simply absent from the page
		// and nothing else on it could say so.
		"coverage": coverage,
	}, nil
}

// eventSeries answers the log's own time axis.
//
// THE FILTERS ARE THE LISTING'S, read through the same function, because the
// two are halves of one screen: a bar counting rows the list below it would not
// show is worse than no bar at all. The store compiles both from one predicate;
// this makes sure both are handed the same one.
func (s Sources) eventSeries(ctx context.Context, p Params) (any, error) {
	filters, err := s.eventFilters(p)
	if err != nil {
		return nil, err
	}
	bucket := store.EventBucket(p.String("bucket"))
	if !bucket.Valid() {
		return nil, fmt.Errorf("%w: the bucket parameter is one of %s, and %q isn't",
			ErrBadParams, words.Or(store.EventBuckets), bucket)
	}
	got, coverage, err := s.Events.Histogram(ctx, store.HistogramQuery{ListQuery: filters, Bucket: bucket})
	switch {
	// A REQUEST THIS SURFACE UNDERSTOOD AND REFUSED, so it answers 400
	// rather than 500 with "something went wrong", each naming the
	// parameter the caller has to change — in words of this surface's,
	// because the store's open with its package's name and say the window
	// as Go prints a duration ("hour over 2160h0m0s is 2160 buckets").
	case errors.Is(err, store.ErrHistogramBucket):
		return nil, fmt.Errorf("%w: the bucket parameter is one of %s, and %q isn't",
			ErrBadParams, words.Or(store.EventBuckets), bucket)
	case errors.Is(err, store.ErrHistogramSpan):
		return nil, fmt.Errorf("%w: that window holds more %s bars than a "+
			"chart shows, which is %d at most. Pick a coarser bucket or a "+
			"shorter window", ErrBadParams, bucket, store.MaxHistogramBuckets)
	case errors.Is(err, store.ErrHistogramRelated):
		return nil, fmt.Errorf("%w: a chart can't be narrowed to a related "+
			"agent, since its bars would disagree with the events they "+
			"count. Leave related_agent out of it", ErrBadParams)
	case err != nil:
		return nil, err
	}
	return SeriesAnswer{EventHistogram: got, Coverage: coverage}, nil
}

// eventFilters reads the filters both the listing and its axis take.
//
// ONE READER, for the reason the store has one predicate: a filter added to the
// list and forgotten here would draw an axis over a wider set than the rows
// beneath it, silently.
func (s Sources) eventFilters(p Params) (store.ListQuery, error) {
	q := store.ListQuery{
		Type:     p.String("type"),
		Source:   p.String("source"),
		Category: p.String("category"),
		// ONE TRACE, and the same trace the `trace` question answers — but
		// as a FILTER, so it pages and takes a window and every other
		// filter beside it, where `trace` is the whole trace oldest first.
		TraceID: p.String("trace_id"),
		// ONE AGENT-TO-AGENT CONVERSATION, by the channel id its events
		// carry. The conversation's own page links here, and before this
		// the link could only land on the unfiltered log.
		ChannelID:    strings.TrimSpace(p.String("channel_id")),
		Actor:        p.String("actor"),
		RelatedAgent: p.String("agent"),
		// TURN_ID WAS DECLARED, DOCUMENTED AGAINST MIGRATION 0015, AND
		// DEAD: the column exists, the reader filters on it, and no
		// surface ever passed one — so "every event of this turn" was
		// answerable by the store and unaskable from anywhere.
		TurnID: p.String("turn_id"),
		// AND THE UNIT OF WORK BEHIND IT, so "every event of every attempt
		// at this trigger" is askable. Declaring the column and shipping
		// its index without a caller is the mistake the paragraph above
		// records, one migration later. See ADR-0017.
		WorkKey: p.String("work_key"),
	}
	item, err := workItemParam(p)
	if err != nil {
		return store.ListQuery{}, err
	}
	q.WorkItem = item
	// WHETHER A COMPLETION PARKED ITS TURN, three-valued as the turn list's
	// `failed` is and for its reason: absent is every row. It is what a turns
	// axis counts over — `type=agent_turn_completed&suspended=false` is the
	// turns that ENDED, one each, where the type alone counts a turn that
	// parked on its executor and resumed twice.
	if raw := strings.TrimSpace(p.String("suspended")); raw != "" {
		switch raw {
		case "true", "false":
			flag := raw == "true"
			q.Suspended = &flag
		default:
			return store.ListQuery{}, badParams("suspended", raw, []string{"true", "false"})
		}
	}
	// WHETHER THE EVENT REPORTS A FAILURE, three-valued for the same reason:
	// absent is every row. The event log's "Failures only" — a filter the
	// engine applies, so its axis and every page it fetches are the one set,
	// where narrowing the rows a tab held drew a window's worth of bars over
	// the failures among the newest hundred.
	if raw := strings.TrimSpace(p.String("failed")); raw != "" {
		switch raw {
		case "true", "false":
			flag := raw == "true"
			q.Failed = &flag
		default:
			return store.ListQuery{}, badParams("failed", raw, []string{"true", "false"})
		}
	}
	// THE ROWS THE ACTIVITY FEED CARRIES, which every view merged with the
	// live ring asks for: the ring holds no accounting row
	// ([events.KeptOutOfFeed]), so an older page that did — or an axis
	// counting them — would show a reader scrolling past the ring a log the
	// ring never was. Absent is every stored row: the event log's own reads
	// of a turn, a trace or a type want the accounting rows too.
	if raw := strings.TrimSpace(p.String("feed_only")); raw != "" {
		switch raw {
		case "true", "false":
			q.FeedOnly = raw == "true"
		default:
			return store.ListQuery{}, badParams("feed_only", raw, []string{"true", "false"})
		}
	}
	// THE EVENTS ONE SEAT PUBLISHED, named by its handle — see seatParam.
	agentID, err := s.seatParam(p)
	if err != nil {
		return store.ListQuery{}, err
	}
	q.AgentID = agentID
	// THE WINDOW, which is what a reader scrubbing a time range means and
	// is NOT the cursor: a cursor is where a page resumes and moves with
	// every page, while these are what was asked for and do not.
	since, err := instantParam(p, "since")
	if err != nil {
		return store.ListQuery{}, err
	}
	until, err := instantParam(p, "until")
	if err != nil {
		return store.ListQuery{}, err
	}
	q.Since, q.Until = since, until
	if !since.IsZero() && !until.IsZero() && !until.After(since) {
		return store.ListQuery{}, fmt.Errorf("%w: until (%s) is not after since (%s). The "+
			"window is half-open, so an empty one names no rows at all",
			ErrBadParams, until.Format(time.RFC3339), since.Format(time.RFC3339))
	}
	return q, nil
}

// workItemParam reads `work_item=`, a work item's identity across trackers:
// `<backend>:<id>`, the value [types.WorkItem.Ref] writes and schema/0033's
// column holds. Empty when absent.
//
// REFUSED RATHER THAN MATCHED when it is not that shape, because a malformed
// ref matches nothing and an empty answer to it reads as "nothing happened on
// this item" — the one conclusion a caller who pasted a key ("ENG-412") or a
// bare id instead must not draw. The backend is NOT tested against this build's
// set: a newer peer's tracker is stored under its own name, and a filter
// refusing it would hide rows this node holds.
func workItemParam(p Params) (string, error) {
	raw := strings.TrimSpace(p.String("work_item"))
	if raw == "" {
		return "", nil
	}
	backend, id, ok := strings.Cut(raw, ":")
	if !ok || backend == "" || id == "" {
		return "", fmt.Errorf("%w: work_item=%q is not a work item's identity. "+
			"Send `<backend>:<id>` (for example `native:<task id>` or `jira:<issue id>`), "+
			"never its key", ErrBadParams, raw)
	}
	return raw, nil
}

// seatParam reads `seat=<handle>` and answers the agent id of the seat it
// names, or "" when absent. See [Sources.seatAgentID].
func (s Sources) seatParam(p Params) (string, error) {
	handle := strings.TrimSpace(p.String("seat"))
	if handle == "" {
		return "", nil
	}
	return s.seatAgentID(handle)
}

// seatAgentID resolves a seat a caller named by HANDLE to the agent id every
// node derives for it — the key the event log's `agent_id` column, a usage
// row and a phase record are all filed under.
//
// A SEAT IS NAMED BY ITS HANDLE on every surface that takes one — the handle is
// a seat's address in the dashboard's URLs and on its roster row — and it is
// RESOLVED HERE, server-side, through the org this node holds, to the id
// derived from the handle (ADR-0013). Neither alternative a caller had was a
// seat's identity: a role name is shared by every seat a unit template stamps
// out, and a raw agent id is a derivation every client would have to repeat.
//
// A HANDLE THE ORG NO LONGER HOLDS still names the history its seat left, so
// it is derived as every node derived it while the seat ran.
//
// Refused rather than matched when it is not a handle, or names a PERSON's
// seat, because either would match nothing and an empty answer reads as a seat
// that never did anything; and UNAVAILABLE before a company is applied, since
// the id is derived from the company's name and there is none to derive it
// from.
func (s Sources) seatAgentID(handle string) (string, error) {
	if !org.ValidHandle(handle) {
		return "", fmt.Errorf("%w: seat=%q is not a handle. A seat is named by "+
			"its handle (lowercase, as on its page), never its role name", ErrBadParams, handle)
	}
	organization := s.organization()
	if organization == nil {
		return "", fmt.Errorf("%w: seat=%q cannot be resolved until a company "+
			"configuration is applied on this node, since a seat's id is derived from "+
			"the company's name", ErrUnavailable, handle)
	}
	if role := organization.Role(handle); role != nil {
		id, ok := organization.AgentIDFor(role)
		if !ok {
			return "", fmt.Errorf("%w: seat=%q is %s, a human seat. The engine "+
				"runs no turns for a person, so no seat's history is filed under "+
				"it; a person's events are the ones they acted in, under `actor`, "+
				"and what was spent for them is a row of its own in the "+
				"company's spend", ErrBadParams, handle, role.Name)
		}
		return id.String(), nil
	}
	id, ok := org.DeriveAgentID(organization.Name, handle)
	if !ok {
		return "", fmt.Errorf("%w: seat=%q cannot be resolved until the company "+
			"this node runs has a name, since a seat's id is derived from it",
			ErrUnavailable, handle)
	}
	return id.String(), nil
}

// instantParam reads an RFC 3339 instant, or the zero time when absent.
//
// THE ZERO IS MEANINGFUL and is what makes each side of a window optional: an
// instant nobody named is unbounded rather than midnight in 1970. A value that
// is present and unparseable is refused naming the parameter, because a
// silently-dropped bound is a read that answers a different question than the
// one asked.
func instantParam(p Params, name string) (time.Time, error) {
	raw := strings.TrimSpace(p.String(name))
	if raw == "" {
		return time.Time{}, nil
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		// NEVER THE PARSE ERROR, which says the value against Go's layout
		// ("cannot parse \"x\" as \"2006\""): what the caller sent and
		// what it should be are the whole of it.
		return time.Time{}, fmt.Errorf("%w: %s must be an RFC 3339 instant "+
			"(2026-04-16T09:00:00Z), and %q isn't", ErrBadParams, name, raw)
	}
	return at.UTC(), nil
}

// cursorTime is the before_time a before_id pages from, refused as
// [instantParam] refuses one, and required: a cursor is the pair.
func cursorTime(p Params) (time.Time, error) {
	at, err := instantParam(p, "before_time")
	switch {
	case err != nil:
		return time.Time{}, err
	case at.IsZero():
		return time.Time{}, fmt.Errorf("%w: before_id needs a before_time, an "+
			"RFC 3339 instant (2026-04-16T09:00:00Z)", ErrBadParams)
	}
	return at, nil
}

// cursorOf is the position a caller resumes from, or nil at the end.
func cursorOf(rows []store.EventRecord) any {
	if len(rows) == 0 {
		return nil
	}
	last := rows[len(rows)-1]
	return map[string]any{
		"before_time": last.Time.UTC().Format(time.RFC3339Nano),
		"before_id":   last.ID,
	}
}

// event answers one row, payload included.
func (s Sources) event(ctx context.Context, p Params) (any, error) {
	id := p.String("id")
	if id == "" {
		return nil, fmt.Errorf("%w: event needs an id", ErrBadParams)
	}
	rec, coverage, err := s.Events.ByID(ctx, id)
	if err != nil {
		// A DEAD LINK IS NOT A BROKEN NODE. `store.ErrNotFound` is not
		// [ErrNotFound], so passing it through untranslated classified an id
		// that simply is not in the log as `query_failed` — which tells a
		// reader the server is faulty and tells an operator to go looking for
		// a fault there is none of. Every event id on the dashboard is a link
		// somebody can follow after the 30-day window has closed over it, so
		// this is the ordinary case rather than the exotic one.
		//
		// The store's own sentence rides along, because across a fleet it
		// is the one that names any node that could not be asked.
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: %w", ErrNotFound, err)
		}
		return nil, err
	}
	return EventAnswer{EventRecord: rec, Coverage: coverage}, nil
}

// trace answers every row sharing one trace.
func (s Sources) trace(ctx context.Context, p Params) (any, error) {
	id := p.String("trace_id")
	if id == "" {
		return nil, fmt.Errorf("%w: trace needs a trace_id", ErrBadParams)
	}
	trace, coverage, err := s.Events.Trace(ctx, id)
	if err != nil {
		return nil, err
	}
	// SAYS WHEN IT CUT. A trace stops at store.MaxTraceEvents — and one
	// shown short with no note reads as a complete causal chain that simply
	// ends, which is the one thing a reader must not conclude from it.
	//
	// ASKED, NOT INFERRED: each node counts what it holds when its read
	// filled (see internal/eventfan), because a trace of exactly the cap
	// holds every row it has and `len(rows) == cap` would badge a complete
	// trace as cut.
	return map[string]any{
		"trace_id":  id,
		"events":    trace.Rows,
		"truncated": trace.Total > len(trace.Rows),
		"coverage":  coverage,
	}, nil
}

// retention answers what the state log's history costs and what is stopping it
// from shrinking.
//
// A PASS-THROUGH, deliberately: the document is assembled by the node that
// runs the appliers, because half its fields are facts only that node can
// state. Re-shaping it here would be a second definition of the answer, and
// `crewlet retention status` reads exactly these bytes.
func (s Sources) retention(ctx context.Context, _ Params) (any, error) {
	return s.Retention(ctx), nil
}
