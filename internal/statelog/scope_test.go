package statelog_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// CONTAINMENT IS NOT A SHARED ANCESTOR, and the difference is the whole
// predicate.
//
// Two sibling projects sit under the same family, so a probe that read a
// shared ancestor as an intersection would refuse every write in the company
// the moment one record anywhere was deferred. And a probe that only walked
// upward would miss a record deferred on an object BENEATH what a write is
// rewriting — which is silent data loss rather than an over-refusal.
func TestAScopeIntersectsOnlyWhatItActuallyCovers(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		a, b statelog.ScopeSet
		want bool
	}{
		"the same object": {
			a:    statelog.ScopeSet{Paths: []string{"project/ENG/task/7"}},
			b:    statelog.ScopeSet{Paths: []string{"project/ENG/task/7"}},
			want: true,
		},
		"a container covers its object": {
			a:    statelog.ScopeSet{Paths: []string{"project/ENG"}},
			b:    statelog.ScopeSet{Paths: []string{"project/ENG/task/7"}},
			want: true,
		},
		"and an object is covered by its container": {
			a:    statelog.ScopeSet{Paths: []string{"project/ENG/task/7"}},
			b:    statelog.ScopeSet{Paths: []string{"project/ENG"}},
			want: true,
		},
		"two siblings share an ancestor and cover nothing": {
			a:    statelog.ScopeSet{Paths: []string{"project/ENG"}},
			b:    statelog.ScopeSet{Paths: []string{"project/OPS"}},
			want: false,
		},
		"two objects in one project are still two objects": {
			a:    statelog.ScopeSet{Paths: []string{"project/ENG/task/7"}},
			b:    statelog.ScopeSet{Paths: []string{"project/ENG/task/8"}},
			want: false,
		},
		"a prefix that is not a path boundary is not containment": {
			a:    statelog.ScopeSet{Paths: []string{"project/EN"}},
			b:    statelog.ScopeSet{Paths: []string{"project/ENG/task/7"}},
			want: false,
		},
		"one path out of several is enough": {
			a:    statelog.ScopeSet{Paths: []string{"project/OPS", "project/ENG/task/7"}},
			b:    statelog.ScopeSet{Paths: []string{"project/ENG"}},
			want: true,
		},
		"an empty scope is about nothing": {
			a:    statelog.ScopeSet{},
			b:    statelog.ScopeSet{Paths: []string{"project/ENG"}},
			want: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.a.Intersects(tc.b); got != tc.want {
				t.Errorf("%v.Intersects(%v) = %v, want %v", tc.a.Paths, tc.b.Paths, got, tc.want)
			}
			// SYMMETRIC, always. The probe runs the query's scope
			// against a stored one and a stored one against the
			// query's, and an asymmetric predicate would answer
			// differently depending on which side the deferred record
			// happened to be.
			if got := tc.b.Intersects(tc.a); got != tc.want {
				t.Errorf("reversed = %v, want %v — the predicate must be "+
					"symmetric or the probe answers differently depending on "+
					"which side a deferred record sits", got, tc.want)
			}
		})
	}
}

// THE PROBE'S TWO CLAUSES ARE THE TWO DIRECTIONS, and each one alone misses
// the other's case.
//
// The closure walks UP — every path that could cover something the query is
// about. The roots are what a descendant test walks DOWN from. A probe with
// only the first is blind to a record deferred on one task inside a project
// this write is rewriting.
func TestTheClosureAndTheRootsAreTheTwoHalvesOfTheProbe(t *testing.T) {
	t.Parallel()
	s := statelog.ScopeSet{Paths: []string{"project/ENG/task/7"}}

	closure := s.Closure()
	want := []string{"project", "project/ENG", "project/ENG/task", "project/ENG/task/7"}
	if len(closure) != len(want) {
		t.Fatalf("Closure() = %v, want %v", closure, want)
	}
	for i := range want {
		if closure[i] != want[i] {
			t.Fatalf("Closure() = %v, want %v", closure, want)
		}
	}

	// A NESTED PATH IS NOT ITS OWN ROOT. Testing a descendant against both
	// a path and its own ancestor asks the same question twice.
	nested := statelog.ScopeSet{Paths: []string{"project/ENG", "project/ENG/task/7", "project/OPS"}}
	roots := nested.Roots()
	if len(roots) != 2 || roots[0] != "project/ENG" || roots[1] != "project/OPS" {
		t.Fatalf("Roots() = %v, want [project/ENG project/OPS]", roots)
	}
}

// A PATH IS NORMALISED BEFORE IT IS STORED, so two records declaring the same
// objects in a different order produce the same rows — which is what lets the
// identity claim compare them at all.
func TestAScopeIsNormalisedBeforeItIsStored(t *testing.T) {
	t.Parallel()
	s := statelog.ScopeSet{Paths: []string{
		" project/ENG ", "/project/OPS/", "project/ENG", "", "   ",
	}}
	got := s.Normalised().Paths
	if len(got) != 2 || got[0] != "project/ENG" || got[1] != "project/OPS" {
		t.Fatalf("Normalised() = %v, want [project/ENG project/OPS]", got)
	}
}
