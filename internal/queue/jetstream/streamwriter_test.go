package jetstream_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// TestOnlyOnePlaceWritesARunningStreamsConfiguration fails the build when a
// second site updates a live stream's configuration.
//
// # Why an absence needs a test
//
// A JetStream update REPLACES the whole configuration, and a stream is shared
// by every node of a fleet. So two writers are not two writes — they are last
// boot wins, over a value an operator may have set deliberately and over one a
// capacity operation may be in the middle of changing. That failure is silent
// in every direction: the update succeeds, the stream is well-formed, and the
// only symptom is a ceiling that quietly went back to what some other node's
// Tier A file said.
//
// The engine had exactly this shape once — every node applying its own spec to
// a shared stream at every boot — and the fix was to remove the writer rather
// than to guard it. [DomainLog.SetMaxBytes] is what remains, and it is safe
// only because the capacity procedure stops every publisher on every node
// first. A second call site would inherit none of that and nothing would say
// so, which is what this test is for.
//
// # What it walks, and what it cannot see
//
// Every non-test .go file under internal/ and cmd/, parsed with go/parser,
// looking for a CALL whose selector is UpdateStream or CreateOrUpdateStream.
// It matches on the NAME rather than on a resolved type, so it would also flag
// a same-named method on something unrelated — which is the safe direction: a
// false positive is one allowance entry with a reason, and a false negative is
// the silent failure above.
//
// It does not see a call made through a variable of interface type whose
// method it cannot name, nor one assembled by reflection. Neither exists here,
// and both would be a stranger thing to write than the call itself.
func TestOnlyOnePlaceWritesARunningStreamsConfiguration(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)

	// THE MATCHER, ON INPUT WHOSE VERDICT IS KNOWN. A guard asserting an
	// absence passes identically when the thing is absent and when the
	// guard has gone inert.
	for _, positive := range []string{
		"js.UpdateStream(ctx, config)",
		"q.js.CreateOrUpdateStream(ctx, cfg)",
	} {
		if !writesStreamConfig(t, positive) {
			t.Errorf("control: %q writes a stream's configuration and the "+
				"matcher did not flag it", positive)
		}
	}
	// AND THE ALLOWANCE, on paths whose verdict is known. The walk only
	// ever reaches it with the one real site, so an allowance that has
	// widened to cover everything reports a clean tree whatever it finds.
	if allowedStreamWriter("internal/engine/capacity.go:67 UpdateStream") {
		t.Error("control: a second site is being allowed, so this guard would " +
			"pass with any number of writers in the tree")
	}
	if !allowedStreamWriter("internal/queue/jetstream/domainlog.go:310 UpdateStream") {
		t.Error("control: the one legitimate writer is not allowed, so this " +
			"guard fails on a tree that is correct")
	}

	for _, negative := range []string{
		"js.CreateStream(ctx, config)",
		"js.Stream(ctx, name)",
		"stream.Purge(ctx)",
		"js.CreateOrUpdateConsumer(ctx, stream, cfg)",
	} {
		if writesStreamConfig(t, negative) {
			t.Errorf("control: %q does not write a stream's configuration and "+
				"the matcher flagged it", negative)
		}
	}

	scan := streamWriters(t, root)
	if scan.read == 0 {
		t.Fatal("read no source files — this guard was certifying nothing")
	}
	found := scan.found

	sort.Strings(found)
	var unexpected []string
	for _, site := range found {
		if allowedStreamWriter(site) {
			continue
		}
		unexpected = append(unexpected, site)
	}
	for _, site := range unexpected {
		t.Errorf("%s writes a running stream's configuration.\n"+
			"\tA JetStream update REPLACES the whole configuration of a stream "+
			"every node of the fleet shares, so a second writer is last-write-"+
			"wins over a ceiling somebody set deliberately — and it succeeds "+
			"every time, so nothing says it happened. If this call is genuinely "+
			"necessary, it belongs behind the same exclusion "+
			"internal/engine/capacity.go establishes.", site)
	}
	if len(found) == 0 {
		t.Fatal("found no stream-configuration writes at all — the one legitimate " +
			"caller is gone, so this guard is watching for a call nobody makes " +
			"and would pass whatever the tree did. Delete it, or fix the walk")
	}
	t.Logf("read %d files, parsed the %d that name a writer; stream-configuration "+
		"writers: %v", scan.read, scan.parsed, found)
}

