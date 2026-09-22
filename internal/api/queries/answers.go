package queries

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tokens"
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
// screen must be able to tell apart, and "not here" is permanent in a way
// [ErrUnavailable]'s "not yet" is not, so a Retry-After for a source this node
// will never have would send a client round a loop.
//
// Only a few are absent on a real node: Work and Pages on a company that runs
// the vendor tracker and wiki, Knowledge where no knowledge backend is
// configured, and Retention where no state log runs. The rest are held by every
// node, and the API wires every one of them. Leaving one of those out is what a
// caller that asks a subset of the questions does, which is how this package's
// own suite exercises one question at a time.
type Sources struct {
	State  *livestate.LiveState
	Events *store.EventLog

	// Health answers the stream question, which is deliberately not called
	// health: a query must never share a name with a push kind, or a
	// reader of the protocol has to know which direction a frame was
	// travelling to know what it means.
	Health func(ctx context.Context) any

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

	// Diary, Episodes and Skills are a seat's memory. Skills is the half
	// that had no query at all: learning.Skills.List exists and is tested,
	// and nothing served it, so a seat's synthesized skills were written,
	// versioned, loadable by the agent itself — and invisible to the
	// operator whose tokens paid for them.
	Diary    *learning.Diary
	Episodes *learning.Episodes
	Skills   *learning.Skills

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
		Usage(ctx context.Context) ([]coord.Usage, error)
	}

	// Sandbox is the durable record of detached coding runs, the fleet's
	// rather than this node's. Nil leaves the question unregistered.
	Sandbox PendingRuns

	// Config serves the config family, and every one of those is
	// operator-gated: reading the document exposes the whole company.
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

	// Work and Pages are this node's projections of the company's own
	// tracker and knowledge base. Nil leaves their questions unregistered,
	// which is the honest answer for a company on Jira and Confluence:
	// there is no native record for this node to have a copy of.
	//
	// Consumer-defined interfaces rather than the concrete readers, like
	// every other seam here — and the READ side only. Nothing on this
	// surface writes: a board's edit goes through a seat's tools or the
	// operator MCP, both of which are attributed to somebody.
	Work  WorkReader
	Pages PageReader

	// WorkSearch is the ranked item search, and it is SEPARATE from
	// [Sources.Work] because the two fail independently: the rows are the
	// fleet's and the lexical index is this node's own, so a node still
	// building one answers every board question and cannot rank a word.
	// Nil leaves `work_search` unregistered, which is what a screen needs
	// in order to offer the board's filters instead of an empty ranking.
	WorkSearch WorkSearcher

	// Conversations is the seat's own thread ledger — what it has said on
	// a surface this engine does not own, and the record that stops it
	// replying twice in one thread. Typed on the client since the client
	// had types and registered nowhere, so the panel that reads it drew an
	// empty list for every seat in every company.
	Conversations Conversations

	// Counterparties is what the learning loop remembers about WHO a seat
	// has worked with — the one memory object that is about somebody else,
	// and the one the memory answer never carried.
	Counterparties Counterparties

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

	// NodeID names this node in the fleet answer, so a reader can tell
	// which row is the one they are talking to. The RESOLVED id
	// (config.ResolveNodeID), which is also the name the node's presence
	// lease carries, never the raw `node.id` field.
	NodeID string

	// Now is injectable so a test can pin the lease countdowns and the
	// next-run projection.
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

// clock reads the injected time, or the wall clock.
func (s Sources) clock() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now()
}

