package adr_test

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/adr"
	"github.com/crewlet/crewlet/internal/regexlit"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// EVERY RECORD IS WELL FORMED AND CARRIES THE FIELDS THAT MAKE IT USABLE.
//
// The field list is short on purpose (see [adr.Required]). What is checked
// here is that each one is present and says something — not that it says
// something true, which no gate reaches.
func TestEveryRecordCarriesItsFields(t *testing.T) {
	t.Parallel()
	records := load(t)

	for _, r := range records {
		for _, field := range adr.Required {
			if strings.TrimSpace(r.Fields[field]) == "" {
				t.Errorf("%s has no %s.\n\t%s", r.File, field, whyRequired[field])
			}
		}
		switch r.Status() {
		case "accepted":
		case "superseded":
			if strings.TrimSpace(r.Fields["Superseded-by"]) == "" {
				t.Errorf("%s is superseded and does not say by what, so a "+
					"reader who finds it has no way to reach the decision "+
					"that replaced it", r.File)
			}
		default:
			t.Errorf("%s has status %q; a record is `accepted` or "+
				"`superseded`", r.File, r.Status())
		}
		if strings.TrimSpace(r.Title) == "" {
			t.Errorf("%s has no title on its heading line", r.File)
		}
	}
	if len(records) == 0 {
		t.Fatal("no records were loaded, so every check in this file is " +
			"passing over an empty list")
	}
	t.Logf("%d record(s)", len(records))
}

// TWO RECORDS WITH ONE NUMBER ARE REFUSED BY THE READER, not noticed beside it.
//
// [adr.Load] is what every check in this file reads through, and each of them
// keys the records by id — so a duplicate a test merely logged still reached
// them as ONE record, and the anchor and citation checks certified whichever
// of the two the map kept. The control loads the same two records under two
// numbers, so the refusal is shown to be about the number and nothing else.
func TestTwoRecordsWithOneNumberAreRefused(t *testing.T) {
	t.Parallel()
	record := func(n int, title string) string {
		return recordUnder("# ADR-" + fmt.Sprintf("%04d", n) + ": " + title)
	}

	control := recordTree(t, map[string]string{
		"0001-one-decision.md":     record(1, "One decision"),
		"0002-another-decision.md": record(2, "Another decision"),
	})
	records, err := adr.Load(control)
	if err != nil || len(records) != 2 {
		t.Fatalf("control: two records under two numbers loaded %d record(s), "+
			"err %v — the refusal below would prove nothing", len(records), err)
	}

	duplicated := recordTree(t, map[string]string{
		"0001-one-decision.md":     record(1, "One decision"),
		"0001-another-decision.md": record(1, "Another decision"),
	})
	records, err = adr.Load(duplicated)
	if err == nil {
		t.Fatalf("two records numbered 0001 loaded as %d record(s) with no "+
			"error; a citation of ADR-0001 would reach two decisions while "+
			"every check read one of them", len(records))
	}
	for _, name := range []string{"0001-one-decision.md", "0001-another-decision.md"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the refusal %q does not name %s, so the reader cannot "+
				"tell which two records collided", err, name)
		}
	}
}

// whyRequired is what a missing field actually costs, for the failure message.
//
// A message that says "field missing" teaches nothing; the point of each field
// is a specific failure it prevents, and the person adding a record is the one
// who needs to hear it.
var whyRequired = map[string]string{
	"Status": "A record with no status cannot be told from one that was " +
		"superseded and left in place, which is how a reader acts on a " +
		"decision that was reversed.",
	"Authority": "The authority is the one place the detail lives. Without " +
		"it this record has to restate its package's doc, which is the " +
		"duplication ADR-0008 is about — and the anchor that puts the " +
		"decision in front of the next reader does not exist.",
	"Enforced-by": "This is the field the whole directory is for. Name the " +
		"test that holds the decision, or the compile error that does, or " +
		"write `nothing` and add an entry to adr.Unenforced saying what a " +
		"gate would have to look at. Leaving it out is the one answer that " +
		"is not allowed.",
	"Tag-status": "A `v*` tag is the only compatibility boundary this project " +
		"has. Without this field a reader cannot tell a decision they may " +
		"change outright from one an operator is running.",
}

