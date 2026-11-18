package statelog

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/words"
)

// Freshness is everything a caller may say about how fresh an answer must be,
// and the ONE shape every read entry point takes for it, so a call site that
// drops a part is a compile error. A derived ask is a copy with only what
// changes reassigned ([Freshness.On]), never a literal naming the parts kept.
//
// Floors are the read-your-writes half: positions a write returned
// ([Result.Position]), honoured at EVERY level: a no-op at `linearizable`, the
// wait itself at `session`, and at `stale` and `consistent_prefix` a node short
// of the floor within [ReadBudget] refuses `behind`. One floor per log the
// caller wrote, since one answer can read two ([ValidFloors]). A floor on a
// log the read does not read is the request's mistake ([ErrForeignPosition]),
// refused rather than waited out ([Freshness.OnlyOn]).
type Freshness struct {
	// Level is the level asked for, EMPTY when the caller said nothing: not a
	// fifth state, but one the SURFACE resolves ([LevelFor]), since a grammar
	// shared by four surfaces cannot know which one is asking.
	Level ReadLevel

	// MaxLag and MaxLagSeq bound this node's lag behind the log in seconds
	// and in records, enforced in [Reader.target] before any row is read,
	// point reads included. A read refuses past whichever is reached first.
	MaxLag    time.Duration
	MaxLagSeq uint64

	// Floors are the positions the answer must include, at most one per
	// log ([ValidFloors]); nil when the caller named none. A zero position
	// holds nothing up ([Position.On]).
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

// MaxFloors is how many floors one read may name: twice the widest answer the
// engine composes (the company feed reads the tracker's and the knowledge
// base's logs), leaving room for a wider one. A request naming more is a
// client sending every floor it holds rather than this answer's, refused as
// that rather than each checked against every log the answer reads.
const MaxFloors = 4

// ValidFloors reports whether floors are floors ONE read can wait on: each a
// position the packed form carries ([Position.Valid]), at most one per log (a
// read waits for one), and at most [MaxFloors] in all. Like a
// [FreshnessError], its sentences name no package.
//
// It is the grammar's check on floors read off a wire (`min_position`); a
// reader itself takes the furthest floor on its own log rather than trusting
// the list's shape.
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

// Floor is the furthest floor this ask names on stream, or the zero position.
// It ignores other logs: a reader that must refuse those asks
// [Freshness.OnlyOn] first.
func (f Freshness) Floor(stream string) Position {
	return floorOn(f.Floors, stream)
}

// OnlyOn refuses a floor this ask names on any log but streams (the logs the
// answer is read from) as [ErrForeignPosition], naming the floor and its log:
// the ONE rule for a caller's floors, the one [Reader.Read] holds
// [Query.Floors] to. A composed read calls it with every log before
// [Freshness.On] splits the ask, or a floor no half reads is dropped. A zero
// floor, or one with no stream, passes ([Position.On]).
func (f Freshness) OnlyOn(streams ...string) error {
	return onlyOn(f.Floors, streams...)
}

// On is this ask as the half of a composed read that reads stream takes it:
// the same level and bounds, and only the floor named on stream (that half's
// reader refuses the others as positions on another log, [Reader.Read]).
//
// The half is a COPY of the whole ask, never a literal naming its parts, or a
// field added to [Freshness] drops silently; its floors go into a slice of
// its own, since every half is cut from the same ask. A `session` half with
// no floor would serve this node's prefix under the stronger name, so it reads
// at the dashboard's default, `stale`, unbounded (`session` is settable there
// alone, [SettableLevels], and carries no bound).
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

// Query is the framework read this ask resolves to over one scope: the ONE
// place a freshness becomes a query, so all four fields reach the reader.
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
// A composed answer must be labelled with it: a `session` half may have been
// read `stale` ([Freshness.On]), and labelled by another half the answer would
// claim a level some of its rows were never read at.
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

// onlyOn is [Freshness.OnlyOn]'s rule over a list of floors, refusing through
// [Position.On]'s sentence where the read is of one log.
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

// floorOn is the furthest of floors on stream, or the zero position.
func floorOn(floors []Position, stream string) Position {
	var out Position
	for _, at := range floors {
		if at.Stream == stream && !at.IsZero() {
			out = furthest(out, at)
		}
	}
	return out
}

// furthest is the later of two positions: what a read waits for when the
// caller's floor and the level's own target both apply.
func furthest(a, b Position) Position {
	if b.Packed() > a.Packed() {
		return b
	}
	return a
}

// FreshnessParams is a request's parameters as [ParseFreshness] reads them,
// satisfied by each surface's own bag (a query string, a socket frame, a
// tool's argument map).
type FreshnessParams interface {
	// String is one value, empty when absent.
	String(key string) string

	// Int is one value as a whole number, def when absent.
	Int(key string, def int) int

	// Has tells an absent key ("no bound") from a zero one ("a bound of
	// zero").
	Has(key string) bool

	// List is every value a REPEATING key was given (`min_position`), false
	// where the value is not a string or a list of them.
	List(key string) ([]string, bool)
}

// FreshnessError is one freshness parameter [ParseFreshness] refused, in a
// sentence that names the PARAMETER and no package: every domain's surface
// reads these keys, so a package would name a domain the caller never asked.
// Param is the key to change.
type FreshnessError struct {
	Param string
	Why   string

	// Err is the cause, for [errors.Is]; never part of the sentence.
	Err error
}

func (e *FreshnessError) Error() string { return e.Param + ": " + e.Why }
func (e *FreshnessError) Unwrap() error { return e.Err }

// ParseFreshness reads the four freshness keys — `read_level`,
// `max_lag_seconds`, `max_lag_seq` and `min_position` — and is the ONE place
// their rules are stated, for every domain's questions, the socket, the REST
// routes and a seat's tools alike: a second reading would be the one place a
// key meant something slightly different. Every refusal is a [FreshnessError].
//
// `min_position` is a log position (`<stream>@<generation>:<sequence>`, as a
// write answers and a wake carries), repeated once per log ([ParseFloors]).
// `session` is accepted ONLY beside one: bare, it would wait for the zero
// position and label this node's prefix `session`. Which log a floor holds up
// is the read's to say ([Query.Floors], [Freshness.On]).
func ParseFreshness(p FreshnessParams) (Freshness, error) {
	var out Freshness
	floors, err := ParseFloors(p)
	if err != nil {
		return out, err
	}
	out.Floors = floors
	value := strings.TrimSpace(p.String("read_level"))
	if value == "" {
		// Absent resolves to the SURFACE's default (a seat tool
		// linearizable, a dashboard poll stale). A bound is still read:
		// the surface that resolves to `stale` is the one whose callers
		// bound staleness, so refusing it would refuse the common case.
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
// route's that takes a floor and no level. An empty value or a zero position
// is no floor. Every refusal is a [FreshnessError] naming the key.
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

// withBounds reads the two staleness bounds onto an answer that may carry
// them: carried, not just checked, or the bound never bounds anything.
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
