package statelog

import "slices"

// WHO IS ASKING, and what they get when they do not say.
//
// # Why a table rather than a literal at each call site
//
// Because the literals drifted, invisibly and in the one direction that looks
// harmless. Every seat tool passed `session` — twenty-one call sites, each
// plausible on its own — where the design says a seat's every read is
// `linearizable`. That would have been a mere downgrade if `session` meant
// anything there, and it did not: a session read waits for the caller's own
// high-water mark, NOTHING populated it at a seat call site, so the target was
// the zero position and the read served this node's committed prefix
// immediately. [Reader.target]'s own comment names the result — "a stale read
// wearing a stronger name".
//
// So a seat asked for the strongest guarantee the engine has, was given the
// weakest, and the answer said `session`. A seat that reads a stale task and
// tells a colleague "nobody is assigned to this" has produced a wrong answer
// no broker refuses.
//
// One table, and a test that walks it, is what makes that visible: a default
// changed here changes everywhere, and a surface added without a decision
// fails the test rather than silently inheriting whatever the last author
// typed.

// Surface is the kind of caller a read comes from.
//
// FOUR, and they are not four places in the code — they are four different
// ANSWERS to "what is this reader entitled to assume", which is why a table
// keyed on the file a call sits in would be the wrong table.
//
// # What is deliberately not a surface
//
// The saved-view furniture read in the tracker is always `stale` whatever the
// caller's surface: what a board's parameters are is not something anybody
// writes and reads back in one gesture. That is a property of the ROWS rather
// than of the reader, so it stays a literal at its one call site with its
// reason beside it, rather than a surface nobody else could ever name.
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
	// report, the Fleet screen's lag column, whether a purge has landed
	// everywhere.
	//
	// A SURFACE OF ITS OWN although it is really a SUBJECT, because it is
	// the one case where what is being ASKED ABOUT decides the level
	// rather than who is asking — and it holds on every surface, the
	// operator MCP included, which is what makes it a row here rather
	// than a rule inside three other rows.
	//
	// `linearizable` means "every mutation committed anywhere in the
	// company before this read was issued is in the answer", and it is
	// established by appending a barrier and waiting through its
	// position. The question these answers ask is HOW FAR BEHIND that
	// same log this node is: the barrier is the instrument and its health
	// is the subject. A node cannot produce a `linearizable` answer to a
	// question whose subject is its own replication — so that is not a
	// stronger answer costing more, it is a level that CANNOT BE SERVED,
	// refusing in precisely the incident somebody opened the page for.
	//
	// The whole containment argument for a full log rests on this row: a
	// full log costs `linearizable` reads rather than reads, and the one
	// answer naming which trim term is blocking has to keep answering
	// while it is true.
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
// A SEAT AND AN OPERATOR READ THE SAME WAY, and that is the point of listing
// them separately rather than folding them: the operator MCP serves a seat's
// own tool implementations, so its level is inherited rather than chosen, and
// an inherited value is exactly the kind that drifts when somebody edits the
// other one. Two rows make the agreement a decision.
//
// A DASHBOARD READS `stale` because it is polling: it asks the same question
// every few seconds and renders the lag beside the answer, so a barrier per
// poll would buy a freshness nobody is reading and pay for it on every screen
// in the company.
func ReadLevelDefaults() map[Surface]ReadLevel {
	return map[Surface]ReadLevel{
		// EVERY SEAT TOOL READ. The answer decides something — a create
		// refuses a project the company does not have, a hand-off names
		// a colleague, a turn reports what it found — and a seat has no
		// screen on which to notice that it was reading a stale copy.
		SurfaceSeat: ReadLinearizable,

		// AND THE OPERATOR'S, about tracker content. NOT SETTABLE: the
		// person asking is making a decision about their own company,
		// and a level they could weaken is one they would weaken by
		// accident.
		SurfaceOperator: ReadLinearizable,

		// A POLLING SCREEN, with its lag on the answer and a bound the
		// caller may set.
		SurfaceDashboard: ReadStale,

		// AN ANSWER ABOUT REPLICATION, on every surface including the
		// operator's — see [SurfaceReplication]. This is the DEFAULT
		// and not the whole rule: when the lag is not a number the
		// resolution below weakens it one step, because `stale` is a
		// claim about age and an unknown lag cannot make one.
		SurfaceReplication: ReadStale,
	}
}

// DefaultReadLevel is the level a surface reads at when nobody asks.
//
// AN UNKNOWN SURFACE READS `linearizable`, which is the fail-safe direction
// and the opposite of what a zero value would give: the zero ReadLevel is the
// empty string, which no level matches, and a caller that fell through to it
// would be served by whatever the reader does with an unrecognised level. The
// strongest answer is the one that is never WRONG, only slower.
func DefaultReadLevel(s Surface) ReadLevel {
	if level, held := ReadLevelDefaults()[s]; held {
		return level
	}
	return ReadLinearizable
}

