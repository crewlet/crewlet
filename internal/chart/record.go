package chart

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/jsoncarry"
	"github.com/crewlet/crewlet/internal/statelog"
)

// RecordVersion is the record shape THIS BUILD writes and reads.
//
// A record above it is RETAINED rather than skipped — see the deferral contract
// in [statelog] — which is what makes a rolling upgrade a period of reduced
// coverage rather than an outage.
//
// # What each version means
//
//   - 1: the first shape.
//   - 2: a structural edge says what the batch did to its object
//     ([Edge.Op]), so a create whose address is held is DECLINED at the apply
//     rather than applied as a move of whatever holds it; a content record
//     never creates its object at the apply either; and a rename is a
//     structural edge on the tree's subject ([OpRename], [Edge.From]) rather
//     than a claim on the address's own ([OpRekey], which only version 1
//     carries); and a seat's kind is structure ([Edge.Kind], [OpSetKind]),
//     which a content record neither carries nor changes. Every address it
//     gives an object is held to the address's SHAPE as well as to who holds
//     it ([addressShape]), an edge under a unit the same record failed to
//     make or keep is declined rather than filed under an address nobody
//     meant, and its history names the first edge that landed.
//   - 3: a seat's `manages:` list is structure — every seat edge states the
//     whole list ([Edge.Manages]) and a `set_manages` operation changes it
//     ([OpSetManages]), on the tree's subject beside the renames whose
//     cascade moves its entries — and a seat's content record neither
//     carries nor changes it. A content record that restated the list
//     arbitrated on the seat's own subject, which a rename's cascade never
//     moves, so a list decided on a node that had not applied a rename yet
//     was accepted after it and wrote the renamed entry back — and a
//     creation that later took the retired address made the manager manage
//     whoever took it.
//
// A LOWER VERSION IS READ FOR EVER, as what it meant when it was written: a
// version-1 edge carries no verb and is a placement that creates what is
// absent, declined only for an address a removal took or another object's
// identity; a version-1 content record may create its row, on the same terms,
// and sets the seat's kind; a version-1 rekey is declined only for an address
// somebody else answers to or a removal took; and a version-1 placement's
// history names its first edge. A version-1 or version-2 content record
// replaces the seat's `manages:` list with the one it carries, and an edge
// below version 3 leaves the list as it is. Nothing rewrites a record on the
// log, and the fleet mid-upgrade still has older writers, so every change of
// meaning is read off the record itself at the apply ([exactVersion],
// [managesVersion]) — never off this constant, which the next reshape moves. A
// rule added at one version and asked of a record written under an earlier
// one would have this build derive different rows from the log than the build
// that applied it first: one replicated estate, two rosters.
//
// A HIGHER ONE IS WHAT KEEPS AN OLDER PEER HONEST. A version-1 build reading a
// version-2 edge would apply a create as a move — the very thing version 2
// exists to stop — and a version-2 build reading a version-3 content record
// would read the list it no longer carries as one somebody cleared, so each
// retains the record instead, and applies it once upgraded.
//
// # What a record is STAMPED with is the table's, and it is the ceiling here
//
// A record is stamped by [Encode] with the lowest version that reads what it
// carries, over [versionedFields], and this constant moves only with that
// table, exactly to its highest version. On this domain the lowest honest
// version of every record but a gate, a barrier and a generation IS this one:
// the applier reads its RULES off the record's version, so a record decided
// under version 3's rules says so whatever fields it happens to carry —
// which is what [MutationRecord.ManagesStructural] is for.
const RecordVersion = 3

