package chart

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE TYPED PAYLOADS, one per (kind, op).
//
// EVERY ONE OF THEM IS FULL POST-STATE, which is the opposite of the tracker's
// and the knowledge base's choice and is the right one here. Both of those
// carry a typed PATCH for their largest object, because a page's body is half a
// mebibyte and putting it on the wire for a label change is absurd. A chart is
// not like that in either direction:
//
//   - THE WRITE ALREADY HOLDS THE WHOLE VALUE. A chart is authored as a
//     document and reconciled whole, so a patch would be a diff the writer
//     computed only so that every node could reassemble it — three chances to
//     get "unchanged" and "set to empty" the wrong way round, bought for
//     nothing.
//   - THE OBJECTS ARE SMALL. A seat is prose bounded at [MaxProse] per field
//     and a unit is smaller; the widest record here is a structural one, and
//     that carries keys rather than content.
//
// The one thing full post-state costs is stated rather than glossed: two
// writers editing two different fields of one seat do not merge. They contend
// at the broker on that seat's subject and the loser re-reads and re-writes,
// which is the ordinary arbitration and not a special case.

// Edge is one object's PLACEMENT: where it sits, and — for a unit — who leads
// it.
//
// FULL POST-STATE, like everything else here, and both of its zero values are
// meaningful rather than absent. An empty Parent is the ORG ROOT, which is
// where a top-level unit and a root-level seat both sit; an empty Lead is a
// unit with no lead of its own, which is a real and common state because lead
// INHERITANCE is what fills it in — see the organisation model. Neither is a
// pointer, because neither has a second meaning to tell apart from.
type Edge struct {
	// Object is the unit or the seat being placed.
	Object ObjectRef `json:"o"`

	// Parent is the unit key this object sits under. Empty is the org
	// root: for a unit, a top-level department; for a seat, an org-wide
	// role above every team.
	Parent string `json:"p,omitempty"`

	// Lead is the handle of the seat leading this unit, for a unit. Empty
	// for a seat, and empty for a unit whose lead comes from an ancestor.
	//
	// IT IS AUTHORED AND NOT EFFECTIVE. Lead inheritance is a derivation
	// over the tree, so writing the inherited answer here would be a second
	// copy of a value the org model already computes — one that goes stale
	// the moment an ancestor's lead changes and that nothing would
	// recompute, because the record that changed the ancestor never named
	// this unit.
	Lead string `json:"l,omitempty"`
}

// PlacementPayload states edges. Its subject is [KindTree].
//
// A SET RATHER THAN ONE EDGE, because the smallest honest structural change is
// already more than one: moving a seat between teams is one edge, and
// dissolving a unit is every child it had. One record per edge would let half a
// reparent commit and the other half be refused, which is the state this
// domain's single structure subject exists to make unreachable.
type PlacementPayload struct {
	V int `json:"v"`

	// Edges is the complete post-state of every placement this record
	// changes. An object absent from it is untouched.
	Edges []Edge `json:"edges"`
}

// ImportPayload is the complete authored structure of one company revision.
// Its subject is [KindTree].
//
// # Why it is a second op rather than a large PlacementPayload
//
// What an operator reads in the log is the difference between "somebody moved a
// seat" and "a config revision rewrote the chart", and reconstructing that from
// the SIZE of a payload is not reading, it is guessing. It is also what
// `chart_import_ledger` is keyed on: a re-activation of a revision the chart
// already holds — which is the credential-rotation gesture, and therefore
// routine — is a no-op every node reaches the same way, rather than a second
// pass that rewrites every row and wakes everybody a second time.
//
// # Why it carries no CONTENT and no REMOVALS
//
// Neither would fit, and neither should. A chart of five hundred seats at
// [MaxProse] of backstory each is megabytes, and an external NATS cluster's
// default max_payload is one mebibyte — so an import that carried content would
// be refused by the broker on exactly the companies large enough to need it.
// The content travels as one [OpUpsert] per object, on that object's own
// subject, which is also what lets two of them be written concurrently.
//
// Removals are absent for a different reason: [OpRemove] installs a gate, and a
// record that installed a gate for some of its objects and not others would
// make "does this record install a gate" a question about its payload — which
// is the one question that has to be answerable from the envelope alone.
type ImportPayload struct {
	V int `json:"v"`

	// Revision is the company configuration revision this structure was
	// read from. It is the ledger's key.
	Revision string `json:"revision"`

	// Edges is the COMPLETE authored placement of every object the revision
	// names.
	Edges []Edge `json:"edges"`
}

