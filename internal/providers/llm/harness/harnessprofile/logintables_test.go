package harnessprofile_test

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// kimiLogin is a config.toml as Kimi Code 2.7.2's login writes it.
const kimiLogin = `default_model = "managed:kimi-code/kimi-for-coding"

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
`

// ONLY A TOML LOGIN'S OWN TABLES ARE HANDED ON to a seat home on the engine
// host: of a Kimi Code config.toml holding a hook, a plugin entry, a
// permission rule and a tool setting beside the login, the login's tables come
// through with every value and nothing else does; a config.toml holding the
// login alone comes through byte for byte, so its digest is the shared one's.
//
// Mutation: keep every key, drop a login table, or re-encode a login alone.
func TestOnlyATOMLLoginsOwnTablesAreHandedOn(t *testing.T) {
	t.Parallel()
	p, ok := harnessprofile.Builtin("kimi-code")
	if !ok {
		t.Fatal("no built-in kimi-code profile")
	}
	extra := "\n[[hooks]]\nevent = \"SessionStart\"\ncommand = \"touch /tmp/planted\"\n\n" +
		"[plugins.planted]\nenabled = true\n\n[[permission.rules]]\npattern = \"mcp__*\"\naction = \"deny\"\n\n" +
		"[tools]\ndisabled = [\"Bash\"]\n"
	got, err := p.FileRule("config.toml").Login([]byte(kimiLogin + extra))
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"hooks", "touch /tmp/planted", "plugins", "permission", "tools"} {
		if strings.Contains(string(got), gone) {
			t.Errorf("%q is handed on:\n%s", gone, got)
		}
	}
	var doc map[string]any
	if _, err := toml.Decode(string(got), &doc); err != nil {
		t.Fatalf("the result is no TOML: %v\n%s", err, got)
	}
	var want map[string]any
	if _, err := toml.Decode(kimiLogin, &want); err != nil {
		t.Fatal(err)
	}
	if !equalTOML(doc, want) {
		t.Errorf("the login came through as\n%s\nwant\n%s", got, kimiLogin)
	}
	same, err := p.FileRule("config.toml").Login([]byte(kimiLogin))
	if err != nil || string(same) != kimiLogin {
		t.Errorf("a login alone came through as %q (%v), want it byte for byte", same, err)
	}
	if _, err := p.FileRule("config.toml").Login([]byte("not = [toml")); err == nil {
		t.Error("a config.toml that is not TOML was taken")
	}
	if json, err := p.FileRule("kimi-code.json").Login([]byte(`{"token":"t"}`)); err != nil || string(json) != `{"token":"t"}` {
		t.Errorf("a file that is not the TOML login came through as %q (%v)", json, err)
	}
}

// equalTOML reports whether two decoded TOML documents are the same.
func equalTOML(a, b map[string]any) bool {
	var x, y strings.Builder
	_ = toml.NewEncoder(&x).Encode(a)
	_ = toml.NewEncoder(&y).Encode(b)
	return x.String() == y.String()
}
