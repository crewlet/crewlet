package host_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// WHAT GIT PRINTS NAMES EVERY REFUSAL. The guide sends a person who meets any
// refusal it does not list to the concept page's "What git prints, and what it
// means", so that table is the one place a refusal's line and its remedy are
// written down — and a code it has no row for is a line a person reads with
// nothing to say what to do. Every code git can print sits in a ref's line as
// `(<code>: <sentence>)`, so the section must carry each in that form: a code
// named only in prose ("protected_delete likewise") is a line nobody can find
// by searching for what git printed.
//
// Mutation: delete the page's repo_size row and this fails naming it.
func TestWhatGitPrintsNamesEveryRefusal(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(sourcetree.Root(t), "docs", "concepts", "code-host.md"))
	if err != nil {
		t.Fatal(err)
	}
	const heading = "## What git prints, and what it means\n"
	page := string(raw)
	start := strings.Index(page, heading)
	if start < 0 {
		t.Fatalf("docs/concepts/code-host.md has no %q section — the guide links to it, and every "+
			"refusal's line is held to it", strings.TrimSpace(heading))
	}
	section := page[start+len(heading):]
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}
	for _, r := range code.Refusals() {
		if !strings.Contains(section, "("+string(r)+":") {
			t.Errorf("What git prints has no line for %q: add the line git prints, `(%s: …)`, "+
				"and what to do about it", r, r)
		}
	}
}
