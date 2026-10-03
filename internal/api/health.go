package api

import (
	"context"
	"net/http"
	"time"

	"github.com/crewlet/crewlet/internal/api/chartapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/usage"
	"github.com/crewlet/crewlet/internal/version"
)

// The statuses a health body reports.
//
// Precedence is shutting_down > a posture other than serve or wait >
// unconfigured > ok. A draining engine is draining first, whatever else is true
// of it. The posture outranks unconfigured because it names the CAUSE: a node
// stuck applying its first revision is unconfigured because it is stuck, and
// `configured: false` still says the rest beside it.
const (
	StatusOK           = "ok"
	StatusUnconfigured = "unconfigured"
	StatusShuttingDown = "shutting_down"
)

// The answers [Health.Identity] gives.
//
// `ready` once anybody is enrolled, `unclaimed` while nobody is — a fresh
// install, waiting for its first person to be invited — and `unknown` where
// this node could not read its identity estate, which is never folded into
// `unclaimed`: a dashboard told nobody is in would give an empty company's
// guidance to one that may have started.
const (
	IdentityReady     = "ready"
	IdentityUnclaimed = "unclaimed"
	IdentityUnknown   = "unknown"
)

// Health is what the /health endpoint and the dashboard's health push both
// carry — WHOLE, on both.
//
// ONE shape backing both, so a field added here reaches the endpoint, the
// snapshot and the periodic push together — and a reconnect restores it with no
// second round trip. The push used to carry three fields of it while a `stream`
// query answered the rest, and five screens polled that query at two cadences
// of their own: the rail could say a revision had applied while the panel in
// front of it said it had not, for as long as fifteen seconds. There is no such
// query any more; a screen reads the pushed body.
//
// Every field the engine answers is ALWAYS PRESENT, and a zero here is a real
// zero: every process that serves the API runs the engine beside it, so there is
// no answer this body could honestly leave out. The OMITTED fields are the ones
// where absence is itself the answer — a lag or a stranding that is not
// happening, a read that did not happen, an evaluation that has not run — and
// each says which.
//
// PUBLIC on /health, like every probe — an orchestrator has no credential, so
// the route is on the guard's exemption list — while the push goes only to a
// socket whose principal holds `state:read` (stream.KindHealth's grant). The
// probe is the wider audience and this body is shaped for it: the fleet, the
// alarm table and the continuous report appear here as COUNTS — how many
// nodes, how many alarms and the one longest unanswered, how many findings of
// each kind — and never as their rows: which nodes hold what, what each alarm
// measured and what each finding says are the `fleet`, `work_retention` and
// `/chart/check` answers, each behind its own grant.
type Health struct {
	Status string `json:"status"`

	// Node names the process that answered — the field that turns "the
	// config apply failed" into "the config apply failed on node-2" once a
	// load balancer sits in front of more than one process, and the only
	// way a caller can tell which one it reached.
	Node string `json:"node"`

	// Configured is true once a company revision is active. It has been on
	// the wire since this surface existed and nothing rendered it, which
	// meant an engine with no active revision — one refusing every inbound
	// webhook with a 503 the sender retries, never dropping one — looked
	// exactly like a healthy idle one, just with empty screens.
	Configured bool `json:"configured"`

	Version string `json:"version"`

	// StartedAt is when this node's engine was built, which is the node's
	// start: the API runs inside the engine's process. ONE start rather
	// than one per surface, and the same instant the fleet view reports
	// for this node off its presence heartbeat.
	StartedAt string `json:"started_at"`

	Queue   string `json:"queue"`
	Clients int    `json:"clients"`

	// EventHistorySeconds is how far back the event log can be read, which
	// is the hard bottom of paging: once a cursor crosses it every page is
	// empty forever. The dashboard has to be able to SAY that floor, and
	// three of its screens restated it as literal copy ("the store keeps 30
	// days") because nothing on the wire carried it. Seconds rather than
	// days, because the retention is a duration and a client that has to
	// re-derive the unit is a second place the number can be wrong.
	EventHistorySeconds int `json:"event_history_seconds"`

	// SpendHistorySeconds is how far back a NAMED spend window can reach:
	// the replicated usage domain's own history (ADR-0020), which is not
	// the event log's. The two floors answer different questions — "can I
	// still open that turn" and "can I still chart that month" — and a
	// screen stating one under the other's name tells a reader a ninety-day
	// spend chart is impossible, or that a month-old turn can still be read.
	SpendHistorySeconds int `json:"spend_history_seconds"`

	InFlight     int      `json:"in_flight"`
	ShuttingDown bool     `json:"shutting_down"`
	Posture      string   `json:"posture"`
	AppliedEpoch int64    `json:"applied_epoch"`
	Seats        []string `json:"seats"`

	// IdentityDutySeconds maps each identity duty this node armed to the
	// interval it runs at, in seconds — `{}` on a node that armed none.
	// Always an object, for Seats' reason: the engine always knows what
	// it armed, so a null would be a claim this body never has to make. It
	// is where an operator reads that the retention sweep and the claim
	// report are running at all, and at which interval each runs.
	IdentityDutySeconds map[string]float64 `json:"identity_duty_seconds"`

	// StallLagSeconds is how far behind this node's watched duty is,
	// present only when it is behind at all. It is the number that climbs
	// towards the seat lease TTL, at which the watchdog ends the process —
	// so an operator watching a node degrade sees it here before the
	// restart rather than only afterwards in the exit code.
	StallLagSeconds *float64 `json:"stall_lag_seconds,omitempty"`

	// Consistency is the continuous report over the two halves of a
	// running company: the chart this node holds and the settings epoch it
	// has applied. See internal/api/chartapi's report.go for what it
	// evaluates and why nothing can refuse these at a write.
	//
	// IT DOES NOT MOVE Status, and that is a decision rather than an
	// omission. Status answers "should this process be serving", which a
	// load balancer reads; a company referencing a provider somebody
	// deleted is a company with a problem and not a node with one, and
	// taking a node out of rotation over a configuration typo would turn
	// one broken seat into an outage. The number is here to be watched,
	// and /chart/check is where it is read.
	Consistency Consistency `json:"consistency"`

	// UnprovenSeconds maps each seat stranded by a teardown that could not
	// be proven to how long it has been stranded, present only when one is.
	// It is the number an alert reads: see [RuntimeState.Unproven]. It was
	// computed by the seat host and documented as this field while nothing
	// served it, so the only evidence of a seat out of service for a week
	// was a log line re-raised every twenty heartbeats.
	UnprovenSeconds map[string]float64 `json:"unproven_seconds,omitempty"`

	// Identity is whether this company has its first person —
	// [IdentityReady], [IdentityUnclaimed] or [IdentityUnknown]. See
	// [Identity] for why it is on this body.
	//
	// ABSENT where the API was given no [Identity] seam — a suite about
	// something else. `crewlet run` always wires it, over the identity
	// estate every node applies from boot, company or none, so a node with
	// no company answers for the fleet's estate. Like
	// [Health.Consistency], IT DOES NOT MOVE Status — a company
	// waiting for its first person is not a node that should leave
	// rotation.
	Identity string `json:"identity,omitempty"`

	// Nodes is how many nodes hold a presence lease: the fleet this node's
	// fan-outs divide their work by. ABSENT WHEN THE PRESENCE READ FAILED,
	// never 0 — the node answering is itself a node, so a zero could only
	// ever be a failed read wearing a number, and "node count unavailable"
	// is what a screen must say instead.
	Nodes *int `json:"nodes,omitempty"`

	// Alarms counts this node's standing alarms from the ONE evaluation
	// the gauge and the alarm log lines come from. Absent before that
	// evaluation first runs, and on a node running no state log: neither
	// has looked, and `{count: 0}` would tell a health card it is healthy.
	Alarms *HealthAlarms `json:"alarms,omitempty"`

	// SeededFrom is which nodes this node's live projection was seeded
	// from at boot — the feed, the spend window and each seat's last turn
	// every screen starts from — absent until that seed has run. A seed
	// that missed a node started those screens a node short, and this is
	// the only place that says so after the log line scrolled away.
	SeededFrom *eventfan.Coverage `json:"seeded_from,omitempty"`
}

