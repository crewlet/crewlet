package statelog_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// referencePath is the generated page, relative to this package.
const referencePath = "../../docs/reference/alarms.md"

// THE REFERENCE IS GENERATED AND DIFFED, the same idiom schema/ and the
// metrics reference use.
//
// An operator meets an alarm for the first time in a log line at an
// inconvenient hour. The page is where they look it up, and a page maintained
// by hand is one that stops matching the table — at which point the alarm
// they were paged for is the one with no entry.
func TestTheAlarmReferenceIsGenerated(t *testing.T) {
	t.Parallel()
	want := statelog.AlarmReference()

	got, err := os.ReadFile(filepath.Clean(referencePath))
	if err != nil {
		t.Fatalf("read %s: %v — the page is generated from the alarm table and "+
			"has to exist for the docs site to link it", referencePath, err)
	}
	if string(got) != want {
		t.Errorf("%s is stale: it is generated from the alarm table, so an alarm "+
			"added without regenerating it is one an operator cannot look up. "+
			"Run:\n\n\tmake alarms-doc\n", referencePath)
	}
}

// EVERY ALARM HAS AN ENTRY. The reference renders one row per rule, so a kind
// with no prose meaning renders an empty cell — a row that names an alarm and
// explains nothing, which is worse than an absent page because it looks like
// documentation.
func TestEveryAlarmHasAMeaningInTheReference(t *testing.T) {
	t.Parallel()
	page := statelog.AlarmReference()
	for _, kind := range statelog.Kinds() {
		row := "| `" + string(kind) + "` |  |"
		if got := page; len(got) > 0 && contains(got, row) {
			t.Errorf("%s renders with no meaning", kind)
		}
		if !contains(page, "`"+string(kind)+"`") {
			t.Errorf("%s is missing from the reference entirely", kind)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		(func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		})()
}
