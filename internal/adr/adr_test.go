package adr_test

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/adr"
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
// hypothetical tidiness. Three package docs in this tree cited
// [internal/projection] for months after migration 0025 deleted the package,
// and the tree's own withdrawn-vocabulary gate did not catch it, because a
// dangling reference reads exactly like a live one.
func TestEveryRecordIsAnchoredAtItsAuthority(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
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
	for _, cite := range citations(t, root) {
		if _, ok := declared[cite.ID]; ok {
			continue
		}
		t.Errorf("%s:%d cites %s and no record declares it — either the "+
			"record was never written, or it was renumbered and this "+
			"reference now points at nothing", cite.File, cite.Line, cite.ID)
	}
}

// AND THE INDEX IS THE RECORDS.
//
// adr/README.md carries the table a reader scans, and a table maintained by
// hand goes stale on the first record somebody adds — silently, and in the
// direction that matters least to the person who added it.
func TestTheIndexNamesEveryRecord(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
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
	root := moduleRoot(t)
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
func testFunctions(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "dist", "static":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		for _, m := range testDeclRE.FindAllStringSubmatch(read(t, path), -1) {
			out[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

var testDeclRE = regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]*)\(`)

// citation is one reference to a record, somewhere in the tree.
type citation struct {
	ID   string
	File string
	Line int
}

// citations walks the tree for ADR-NNNN references outside adr/ itself.
//
// Go sources and markdown only: those are the two places a decision is cited
// in a sentence somebody reads. A reference in a generated file or a committed
// bundle would be noise, and static/ is skipped for that reason.
func citations(t *testing.T, root string) []citation {
	t.Helper()
	var out []citation
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "dist", "static", adr.Dir:
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, ".md") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		for i, line := range strings.Split(read(t, path), "\n") {
			for _, m := range adr.Reference.FindAllString(line, -1) {
				out = append(out, citation{ID: m, File: rel, Line: i + 1})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
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
	records, err := adr.Load(moduleRoot(t))
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

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's own source file")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("expected the module root at %s: %v", root, err)
	}
	return root
}
