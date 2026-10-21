package artifactsapi_test

import (
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/tools"
)

// ONE REFUSAL, ONE CLASS, ON BOTH SURFACES THAT ANSWER AN ARTIFACT WRITE: the
// tools' class (builtin.WriterClass, the one table every tool and the person's
// write surface read) and this API's status. Each artifactsdomain refusal is
// published through the real publish route and answered here, and answered
// again as the tools' class through the operator surface's one class-to-status
// mapping ([operator.Fail]); the two statuses must be one.
//
// THEY DISAGREED THREE TIMES before this case existed: a label another version
// holds was 409 here and `invalid` to a tool, an artifact in the trash 409 here
// and `forbidden` to a tool, and the company's quota 413 here and `forbidden`
// to a tool — so a model was told to send another label for a version that
// holds it, to ask for a permission the trash does not care about, and that
// it may not publish when nobody may.
//
// THE PUBLISH STATES ITS BASE AS AN If-Match HEADER, so the status it is held
// to is the one mapping's for a call such a header conditioned
// ([operator.FailIfMatched]): a head that moved past the base is HTTP's failed
// precondition, `412 version_moved`, where a tool, which takes the base as an
// argument, is told the class's `409 stale_version`. It was a refinement this
// case declared for the one row, which said 412 for a publish and left every
// other surface's If-Match answering 409.
//
// Mutation: class ErrLabelTaken `invalid` in builtin's artifactClass, or answer
// QuotaExceeded 413 in writeFailed, and this goes red naming it.
func TestEveryArtifactRefusalIsOneClassOnBothSurfaces(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		err error
		why string // set: not a write's refusal, and why
	}{
		"ErrAboveOwnRole":      {err: artifactsdomain.ErrAboveOwnRole},
		"ErrAgentCompanyShare": {err: artifactsdomain.ErrAgentCompanyShare},
		"ErrConflict":          {err: artifactsdomain.ErrConflict},
		"ErrExists":            {err: artifactsdomain.ErrExists},
		"ErrInvalid":           {err: artifactsdomain.ErrInvalid},
		"ErrLabelTaken":        {err: artifactsdomain.ErrLabelTaken},
		"ErrLabelsFull":        {err: artifactsdomain.ErrLabelsFull},
		"ErrNotFound":          {err: artifactsdomain.ErrNotFound},
		"ErrPurged":            {err: artifactsdomain.ErrPurged},
		"ErrPurging":           {err: artifactsdomain.ErrPurging},
		"ErrSourceTaken":       {err: artifactsdomain.ErrSourceTaken},
		"ErrTrashed":           {err: artifactsdomain.ErrTrashed},
		"ErrUploadStale":       {err: artifactsdomain.ErrUploadStale},
		"CarriedObjectGone":    {err: &artifactsdomain.CarriedObjectGone{Paths: []string{"a.css"}}},
		"QuotaExceeded": {err: &artifactsdomain.QuotaExceeded{Limit: 1, Usage: 2,
			Key: "artifacts.limits.company_bytes"}},
		"VersionMoved": {err: &artifactsdomain.VersionMoved{}},

		"ErrBadCursor": {why: "a read's refusal, answered by the listing routes, never a write's"},
		"ErrNotInTrash": {why: "the trash reaper's own purge, made as the engine, never a " +
			"caller's request"},
		"ErrFutureVersion": {why: "a record a newer build wrote, which the framework defers " +
			"rather than refuses"},
	}
	declared := declaredErrors(t, filepath.Join(sourcetree.Root(t), "internal", "artifactsdomain"))
	if len(declared) < 10 {
		t.Fatalf("artifactsdomain declares %d errors; this is not reading the package", len(declared))
	}
	for _, name := range declared {
		if _, ok := cases[name]; !ok {
			t.Errorf("artifactsdomain.%s is an error the domain declares and this case does "+
				"not answer on both surfaces: give it a row", name)
		}
	}
	for name, c := range cases {
		if !slices.Contains(declared, name) {
			t.Errorf("artifactsdomain.%s has a row and the domain declares no such error", name)
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
		rec := httptest.NewRecorder()
		operator.FailIfMatched(rec, tools.Classify(class, c.err), caveat.Refusal{}, "op", nil)
		want, wantCode := rec.Code, errorOf(t, rec)

		g := newRig(t, true)
		g.pipeline.err = c.err
		w := g.do(person("sam"), "POST", at(page, "/versions"), publishV3,
			append(keyed(), "If-Match", ifMatch2)...)
		if w.Code != want {
			t.Errorf("%s: the API answered %d %s and the tools class it %q, which the "+
				"operator surface answers %d %s under an If-Match: one refusal is one "+
				"class", name, w.Code, errorOf(t, w), class, want, wantCode)
		}
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
