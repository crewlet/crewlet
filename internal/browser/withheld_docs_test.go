package browser

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// withheldHeading opens code-sandbox.md's table of every reason and cause a
// browser is withheld for.
const withheldHeading = "### When a browser is withheld"

// withheldRow is one row of that table: its reason, then its cause, or none
// for a reason that takes no cause.
var withheldRow = regexp.MustCompile("^\\| `([a-z_]+)` \\| (?:`([a-z_]+)`|(none)) \\|")

// EVERY REASON A BROWSER IS WITHHELD FOR IS IN THE DOCS: the table under "When
// a browser is withheld" in docs/concepts/code-sandbox.md has one row for each
// cause of each reason, and one for each reason that takes none, in the
// list's order, and no other, so an operator who reads a withheld browser's
// record finds what it means and what to change.
//
// Mutations, each red: give a reason a cause and the table no row for it;
// drop a row; swap two rows; add a row the list does not hold.
func TestEveryReasonABrowserIsWithheldForIsInTheDocs(t *testing.T) {
	t.Parallel()
	page, err := os.ReadFile(filepath.Join(sourcetree.Root(t), "docs", "concepts", "code-sandbox.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(page), "\n"+withheldHeading+"\n")
	if !ok {
		t.Fatalf("docs/concepts/code-sandbox.md has no %q section", withheldHeading)
	}
	section, _, _ = strings.Cut(section, "\n#")
	var rows []string
	for line := range strings.Lines(section) {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		m := withheldRow.FindStringSubmatch(line)
		if m == nil {
			t.Errorf("a row of the table names no reason and cause: %q", line)
			continue
		}
		rows = append(rows, m[1]+" "+m[2]+m[3])
	}
	var want []string
	for _, r := range EnsureReasons() {
		causes := r.Causes()
		if len(causes) == 0 {
			want = append(want, string(r)+" none")
		}
		for _, c := range causes {
			want = append(want, string(r)+" "+string(c))
		}
	}
	if slices.Equal(rows, want) {
		return
	}
	for _, w := range want {
		if !slices.Contains(rows, w) {
			t.Errorf("the table has no row for %s", w)
		}
	}
	for _, r := range rows {
		if !slices.Contains(want, r) {
			t.Errorf("the table has a row for %s, which no reason takes", r)
		}
	}
	if !t.Failed() {
		t.Errorf("the table's rows are\n%s\nwant the list's order:\n%s", strings.Join(rows, "\n"),
			strings.Join(want, "\n"))
	}
}
