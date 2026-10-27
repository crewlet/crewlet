package statelog_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// TestAWaitIsSaidAsAPersonPlansBy pins the one wait rule the engine and the
// dashboard share (`fmtWait`, dashboard/src/lib/format.ts): the largest two
// units, ROUNDED UP at every magnitude, so a wait never reads shorter than it
// is and a reader who comes back when told is not refused again.
func TestAWaitIsSaidAsAPersonPlansBy(t *testing.T) {
	for _, c := range []struct {
		wait time.Duration
		want string
	}{
		{0, "1s"},
		{200 * time.Millisecond, "1s"},
		{45 * time.Second, "45s"},
		{59100 * time.Millisecond, "1m"},
		{4*time.Minute + 29*time.Second, "5m"},
		{time.Hour, "1h"},
		{81155 * time.Second, "22h 33m"},
		{2*24*time.Hour + 3*time.Hour, "2d 3h"},
		{3 * 24 * time.Hour, "3d"},
	} {
		if got := statelog.SpokenWait(c.wait); got != c.want {
			t.Errorf("SpokenWait(%v) = %q, want %q", c.wait, got, c.want)
		}
	}
	// A DURATION THAT IS NOT A WAIT keeps the measuring rule (nearest unit).
	if got := statelog.Spoken(81155 * time.Second); got != "22h 33m" {
		t.Fatalf("Spoken(81155s) = %q, want %q", got, "22h 33m")
	}
}

// waitPhrase is a sentence telling a person how long to wait: "retry in",
// "again in", "try again in", each optionally "about".
var waitPhrase = regexp.MustCompile(`(?i)\b(retry|again) in (about )?$`)

// verb is one fmt verb, `%%` excluded by the caller.
var verb = regexp.MustCompile(`%[-+ #0-9.*]*[a-zA-Z]`)

// TestEveryWaitIsSaidThroughSpokenWait holds the engine's sentences to that
// rule, structurally: in every fmt call whose format tells a person to wait
// ("retry in about %s", "holds it again in %v"), the argument the verb prints
// is a call to statelog.SpokenWait. A count printed as seconds ("retry in
// about %d seconds", the bulk lease's 81155 seconds) and a Duration printed by
// its own String ("again in 1m23.456s") both fail, because neither argument
// is that call.
func TestEveryWaitIsSaidThroughSpokenWait(t *testing.T) {
	root := filepath.Join("..", "..")
	var offenders []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			offenders = append(offenders, unspokenWaits(t, path)...)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("a wait is printed in machine form; say it through statelog.SpokenWait:\n%s",
			strings.Join(offenders, "\n"))
	}
}

// unspokenWaits returns every wait verb in one file whose argument is not a
// statelog.SpokenWait call.
func unspokenWaits(t *testing.T, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "fmt" {
			return true
		}
		// The format is the first argument of Sprintf/Errorf/Printf and the
		// second of Fprintf.
		at := 0
		if strings.HasPrefix(sel.Sel.Name, "F") {
			at = 1
		}
		if len(call.Args) <= at {
			return true
		}
		format, ok := constantString(call.Args[at])
		if !ok {
			return true
		}
		args := call.Args[at+1:]
		index := 0
		rest := strings.ReplaceAll(format, "%%", "")
		for _, loc := range verb.FindAllStringIndex(rest, -1) {
			before, after := rest[:loc[0]], rest[loc[1]:]
			wait := waitPhrase.MatchString(before) ||
				regexp.MustCompile(`^ ?(seconds|secs)\b`).MatchString(after)
			if wait {
				if index >= len(args) || !isSpokenWait(args[index]) {
					out = append(out, fset.Position(call.Pos()).String()+": "+strconv.Quote(format))
				}
			}
			index++
		}
		return true
	})
	return out
}

// constantString evaluates a string literal, or a `+` of string literals.
func constantString(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		l, ok := constantString(v.X)
		if !ok {
			return "", false
		}
		r, ok := constantString(v.Y)
		return l + r, ok
	case *ast.ParenExpr:
		return constantString(v.X)
	}
	return "", false
}

// isSpokenWait reports whether an argument is `statelog.SpokenWait(...)`, or
// `SpokenWait(...)` inside this package.
func isSpokenWait(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		pkg, ok := fn.X.(*ast.Ident)
		return ok && pkg.Name == "statelog" && fn.Sel.Name == "SpokenWait"
	case *ast.Ident:
		return fn.Name == "SpokenWait"
	}
	return false
}
