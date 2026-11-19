package wiregate

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
)

// A PLANTED OMITTED FLAG FAILS, AND THE SAME TREE FIXED PASSES. The planted
// surface sends an answer with a flag and a number left out at zero, one more
// on a struct the answer reaches, two malformed markers, and a marker on a
// struct nothing sends; its request, which it only decodes, is never judged.
func TestAPlantedOmittedFlagIsRefusedAndItsFixPasses(t *testing.T) {
	t.Parallel()
	w := plant(t, filepath.Join("testdata", "planted"), nil)
	found, refused := w.judge(newMarkers())
	if got, want := entries(found), []string{"Answer.count", "Answer.open", "Row.done"}; !slices.Equal(got, want) {
		t.Errorf("found %v, want %v", got, want)
	}
	if len(refused) != 2 || !strings.Contains(refused[0], "Answer.wrong is a number, and its marker says absent-is-false") ||
		!strings.Contains(refused[1], "Answer.bare's marker gives no reason") {
		t.Errorf("refused %q, want the number marked as a flag and the marker with no reason", refused)
	}
	if stray := w.strayMarkers(w.root); len(stray) != 1 || !strings.Contains(stray[0], "Gone carries a wiregate marker") {
		t.Errorf("stray markers %q, want the one on Stray.Gone", stray)
	}

	fixed := plant(t, filepath.Join("testdata", "planted"), strings.NewReplacer(
		`json:"open,omitempty"`, `json:"open"`,
		`json:"count,omitzero"`, `json:"count"`,
		`json:"done,omitempty"`, `json:"done"`,
		`//wiregate:absent-is-false a number marked as a flag`, `//wiregate:absent-is-zero zero is none`,
		`//wiregate:absent-is-false`+"\n", `//wiregate:absent-is-false a mark`+"\n",
		`//wiregate:absent-is-false nobody sends this struct`, ``))
	if found, refused := fixed.judge(newMarkers()); len(found)+len(refused) != 0 {
		t.Errorf("the fixed tree: found %v, refused %q, want nothing", entries(found), refused)
	}
	if stray := fixed.strayMarkers(fixed.root); len(stray) != 0 {
		t.Errorf("the fixed tree: stray markers %q", stray)
	}
}

// A DASHBOARD MEMBER DECLARED OTHERWISE THAN IT IS SENT IS FOUND: required
// where the struct leaves it out at zero, optional where it always sends it,
// and a member the struct sends as neither a flag nor a number is not judged.
func TestAMirrorDeclaredOtherwiseThanItIsSentIsFound(t *testing.T) {
	t.Parallel()
	w := plant(t, filepath.Join("testdata", "planted"), nil)
	paired, found := w.mirror(map[string][]clientsource.Member{
		"PlantedAnswer": {{Name: "open"}, {Name: "always", Optional: true}, {Name: "inner", Optional: true},
			{Name: "count", Optional: true}},
		"PlantedRow": {{Name: "done", Optional: true}},
		"Elsewhere":  {{Name: "nowhere"}},
	})
	var got []string
	for _, m := range found {
		got = append(got, m.iface+"."+m.member+map[bool]string{true: " required", false: " optional"}[m.required])
	}
	want := []string{"PlantedAnswer.open required", "PlantedAnswer.always optional"}
	if paired != 2 || !slices.Equal(got, want) {
		t.Errorf("paired %d, found %v; want 2 and %v", paired, got, want)
	}
}

// plant type-checks the package in dir, every file rewritten by fix when it
// is set, as a surface, and walks what it sends.
func plant(t *testing.T, dir string, fix *strings.Replacer) *wire {
	t.Helper()
	if fix != nil {
		into := t.TempDir()
		names, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			src, err := os.ReadFile(name) //nolint:gosec // this package's testdata
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(into, filepath.Base(name)), []byte(fix.Replace(string(src))), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		dir = into
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	names, err := filepath.Glob(filepath.Join(abs, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, name := range names {
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{},
		Uses: map[*ast.Ident]types.Object{}}
	pkg, err := (&types.Config{Importer: importer.ForCompiler(fset, "source", nil)}).Check("planted", fset, files, info)
	if err != nil {
		t.Fatalf("type-checking the planted package: %v", err)
	}
	d := newDiscovery(fset)
	d.surface = func(string) bool { return true }
	d.add(pkg, files, info, abs)
	w, err := d.wire(abs, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// entries is found as omittedInventory spells it, sorted.
func entries(found []finding) []string {
	var out []string
	for _, f := range found {
		out = append(out, f.entry)
	}
	slices.Sort(out)
	return out
}
