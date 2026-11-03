package gittest

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A CLIENT READS NOTHING OF THE MACHINE: its environment is exactly its own
// table, whatever the test's holds, and every file git would otherwise read
// from the host — the system configuration, the system ATTRIBUTES — is off,
// and the global ones are the client's. The attributes are the half that is
// easy to miss: GIT_CONFIG_NOSYSTEM does not reach them, and a host's
// /etc/gitattributes decides what a commit stores (working-tree-encoding,
// text and eol), what a merge does (merge=union) and what check-attr answers,
// so a fixture built under one is other bytes than the same fixture
// elsewhere.
func TestAClientReadsNothingOfTheMachine(t *testing.T) {
	for name, value := range map[string]string{
		"GIT_DIR":           "/nonexistent",
		"GIT_CONFIG_SYSTEM": "/tmp/x",
		"XDG_CONFIG_HOME":   "/tmp",
		"GIT_ATTR_SOURCE":   "HEAD",
	} {
		t.Setenv(name, value)
	}
	c := NewClient(t)

	env := map[string]string{}
	for _, kv := range c.env {
		name, value, _ := strings.Cut(kv, "=")
		env[name] = value
	}
	want := map[string]string{
		"PATH":                    os.Getenv("PATH"),
		"HOME":                    c.home,
		"GIT_CONFIG_NOSYSTEM":     "1",
		"GIT_CONFIG_GLOBAL":       filepath.Join(c.home, ".gitconfig"),
		"GIT_ATTR_NOSYSTEM":       "1",
		"GIT_TERMINAL_PROMPT":     "0",
		"LC_ALL":                  "C",
		"GIT_AUTHOR_DATE":         clientDate,
		"GIT_COMMITTER_DATE":      clientDate,
		"GIT_CEILING_DIRECTORIES": filepath.Dir(c.home),
	}
	if !maps.Equal(env, want) {
		t.Errorf("the client's environment is\n  %v\nwant exactly\n  %v", env, want)
	}

	// And git agrees, where it can say so: `git var GIT_ATTR_SYSTEM` names
	// the system attributes file only while it is read (git 2.42 and newer;
	// an older git knows no such variable and names nothing either way, and
	// the table above is what holds the line there).
	if out, _, err := c.Try(t, "", nil, "var", "GIT_ATTR_SYSTEM"); err == nil {
		t.Errorf("the client's git reads the system attributes file %s", strings.TrimSpace(out))
	}
	own := filepath.Join(c.home, ".config", "git", "attributes")
	if out, _, err := c.Try(t, "", nil, "var", "GIT_ATTR_GLOBAL"); err == nil && strings.TrimSpace(out) != own {
		t.Errorf("the client's git reads the global attributes file %s, want its own HOME's %s",
			strings.TrimSpace(out), own)
	}
}