// RemovePayload is what the chart no longer names. Its subject is [KindTree].
//
// PINNED AT [GateRecordVersion] FOR EVER. See the constant: this is the one
// record here a node must not be able to defer, so its shape can never be one a
// node might not know.
//
// A REMOVAL CARRIES ITS REASON, because it is the one apply whose consequences
// outlive its object: a seat leaving the chart releases a mailbox, retires a
// coding run and orphans nothing else, and the person asking why next month has
// only this row to read.
type RemovePayload struct {
	V int `json:"v"`

	// Objects is what goes. Bounded by [MaxScopeTerms] the same way every
	// enumeration here is — a removal past that is a company being
	// dissolved, and it states the root term.
	Objects []ObjectRef `json:"objects"`

	Reason string `json:"reason,omitempty"`
}

// UnitPayload is a unit's own content, as FULL POST-STATE. Its subject is
// [KindUnit].
//
// IT CARRIES NO PARENT AND NO LEAD. Both are structure, and structure
// arbitrates on [KindTree]: a unit's content record that could move it would be
// a second writer of the tree, contending with nobody.
type UnitPayload struct {
	V int `json:"v"`

	// Key is the unit's own key — the address, and what the subject was
	// arbitrated on. The applier recomputes the subject from it and REFUSES
	// a record that took one address and claimed another.
	Key string `json:"key"`

	// Name is display only. Nothing references a unit by it, which is
	// exactly why it is safe for a founder to edit at will.
	Name string `json:"name,omitempty"`

	Type    string   `json:"type,omitempty"`
	Purpose string   `json:"purpose,omitempty"`
	Goals   []string `json:"goals,omitempty"`

	// Channel is the team's channel on the company's chat surface,
	// inherited by children where the org model reads it.
	Channel string `json:"channel,omitempty"`

	// Project and Space are the unit's tracker and knowledge identities,
	// VENDOR-NEUTRAL: each names a native container or a vendor's,
	// whichever backend the company runs.
	Project string `json:"project,omitempty"`
	Space   string `json:"space,omitempty"`

	// KnowledgeRefs are the pages this unit points its seats at. They do
	// NOT scope what a seat may read — that is the org-wide knowledge
	// scope, for the reason the organisation model gives.
	KnowledgeRefs []string `json:"knowledge_refs,omitempty"`
}

// SeatPayload is a seat's own content, as FULL POST-STATE. Its subject is
// [KindSeat].
//
// IT CARRIES NO UNIT, for [UnitPayload]'s reason: which unit a seat sits in is
// structure.
type SeatPayload struct {
	V int `json:"v"`

	// Handle is the seat's own handle — the address, and what the subject
	// was arbitrated on.
	Handle string `json:"handle"`

	// Kind is what holds the seat. An agent seat has an inbox, a turn loop
	// and a model chain; a human seat has none of them and is addressable
	// only.
	Kind SeatKind `json:"kind"`

	Name string `json:"name,omitempty"`

	// Email is the address a vendor payload identifies this person by. The
	// applier derives `email_index` from it with [NormalizeEmail]; the
	// record carries what was AUTHORED, because that is what a person reads
	// back out of the config.
	Email string `json:"email,omitempty"`

	Backstory string `json:"backstory,omitempty"`
	Goal      string `json:"goal,omitempty"`

	Responsibilities     []string `json:"responsibilities,omitempty"`
	BehavioralGuidelines []string `json:"behavioral_guidelines,omitempty"`

	// Manages is the AUTHORED list: seat handles and unit keys exactly as
	// they were written, including entries that resolve to nothing.
	//
	// KEPT AS WRITTEN AND NEVER EXPANDED HERE. A `manages:` entry naming a
	// unit reaches every seat in its subtree, and that expansion is a
	// function of the tree at the moment it is read — so storing the
	// expansion would be a derived value written down, stale from the next
	// structural record onwards and recomputed by nothing.
	Manages []string `json:"manages,omitempty"`

	// Project and Space are a root-level seat's own tracker and knowledge
	// identities. A seat inside a unit takes the unit's.
	Project string `json:"project,omitempty"`
	Space   string `json:"space,omitempty"`
}

