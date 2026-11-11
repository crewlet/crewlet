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

// spellsMethod reports whether src spells one of [instantMethods] as an
// identifier: its bytes with no identifier byte on either side. EXACT, as
// [sourcetree.Identifiers] is, and tighter than its substring test, which
// would also admit every Subscribe, Subject, AfterFunc and BeforeEach in the
// module's tests and parse their files for nothing. A byte at or above 0x80
// counts as an identifier's: outside a comment or a string Go allows a
// non-ASCII character only in an identifier, so a name beside one there is
// part of a longer name, and inside a comment or a string it is not a
// reference at all.
func spellsMethod(src []byte) bool {
	ident := func(b byte) bool {
		return b == '_' || b >= 0x80 || '0' <= b && b <= '9' || 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z'
	}
	for name := range instantMethods {
		for from := 0; ; {
			i := bytes.Index(src[from:], []byte(name))
			if i < 0 {
				break
			}
			at, end := from+i, from+i+len(name)
			if (at == 0 || !ident(src[at-1])) && (end == len(src) || !ident(src[end])) {
				return true
			}
			from = at + 1
		}
	}
	return false
}

// firstMethod is the first selector in f that may refer to one of
// [instantMethods] on a time.Time, or nil: a selector of one of their names,
// since only the receiver's type says whose it is — a value's, an
// embedding's, an alias's or a method expression's alike.
//
// BUT THE WAIT OF THE SAME NAME: time.After, through the file's own name for
// package time where the enclosing declaration declares no local of that
// name, is package time's function, which [timeRefs] counts as a wait, and
// never a method, since Go lets no package declare a name its files import a
// package by. It is the one counted name package time spells both ways, and
// the module's tests spell it as the wait far more often than as the method:
// left in, it would type-check a package for every test waiting on
// time.After, and refuse as untyped every such test on a platform the gate
// does not run on. Only time's own name is told apart here — another
// package's name for itself is its package clause, which a file's imports do
// not say, so an identifier that might be one is left to the types.
func firstMethod(f *ast.File) *ast.Ident {
	timeName := importName(f, "time")
	for _, d := range f.Decls {
		var local map[string]bool
		var first *ast.Ident
		ast.Inspect(d, func(n ast.Node) bool {
			if first != nil {
				return false
			}
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || !instantMethods[sel.Sel.Name] {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); ok && sel.Sel.Name == "After" &&
				timeName != "" && timeName != "." && x.Name == timeName {
				if local == nil {
					local = localNames(d)
				}
				if !local[x.Name] {
					return true
				}
			}
			first = sel.Sel
			return false
		})
		if first != nil {
			return first
		}
	}
	return nil
}

// typedReadings is every reference in test code to a time.Time's method of
// [instantMethods], by the file it is in, read from the types of the
// packages that may hold one: the directories of the test-code files in
// which [firstMethod] finds a selector that may be one, each type-checked as
// its test binary compiles it through [sourcetree.TypeCheck], the module's
// one loader of its own types. Only those packages are checked, since only
// such a file can hold one; everything they import is export data.
//
// A file holding such a selector that this platform does not build is in no
// package go list answers, so its receiver cannot be typed: it is refused
// ([Untyped]) rather than passed, since a reading of the clock it held would
// otherwise be in no count on any platform the gate ran on.
func typedReadings(root string, fset *token.FileSet, files []*source,
	parse func(*source) error) (map[*source][]candidate, []Finding, error) {
	abs, err := filepath.Abs(root)
	if err == nil {
		abs, err = filepath.EvalSymlinks(abs)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("fixedwait: resolving %s: %w", root, err)
	}
	byPath := map[string]*source{}
	may := map[*source]*ast.Ident{}
	dirs := map[string]bool{}
	for _, s := range files {
		byPath[filepath.Join(abs, filepath.FromSlash(s.rel))] = s
		if !s.test || !spellsMethod(s.src) {
			continue
		}
		if parseErr := parse(s); parseErr != nil {
			return nil, nil, parseErr
		}
		if first := firstMethod(s.file); first != nil {
			may[s] = first
			dirs[s.dir] = true
		}
	}
	if len(may) == 0 {
		return nil, nil, nil
	}
	var patterns []string
	for dir := range dirs {
		patterns = append(patterns, "./"+dir)
	}
	refs := map[*source][]candidate{}
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
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || !instantMethods[sel.Sel.Name] {
					return true
				}
				if fn, ok := u.Info.Uses[sel.Sel].(*types.Func); ok && fn.FullName() == timeMethod(sel.Sel.Name) {
					refs[s] = append(refs[s], candidate{pos: sel.Sel.Pos(), name: methodPrefix + sel.Sel.Name})
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("fixedwait: typing test code's comparisons of instants: %w", err)
	}
	var findings []Finding
	for s, first := range may {
		if typed[s] {
			continue
		}
		findings = append(findings, Finding{
			Rule: Untyped, File: s.rel, Line: fset.Position(first.Pos()).Line,
			Detail: fmt.Sprintf("%s/%s does not build this file, so the gate cannot type "+
				"this %s's receiver and tell a time.Time's from any other: move it into a "+
				"file of %s that every platform builds, or split this file so the part "+
				"holding it builds everywhere", runtime.GOOS, runtime.GOARCH, first.Name, path.Dir(s.rel)),
		})
	}
	return refs, findings, nil
}
