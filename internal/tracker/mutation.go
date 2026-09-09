package tracker

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// RecordVersion is the record shape this build writes.
//
// A record at a HIGHER version leaves the envelope decoded and everything else
// opaque, and is RETAINED at its position rather than skipped — which is the
// whole reason the decode is two passes. The one exception is a record that
// installs a gate: there an unknown version stops the applier, because a
// deferred gate licenses every later record on this node and the eviction gate
// has no inverse that repairs it.
const RecordVersion = 1

// DocumentVersion is the object shape this build writes.
//
// SEPARATE FROM THE RECORD VERSION, because they move for different reasons: a
// record version is a change to the wire format every node reads, and a
// document version is a change to what an object IS. Collapsing them would
// make a new field on a task look like a new protocol.
const DocumentVersion = 1

// OpKind is what a record does.
type OpKind string

const (
	// OpCreate mints an object at an expectation of zero.
	OpCreate OpKind = "create"

	// OpPatch changes one, arbitrated against its own last sequence.
	OpPatch OpKind = "patch"

	// OpTombstone hides an object; OpRestore clears the tombstone at any
	// age. Neither deletes anything, and there is no horizon behind
	// either.
	OpTombstone OpKind = "tombstone"
	// OpRestore clears a tombstone.
	OpRestore OpKind = "restore"

	// OpPurge is the one operation that removes rows, and the second kind
	// that INSTALLS A GATE.
	OpPurge OpKind = "purge"

	// OpTurn records a turn's spend. THE ONE ADDITIVE OP: no expectation,
	// no version bump, and an apply whose running totals are gated on its
	// own id insert affecting a row.
	OpTurn OpKind = "turn"

	// OpGeneration is a reanchor's record.
	OpGeneration OpKind = "generation"

	// OpEviction is a node's eviction or readmission.
	OpEviction OpKind = "eviction"

	// OpBarrier is the read index's append, which writes nothing anywhere.
	OpBarrier OpKind = "barrier"
)

// OpKinds are the nine, in the order they are documented.
var OpKinds = []OpKind{
	OpCreate, OpPatch, OpTombstone, OpRestore, OpPurge,
	OpTurn, OpGeneration, OpEviction, OpBarrier,
}

// Valid reports whether an op off the wire is one this build knows.
func (o OpKind) Valid() bool { return slices.Contains(OpKinds, o) }

// AuthorKind is who wrote something.
//
// `operator` is a TOKEN acting on the company's behalf and is never a seat
// handle: a tracker whose author field is chosen by the writer is not an audit
// trail, so the two are different kinds rather than two spellings of one.
type AuthorKind string

const (
	// AuthorAgent is a seat, acting inside a turn.
	AuthorAgent AuthorKind = "agent"
	// AuthorHuman is a person, through the dashboard or a chat surface.
	AuthorHuman AuthorKind = "human"
	// AuthorOperator is an API token, recorded under its own label.
	AuthorOperator AuthorKind = "operator"
	// AuthorSystem is the engine itself — a sprint close, a rollover, a
	// chart apply.
	AuthorSystem AuthorKind = "system"
)

// AuthorKinds are the four.
var AuthorKinds = []AuthorKind{
	AuthorAgent, AuthorHuman, AuthorOperator, AuthorSystem,
}

// Valid reports whether an author kind off the wire is one this build knows.
func (a AuthorKind) Valid() bool { return slices.Contains(AuthorKinds, a) }

// TermKind is what a scope term names.
type TermKind string

const (
	// TermObject is one object by id, WITHIN its container. The id is the
	// uuid alone and never the kind: a turn commit about a task and that
	// task's own create must collide, and they only do if both name the
	// object rather than their own kind of record.
	TermObject TermKind = "object"

	// TermKey is an addressable NAME — "ENG-142" — because the alias row
	// is keyed on the key rather than on any object's id.
	TermKey TermKind = "key"

	// TermContainer is a project key, or the workspace.
	TermContainer TermKind = "container"

	// TermFamily is a set of objects with no container: "person",
	// "catalogue".
	TermFamily TermKind = "family"

	// TermDomain is everything on this domain's log. The fallback for an
	// unknown term kind, and the honest reading of an absent scope.
	TermDomain TermKind = "domain"
)

// TermKinds are the five.
var TermKinds = []TermKind{
	TermObject, TermKey, TermContainer, TermFamily, TermDomain,
}

// Valid reports whether a term kind off the wire is one this build knows.
func (t TermKind) Valid() bool { return slices.Contains(TermKinds, t) }

// WorkspaceContainer is the container every object that is not a project's
// lives in.
//
// A REAL CONTAINER rather than an empty one, so "everything in the workspace"
// is a term a writer can state and a probe can match, and so no path in the
// alphabet has a hole in the middle of it.
const WorkspaceContainer = "workspace"

