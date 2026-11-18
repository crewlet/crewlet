package worklet

import (
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
)

// countingLog counts what reaches the turn.
type countingLog struct{ items []types.WorkItem }

func (l *countingLog) Add(item types.WorkItem) { l.items = append(l.items, item) }

func (l *countingLog) Commit(types.CommittedChange) {}

// AN `unknown` WRITE CHARGES THE TURN TO NOTHING, whatever else it carries:
// a turn is charged only for records this write appended (`applied`,
// `pending`), and an `unknown` one may not exist. The writer keys on the OUTCOME, never the position, since an
// `unknown` result may carry the position of a record found rather than one
// acknowledged.
func TestAnUnknownWriteReportsNoItem(t *testing.T) {
	t.Parallel()
	at := statelog.Position{Stream: "S", Generation: 1, Seq: 7}
	item := types.WorkItem{Backend: types.WorkWorklet, ID: "t-1", Key: "ENG-1", Project: "ENG"}
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