// THE ANCHOR, IN BOTH DIRECTIONS. Without one, nothing points at a record, so
// nobody reads it and nobody corrects it.
//
// Forward: a record names an authority, and that authority's own doc carries
// the record's id — so `go doc ./internal/statelog` shows ADR-0002 to the
// person about to change the state log.
//
// Backward: a doc that cites a record which does not exist fails, because a
// dangling reference reads exactly like a live one and no other gate sees it.
func TestEveryRecordIsAnchoredAtItsAuthority(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	records := load(t)

	declared := map[string]adr.Record{}
	for _, r := range records {
		declared[r.ID] = r
	}

	for _, r := range records {
		path, isPackage, err := adr.AuthorityPath(r.Authority())
		if err != nil {
			t.Errorf("%s: %v", r.File, err)
			continue
		}
		full := filepath.Join(root, path)
		if _, statErr := os.Stat(full); statErr != nil {
			t.Errorf("%s names %s as its authority and there is no such "+
				"package or page — the record points at nothing, which is "+
				"how a reader concludes the decision was withdrawn",
				r.File, r.Authority())
			continue
		}
		text := authorityText(t, full, isPackage)
		if !strings.Contains(text, r.ID) {
			what := "package doc"
			if !isPackage {
				what = "page"
			}
			t.Errorf("%s names %s as its authority and that %s does not "+
				"mention %s.\n\tThe anchor is what puts this decision in "+
				"front of somebody about to change that code: add %s to the "+
				"doc comment, in the sentence it belongs to. A record nothing "+
				"points at is a record nobody reads, which is exactly how "+
				"docs/reference/design-decisions.md drifted.",
				r.File, r.Authority(), what, r.ID, r.ID)
		}
	}

	// THE OTHER DIRECTION, over the whole tree.
	dangling, total := danglingCitations(t, root, declared)
	for _, cite := range dangling {
		t.Errorf("%s:%d cites %s and no record declares it — either the "+
			"record was never written, or it was renumbered and this "+
			"reference now points at nothing", cite.File, cite.Line, cite.ID)
	}
	// AND IT READ CITATIONS AT ALL. Counting files proves the walk ran and
	// says nothing about the matcher: an [adr.Reference] that stopped
	// matching finds no citation, so none dangles, and this direction
	// passes having checked nothing.
	if total < minCitations {
		t.Fatalf("found %d citations of a record in the tree, under the floor "+
			"of %d — adr.Reference has stopped recognising a citation, so the "+
			"backward check is certifying almost nothing", total, minCitations)
	}
	t.Logf("%d citation(s) of a record, %d of them dangling", total, len(dangling))
}

// minCitations is the fewest citations of a record the tree may hold. There
// are 379 today, in 218 files; a floor near a quarter of that survives a
// rewrite of any one page or package doc and fails the day the matcher, or
// the walk, stops finding them.
const minCitations = 100

// recordUnder is a record with every [adr.Required] field, opening with the
// given title line.
func recordUnder(heading string) string {
	return heading + "\n\n" +
		"- **Status:** accepted\n" +
		"- **Authority:** `internal/adr`\n" +
		"- **Enforced-by:** nothing\n" +
		"- **Tag-status:** unreleased\n\n" +
		"## The decision\n\nA body.\n"
}

// recordTree writes files (name to body) into a fresh tree's [adr.Dir] and
// answers the tree's root.
func recordTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, adr.Dir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// A RECORD'S HEADING SEPARATES ITS ID FROM ITS TITLE WITH A COLON, and the em
// dash form it replaced is refused rather than skipped.
//
// The gate once required an em dash there, which this repository's prose no
// longer takes. A heading in the old form still opens with "# ", so the reader
// names it and the form it wants instead of reading on and reporting a record
// with no heading at all. The control proves the colon form loads, with the id
// and a title that keeps a colon of its own.
func TestARecordHeadingIsTheColonForm(t *testing.T) {
	t.Parallel()
	const name = "0001-one-decision.md"
	records, err := adr.Load(recordTree(t, map[string]string{
		name: recordUnder("# ADR-0001: One decision: and its qualifier"),
	}))
	if err != nil || len(records) != 1 {
		t.Fatalf("control: a colon heading loaded %d record(s), err %v; the "+
			"refusals below would prove nothing", len(records), err)
	}
	if got := records[0]; got.ID != "ADR-0001" || got.Title != "One decision: and its qualifier" {
		t.Errorf("a colon heading read as id %q and title %q", got.ID, got.Title)
	}

	for label, line := range map[string]string{
		"an em dash":       "# ADR-0001 \u2014 One decision",
		"no separator":     "# ADR-0001 One decision",
		"no space after":   "# ADR-0001:One decision",
		"a space before":   "# ADR-0001 : One decision",
		"no title":         "# ADR-0001: ",
		"a three digit id": "# ADR-001: One decision",
	} {
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			_, err := adr.Load(recordTree(t, map[string]string{name: recordUnder(line)}))
			if err == nil {
				t.Fatalf("a record opening %q loaded", line)
			}
			if !strings.Contains(err.Error(), "`# ADR-NNNN: Title`") {
				t.Errorf("the refusal of %q does not name the colon form: %v", line, err)
			}
		})
	}
}

