package builtin_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/chatlet"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// nativeTools is what registering Worklet, or Doclet, ADDS to a seat's
// catalogue: the tool names beyond those registered with neither. Counted
// from the registry rather than written down, because
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
// never from [worklet.OperatorOnlyTools], which is the list the pages below
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
	work.Placer = func(builtin.Actor) builtin.WorkPlacer { return nil }
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
	if len(only) != len(worklet.OperatorOnlyTools()) {
		t.Fatalf("the operator surface adds %v over a seat's registry, and the "+
			"tracker declares %v operator-only — the fixture wires a different "+
			"surface than the one these pages describe", only,
			worklet.OperatorOnlyTools())
	}
	return only
}

// everyOperatorOnlyTool is [operatorOnlyTools] and the operator-only tools
// that are not the tracker's: answering a parked coding run, pausing and
// resuming a seat, steering a running turn, answering a question from the
// company's knowledge, who read a unit's repository and a person's own marks
// on their code inbox — each a person's decision, audit or gesture, served on
// the operator surface alone —
// and the chat gestures only a person makes. Counted from the catalogue with
// every seam wired — the code reads on both sides, so the code reads a seat
// holds too are not counted — for the pages that count them all.
//
// EVERY SEAM, AND CHECKED: the operator surface the fixture builds must serve
// every tool [builtin.OperatorTools] declares ([operatorCandidates]), or a
// seam this fixture leaves unwired is a tool counted as served to nobody at
// all — and the pages' count goes on matching it, one short.
func everyOperatorOnlyTool(t *testing.T) []string {
	t.Helper()
	full := fullDeps(t)
	work := full.Work
	work.ViewWriter = func(builtin.Actor) builtin.ViewWriter { return nil }
	work.CatalogueWriter = func(builtin.Actor) builtin.CatalogueWriter { return nil }
	work.PersonWriter = func(builtin.Actor) builtin.PersonWriter { return nil }
	work.TrashWriter = func(builtin.Actor) builtin.TrashWriter { return nil }
	work.Placer = func(builtin.Actor) builtin.WorkPlacer { return nil }
	work.Inbox = newFakeTracker()
	seat, err := builtin.Register(tools.NewRegistry(), builtin.Deps{Work: work,
		Pages: full.Pages, Knowledge: full.Knowledge, Chat: full.Chat, Code: full.Code})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	// THE CHAT WITH ITS READ-STATE STORE TOO, which only the operator
	// surface is given.
	talk := full.Chat
	talk.Cursors = fakeCursors{}
	actor := func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
		return builtin.Actor{Handle: "founder", Kind: worklet.AuthorOperator}, nil
	}
	company := &org.Organization{Name: "Acme"}
	answers := newAnswerRig()
	served := builtin.OperatorTools(builtin.OperatorDeps{
		Work: work, Pages: full.Pages, Knowledge: full.Knowledge, Chat: talk,
		Org:  func() *org.Organization { return company },
		Runs: builtin.RunDeps{Desk: &deskFake{}, Actor: actor},
		Pauses: builtin.SeatPauseDeps{Pauses: coordmem.NewFleet(), Announce: &announced{},
			Org: func() *org.Organization { return company }, Actor: actor},
		Steer:  builtin.SteerDeps{Asker: &fleetAsker{}, Wire: steerWire, Actor: actor},
		Answer: builtin.AnswerDeps{Models: answers.model, Budget: answers.budget, Actor: actor},
		// THE CODE READS AS EVERY SURFACE WIRES THEM — the code host and
		// the requester ceiling's halves — or the catalogue serves none of
		// them, nor the readers audit that stands beside them; AND ITS
		// WRITES, or neither they nor a person's own code mark are served.
		Code: builtin.CodeDeps{Code: full.Code.Code, Requesters: full.Code.Requesters, Chart: full.Code.Chart,
			Writer: full.Code.Writer, Holder: full.Code.Holder, Actor: builtin.PrincipalActor},
		Readers: builtin.ReadersDeps{Readers: &readersFake{}},
	})
	// THE CODE TOOLS JOIN THE LIST through codeCandidates, which this
	// fixture wires: every read and write a seat's registry takes, beside
	// the list's own audit and mark.
	declared := operatorCandidates(t) + len(builtin.CodeToolNames()) + len(builtin.CodeWrites())
	if len(served) != declared {
		names := make([]string, 0, len(served))
		for _, tool := range served {
			names = append(names, tool.Name())
		}
		t.Fatalf("the operator surface this fixture builds serves %d tools %v, "+
			"and OperatorTools declares %d: a seam is left unwired here, so the "+
			"count of what no seat is given would be short by what it omits",
			len(served), names, declared)
	}
	var only []string
	for _, tool := range served {
		if !slices.Contains(seat, tool.Name()) {
			only = append(only, tool.Name())
		}
	}
	for _, name := range append([]string{builtin.AnswerRunTool, builtin.PauseSeatTool,
		builtin.ResumeSeatTool, builtin.SteerTurnTool, builtin.AnswerKnowledgeTool,
		builtin.CodeRepoReadersTool, builtin.CodeMarkInboxTool}, chatlet.OperatorOnlyTools()...) {
		if !slices.Contains(only, name) {
			t.Fatalf("the operator surface with every seam wired does not serve %s "+
				"(it adds %v), so this count is not the one the page lists", name, only)
		}
	}
	return only
}

