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
		"configapi/configapi.go:checkPrecondition": "412 revision_advanced at the check, " +
			"400 conflicting_preconditions beside If-None-Match, and a named revision " +
			"carried on to the write ([configapi.RacedError.Stated]): a race it loses " +
			"is 412, one a write naming none or `*` loses is 409; " +
			"TestThePreconditionsFollowTheSpec and " +
			"TestALostRaceIsAFailedPreconditionOnlyWhereARevisionWasNamed",
		"configapi/charter.go:putCharter": "the charter's tag, never a revision: 412 " +
			"version_moved with the charter as it stands where the charter moved, 428 " +
			"bad_if_match where none was sent, and 409 for a race lost on the " +
			"revision it never named; TestAStaleCharterDraftIsRefusedWithTheCharterNow, " +
			"TestACharterWriteNeedsAPrecondition and TestACharterWriteThatKeepsLosingIsToldSo",
		"configapi/entities.go:entityPrecondition": "the entity's tag, never a " +
			"revision: 412 version_moved with the entity as it stands, 400 " +
			"conflicting_preconditions beside If-None-Match: *, 400 bad_if_match for " +
			"another If-None-Match, and 409 for a race lost on the revision it never " +
			"named; TestAnEntitysPreconditionIsTheEntityNotTheDocument and " +
			"TestARouteThatJudgesNoRevisionLosesItsRaceAsAConflict",
		"setupapi/access.go:accessSave": "the header is the base, a stated revision " +
			"([configapi.ApplySeatsRequest.Stated]) whose lost race is 412, where the " +
			"body's or the read's revision loses as 409; TestASaveIsOneRevisionThenASync",
		"setupapi/access.go:keepVendorSetting": "the header is the base, stated as on a " +
			"save: a lost race is 412, the read's revision loses as 409; " +
			"TestAKeepUnderAStaleIfMatchIsAFailedPrecondition",
		"setupapi/setupapi.go:disconnect": "the header is the base, a stated revision " +
			"([configapi.ApplyRequest.Stated]) whose lost race is 412, where a " +
			"connect's body base loses as 409; " +
			"TestADisconnectUnderAStaleIfMatchIsAFailedPrecondition",
		"workapi/workapi.go:ifMatch": "folded into the tool's version and answered " +
			"412 version_moved by callIfMatched; " +
			"TestAVersionTheHeaderStatedIsAFailedPrecondition",
		"workapi/files.go:fileVersion": "412 version_moved; " +
			"TestTheWritersRefusalsReadAsTheFileRoutesDocument",
		"artifactsapi/publish.go:ifMatch": "412 version_moved, carrying the live " +
			"entry; TestAPublishBasedOnAMovedHeadCarriesTheLiveEntry and " +
			"TestEveryArtifactRefusalIsOneClassOnBothSurfaces",
		"llmapi/credential.go:credential": "the revision the re-activation after " +
			"the seal is held to (configapi.Service.Reload): the value is sealed " +
			"first, so a moved revision is no refusal but the answer's `reload` " +
			"saying it was not re-activated (ADR-0070); " +
			"TestACredentialThatSignsInIsSealedAndReactivated",
		"refusallog.go:logRefusals": "read to log the base revision a refused " +
			"write named (`base_revision`), never to decide anything; " +
			"TestAStaleCompanyWriteLogsOneLineThroughTheApp",
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
