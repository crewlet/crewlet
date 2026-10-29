package host_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/host"
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
	section := whatGitPrints(t)
	for _, r := range code.Refusals() {
		if !strings.Contains(section, "("+string(r)+":") {
			t.Errorf("What git prints has no line for %q: add the line git prints, `(%s: …)`, "+
				"and what to do about it", r, r)
		}
	}
}

// WHAT GIT PRINTS NAMES EVERY CHANGE ANSWER: a push that asked for a change is
// told what became of it in one line the relay writes — the change it opened
// or named, why it opened none under each code a push's change can come to,
// that nothing landed, that it is not known — and a person who reads one
// finds it in the concept page's table, as it is printed, with what to do.
//
// Mutation: delete the table's no_difference row and this fails naming it.
func TestWhatGitPrintsNamesEveryChangeAnswer(t *testing.T) {
	t.Parallel()
	section := whatGitPrints(t)
	for _, line := range host.ChangeAnswerLines() {
		if !strings.Contains(section, "crewlet: "+line) {
			t.Errorf("What git prints has no line for a push's change answering %q: add the line git prints, "+
				"`remote: crewlet: %s`, and what to do about it", line, line)
		}
	}
}

// whatGitPrints is the concept page's "What git prints, and what it means"
// section, to the next heading of its level: the one place a line git prints
// and its remedy are written down.
func whatGitPrints(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(sourcetree.Root(t), "docs", "concepts", "code-host.md"))
	if err != nil {
		t.Fatal(err)
	}
	const heading = "## What git prints, and what it means\n"
	page := string(raw)
	start := strings.Index(page, heading)
	if start < 0 {
		t.Fatalf("docs/concepts/code-host.md has no %q section — the guide links to it, and every "+
			"line git prints is held to it", strings.TrimSpace(heading))
	}
	section := page[start+len(heading):]
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}
	return section
}
