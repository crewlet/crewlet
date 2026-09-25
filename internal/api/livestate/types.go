package livestate

// The wire shapes. Every json tag here is part of the frozen dashboard
// protocol: the client ships
// unchanged and is the compatibility reference, so a renamed field is a broken
// dashboard, not a refactor.

import (
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
)

// Envelope is one serialized event as the dashboard sees it.
//
// Payload stays untyped on purpose. This projection is fed engine events AND
// webhook events through one path — that is what keeps them on the same code
// path rather than in two state machines — and a webhook payload has no Go type
// to decode into. The fields actually read are few and are pulled through the
// accessors below, so the untyped map never spreads past this package.
type Envelope struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Timestamp    string         `json:"timestamp"`
	Source       string         `json:"source"`
	Actor        string         `json:"actor"`
	Summary      string         `json:"summary"`
	Category     string         `json:"category"`
	TraceID      string         `json:"trace_id"`
	SpanID       string         `json:"span_id"`
	ParentSpanID string         `json:"parent_span_id"`
	Topic        string         `json:"topic"`
	Payload      map[string]any `json:"payload,omitempty"`

	// Failed says whether the work this event reports failed.
	//
	// It carries the SAME derivation FeedRow gets, from the same function,
	// because the two halves of one list must agree. They did not: the
	// snapshot's rows carried it and the live `event` push did not, so a
	// failed turn arriving while a reader watched rendered identically to a
	// successful one — and then grew its failure mark on the next reload,
	// when the same row came back through the store. Set by Apply, not by a
	// producer, so it cannot be forgotten at a call site.
	Failed bool `json:"failed"`
}

// FeedRow is one payload-free row of the activity feed.
//
// The shape is named once because the row is built in two places — live from
// the stream and hydrated from the store — and two hand-maintained copies of a
// shape is how a field ends up present on a running dashboard and missing after
// a reload.
type FeedRow struct {
	ID           string `json:"id"`
	Type         string `json:"type"`
	Timestamp    string `json:"timestamp"`
	Source       string `json:"source"`
	Actor        string `json:"actor"`
	Summary      string `json:"summary"`
	Category     string `json:"category"`
	TraceID      string `json:"trace_id"`
	SpanID       string `json:"span_id"`
	ParentSpanID string `json:"parent_span_id"`
	Topic        string `json:"topic"`
	Failed       bool   `json:"failed"`
}

// WebhookTopic is the topic a webhook delivery's envelope and feed row name.
//
// Not a subject anything is published on: the receiver writes a delivery's row
// itself and ingests its envelope directly, because the engine never publishes
// it on crewlet.events.*. The label tells a reader which surface a delivery came
// through, and ONE spelling is what keeps the live row the receiver pushes and
// the row a restarted process seeds from the store the same.
func WebhookTopic(source string) string { return "crewlet.webhooks." + source }

// ErrorInfo is why a seat stopped.
//
// One shape for every stop — a failed phase, a failed task, an exhausted
// budget, a dead provider, a guard breach — so one panel explains all of them
// rather than each kind of failure needing the reader to know where to look.
type ErrorInfo struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Phase   string `json:"phase"`
	TurnID  string `json:"turn_id"`
	At      string `json:"at"`
	EventID string `json:"event_id"`
}

