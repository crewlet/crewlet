package boxhelper

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// THE ENGINE READS THE BROWSER'S TOOLS WHOLE FROM ITS MODULE: Tools is
// serve.mjs's ALLOWED as the module embedded and the source tree both hold
// it, a name a line in the list's order; and a list laid out any other way
// (a comment, a name with no comma, single quotes, a spread, a name twice, no
// name at all, no opening or closing line) is refused rather than read in
// part, since a launch would then count fewer tools than a box serves.
// TestTheModelSeesOnlyTheAllowedTools holds the same list to what a box's
// server lists its model.
//
// Mutations, each red: skip a line the reader does not match; read the list
// to its first name; let a name listed twice through.
func TestTheEngineReadsTheBrowsersToolsWholeFromItsModule(t *testing.T) {
	t.Parallel()
	source, err := os.ReadFile(filepath.Join("js", "serve.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	_, list, _ := strings.Cut(string(source), allowedOpen+"\n")
	list, _, _ = strings.Cut(list, "\n"+allowedClose+"\n")
	var want []string
	for line := range strings.SplitSeq(list, "\n") {
		want = append(want, strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(line), `"`), `",`))
	}
	if got := Tools(); len(got) == 0 || !slices.Equal(got, want) {
		t.Fatalf("Tools is %q, want serve.mjs's ALLOWED %q", got, want)
	}
	module := func(lines ...string) []byte {
		return []byte("// a module\n" + allowedOpen + "\n" + strings.Join(lines, "\n") + "\n" + allowedClose + "\n")
	}
	if got, err := allowedTools(module(`  "browser_click",`, `  "browser_type",`)); err != nil ||
		!slices.Equal(got, []string{"browser_click", "browser_type"}) {
		t.Errorf("a list a name a line reads as %q (%v), want both names in order", got, err)
	}
	for name, layout := range map[string][]byte{
		"a comment":        module(`  "browser_click",`, `  // a later pin's`, `  "browser_type",`),
		"a trailing note":  module(`  "browser_click", // the first`, `  "browser_type",`),
		"no comma":         module(`  "browser_click",`, `  "browser_type"`),
		"single quotes":    module(`  'browser_click',`, `  "browser_type",`),
		"a spread":         module(`  "browser_click",`, `  ...MORE,`),
		"a name twice":     module(`  "browser_click",`, `  "browser_type",`, `  "browser_click",`),
		"two on a line":    module(`  "browser_click", "browser_type",`),
		"no name":          []byte("// a module\n" + allowedOpen + "\n" + allowedClose + "\n"),
		"no opening line":  []byte(`export const ALLOWED = new Set(["browser_click"])` + "\n"),
		"no closing line":  []byte("// a module\n" + allowedOpen + "\n" + `  "browser_click",` + "\n"),
		"a closing on one": module(`  "browser_click",`, `  "browser_type",])`),
	} {
		if got, err := allowedTools(layout); err == nil {
			t.Errorf("a list with %s reads as %q, want it refused", name, got)
		}
	}
}
