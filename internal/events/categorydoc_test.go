package events_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	_ "github.com/crewlet/crewlet/internal/events/types"
)

// categoryDoc is the operator-facing vocabulary for the `category` column —
// the values `GET /events?category=` and the dashboard's filter accept.
const categoryDoc = "../../docs/guides/deployment.md"

// An operator scripting against the filter needs to know what to put in it,
// and the value set is a Go map they cannot read. So the docs carry a table —
// and a table maintained by hand goes stale on the first event type somebody
// adds, silently, in the direction that matters least to the person who added
// it and most to the person reading it.
//
// This is the guard, and it fails in BOTH directions: a type in the map that
// the page does not list, and a type on the page that no longer exists.
func TestTheDocumentedCategoryVocabularyMatchesTheMap(t *testing.T) {
	t.Parallel()
	page := readDoc(t)

	for _, category := range events.CategoryNames() {
		if !strings.Contains(page, "| `"+category+"` |") {
			t.Errorf("category %q has no row in %s — an operator filtering on it "+
				"has no way to learn it exists", category, categoryDoc)
		}
	}
	for eventType := range allTypes() {
		if !strings.Contains(page, "`"+eventType+"`") {
			t.Errorf("event type %q is stored under a category but is not in %s; "+
				"add it to the table in § What gets stored", eventType, categoryDoc)
		}
	}
	for eventType, reason := range events.Exclusions() {
		if !strings.Contains(page, "`"+eventType+"`") {
			t.Errorf("event type %q is deliberately excluded from the store and "+
				"%s does not say so; an operator looking for it in a query finds "+
				"nothing and no explanation (%s)", eventType, categoryDoc, reason)
		}
	}
}

// And the other direction: a type the page names that the engine no longer
// has. A stale row is worse than a missing one — it sends somebody to build a
// filter against a value nothing will ever match.
func TestTheDocumentedTableNamesNoVanishedType(t *testing.T) {
	t.Parallel()
	known := allTypes()
	for name := range events.Exclusions() {
		known[name] = true
	}

	page := readDoc(t)
	start := strings.Index(page, "#### What gets stored")
	if start < 0 {
		t.Fatalf("%s has no § What gets stored — this guard is asserting nothing", categoryDoc)
	}
	end := strings.Index(page[start:], "#### Querying events")
	if end < 0 {
		t.Fatalf("%s § What gets stored has no end marker", categoryDoc)
	}
	section := page[start : start+end]

	var vanished []string
	for _, quoted := range strings.Split(section, "`") {
		// Only the odd-indexed pieces are inside backticks, but scanning
		// every piece and filtering on shape is simpler and cannot get
		// the parity wrong: an event type is snake_case or dotted, never
		// prose.
		if !looksLikeEventType(quoted) || known[quoted] {
			continue
		}
		vanished = append(vanished, quoted)
	}
	slices.Sort(vanished)
	vanished = slices.Compact(vanished)
	if len(vanished) > 0 {
		t.Errorf("%s names event types the engine no longer has:\n  %s\n\nA stale "+
			"row sends an operator to build a filter against a value nothing "+
			"will ever match.", categoryDoc, strings.Join(vanished, "\n  "))
	}
}

func readDoc(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(categoryDoc)
	if err != nil {
		t.Fatalf("read %s: %v", categoryDoc, err)
	}
	return string(raw)
}

// allTypes is every type that has a category, as a set.
func allTypes() map[string]bool {
	out := map[string]bool{}
	for _, group := range events.TypesByCategory() {
		for _, name := range group {
			out[name] = true
		}
	}
	return out
}

// looksLikeEventType matches the wire-type grammar: lower-case words joined by
// underscores, optionally one dotted prefix (`phase.tool_activated`). It
// deliberately does not match a category name, a column name or prose.
func looksLikeEventType(s string) bool {
	if s == "" || !strings.Contains(s, "_") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '.':
		default:
			// Digits are in the set because a2a_* is a real prefix.
			// Leaving them out is how the first version of this guard
			// passed while the table named a type that does not exist.
			return false
		}
	}
	// Column names live in the same page and share the grammar. They are
	// listed in the paragraph above the table, not inside it, so the
	// section bound already excludes them — but naming them here keeps the
	// guard honest if that paragraph ever moves.
	return !slices.Contains([]string{
		"event_type", "agent_id", "agent_role", "task_id", "channel_id", "trace_id",
	}, s)
}

// A category name printed in the docs has to be a value the engine can
// actually file a row under, `webhook` included — which no event TYPE carries,
// because the receiver writes that row itself.
func TestWebhookIsInTheVocabulary(t *testing.T) {
	t.Parallel()
	if !slices.Contains(events.CategoryNames(), events.WebhookCategory) {
		t.Fatalf("CategoryNames() = %v, want %q among them: the receiver files "+
			"every delivery under it, so a vocabulary without it reads as "+
			"complete and is wrong",
			events.CategoryNames(), events.WebhookCategory)
	}
	if _, placed := events.Category("raw_webhook"); placed {
		t.Error("raw_webhook is categorised; it must stay excluded, or every " +
			"delivery is stored twice")
	}
}
