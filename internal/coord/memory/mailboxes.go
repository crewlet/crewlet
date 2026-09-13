package memory

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the seat mailbox registry ----------------------------------------- //

// Mailbox reads one record.
func (f *Fleet) Mailbox(_ context.Context, handle string) (coord.MailboxRecord, bool, error) {
	if handle == "" {
		return coord.MailboxRecord{}, false, errors.New("coord/memory: a mailbox record needs a handle")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.mailboxes[handle]
	return rec, ok, nil
}

// Mailboxes returns every record, ordered by handle.
func (f *Fleet) Mailboxes(context.Context) ([]coord.MailboxRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]coord.MailboxRecord, 0, len(f.mailboxes))
	for _, rec := range f.mailboxes {
		out = append(out, rec)
	}
	slices.SortFunc(out, func(a, b coord.MailboxRecord) int { return cmp.Compare(a.Handle, b.Handle) })
	return out, nil
}

// CreateMailbox writes a new record, leaving an existing one alone.
func (f *Fleet) CreateMailbox(_ context.Context, rec coord.MailboxRecord) (coord.MailboxRecord, bool, error) {
	if rec.Handle == "" {
		return coord.MailboxRecord{}, false, errors.New("coord/memory: a mailbox record needs a handle")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.mailboxes[rec.Handle]; exists {
		return coord.MailboxRecord{}, false, nil
	}
	stored := f.storeMailboxLocked(rec)
	return stored, true, nil
}

// UpdateMailbox writes a record at the version it was read at.
func (f *Fleet) UpdateMailbox(_ context.Context, rec coord.MailboxRecord) (coord.MailboxRecord, bool, error) {
	if rec.Handle == "" {
		return coord.MailboxRecord{}, false, errors.New("coord/memory: a mailbox record needs a handle")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	current, ok := f.mailboxes[rec.Handle]
	if !ok || current.Version != rec.Version {
		return coord.MailboxRecord{}, false, nil
	}
	return f.storeMailboxLocked(rec), true, nil
}

// DeleteMailbox removes a record at a version.
func (f *Fleet) DeleteMailbox(_ context.Context, handle string, version uint64) (bool, error) {
	if handle == "" {
		return false, errors.New("coord/memory: a mailbox record needs a handle")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	current, ok := f.mailboxes[handle]
	if !ok || current.Version != version {
		return false, nil
	}
	delete(f.mailboxes, handle)
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
	f.mailboxVersion++
	rec.Version = f.mailboxVersion
	rec.AbsentSince = utcUnlessZero(rec.AbsentSince)
	rec.RetiringSince = utcUnlessZero(rec.RetiringSince)
	f.mailboxes[rec.Handle] = rec
	return rec
}

func utcUnlessZero(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return t.UTC()
}
