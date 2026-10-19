package objectives

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Period is the window every objective's target is a fraction of: thirty days,
// the month an error budget is spent over. A 99.9 percent objective allows 43
// minutes of failure in it.
const Period = 30 * 24 * time.Hour

// The bounds a good event is answered within. Each is the number every
// recording site, alarm and panel reads, and nothing else spells it.
const (
	// PublishWithin bounds a publish, from the request to its answer,
	// excluding the upload transfer (a separate request whose time is the
	// client's bandwidth). Thirty seconds: a publish reads its bytes back,
	// lints, copies CDN files and waits for its record to apply, and past
	// half a minute an agent's tool call or a person's save button has
	// already been given up on.
	PublishWithin = 30 * time.Second

	// PublishAppliedWithin bounds a publish that moves no new bytes (a
	// rollback, a patch that only carries, a set of edits), from the domain's
	// decision to the record applied. Two seconds: such a publish is one
	// record on the log and a manifest, so anything slower is the log or the
	// applier, never the content.
	PublishAppliedWithin = 2 * time.Second

	// ViewFirstByteWithin bounds a served document's first byte on the
	// artifact origin. Three hundred milliseconds: the frame cannot paint
	// before it, and it is a tenth of the ready objective's whole budget, the
	// rest of which is the browser's.
	ViewFirstByteWithin = 300 * time.Millisecond

	// ReadyWithin bounds a frame's time from its ticket being minted to the
	// page's bridge hello, as the viewer's browser reports it. Three
	// seconds: the point past which a person reads a blank frame as broken.
	ReadyWithin = 3 * time.Second

	// DataRoundTripWithin bounds a datastore write's verdict on a healthy
	// fleet: a person's checkbox has to settle before they look away from
	// it, and 250 ms is the log's append plus one apply with room to spare.
	DataRoundTripWithin = 250 * time.Millisecond

	// DataResolvedWithin bounds a datastore write's verdict at worst: past
	// five seconds the runtime shows the edit as unconfirmed, so a verdict
	// later than this is one the person never saw arrive.
	DataResolvedWithin = 5 * time.Second

	// ChangeVisibleWithin bounds the time from an applier committing a
	// change to the push frame written to a watching socket: one second,
	// which is when a collaborator stops reading a sheet as live.
	ChangeVisibleWithin = time.Second

	// WakeSlack bounds a wake past its debounce window: from the end of the
	// window a watch or a mention waits out to the woken turn's start. Ten
	// seconds, a placement and a turn start with room for one retry; the
	// debounce itself is the author's choice and is never counted against
	// the engine.
	WakeSlack = 10 * time.Second

	// PresenceJoinWithin and PresenceCursorWithin bound presence: a joiner
	// sees the room's roster within a second, and a cursor crosses nodes
	// within 150 ms, the point past which a followed pointer visibly lags
	// the voice describing it.
	PresenceJoinWithin   = time.Second
	PresenceCursorWithin = 150 * time.Millisecond
)

// The targets: the fraction of events over [Period] that must be good.
const (
	// TargetPublishSuccess is three nines: a publish that fails is work an
	// agent or a person has to redo, and 43 minutes a month is the budget an
	// operator can plan maintenance inside.
	TargetPublishSuccess = 0.999

	// TargetPublishApplied is the p95 of a bytes-free publish: a latency
	// promise with a tail, since one slow apply during a log's catch-up is
	// not a failure anybody acts on.
	TargetPublishApplied = 0.95

	// TargetViewFirstByte is three nines, the same as publishing: a document
	// the origin fails to start serving is a page nobody can read.
	TargetViewFirstByte = 0.999

	// TargetReady is 99.5 percent, half a nine below the server's own:
	// the browser's share of the path (the network, the device, the page's
	// own scripts) is not the engine's to promise, and a target the server
	// could meet only by owning it would page on every slow laptop.
	TargetReady = 0.995

	// TargetDataRoundTrip is the p95 of a datastore verdict.
	TargetDataRoundTrip = 0.95

	// TargetDataResolved is three nines of verdicts within the worst-case
	// bound: an edit whose verdict never arrives is lost from the person's
	// point of view.
	TargetDataResolved = 0.999

	// TargetChangeVisible is the p99 of change visibility: a collaborator
	// sees a hundredth of changes late at most.
	TargetChangeVisible = 0.99

	// TargetWake is the p95 of wake latency.
	TargetWake = 0.95
)

