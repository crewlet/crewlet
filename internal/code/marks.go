package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A PERSON'S MARKS OVER THEIR CODE NOTICES: how far they have read on the code
// log, the notices they marked read out of order or unread again, the ones
// they put off, and which reasons are theirs to act on — the code domain's
// half of a person's inbox, kept by internal/inbox's one set of rules on this
// log, beside the notices it marks (apply_notices.go).
//
// # Decided on the person's own record, one gesture at a time
//
// A screen says what a person did — "this one is done", "put this one off",
// "I have read everything up to here" — never the lists ([inbox.Gesture]). The
// [Marks] writer resolves the gesture inside ONE snapshot under
// [statelog.PatternArbitrated] on the person's subject ([KindPerson]): the
// marks row the person's last record left, and where each notice the gesture
// names sits, read from the notice's own row for this person — a mark carries
// its notice's position, because the lists are pruned against the
// seen-through position, and a caller that supplied it could get it wrong in
// the one way that loses the mark silently. Two screens marking two notices at
// once both land: each round the broker refuses re-reads the record that won.
//
// # The record carries the gesture and the positions, the apply the rules
//
// The notice rows are DIVERGENT (each node writes them within its own horizon,
// apply_notices.go) and the marks row is not, so the apply never reads a
// notice: the record carries the positions its decide found, and every node
// applies the same gesture with them to the same prior row at the record's
// broker instant — pure over the record, its instant and the row before it.
// A gesture the rules refuse at the apply — possible only where a snooze the
// decide saw as live had come due by the broker's instant, which only shortens
// a list — writes nothing on every node alike, rather than stopping the
// applier.
//
// # Only the person's own
//
// A gesture is a PERSON'S, on their own record (authz's `code_mark_inbox`
// row): their assistant's tool and their screen act as the human seat they
// hold, and the writer refuses a mark asked by anybody else — a seat, an
// unbound credential, a person naming another's handle — whatever admitted it.
// A lead reads a report's inbox; marking it is a gesture nobody asked a lead to
// make (the tracker's own rule).

// MarkGesture is one gesture over a person's code notices, as a marks record
// and an estate request carry it: [inbox.Gesture] over [Reason], spelled with
// its keys.
type MarkGesture struct {
	Read           []string           `json:"read,omitempty"`
	Unread         []string           `json:"unread,omitempty"`
	Snooze         []MarkSnooze       `json:"snooze,omitempty"`
	Unsnooze       []string           `json:"unsnooze,omitempty"`
	ReadThrough    *statelog.Position `json:"read_through,omitempty"`
	PrimaryReasons *[]Reason          `json:"primary_reasons,omitempty"`
}

// MarkSnooze is one notice put off until an instant.
type MarkSnooze struct {
	RecordID string    `json:"record_id"`
	Until    time.Time `json:"until"`
}

// Gesture is g as internal/inbox's rules read it.
func (g MarkGesture) Gesture() inbox.Gesture[Reason] {
	out := inbox.Gesture[Reason]{Read: g.Read, Unread: g.Unread, Unsnooze: g.Unsnooze,
		ReadThrough: g.ReadThrough, PrimaryReasons: g.PrimaryReasons}
	for _, s := range g.Snooze {
		out.Snooze = append(out.Snooze, inbox.Snooze{RecordID: s.RecordID, Until: s.Until})
	}
	return out
}

// MarkGestureOf is g as a marks record carries it.
func MarkGestureOf(g inbox.Gesture[Reason]) MarkGesture {
	out := MarkGesture{Read: g.Read, Unread: g.Unread, Unsnooze: g.Unsnooze, ReadThrough: g.ReadThrough,
		PrimaryReasons: g.PrimaryReasons}
	for _, s := range g.Snooze {
		out.Snooze = append(out.Snooze, MarkSnooze{RecordID: s.RecordID, Until: s.Until})
	}
	return out
}

// PersonMarks is a marks record's payload: whose record, the gesture, and the
// PACKED position of every notice the gesture names as the decide read it
// from the notice's own row — what the apply resolves the gesture with, since
// it reads no notice ([OpMarks], this file's head).
//
// BASE FORMAT, AS A STATED CONDITION, as [KindPerson] states it.
type PersonMarks struct {
	Handle    string            `json:"handle"`
	Gesture   MarkGesture       `json:"gesture"`
	Positions map[string]uint64 `json:"positions,omitempty"`
}

// MarkWrite is one gesture a person makes over their own code notices: the
// operation it is named by, who asks — the person, by the credential they act
// through and the human seat they hold — whose record it is, the gesture, and
// the position on the code log the asker last read, which the decide waits
// for (a mark of a notice this node has not applied yet is refused naming it
// otherwise).
type MarkWrite struct {
	OpID    string            `json:"op_id"`
	Asker   Principal         `json:"asker"`
	Handle  string            `json:"handle"`
	Gesture MarkGesture       `json:"gesture"`
	Seen    statelog.Position `json:"seen,omitzero"`
}