// LiveCall is the in-flight LLM call: the latest progress round, or the
// placeholder a phase start seeds.
//
// It is held here because agent_turn_progress is STREAM-ONLY — the event-store
// writer drops it — so the durable record of a turn only appears once its phase
// completes. A dashboard that rebuilt from the store on every reconnect would
// lose any call mid-flight the moment someone hit refresh. Holding it here and
// shipping it in the snapshot is what makes the live row survive that.
type LiveCall struct {
	// TurnID names ONE RUN of a turn, so (TurnID, Phase, Iteration) — the
	// key this whole projection is built on — is unique per execution. It
	// was the WORK KEY once, which a redelivery reproduces: a retry's
	// rounds then folded into the previous attempt's frozen failed call
	// and the live row never moved. See ADR-0017.
	TurnID string `json:"turn_id"`

	// WorkKey is the unit of work that run was dispatched for, carried so
	// a reader can find the attempt this one is repeating.
	WorkKey string `json:"work_key,omitempty"`

	Phase     string `json:"phase"`
	Iteration int    `json:"iteration"`
	Model     string `json:"model"`

	// Trigger is the event that woke this turn, carried so a refresh
	// mid-call still shows the live row's source.
	Trigger map[string]any `json:"trigger"`

	Prompt         string `json:"prompt"`
	PromptMessages []any  `json:"prompt_messages"`
	Response       string `json:"response"`

	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`

	ToolExecutions []any `json:"tool_executions"`
	// RoundNarration is what the model said in each round, so the live view
	// can put a round's thinking beside the calls it asked for instead of
	// re-splitting the joined Response and getting it wrong.
	RoundNarration []any `json:"round_narration"`
	// PartialRound is the round being written right now, absent when no
	// round is open. Never merged into RoundNarration: a reader has to be
	// able to tell arriving text from committed text.
	PartialRound map[string]any `json:"partial_round,omitempty"`

	RoundNum int `json:"round_num"`

	// RoundsUsed is how many rounds have come back, ONE-BASED — the same
	// name and the same quantity the finished phase record carries, so a
	// reader draws a running phase and a settled one from one field. It
	// was `rounds` here while the record's `rounds` became the per-round
	// list, and one key meaning a count on the live row and a list on the
	// durable one is how a reader ends up reading the wrong one.
	RoundsUsed int `json:"rounds_used"`

	// Rounds is the per-round timing the loop recorded — when each provider
	// call was made, how long it took, its tokens and how many tools it
	// asked for — as the progress frame carries it ([types.PhaseRound]).
	// Empty on a frame from a build that predates it.
	Rounds []any `json:"rounds"`

	// MaxRounds is the round cap currently GRANTED to this phase, which an
	// extension raises mid-phase, and RoundCeiling the most any extension
	// may raise it to. Zero on a frame that did not state them.
	MaxRounds    int `json:"max_rounds,omitempty"`
	RoundCeiling int `json:"round_ceiling,omitempty"`

	// RoundStartedAt is when the round in flight began its provider call,
	// so "how long has the model been thinking" is measured from the
	// round rather than from the call's start. Empty when no round is
	// open.
	RoundStartedAt string `json:"round_started_at,omitempty"`

	// RunningCall is the tool call the phase is running RIGHT NOW —
	// `{round, name, arguments, started_at}`, published before the call is
	// handed to its tool and cleared by the frame after it returns. Absent
	// between calls. Never merged into ToolExecutions, which lists calls
	// that ANSWERED: a reader has to be able to tell a call in flight from
	// one that returned nothing.
	RunningCall map[string]any `json:"running_call,omitempty"`

	// CacheReadTokens and CacheWriteTokens are the share of InputTokens the
	// provider's prompt cache served and stored so far — a breakdown of
	// the input, never an addition to it.
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`

	// WorkItem is the item the turn is charged to, when it is on one — the
	// same `{backend, id, key, project}` the turn's start named. Carried
	// like WorkKey, because a frame from an older build names none.
	WorkItem *types.WorkItem `json:"work_item,omitempty"`

	// Node is the node that published the call's frames: the node running
	// the seat right now, which a fleet's dashboard has no other way to
	// name.
	Node string `json:"node,omitempty"`

	InProgress bool `json:"in_progress"`

	// Failed and Error appear only on a frozen call. A phase that dies
	// mid-call is exactly when an operator most wants to see the call, so
	// it is stamped and kept rather than cleared.
	Failed bool       `json:"failed,omitempty"`
	Error  *ErrorInfo `json:"error,omitempty"`

	UpdatedAt string `json:"updated_at"`
	// StartedAt is when this call began, and it NEVER moves. UpdatedAt
	// advances on every published round — several times a second while a
	// round streams — so "how long has this been running" measured against
	// it is always about zero. They are different questions and need
	// different fields.
	StartedAt string `json:"started_at"`
}