// ErrUnavailable is a question this node understood and cannot answer YET: its
// copy of the company's records is still catching up, or the coordination store
// it reads could not be reached.
//
// Distinct from an empty answer, and the distinction is the point: a dashboard
// that drew "there is no work" for "this node cannot read the work right now"
// would report a quiet company during an outage. Distinct from [ErrUnknown]
// too, which is the answer for a source this node does not have at all: that
// one never clears by waiting, so it must not carry a Retry-After.
//
// Answers do not return it themselves. The registry classifies at one boundary
// (see unavailableIfTransient), so an answer that reads a store which can be
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
		r.Register("agent", s.agent)
		r.Register("tokens", s.tokens)
	}
	if s.Events != nil {
		r.Register("events", s.events)
		// THE SAME ROWS WITH A TIME AXIS, which the listing has no
		// dimension for: a page of rows says what happened and nothing
		// about when the company was busy. A second question rather than
		// a flag on the first, because the two answers have different
		// shapes and one route returning either would make every caller
		// branch on what came back — the same split `tokens` and
		// `token_series` already carry.
		r.Register("event_series", s.eventSeries)
		r.Register("event", s.event)
		r.Register("trace", s.trace)
		// A turn is its own question, not a slice of the trace: one trace
		// can span several turns and one turn several traces. See the
		// answer, and migration 0014 which made it askable at all.
		r.Register("turn", s.turn)
		// AND THE LIST OF THEM, which did not exist: a turn is the unit
		// of work this engine does and every other surface is a
		// projection of one. The dashboard faked it by paging the raw
		// feed sixty-one times and folding in the browser.
		r.Register("turns", s.turns)
		// The company's phase records, with their payloads. `events` cannot
		// serve this: its listing never selects the payload, and a phase
		// record without one has no prompts, no response and no decision.
		r.Register("phases", s.phases)
		// AND THE TIME AXIS. `tokens` is a breakdown whose every row is a
		// sum over the whole window, so it cannot say WHEN — which is the
		// question a cost explorer is for. Gated on the event store rather
		// than on the projection: the projection holds a day, and an axis
		// that changed source when a reader widened the range is a seam
		// across the one comparison the screen exists to make.
		r.Register("token_series", s.tokenSeries)
	}
	if s.Health != nil {
		r.Register("stream", s.stream)
	}
	if s.Coord != nil {
		// OPERATOR-ONLY, like every other answer the Admin workspace
		// draws. It reports the node ids, which node holds which seat,
		// the lease epochs and how far a config rollout has reached —
		// the shape of the deployment rather than the company's work.
		// The dashboard already locks the row and its palette entry
		// says "needs a token", and its own sidebar asks this beside
		// `integrations`, which has always been operator-only. So this
		// was the one destination of the five where the client claimed
		// a guard the server did not keep, and on a node with
		// `api.allow_anonymous_read` an anonymous GET read all of it.
		r.RegisterOperator("fleet", s.fleet)
	}
	// WHO IS ASKING. Registered unconditionally: a process with no company
	// still has a credential presented to it, and "this token resolves to
	// no seat" is the answer a screen needs in order to say what to bind.
	r.Register("viewer", s.viewer)
	if s.Runs != nil {
		// ONE SCHEDULE'S OWN HISTORY, gated on the LEDGER rather than on
		// the company: the configured rows are a projection of the org
		// and this is a store read, so a node with one and not the other
		// is a real shape.
		r.Register("schedule_runs", s.scheduleRuns)
	}
	if s.Company != nil {
		// Gated on the COMPANY, not on the durable counter: the caps are
		// what the screen is about, and a counter that cannot be read
		// answers "these are the ceilings, and nobody can read the usage",
		// which is a real state an operator needs to see and is not the
		// same as the question being unavailable here.
		r.Register("budgets", s.budgets)
		// Both are projections of the epoch: what the company DECLARES,
		// which is a different question from what it has done.
		r.Register("schedules", s.schedules)
		// OPERATOR-ONLY, alone among the projections, because of what it
		// projects. `/setup` is guarded in FULL — reads included — for the
		// reason its own package doc gives: "the list of which credentials
		// a company has NOT configured is a map of what to attack." This
		// answer is that same map, per surface: which are configured, which
		// hold a secret, which are half-set-up, and the address each is
		// registered against. Serving it anonymously guarded the write and
		// published the reconnaissance.
		r.RegisterOperator("integrations", s.integrations)
		// Gated on the COMPANY, not on the searcher, for the same reason
		// budgets is: "this company has no knowledge backend configured" is
		// a fact the company alone establishes, and it is a far more useful
		// answer than an unknown query. A nil searcher IS the answer here,
		// not the absence of one.
		r.Register("knowledge", s.knowledgeSearch)
	}
	if s.Sandbox != nil {
		r.Register("sandbox_runs", s.sandboxRuns)
	}
	if s.Retention != nil {
		// OPERATOR-ONLY. The answer names every node in the fleet, its
		// position, its disk and its snapshot repository — a map of
		// which machine to take out to lose the company's history — and
		// it is read by a person or their cron, never by the dashboard's
		// anonymous shell.
		r.RegisterOperator("retention", s.retention)
	}
	// The NATIVE backends, each gated on its own reader: a company can run
	// the native tracker on Confluence, or the native knowledge base on
	// Jira, and registering the pair together would offer one screen a
	// question its half of the company cannot answer.
	if s.Work != nil {
		r.Register("work_items", s.workItems)
		r.Register("work_item", s.workItem)
		// A SEPARATE QUESTION from `work_items`, for the reason
		// `containers` is separate from `pages`: a screen draws the tab
		// strip once and the rows in it on every filter change.
		r.Register("work_views", s.workViews)
		// A SEPARATE QUESTION from `work_items` for the reason
		// `work_views` is: a home screen draws the project list once
		// and its rows' tasks on every navigation, and the counts here
		// are three MAINTAINED columns rather than an aggregate over
		// every task in the company.
		r.Register("work_projects", s.workProjects)
		r.Register("work_project", s.workProject)
		// AND WHO IS CARRYING HOW MUCH, across every project at once.
		// A caller that grouped it itself paid one round trip per
		// project and rewrote the arithmetic per surface.
		r.Register("work_workload", s.workWorkload)
		// THE FEED IS ITS OWN QUESTION, because it is ordered by the
		// LOG rather than by anything a board sorts on: one durable
		// table at any age, with a cursor that is a position.
		r.Register("work_activity", s.workActivity)
		// AND ONE PERSON'S DAY, plus the notices that reached them.
		//
		// SCOPED RATHER THAN OPERATOR-ONLY — see [Sources.viewerHandle].
		// A caller reads the seat their own token is bound to, and
		// naming anybody else's needs an operator credential, which is
		// the same authority the tools that WRITE these records
		// enforce. Registered operator-only, as `work_my_work` was, the
		// landing screen becomes the most-gated screen in the product
		// and the human teammate — one of the two readers this
		// dashboard is for — is fictional. `work_person` has always
		// been registered ungated, so this is what already ships rather
		// than a new posture.
		r.Register("work_my_work", s.workMyWork)
		// THE READER HAS ALWAYS EXISTED and nothing asked it: twenty
		// typed wake reasons, an addressed flag, a fallback flag and
		// this person's own read and snooze marks, swept on a 365-day
		// retention and reaching no screen.
		r.Register("work_inbox", s.workInbox)
		// WHO ONE CHANGE WOKE. The applier has written the set
		// since the domain landed and its only trace on any surface
		// was `work_activity.notified`: a boolean saying that
		// somebody, somewhere, was told.
		r.Register("work_routing", s.workRouting)
		r.Register("work_catalogue", s.workCatalogue)
		r.Register("work_person", s.workPerson)
	}
	if s.Pages != nil {
		r.Register("pages", s.pageList)
		r.Register("page", s.page)
		// A SEPARATE QUESTION from `pages`, not a facet of it: a browser
		// draws the container list once and the page list on every
		// navigation, and folding them together would ship every
		// container's record with every page listing.
		r.Register("containers", s.containers)
		// WHAT HAPPENED TO THE PAGES, which `pages_history` has recorded
		// since the domain landed with two indexes naming readers nobody
		// wrote — and ONE REVISION'S BODY, which the detail's summaries
		// could say existed and never show.
		r.Register("page_activity", s.pageActivity)
		r.Register("page_revision", s.pageRevision)
	}
	if s.Diary != nil || s.Episodes != nil || s.Skills != nil ||
		s.Counterparties != nil {

		// FOUR HALVES NOW. Each is gated inside the answer rather than
		// here, so a node holding one of them answers with that one and
		// empty lists for the rest — which is what a client needs to
		// tell "this seat has learned nothing" from "this node does not
		// keep that half".
		r.Register("agent_memory", s.agentMemory)
	}
	if s.Channels != nil {
		r.Register("a2a_channels", s.a2aChannels)
	}
	if s.WorkSearch != nil {
		// SEARCH IS A QUESTION, not a filter on the board, and it is
		// gated on its own index rather than on the tracker: the ranked
		// reader is what `search_work` gives a seat, and the operator
		// reading the same company had only `q=` — an escaped LIKE over
		// the excerpt, gated to a span of days.
		r.Register("work_search", s.workSearch)
	}
	if s.Conversations != nil {
		// SCOPED, like every other per-seat question — see
		// [Sources.viewerHandle].
		r.Register("conversations", s.conversations)
	}
	if s.Config != nil {
		// OPERATOR-ONLY, all three. Reading the config document exposes
		// the whole company — its org chart, which integrations are
		// wired, and every ${VAR} reference by name — which is what makes
		// /config the one prefix never eligible for anonymous read.
		r.RegisterOperator("config", s.configDocument)
		r.RegisterOperator("config_audit", s.configAudit)
		r.RegisterOperator("config_diff", s.configDiff)
		r.RegisterOperator("config_entities", s.configEntities)
	}
}

