package worklet

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// An item in progress says why it is or is not moving (ADR-0162). [flowExpr]
// is the one definition: an SQL expression evaluated inside the statement each
// reader already runs, so a list page, the detail, my_work, ready work and
// Workload's one GROUP BY read one rule and no renderer derives its own. The
// estate's facts are columns and index seeks on the item's row; the live facts
// (which items a turn or run is on, which seats are stopped and why) are bound
// from the [Posture] the reader's [Postures] answers. The first state that
// holds wins, in [FlowStates]' order; the record says why that order.

// FlowState is why an item in progress is or is not moving.
type FlowState string

const (
	// FlowWorking is a turn or an executor run of the assignee on the item now.
	FlowWorking FlowState = "working"
	// FlowBlocked is an open blocker, since the change the flow duty recorded.
	FlowBlocked FlowState = "blocked"
	// FlowWaiting is a start in the future, the assignee's own unanswered
	// question on the item, or the assignee's run parked on it for a person.
	FlowWaiting FlowState = "waiting"
	// FlowHeld is an assignee that cannot take a turn, or no assignee.
	FlowHeld FlowState = "held"
	// FlowDue is a start that passed with nothing of the assignee's on the
	// item since.
	FlowDue FlowState = "due"
	// FlowStuck is nothing changed on the item for its project's window.
	FlowStuck FlowState = "stuck"
	// FlowMoving is none of the above: the ordinary case.
	FlowMoving FlowState = "moving"
)

// FlowStates are the seven, in the order the first that holds wins.
var FlowStates = []FlowState{FlowWorking, FlowBlocked, FlowWaiting, FlowHeld,
	FlowDue, FlowStuck, FlowMoving}

// Valid reports whether a state off the wire is one of the seven.
func (s FlowState) Valid() bool { return slices.Contains(FlowStates, s) }

// Loads reports a state whose item wants its seat now: a seat's load is its
// items working, moving, due and stuck. Blocked, waiting and held work would
// not move whatever the seat did.
func (s FlowState) Loads() bool {
	return s == FlowWorking || s == FlowMoving || s == FlowDue || s == FlowStuck
}

// FlowCause is the fact that put an item in its state, for the states more
// than one fact can: working, waiting and held. Empty on the others.
type FlowCause string

const (
	// FlowCauseTurn is a turn of the assignee on the item.
	FlowCauseTurn FlowCause = "turn"
	// FlowCauseRun is an executor run of the assignee on the item.
	FlowCauseRun FlowCause = "run"

	// FlowCauseStart is a start in the future: `until` is the start and
	// `reason` the wait's words.
	FlowCauseStart FlowCause = "start"
	// FlowCauseQuestion is the assignee's own open question on the item.
	FlowCauseQuestion FlowCause = "question"
	// FlowCauseParked is the assignee's run parked on the item, waiting on
	// a person's answer.
	FlowCauseParked FlowCause = "parked"

	// FlowCausePaused is a paused seat: the first of a seat's stop reasons in
	// the live projection's own ranking.
	FlowCausePaused FlowCause = "paused"
	// FlowCauseUnplaced is a seat no node holds.
	FlowCauseUnplaced FlowCause = "unplaced"
	// FlowCauseBudget is a seat out of its token budget.
	FlowCauseBudget FlowCause = "budget"
	// FlowCauseLimit is a seat out of its model limit.
	FlowCauseLimit FlowCause = "limit"
	// FlowCauseParkedElsewhere is the assignee's run parked on another item,
	// waiting on a person's answer: a seat whose run works takes no other
	// work.
	FlowCauseParkedElsewhere FlowCause = "parked_elsewhere"
	// FlowCauseDeparted is an assignee the chart no longer holds.
	FlowCauseDeparted FlowCause = "departed"
	// FlowCauseNobody is no assignee at all.
	FlowCauseNobody FlowCause = "nobody"
)

// FlowCauses are every cause, grouped by the state each explains.
var FlowCauses = []FlowCause{FlowCauseTurn, FlowCauseRun,
	FlowCauseStart, FlowCauseQuestion, FlowCauseParked,
	FlowCausePaused, FlowCauseUnplaced, FlowCauseBudget, FlowCauseLimit,
	FlowCauseParkedElsewhere, FlowCauseDeparted, FlowCauseNobody}

// Valid reports whether a cause off the wire is one this build knows, the
// empty cause of a state only one fact explains included.
func (c FlowCause) Valid() bool { return c == "" || slices.Contains(FlowCauses, c) }

