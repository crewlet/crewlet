package authz

import (
	"strings"

	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/iam"
)

// A GROWTH TOOL'S ROW IS ITS CATALOG'S (internal/growth; ADR-0170). A growth
// product lists its tools in its catalog, so no row is written here per tool:
// every growth tool is decided as the SSH tools are, by a grant its agent's
// role carries in place of a class. The grant is the tool's product and the
// least seat level its class needs (`google_analytics_operate`), which an
// agent's role carries where its lane is on at that level or above
// (internal/authz/orgchart). A person reaches a growth vendor in its own
// console, never through these tools.

// GrowthSeatGrant is the grant a growth tool of kind needing level asks of
// an agent's role.
func GrowthSeatGrant(kind string, level growth.Level) SeatGrant {
	return SeatGrant(kind + "_" + string(level))
}

// GrowthSeatGrantOf is the product and level a growth grant names, false for
// a grant that is not one: its product registers no catalog, or its level is
// not one.
func GrowthSeatGrantOf(g SeatGrant) (string, growth.Level, bool) {
	i := strings.LastIndex(string(g), "_")
	if i <= 0 {
		return "", "", false
	}
	kind, level := string(g)[:i], growth.Level(string(g)[i+1:])
	if _, ok := growth.CatalogOf(kind); !ok || !level.Valid() {
		return "", "", false
	}
	return kind, level, true
}

// growthRule is the row of a growth tool's action, false for an action no
// registered catalog names as a tool.
func growthRule(a Action) (rule, bool) {
	kind, ok := growth.KindOfTool(string(a))
	if !ok {
		return rule{}, false
	}
	c, _ := growth.CatalogOf(kind)
	tool, _ := c.Tool(string(a))
	assist := AssistRead
	if tool.Class != growth.ClassRead {
		assist = AssistApprove
	}
	return rule{class: ClassAdmin, seatGrant: GrowthSeatGrant(kind, tool.Class.Level()),
		recency: iam.RecencyAny, assist: assist}, true
}

// ruleOf is a's row: the table's, or a growth tool's from its catalog.
func ruleOf(a Action) (rule, bool) { return ruleIn(rules, growthRule, a) }

// ruleIn is [ruleOf] over table and a growth tool's row.
//
// A NAME BOTH CLAIM IS NO VERB: either row would decide the other's call, so
// it fails closed for both, and [Router.Handle] refuses its route at start.
func ruleIn(table map[Action]rule, growthOf func(Action) (rule, bool), a Action) (rule, bool) {
	r, inTable := table[a]
	g, isGrowth := growthOf(a)
	switch {
	case inTable && isGrowth:
		return rule{}, false
	case inTable:
		return r, true
	case isGrowth:
		return g, true
	}
	return rule{}, false
}

// growthVerbs is every tool a registered catalog lists.
func growthVerbs() []Action {
	var out []Action
	for _, c := range growth.Catalogs() {
		for _, t := range c.Tools {
			out = append(out, Action(t.Name))
		}
	}
	return out
}