// Validate refuses a write no node could decide ([ErrInvalid] naming the
// field): an operation id a caller may not hold, an asker no principal can be
// or one who is not a person, or a handle that is no seat's.
func (w MarkWrite) Validate() error {
	if err := statelog.CheckCallerOpID(w.OpID); err != nil {
		return fmt.Errorf("%w: op_id: %w", ErrInvalid, err)
	}
	if why := w.Asker.invalid(); why != "" {
		return fmt.Errorf("%w: asker: %s", ErrInvalid, why)
	}
	if !org.ValidHandle(w.Handle) {
		return fmt.Errorf("%w: handle: %q is not a seat's handle — a person's code inbox is kept on the "+
			"human seat they hold", ErrInvalid, w.Handle)
	}
	if !w.Seen.IsZero() && w.Seen.Stream != topics.CodeLogStream {
		return fmt.Errorf("%w: seen: %s is a position on another log than %s", ErrInvalid, w.Seen,
			topics.CodeLogStream)
	}
	return nil
}

// MarkRefusal is why a mark wrote nothing, a closed set.
type MarkRefusal string

const (
	// MarkNotPermitted is a mark of somebody else's record, or one asked by
	// no person: marks are a person's own.
	MarkNotPermitted MarkRefusal = "not_permitted"
	// MarkInvalid is a gesture the inbox's rules refuse on what it says, or
	// one naming a record that is none of the person's code notices.
	MarkInvalid MarkRefusal = "invalid"
	// MarkFull is a gesture that would leave one of the person's lists past
	// [inbox.MaxEntries]: `read_through` marks in one move what it marks one
	// at a time.
	MarkFull MarkRefusal = "inbox_full"
	// MarkBusy is a mark whose person's record kept moving for every round a
	// publish may take: the same mark again is the same write.
	MarkBusy MarkRefusal = "busy"
	// MarkUnavailable is a mark the code log refused for a reason no other
	// node answers differently.
	MarkUnavailable MarkRefusal = "unavailable"
)

// MarkRefusals is the closed set.
func MarkRefusals() []MarkRefusal {
	return []MarkRefusal{MarkNotPermitted, MarkInvalid, MarkFull, MarkBusy, MarkUnavailable}
}

// Valid reports a refusal this build knows.
func (r MarkRefusal) Valid() bool { return slices.Contains(MarkRefusals(), r) }

// MarkResult is what became of a mark: its outcome and the position its record
// landed at — or, for a gesture that moved nothing, Unchanged and the position
// the person's record last moved at — or the refusal and its sentence.
type MarkResult struct {
	Outcome   statelog.Outcome  `json:"outcome,omitempty"`
	Position  statelog.Position `json:"position,omitzero"`
	Unchanged bool              `json:"unchanged,omitempty"`
	Refusal   MarkRefusal       `json:"refusal,omitempty"`
	Detail    string            `json:"detail,omitempty"`
}

// MarksOptions configure a marks writer.
type MarksOptions struct {
	// Publisher is the code log's write authority.
	Publisher Publisher
	// DB is the replicated estate a mark's answer is read back from.
	DB store.ReplicatedReader
	// NodeID is the deciding node, which every record carries as its writer
	// through the publisher's stamp.
	NodeID string
	// Now is the decide's clock — a record's authored instant and the
	// instant a snooze is judged live at; nil takes the wall clock in UTC.
	Now func() time.Time
}

// Marks is this node's writer of persons' code inbox marks. Every data node
// that publishes holds one, and a mark is asked of any of them: a person's
// marks contend with nothing but their own other marks, which the broker
// arbitrates on the person's subject.
type Marks struct {
	pub    Publisher
	db     store.ReplicatedReader
	nodeID string
	now    func() time.Time
}

// NewMarks builds a marks writer, refusing what it cannot work without.
func NewMarks(o MarksOptions) (*Marks, error) {
	switch {
	case o.Publisher == nil:
		return nil, errors.New("code: a marks writer needs the code log's write authority — every mark is a " +
			"record on that log")
	case o.DB.IsZero():
		return nil, errors.New("code: a marks writer needs the replicated estate a mark's answer is read from")
	case !config.ValidNodeID(o.NodeID):
		return nil, fmt.Errorf("code: a marks writer needs the deciding node's id; %q is not one config admits",
			o.NodeID)
	}
	m := &Marks{pub: o.Publisher, db: o.DB, nodeID: o.NodeID, now: o.Now}
	if m.now == nil {
		m.now = func() time.Time { return time.Now().UTC() }
	}
	return m, nil
}