// RekeyPayload moves one key onto one object. Its subject is the NEW key.
//
// It carries the FORMER key because the apply retires that claim, and a
// retirement the record did not state would be a row rewritten on one node's
// authority rather than the log's.
type RekeyPayload struct {
	V int `json:"v"`

	// Object is what takes the key.
	Object ObjectRef `json:"object"`

	// Key is the NEW key, and the one the subject was arbitrated on — the
	// applier recomputes the subject from it and refuses a record that took
	// one address and claims another.
	Key string `json:"key"`

	// FormerKey is the address this record RETIRES. It goes on resolving to
	// this object, in `former_keys_json`, until something else claims it:
	// a key is pasted into chat and typed into `manages:` entries, so a
	// rename that stopped the old one resolving would break every reference
	// anybody had already written.
	FormerKey string `json:"former_key"`
}

// Eviction gates one node's records on this log, or readmits it. Its subject is
// [KindEviction].
//
// PINNED AT [GateRecordVersion], like [RemovePayload]: see [OpEviction].
//
// THE POSITION IS NOT ON THE PAYLOAD, and that is the whole design of the gate.
// The window is the EVICTION RECORD'S OWN position on this log, which every
// node already has and none of them has to agree about — so there is no clock,
// no coordination read, and no field a writer could set wrong. A payload
// carrying its own position would be a second answer to a question the log
// already answers, and the two would differ exactly when the record was
// republished by a reanchor.
type Eviction struct {
	V int `json:"v"`

	// NodeID is the node whose records are dropped. NOT folded: a node id
	// is what that node published as its own writer rather than an address
	// a person types, and normalising it here would make the gate miss
	// every record it was installed for.
	NodeID string `json:"node_id"`

	// Readmit turns the record into the INVERSE COMMIT rather than a
	// delete, so an eviction's whole history survives a replay and a node
	// that was evicted, readmitted and evicted again reads correctly rather
	// than as one long absence.
	Readmit bool `json:"readmit,omitempty"`

	// By is the operator who asked, for the row an operator reads later.
	By string `json:"by,omitempty"`
}

// Generation is a reanchor's own record, on [KindGeneration]: create-only at an
// expectation of zero on the new stream.
//
// IT IS WHAT MAKES A REANCHOR REPRODUCIBLE rather than an event that happened
// to a fleet. A replay of the new stream writes this audit row, so a node that
// joined afterwards knows the recovery took place and what it covered, without
// anybody having to have remembered it.
type Generation struct {
	V int `json:"v"`

	// Generation is the number the new stream opens at.
	Generation uint32 `json:"generation"`

	// PrevLastSeqSeen is the last sequence any node reported having applied
	// on the PREVIOUS stream — the high-water mark the reanchor preserved.
	PrevLastSeqSeen uint64 `json:"prev_last_seq_seen,omitempty"`

	// NewStreamCreatedAt is the broker's creation instant for the new
	// stream, which is what a node compares against to tell a stream that
	// was recreated from one it merely lost its place on.
	NewStreamCreatedAt time.Time `json:"new_stream_created_at,omitzero"`

	By string `json:"by,omitempty"`
}

