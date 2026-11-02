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

// An operation id carries the instant it was minted, in its own bytes.
//
// # Why the instant has to travel with the id
//
// The publisher asks when an operation was minted, in two places — before it
// publishes a decision, and when it resolves an append whose answer was lost —
// and both times the question is the same: can this node's operation ledger
// vouch for it? It cannot for an operation minted before the ledger's own
// watermark — the instant before which it may have lost rows to its retention
// sweep ([Rows.LostBefore]) — see [Publisher.vouches]. An operation id is
// minted ONCE and reused by every retry of the operation, including retries in
// another call, another run of the same turn and another process on another
// node. So "when was this minted" is a property of the ID, and anything that
// states it separately states something the retry cannot reproduce.
//
// It was stated separately: a request carried a MintedAt beside its op id, and
// every writer filled it with its own clock at the call. A turn re-run after
// the ledger lost rows then carried an op id minted before the loss with a
// MintedAt after it, the check could not fire, and the writer re-decided
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
// the READER of the ledger and nothing in this package parses it: an optional
// NAME saying what the operation is — its verb and its object, which is what
// makes a stuck operation findable in the ledger rather than a row nobody can
// trace back — and one `.step` per append of a multi-append gesture
// ([StepOpID]). A surface that answers ids to callers may put in the name what
// it needs to recognise the call an id was minted for, and hold a brought-back
// id to it ([OpNamed]) — the operator's tracker tools carry the call's tool
// and a digest of its arguments there, and refuse the id with any other call.
// Three constructors mint every id the engine publishes under:
//
//   - [NewOpID] for an operation minted NOW, by the call that publishes it;
//   - [DeriveOpID] for an id a retry must REPRODUCE — a turn's writes, which
//     a re-run must collapse into the first run's — whose instant is the
//     start of the unit of work it is derived from, never the call's; a
//     write made under a caller's own key derives through [KeyedOpID], which
//     is this over the key at the key's instant;
//   - [StepOpID] for one append of a gesture, which inherits the gesture's.
//
// A derived id is a UUIDv7 whose non-time bits are a digest of what makes the
// operation this operation, rather than random: the same unit of work and the
// same identity produce the same id on every node and in every run, which is
// the idempotency, and the instant is still in the leading bits, which is the
// vouching.
//
// # What a name and a step may hold
//
// The name and every step are each ONE SEGMENT of the id: visible ASCII with
// no dot — the separator that begins the next step — at most
// [MaxOpSegmentBytes], and not beginning with `~`, which marks an escaped
// value. Every constructor writes every segment through that rule, and a
// segment that breaks it is written ESCAPED rather than refused: `~` and the
// first sixteen bytes of its SHA-256 in hex.
//
// HELD AT THE MINT, because nothing downstream holds it. A dot inside a name
// made a file write's `<uuid>.file-ENG/docs/a.md` read as the name
// `file-ENG/docs/a` with a step `md`; a node id that holds dots spelled steps
// of its own in a gate's id; and a value past the bound or carrying a space —
// a kilobyte of path, prose — put into every record, ledger row and
// message-id header an id the engine's own rule for a caller's id refuses
// ([CheckCallerOpID]), in a header whose client trims its ends and rewrites a
// line break. Escaped rather than refused because an id is never worth
// failing a write over, and because the escape is a function of the value: a
// derived id still reproduces, byte for byte.
//
// INJECTIVE, so two values never spell one segment: a segment written as
// itself never begins with `~` and an escaped one always does, so no value
// can be written as another's escape, and two escapes meet only where two
// values share the first 128 bits of a SHA-256.
//
// A VALUE A CALLER COMPOSES INTO A SEGMENT — the object of `update-<task>`,
// the node of `evict-<node>` — goes through [OpNamePart] first, by the same
// rule at a narrower bound ([MaxOpNamePartBytes]), so a value that has to be
// escaped is escaped alone and the verb beside it stays readable.
//
// # An id that carries no instant
//
// An id the engine did not mint — a caller's own string, a test's literal —
// has no instant to recover, and it is read as minted at the zero instant:
// before every loss the ledger has recorded. The ledger vouches for it only
// where it has never lost a row; anywhere else an absent row answers
// `unknown` rather than "somebody else won". That is the only reading under which an id of unknown
// age can never be re-decided across a loss, and the price — a caller's own
// id cannot be retried to a conclusion on a node whose ledger has lost rows —
// falls on the caller that invented it, never on a colleague's write.
//
// # A domain with no ledger mints outside the grammar
//
// The two compacted domains — the vectors (`vec-<hex>`) and each data node's
// usage days (`usage-<hex>`) — publish under ids that are a digest of the object and
// the value written, and carry no instant. That is not the caller's own
// string the paragraph above prices: neither domain keeps an operation ledger
// ([Domain.OpsTable] is empty), so there is no watermark for an instant to be
// compared with and [Publisher.vouches] answers true before it would read one.
// What makes a retry of theirs safe is the compacted apply itself — a subject's
// current value replaced under a monotone position, so the same value written
// twice lands as one — and a grammar whose only purpose is vouching would buy
// them nothing. A domain that GAINS a ledger takes its ids into the grammar
// in the same change, or every write it makes reads as minted before every
// loss that ledger records.
//
// # Whose clock
//
// The instant is read off the clock of whichever node minted the id, and the
// watermark off the clock of the node whose sweep lost the rows — this node's,
// or a donor's whose watermark travelled with its ledger — so a retry that
// crosses nodes compares two clocks. The comparison errs safely in one
// direction only. A minting clock BEHIND the other makes an operation look
// older, and answers `unknown` where the ledger could have vouched. A minting
// clock AHEAD of it by δ can make an operation whose row was lost up to δ
// before the watermark look minted after it, and that one is re-decided. So the
// fleet's wall clocks are assumed to agree to within the margin the sweep
// leaves, which is its horizon: the whole thirty days for the table's own
// sweep, or a subject kind's shorter one where a domain declares it (an hour
// for the identity estate's sessions, whose writers never re-ask late —
// internal/maintenance's OpsHorizon.Kinds). It is the same kind of assumption
// the trim's age term already rests on (see the package doc), and it is stated
// here because this is where it is spent.