// versionedFields is every field a chart record has gained since the base
// format, and the version a reader must be at to apply a record carrying it.
//
// # Adding a field to any record or payload is adding a row here
//
// A field an older build has no home for is decoded AROUND: the build reads the
// version, finds it readable, applies what it understands and drops the rest,
// so its rows for that object differ from every upgraded node's for good, on
// tables the identity claim compares byte for byte. The row is what makes the
// encoder stamp a version that build retains instead. A new field takes the
// next version above [RecordVersion], which moves with it, and the domain's
// statelogtest candidate gains a record carrying it — which is what certifies
// the path is the one the encoder actually writes.
//
// # And a new RULE is a row too
//
// This domain's applier reads what a record MEANS off its version
// ([exactVersion], [managesVersion]), so a record whose fields are all old can
// still need a new reader: a seat's content record stating no `manages:` list
// meant "manages nobody" below version 3 and "leave the list alone" from it. A
// rule like that is stamped by a marker on the record's root
// ([MutationRecord.ManagesStructural], after the tracker's `keeps_place`), set
// by the writer on every record it decides under the rule.
var versionedFields = statelog.RecordFields{
	// A STRUCTURAL EDGE THAT STATES ITS VERB, a rename's source and a seat's
	// kind, all at version 2 ([exactVersion]). A version-1 build reads an edge
	// as an object, a parent and a lead: it would apply a create that met a
	// held address as a move of whatever held it, a rename as a placement of
	// an object at an address it never held, and leave a seat's kind where
	// the edge changed it. Every op, because an edge rides both a placement
	// and an import and no other payload has `edges`.
	{Name: "Edge.Op", Since: exactVersion,
		Path: []string{"mutation", "edges", "op"}},
	{Name: "Edge.From", Since: exactVersion,
		Path: []string{"mutation", "edges", "from"}},
	{Name: "Edge.Kind", Since: exactVersion,
		Path: []string{"mutation", "edges", "k"}},
	// A SEAT'S `manages:` LIST ON ITS EDGE, at version 3 ([managesVersion]): a
	// version-2 build reads the list as the content record's to state and
	// would leave the stored one in place of the one the edge says.
	{Name: "Edge.Manages", Since: managesVersion,
		Path: []string{"mutation", "edges", "m"}},
	// THE RULE VERSION 3 IS, carried as a marker on the record's root —
	// [MutationRecord.ManagesStructural]. The list is OMITTED from a content
	// record and from a seat edge whose seat manages nobody, so a record
	// stamped by what it carries would be version 1 or 2, and every build
	// below 3 — this one too, since it reads the rules off the version —
	// would read that absence as a list somebody cleared.
	{Name: "MutationRecord.ManagesStructural", Since: managesVersion,
		Path: []string{"manages_structural"}},
}

// VersionedFields is the table, for the conformance suite and for an operator
// surface that names why a record was held back.
func VersionedFields() statelog.RecordFields { return slices.Clone(versionedFields) }

// exactVersion is the first record version whose structure is EXACT: its edges
// state their verb, and every version-2 rule above is asked of it.
//
// PINNED AT 2, and never written as [RecordVersion]: the next reshape moves
// that constant, and every rule version 2 introduced must go on being asked of
// the version-2 records already on the log. See [applyContext.exact].
const exactVersion = 2

// managesVersion is the first record version on which a seat's `manages:` list
// is STRUCTURE: a seat edge states it and a content record does not.
//
// PINNED AT 3 for [exactVersion]'s reason — see [applyContext.managesStructural].
const managesVersion = 3

// BaseRecordVersion is the version a record that has never changed shape is
// written at, so every build still on the stream reads it: the read index's
// payload-free barrier, which an older node would otherwise retain once per
// linearizable read anywhere in the fleet.
const BaseRecordVersion = 1

// GateRecordVersion is the version every gate-installing record carries, FOR
// EVER.
//
// A removal whose version this build could not read would be deferred, and a
// deferred gate does not postpone one record's effect on one node: it licenses
// every later record on the objects the removal destroyed, with no inverse that
// repairs it. So [OpRemove]'s payload is pinned at 1 and never evolves — a
// field it needs that it cannot have is a field that belongs on the structural
// record that accompanies it.
//
// [OpEviction] is pinned at the same version for the same shape of reason one
// layer up: a node that deferred an eviction goes on applying records every
// peer is dropping, and the rows it writes from them have no later record that
// corrects them.
const GateRecordVersion = 1

