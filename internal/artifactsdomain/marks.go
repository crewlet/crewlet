package artifactsdomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A PERSON'S MARKS OVER THEIR COMMENT NOTICES, by internal/inbox's rules: a
// gesture decided on their own reader subject, the record carrying the notice
// positions it found, since the apply reads no notice (that table diverges).

// MarkGesture is one gesture over a person's comment notices, as a marks
// record and an estate request carry it: [inbox.Gesture] over [NotifyReason],
// spelled with its keys.
type MarkGesture struct {
	Read           []string           `json:"read,omitempty"`
	Unread         []string           `json:"unread,omitempty"`
	Snooze         []MarkSnooze       `json:"snooze,omitempty"`
	Unsnooze       []string           `json:"unsnooze,omitempty"`
	ReadThrough    *statelog.Position `json:"read_through,omitempty"`
	PrimaryReasons *[]NotifyReason    `json:"primary_reasons,omitempty"`
}

// MarkSnooze is one notice put off until an instant.
type MarkSnooze struct {
	RecordID string    `json:"record_id"`
	Until    time.Time `json:"until"`
}

// Gesture is g as internal/inbox's rules read it.
func (g MarkGesture) Gesture() inbox.Gesture[NotifyReason] {
	out := inbox.Gesture[NotifyReason]{Read: g.Read, Unread: g.Unread, Unsnooze: g.Unsnooze,
		ReadThrough: g.ReadThrough, PrimaryReasons: g.PrimaryReasons}
	for _, s := range g.Snooze {
		out.Snooze = append(out.Snooze, inbox.Snooze{RecordID: s.RecordID, Until: s.Until})
	}
	return out
}

// MarkGestureOf is g as a marks record carries it.
func MarkGestureOf(g inbox.Gesture[NotifyReason]) MarkGesture {
	out := MarkGesture{Read: g.Read, Unread: g.Unread, Unsnooze: g.Unsnooze, ReadThrough: g.ReadThrough,
		PrimaryReasons: g.PrimaryReasons}
	for _, s := range g.Snooze {
		out.Snooze = append(out.Snooze, MarkSnooze{RecordID: s.RecordID, Until: s.Until})
	}
	return out
}

// PersonMarks is a marks record's payload ([OpInboxMarks]): whose record, the
// gesture, and the PACKED position of each notice it names as the decide read
// it, which the apply uses since it reads no notice.
type PersonMarks struct {
	V         int               `json:"v"`
	Handle    string            `json:"handle"`
	Gesture   MarkGesture       `json:"gesture"`
	Positions map[string]uint64 `json:"positions,omitempty"`
}

// MarkInput is one gesture over a person's own comment notices, and Seen, the
// asker's floor on the artifacts log, which the decide waits for: the page may
// come from a node ahead of this one. The zero position waits for nothing.
type MarkInput struct {
	Gesture MarkGesture       `json:"gesture"`
	Seen    statelog.Position `json:"seen,omitzero"`
}

// MarkInbox decides one gesture over the calling person's comment notices. A
// gesture that moves nothing publishes nothing. A gesture the rules refuse is
// [ErrInvalid] with their sentence, or their [inbox.ErrFull] refusal.
func (w *Writer) MarkInbox(ctx context.Context, c Call, in MarkInput) (Written, error) {
	if err := validCall(c); err != nil {
		return Written{}, err
	}
	actor := c.Authority.Actor
	if actor.Kind != iam.ActorHuman {
		return Written{}, fmt.Errorf("%w: an inbox is a person's, marked by the person who holds it", ErrInvalid)
	}
	handle := actor.Name
	if err := checkName("the reader", handle); err != nil {
		return Written{}, err
	}
	stream := Domain{}.Stream().Name
	if !in.Seen.IsZero() && in.Seen.Stream != stream {
		return Written{}, fmt.Errorf("%w: seen: %s is a position on another log than %s", ErrInvalid, in.Seen,
			stream)
	}
	gesture := in.Gesture.Gesture()
	if err := inbox.CheckGesture(gesture, stream, NotifyReasons, w.now()); err != nil {
		return Written{}, markRefused(err)
	}
	opID, err := w.operation(c, string(OpInboxMarks), handle, in.Gesture)
	if err != nil {
		return Written{}, err
	}
	subject := ReaderSubject(handle)
	scope := ScopeSet{Terms: []ScopeTerm{{Kind: TermReader, ID: handle}}}
	req := request(subject, scope, opID, statelog.PatternArbitrated,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			at := w.now()
			prior, _, err := marksIn(ctx, tx, handle)
			if err != nil {
				return statelog.Decision{}, err
			}
			positions, missing, err := noticePositionsIn(ctx, tx, handle, gesture.Records())
			if err != nil {
				return statelog.Decision{}, err
			}
			if missing != "" {
				return statelog.Decision{}, fmt.Errorf("%w: record %s is not one of %s's comment notices; a "+
					"mark names the record_id an inbox notice carries", ErrInvalid, missing, handle)
			}
			post := prior
			if err := inbox.Apply(&post, gesture, positions, at); err != nil {
				return statelog.Decision{}, markRefused(err)
			}
			if reflect.DeepEqual(normalMarks(post), normalMarks(prior)) {
				return statelog.Decision{}, nil
			}
			return w.decision(stamp, c, subject, OpInboxMarks, scope, opID,
				PersonMarks{V: baseRecordVersion, Handle: handle, Gesture: in.Gesture, Positions: positions}, at)
		})
	req.Session = in.Seen
	res, err := w.publish(ctx, req)
	return Written{Result: res}, err
}