// A FIELD'S LINE IS AS LONG AS ITS VALUE. A value written on its line, rather
// than listed beneath it, may run past a line scanner's default cap of 64 KiB;
// the record still loads, its field whole. The control is a field just under
// that cap.
func TestAFieldLongerThanAScannersLineLoadsWhole(t *testing.T) {
	t.Parallel()
	for label, size := range map[string]int{"under 64 KiB": 60 << 10, "over 64 KiB": 96 << 10} {
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			gates := strings.Repeat("`internal/adr.TestX`, ", size/22)
			body := strings.Replace(recordUnder("# ADR-0001: One decision"),
				"- **Enforced-by:** nothing\n", "- **Enforced-by:** "+gates+"`internal/adr.TestY`\n", 1)
			records, err := adr.Load(recordTree(t, map[string]string{"0001-one-decision.md": body}))
			if err != nil || len(records) != 1 {
				t.Fatalf("a record whose Enforced-by runs %d bytes loaded %d record(s), err %v",
					len(gates), len(records), err)
			}
			if got := records[0].Fields["Enforced-by"]; !strings.HasSuffix(got, "internal/adr.TestY") {
				t.Errorf("the field read as %d bytes ending %q, want it whole", len(got), got[max(0, len(got)-40):])
			}
		})
	}
}

// A FIELD LISTED BENEATH ITS LINE READS AS THE SAME ENTRIES WRITTEN ON IT, so
// every check over Enforced-by sees one set of gates whichever form a record
// takes. The list ends at the next field, across a blank line inside it, and
// at the body, whichever comes first.
func TestAListedFieldReadsAsItsEntriesWrittenOnItsLine(t *testing.T) {
	t.Parallel()
	entries := []string{"`internal/adr.TestA`", "`internal/engine.TestB` and its control", "`cmd/crewlet.TestC`"}
	oneLine := "- **Enforced-by:** " + strings.Join(entries, ", ") + "\n"
	listed := "- **Enforced-by:**\n  - " + entries[0] + "\n  - " + entries[1] + "\n\n  - " + entries[2] + "\n"
	last := func(field string) string { // the field last in the header
		return "# ADR-0001: One decision\n\n- **Status:** accepted\n- **Authority:** `internal/adr`\n" +
			"- **Tag-status:** unreleased\n" + field + "\n## The decision\n\nA body.\n"
	}
	before := func(field string) string { // the field followed by another
		return strings.Replace(recordUnder("# ADR-0001: One decision"), "- **Enforced-by:** nothing\n", field, 1)
	}
	read := func(t *testing.T, body string) adr.Record {
		t.Helper()
		records, err := adr.Load(recordTree(t, map[string]string{"0001-one-decision.md": body}))
		if err != nil || len(records) != 1 {
			t.Fatalf("loaded %d record(s), err %v", len(records), err)
		}
		return records[0]
	}

	want := read(t, before(oneLine))
	if got := want.EnforcedBy(); got != "internal/adr.TestA`, `internal/engine.TestB` and its control, `cmd/crewlet.TestC" {
		t.Fatalf("control: the one-line form read as %q", got)
	}
	for label, body := range map[string]string{"before another field": before(listed), "last in the header": last(listed)} {
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			if got := read(t, body); !maps.Equal(got.Fields, want.Fields) {
				t.Errorf("the listed form read as %v, the one-line form as %v", got.Fields, want.Fields)
			}
		})
	}
}

