package tracker

import (
	"slices"
	"testing"
	"time"
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
	// A RESOLVED CALENDAR, because one axis refuses to compile without one
	// — see [dueBucketAxis] — and a zero window is the silent answer it
	// refuses in order to avoid.
	start := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	window := dayWindow{
		Start:   start,
		DayEnd:  start.AddDate(0, 0, 1),
		WeekEnd: start.AddDate(0, 0, 5),
	}
	closed := 0
	for _, key := range groupKeys {
		axis, err := compileGroup(key, nil, window)
		if err != nil {
			t.Fatalf("compile %s: %v", key, err)
		}
		q := Query{
			GroupBy: key, ShowClosed: ShowClosed{All: true},
			DayStart: window.Start, DayEnd: window.DayEnd, WeekEnd: window.WeekEnd,
		}
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
	if closed != 4 {
		t.Fatalf("%d closed axes, want the four (status, status_group, "+
			"priority and the relative due bands)", closed)
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

// THE RELATIVE DUE BANDS ARE NARROWED BY TWO DIFFERENT FILTERS, and this walks
// both halves of [admittedDueBands] one narrowing at a time.
//
// The axis is the one closed set whose columns are not a stored value, so
// which of them a query could hold is arithmetic rather than a lookup: the
// status half decides Overdue and Earlier, the `due=` filter decides the four
// that are intervals, and the alias is the one value that is both at once.
// Each case names what it narrows and the bands that survive it.
func TestWhichDueBandsAQueryAdmits(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC) // a Wednesday
	dayEnd := start.AddDate(0, 0, 1)
	weekEnd := start.AddDate(0, 0, 5) // the following Monday
	at := func(d int) DateAnchor {
		return DateAnchor{At: start.AddDate(0, 0, d), AllDay: true}
	}
	named := func(v string) *string { return &v }

	for _, tc := range []struct {
		name string
		q    Query
		want []string
	}{{
		name: "nothing narrows it, and finished work is asked for",
		q:    Query{ShowClosed: ShowClosed{All: true}},
		want: []string{"overdue", "earlier", "today", "this_week", "later", ""},
	}, {
		// THE DEFAULT BOARD IS OPEN WORK, so nothing in it can be work
		// that was finished late — which is the whole of what Earlier is.
		name: "open work only, which is the default",
		q:    Query{},
		want: []string{"overdue", "today", "this_week", "later", ""},
	}, {
		// AND THE MIRROR: a board of finished work has no open row to be
		// past its date, so Overdue would claim nothing is late about a
		// set that could not hold a late thing.
		name: "narrowed to one finished status",
		q: Query{
			Status: []Status{StatusDone}, ShowClosed: ShowClosed{All: true},
		},
		want: []string{"earlier", "today", "this_week", "later", ""},
	}, {
		name: "the overdue alias, which carries the open condition",
		q: Query{
			ShowClosed: ShowClosed{All: true},
			Dates: map[string]DateFilter{
				"due": {Op: DateLT, From: at(0), Overdue: true},
			},
		},
		want: []string{"overdue"},
	}, {
		// THE SAME INTERVAL WITHOUT THE ALIAS is both bands before the
		// day start, because a task finished late is in it too.
		name: "everything before today, finished work included",
		q: Query{
			ShowClosed: ShowClosed{All: true},
			Dates:      map[string]DateFilter{"due": {Op: DateLT, From: at(0)}},
		},
		want: []string{"overdue", "earlier"},
	}, {
		name: "the next seven days",
		q: Query{
			ShowClosed: ShowClosed{All: true},
			Dates: map[string]DateFilter{
				"due": {Op: DateRange, From: at(0), To: at(7)},
			},
		},
		want: []string{"today", "this_week", "later"},
	}, {
		name: "from the end of next week on",
		q: Query{
			ShowClosed: ShowClosed{All: true},
			Dates:      map[string]DateFilter{"due": {Op: DateGTE, From: at(12)}},
		},
		want: []string{"later"},
	}, {
		// A DATE FILTER ON ANOTHER COLUMN NARROWS NOTHING HERE. `created=`
		// bounds when the row was written, which says nothing about when
		// its work is due.
		name: "a bound on a different date column",
		q: Query{
			ShowClosed: ShowClosed{All: true},
			Dates:      map[string]DateFilter{"created": {Op: DateGTE, From: at(-30)}},
		},
		want: []string{"overdue", "earlier", "today", "this_week", "later", ""},
	}, {
		// `group=` IS READ BY PRESENCE, and the empty string names the
		// band holding the rows with no due date — which on this axis is
		// a declared column rather than an absence.
		name: "group= naming the undated band",
		q: Query{
			ShowClosed: ShowClosed{All: true}, Group: named(""),
		},
		want: []string{""},
	}, {
		name: "group= naming one band",
		q: Query{
			ShowClosed: ShowClosed{All: true}, Group: named("today"),
		},
		want: []string{"today"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := tc.q
			q.GroupBy = groupByDueBucket
			q.DayStart, q.DayEnd, q.WeekEnd = start, dayEnd, weekEnd
			axis, err := compileGroup(groupByDueBucket, nil, q.dayWindow())
			if err != nil {
				t.Fatalf("compile the due axis: %v", err)
			}
			if got := admittedColumns(q, axis); !slices.Equal(got, tc.want) {
				t.Errorf("admits %q, want %q", got, tc.want)
			}
		})
	}
}

// AND A PRESENT-EMPTY `group=` ON AN AXIS WITH NO UNSET COLUMN PADS NOTHING.
//
// A status is never empty and neither is a priority, so the empty string names
// no declared value there — and a board narrowed to a column the axis does not
// have is honestly a board with no column to draw, rather than the whole board
// the absent narrowing would have given.
func TestAPresentEmptyGroupAdmitsNothingWhereTheAxisHasNoUnsetColumn(t *testing.T) {
	t.Parallel()
	empty := ""
	for _, key := range []string{"status", "status_group", "priority"} {
		axis, err := compileGroup(key, nil, dayWindow{})
		if err != nil {
			t.Fatalf("compile %s: %v", key, err)
		}
		q := Query{GroupBy: key, ShowClosed: ShowClosed{All: true}, Group: &empty}
		if got := admittedColumns(q, axis); got != nil {
			t.Errorf("%s admits %q for a present-empty group=, want nothing", key, got)
		}
	}
}
