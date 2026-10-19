package httpjson

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/iam"
)

// THE UNAUTHORIZED SENTENCE NAMES EVERY PERMISSION THERE IS, read off the list.
//
// A person refused on a screen reads this sentence to learn what to ask an
// admin for, and it typed the list by hand: the next app's permission would
// have been missing from it the day it shipped, beside a `grants` field naming
// exactly that permission. So the sentence is the list's, in the words People
// & access uses — each capitalised, commas, "or" before the last — and the
// table carries it for [iam.AllGrants].
//
// Mutation: have the builder name a fixed list rather than its argument and
// the rows fail — the four-permission row is the app that does not exist yet;
// give the table any sentence but the builder's and the last check does.
func TestTheUnauthorizedSentenceNamesEveryPermission(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		grants []iam.Grant
		names  string
	}{
		{[]iam.Grant{iam.GrantAdmin}, "(Admin)"},
		{[]iam.Grant{iam.GrantAdmin, iam.GrantWorklet}, "(Admin or Worklet)"},
		{[]iam.Grant{iam.GrantAdmin, iam.GrantWorklet, iam.GrantDoclet, "chatlet"},
			"(Admin, Worklet, Doclet or Chatlet)"},
	} {
		if got := unauthorizedSentence(c.grants); !strings.Contains(got, c.names) {
			t.Errorf("for %v the sentence reads %q, want it to name %s", c.grants, got, c.names)
		}
	}
	if got, want := CodeUnauthorized.Message(), unauthorizedSentence(iam.AllGrants); got != want {
		t.Errorf("the table's sentence is %q, want the one naming every permission "+
			"this build knows: %q", got, want)
	}
}