// AN ENTRY BELONGS TO THE FIELD LISTED ABOVE IT, AND ANY OTHER IS REFUSED rather
// than read into a field it was not written under or dropped with the header's
// end. The control is the same shape with every entry beneath a listed field.
func TestAnEntryNoListedFieldOwnsIsRefused(t *testing.T) {
	t.Parallel()
	const name = "0001-one-decision.md"
	record := func(header string) string {
		return "# ADR-0001: One decision\n\n" + header + "- **Tag-status:** unreleased\n\n## The decision\n\nA body.\n"
	}
	if _, err := adr.Load(recordTree(t, map[string]string{name: record(
		"- **Status:** accepted\n- **Enforced-by:**\n  - `internal/adr.TestA`\n")})); err != nil {
		t.Fatalf("control: a listed field failed to load: %v", err)
	}

	for label, c := range map[string]struct{ header, want string }{
		"before the first field": {
			"  - `internal/adr.TestA`\n- **Status:** accepted\n- **Enforced-by:** nothing\n", "before its first field"},
		"beneath a value on its line": {
			"- **Status:** accepted\n- **Enforced-by:** `internal/adr.TestA`\n  - `internal/adr.TestB`\n", "not both"},
		"beneath a value across a blank line": {
			"- **Status:** accepted\n- **Enforced-by:** `internal/adr.TestA`\n\n  - `internal/adr.TestB`\n", "not both"},
		"a listed field with no entry": {
			"- **Status:** accepted\n- **Enforced-by:**\n", "no entry beneath it"},
		"an entry wrapped onto a second line": {
			"- **Status:** accepted\n- **Enforced-by:**\n  - `internal/adr.TestA` and\n    its control\n", "not an entry"},
		"a listed field set twice": {
			"- **Status:** accepted\n- **Enforced-by:**\n  - `internal/adr.TestA`\n- **Enforced-by:** nothing\n", "twice"},
	} {
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			_, err := adr.Load(recordTree(t, map[string]string{name: record(c.header)}))
			if err == nil {
				t.Fatalf("a record whose header is\n%s\nloaded", c.header)
			}
			if !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), c.want) {
				t.Errorf("the refusal does not name %s and say %q: %v", name, c.want, err)
			}
		})
	}
}

// THE HEADER STILL ENDS AT ITS FIRST LINE THAT IS NEITHER A FIELD NOR AN ENTRY
// OF ONE: a bullet or an indented entry in the body is the body's, never a
// field of its own nor one more entry of the field listed last.
func TestTheHeaderEndsWhereTheBodyBegins(t *testing.T) {
	t.Parallel()
	const head = "# ADR-0001: One decision\n\n- **Status:** accepted\n- **Enforced-by:**\n  - `internal/adr.TestA`\n"
	const body = "- **Note:** a bullet\n  - `internal/adr.TestZ`\n"
	for label, record := range map[string]string{
		"at a heading":   head + "\n## The decision\n\n" + body,
		"at a paragraph": head + "A paragraph straight after the list.\n" + body,
	} {
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			records, err := adr.Load(recordTree(t, map[string]string{"0001-one-decision.md": record}))
			if err != nil || len(records) != 1 {
				t.Fatalf("loaded %d record(s), err %v", len(records), err)
			}
			if got := records[0]; got.EnforcedBy() != "internal/adr.TestA" || got.Fields["Note"] != "" {
				t.Errorf("the header read past its end: Enforced-by %q, Note %q",
					got.EnforcedBy(), got.Fields["Note"])
			}
		})
	}
}

// THE BACKWARD CHECK, ON A TREE WHOSE VERDICT IS KNOWN — through the same walk,
// the same prefilter and the same matcher the gate runs.
//
// A citation of a record nobody wrote is what that direction exists to catch,
// and nothing in the real tree is one, so without a planted one the check is
// never seen to fail. It is planted in a package doc and on a page, beside a
// citation of a record that exists, and where the walk must not look: the
// template, the committed bundle, and a file that is neither Go nor markdown.
//
// Every id below is ASSEMBLED, because this file is in the tree the gate
// walks: written out whole, each would be a dangling citation of its own.
func TestACitationOfARecordNobodyWroteIsReported(t *testing.T) {
	t.Parallel()
	records := load(t)
	if len(records) == 0 {
		t.Fatal("no records were loaded, so no citation can be declared")
	}
	id := func(number string) string { return "ADR" + "-" + number }
	live, nobodys := records[0].ID, id("9999")
	root := t.TempDir()
	for path, body := range map[string]string{
		"internal/x/x.go":      "// The rule is " + nobodys + "'s.\npackage x\n",
		"internal/x/x_test.go": "package x // cites nothing\n",
		"docs/page.md":         "# Page\n\nSee " + live + " and " + nobodys + ".\n",
		"adr/0000-template.md": "# " + id("0000") + ": Title\n",
		"static/bundle/b.go":   "// " + id("9998") + "\npackage b\n",
		"docs/notes.txt":       id("9997") + "\n",
	} {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	declared := map[string]adr.Record{}
	for _, r := range records {
		declared[r.ID] = r
	}
	dangling, total := danglingCitations(t, root, declared)
	if total != 3 {
		t.Errorf("read %d citations in the planted tree, want 3 — two of %s "+
			"and one of %s", total, nobodys, live)
	}
	var got []string
	for _, cite := range dangling {
		got = append(got, cite.File+":"+strconv.Itoa(cite.Line)+" "+cite.ID)
	}
	slices.Sort(got)
	want := []string{
		filepath.FromSlash("docs/page.md") + ":3 " + nobodys,
		filepath.FromSlash("internal/x/x.go") + ":1 " + nobodys,
	}
	if !slices.Equal(got, want) {
		t.Errorf("dangling citations = %q, want %q", got, want)
	}
}

// AND THE INDEX IS THE RECORDS, each on one row.
//
// adr/README.md carries the table a reader scans, and a table maintained by
// hand goes stale on the first record somebody adds — silently, and in the
// direction that matters least to the person who added it. A record's row is
// counted as well as found: a merge that keeps both sides of a conflict in the
// table lists a record twice, and a reader of two rows cannot tell which one
// is current. ADR-0071 sat on two identical rows while the gate only asked
// whether its file name appeared.
func TestTheIndexNamesEveryRecord(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	index := read(t, filepath.Join(root, adr.Dir, "README.md"))

	for _, r := range load(t) {
		if !strings.Contains(index, r.File) {
			t.Errorf("%s is not linked from adr/README.md, so it is reachable "+
				"only by listing the directory", r.File)
			continue
		}
		if rows := indexRows(index, r.File); rows != 1 {
			t.Errorf("%s has %d rows in adr/README.md's table, want one: delete "+
				"the copies, keeping the row that states the decision as it stands",
				r.File, rows)
		}
	}
	for _, link := range markdownLinks(index) {
		if !strings.HasSuffix(link, ".md") || link == "0000-template.md" {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, adr.Dir, link)); err != nil {
			t.Errorf("adr/README.md links %s and there is no such record", link)
		}
	}
}