// Consistency is the continuous report, summarised for a body that is read
// every few seconds.
//
// THE COUNTS AND NOT THE FINDINGS. A health body is polled by probes, pushed
// to every connected dashboard on a timer, and kept in logs; the findings
// carry a sentence and a remedy each and belong on the one surface somebody
// opened to read them. What a gauge needs is a number that moves and a name
// for what moved.
type Consistency struct {
	// Evaluated is false when this node could not evaluate — it holds no
	// chart view, or has applied no settings epoch. ABSENT EVIDENCE IS NOT
	// A CLEAN BILL: `findings: 0` from a node that read nothing is the
	// most misleading answer this body could carry, so a reader checks
	// this before the count.
	Evaluated bool `json:"evaluated"`

	// Findings is how many things are wrong, and Worst the highest
	// severity among them — absent when nothing is.
	Findings int    `json:"findings"`
	Worst    string `json:"worst,omitempty"`

	// Counts is how many of each kind, so a reader watching the number
	// climb can say WHICH class grew without opening another surface.
	Counts map[string]int `json:"counts,omitempty"`

	// Unchecked is how many human seats this node could not ask the
	// identity directory about, so `seat_unheld` was left undecided for
	// them — [chartapi.Report.Unchecked], carried here for the reason
	// Evaluated is: a count of findings that silently excluded them would
	// read as a clean bill. Absent at zero.
	Unchecked int `json:"unchecked,omitempty"`
}

