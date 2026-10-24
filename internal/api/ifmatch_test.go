package api_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// EVERY If-Match THIS SURFACE READS IS ANSWERED AS HTTP ANSWERS IT: a version
// the request named in the header that turns out false — when it arrives, or
// before the write it guards can land — is `412` (RFC 9110 §13.1.1), and the
// same version stated in a BODY field is the domain's own `409`.
//
// The rule is what every reader below follows, each held by its own surface's
// cases (named in the table). It was not what every reader followed: `/config`
// answered a stale If-Match `409`, the tracker and page routes folded the
// header into a tool's argument and answered its staleness as the argument's
// `409 stale_version`, and a disconnect answered its header as a connect
// answers its body, while a project file and an artifact publish answered
// `412 version_moved` — so one header meant two things by which route read it.
// What this walk holds is that nobody adds a reader without deciding which
// answer it gives: a call reading the header that is not in the table, or a
// row naming a reader the tree no longer has, fails here.
//
// Mutation: read If-Match in any new function under internal/api and this
// goes red until it has a row.
func TestEveryIfMatchReaderIsAReviewedPrecondition(t *testing.T) {
	t.Parallel()
	// reviewed is every function that reads the header, by file and name,
	// with what a false condition is answered and the case that holds it.
	reviewed := map[string]string{
		"configapi/configapi.go:checkPrecondition": "412 revision_advanced; " +
			"TestThePreconditionsFollowTheSpec",
		"configapi/configapi.go:RevisionStatus": "412 where a revision was named, 409 " +
			"for none or `*`; TestALostRaceIsAFailedPreconditionOnlyWhereARevisionWasNamed",
		"configapi/entities.go:entityPrecondition": "names no version beside a " +
			"create (400 conflicting_preconditions); checkPrecondition otherwise",
		"setupapi/setupapi.go:disconnect": "the header is the base, answered by " +
			"refuseConditioned; TestADisconnectUnderAStaleIfMatchIsAFailedPrecondition",
		"workapi/workapi.go:ifMatch": "folded into the tool's version and answered " +
			"412 version_moved by callIfMatched; " +
			"TestAVersionTheHeaderStatedIsAFailedPrecondition",
		"workapi/files.go:fileVersion": "412 version_moved; " +
			"TestTheWritersRefusalsReadAsTheFileRoutesDocument",
		"artifactsapi/publish.go:ifMatch": "412 version_moved, carrying the live " +
			"entry; TestAPublishBasedOnAMovedHeadCarriesTheLiveEntry and " +
			"TestEveryArtifactRefusalIsOneClassOnBothSurfaces",
	}
	var found []string
	walked := 0
	err := sourcetree.Walk(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		walked++
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && readsIfMatch(call) {
					found = append(found, filepath.ToSlash(path)+":"+fn.Name.Name)
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if walked < 100 {
		t.Fatalf("walked %d source files, so the walk certifies nothing", walked)
	}
	for _, reader := range found {
		if _, ok := reviewed[reader]; !ok {
			t.Errorf("%s reads If-Match and has no row: decide what a false "+
				"condition is answered (412 for the header, the domain's 409 for a "+
				"body field) and add it", reader)
		}
	}
	for reader := range reviewed {
		if !slices.Contains(found, reader) {
			t.Errorf("%s has a row and no longer reads If-Match: remove it", reader)
		}
	}
}

// readsIfMatch reports a `….Header.Get("If-Match")` call.
func readsIfMatch(call *ast.CallExpr) bool {
	get, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || get.Sel.Name != "Get" || len(call.Args) != 1 {
		return false
	}
	if header, ok := get.X.(*ast.SelectorExpr); !ok || header.Sel.Name != "Header" {
		return false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	name, _ := strconv.Unquote(lit.Value)
	return strings.EqualFold(name, "If-Match")
}