// OpKind is what a record does.
type OpKind string

const (
	// OpPlace is the structural record: where a unit sits, which unit a
	// seat sits in, and which seat leads a unit. Its subject is
	// [KindTree].
	//
	// ONE OP FOR ALL THREE, because all three are the same fact — an edge
	// in the tree — and a chart is only ever wrong as a whole. Splitting
	// them would let a reparent and a lead change on one unit commit
	// independently, which is two writers each holding half of one
	// decision.
	OpPlace OpKind = "place"

	// OpImport is a whole chart applied from one company revision: the
	// batch, and the record whose enumeration can exceed [MaxScopeTerms]
	// and collapse to the root term.
	//
	// ITS OWN OP rather than a large [OpPlace], because what an operator
	// reads in the log is the difference between "somebody moved a seat"
	// and "a config revision rewrote the chart", and reconstructing that
	// from the size of a payload is not reading, it is guessing.
	OpImport OpKind = "import"

	// OpRemove is what the chart no longer names, and it INSTALLS A GATE by
	// its op rather than by its kind: its subject is the ordinary structure
	// subject.
	//
	// The gate exists because a removal is the one operation here with no
	// inverse a later record supplies. Every other record is a full
	// post-state under a monotone guard, so a node that deferred one is
	// repaired by the next record on that object; nothing ever names a
	// removed object again, so a node that deferred a removal keeps the
	// object for ever while every peer has dropped it.
	OpRemove OpKind = "remove"

	// OpUpsert is an object's own content, as FULL POST-STATE. Its subject
	// is [KindUnit] or [KindSeat].
	//
	// FULL POST-STATE AND NOT A PATCH, which is the opposite of the
	// tracker's and the knowledge base's choice for their largest objects,
	// and the reason is where the write comes from: a chart is authored as
	// a DOCUMENT and reconciled whole, so the writer always holds the
	// complete new value and a patch would be a diff it computed in order
	// to be reassembled by every node. The objects are also small — a seat
	// is prose bounded at [MaxProse] per field — where a page is half a
	// mebibyte and a patch genuinely saves the wire.
	OpUpsert OpKind = "upsert"

	// OpRekey moves one KEY onto one object, keeping the old one resolving.
	// Its subject is [KindRekey] — the key being claimed.
	//
	// VERSION 1 ONLY, AND READ FOR EVER. Nothing writes it: a rename is a
	// structural operation on the tree's subject ([OpRename]), for the reason
	// that operation's doc gives. A record already on the log is applied as
	// what it meant ([Applier.applyRekey]).
	OpRekey OpKind = "rekey"

	// OpBarrier is the read index's payload-free append.
	OpBarrier OpKind = "barrier"

	// OpEviction gates a node's records on this log, or readmits it. Its
	// subject is [KindEviction].
	//
	// PINNED AT [GateRecordVersion], like [OpRemove] and for the same
	// reason one layer up: a node that deferred an eviction goes on
	// applying records every peer is dropping, and there is no later
	// record that repairs the rows it wrote from them.
	OpEviction OpKind = "eviction"

	// OpGeneration is a reanchor's own record, on [KindGeneration].
	OpGeneration OpKind = "generation"
)

// OpKinds are the eight, in the order they are documented.
var OpKinds = []OpKind{
	OpPlace, OpImport, OpRemove, OpUpsert, OpRekey, OpBarrier,
	OpEviction, OpGeneration,
}

// Valid reports whether an op off the wire is one this build knows.
func (o OpKind) Valid() bool { return slices.Contains(OpKinds, o) }

