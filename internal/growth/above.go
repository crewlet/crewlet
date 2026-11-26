package growth

import (
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/words"
)

// The catalog's words for where a method is made (package doc, "Above a
// seat's level"), which every product's refusals and class tools use.

// Named is the level's label and, where it differs, its written name, so a
// person finds it on any Agents tab.
func (l Level) Named() string {
	if strings.EqualFold(l.Label(), string(l)) {
		return l.Label()
	}
	return l.Label() + " (" + string(l) + ")"
}

// approved is how a held class's call is made, as a tool or refusal says it.
const approved = "each call held for an administrator's approval at a fresh sign-in"

// TakesOperation reports whether t is a class tool, naming its method by ID.
func (t Tool) TakesOperation() bool {
	props, _ := t.Parameters["properties"].(map[string]any)
	_, ok := props["operation"]
	return ok
}

// Outside refuses tool's call of method id, which tool does not list, for a
// seat at held ("" for a lane that is off): with the way to it, a never
// method's reason, or that no level reaches it.
func (c Catalog) Outside(tool, id string, held Level) error {
	op, listed := c.Operation(id)
	switch {
	case !listed:
		return Refuse(RefusedNotFound, fmt.Sprintf("%s didn't run: %q is no %s method Crewlet offers, so no "+
			"tool makes it at any level and no change to your access would. No vendor was reached.", tool, id,
			c.API))
	case op.Class == ClassNever:
		return Refuse(RefusedForbidden, fmt.Sprintf("%s didn't run: %s is not offered at any level: %s No "+
			"vendor was reached.", tool, id, op.Reason))
	}
	least, carriers := ClassNever, []words.Said(nil)
	for _, t := range c.Tools {
		switch {
		case !slices.Contains(t.Operations, id) || t.Class.Rank() > least.Rank():
		case t.Class.Rank() < least.Rank():
			least, carriers = t.Class, []words.Said{words.Said(t.Name)}
		default:
			carriers = append(carriers, words.Said(t.Name))
		}
	}
	if len(carriers) == 0 {
		return Refuse(RefusedForbidden, fmt.Sprintf("%s didn't run: %s is %s-class, and no tool makes it at any "+
			"level: a person makes it in the vendor's own console. No vendor was reached.", tool, id, op.Class))
	}
	kind := RefusedInvalid
	if !held.Allows(least) {
		kind = RefusedForbidden
	}
	return Refuse(kind, fmt.Sprintf("%s didn't run: %s is %s-class, which %s does not make. %s No vendor was "+
		"reached.", tool, id, least, tool, c.way(carriers, least, held)))
}

// WayTo says which tools make a call of class and, where held does not reach
// them, their level, their approval and how the seat is raised: in words,
// since request_access asks for no level (ADR-0118).
func (c Catalog) WayTo(class Class, held Level) string {
	through := c.toolsOf(class)
	if len(through) == 0 || !class.Callable() {
		return "No tool makes it at any level: a person makes it in the vendor's own console."
	}
	return c.way(through, class, held)
}

// way is [Catalog.WayTo] through the tools named.
func (c Catalog) way(through []words.Said, class Class, held Level) string {
	if held.Allows(class) {
		out := "Call " + words.Or(through) + " instead"
		if class.Held() {
			out += ", " + approved
		}
		return out + "."
	}
	need := class.Level()
	out := words.Or(through) + " makes it, offered to an agent at " + need.Named()
	if class.Held() {
		out += ", " + approved
	}
	ask := fmt.Sprintf("Your access to %s is off, so ask an administrator to turn it on at %s", c.API, need.Label())
	if held.Valid() {
		ask = fmt.Sprintf("Your access to %s is at %s, so ask an administrator to raise it to %s", c.API,
			held.Named(), need.Label())
	}
	return fmt.Sprintf("%s. %s, in words with %s: they set it under Settings, Integrations, then the "+
		"integration's Agents tab (%s asks for no level).", out, ask, toolname.CommentOnWorkItem,
		toolname.RequestAccess)
}

// Above is what lies above a class tool: each higher class's tools with what
// they change, their level and approval; "" for a curated or top tool.
func (c Catalog) Above(tool string) string {
	t, ok := c.Tool(tool)
	if !ok || !t.TakesOperation() {
		return ""
	}
	var parts []string
	for _, class := range Classes() {
		if class.Rank() <= t.Class.Rank() || !class.Callable() {
			continue
		}
		for _, name := range c.toolsOf(class) {
			above, _ := c.Tool(string(name))
			part := string(name) + " (" + string(class)
			if above.Covers != "" {
				part += ": " + above.Covers
			}
			part += ", at " + class.Level().Named()
			if class.Held() {
				part += ", " + approved
			}
			parts = append(parts, part+")")
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "Methods above its class are made by " + strings.Join(parts, "; ") + ". Name one here and the " +
		"refusal says what it needs."
}

// toolsOf is the class's class tools, or all its tools where it has none.
func (c Catalog) toolsOf(class Class) []words.Said {
	var generic, all []words.Said
	for _, t := range c.Tools {
		if t.Class != class {
			continue
		}
		all = append(all, words.Said(t.Name))
		if t.TakesOperation() {
			generic = append(generic, words.Said(t.Name))
		}
	}
	if len(generic) > 0 {
		return generic
	}
	return all
}