// HealthAlarms is the alarm table as a public health body can carry it: how
// many, and which has stood longest.
type HealthAlarms struct {
	// Count is how many alarms are firing. Zero is a real zero: the table
	// was evaluated and nothing holds.
	Count int `json:"count"`

	// Worst names the alarm that has been firing LONGEST
	// ([statelog.Tracker.Standing]) — the condition that has gone
	// unanswered longest, since the table asserts no severity of its own.
	// Absent when nothing is firing.
	Worst string `json:"worst,omitempty"`

	// WorstDomain is the state log [HealthAlarms.Worst] fired on, for an
	// alarm the table keeps PER LOG — absent for one about the node as a
	// whole. The kind alone named a class of condition while the alarm is
	// one log's: two logs' `trim_blocked` are two conditions with two
	// starts and two remedies, and a card naming the kind could not say
	// which log to look at.
	WorstDomain string `json:"worst_domain,omitempty"`
}

// Readiness is what /ready answers.
type Readiness struct {
	Ready      bool   `json:"ready"`
	Node       string `json:"node"`
	Configured bool   `json:"configured"`
	Draining   bool   `json:"draining"`
	Posture    string `json:"posture"`

	// Reason names what took this node out of rotation, and is absent while
	// it is in rotation: [ReasonDraining], [ReasonUnconfigured], or the
	// diverged posture itself. ONE FIELD, decided here in the precedence the
	// health status uses, so a load balancer's record of a failed probe says
	// why without its reader re-deriving it from the three fields above.
	Reason string `json:"reason,omitempty"`
}

// The reasons a refused /ready names, beside the diverged postures, which are
// named as themselves.
//
// ReasonDraining is the drain gate's own code, spelled once: a node that
// refuses a webhook because it is draining and a probe that reports it out of
// rotation are describing one fact.
const (
	ReasonDraining     = string(httpjson.CodeDraining)
	ReasonUnconfigured = StatusUnconfigured
)

// divergedPostures take a node out of rotation.
//
// shed and stuck only. WAIT and ISOLATED deliberately stay ready, and both
// exclusions are load-bearing: wait is ordinary propagation during a rollout,
// so failing readiness there would make every successful rollout a fleet-wide
// outage; isolated means NO node applied the revision, so taking this one out
// would take the fleet out over one bad revision.
var divergedPostures = map[string]struct{}{"shed": {}, "stuck": {}}

