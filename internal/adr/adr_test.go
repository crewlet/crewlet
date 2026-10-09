package adr_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/adr"
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

	seen := map[int]string{}
	for _, r := range records {
		if prior, dup := seen[r.Number]; dup {
			t.Errorf("%s and %s are both numbered %04d — a citation of "+
				"ADR-%04d reaches two different decisions", prior, r.File, r.Number, r.Number)
		}
		seen[r.Number] = r.File

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

// THE ANCHOR, IN BOTH DIRECTIONS.
//
// This is the mechanism the register had no version of before, and the reason
// its predecessor drifted into six false statements without anybody noticing:
// nothing pointed at it, so nobody read it, so nobody corrected it.
//
// Forward: a record names an authority, and that authority's own doc carries
// the record's id — so `go doc ./internal/statelog` shows ADR-0002 to the
// person about to change the state log.
//
// Backward: a doc that cites a record which does not exist fails. This is not
// hypothetical tidiness. Three package docs in this tree cited a package that
// migration 0025 had deleted, for months afterwards, and the tree's own
// withdrawn-vocabulary gate did not catch it — because a dangling reference
// reads exactly like a live one.
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
		"adr/0000-template.md": "# " + id("0000") + " — Title\n",
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

// AND THE INDEX IS THE RECORDS.
//
// adr/README.md carries the table a reader scans, and a table maintained by
// hand goes stale on the first record somebody adds — silently, and in the
// direction that matters least to the person who added it.
func TestTheIndexNamesEveryRecord(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	index := read(t, filepath.Join(root, adr.Dir, "README.md"))

	for _, r := range load(t) {
		if !strings.Contains(index, r.File) {
			t.Errorf("%s is not linked from adr/README.md, so it is reachable "+
				"only by listing the directory", r.File)
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

// A NAMED TEST EXISTS.
//
// `Enforced-by: internal/store.TestOnlyTheApplierWritesTheReplicatedEstate` is
// a claim about the tree, and a claim nobody checks is how the register's
// predecessor came to assert that full-text search was unavailable while
// internal/textindex shipped a BM25 index. A renamed or deleted test leaves the
// record asserting a gate that is not there — which is worse than `nothing`,
// because it reads as covered.
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
			if tests[name] {
				continue
			}
			t.Errorf("%s says it is enforced by %s and no such test exists in "+
				"the tree.\n\tEither it was renamed — update the record — or "+
				"it was deleted, in which case this decision is now enforced "+
				"by nothing and the record has to say so.", r.File, name)
		}
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

// testFunctions is every Test function declared anywhere in the module.
//
// LINE BY LINE, rather than one multi-line expression over each file: a
// `(?m)^` pattern has no literal prefix to jump to, so the regexp package
// tried it at every byte of every test file — eight seconds under the race
// detector. A declaration is the start of a line, which is all `(?m)^` ever
// meant here, so the prefix is checked as a prefix and the expression runs
// only on the lines that start with it; neither half can span a newline, so
// the names found are the same.
func testFunctions(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
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
		for _, line := range strings.Split(read(t, path), "\n") {
			if name, ok := testDeclaredOn(line); ok {
				out[name] = true
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
// Go sources and markdown only: those are the two places a decision is cited
// in a sentence somebody reads. A reference in a generated file or a committed
// bundle would be noise, and static/ is skipped for that reason.
//
// THE RECORDS THEMSELVES ARE WALKED TOO, which they were not at first. A record
// cites its neighbours — "which is ADR-0003", "see ADR-0002" — and those are
// the citations most likely to survive a renumbering, because they are the
// ones written by somebody who was holding both records in mind at the time
// and is not around for the edit that moves one. A record's own id is declared
// by its own file, so walking adr/ costs no false positives.
//
// The template is the exception and is skipped by NAME here rather than by
// number, because what has to be skipped is the FILE: it carries the zero id
// throughout as the shape of a record, and zero is the one number [adr.Load]
// never declares.
//
// A FILE IS SPLIT INTO LINES ONLY IF IT CAN CITE: [adr.Reference] opens with
// `\b`, so the regexp package has no prefix to jump to and tried it at every
// byte of the tree, which was most of this gate's twenty-five seconds. Its
// derived prefilter (see [sourcetree.Required]) skips a file that holds no
// substring a citation needs, and that file holds no citation.
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
var referenceNeeds = sourcetree.Required(adr.Reference)

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
// This is the same failure the anchor above is for, one level out. A doc
// comment that puts an import path in square brackets is making a claim about
// the tree; godoc renders it as a link, and when the package goes the claim
// stays — silently, because a deleted package produces no compile error in a
// comment.
//
// It is not hypothetical. internal/projection was deleted by node migration
// 0025 and three package docs went on citing it for months, including the
// state log's own, where the dangling reference sat inside the paragraph that
// argues the framework's central safety property. The tree's
// withdrawn-vocabulary gate did not catch it, because that list is keyed on
// names somebody thought to withdraw.
//
// # What it checks, and what it leaves alone
//
// Only a bracketed reference whose text looks like an import path in THIS
// module — it starts with internal/ or cmd/ — because that is the form whose
// target this walk can resolve. [Type], [pkg.Func] and [Type.Method] are
// godoc's other link forms and are left to the compiler and to go vet, which
// see them.
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
			// the matcher could find (see sourcetree.Required).
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
var packageLinkNeeds = sourcetree.Required(packageLinkRE)

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
