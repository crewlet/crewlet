package membership

import (
	"reflect"
	"testing"
)

// THE ACCOUNT OF A TICK NAMES EVERY TRANSITION ONCE, in the order a log reads
// it — over a real sequence of ticks, so what it names is what the lifecycle
// did: a node joining, a removed node back on probation, a probation ending,
// a member removed off probation and one removed on it, a run opening and a
// run closing.
func TestDiffNamesEveryTransition(t *testing.T) {
	t.Parallel()
	c := company(1, 2, "")
	live := roster(3)
	gone := without(live, "data-02")

	start := first(t, roster(2), c)
	joined := tick(t, start, live, c)
	if got := Diff(start.s, start.d.Members, joined.s, joined.d.Members); len(got.Admitted) != 1 ||
		got.Admitted[0].How != Joined || got.Admitted[0].Member.Node != "data-02" ||
		got.Removed != nil || got.Absent != nil || got.Returned != nil {
		t.Fatalf("a join is accounted as %+v", got)
	}

	opened := tick(t, joined, gone, c)
	want := Changes{Absent: []Opened{{Node: "data-02", Absence: opened.s.Absence["data-02"]}}}
	if got := Diff(joined.s, joined.d.Members, opened.s, opened.d.Members); !reflect.DeepEqual(got, want) {
		t.Fatalf("a run opening is accounted as %+v, want %+v", got, want)
	}

	due := ticks(t, opened, gone, c, OutTicks-2)
	removed := tick(t, due, gone, c)
	want = Changes{Removed: []Departure{{Node: "data-02", Removal: removed.s.Removed["data-02"],
		AbsentTicks: OutTicks}}}
	if got := Diff(due.s, due.d.Members, removed.s, removed.d.Members); !reflect.DeepEqual(got, want) {
		t.Fatalf("a removal is accounted as %+v, want %+v", got, want)
	}

	back := tick(t, removed, live, c)
	m, _ := back.d.Member("data-02")
	want = Changes{Admitted: []Admission{{Member: m, How: OnProbation,
		PlacedAfterTicks: StableTicks - 1}}}
	if got := Diff(removed.s, removed.d.Members, back.s, back.d.Members); !reflect.DeepEqual(got, want) {
		t.Fatalf("a return on probation is accounted as %+v, want %+v", got, want)
	}

	left := tick(t, back, gone, c)
	want = Changes{Removed: []Departure{{Node: "data-02", Removal: left.s.Removed["data-02"],
		OnProbation: true}}}
	if got := Diff(back.s, back.d.Members, left.s, left.d.Members); !reflect.DeepEqual(got, want) {
		t.Fatalf("leaving on probation is accounted as %+v, want %+v", got, want)
	}

	nearly := ticks(t, tick(t, left, live, c), live, c, StableTicks-2)
	trusted := tick(t, nearly, live, c)
	m, _ = trusted.d.Member("data-02")
	want = Changes{Admitted: []Admission{{Member: m, How: Trusted}}}
	if got := Diff(nearly.s, nearly.d.Members, trusted.s, trusted.d.Members); !reflect.DeepEqual(got, want) {
		t.Fatalf("a probation ending is accounted as %+v, want %+v", got, want)
	}

	// A run closing on a member still held: back for the stable span.
	returning := ticks(t, tick(t, joined, gone, c), live, c, StableTicks-1)
	closed := tick(t, returning, live, c)
	want = Changes{Returned: []string{"data-02"}}
	if got := Diff(returning.s, returning.d.Members, closed.s, closed.d.Members); !reflect.DeepEqual(got, want) {
		t.Fatalf("a run closing is accounted as %+v, want %+v", got, want)
	}

	// Nothing changed, nothing named.
	if got := Diff(closed.s, closed.d.Members, closed.s, closed.d.Members); !reflect.DeepEqual(got, Changes{}) {
		t.Fatalf("an unchanged record is accounted as %+v", got)
	}
	for _, a := range []Admit{Joined, OnProbation, Trusted} {
		if !a.Valid() {
			t.Errorf("%q is not valid", a)
		}
	}
	if Admit("rejoined").Valid() {
		t.Error("an unknown admission is valid")
	}
}
