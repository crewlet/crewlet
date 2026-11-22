package harnessprofile_test

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// A BOX IS HANDED ONLY A TOML LOGIN'S OWN TABLES: of a config.toml holding
// a Kimi Code login and a hook, a permission rule and a tool setting a run
// appended, the login's tables come through with every value, and nothing
// else does, an array of tables included.
//
// Mutation: keep every key, or drop a login table.
func TestABoxIsHandedOnlyATOMLLoginsOwnTables(t *testing.T) {
	t.Parallel()
	config := `default_model = "managed:kimi-code/kimi-for-coding"

[providers."managed:kimi-code"]
type = "kimi"
base_url = "https://api.kimi.com/coding/v1"
oauth = { storage = "file", key = "oauth/kimi-code" }

[models."managed:kimi-code/kimi-for-coding"]
provider = "managed:kimi-code"
model = "kimi-for-coding"

[services.moonshot_search]
base_url = "https://api.kimi.com/coding/v1/search"
oauth = { storage = "file", key = "oauth/kimi-code" }

[[hooks]]
event = "SessionStart"
command = "touch /tmp/planted"

[[permission.rules]]
pattern = "mcp__*"
action = "deny"

[tools]
disabled = ["Bash"]
`
	got, err := harnessprofile.KeepLoginTables([]byte(config),
		[]string{"default_model", "models", "providers", "services", "thinking"})
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"hooks", "touch /tmp/planted", "permission", "tools", "disabled"} {
		if strings.Contains(string(got), gone) {
			t.Errorf("the box is handed %q:\n%s", gone, got)
		}
	}
	var doc map[string]any
	if _, err := toml.Decode(string(got), &doc); err != nil {
		t.Fatalf("the result is no TOML: %v\n%s", err, got)
	}
	if doc["default_model"] != "managed:kimi-code/kimi-for-coding" {
		t.Errorf("default_model = %v", doc["default_model"])
	}
	providers, _ := doc["providers"].(map[string]any)
	provider, _ := providers["managed:kimi-code"].(map[string]any)
	oauth, _ := provider["oauth"].(map[string]any)
	if provider["base_url"] != "https://api.kimi.com/coding/v1" || oauth["key"] != "oauth/kimi-code" {
		t.Errorf("the provider came through as %v", provider)
	}
	for _, table := range []string{"models", "services"} {
		if _, ok := doc[table]; !ok {
			t.Errorf("the login's %s table is missing:\n%s", table, got)
		}
	}
	if _, err := harnessprofile.KeepLoginTables([]byte("not = [toml"), []string{"providers"}); err == nil {
		t.Error("a file that is not TOML was taken")
	}
}
