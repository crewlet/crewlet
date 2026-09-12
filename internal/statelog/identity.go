package statelog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// CONTRACT 1: A STREAM'S IDENTITY, and what a consumer must do when it moves.
//
// # The failure this exists for
//
// A stream that is deleted and remade restarts its sequences at 1. Every
// position a node holds then names a number in a space that no longer exists —
// and the new stream is EMPTY, so a consumer that reads it sees nothing and
// reports nothing. That is the shape of the failure: not an error, not a
// refusal, but a successful read of a stream that has forgotten everything.
//
// The broker's own creation instant is what detects it, and it is the only
// thing that can: a sequence cannot, because a recreated stream's sequences
// are perfectly plausible; a message count cannot, because an empty stream and
// an emptied one are the same count.
//
// # Why it is a named contract rather than a check each consumer writes
//
// Every consumer of a durable stream inherits this hazard, whether or not it
// is a state-log domain. [Runner] carries the instant beside its checkpoint
// and answers with a generation; a COMPACTED changelog has no checkpoint and
// no generation to answer with, so its only honest answer is to refuse the
// work that depended on the stream. Both need the same comparison, and a
// comparison written twice is one that is right in one place.

// StreamState is what a consumer has learned about a stream's identity.
type StreamState int

const (
	// StreamFirstSight is a stream this node has no record of. It is NOT a
	// recreation: a fresh node, a fresh company and a newly added consumer
	// all reach it, and refusing there would make a first boot an outage.
	StreamFirstSight StreamState = iota

	// StreamSame is the stream this node last saw.
	StreamSame

	// StreamRecreated is a DIFFERENT stream wearing the same name. Every
	// position below is meaningless and everything the old one held is
	// gone.
	StreamRecreated

	// StreamUnknown is a creation instant that could not be established —
	// the broker did not answer, or answered a zero instant.
	//
	// ITS OWN VALUE, because it is not "same". A consumer that read it as
	// same would resume against a stream it cannot identify, which is the
	// whole failure this contract is about; one that read it as recreated
	// would tear down a healthy node over a broker blip.
	StreamUnknown
)

// String renders the state for a log line.
func (s StreamState) String() string {
	switch s {
	case StreamFirstSight:
		return "first_sight"
	case StreamSame:
		return "same"
	case StreamRecreated:
		return "recreated"
	}
	return "unknown"
}

// identityResolution is the precision at which two observations of one instant
// are the same observation.
//
// A MICROSECOND, because that is what [store.EncodeTime] keeps: an instant
// written and read back is truncated there, while the broker reports its
// stream's creation with nanosecond precision. Comparing the two exactly makes
// EVERY boot after the first report a recreation — a node that refuses to
// hydrate any seat, for ever, over a difference nothing can see.
const identityResolution = time.Microsecond

// IdentityOf compares a stream's current creation instant against the one this
// node recorded.
//
// A PURE FUNCTION OVER TWO INSTANTS, so every consumer reaches the same verdict
// and the verdict is testable without a broker.
func IdentityOf(recorded, current time.Time, known bool) StreamState {
	if current.IsZero() {
		return StreamUnknown
	}
	if !known || recorded.IsZero() {
		return StreamFirstSight
	}
	if current.Truncate(identityResolution).Equal(recorded.Truncate(identityResolution)) {
		return StreamSame
	}
	return StreamRecreated
}

// ErrStreamRecreated reports a stream that is not the one this node last saw.
//
// A SENTINEL, because the caller's response is specific and is never a retry:
// what it holds derived from that stream is stale in a way no re-read repairs,
// and the honest move is to refuse the work rather than to serve an empty
// answer.
var ErrStreamRecreated = errors.New("statelog: the stream was recreated")

// RecordedIdentity reads the creation instant this node last recorded for a
// stream, from its OWN estate.
//
// # Why the node estate and not the replicated one
//
// This is a per-node observation, not shared state: two nodes can legitimately
// have seen a stream at different moments, and a donated snapshot must not
// carry the donor's answer. The framework's own checkpoint table is in the
// replicated estate and is walked by the backup manifest and the adoption
// path, both of which read every row as a DOMAIN — so a row for a stream that
// is not a domain would put a phantom domain in an artefact's manifest, and a
// recipient refuses an artefact naming a domain it does not register.
func RecordedIdentity(ctx context.Context, db *store.DB, stream string) (
	time.Time, bool, error) {

	var created int64
	err := db.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx,
			`SELECT created_at FROM stream_identity WHERE stream = ?`,
			stream).Scan(&created)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return time.Time{}, false, nil
	case err != nil:
		return time.Time{}, false, fmt.Errorf(
			"statelog: read %s's recorded identity: %w", stream, err)
	}
	return store.DecodeTime(created), true, nil
}

// RecordIdentity writes what this node has seen.
//
// LAST WRITER WINS, deliberately: a node that has decided to follow a
// recreated stream records the new instant, and the next boot then reads it as
// the same stream. The decision to follow one is the OPERATOR's — a reanchor —
// and this is the record of it rather than a second opinion about it.
func RecordIdentity(ctx context.Context, db *store.DB, stream string,
	created, now time.Time) error {

	if created.IsZero() {
		return nil
	}
	return db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO stream_identity (stream, created_at, seen_at)
			VALUES (?, ?, ?)
			ON CONFLICT (stream) DO UPDATE SET
				created_at = excluded.created_at, seen_at = excluded.seen_at`,
			stream, store.EncodeTime(created), store.EncodeTime(now))
		if err != nil {
			return fmt.Errorf("statelog: record %s's identity: %w", stream, err)
		}
		return nil
	})
}
