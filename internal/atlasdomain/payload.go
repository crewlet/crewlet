package atlasdomain

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE TYPED PAYLOADS, one per (kind, op). A record carries its complete values,
// decided and judged by the writer, so a replay from zero writes the rows the
// first apply did. Vocabulary words travel as text: the applier stores them and
// judges none, so two builds that know different words still write one row.

// Ref is an entry on the wire: its kind and its canonical key.
type Ref struct {
	Kind atlas.Kind `json:"kind"`
	Key  string     `json:"key"`
}

// refOf is a vocabulary ref on the wire.
func refOf(r atlas.Ref) Ref { return Ref{Kind: r.Kind, Key: r.Key} }

// Target is the other entry a claim names: its id, as the writer derived it
// ([atlas.Ref.ID]), and its kind and key. The id travels so the applier keys
// what it writes on it without deriving anything.
type Target struct {
	ID   string     `json:"id"`
	Kind atlas.Kind `json:"kind"`
	Key  string     `json:"key"`
}

// Value is a fact's value on the wire: its type, its canonical text, and the
// writer's reading of whether it names a private network.
type Value struct {
	Type    atlas.ValueType `json:"type"`
	Text    string          `json:"text"`
	Private bool            `json:"private,omitempty"`
}

// Source is who made a claim. The base format records a person, by the name
// their record is kept under; another source joins at its own version: an
// integration's read at 2, and at 3 an agent, by its seat's handle, with the
// turn it recorded in and the evidence the engine checked behind it.
type Source struct {
	Kind atlas.SourceKind `json:"kind"`
	Name string           `json:"name"`

	// Turn and Evidence are an agent's alone (ADR-0132): the run that made
	// the claim, and the class of the call it cited, as the node that ran the
	// turn derived it. No caller passes a confidence; it is read off these.
	Turn     string         `json:"turn,omitempty"`
	Evidence atlas.Evidence `json:"evidence,omitempty"`
}

// sourceOf is a vocabulary source on the wire.
func sourceOf(s atlas.Source) Source {
	return Source{Kind: s.Kind, Name: s.Name, Turn: s.Turn, Evidence: s.Evidence}
}

// vocabulary is the source in the vocabulary's own type.
func (s Source) vocabulary() atlas.Source {
	return atlas.Source{Kind: s.Kind, Name: s.Name, Turn: s.Turn, Evidence: s.Evidence}
}

// Claim is one claim on one entry: the entry itself, one fact, one relation,
// or two entries a person joined or kept apart, a person's denial of any of
// them, or an agent's dispute of one. It is what [atlas.Claim] says, in the
// log's own stable shape.
type Claim struct {
	V int `json:"v"`

	// ID is the claim's own id, derived from the operation that recorded it
	// ([ClaimID]): its row's key.
	ID string `json:"id"`

	// Entry is the entry the claim is about. The record's subject is its id.
	Entry Ref `json:"entry"`

	// Slot is the one thing the claim fills on its entry ([atlas.Claim.Slot]),
	// named by the writer, so a voice's newer claim replaces its older one
	// where the fold reads it to and the applier needs no vocabulary.
	Slot string `json:"slot"`

	Facet    atlas.FacetKind `json:"facet"`
	Fact     atlas.Fact      `json:"fact,omitempty"`
	Relation atlas.Relation  `json:"relation,omitempty"`

	// Target is the other entry of a relation, a "the same" or a "not the
	// same"; External and Name an identity outside the atlas a relation names
	// instead (a seat's handle, a unit's key, a page's id).
	Target   *Target        `json:"target,omitempty"`
	External atlas.External `json:"external,omitempty"`
	Name     string         `json:"name,omitempty"`

	Value  *Value `json:"value,omitempty"`
	Denied bool   `json:"denied,omitempty"`
	Source Source `json:"source"`

	// ObservedAt is when the source saw what it claims, never later than the
	// log took it. Zero for a person, whose claim is seen when the log takes
	// it, so the broker's own instant orders people's words.
	ObservedAt time.Time `json:"observed_at,omitzero"`
}

