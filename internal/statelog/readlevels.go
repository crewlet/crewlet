package statelog

import "slices"

// WHO IS ASKING, and what they get when they do not say: one table, not a
// literal at each call site, because literals drift invisibly toward the
// weaker level. A seat passing bare `session` waits for a zero high-water mark
// and serves this node's committed prefix under a stronger name
// ([Reader.target]). TestEverySurfaceHasAReadLevelDefault walks the table, so
// a default changed here changes everywhere and a surface added without a
// decision fails rather than inheriting whatever the last author typed.

// Surface is the kind of caller a read comes from: four answers to "what is
// this reader entitled to assume", not four places in the code, so a table
// keyed on the calling file would be wrong.
//
// The tracker's saved-view furniture read is always `stale` whatever the
// surface: nobody writes a board's parameters and reads them back in one
// gesture. That is a property of the rows, so it stays a literal at its one
// call site rather than a surface nobody else could name.
type Surface string

const (
	// SurfaceSeat is an agent's own tool call, inside a turn.
	SurfaceSeat Surface = "seat"

	// SurfaceOperator is a person's assistant over the operator MCP.
	SurfaceOperator Surface = "operator"

	// SurfaceDashboard is the dashboard and the REST read path — a human
	// polling a screen.
	SurfaceDashboard Surface = "dashboard"

	// SurfaceReplication is any answer ABOUT replication: the retention
	// report, Settings › Nodes' lag column, whether a purge has landed
	// everywhere.
	//
	// A surface although really a subject: what is asked about decides the
	// level, on every surface including the operator MCP. A node cannot
	// answer `linearizable` about its own replication, because the barrier
	// that level appends is the instrument whose health is the subject; it
	// would refuse in exactly the incident the page was opened for. A full
	// log's containment rests on this: it costs `linearizable` reads, and
	// the answer naming the blocking trim term must keep answering.
	SurfaceReplication Surface = "replication"
)

// Surfaces are the four.
var Surfaces = []Surface{
	SurfaceSeat, SurfaceOperator, SurfaceDashboard, SurfaceReplication,
}

// Valid reports whether a surface off the wire is one this build knows.
func (s Surface) Valid() bool { return slices.Contains(Surfaces, s) }

// ReadLevelDefaults is what each surface reads at when the caller says nothing.
//
// Seat and operator are separate rows that agree: the operator MCP serves a
// seat's own tool implementations, so its level is inherited, and two rows
// make the agreement a decision rather than a value that drifts when the other
// is edited. A dashboard reads `stale` because it polls and renders the lag
// beside the answer; a barrier per poll would buy freshness nobody reads.
func ReadLevelDefaults() map[Surface]ReadLevel {
	return map[Surface]ReadLevel{
		// The answer decides something (a create refuses an unknown
		// project, a hand-off names a colleague), and a seat has no
		// screen on which to notice a stale copy.
		SurfaceSeat: ReadLinearizable,

		// Not settable: the person is deciding about their own company,
		// and a level they could weaken they would weaken by accident.
		SurfaceOperator: ReadLinearizable,

		// A polling screen, its lag on the answer and a bound the caller
		// may set.
		SurfaceDashboard: ReadStale,

		// The default, not the whole rule: [ResolveReplicationLevel]
		// weakens it one step when the lag is unknown.
		SurfaceReplication: ReadStale,
	}
}

// DefaultReadLevel is the level a surface reads at when nobody asks.
//
// An unknown surface reads `linearizable`, the fail-safe: the zero ReadLevel
// matches no level, and the strongest answer is never wrong, only slower.
func DefaultReadLevel(s Surface) ReadLevel {
	if level, held := ReadLevelDefaults()[s]; held {
		return level
	}
	return ReadLinearizable
}

// ResolveReplicationLevel is the level an answer ABOUT replication is served
// at, given whether this node's own lag came back as a number.
//
// `stale` is a claim about AGE, which an answer assembled while the lag could
// not be read (broker or coordination unreachable: the very outage being
// diagnosed) cannot make; `consistent_prefix` claims only a coherent point in
// the log's order. Resolving, not exempting: an exemption would take these
// answers outside the read-level contract, a silent downgrade, while
// resolution keeps the served level on the answer and the caller chooses
// neither.
func ResolveReplicationLevel(lagKnown bool) ReadLevel {
	if lagKnown {
		return DefaultReadLevel(SurfaceReplication)
	}
	return ReadConsistentPrefix
}

// SettableLevels are the levels a CALLER may ask for on a surface; empty means
// the level is not the caller's to pick.
//
// Only the dashboard chooses: it renders level and lag beside the rows, so a
// person who asks for less sees what they got. The operator's reads refuse: a
// knob that only weakens gets turned once and forgotten, and an audit surface
// cannot let the caller choose how stale its evidence is. A replication
// answer's level is derived ([ResolveReplicationLevel]); a seat's is not a
// model's to trade for latency it cannot perceive. `session` is offered only
// because the screen's grammar requires `min_position` beside it (a write
// answers with where its record landed); bare, it would wait for the zero
// position and mislabel this node's committed prefix.
func SettableLevels(s Surface) []ReadLevel {
	if s != SurfaceDashboard {
		return nil
	}
	// Each is a promise this surface keeps: `linearizable` establishes the
	// log's end, the stale levels serve what this node holds, and `session`
	// waits for the position the caller names.
	return []ReadLevel{ReadLinearizable, ReadSession, ReadStale, ReadConsistentPrefix}
}

// LevelFor resolves what a read should use, given a surface and what the
// caller asked for.
//
// An unsettable surface IGNORES the ask rather than refusing it: one query
// grammar serves the board, socket, REST route and seat tools, so the ask may
// come from a path that never knew its surface, and on a seat a refusal would
// send a model to fix a key it never typed. Every answer reports the level it
// was actually read at. Call it unconditionally, never behind
// `if asked == ""`: that guard looks like enforcement only while no path
// populates the field. `consistent_prefix` is never a default
// ([ReadConsistentPrefix]) and is reached only by asking, where allowed.
func LevelFor(s Surface, asked ReadLevel) ReadLevel {
	if !slices.Contains(SettableLevels(s), asked) {
		return DefaultReadLevel(s)
	}
	return asked
}
