package statelog

import (
	"fmt"
	"slices"
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
// That holds only where the value travels whole, so an ask derived from
// another is a copy of it with only what it changes reassigned
// ([Freshness.On]), never a literal naming the parts it keeps.
//
// # The floors
//
// [Freshness.Floors] is the read-your-writes half. A write returns the
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
// node holds, from here on" — the answer is still labelled with its lag, and
// a node that has not reached the floor within [ReadBudget] refuses `behind`
// rather than serving rows from before the write.
//
// ONE FLOOR PER LOG, and more than one log, because a position names its log
// and one answer can be read from two: the company feed is the tracker's work
// and the knowledge base's pages. One floor could hold up only one of the
// halves, so a caller that wrote both logs — a task closed and a page saved
// from one screen, or one gesture laying a record in each — could not read its
// writes back in the answer that drew them: whichever half the floor did not
// name was served from before the write. So each log a caller wrote carries
// its own floor ([ValidFloors]: at most one per log, at most [MaxFloors]), and
// a read of several logs hands each of its halves the floor on that half's log
// ([Freshness.On]).
//
// A floor on a log the read does not read is refused, never waited for — and
// refused as the REQUEST's mistake ([ErrForeignPosition]) rather than as a
// state of the node: a position names its stream, a caller pasting one
// across domains would otherwise wait out the whole budget for a sequence that
// means nothing here, and every node in the fleet refuses it the same. A
// reader of ONE log holds every floor to that log ([Freshness.OnlyOn],
// [Reader.Read]), so a half handed a floor its composer should have kept for
// the other half says so rather than dropping it.
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

	// Floors are the positions the answer must include, at most one per
	// log ([ValidFloors]); nil when the caller named none. A zero position
	// among them names nothing and holds nothing up, as an absent one
	// does ([Position.On]).
	Floors []Position
}

// FloorAt is the ask of a read that waits for at, at level: an engine path's
// read of at's own log by a caller holding that one position — a sync brought
// to a push's record, a page read after its own write. A zero at names no
// floor, and the ask carries none ([Freshness.Floors]).
func FloorAt(level ReadLevel, at Position) Freshness {
	f := Freshness{Level: level}
	if !at.IsZero() {
		f.Floors = []Position{at}
	}
	return f
}

// MaxFloors is how many floors one read may name, which is how many logs one
// answer may be read from at a caller's floor.
//
// FOUR, TWICE THE WIDEST ANSWER THE ENGINE COMPOSES. A floor is a position on
// the log a caller wrote, and a caller names one per log the answer reads:
// the widest answer reads two (the company feed, the tracker's log and the
// knowledge base's), so four leaves an answer composed later room for two
// logs more. Bounded at all because every floor is checked against every log
// an answer reads before anything else is, and a request naming a floor on
// each of a fleet's logs — eight state logs at this build — is a client that
// sends what it holds rather than what this answer reads: refused as that,
// rather than checked against each in turn.
const MaxFloors = 4

// ValidFloors reports whether floors are floors ONE read can wait on: each a
// position the packed form carries ([Position.Valid]), at most one per log —
// two on one log are two answers to "how far has this caller written here",
// and a read waits for one — and at most [MaxFloors] in all.
//
// IT IS THE GRAMMAR'S CHECK, made where a caller's floors are read off a wire
// (`min_position`, which repeats once per log): what an engine path builds for
// itself is one floor on the log it reads, and a reader takes the furthest of
// the floors it is handed on its own log rather than trusting the list's shape.
func ValidFloors(floors []Position) error {
	if len(floors) > MaxFloors {
		return fmt.Errorf("statelog: %d floors named, and a read waits on at most %d — "+
			"one per log it reads; name the floors of the logs this answer is read "+
			"from, never every one held", len(floors), MaxFloors)
	}
	for i, at := range floors {
		if err := at.Valid(); err != nil {
			return fmt.Errorf("statelog: floor %s: %w", at, err)
		}
		for _, earlier := range floors[:i] {
			if earlier.Stream == at.Stream {
				return fmt.Errorf("statelog: two floors on %s, %s and %s — a read "+
					"waits on one floor per log, so send the later of the two",
					at.Stream, earlier, at)
			}
		}
	}
	return nil
}

// Floor is the floor this ask names on stream — the furthest of them, where an
// engine path handed more than one — or the zero position where it names none
// there. It reads nothing else: a reader of one log that must refuse a floor
// on another asks [Freshness.OnlyOn] first.
func (f Freshness) Floor(stream string) Position {
	return floorOn(f.Floors, stream)
}

// OnlyOn refuses a floor this ask names on any log but streams — the logs, one
// or more, the answer it is asked of is read from — as [ErrForeignPosition],
// naming the floor and its log: the ONE rule a reader holds a caller's floors
// to, the same one [Reader.Read] holds a query's to ([Query.Floors]). A reader
// of one log names that log; a read of several names each, before it hands
// each half its own floor ([Freshness.On]), since a floor none of its halves
// reads would otherwise be dropped there. A zero floor, or one with no stream,
// names nothing and passes ([Position.On]).
func (f Freshness) OnlyOn(streams ...string) error {
	return onlyOn(f.Floors, streams...)
}