// stops reports a seat's stop reason, the causes a [SeatPosture] carries.
func (c FlowCause) stops() bool {
	return c == FlowCausePaused || c == FlowCauseUnplaced || c == FlowCauseBudget ||
		c == FlowCauseLimit
}

// Flow is an item in progress's state, the fact that put it there and the
// instants that bound it. Every key is always sent: an instant nobody knows
// is null, and a cause or reason nothing gives is empty.
type Flow struct {
	State FlowState `json:"state"`
	Cause FlowCause `json:"cause"`

	// Since is when the state began, where the facts give it: the turn's
	// start, the start that passed, the last change, a stop's or a park's
	// beginning; for a blocked item, when the flow duty recorded it blocked.
	Since *time.Time `json:"since"`

	// Until is when it ends, where known: the start waited for, a seat's
	// reset.
	Until *time.Time `json:"until"`

	// Reason is the wait's words, on an item waiting for its start.
	Reason string `json:"reason" untrusted:"name"`
}

// Postures is where the flow reads the seats' live facts: the live
// projection's one ranking of a seat's stop reasons, and what its turns and
// runs are on.
type Postures interface {
	// SeatPostures answers every seat's posture now.
	SeatPostures() Posture
}

// Posture is the seats' live facts at one instant.
type Posture struct {
	// Live says the source has been seeded. A posture that is not reads
	// no live fact, and every answer read with it says `flow_live: false`
	// rather than drawing every stopped seat's work as moving.
	Live bool

	// Seats is each seat's facts by handle; a seat absent is idle and
	// free to take a turn.
	Seats map[string]SeatPosture
}

// SeatPosture is one seat's live facts.
type SeatPosture struct {
	// On is the items a turn or an executor run of the seat is on now.
	On []PostureItem

	// Parked is the id of the item a run of the seat is parked on waiting
	// on a person's answer, empty for none, and ParkedSince when it parked.
	Parked      string
	ParkedSince time.Time

	// Stop is why the seat cannot take a turn, the projection's first by
	// its ranking ([FlowCause.stops]); empty when it can. StopSince and
	// StopUntil bound it where known, zero where not.
	Stop      FlowCause
	StopSince time.Time
	StopUntil time.Time
}

// PostureItem is one item a seat's turn or run is on.
type PostureItem struct {
	// Item is the item's id.
	Item string

	// Cause is [FlowCauseTurn] or [FlowCauseRun], and Since its start.
	Cause FlowCause
	Since time.Time
}

// flowAt is what one read's flows are read against: its instant and the one
// posture snapshot every row of the answer shares.
type flowAt struct {
	now     time.Time
	posture Posture
}

// flowAt snapshots the reader's posture for a read at now. A reader with no
// source reads none, which its answer says.
func (r *Reader) flowAt(now time.Time) flowAt {
	at := flowAt{now: now.UTC()}
	if r.postures != nil {
		at.posture = r.postures.SeatPostures()
	}
	return at
}

// live is the answer's `flow_live`.
func (at flowAt) live() bool { return at.posture.Live }

// DefaultStuckAfterHours is a project's window when it sets none: a working
// day, the span over which "nobody touched it" stops being a pause.
const DefaultStuckAfterHours = 8

// MinStuckAfterHours and MaxStuckAfterHours bound a project's window: an hour
// is the shortest a person can be told and act on, and a week the longest
// before "stuck" says nothing a weekly review does not.
const (
	MinStuckAfterHours = 1
	MaxStuckAfterHours = 168
)

