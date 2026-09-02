package mcp

import "strings"

// catalogue.go's renderer, kept here because internal/tools imports this
// package and not the reverse.
//
// This file also held a second implementation of the discover-then-activate
// meta-tools — MetaTool, ListServerTools, ActivateTool and their helpers,
// some 280 lines. Nothing outside this package ever called them: the live
// pair is internal/agent/runner's listMCPTools and activateTool, which the
// runner registers on the Plan and Execute surfaces. Two implementations of
// one contract, only one of them reachable, is indistinguishable to the next
// reader from a caller nobody found — and the dead half had drifted, cutting
// every tool description to its first line long after the live one was
// expected not to.

// CatalogueLine renders one "- name: description" catalogue entry with the
// description WHOLE.
//
// Continuation lines are indented under the bullet rather than dropped. The
// shape a model reads a catalogue as is one entry per bullet, and keeping only
// the first line was paying for that shape with the description's content: a
// tool whose usage rules, argument meanings or "call X first" precondition sit
// below its opening sentence was advertised without them, and the model then
// called it wrong. Vendor-authored MCP descriptions are routinely several
// paragraphs, and a catalogue is the only place they are ever shown.
//
// HERE, in the deepest package that needs it, because four callers render this
// same line — the tool registry's catalogue, both list_mcp_server_tools
// listings, and a sub-agent's surface — and a model sees more than one of them
// in a single turn. Two renderers is how one of them starts cutting again;
// internal/tools re-exports this rather than keeping a copy, since it already
// imports this package and the dependency cannot run the other way.
func CatalogueLine(name, description string) string {
	description = strings.TrimSpace(description)
	if description == "" {
		return "- " + name
	}
	lines := strings.Split(description, "\n")
	for i := 1; i < len(lines); i++ {
		// Blank lines stay blank: indenting one leaves trailing
		// whitespace on a line whose only job is the gap.
		if strings.TrimSpace(lines[i]) != "" {
			lines[i] = "  " + lines[i]
		} else {
			lines[i] = ""
		}
	}
	return "- " + name + ": " + strings.Join(lines, "\n")
}