// opIDLength is the length of the UUID an operation id starts with.
const opIDLength = 36

// opStepSeparator joins an id's uuid to its name and a gesture's id to one
// step's.
const opStepSeparator = "."

// MaxOpSegmentBytes bounds one segment of an operation id — its name, or one
// step — as the grammar writes it ([OpNamePart] has the rule).
//
// NINETY-ONE BYTES, which is what [MaxCallerOpIDBytes] leaves after the uuid
// and the dot, so a named id the engine answers with is always one a caller
// can send back.
const MaxOpSegmentBytes = MaxCallerOpIDBytes - opIDLength - len(opStepSeparator)

// MaxOpNamePartBytes bounds a value [OpNamePart] writes as itself.
//
// SIXTY-FOUR BYTES, the widest identifier the engine writes into a name — a
// seat handle, a login, a machine handle and a node id all stop there, and a
// uuid is thirty-six — so every identifier stays readable in the ledger, and
// a verb, a dash and the widest part still make one segment
// ([MaxOpSegmentBytes]): `file-remove-` and sixty-four bytes is seventy-six.
// Anything longer is a path or prose, which is what the escape is for.
const MaxOpNamePartBytes = 64

// opEscape begins every escaped segment, and no segment written as itself.
const opEscape = "~"

// opEscapeDigestBytes is how much of an escaped value's SHA-256 its segment
// carries: 128 bits, which no two values a ledger will ever hold share.
const opEscapeDigestBytes = 16

// NewOpID mints a fresh operation id at the given instant, named name.
//
// The instant is an ARGUMENT rather than a read of the wall clock, so the one
// reading of the clock is the caller's and a test can pin it. It must be the
// wall clock of the node minting the id: it is compared with the ledger's
// watermark, which is read off the wall. The name may be empty, and
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

