package statelog

import (
	"fmt"
	"strings"
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
// REQUEST's mistake ([ErrForeignPosition]) rather than as a state of the node: a
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

// FreshnessParams is a request's parameters as [ParseFreshness] reads them.
//
// DECLARED HERE, BY THE CONSUMER, and kept to the three methods the grammar
// calls: every surface that carries the four keys fills a bag of its own —
// a query string, a socket frame, a tool's argument map — and each satisfies
// this without the framework importing any of them.
type FreshnessParams interface {
	// String is one value, empty when absent.
	String(key string) string

	// Int is one value as a whole number, def when absent.
	Int(key string, def int) int

	// Has distinguishes an absent key from a zero one, which is what tells
	// "no bound" from "a bound of zero".
	Has(key string) bool
}

// FreshnessError is one freshness parameter [ParseFreshness] refused, in a
// sentence that names the PARAMETER and nothing else.
//
// NO PACKAGE IN THE SENTENCE, because every domain's surface reads these four
// keys and a person's request is refused through whichever one they asked: the
// grammar used to live in the tracker and say "worklet:", so a bad
// `read_level` on a chat or a wiki question named a domain the caller never
// asked about. Param is the key to change.
type FreshnessError struct {
	Param string
	Why   string

	// Err is the cause, where the refusal has one, for [errors.Is]; it is
	// never part of the sentence.
	Err error
}

func (e *FreshnessError) Error() string { return e.Param + ": " + e.Why }
func (e *FreshnessError) Unwrap() error { return e.Err }

// ParseFreshness reads the four freshness keys — `read_level`,
// `max_lag_seconds`, `max_lag_seq` and `min_position` — and is the ONE place
// their rules are stated. Every refusal is a [FreshnessError].
//
// # Why one grammar and not one per question
//
// Seventeen registered questions took a level and a staleness bound, and
// exactly ONE of them — the task board — used to read either. The rest
// hardcoded a level and never looked at a bound at all, so a caller asking a
// project listing for a fresher answer was served the same one and told it
// came back at the level asked for. One grammar serves every domain's
// questions, the socket, the REST routes and a seat's own tools, and a second
// reading of `read_level` would be the one place a freshness key meant
// something slightly different.
//
// HERE, BESIDE [Freshness], because the shape it returns is the framework's
// and every domain reads through it. It lived in the tracker, which made every
// other domain's refusal a tracker's.
//
// # `min_position` and `session`
//
// `min_position` is a log position — `<stream>@<generation>:<sequence>`, the
// form every write answers with and every wake carries — and the answer is
// served from no earlier than it, at whatever level. It is what makes
// `read_level=session` honest: a session read waits for the caller's own last
// write, a grammar with no key to carry one would accept the level bare, wait
// for the zero position, serve this node's committed prefix and label the
// answer `session`. So `session` is accepted ONLY beside a `min_position`,
// and refused without one naming the key.
func ParseFreshness(p FreshnessParams) (Freshness, error) {
	var out Freshness
	if raw := strings.TrimSpace(p.String("min_position")); raw != "" {
		at, why, cause := parsePosition(raw)
		if why != "" {
			if cause != nil {
				why += ": " + cause.Error()
			}
			return out, &FreshnessError{Param: "min_position", Why: why, Err: cause}
		}
		out.MinPosition = at
	}
	value := strings.TrimSpace(p.String("read_level"))
	if value == "" {
		// ABSENT IS NOT A FIFTH STATE. It resolves to the SURFACE's own
		// default — a seat tool linearizable, a dashboard poll stale —
		// which is what makes the default a property of the surface
		// rather than of the model that happened to omit the key.
		//
		// AND A BOUND WITHOUT A LEVEL IS STILL READ, because the surface
		// that resolves an absent level to `stale` is exactly the one
		// whose callers bound staleness — a dashboard poll. Refusing it
		// here would refuse the common case; carrying it and letting a
		// non-stale resolution ignore it would be the silent drop this
		// grammar was already fixed for once.
		return withBounds(out, p)
	}
	level := ReadLevel(value)
	if !level.Valid() {
		return out, &FreshnessError{Param: "read_level", Why: fmt.Sprintf("%q is "+
			"not a read level; the four are linearizable, session, stale and "+
			"consistent_prefix", value)}
	}
	if level == ReadSession && out.MinPosition.IsZero() {
		return out, &FreshnessError{Param: "read_level", Why: "session waits for " +
			"the caller's own last write, and none was named. Send the position " +
			"that write answered with as min_position, or ask for linearizable, " +
			"or for stale with max_lag_seq to bound how far behind an answer may be"}
	}
	out.Level = level
	if level != ReadStale {
		for _, key := range []string{"max_lag_seq", "max_lag_seconds"} {
			if p.Has(key) {
				return out, &FreshnessError{Param: key, Why: fmt.Sprintf("bounds "+
					"how STALE an answer may be and read_level is %s, which is not "+
					"a staleness bound at all", level)}
			}
		}
		return out, nil
	}
	return withBounds(out, p)
}

// withBounds reads the two staleness bounds onto an answer that may carry them.
//
// THE BOUND IS CARRIED, not just checked. A bound refused where it is
// inconsistent and dropped where it is not is a bound that never bounded
// anything — which is what `max_lag_seconds` did before it was fixed, and what
// `max_lag_seq` did after, in the same function, for longer.
func withBounds(f Freshness, p FreshnessParams) (Freshness, error) {
	if p.Has("max_lag_seconds") {
		secs := p.Int("max_lag_seconds", 0)
		if secs < 0 {
			return f, &FreshnessError{Param: "max_lag_seconds", Why: fmt.Sprintf(
				"how many seconds behind an answer may be, and %d is not a "+
					"duration", secs)}
		}
		f.MaxLag = time.Duration(secs) * time.Second
	}
	if p.Has("max_lag_seq") {
		records := p.Int("max_lag_seq", 0)
		if records < 0 {
			return f, &FreshnessError{Param: "max_lag_seq", Why: fmt.Sprintf(
				"how many records behind an answer may be, and %d is not a "+
					"count", records)}
		}
		f.MaxLagSeq = uint64(records)
	}
	return f, nil
}
