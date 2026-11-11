package config_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/config"
)

// THE DASHBOARD KEYS A CONTAINER BY THE ENGINE'S GRAMMAR.
//
// The new-project dialog checks the key a person types before the write that
// names it, and stops the field at the grammar's width; and past what an enum
// may list, Ask Crewlet is told a Doclet space by the key's grammar rather
// than by every space the company has. So the dashboard's copy is the
// engine's own text: a looser one let `MY_PROJ`, a single letter and a
// twelve-letter key through to a write every node refuses, and tells the
// assistant keys no space can have; a tighter one refuses a key the engine
// takes. And the width it stops at is the pattern's bound.
//
// Mutation: change either side's pattern or bound, or MaxContainerKey alone,
// and this fails.
func TestTheDashboardKeysAContainerByTheEnginesGrammar(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	for name, engine := range map[string]string{
		"CONTAINER_KEY_PATTERN": config.ContainerKeyPattern,
		"CONTAINER_KEY_MAX":     strconv.Itoa(config.MaxContainerKey),
	} {
		got, err := clientsource.Scalar(tree, name)
		if err != nil {
			t.Fatal(err)
		}
		if got != engine {
			t.Errorf("the dashboard's %s is %q and the engine's is %q: change it "+
				"in contract/config.ts", name, got, engine)
		}
	}
	if !config.ValidContainerKey(strings.Repeat("A", config.MaxContainerKey)) ||
		config.ValidContainerKey(strings.Repeat("A", config.MaxContainerKey+1)) {
		t.Errorf("ContainerKeyPattern %q does not stop at MaxContainerKey, %d characters",
			config.ContainerKeyPattern, config.MaxContainerKey)
	}
}
