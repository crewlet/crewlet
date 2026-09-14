package events_test

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
)

// categoryChips is the dashboard screen that offers the category filter, as
// SOURCE: it is committed, so it is in every checkout, where the built bundle
// would be a step behind a branch that changes it.
const categoryChips = "../../dashboard/src/routes/Activity.tsx"

// THE DASHBOARD OFFERS EXACTLY THE CATEGORIES THE ENGINE FILES UNDER.
//
// Its list says it mirrors the map in this package, and nothing checked that
// it did: when the retired event types left, `communication` and `knowledge`
// stayed on the Activity screen as filters no stored row could ever match. A
// chip for a category that is merely quiet is useful; one for a category that
// no longer exists sends a person to look for events that cannot arrive. This
// fails in both directions, like the docs table's guard beside it.
func TestTheDashboardOffersExactlyTheCategoriesTheEngineFiles(t *testing.T) {
	t.Parallel()
	source, err := os.ReadFile(categoryChips)
	if err != nil {
		// FAILS rather than skips: a missing file is a moved file.
		t.Fatalf("read %s: %v", categoryChips, err)
	}
	block := regexp.MustCompile(`(?s)const CATEGORIES = \[(.*?)\] as const;`).FindSubmatch(source)
	if block == nil {
		t.Fatalf("%s declares no CATEGORIES list, so this guard asserts nothing", categoryChips)
	}
	var offered []string
	for _, m := range regexp.MustCompile(`"([^"]+)"`).FindAllSubmatch(block[1], -1) {
		offered = append(offered, string(m[1]))
	}
	slices.Sort(offered)
	filed := events.CategoryNames()
	if !slices.Equal(offered, filed) {
		t.Errorf("the dashboard offers categories %s; the engine files rows under %s",
			strings.Join(offered, ", "), strings.Join(filed, ", "))
	}
}
