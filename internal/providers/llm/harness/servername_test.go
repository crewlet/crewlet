package harness

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// EVERY PROFILE SPELLS THE SEAT'S TOOL SERVER AS CONFIG RESERVES IT. A profile
// writes the server's name into its CLI's own grammar — a JSON key in a seeded
// file, a `-c mcp_servers.<name>…` override, an allowlist pattern — and reads
// the CLI's record by the prefix it gives that server's tools, while the box
// bridge and the session are named from [config.ToolServerName]. Nothing ties
// the hand-written spellings to the constant but this: a profile naming another
// server would hand its CLI tools the record reader never claims, and a seat
// moved into a box would see its tools renamed.
func TestEveryProfileSpellsTheToolServerAsConfigReservesIt(t *testing.T) {
	t.Parallel()
	for _, name := range harnessprofile.BuiltinNames() {
		p, _ := harnessprofile.Builtin(name)
		if p.MCP == nil {
			continue
		}
		var said []string
		said = append(said, p.MCP.Args...)
		said = append(said, p.MCP.AllowArgs...)
		said = append(said, p.MCP.CompleteArgs...)
		said = append(said, p.MCP.UsageArgs...)
		for _, seed := range p.MCP.Seed {
			said = append(said, seed.Content)
		}
		for _, value := range p.MCP.Env {
			said = append(said, value)
		}
		if !strings.Contains(strings.Join(said, "\n"), config.ToolServerName) {
			t.Errorf("%s: its mcp block never names the %q server", name, config.ToolServerName)
		}
		if p.Requests == nil {
			continue
		}
		for field, value := range map[string]string{
			"host_prefix": p.Requests.HostPrefix, "host_namespace": p.Requests.HostNamespace,
		} {
			if value != "" && !strings.Contains(value, config.ToolServerName) {
				t.Errorf("%s: requests.%s %q does not name the %q server",
					name, field, value, config.ToolServerName)
			}
		}
	}
}
