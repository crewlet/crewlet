package builtin_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// nativeTools is what registering the native tracker, or the native knowledge
// base, ADDS to a seat's catalogue: the tool names beyond those registered
// with neither. Counted from the registry rather than written down, because
// the pages below state the count in words and a count kept by hand is how
// one page came to say eighteen while listing nineteen.
func nativeTools(t *testing.T) (tracker, pages []string) {
	t.Helper()
	full := fullDeps(t)
	register := func(deps builtin.Deps) []string {
		names, err := builtin.Register(tools.NewRegistry(), deps)
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		return names
	}
	base := register(builtin.Deps{})
	beyond := func(names []string) []string {
		var out []string
		for _, name := range names {
			if !slices.Contains(base, name) {
				out = append(out, name)
			}
		}
		return out
	}
	tracker = beyond(register(builtin.Deps{Work: full.Work}))
	pages = beyond(register(builtin.Deps{Pages: full.Pages}))
	if len(tracker) == 0 || len(pages) == 0 {
		t.Fatalf("the native backends added %d tracker and %d page tools — the "+
			"fixture wired neither, so this asserts nothing", len(tracker), len(pages))
	}
	return tracker, pages
}

// operatorOnlyTools is what the operator's surface serves that no seat's
// registry does, counted from both catalogues with every write side wired —
// never from [tracker.OperatorOnlyTools], which is the list the pages below
// would otherwise be checked against their own copy of.
func operatorOnlyTools(t *testing.T) []string {
	t.Helper()
	full := fullDeps(t)
	// EVERY WRITE SIDE WIRED on both, so a tool missing from the seat's
	// registry is missing because the registry does not offer it rather than
	// because its dependency was nil.
	work := full.Work
	work.ViewWriter = func(builtin.Actor) builtin.ViewWriter { return nil }
	work.CatalogueWriter = func(builtin.Actor) builtin.CatalogueWriter { return nil }
	work.PersonWriter = func(builtin.Actor) builtin.PersonWriter { return nil }
	work.TrashWriter = func(builtin.Actor) builtin.TrashWriter { return nil }
	work.Inbox = newFakeTracker()
	seat, err := builtin.Register(tools.NewRegistry(), builtin.Deps{Work: work,
		Pages: full.Pages})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	var only []string
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{Work: work,
		Pages: full.Pages}) {
		if !slices.Contains(seat, tool.Name()) {
			only = append(only, tool.Name())
		}
	}
	if len(only) != len(tracker.OperatorOnlyTools()) {
		t.Fatalf("the operator surface adds %v over a seat's registry, and the "+
			"tracker declares %v operator-only — the fixture wires a different "+
			"surface than the one these pages describe", only,
			tracker.OperatorOnlyTools())
	}
	return only
}

// numberWords spells the counts these pages state. Past twenty-nine a page
// would be better served by a table than a sentence, and this fails loudly.
var numberWords = []string{"zero", "one", "two", "three", "four", "five", "six",
	"seven", "eight", "nine", "ten", "eleven", "twelve", "thirteen", "fourteen",
	"fifteen", "sixteen", "seventeen", "eighteen", "nineteen", "twenty",
	"twenty-one", "twenty-two", "twenty-three", "twenty-four", "twenty-five",
	"twenty-six", "twenty-seven", "twenty-eight", "twenty-nine"}

func spelled(t *testing.T, n int) string {
	t.Helper()
	if n >= len(numberWords) {
		t.Fatalf("%d tools is past what a sentence should count", n)
	}
	return numberWords[n]
}

