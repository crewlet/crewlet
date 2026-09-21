package tracker

import (
	"slices"
	"testing"
)

// EVERY CLOSED AXIS HAS AN ADMISSION ARM, and with nothing narrowing the query
// it admits the whole declared set.
//
// [groupAxis.Order] is what marks an axis closed and [admittedColumns] switches
// on the key by name, so an axis that gained an order without gaining an arm
// would silently fall back to "only what is present" — which is the state this
// fill exists to end. Walked over the grammar's own list so a grouping added
// to it cannot slip past.
func TestEveryClosedAxisAdmitsItsWholeSetWhenNothingNarrowsIt(t *testing.T) {
	t.Parallel()
	closed := 0
	for _, key := range groupKeys {
		axis, err := compileGroup(key, nil)
		if err != nil {
			t.Fatalf("compile %s: %v", key, err)
		}
		q := Query{GroupBy: key, ShowClosed: ShowClosed{All: true}}
		got := admittedColumns(q, axis)
		if len(axis.Order) == 0 {
			if got != nil {
				t.Fatalf("%s is an open axis and admitted %v", key, got)
			}
			continue
		}
		closed++
		if !slices.Equal(got, axis.Order) {
			t.Fatalf("%s admits %v with nothing narrowing it, want its whole "+
				"declared set %v", key, got, axis.Order)
		}
	}
	if closed != 3 {
		t.Fatalf("%d closed axes, want the three (status, status_group, priority)", closed)
	}
}

// A VALUE THE FILL DOES NOT KNOW KEEPS ITS COLUMN. The predicate admitted it
// and it has rows, so it lands after the declared set rather than vanishing.
func TestFillColumnsKeepsAPresentColumnOutsideTheDeclaredSet(t *testing.T) {
	t.Parallel()
	present := []Group{{Key: "in_review", Count: 2}, {Key: "triage", Count: 1}}
	got := fillColumns(present, []string{"todo", "in_review"})
	want := []string{"todo", "in_review", "triage"}
	for i, group := range got {
		if group.Key != want[i] {
			t.Fatalf("the filled columns are %v, want %v", got, want)
		}
	}
	if got[0].Count != 0 || got[0].Rows == nil {
		t.Fatalf("the minted column is %+v, want count 0 and an empty row list", got[0])
	}
	if got[1].Count != 2 || got[2].Count != 1 {
		t.Fatalf("the present columns lost their counts: %+v", got)
	}
}
