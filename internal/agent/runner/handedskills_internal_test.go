package runner

import (
	"maps"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A SKILL IS HANDED UNDER A NAME BOTH HARNESSES READ, derived from its key the
// same way on every node and at every launch: lowercase, every run of anything
// but an ASCII letter or digit one hyphen, trimmed at either end and to 64. Two
// keys that come to one name are told apart in key order, the first keeping
// it, so a name a run's model already loaded stays its own across a relaunch
// handed the same keys.
//
// Mutation: tell two names apart in the order the keys were given, and the
// same skill set is named differently on two nodes.
func TestASkillIsHandedUnderANameBothHarnessesRead(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 70)
	for name, c := range map[string]struct {
		keys []string
		want map[string]string
	}{
		"a key's own": {[]string{"mcp:github", "skill:platform_mentions", "tool:refresh_memory"},
			map[string]string{"mcp:github": "mcp-github",
				"skill:platform_mentions": "skill-platform-mentions",
				"tool:refresh_memory":     "tool-refresh-memory"}},
		"cased and spaced": {[]string{"Chat Conventions!", "--odd--"},
			map[string]string{"Chat Conventions!": "chat-conventions", "--odd--": "odd"}},
		"nothing to keep": {[]string{"::"}, map[string]string{"::": "skill"}},
		"too long":        {[]string{long}, map[string]string{long: strings.Repeat("x", 64)}},
		"two that collide, in key order": {[]string{"mcp_github", "mcp:github", "MCP github"},
			map[string]string{"MCP github": "mcp-github", "mcp:github": "mcp-github-2",
				"mcp_github": "mcp-github-3"}},
		"a long collision keeps its suffix": {[]string{long + "a", long + "b"},
			map[string]string{long + "a": strings.Repeat("x", 64),
				long + "b": strings.Repeat("x", 62) + "-2"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := nativeNames(c.keys)
			if !maps.Equal(got, c.want) {
				t.Fatalf("nativeNames(%q) = %v, want %v", c.keys, got, c.want)
			}
			for _, n := range got {
				if !sandbox.ValidSkillName(n) {
					t.Errorf("%q is no name a harness reads a skill by", n)
				}
			}
		})
	}
}
