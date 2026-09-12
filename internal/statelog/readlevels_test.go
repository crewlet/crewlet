package statelog_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY SURFACE HAS A DECIDED LEVEL, and this is the test whose absence let
// twenty-one call sites drift.
//
// The literals were each plausible alone: a seat tool passing `session` reads
// as "the caller sees its own writes", which is what a tool wants. What it
// actually produced was a read that waited for NOTHING — nothing populated the
// session's high-water mark at a seat call site, so the target was the zero
// position — served this node's committed prefix, and labelled the answer
// `session`. The spec says a seat's every read is `linearizable`, in five
// independent places.
//
// So the table is the decision and this walks it: a surface added without a
// row fails here rather than inheriting whatever the last author typed.
func TestEverySurfaceHasAReadLevelDefault(t *testing.T) {
	t.Parallel()
	defaults := statelog.ReadLevelDefaults()
	for _, surface := range statelog.Surfaces {
		level, held := defaults[surface]
		if !held {
			t.Errorf("%q has no default read level — a surface with no row "+
				"inherits whatever the last caller typed, which is how every "+
				"seat tool came to ask for a guarantee it was not given", surface)
			continue
		}
		if !level.Valid() {
			t.Errorf("%q defaults to %q, which is not a read level", surface, level)
		}
		// NO SURFACE DEFAULTS TO consistent_prefix. It is weaker than
		// session in a way that is INVISIBLE in the answer, so a default
		// would silently downgrade every reader that did not know to
		// ask for more.
		if level == statelog.ReadConsistentPrefix {
			t.Errorf("%q defaults to consistent_prefix, which no surface may "+
				"default to: it is weaker than session in a way the answer "+
				"does not show", surface)
		}
	}
	if len(defaults) != len(statelog.Surfaces) {
		t.Errorf("the table has %d rows and there are %d surfaces — a row for "+
			"a surface that no longer exists is a decision about nothing",
			len(defaults), len(statelog.Surfaces))
	}
}

// A SEAT AND AN OPERATOR READ ALIKE, and it is asserted rather than left to
// two rows that happen to match: the operator MCP serves a seat's own tool
// implementations, so its level is INHERITED, and an inherited value is
// exactly the kind that drifts when somebody edits the other one.
func TestASeatAndAnOperatorReadAtTheSameLevel(t *testing.T) {
	t.Parallel()
	seat := statelog.DefaultReadLevel(statelog.SurfaceSeat)
	operator := statelog.DefaultReadLevel(statelog.SurfaceOperator)
	if seat != operator {
		t.Fatalf("a seat reads at %q and an operator at %q — the operator MCP "+
			"serves the seat's own tools, so a difference here is one surface "+
			"quietly answering a different question", seat, operator)
	}
	if seat != statelog.ReadLinearizable {
		t.Fatalf("a seat reads at %q — the design says every seat read is "+
			"linearizable, because the answer decides something and a seat "+
			"has no screen on which to notice it was reading a stale copy",
			seat)
	}
}

// AN ANSWER ABOUT REPLICATION IS `stale` ON EVERY SURFACE, including the
// operator's — and WEAKER STILL when this node could not measure its own lag.
//
// `linearizable` means "every mutation committed anywhere before this read was
// issued is in the answer", established by appending a barrier and waiting
// through its position. The question these answers ask is how far behind that
// same log this node is: the barrier is the instrument and its health is the
// subject, so the stronger level is not a better answer costing more — it is
// one that cannot be served, refusing in exactly the incident somebody opened
// the page for.
//
// And `stale` is a claim about AGE, which an answer assembled while the lag
// could not be read cannot make. That is the ordinary signature of the outage
// being diagnosed, so the resolution weakens one step and NAMES it rather than
// delivering the weaker answer under the stronger name.
func TestAReplicationAnswerIsStaleAndWeakerWhenTheLagIsUnknown(t *testing.T) {
	t.Parallel()
	if got := statelog.DefaultReadLevel(statelog.SurfaceReplication); got != statelog.ReadStale {
		t.Fatalf("a replication answer defaults to %q — a node cannot report "+
			"its own lag at a level that requires the broker it is reporting "+
			"about", got)
	}
	if got := statelog.ResolveReplicationLevel(true); got != statelog.ReadStale {
		t.Errorf("a measured lag resolved to %q, want stale", got)
	}
	if got := statelog.ResolveReplicationLevel(false); got != statelog.ReadConsistentPrefix {
		t.Errorf("an unmeasurable lag resolved to %q — `stale` is a claim "+
			"about age and there is no age to claim", got)
	}
	// AND NO CALLER MAY ASK FOR ANYTHING. The level is DERIVED from a
	// fact about the moment, so it is not a preference anybody holds.
	if levels := statelog.SettableLevels(statelog.SurfaceReplication); len(levels) != 0 {
		t.Errorf("a replication answer offers %v — an operator surface where "+
			"the caller chooses how stale its evidence may be is not an audit "+
			"surface", levels)
	}
}

