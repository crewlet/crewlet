package provenance

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// sourceList is the sentence of the agent runtime's page that names every
// surface a fence's opening line can carry.
var sourceList = regexp.MustCompile(`The opening line names the surface the text came from \(([^\n]*)\), who wrote it`)

// backticked is one code span in that sentence.
var backticked = regexp.MustCompile("`([a-z0-9_]+)`")

// EVERY LABEL A MODEL CAN READ IS ONE A PERSON CAN LOOK UP, and no page names a
// label a fence never carries.
//
// [Sources] is a closed set precisely so a person auditing a transcript can
// look a fence's `source=` up; the page they look it up on is the agent
// runtime's "Text somebody else wrote", and it is held to the set in both
// directions — a source added here without a word there, or a word left there
// after its source went, fails the build rather than a reader.
func TestEverySourceIsNamedWhereAReaderLooksItUp(t *testing.T) {
	path := filepath.Join(sourcetree.Root(t), "docs", "concepts", "agent-runtime.md")
	page, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the agent runtime's page: %v", err)
	}
	match := sourceList.FindSubmatch(page)
	if match == nil {
		t.Fatalf("%s no longer carries the sentence naming every source", path)
	}
	var named []string
	for _, m := range backticked.FindAllSubmatch(match[1], -1) {
		named = append(named, string(m[1]))
	}
	slices.Sort(named)
	want := make([]string, 0, len(Sources))
	for _, s := range Sources {
		want = append(want, string(s))
	}
	slices.Sort(want)
	if !slices.Equal(named, want) {
		t.Errorf("the page names the sources %s, and a fence can carry %s",
			strings.Join(named, ", "), strings.Join(want, ", "))
	}
}
