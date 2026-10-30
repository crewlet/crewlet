package artifactdata

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The publisher's three seams: what it reads, what refuses a write it must not
// make, and what tells a dropped record from a lost race.

// NewRows builds the publisher's read seam over the log spec names.
func NewRows(db store.ReplicatedReader, spec statelog.StreamSpec) (statelog.Rows, error) {
	return statelog.NewRows(db, Domain{}, spec, guards)
}

// guards answers the two object-level facts a first write needs. A STORE HAS A
// DELETION MARKER and no guarding row: a store is created by whichever record
// reaches it first, sequenced or arbitrated, so no write is a create the
// framework guards.
func guards(ctx context.Context, tx *sql.Tx, subj statelog.Subject) (deleted, guard bool, err error) {
	if ObjectKind(subj.Kind) != KindStore {
		return false, false, nil
	}
	_, marked, err := deletionMarker(ctx, tx, subj.ID)
	return marked, false, err
}

// Fence refuses a write this node must not make: Evicted on every append, from
// rows this node already has, and ClearForZero before an expectation of zero.
type Fence struct {
	db     store.ReplicatedReader
	nodeID string

	Floor     func(ctx context.Context, generation uint32) (uint64, error)
	Ends      func(ctx context.Context) (statelog.LogEnds, error)
	Committed func() statelog.Position
}

// NewFence builds it.
func NewFence(db store.ReplicatedReader, nodeID string) *Fence {
	return &Fence{db: db, nodeID: nodeID}
}

// Evicted reports this node's own eviction, from its own applied rows.
// UNREADABLE IS NOT "not evicted".
func (f *Fence) Evicted(ctx context.Context) (bool, error) {
	if f == nil || f.db.IsZero() || f.nodeID == "" {
		return false, nil
	}
	var from, readmitted sql.NullInt64
	err := f.db.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT from_position, readmitted_position FROM artifactdata_evictions
			WHERE node_id = ?`, f.nodeID).Scan(&from, &readmitted)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("artifactdata: read this node's own eviction: %w", err)
	}
	if !from.Valid {
		return false, nil
	}
	return !readmitted.Valid || readmitted.Int64 < from.Int64, nil
}

// ClearForZero verifies, freshly, that publishing at an expectation of zero is
// safe from this node ([statelog.ZeroFence]).
func (f *Fence) ClearForZero(ctx context.Context, cursor statelog.Position) error {
	return statelog.ZeroFence{
		Evicted: f.Evicted, Floor: f.Floor, Ends: f.Ends, Committed: f.Committed,
	}.ClearForZero(ctx, cursor)
}

// Gates answers whether a durable record produced rows on NO node: the two
// gates [Applier.Gated] installs, read from the publisher's side.
type Gates struct{ db store.ReplicatedReader }

// NewGates builds it.
func NewGates(db store.ReplicatedReader) *Gates { return &Gates{db: db} }

// GatedAt reports whether a record at p applies nowhere: THE DELETION GATE
// FIRST, the purge's own record exempt by its operation id, then the writer's
// eviction window, in one transaction.
func (g *Gates) GatedAt(ctx context.Context, subj statelog.Subject,
	writer, opID string, p statelog.Position, _ []byte) (statelog.Reason, bool, error) {

	if g == nil || g.db.IsZero() {
		return "", false, nil
	}
	var reason statelog.Reason
	err := g.db.Read(ctx, func(tx *sql.Tx) error {
		if artifact, ok := gatedStore(subj.Kind, subj.ID); ok {
			author, marked, err := deletionMarker(ctx, tx, artifact)
			if err != nil {
				return err
			}
			if marked && author != opID {
				reason = statelog.ReasonDeleted
				return nil
			}
		}
		if writer == "" {
			return nil
		}
		evicted, err := evictedAt(ctx, tx, writer, p.Packed())
		if err != nil {
			return err
		}
		if evicted {
			reason = statelog.ReasonEvicted
		}
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return reason, reason != "", nil
}

// GenerationRecord is the datastore log's [statelog.GenerationEncoder]: its
// half of a reanchor, one record the applier writes
// `artifactdata_log_generations` from.
type GenerationRecord struct{}

// GenerationSubject is the subject generation gen's record is published on.
func (GenerationRecord) GenerationSubject(gen uint32) (statelog.Subject, bool) {
	return GenerationSubject(gen).framework(), true
}

// GenerationRecord encodes the reanchor's record for the new generation,
// create-only, under the operation id the framework derives and authored by
// the operator who ran it.
func (GenerationRecord) GenerationRecord(f statelog.GenerationFacts) (statelog.GenerationRecord, bool, error) {
	subject := GenerationSubject(f.Generation)
	actor := generationActor(f)
	body, err := json.Marshal(Generation{
		V: GateRecordVersion, Generation: f.Generation, By: actor.Name,
		PrevHighest: f.Inputs.Highest, StreamAt: f.Inputs.StreamCreatedAt.UTC(),
	})
	if err != nil {
		return statelog.GenerationRecord{}, false, fmt.Errorf("artifactdata: encode generation %d: %w",
			f.Generation, err)
	}
	opID := f.OpID()
	encoded, err := Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			OpID: opID, Subject: subject, Op: OpGeneration,
			CreatedAt: f.At.UTC(), Gen: f.Generation, Writer: f.Writer,
			Scope: ScopeSet{Subject: true},
		},
		Mutation: body,
		Actor:    actor.Name, ActorKind: actor.Kind, OperatorID: actor.OperatorID,
	})
	if err != nil {
		return statelog.GenerationRecord{}, false, err
	}
	return statelog.GenerationRecord{Subject: subject.framework(), OpID: opID, Payload: encoded}, true, nil
}

// generationActor is who a generation record names as its author: the party
// the facts name, read by the one rule every log's generation record reads it
// by ([statelog.GenerationFacts.Author]), already in the identity
// vocabulary's actor kinds this log's columns hold. NOBODY NAMED IS THE NODE,
// recorded under its own id as the engine.
func generationActor(f statelog.GenerationFacts) iam.Actor {
	party, named := f.Author()
	if !named {
		return iam.Actor{Name: f.Writer, Kind: iam.ActorSystem}
	}
	return party
}
