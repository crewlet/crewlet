package statelog

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// GenerationStride is the sequence space one generation owns in the packed
// form, and therefore the shift [Position.Packed] uses.
//
// 2^40 sequences per generation against 2^23 generations in an int64's
// positive range. The split is not arbitrary: at the modelled 2.4 million
// records a year a generation lasts four hundred thousand years, and a
// generation only advances when an operator reanchors after losing the whole
// broker estate — an event with no plausible count in the thousands. So the
// wide half goes to the number that grows on its own.
const GenerationStride = 1 << 40

// MaxGeneration is the highest generation the packed form can carry, and
// MaxSeq the highest sequence one generation's space holds.
//
// CHECKED WHERE A POSITION IS MINTED rather than on every conversion, by
// [Position.Valid]: a Packed() that returned an error would put an error
// check on every comparison in the tree for a bound a fleet reaches after
// four hundred thousand years, and a call site that ignored it would be worse
// than the bound. The mint points are few and each one calls Valid.
const (
	MaxGeneration = uint32(1<<23 - 1)
	MaxSeq        = uint64(GenerationStride - 1)
)

// ErrPositionRange reports a position the packed form cannot carry.
var ErrPositionRange = errors.New("position outside the packed range")

// ErrWrongStream reports a comparison between positions on two different
// streams.
//
// It is an error rather than a false, because the two answers are not the
// same fact: "this position is not before that one" and "these two positions
// are not comparable at all" lead to different code, and a bool collapses the
// second into the first — which reads as "already caught up" at every call
// site that asks.
var ErrWrongStream = errors.New("positions are on different streams")

// Position is where a domain is on a stream. THE THREE FIELDS ARE ONE VALUE.
//
// # Why a triple and never a bare integer
//
// A sequence alone is meaningless the moment a stream can be recreated. An
// operator who loses the broker estate rebuilds it, and the new stream starts
// at 1 — so every stored sequence is suddenly a number in a space it does not
// belong to, indistinguishable from a current one. The generation is what
// makes an old sequence COMPARABLE AND SAFELY STALE rather than plausible: it
// is a monotone integer that only an audited reanchor advances, and one
// integer comparison answers "is this from before the estate was rebuilt"
// with no clock and no round trip.
//
// The stream name is the third field for the same reason at a different
// scale: a fleet runs more than one domain, each with its own stream and its
// own sequence space, and a position that travelled between them would be a
// number that means something on both.
//
// # One conversion, and one name for it
//
// [Position.Packed] is the SQL-facing form and there is deliberately no
// second name for it. A Version() beside a Packed() is one value with two
// spellings, and two spellings of one value is how a comparison in one place
// stops meaning what it means in another.
//
// # On the wire
//
// A position travels in two forms and both name the same triple. Inside an
// answer it is the object `{stream, generation, seq}` — the keys below, which
// are what the dashboard's protocol types declare and what a reader of
// `seen_through` or `incomplete.from` indexes. Untagged, the encoder used
// Go's own field names, so the dashboard read `from.seq` and got `undefined`
// while the Go side saw the same fields under `Seq` and compiled. As a
// PARAMETER — a cursor, `since`, `min_position` — it is the token
// [Position.String] renders, `<stream>@<generation>:<sequence>`, because a
// query string carries no object.
type Position struct {
	// Stream is the stream this position is on.
	Stream string `json:"stream"`

	// Generation is the generation of THIS STREAM — each domain's log has
	// its own, advanced only by an operator's reanchor of that log — so
	// it is a coordinate within the stream's number space rather than a
	// fact about the node.
	//
	// uint32, NOT uint64: the packed form shifts it by GenerationStride's
	// exponent inside an int64, which bounds it at 2^23 either way. A
	// wider field would promise a range the packed form cannot carry, and
	// the value that overflowed would be silently negative.
	Generation uint32 `json:"generation"`

	// Seq is the broker's own sequence — a PubAck's, an applier's
	// checkpoint, an object's version — in one number space.
	Seq uint64 `json:"seq"`
}