// BudgetMeter is a seat's or the org's live token meters: one entry per CAPPED
// calendar window, as the engine's `budget_meters` frame states it.
//
// The fleet's SHARED counters as the gate enforces them: every node's spend in
// the window on the company's clock, against the ceiling in the active
// revision, with the gate's own refusal stamp and the engine's judgement of the
// window ([types.BudgetState]). It is never to be compared with a spend rollup
// beside it on the same screen, which covers a window of time a reader chose
// rather than the calendar window a ceiling is written for.
type BudgetMeter struct {
	Windows []WindowMeter `json:"windows"`
}

// WindowMeter is one capped window of a live meter, held exactly as the frame
// carried it — an alias rather than a copy of the wire struct, because a
// projection that re-declared the fields is how `refused_at` was dropped on its
// way from the frame to the push.
type WindowMeter = types.BudgetWindow

// Overlay is the live half of an agent row, merged onto its static config row.
type Overlay struct {
	// State is empty, and OMITTED on the wire, until an event says whether
	// the seat is running: see ensureAgent for what claiming one cost.
	State            string    `json:"state,omitempty"`
	RuntimeID        string    `json:"runtime_id"`
	CurrentPhase     *string   `json:"current_phase"`
	CurrentIteration int       `json:"current_iteration"`
	LiveCall         *LiveCall `json:"live_call"`

	// LastError is nil once the seat does real work again. It says WHY a
	// seat stopped, rather than leaving a call on screen that never
	// answers.
	LastError *ErrorInfo `json:"last_error"`

	// Budget is nil when there is no meter, which covers two situations
	// that look the same from here: the seat has no per-agent budget, or
	// no report has arrived yet. Either way a bar drawn without one
	// would be a claim nobody measured.
	Budget *BudgetMeter `json:"budget"`

	// AFKReason is ALWAYS present, even when empty. The overlay is merged
	// into a client's row rather than replacing it, so an omitted key
	// reads as "unchanged" — which would leave a recovered agent wearing
	// the reason it was AFK for.
	AFKReason string `json:"afk_reason"`

	// Turn is the turn the seat is on — running, or parked on a detached
	// coding run — and null when it is on none. ALWAYS PRESENT for the
	// reason AFKReason is: a merged overlay with the key omitted would
	// leave a finished turn on the client's row.
	Turn *LiveTurn `json:"turn"`

	// LastTurn is the newest turn the seat ENDED, and null while it has
	// ended none this projection knows of. Seeded at boot from the fleet's
	// turn list, so "idle · last turn 24m ago" survives a restart.
	LastTurn *LastTurn `json:"last_turn"`

	// Paused is who paused the seat, when and why, and null while nobody
	// has. ALWAYS PRESENT for the reason Turn is: a merged overlay with the
	// key omitted would leave a resumed seat wearing its old pause. Seeded
	// at boot from the coordination record, so a pause taken before this
	// process started is still on the seat.
	Paused *Paused `json:"paused"`
}

// Stage is where in a turn a seat is.
//
// A NAMED STRING WITH Valid, the engine's enum idiom, so a stage a newer build
// sends is a value a reader can refuse by name rather than a panic.
type Stage string

const (
	// StageContext — the turn has started and is assembling what it
	// knows (the prefetch) before its first phase.
	StageContext Stage = "context"
	// StagePhase — a phase is running: a model is being called or a tool
	// is running on its behalf.
	StagePhase Stage = "phase"
	// StageParked — the turn launched a detached coding run and SUSPENDED:
	// nothing runs on the seat's behalf until the run is collected, and
	// the same turn resumes then — possibly on another node, possibly
	// after a restart. Not idle, and not an end: the turn has not
	// finished, and its work is still in flight in a box.
	StageParked Stage = "parked"
)

// Stages is the closed set.
var Stages = []Stage{StageContext, StagePhase, StageParked}

// Valid reports whether s is a stage this build knows.
func (s Stage) Valid() bool { return slices.Contains(Stages, s) }