// agent answers one seat's live state.
func (s Sources) agent(ctx context.Context, p Params) (any, error) {
	// EITHER NAME, and the seat may be addressed by handle or by role.
	//
	// The dashboard sends `id`, carrying the handle — `query("agent", {id})`
	// from the seat page, and /agents/{id} from the REST table — while the
	// projection keys its overlays by ROLE NAME, which is what the engine's
	// `agents` push carries. Reading only `role` meant every seat page
	// answered 400 and rendered its error state; the client is the
	// compatibility reference for a frame's shape, so the answer
	// takes what the client sends and resolves it.
	seat := firstOf(p.String("id"), p.String("role"))
	role := s.roleOf(seat)
	if role == "" {
		return nil, fmt.Errorf("%w: agent needs a handle or a role", ErrBadParams)
	}
	// LIVE STATE AND HISTORY, which are two different sources and always
	// were: the projection holds the call in flight, the event store holds
	// the ones that finished. The answer carried only the first, so a seat
	// page showed the round happening now and nothing before it — while the
	// spend chart beside it, which reads the store, reported every phase
	// the turn had already completed. Two panels on one screen disagreeing
	// about whether a seat had done anything.
	//
	// A seat the projection has never seen is NOT an error: a role
	// configured and never spawned is exactly that, and a 404 there would
	// make a healthy new company look broken. Its history is answered the
	// same way.
	history, next := s.phaseHistory(ctx, seat, role, p)
	answer := map[string]any{
		"role": role,
		"live": nil,
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
	}
	// ASSIGNED ONLY WHEN THERE IS ONE, because a nil *Overlay stored in an
	// any is not nil: every `live != nil` check above this layer would read
	// a seat that has never run as one that has. It marshals to null either
	// way, so the client never saw it and only a Go caller would — which is
	// exactly the kind of trap that survives until something depends on it.
	if overlay := s.State.AgentOverlay(role); overlay != nil {
		answer["live"] = overlay
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
// model, response, tool_executions, total_tokens, cost_usd — because the same
// shape drives the live row, and the timestamp is the one field that lives on
// the envelope rather than inside it.
func (s Sources) phaseHistory(ctx context.Context, seat, role string, p Params) ([]store.EventRecord, string) {
	if s.Events == nil {
		return []store.EventRecord{}, ""
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
	records, err := s.Events.AgentPhases(ctx, s.agentIDOf(seat), role, before)
	if err != nil {
		log.WarnContext(ctx, "agent_history_unavailable", "seat", seat, "error", err)
		return []store.EventRecord{}, ""
	}
	if len(records) == 0 {
		// EMPTINESS, not nil-ness, and the two are not interchangeable here:
		// `AgentPhases` answers a nil slice for a seat it cannot name, an
		// allocated empty one for a seat with no phases yet, and the index
		// below is out of range on BOTH. Testing for nil alone left a seat
		// whose history read came back empty indexing a slice of length zero.
		return []store.EventRecord{}, ""
	}
	// The cursor is the LAST row's key, echoed rather than left for a client
	// to assemble: (time, id) is the table's key, and a client rebuilding it
	// from a rendered timestamp would lose the sub-second precision the
	// tiebreak depends on. Offered only on a FULL page — a short one is the
	// end of the record, and a cursor there would page for ever.
	next := ""
	if len(records) == store.AgentPhaseLimit {
		last := records[len(records)-1]
		next = last.Time.UTC().Format(time.RFC3339Nano) + "|" + last.ID
	}
	return records, next
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

// roleOf resolves a seat identifier to the ROLE NAME the projection keys on.
//
// A handle resolves through the org; anything else is passed through as a role
// name, so a caller that already had one is unaffected. An unknown identifier
// comes back unchanged rather than empty: the answer for a seat the projection
// has never seen is a live-state-free row, not an error, and a company that
// renamed a role should not turn a bookmarked page into a failure.
func (s Sources) roleOf(id string) string {
	if id == "" || s.Company == nil {
		return id
	}
	company, roster := s.Company()
	if company == nil {
		return id
	}
	// THE COMPANY'S OWN ORG, derived from this node's chart rows rather
	// than re-resolved from the document: a stored revision carries no
	// seats at all, so the derivation this replaced answered an EMPTY
	// organization for every running company.
	if roster == nil {
		return id
	}
	if role := roster.AgentSeatByHandle(id); role != nil {
		return role.Name
	}
	return id
}

// tokens answers the live spend window.
// tokens answers the spend breakdown.
//
// TWO SOURCES, one aggregation. The live projection holds the records for its
// own window and answers instantly; any other window is a scan of the event
// store. Both are folded by internal/tokens, so the number a reader sees when
// they change the window is comparable with the one they were looking at — a
// second implementation for the second source is precisely how those two came
// to disagree before.
//
// The live window is the default because it is what the dashboard opens on: a
// page load that scanned the store would put a query on the critical path of
// every tab, for an answer already in memory.
func (s Sources) tokens(ctx context.Context, p Params) (any, error) {
	opts := tokens.Options{
		Handles:     s.RoleHandles(),
		AgentRole:   p.String("agent_role"),
		RecentTurns: Clamp(p.Int("recent_turns", 0), tokens.DefaultRecentTurns, tokens.MaxRecentTurns),
	}
	// THE WINDOW AS TWO INSTANTS, which `since_days` cannot name: a
	// time-range control produces two edges and they need not end at now.
	// The same pair the series takes, and the same refusal, so a reader who
	// scrubs to a window sees the chart and the figures above it move
	// together rather than one of them staying anchored to this afternoon.
	since, err := instantParam(p, "since")
	if err != nil {
		return nil, err
	}
	until, err := instantParam(p, "until")
	if err != nil {
		return nil, err
	}
	if !since.IsZero() && !until.IsZero() && !until.After(since) {
		return nil, fmt.Errorf("%w: until (%s) is not after since (%s) — the "+
			"window is half-open, so an empty one names no rows at all",
			ErrBadParams, until.Format(time.RFC3339), since.Format(time.RFC3339))
	}
	live := livestate.LiveSpendWindowDays()
	days := p.Int("since_days", live)
	q := store.PhaseTokenQuery{
		SinceDays: days,
		Since:     since,
		Until:     until,
		AgentRole: opts.AgentRole,
	}
	// LABELLED WITH WHAT THE STORE WILL ACTUALLY COVER, never with what was
	// asked for: `since` is floored at the retention window, so a request
	// for a year answered over thirty days and headed "a year" is a lie
	// about the numbers beside it.
	opts.Since, opts.Until = q.Window(time.Now())

	// The live window, unfiltered, is the one the projection can answer —
	// and only when the caller named no instants of their own, since the
	// projection holds one rolling window and cannot look behind it.
	if since.IsZero() && until.IsZero() && days == live && opts.AgentRole == "" {
		return tokens.Aggregate(s.State.SpendRecords(), opts), nil
	}
	if s.Events == nil {
		// A registry wired without the event log (a caller asking only the
		// projection's questions) cannot see this window. The honest answer
		// is an EMPTY rollup labelled with the window asked for, not the
		// live one relabelled, which would put a week's heading over an
		// hour's numbers.
		return tokens.Aggregate(nil, opts), nil
	}
	records, err := s.Events.PhaseTokens(ctx, q)
	if err != nil {
		return nil, err
	}
	return tokens.Aggregate(records, opts), nil
}

// RoleHandles maps each seat's role name to its handle, for the per-agent
// rollup's cross-links. Empty when no revision is active, which links to
// nothing rather than guessing a handle.
//
// Exported because the live stream needs the same map for the rollup it
// pushes: two derivations of "which handle is this role" is how a pushed row
// and a queried one come to link to different pages.
func (s Sources) RoleHandles() map[string]string {
	out := map[string]string{}
	if s.Company == nil {
		return out
	}
	_, roster := s.Company()
	if roster == nil {
		return out
	}
	// EVERY SEAT THE COMPANY RUNS. This walked `company.Roles` — the
	// document's TOP-LEVEL list — so a seat inside a unit had no
	// cross-link in the rollup at all, which in a company with an org
	// chart is most of them; and once a stored revision stopped carrying
	// seats the map was empty for every company, so every per-agent row
	// linked nowhere.
	for role := range roster.AllRoles() {
		if role.Name == "" {
			continue
		}
		// THE SEAT'S OWN HANDLE, not a re-spelling of the derivation: a
		// handle that differs from the seat's real one is a cross-link
		// to a page that does not exist.
		out[role.Name] = role.Handle()
	}
	return out
}

// stream answers the engine's health.
func (s Sources) stream(ctx context.Context, _ Params) (any, error) {
	return s.Health(ctx), nil
}

// events answers a page of the log.
//
// The filters are the store's own, passed through rather than re-implemented:
// a listing this surface filtered itself would page differently from one the
// store filtered, and the difference shows up as rows that vanish when a reader
// scrolls.
func (s Sources) events(ctx context.Context, p Params) (any, error) {
	q, err := eventFilters(p)
	if err != nil {
		return nil, err
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

	rows, err := s.Events.List(ctx, q)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"events": rows,
		// The cursor the caller pages with next, echoed rather than left
		// for a client to assemble: (time, id) is the table's key and a
		// client that built it from the last row's fields would be
		// reimplementing the one thing that must not drift.
		"next": cursorOf(rows),
		// A page shorter than the limit does NOT mean history is
		// exhausted when a related-agent filter is set: that filter
		// over-fetches and post-filters, so only a zero-row page ends
		// the walk. Saying so beats a client inferring it wrongly.
		"exhausted": len(rows) == 0,
	}, nil
}

// eventSeries answers the log's own time axis.
//
// THE FILTERS ARE THE LISTING'S, read through the same function, because the
// two are halves of one screen: a bar counting rows the list below it would not
// show is worse than no bar at all. The store compiles both from one predicate;
// this makes sure both are handed the same one.
func (s Sources) eventSeries(ctx context.Context, p Params) (any, error) {
	filters, err := eventFilters(p)
	if err != nil {
		return nil, err
	}
	bucket := store.EventBucket(p.String("bucket"))
	if !bucket.Valid() {
		return nil, fmt.Errorf("%w: bucket %q is not one of %v",
			ErrBadParams, bucket, store.EventBuckets)
	}
	got, err := s.Events.Histogram(ctx, store.HistogramQuery{ListQuery: filters, Bucket: bucket})
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
	return got, nil
}

// eventFilters reads the filters both the listing and its axis take.
//
// ONE READER, for the reason the store has one predicate: a filter added to the
// list and forgotten here would draw an axis over a wider set than the rows
// beneath it, silently.
func eventFilters(p Params) (store.ListQuery, error) {
	q := store.ListQuery{
		Type:         p.String("type"),
		Source:       p.String("source"),
		Category:     p.String("category"),
		TraceID:      p.String("trace_id"),
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
	rec, err := s.Events.ByID(ctx, id)
	if err != nil {
		// A DEAD LINK IS NOT A BROKEN NODE. `store.ErrNotFound` is not
		// [ErrNotFound], so passing it through untranslated classified an id
		// that simply is not in the log as `query_failed` — which tells a
		// reader the server is faulty and tells an operator to go looking for
		// a fault there is none of. Every event id on the dashboard is a link
		// somebody can follow after the 30-day window has closed over it, so
		// this is the ordinary case rather than the exotic one.
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: event %s", ErrNotFound, id)
		}
		return nil, err
	}
	return rec, nil
}

// trace answers every row sharing one trace.
func (s Sources) trace(ctx context.Context, p Params) (any, error) {
	id := p.String("trace_id")
	if id == "" {
		return nil, fmt.Errorf("%w: trace needs a trace_id", ErrBadParams)
	}
	rows, err := s.Events.Trace(ctx, id)
	if err != nil {
		return nil, err
	}
	// SAYS WHEN IT CUT. EventLog.Trace stops at store.MaxTraceEvents — a
	// trace shown short with no note reads as a complete causal chain that
	// simply ends, which is the one thing a reader must not conclude from it.
	// Additive, so a client that predates the field is unaffected.
	//
	// ASKED, NOT INFERRED, for the reason the turn answer gives at length: a
	// trace of exactly the cap holds every row it has, and `len(rows) == cap`
	// reports it cut. That is a caution badge on a complete trace, which is
	// the same class of lie as the note's absence and costs one indexed count
	// on the reads that filled.
	//
	// DEGRADES like `turn`'s does: the rows are in hand, and failing the
	// whole answer because a follow-up count could not be taken would turn
	// the largest traces — the only ones that reach this branch — into
	// `query_failed`. A missing caution badge beats a missing screen.
	truncated := false
	if len(rows) >= store.MaxTraceEvents {
		total, err := s.Events.TraceEventCount(ctx, id)
		if err != nil {
			log.WarnContext(ctx, "trace_extent_unavailable", "trace", id, "error", err)
		} else {
			truncated = total > len(rows)
		}
	}
	return map[string]any{
		"trace_id":  id,
		"events":    rows,
		"truncated": truncated,
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
