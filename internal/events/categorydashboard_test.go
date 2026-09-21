package events_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/events"
)

// THE DASHBOARD OFFERS EXACTLY THE CATEGORIES THE ENGINE FILES UNDER.
//
// The Activity screen's chip list says it mirrors the map in this package, and
// nothing checked that it did: when the retired event types left, the two
// categories that lost their last member stayed on the screen as filters no
// stored row could ever match. A chip for a category that is merely quiet is
// useful, and says so; one for a category that no longer exists sends a person
// to look for events that cannot arrive, and reads as an engine gone silent.
//
// BOTH DIRECTIONS, like the docs table's guard beside it. A category the
// engine files under and the screen does not offer is a filter nobody can
// reach the rows through.
//
// Read through [clientsource] rather than from a path, because the screen has
// already moved once: a gate naming the file it used to live in fails loudly
// and wrongly, reporting a drift between two lists neither of which changed.
func TestTheDashboardOffersExactlyTheCategoriesTheEngineFiles(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Declaration(clientsource.Tree,
		`(?s)const CATEGORIES = \[(.*?)\] as const;`)
	if err != nil {
		t.Fatal(err)
	}
	offered := clientsource.Strings(body)
	if len(offered) == 0 {
		t.Fatal("the dashboard offers no categories at all, so this gate certifies nothing")
	}
	slices.Sort(offered)
	filed := events.CategoryNames()
	if !slices.Equal(offered, filed) {
		t.Errorf("the dashboard offers categories %s; the engine files rows under %s",
			strings.Join(offered, ", "), strings.Join(filed, ", "))
	}
}