// On is this ask as the half of a composed read that reads stream takes it:
// the same level and bounds, and the floor the caller named on stream alone —
// never the others, which that half's reader refuses as positions on another
// log ([Reader.Read]), and which are the other halves' to wait for.
//
// # A copy of the whole ask, never a literal naming its parts
//
// The half is read on its own, so a part of the ask it is not handed is one
// nothing downstream can restore: a staleness bound dropped here reads both
// halves of the company feed unbounded while the answer still says `stale`.
// Built field by field, the half carried each part only for as long as the
// literal named it, and a part added to [Freshness] later would be left out
// without a word, which is the silent drop this type exists to make
// impossible. So the half starts as a copy of the ask and changes only what
// this method is for: its floors, narrowed into a slice of its own (never the
// ask's, which every other half is cut from too), and a `session` level left
// with nothing to wait for (below).
//
// # A `session` half with no floor of its own is read `stale`
//
// `session` IS the floor's wait: no answer from before the caller's own last
// write, which the caller names as a floor. A read of two logs is asked at
// `session` with a floor on each log the caller wrote, and a log it named no
// floor on is one it wrote nothing to that it can name — so that half has
// nothing to wait for. Read at `session` anyway it would wait for the zero
// position and serve this node's prefix under the stronger name, the wrong
// label [ReadSession] warns of and every read grammar refuses a bare
// `session` over. So the half is asked what serving this node's prefix IS:
// the dashboard's own level, `stale`, its lag reported — the dashboard's
// because `session` is settable on that surface alone ([SettableLevels]), so a
// `session` ask is always that surface's, and `stale` is what the surface
// reads at when nothing strengthens it. A session ask carries no staleness
// bound (every grammar refuses one beside it), so the half is unbounded, as
// the surface's own default is.
func (f Freshness) On(stream string) Freshness {
	out := f
	out.Floors = nil
	if at := f.Floor(stream); !at.IsZero() {
		out.Floors = []Position{at}
	}
	if out.Level == ReadSession && len(out.Floors) == 0 {
		out.Level = DefaultReadLevel(SurfaceDashboard)
	}
	return out
}

// Query is the framework read this ask resolves to over one scope.
//
// It is the ONE place a freshness becomes a query, so the four fields reach
// the reader from every entry point or from none.
func (f Freshness) Query(scope ScopeSet, set bool) Query {
	return Query{
		Level:     f.Level,
		Scope:     scope,
		Set:       set,
		MaxLag:    f.MaxLag,
		MaxLagSeq: f.MaxLagSeq,
		Floors:    f.Floors,
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

// Weakest is the level an answer composed of halves read at levels was read
// at: the weakest of them, in [ReadLevels]' order, since the answer is no
// fresher than its stalest half. An empty level is a half that read nothing
// and is passed over; none read is the empty level.
//
// IT IS THE COMPOSED ANSWER'S OWN LABEL, and it is not optional: a half whose
// log a `session` ask named no floor on is read `stale` ([Freshness.On]), and
// an answer labelled by whichever half answered first would tell a caller its
// rows were read at a level some of them never were — the claim every answer's
// level exists to make true.
func Weakest(levels ...ReadLevel) ReadLevel {
	var out ReadLevel
	for _, level := range levels {
		if level == "" {
			continue
		}
		if out == "" || slices.Index(ReadLevels, level) > slices.Index(ReadLevels, out) {
			out = level
		}
	}
	return out
}

// onlyOn is [Freshness.OnlyOn]'s rule over a list of floors: the first one on
// a log none of streams names, refused — through [Position.On] where the read
// is of one log, which is that rule's own sentence.
func onlyOn(floors []Position, streams ...string) error {
	for _, at := range floors {
		switch {
		case at.IsZero() || at.Stream == "" || slices.Contains(streams, at.Stream):
		case len(streams) == 1:
			return at.On(streams[0])
		default:
			return fmt.Errorf("%w: %s is a position on %s, and this answer is read "+
				"from %s — a position names the log it is a position in, so send "+
				"one those logs answered with", ErrForeignPosition, at, at.Stream,
				strings.Join(streams, " and "))
		}
	}
	return nil
}

// floorOn is the furthest of floors on stream, or the zero position where none
// is: the one floor a read of stream waits for.
func floorOn(floors []Position, stream string) Position {
	var out Position
	for _, at := range floors {
		if at.Stream == stream && !at.IsZero() {
			out = furthest(out, at)
		}
	}
	return out
}

// furthest is the later of two positions, which is what a read waits for when
// the caller's floor and the level's own target both apply.
func furthest(a, b Position) Position {
	if b.Packed() > a.Packed() {
		return b
	}
	return a
}
