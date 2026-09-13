package search

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// The VECTOR RECORD, and why this domain is shaped nothing like the tracker's.
//
// Both are [statelog] domains and that is the point of having two: the
// framework's contracts are the same for a strict, arbitrated, identity-
// claiming log and for a compacted, unarbitrated, derived one, and everything
// that differs between them is DECLARED rather than assumed. What differs
// here:
//
//   - THE STREAM IS A KEYED TABLE. One message per subject, so an ordinary
//     write removes an interior sequence and the replay loop steps over it. A
//     gap is a coverage number rather than a fault.
//   - NOTHING ARBITRATES. Exactly one writer — the fleet's embedding duty —
//     publishes on a subject, so there is no expectation to form and no anchor
//     row to advance. Concurrency control is the applier's own version guard.
//   - THERE IS NO OPERATION LEDGER, because nothing reports a committed
//     position to a caller: an embedding is written for a source, not for a
//     person waiting on an answer.
//   - NOTHING CLAIMS IDENTITY. Two nodes legitimately hold different coverage
//     — one joined after a trim, one is mid-fill — and asserting they hold the
//     same rows would report a healthy fleet as broken.
//
// # Why an embedding is a record at all
//
// It costs a provider call per source, and it is the one thing in the search
// path a node cannot recompute on its own. Published once, applied N times,
// the fleet pays the bill once and every node holds the answer — which is what
// the node estate's own rule said a table there may NOT be.

// RecordVersion is the record shape this build reads.
//
// A record above it is RETAINED at its position rather than skipped, so a
// newer peer's shape survives a rolling upgrade in both directions.
const RecordVersion = 1

// Source is where an embedded document came from.
//
// A NAMED STRING TYPE with a Valid method, so an unknown value off the wire is
// a value rather than a panic — and one that arrives from a newer build is a
// record this build defers rather than one it mis-files.
type Source string

const (
	// SourcePage is a knowledge-base page.
	SourcePage Source = "page"

	// SourceTask is a work item in the native tracker.
	SourceTask Source = "task"
)

// Sources is every source this build embeds, for the enum's own validation and
// for the tests that walk them all.
var Sources = []Source{SourcePage, SourceTask}

// Valid reports whether a source off the wire is one this build knows.
func (s Source) Valid() bool { return slices.Contains(Sources, s) }

// Op is what a vector record does.
type Op string

const (
	// OpEmbed writes the vector for a source, replacing whatever was
	// there.
	OpEmbed Op = "embed"

	// OpForget removes it, because the source is gone.
	//
	// A RECORD RATHER THAN AN ABSENCE, and on a compacted stream that is
	// the only shape available: a subject with no message is a subject
	// nothing was ever published on, so a node replaying from the
	// beginning would never learn that a vector was withdrawn. The
	// tombstone leaves with the stream's own age bound, by which time
	// the embed it supersedes has left too.
	OpForget Op = "forget"
)

// Ops is every operation this build writes.
var Ops = []Op{OpEmbed, OpForget}

// Valid reports whether an operation off the wire is one this build knows.
func (o Op) Valid() bool { return slices.Contains(Ops, o) }

// Subject is the object a vector record is about: one source document.
//
// ONE SUBJECT PER SOURCE, which is what makes the compaction do the right
// thing — the stream keeps the newest vector for each document and nothing
// else, which is exactly the state the table holds.
type Subject struct {
	Source Source `json:"source"`
	ID     string `json:"id"`
}

// Validate refuses a subject that cannot address a row.
func (s Subject) Validate() error {
	if !s.Source.Valid() {
		return fmt.Errorf("search: %q is not a source this build embeds", s.Source)
	}
	if strings.TrimSpace(s.ID) == "" || strings.ContainsAny(s.ID, " \t\n*>") {
		return fmt.Errorf("search: %q is not a source id — it is a subject "+
			"token on the wire, so it can be neither empty nor a wildcard",
			s.ID)
	}
	return nil
}

// String renders a subject for a log line and for the wire.
func (s Subject) String() string { return string(s.Source) + "." + s.ID }