// indexRows counts the table rows of index whose first cell links file.
func indexRows(index, file string) int {
	n := 0
	for _, line := range strings.Split(index, "\n") {
		rest, ok := strings.CutPrefix(line, "| [")
		if !ok {
			continue
		}
		if _, after, found := strings.Cut(rest, "]("); found && strings.HasPrefix(after, file+")") {
			n++
		}
	}
	return n
}

// A ROW IS COUNTED BY ITS LINK, so a row that names a record in its prose is
// not a second row for it, and a repeated row is.
func TestAnIndexRowIsCountedByItsLink(t *testing.T) {
	t.Parallel()
	index := "| [0001](0001-a.md) | A, which [0002](0002-b.md) refines | `TestA` |\n" +
		"| [0002](0002-b.md) | B | `TestB` |\n" +
		"| [0002](0002-b.md) | B | `TestB` |\n" +
		"See [0001](0001-a.md) for the first.\n"
	for file, want := range map[string]int{"0001-a.md": 1, "0002-b.md": 2, "0003-c.md": 0} {
		if got := indexRows(index, file); got != want {
			t.Errorf("indexRows(%s) = %d, want %d", file, got, want)
		}
	}
}

// AN UNENFORCED DECISION IS DECLARED, AND THE DECLARATION IS TWO-SIDED.
//
// `Enforced-by: nothing` is an allowed and often honest answer — this tree has
// several rules no walk over source can reach. What is not allowed is writing
// it without saying what a gate would have to look at, because that sentence is
// the thing somebody later turns into the gate.
//
// Both directions, for internal/skipgate's reason: an entry that stopped being
// true is worse than no entry, since it sits where the next reader takes it for
// a live one. A record that acquires a gate and stays on this list fails here.
func TestAnUnenforcedDecisionSaysWhyAndStaysTrue(t *testing.T) {
	t.Parallel()
	records := load(t)

	exempt := map[string]string{}
	for _, e := range adr.Unenforced {
		if strings.TrimSpace(e.Why) == "" {
			t.Errorf("%s is exempt from having a gate and gives no reason — "+
				"the point of the entry is the sentence", e.ID)
		}
		if _, dup := exempt[e.ID]; dup {
			t.Errorf("%s is exempt twice", e.ID)
		}
		exempt[e.ID] = e.Why
	}

	declared := map[string]bool{}
	for _, r := range records {
		declared[r.ID] = true
		enforced := strings.TrimSpace(r.EnforcedBy())
		_, isExempt := exempt[r.ID]
		switch {
		case strings.EqualFold(enforced, "nothing") && !isExempt:
			t.Errorf("%s says it is enforced by nothing and is not in "+
				"adr.Unenforced.\n\tThat is a legitimate answer and it has to "+
				"be declared: add an entry saying what a gate would have to "+
				"look at, and why that is out of reach. An undeclared "+
				"'nothing' is indistinguishable from a field somebody left "+
				"vague.", r.File)
		case !strings.EqualFold(enforced, "nothing") && isExempt:
			t.Errorf("%s is listed in adr.Unenforced and now says it is "+
				"enforced by %q. Delete the entry — an exemption that stopped "+
				"being true sits exactly where the next reader takes it for a "+
				"live one.", r.File, enforced)
		}
	}
	for _, e := range adr.Unenforced {
		if !declared[e.ID] {
			t.Errorf("adr.Unenforced names %s and no record declares it", e.ID)
		}
	}
}