// chatTools is what registering Chatlet ADDS to a seat's catalogue,
// and what the operator surface adds beyond that — counted from both
// registries for the guide's own chat section, a third axis beside the
// tracker's and the pages'.
func chatTools(t *testing.T) (seat, operatorOnly []string) {
	t.Helper()
	full := fullDeps(t)
	base, err := builtin.Register(tools.NewRegistry(), builtin.Deps{})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	withChat, err := builtin.Register(tools.NewRegistry(), builtin.Deps{Chat: full.Chat})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, name := range withChat {
		if !slices.Contains(base, name) {
			seat = append(seat, name)
		}
	}
	talk := full.Chat
	talk.Cursors = fakeCursors{}
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{Chat: talk}) {
		if !slices.Contains(seat, tool.Name()) {
			operatorOnly = append(operatorOnly, tool.Name())
		}
	}
	if len(seat) == 0 || len(operatorOnly) == 0 {
		t.Fatalf("Chatlet added %d seat and %d operator tools — the "+
			"fixture wired neither, so this asserts nothing", len(seat), len(operatorOnly))
	}
	return seat, operatorOnly
}

// operatorCandidates is how many tools [builtin.OperatorTools] declares: the
// entries of its `candidates` list, read off the function's syntax tree.
//
// FROM THE SOURCE rather than from a call, because a call answers only for the
// seams its caller wired — which is the very thing this is checked against.
func operatorCandidates(t *testing.T) int {
	t.Helper()
	rel := "internal/agent/builtin/operator.go"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset,
		filepath.Join(sourcetree.Root(t), filepath.FromSlash(rel)), nil,
		parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	count := -1
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "OperatorTools" || fn.Recv != nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
				return true
			}
			if id, ok := assign.Lhs[0].(*ast.Ident); !ok || id.Name != "candidates" {
				return true
			}
			if list, ok := assign.Rhs[0].(*ast.CompositeLit); ok {
				count = len(list.Elts)
			}
			return false
		})
	}
	if count <= 0 {
		t.Fatalf("%s has no OperatorTools with a `candidates` list to count, so "+
			"the catalogue's size cannot be read", rel)
	}
	return count
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
	// WHAT NO SEAT IS GIVEN, two ways, because the pages count both: the
	// tracker's own operator-only tools, and the whole catalogue's — those
	// plus the fleet-record gestures (a parked run, a paused seat, a running
	// turn, a question answered from the knowledge base). A page that counts
	// what an operator's assistant gets beyond a seat counts the second; the
	// tracker's set is held to be inside it.
	operatorOnly := operatorOnlyTools(t)
	every := everyOperatorOnlyTool(t)
	for _, name := range operatorOnly {
		if !slices.Contains(every, name) {
			t.Errorf("the tracker's operator-only %s is missing from the "+
				"catalogue's set of what no seat is given", name)
		}
	}
	// AND THE CHAT, a third axis counted apart: what a seat holds there,
	// and the gestures only a person makes, which are inside `every` too.
	chatSeat, chatOnly := chatTools(t)
	for _, name := range chatOnly {
		if !slices.Contains(every, name) {
			t.Errorf("the chat's operator-only %s is missing from the "+
				"catalogue's set of what no seat is given", name)
		}
	}
	decisions := len(every) - len(operatorOnly) - len(chatOnly)
	title := func(word string) string { return strings.ToUpper(word[:1]) + word[1:] }

	guide := readPage(t, "docs/guides/tools-and-mcp.md")
	for _, c := range []struct{ page, text, want string }{
		{"docs/guides/tools-and-mcp.md", guide, title(spelled(t, total)) +
			" more, registered **only where the company runs Worklet or Doclet**"},
		{"docs/guides/tools-and-mcp.md", guide, "lists the tracker's " +
			spelled(t, len(tracker)) + " in full"},
		{"docs/getting-started/choosing-your-stack.md",
			readPage(t, "docs/getting-started/choosing-your-stack.md"),
			"seats get " + spelled(t, total) + " tools for them — " +
				spelled(t, len(tracker)) + " over the tracker and " +
				spelled(t, len(pages)) + " over the pages"},
		{"docs/guides/worklet.md", readPage(t, "docs/guides/worklet.md"),
			title(spelled(t, len(tracker))) + " tools, and they are deliberately few"},
		// THE INDEX'S SUMMARY OF THAT GUIDE, and the quickstart's account of
		// what an operator's own assistant gets — both hand-kept counts of
		// the same registry, which is how one page drifts from the rest.
		{"docs/index.md", readPage(t, "docs/index.md"),
			"the " + spelled(t, len(tracker)) + " tools a seat has and the " +
				spelled(t, len(builtin.WorkWrites())) + " that count as a delivery"},
		{"docs/getting-started/quickstart.md",
			readPage(t, "docs/getting-started/quickstart.md"),
			"tools a seat holds (" + spelled(t, len(tracker)) + " over the tracker, " +
				spelled(t, len(pages)) + " over the pages and " +
				spelled(t, len(chatSeat)) + " in the chat) and " +
				spelled(t, len(every)) + " more that no seat is given"},
		// THE GUIDE COUNTS THE TWO HALVES APART: the tracker's beside the
		// tables, and the person's decisions about the company after them.
		{"docs/guides/tools-and-mcp.md", guide, "and " + spelled(t, len(operatorOnly)) +
			" more beside them that no seat is given"},
		{"docs/guides/tools-and-mcp.md", guide,
			title(spelled(t, decisions)) +
				" more are a person's decisions about the company"},
		// AND THE GUIDE'S CHAT SECTION COUNTS ITS OWN: what a seat holds,
		// and what a person's surface adds.
		{"docs/guides/tools-and-mcp.md", guide, title(spelled(t, len(chatSeat))) +
			" more, on the same terms and on the third axis"},
		{"docs/guides/tools-and-mcp.md", guide, "and " +
			spelled(t, len(chatOnly)) + " more that no seat is given"},
		{"docs/reference/api-endpoints.md",
			readPage(t, "docs/reference/api-endpoints.md"),
			"the " + spelled(t, len(chatOnly)) + " chat gestures only a person makes"},
		// THE REFERENCE LISTS EVERY ONE, the tracker's and the rest.
		{"docs/reference/api-endpoints.md",
			readPage(t, "docs/reference/api-endpoints.md"),
			"Plus **" + spelled(t, len(every)) + " no seat is given**"},
	} {
		if !strings.Contains(c.text, c.want) {
			t.Errorf("%s does not say %q — the registry adds %d tracker and %d "+
				"page tools", c.page, c.want, len(tracker), len(pages))
		}
	}

	// AND THE GUIDE'S TABLE: every tool in it is one the registry adds, and
	// the sentence above it counts its rows and the rest.
	start := strings.Index(guide, "### Worklet and Doclet")
	end := strings.Index(guide, "The writes on each side count as a **delivery**")
	if start < 0 || end < start {
		t.Fatal("the tools guide has no Worklet and Doclet section to read")
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

	// AND THE ARTIFACTS SECTION, counted off the two lists the registry adds on
	// every node that takes artifact uploads, which the operator catalogue
	// builds from the same sets. Kept by hand, it read "Twelve more" above
	// nine and four.
	art, data := len(builtin.ArtifactTools()), len(builtin.ArtifactDataTools())
	for _, want := range []string{
		title(spelled(t, art+data)) + " more, registered on every node that takes " +
			"artifact uploads",
		spelled(t, art) + " for artifacts, their folders and the company's brand, and " +
			spelled(t, data) + " for an artifact's datastore",
		"the operator catalogue serves the same " + spelled(t, art+data),
	} {
		if !strings.Contains(guide, want) {
			t.Errorf("the tools guide does not say %q — the registry adds %d artifact "+
				"and %d datastore tools", want, art, data)
		}
	}

	// AND THE REFERENCE'S LIST of the artifact tools the operator surface
	// serves beside a seat's: every one of the two lists, in their order.
	// Kept by hand, it left out list_dashboard_sources and then
	// get_artifact_format, both served all along.
	reference := readPage(t, "docs/reference/api-endpoints.md")
	from := strings.Index(reference, "wherever the node takes artifact uploads,")
	to := strings.Index(reference, "(see [Artifacts]")
	if from < 0 || to < from {
		t.Fatal("the API reference has no list of the artifact tools the operator " +
			"surface serves to read")
	}
	var named []string
	for _, m := range regexp.MustCompile("`([a-z_]+)`").
		FindAllStringSubmatch(reference[from:to], -1) {
		named = append(named, m[1])
	}
	if want := append(builtin.ArtifactTools(), builtin.ArtifactDataTools()...); !slices.Equal(named, want) {
		t.Errorf("the API reference lists the artifact tools the operator surface "+
			"serves as %v, and the registry adds %v", named, want)
	}
}
