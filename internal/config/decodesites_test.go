package config_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// WHICH READER A STORED REVISION GOES THROUGH IS A DECISION PER CALL SITE, AND
// THIS IS WHAT HOLDS IT.
//
// # The two readers, and why both have to exist
//
// [config.DecodeSettings] refuses a revision that still carries an org chart.
// [config.DecodeCompany] does not. Neither is the safe default:
//
//   - A site that APPLIES a revision must refuse one, or the node boots,
//     serves, and runs a company with no seats in it — every mailbox gone,
//     every routing decision answering nobody — from bytes that decoded
//     cleanly.
//   - A site that READS one must not, or an operator is locked out of the
//     revision they have to look at in order to repair it. That is the one
//     moment the read is load bearing.
//
// # Why a test and not a type
//
// The two take the same bytes and answer the same shape, so a new apply path
// that reached for the lenient reader compiles, passes every test written
// about what it does, and fails only on a fleet, at a restart, weeks later.
// Nothing in the type system distinguishes them.
//
// So every call site is declared here with the reason it reads the way it
// does, and the walk fails in BOTH directions: an undeclared site, and a
// declared one that has moved or gone.
func TestEveryStoredDecodeSiteIsDeclared(t *testing.T) {
	t.Parallel()

	// THE READ PATHS. Each one exists to show an operator a revision, and
	// none of them runs it.
	readers := map[string]string{
		"cmd/crewlet/config_cmd.go": "show, export and diff: the revision an " +
			"operator has to look at is exactly the one this build refuses to run",
		"internal/api/configapi/configapi.go": "GET /config and the entity " +
			"reads, for the same reason",
		"internal/api/configapi/apply.go": "the merged document a PATCH " +
			"produces, read back leniently where the strict reader found a " +
			"peer's field; a patch over a pre-split base carries that base's " +
			"chart through, and judging the merge would refuse an operator " +
			"editing a mission for a chart they did not send",
		"internal/api/configapi/entity_apply.go": "the active revision an " +
			"entity write splices into, which may be a pre-split one",
	}

	// THE APPLY PATHS. Each one is about to RUN the revision.
	appliers := map[string]string{
		"internal/engine/reconcile.go": "the control plane's apply",
		"cmd/crewlet/reconcile.go":     "the node's own boot",
	}

	root := filepath.Join("..", "..")
	sawRead := map[string]bool{}
	sawApply := map[string]bool{}

	err := sourcetree.Walk(root, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules"):
			return fs.SkipDir
		case d.IsDir() || !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "_test.go"):
			return nil
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil,
			parser.SkipObjectResolution)
		if parseErr != nil {
			return parseErr
		}
		rel := filepath.ToSlash(filepath.Clean(strings.TrimPrefix(path, root+string(filepath.Separator))))
		ast.Inspect(file, func(n ast.Node) bool {
			call, isCall := n.(*ast.CallExpr)
			if !isCall {
				return true
			}
			sel, isSel := call.Fun.(*ast.SelectorExpr)
			if !isSel {
				return true
			}
			// THE PACKAGE QUALIFIER, checked after the selector exists: a
			// call that is not one at all has no X to type-assert, and
			// reading it in the same condition dereferences nil.
			if ident, isIdent := sel.X.(*ast.Ident); !isIdent || ident.Name != "config" {
				return true
			}
			switch sel.Sel.Name {
			case "DecodeCompany":
				sawRead[rel] = true
				if _, declared := readers[rel]; !declared {
					t.Errorf("%s reads a stored revision with the LENIENT "+
						"reader and is not declared here. If it applies the "+
						"revision it must use config.DecodeSettingsAsCompany, "+
						"which refuses one still carrying an org chart; if it "+
						"only shows one, add it above with the reason", rel)
				}
			case "DecodeSettings", "DecodeSettingsAsCompany":
				sawApply[rel] = true
				if _, declared := appliers[rel]; !declared {
					t.Errorf("%s reads a stored revision with the REFUSING "+
						"reader and is not declared here. A read path that "+
						"refuses a pre-split revision locks an operator out "+
						"of the one they have to repair", rel)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}

	// THE OTHER DIRECTION, which is what keeps the table from decaying into
	// a list of files that used to matter: a declared site that no longer
	// decodes is a decision nobody is making any more.
	for _, pair := range []struct {
		name     string
		declared map[string]string
		seen     map[string]bool
	}{
		{"read", readers, sawRead},
		{"apply", appliers, sawApply},
	} {
		for site := range pair.declared {
			if !pair.seen[site] {
				t.Errorf("%s is declared as a %s path and no longer decodes a "+
					"stored revision — remove it, or the table stops saying "+
					"anything about the sites that do", site, pair.name)
			}
		}
	}

	// AND THE CONTROL: the walk has to have found the sites at all. A
	// filter that matched nothing would report a clean module.
	if len(sawRead) == 0 || len(sawApply) == 0 {
		t.Errorf("the walk found %d read sites and %d apply sites, so it "+
			"certified nothing — check that it still recognises a call "+
			"through the `config` package name",
			len(sawRead), len(sawApply))
	}
	t.Logf("read sites: %v; apply sites: %v",
		slices.Sorted(maps.Keys(sawRead)), slices.Sorted(maps.Keys(sawApply)))
}