// KeyedOpID is the operation id of one write made under a caller's own
// operation KEY — the value a retry of the same gesture reproduces: a turn's
// call key, or the Idempotency-Key a person's request carried. It is
// [DeriveOpID] over the key, the verb, the object the write is about and a
// digest of what it says, at the key's own instant, and NAMED by the verb.
//
// ONE RULE FOR EVERY DOMAIN THAT TAKES A KEY, because what makes two keyed
// writes one operation is a rule, and two copies of a rule are two answers to
// it. Each clause buys a failure:
//
//   - THE VERB AND THE OBJECT ARE IN THE IDENTITY, so one key covers every
//     record one gesture writes — a save and the rename that follows it are
//     two records, and one id for both would have the ledger collapse the
//     second as a redelivery of the first. The verb is the id's NAME too,
//     which is what a reader of the ledger finds it by, so it is one segment
//     as itself — never escaped, and never holding the dot that begins a
//     gesture's STEP ([StepOpID]).
//   - WHAT THE WRITE SAYS IS IN IT: content is the write's own input, digested
//     whole through its JSON encoding, which writes a struct's fields in one
//     order and a map's keys sorted. The ledger answers an operation it
//     already holds BEFORE the write is decided ([Result.Collapsed]), so an id
//     derived from the key, the verb and the object alone made the same key
//     sent with another body, title or reason the first write's operation:
//     answered from the ledger with nothing of the second written — a change
//     reported as made and silently dropped. A retry resends the same input,
//     so it is still the same operation.
//   - IT CARRIES THE KEY'S OWN INSTANT ([OpMintedAt]). The ledger vouches for
//     a retry by the instant its id carries, so a derivation that dropped it
//     would be read as minted before every loss the ledger ever had, and once
//     it had swept anything such a write was answered `unknown` without being
//     published, on every attempt. A key that carries no instant is therefore
//     REFUSED here rather than derived from: every surface that takes one
//     holds it to [CheckCallerOpID], and this is the promise that check
//     exists for.
//
// The parts are what the caller ASKED for, never anything read from rows: a
// retry arrives after its first attempt may have moved them, and a value read
// at call time would differ between a request and its own retry.
//
// A write with no key has nothing to derive from and mints a fresh id the way
// its store mints any other ([NewOpID]); that choice is the store's, so an
// empty key is refused here like any other key without an instant. An error
// otherwise is a verb that is not a name, or content that will not encode —
// a type its domain declared wrongly rather than anything a caller sent.
func KeyedOpID(key, verb, object string, content any) (string, error) {
	return keyedOpID(key, verb, verb, object, content)
}

// KeyedObjectOpID is [KeyedOpID] for a write whose operation id is ALSO the id
// of the object it makes — a page's comment, a chat message — and so is
// UNNAMED: a reader addresses the object by it, and the gesture and the call
// it came from are on the object's row. One object is one operation there, and
// two identifiers for one thing are two places for a retry to disagree with
// itself.
//
// THE SAME DERIVATION, the verb still in the identity, so an object id and a
// named operation id under one key, verb, object and content are two ids.
func KeyedObjectOpID(key, verb, object string, content any) (string, error) {
	return keyedOpID(key, "", verb, object, content)
}

// keyedOpID is the one derivation behind [KeyedOpID] and [KeyedObjectOpID],
// under name.
//
// The verb is held to a segment AS ITSELF rather than escaped, because it is a
// word a domain declares and a reader finds the operation by: a verb the
// grammar would escape is a domain's mistake, never a caller's input.
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

// OpNamePart is how a VALUE goes into an operation id's name or one of its
// steps — a task's id, a node's, a file's path: as itself where it already is
// a segment of at most [MaxOpNamePartBytes] bytes, and escaped otherwise (see
// the grammar), so `"file-" + OpNamePart(path)` is one readable segment
// whatever the path holds.
//
// The IDENTITY on a value that is already a part, so an identifier the
// engine mints — a uuid, a project key, a handle — reads in the ledger as
// itself; and DETERMINISTIC, so an id derived from a name built with it
// reproduces on every node and in every run.
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
// separator and that name as the grammar writes it ([opSegment]), with no step
// after it ([NewOpID]'s grammar). Compared through the same rule the mint
// writes by, so a name the mint had to escape still names its own id.
//
// THE ONE TEST A SURFACE HOLDS A BROUGHT-BACK ID TO, for a surface that puts
// in an id's name what it needs to recognise the call the id was minted for —
// the operator's tracker tools. Written once, because each copy was one more
// place an id with a step after it could pass: a step's id handed back would
// be a NEW call's operation, deriving new objects under the old call's
// identity.
//
// It is not a credential and decides nothing about the instant: a caller can
// mint any id that carries one ([CheckCallerOpID] is that rule). What it
// catches is an id carried to another call by accident, which is the one way
// an `op_id` can make a second gesture answer as the first.
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
// STABLE AND DISTINCT from the gesture's own and from every other step's, and
// both halves matter: the ledger records one RECORD, so a shared id would make
// the second append's resolution read the first's row; and a fresh id per
// attempt would defeat the ledger for the lost-acknowledgement case it exists
// for. It INHERITS the gesture's instant, because every step of the gesture
// was minted when the gesture was.
//
// Every step is ONE SEGMENT, written through the grammar's rule ([opSegment]),
// so reading a stepped id from the right recovers each step: a step that held
// a dot would have spelled two, and a gesture `x` with the step `y.z` would
// have been the gesture `x.y` with the step `z`.
func StepOpID(opID string, steps ...string) string {
	for _, step := range steps {
		opID += opStepSeparator + opSegment(step)
	}
	return opID
}

