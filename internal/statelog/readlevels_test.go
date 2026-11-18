package statelog_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY SURFACE HAS A DECIDED LEVEL: the table is the decision, and a surface
// added without a row fails here rather than inheriting whatever the last
// author typed, as a literal `session` at a seat once did (it waited for a
// zero high-water mark and served this node's committed prefix).
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
		// No consistent_prefix default: it is weaker than session in a
		// way the answer does not show.
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

// A SEAT AND AN OPERATOR READ ALIKE, asserted rather than left to two rows
// that happen to match: the operator MCP serves a seat's own tools, so its
// level is inherited and would drift when the other is edited.
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
// operator's, and `consistent_prefix` when this node could not measure its
// lag: `linearizable` needs the barrier whose health is the subject, and
// `stale` claims an age an unmeasured lag cannot give, so the weaker level is
// named rather than served under the stronger name
// ([statelog.SurfaceReplication], [statelog.ResolveReplicationLevel]).
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
	// No caller may ask: the level is derived from a fact about the moment.
	if levels := statelog.SettableLevels(statelog.SurfaceReplication); len(levels) != 0 {
		t.Errorf("a replication answer offers %v — an operator surface where "+
			"the caller chooses how stale its evidence may be is not an audit "+
			"surface", levels)
	}
}

// A LEVEL IS NOT A MODEL'S TO PICK: a seat's and an operator's asks are
// ignored, and only a dashboard chooses.
func TestOnlyTheDashboardChoosesItsLevel(t *testing.T) {
	t.Parallel()
	for _, surface := range statelog.Surfaces {
		settable := statelog.SettableLevels(surface)
		want := surface == statelog.SurfaceDashboard
		if (len(settable) > 0) != want {
			t.Errorf("SettableLevels(%q) = %v, want a choice: %v",
				surface, settable, want)
		}
		// Every level: a surface that honoured three asks and ignored the
		// fourth would pass a test naming only the fourth.
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
		// Only the screen offers `session`: only its grammar carries the
		// position it waits for ([statelog.SettableLevels]).
		if got := slices.Contains(settable, statelog.ReadSession); got != want {
			t.Errorf("%q offers session: %v, want %v — the level is honest "+
				"exactly where the caller can name the position it waits "+
				"for, and a wrong label everywhere else", surface, got, want)
		}
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
	// An unknown surface reads linearizable, the fail-safe direction.
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
