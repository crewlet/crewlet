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
// Gates with their own EXPLAIN loops asked less of a plan than the package
// doc's shared reading, so every Go file, tests included, is walked and a
// string literal spelling EXPLAIN QUERY PLAN anywhere but plan.go fails.
// ONE EXEMPTION, from the parsed package name: package store's own files, whose
// tests cannot import this package (it imports theirs).
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
