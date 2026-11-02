package authz_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE DASHBOARD HAS A SENTENCE FOR EVERY RULE THE ENGINE DECIDES BY, and for
// nothing else.
//
// A refusal on authority that names no access — `grants` empty, because none
// would admit the caller — is drawn in the sentence `REFUSAL_SENTENCES` keeps
// for the deciding rule's [authz.Reason]. One sentence for all of them told a
// credential bound to no seat, and a token on a gesture that needs the person
// present, that what they lacked was leading somebody; so the table is keyed
// by the engine's closed set, and held here in both directions. A reason the
// engine grew with no row is drawn as a claim about a rule that did not
// decide; a row for a reason nothing reports is what a rename leaves behind.
//
// Mutation: drop a reason from the table, or add one [authz.Reasons] does not
// have, and this fails.
func TestTheDashboardSaysEveryReasonTheEngineDecidesBy(t *testing.T) {
	t.Parallel()
	table, err := clientsource.Literal(clientsource.Tree(t), "REFUSAL_SENTENCES")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := clientsource.Keys(table)
	if err != nil {
		t.Fatal(err)
	}
	said := map[string]bool{}
	for _, key := range keys {
		said[key] = true
	}
	if len(said) == 0 {
		t.Fatal("REFUSAL_SENTENCES reads as empty, so this gate certifies nothing")
	}
	for _, reason := range authz.Reasons {
		if !said[string(reason)] {
			t.Errorf("the engine decides by %q and the dashboard has no row for it, "+
				"so a refusal it reports with no grants is told another rule's sentence", reason)
		}
		delete(said, string(reason))
	}
	for leftover := range said {
		t.Errorf("the dashboard keeps a sentence for %q and no rule reports it — a "+
			"renamed reason leaves exactly this behind", leftover)
	}
}
