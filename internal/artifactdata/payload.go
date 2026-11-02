package artifactdata

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE TYPED PAYLOADS, one per (kind, op).
//
// A write carries its ops and the AUTHORITY the API decided, never a rule for
// computing either: the applier decides each op from the record and its own
// rows, and the role, the actor's kind and the limits in force are facts it
// reads off the stamp rather than asks another domain for.

// WritePayload is one batch of document ops.
type WritePayload struct {
	V         int                 `json:"v"`
	Write     artifacts.DataWrite `json:"write"`
	Authority artifacts.Authority `json:"authority"`
}

// UndoPayload restores what one earlier write changed.
//
// THE OPS ARE COMPLETE: decided by the writer from the undone write's own
// history row, each conditioned on the document still holding the version that
// write left it at, so the applier judges an undo exactly as it judges any
// write and a document a teammate has changed since is a conflict rather than
// their edit silently reverted.
type UndoPayload struct {
	V int `json:"v"`

	// Undoes is the position of the write being undone.
	Undoes int64 `json:"undoes"`

	Ops       []artifacts.DataOp  `json:"ops"`
	ClientID  string              `json:"client_id,omitempty"`
	Authority artifacts.Authority `json:"authority"`
}

// ConfigurePayload replaces the store's collections: every one the store keeps
// after the record, each with its rules compiled from its preset. A collection
// the store holds that the list omits is dropped, which the applier admits only
// for a collection holding no live document.
type ConfigurePayload struct {
	V           int                    `json:"v"`
	Collections []artifacts.Collection `json:"collections"`
	Authority   artifacts.Authority    `json:"authority"`
}

// PurgePayload removes the store permanently. It names its dependents' step
// for the operator reading the log: the artifact purge that ran it.
type PurgePayload struct {
	V int `json:"v"`
}

// Eviction is a node's eviction from this log, or its readmission. PINNED AT
// [GateRecordVersion] FOR EVER.
type Eviction struct {
	V          int       `json:"v"`
	NodeID     string    `json:"node_id"`
	EvictedBy  string    `json:"evicted_by,omitempty"`
	EvictedAt  time.Time `json:"evicted_at"`
	Readmitted bool      `json:"readmitted,omitempty"`
}

// Generation is a reanchor's record.
type Generation struct {
	V           int       `json:"v"`
	Generation  uint32    `json:"generation"`
	By          string    `json:"by,omitempty"`
	PrevHighest uint64    `json:"prev_highest,omitempty"`
	StreamAt    time.Time `json:"stream_created_at"`
}

// DecodeMutation reads the typed payload for one record. THE DISPATCH IS ON
// (kind, op) AND NOTHING ELSE.
func DecodeMutation(rec MutationRecord) (any, error) {
	k, op := rec.Subject.Kind, rec.Op
	switch {
	case k == KindStore && op == OpWrite:
		return decodePayload[WritePayload](rec)
	case k == KindStore && op == OpUndo:
		return decodePayload[UndoPayload](rec)
	case k == KindStore && op == OpConfigure:
		return decodePayload[ConfigurePayload](rec)
	case k == KindStore && op == OpPurge:
		return decodePayload[PurgePayload](rec)
	case k == KindEviction && op == OpEviction:
		return decodePayload[Eviction](rec)
	case k == KindGeneration && op == OpGeneration:
		return decodePayload[Generation](rec)
	case k == KindBarrier && op == OpBarrier, k == KindTrimmed && op == OpTrimmed:
		// THE TWO PAYLOAD-FREE RECORDS: nil is their value, and the
		// applier has a case for each that writes nothing of this
		// domain's.
		return nil, nil
	}
	return nil, fmt.Errorf("artifactdata: no payload shape for (%s, %s): a record's kind "+
		"and op together name its payload", k, op)
}

// decodePayload reads one typed payload.
func decodePayload[T any](rec MutationRecord) (T, error) {
	var out T
	if len(rec.Mutation) == 0 {
		return out, fmt.Errorf("artifactdata: the record on %s carries no payload, and "+
			"(%s, %s) needs one", rec.Subject, rec.Subject.Kind, rec.Op)
	}
	if err := json.Unmarshal(rec.Mutation, &out); err != nil {
		return out, fmt.Errorf("artifactdata: decode the %s payload on %s: %w",
			rec.Op, rec.Subject, err)
	}
	return out, nil
}

// EncodeBarrier renders the read index's payload-free append. AN OP ID IS
// REFUSED: a repeat would be answered from the broker's dedupe cache with no
// quorum round trip, the one claim a barrier must never fake.
func EncodeBarrier(env statelog.Envelope) ([]byte, error) {
	if env.Kind != statelog.BarrierKind {
		return nil, fmt.Errorf("artifactdata: %q is not a barrier envelope", env.Kind)
	}
	if env.OpID != "" {
		return nil, fmt.Errorf("artifactdata: a barrier carries no op id and this one has one")
	}
	return Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			Subject: BarrierSubject(), Op: OpBarrier,
			Scope: ScopeSet{Subject: true}, Gen: env.Gen,
		},
	})
}

// EncodeTrimmed renders the trim's trimmed record ([statelog.TrimmedEnvelope],
// ADR-0073) as a record this domain's applier decodes: the framework owns when
// one is appended, what it removes and the floor its subject names, and the
// domain owns what a record on its log looks like.
//
// THE WRITER TRAVELS, unlike a barrier's: a trimmed record removes rows, so the
// eviction gate must be able to drop an evicted node's. NO OP ID, asserted: one
// becomes a message id and a ledger row nothing resolves. And the floor is
// CHECKED here, at the write, rather than in [Subject.Validate], which the read
// path runs too: a refusal there would turn one malformed record into an
// applier that stops on every node, where the framework's apply faults instead.
func EncodeTrimmed(env statelog.Envelope) ([]byte, error) {
	if env.Kind != statelog.TrimmedKind || env.Subject.Kind != statelog.TrimmedKind {
		return nil, fmt.Errorf("artifactdata: a %q envelope on %q is not a trimmed record",
			env.Kind, env.Subject)
	}
	if env.OpID != "" {
		return nil, fmt.Errorf("artifactdata: a trimmed record carries no op id and this " +
			"one has one: an op id becomes a message id and a ledger row nothing resolves")
	}
	floor, err := statelog.TrimmedFloor(Domain{}.Stream().Name, env.Subject)
	if err != nil {
		return nil, fmt.Errorf("artifactdata: %w", err)
	}
	return Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			// PINNED at the base version for ever: an older node
			// retains a record it cannot read, and with it every anchor
			// the record names.
			V:       baseRecordVersion,
			Subject: TrimmedSubject(floor), Op: OpTrimmed,
			Scope: ScopeSet{Subject: true}, Gen: env.Gen, Writer: env.Writer,
		},
	})
}