// claimOf is the wire shape of a claim the writer judged.
func claimOf(id string, c atlas.Claim) Claim {
	out := Claim{
		V: baseRecordVersion, ID: id, Entry: refOf(c.Subject), Slot: c.Slot(),
		Facet: c.Facet.Kind, Fact: c.Facet.Fact, Relation: c.Facet.Relation,
		External: c.Facet.Target.External, Name: c.Facet.Target.Name,
		Denied: c.Denied, Source: sourceOf(c.Source),
	}
	if t := c.Facet.Target.Entry; t != (atlas.Ref{}) {
		out.Target = &Target{ID: t.ID().String(), Kind: t.Kind, Key: t.Key}
	}
	if c.Value != (atlas.Value{}) {
		out.Value = &Value{Type: c.Value.Type, Text: c.Value.Text, Private: c.Value.Private}
	}
	return out
}

// vocabulary is the claim in the vocabulary's own type, seen at observed and
// recorded at position.
func (c Claim) vocabulary(observed time.Time, position uint64) atlas.Claim {
	out := atlas.Claim{
		Subject: atlas.Ref{Kind: c.Entry.Kind, Key: c.Entry.Key},
		Facet: atlas.Facet{Kind: c.Facet, Fact: c.Fact, Relation: c.Relation,
			Target: atlas.Target{External: c.External, Name: c.Name}},
		Denied:     c.Denied,
		Source:     c.Source.vocabulary(),
		ObservedAt: observed,
		Position:   position,
	}
	if c.Target != nil {
		out.Facet.Target.Entry = atlas.Ref{Kind: c.Target.Kind, Key: c.Target.Key}
	}
	if c.Value != nil {
		out.Value = atlas.Value{Type: c.Value.Type, Text: c.Value.Text, Private: c.Value.Private}
	}
	return out
}

// Read is one integration's read of one entry, as the collect duty records it
// (ADR-0129): every claim the read holds on the entry that is new or changed,
// each the voice's newest on its slot, and the slots of the voice's earlier
// claims the read no longer holds, which the applier withdraws. An entry the
// read stopped seeing is retired by a read carrying one denied exists claim
// and withdrawing every other slot. A read that holds exactly what the log
// already holds is never recorded.
type Read struct {
	V int `json:"v"`

	// Source is the integration every claim is from and every withdrawal is
	// of: the one voice a read speaks in.
	Source Source `json:"source"`

	// ObservedAt is when the read saw what it records.
	ObservedAt time.Time `json:"observed_at"`

	Claims    []Claim  `json:"claims,omitempty"`
	Withdrawn []string `json:"withdrawn,omitempty"`
}

// readClaimNamespace is the name space a read's claim ids are derived in,
// fixed for good as [claimNamespace] is.
var readClaimNamespace = uuid.MustParse("c2a7e1f4-3d58-4b06-9e2f-7a41c0d8e5b3")

// ReadClaimID is the id of the claim a read under opID records on slot: one
// claim a slot an operation, so a retry names the same claims.
func ReadClaimID(opID, slot string) string {
	return uuid.NewSHA1(readClaimNamespace, []byte(opID+"\x00"+slot)).String()
}

// Purge erases its subject everywhere: an entry with every claim on it and
// every claim naming it, or a value with every claim carrying it and every
// entry it is the key of. It carries nothing else: the subject names what is
// erased, a value by its digest, so the record never writes the value down.
type Purge struct {
	V int `json:"v"`
}

// Expire removes the history past the retention through an instant: every
// claim a newer claim of its voice replaced before Through, and every entry a
// person retired that nothing was recorded about since before Through. Through
// is never later than the retention's cutoff.
type Expire struct {
	V       int       `json:"v"`
	Through time.Time `json:"through"`
}

// Eviction is a node's eviction from this log, or its readmission: pinned at
// [GateRecordVersion] for ever.
type Eviction struct {
	V      int    `json:"v"`
	NodeID string `json:"node_id"`

	EvictedBy string    `json:"evicted_by,omitempty"`
	EvictedAt time.Time `json:"evicted_at"`

	// Readmitted makes this the inverse commit rather than a delete.
	Readmitted bool `json:"readmitted,omitempty"`
}

