package config_test

import (
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/config"
)

// THE ORG BUILDER KEYS A NEW UNIT BY THE ENGINE'S GRAMMAR.
//
// A unit added on the canvas is given its key there — offered from its name,
// editable until the add — and the document carries it from then on, so the
// builder mints the key in [config.UnitIDPattern], cuts a long name's slug at
// [config.MaxUnitID] as [config.MintUnitID] does, and refuses a typed key
// outside the grammar before the add is recorded. A pattern looser than the
// engine's records a node every save is then refused for; a tighter one
// refuses a key the engine takes; a bound past the engine's offers a key the
// engine refuses.
//
// Mutation: change either side's pattern or bound and this fails.
func TestTheOrgBuilderKeysAUnitByTheEnginesGrammar(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	for name, engine := range map[string]string{
		"UNIT_ID_PATTERN": config.UnitIDPattern,
		"MAX_UNIT_ID":     strconv.Itoa(config.MaxUnitID),
	} {
		got, err := clientsource.Scalar(tree, name)
		if err != nil {
			t.Fatal(err)
		}
		if got != engine {
			t.Errorf("the org builder's %s is %q and the engine's is %q: change it "+
				"in contract/config.ts", name, got, engine)
		}
	}
}
