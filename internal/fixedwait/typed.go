package fixedwait

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path"
	"path/filepath"
	"runtime"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// spellsSub reports whether src spells the identifier Sub: the bytes "Sub"
// with no identifier byte on either side. EXACT, as [sourcetree.Identifiers]
// is, and tighter than its substring test, which would also admit every
// Subscribe and Subject in the queue's tests and type-check their packages
// for nothing. A byte at or above 0x80 counts as an identifier's: outside a
// comment or a string Go allows a non-ASCII character only in an identifier,
// so "Sub" beside one there is part of a longer name, and inside a comment or
// a string it is not a reference at all.
func spellsSub(src []byte) bool {
	ident := func(b byte) bool {
		return b == '_' || b >= 0x80 || '0' <= b && b <= '9' || 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z'
	}
	for from := 0; ; {
		i := bytes.Index(src[from:], []byte("Sub"))
		if i < 0 {
			return false
		}
		at, end := from+i, from+i+len("Sub")
		if (at == 0 || !ident(src[at-1])) && (end == len(src) || !ident(src[end])) {
			return true
		}
		from = at + 1
	}
}

// typedSubs is every reference to a time.Time's Sub in test code, by the file
// it is in, read from the types of the packages that hold one: the
// directories of the test-code files spelling Sub, each type-checked as its
// test binary compiles it through [sourcetree.TypeCheck], the module's one
// loader of its own types. Only those packages are checked, since only a
// file spelling Sub can hold one; everything they import is export data.
//
// A file spelling Sub that this platform does not build is in no package go
// list answers, so its Sub cannot be typed: it is refused ([Untyped]) rather
// than passed, since a reading of the clock it held would otherwise be in no
// count on any platform the gate ran on.
func typedSubs(root string, fset *token.FileSet, files []*source,
	parse func(*source) error) (map[*source][]token.Pos, []Finding, error) {
	abs, err := filepath.Abs(root)
	if err == nil {
		abs, err = filepath.EvalSymlinks(abs)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("fixedwait: resolving %s: %w", root, err)
	}
	byPath := map[string]*source{}
	spelling := map[*source]bool{}
	dirs := map[string]bool{}
	for _, s := range files {
		byPath[filepath.Join(abs, filepath.FromSlash(s.rel))] = s
		if s.test && spellsSub(s.src) {
			spelling[s] = true
			dirs[s.dir] = true
		}
	}
	if len(spelling) == 0 {
		return nil, nil, nil
	}
	var patterns []string
	for dir := range dirs {
		patterns = append(patterns, "./"+dir)
	}
	subs := map[*source][]token.Pos{}
	typed := map[*source]bool{}
	// The walk parses a file under its path from the root, and go list
	// names it absolutely: each tree the loader is handed is known by the
	// source it came from.
	ofFile := map[*ast.File]*source{}
	err = sourcetree.TypeCheck{
		Fset:     fset,
		Patterns: patterns,
		Tests:    true,
		Parse: func(filename string) (*ast.File, error) {
			s := byPath[filename]
			if s == nil {
				// Not a file the walk keeps (go list read it where the
				// walk does not), so nothing in it is judged.
				return parser.ParseFile(fset, filename, nil, parser.SkipObjectResolution)
			}
			if parseErr := parse(s); parseErr != nil {
				return nil, parseErr
			}
			ofFile[s.file] = s
			return s.file, nil
		},
		Info: func() *types.Info { return &types.Info{Uses: map[*ast.Ident]types.Object{}} },
	}.Run(abs, func(u *sourcetree.Unit) error {
		for _, f := range u.Files {
			s := ofFile[f]
			if s == nil || !s.test {
				continue
			}
			typed[s] = true
			ast.Inspect(f, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Sub" {
					if fn, ok := u.Info.Uses[sel.Sel].(*types.Func); ok && fn.FullName() == timeSub {
						subs[s] = append(subs[s], sel.Sel.Pos())
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("fixedwait: typing the Subs of test code: %w", err)
	}
	var findings []Finding
	for s := range spelling {
		if typed[s] {
			continue
		}
		if parseErr := parse(s); parseErr != nil {
			return nil, nil, parseErr
		}
		if at := firstSub(s.file); at.IsValid() {
			findings = append(findings, Finding{
				Rule: Untyped, File: s.rel, Line: fset.Position(at).Line,
				Detail: fmt.Sprintf("%s/%s does not build this file, so the gate cannot type "+
					"this Sub and tell a time.Time's from any other: move it into a file of %s "+
					"that every platform builds, or split this file so the part holding it "+
					"builds everywhere", runtime.GOOS, runtime.GOARCH, path.Dir(s.rel)),
			})
		}
	}
	return subs, findings, nil
}

// firstSub is the position of the first selector of a member named Sub in f.
func firstSub(f *ast.File) token.Pos {
	var at token.Pos
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Sub" && !at.IsValid() {
			at = sel.Sel.Pos()
		}
		return !at.IsValid()
	})
	return at
}