// streamWriterScan is what one walk for stream-configuration writers found.
type streamWriterScan struct {
	// read is every non-test Go file under internal/ and cmd/, counted
	// before the prefilter; parsed is the ones that passed it.
	read, parsed int
	found        []string
}

// streamWriters walks root for calls that write a stream's configuration.
//
// ONLY A FILE THAT SPELLS A WRITER IS PARSED. A selector names its method in
// the source, and an identifier has no escapes, so a file whose bytes do not
// hold one of streamConfigWriters holds no call of it (sourcetree.Identifiers)
// — and of the tree's thousand-odd files, one does. Parsing all of them to
// find it was six seconds under the race detector.
func streamWriters(t *testing.T, root string) streamWriterScan {
	t.Helper()
	names := sourcetree.MustIdentifiers(streamConfigWriters...)
	var scan streamWriterScan
	err := sourcetree.Walk(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "dist", "static", "dashboard":
				return fs.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if !strings.HasPrefix(rel, "internal"+string(filepath.Separator)) &&
			!strings.HasPrefix(rel, "cmd"+string(filepath.Separator)) {
			return nil
		}
		scan.read++
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !names.In(src) {
			return nil
		}
		scan.parsed++
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !streamConfigWrite(sel.Sel.Name) {
				return true
			}
			pos := fset.Position(call.Pos())
			scan.found = append(scan.found, filepath.ToSlash(rel)+":"+itoaLine(pos.Line)+" "+sel.Sel.Name)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return scan
}

// THE WALK, ON A TREE WHOSE VERDICT IS KNOWN: both writers are found through
// the same prefilter, and a file that names neither is never parsed — it is
// not even Go, so parsing it would fail the walk.
func TestTheStreamWriterWalkParsesOnlyWhatNamesAWriter(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for path, body := range map[string]string{
		"internal/a/a.go":      "package a\n\nfunc f() { js.UpdateStream(ctx, cfg) }\n",
		"cmd/c/c.go":           "package main\n\nfunc g() { q.js.CreateOrUpdateStream(ctx, cfg) }\n",
		"internal/b/b.go":      "package b\n\nthis is not Go and names no writer\n",
		"internal/a/a_test.go": "package a\n\nfunc h() { js.UpdateStream(ctx, cfg) }\n",
		"docs/elsewhere.go":    "package docs\n\nfunc i() { js.UpdateStream(ctx, cfg) }\n",
	} {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	scan := streamWriters(t, root)
	sort.Strings(scan.found)
	want := []string{"cmd/c/c.go:3 CreateOrUpdateStream", "internal/a/a.go:3 UpdateStream"}
	if scan.read != 3 || scan.parsed != 2 || !slices.Equal(scan.found, want) {
		t.Errorf("read %d, parsed %d, found %q; want 3, 2 and %q", scan.read,
			scan.parsed, scan.found, want)
	}
}

// streamConfigWriters are the calls that replace a live stream's config.
var streamConfigWriters = []string{"UpdateStream", "CreateOrUpdateStream"}

// streamConfigWrite reports whether a call's method replaces a live stream's
// config.
func streamConfigWrite(name string) bool {
	return slices.Contains(streamConfigWriters, name)
}

// allowedStreamWriter is the one site, named by file and function rather than
// by line so an edit above it does not have to be reflected here.
func allowedStreamWriter(site string) bool {
	return strings.HasPrefix(site, "internal/queue/jetstream/domainlog.go:")
}

// writesStreamConfig parses one expression and reports whether the matcher
// flags it, so the controls above exercise the same code the walk does.
func writesStreamConfig(t *testing.T, expr string) bool {
	t.Helper()
	parsed, err := parser.ParseExpr(expr)
	if err != nil {
		t.Fatalf("control %q does not parse: %v", expr, err)
	}
	flagged := false
	ast.Inspect(parsed, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && streamConfigWrite(sel.Sel.Name) {
			flagged = true
		}
		return true
	})
	return flagged
}

func itoaLine(n int) string {
	if n == 0 {
		return "0"
	}
	var out []byte
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	return string(out)
}
