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
// positive range: at the modelled 2.4 million records a year a generation
// lasts four hundred thousand years, and a generation advances only when an
// operator reanchors after losing the whole broker estate. So the wide half
// goes to the number that grows on its own.
const GenerationStride = 1 << 40

// MaxGeneration is the highest generation the packed form can carry, and
// MaxSeq the highest sequence one generation's space holds.
//
// CHECKED WHERE A POSITION IS MINTED, by [Position.Valid], not on every
// conversion: an error from Packed() would put a check on every comparison in
// the tree for a bound a fleet reaches after four hundred thousand years.
const (
	MaxGeneration = uint32(1<<23 - 1)
	MaxSeq        = uint64(GenerationStride - 1)
)

// ErrPositionRange reports a position the packed form cannot carry.
var ErrPositionRange = errors.New("position outside the packed range")

// ErrWrongStream reports a comparison between positions on two different
// streams. An error rather than a false: a bool collapses "not comparable"
// into "not before", which every caller reads as "already caught up".
var ErrWrongStream = errors.New("positions are on different streams")

// ErrForeignPosition is a position a CALLER named — a read's floor, a feed's
// cursor or its lower bound — on another log than the one it is asked of: the
// caller's mistake, answered as a bad request like [ReadLevel.Valid]'s.
//
// Not a [Refused]: that is a state of THIS node, unwrapping to
// [ErrUnavailable], which sends a client to another node and an operator to
// this one, while every node refuses a foreign position identically. Not
// [ErrWrongStream]: that is the engine comparing two positions of its own, a
// fault rather than anything a caller sent.
var ErrForeignPosition = errors.New("statelog: a position on another log")

// Position is where a domain is on a stream. THE THREE FIELDS ARE ONE VALUE: a
// rebuilt stream restarts at 1, so a bare sequence is indistinguishable from a
// current one; the generation, advanced only by an audited reanchor, makes an
// old sequence comparable and SAFELY STALE in one integer comparison; and the
// stream keeps a position from meaning something on another domain's log.
// [Position.Packed] is the one SQL-facing form, with no second name, so a
// comparison means the same everywhere.
//
// In an answer it is the object `{stream, generation, seq}`, the keys the
// dashboard's protocol types index; as a PARAMETER (a cursor, `since`,
// `min_position`) it is the token [Position.String] renders, because a query
// string carries no object.
type Position struct {
	// Stream is the stream this position is on.
	Stream string `json:"stream"`

	// Generation is THIS STREAM's, advanced only by an operator's reanchor
	// of that log: a coordinate in the stream's number space, not a fact
	// about the node. uint32, NOT uint64: the packed form bounds it at 2^23
	// either way, and a wider field would promise values that pack
	// silently negative.
	Generation uint32 `json:"generation"`

	// Seq is the broker's own sequence — a PubAck's, an applier's
	// checkpoint, an object's version — in one number space.
	Seq uint64 `json:"seq"`
}

// Packed is the position as one integer: the SQL-facing form, and what every
// durable column stores. (generation << 40) | seq orders by (generation, seq),
// so a cursor spanning a reanchor resumes with no gap or repeat and SQL
// compares a column with a plain `>`. The stream is NOT in it: it names the
// number space, and [Position.Before] refuses the cross-stream comparison the
// packed form cannot.
func (p Position) Packed() int64 {
	return int64(p.Generation)*GenerationStride | int64(p.Seq)
}

// Unpack is [Position.Packed]'s inverse, over the stream the column belongs
// to: every reader of a packed column uses this one copy of the arithmetic,
// since a slip in it puts a position silently in the wrong number space,
// where it compares as plausible and orders wrongly. The stream is an argument
// because the packed form does not carry it; a position built without one
// fails [Position.Valid] and refuses every comparison.
func Unpack(stream string, packed int64) Position {
	return Position{
		Stream:     stream,
		Generation: uint32(packed / GenerationStride),
		Seq:        uint64(packed % GenerationStride),
	}
}

// Before reports whether p is strictly before q, and refuses a comparison
// across streams. GENERATION FIRST, ALWAYS: a sequence from a previous
// generation is below every sequence in the current one, and this is the one
// place that is enforced.
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
// writes — a cursor, a `since`, a `min_position`, a wake's trigger — beside
// its writer, because a token is one vocabulary. The token is the triple
// because a bare sequence names no stream and no generation: from a previous
// generation, a reader cannot tell behind from impossibly far ahead.
func ParsePosition(raw string) (Position, error) {
	at, why, cause := parsePosition(raw)
	switch {
	case cause != nil:
		return Position{}, fmt.Errorf("statelog: %s: %w", why, cause)
	case why != "":
		return Position{}, fmt.Errorf("statelog: %s", why)
	}
	return at, nil
}

// parsePosition is [ParsePosition] without the package's prefix: why a token
// is not a position, as a sentence naming no package (which [ParseFreshness]
// renders under its own parameter's name), and the error it was concluded
// from, if any. Both are empty for a position. Not an error type: one in this
// package crosses the estate's wire or loses its sentence there.
func parsePosition(raw string) (Position, string, error) {
	refuse := func() (Position, string, error) {
		return Position{}, fmt.Sprintf("%q is not a log position — one reads "+
			"`<stream>@<generation>:<sequence>`, and the answer that produced it "+
			"carries the value to send back", raw), nil
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
		return Position{}, fmt.Sprintf("%q is not a usable log position", raw), err
	}
	return at, "", nil
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

// On is the ONE rule a position a caller named is held to: it is on stream,
// or it names nothing — a zero position, or one with no stream, has nothing to
// compare and is honoured nowhere, so neither is refused. Anything else wraps
// [ErrForeignPosition].
//
// It runs before the position is used at all: [Position.Packed] carries no
// stream, so a foreign floor that sorts low would be silently discarded and
// the read served as though none was named, and a foreign cursor or `since`
// would pick a page of this log by another log's number.
func (p Position) On(stream string) error {
	if p.IsZero() || p.Stream == "" || p.Stream == stream {
		return nil
	}
	return fmt.Errorf("%w: %s is a position on %s, and this is answered from "+
		"%s — a position names the log it is a position in, so send one this "+
		"log answered with", ErrForeignPosition, p, p.Stream, stream)
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
