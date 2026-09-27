package statelog

import (
	"errors"
	"fmt"
	"time"
)

// Freshness is everything a caller may say about how fresh an answer must be,
// and the ONE shape every read entry point takes for it.
//
// # Why a type rather than a level and three fields
//
// Because the fields were dropped at the doors that took only a level.
// Thirteen tracker questions and the three page reads take "a level" from the
// same grammar, and the two point reads plus the page reads took ONLY the
// level — so a caller declaring `max_lag_seq=250` on a task detail was served
// an answer of any distance and told, in the answer's own `read_level`, that
// it came back at the level asked for. The comment beside the drop said a
// bound "has nowhere to be enforced" on a single row, which is false: the
// bound is about THIS NODE'S LAG behind the log ([Query.MaxLag],
// [Query.MaxLagSeq]), enforced in [Reader.target] before any row is read, and
// a row's read is exactly as far behind as a set's on the same node.
//
// One value carrying all four means a reader that takes it cannot take less,
// and a call site that drops one is a compile error rather than a comment.
//
// # The floor
//
// [Freshness.MinPosition] is the read-your-writes half. A write returns the
// position its record landed at ([Result.Position]), and a caller that holds
// one — a screen redrawing after the write it just made, an operator's
// assistant reading back the task it created, a client acting on a wake that
// carried the record's position — names it here and is served nothing from
// before it. It is honoured at EVERY level: at `linearizable` the barrier's
// own position is already at or past any position a write returned on this
// stream, so the floor is a no-op that costs nothing to accept; at `session`
// it IS the high-water mark the level waits for, which is what makes the level
// offerable to a caller outside the engine at all; at `stale` and
// `consistent_prefix` it turns "whatever this node holds" into "whatever this
// node holds, from this position on" — the answer is still labelled with its
// lag, and a node that has not reached the floor within [ReadBudget] refuses
// `behind` rather than serving rows from before the write.
//
// A floor on ANOTHER stream is refused, never waited for — and refused as the
// REQUEST's mistake ([ErrForeignFloor]) rather than as a state of the node: a
// position names its stream, a caller pasting one across domains would
// otherwise wait out the whole budget for a sequence that means nothing here,
// and every node in the fleet refuses it the same.
type Freshness struct {
	// Level is the level asked for, EMPTY when the caller said nothing —
	// which is not a fifth state: the SURFACE resolves it, through
	// [LevelFor], because a grammar shared by four surfaces cannot know
	// which one is asking.
	Level ReadLevel

	// MaxLag and MaxLagSeq are the same bound in the two units a caller
	// may have: seconds, and the RECORD count the broker actually
	// answers. Both may be set and a read refuses past whichever is
	// reached first, because they are two readings of one distance.
	MaxLag    time.Duration
	MaxLagSeq uint64

	// MinPosition is the floor: the position the answer must include,
	// zero when the caller named none.
	MinPosition Position
}

// Query is the framework read this ask resolves to over one scope.
//
// It is the ONE place a freshness becomes a query, so the four fields reach
// the reader from every entry point or from none.
func (f Freshness) Query(scope ScopeSet, set bool) Query {
	return Query{
		Level:       f.Level,
		Scope:       scope,
		Set:         set,
		MaxLag:      f.MaxLag,
		MaxLagSeq:   f.MaxLagSeq,
		MinPosition: f.MinPosition,
	}
}

// Bounded reports whether this ask carries a staleness bound in either unit.
func (f Freshness) Bounded() bool { return f.MaxLag > 0 || f.MaxLagSeq > 0 }

// ErrForeignFloor is a read floored at a position on ANOTHER log: the caller's
// mistake about the request, never a state of the node answering it.
//
// # Why it is not a refusal
//
// A [Refused] is a state of THIS node, and it unwraps to [ErrUnavailable],
// which every surface answers as "this node cannot answer here": a client acts
// on it by coming back later or by asking another node, and an operator by
// acting on this one. A floor on another log is none of that. Every node in the
// fleet refuses it identically however long anybody waits, so answered as a
// refusal — it was `wrong_stream`, the code this node's own recreated stream
// answers — it sent a client to a node that would refuse it the same, and a
// dashboard told a person an operator had to act on a node with nothing wrong
// with it. It is [ReadLevel.Valid]'s kind of failure instead: the request,
// refused before the read looks at anything, which a surface answers as a bad
// request.
var ErrForeignFloor = errors.New("statelog: the read's floor is a position on another log")

// floorOn is the ONE rule a caller's floor is held to: it names this read's
// own log, or nothing.
//
// A zero position and one that names no stream name nothing to compare, and a
// floor is honoured only where it names something, so neither is refused.
//
// # Why it is checked before anything else
//
// [Position.Packed] deliberately carries the generation and the sequence and
// NOT the stream, so a foreign floor compared against a local target is two
// coordinates from two number spaces. Selecting the later of the two first
// DISCARDED a foreign floor whenever it happened to sort low (`OTHER@0:0`
// against any live barrier), and a guard applied afterwards then inspected a
// purely local position and waved it through: the read was served as though no
// floor had been named, labelled with the level the caller asked for. And a
// request that can never be answered is refused as the same request on every
// node, so it is refused before this node's own state — an evicted node, a
// stalled one — is consulted and could answer something else.
func floorOn(floor Position, stream string) error {
	if floor.IsZero() || floor.Stream == "" || floor.Stream == stream {
		return nil
	}
	return fmt.Errorf("%w: this read floors at %s, a position on %s, and it is "+
		"answered from %s — a position names the log it is a position in, so "+
		"send one a write on %s answered with, or none", ErrForeignFloor,
		floor, floor.Stream, stream, stream)
}

// furthest is the later of two positions, which is what a read waits for when
// the caller's floor and the level's own target both apply.
func furthest(a, b Position) Position {
	if b.Packed() > a.Packed() {
		return b
	}
	return a
}