// Generation is a reanchor's record.
type Generation struct {
	V           int       `json:"v"`
	Generation  uint32    `json:"generation"`
	By          string    `json:"by,omitempty"`
	PrevHighest uint64    `json:"prev_highest,omitempty"`
	StreamAt    time.Time `json:"stream_created_at"`
}

// DecodeMutation reads the typed payload for one record. The dispatch is on
// (kind, op) alone, so an unknown pair is an error naming both.
func DecodeMutation(rec MutationRecord) (any, error) {
	k, op := rec.Subject.Kind, rec.Op
	switch {
	case k == KindEntry && op == OpClaim:
		return decodePayload[Claim](rec)
	case k == KindEntry && op == OpRead:
		return decodePayload[Read](rec)
	case (k == KindEntry || k == KindValue) && op == OpPurge:
		return decodePayload[Purge](rec)
	case k == KindExpiry && op == OpExpire:
		return decodePayload[Expire](rec)
	case k == KindEviction && op == OpEviction:
		return decodePayload[Eviction](rec)
	case k == KindGeneration && op == OpGeneration:
		return decodePayload[Generation](rec)
	case k == KindBarrier && op == OpBarrier, k == KindTrimmed && op == OpTrimmed:
		// The two payload-free records: nil is their value.
		return nil, nil
	}
	return nil, fmt.Errorf("atlasdomain: no payload shape for (%s, %s): a pair this build "+
		"does not know is a newer peer's record", k, op)
}

// decodePayload reads one typed payload.
func decodePayload[T any](rec MutationRecord) (T, error) {
	var out T
	if len(rec.Mutation) == 0 {
		return out, fmt.Errorf("atlasdomain: the record on %s carries no payload, and "+
			"(%s, %s) needs one", rec.Subject, rec.Subject.Kind, rec.Op)
	}
	if err := json.Unmarshal(rec.Mutation, &out); err != nil {
		return out, fmt.Errorf("atlasdomain: decode the %s payload on %s: %w", rec.Op, rec.Subject, err)
	}
	return out, nil
}

// EncodeBarrier renders the read index's payload-free append. An op id is
// refused: a repeat would be answered from the dedupe cache with no quorum.
func EncodeBarrier(env statelog.Envelope) ([]byte, error) {
	if env.Kind != statelog.BarrierKind {
		return nil, fmt.Errorf("atlasdomain: %q is not a barrier envelope", env.Kind)
	}
	if env.OpID != "" {
		return nil, fmt.Errorf("atlasdomain: a barrier carries no op id and this one has one")
	}
	return Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			Subject: BarrierSubject(), Op: OpBarrier, Scope: ScopeSet{Subject: true}, Gen: env.Gen,
		},
	})
}

// EncodeTrimmed renders the trim's trimmed record (ADR-0080). The writer
// travels, so the eviction gate can drop an evicted node's; no op id.
func EncodeTrimmed(env statelog.Envelope) ([]byte, error) {
	if env.Kind != statelog.TrimmedKind || env.Subject.Kind != statelog.TrimmedKind {
		return nil, fmt.Errorf("atlasdomain: a %q envelope on %q is not a trimmed record",
			env.Kind, env.Subject)
	}
	if env.OpID != "" {
		return nil, fmt.Errorf("atlasdomain: a trimmed record carries no op id and this one has one")
	}
	floor, err := statelog.TrimmedFloor(Domain{}.Stream().Name, env.Subject)
	if err != nil {
		return nil, fmt.Errorf("atlasdomain: %w", err)
	}
	return Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			// Pinned at the base version for ever: every build must read it.
			V: baseRecordVersion, Subject: TrimmedSubject(floor), Op: OpTrimmed,
			Scope: ScopeSet{Subject: true}, Gen: env.Gen, Writer: env.Writer,
		},
	})
}