// LiveTurn is the turn a seat is on.
type LiveTurn struct {
	// TurnID is the RUN (ADR-0017), the id every phase and progress frame
	// of it carries.
	TurnID string `json:"turn_id"`

	// WorkItem and WorkItemBasis are the item the turn is charged to and
	// the rule that charged it, as the turn's start named them (ADR-0022).
	// Absent on a turn on nothing — including one whose only claim to an
	// item is a sole write, which is named only at the end.
	WorkItem      *types.WorkItem     `json:"work_item,omitempty"`
	WorkItemBasis types.WorkItemBasis `json:"work_item_basis,omitempty"`

	// StartedAt is when the turn began, and it NEVER moves: a resumed
	// segment is the same turn, so the parked turn's start is kept.
	StartedAt string `json:"started_at"`

	Stage Stage `json:"stage"`

	// Node is the node running the turn: the publisher of its newest
	// frame, since a parked turn can resume on another.
	Node string `json:"node,omitempty"`

	// failed is whether any event of this turn so far was a failure — the
	// rule a stored turn row applies ([types.Failed] per event), so the
	// outcome the live projection records when the turn ends is the one a
	// seeded row states for the same turn. Internal, never on the wire.
	failed bool
}

// TurnOutcome is how a turn that ended went.
type TurnOutcome string

const (
	// OutcomeCompleted — the turn ended and none of its events was a
	// failure.
	OutcomeCompleted TurnOutcome = "completed"
	// OutcomeFailed — the turn ended and at least one of its events was a
	// failure: a failed phase, an unavailable provider, a breached guard,
	// a lost coding run. The same rule the turn list's `failed` applies, so
	// a turn seeded from the store and one watched live say the same.
	OutcomeFailed TurnOutcome = "failed"
)

// TurnOutcomes is the closed set.
var TurnOutcomes = []TurnOutcome{OutcomeCompleted, OutcomeFailed}

// Valid reports whether o is an outcome this build knows.
func (o TurnOutcome) Valid() bool { return slices.Contains(TurnOutcomes, o) }

// LastTurn is the newest turn a seat ended.
type LastTurn struct {
	TurnID string `json:"turn_id"`

	// EndedAt is the turn's newest event: its completion, or the
	// reflection pass that publishes after it — the same instant the turn
	// list's `ended_at` reads, so a seeded value and a live one agree.
	EndedAt string `json:"ended_at"`

	Outcome TurnOutcome `json:"outcome"`
}

// SandboxEntry is one in-flight detached coding run.
type SandboxEntry struct {
	TurnID      string `json:"turn_id"`
	Role        string `json:"role"`
	AgentHandle string `json:"agent_handle"`
	AgentID     string `json:"agent_id"`
	CodingAgent string `json:"coding_agent"`
	SandboxID   string `json:"sandbox_id"`
	Task        string `json:"task"`

	// Status is the run record's own word for where the run is — see
	// [SandboxStatus].
	Status SandboxStatus `json:"status"`

	StartedAt string `json:"started_at"`

	// Question and Audience appear once a run pauses on a clarification.
	Question string `json:"question,omitempty"`
	Audience string `json:"audience,omitempty"`

	// WorkItem is the item the launching turn is charged to, absent when
	// it is on nothing — so a panel can say what a box is working on
	// without joining back to a turn that may have parked days ago.
	WorkItem *types.WorkItem `json:"work_item,omitempty"`

	// Owner is the node driving the run: the one that launched it, or the
	// one that took it over when its owner left. Empty until something
	// names it.
	Owner string `json:"owner,omitempty"`

	// PausedAt is when the run's box began being held paused while the run
	// waits on a person — the reaper's own reading of the record
	// ([sandbox.PendingRun.HeldSince]) — and empty while the box is live.
	PausedAt string `json:"paused_at,omitempty"`
}

// SandboxStatus is where a detached run is, in the RUN RECORD'S OWN WORDS.
//
// It used to be two words of this projection's own, `running` and
// `awaiting_input`, and the second is not one the run record can write — so
// the dashboard, which asks "is a person needed?" in the record's vocabulary,
// never once saw a live run that was waiting on somebody: the state this panel
// most needs to surface reached no screen through it. One vocabulary, held
// against sandbox's own constants by internal/observe, which sees both.
type SandboxStatus string

const (
	// SandboxLaunching — the job has started and the turn that started it
	// is still unwinding.
	SandboxLaunching SandboxStatus = "launching"
	// SandboxRunning — the coding agent is working in its box.
	SandboxRunning SandboxStatus = "running"
	// SandboxAwaiting — the agent asked a person and stopped; its box is
	// held paused until the answer arrives.
	SandboxAwaiting SandboxStatus = "awaiting_clarification"
	// SandboxReseed — the box was reclaimed past its pause TTL while the
	// run waited. The question survives and an answer relaunches it.
	SandboxReseed SandboxStatus = "reseed"
)