// The scope path alphabet.
//
// # Why the paths NEST, when the term kinds look flat
//
// The framework knows exactly one thing about a scope path: that it is a
// hierarchy written left to right with a separator, from which it computes
// CONTAINMENT. Everything the scope field is for rests on that — a record
// deferred on a project must block a write to a task in that project, and a
// record deferred on one task must NOT block a write to its neighbour.
//
// Written flat — "container/ENG" beside "object/<uuid>" — neither holds:
// the container path is not an ancestor of the object path, so the first
// blocks nothing; and adding the container beside the object on every write
// would make one un-decodable task record block its entire project.
//
// So an object's path is written UNDER its container, and that is why
// [ScopeTerm] carries a container for the two term kinds that have one. The
// plan's own sentence — "the uuid ALONE" — is about the KIND being absent
// from the path, which it is: a turn commit and its task's create both name
// o/<uuid> and therefore collide, exactly as they must.
const (
	pathDomain    = "t"
	pathContainer = "c"
	pathFamily    = "f"
	pathObject    = "o"
	pathKey       = "k"
)

// ScopeTerm is one element of a record's blast radius.
//
// DATA, NOT CODE: a build that has never heard of the op still reads every
// term, because a term names an object, a key, a container, a family or the
// domain and nothing about the operation that produced it.
type ScopeTerm struct {
	Kind TermKind `json:"k"`

	// Container is the project key an object or a key lives in, or
	// [WorkspaceContainer]. Required for TermObject and TermKey, empty
	// for the other three, and never guessed: see the alphabet above.
	Container string `json:"c,omitempty"`

	// ID is the object's uuid, the key's name, the container's key or the
	// family's name. Empty for TermDomain, which names everything.
	ID string `json:"i,omitempty"`
}

// Path renders the term as a scope path the framework can order.
func (t ScopeTerm) Path() string {
	switch t.Kind {
	case TermObject:
		return join(pathDomain, pathContainer, t.container(), pathObject, t.ID)
	case TermKey:
		return join(pathDomain, pathContainer, t.container(), pathKey, t.ID)
	case TermContainer:
		// THE CONTAINER TERM'S OWN NAME IS ITS ID, not its Container
		// field: "container(ENG)" names ENG, and reading the field
		// here would resolve every container term to the workspace and
		// make a project-wide deferral cover the whole company.
		key := t.ID
		if key == "" {
			key = t.container()
		}
		return join(pathDomain, pathContainer, key)
	case TermFamily:
		if t.ID == "" {
			return pathDomain
		}
		return join(pathDomain, pathFamily, t.ID)
	case TermDomain:
		return pathDomain
	}
	// AN UNKNOWN TERM KIND IS THE DOMAIN, NOT A SKIPPED TERM. A term this
	// build cannot interpret is a claim about a blast radius it cannot
	// bound, and the only safe reading of an unbounded claim is the widest
	// one. Dropping it would silently narrow a newer peer's scope to
	// whatever this build happened to recognise.
	return pathDomain
}

// container is the term's container, defaulting to the workspace.
func (t ScopeTerm) container() string {
	if t.Container == "" {
		return WorkspaceContainer
	}
	return t.Container
}

// join builds a scope path from its segments, skipping empty ones so a
// missing id can never produce a path with a hole in it.
func join(segments ...string) string {
	kept := make([]string, 0, len(segments))
	for _, s := range segments {
		if s = strings.TrimSpace(s); s != "" {
			kept = append(kept, s)
		}
	}
	return strings.Join(kept, statelog.ScopeSeparator)
}

// MaxScopeTerms is the cap on a record's declared scope.
//
// NOT A NEW NUMBER: it is this design's universal fan-out batch — 64
// descendants per move batch, 64 children per merge batch, 64 dependents, 64
// watchers, 64 tasks per goal target, 64 tasks per bulk update. A writer whose
// affected set would exceed it emits the smallest COVERING container or family
// term instead, so the field is bounded by construction rather than by a cap a
// writer can hit and then have to handle.
//
// Priced, so the cap reads as load-bearing: if every loud record enumerated 64
// ids it would cost ≈ 1 068 MB/yr, +31 % of the whole log. An unbounded
// cascade is expressed in O(1) bytes or not at all.
const MaxScopeTerms = 64

