package statelog

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// An operation id carries the instant it was minted, in its own bytes
// (package doc, "An operation id carries the instant it was minted"): every
// retry reuses the id, so an instant stated beside it would be the call's,
// later than any ledger loss, and [Publisher.vouches] could never refuse.
//
// The grammar is `<uuidv7>[.<name>][.<step>...]`. The UUIDv7's leading 48 bits
// are the Unix millisecond of the mint ([OpMintedAt]); nothing here parses the
// rest: an optional NAME (verb and object, so a stuck operation is findable in
// the ledger; a surface may hold a brought-back id to it, [OpNamed]) and one
// `.step` per append of a gesture. [NewOpID] mints now; [DeriveOpID] (and
// [KeyedOpID] over a caller's key) reproduces an id from the start of its unit
// of work, its non-time bits a digest; [StepOpID] inherits a gesture's.

// A name and each step are ONE SEGMENT: visible ASCII, no dot (the separator
// that begins a step), at most [MaxOpSegmentBytes], not beginning with `~`.
// Every constructor writes segments through that rule ([opPart]), because
// nothing downstream holds it: a dot reads as a step, and a long or spaced
// value breaks [CheckCallerOpID] and the broker's message-id header. A value
// that breaks it is ESCAPED, `~` and the first 16 bytes of its SHA-256 in hex,
// not refused: an id is never worth failing a write over, and the escape is
// deterministic, so a derived id still reproduces. INJECTIVE: a plain segment
// never begins with `~` and an escape always does. A value composed into a
// segment goes through [OpNamePart] first, at the narrower
// [MaxOpNamePartBytes], so it is escaped alone and the verb stays readable.

// An id the engine did not mint (a caller's string, a test's literal) carries
// no instant and is read as minted at the zero instant, before every loss: the
// ledger vouches for it only where it never lost a row and answers `unknown`
// elsewhere, so an id of unknown age is never re-decided across a loss, and
// the price falls on the caller that invented it.
//
// The compacted domains (vectors `vec-<hex>`, usage days `usage-<hex>`) mint
// outside the grammar, a digest of object and value: with no ledger
// ([Domain.OpsTable] empty) [Publisher.vouches] answers true, and the
// compacted apply lands a repeated value once. A domain that GAINS a ledger
// takes its ids into the grammar in the same change, or every write it makes
// reads as minted before every loss.

// Whose clock: the instant is the minting node's, the watermark the sweeping
// node's (this one, or a donor's that travelled with its ledger), so a retry
// across nodes compares two clocks. A minting clock BEHIND only answers
// `unknown` where the ledger could have vouched; one AHEAD by δ can re-decide
// an operation lost up to δ before the watermark. So the fleet's wall clocks
// are assumed to agree within the sweep's horizon: thirty days for the table,
// or a subject kind's shorter one (an hour for the identity estate's sessions,
// whose writers never re-ask late; internal/maintenance's OpsHorizon.Kinds),
// the same kind of assumption the trim's age term rests on (package doc).

// opIDLength is the length of the UUID an operation id starts with.
const opIDLength = 36

// opStepSeparator joins an id's uuid to its name and a gesture's id to one
// step's.
const opStepSeparator = "."

// MaxOpSegmentBytes bounds one segment of an operation id (its name, or one
// step): 91 bytes, what [MaxCallerOpIDBytes] leaves after the uuid and the
// dot, so a named id the engine answers with can always be sent back.
const MaxOpSegmentBytes = MaxCallerOpIDBytes - opIDLength - len(opStepSeparator)

// MaxOpNamePartBytes bounds a value [OpNamePart] writes as itself: 64 bytes,
// the widest identifier the engine writes into a name (a seat handle, login,
// machine handle or node id; a uuid is 36), so each stays readable in the
// ledger and a verb plus the widest part still fit one segment
// ([MaxOpSegmentBytes]): `file-remove-` and 64 bytes is 76. Anything longer is
// a path or prose, which is what the escape is for.
const MaxOpNamePartBytes = 64

