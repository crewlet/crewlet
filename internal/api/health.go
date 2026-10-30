package api

import (
	"context"
	"net/http"
	"time"

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
// Precedence is shutting_down > unconfigured > a posture other than serve or
// wait > ok, which is the order [App.health] and /ready's reason both apply. A
// draining engine is draining first, whatever else is true of it. Unconfigured
// outranks a posture because it is the fact that decides what the node does
// with a delivery: with no revision it refuses every one, whatever it
// concluded about the fleet's epoch, and the posture still travels beside it
// in its own field.
//
// StatusStarting is answered before any of them, by the surface a node's
// listener serves from its bind until the node hands it its own ([Boot]):
// shutting_down outranks it there as everywhere, and the other three are facts
// about a running engine, which a starting node has none of yet.
const (
	StatusOK           = "ok"
	StatusUnconfigured = "unconfigured"
	StatusShuttingDown = "shutting_down"
	StatusStarting     = "starting"
)

// The phases a starting node's /health names, in the order a boot passes
// through them. See [Boot] for where the line between them is drawn and why.
const (
	// PhaseRecovering: the node is opening and recovering what it holds
	// itself — its store and its migrations, the broker's file store, the
	// replicated estate, the logs it provisions and a join's snapshot.
	// Bounded by the deployment, which alone knows how large a store it gave
	// the node, so /health answers 503 for all of it. Applying the logs is
	// not part of it: that catches up in the background afterwards, and
	// holds a seat's acquisition rather than this answer.
	PhaseRecovering = "recovering"

	// PhasePreparing: what the node holds is open, and it is bringing up
	// what its company configures — the shared tool servers, the sandboxes,
	// the native halves, the first apply, the dashboard's projection. /health
	// answers 200: the process is alive and past the part only a dead disk
	// could hold for ever, and nothing the company configures may hold a
	// startup probe. A part of it that never returns leaves the node here,
	// which /ready's `starting` shows and no probe restarts — see [Boot].
	PhasePreparing = "preparing"
)

// The answers [Health.Identity] gives.
//
// `ready` once a person is enrolled, `unclaimed` while nobody is — a fresh
// install, waiting for its first person to be invited or created onto a human
// seat its company declares; a service account alone signs nobody in with a
// password, so it does not end the wait — and `unknown` where this node could
// not read its identity estate, which is never folded into `unclaimed`: a
// dashboard told nobody is in would give an empty company's guidance to one
// that may have started. A created person ends the wait as their create
// lands, before they have spent their first password link.
const (
	IdentityReady     = "ready"
	IdentityUnclaimed = "unclaimed"
	IdentityUnknown   = "unknown"
)

// ConfigAttempt is a node's last concluded apply attempt: the epoch, the
// revision it tried at that epoch and how that went — `ok`, `error` or
// `degraded`, the apply record's own words (`coord.NodeApply`).
//
// THE APPLIED EPOCH CANNOT SAY IT. A failed attempt leaves `applied_epoch`
// where it was, so a node still applying a revision and one that refused it
// read the same, and every reader of the push who may not read the fleet — a
// unit lead following their own save — was told the engine was applying a
// revision this node had refused, for as long as the tab stayed open. The
// REVISION rides beside the epoch because an epoch number names two
// revisions once a rebuilt coordination store has restarted the count.
//
// NEVER THE ERROR TEXT: this body is public on /health, and a refusal can quote
// the config value it was given. Why the node refused is the fleet view's
// `config_error`, an admin's.
type ConfigAttempt struct {
	Epoch      int64  `json:"epoch"`
	RevisionID string `json:"revision_id"`
	Status     string `json:"status"`
}

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
// the route is on the guard's exemption list — while the push goes to every
// resolved socket (stream.KindHealth is NeedEverybody, the overview's). The
// probe is the wider audience and this body is shaped for it: the fleet and
// the alarm table appear here as COUNTS — how many nodes, how many alarms and
// the one longest unanswered — and never as their rows: which nodes hold what
// and what each alarm measured are the `fleet` and `work_retention` answers,
// each an admin's.
type Health struct {
	Status string `json:"status"`

	// Phase is how far a STARTING node's boot has got — [PhaseRecovering] or
	// [PhasePreparing] — beside [StatusStarting], on the body the boot
	// surface answers ([BootHealth]) before the node serves. ABSENT on every
	// body built here: this one is answered only once the node serves, so
	// its boot is over, and the absence is that answer.
	Phase string `json:"phase,omitempty"`

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

	// ConfigAttempt is this node's last concluded apply attempt — ABSENT
	// before this process has concluded one, which is that answer.
	ConfigAttempt *ConfigAttempt `json:"config_attempt,omitempty"`

	// IdentityDutySeconds maps each identity duty this node armed to the
	// interval it runs at, in seconds — `{}` on a node that armed none.
	// Always an object, for Seats' reason: the engine always knows what
	// it armed, so a null would be a claim this body never has to make. It
	// is where an operator reads that the retention sweep is running at
	// all, and at which interval.
	IdentityDutySeconds map[string]float64 `json:"identity_duty_seconds"`

	// StallLagSeconds is how far behind this node's watched duty is,
	// present only when it is behind at all: when its last beat is older
	// than the beat interval it keeps (a second at the shipped lease TTL),
	// which is a missed beat, and then the whole time since that beat. It is
	// the number that climbs towards the seat lease TTL, at which the
	// watchdog ends the process, so an operator watching a node degrade
	// sees it here before the restart rather than only afterwards in the
	// exit code.
	StallLagSeconds *float64 `json:"stall_lag_seconds,omitempty"`

	// UnprovenSeconds maps each seat stranded by a teardown that could not
	// be proven to how long it has been stranded, present only when one is.
	// It is the number an alert reads: see [RuntimeState.Unproven]. It was
	// computed by the seat host and documented as this field while nothing
	// served it, so the only evidence of a seat out of service for a week
	// was a log line re-raised every five minutes.
	UnprovenSeconds map[string]float64 `json:"unproven_seconds,omitempty"`

	// Identity is whether this company has its first person —
	// [IdentityReady], [IdentityUnclaimed] or [IdentityUnknown]. See
	// [Identity] for why it is on this body.
	//
	// ABSENT where the API was given no [Identity] seam — a suite about
	// something else. `crewlet run` always wires it, over the identity
	// estate every data node applies from boot, company or none — which a
	// node serving this API always is — so a node with no company answers
	// for the fleet's estate. IT DOES NOT MOVE Status
	// — a company waiting for its first person is not a node that should
	// leave rotation.
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

	// Objects is what this node's object store offers beyond streaming
	// bytes through the engine. ABSENT on a node running no object store.
	Objects *HealthObjects `json:"objects,omitempty"`
}

// HealthObjects is the object store's half of a health body.
type HealthObjects struct {
	// DirectUpload is `available` where an upload may go straight to the
	// S3 bucket on a presigned put, or `unavailable (<reason>)`: the
	// broker's own bucket, or a provider the boot probe could not prove
	// enforces the checksum and the If-None-Match condition a presigned
	// put relies on. Unavailable is no fault: every upload then streams
	// through the engine.
	DirectUpload string `json:"direct_upload"`
}

// HealthAlarms is the alarm table as a public health body can carry it: how
// many, and which has stood longest.
type HealthAlarms struct {
	// Count is how many alarms are firing, one per kind however many logs
	// it stands on, which is the count the gauge gives. Zero is a real
	// zero: the table was evaluated and nothing holds.
	Count int `json:"count"`

	// Worst names the alarm that has been firing LONGEST
	// ([statelog.Tracker.Standing]): the condition that has gone
	// unanswered longest, since the table asserts no severity of its own.
	// Absent when nothing is firing.
	//
	// A KIND, and no log beside it: an alarm is one per kind, listing the
	// logs it stands on ([statelog.Alarm.Domains]), and which logs those
	// are is the admin-only `work_retention` answer this public body
	// points at rather than restates.
	Worst string `json:"worst,omitempty"`
}

// Readiness is what /ready answers.
type Readiness struct {
	Ready      bool   `json:"ready"`
	Node       string `json:"node"`
	Configured bool   `json:"configured"`
	Draining   bool   `json:"draining"`
	Posture    string `json:"posture"`

	// Reason names what took this node out of rotation, and is absent while
	// it is in rotation: [ReasonStarting] before the node serves at all, then
	// [ReasonDraining], [ReasonUnconfigured], or the
	// diverged posture itself — then, on a node with the ingress role,
	// [ReasonEstateBehind], and on a node without it
	// [ReasonBrokerUnlinked], [ReasonNoPresence] or
	// [ReasonAdmissionWithheld]. ONE FIELD, decided by [judgeReadiness] in
	// one precedence — the estate term after it, see [App.serveReady] — so a
	// record of a failed probe says why without its reader re-deriving it
	// from the fields above.
	Reason string `json:"reason,omitempty"`

	// Detail says what is down, where the reason alone cannot: the broker
	// link's own error beside [ReasonBrokerUnlinked], since a leaf with no
	// link and an external connection reconnecting are fixed in different
	// places. Absent with every other reason.
	Detail string `json:"detail,omitempty"`
}

// The reasons a refused /ready names, beside the diverged postures, which are
// named as themselves.
//
// ReasonDraining is the drain gate's own code, spelled once: a node that
// refuses a webhook because it is draining and a probe that reports it out of
// rotation are describing one fact.
//
// ReasonStarting is the boot surface's ([Boot]): a node whose listener is
// bound and whose own surface is not served yet, spelled as the code its
// other routes answer with for the same reason.
const (
	ReasonDraining     = string(httpjson.CodeDraining)
	ReasonUnconfigured = StatusUnconfigured
	ReasonStarting     = string(httpjson.CodeStarting)
)

// The reasons only a node WITHOUT the ingress role refuses readiness on — see
// [judgeReadiness]. A node that takes no traffic is ready while it is doing
// its work, and each of these is a way of not doing it while the process stays
// perfectly alive.
const (
	// ReasonBrokerUnlinked: this node cannot reach the fleet's broker — a
	// leaf whose link to every member is down, or a connection to an
	// external cluster that is reconnecting. Its seats can neither receive
	// nor publish anything.
	ReasonBrokerUnlinked = "broker_unlinked"

	// ReasonNoPresence: this node does not hold its presence lease, so no
	// peer counts it and the estate's router asks it nothing.
	ReasonNoPresence = "no_presence"

	// ReasonAdmissionWithheld: this node runs seats and its latest
	// placement pass was not admitted to claim — no data node has yet
	// answered that a copy of the estate admits a seat, or it cannot serve
	// the seats it holds — or it has made no pass yet.
	ReasonAdmissionWithheld = "admission_withheld"
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
	state, fleet := readNode(ctx, a.runtime)
	body := Health{
		Status:        healthStatus(state, configured),
		Node:          a.nodeID,
		Configured:    configured,
		Version:       version.String(),
		StartedAt:     state.StartedAt,
		Queue:         a.queueBackend,
		Clients:       a.stream.Hub().Clients(),
		InFlight:      state.InFlight,
		ShuttingDown:  state.ShuttingDown,
		Posture:       state.Posture,
		AppliedEpoch:  state.AppliedEpoch,
		ConfigAttempt: state.ConfigAttempt,
		Seats:         heldSeats(state),
		// The floor is the store's own, not a number this package picked:
		// it is what every read is bounded by.
		EventHistorySeconds: int(store.EventHistory.Seconds()),
		SpendHistorySeconds: int(usage.History.Seconds()),
		IdentityDutySeconds: identityDutySeconds(state),
	}
	a.identityOf(ctx, &body)
	body.StallLagSeconds, body.UnprovenSeconds = stallAndStranded(state)
	body.Nodes, body.Alarms = fleetCounts(fleet)
	if coverage, seeded := a.state.SeededFrom(); seeded {
		body.SeededFrom = &coverage
	}
	if state.DirectUpload != "" {
		body.Objects = &HealthObjects{DirectUpload: state.DirectUpload}
	}
	return body
}

// nodeReader is what a health body reads the engine through — the part of
// [NodeRuntime] and [ProbeRuntime] the two surfaces share.
type nodeReader interface {
	Snapshot(ctx context.Context) RuntimeState
	Fleet(ctx context.Context) FleetState
}

// readNode takes the snapshot and the fleet counts a health body is built from.
//
// THE FLEET COUNTS BESIDE THE SNAPSHOT, NOT AFTER IT, and under their own
// budget. This body answers the LIVENESS probe, and both reads can reach the
// coordination plane: the posture inside the snapshot is bounded to
// engine.ProbeReadBudget, and run one after the other a wedged broker would
// cost the probe twice that — most of a 5 s liveness timeout, which an
// orchestrator answers by killing a healthy node over a coordination blip.
// Concurrent, the probe's worst case is one budget. The counts are the part of
// the envelope it can best afford to lose: an out-of-budget presence read is
// an absent `nodes`, which is already what "cannot say" means here.
func readNode(ctx context.Context, runtime nodeReader) (RuntimeState, FleetState) {
	fleetCtx, cancel := context.WithTimeout(ctx, engine.ProbeReadBudget)
	defer cancel()
	fleetRead := make(chan FleetState, 1)
	go func() { fleetRead <- runtime.Fleet(fleetCtx) }()
	state := runtime.Snapshot(ctx)
	return state, <-fleetRead
}

// healthStatus is a health body's `status`, IN THE PRECEDENCE THE STATUSES
// DECLARE. The posture case used to be reached whether or not the node was
// configured, so a node with no revision that had also concluded `shed`
// reported the posture, and the one fact that matters on such a node, that it
// refuses every delivery, was the one its status did not say.
func healthStatus(state RuntimeState, configured bool) string {
	switch {
	case state.ShuttingDown:
		return StatusShuttingDown
	case !configured:
		return StatusUnconfigured
	case state.Posture != "" && state.Posture != "serve" && state.Posture != "wait":
		return state.Posture
	}
	return StatusOK
}

// heldSeats is the seats a node holds, as a body carries them. A node holding
// no seats holds an empty list, and says so as one: a null here would read as
// "cannot say", which this node can.
func heldSeats(state RuntimeState) []string {
	if state.Seats == nil {
		return []string{}
	}
	return state.Seats
}

// identityDutySeconds is the identity duties a node armed, by name, at the
// interval each runs at in seconds — an empty object on a node that armed
// none, never null: the engine always knows what it armed. See
// [Health.IdentityDutySeconds].
func identityDutySeconds(state RuntimeState) map[string]float64 {
	out := make(map[string]float64, len(state.IdentityDuties))
	for name, every := range state.IdentityDuties {
		out[name] = every.Seconds()
	}
	return out
}

// stallAndStranded are the two fields present only when there is something to
// say. A field that is always present and always 0 trains a reader to skip it,
// which is the one line of the body that must be read when it appears.
func stallAndStranded(state RuntimeState) (*float64, map[string]float64) {
	var lag *float64
	if state.StallLag > 0 {
		seconds := state.StallLag.Seconds()
		lag = &seconds
	}
	var stranded map[string]float64
	if len(state.Unproven) > 0 {
		stranded = make(map[string]float64, len(state.Unproven))
		for seat, since := range state.Unproven {
			stranded[seat] = since.Seconds()
		}
	}
	return lag, stranded
}

// fleetCounts are the fleet's size and alarm count as a public body carries
// them — absent where the read did not happen, never a zero standing in for it.
func fleetCounts(fleet FleetState) (*int, *HealthAlarms) {
	var nodes *int
	if fleet.LiveNodes != nil {
		live := *fleet.LiveNodes
		nodes = &live
	}
	var alarms *HealthAlarms
	if fleet.Alarms != nil {
		alarms = &HealthAlarms{Count: len(fleet.Alarms)}
		if len(fleet.Alarms) > 0 {
			// The first is the one standing longest
			// ([statelog.Tracker.Standing]).
			alarms.Worst = string(fleet.Alarms[0])
		}
	}
	return nodes, alarms
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
// slow for a fact it never reads. Nor does it ask what the probe surface's
// /ready asks ([WorkState]): this node is ready while traffic should come to
// it, and none of those facts decides that.
func (a *App) readiness(ctx context.Context) (Readiness, int) {
	return judgeReadiness(a.nodeID, a.Configured(), a.runtime.Snapshot(ctx), nil)
}

// judgeReadiness is the ONE readiness judgement, for both surfaces: the full
// API's, which passes no work state, and the probe surface's, which passes the
// node's ([WorkState]).
//
// ONE PRECEDENCE, so a reason the two share means one thing on both. A drain
// first, whatever else is true. Then a broken broker link, because it is the
// cause most of what follows would be a symptom of — a node that cannot reach
// the broker cannot apply a revision, renew its presence or be admitted. Then
// the full surface's own reasons, in its own order. Then the two facts only a
// node doing work without taking traffic is judged on: whether its fleet can
// see it, and whether it was admitted to claim seats.
func judgeReadiness(node string, configured bool, state RuntimeState, work *WorkState) (Readiness, int) {
	body := Readiness{
		Node: node, Configured: configured,
		Draining: state.ShuttingDown, Posture: state.Posture,
	}
	_, diverged := divergedPostures[body.Posture]
	switch {
	case body.Draining:
		body.Reason = ReasonDraining
	case work != nil && work.Broker != nil:
		body.Reason, body.Detail = ReasonBrokerUnlinked, work.Broker.Error()
	case !configured:
		body.Reason = ReasonUnconfigured
	case diverged:
		body.Reason = body.Posture
	case work != nil && !work.Presence:
		body.Reason = ReasonNoPresence
	case work != nil && work.Admission && !work.Admitted:
		body.Reason = ReasonAdmissionWithheld
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