// RecordEnvelope decodes at EVERY version, BEFORE V is consulted.
//
// Its EIGHT keys are RESERVED at the top level of the format for its life: a
// later version may add fields beside them and may never repurpose one. It is
// the same split the event envelope makes between a known header and an opaque
// body, for the same reason — a rolling upgrade puts a record this build cannot
// read on the wire, and dropping it would make every upgrade an outage.
type RecordEnvelope struct {
	// V is the record version. Refused for the PAYLOAD when unknown, never
	// for this struct.
	V int `json:"v"`

	// OpID is a uuid7: the idempotency key, the Nats-Msg-Id, and the ops
	// table's key. EMPTY on a barrier, deliberately — an op id is what
	// invites a message id, and a duplicate ack is served out of the
	// dedupe window with no quorum round trip at all, which is the one
	// thing a read barrier must never be.
	OpID string `json:"op_id,omitempty"`

	// Subject is the object this record mutates, and the unit the broker
	// arbitrates it on.
	Subject Subject `json:"subject"`

	// Op is what the record does.
	Op OpKind `json:"op"`

	// CreatedAt is THE AUTHORED INSTANT, the writer's own clock. It rides
	// the record so an operator reading the log can see when a
	// reorganisation was decided, and it is NEVER ORDERED ON and never
	// written to a row: every instant this domain stores is the broker's,
	// which is what makes one node's copy of the chart byte-identical to
	// another's. Empty on a barrier, because nothing renders one.
	CreatedAt time.Time `json:"created_at,omitzero"`

	// Gen is the generation. It is what makes an object's version a pure
	// function of the record — (Gen << 40) | the broker's own sequence — so
	// the applier needs no table lookup, no config read and no clock.
	// Present on a barrier too, because a barrier from a previous
	// generation must be refusable.
	Gen uint32 `json:"gen,omitempty"`

	// Writer is the publishing node's id, read by the eviction gate — which
	// runs before any kind rule, on a node that may not be able to decode
	// the payload at all. Empty on a barrier, which writes no rows for a
	// gate to drop.
	Writer string `json:"writer,omitempty"`

	// Scope is the complete set of objects this record's apply may write.
	Scope ScopeSet `json:"scope"`
}

// InstallsGate reports whether this record installs an apply gate.
//
// ANSWERED FROM THE ENVELOPE ALONE, because it must be answerable by a node
// that cannot decode the payload: it is what turns an unknown version into a
// STOP rather than a deferral.
//
// ON THE OP AND NOT ON A KIND, and a removal is why: it rides the ORDINARY
// STRUCTURE SUBJECT, so a reader that keyed on the kind would defer it, and a
// deferred removal is a node that goes on serving a unit every other node has
// dropped. An eviction has a kind of its own and could have been read either
// way; it is read here so that the question is asked once, in one place, for
// both — a gate rule written twice is a record that stops the applier on the
// write side and is filed for later on the read side.
func (e RecordEnvelope) InstallsGate() bool {
	return e.Op == OpRemove || e.Op == OpEviction
}

