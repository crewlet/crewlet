package artifactdataapi_test

import (
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// ONE REFUSAL, ONE CLASS, ON BOTH SURFACES THAT ANSWER A DATASTORE WRITE: the
// tools' class (builtin.DataVerdictClass for what the applier decided,
// builtin.WriterClass for what the writer refused before anything was
// appended) and this API's status, each answered here through the real write
// route and again through the operator surface's one class-to-status mapping
// ([operator.Fail]).
//
// THEY DISAGREED TWICE before this case existed: the company's quota was 413
// here and `forbidden` to a tool, which told a model it may not write what
// nobody may, and an artifact in the trash was `410 data_gone` here — a purge's
// answer, for data a restore brings back whole — while every other artifacts
// route answers it `409 artifact_trashed`.
//
// WHAT HTTP SAYS MORE PRECISELY is declared, never assumed: `refines` is a
// status HTTP has for one case of the class's meaning, with the reason.
//
// Mutation: class the quota verdict `forbidden` in DataVerdictClass, or answer
// a trashed artifact 410 here, and this goes red naming it.
func TestEveryDatastoreRefusalIsOneClassOnBothSurfaces(t *testing.T) {
	t.Parallel()
	// A PURGE IS FOR GOOD, and 410 is HTTP's answer for what will never come
	// back — one case of `not_found`'s meaning, said more precisely.
	const purged = http.StatusGone

	// WHAT THE APPLIER DECIDED, every verdict this build writes and one a
	// newer peer's applier wrote.
	verdicts := append(slices.Clone(artifacts.DataVerdicts), artifacts.DataVerdict("a-newer-verdict"))
	for _, v := range verdicts {
		if v == artifacts.DataApplied {
			continue
		}
		mux, data := rig(t)
		data.answer = artifactdata.WriteResult{Verdict: v, Reason: "refused",
			Result: statelog.Result{Outcome: statelog.OutcomeApplied}}
		w := do(t, mux, person("bob"), "POST", "/artifacts/"+sheet.String()+"/data/writes", tick, true)
		class := builtin.DataVerdictClass(v)
		refines := 0
		if v == artifacts.DataGone {
			refines = purged
		}
		agree(t, "verdict "+string(v), w, class, artifactdata.ErrConflict, refines)
	}

	// WHAT THE WRITER REFUSED BEFORE ANYTHING WAS APPENDED, every error the
	// datastore declares.
	refusals := map[string]struct {
		err     error
		refines int
		why     string
	}{
		"ErrConflict":          {err: artifactdata.ErrConflict},
		"ErrForbidden":         {err: artifactdata.ErrForbidden},
		"ErrGone":              {err: artifactdata.ErrGone, refines: purged},
		"ErrInvalid":           {err: artifactdata.ErrInvalid},
		"ErrMigrationTooLarge": {err: artifactdata.ErrMigrationTooLarge},
		"ErrNotFound":          {err: artifactdata.ErrNotFound},
		"ErrNotUndoable":       {err: artifactdata.ErrNotUndoable},
		// A BUDGET THAT REFILLS is a condition waiting clears, and 429 is
		// HTTP's answer for one with a Retry-After saying when — one case of
		// `unavailable`'s meaning, said more precisely.
		"RateLimited": {err: &artifactdata.RateLimited{Scope: "principal", Cost: 1,
			RetryAfter: time.Second}, refines: http.StatusTooManyRequests},

		"ErrBadCursor": {why: "a read's refusal, answered by the read routes, never a write's"},
		"ErrFutureVersion": {why: "a record a newer build wrote, which the framework defers " +
			"rather than refuses"},
	}
	declared := declaredErrors(t, filepath.Join(sourcetree.Root(t), "internal", "artifactdata"))
	if len(declared) < 5 {
		t.Fatalf("artifactdata declares %d errors; this is not reading the package", len(declared))
	}
	for _, name := range declared {
		if _, ok := refusals[name]; !ok {
			t.Errorf("artifactdata.%s is an error the datastore declares and this case does "+
				"not answer on both surfaces: give it a row", name)
		}
	}
	for name, c := range refusals {
		if !slices.Contains(declared, name) {
			t.Errorf("artifactdata.%s has a row and the datastore declares no such error", name)
			continue
		}
		if c.why != "" {
			continue
		}
		class, ok := builtin.WriterClass(c.err)
		if !ok {
			t.Errorf("%s: the tools class nothing — a 500 on every surface that reads the table", name)
			continue
		}
		mux, data := rig(t)
		data.err = c.err
		w := do(t, mux, person("bob"), "POST", "/artifacts/"+sheet.String()+"/data/writes", tick, true)
		agree(t, name, w, class, c.err, c.refines)
	}

	// AN ARTIFACT IN THE TRASH, refused before the datastore is asked, as the
	// tools refuse it: the artifacts domain's own sentinel and class.
	mux, _ := rig(t)
	w := do(t, mux, person("bob"), "POST", "/artifacts/"+trashed.String()+"/data/writes", tick, true)
	class, ok := builtin.WriterClass(artifactsdomain.ErrTrashed)
	if !ok {
		t.Fatal("the tools class an artifact in the trash nothing")
	}
	agree(t, "an artifact in the trash", w, class, artifactsdomain.ErrTrashed, 0)
}

// agree holds the API's answer w to the status the tools' class has on the
// operator surface, or to the declared refinement of it.
func agree(t *testing.T, what string, w *httptest.ResponseRecorder, class tools.Refusal,
	cause error, refines int) {

	t.Helper()
	rec := httptest.NewRecorder()
	operator.Fail(rec, tools.Classify(class, cause), caveat.Refusal{}, "op", nil)
	switch {
	case refines != 0 && w.Code != refines:
		t.Errorf("%s: the API answered %d %s, and its declared refinement of the tools' "+
			"%q (%d) is %d", what, w.Code, errorOf(t, w), class, rec.Code, refines)
	case refines == 0 && w.Code != rec.Code:
		t.Errorf("%s: the API answered %d %s and the tools class it %q, which the operator "+
			"surface answers %d: one refusal is one class", what, w.Code, errorOf(t, w), class,
			rec.Code)
	}
}

// declaredErrors is every exported Err* variable and every exported type with
// an `Error() string` method in one package directory, tests excluded.
func declaredErrors(t *testing.T, dir string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := gotoken.NewFileSet()
	var out []string
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				if d.Tok != gotoken.VAR {
					continue
				}
				for _, spec := range d.Specs {
					for _, n := range spec.(*ast.ValueSpec).Names {
						if n.IsExported() && strings.HasPrefix(n.Name, "Err") {
							out = append(out, n.Name)
						}
					}
				}
			case *ast.FuncDecl:
				if d.Recv == nil || d.Name.Name != "Error" || len(d.Type.Params.List) != 0 {
					continue
				}
				recv := d.Recv.List[0].Type
				if star, ok := recv.(*ast.StarExpr); ok {
					recv = star.X
				}
				if id, ok := recv.(*ast.Ident); ok && id.IsExported() {
					out = append(out, id.Name)
				}
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