// ScopeSet is the COMPLETE set of objects a record's apply may write, stated
// by the WRITER and readable at every version.
//
// NEVER EMPTY. The exactly-the-subject case — almost every record — encodes
// as the one-byte sentinel, so the common case costs thirteen bytes and the
// field can never be absent by accident. An absent or unreadable scope on a
// record this build cannot decode is the DOMAIN term, never "just the
// subject": assuming a V1 convention holds for a V2 operation is the bug this
// field exists to close.
type ScopeSet struct {
	// Subject is true for the sentinel: this record touches exactly the
	// object its subject names, and nothing else.
	Subject bool

	// Container is the project the subject lives in, and rides the
	// sentinel.
	//
	// # Why the container is ON THE RECORD and not derived from the subject
	//
	// A subject names an object, not its home: a task's subject is its
	// uuid, and which project it lives in is a fact about the row — one
	// that CHANGES, because a task can move between projects. So the path
	// a record's scope resolves to cannot be computed from the subject
	// alone, and the only party that knows it at the moment the record is
	// written is the writer.
	//
	// Leaving it out is silent rather than wrong-looking. Every task
	// record would file its deferral under the workspace container while
	// every project-scoped read probed its own, and the two-clause
	// containment probe — the one piece of SQL here where a wrong clause
	// is data loss and not a wrong answer — would simply never match.
	//
	// Empty is [WorkspaceContainer], which is what the objects that
	// genuinely live at the top of the company resolve to.
	Container string

	// Terms is the enumeration, when the record touches more than its own
	// subject. A container rides the sentinel and never an enumeration:
	// every term already states its own.
	Terms []ScopeTerm
}

// ScopeSentinel is the one-byte encoding of "exactly the subject".
const ScopeSentinel = "s"

// MarshalJSON encodes the sentinel as a bare string and everything else as an
// array, so the common case is one byte on the wire.
//
// A container is appended to the sentinel with the path separator — "s/ENG" —
// which keeps the workspace case at one byte and costs a project key on the
// records that have one. The separator cannot appear inside a container: a
// project key is a slug, checked by [ValidSlug].
func (s ScopeSet) MarshalJSON() ([]byte, error) {
	if s.Subject || len(s.Terms) == 0 {
		if s.Container != "" {
			return json.Marshal(ScopeSentinel + statelog.ScopeSeparator + s.Container)
		}
		return json.Marshal(ScopeSentinel)
	}
	return json.Marshal(s.Terms)
}

// UnmarshalJSON accepts both encodings, and reads anything else as the domain.
//
// A SCOPE THAT DOES NOT DECODE IS THE WIDEST TERM, never an error and never an
// empty set. This runs on a node reading a record a newer build wrote: the
// only honest reading of a blast radius it cannot parse is "everything", and
// returning an error here would take the whole two-pass decode down with it.
func (s *ScopeSet) UnmarshalJSON(b []byte) error {
	var sentinel string
	if err := json.Unmarshal(b, &sentinel); err == nil {
		head, container, _ := strings.Cut(sentinel, statelog.ScopeSeparator)
		if head != ScopeSentinel {
			*s = ScopeSet{Terms: []ScopeTerm{{Kind: TermDomain}}}
			return nil
		}
		*s = ScopeSet{Subject: true, Container: container}
		return nil
	}
	var terms []ScopeTerm
	if err := json.Unmarshal(b, &terms); err != nil || len(terms) == 0 {
		*s = ScopeSet{Terms: []ScopeTerm{{Kind: TermDomain}}}
		return nil
	}
	*s = ScopeSet{Terms: terms}
	return nil
}

// Resolve renders the scope as the framework's own path set.
//
// ONE ARGUMENT, deliberately: the container used to be passed in beside the
// subject, and a scope resolved with it on the write path and without it on
// the decode path is two different paths for one record — the writer probing
// the project's closure and the applier filing under the workspace's. It takes
// the subject alone now, and the container comes from the record itself.
func (s ScopeSet) Resolve(subject Subject) statelog.ScopeSet {
	if s.Subject || len(s.Terms) == 0 {
		return statelog.ScopeSet{Paths: []string{subjectPath(subject, s.Container)}}
	}
	paths := make([]string, 0, len(s.Terms))
	for _, t := range s.Terms {
		paths = append(paths, t.Path())
	}
	return statelog.ScopeSet{Paths: paths}.Normalised()
}

