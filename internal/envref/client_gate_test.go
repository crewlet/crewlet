package envref_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/envref"
)

// THE DASHBOARD READS A REFERENCE BY THE ENGINE'S GRAMMAR.
//
// Every screen that decides whether a value is a `${NAME}` reference — to mask
// a credential field, to tell a provisionable value from a hand-written one,
// to label a configuration value — builds its expression from the one name the
// contract declares. Five screens each spelled the grammar for themselves, and
// nothing held any of them to this one: a copy that drifted would mask a
// reference as a secret, or show a secret as a reference.
func TestTheDashboardReadsAReferenceByTheEnginesGrammar(t *testing.T) {
	t.Parallel()
	client, err := clientsource.Scalar(clientsource.Tree(t), "REFERENCE_NAME")
	if err != nil {
		t.Fatal(err)
	}
	if client != envref.NameGrammar {
		t.Errorf("the dashboard reads a reference's name as %q and the engine as %q — "+
			"change REFERENCE_NAME in contract/references.ts to the engine's", client,
			envref.NameGrammar)
	}
}
