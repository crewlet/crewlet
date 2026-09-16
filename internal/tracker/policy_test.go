package tracker_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/tracker"
)

// EVERY STATUS HAS A GROUP, A LABEL AND A DESCRIPTION.
//
// The group is COPIED onto every record that names a status, so a record is
// self-describing without a catalogue read; the description is what an agent
// chooses by, and a status with none is chosen by spelling — which is how a
// model picks "done" for work awaiting a colleague's check.
func TestEveryStatusIsFullyDescribed(t *testing.T) {
	t.Parallel()
	if len(tracker.Statuses) != 6 {
		t.Fatalf("the company has %d statuses; the set is fixed at six",
			len(tracker.Statuses))
	}
	for _, s := range tracker.Statuses {
		if !s.Valid() {
			t.Errorf("%q is in the list and not valid", s)
		}
		if s.Label() == "" || s.Description() == "" {
			t.Errorf("%q has label %q and description %q", s, s.Label(), s.Description())
		}
		if !s.Group().Valid() {
			t.Errorf("%q is in group %q, which is not one of the four", s, s.Group())
		}
	}
	if tracker.Status("blocked").Valid() {
		t.Error("a seventh status was accepted")
	}
}

// EXACTLY ONE STATUS IS CLOSED, AND CANCELLED IS FINISHED WITHOUT BEING
// DELIVERED.
//
// Delivered is THE measurement predicate — velocity, burndown, cycle time,
// children_done and a goal's task targets all read it. No bit is stamped
// anywhere: the status IS the verdict, which is what lets a duplicate closed
// by a merge and a task a sprint swept away both write "cancelled" and vanish
// from velocity without a second field.
func TestDeliveredIsTheOneMeasurementPredicate(t *testing.T) {
	t.Parallel()
	closed := 0
	for _, s := range tracker.Statuses {
		if s.Group() == tracker.GroupClosed {
			closed++
		}
	}
	if closed != 1 {
		t.Fatalf("%d statuses are in the closed group; exactly one is", closed)
	}
	for s, want := range map[tracker.Status]bool{
		tracker.StatusTodo:       false,
		tracker.StatusInProgress: false,
		tracker.StatusInReview:   false,
		tracker.StatusDone:       true,
		tracker.StatusCancelled:  false,
		tracker.StatusClosed:     true,
	} {
		if got := tracker.Delivered(s); got != want {
			t.Errorf("Delivered(%q) = %v, want %v", s, got, want)
		}
	}
	if !tracker.StatusCancelled.Group().Finished() {
		t.Error("cancelled is not in a finished group, so an abandoned task " +
			"would have no finish time and every recent: filter would miss it")
	}
}

// OPEN AND FINISHED PARTITION THE FOUR GROUPS.
func TestTheGroupsPartitionCleanly(t *testing.T) {
	t.Parallel()
	for _, g := range tracker.StatusGroups {
		if g.Open() == g.Finished() {
			t.Errorf("group %q is open=%v finished=%v — a group is one or the "+
				"other and every query predicate assumes it", g, g.Open(), g.Finished())
		}
	}
}

// THE THREE RESOLUTION TIERS, AND THE ORDER IS THE WHOLE RULE.
//
// A query that IS somebody's slug must never be fuzzy-matched against
// everybody else's label — which is how "api" resolves to "apis" on a company
// that has both, and why an exact match at an earlier tier settles it whatever
// the later tiers hold.
func TestResolveShortCircuitsAtTheEarliestTier(t *testing.T) {
	t.Parallel()
	candidates := []named{
		{"api", "APIs"},
		{"apis", "api"},
		{"billing", "Billing"},
	}
	for name, tc := range map[string]struct{ query, want string }{
		"an exact slug beats another candidate's exact label": {"api", "api"},
		"an exact label when no slug matches":                 {"APIs", "api"},
		"a case-insensitive label last":                       {"bILLing", "billing"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := tracker.Resolve(tc.query, candidates)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tc.query, err)
			}
			if got.Ident() != tc.want {
				t.Fatalf("Resolve(%q) = %q, want %q", tc.query, got.Ident(), tc.want)
			}
		})
	}
}

// TWO MATCHES AT ONE TIER IS AMBIGUOUS, AND THE REFUSAL NAMES BOTH.
//
// The caller is usually a model with one round left: "ambiguous" sends it
// guessing, and naming both is something it can act on.
func TestAmbiguityNamesBothCandidates(t *testing.T) {
	t.Parallel()
	_, err := tracker.Resolve("Billing", []named{
		{"billing_a", "Billing"},
		{"billing_b", "Billing"},
	})
	var ambiguous *tracker.ErrAmbiguous
	if !errors.As(err, &ambiguous) {
		t.Fatalf("two exact-label matches resolved with %v", err)
	}
	if len(ambiguous.Matches) != 2 ||
		!strings.Contains(err.Error(), "billing_a") ||
		!strings.Contains(err.Error(), "billing_b") {
		t.Fatalf("the refusal is %q and does not name both", err)
	}
}

// THE TWO SLUG GRAMMARS ARE DIFFERENT ON PURPOSE.
//
// A status, a type and a field are identifiers a tool argument names, so they
// take underscores; a tag is a label people write in prose, so it takes
// hyphens and digits at the front. Merging them means accepting "in-progress"
// as a status or refusing "v2-api" as a tag, and both have been asked for.
func TestTheTwoSlugGrammarsStayApart(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		value     string
		slug, tag bool
	}{
		{"in_progress", true, false},
		{"v2-api", false, true},
		{"api", true, true},
		{"2fa", false, true},
		{"", false, false},
		{"Api", false, false},
		{strings.Repeat("a", 33), false, true},
		{strings.Repeat("a", 65), false, false},
	} {
		if got := tracker.ValidSlug(tc.value); got != tc.slug {
			t.Errorf("ValidSlug(%q) = %v, want %v", tc.value, got, tc.slug)
		}
		if got := tracker.ValidTagSlug(tc.value); got != tc.tag {
			t.Errorf("ValidTagSlug(%q) = %v, want %v", tc.value, got, tc.tag)
		}
	}
}

// named is a candidate for the resolver.
type named struct{ slug, label string }

func (n named) Ident() string { return n.slug }
func (n named) Label() string { return n.label }