// opEscape begins every escaped segment, and no segment written as itself.
const opEscape = "~"

// opEscapeDigestBytes is how much of an escaped value's SHA-256 its segment
// carries: 128 bits, which no two values a ledger will ever hold share.
const opEscapeDigestBytes = 16

// NewOpID mints a fresh operation id at the given instant, named name (which
// may be empty). The instant is an argument so the caller owns the one clock
// read and a test can pin it; it must be the minting node's wall clock, since
// it is compared with the ledger's watermark.
func NewOpID(at time.Time, name string) string {
	var tail [10]byte
	if _, err := rand.Read(tail[:]); err != nil {
		// crypto/rand does not fail where this engine builds; if it does,
		// a fresh v4's digest is still unique, and an id is never worth
		// failing a write over.
		sum := sha256.Sum256([]byte(uuid.NewString()))
		copy(tail[:], sum[:])
	}
	return named(layout(at, tail), name)
}

// DeriveOpID derives an operation id a retry must reproduce: the same instant,
// name and identity always produce the same id. at is when the unit of work
// BEGAN, never the deriving call's instant, which a re-run moves. Each part is
// NUL-terminated, so ("ab", "c") and ("a", "bc") are two operations.
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

// KeyedOpID is the operation id of one write made under a caller's operation
// KEY (a turn's call key, a request's Idempotency-Key): [DeriveOpID], named by
// the verb, at the key's own instant, over the key, the verb, the object and a
// digest of content's JSON encoding. One rule for every keyed domain, each part
// buying a failure: without the verb and object a gesture's save and rename
// collapse into one; without the content the same key sent with another body
// is answered from the ledger ([Result.Collapsed]) and silently dropped;
// without the key's instant the id reads as minted before every loss and is
// answered `unknown` forever, so a key with no instant, empty included, is
// REFUSED ([CheckCallerOpID] exists for this; a keyless write mints [NewOpID]
// in its store). The parts are what the caller ASKED for, never read from
// rows, which a first attempt may have moved before its retry.
func KeyedOpID(key, verb, object string, content any) (string, error) {
	return keyedOpID(key, verb, verb, object, content)
}

// KeyedObjectOpID is [KeyedOpID] for a write whose operation id is ALSO the id
// of the object it makes (a page's comment, a chat message), and so UNNAMED:
// readers address the object by it, and one identifier per thing leaves a
// retry nothing to disagree with itself over. The verb stays in the identity,
// so it never equals the named id under the same inputs.
func KeyedObjectOpID(key, verb, object string, content any) (string, error) {
	return keyedOpID(key, "", verb, object, content)
}

// keyedOpID is the derivation behind [KeyedOpID] and [KeyedObjectOpID], under
// name. The verb must be a segment AS ITSELF, never escaped: a domain declares
// it and a reader finds the operation by it, so one the grammar would escape
// is a domain's mistake.
func keyedOpID(key, name, verb, object string, content any) (string, error) {
	if !isOpPart(verb, MaxOpSegmentBytes) {
		return "", fmt.Errorf("statelog: a keyed write's verb is a name, and %q "+
			"is not one: a name is up to %d bytes of visible ASCII with no %q, "+
			"which begins a gesture's step, and does not begin with %q, which "+
			"marks an escaped value", verb, MaxOpSegmentBytes, opStepSeparator, opEscape)
	}
	at, minted := OpMintedAt(key)
	if !minted {
		return "", fmt.Errorf("statelog: the %s's key %q carries no instant, so "+
			"an id derived from it would read as minted before every loss the "+
			"ledger has had — a key is an operation id this engine minted "+
			"(CheckCallerOpID)", verb, key)
	}
	said, err := json.Marshal(content)
	if err != nil {
		return "", fmt.Errorf("statelog: digest what a %s says: %w", verb, err)
	}
	sum := sha256.Sum256(said)
	return DeriveOpID(at, name, verb, key, object, hex.EncodeToString(sum[:])), nil
}

