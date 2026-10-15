package memory

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the seat mailbox registry ----------------------------------------- //

// Mailbox reads one record.
func (f *Fleet) Mailbox(_ context.Context, seat uuid.UUID) (coord.MailboxRecord, bool, error) {
	if seat == uuid.Nil {
		return coord.MailboxRecord{}, false, errors.New("coord/memory: a mailbox record needs a seat id")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.mailboxes[seat]
	return rec, ok, nil
}

// Mailboxes returns every record, ordered by seat id.
func (f *Fleet) Mailboxes(context.Context) ([]coord.MailboxRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]coord.MailboxRecord, 0, len(f.mailboxes))
	for _, rec := range f.mailboxes {
		out = append(out, rec)
	}
	slices.SortFunc(out, func(a, b coord.MailboxRecord) int {
		return cmp.Compare(a.Seat.String(), b.Seat.String())
	})
	return out, nil
}

// CreateMailbox writes a new record, leaving an existing one alone.
func (f *Fleet) CreateMailbox(_ context.Context, rec coord.MailboxRecord) (coord.MailboxRecord, bool, error) {
	if rec.Seat == uuid.Nil {
		return coord.MailboxRecord{}, false, errors.New("coord/memory: a mailbox record needs a seat id")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.mailboxes[rec.Seat]; exists {
		return coord.MailboxRecord{}, false, nil
	}
	stored := f.storeMailboxLocked(rec)
	return stored, true, nil
}

// UpdateMailbox writes a record at the version it was read at.
func (f *Fleet) UpdateMailbox(_ context.Context, rec coord.MailboxRecord) (coord.MailboxRecord, bool, error) {
	if rec.Seat == uuid.Nil {
		return coord.MailboxRecord{}, false, errors.New("coord/memory: a mailbox record needs a seat id")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	current, ok := f.mailboxes[rec.Seat]
	if !ok || current.Version != rec.Version {
		return coord.MailboxRecord{}, false, nil
	}
	return f.storeMailboxLocked(rec), true, nil
}

// DeleteMailbox removes a record at a version.
func (f *Fleet) DeleteMailbox(_ context.Context, seat uuid.UUID, version uint64) (bool, error) {
	if seat == uuid.Nil {
		return false, errors.New("coord/memory: a mailbox record needs a seat id")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	current, ok := f.mailboxes[seat]
	if !ok || current.Version != version {
		return false, nil
	}
	delete(f.mailboxes, seat)
	return true, nil
}

// storeMailboxLocked writes a record under the held lock with the next
// store-wide version, normalising both stamps to UTC.
//
// UTC explicitly, because the KV backend gets it for free from a JSON round
// trip and the two must not disagree about a stamp a caller handed in with a
// zone. A zero stamp stays zero: it is the "not absent" and "not retiring"
// value, and converting it would move it off the zero instant.
func (f *Fleet) storeMailboxLocked(rec coord.MailboxRecord) coord.MailboxRecord {
	f.version++
	rec.Version = f.version
	rec.AbsentSince = utcUnlessZero(rec.AbsentSince)
	rec.RetiringSince = utcUnlessZero(rec.RetiringSince)
	f.mailboxes[rec.Seat] = rec
	return rec
}

func utcUnlessZero(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return t.UTC()
}
