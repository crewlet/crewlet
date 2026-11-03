package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/inbox"
)

// A PERSON'S MARKS, APPLIED: a marks record's gesture laid over the row the
// person's last record left — the applier's write of code_person_marks, and
// no other code's (the replicated estate is written by its log's applier
// alone). marks.go has the record, its decide and why the apply reads no
// notice.

// applyMarks applies one marks record: the gesture it carries, with the
// positions its decide found, to the row the person's last record left, at the
// record's broker instant (marks.go's head).
func (a *Applier) applyMarks(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	payload, err := DecodeMutation(at.record)
	if err != nil {
		return 0, err
	}
	p, ok := payload.(PersonMarks)
	if !ok || at.record.Op != OpMarks {
		return 0, fmt.Errorf("code: %s is not an op this build applies to a person's code inbox (the record at %s)",
			at.record.Op, at.position)
	}
	if p.Handle != at.record.Subject.ID {
		return 0, fmt.Errorf("code: the record at %s is on person.%s and its payload names %s — a record on one "+
			"person's subject may write only that person's marks", at.position, at.record.Subject.ID, p.Handle)
	}
	marks, _, err := marksIn(ctx, tx, p.Handle)
	if err != nil {
		return 0, err
	}
	if err := inbox.Apply(&marks, p.Gesture.Gesture(), p.Positions, at.brokerAt); err != nil {
		// THE SAME ON EVERY NODE: the rules, the row before and the instant
		// are each node's alike, so a refusal here is every node's, and the
		// record writes nothing anywhere rather than stopping an applier.
		return 0, nil //nolint:nilerr // a gesture the rules refuse at the apply writes no row on any node
	}
	if err := writeMarks(ctx, tx, p.Handle, marks, at.packed); err != nil {
		return 0, err
	}
	return 1, nil
}

// writeMarks stores a person's marks as the record at position leaves them.
func writeMarks(ctx context.Context, tx *sql.Tx, handle string, m inbox.Marks[Reason], position int64) error {
	list := func(v any, empty bool) (string, error) {
		if empty {
			return "[]", nil
		}
		b, err := json.Marshal(v)
		return string(b), err
	}
	var cols [4]string
	for i, part := range []struct {
		v     any
		empty bool
	}{{m.Read, len(m.Read) == 0}, {m.Unread, len(m.Unread) == 0}, {m.Snoozed, len(m.Snoozed) == 0},
		{m.PrimaryReasons, len(m.PrimaryReasons) == 0}} {
		var err error
		if cols[i], err = list(part.v, part.empty); err != nil {
			return fmt.Errorf("code: encode %s's code inbox marks: %w", handle, err)
		}
	}
	if _, err := tx.ExecContext(ctx, writeMarksSQL, handle, m.Generation, m.SeenThrough.Stream,
		int64(m.SeenThrough.Packed()), cols[0], cols[1], cols[2], cols[3], position); err != nil {
		return fmt.Errorf("code: write %s's code inbox marks: %w", handle, err)
	}
	return nil
}

// writeMarksSQL writes a person's marks row whole by its key, registered in
// plan_test.go.
const writeMarksSQL = `INSERT INTO code_person_marks (handle, generation, seen_stream, seen_through, read, unread,
		snoozed, primary_reasons, position)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT (handle) DO UPDATE SET generation = excluded.generation, seen_stream = excluded.seen_stream,
		seen_through = excluded.seen_through, read = excluded.read, unread = excluded.unread,
		snoozed = excluded.snoozed, primary_reasons = excluded.primary_reasons, position = excluded.position`