// A LEVEL IS NOT A MODEL'S TO PICK. A tool argument for it would be a model
// trading correctness for latency it cannot perceive — so a seat's and an
// operator's asks are ignored, and only a dashboard chooses.
func TestOnlyTheDashboardChoosesItsLevel(t *testing.T) {
	t.Parallel()
	for _, surface := range statelog.Surfaces {
		settable := statelog.SettableLevels(surface)
		if want := surface == statelog.SurfaceDashboard; (len(settable) > 0) != want {
			t.Errorf("SettableLevels(%q) = %v, want a choice: %v",
				surface, settable, want)
		}
		// EVERY LEVEL, not just the interesting one: a surface that
		// honoured three asks and ignored the fourth would pass a test
		// that named only the fourth.
		for _, asked := range statelog.ReadLevels {
			got := statelog.LevelFor(surface, asked)
			if slices.Contains(settable, asked) {
				if got != asked {
					t.Errorf("%q may ask for %q and got %q", surface, asked, got)
				}
				continue
			}
			if got != statelog.DefaultReadLevel(surface) {
				t.Errorf("%q may not ask for %q and it changed the level to %q",
					surface, asked, got)
			}
		}
		// AND NO SURFACE OFFERS `session`, on which see
		// [statelog.SettableLevels]: it waits for a position the caller
		// has to supply, and no caller out here can.
		if slices.Contains(settable, statelog.ReadSession) {
			t.Errorf("%q offers session, which it cannot honour — the read "+
				"would wait for the zero position and label a stale answer "+
				"`session`", surface)
		}
		// AND A LEVEL OFFERED IS A LEVEL THAT EXISTS.
		for _, offered := range settable {
			if !offered.Valid() {
				t.Errorf("%q offers %q, which is not a read level",
					surface, offered)
			}
		}
	}
}

// AN UNKNOWN OR ABSENT LEVEL FALLS TO THE SURFACE'S DEFAULT rather than to the
// zero value, which is the empty string and matches no level at all.
func TestAnUnknownAskFallsToTheDefault(t *testing.T) {
	t.Parallel()
	for _, asked := range []statelog.ReadLevel{"", "eventually", "LINEARIZABLE"} {
		if got := statelog.LevelFor(statelog.SurfaceDashboard, asked); got !=
			statelog.DefaultReadLevel(statelog.SurfaceDashboard) {

			t.Errorf("asking for %q gave %q rather than the default", asked, got)
		}
	}
	// AND AN UNKNOWN SURFACE READS linearizable, which is the fail-safe
	// direction: the strongest answer is the one that is never wrong,
	// only slower.
	if got := statelog.DefaultReadLevel(statelog.Surface("invented")); got !=
		statelog.ReadLinearizable {

		t.Errorf("an unknown surface reads at %q, want the strongest", got)
	}
	if statelog.Surface("invented").Valid() {
		t.Error("an invented surface reports itself valid")
	}
	if !slices.Contains(statelog.Surfaces, statelog.SurfaceSeat) {
		t.Error("the surface list does not contain the seat")
	}
}
