package events_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	_ "github.com/crewlet/crewlet/internal/events/types"
)

// AN UNFED TYPE IS A PERSISTED ONE. The class narrows what the activity feed
// carries and nothing else: a type kept out of the feed that was not placed at
// all would be a type neither stored nor shown, which is the silent drop the
// admission list exists to make impossible — and declaring it here would read
// as a decision about the feed when nothing would ever store it.
func TestAnUnfedTypeIsPersisted(t *testing.T) {
	t.Parallel()
	if len(events.Unfed()) == 0 {
		t.Fatal("no unfed type is declared — this guard is asserting nothing")
	}
	for _, name := range events.Unfed() {
		if _, placed := events.Category(name); !placed {
			t.Errorf("%q is kept out of the feed and has no category, so nothing stores it", name)
		}
		if !events.KeptOutOfFeed(name) {
			t.Errorf("KeptOutOfFeed(%q) is false for a type declared unfed", name)
		}
		if strings.TrimSpace(events.UnfedReasons()[name]) == "" {
			t.Errorf("%q is unfed with no reason — an exclusion and an oversight "+
				"look identical without one", name)
		}
	}
}

// EVERY OTHER TYPE IS FED: the class is a narrowing of the feed and never a
// second admission list, so a type nobody declared — a placed one, or one a
// newer peer stored that this build has never heard of — is a feed row
// exactly as the category says.
func TestEveryOtherTypeIsFed(t *testing.T) {
	t.Parallel()
	unfed := map[string]bool{}
	for _, name := range events.Unfed() {
		unfed[name] = true
	}
	for _, group := range events.TypesByCategory() {
		for _, name := range group {
			if !unfed[name] && events.KeptOutOfFeed(name) {
				t.Errorf("KeptOutOfFeed(%q) is true for a type nobody declared unfed", name)
			}
		}
	}
	if events.KeptOutOfFeed("a_type_from_a_newer_peer") {
		t.Error("a type this build does not know is kept out of the feed")
	}
}

// THE OPERATOR GUIDE SAYS WHICH STORED TYPES THE FEED DOES NOT CARRY: an
// operator who finds a type in `GET /events` and never in the activity feed
// is otherwise left to conclude the feed is broken.
func TestTheGuideNamesEveryUnfedType(t *testing.T) {
	t.Parallel()
	page := readDoc(t)
	start := strings.Index(page, "| Stored, not in the activity feed |")
	if start < 0 {
		t.Fatalf("%s has no table of the stored types the feed does not carry", categoryDoc)
	}
	table := page[start:]
	if end := strings.Index(table, "\n\n"); end >= 0 {
		table = table[:end]
	}
	for _, name := range events.Unfed() {
		if !strings.Contains(table, "| `"+name+"` |") {
			t.Errorf("%s does not say %q is kept out of the activity feed", categoryDoc, name)
		}
	}
}