// MutationRecord is one committed mutation: the envelope plus everything a
// build at this version may read.
type MutationRecord struct {
	RecordEnvelope

	// Expect is the version this mutation was decided against. PROVENANCE
	// ONLY — the broker is what enforced it, and nothing reads this to
	// decide anything.
	Expect uint64 `json:"expect,omitempty"`

	// Mutation is the typed payload for (Subject.Kind, Op), left as OPAQUE
	// BYTES when the version is above this build's.
	Mutation json.RawMessage `json:"mutation,omitempty"`

	Actor      string     `json:"actor,omitempty"`
	ActorKind  AuthorKind `json:"actor_kind,omitempty"`
	OperatorID string     `json:"operator_id,omitempty"`
	TurnID     string     `json:"turn_id,omitempty"`

	// ManagesStructural says this record was decided under version 3's rules
	// ([managesVersion]): a seat's `manages:` list is STRUCTURE, so a seat
	// edge states it whole and a content record carries none.
	//
	// A MARKER AND NOT A VALUE, read through the record's VERSION rather than
	// by the applier, after the tracker's `keeps_place`: its whole job is the
	// row in [versionedFields] that makes the encoder stamp version 3. The
	// list's ABSENCE is the one thing presence cannot stamp — omitted from a
	// content record, and from a seat edge whose seat manages nobody — and a
	// build below 3 reads that absence as a list somebody cleared. Set by the
	// writer on EVERY record it decides that is not a gate, a barrier or a
	// generation, because the applier reads every rule a record is applied
	// under off its version, and each of those records is decided under
	// version 3's; a unit's content and a placement of units alone need only
	// version 2's, and stamping them 3 holds them back from a version-2 peer
	// one upgrade longer than strictly necessary, which is the price of one
	// marker rather than one per rule.
	ManagesStructural bool `json:"manages_structural,omitempty"`

	// Revision is the company configuration revision this change came
	// from, when it came from one.
	//
	// ON THE RECORD rather than looked up, because it is the answer to the
	// question an operator asks about a chart they did not expect: which
	// edit did this. A change made through a tool carries none, and that
	// absence is itself the answer — somebody did it by hand.
	//
	// It is also what `chart_import_ledger` is keyed on, so a re-import of
	// a revision the chart already holds is a no-op every node reaches the
	// same way rather than a second pass that rewrites every row.
	Revision string `json:"revision,omitempty"`

	// Notify is the routing snapshot, and NIL is what "wakes nobody" means.
	//
	// A NIL POINTER RATHER THAN A quiet FLAG, so the wake filter is "has a
	// Notify" — a question about the record's own shape — instead of a
	// boolean a writer can forget to set. A quiet commit is a full record,
	// arbitrated exactly like a loud one, and writes its history row like
	// every other: quiet means it wakes nobody, and nothing else.
	//
	// AN IMPORT IS ORDINARILY QUIET, which is this domain's own version of
	// the rule a page's comment follows: a config revision that touches
	// forty seats would otherwise wake forty seats to tell each of them
	// their goal was reworded.
	Notify *Notify `json:"notify,omitempty"`

	// Extra carries fields a newer build wrote, so a record round-trips
	// losslessly through a node that cannot interpret them.
	Extra map[string]json.RawMessage `json:"-"`
}

// What is NOT in the envelope, deliberately: the BROKER'S instant.
//
// The effective instant every duration is measured against derives from the
// broker's own store timestamp, which the replication loop stamps onto the
// record before calling the applier. A writer must not be able to author it,
// and a field a writer could set is a field a writer could lie about — so it
// reaches the applier as an argument rather than as a key in the format.
// Stated here so nobody "completes" the envelope by adding it.

// Notify is the routing snapshot a wake is derived from, copied at write time
// so the node that wins a feed message routes without reading anything.
//
// EVERYTHING A NOTIFICATION IS BUILT FROM IS HERE, and that is the contract
// rather than a convenience: the feed relays the RECORD, and the node that wins
// that message may be one whose applier has not reached the change — so a
// parser that read a chart row instead would route from a stale tree, or block
// the feed until it had caught up.
type Notify struct {
	// Kind is what happened, in the vocabulary a card renders.
	Kind ChangeKind `json:"kind"`

	// Object is what the change is about.
	//
	// ON THE NOTIFICATION rather than taken from the subject, because the
	// records that wake somebody most are the ones whose subject is NOT the
	// object: a placement and a removal both arbitrate on the structure,
	// and the seat that moved is inside a payload whose shape a parser
	// would otherwise have to decode per op.
	Object ObjectRef `json:"object"`

	// Recipients are the handles woken, computed once at write time so the
	// feed never has to resolve a tree and can never forget to.
	//
	// WHO THEY ARE IS THE WRITER'S JUDGEMENT AND NOT A RULE HERE, for the
	// reason the tracker's routing precedence is order in one function: a
	// move's recipients are the seat that moved, the lead it left and the
	// lead it joined, and stating that as a list on the record is what
	// makes it the same list on every node.
	Recipients []string `json:"recipients,omitempty"`

	// Summary is at most [MaxSummary] bytes of what a card should show.
	Summary string `json:"summary,omitempty"`
}

