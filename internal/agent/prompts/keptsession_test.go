package prompts

import (
	"strings"
	"testing"
)

// A FRESH BRIEF SAYS ITS TURN'S EARLIER SESSION IS KEPT UNTIL ITS RUN SAVES:
// this run's first capture, a save or its end's, replaces that session on the
// turn's row, so the run is told it is kept only until then, a fire in the
// schedule's words, and given no time. The last turn's is another row, which
// nothing this turn keeps replaces, so its words carry no such clause.
//
// Mutation: tell this turn's earlier launches' session in the old words, or
// tell the last turn's with the clause, and a case goes red.
func TestAFreshBriefSaysItsTurnsEarlierSessionIsKeptUntilItsRunSaves(t *testing.T) {
	t.Parallel()
	const clause = "kept until this run first saves its own"
	for _, c := range []struct {
		name           string
		thisTurn, fire bool
		want           string
	}{
		{name: "this turn's earlier launches", thisTurn: true,
			want: "This turn's earlier launches worked in a session this run does not continue, kept until this " +
				"run first saves its own. Until then, read what they did with `read_session`, turn_id `t-1`; " +
				"`match` finds a word in it."},
		{name: "this fire's earlier launches", thisTurn: true, fire: true,
			want: "This fire's earlier launches worked in a session this run does not continue, kept until this " +
				"run first saves its own. Until then, read what they did with `read_session`, turn_id `t-1`; " +
				"`match` finds a word in it."},
		{name: "the conversation's last turn",
			want: "This conversation's last turn worked in a session this run does not continue. Read what it " +
				"did with `read_session`, turn_id `t-1`; `match` finds a word in it."},
		{name: "the schedule's last fire", fire: true,
			want: "This schedule's last fire worked in a session this run does not continue. Read what it " +
				"did with `read_session`, turn_id `t-1`; `match` finds a word in it."},
	} {
		got := KeptSession("t-1", c.thisTurn, c.fire)
		if want := keptSessionHeader + "\n" + c.want; got != want {
			t.Errorf("%s: the brief says\n%s\nwant\n%s", c.name, got, want)
		}
		if strings.Contains(got, clause) != c.thisTurn {
			t.Errorf("%s: the brief says %q is %v, want %v:\n%s", c.name, clause,
				strings.Contains(got, clause), c.thisTurn, got)
		}
	}
}
