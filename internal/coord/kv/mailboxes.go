package kv

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the seat mailbox registry ----------------------------------------- //

// mailboxRecord is one registry row on the wire. The handle is the KEY, so a
// record cannot describe a seat other than the one it is filed under.
//
// Both stamps are POINTERS, for the reason channelRecord.ClosedAt is one: the
// zero instant is the "not absent" and "not retiring" value, and an absent
// field is the one encoding a decoder cannot mistake for an instant.
type mailboxRecord struct {
	AbsentSince   *time.Time `json:"absent_since,omitempty"`
	RetiringSince *time.Time `json:"retiring_since,omitempty"`
}

func encodeMailbox(rec coord.MailboxRecord) ([]byte, error) {
	var wire mailboxRecord
	if !rec.AbsentSince.IsZero() {
		at := rec.AbsentSince.UTC()
		wire.AbsentSince = &at
	}
	if !rec.RetiringSince.IsZero() {
		at := rec.RetiringSince.UTC()
		wire.RetiringSince = &at
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("coord/kv: encode the mailbox record: %w", err)
	}
	return raw, nil
}

func decodeMailbox(handle string, raw []byte, version uint64) (coord.MailboxRecord, error) {
	var wire mailboxRecord
	if err := json.Unmarshal(raw, &wire); err != nil {
		return coord.MailboxRecord{}, unavailable("decode the mailbox record", err)
	}
	rec := coord.MailboxRecord{Handle: handle, Version: version}
	if wire.AbsentSince != nil {
		rec.AbsentSince = wire.AbsentSince.UTC()
	}
	if wire.RetiringSince != nil {
		rec.RetiringSince = wire.RetiringSince.UTC()
	}
	return rec, nil
}

// Mailbox reads one record.
func (f *FleetStore) Mailbox(ctx context.Context, handle string) (coord.MailboxRecord, bool, error) {
	if handle == "" {
		return coord.MailboxRecord{}, false, errors.New("coord/kv: a mailbox record needs a handle")
	}
	entry, err := f.mailboxes.Get(ctx, encodeKey(handle))
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return coord.MailboxRecord{}, false, nil
	case err != nil:
		return coord.MailboxRecord{}, false, unavailable("read the mailbox record", err)
	}
	rec, err := decodeMailbox(handle, entry.Value(), entry.Revision())
	if err != nil {
		return coord.MailboxRecord{}, false, err
	}
	return rec, true, nil
}

// Mailboxes returns every record, ordered by handle.
//
// ONE PASS through [FleetStore.each], never the client's key lister: that one
// stops on the nil a closed subscription yields, so a listing cut off part way
// comes back short with no error, and a short listing here is a sweep that
// concludes a removed seat has no mailbox left to retire. See the package doc
// for the rule and walk.go for the pass.
func (f *FleetStore) Mailboxes(ctx context.Context) ([]coord.MailboxRecord, error) {
	var out []coord.MailboxRecord
	err := f.each(ctx, f.mailboxes, func(kve jetstream.KeyValueEntry) error {
		handle, ok := decodeKey(kve.Key())
		if !ok {
			// A key this backend did not write. Skipped rather than
			// guessed at: an invented handle would send a sweep to delete
			// the mailbox of a seat nobody named.
			return nil
		}
		rec, err := decodeMailbox(handle, kve.Value(), kve.Revision())
		if err != nil {
			return err
		}
		out = append(out, rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b coord.MailboxRecord) int { return cmp.Compare(a.Handle, b.Handle) })
	return out, nil
}

// CreateMailbox writes a new record, leaving an existing one alone.
//
// Create rather than Put, so the first writer wins and every other gets
// ErrKeyExists: a record that exists may be mid-way through a retirement, and
// only an update conditioned on the version its writer read may change it. A
// key a previous retirement purged is created again, which the client does by
// conditioning on the purge marker's own revision.
func (f *FleetStore) CreateMailbox(ctx context.Context, rec coord.MailboxRecord) (coord.MailboxRecord, bool, error) {
	if rec.Handle == "" {
		return coord.MailboxRecord{}, false, errors.New("coord/kv: a mailbox record needs a handle")
	}
	raw, err := encodeMailbox(rec)
	if err != nil {
		return coord.MailboxRecord{}, false, err
	}
	revision, err := f.mailboxes.Create(ctx, encodeKey(rec.Handle), raw)
	switch {
	case errors.Is(err, jetstream.ErrKeyExists):
		return coord.MailboxRecord{}, false, nil
	case err != nil:
		return coord.MailboxRecord{}, false, unavailable("create the mailbox record", err)
	}
	return storedMailbox(rec, revision), true, nil
}

// UpdateMailbox writes a record at the version it was read at.
func (f *FleetStore) UpdateMailbox(ctx context.Context, rec coord.MailboxRecord) (coord.MailboxRecord, bool, error) {
	if rec.Handle == "" {
		return coord.MailboxRecord{}, false, errors.New("coord/kv: a mailbox record needs a handle")
	}
	if rec.Version == 0 {
		// NO VERSION IS A LOST RACE, never an unconditional write. The
		// client reads an expected revision of 0 as "the key must not exist
		// yet", so passing one through would CREATE a record for a caller
		// that never read one, where the contract says it must lose.
		return coord.MailboxRecord{}, false, nil
	}
	raw, err := encodeMailbox(rec)
	if err != nil {
		return coord.MailboxRecord{}, false, err
	}
	revision, err := f.mailboxes.Update(ctx, encodeKey(rec.Handle), raw, rec.Version)
	switch {
	case errors.Is(err, jetstream.ErrKeyRevisionMismatch), errors.Is(err, jetstream.ErrKeyNotFound):
		// A LOST RACE, not a fault. A purged record lands here too: its
		// purge marker is a newer revision than the one the caller read.
		return coord.MailboxRecord{}, false, nil
	case err != nil:
		return coord.MailboxRecord{}, false, unavailable("update the mailbox record", err)
	}
	return storedMailbox(rec, revision), true, nil
}

// DeleteMailbox removes a record at a version.
//
// Purge rather than Delete, matching every other ageless bucket here: a Delete
// leaves a tombstone revision, and a bucket with no TTL keeps every one of them
// for the life of the deployment.
func (f *FleetStore) DeleteMailbox(ctx context.Context, handle string, version uint64) (bool, error) {
	if handle == "" {
		return false, errors.New("coord/kv: a mailbox record needs a handle")
	}
	if version == 0 {
		// The client drops a LastRevision of 0 and purges unconditionally,
		// so a caller that never read a version would delete whatever is
		// there, a retirement in flight included.
		return false, nil
	}
	err := f.mailboxes.Purge(ctx, encodeKey(handle), jetstream.LastRevision(version))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, jetstream.ErrKeyRevisionMismatch), errors.Is(err, jetstream.ErrKeyNotFound):
		return false, nil
	default:
		return false, unavailable("delete the mailbox record", err)
	}
}

// storedMailbox is what a successful write stored: the caller's stamps as the
// wire carries them, and the revision the store assigned.
func storedMailbox(rec coord.MailboxRecord, revision uint64) coord.MailboxRecord {
	rec.Version = revision
	if !rec.AbsentSince.IsZero() {
		rec.AbsentSince = rec.AbsentSince.UTC()
	}
	if !rec.RetiringSince.IsZero() {
		rec.RetiringSince = rec.RetiringSince.UTC()
	}
	return rec
}
