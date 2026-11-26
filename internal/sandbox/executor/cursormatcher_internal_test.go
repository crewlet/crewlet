package executor

import "testing"

// A MATCHER IS MCP-SAFE ONLY WHERE NO TOOL NAME MAKES IT FIRE: Cursor searches
// a hook's matcher anywhere in `MCP:<tool>` (measured), and a server may name
// a tool anything, so only a matcher whose every alternative is anchored at
// the start on a literal departing from `MCP:` (case kept, as Cursor matches)
// never fires on an MCP call; any other, or one that does not parse, can.
//
// Mutation: take an unanchored literal, a fold-case one, or one that is a
// prefix of `MCP:`, for safe, and its row goes red.
func TestACursorMatcherIsMCPSafeOnlyWhereNoToolNameMakesItFire(t *testing.T) {
	t.Parallel()
	for matcher, safe := range map[string]bool{
		"^Shell$": true, "^(Shell|Write)$": true, "^Shell|^Write": true, "^t_tool$": true, "^mcp": true,
		"^Search|^Shell": true,
		"":               false, ".*": false, "*": false, "Shell": false, "MCP": false, "t_tool": false, "^MCP:x": false,
		"^M": false, "^MC|^Shell": false, "(?i)^shell": false, "^Shell|Write": false, "[": false, "^.hell": false,
	} {
		if got := cursorMatcherSkipsMCP(matcher); got != safe {
			t.Errorf("matcher %q: MCP-safe %v, want %v", matcher, got, safe)
		}
	}
}
