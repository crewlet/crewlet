package queries

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/usage"
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

	// Events is the FLEET's turn-level history: every node's own event
	// store, asked at query time, with a [eventfan.Coverage] beside every
	// answer saying which nodes it came from (ADR-0021).
	//
	// An interface every method of which answers a coverage, so a bare
	// *store.EventLog does not compile here: a read of this node's store
	// alone answered for a third of a three-node fleet and said nothing
	// about the other two, which is exactly the answer a screen must never
	// be handed without being told.
	Events FleetEvents

	// Usage is the replicated estate's usage domain: every node's company
	// days, for every NAMED spend window and its series (ADR-0020). Not
	// this node's event log, which answered for this node alone and only
	// thirty days back. Nil leaves `token_series` unregistered and a named
	// `tokens` window empty.
	Usage usage.Estate

	// Company reads the CURRENT company, for the questions answered from
	// the company rather than from a store: the SETTINGS a revision stores
	// and the ORG this node derives from the chart's own log.
	//
	// A function, not a value: a company is republished by an activation
	// AND by a chart write, and an answer bound to the one this process
	// booted on would describe a company that is no longer running.
	//
	// ONE FUNCTION RETURNING BOTH HALVES, never two accessors. They move on
	// different rhythms, so two reads can straddle a publish — and a screen
	// that took the integrations from one and the roster from the next
	// would describe a company that never existed.
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
	// none: a node with no native backend has nothing a person could act
	// on.
	Acts func(ctx context.Context, p iam.Principal) ([]string, error)

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

	// Coord is the lease table: the fleet's one shared answer to "which
	// node holds what". Nil leaves the fleet question unregistered.
	Coord coord.Backend

	// Plane is the control plane, for the config columns of the fleet view.
	Plane coord.Plane

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
	// HANDED THE SEAT WHOLE ([memread.Seat]: its agent id, the handle it
	// was created under, and the one it answers to now), resolved here
	// from the one chart reading the answer is made of — the reader never
	// resolves a handle itself, because the node that answers may hold a
	// different chart from the node that asked. Nil leaves `agent_memory`,
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

	// Sandbox is the durable record of detached coding runs, the fleet's
	// rather than this node's. Nil leaves the question unregistered.
	Sandbox PendingRuns

	// SandboxTail is a running coding run's live output, ANSWERED BY THE
	// NODE THAT OWNS THE RUN (internal/sandbox/livetail.go): the box is
	// reachable only through the node driving it, and what it has said so
	// far is on no record anywhere until the run is collected. Nil leaves
	// the question unregistered.
	SandboxTail SandboxTails

	// Config serves the config family, every one of which is read on the
	// configuration grant: the document names every ${VAR} the company
	// holds a credential under.
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

	// Work and Pages are this node's projections of the company's own
	// tracker and knowledge base. Nil leaves their questions unregistered,
	// which is the honest answer for a company on Jira and Confluence:
	// there is no native record for this node to have a copy of.
	//
	// Consumer-defined interfaces rather than the concrete readers, like
	// every other seam here — and the READ side only. Nothing on this
	// surface writes: a board's edit goes through a seat's tools or the
	// operator surface (internal/api/operator), both of which are
	// attributed to somebody.
	Work  WorkReader
	Pages PageReader

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

	// PublicBase is where a browser and a third-party app reach this
	// deployment — `api.external_url` — or nil when this process cannot
	// say.
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
	PublicBase func() string
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

	// CredentialPools is every LLM provider entry in this node's current
	// epoch, each configured key's provenance beside this node's pool state
	// for it — hints, never values (see [engine.Engine.CredentialPools]).
	// A FUNCTION for the reason [Sources.Company] is one: an apply replaces
	// every pool. Nil leaves `credential_pool` unregistered.
	CredentialPools func() []engine.CredentialPool

	// Cooldowns is the fleet's credential cooldown ledger, read so a key a
	// PEER benched is reported cooling here before this node's refresher
	// has pulled it. Nil answers with this node's cooldowns alone and says
	// so (`fleet: false`), rather than calling every key the fleet benched
	// ready.
	Cooldowns interface {
		Since(ctx context.Context, now time.Time) (map[string]time.Time, error)
	}

	// Backups is the fleet's backup register — each owner's newest
	// announced point, which is what the trim's backup term reads. With
	// [Sources.Events] it answers `backups`; nil leaves that unregistered.
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
		r.Register("agent", iam.GrantStateRead, s.agent)
		r.Register("tokens", iam.GrantStateRead, s.tokens)
	}
	if s.Events != nil {
		r.Register("events", iam.GrantAuditRead, s.events)
		// THE SAME ROWS WITH A TIME AXIS, which the listing has no
		// dimension for: a page of rows says what happened and nothing
		// about when the company was busy. A second question rather than
		// a flag on the first, because the two answers have different
		// shapes and one route returning either would make every caller
		// branch on what came back — the same split `tokens` and
		// `token_series` already carry.
		r.Register("event_series", iam.GrantAuditRead, s.eventSeries)
		r.Register("event", iam.GrantAuditRead, s.event)
		r.Register("trace", iam.GrantAuditRead, s.trace)
		// A turn is its own question, not a slice of the trace: one trace
		// can span several turns and one turn several traces. See the
		// answer, and migration 0014 which made it askable at all.
		r.Register("turn", iam.GrantAuditRead, s.turn)
		// AND THE LIST OF THEM, which did not exist: a turn is the unit
		// of work this engine does and every other surface is a
		// projection of one. The dashboard faked it by paging the raw
		// feed sixty-one times and folding in the browser.
		r.Register("turns", iam.GrantAuditRead, s.turns)
		// The company's phase records, with their payloads. `events` cannot
		// serve this: its listing never selects the payload, and a phase
		// record without one has no prompts, no response and no decision.
		r.Register("phases", iam.GrantAuditRead, s.phases)
	}
	if s.Usage != nil {
		// AND THE TIME AXIS. `tokens` is a breakdown whose every row is a
		// sum over the whole window, so it cannot say WHEN — which is the
		// question a cost explorer is for. Gated on the usage domain rather
		// than on the projection: its buckets are company days, which only
		// the replicated rows hold, and an axis that changed source when a
		// reader widened the range is a seam across the one comparison the
		// screen exists to make.
		r.Register("token_series", iam.GrantStateRead, s.tokenSeries)
		// EVERY SEAT'S TURNS over a window of company days, from the same
		// replicated rows: counts, the first-pass rate over reviewed turns,
		// merged duration quantiles and a day-by-day series. A screen
		// counting the rows of a list it loaded was counting the list.
		r.Register("seat_activity", iam.GrantStateRead, s.seatActivity)
		// WHO READ A PAGE, from every node's days — see pagereads.go.
		// Gated on the USAGE domain rather than on the native pages: a
		// read is recorded with its backend, so a company on Confluence
		// has readers too.
		r.Register("page_reads", iam.GrantStateRead, s.pageReads)
	}
	if s.Coord != nil {
		// THE DEPLOYMENT'S OWN SHAPE rather than the company's work: the
		// node ids, which node holds which seat, the lease epochs and how
		// far a config rollout has reached. So it asks the grant every
		// other control of the deployment asks — the same one `retention`
		// and `backups` take — rather than the board's read.
		r.Register("fleet", iam.GrantFleetOperate, s.fleet)
		// WHAT EACH MCP SERVER DID ON EACH NODE, off the same lease
		// table: every node re-publishes its starts on its presence
		// heartbeat, so one listing answers for the fleet. On the grant
		// that reads the company document, because that is what it
		// discloses — every server's launch command and the first line
		// of each failure, which `/config` shows the same reader. See
		// [Sources.mcpServersStatus].
		r.Register("mcp_servers_status", iam.GrantConfigRead, s.mcpServersStatus)
	}
	if s.Backups != nil && s.Events != nil {
		// THE DEPLOYMENT'S, for retention's reason and more: every row
		// names a directory on a named host that holds the company's
		// sealed credentials. Gated on BOTH halves, since an answer with
		// the register and no history would read as a fleet nobody ever
		// asked for a backup.
		r.Register("backups", iam.GrantFleetOperate, s.backups)
	}
	if s.CredentialPools != nil {
		// ON THE CONFIGURATION READ: which variable holds each model's
		// keys and which of them a vendor is refusing right now is a map
		// of which credential to take, and of when — what the company
		// document's own reader is already trusted with. See
		// [Sources.credentialPool].
		r.Register("credential_pool", iam.GrantConfigRead, s.credentialPool)
	}
	// WHO IS ASKING. Registered unconditionally: a process with no company
	// still has a credential presented to it, and "this token resolves to
	// no seat" is the answer a screen needs in order to say what to bind.
	r.Register("viewer", iam.GrantStateRead, s.viewer)
	if s.Runs != nil {
		// ONE SCHEDULE'S OWN HISTORY, gated on the LEDGER rather than on
		// the company: the configured rows are a projection of the org
		// and this is a store read, so a node with one and not the other
		// is a real shape.
		r.Register("schedule_runs", iam.GrantStateRead, s.scheduleRuns)
	}
	if s.Company != nil {
		// Gated on the COMPANY, not on the durable counter: the caps are
		// what the screen is about, and a counter that cannot be read
		// answers "these are the ceilings, and nobody can read the usage",
		// which is a real state an operator needs to see and is not the
		// same as the question being unavailable here.
		r.Register("budgets", iam.GrantStateRead, s.budgets)
		// Both are projections of the epoch: what the company DECLARES,
		// which is a different question from what it has done.
		r.Register("schedules", iam.GrantStateRead, s.schedules)
		// ON THE CONFIGURATION READ, alone among the projections, because
		// of what it projects. `/setup` is guarded in FULL — reads
		// included — for the reason its own package doc gives: "the list
		// of which credentials a company has NOT configured is a map of
		// what to attack." This answer is that same map, per surface:
		// which are configured, which hold a secret, which are
		// half-set-up, and the address each is registered against.
		r.Register("integrations", iam.GrantConfigRead, s.integrations)
		// Gated on the COMPANY, not on the searcher, for the same reason
		// budgets is: "this company has no knowledge backend configured" is
		// a fact the company alone establishes, and it is a far more useful
		// answer than an unknown query. A nil searcher IS the answer here,
		// not the absence of one.
		r.Register("knowledge", iam.GrantStateRead, s.knowledgeSearch)
		// A NAME TO A SEAT, through the tiers an agent's own lookup uses —
		// the command palette's assign and ask pickers. A projection of
		// the chart, so it rides the company like the others here.
		r.Register("colleague", iam.GrantStateRead, s.colleague)
	}
	if s.Sandbox != nil {
		r.Register("sandbox_runs", iam.GrantStateRead, s.sandboxRuns)
	}
	if s.SandboxTail != nil {
		// ASKED ONLY WHILE SOMEBODY WATCHES: a trace polls it while a
		// running coding run's span is open, and nothing else does. There
		// is no event and no row behind it — see [Sources.SandboxTail].
		// ON THE AUDIT READ, beside the trace it is read from: what a
		// coding agent prints is its transcript, as a phase record's
		// prompt and response are.
		r.Register("sandbox_tail", iam.GrantAuditRead, s.sandboxTail)
	}
	if s.Retention != nil {
		// THE DEPLOYMENT'S. The answer names every node in the fleet, its
		// position, its disk and its snapshot repository — a map of which
		// machine to take out to lose the company's history — and it is
		// read by a person or their cron.
		r.Register("retention", iam.GrantFleetOperate, s.retention)
	}
	// The NATIVE backends, each gated on its own reader: a company can run
	// the native tracker on Confluence, or the native knowledge base on
	// Jira, and registering the pair together would offer one screen a
	// question its half of the company cannot answer.
	if s.Work != nil {
		r.Register("work_items", iam.GrantStateRead, s.workItems)
		r.Register("work_item", iam.GrantStateRead, s.workItem)
		// A TASK'S THREAD, PAGED — the one collection on a detail with
		// no bound of its own, so the detail returns its newest page and
		// this walks the rest. See [Sources.workComments].
		r.Register("work_comments", iam.GrantStateRead, s.workComments)
		// A TASK'S TURNS, PAGED, from the tracker's own rows — the
		// durable account its cost panel sums. See
		// [Sources.workItemTurns].
		r.Register("work_item_turns", iam.GrantStateRead, s.workItemTurns)
		// A SEPARATE QUESTION from `work_items`, for the reason
		// `containers` is separate from `pages`: a screen draws the tab
		// strip once and the rows in it on every filter change.
		r.Register("work_views", iam.GrantStateRead, s.workViews)
		// AND EVERY VIEW THE CALLER CAN SEE, across the containers: the
		// inventory and the sidebar's pins are about the caller's own
		// views, not one container's tabs. See [Sources.workSavedViews].
		r.Register("work_saved_views", iam.GrantStateRead, s.workSavedViews)
		// A SEPARATE QUESTION from `work_items` for the reason
		// `work_views` is: a home screen draws the project list once
		// and its rows' tasks on every navigation, and the counts here
		// are three MAINTAINED columns rather than an aggregate over
		// every task in the company.
		r.Register("work_projects", iam.GrantStateRead, s.workProjects)
		r.Register("work_project", iam.GrantStateRead, s.workProject)
		// AND WHO IS CARRYING HOW MUCH, across every project at once.
		// A caller that grouped it itself paid one round trip per
		// project and rewrote the arithmetic per surface.
		r.Register("work_workload", iam.GrantStateRead, s.workWorkload)
		// THE FEED IS ITS OWN QUESTION, because it is ordered by the
		// LOG rather than by anything a board sorts on: one durable
		// table at any age, with a cursor that is a position.
		r.Register("work_activity", iam.GrantStateRead, s.workActivity)
		// AND ONE PERSON'S DAY, plus the notices that reached them.
		//
		// SCOPED RATHER THAN ADMIN-ONLY — see [Sources.recordHandle].
		// A caller reads their own record, and naming anybody else's is
		// the owner-or-lead rule the tools that WRITE these records are
		// decided by: theirs, whoever leads them, or the deployment's
		// admin grant. Registered admin-only, the landing screen becomes
		// the most-gated screen in the product and the human teammate —
		// one of the two readers this dashboard is for — is fictional.
		r.Register("work_my_work", iam.GrantStateRead, s.workMyWork)
		// THE READER HAS ALWAYS EXISTED and nothing asked it: eighteen
		// typed wake reasons, an addressed flag, a fallback flag and
		// this person's own read and snooze marks, swept on a 365-day
		// retention and reaching no screen.
		r.Register("work_inbox", iam.GrantStateRead, s.workInbox)
		// WHO ONE CHANGE WOKE. The applier has written the set
		// since the domain landed and its only trace on any surface
		// was `work_activity.notified`: a boolean saying that
		// somebody, somewhere, was told.
		r.Register("work_routing", iam.GrantStateRead, s.workRouting)
		r.Register("work_catalogue", iam.GrantStateRead, s.workCatalogue)
		r.Register("work_person", iam.GrantStateRead, s.workPerson)
		// THE LANDING SCREEN'S THREE. The series is the tracker's history
		// replayed backward from today's census; the feed merges the
		// tracker's completions, creates and hand-offs with the pages'
		// and the schedules' where this node keeps them; and what is
		// waiting on a person is their open asks beside the coding runs
		// parked on a question to them — scoped as `work_my_work` is. See
		// home.go.
		r.Register("work_flow", iam.GrantStateRead, s.workFlow)
		r.Register("company_feed", iam.GrantStateRead, s.companyFeed)
		r.Register("decisions", iam.GrantStateRead, s.decisions)
	}
	if s.Pages != nil {
		r.Register("pages", iam.GrantStateRead, s.pageList)
		r.Register("page", iam.GrantStateRead, s.page)
		// A SEPARATE QUESTION from `pages`, not a facet of it: a browser
		// draws the container list once and the page list on every
		// navigation, and folding them together would ship every
		// container's record with every page listing.
		r.Register("containers", iam.GrantStateRead, s.containers)
		// WHAT HAPPENED TO THE PAGES, which `pages_history` has recorded
		// since the domain landed with two indexes naming readers nobody
		// wrote — and ONE REVISION'S BODY, which the detail's summaries
		// could say existed and never show.
		r.Register("page_activity", iam.GrantStateRead, s.pageActivity)
		r.Register("page_revision", iam.GrantStateRead, s.pageRevision)
	}
	if s.Memory != nil {
		// A SEAT'S TRAIL — what it remembers and what it said — ANSWERED
		// BY THE HOLDER, and saying which node that was (see
		// [Sources.Memory]). Registered on the audit read and decided per
		// seat by [authz.ActionSeatTrailRead], the one verb all three ask,
		// so the halves of what a seat has said and remembered cannot
		// answer one reader differently.
		r.Register("agent_memory", iam.GrantAuditRead, s.agentMemory)
		// EVERY AGENT SEAT'S TOTALS IN ONE ANSWER, each counted by its
		// holder in one scatter — what the diaries list draws, where a
		// read per seat would be a lease read and a scatter per row.
		r.Register("memory_overview", iam.GrantAuditRead, s.memoryOverview)
		// An absent handle is the caller's own seat; see
		// [Sources.trailSeat].
		r.Register("conversations", iam.GrantAuditRead, s.conversations)
	}
	if s.Channels != nil {
		r.Register("a2a_channels", iam.GrantAuditRead, s.a2aChannels)
	}
	if s.WorkSearch != nil {
		// SEARCH IS A QUESTION, not a filter on the board, and it is
		// gated on its own index rather than on the tracker: the ranked
		// reader is what `search_work_items` gives a seat, and the operator
		// reading the same company had only `q=` — an escaped LIKE over
		// the excerpt, gated to a span of days.
		r.Register("work_search", iam.GrantStateRead, s.workSearch)
	}
	if s.Config != nil {
		// ON THE CONFIGURATION READ, all four. Reading the config
		// document exposes the whole company — which integrations are
		// wired, and every ${VAR} reference by name.
		r.Register("config", iam.GrantConfigRead, s.configDocument)
		r.Register("config_audit", iam.GrantConfigRead, s.configAudit)
		r.Register("config_diff", iam.GrantConfigRead, s.configDiff)
		r.Register("config_entities", iam.GrantConfigRead, s.configEntities)
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
	// other's live call and read each other's transcript. A retired handle
	// still resolves, through the chart's own aliases, so a link somebody
	// kept names the seat it named when they kept it.
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
	history, next, coverage := s.phaseHistory(ctx, agentID, p)
	answer := map[string]any{
		"handle":   seat.Handle(),
		"agent_id": agentID,
		"role":     seat.Name,
		"live":     nil,
		// The rows are `store.EventRecord`s, PAYLOAD NESTED — the same
		// shape `event`, `trace` and `turn` answer with. They used to be
		// the payload flattened with an id and a timestamp merged in,
		// which meant one client had to carry two readers for one kind of
		// thing and a field added to the envelope reached three screens
		// and not the fourth.
		"llm_history": history,
		// The cursor the caller pages with, or "" at the end of the
		// record. Its absence is why a seat's transcript was a hard fifty
		// rows with no way past them, while the events it was made of sat
		// in the store addressable by id.
		"next": next,
		// WHICH NODES THE HISTORY CAME FROM. A seat moves between nodes,
		// so its history is on every node that ever held it; null when it
		// could not be read at all, which is not the same as a seat that
		// has never run.
		"coverage": coverage,
	}
	// ASSIGNED ONLY WHEN THERE IS ONE, because a nil *Overlay stored in an
	// any is not nil: every `live != nil` check above this layer would read
	// a seat that has never run as one that has. It marshals to null either
	// way, so the client never saw it and only a Go caller would — which is
	// exactly the kind of trap that survives until something depends on it.
	if overlay := s.State.AgentOverlay(agentID); overlay != nil {
		answer["live"] = overlay
	}
	return answer, nil
}