// The estate's facts the expression reads, each on the item's own row or an
// index seek keyed by it, under aliases of their own (`f…`) so no reader's
// alias for the same table is shadowed.
const (
	flowInProgress  = "t.status = '" + string(StatusInProgress) + "'"
	flowOpenBlocker = "EXISTS (SELECT 1 FROM tracker_task_deps fd " +
		"WHERE fd.task_id = t.id AND fd.blocker_open = 1)"
	flowWaitReason = "COALESCE(json_extract(t.document, '$.wait_reason'), '')"
	// The assignee's own open question, as ready work holds it out.
	flowOwnAsk = "EXISTS (SELECT 1 FROM tracker_comments fc " +
		"WHERE fc.task_id = t.id AND fc.author = t.assignee AND fc.ask <> '' " +
		"AND fc.resolved = 0 AND fc.answered_by IS NULL AND fc.removed = 0)"
	// Departed only once a roster was ever applied: an estate that holds
	// none does not know who is in the chart, and unknown is never absent.
	flowDeparted = "EXISTS (SELECT 1 FROM tracker_rosters fr WHERE fr.name = ?) " +
		"AND NOT EXISTS (SELECT 1 FROM tracker_seat_places fs WHERE fs.handle = t.assignee)"
	// Nothing of the assignee's on the item since its start: no turn
	// segment of theirs on it ended, and they wrote nothing on it. A
	// person has no turns, so their own write is what ends a due item.
	flowNothingSinceStart = "t.start_at IS NOT NULL " +
		"AND NOT EXISTS (SELECT 1 FROM tracker_turns fu WHERE fu.task_id = t.id " +
		"AND fu.effective_at >= t.start_at AND fu.seat = t.assignee) " +
		"AND NOT EXISTS (SELECT 1 FROM tracker_history fh WHERE fh.subject_id = t.id " +
		"AND fh.actor = t.assignee AND fh.effective_at >= t.start_at)"
	flowStuck = "t.updated_at <= ? - 3600000000 * COALESCE((SELECT fp.stuck_after_hours " +
		"FROM tracker_projects fp WHERE fp.key = t.project_key), ?)"
	// A blocker gives no instant of its own, so a blocked item is blocked
	// since the flow duty recorded it so: its latest recorded change, where
	// that change is to blocked.
	flowBlockedSince = "(SELECT CASE WHEN fb.state = '" + string(FlowBlocked) +
		"' THEN fb.recorded_at END FROM tracker_flow fb WHERE fb.task_id = t.id " +
		"AND fb.notice = '' ORDER BY fb.log_seq DESC LIMIT 1)"
)

// flowRendering is what each branch of the expression reads as.
type flowRendering int

const (
	// flowWhole reads the flow packed as one JSON array, [decodeFlow]'s
	// input: one column, so a row's subqueries run once for all five
	// facts.
	flowWhole flowRendering = iota
	// flowStateOnly reads the state alone, for a census's buckets.
	flowStateOnly
)

// flowBranch is one way an item is in a state: an estate predicate and the
// flow it reads, each fact an SQL expression over the item's row.
type flowBranch struct {
	when  string // empty on the last branch, which always holds
	args  []any
	state FlowState
	cause FlowCause
	since string
	until string
	why   string
}

func (b flowBranch) value(how flowRendering) string {
	if how == flowStateOnly {
		return "'" + string(b.state) + "'"
	}
	return "json_array('" + string(b.state) + "', '" + string(b.cause) + "', " +
		cmp.Or(b.since, "NULL") + ", " + cmp.Or(b.until, "NULL") + ", " +
		cmp.Or(b.why, "''") + ")"
}

// flowLive is one live fact as the expression binds it: the key it is looked
// up by and the flow it reads.
type flowLive struct {
	key  string
	flow Flow
}

func (l flowLive) value(how flowRendering) string {
	if how == flowStateOnly {
		return string(l.flow.State)
	}
	// Spelled rather than marshalled: the state and the cause are this
	// package's closed sets ([Posture.facts] admits no other), a live fact
	// carries no words, and the instants are integers.
	return `["` + string(l.flow.State) + `","` + string(l.flow.Cause) + `",` +
		packedInstant(l.flow.Since) + "," + packedInstant(l.flow.Until) + `,""]`
}

