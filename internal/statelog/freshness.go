package statelog

import "time"

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
// A floor on ANOTHER stream is refused `wrong_stream`, never waited for: a
// position names its stream, and a caller pasting one across domains would
// otherwise wait out the whole budget for a sequence that means nothing here.
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

// Resolved is the framework read this ask resolves to over one object named by
// reference, whose scope only its rows can answer — see [Query.Resolve].
func (f Freshness) Resolved(resolve Resolver, set bool) Query {
	q := f.Query(ScopeSet{}, set)
	q.Resolve = resolve
	return q
}

// Bounded reports whether this ask carries a staleness bound in either unit.
func (f Freshness) Bounded() bool { return f.MaxLag > 0 || f.MaxLagSeq > 0 }

// furthest is the later of two positions, which is what a read waits for when
// the caller's floor and the level's own target both apply.
func furthest(a, b Position) Position {
	if b.Packed() > a.Packed() {
		return b
	}
	return a
}
