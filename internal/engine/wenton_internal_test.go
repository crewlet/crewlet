package engine

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/executorkeep"
)

// A HEAD ITS TURN WENT ON PAST IS HANDED THE NEXT TURN AS ONE. A turn's dispatch
// reads its conversation's head, and its first launch freezes it as the base it
// continues: where the head's end said its turn went on past the session its
// row names, the base the launch is answered says so, with when that session
// was kept — what the run is told where the session ends — and where the turn
// was lost, it says that apart, so a lost turn whose end said nothing of going
// on is told only what a lost turn's end can say. Through the row's one view of
// itself, the frozen base and the basis the engine builds from it. A head
// whose turn ended on its last word is handed as one.
//
// Mutation: leave WentOn, Lost or CapturedAt off the frozen base, or off the
// basis the engine builds from it, and the next turn is told the session runs
// through its turn's end; join the two, and a lost turn is told its session was
// kept while it still worked.
func TestAHeadItsTurnWentOnPastIsHandedTheNextTurnAsOne(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name         string
		ending       executorkeep.Ending
		wentOn, lost bool
	}{
		{name: "its end said it went on past the session",
			ending: executorkeep.Ending{Outcome: executorkeep.OutcomeStopped, WentOn: true}, wentOn: true},
		{name: "it was lost", ending: executorkeep.Ending{Outcome: executorkeep.OutcomeLost}, lost: true},
		{name: "its end said it went on past the session, and it was lost",
			ending: executorkeep.Ending{Outcome: executorkeep.OutcomeLost, WentOn: true}, wentOn: true, lost: true},
		{name: "it ended on its last word", ending: executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			k := newConversingLaunch(t, 0, 0)
			row := k.log.endedTurn("b1", "s-b1", "B1's transcript", 42000)
			kept := row.EndedAt.Add(-time.Hour)
			ending := c.ending
			row.Ending, row.CapturedAt = &ending, kept
			k.log.mu.Lock()
			k.log.rows["b1"] = row
			k.log.mu.Unlock()

			basis := k.launcher("t2", k.read("t2")).ProbeSession(k.ctx)
			if !basis.ContinuesBase() || basis.Base.Turn != "b1" {
				t.Fatalf("the first launch is answered %+v, want b1's session", basis)
			}
			if b := basis.Base; b.WentOn != c.wentOn || b.Lost != c.lost || !b.CapturedAt.Equal(kept) {
				t.Errorf("the base is handed went on %v, lost %v, kept at %v; want %v, %v, %v", b.WentOn, b.Lost,
					b.CapturedAt, c.wentOn, c.lost, kept)
			}
		})
	}
}
