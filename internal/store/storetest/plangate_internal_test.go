package storetest

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// A PLAN IS READ THROUGH STORETEST, AND NOWHERE ELSE.
//
// The package doc says what every plan gate reads is here, once: a statement's
// plan ([PlanTree], [Plan]) and what a plan line says ([Decorrelated], [Seeks]
// and the rest). That held only for the gates that happened to call it — six
// read their plans through EXPLAIN QUERY PLAN loops of their own, each asking
// less of the plan than the shared reading does, and one of them planned a
// copy of the statement it was written to hold. So every Go file of the
// module, tests included, is walked, and a string literal that spells an
// EXPLAIN QUERY PLAN anywhere but this package's plan.go fails it, by file and
// line.
//
// ONE EXEMPTION, recognised from the parsed package name rather than a list of
// files: package store's own files under internal/store, whose internal tests
// cannot import this package, because this package imports theirs. None of
// that package's statements is a correlated aggregate the shared reading would
// catch, which is why the copy there costs nothing it would find.
//
// Comments are not literals, so a doc comment saying a plan was measured with
// EXPLAIN is not one.
//
// Mutation: re-add the usage gate's own EXPLAIN loop and this names
// internal/usage/plan_internal_test.go; drop the package-store exemption and
// it names internal/store/eventfilter_internal_test.go; skip the test files
// and it reads too few of them to say anything.
func TestEveryPlanIsReadThroughStoretest(t *testing.T) {
	t.Parallel()
	root, err := sourcetree.ModuleRoot()
	if err != nil {
		t.Fatalf("find the module root: %v", err)
	}
	explains := regexp.MustCompile(`(?i)\bEXPLAIN\s+QUERY\s+PLAN\b`)
	reading := filepath.Join("internal", "store", "storetest", "plan.go")
	storeDir := filepath.Join("internal", "store")
	fset := token.NewFileSet()
	files, tests, exempt := 0, 0, 0
	err = sourcetree.Walk(root, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && d.Name() == "testdata":
			// NOT THE MODULE'S CODE: nothing compiles it.
			return fs.SkipDir
		case d.IsDir() || !strings.HasSuffix(path, ".go"):
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("parse %s: %w", rel, err)
		}
		files++
		if strings.HasSuffix(path, "_test.go") {
			tests++
		}
		switch {
		case rel == reading:
			return nil
		case filepath.Dir(rel) == storeDir && file.Name.Name == "store":
			// THE IMPORT CYCLE: see the exemption above.
			exempt++
			return nil
		}
		for _, run := range literalRunsAt(file) {
			if explains.MatchString(run.text) {
				t.Errorf("%s:%d reads a plan itself — plan the statement through "+
					"storetest.Plan, the one reading of a plan every gate shares: "+
					"it plans a write in a write transaction and refuses a plan "+
					"that says nothing", rel, fset.Position(run.pos).Line)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}
	// A FLOOR ON EACH HALF, because a walk that read nothing, or no tests —
	// which is where a gate's own plan reading lives — would pass having
	// held nothing.
	if files < 1000 || tests < 500 || exempt == 0 {
		t.Fatalf("read %d Go files, %d of them tests, and %d of package store's "+
			"own — this walk is not reading the module it holds", files, tests, exempt)
	}
}
