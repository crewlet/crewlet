package caveat_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE DASHBOARD READS WHAT A CAVEAT ASKS BY THE ENGINE'S WORDS.
//
// A press whose answer carries a caveat raises a toast that stays until it is
// dismissed when the caveat is `required`, and one that goes by itself when it
// is `none`. The dashboard reads the action off the answer against its own
// list of them; a list that drifted would read an action it does not know as
// something to merely know about, and a setting left broken would scroll away.
func TestTheDashboardReadsWhatACaveatAsks(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "CAVEAT_ACTIONS")
	if err != nil {
		t.Fatal(err)
	}
	client := clientsource.Strings(body)
	var engine []string
	for _, a := range caveat.Actions() {
		engine = append(engine, string(a))
	}
	if len(client) == 0 {
		t.Fatal("CAVEAT_ACTIONS names nothing, so this gate certifies nothing")
	}
	if !slices.Equal(client, engine) {
		t.Errorf("the dashboard's CAVEAT_ACTIONS is %q and the engine's is %q, in "+
			"that order: change contract/caveats.ts to the engine's", client, engine)
	}
}
