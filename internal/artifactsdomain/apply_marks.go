package artifactsdomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/inbox"
)

// A PERSON'S MARKS, APPLIED: a marks record's gesture laid over the row the
// person's last record left, the applier's write of artifact_person_marks and
// no other code's. marks.go has the record, its decide and why the apply reads
// no notice.

// applyMarks applies one marks record: the gesture it carries, with the
// positions its decide found, to the row the person's last record left, at the
// record's broker instant. A record whose payload names another person than
// its subject writes nothing, as a refusal does.
func (a *Applier) applyMarks(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	payload, err := DecodeMutation(c.record)
	if err != nil {
		return 0, err
	}
	p, ok := payload.(PersonMarks)
	if !ok || p.Handle != c.record.Subject.ID {
		return 0, nil
	}
	marks, position, err := marksIn(ctx, tx, p.Handle)
	if err != nil || position >= c.packed {
		return 0, err
	}
	if err := inbox.Apply(&marks, p.Gesture.Gesture(), p.Positions, c.brokerAt); err != nil {
		// THE SAME ON EVERY NODE: the rules, the row before and the instant
		// are each node's alike, so the record writes nothing anywhere.
		return 0, nil //nolint:nilerr // a gesture the rules refuse at the apply writes no row on any node
	}
	if err := writeMarks(ctx, tx, p.Handle, marks, c.packed); err != nil {
		return 0, err
	}
	return 1, nil
}

// writeMarks stores a person's marks as the record at position leaves them.
func writeMarks(ctx context.Context, tx *sql.Tx, handle string, m inbox.Marks[NotifyReason], position int64) error {
	var cols [4]string
	for i, part := range []struct {
		v     any
		empty bool
	}{{m.Read, len(m.Read) == 0}, {m.Unread, len(m.Unread) == 0}, {m.Snoozed, len(m.Snoozed) == 0},
		{m.PrimaryReasons, len(m.PrimaryReasons) == 0}} {
		if part.empty {
			cols[i] = "[]"
			continue
		}
		b, err := json.Marshal(part.v)
		if err != nil {
			return fmt.Errorf("artifactsdomain: encode %s's inbox marks: %w", handle, err)
		}
		cols[i] = string(b)
	}
	if _, err := tx.ExecContext(ctx, writeMarksSQL, handle, m.Generation, m.SeenThrough.Stream,
		int64(m.SeenThrough.Packed()), cols[0], cols[1], cols[2], cols[3], position); err != nil {
		return fmt.Errorf("artifactsdomain: write %s's inbox marks: %w", handle, err)
	}
	return nil
}

// writeMarksSQL writes a person's marks row whole by its key.
const writeMarksSQL = `INSERT INTO artifact_person_marks (handle, generation, seen_stream, seen_through, read,
		unread, snoozed, primary_reasons, position)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT (handle) DO UPDATE SET generation = excluded.generation, seen_stream = excluded.seen_stream,
		seen_through = excluded.seen_through, read = excluded.read, unread = excluded.unread,
		snoozed = excluded.snoozed, primary_reasons = excluded.primary_reasons, position = excluded.position`