// MinBadEvents is how many bad events the long window of a burn must hold
// before the burn alarms. TWO, because one failure is a fault report and not a
// rate: on a quiet node a single failed request among a dozen reads as a
// burn of a hundred times the budget, and the alarm would page on every
// isolated error that the request's own log line already reports. Two in the
// same window is the smallest count that says the failures are recurring.
const MinBadEvents = 2

// Name names one objective.
//
// A named string because it travels: it is the `objective` attribute of
// `crewlet.objectives.events` and a word in an alarm's detail, and an operator
// searching for one has to be able to type it.
type Name string

// The objectives.
const (
	PublishSuccess   Name = "publish_success"
	PublishApplied   Name = "publish_applied"
	ViewFirstByte    Name = "view_first_byte"
	ArtifactReady    Name = "artifact_ready"
	DataRoundTrip    Name = "data_round_trip"
	DataResolved     Name = "data_resolved"
	ChangeVisibility Name = "change_visibility"
	WakeLatency      Name = "wake_latency"
	PresenceJoin     Name = "presence_join"
	PresenceCursor   Name = "presence_cursor"
)

// Valid reports whether n names an objective this build holds.
func (n Name) Valid() bool {
	_, ok := Of(n)
	return ok
}

// Objective is one service objective: what is measured, where, what a good
// event is, the bound it is answered within and the fraction of events over
// [Period] that must be good.
type Objective struct {
	Name Name

	// Measures is the indicator in an operator's words, Where the code that
	// measures it, and GoodEvent the event that counts as good.
	Measures  string
	Where     string
	GoodEvent string

	// Within is the bound a good event is answered within.
	Within time.Duration

	// Target is the fraction of events over [Period] that must be good.
	// ZERO IS BEST EFFORT: an indicator measured and exported with no error
	// budget, which no burn rate is judged for (presence, whose frames are
	// ephemeral by design and degrade silently).
	Target float64

	// Reason is why the objective is what it is.
	Reason string
}

// Budgeted reports whether the objective carries an error budget.
func (o Objective) Budgeted() bool { return o.Target > 0 }

// Budget is the fraction of events the objective allows to be bad: one minus
// its target, zero for a best-effort indicator.
func (o Objective) Budget() float64 {
	if !o.Budgeted() {
		return 0
	}
	return 1 - o.Target
}

// Good classifies one observation: answered is whether the event was answered
// without fault on this engine's side (a client's own refusal counts, a 5xx or
// an outcome nobody could establish does not), and elapsed how long it took.
//
// ONE RULE FOR EVERY OBJECTIVE, so a recording site decides only what its own
// event's fault is and never restates a bound.
func (o Objective) Good(elapsed time.Duration, answered bool) bool {
	return answered && elapsed >= 0 && elapsed <= o.Within
}

