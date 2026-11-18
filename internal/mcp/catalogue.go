package mcp

import "strings"

// catalogue.go's renderer, kept here because internal/tools imports this
// package and not the reverse.
//
// This file also held a second implementation of the discover-then-activate
// meta-tools — MetaTool, ListServerTools, ActivateTool and their helpers,
// some 280 lines. Nothing outside this package ever called them: the live
// pair is internal/agent/runner's listMCPTools and activateTool, which the
// runner registers on the onboarding and executor surfaces. Two
// implementations of one contract, only one of them reachable, is
// indistinguishable to the next reader from a caller nobody found, and the
// dead half had drifted: it cut every tool description to its first line long
// after the live one was expected not to.

// CatalogueLine renders one "- name: description" catalogue entry with the
// description WHOLE.
//
// Continuation lines are indented under the bullet, not dropped: usage rules,
// argument meanings or a "call X first" precondition below the opening
// sentence are the only place a model sees them, and without them it calls
// the tool wrong.
//
// It lives HERE because two callers render the line (a snapshot's discoverable
// tools and list_mcp_server_tools) and a model sees both in one turn; two
// renderers is how one starts cutting again. internal/tools re-exports it, as
// the dependency cannot run the other way.
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