// named appends a name to an id as one segment ([opSegment]), or leaves an
// unnamed one bare.
func named(id uuid.UUID, name string) string {
	if name == "" {
		return id.String()
	}
	return id.String() + opStepSeparator + opSegment(name)
}

// OpNamePart is how a VALUE (a task's id, a node's, a file's path) goes into
// an operation id's name or step: as itself where it is a segment of at most
// [MaxOpNamePartBytes] bytes, escaped otherwise, so `"file-" + OpNamePart(path)`
// is one readable segment. The identity on an engine-minted identifier, so the
// ledger shows it as itself; deterministic, so a derived id reproduces.
func OpNamePart(value string) string {
	return opPart(value, MaxOpNamePartBytes)
}

// opSegment is s as the grammar writes it into an id: itself where it is a
// segment, escaped otherwise.
func opSegment(s string) string {
	return opPart(s, MaxOpSegmentBytes)
}

// opPart is s written as itself where it is a part of at most limit bytes
// ([isOpPart]), and escaped otherwise.
func opPart(s string, limit int) string {
	if isOpPart(s, limit) {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	return opEscape + hex.EncodeToString(sum[:opEscapeDigestBytes])
}

// isOpPart reports whether s may be written into an id as itself: one to limit
// bytes of visible ASCII, none of them the step separator, and not beginning
// with the escape marker — which is what keeps a value written as itself from
// ever equalling another's escape.
func isOpPart(s string, limit int) bool {
	if s == "" || len(s) > limit || strings.HasPrefix(s, opEscape) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= ' ' || c > '~' || c == opStepSeparator[0] {
			return false
		}
	}
	return true
}

// OpNamed reports whether opID was minted under exactly name: a uuid, the
// separator and name as [opSegment] writes it (so an escaped name still
// matches), with no step after. It is the one test a surface that names the
// call in the id (the operator's tracker tools) holds a brought-back id to, so
// no copy lets a step's id pass and derive a NEW call's objects under the old
// call's identity. Not a credential and blind to the instant
// ([CheckCallerOpID] is that rule): it catches an id carried to another call
// by accident, making a second gesture answer as the first.
func OpNamed(opID, name string) bool {
	if name == "" {
		return false
	}
	want := opSegment(name)
	return len(opID) == opIDLength+len(opStepSeparator)+len(want) &&
		opID[opIDLength:opIDLength+len(opStepSeparator)] == opStepSeparator &&
		opID[opIDLength+len(opStepSeparator):] == want
}

// StepOpID is the operation id of one append of a multi-append gesture, the
// steps naming it from the outermost in: a gesture that publishes one record
// per log under each of two signs is StepOpID(gesture, sign, log).
//
// STABLE AND DISTINCT from the gesture's id and every other step's: the
// ledger records one RECORD per id, so a shared id would resolve one append by
// another's row, and a fresh id per attempt would defeat the ledger for a
// lost acknowledgement. It INHERITS the gesture's instant. Each step is one
// segment ([opSegment]), so `x` with step `y.z` never reads as `x.y` with `z`.
func StepOpID(opID string, steps ...string) string {
	for _, step := range steps {
		opID += opStepSeparator + opSegment(step)
	}
	return opID
}