// health builds the body every health surface shares.
func (a *App) health(ctx context.Context) Health {
	configured := a.Configured()
	// THE FLEET COUNTS BESIDE THE SNAPSHOT, NOT AFTER IT, and under their
	// own budget. This body answers the LIVENESS probe, and both reads can
	// reach the coordination plane: the posture inside the snapshot is
	// bounded to engine.ProbeReadBudget, and run one after the other a
	// wedged broker would cost the probe twice that — most of a 5 s
	// liveness timeout, which an orchestrator answers by killing a healthy
	// node over a coordination blip. Concurrent, the probe's worst case is
	// one budget. The counts are the part of the envelope it can best
	// afford to lose: an out-of-budget presence read is an absent `nodes`,
	// which is already what "cannot say" means here.
	fleetCtx, cancel := context.WithTimeout(ctx, engine.ProbeReadBudget)
	defer cancel()
	fleetRead := make(chan FleetState, 1)
	go func() { fleetRead <- a.runtime.Fleet(fleetCtx) }()
	state := a.runtime.Snapshot(ctx)
	fleet := <-fleetRead
	seats := state.Seats
	if seats == nil {
		// A node holding no seats holds an empty list, and says so as one:
		// a null here would read as "cannot say", which this node can.
		seats = []string{}
	}
	body := Health{
		Status:       StatusOK,
		Node:         a.nodeID,
		Configured:   configured,
		Version:      version.String(),
		StartedAt:    state.StartedAt,
		Queue:        a.queueBackend,
		Clients:      a.stream.Hub().Clients(),
		InFlight:     state.InFlight,
		ShuttingDown: state.ShuttingDown,
		Posture:      state.Posture,
		AppliedEpoch: state.AppliedEpoch,
		Seats:        seats,
		// The floor is the store's own, not a number this package picked:
		// it is what every read is bounded by.
		EventHistorySeconds: int(store.EventHistory.Seconds()),
		SpendHistorySeconds: int(usage.History.Seconds()),
		IdentityDutySeconds: make(map[string]float64, len(state.IdentityDuties)),
	}
	for name, every := range state.IdentityDuties {
		body.IdentityDutySeconds[name] = every.Seconds()
	}
	body.Consistency = consistencyOf(a.report(ctx))
	a.identityOf(ctx, &body)
	if state.StallLag > 0 {
		// Only when there is something to say. A field that is always
		// present and always 0 trains a reader to skip it, which is the
		// one line of this body that must be read when it appears.
		lag := state.StallLag.Seconds()
		body.StallLagSeconds = &lag
	}
	if len(state.Unproven) > 0 {
		body.UnprovenSeconds = make(map[string]float64, len(state.Unproven))
		for seat, stranded := range state.Unproven {
			body.UnprovenSeconds[seat] = stranded.Seconds()
		}
	}
	if fleet.LiveNodes != nil {
		nodes := *fleet.LiveNodes
		body.Nodes = &nodes
	}
	if fleet.Alarms != nil {
		body.Alarms = &HealthAlarms{Count: len(fleet.Alarms)}
		if len(fleet.Alarms) > 0 {
			worst := fleet.Alarms[0]
			body.Alarms.Worst, body.Alarms.WorstDomain = string(worst.Kind), worst.Domain
		}
	}
	if coverage, seeded := a.state.SeededFrom(); seeded {
		body.SeededFrom = &coverage
	}

	// IN THE PRECEDENCE THE STATUSES DECLARE. The posture case used to be
	// reached whether or not the node was configured, so a node with no
	// revision that had also concluded `shed` reported the posture, and the
	// one fact that matters on such a node, that it refuses every
	// delivery, was the one its status did not say.
	switch {
	case state.ShuttingDown:
		body.Status = StatusShuttingDown
	case !configured:
		body.Status = StatusUnconfigured
	case state.Posture != "" && state.Posture != "serve" && state.Posture != "wait":
		body.Status = state.Posture
	}
	return body
}

// identityOf fills in whether the company has its first person, leaving it out
// where the API was given nothing to ask.
func (a *App) identityOf(ctx context.Context, body *Health) {
	if a.identity == nil {
		return
	}
	enrolled, err := a.identity(ctx)
	switch {
	case err != nil:
		body.Identity = IdentityUnknown
	case enrolled:
		body.Identity = IdentityReady
	default:
		body.Identity = IdentityUnclaimed
	}
}

