package authz

import "testing"

// EVERY REASON HAS ONE CLASS IN THE FOLD.
//
// [Combine] places a refusal by its reason's class, and a reason in none would
// be folded as a refusal on authority whatever it means — a new bar on the
// caller told to ask for a permission. Every reason this build decides by is
// held to exactly one class, and every bar to a place in [Decide]'s order.
// Mutation: add a reason to [Reasons] without a class, or a bar without a
// place, and this fails.
func TestEveryReasonHasOneClassInTheFold(t *testing.T) {
	t.Parallel()
	for _, r := range Reasons {
		switch fold[r] {
		case foldAdmission, foldAuthority, foldProof:
		case foldBar:
			if barRank(r) == len(barOrder) {
				t.Errorf("the bar %s has no place in Decide's order", r)
			}
		default:
			t.Errorf("the reason %s has no class in the fold", r)
		}
	}
	if len(fold) != len(Reasons) {
		t.Errorf("the fold classes %d reasons and this build decides by %d",
			len(fold), len(Reasons))
	}
	for _, r := range barOrder {
		if fold[r] != foldBar {
			t.Errorf("%s is in the bars' order and is no bar", r)
		}
	}
}
