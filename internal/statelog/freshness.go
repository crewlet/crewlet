package statelog

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/words"
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
// each of a fleet's logs — ten state logs at this build — is a client that
// sends what it holds rather than what this answer reads: refused as that,
// rather than checked against each in turn.
const MaxFloors = 4

// ValidFloors reports whether floors are floors ONE read can wait on: each a
// position the packed form carries ([Position.Valid]), at most one per log —
// two on one log are two answers to "how far has this caller written here",
// and a read waits for one — and at most [MaxFloors] in all.
//
// NO PACKAGE IN ITS SENTENCES, as in every [FreshnessError] it is carried
// by: a person asking any domain's question reads them.
//
// IT IS THE GRAMMAR'S CHECK, made where a caller's floors are read off a wire
// (`min_position`, which repeats once per log): what an engine path builds for
// itself is one floor on the log it reads, and a reader takes the furthest of
// the floors it is handed on its own log rather than trusting the list's shape.
func ValidFloors(floors []Position) error {
	if len(floors) > MaxFloors {
		return fmt.Errorf("%d floors named, and a read waits on at most %d, "+
			"one per log it reads; name the floors of the logs this answer is read "+
			"from, never every one held", len(floors), MaxFloors)
	}
	for i, at := range floors {
		if err := at.Valid(); err != nil {
			return fmt.Errorf("floor %s: %w", at, err)
		}
		for _, earlier := range floors[:i] {
			if earlier.Stream == at.Stream {
				return fmt.Errorf("two floors on %s, %s and %s: a read "+
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
				words.And(streams))
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

// FreshnessParams is a request's parameters as [ParseFreshness] reads them.
//
// DECLARED HERE, BY THE CONSUMER, and kept to the four methods the grammar
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

	// List is every value a key that may REPEAT was given — `min_position`,
	// once per log ([ParseFloors]) — and false where the value is not a
	// string or a list of them.
	List(key string) ([]string, bool)
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
//
// IT REPEATS, ONCE PER LOG ([ParseFloors]): one answer can be read from two
// logs — the company feed is Worklet's work and Doclet's pages — and the
// caller reading its writes back may have written to both, so each log the
// caller wrote takes its own floor, at most one per log and [MaxFloors] in all
// ([ValidFloors]). Which log each floor holds up is the READ's to say, since a
// position names its log and only the reader knows its own: a read of one log
// refuses a floor on another ([Query.Floors]), and a read of several hands
// each half its own ([Freshness.On]).
func ParseFreshness(p FreshnessParams) (Freshness, error) {
	var out Freshness
	floors, err := ParseFloors(p)
	if err != nil {
		return out, err
	}
	out.Floors = floors
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
	if level == ReadSession && len(out.Floors) == 0 {
		return out, &FreshnessError{Param: "read_level", Why: "session waits for " +
			"the caller's own last write, and none was named. Send the position " +
			"that write answered with as min_position (one for each log it landed " +
			"on), or ask for linearizable, or for stale with max_lag_seq to bound " +
			"how far behind an answer may be"}
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

// ParseFloors reads `min_position` — the caller's floors, one per log, held to
// [ValidFloors] — and is the ONE reading of the key: [ParseFreshness]'s, and a
// route's that takes a floor and no level. An empty value names nothing, and
// so does a zero position: it names no record, so it holds nothing up and is
// no floor at all, as an absent one is. Every refusal is a [FreshnessError]
// naming the key.
func ParseFloors(p FreshnessParams) ([]Position, error) {
	raws, ok := p.List("min_position")
	if !ok {
		return nil, &FreshnessError{Param: "min_position", Why: "a log position, " +
			"`<stream>@<generation>:<sequence>`, or one per log as a list of them: the " +
			"value a write answered with as its position"}
	}
	var floors []Position
	for _, raw := range raws {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		at, why, cause := parsePosition(raw)
		if why != "" {
			if cause != nil {
				why += ": " + cause.Error()
			}
			return nil, &FreshnessError{Param: "min_position", Why: why, Err: cause}
		}
		if !at.IsZero() {
			floors = append(floors, at)
		}
	}
	if err := ValidFloors(floors); err != nil {
		return nil, &FreshnessError{Param: "min_position", Why: err.Error(), Err: err}
	}
	return floors, nil
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