// ScopePath is the one path a vector record's apply touches.
//
// DERIVED FROM THE CONTAINER AND THE SUBJECT, and carried on the wire rather
// than recomputed by a reader: a build that cannot decode the payload still
// has to know what this record makes stale, and a scope a reader derives is a
// scope that narrows the moment a newer build widens one.
//
// The alphabet is three levels — the domain, the container, the document — so
// a read scoped to one container is covered by the framework's own containment
// walk with no vector-shaped rule inside it.
func ScopePath(container string, subject Subject) string {
	container = strings.TrimSpace(container)
	if container == "" {
		container = UnfiledContainer
	}
	return strings.Join([]string{ScopeRoot, container, subject.String()},
		statelog.ScopeSeparator)
}

const (
	// ScopeRoot is the domain's own level, so a scope naming it covers
	// every document in the company.
	ScopeRoot = "v"

	// UnfiledContainer is the container a source with none is filed under.
	//
	// A CONCRETE LEVEL rather than an empty one: an empty segment would
	// collapse `v//<doc>` to a path whose parent is the domain root, so a
	// read scoped to one container would be blocked by a deferral on a
	// document filed nowhere near it.
	UnfiledContainer = "_"
)

// RecordEnvelope is the half EVERY build can read, for ever.
//
// Its EIGHT keys are reserved at the top level of the format for its life: a
// later version may add fields beside them and may never repurpose one. Every
// branch that makes an un-decodable record survivable turns on one of them.
type RecordEnvelope struct {
	// V is the record version. Refused for the PAYLOAD when unknown,
	// never for this struct.
	V int `json:"v"`

	// OpID is the publisher's own idempotency key and the Nats-Msg-Id.
	//
	// Carried although this domain keeps NO operation ledger: the broker's
	// duplicate window still collapses a retry, which saves the round trip
	// on the one path that retries constantly — a duty re-running a batch
	// it could not confirm.
	OpID string `json:"op_id,omitempty"`

	// Subject is the source document this record is about.
	Subject Subject `json:"subject"`

	// Op is what the record does.
	Op Op `json:"op"`

	// CreatedAt is the writer's own clock. Reported, never ordered on.
	CreatedAt time.Time `json:"created_at,omitzero"`

	// Gen is the generation the writer published in, so a record from a
	// previous one is comparable and safely stale.
	Gen uint32 `json:"gen,omitempty"`

	// Writer is the publishing node.
	Writer string `json:"writer,omitempty"`

	// Scope is every object this record's apply may write — one path, and
	// carried rather than derived so a newer build's wider scope is
	// readable by this one.
	Scope statelog.ScopeSet `json:"scope"`
}

// VectorRecord is one committed vector mutation.
type VectorRecord struct {
	RecordEnvelope

	// Container is where the source is filed, denormalised onto the row
	// because every scoped search filters on it and the source table lives
	// in the other estate.
	Container string `json:"container,omitempty"`

	// Model and Dim are the embedding space. BOTH, because a model change
	// at the SAME width leaves two incompatible spaces in one candidate
	// pool with nothing able to exclude either half.
	Model string `json:"model,omitempty"`
	Dim   int    `json:"dim,omitempty"`

	// SourceRev is the source version this vector was computed from, so
	// the duty knows a row is stale without re-embedding it.
	SourceRev uint64 `json:"source_rev,omitempty"`

	// TextSHA is the digest of the exact text that was embedded. A source
	// rewritten into the same words — a re-file, a label, a parent move —
	// is what this stops the duty paying for.
	TextSHA string `json:"text_sha,omitempty"`

	// Embedding is the vector as PACKED LITTLE-ENDIAN FLOAT32, which is
	// byte-for-byte what the column holds and what `vector1bit(?)`
	// consumes.
	//
	// NOT A JSON ARRAY OF NUMBERS. The packed form is 12 KiB at 3 072
	// dimensions and base64 makes it 16 KiB; a decimal array of the same
	// vector is around 30 KiB and has to be re-parsed and re-packed by
	// every node that applies it, to a value that must be bit-identical on
	// all of them.
	Embedding []byte `json:"embedding,omitempty"`

	// Extra carries fields a newer build wrote, so a record round-trips
	// losslessly through a node that cannot interpret them.
	Extra map[string]json.RawMessage `json:"-"`
}

// knownKeys are the top-level keys this build writes, so Extra holds exactly
// what it does not.
//
// LISTED RATHER THAN REFLECTED, because the list is the format's own reserved
// set: a key added to the struct and not here would be carried in Extra as
// well as in its field, and re-encoded twice.
var knownKeys = []string{
	"v", "op_id", "subject", "op", "created_at", "gen", "writer", "scope",
	"container", "model", "dim", "source_rev", "text_sha", "embedding",
}