// markDecision is what one round of a mark's decide came to.
type markDecision struct {
	refusal   MarkRefusal
	detail    string
	unchanged bool
	position  int64
}

// Mark decides one gesture over a person's code notices and answers what
// became of it. A write no node could decide is [ErrInvalid]; one this node
// ran nothing of — the code log refusing this node — is [ErrNotServing], which
// the router asks of the next data node under the same operation id. A
// refusal is an answer, never an error.
func (m *Marks) Mark(ctx context.Context, w MarkWrite) (MarkResult, error) {
	if err := w.Validate(); err != nil {
		return MarkResult{}, err
	}
	// THE PERSON'S OWN, whatever admitted the call: a seat, an unbound
	// credential and a person naming another's record write nothing.
	if w.Asker.Kind != PrincipalOperator || w.Asker.Seat != w.Handle {
		return MarkResult{Refusal: MarkNotPermitted, Detail: fmt.Sprintf("%s's code inbox is marked only by "+
			"the person who holds that seat, as themselves — a lead reads a report's inbox and marks none of it",
			w.Handle)}, nil
	}
	gesture := w.Gesture.Gesture()
	now := m.now()
	if err := inbox.CheckGesture(gesture, topics.CodeLogStream, Reasons(), now); err != nil {
		return markRefused(err), nil
	}
	subject := PersonSubject(w.Handle)
	scope := []string{PersonPath(w.Handle)}
	actor := Actor{ID: w.Handle, Kind: ActorOperator, OperatorID: w.Asker.OperatorID}
	var (
		last    markDecision
		decided bool
	)
	res, err := m.pub.Publish(ctx, statelog.Request{
		Subject: subject.statelog(), Scope: statelog.ScopeSet{Paths: scope},
		OpID: w.OpID, Pattern: statelog.PatternArbitrated, Session: w.Seen,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			last, decided = markDecision{}, false
			prior, position, err := marksIn(ctx, tx, w.Handle)
			if err != nil {
				return statelog.Decision{}, err
			}
			positions, missing, err := noticePositionsIn(ctx, tx, w.Handle, gesture.Records())
			if err != nil {
				return statelog.Decision{}, err
			}
			decided = true
			if missing != "" {
				last.refusal, last.detail = MarkInvalid, fmt.Sprintf("record %s is not one of %s's code "+
					"notices — a mark names the record_id an inbox notice carries", missing, w.Handle)
				return statelog.Decision{}, nil
			}
			post := prior
			if err := inbox.Apply(&post, gesture, positions, now); err != nil {
				r := markRefused(err)
				last.refusal, last.detail = r.Refusal, r.Detail
				return statelog.Decision{}, nil
			}
			if reflect.DeepEqual(normalMarks(post), normalMarks(prior)) {
				last.unchanged, last.position = true, position
				return statelog.Decision{}, nil
			}
			return recordFor(stamp, subject, OpMarks, scope, w.OpID, actor,
				PersonMarks{Handle: w.Handle, Gesture: w.Gesture, Positions: positions}, now)
		},
	})
	return m.answer(ctx, w, last, decided, res, err)
}

// markRefused is a gesture the inbox's rules refused, as this writer answers
// it: a list past its ceiling [MarkFull], anything else [MarkInvalid], each
// with the rules' own sentence.
func markRefused(err error) MarkResult {
	if errors.Is(err, inbox.ErrFull) {
		return MarkResult{Refusal: MarkFull, Detail: inbox.Sentence(err)}
	}
	return MarkResult{Refusal: MarkInvalid, Detail: inbox.Sentence(err)}
}

// normalMarks is m with every empty list nil, so two marks that hold the same
// are equal however each was built.
func normalMarks(m inbox.Marks[Reason]) inbox.Marks[Reason] {
	if len(m.Read) == 0 {
		m.Read = nil
	}
	if len(m.Unread) == 0 {
		m.Unread = nil
	}
	if len(m.Snoozed) == 0 {
		m.Snoozed = nil
	}
	if len(m.PrimaryReasons) == 0 {
		m.PrimaryReasons = nil
	}
	return m
}

