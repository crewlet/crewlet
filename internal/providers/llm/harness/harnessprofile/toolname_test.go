package cliprofile

import (
	"strings"
	"testing"
)

// THE LONGEST TOOL NAME FOLLOWS THE LONGEST CLI PREFIX: it is what the longest
// prefix a built-in profile puts in front of the engine's tools leaves of a
// model's 64 characters, computed from the table rather than written down, so
// a profile that spells the tools longer lowers it with nothing else told.
//
// Mutation: add 9 to toolNameRoom's longest prefix, or count a namespace
// without its separator, and this is red; drop Muse Code's name_prefix back
// into its host_prefix and the limit reads 45.
func TestTheToolNameLimitFollowsTheLongestCLIPrefix(t *testing.T) {
	t.Parallel()
	longest, by := 0, ""
	for _, name := range BuiltinNames() {
		p, _ := Builtin(name)
		if n := len(p.ModelToolPrefix()); n > longest {
			longest, by = n, name
		}
	}
	if longest == 0 {
		t.Fatal("no built-in profile spells the engine's tools, so the limit asserts nothing")
	}
	if got, want := ToolNameMax(), ModelToolNameMax-longest; got != want {
		t.Errorf("ToolNameMax() = %d, want %d: %d less %s's prefix", got, want, ModelToolNameMax, by)
	}
	// Claude Code's mcp__crewlet__, 14 characters, is the longest today.
	if got := ToolNameMax(); got != 50 {
		t.Errorf("ToolNameMax() = %d: a profile now spells the tools longer than Claude Code's "+
			"mcp__crewlet__ (%s, %d characters). Say so in docs/guides/tools-and-mcp.md, and "+
			"move this number", got, by, longest)
	}

	p, _ := Builtin("claude-code")
	longer := p.Clone()
	longer.Requests.HostPrefix = "mcp__crewlet_tools__"
	if got, want := toolNameRoom([]Profile{p, longer}), ModelToolNameMax-len("mcp__crewlet_tools__"); got != want {
		t.Errorf("a profile with a longer prefix leaves %d, want %d", got, want)
	}
	namespaced := p.Clone()
	namespaced.Requests.HostPrefix, namespaced.Requests.HostNamespace = "", "mcp__crewlet_with_a_long_namespace"
	if got, want := toolNameRoom([]Profile{namespaced}), ModelToolNameMax-len("mcp__crewlet_with_a_long_namespace__"); got != want {
		t.Errorf("a namespaced profile leaves %d, want %d: the namespace and the separator are the model's", got, want)
	}
}

// EVERY CLI SPELLS THE ENGINE'S TOOLS IN A MODEL'S OWN CHARACTERS: the prefix a
// profile declares is the one its model reads, so it is held to what a model's
// tool name may hold. A record's own spelling (Muse Code's operation is
// `tool:<name>`) is its call rule's name_prefix; written into host_prefix it
// would count toward every name's length and lower the limit for a character
// no model ever sees.
//
// Mutation: put Muse Code's `tool:` back into its host_prefix, and this is red.
func TestEveryCLISpellsTheEnginesToolsAsItsModelReadsThem(t *testing.T) {
	t.Parallel()
	for _, name := range BuiltinNames() {
		p, _ := Builtin(name)
		prefix := p.ModelToolPrefix()
		if prefix == "" {
			if p.MCP != nil {
				t.Errorf("%s runs the engine's tools and says nothing of how its model reads them", name)
			}
			continue
		}
		if !ToolNameChars(prefix) {
			t.Errorf("%s's model prefix %q holds characters no model's tool name can: a "+
				"record's own spelling belongs in its call rule's name_prefix", name, prefix)
		}
	}
}

// A MODEL'S TOOL NAME IS LETTERS, DIGITS, `_` AND `-`, and at least one of
// them.
func TestAToolNameHoldsOnlyWhatAModelReads(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"search":                 true,
		"Get_Issue-2":            true,
		"":                       false,
		"tool:search":            false,
		"read.file":              false,
		"two words":              false,
		"naïve":                  false,
		strings.Repeat("x", 200): true, // length is ToolNameMax's question, not this one's
	} {
		if got := ToolNameChars(name); got != want {
			t.Errorf("ToolNameChars(%q) = %v, want %v", name, got, want)
		}
	}
}