// consistencyOf summarises one evaluation for the health body.
func consistencyOf(got chartapi.Report) Consistency {
	out := Consistency{
		Evaluated: got.Evaluated,
		Findings:  len(got.Findings),
		Worst:     string(got.Worst()),
		Unchecked: got.Unchecked,
	}
	if len(got.Counts) > 0 {
		out.Counts = make(map[string]int, len(got.Counts))
		for kind, n := range got.Counts {
			out.Counts[string(kind)] = n
		}
	}
	return out
}

// report is the ONE evaluation every surface here renders.
//
// A METHOD ON THE APP rather than a value computed at boot, because both
// halves change while the process runs: a chart record lands and a settings
// epoch is applied, independently. A cached report would be a claim about a
// company that no longer exists, and the evaluation is a walk over rows this
// node already holds in memory.
func (a *App) report(ctx context.Context) chartapi.Report {
	if a.company == nil {
		return chartapi.Report{}
	}
	settings, view := a.company()
	// THE SAME SEAM THE /chart/check ROUTE USES, so a gauge on /health and
	// the screen that renders the report can never disagree about whether
	// a seat is held — which is the whole reason one evaluation feeds
	// every surface.
	return chartapi.Evaluate(ctx, view, settings, a.seatHeld, a.resolve)
}

// tickReadBudget bounds a read done for a push tick rather than a request.
//
// The dashboard's shared tick and its placement read have no request context
// to inherit, and what they call reaches the coordination plane. Five seconds
// is far longer than the read needs and far shorter than the tick's own
// cadence, so a wedged plane costs one stale push rather than a goroutine per
// tick for the life of the process.
const tickReadBudget = 5 * time.Second

// readiness answers whether traffic should come here.
//
// Distinct from health on purpose. Liveness answers "is this process alive" and
// must stay 200 through a drain, so an orchestrator does not SIGKILL a node
// mid-turn. Readiness answers "should traffic come here", and during a drain
// the answer is no.
//
// Also not ready before the first config revision applies: an unconfigured node
// cannot verify a webhook signature, and taking it out of rotation is how a
// fleet avoids answering with a node that would only reject the delivery.
//
// IT NEVER ASKS [NodeRuntime.Fleet]. The fleet counts decide nothing here, and
// the presence count is a scan of the fleet's keys: a readiness probe paying
// for one on every call, to throw it away, is a probe a wedged broker can
// slow for a fact it never reads.
func (a *App) readiness(ctx context.Context) (Readiness, int) {
	configured := a.Configured()
	state := a.runtime.Snapshot(ctx)
	body := Readiness{
		Node: a.nodeID, Configured: configured,
		Draining: state.ShuttingDown, Posture: state.Posture,
	}
	_, diverged := divergedPostures[body.Posture]
	switch {
	case body.Draining:
		body.Reason = ReasonDraining
	case !configured:
		body.Reason = ReasonUnconfigured
	case diverged:
		body.Reason = body.Posture
	}
	body.Ready = body.Reason == ""
	if body.Ready {
		return body, http.StatusOK
	}
	return body, http.StatusServiceUnavailable
}

// NodeStatus is the node posture this body reports — [Health.Status] — which
// is what a socket's own posture is decided from ([framePosture]).
//
// A METHOD, so the live channel can carry this body WHOLE and still decide its
// posture from the same reading: internal/api/stream cannot name this type
// (this package imports it), and a second, narrower type declared there is how
// the push came to carry three fields of a body whose rest a query answered.
func (h Health) NodeStatus() string { return h.Status }

// streamHealth is the shared tick's view of the same facts: the WHOLE body.
//
// A CONTEXT OF ITS OWN, and this is one of the few places that is right: the
// push tick is a timer, not a request, so there is nothing to inherit. It is
// bounded rather than Background alone, because the posture and presence reads
// underneath reach the coordination plane and a push tick must not outlive the
// interval that will fire the next one.
func (a *App) streamHealth() stream.Health {
	ctx, cancel := context.WithTimeout(context.Background(), tickReadBudget)
	defer cancel()
	return a.health(ctx)
}