// ResolveReplicationLevel is the level an answer ABOUT replication is served
// at, given whether this node's own lag came back as a number.
//
// # Why the one surface that resolves instead of defaulting
//
// `stale` is not merely "possibly old" — it is a claim about AGE, which the
// answer carries and a caller may bound. An answer assembled while the lag
// could not be read cannot make that claim: the broker was unreachable, or
// coordination was, which is the ordinary signature of the outage somebody is
// diagnosing when they open this page. `consistent_prefix` is the honest name
// for what they get then — a coherent point in the log's own order, with no
// statement about age.
//
// AN ORDERED PAIR RATHER THAN AN EXEMPTION, which was the obvious move and is
// worse: an exemption would put these answers outside the read-level contract
// entirely, and a surface that quietly stopped naming its level is the silent
// downgrade wearing a different hat. Resolution keeps them inside it — the
// served level is on the answer, and the caller chooses neither.
func ResolveReplicationLevel(lagKnown bool) ReadLevel {
	if lagKnown {
		return DefaultReadLevel(SurfaceReplication)
	}
	return ReadConsistentPrefix
}

// SettableLevels are the levels a CALLER may ask for on a surface, and the
// empty slice is a surface where the level is not the caller's to pick.
//
// # Why only the screen chooses
//
// A dashboard is the one caller that can SEE what it got: the level and the
// lag are rendered beside the rows, so a person who asks for a weaker answer
// is told what they were given. That is what makes the choice honest there and
// nowhere else.
//
// The operator's tracker reads refuse it because nothing is wrong and the
// person is deciding something about their company. A knob that only ever
// weakens the answer is one somebody turns once, forgets, and then reads a
// stale board from for a year. An operator surface where the caller chooses
// how stale its evidence may be is not an audit surface — the same argument
// that makes the writer's own name unchoosable there.
//
// A REPLICATION ANSWER REFUSES IT TOO, and there the reason is that the level
// is not a preference at all: it is DERIVED from whether the lag could be
// read, which is a fact about the moment rather than a choice anybody makes.
// See [ResolveReplicationLevel].
//
// A SEAT CANNOT CHOOSE EITHER, and that is the strongest form of the same
// argument: the level is not a model's to pick. A tool argument for it would
// be a model trading correctness for latency it cannot perceive.
//
// # And `session` is on NOBODY's list
//
// A session read waits for THE CALLER'S OWN high-water mark, which the caller
// has to supply — [Query.Session]. Nothing outside this package can: a screen
// is an HTTP request holding no position, and a seat's tools carry none
// either. A surface that accepted `session` would hand the reader the zero
// position, wait for nothing, serve this node's committed prefix and label the
// answer `session` — which is not a weaker answer than the one asked for, it
// is a WRONG LABEL on it, and it is the whole finding this file was written
// for. So the level is honoured where a position exists (the write path's
// snapshot wait, [Request.Session]) and offered to nobody who cannot name one.
func SettableLevels(s Surface) []ReadLevel {
	if s != SurfaceDashboard {
		return nil
	}
	// THE THREE THAT NEED NO POSITION FROM THE CALLER. `linearizable`
	// establishes the log's end itself, and the two stale levels serve
	// what this node holds — so each is a promise this surface can keep.
	return []ReadLevel{ReadLinearizable, ReadStale, ReadConsistentPrefix}
}

// LevelFor resolves what a read should use, given a surface and what the
// caller asked for.
//
// AN UNSETTABLE SURFACE IGNORES THE ASK rather than refusing it, because the
// ask is not a request the caller made. One query grammar serves the board,
// the socket, the REST route and a seat's own tools, and a level reaching this
// call arrived through that grammar from a path with no idea which surface it
// would be answered on. The surface is the authority; the grammar is not, so
// what it carried is overruled rather than made an error somebody has to
// understand — and on the seat surface the somebody is a model, which would
// try to fix a refusal about a key it never typed.
//
// Nothing is hidden by that: every answer reports the level it was ACTUALLY
// read at, so a caller that asked for one and got another can see it.
//
// CALL THIS UNCONDITIONALLY rather than guarding it with `if asked == ""`.
// A guard makes the default apply only where nothing was asked, which is
// indistinguishable from enforcement for exactly as long as no path populates
// the field — and the whole finding behind this file is a level that was
// wrong at twenty-one call sites and looked right at every one.
//
// `consistent_prefix` is never a DEFAULT anywhere (see [ReadConsistentPrefix])
// and reaches a read only by being asked for, on a surface that allows it.
func LevelFor(s Surface, asked ReadLevel) ReadLevel {
	if !slices.Contains(SettableLevels(s), asked) {
		return DefaultReadLevel(s)
	}
	return asked
}
