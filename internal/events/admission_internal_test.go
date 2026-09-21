package events

import (
	"strings"
	"testing"
)

// THE ADMISSION RULE: NO EVENT TYPE WHOSE RATE AN UNAUTHENTICATED CALLER
// AUTHORS REACHES THE NODE ESTATE.
//
// The estate is a local file on a disk an operator sized, and every artefact
// taken from it — the backup, the snapshot a lagging node installs, the
// integrity check before either counts — is a copy of the whole thing. A type
// whose rate anybody who can reach the listener decides therefore hands the
// size of all four to whoever is making the requests, and it does it without
// a credential to revoke or an identity to attribute.
//
// A FAILED LOGIN IS THE WORKED EXAMPLE, and the reason this rule is written
// down before the types that need it exist: an anonymous caller authors
// millions of them a day for nothing, so the per-attempt fact belongs on a
// metrics counter and only a coalesced event — per source, per minute,
// published by the engine's own loop and therefore [RateEngine] — is a row.
// Nothing about that is obvious from either side of the decision when somebody
// is adding an event type at the time, which is what a walk is for.
func TestNoTypeAnAnonymousCallerRatesReachesTheStore(t *testing.T) {
	t.Parallel()
	if got := AdmissionViolations(); len(got) > 0 {
		t.Errorf("the taxonomy breaks the estate's admission rule:\n  %s",
			strings.Join(got, "\n  "))
	}

	// THE CONTROL, three ways, because a walk over a correct table proves
	// nothing about what it would do with a wrong one — and each of these
	// is a shape a later change can actually arrive in.
	for _, tc := range []struct {
		name   string
		placed map[string]placement
		kept   map[string]Exclusion
		want   string
	}{
		{
			name:   "a type an anonymous caller rates, given a category",
			placed: map[string]placement{"login_failed": {"system", RateAnonymous}},
			want:   "unauthenticated caller",
		},
		{
			name:   "a type placed with no rate author at all",
			placed: map[string]placement{"login_failed": {"system", ""}},
			want:   "states no rate author",
		},
		{
			name:   "a type both categorised and excluded",
			placed: map[string]placement{"login_failed": {"system", RateEngine}},
			kept: map[string]Exclusion{"login_failed": {
				Cause: CauseAnonymousRate, Reason: "the per-attempt fact is a counter",
			}},
			want: "excludes nothing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := admissionViolations(tc.placed, tc.kept)
			if len(got) != 1 {
				t.Fatalf("violations = %v, want exactly one: the walk passes a "+
					"taxonomy that breaks the rule, so it is asserting nothing", got)
			}
			if !strings.Contains(got[0], "login_failed") ||
				!strings.Contains(got[0], tc.want) {
				t.Errorf("violation = %q, want it to name login_failed and %q: a "+
					"report that does not say which type or what is wrong with it "+
					"is a red build nobody can act on", got[0], tc.want)
			}
		})
	}
}

// EVERY EXCLUSION STATES A TYPED CAUSE, not only a sentence.
//
// The sentence is for a person looking for a type and not finding it. The
// cause is the same decision in the form a walk can read, and it is what makes
// [AdmissionViolations] a rule rather than a convention: "this is already a
// row" and "an unauthenticated caller sets this rate" are one English shrug to
// a regexp and two different decisions to the engine.
func TestEveryExclusionHasATypedCause(t *testing.T) {
	t.Parallel()
	for eventType, why := range excluded {
		if !why.Cause.Valid() {
			t.Errorf("%s is excluded with cause %q, which is not one this build "+
				"declares: a cause nothing can read is a sentence with extra "+
				"punctuation", eventType, why.Cause)
		}
		if len(why.Reason) < 40 {
			t.Errorf("%s is excluded with reason %q; say why it must stay out, "+
				"for whoever comes looking for the type and does not find it",
				eventType, why.Reason)
		}
	}
	// The control: the zero cause is what an entry written without one
	// carries, so the guard above is worth having only if that value fails
	// it.
	if ExclusionCause("").Valid() {
		t.Error("the zero ExclusionCause is Valid, so an exclusion that states " +
			"no cause passes the guard above")
	}
	if ExclusionCause("it just is").Valid() {
		t.Error("a cause this build does not declare reported Valid")
	}
}

// EVERY RATE AUTHOR IS ONE OF THREE, and the zero value is not one.
//
// The zero value is the whole reason [placement] carries the author rather
// than a separate table doing it: an entry added without one is caught by the
// admission walk, where an entry missing from a table beside this one is
// caught by nothing.
func TestTheZeroRateAuthorIsNotAClaim(t *testing.T) {
	t.Parallel()
	for _, author := range []RateAuthor{RateEngine, RateAuthenticated, RateAnonymous} {
		if !author.Valid() {
			t.Errorf("%q is declared but not Valid", author)
		}
	}
	if RateAuthor("").Valid() {
		t.Error("the zero RateAuthor is Valid: an entry that states no author " +
			"would read as a claim that its rate is bounded")
	}
	if RateAuthor("somebody").Valid() {
		t.Error("an author this build does not declare reported Valid")
	}
}