// readPage is one docs page with its whitespace collapsed, so a sentence the
// page wraps across lines still reads as one.
func readPage(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(sourcetree.Root(t), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return strings.Join(strings.Fields(string(raw)), " ")
}

// EVERY PAGE THAT COUNTS THE NATIVE TOOLS COUNTS WHAT THE REGISTRY HOLDS.
//
// The tools guide said "Eighteen more" above a table and a sentence that added
// up to nineteen, and the stack guide said nineteen: two pages disagreeing
// about a number the registry answers, because each was kept by hand.
func TestThePagesCountTheNativeToolsTheRegistryHolds(t *testing.T) {
	t.Parallel()
	tracker, pages := nativeTools(t)
	total := len(tracker) + len(pages)
	operatorOnly := operatorOnlyTools(t)
	title := func(word string) string { return strings.ToUpper(word[:1]) + word[1:] }

	guide := readPage(t, "docs/guides/tools-and-mcp.md")
	for _, c := range []struct{ page, text, want string }{
		{"docs/guides/tools-and-mcp.md", guide, title(spelled(t, total)) +
			" more, registered **only where the company runs the engine's own backends**"},
		{"docs/guides/tools-and-mcp.md", guide, "lists the tracker's " +
			spelled(t, len(tracker)) + " in full"},
		{"docs/getting-started/choosing-your-stack.md",
			readPage(t, "docs/getting-started/choosing-your-stack.md"),
			"seats get " + spelled(t, total) + " tools for them — " +
				spelled(t, len(tracker)) + " over the tracker and " +
				spelled(t, len(pages)) + " over the pages"},
		{"docs/guides/work-tracker.md", readPage(t, "docs/guides/work-tracker.md"),
			title(spelled(t, len(tracker))) + " tools, and they are deliberately few"},
		// THE INDEX'S SUMMARY OF THAT GUIDE, and the quickstart's account of
		// what an operator's own assistant gets — both hand-kept counts of
		// the same registry, which is how one page drifts from the rest.
		{"docs/index.md", readPage(t, "docs/index.md"),
			"the " + spelled(t, len(tracker)) + " tools a seat has and the " +
				spelled(t, len(builtin.WorkWrites())) + " that count as a delivery"},
		{"docs/getting-started/quickstart.md",
			readPage(t, "docs/getting-started/quickstart.md"),
			"tools a seat holds — " + spelled(t, len(tracker)) + " over the tracker and " +
				spelled(t, len(pages)) + " over the pages — and " +
				spelled(t, len(operatorOnly)) + " more that no seat is given"},
		{"docs/guides/tools-and-mcp.md", guide, "and " + spelled(t, len(operatorOnly)) +
			" more beside them that no seat is given"},
		{"docs/reference/api-endpoints.md",
			readPage(t, "docs/reference/api-endpoints.md"),
			"Plus **" + spelled(t, len(operatorOnly)) + " no seat is given**"},
	} {
		if !strings.Contains(c.text, c.want) {
			t.Errorf("%s does not say %q — the registry adds %d tracker and %d "+
				"page tools", c.page, c.want, len(tracker), len(pages))
		}
	}

	// AND THE GUIDE'S TABLE: every tool in it is one the registry adds, and
	// the sentence above it counts its rows and the rest.
	start := strings.Index(guide, "### The native tracker and knowledge base")
	end := strings.Index(guide, "The writes on each side count as a **delivery**")
	if start < 0 || end < start {
		t.Fatal("the tools guide has no native-tools section to read")
	}
	var rows []string
	for _, m := range regexp.MustCompile("\\| `([a-z_]+)` \\|").
		FindAllStringSubmatch(guide[start:end], -1) {
		rows = append(rows, m[1])
	}
	for _, name := range rows {
		if !slices.Contains(tracker, name) && !slices.Contains(pages, name) {
			t.Errorf("the tools guide lists %s as a native tool and the registry "+
				"adds no such tool", name)
		}
	}
	listed := 0
	for _, name := range tracker {
		if slices.Contains(rows, name) {
			listed++
		}
	}
	if want := "The " + spelled(t, len(rows)) + " below are the item, file and page " +
		"tools; the other " + spelled(t, total-len(rows)) + " read"; !strings.Contains(guide, want) {
		t.Errorf("the tools guide's table does not add up: want %q (%d rows of %d "+
			"tools)", want, len(rows), total)
	}
	if listed+len(pages) != len(rows) {
		t.Errorf("the tools guide's table lists %d tools, want every page tool (%d) "+
			"and the tracker's item and file tools (%d)", len(rows), len(pages), listed)
	}
}