// catalogue is every objective, in the order a reader meets them.
var catalogue = []Objective{
	{
		Name:      PublishSuccess,
		Measures:  "Publish success",
		Where:     "the publish pipeline (`internal/artifactupload`), every create and publish",
		GoodEvent: "published, or refused for the client's own reason (a moved head, a quota, a policy, a bad request), excluding the upload transfer",
		Within:    PublishWithin,
		Target:    TargetPublishSuccess,
		Reason:    "a failed publish is work an agent or a person redoes",
	},
	{
		Name:      PublishApplied,
		Measures:  "Publish latency",
		Where:     "the publish pipeline, for a publish that moves no new bytes (a rollback, carries, edits)",
		GoodEvent: "the domain's decision to the record applied",
		Within:    PublishAppliedWithin,
		Target:    TargetPublishApplied,
		Reason:    "such a publish is one record and a manifest, so its latency is the log's",
	},
	{
		Name:      ViewFirstByte,
		Measures:  "View time to first byte",
		Where:     "the artifact origin (`internal/artifactserve`), every document request",
		GoodEvent: "the first byte written, under any status but a 5xx",
		Within:    ViewFirstByteWithin,
		Target:    TargetViewFirstByte,
		Reason:    "the frame cannot paint before its document starts",
	},
	{
		Name:      ArtifactReady,
		Measures:  "Artifact ready, reported by the viewer's browser",
		Where:     "the dashboard, through `POST /artifacts/ready`",
		GoodEvent: "the frame's ticket minted to the page's bridge hello",
		Within:    ReadyWithin,
		Target:    TargetReady,
		Reason:    "the only indicator that sees an outage keeping requests from reaching the engine",
	},
	{
		Name:      DataRoundTrip,
		Measures:  "Datastore op round trip",
		Where:     "the datastore writer (`internal/artifactdata`), every write and undo",
		GoodEvent: "a verdict, not an unknown outcome",
		Within:    DataRoundTripWithin,
		Target:    TargetDataRoundTrip,
		Reason:    "a person's edit settles before they look away",
	},
	{
		Name:      DataResolved,
		Measures:  "Datastore op resolution",
		Where:     "the datastore writer, every write and undo",
		GoodEvent: "a verdict, not an unknown outcome",
		Within:    DataResolvedWithin,
		Target:    TargetDataResolved,
		Reason:    "an edit whose verdict never arrives is lost to the person who made it",
	},
	{
		Name:      ChangeVisibility,
		Measures:  "Change visibility",
		Where:     "the stream push of a committed artifact or datastore change",
		GoodEvent: "the applier's commit to the frame written to the socket",
		Within:    ChangeVisibleWithin,
		Target:    TargetChangeVisible,
		Reason:    "past a second a collaborator stops reading the sheet as live",
	},
	{
		Name:      WakeLatency,
		Measures:  "Wake latency",
		Where:     "the changefeed, for a watch that fired or a mention",
		GoodEvent: "the end of the debounce window to the woken turn's start",
		Within:    WakeSlack,
		Target:    TargetWake,
		Reason:    "the debounce is the author's; the engine answers for what follows it",
	},
	{
		Name:      PresenceJoin,
		Measures:  "Presence join",
		Where:     "the presence hub",
		GoodEvent: "a join to the first roster",
		Within:    PresenceJoinWithin,
		Reason:    "best effort: presence is told, never stored, and degrades silently",
	},
	{
		Name:      PresenceCursor,
		Measures:  "Presence cursor",
		Where:     "the presence hub, across nodes",
		GoodEvent: "a cursor frame to its delivery on another node",
		Within:    PresenceCursorWithin,
		Reason:    "best effort: a late cursor is redrawn by the next one",
	},
}

// All is every objective, in the order a reader meets them.
func All() []Objective { return slices.Clone(catalogue) }

// Of is the objective n names.
func Of(n Name) (Objective, bool) {
	i := slices.IndexFunc(catalogue, func(o Objective) bool { return o.Name == n })
	if i < 0 {
		return Objective{}, false
	}
	return catalogue[i], true
}

// Reference renders the catalogue as the Markdown table
// docs/reference/service-objectives.md carries, which a test holds the page
// to: the page an operator builds a panel from must say what the code judges.
func Reference() string {
	var b strings.Builder
	b.WriteString("| Objective | Measures | Where | Good event | Within | Target |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, o := range catalogue {
		target := "best effort, no budget"
		if o.Budgeted() {
			target = strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.2f", o.Target*100), "0"), ".") + "%"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s | %s |\n",
			o.Name, o.Measures, o.Where, o.GoodEvent, spoken(o.Within), target)
	}
	return b.String()
}

// spoken is a bound as the reference page states it.
func spoken(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%d ms", d.Milliseconds())
	}
	return fmt.Sprintf("%g s", d.Seconds())
}
