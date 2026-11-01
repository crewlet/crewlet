package httpx_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// TestNoOtherPackageJudgesWhetherAnAddressIsPublic fails the build when a
// package outside this one asks an address the questions [httpx.Public]
// answers, which is how a second copy of the rule starts (ADR-0072).
//
// Two copies were in the tree when the rule moved here: the artifact CDN
// fetch's guard and GitHub's reachability check, and the second admitted
// carrier-grade NAT, the documentation ranges and multicast that the first
// refused. Neither said it was a copy of the other.
//
// # What it flags
//
// In every production file under internal/ and cmd/ that imports net or
// net/netip, outside this package: a call of IsPrivate, IsLinkLocalUnicast,
// IsLinkLocalMulticast, IsInterfaceLocalMulticast or IsGlobalUnicast. The
// last is here because it is the tempting wrong answer: it reports true for
// 10.0.0.1, so a check written with it admits every private address. IsLoopback
// and IsUnspecified are not flagged, since "does this name this host" is a
// question of its own that several packages rightly ask.
//
// The match is syntactic, a method name on any receiver in a file that imports
// an address package, so an unrelated method of the same name in such a file
// would be reported too; none exists, and the message says what to do if one
// appears.
func TestNoOtherPackageJudgesWhetherAnAddressIsPublic(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		src  string
		want bool
	}{
		{`package p; import "net"; func f(s string) bool { return net.ParseIP(s).IsPrivate() }`, true},
		{`package p; import "net/netip"; func f(a netip.Addr) bool { return a.IsGlobalUnicast() }`, true},
		{`package p; import "net/netip"; func f(a netip.Addr) bool { return a.IsLinkLocalUnicast() }`, true},
		{`package p; import "net"; func f(ip net.IP) bool { return ip.IsLoopback() || ip.IsUnspecified() }`, false},
		{`package p; type room struct{}; func (room) IsPrivate() bool { return true }; var _ = room{}.IsPrivate()`, false},
	} {
		if got := len(addressJudgementsInSource(t, c.src)) > 0; got != c.want {
			t.Errorf("control: matcher returned %v for %q, want %v", got, c.src, c.want)
		}
	}

	root := sourcetree.Root(t)
	files, hits := walkForAddressJudgements(t, root)
	if files == 0 {
		t.Fatal("parsed no production file importing net or net/netip under internal/ " +
			"and cmd/, so this guard was certifying nothing")
	}
	for _, h := range hits {
		t.Errorf("%s judges whether an address is public with %s.\n"+
			"\task httpx.Public, the one rule, or dial through httpx.Guarded; if this "+
			"is a method of another type that only shares the name, rename it", h.pos, h.what)
	}
	t.Logf("scanned %d production files importing an address package: %d judgement(s) outside httpx",
		files, len(hits))
}

// addressJudgementNames are the methods whose answers [httpx.Public] combines.
var addressJudgementNames = map[string]bool{
	"IsPrivate": true, "IsLinkLocalUnicast": true, "IsLinkLocalMulticast": true,
	"IsInterfaceLocalMulticast": true, "IsGlobalUnicast": true,
}

type addressHit struct{ pos, what string }

// walkForAddressJudgements parses every production .go file under internal/
// and cmd/, outside this package's own directory, that imports net or
// net/netip, and collects its address judgements. The imports are read first,
// as the pool guard beside this one reads them, so a file that cannot hold a
// hit is never parsed whole.
func walkForAddressJudgements(t *testing.T, root string) (int, []addressHit) {
	t.Helper()
	selfDir := filepath.Join(root, "internal", "httpx")
	fset := token.NewFileSet()
	var files int
	var hits []addressHit
	for _, tree := range []string{"internal", "cmd"} {
		err := sourcetree.Walk(filepath.Join(root, tree), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if filepath.Base(path) == "testdata" {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") ||
				filepath.Dir(path) == selfDir {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			imports, err := parser.ParseFile(token.NewFileSet(), path, src, parser.ImportsOnly)
			if err != nil {
				t.Errorf("parse %s: %v", path, err)
				return nil
			}
			if !importsAddressPackage(imports) {
				return nil
			}
			file, err := parser.ParseFile(fset, path, src, 0)
			if err != nil {
				t.Errorf("parse %s: %v", path, err)
				return nil
			}
			files++
			for _, sel := range addressJudgements(file) {
				hits = append(hits, addressHit{
					pos:  shortPos(root, fset.Position(sel.Pos()).String()),
					what: sel.Sel.Name,
				})
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", tree, err)
		}
	}
	return files, hits
}

// importsAddressPackage reports whether file imports net or net/netip.
func importsAddressPackage(file *ast.File) bool {
	for _, imp := range file.Imports {
		if imp.Path != nil && (imp.Path.Value == `"net"` || imp.Path.Value == `"net/netip"`) {
			return true
		}
	}
	return false
}

// addressJudgements finds every call of an [addressJudgementNames] method.
func addressJudgements(file *ast.File) []*ast.SelectorExpr {
	var out []*ast.SelectorExpr
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && addressJudgementNames[sel.Sel.Name] {
			out = append(out, sel)
		}
		return true
	})
	return out
}

// addressJudgementsInSource runs the matcher over one snippet, for the
// controls, as the walk would: nothing unless it imports an address package.
func addressJudgementsInSource(t *testing.T, src string) []*ast.SelectorExpr {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "control.go", src, 0)
	if err != nil {
		t.Fatalf("parse control %q: %v", src, err)
	}
	if !importsAddressPackage(file) {
		return nil
	}
	return addressJudgements(file)
}
