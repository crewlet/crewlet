package statelog

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// An operation id carries the instant it was minted, in its own bytes.
//
// # Why the instant has to travel with the id
//
// The publisher asks when an operation was minted, in two places — before it
// publishes a decision, and when it resolves an append whose answer was lost —
// and both times the question is the same: can this node's operation ledger
// vouch for it? It cannot for an operation minted before this node's latest
// adoption of a donated snapshot, because the ledger is scrubbed out of every
// one ([AdoptedAt], [Publisher.vouches]). An operation id is minted ONCE and
// reused by every retry of the operation, including retries in another call,
// another run of the same turn and another process on another node. So "when
// was this minted" is a property of the ID, and anything that states it
// separately states something the retry cannot reproduce.
//
// It was stated separately: a request carried a MintedAt beside its op id, and
// every writer filled it with its own clock at the call. A turn re-run after an
// adoption then carried an op id minted before the adoption with a MintedAt
// after it, the pre-adoption arm could not fire, and the writer re-decided
// against a ledger that no longer held the first application — publishing the
// operation a second time. (Nor was the arm consulted before a decision at
// all, only in the resolution of a lost acknowledgement — which a re-run whose
// append is acknowledged cleanly never reaches.)
//
// # The grammar
//
//	<uuidv7>[.<name>][.<step>...]
//
// The UUIDv7's leading 48 bits are the Unix millisecond the operation was
// minted at, and the instant is recovered from the id itself ([OpMintedAt]),
// so nothing can stamp a later one: not a writer, not a retry, not a caller
// outside the engine that hands an id back. Everything after the uuid is for
// the READER of the ledger and nothing parses it: an optional NAME saying
// what the operation is — its verb and its object, which is what makes a stuck
// operation findable in the ledger rather than a row nobody can trace back —
// and one `.step` per append of a multi-append gesture ([StepOpID]). Three
// constructors mint every id the engine publishes under:
//
//   - [NewOpID] for an operation minted NOW, by the call that publishes it;
//   - [DeriveOpID] for an id a retry must REPRODUCE — a turn's writes, which
//     a re-run must collapse into the first run's — whose instant is the
//     start of the unit of work it is derived from, never the call's;
//   - [StepOpID] for one append of a gesture, which inherits the gesture's.
//
// A derived id is a UUIDv7 whose non-time bits are a digest of what makes the
// operation this operation, rather than random: the same unit of work and the
// same identity produce the same id on every node and in every run, which is
// the idempotency, and the instant is still in the leading bits, which is the
// vouching.
//
// # An id that carries no instant
//
// An id the engine did not mint — a caller's own string, a test's literal —
// has no instant to recover, and it is read as minted at the zero instant:
// before every adoption this node has recorded. The ledger vouches for it only
// on a node that has never adopted a snapshot, which is the one ledger that
// has lost nothing; anywhere else an absent row answers `unknown` rather than
// "somebody else won". That is the only reading under which an id of unknown
// age can never be re-decided across an adoption, and the price — a caller's
// own id cannot be retried to a conclusion on a node that adopted — falls on
// the caller that invented it, never on a colleague's write.
//
// # Whose clock
//
// The instant is read off the clock of whichever node minted the id, and the
// adoption bound off this node's, so a retry that crosses nodes compares two
// clocks. The comparison errs safely in one direction only. A minting clock
// BEHIND this node's makes an operation look older, and answers `unknown`
// where the ledger could have vouched. A minting clock AHEAD of it by δ can
// make an operation whose first copy landed up to δ before a donor finished
// the artefact this node adopted look minted after the adoption, and that one
// is re-decided. So the fleet's wall clocks are assumed to agree to within the
// time between a donor finishing the artefact it offers and this node
// stamping its adoption of it — which the offer window makes seconds in the
// ordinary case and which nothing enforces. It is the same kind of assumption
// the trim's age term already rests on (see the package doc), and it is
// stated here because this is where it is spent.

// opIDLength is the length of the UUID an operation id starts with.
const opIDLength = 36

// opStepSeparator joins an id's uuid to its name and a gesture's id to one
// step's.
const opStepSeparator = "."

// NewOpID mints a fresh operation id at the given instant, named name.
//
// The instant is an ARGUMENT rather than a read of the wall clock, so the one
// reading of the clock is the caller's and a test can pin it. It must be the
// wall clock of the node minting the id: it is compared with this node's
// adoption record, which is read off the wall. The name may be empty, and
// says what the operation is for whoever reads the ledger; see the grammar.
func NewOpID(at time.Time, name string) string {
	var tail [10]byte
	if _, err := rand.Read(tail[:]); err != nil {
		// crypto/rand does not fail on any platform this engine builds
		// for; a digest of a fresh v4 is still unique where it somehow
		// does, and an id is never worth failing a write over.
		sum := sha256.Sum256([]byte(uuid.NewString()))
		copy(tail[:], sum[:])
	}
	return named(layout(at, tail), name)
}

