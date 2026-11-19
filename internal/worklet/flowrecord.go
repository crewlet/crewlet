package worklet

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A change of an item's flow and a notice of it are each a `flow` record on the
// item's own flow subject, applied into `tracker_flow` and never into the
// item's row (package doc, "An item in progress says why it is not moving"),
// and each lands once by a key derived from what it says, never from the tick
// that published it.

// FlowRole is how a flow notice reaches the one person it tells: as the
// item's assignee, or as the lead of its project.
type FlowRole string

const (
	// FlowToldAssignee is the assignee, a person, told first.
	FlowToldAssignee FlowRole = "assignee"
	// FlowToldLead is the project's lead: after a person assignee, first
	// for an agent's item, whose own notice is its pickup, and alone for
	// a held item.
	FlowToldLead FlowRole = "lead"
)

// FlowRoles are both roles.
var FlowRoles = []FlowRole{FlowToldAssignee, FlowToldLead}

// Valid reports whether a role off the wire is one this build knows, the
// empty role of a record that tells nobody included.
func (r FlowRole) Valid() bool { return r == "" || slices.Contains(FlowRoles, r) }

// FlowRecord is one flow record's payload, a row of `tracker_flow`: a change
// of an item's flow state (Notice empty), or a notice of the state to one
// person. ONE SHAPE FOR BOTH ENDS, as [TurnRecord] is.
type FlowRecord struct {
	// ID is the record's derived key ([FlowChangeID], [FlowNoticeID]), the
	// row's own: what lands the record once however often it is published.
	ID string `json:"id"`

	// Task is the item's id.
	Task string `json:"task"`

	// State, Cause, Since and Until are the item's flow as read. Since is the
	// instant the facts give (the start, the last change, the hold's start),
	// never the instant it was read, and nil where they give none.
	State FlowState  `json:"state"`
	Cause FlowCause  `json:"cause"`
	Since *time.Time `json:"since"`
	Until *time.Time `json:"until"`

	// Seat is the item's assignee as read, empty for nobody.
	Seat string `json:"seat"`

	// Notice is the role the record tells Told in, empty for a change of
	// state that tells nobody.
	Notice FlowRole `json:"notice"`
	Told   string   `json:"told"`

	// Backfilled says the record was derived from the item's past rather
	// than read as it happened.
	Backfilled bool `json:"backfilled"`
}

// FlowNotice is what a flow notice's routing snapshot carries
// ([Snapshot.Flow]): whom it tells, and the state it tells of, which the moot
// rule compares the item with at dispatch ([FlowLeft]).
type FlowNotice struct {
	// ID is the record's key ([FlowRecord.ID]): the wake's identity and the
	// inbox row's, so a notice published twice wakes once.
	ID string `json:"id"`

	Told string   `json:"told"`
	Role FlowRole `json:"role"`

	State FlowState  `json:"state"`
	Cause FlowCause  `json:"cause"`
	Since *time.Time `json:"since"`
	Until *time.Time `json:"until"`
}

// FlowChangeID is the key of a change of item's flow to state and cause since
// the instant the facts give, after the change keyed previous ("" for the
// item's first): the change before it is what makes an item that leaves a
// state and enters it again a change of its own where no instant tells them
// apart.
func FlowChangeID(item string, state FlowState, cause FlowCause, since *time.Time,
	previous string) string {

	return flowKey("change", item, string(state), string(cause), keyInstant(since), previous)
}

// FlowNoticeID is the key of a notice to told that item has been in state
// since entered: once per item, state, transition and person.
func FlowNoticeID(item string, state FlowState, entered time.Time, told string) string {
	return flowKey("notice", item, string(state), keyInstant(&entered), told)
}

