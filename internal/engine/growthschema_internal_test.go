package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/tools/toolschema/toolschematest"
)

// EVERY GROWTH TOOL'S SCHEMA IS ONE AN MCP CLIENT TAKES, in every class and
// level: each product this engine serves and each catalogue registered, so a
// new product is held without a line here. Claude Code lists none of the
// bridge's tools when one schema is invalid (package toolschema).
//
// Mutation: put a nil required list back in a catalogue's object helper and
// this names its tools.
func TestEveryGrowthToolsSchemaIsOneAnMCPClientTakes(t *testing.T) {
	t.Parallel()
	catalogs := growth.Catalogs()
	for _, p := range (&Engine{}).growthProducts() {
		catalogs = append(catalogs, p.Catalog())
	}
	seen := 0
	for _, c := range catalogs {
		for _, tool := range c.Tools {
			seen++
			if faults := toolschematest.Faults(tool.Parameters); len(faults) > 0 {
				t.Errorf("%s %s (%s): %v", c.Kind, tool.Name, tool.Class, faults)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no growth tool was swept")
	}
}