// DeriveOpID derives an operation id a retry must reproduce: the same instant,
// name and identity always produce the same id.
//
// at is the instant the unit of work the id is derived from BEGAN — never the
// instant of the call that derives it, which is what a re-run moves. identity
// is everything that makes the operation this operation, the name included;
// its parts are joined with a separator no part can contain, so ("ab", "c")
// and ("a", "bc") are two operations.
func DeriveOpID(at time.Time, name string, identity ...string) string {
	h := sha256.New()
	for _, part := range append([]string{name}, identity...) {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	var tail [10]byte
	copy(tail[:], h.Sum(nil))
	return named(layout(at, tail), name)
}

// named appends a name to an id, or leaves an unnamed one bare.
func named(id uuid.UUID, name string) string {
	if name == "" {
		return id.String()
	}
	return id.String() + opStepSeparator + name
}

// StepOpID is the operation id of one append of a multi-append gesture, the
// steps naming it from the outermost in: a gesture that publishes one record
// per log under each of two signs is StepOpID(gesture, sign, log).
//
// STABLE AND DISTINCT from the gesture's own and from every other step's, and
// both halves matter: the ledger records one RECORD, so a shared id would make
// the second append's resolution read the first's row; and a fresh id per
// attempt would defeat the ledger for the lost-acknowledgement case it exists
// for. It INHERITS the gesture's instant, because every step of the gesture
// was minted when the gesture was.
func StepOpID(opID string, steps ...string) string {
	for _, step := range steps {
		opID += opStepSeparator + step
	}
	return opID
}

// OpID is the operation id of the generation record a reanchor to f appends.
//
// DERIVED, from the new generation and the stream it adopts, so two operators
// who derive the same generation from the same stream name the same operation
// and contend for one record rather than landing two. Its instant is the
// adopted stream's creation — the earliest moment the transition could exist,
// and a value every operator reads identically off the broker, which a
// reanchor's own clock is not.
func (f GenerationFacts) OpID() string {
	return DeriveOpID(f.Inputs.StreamCreatedAt,
		"reanchor-"+strconv.FormatUint(uint64(f.Generation), 10),
		"crewlet.statelog.reanchor",
		strconv.FormatUint(uint64(f.Generation), 10),
		strconv.FormatInt(f.Inputs.StreamCreatedAt.UTC().UnixNano(), 10))
}

// OpMintedAt recovers the instant an operation id was minted at, reporting
// false for an id that carries none — see the file head for how such an id is
// read.
func OpMintedAt(opID string) (time.Time, bool) {
	if len(opID) < opIDLength {
		return time.Time{}, false
	}
	head, rest := opID[:opIDLength], opID[opIDLength:]
	if rest != "" && !strings.HasPrefix(rest, opStepSeparator) {
		return time.Time{}, false
	}
	id, err := uuid.Parse(head)
	if err != nil || id.Version() != 7 || id.Variant() != uuid.RFC4122 {
		return time.Time{}, false
	}
	var ms [8]byte
	copy(ms[2:], id[:6])
	return time.UnixMilli(int64(binary.BigEndian.Uint64(ms[:]))).UTC(), true
}

// mintedAt is the instant the publisher reads an operation as minted at: the
// one its id carries, or the zero instant for an id that carries none.
func mintedAt(opID string) time.Time {
	at, _ := OpMintedAt(opID)
	return at
}

// layout writes the UUIDv7 bit layout over an instant and ten bytes of tail.
//
// The instant is TRUNCATED to the millisecond, which is the direction that
// keeps the recovered instant at or before the real one, and clamped to the 48
// bits the layout holds: before the Unix epoch — the zero time.Time among
// them — reads as the epoch itself, which is before every adoption, the
// conservative end.
func layout(at time.Time, tail [10]byte) uuid.UUID {
	ms := at.UnixMilli()
	switch {
	case ms < 0:
		ms = 0
	case ms > 1<<48-1:
		ms = 1<<48 - 1
	}
	var id uuid.UUID
	var stamp [8]byte
	binary.BigEndian.PutUint64(stamp[:], uint64(ms))
	copy(id[:6], stamp[2:])
	copy(id[6:], tail[:])
	id[6] = 0x70 | id[6]&0x0f // version 7
	id[8] = 0x80 | id[8]&0x3f // the RFC 9562 variant
	return id
}
