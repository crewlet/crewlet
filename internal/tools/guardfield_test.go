package tools_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// THE GUARD FIELD IS THE SKILL GATE'S, AND AUTHORITY NEVER GOES THROUGH IT.
//
// [Surface.WithGuard] REPLACES the guard rather than composing with it, and a
// surface holds exactly one. So the day somebody wires an authority decision
// through it — the obvious move, since it already sits in front of every call
// — the skill gate is displaced, and a tool a company locked behind a required
// skill is callable before the skill was ever loaded. Nothing would fail: the
// authority check would pass the tools it should, and the locked tool would
// simply stop being locked.
//
// Authority lives in internal/agent/builtin's own gate instead, which wraps
// each tool at registration and covers the operator's assistant and the HTTP
// surface as well — two callers that never build a Surface at all.
//
// A WALK OF THE SOURCE rather than a behavioural test, because the failure is
// an INSTALLATION nobody would write a test for: every call site that installs
// a guard is named here, and a new one fails the build until somebody reads
// this and decides whether it displaces the skill gate.
func TestTheSkillGateStillHoldsTheGuardField(t *testing.T) {
	t.Parallel()
	// THE TWO INSTALLERS OF THE SKILL GATE: a phase's surface, and a
	// worker's child surface built from its parent's.
	want := []string{
		"internal/agent/runner/phases.go",
		"internal/agent/subagent/subagent.go",
	}
	root := filepath.Join("..", "..")
	var installers, authority []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string,
			d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") ||
				strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch {
				case sel.Sel.Name == "WithGuard" && rel != "internal/tools/surface.go":
					if !slices.Contains(installers, rel) {
						installers = append(installers, rel)
					}
				// AND THE AUTHORITY PACKAGE NEVER NAMES THE SEAM AT
				// ALL: an authority decision expressed as a
				// tools.Guard is the displacement this test exists
				// to stop, whoever ends up installing it.
				case strings.HasPrefix(rel, "internal/agent/builtin/") &&
					sel.Sel.Name == "Guard":
					if x, ok := sel.X.(*ast.Ident); ok && x.Name == "tools" {
						authority = append(authority, rel)
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	slices.Sort(installers)
	if !slices.Equal(installers, want) {
		t.Errorf("a surface's guard is installed from %v, want only the skill "+
			"gate's two installers %v — WithGuard REPLACES the guard, so a new "+
			"installer displaces the skill gate and unlocks every tool a "+
			"company locked behind a skill", installers, want)
	}
	if len(authority) > 0 {
		t.Errorf("internal/agent/builtin names tools.Guard in %v: authority "+
			"is decided by the registration gate, never through the skill "+
			"gate's seat", authority)
	}
}