// DecodeEnvelope is the FIRST pass, and it never fails on version.
func DecodeEnvelope(payload []byte) (RecordEnvelope, error) {
	var env RecordEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return RecordEnvelope{}, fmt.Errorf("search: decode the vector record "+
			"envelope: %w", err)
	}
	if env.V <= 0 {
		return RecordEnvelope{}, fmt.Errorf("search: a vector record carries "+
			"version %d — every record states its version, and one that does "+
			"not cannot be told apart from a newer build's", env.V)
	}
	if err := env.Subject.Validate(); err != nil {
		return RecordEnvelope{}, err
	}
	if env.Scope.Empty() {
		return RecordEnvelope{}, fmt.Errorf("search: the vector record on %s "+
			"declares no scope — an empty scope says the record makes nothing "+
			"stale, which is the one claim a record a build cannot read may "+
			"not make", env.Subject)
	}
	return env, nil
}

// ErrFutureVersion reports a record a newer build wrote.
type ErrFutureVersion struct {
	Got, Want int
	Subject   Subject
}

func (e *ErrFutureVersion) Error() string {
	return fmt.Sprintf("search: the vector record on %s is version %d and this "+
		"build reads %d — it is RETAINED rather than skipped, so a build that "+
		"can read it applies it later", e.Subject, e.Got, e.Want)
}

// Decode is the SECOND pass, and the one that may refuse on version.
//
// It returns the envelope alongside the error whenever the envelope itself
// decoded, because that is precisely the case the applier retains.
func Decode(payload []byte) (VectorRecord, error) {
	env, err := DecodeEnvelope(payload)
	if err != nil {
		return VectorRecord{}, err
	}
	if env.V > RecordVersion {
		return VectorRecord{RecordEnvelope: env}, &ErrFutureVersion{
			Got: env.V, Want: RecordVersion, Subject: env.Subject,
		}
	}
	var rec VectorRecord
	if err := json.Unmarshal(payload, &rec); err != nil {
		return VectorRecord{RecordEnvelope: env}, fmt.Errorf("search: decode "+
			"the vector record on %s: %w", env.Subject, err)
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

// Encode writes a record.
func (r VectorRecord) Encode() ([]byte, error) {
	if r.V == 0 {
		r.V = RecordVersion
	}
	if err := r.Subject.Validate(); err != nil {
		return nil, err
	}
	if !r.Op.Valid() {
		return nil, fmt.Errorf("search: op %q is not one this build writes", r.Op)
	}
	if r.Scope.Empty() {
		r.Scope = statelog.ScopeSet{Paths: []string{ScopePath(r.Container, r.Subject)}}
	}
	if r.Op == OpEmbed {
		if len(r.Embedding) == 0 || r.Model == "" || r.Dim <= 0 {
			return nil, fmt.Errorf("search: an embed record for %s carries no "+
				"vector, model or width — all three are the candidate pool's "+
				"own predicate, and a row missing any of them can never be "+
				"scanned or excluded", r.Subject)
		}
		if want := 4 * r.Dim; len(r.Embedding) != want {
			return nil, fmt.Errorf("search: the embed record for %s says %d "+
				"dimensions and carries %d bytes, not %d — vector_distance_cos "+
				"over mismatched lengths is undefined and fails the whole "+
				"statement", r.Subject, r.Dim, len(r.Embedding), want)
		}
	}
	body, err := json.Marshal(r)
	if err != nil || len(r.Extra) == 0 {
		return body, err
	}

	// A RECORD FROM A NEWER BUILD RE-ENCODES WITH ITS UNKNOWN FIELDS, so a
	// relayed record loses nothing its writer wrote. The map path is taken
	// only when there is something to add, so a record this build wrote is
	// the struct's own bytes.
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(body, &merged); err != nil {
		return nil, fmt.Errorf("search: re-encode a vector record carrying %d "+
			"field(s) this build does not know: %w", len(r.Extra), err)
	}
	for key, value := range r.Extra {
		if _, taken := merged[key]; !taken {
			merged[key] = value
		}
	}
	return json.Marshal(merged)
}
