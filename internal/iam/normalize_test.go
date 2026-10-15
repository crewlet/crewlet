package iam

import (
	"strings"
	"testing"
)

// AN ADDRESS IS MATCHED IN THE FORM A VENDOR SENDS IT.
//
// Inbound Jira and GitHub payloads identify people by address, and people
// routinely sign a vendor up with a sub-addressed form so its mail is
// filterable — so the address somebody declared and the address in the payload
// are routinely different strings naming one mailbox.
func TestAnAddressIsMatchedInTheFormAVendorSendsIt(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ in, want string }{
		"already the matched form": {"sarah.chen@example.com", "sarah.chen@example.com"},
		"capitalised":              {"Sarah.Chen@Example.COM", "sarah.chen@example.com"},
		"sub-addressed":            {"sarah.chen+jira@example.com", "sarah.chen@example.com"},
		"both":                     {" Sarah.Chen+Jira@Example.com ", "sarah.chen@example.com"},
		"no address at all":        {"", ""},
		// A value that is not an address is folded and handed back
		// rather than refused: this runs over whatever a founder typed,
		// and what it yields is a lookup key. A value that matches
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
// The rule is one rule: the party registry folds a seat's declared address
// and a payload's with it so a vendor payload resolves to a SEAT, and the
// identity estate derives a person's keyed blind from it so a sign-in resolves
// to a PERSON. Written twice, one address would reach a seat and a different
// person — the two halves of "who is this" answering differently about one
// string.
//
// The case is here rather than in either domain for that reason: a copy in
// each is exactly the shape that lets them drift, and a copy in one is a rule
// the other is not held to.
func TestTheAddressRuleIsOneRuleBothDomainsRead(t *testing.T) {
	t.Parallel()
	const typed = " Sarah.Chen+Jira@Example.com "
	folded := NormalizeEmail(typed)
	if folded != "sarah.chen@example.com" {
		t.Fatalf("NormalizeEmail(%q) = %q", typed, folded)
	}
	// IDEMPOTENT, which is what lets a value be folded where it is stored
	// or indexed and folded again at a lookup without the two disagreeing.
	if again := NormalizeEmail(folded); again != folded {
		t.Errorf("folding %q again gave %q — a lookup folds what a write "+
			"already folded, so the second pass must be a no-op", folded, again)
	}
}

// A PROPOSED LOGIN IS ALWAYS ONE THE PERSON GRAMMAR ACCEPTS, OR NOTHING.
//
// Every person enrols with a login, and somebody redeeming an invitation is
// shown one proposed from their address. A proposal outside the grammar would
// be a form that refuses its own default — which is what `jane@example.com`
// produced before the domain's first label became the second segment, since a
// login without a dot is a name an audit row cannot tell from a seat's.
func TestAProposedLoginFitsThePersonGrammarOrIsNothing(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ in, want string }{
		"a dotted local part":         {"Jane.Doe@example.com", "jane.doe"},
		"a plus tag is not a name":    {"jane.doe+crewlet@example.com", "jane.doe"},
		"one segment borrows a label": {"jane@acme.example.com", "jane.acme"},
		"an underscore separates":     {"jane_doe@example.com", "jane.doe"},
		"punctuation separates once":  {"o'brien..x@example.com", "o.brien.x"},
		"a hyphen only inside a word": {"--jane--doe-@example.com", "jane-doe.example"},
		"hyphens survive in a word":   {"mary-jane.watson@example.com", "mary-jane.watson"},
		// AT THE BOUND AND ONE PAST IT: fifty-six letters and `.example` is
		// sixty-four bytes, and a fifty-seventh is a login no enrolment
		// would accept — so it proposes nothing rather than a cut name.
		"a proposal at the bound": {strings.Repeat("j", 56) + "@example.com",
			strings.Repeat("j", 56) + ".example"},
		"a proposal past the bound": {strings.Repeat("j", 57) + "@example.com", ""},
		"no local part":             {"@example.com", ""},
		"no domain to borrow from":  {"jane@", ""},
		"not an address":            {"jane.doe", ""},
		"nothing":                   {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := LoginFromAddress(tc.in)
			if got != tc.want {
				t.Errorf("LoginFromAddress(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if got != "" && !ValidLoginFor(KindPerson, got) {
				t.Errorf("LoginFromAddress(%q) proposed %q, which the person "+
					"grammar refuses", tc.in, got)
			}
		})
	}
}