// MaxSummary bounds the summary a change record carries.
//
// SIX HUNDRED, the same as a page change's excerpt, and for the same reason: it
// is rendered into a wake prompt beside everything else a turn is handed, so it
// is a token budget rather than a storage one.
const MaxSummary = 600

// ErrFutureVersion reports a record a newer build wrote.
//
// It carries the SUBJECT because the caller is the applier's deferral arm,
// which has to file the record under something — and a version error with no
// subject is a record that can only be dropped.
type ErrFutureVersion struct {
	Got     int
	Want    int
	Subject Subject
}

func (e *ErrFutureVersion) Error() string {
	return fmt.Sprintf("chart: the record on %s is version %d and this build "+
		"reads %d — it is RETAINED at its position rather than skipped, and "+
		"reprocessed by a build that knows the shape", e.Subject, e.Got, e.Want)
}

// DecodeEnvelope is the FIRST pass, and it never fails on version.
//
// Every branch that makes an un-decodable record survivable turns on something
// here: the subject it is filed under, the scope a writer probes for, the op a
// gate reads, and the version that decides whether there is a second pass at
// all.
func DecodeEnvelope(payload []byte) (RecordEnvelope, error) {
	var env RecordEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return RecordEnvelope{}, fmt.Errorf("chart: decode the record "+
			"envelope: %w", err)
	}
	if env.V <= 0 {
		return RecordEnvelope{}, fmt.Errorf("chart: a record carries version "+
			"%d — every record states its version, and one that does not "+
			"cannot be told apart from a newer build's", env.V)
	}
	if err := env.Subject.Validate(); err != nil {
		return RecordEnvelope{}, err
	}
	return env, nil
}

// Decode is the SECOND pass, and it is the one that may refuse on version.
//
// It returns the envelope alongside the error whenever the envelope itself
// decoded, because that is precisely the case the applier retains: the caller
// needs the subject, the scope and the op of a record it cannot read.
func Decode(payload []byte) (MutationRecord, error) {
	env, err := DecodeEnvelope(payload)
	if err != nil {
		return MutationRecord{}, err
	}
	if env.V > RecordVersion {
		return MutationRecord{RecordEnvelope: env}, &ErrFutureVersion{
			Got: env.V, Want: RecordVersion, Subject: env.Subject,
		}
	}
	var rec MutationRecord
	extra, err := jsoncarry.Decode(payload, &rec, recordFields)
	if err != nil {
		return MutationRecord{RecordEnvelope: env}, fmt.Errorf("chart: decode "+
			"the record on %s: %w", env.Subject, err)
	}
	rec.Extra = extra
	return rec, nil
}

// Encode renders a record, stamping it with the lowest version that reads it
// and carrying back whatever a newer build wrote.
//
// A ZERO VERSION IS "STAMP IT": the writer leaves V unset on every record it
// decides and this computes the minimum over [versionedFields] from the bytes
// it is about to publish, so the stamp cannot drift from what the record
// carries. A version the caller DID set is kept — a gate, a barrier and a
// generation are pinned, and a relay re-encodes a record at the version its
// writer gave it — but is refused when it is below what the record's own fields
// need, because that record would be applied, lossily, by exactly the builds
// the stamp exists to hold it back from. A record every build must read
// ([MutationRecord.readByEveryBuild]) is refused whenever it carries a
// versioned field at all.
//
// LOSSLESS IN BOTH DIRECTIONS, which is what makes a rolling upgrade safe: a
// node that read a record it only half understood and republished it — the
// reanchor path does exactly that — must not strip the half it did not. It
// goes through [jsoncarry], as every document shape here does, rather than a
// second merge of its own: a carried field LOSES to a known one, and two
// implementations of that rule are one place where a stale carried copy undoes
// the write that set it.
func Encode(rec MutationRecord) ([]byte, error) {
	return rec.encodeWith(versionedFields)
}

