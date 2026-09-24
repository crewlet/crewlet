package tracker

import (
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
)

// countingLog counts what reaches the turn, which is all this case asks.
type countingLog struct{ items []types.WorkItem }

func (l *countingLog) Add(item types.WorkItem) { l.items = append(l.items, item) }

// AN `unknown` WRITE CHARGES THE TURN TO NOTHING, whatever else it carries.
//
// The set a turn is charged from holds only records this write appended:
// `applied` and `pending` are, `unknown` may not exist at all. The test is the
// OUTCOME and never the position — the ambiguous path once answered `unknown`
// WITH the position of a record it found rather than one it was acknowledged
// for, and a writer reading the position would have charged the turn on it.
func TestAnUnknownWriteReportsNoItem(t *testing.T) {
	t.Parallel()
	at := statelog.Position{Stream: "S", Generation: 1, Seq: 7}
	item := types.WorkItem{Backend: types.WorkNative, ID: "t-1", Key: "ENG-1", Project: "ENG"}
	cases := map[string]struct {
		result statelog.Result
		want   int
	}{
		"applied":                 {statelog.Result{Outcome: statelog.OutcomeApplied, Position: at}, 1},
		"pending":                 {statelog.Result{Outcome: statelog.OutcomePending, Position: at}, 1},
		"unknown with a position": {statelog.Result{Outcome: statelog.OutcomeUnknown, Position: at}, 0},
		"unknown":                 {statelog.Result{Outcome: statelog.OutcomeUnknown}, 0},
	}
	for name, c := range cases {
		log := &countingLog{}
		w := &Writer{written: log}
		named := item
		w.report(&committing{item: &named}, c.result)
		if len(log.items) != c.want {
			t.Errorf("%s: reported %d item(s), want %d", name, len(log.items), c.want)
		}
	}
}
