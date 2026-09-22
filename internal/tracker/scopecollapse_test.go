package tracker

import (
	"fmt"
	"testing"
)

// A TASK WRITE'S ENUMERATION COLLAPSES ONLY WHEN IT MUST.
//
// # The two failures this sits between
//
// A write on a blocker enumerates its own subject AND every dependent, so a
// blocker at [MaxDependents] needs one term more than [MaxScopeTerms] allows.
// Refused there, that task can never be written again — not closed, not
// renamed, and not even edited to drop the dependent that would bring it back
// under the cap, because that edit is a write on the same subject.
//
// Collapsing EARLY is the opposite failure and just as real: a container term
// defers every write inside the project behind this record, so an ordinary
// task with three dependents claiming its whole project would make a routine
// rename block the board.
//
// So this is two-sided on purpose, at the boundary itself.
func TestATaskScopeCollapsesOnlyWhenItMust(t *testing.T) {
	t.Parallel()
	subject := ScopeSet{Subject: true, Container: "ENG"}
	// A PROJECT MOVE is the other shape [Writer.scopeForDependents] is
	// handed: two object terms already, because the task's rows leave one
	// project's closure and arrive in another's.
	moving := ScopeSet{Terms: []ScopeTerm{
		{Kind: TermObject, Container: "ENG", ID: "blk"},
		{Kind: TermObject, Container: "OPS", ID: "blk"},
	}}
	for _, tc := range []struct {
		name       string
		start      ScopeSet
		dependents int
		want       []ScopeTerm
	}{
		{
			name: "a handful of dependents is enumerated", start: subject,
			dependents: 3,
			want: []ScopeTerm{
				{Kind: TermObject, Container: "ENG", ID: "blk"},
				{Kind: TermObject, Container: "ENG", ID: "d-000"},
				{Kind: TermObject, Container: "ENG", ID: "d-001"},
				{Kind: TermObject, Container: "ENG", ID: "d-002"},
			},
		},
		{
			// EXACTLY THE CAP, subject included, is still enumerated:
			// a collapse here would be over-claiming for nothing.
			name:  "the largest enumeration that fits is enumerated",
			start: subject, dependents: MaxScopeTerms - 1,
			want: nil, // asserted by shape below rather than spelled out.
		},
		{
			name:  "a blocker at its dependent cap collapses to its container",
			start: subject, dependents: MaxDependents,
			want: []ScopeTerm{{Kind: TermContainer, ID: "ENG"}},
		},
		{
			// BOTH CONTAINERS, because both were named: a collapse that
			// kept only the first would under-claim the project the
			// task is leaving, which is the failure the move's own two
			// terms exist to prevent.
			name:  "a move past the cap collapses to both containers",
			start: moving, dependents: MaxScopeTerms - 1,
			want: []ScopeTerm{
				{Kind: TermContainer, ID: "ENG"},
				{Kind: TermContainer, ID: "OPS"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ids := make([]string, 0, tc.dependents)
			for i := range tc.dependents {
				ids = append(ids, fmt.Sprintf("d-%03d", i))
			}
			got := ScopeSet{Subject: tc.start.Subject, Container: tc.start.Container,
				Terms: tc.start.Terms}.withObjects("ENG", "blk", ids)
			if err := got.Validate(); err != nil {
				t.Fatalf("the resulting scope is not writable at all: %v", err)
			}
			if tc.want == nil {
				// THE BOUNDARY CASE: every term still an object, and
				// exactly the cap of them.
				if len(got.Terms) != MaxScopeTerms {
					t.Fatalf("%d dependents produced %d terms, want the cap of %d",
						tc.dependents, len(got.Terms), MaxScopeTerms)
				}
				for _, term := range got.Terms {
					if term.Kind != TermObject {
						t.Fatalf("a scope that fits collapsed anyway: %+v — a "+
							"container term defers every write in the project "+
							"behind this record", got.Terms)
					}
				}
				return
			}
			if len(got.Terms) != len(tc.want) {
				t.Fatalf("%d dependents produced %+v, want %+v",
					tc.dependents, got.Terms, tc.want)
			}
			for i, term := range got.Terms {
				if term != tc.want[i] {
					t.Fatalf("%d dependents produced %+v, want %+v",
						tc.dependents, got.Terms, tc.want)
				}
			}
		})
	}
}

// THE DECIDE'S OWN CHECK ACCEPTS THE COVERING CONTAINER.
//
// [ScopeSet.covers] is what refuses a write whose declared scope is short of
// the dependents its apply will rewrite, and it runs inside the decide against
// the rows that are there NOW. Reading only object terms, it refuses the
// collapsed scope for every dependent — so the collapse would trade a refusal
// at the cap for a refusal one layer down, and the blocker would stay exactly
// as unwritable.
func TestTheDecideAcceptsACollapsedScope(t *testing.T) {
	t.Parallel()
	ids := make([]string, 0, MaxDependents)
	for i := range MaxDependents {
		ids = append(ids, fmt.Sprintf("d-%03d", i))
	}
	collapsed := ScopeSet{Subject: true, Container: "ENG"}.
		withObjects("ENG", "blk", ids)
	if err := collapsed.covers("ENG", ids); err != nil {
		t.Fatalf("the decide refuses the scope the write path just built: %v", err)
	}
	// AND IT IS NOT BLANKET ACCEPTANCE: a container term for somewhere
	// else covers nothing here, or the check would pass on the one shape
	// it exists to catch.
	if err := collapsed.covers("OPS", ids); err == nil {
		t.Error("a container term for ENG was read as covering a write scoped " +
			"to OPS — the check would then accept any scope at all")
	}
}