// answer is a mark's answer from the publish's result and the last round's
// decision — applied answered from the person's row, never from the decision.
func (m *Marks) answer(ctx context.Context, w MarkWrite, last markDecision, decided bool, res statelog.Result,
	err error) (MarkResult, error) {

	if err != nil {
		var refusal *statelog.Unavailable
		switch {
		case errors.Is(err, ErrInvalidProposal):
			return MarkResult{}, err
		case errors.Is(err, statelog.ErrConflict):
			return MarkResult{Refusal: MarkBusy, Detail: fmt.Sprintf("%s's code inbox kept moving for every "+
				"round the mark may take; the same mark again is the same write", w.Handle)}, nil
		case errors.As(err, &refusal) && nodeRefusals[refusal.Reason]:
			return MarkResult{}, fmt.Errorf("%w: %w", ErrNotServing, refusal)
		case errors.As(err, &refusal):
			return MarkResult{Refusal: MarkUnavailable, Detail: "the code log refused the mark: " +
				strings.TrimPrefix(refusal.Error(), "statelog: ")}, nil
		}
		return MarkResult{}, fmt.Errorf("code: mark operation %s: %w", w.OpID, err)
	}
	switch res.Outcome {
	case statelog.OutcomeApplied:
		if res.Position.IsZero() {
			switch {
			case decided && last.refusal != "":
				return MarkResult{Refusal: last.refusal, Detail: last.detail}, nil
			case decided && last.unchanged:
				return MarkResult{Outcome: statelog.OutcomeApplied, Unchanged: true,
					Position: unpacked(last.position)}, nil
			}
			return MarkResult{}, fmt.Errorf("code: the publish of mark %s answered applied with no record, and "+
				"its decide neither refused it nor found it unchanged", w.OpID)
		}
		var position int64
		if err := m.db.Read(ctx, func(tx *sql.Tx) error {
			var err error
			_, position, err = marksIn(ctx, tx, w.Handle)
			return err
		}); err != nil {
			return MarkResult{}, fmt.Errorf("code: the mark %s applied at %s and %s's marks could not be read: %w",
				w.OpID, res.Position, w.Handle, err)
		}
		if position < res.Position.Packed() {
			return MarkResult{Outcome: statelog.OutcomeUnknown, Detail: fmt.Sprintf("the mark's record "+
				"applied at %s and %s's marks this node holds are not at it", res.Position, w.Handle)}, nil
		}
		return MarkResult{Outcome: statelog.OutcomeApplied, Position: res.Position}, nil
	case statelog.OutcomePending:
		return MarkResult{Outcome: statelog.OutcomePending, Position: res.Position}, nil
	}
	return MarkResult{Outcome: statelog.OutcomeUnknown}, nil
}

// marksIn is the person's code inbox marks in tx and the position of the
// record that last wrote them — zero marks and zero for a person who never
// marked anything.
func marksIn(ctx context.Context, tx *sql.Tx, handle string) (inbox.Marks[Reason], int64, error) {
	var (
		m                                inbox.Marks[Reason]
		stream, read, unread, snoozed, p string
		seen, position                   int64
	)
	err := tx.QueryRowContext(ctx, personMarksSQL, handle).Scan(&m.Generation, &stream, &seen, &read, &unread,
		&snoozed, &p, &position)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return inbox.Marks[Reason]{}, 0, nil
	case err != nil:
		return inbox.Marks[Reason]{}, 0, fmt.Errorf("code: read %s's code inbox marks: %w", handle, err)
	}
	if seen != 0 {
		m.SeenThrough = inbox.Position{Stream: stream, Generation: uint32(seen / statelog.GenerationStride),
			Seq: uint64(seen % statelog.GenerationStride)}
	}
	for _, part := range []struct {
		raw string
		to  any
	}{{read, &m.Read}, {unread, &m.Unread}, {snoozed, &m.Snoozed}, {p, &m.PrimaryReasons}} {
		if err := json.Unmarshal([]byte(part.raw), part.to); err != nil {
			return inbox.Marks[Reason]{}, 0, fmt.Errorf("code: decode %s's code inbox marks: %w", handle, err)
		}
	}
	return normalMarks(m), position, nil
}

// noticePositionsIn is the packed position of every record ids names among
// handle's own code notices, and the first id that is none of them, "" when
// every one is.
func noticePositionsIn(ctx context.Context, tx *sql.Tx, handle string, ids []string) (map[string]uint64, string,
	error) {

	out := make(map[string]uint64, len(ids))
	for _, id := range ids {
		var packed int64
		switch err := tx.QueryRowContext(ctx, noticePositionSQL, id, handle).Scan(&packed); {
		case errors.Is(err, sql.ErrNoRows):
			return nil, id, nil
		case err != nil:
			return nil, "", fmt.Errorf("code: read where notice %s of %s sits: %w", id, handle, err)
		}
		out[id] = uint64(packed)
	}
	return out, "", nil
}

// The marks' reads, each registered in plan_test.go: a person's row by its
// key, and one notice of theirs by the notice table's key — named, since the
// planner would otherwise take the person's inbox range
// (code_notifications_inbox_idx) and read every notice they hold to find one.
// The row's one write is the applier's (apply_marks.go).
const (
	personMarksSQL = `SELECT generation, seen_stream, seen_through, read, unread, snoozed, primary_reasons,
		position FROM code_person_marks WHERE handle = ?`
	noticePositionSQL = `SELECT log_seq FROM code_notifications INDEXED BY sqlite_autoindex_code_notifications_1
		WHERE record_id = ? AND recipient = ?`
)