// DecodeMutation reads the typed payload for one record.
//
// THE DISPATCH IS ON (kind, op) AND NOTHING ELSE, so a record whose pair this
// build does not know is an error naming both rather than a nil payload the
// applier would treat as an empty write.
func DecodeMutation(rec MutationRecord) (any, error) {
	switch {
	case rec.Subject.Kind == KindTree && rec.Op == OpPlace:
		return decodePayload[PlacementPayload](rec)
	case rec.Subject.Kind == KindTree && rec.Op == OpImport:
		return decodePayload[ImportPayload](rec)
	case rec.Subject.Kind == KindTree && rec.Op == OpRemove:
		return decodePayload[RemovePayload](rec)
	case rec.Subject.Kind == KindUnit && rec.Op == OpUpsert:
		return decodePayload[UnitPayload](rec)
	case rec.Subject.Kind == KindSeat && rec.Op == OpUpsert:
		return decodePayload[SeatPayload](rec)
	case rec.Subject.Kind == KindRekey && rec.Op == OpRekey:
		return decodePayload[RekeyPayload](rec)
	case rec.Subject.Kind == KindEviction && rec.Op == OpEviction:
		return decodePayload[Eviction](rec)
	case rec.Subject.Kind == KindGeneration && rec.Op == OpGeneration:
		return decodePayload[Generation](rec)
	case rec.Subject.Kind == KindBarrier && rec.Op == OpBarrier:
		// THE ONE PAYLOAD-FREE RECORD, and nil is its value rather than
		// an absence: the applier's switch has a case for it that writes
		// nothing, which is what keeps "this kind wrote nothing"
		// distinguishable from "nobody classified this kind".
		return nil, nil
	}
	return nil, fmt.Errorf("chart: no payload shape for (%s, %s) — a record's "+
		"kind and op together name its payload, and a pair this build does not "+
		"know is a newer peer's record rather than an empty one",
		rec.Subject.Kind, rec.Op)
}

// decodePayload reads one typed payload, refusing a shape from a newer build.
func decodePayload[T any](rec MutationRecord) (T, error) {
	var out T
	if len(rec.Mutation) == 0 {
		return out, fmt.Errorf("chart: the record on %s carries no payload, and "+
			"(%s, %s) needs one", rec.Subject, rec.Subject.Kind, rec.Op)
	}
	if err := json.Unmarshal(rec.Mutation, &out); err != nil {
		return out, fmt.Errorf("chart: decode the %s payload on %s: %w",
			rec.Op, rec.Subject, err)
	}
	return out, nil
}

// EncodeBarrier renders the read index's payload-free append.
//
// # Why the framework cannot write this itself
//
// The read index owns WHEN a barrier goes out and what its acknowledgement
// proves — a quorum-committed position, which is the only client-visible fact
// this broker offers that the responder confirmed its own authority for. The
// DOMAIN owns what a record on its log looks like: this one's envelope, its
// version gate, its subject grammar and the scope alphabet its applier files
// deferrals under. Neither can write the other's half, and this function is
// where they meet.
//
// AN OP ID IS REFUSED, and that is a correctness rule rather than tidiness. An
// op id becomes the Nats-Msg-Id; a repeat inside the duplicate window is
// answered from the dedupe cache with no quorum round trip at all, and the
// sequence it returns is then a position nothing confirmed — which is exactly
// the claim a barrier exists to make and the one it must never fake.
func EncodeBarrier(env statelog.Envelope) ([]byte, error) {
	if env.Kind != statelog.BarrierKind {
		return nil, fmt.Errorf("chart: %q is not a barrier envelope", env.Kind)
	}
	if env.OpID != "" {
		return nil, fmt.Errorf("chart: a barrier carries no op id and this one " +
			"has one — an op id becomes a message id, and a duplicate ack is " +
			"served with no quorum round trip at all")
	}
	return Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			V:       RecordVersion,
			Subject: BarrierSubject(),
			Op:      OpBarrier,
			// THE BARRIER'S OWN SCOPE IS THE SENTINEL, which resolves
			// through [subjectPath] to the framework's [statelog.BarrierScope]
			// rather than to anything in this alphabet — so a barrier
			// never intersects a read's closure and no linearizable read
			// ever waits behind another.
			Scope: ScopeSet{Subject: true},
			Gen:   env.Gen,
		},
	})
}