// subjectPath is where an object's own subject sits in the alphabet.
func subjectPath(s Subject, container string) string {
	switch s.Kind {
	case KindProject, KindCounter, KindTags, KindRankOrder:
		// THE FOUR PROJECT-SCOPED OBJECTS whose id IS a container key.
		// Their own path is the container, which is what makes a
		// deferred settings edit block every write inside that project
		// — which is exactly what an archive is.
		return ScopeTerm{Kind: TermContainer, ID: s.ID}.Path()
	case KindSprint:
		project, _, _ := strings.Cut(s.ID, ".")
		return ScopeTerm{Kind: TermObject, Container: project, ID: s.ID}.Path()
	case KindAlias:
		key, _, _ := strings.Cut(s.ID, ".")
		project, _, _ := strings.Cut(key, "-")
		return ScopeTerm{Kind: TermKey, Container: project, ID: key}.Path()
	case KindCatalogue:
		return ScopeTerm{Kind: TermFamily, ID: string(KindCatalogue)}.Path()
	case KindPerson:
		return ScopeTerm{Kind: TermFamily, ID: string(KindPerson)}.Path()
	case KindBarrier:
		// THE ONE-BYTE SENTINEL OF THE FRAMEWORK'S OWN SCOPE, so a
		// barrier never intersects any read's closure. A barrier
		// writes no row anywhere, so a scope that intersected anything
		// would make every linearizable read wait behind every other.
		return statelog.BarrierScope
	case KindGeneration, KindEviction:
		// A GATE IS ABOUT THE WHOLE DOMAIN. It licenses or drops
		// records on every subject, so anything narrower would be a
		// claim the record does not make.
		return pathDomain
	default:
		return ScopeTerm{Kind: TermObject, Container: container, ID: s.ID}.Path()
	}
}

// Validate refuses a scope a writer could not have meant.
func (s ScopeSet) Validate() error {
	if s.Subject {
		if len(s.Terms) != 0 {
			return fmt.Errorf("tracker: a scope carries the exactly-the-subject "+
				"sentinel and %d enumerated term(s): the two say different "+
				"things about the same record", len(s.Terms))
		}
		if s.Container != "" {
			// THE SEPARATOR IS WHAT THE SENTINEL ENCODING RESTS ON, and a
			// blank segment is what the path join drops. A container
			// carrying either decodes as a DIFFERENT container and files
			// the record's deferral under a path no probe reaches — so
			// both are refused where the value is accepted rather than
			// trusted where it is split.
			switch {
			case strings.Contains(s.Container, statelog.ScopeSeparator):
				return fmt.Errorf("tracker: container %q contains %q, which is "+
					"the path separator the sentinel encoding splits on — it "+
					"would decode as %q and file this record where no probe "+
					"for it looks", s.Container, statelog.ScopeSeparator,
					strings.SplitN(s.Container, statelog.ScopeSeparator, 2)[0])
			case strings.TrimSpace(s.Container) == "":
				return fmt.Errorf("tracker: container %q is blank, and a blank "+
					"segment is dropped from the path — the record would "+
					"resolve to its container's own path rather than to its "+
					"own", s.Container)
			}
		}
		return nil
	}
	if s.Container != "" {
		return fmt.Errorf("tracker: a scope enumerates %d term(s) and also "+
			"names container %q: every term states its own container, so the "+
			"field says nothing an enumeration has not already said",
			len(s.Terms), s.Container)
	}
	if len(s.Terms) == 0 {
		return fmt.Errorf("tracker: a scope is empty — a record that touches " +
			"nothing writes nothing, and an absent scope is read as the whole " +
			"domain rather than as the subject")
	}
	if len(s.Terms) > MaxScopeTerms {
		return fmt.Errorf("tracker: a scope enumerates %d terms and the cap is "+
			"%d — a writer whose affected set is larger states the smallest "+
			"covering container or family term instead, so the field stays "+
			"bounded by construction", len(s.Terms), MaxScopeTerms)
	}
	for _, t := range s.Terms {
		switch t.Kind {
		case TermObject, TermKey:
			if t.ID == "" {
				return fmt.Errorf("tracker: a %s term names nothing", t.Kind)
			}
		case TermContainer, TermFamily:
			if t.ID == "" {
				return fmt.Errorf("tracker: a %s term names nothing", t.Kind)
			}
		case TermDomain:
		default:
			return fmt.Errorf("tracker: scope term kind %q is not one this build "+
				"writes; it is READ as the whole domain, which is safe, and "+
				"WRITING one is a writer that cannot say what it touched", t.Kind)
		}
	}
	return nil
}