// A NAMED TEST EXISTS, IN THE PACKAGE THE RECORD NAMES.
//
// `Enforced-by: internal/store.TestOnlyTheApplierWritesTheReplicatedEstate` is
// a claim about the tree. A renamed or deleted test leaves the record
// asserting a gate that is not there, worse than `nothing` because it reads as
// covered.
//
// THE PACKAGE A NAME IS QUALIFIED BY IS PART OF THE CLAIM. A test that moves
// still exists by its name, so a check of the name alone would send a reader to
// the wrong package, and keep passing while any package declared that name.
func TestEveryNamedGateExists(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	tests := testFunctions(t, root)
	if len(tests) == 0 {
		t.Fatal("found no test functions in the tree, so this guard is " +
			"certifying nothing")
	}

	for _, r := range load(t) {
		for _, name := range namedTests(r.EnforcedBy()) {
			if len(tests[name]) > 0 {
				continue
			}
			t.Errorf("%s says it is enforced by %s and no such test exists in "+
				"the tree.\n\tEither it was renamed — update the record — or "+
				"it was deleted, in which case this decision is now enforced "+
				"by nothing and the record has to say so.", r.File, name)
		}
		// A NAME NO PACKAGE DECLARES is the error above, said once.
		for _, q := range qualifiedTests(r.EnforcedBy()) {
			if dirs := tests[q.name]; len(dirs) > 0 && !slices.Contains(dirs, q.dir) {
				t.Errorf("%s says it is enforced by %s.%s and %s declares no such "+
					"test: it is declared in %s.\n\tQualify it by the package "+
					"that declares it.", r.File, q.dir, q.name, q.dir,
					strings.Join(dirs, ", "))
			}
		}
	}
}

// qualifiedTest is a test an Enforced-by value names by the directory of its
// package, as `internal/engine.TestAFrozenReadFitsItsBound` names it.
type qualifiedTest struct{ dir, name string }

// qualifiedTests pulls the `<package directory>.Test...` names out of an
// Enforced-by value, each once. A bare name is not one of them: it says
// nothing of its package, so [namedTests] alone holds it.
func qualifiedTests(enforcedBy string) []qualifiedTest {
	var out []qualifiedTest
	for _, m := range qualifiedTestRE.FindAllStringSubmatch(enforcedBy, -1) {
		if q := (qualifiedTest{dir: m[1], name: m[2]}); !slices.Contains(out, q) {
			out = append(out, q)
		}
	}
	return out
}

var qualifiedTestRE = regexp.MustCompile(`([A-Za-z0-9_./-]+)\.(Test[A-Z][A-Za-z0-9_]*)`)

// A QUALIFIED GATE IS READ AS ITS PACKAGE'S DIRECTORY AND ITS TEST, each
// once, and a bare name as none: the package check holds every name a record
// qualifies, nested packages and commands included, and assumes nothing of a
// name it does not.
func TestAQualifiedGateIsReadAsItsPackageAndItsTest(t *testing.T) {
	t.Parallel()
	got := qualifiedTests("`internal/engine.TestA`, `internal/sandbox/executor.TestB` and " +
		"TestC, held by `cmd/crewlet.TestD`; `internal/engine.TestA` again, and Tested.")
	want := []qualifiedTest{{dir: "internal/engine", name: "TestA"},
		{dir: "internal/sandbox/executor", name: "TestB"}, {dir: "cmd/crewlet", name: "TestD"}}
	if !slices.Equal(got, want) {
		t.Errorf("qualifiedTests read %v, want %v", got, want)
	}
}