// flowKey is a digest of its parts, each NUL-terminated so ("ab", "c") and
// ("a", "bc") are two keys: the first sixteen bytes of SHA-256, in hex.
func flowKey(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// keyInstant spells an instant in a key as the store keeps it, to the
// microsecond, and none as empty.
func keyInstant(at *time.Time) string {
	if at == nil {
		return ""
	}
	return strconv.FormatInt(store.EncodeTime(*at), 10)
}

// check refuses a record that could not say what it records.
func (f FlowRecord) check() error {
	switch {
	case f.ID == "":
		return invalid("a flow record names no key, and the key is what lands " +
			"it once however often the duty publishes it")
	case f.Task == "":
		return invalid("a flow record names no item")
	case !f.State.Valid(), f.State == FlowWorking:
		// Working time is the turns' own record and is never copied.
		return invalid("a flow record of item %s says %q, which is no state "+
			"the flow records", f.Task, f.State)
	case !f.Cause.Valid():
		return invalid("a flow record of item %s says cause %q", f.Task, f.Cause)
	case !f.Notice.Valid():
		return invalid("a flow record of item %s tells in role %q", f.Task, f.Notice)
	case (f.Notice == "") != (f.Told == ""):
		return invalid("a flow record of item %s names role %q and person %q: "+
			"a notice names both and a change of state neither", f.Task, f.Notice, f.Told)
	}
	return nil
}

// RecordFlow publishes one flow record: a change of an item's flow state, or
// a notice of it to one person, whose routing snapshot the decide reads off
// the item's own row so the card names the item as it stands.
//
// THE PROJECT IS READ, as a turn's is ([Writer.RecordTurn]): the record's
// scope is the item's project, and an item moved since the read is read again.
func (w *Writer) RecordFlow(ctx context.Context, opID string, flow FlowRecord) (WriteResult, error) {
	switch {
	case opID == "":
		return WriteResult{}, invalid("a flow record of item %s names no "+
			"operation id", flow.Task)
	case w.db.IsZero():
		return WriteResult{}, invalid("this writer holds no replicated estate, " +
			"and a flow record's scope is its item's project, which only the " +
			"item's own row can say")
	}
	if err := flow.check(); err != nil {
		return WriteResult{}, err
	}
	for attempt := 1; ; attempt++ {
		project, err := w.projectOfTask(ctx, flow.Task)
		if err != nil {
			return WriteResult{}, err
		}
		result, err := w.recordFlowIn(ctx, opID, project, flow)
		if !errors.Is(err, errTurnTaskMoved) || attempt == recordTurnAttempts {
			return result, err
		}
	}
}

// recordFlowIn is one attempt at [Writer.RecordFlow], scoped to the project
// the item was read in.
func (w *Writer) recordFlowIn(ctx context.Context, opID, project string,
	flow FlowRecord) (WriteResult, error) {

	subject := FlowSubject(flow.Task)
	scope := ScopeSet{Subject: true, Container: project}
	at := w.Now()
	return w.published(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternAdditive,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			if err := chargeable(ctx, tx, flow.Task, project, "its flow "+
				"cannot be recorded on rows that no longer exist"); err != nil {
				return statelog.Decision{}, err
			}
			notify, err := flowNotify(ctx, tx, flow)
			if err != nil {
				return statelog.Decision{}, err
			}
			return w.decide(ctx, tx, stamp, subject, OpFlow, "", scope, opID, flow, notify, at)
		},
	})
}

// flowNotify is a notice's routing snapshot, read off the item's row in the
// decide's own snapshot; nil for a change of state, which tells nobody.
func flowNotify(ctx context.Context, tx *sql.Tx, flow FlowRecord) (*Notify, error) {
	if flow.Notice == "" {
		return nil, nil
	}
	task, found, err := readTask(ctx, tx, flow.Task)
	if err != nil {
		return nil, fmt.Errorf("worklet: read item %s for its flow notice: %w", flow.Task, err)
	}
	if !found {
		return nil, missingTask(ctx, tx, flow.Task, "a notice cannot tell of an item "+
			"that no longer exists")
	}
	notice := FlowNotice{ID: flow.ID, Told: flow.Told, Role: flow.Notice,
		State: flow.State, Cause: flow.Cause, Since: flow.Since, Until: flow.Until}
	return &Notify{
		Kind:    ChangeFlow,
		Excerpt: flowExcerpt(notice, task.Assignee),
		Snapshot: Snapshot{
			Key: task.Key, Project: task.Project, Title: task.Title,
			Status: task.Status, StatusGroup: task.StatusGroup,
			Assignee: task.Assignee, Task: task.ID, Flow: &notice,
		},
	}, nil
}

// flowExcerpt is a notice's card line, in the engine's own words only: the
// wait's words are somebody's and stay on the item.
func flowExcerpt(n FlowNotice, assignee string) string {
	who := assignee
	if who == "" {
		who = "nobody"
	}
	switch n.State {
	case FlowDue:
		return "Due: its start passed and nothing of " + who + "'s has moved it since."
	case FlowStuck:
		return "Stuck: nothing changed on it for its project's stuck window."
	case FlowHeld:
		return "Held: " + heldWords(n.Cause, who) + "."
	}
	return "In progress and not moving: " + string(n.State) + "."
}

// heldWords says why a held item's assignee cannot take a turn.
func heldWords(cause FlowCause, who string) string {
	switch cause {
	case FlowCausePaused:
		return who + " is paused"
	case FlowCauseUnplaced:
		return who + " is placed on no node"
	case FlowCauseBudget:
		return who + " is out of its token budget"
	case FlowCauseLimit:
		return who + " is out of its model limit"
	case FlowCauseParkedElsewhere:
		return who + " is waiting on a person's answer on another item"
	case FlowCauseDeparted:
		return who + " is no longer in the org chart"
	case FlowCauseNobody:
		return "it has no assignee"
	}
	return who + " cannot take a turn"
}

// FlowLeft reports whether an item now read as now (nil for an item no longer
// in progress) has left the state a flow notice told of, so the notice is moot
// (ADR-0162). Judged only where the read can tell: one without live facts sees
// no turn, parked run or stopped seat, which can only put working, waiting or
// held before the state it read, so it settles every notice but one of a hold
// it cannot see, which it keeps unless an estate state that comes before held
// reads instead.
func FlowLeft(told FlowState, now *Flow, live bool) bool {
	switch {
	case now == nil:
		return true
	case now.State == told:
		return false
	case live, told != FlowHeld:
		return true
	}
	return now.State == FlowBlocked || now.State == FlowWaiting
}