// flowExpr is THE flow expression (ADR-0162), over the task row aliased `t`,
// and its arguments in statement order: NULL for an item not in progress.
//
// A COALESCE of tiers in the order the first state that holds wins, each tier
// either estate branches or a CASE over one live fact's keys: the live facts
// are bound, one entry per working item, parked run and stopped seat, and a
// tier with no entry is left out.
func flowExpr(at flowAt, how flowRendering) (string, []any) {
	working, parked, stops, elsewhere := at.posture.facts()
	var tiers []string
	var args []any
	live := func(key string, facts []flowLive) {
		if len(facts) == 0 {
			return
		}
		var b strings.Builder
		b.WriteString("CASE " + key)
		for _, fact := range facts {
			b.WriteString(" WHEN ? THEN ?")
			args = append(args, fact.key, fact.value(how))
		}
		b.WriteString(" END")
		tiers = append(tiers, b.String())
	}
	branches := func(list ...flowBranch) {
		var b strings.Builder
		b.WriteString("CASE")
		for _, branch := range list {
			if branch.when == "" {
				b.WriteString(" ELSE " + branch.value(how))
				continue
			}
			b.WriteString(" WHEN " + branch.when + " THEN " + branch.value(how))
			args = append(args, branch.args...)
		}
		b.WriteString(" END")
		tiers = append(tiers, b.String())
	}
	now := store.EncodeTime(at.now)
	const itemSeat, seat = "t.id || ' ' || t.assignee", "t.assignee"

	live(itemSeat, working)
	branches(
		flowBranch{when: flowOpenBlocker, state: FlowBlocked, since: flowBlockedSince},
		flowBranch{when: "t.start_at > ?", args: []any{now}, state: FlowWaiting,
			cause: FlowCauseStart, until: "t.start_at", why: flowWaitReason},
		flowBranch{when: flowOwnAsk, state: FlowWaiting, cause: FlowCauseQuestion})
	live(itemSeat, parked)
	branches(
		flowBranch{when: "t.assignee = ''", state: FlowHeld, cause: FlowCauseNobody},
		flowBranch{when: flowDeparted, args: []any{RosterSubject().ID}, state: FlowHeld,
			cause: FlowCauseDeparted})
	live(seat, stops)
	live(seat, elsewhere)
	// A start in the future already read as waiting, so a start here passed.
	branches(
		flowBranch{when: flowNothingSinceStart, state: FlowDue, since: "t.start_at"},
		flowBranch{when: flowStuck, args: []any{now, DefaultStuckAfterHours},
			state: FlowStuck, since: "t.updated_at"},
		flowBranch{state: FlowMoving, since: "t.updated_at"})

	return "CASE WHEN " + flowInProgress + " THEN COALESCE(" +
		strings.Join(tiers, ", ") + ") END", args
}

// facts is the posture as the expression's live tiers bind it, each in the
// order of its keys so one posture always renders one statement.
func (p Posture) facts() (working, parked, stops, elsewhere []flowLive) {
	if !p.Live {
		return nil, nil, nil, nil
	}
	for _, handle := range slices.Sorted(maps.Keys(p.Seats)) {
		if handle == "" {
			continue
		}
		seat := p.Seats[handle]
		for _, on := range seat.On {
			if on.Item == "" || (on.Cause != FlowCauseTurn && on.Cause != FlowCauseRun) {
				continue
			}
			working = append(working, flowLive{key: on.Item + " " + handle, flow: Flow{
				State: FlowWorking, Cause: on.Cause, Since: instantOf(on.Since)}})
		}
		if seat.Parked != "" {
			since := instantOf(seat.ParkedSince)
			parked = append(parked, flowLive{key: seat.Parked + " " + handle, flow: Flow{
				State: FlowWaiting, Cause: FlowCauseParked, Since: since}})
			elsewhere = append(elsewhere, flowLive{key: handle, flow: Flow{
				State: FlowHeld, Cause: FlowCauseParkedElsewhere, Since: since}})
		}
		if seat.Stop.stops() {
			stops = append(stops, flowLive{key: handle, flow: Flow{State: FlowHeld,
				Cause: seat.Stop, Since: instantOf(seat.StopSince),
				Until: instantOf(seat.StopUntil)}})
		}
	}
	byKey := func(a, b flowLive) int { return strings.Compare(a.key, b.key) }
	slices.SortStableFunc(working, byKey)
	slices.SortStableFunc(parked, byKey)
	return working, parked, stops, elsewhere
}

// decodeFlow reads the packed column [flowExpr] renders whole: nil for an
// item not in progress.
func decodeFlow(packed []byte) (*Flow, error) {
	if len(packed) == 0 {
		return nil, nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(packed, &parts); err != nil || len(parts) != 5 {
		return nil, fmt.Errorf("worklet: read an item's flow %q: the expression "+
			"packs five facts", packed)
	}
	var out Flow
	var since, until *int64
	for i, target := range []any{&out.State, &out.Cause, &since, &until, &out.Reason} {
		if err := json.Unmarshal(parts[i], target); err != nil {
			return nil, fmt.Errorf("worklet: read an item's flow %q: %w", packed, err)
		}
	}
	out.Since, out.Until = decodedInstant(since), decodedInstant(until)
	return &out, nil
}

func instantOf(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	at = at.UTC()
	return &at
}

func packedInstant(at *time.Time) string {
	if at == nil {
		return "null"
	}
	return strconv.FormatInt(store.EncodeTime(*at), 10)
}

func decodedInstant(micros *int64) *time.Time {
	if micros == nil {
		return nil
	}
	at := store.DecodeTime(*micros)
	return &at
}