// namedTests pulls the Test... identifiers out of an Enforced-by value.
//
// The field is prose, deliberately: some decisions are held by a compile error
// or by a type's signature, and forcing those into a test name would make the
// field a lie. So anything shaped like a test name is checked and everything
// else is left alone.
func namedTests(enforcedBy string) []string {
	var out []string
	for _, m := range testNameRE.FindAllString(enforcedBy, -1) {
		if !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	return out
}

var testNameRE = regexp.MustCompile(`\bTest[A-Z][A-Za-z0-9_]*`)

// testFunctions is every Test function declared anywhere in the module, with
// the directories, relative to root and slash-separated, of the packages that
// declare it: the spelling a record qualifies a name by.
//
// LINE BY LINE, rather than one multi-line expression over each file: a
// `(?m)^` pattern has no literal prefix to jump to, so the regexp package
// tried it at every byte of every test file — eight seconds under the race
// detector. A declaration is the start of a line, which is all `(?m)^` ever
// meant here, so the prefix is checked as a prefix and the expression runs
// only on the lines that start with it; neither half can span a newline, so
// the names found are the same.
func testFunctions(t *testing.T, root string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	err := sourcetree.Walk(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "dist", "static":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		dir, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		dir = filepath.ToSlash(dir)
		for _, line := range strings.Split(read(t, path), "\n") {
			if name, ok := testDeclaredOn(line); ok && !slices.Contains(out[name], dir) {
				out[name] = append(out[name], dir)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// testDeclaredOn is the Test function one line declares, if it declares one.
func testDeclaredOn(line string) (string, bool) {
	if !strings.HasPrefix(line, testDeclPrefix) {
		return "", false
	}
	m := testDeclRE.FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// testDeclPrefix is what every line testDeclRE matches starts with.
const testDeclPrefix = "func Test"

var testDeclRE = regexp.MustCompile(`^func (Test[A-Za-z0-9_]*)\(`)

// THE DECLARATION MATCHER, on lines whose verdict is known. The walk only
// ever hands it real test files, so a matcher that stopped recognising a
// declaration would show only as the floor above — and one that recognised a
// method, a call or an indented closure as a declaration not at all.
func TestATestDeclarationIsReadFromTheStartOfItsLine(t *testing.T) {
	t.Parallel()
	for line, want := range map[string]string{
		"func TestX(t *testing.T) {":            "TestX",
		"func TestEveryNamedGateExists(t *T) {": "TestEveryNamedGateExists",
		"func Test_underscore(t *testing.T) {":  "Test_underscore",
		"func Test(t *testing.T) {":             "Test",
		"func (s suite) TestMethod(t *T) {":     "",
		" func TestIndented(t *testing.T) {":    "",
		"\tfunc TestTabbed(t *testing.T) {":     "",
		"// func TestInAComment(t *T) {":        "",
		"x := func TestNotADeclaration(":        "",
		"func Tester(t *testing.T) {":           "Tester",
		"func TestGeneric[T any](t *T) {":       "",
	} {
		got, ok := testDeclaredOn(line)
		if got != want || ok != (want != "") {
			t.Errorf("testDeclaredOn(%q) = %q, %v; want %q", line, got, ok, want)
		}
	}
}

// citation is one reference to a record, somewhere in the tree.
type citation struct {
	ID   string
	File string
	Line int
}

// citations walks the tree for ADR-NNNN references.
//
// Go sources and markdown only; static/ and dist are generated, so skipped.
// The records are walked too: a record cites its neighbours, the citations
// likeliest to survive a renumbering unnoticed. The template is skipped by
// NAME: its file carries the zero id throughout, which [adr.Load] never declares.
//
// A FILE IS SPLIT INTO LINES ONLY IF IT CAN CITE: [adr.Reference] opens with
// `\b`, so regexp has no prefix to jump to. Its derived prefilter (see
// [regexlit.Required]) skips a file holding no substring a citation needs.
func citations(t *testing.T, root string) []citation {
	t.Helper()
	var out []citation
	files := 0
	err := sourcetree.Walk(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "dist", "static":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, ".md") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if rel == filepath.Join(adr.Dir, "0000-template.md") {
			return nil
		}
		files++
		body := read(t, path)
		if !referenceNeeds.AdmitsString(body) {
			return nil
		}
		for i, line := range strings.Split(body, "\n") {
			if !referenceNeeds.AdmitsString(line) {
				continue
			}
			for _, m := range adr.Reference.FindAllString(line, -1) {
				out = append(out, citation{ID: m, File: rel, Line: i + 1})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	// The backward check is an absence — no citation of a record nobody
	// declared — and a walk that read nothing finds none.
	if files == 0 {
		t.Fatalf("read no Go or markdown files under %s — the citation check "+
			"was certifying nothing", root)
	}
	return out
}

// referenceNeeds is what a citation cannot be written without.
var referenceNeeds = regexlit.Required(adr.Reference)

// danglingCitations is every citation under root of a record declared does
// not hold, and how many citations the walk read in all.
func danglingCitations(t *testing.T, root string, declared map[string]adr.Record) ([]citation, int) {
	t.Helper()
	all := citations(t, root)
	var dangling []citation
	for _, cite := range all {
		if _, ok := declared[cite.ID]; !ok {
			dangling = append(dangling, cite)
		}
	}
	return dangling, len(all)
}

// authorityText is the doc a record's id has to appear in.
//
// For a PACKAGE that is its package doc comment — the block immediately above
// `package x` — rather than the whole directory, because an id anywhere in the
// package's source would be satisfied by a passing mention in a function
// comment and the anchor's whole job is to be on the first screen of `go doc`.
func authorityText(t *testing.T, dir string, isPackage bool) string {
	t.Helper()
	if !isPackage {
		return read(t, dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var docs []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		if doc := packageDoc(read(t, filepath.Join(dir, e.Name()))); doc != "" {
			docs = append(docs, doc)
		}
	}
	return strings.Join(docs, "\n")
}

// packageDoc is the comment block immediately above the package clause.
func packageDoc(src string) string {
	lines := strings.Split(src, "\n")
	end := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "package ") {
			end = i
			break
		}
	}
	if end <= 0 {
		return ""
	}
	start := end
	for start > 0 && strings.HasPrefix(strings.TrimSpace(lines[start-1]), "//") {
		start--
	}
	if start == end {
		return ""
	}
	return strings.Join(lines[start:end], "\n")
}

// A REFERENCE THAT POINTS AT NOTHING READS EXACTLY LIKE A LIVE ONE.
//
// The anchor's failure, one level out. A bracketed import path in a doc comment
// claims something about the tree; when the package goes the claim stays,
// silently. The withdrawn-vocabulary gate cannot see it: it is keyed on names
// somebody thought to withdraw.
//
// # What it checks, and what it leaves alone
//
// Only a bracketed reference that starts with internal/ or cmd/, the form
// whose target this walk can resolve. [Type], [pkg.Func] and [Type.Method] are
// left to the compiler and go vet.
func TestEveryDocLinkNamesAPackageThatExists(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)

	// The matcher, on input whose verdict is known.
	for _, positive := range []string{"[internal/coord]", "see [cmd/crewlet] for"} {
		if len(packageLinks(positive)) != 1 {
			t.Errorf("control: %q names a package and the matcher missed it", positive)
		}
	}
	for _, negative := range []string{"[Writer.Tx]", "[Scope]", "[maintenance.Fleet]", "a [link](x.md)"} {
		if got := packageLinks(negative); len(got) != 0 {
			t.Errorf("control: %q names no package and the matcher found %v", negative, got)
		}
	}

	files := 0
	err := sourcetree.Walk(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "dist", "static":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		files++
		body := read(t, path)
		if !packageLinkNeeds.AdmitsString(body) {
			// Not one `[internal/` or `[cmd/` in the file, so no link
			// the matcher could find (see regexlit.Required).
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		for i, line := range strings.Split(body, "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			for _, pkg := range packageLinks(line) {
				if _, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(pkg))); statErr == nil {
					continue
				}
				t.Errorf("%s:%d links [%s] and there is no such package.\n"+
					"\tgodoc renders that as a live link and a deleted "+
					"package leaves no compile error behind in a comment, so "+
					"the claim outlives the code. Name the package that took "+
					"the responsibility over, or drop the brackets and say "+
					"what the reasoning was.", rel, i+1, pkg)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if files == 0 {
		t.Fatal("parsed no Go files — this guard was certifying nothing")
	}
}

// packageLinks is every [internal/…] or [cmd/…] reference on one line.
func packageLinks(line string) []string {
	var out []string
	for _, m := range packageLinkRE.FindAllStringSubmatch(line, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}

var packageLinkRE = regexp.MustCompile(`\[((?:internal|cmd)/[a-z0-9]+(?:/[a-z0-9]+)*)\]`)

// packageLinkNeeds is what a package link cannot be written without.
var packageLinkNeeds = regexlit.Required(packageLinkRE)

var linkRE = regexp.MustCompile(`\]\(([^)]+)\)`)

func markdownLinks(page string) []string {
	var out []string
	for _, m := range linkRE.FindAllStringSubmatch(page, -1) {
		out = append(out, m[1])
	}
	return out
}

func load(t *testing.T) []adr.Record {
	t.Helper()
	records, err := adr.Load(sourcetree.Root(t))
	if err != nil {
		t.Fatalf("load the records: %v", err)
	}
	return records
}

func read(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path) //nolint:gosec // paths this test built by walking the module
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}
