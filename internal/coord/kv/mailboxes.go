package kv

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the seat mailbox registry ----------------------------------------- //

// mailboxRecord is one registry row on the wire. The seat's ID is the KEY, so
// a record cannot describe a seat other than the one it is filed under; the
// HANDLE, not an identity, rides the value so a reader or a retirement log
// line knows whose mailbox it is.
//
// Both stamps are POINTERS, as channelRecord.ClosedAt is: the zero instant
// means "not absent" / "not retiring", and an absent field is the one encoding
// a decoder cannot mistake for an instant.
type mailboxRecord struct {
	Handle        string     `json:"handle,omitempty"`
	AbsentSince   *time.Time `json:"absent_since,omitempty"`
	RetiringSince *time.Time `json:"retiring_since,omitempty"`
}

func encodeMailbox(rec coord.MailboxRecord) ([]byte, error) {
	wire := mailboxRecord{Handle: rec.Handle}
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

func decodeMailbox(seat uuid.UUID, raw []byte, version uint64) (coord.MailboxRecord, error) {
	var wire mailboxRecord
	if err := json.Unmarshal(raw, &wire); err != nil {
		return coord.MailboxRecord{}, unavailable("decode the mailbox record", err)
	}
	rec := coord.MailboxRecord{Seat: seat, Handle: wire.Handle, Version: version}
	if wire.AbsentSince != nil {
		rec.AbsentSince = wire.AbsentSince.UTC()
	}
	if wire.RetiringSince != nil {
		rec.RetiringSince = wire.RetiringSince.UTC()
	}
	return rec, nil
}

// seatOfKey reads a registry key back as its seat id: a canonical uuid only,
// the one spelling this backend writes ([uuid.UUID.String]).
func seatOfKey(key string) (uuid.UUID, bool) {
	name, ok := decodeKey(key)
	if !ok {
		return uuid.Nil, false
	}
	seat, err := uuid.Parse(name)
	if err != nil || seat == uuid.Nil || seat.String() != name {
		return uuid.Nil, false
	}
	return seat, true
}

// Mailbox reads one record.
func (f *FleetStore) Mailbox(ctx context.Context, seat uuid.UUID) (coord.MailboxRecord, bool, error) {
	if seat == uuid.Nil {
		return coord.MailboxRecord{}, false, errors.New("coord/kv: a mailbox record needs a seat id")
	}
	entry, err := f.get(ctx, f.mailboxes, encodeKey(seat.String()))
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return coord.MailboxRecord{}, false, nil
	case err != nil:
		return coord.MailboxRecord{}, false, unavailable("read the mailbox record", err)
	}
	rec, err := decodeMailbox(seat, entry.Value(), entry.Revision())
	if err != nil {
		return coord.MailboxRecord{}, false, err
	}
	return rec, true, nil
}

// Mailboxes returns every record, ordered by seat id.
//
// ONE CERTIFIED WALK, like every listing (package doc, "Every listing is ONE
// CERTIFIED PASS, and never the client's ListKeys"): a record left out is one
// the retention sweep's discovery step reads as a mailbox that escaped the
// registry (internal/maintenance/mailboxes.go).
func (f *FleetStore) Mailboxes(ctx context.Context) ([]coord.MailboxRecord, error) {
	var out []coord.MailboxRecord
	err := f.each(ctx, f.mailboxes, func(kve jetstream.KeyValueEntry) error {
		seat, ok := seatOfKey(kve.Key())
		if !ok {
			// A key this backend did not write. Skipped, not guessed at: an
			// invented id would send a sweep to delete a mailbox nobody named.
			return nil
		}
		// RAISED, never skipped: a dropped record is a record left out.
		rec, err := decodeMailbox(seat, kve.Value(), kve.Revision())
		if err != nil {
			return err
		}
		out = append(out, rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b coord.MailboxRecord) int {
		return cmp.Compare(a.Seat.String(), b.Seat.String())
	})
	return out, nil
}

// CreateMailbox writes a new record, leaving an existing one alone.
//
// Create, not Put: an existing record may be mid-retirement, and only an
// update conditioned on the version its writer read may change it. A purged
// key is created again through [createKey], which asks the leader rather than
// trusting a marker a sweep may have removed, and reports a racing creator
// that won as [jetstream.ErrKeyExists].
func (f *FleetStore) CreateMailbox(ctx context.Context, rec coord.MailboxRecord) (coord.MailboxRecord, bool, error) {
	if rec.Seat == uuid.Nil {
		return coord.MailboxRecord{}, false, errors.New("coord/kv: a mailbox record needs a seat id")
	}
	if err := rec.Check(); err != nil {
		return coord.MailboxRecord{}, false, err
	}
	raw, err := encodeMailbox(rec)
	if err != nil {
		return coord.MailboxRecord{}, false, err
	}
	revision, err := f.create(ctx, f.mailboxes, encodeKey(rec.Seat.String()), raw)
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
	if rec.Seat == uuid.Nil {
		return coord.MailboxRecord{}, false, errors.New("coord/kv: a mailbox record needs a seat id")
	}
	if err := rec.Check(); err != nil {
		return coord.MailboxRecord{}, false, err
	}
	if rec.Version == 0 {
		// NO VERSION IS A LOST RACE: the client reads revision 0 as "must
		// not exist yet", so passing it would CREATE a record for a caller
		// that never read one.
		return coord.MailboxRecord{}, false, nil
	}
	raw, err := encodeMailbox(rec)
	if err != nil {
		return coord.MailboxRecord{}, false, err
	}
	revision, err := f.mailboxes.Update(ctx, encodeKey(rec.Seat.String()), raw, rec.Version)
	switch {
	case errors.Is(err, jetstream.ErrKeyRevisionMismatch), errors.Is(err, jetstream.ErrKeyNotFound):
		// A LOST RACE, not a fault; a purged record too, its marker being
		// newer than the revision the caller read.
		return coord.MailboxRecord{}, false, nil
	case err != nil:
		return coord.MailboxRecord{}, false, unavailable("update the mailbox record", err)
	}
	return storedMailbox(rec, revision), true, nil
}

// DeleteMailbox removes a record at a version.
//
// PURGED (markers.go, "Every removal is a purge"); [FleetStore.SweepMarkers]
// removes the marker it leaves.
func (f *FleetStore) DeleteMailbox(ctx context.Context, seat uuid.UUID, version uint64) (bool, error) {
	if seat == uuid.Nil {
		return false, errors.New("coord/kv: a mailbox record needs a seat id")
	}
	if version == 0 {
		// The client drops a LastRevision of 0 and purges unconditionally,
		// so a caller that never read a version would delete whatever is
		// there, a retirement in flight included.
		return false, nil
	}
	err := f.mailboxes.Purge(ctx, encodeKey(seat.String()), jetstream.LastRevision(version))
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