// OpID is the operation id of the generation record a reanchor to f appends.
//
// DERIVED, so a re-run of an interrupted reanchor on one node reproduces its
// own earlier id, at the adopted stream's creation: the earliest instant the
// transition could exist, read identically off the broker by every node, as a
// reanchor's own clock is not. THE WRITER IS IN THE IDENTITY, because two
// nodes deriving one generation are two operations: a clustered broker checks
// its duplicate window before the append's expectation, so a shared id would
// acknowledge the second as a duplicate of the first's record, and that node
// would open the generation over its own rows; with the writer in it the
// second is refused instead. [Reanchor] reads back whose record landed either
// way, so it does not rest on this alone.
func (f GenerationFacts) OpID() string {
	return DeriveOpID(f.Inputs.StreamCreatedAt,
		"reanchor-"+strconv.FormatUint(uint64(f.Generation), 10),
		"crewlet.statelog.reanchor",
		strconv.FormatUint(uint64(f.Generation), 10),
		strconv.FormatInt(f.Inputs.StreamCreatedAt.UTC().UnixNano(), 10),
		f.Writer)
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

// CheckCallerOpID is the one rule an operation id a caller brings BACK (the
// gate and purge routes' `?op_id=`, the operator tools' `op_id`) is held to:
// an id this engine minted, of at most [MaxCallerOpIDBytes] bytes of visible
// ASCII with no space. REFUSED rather than cleaned, since a retry must name
// the id the caller holds byte for byte; one rule, so no surface accepts what
// another refuses. MINTED, because an id with no instant reads as older than
// every ledger loss ([OpMintedAt]) and is answered `unknown` on every attempt,
// never running and never saying why; a client minting its own (the command
// line, the dashboard) uses this layout. NO SPACE OR CONTROL BYTE, because the
// whole id travels as the broker's message-id header, whose client trims its
// ends and turns a line break into a space: the broker would deduplicate it as
// another id than the one every ledger answers for.
func CheckCallerOpID(opID string) error {
	if len(opID) > MaxCallerOpIDBytes {
		return &CallerOpIDError{Fault: fmt.Sprintf("is %d bytes, and an operation id is at most %d",
			len(opID), MaxCallerOpIDBytes)}
	}
	for i := 0; i < len(opID); i++ {
		if c := opID[i]; c <= ' ' || c > '~' {
			return &CallerOpIDError{Fault: fmt.Sprintf("holds %q at byte %d, and an "+
				"operation id is visible ASCII with no space", c, i)}
		}
	}
	if _, minted := OpMintedAt(opID); !minted {
		return &CallerOpIDError{Fault: "is not an operation id this engine accepts, " +
			"which is a UUIDv7, optionally followed by a dot and a name"}
	}
	return nil
}

// CallerOpIDError is an operation id [CheckCallerOpID] refused. Fault says
// what is wrong with it, as a clause about the id ("is 140 bytes, and an
// operation id is at most 128"), so a surface can name the id the way its
// caller sent it (a tool's `op_id`, a route's Idempotency-Key header) and say
// the remedy that holds there: only a surface whose caller may leave the id
// out can tell them to.
type CallerOpIDError struct {
	Fault string
}

// Error is the refusal as a tool argument's caller reads it: such a caller may
// always leave the id out to start the operation afresh.
func (e *CallerOpIDError) Error() string {
	return "op_id " + e.Fault + ": send back the op_id an earlier answer returned, " +
		"unchanged, or omit it to start the operation afresh"
}

// MaxCallerOpIDBytes bounds an operation id a caller brings back: 128 bytes,
// the longest id a surface mints rounded up to a power of two (the uuid, a dot
// and `readmit-` with a 64-byte node id is 109), and names are held to what it
// leaves ([MaxOpSegmentBytes]), so every id an answer carried fits. No more,
// because a gesture appends steps to it ([StepOpID]), and every record, ledger
// row and message-id header of the gesture carries the result.
const MaxCallerOpIDBytes = 128

// mintedAt is the instant the publisher reads an operation as minted at: the
// one its id carries, or the zero instant for an id that carries none.
func mintedAt(opID string) time.Time {
	at, _ := OpMintedAt(opID)
	return at
}

// layout writes the UUIDv7 bit layout over an instant and ten bytes of tail.
// The instant is TRUNCATED to the millisecond, keeping the recovered instant
// at or before the real one, and clamped to 48 bits: anything before the Unix
// epoch, the zero time.Time included, reads as the epoch, the conservative end.
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