// encodeWith is [Encode] under a named field table — the seam the stamping
// rule is tested through with a field a later build would add.
func (rec MutationRecord) encodeWith(fields statelog.RecordFields) ([]byte, error) {
	stamp := rec.V == 0
	if stamp {
		// THE LOWEST, never [RecordVersion]: the table raises it to what
		// the record's own fields need.
		rec.V = BaseRecordVersion
	}
	data, err := jsoncarry.Encode(rec, rec.Extra)
	if err != nil {
		return nil, fmt.Errorf("chart: encode the record on %s: %w",
			rec.Subject, err)
	}
	// THE STAMP IS TAKEN OVER WHAT THIS BUILD WROTE, never over what it
	// CARRIES: a carried field is one this build does not read, so it
	// cannot be what this build's table stamps — and a relay of a record
	// carrying one keeps the version its writer gave it.
	known := data
	if len(rec.Extra) > 0 {
		if known, err = jsoncarry.Encode(rec, nil); err != nil {
			return nil, fmt.Errorf("chart: encode the record on %s: %w",
				rec.Subject, err)
		}
	}
	minimum, err := fields.Minimum(string(rec.Op), known)
	if err != nil {
		return nil, fmt.Errorf("chart: stamp the record on %s: %w", rec.Subject, err)
	}
	switch {
	case rec.readByEveryBuild() && minimum > BaseRecordVersion:
		// A GATE, A BARRIER OR A GENERATION NEVER CARRIES A VERSIONED
		// FIELD, stamped or set: a gate an older node cannot read is one
		// it defers, and a deferred gate licenses every record above it;
		// a barrier or a generation it cannot read is one it retains.
		return nil, fmt.Errorf("chart: the %s record on %s must be readable by "+
			"every build for ever, and it carries %s — a field it needs belongs "+
			"on a record that is not a gate", rec.Op, rec.Subject,
			strings.Join(fields.Carried(string(rec.Op), known), ", "))
	case rec.V >= minimum:
	case stamp:
		rec.V = minimum
		if data, err = jsoncarry.Encode(rec, rec.Extra); err != nil {
			return nil, fmt.Errorf("chart: encode the record on %s: %w",
				rec.Subject, err)
		}
	default:
		return nil, fmt.Errorf("chart: the %s record on %s is stamped version %d "+
			"and carries %s — a build reading %d would decode it, drop what it "+
			"has no field for and apply the rest; leave the version unset and "+
			"the encoder stamps the lowest one that reads it", rec.Op,
			rec.Subject, rec.V, strings.Join(fields.Carried(string(rec.Op), known), ", "),
			rec.V)
	}
	return data, nil
}

// readByEveryBuild reports a record every build there will ever be must be
// able to read: a gate, whose version is pinned at [GateRecordVersion] so an
// undecodable one is a stop rather than a deferral, and the read index's
// barrier and a reanchor's generation, pinned at [BaseRecordVersion] because an
// older node RETAINS a record it cannot read — a barrier once per linearizable
// read anywhere in the fleet, a generation as a transition it never makes.
func (rec MutationRecord) readByEveryBuild() bool {
	return rec.InstallsGate() || rec.Op == OpBarrier || rec.Op == OpGeneration
}

// recordFields is every top-level name this build writes.
//
// DERIVED from the struct rather than typed again, on [fieldSet]'s terms: the
// omitempty names have to be listed because a zero value does not marshal them,
// and a name missing here is decoded into the struct AND carried as unknown —
// so the next encode writes the stale carried copy back over what the caller
// set.
var recordFields = jsoncarry.Names(MutationRecord{})