// markRefused is a gesture the inbox's rules refused, as this domain answers
// it: a list past its ceiling is the rules' own refusal, anything else
// [ErrInvalid] with the rules' sentence.
func markRefused(err error) error {
	if errors.Is(err, inbox.ErrFull) {
		return err
	}
	return fmt.Errorf("%w: %s", ErrInvalid, inbox.Sentence(err))
}

// normalMarks is m with every empty list nil, so two marks that hold the same
// are equal however each was built.
func normalMarks(m inbox.Marks[NotifyReason]) inbox.Marks[NotifyReason] {
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

// marksIn is the person's comment inbox marks in tx and the position of the
// record that last wrote them: zero marks and zero for a person who never
// marked anything.
func marksIn(ctx context.Context, tx *sql.Tx, handle string) (inbox.Marks[NotifyReason], int64, error) {
	var (
		m                                inbox.Marks[NotifyReason]
		stream, read, unread, snoozed, p string
		seen, position                   int64
	)
	err := tx.QueryRowContext(ctx, personMarksSQL, handle).Scan(&m.Generation, &stream, &seen, &read, &unread,
		&snoozed, &p, &position)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return inbox.Marks[NotifyReason]{}, 0, nil
	case err != nil:
		return inbox.Marks[NotifyReason]{}, 0, fmt.Errorf("artifactsdomain: read %s's inbox marks: %w", handle, err)
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
			return inbox.Marks[NotifyReason]{}, 0, fmt.Errorf("artifactsdomain: decode %s's inbox marks: %w",
				handle, err)
		}
	}
	return normalMarks(m), position, nil
}

// noticePositionsIn is the packed position of each of ids among handle's own
// comment notices, and the first id that is none ("" if all are): one
// statement, as the decide reruns on every round the broker refuses.
func noticePositionsIn(ctx context.Context, tx *sql.Tx, handle string, ids []string) (map[string]uint64, string,
	error) {

	out := make(map[string]uint64, len(ids))
	if len(ids) == 0 {
		return out, "", nil
	}
	list, err := json.Marshal(ids)
	if err != nil {
		return nil, "", fmt.Errorf("artifactsdomain: name the notices a mark of %s names: %w", handle, err)
	}
	rows, err := tx.QueryContext(ctx, noticePositionsSQL, string(list), handle)
	if err != nil {
		return nil, "", fmt.Errorf("artifactsdomain: read where the notices a mark of %s names sit: %w", handle, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var record string
		var packed int64
		if err := rows.Scan(&record, &packed); err != nil {
			return nil, "", fmt.Errorf("artifactsdomain: read where the notices a mark of %s names sit: %w",
				handle, err)
		}
		out[record] = uint64(packed)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("artifactsdomain: read where the notices a mark of %s names sit: %w", handle, err)
	}
	for _, id := range ids {
		if _, found := out[id]; !found {
			return nil, id, nil
		}
	}
	return out, "", nil
}

// The marks' reads: a person's row by its key, which the applier alone writes
// (apply_marks.go); and the notices of theirs a gesture names, one seek of the
// notice table's key an id of the JSON array bound.
const (
	personMarksSQL = `SELECT generation, seen_stream, seen_through, read, unread, snoozed, primary_reasons,
		position FROM artifact_person_marks WHERE handle = ?`
	noticePositionsSQL = `SELECT n.record_id, n.log_seq
		FROM json_each(?1) AS j CROSS JOIN artifact_notifications AS n
		WHERE n.record_id = CAST(j.value AS TEXT) AND n.recipient = ?2`
)