// SandboxStatuses is the closed set a live entry can carry. The record's
// `resumed` is deliberately not in it: the run itself is over and the turn
// that launched it has taken its result back, so it is not a run in flight.
var SandboxStatuses = []SandboxStatus{SandboxLaunching, SandboxRunning, SandboxAwaiting, SandboxReseed}

// Valid reports whether s is a status a live entry can carry.
func (s SandboxStatus) Valid() bool { return slices.Contains(SandboxStatuses, s) }

// SandboxRecord is one run as the DURABLE record states it, for
// [LiveState.ReconcileSandboxes]: the entry it becomes, plus the two facts the
// reconcile needs to tell a run that has ended since from one that has not.
type SandboxRecord struct {
	Entry SandboxEntry

	// LaunchID is the job the record describes. A completion names the job
	// it finished, so a record still describing that job after its
	// completion landed is a record that has not caught up.
	LaunchID string

	// WrittenAt is the record's last write. A failure names no job, so a
	// record not written since a failure landed is the failed run's.
	WrittenAt time.Time
}

// OrgBudget is the org-wide half of the live meter, plus the identity and
// sequence of the node incarnation whose report is held and the company clock
// its windows were cut on.
type OrgBudget struct {
	MeterID  string      `json:"meter_id"`
	Seq      int         `json:"seq"`
	Timezone string      `json:"timezone"`
	Org      BudgetMeter `json:"org"`
}

// Change is what one applied event moved.
//
// The stream service turns this into the push envelopes a dashboard consumes:
// the changed agents' overlays, the sandbox set, the spend rollup. Anything not
// named here did not move and is not re-sent — which is the whole point, since
// a dashboard should mirror this projection rather than re-implement it.
type Change struct {
	Agents    map[string]struct{}
	Sandboxes bool
	Tokens    bool
	Events    bool
	Budget    bool
}

// Moved reports whether anything changed at all.
func (c Change) Moved() bool {
	return len(c.Agents) > 0 || c.Sandboxes || c.Tokens || c.Events || c.Budget
}

func (c *Change) agentMoved(role string) {
	if c.Agents == nil {
		c.Agents = map[string]struct{}{}
	}
	c.Agents[role] = struct{}{}
}

// --- payload accessors ------------------------------------------------- //
//
// Small, total, and in one place. A projection that reached into the map
// inline would grow a different coercion at every call site, and the payloads
// it reads come off a wire where a number may arrive as a float, a string or
// not at all.

// str returns the first non-empty string among keys.
//
// FIRST NON-EMPTY, not first present: several payloads name the same thing two
// ways (role / agent_role, model / provider_key) and the fallback only helps if
// an empty value falls through to it.
func str(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := payload[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// num reads an integer field.
//
// TWO cases, and only two. float64 is what a JSON round trip produces — every
// payload that crossed a broker arrives that way, since JSON has one number
// type — and int is what an in-process caller building the map directly gives.
// Nothing on this path decodes with UseNumber, so json.Number is not a shape
// these payloads can arrive in and a case for it would be a branch no envelope
// can reach.
func num(payload map[string]any, key string) int {
	switch v := payload[key].(type) {
	case int:
		return v
	case float64:
		return int(v)
	}
	return 0
}

// fraction reads a fractional value, for the one payload field that is money.
//
// SEPARATE FROM num rather than a widening of it: num TRUNCATES, which is
// correct for a token count and silently wrong for a price — every phase that
// cost less than a dollar would report zero, which is most of them.
func fraction(payload map[string]any, key string) float64 {
	switch v := payload[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	}
	return 0
}

func flag(payload map[string]any, key string) bool {
	b, _ := payload[key].(bool)
	return b
}

func mapping(payload map[string]any, key string) map[string]any {
	m, _ := payload[key].(map[string]any)
	return m
}

func list(payload map[string]any, key string) []any {
	l, _ := payload[key].([]any)
	return l
}
