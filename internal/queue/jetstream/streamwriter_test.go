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
// second site updates a live stream's configuration. A JetStream update
// REPLACES the whole configuration of a stream every node shares, so a second
// writer is last-boot-wins over a ceiling set deliberately, or one a capacity
// operation is changing, and the update succeeds silently. The writers
// [allowedStreamWriter] admits are safe only behind exclusions a third lacks.
//
// It flags a CALL by its selector's NAME (UpdateStream, CreateOrUpdateStream)
// in every non-test file under internal/ and cmd/, never a resolved type: a
// false positive costs an allowance entry, a false negative the silent
// failure above. A call through an interface method it cannot name, or by
// reflection, it cannot see; neither exists here.
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
	// ever reaches it with the real sites, so an allowance that has
	// widened to cover everything reports a clean tree whatever it finds.
	if allowedStreamWriter("internal/engine/capacity.go:67 UpdateStream") {
		t.Error("control: an unexcluded site is being allowed, so this guard " +
			"would pass with any number of writers in the tree")
	}
	if allowedStreamWriter("internal/objstore/natsobj/natsobj.go:120 CreateOrUpdateStream") {
		t.Error("control: the allowance names a file rather than its one " +
			"writer, so a second write in that file would pass unseen")
	}
	for _, legitimate := range []string{
		"internal/queue/jetstream/domainlog.go:310 UpdateStream",
		"internal/objstore/natsobj/natsobj.go:496 UpdateStream",
	} {
		if !allowedStreamWriter(legitimate) {
			t.Errorf("control: the legitimate writer %q is not allowed, so "+
				"this guard fails on a tree that is correct", legitimate)
		}
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
		t.Fatal("found no stream-configuration writes at all. The legitimate " +
			"callers are gone, so this guard is watching for a call nobody makes " +
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
// ONLY A FILE THAT SPELLS A WRITER IS PARSED: an identifier has no escapes, so
// a file whose bytes hold none of streamConfigWriters holds no call of one
// (sourcetree.Identifiers), and parsing the whole tree costs seconds under
// the race detector.
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

// allowedStreamWriter reports whether site is one of the excluded writers,
// named by file and call rather than by line:
//
//   - internal/queue/jetstream/domainlog.go: [DomainLog.SetMaxBytes], run only
//     by the capacity procedure once every publisher on every node is stopped.
//   - internal/objstore/natsobj/natsobj.go UpdateStream alone: the bucket's
//     seal, run only by `crewlet objects migrate -final`, which refuses unless
//     this node is in maintenance mode and no node holds an admission. The
//     bucket is made with CreateStream, so no CreateOrUpdateStream is allowed.
func allowedStreamWriter(site string) bool {
	if strings.HasPrefix(site, "internal/queue/jetstream/domainlog.go:") {
		return true
	}
	file, call, ok := strings.Cut(site, " ")
	return ok && call == "UpdateStream" &&
		strings.HasPrefix(file, "internal/objstore/natsobj/natsobj.go:")
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