// agentSeat resolves a handle — current, created-under or retired — to the
// AGENT seat answering to it on this node's chart, and that seat's agent id.
// Nil for a handle no agent seat answers to, and on a node with no chart view
// to ask.
func (s Sources) agentSeat(handle string) (*org.Role, string) {
	if handle == "" || s.Company == nil {
		return nil, ""
	}
	company, roster := s.Company()
	// THE COMPANY'S OWN ORG, derived from this node's chart rows rather
	// than re-resolved from the document: a stored revision carries no
	// seats at all.
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
// BY THE SEAT'S AGENT ID, the one identifier every phase row carries that
// neither a rename nor a namesake moves — never its role name, which a unit
// template stamps onto every seat it makes.
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
	// HANDLE — any handle it answers to — resolved through the chart this
	// node holds, exactly as the event log resolved the handles each event
	// named when it was written (see store.DB.SetEventSeats). A handle no
	// agent seat answers to is refused rather than compared: there is no
	// id it could match, and an empty page reads as an agent that has done
	// nothing.
	if q.RelatedAgent != "" {
		id, ok := s.agentSeatID(q.RelatedAgent)
		if !ok {
			return nil, fmt.Errorf("%w: agent %q names no agent seat in this "+
				"company — the related filter is about an agent; a person's "+
				"events are the ones they acted in, under `actor`",
				ErrBadParams, q.RelatedAgent)
		}
		q.RelatedAgent = id
	}
	q.Limit = Clamp(p.Int("limit", 0), DefaultEventPage, MaxEventPage)
	if before := p.String("before_id"); before != "" {
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		at, err := time.Parse(time.RFC3339Nano, p.String("before_time"))
		if err != nil {
			return nil, fmt.Errorf("%w: before_id needs a before_time: %w", ErrBadParams, err)
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
		return nil, fmt.Errorf("%w: bucket %q is not one of %v",
			ErrBadParams, bucket, store.EventBuckets)
	}
	got, coverage, err := s.Events.Histogram(ctx, store.HistogramQuery{ListQuery: filters, Bucket: bucket})
	switch {
	case errors.Is(err, store.ErrHistogramBucket),
		errors.Is(err, store.ErrHistogramSpan),
		errors.Is(err, store.ErrHistogramRelated):
		// A REQUEST THIS SURFACE UNDERSTOOD AND REFUSED, so it answers
		// 400 with the store's own sentence rather than 500 with
		// "something went wrong": every one of these names the parameter
		// the caller has to change.
		return nil, fmt.Errorf("%w: %w", ErrBadParams, err)
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
		// TURN_ID WAS DECLARED, DOCUMENTED AGAINST MIGRATION 0014, AND
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
	// parked on a coding run and resumed twice.
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
		return store.ListQuery{}, fmt.Errorf("%w: until (%s) is not after since (%s) — the "+
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
		return "", fmt.Errorf("%w: work_item=%q is not a work item's identity — "+
			"want `<backend>:<id>` (for example `native:<task id>` or `jira:<issue id>`), "+
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
// RESOLVED HERE, server-side, through the chart this node holds: the handle it
// answers to now, the one it was created under, or one a rename retired. The
// id is derived from the handle the seat was CREATED under (ADR-0026), so
// deriving it from whatever was typed — which the alternative did — named a
// different seat, or none, the moment a seat had been renamed. Neither
// alternative a caller had was a seat's identity either: a role name is shared
// by every seat a unit template stamps out and changes with a rename, and a raw
// agent id is a derivation every client would have to repeat.
//
// A HANDLE THE CHART NO LONGER HOLDS still names the history its seat left, by
// the handle that seat was created under — which the chart never issues to
// anybody else — so it is derived as every node derived it while the seat ran.
//
// Refused rather than matched when it is not a handle, or names a PERSON's
// seat, because either would match nothing and an empty answer reads as a seat
// that never did anything; and UNAVAILABLE before a company is applied, since
// the id is derived from the company's name and there is none to derive it
// from.
func (s Sources) seatAgentID(handle string) (string, error) {
	if !org.ValidHandle(handle) {
		return "", fmt.Errorf("%w: seat=%q is not a handle — a seat is named by "+
			"its handle (lowercase, as on its page), never its role name", ErrBadParams, handle)
	}
	organization := s.organization()
	if organization == nil {
		return "", fmt.Errorf("%w: seat=%q cannot be resolved until a company "+
			"configuration is applied on this node — a seat's id is derived from "+
			"the company's name", ErrUnavailable, handle)
	}
	if role := organization.Role(handle); role != nil {
		id, ok := organization.AgentIDFor(role)
		if !ok {
			return "", fmt.Errorf("%w: seat=%q is %s, a human seat — the engine "+
				"runs no turns for a person, so nothing is filed under it; a "+
				"person's events are the ones they acted in, under `actor`",
				ErrBadParams, handle, role.Name)
		}
		return id.String(), nil
	}
	id, ok := org.DeriveAgentID(organization.Name, handle)
	if !ok {
		return "", fmt.Errorf("%w: seat=%q cannot be resolved until the company "+
			"this node runs has a name — a seat's id is derived from it",
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
		return time.Time{}, fmt.Errorf("%w: %s must be an RFC 3339 instant "+
			"(2026-04-16T09:00:00Z), and %q is not: %w", ErrBadParams, name, raw, err)
	}
	return at.UTC(), nil
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
	// Additive, so a client that predates the field is unaffected.
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