// RecordEnvelope decodes at EVERY version, BEFORE V is consulted.
//
// Its EIGHT keys are RESERVED at the top level of the format for its life: a
// later version may add fields beside them and may never repurpose one. It is
// the same split the event envelope makes between a known header and an opaque
// body, for the same reason — a rolling upgrade puts a record this build
// cannot read on the wire, and dropping it would make every upgrade an outage.
type RecordEnvelope struct {
	// V is the record version. Refused for the PAYLOAD when unknown,
	// never for this struct.
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

	// CreatedAt is THE AUTHORED INSTANT, the writer's own clock. Reported,
	// never ORDERED ON, and differenced against exactly one thing — the
	// broker's stored instant, to publish a skew. Empty on a barrier,
	// because nothing renders one.
	CreatedAt time.Time `json:"created_at,omitzero"`

	// Gen is the generation. It is what makes an object's version a pure
	// function of the record — (Gen << 40) | the broker's own sequence —
	// so the applier needs no table lookup, no config read and no clock.
	// Present on a barrier too, because a barrier from a previous
	// generation must be refusable.
	Gen uint32 `json:"gen,omitempty"`

	// Writer is the publishing node's id, read by the eviction gate —
	// which runs before any kind rule, on a node that may not be able to
	// decode the payload at all. Empty on a barrier, which writes no rows
	// for a gate to drop.
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
// TWO CONDITIONS, NOT ONE. An eviction is a gate by its KIND; a purge is a
// gate by its OP, on an ordinary task subject. Reading only the kind would let
// a purge this build cannot decode be deferred — and a deferred purge is a
// node that goes on serving rows every other node has removed, with no inverse
// that repairs it.
func (e RecordEnvelope) InstallsGate() bool {
	return e.Subject.Kind.InstallsGate() || e.Op == OpPurge
}

// MutationRecord is one committed mutation: the envelope plus everything a
// build at this version may read.
type MutationRecord struct {
	RecordEnvelope

	// Expect is the version this mutation was decided against. PROVENANCE
	// ONLY — the broker is what enforced it, and nothing reads this to
	// decide anything.
	Expect uint64 `json:"expect,omitempty"`

	// Mutation is the typed payload for (Subject.Kind, Op), left as
	// OPAQUE BYTES when the version is above this build's.
	Mutation json.RawMessage `json:"mutation,omitempty"`

	Actor      string     `json:"actor,omitempty"`
	ActorKind  AuthorKind `json:"actor_kind,omitempty"`
	OperatorID string     `json:"operator_id,omitempty"`
	TurnID     string     `json:"turn_id,omitempty"`
	Chain      []string   `json:"chain,omitempty"`
	BatchID    *string    `json:"batch_id,omitempty"`

	// Notify is the routing snapshot, and NIL is what "wakes nobody"
	// means.
	//
	// A NIL POINTER RATHER THAN A quiet FLAG, so the wake filter is "has a
	// Notify" — a question about the record's own shape — instead of a
	// boolean a writer can forget to set. A quiet commit is a full record,
	// arbitrated exactly like a loud one, and writes its history row like
	// every other: quiet means it wakes nobody, and nothing else.
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

// DecodeEnvelope is the FIRST pass, and it never fails on version.
//
// Every branch that makes an un-decodable record survivable turns on something
// here: the subject it is filed under, the scope a writer probes for, the kind
// a gate reads, and the version that decides whether there is a second pass at
// all.
func DecodeEnvelope(payload []byte) (RecordEnvelope, error) {
	var env RecordEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return RecordEnvelope{}, fmt.Errorf("tracker: decode the record "+
			"envelope: %w", err)
	}
	if env.V <= 0 {
		return RecordEnvelope{}, fmt.Errorf("tracker: a record carries version "+
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
// needs the subject, the scope and the kind of a record it cannot read.
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
	if err := json.Unmarshal(payload, &rec); err != nil {
		return MutationRecord{RecordEnvelope: env}, fmt.Errorf("tracker: decode "+
			"the record on %s: %w", env.Subject, err)
	}
	var extra map[string]json.RawMessage
	if err := json.Unmarshal(payload, &extra); err == nil {
		for _, known := range knownKeys {
			delete(extra, known)
		}
		if len(extra) > 0 {
			rec.Extra = extra
		}
	}
	return rec, nil
}

// knownKeys are the top-level keys this build writes, so Extra holds exactly
// what it does not.
//
// LISTED RATHER THAN REFLECTED, because the list is the format's own reserved
// set: a key added to the struct and not here would be carried in Extra as
// well as in its field, and re-encoded twice.
var knownKeys = []string{
	"v", "op_id", "subject", "op", "created_at", "gen", "writer", "scope",
	"expect", "mutation", "actor", "actor_kind", "operator_id", "turn_id",
	"chain", "batch_id", "notify",
}

// ErrFutureVersion reports a record a newer build wrote.
type ErrFutureVersion struct {
	Got, Want int
	Subject   Subject
}

func (e *ErrFutureVersion) Error() string {
	return fmt.Sprintf("tracker: the record on %s is version %d and this build "+
		"reads %d — it is RETAINED at its position rather than skipped, so a "+
		"build that can read it applies it later", e.Subject, e.Got, e.Want)
}

// Encode writes a record.
//
// The scope is validated HERE rather than at the applier, because a scope that
// under-states a record's blast radius is only ever a writer's mistake and the
// applier has nothing to compare it against.
func (r MutationRecord) Encode() ([]byte, error) {
	if r.V == 0 {
		r.V = RecordVersion
	}
	if err := r.Subject.Validate(); err != nil {
		return nil, err
	}
	if !r.Op.Valid() {
		return nil, fmt.Errorf("tracker: op %q is not one this build writes", r.Op)
	}
	if err := r.Scope.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(r)
}

// ChangeKind is what happened, as a recipient needs to be told it.
//
// THE SPLIT IS BY WHAT A RECIPIENT NEEDS, not by which column moved: a card
// saying "the status changed" and a card saying "you were assigned" are two
// different messages about one write. Every kind has exactly ONE writer and no
// kind has variants — a reader tells a close from a re-target of its
// spillover by the delta, not by a second kind.
type ChangeKind string

const (
	ChangeCreated         ChangeKind = "created"
	ChangeFields          ChangeKind = "fields"
	ChangeStatus          ChangeKind = "status"
	ChangeAssignee        ChangeKind = "assignee"
	ChangeCollaborators   ChangeKind = "collaborators"
	ChangeWatchers        ChangeKind = "watchers"
	ChangeTags            ChangeKind = "tags"
	ChangeRelations       ChangeKind = "relations"
	ChangeRouted          ChangeKind = "routed"
	ChangeMoved           ChangeKind = "moved"
	ChangeReparented      ChangeKind = "reparented"
	ChangeSprint          ChangeKind = "sprint"
	ChangeChecklist       ChangeKind = "checklist"
	ChangeArchived        ChangeKind = "archived"
	ChangeComment         ChangeKind = "comment"
	ChangeCommentEdited   ChangeKind = "comment_edited"
	ChangeCommentResolved ChangeKind = "comment_resolved"
	ChangeCommentRemoved  ChangeKind = "comment_removed"
	ChangeRemoved         ChangeKind = "removed"
	ChangeRestored        ChangeKind = "restored"
	ChangePurged          ChangeKind = "purged"
	ChangeProjectCreated  ChangeKind = "project_created"
	ChangeProjectUpdated  ChangeKind = "project_updated"
	ChangePolicyChanged   ChangeKind = "policy_changed"
	ChangeSprintMinted    ChangeKind = "sprint_minted"
	ChangeSprintStarted   ChangeKind = "sprint_started"
	ChangeSprintClosed    ChangeKind = "sprint_closed"
	ChangeGoalUpdated     ChangeKind = "goal_updated"
	ChangeViewSaved       ChangeKind = "view_saved"
	ChangeCatalogue       ChangeKind = "catalogue_updated"
	ChangePrioritised     ChangeKind = "prioritised"
)

// ChangeKinds are the thirty-one.
//
// THIRTY-ONE AGAINST FIFTEEN SUBJECTS, and the gap is not an inconsistency:
// five commit classes carry no notification at all — a turn, a generation, an
// eviction, a rank move and a barrier — because a reposition is not history
// and a barrier writes no rows whatever.
var ChangeKinds = []ChangeKind{
	ChangeCreated, ChangeFields, ChangeStatus, ChangeAssignee,
	ChangeCollaborators, ChangeWatchers, ChangeTags, ChangeRelations,
	ChangeRouted, ChangeMoved, ChangeReparented, ChangeSprint,
	ChangeChecklist, ChangeArchived, ChangeComment, ChangeCommentEdited,
	ChangeCommentResolved, ChangeCommentRemoved, ChangeRemoved,
	ChangeRestored, ChangePurged, ChangeProjectCreated, ChangeProjectUpdated,
	ChangePolicyChanged, ChangeSprintMinted, ChangeSprintStarted,
	ChangeSprintClosed, ChangeGoalUpdated, ChangeViewSaved, ChangeCatalogue,
	ChangePrioritised,
}

// Valid reports whether a change kind off the wire is one this build knows.
func (k ChangeKind) Valid() bool { return slices.Contains(ChangeKinds, k) }

// MaxDeltas caps what a notification DISPLAYS, and governs nothing else.
//
// THIRTY-TWO IS A DISPLAY LIMIT. It must never govern the mutation: a write
// touching 128 field values carries all 128 in its payload — that is what
// makes the record reproducible — and shows 32. Letting one number do both
// jobs is how a record becomes structurally unable to represent a write the
// tools accept.
const MaxDeltas = 32

// MaxExcerpt is how much of a body or a comment a card carries.
//
// Six hundred bytes, on the notification side only. There is no excerpt on the
// mutation side at all: a patch carries the COMPLETE new value, because a
// record that carried an excerpt could not rebuild the row.
const MaxExcerpt = 600

// Delta is one field's before and after, AS TEXT.
//
// Text rather than the typed value, because a change record is read by a
// notification card, a person and a model, and every one of them wants
// "todo → in_progress". The typed value is on the row for anything that
// needs it.
type Delta struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// TaskParty is one task and who is on it, for the two lists that name
// somebody else's task.
type TaskParty struct {
	Task     string `json:"task"`
	Key      string `json:"key,omitempty"`
	Assignee string `json:"assignee,omitempty"`
}

// Snapshot is the routing state of a task at the moment of a change.
//
// COPIED RATHER THAN LOOKED UP, and that is what lets the node that wins a
// feed delivery route without reading anything: a node whose own replica had
// not caught up would otherwise route from a stale row, or block the feed
// until it had.
type Snapshot struct {
	Key         string      `json:"key"`
	Project     string      `json:"project"`
	Title       string      `json:"title"`
	Status      Status      `json:"status"`
	StatusGroup StatusGroup `json:"status_group,omitempty"`
	Assignee    string      `json:"assignee,omitempty"`
	Reporter    string      `json:"reporter,omitempty"`

	// Watchers is the set MINUS the muted, so the feed never has to
	// subtract and can never forget to. The MUTE ITSELF travels in the
	// mutation payload whenever the watcher collection is touched — a
	// replay that saw only this list could not tell "not a watcher" from
	// "watching but muted", and would silently re-add every unwatched
	// person on the next mention.
	Watchers      []string `json:"watchers,omitempty"`
	Collaborators []string `json:"collaborators,omitempty"`

	// ProjectLead and RoutingUnitLead are BOTH carried, because they are
	// two different fallbacks and which one applies depends on whether the
	// task has a routing unit at the moment of the change — a fact the
	// receiving node cannot reconstruct later.
	ProjectLead     string `json:"project_lead,omitempty"`
	RoutingUnitLead string `json:"routing_unit_lead,omitempty"`

	// Unblocked names every dependent this change cleared, with its
	// assignee. Stamped from the task's own dependents when it enters a
	// finished group — a WAKE, not a safeguard: nothing was refused,
	// somebody was told.
	Unblocked []TaskParty `json:"unblocked,omitempty"`

	// Dependents names every dependent this change added or removed, for
	// the BLOCKER's side of a relations commit — a new dependent is a fact
	// to weigh rather than a question to answer.
	Dependents []TaskParty `json:"dependents,omitempty"`

	// PrevAssignee and PrevStatusGroup are what the row held BEFORE this
	// change.
	//
	// ON THE SNAPSHOT rather than looked up, because routing turns on the
	// TRANSITION and not on the destination: "entered or left a finished
	// group" is what wakes a reporter and a parent's assignee, and a node
	// deriving it later would be reading a row that has moved on.
	PrevAssignee    string      `json:"prev_assignee,omitempty"`
	PrevStatusGroup StatusGroup `json:"prev_status_group,omitempty"`

	// CommentAuthorKind decides whether the assignee is ADDRESSED by a
	// comment: a person's comment on your ticket is the ask, and another
	// agent's is not.
	CommentAuthorKind AuthorKind `json:"comment_author_kind,omitempty"`

	// CommentAsk is the handle a comment asked, set only at creation.
	CommentAsk string `json:"comment_ask,omitempty"`

	// AnsweredAuthor is the author of the ASKED comment.
	//
	// A SNAPSHOT FIELD RATHER THAN A LOOKUP, and the reason is that the
	// comment author on a reply is the ANSWERER: routing off that handle
	// would wake the seat that just answered and leave the person who
	// asked unwoken.
	AnsweredAuthor string `json:"answered_author,omitempty"`

	// ThreadParticipants are the people already in a comment thread, set
	// only on a reply.
	ThreadParticipants []string `json:"thread_participants,omitempty"`

	// RemovedWatchers are the handles a watchers commit dropped. They go
	// to the INBOX only: a human learns that a lead removed her watch, and
	// an agent is not woken about a decision that was not its own.
	RemovedWatchers []string `json:"removed_watchers,omitempty"`

	// ParentAssignee hears when a child enters or leaves a finished group.
	ParentAssignee string `json:"parent_assignee,omitempty"`

	// ChecklistAssignees are the people whose checklist items changed.
	ChecklistAssignees []string `json:"checklist_assignees,omitempty"`

	// GoalOwners and GoalMembers hear about a goal.
	GoalOwners  []string `json:"goal_owners,omitempty"`
	GoalMembers []string `json:"goal_members,omitempty"`

	// SprintAssignees hear about a sprint starting or closing.
	SprintAssignees []string `json:"sprint_assignees,omitempty"`

	// RoutedTo is the effective lead of a task's NEW routing unit, and it
	// is an ORDINARY candidate rather than a fallback.
	//
	// The promise is "the new unit's lead learns that work routes to them
	// now", and a fallback candidate cannot keep it: a fallback survives
	// only when no ordinary candidate did, so on any task that still has
	// an assignee, a collaborator or a watcher the new lead would be
	// dropped in silence.
	RoutedTo string `json:"routed_to,omitempty"`

	// Person is whose object a person-scoped change was about — the
	// handle whose priorities somebody else wrote.
	Person string `json:"person,omitempty"`
}

// Notify is the routing snapshot a wake is derived from.
//
// # Why it is a pointer, and what nil means
//
// Nil means the commit wakes nobody, and nothing else. A quiet commit is a
// full mutation record, is arbitrated exactly like a loud one, and writes its
// history row like every other — so the tracker's activity feed is a complete
// account of what happened rather than an account of what was announced.
//
// A POINTER RATHER THAN A quiet FLAG, so the wake filter asks "does this
// record have a Notify" — a question about the record's own shape — instead
// of reading a boolean a writer can forget to set. The flag was set by hand in
// six places and missed in one.
type Notify struct {
	Kind ChangeKind `json:"kind"`

	// Fields are the deltas a card renders, capped at MaxDeltas.
	Fields map[string]Delta `json:"fields,omitempty"`

	CommentID string `json:"comment_id,omitempty"`

	// Excerpt is at most MaxExcerpt bytes of what a card should show, cut
	// rune-safely.
	Excerpt string `json:"excerpt,omitempty"`

	Mentions []string `json:"mentions,omitempty"`

	// Late marks a wake the repair duty issued rather than the write
	// itself — a one-sided relation whose blocker-side commit was never
	// written, so its assignee was never told. It is the only wake that
	// side gets, and saying so on the card is what stops it reading as a
	// duplicate.
	Late bool `json:"late,omitempty"`

	Snapshot Snapshot `json:"snapshot"`
}

// Validate refuses a notification that would render wrong.
func (n *Notify) Validate() error {
	if n == nil {
		return nil
	}
	if !n.Kind.Valid() {
		return fmt.Errorf("tracker: %q is not a change kind this build writes — "+
			"every kind has exactly one writer, so an unknown one is a wake "+
			"nothing renders a card for", n.Kind)
	}
	if len(n.Fields) > MaxDeltas {
		return fmt.Errorf("tracker: a notification carries %d deltas and a card "+
			"shows %d — the cap is on the DISPLAY, and a writer that hit it "+
			"should be trimming what it shows rather than what it recorded",
			len(n.Fields), MaxDeltas)
	}
	if len(n.Excerpt) > MaxExcerpt {
		return fmt.Errorf("tracker: a notification excerpt is %d bytes against a "+
			"%d cap", len(n.Excerpt), MaxExcerpt)
	}
	return nil
}

// The two folds, and what each one saves.
//
// A FOLD is a record class this design does NOT have because its content
// rides one it already has. Both are stated here because both were separate
// record classes first, and both were removed on arithmetic rather than on
// taste.

// TurnSpend is a turn's cost, and it rides the turn record it is computed
// from.
//
// THE FIRST FOLD. Carried as its own class it was 1 095 000 records a year —
// a third of a naive census — and it made a task's running total a separately
// transmitted number that could disagree with the turns it was meant to
// summarise. Riding the turn makes the total a FUNCTION of the applied
// records: the applier inserts the turn, and adds these counters only when
// that insert affected a row, in the same transaction. A redelivery therefore
// cannot double-count, and an update affecting zero rows is a malformed record
// that stops the loop rather than a rounding error nobody sees.
type TurnSpend struct {
	Turns      int `json:"turns,omitempty"`
	Rounds     int `json:"rounds,omitempty"`
	Input      int `json:"input,omitempty"`
	Output     int `json:"output,omitempty"`
	CacheRead  int `json:"cache_read,omitempty"`
	CacheWrite int `json:"cache_write,omitempty"`
	WallMs     int `json:"wall_ms,omitempty"`
}

// Tokens is the derived eighth counter, so nothing else adds the two halves.
func (s TurnSpend) Tokens() int { return s.Input + s.Output }

// InboxDelta is a person's inbox change, as a DELTA rather than the document.
//
// THE SECOND FOLD, and it is pure arithmetic. The person object holds read,
// unread and snoozed at a cap of 256 each — about 2 KiB. At fifty people
// marking twenty items a day, carrying the whole document is 365 000 × 2 KiB
// = 730 MB a year; as a delta at ≈ 300 B it is 109.5 MB. The document is
// still what the object IS: this is what a record carries about it.
type InboxDelta struct {
	// SeenThrough is the position everything at or below is pruned at, and
	// it is what makes the delta safe to apply out of order: a stale delta
	// re-adds nothing, because the prune runs on every write.
	SeenThrough uint64 `json:"seen_through,omitempty"`

	// ReadAdd and UnreadDrop are the two edges an inbox action moves.
	ReadAdd    []string `json:"read_add,omitempty"`
	UnreadDrop []string `json:"unread_drop,omitempty"`
}