// Packed is the position as one integer: the SQL-facing form, and what every
// durable column stores.
//
// (generation << 40) | seq, which orders lexicographically by (generation,
// seq) for free — so a cursor that spans a reanchor resumes with no gap and
// no repeat, and a column can be compared with a plain `>` in SQL that has no
// idea a generation exists.
//
// The stream is NOT in the packed form. It is the identity of the number
// space rather than a coordinate within it, and two streams' packed positions
// are never compared — [Position.Before] refuses that comparison, which is
// the guard the packed form itself cannot carry.
func (p Position) Packed() int64 {
	return int64(p.Generation)*GenerationStride | int64(p.Seq)
}

// Before reports whether p is strictly before q, and refuses a comparison
// across streams.
//
// GENERATION FIRST, ALWAYS. A sequence from a previous generation is below
// every sequence in the current one however large it is, because they are
// numbers in unrelated spaces — and the whole point of the generation is that
// this comparison is the one place that fact is enforced.
func (p Position) Before(q Position) (bool, error) {
	if p.Stream != q.Stream {
		return false, fmt.Errorf("%w: %q and %q", ErrWrongStream, p.Stream, q.Stream)
	}
	return p.Packed() < q.Packed(), nil
}

// String renders a position the way an operator surface and a log line both
// want it — the token [ParsePosition] reads back.
func (p Position) String() string {
	return fmt.Sprintf("%s@%d:%d", p.Stream, p.Generation, p.Seq)
}

// ParsePosition reads back the `stream@generation:seq` token [Position.String]
// writes — a cursor, a `since`, a `min_position`, a wake's trigger.
//
// BESIDE THE WRITER, because a token is one vocabulary and its reader belongs
// with the rule that writes it: the reader lived in the tracker, so a wake
// carrying a knowledge base's position, or a node reading one back, had to ask
// the tracker how a position is spelled.
//
// THE TRIPLE RATHER THAN A BARE SEQUENCE, because a sequence names no stream
// and no generation: handed one from a previous generation, a reader cannot
// tell a position that is behind from one that is impossibly far ahead — and
// the whole reason the public token carries all three is that it then never
// needs a migration.
func ParsePosition(raw string) (Position, error) {
	refuse := func() (Position, error) {
		return Position{}, fmt.Errorf("statelog: %q is not a log position — one "+
			"reads `<stream>@<generation>:<sequence>`, and the answer that "+
			"produced it carries the value to send back", raw)
	}
	stream, rest, found := strings.Cut(raw, "@")
	if !found || stream == "" {
		return refuse()
	}
	generation, sequence, found := strings.Cut(rest, ":")
	if !found {
		return refuse()
	}
	gen, err := strconv.ParseUint(generation, 10, 32)
	if err != nil {
		return refuse()
	}
	seq, err := strconv.ParseUint(sequence, 10, 64)
	if err != nil {
		return refuse()
	}
	at := Position{Stream: stream, Generation: uint32(gen), Seq: seq}
	if err := at.Valid(); err != nil {
		return Position{}, fmt.Errorf("statelog: %q is not a usable log position: %w", raw, err)
	}
	return at, nil
}

// Valid reports whether this position survives the packed form, and is called
// wherever one is MINTED — a PubAck composed into a position, a generation
// read back from the store — so an out-of-range value is refused at its
// source rather than turning into a negative integer in a durable column.
func (p Position) Valid() error {
	if p.Stream == "" {
		return fmt.Errorf("%w: no stream", ErrPositionRange)
	}
	if p.Generation > MaxGeneration {
		return fmt.Errorf("%w: generation %d is past %d, which is what "+
			"(generation << 40) leaves inside an int64",
			ErrPositionRange, p.Generation, MaxGeneration)
	}
	if p.Seq > MaxSeq {
		return fmt.Errorf("%w: sequence %d is past %d, which is one "+
			"generation's whole space", ErrPositionRange, p.Seq, MaxSeq)
	}
	return nil
}

// IsZero reports a position that names nothing — a domain that has consumed
// no record, or an absent anchor.
func (p Position) IsZero() bool { return p.Generation == 0 && p.Seq == 0 }

// At returns p moved to seq in the same stream and generation. It is how a
// PubAck becomes a position: the broker answers a sequence and the generation
// is the caller's own, never the broker's.
func (p Position) At(seq uint64) Position {
	p.Seq = seq
	return p
}
