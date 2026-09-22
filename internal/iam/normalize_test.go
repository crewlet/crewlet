package iam

import "testing"

// AN ADDRESS IS MATCHED IN THE FORM A VENDOR SENDS IT.
//
// Inbound Jira and GitHub payloads identify people by address, and a company
// routinely subscribes its seats with a plus-addressed form so vendor mail is
// filterable — so the address on the record and the address in the payload are
// routinely different strings naming one person. The normalised form is
// computed once, at the write, and stored: the alternative is a LOWER(...)
// predicate over every row on every inbound webhook, which cannot use an index
// and has to re-derive the plus rule in SQL.
func TestAnAddressIsMatchedInTheFormAVendorSendsIt(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ in, want string }{
		"already the matched form": {"sarah.chen@example.com", "sarah.chen@example.com"},
		"capitalised":              {"Sarah.Chen@Example.COM", "sarah.chen@example.com"},
		"plus-addressed":           {"notif+sarah-chen@example.com", "notif@example.com"},
		"both":                     {" Notif+Sarah-Chen@Example.com ", "notif@example.com"},
		"no address at all":        {"", ""},
		// A value that is not an address is folded and handed back
		// rather than refused: this runs over whatever a founder typed,
		// and the column it fills is a lookup key. A value that matches
		// nothing is the correct outcome for a value that is nothing.
		"not an address": {"  Sarah Chen ", "sarah chen"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := NormalizeEmail(tc.in); got != tc.want {
				t.Fatalf("NormalizeEmail(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	// AND THE PLUS RULE APPLIES TO THE LOCAL PART ONLY. A domain carrying
	// one is not a tag, and stripping there would collapse two companies
	// onto one address.
	if got := NormalizeEmail("sarah@ex+ample.com"); got != "sarah@ex+ample.com" {
		t.Errorf("NormalizeEmail stripped past the @: %q", got)
	}
}

// AND BOTH DOMAINS THAT MATCH ON IT REACH THE SAME ANSWER.
//
// The rule is one rule: the org chart derives `chart_seats.email_index` from
// it so a vendor payload resolves to a SEAT, and the identity estate derives a
// person's keyed blind from it so a sign-in resolves to a PERSON. Written
// twice, one address would reach a seat and a different person — the two
// halves of "who is this" answering differently about one string.
//
// The case is here rather than in either domain for that reason: a copy in
// each is exactly the shape that lets them drift, and a copy in one is a rule
// the other is not held to.
func TestTheAddressRuleIsOneRuleBothDomainsRead(t *testing.T) {
	t.Parallel()
	const typed = " Notif+Sarah-Chen@Example.com "
	folded := NormalizeEmail(typed)
	if folded != "notif@example.com" {
		t.Fatalf("NormalizeEmail(%q) = %q", typed, folded)
	}
	// IDEMPOTENT, which is what lets a value be folded at the write and
	// folded again at a lookup without the two disagreeing.
	if again := NormalizeEmail(folded); again != folded {
		t.Errorf("folding %q again gave %q — a lookup folds what a write "+
			"already folded, so the second pass must be a no-op", folded, again)
	}
}