// OpID is the operation id of the generation record a reanchor to f appends.
//
// DERIVED, from the new generation, the stream it adopts and the node that
// writes it, so a re-run of an interrupted reanchor on one node reproduces the
// id of its own earlier attempt. Its instant is the adopted stream's creation —
// the earliest moment the transition could exist, and a value every operator
// reads identically off the broker, which a reanchor's own clock is not.
//
// THE WRITER IS PART OF THE IDENTITY, because two nodes that derive the same
// generation are two operations, not one retried. The subject holds one record
// either way — the append expects nothing on it — but a clustered broker checks
// its duplicate window before that expectation, so one id between two nodes
// had the second acknowledged as a duplicate of the first's record, as though
// its own had landed, and it opened the generation over its own rows too. With
// the writer in the id the second is refused on the subject instead; and the
// transition reads back whose record landed whichever answer it got
// ([Reanchor]), so it does not rest on this.
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

// CheckCallerOpID is the one rule an operation id a caller brings BACK is held
// to — an id a route or a tool answered with, sent again to finish the
// operation it names: an id this engine minted, of at most
// [MaxCallerOpIDBytes] bytes of visible ASCII with no space. It is REFUSED
// rather than cleaned, because a retry has to name the id the caller holds,
// byte for byte — and each clause buys a failure that nothing downstream
// reports.
//
// ONE RULE FOR EVERY SURFACE THAT TAKES ONE — the gate and purge routes'
// `?op_id=` and the operator tools' `op_id` argument — because an id one of
// them accepts and another refuses is an operation finishable from one place
// and not the other, and written twice the rule would drift.
//
// MINTED BY THE ENGINE, because the id carries the instant it was minted and
// the publisher reads it to decide whether its ledger can vouch for the retry
// ([OpMintedAt]). One with no instant is read as older than every loss the
// ledger has had — a retention sweep that deleted anything, which every
// deployment older than the ledger's retention has had — and such a write is
// answered `unknown` without being published, on the first attempt as on every
// retry: an operation that can never run and never says why. A client that
// mints its own — the command line, the dashboard — mints through this
// grammar's layout, and is held to it here like any other.
//
// VISIBLE ASCII WITH NO SPACE, because only the id's first thirty-six bytes
// are the minted uuid and this check reads nothing after them — though a
// surface may: the operator tools read the name after the uuid to hold an id
// to the call it was minted for — while the whole id travels as the broker's
// message-id header, and the client writing that header trims its ends and
// turns a line break into a space. An id carrying either was deduplicated at
// the broker as another id than the one every ledger answers for.
func CheckCallerOpID(opID string) error {
	if len(opID) > MaxCallerOpIDBytes {
		return &CallerOpIDError{Fault: fmt.Sprintf("is %d bytes, and an operation id is at most %d",
			len(opID), MaxCallerOpIDBytes)}
	}
	for i := 0; i < len(opID); i++ {
		if c := opID[i]; c <= ' ' || c > '~' {
			// The broker trims the ends of the header the id travels in and
			// rewrites a line break, so it would carry another id than the one
			// every log answers for.
			return &CallerOpIDError{Fault: fmt.Sprintf("holds %q at byte %d, and an "+
				"operation id is visible ASCII with no space", c, i)}
		}
	}
	if _, minted := OpMintedAt(opID); !minted {
		// It carries no instant, so no node could tell whether it already ran.
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

// MaxCallerOpIDBytes bounds an operation id a caller brings back.
//
// A HUNDRED AND TWENTY-EIGHT BYTES, the longest id a surface mints rounded up
// to a power of two: a gesture's id is a thirty-six-byte uuid, a dot and a name
// ([NewOpID]) — `purge-` and a task's uuid is seventy-nine, and `readmit-` and
// a node id at the sixty-four bytes one may be is a hundred and nine — and a
// name is held to what this leaves after the uuid and the dot
// ([MaxOpSegmentBytes]), so every id an answer ever carried fits. And no more,
// because a gesture derives each log's own id from the caller's by appending
// steps ([StepOpID]), and every record, ledger row and message-id header of
// the gesture carries the result.
const MaxCallerOpIDBytes = 128

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
