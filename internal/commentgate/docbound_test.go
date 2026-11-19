package commentgate

import (
	"bufio"
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// docBound is how many bytes a package's doc may run: about 5,000 tokens, what
// reading one package's decisions should cost. On 2026-11-19, 308 of 326
// package docs were within it, and the 18 past it held 1.3 MB between them.
const docBound = 20 * 1024

// TestNoPackageDocGrowsPastItsBound holds every package doc to docBound, or to
// the size docInventory records for one that predates the bound: more is
// refused, and fewer is a number to lower in the same change.
func TestNoPackageDocGrowsPastItsBound(t *testing.T) {
	t.Parallel()
	docs, err := packageDocs(sourcetree.Root(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) == 0 {
		t.Fatal("found no package doc anywhere, so this gate would pass on nothing")
	}
	for _, pkg := range keysOf(docs, docInventory) {
		got := docs[pkg]
		allowed, inventoried := docInventory[pkg]
		switch {
		case !inventoried && got > docBound:
			t.Errorf("%s's package doc runs %d bytes, past the %d bound: keep the package's decisions and why each "+
				"alternative is wrong, and leave history to the commit and a rule another package decides to its doc "+
				"or an ADR", pkg, got, docBound)
		case inventoried && got > allowed:
			t.Errorf("%s's package doc grew to %d bytes from %d: say what was added in fewer words", pkg, got, allowed)
		case inventoried && got <= docBound:
			t.Errorf("%s's package doc is within the bound now: drop its docInventory entry", pkg)
		case inventoried && got < allowed:
			t.Errorf("%s's package doc now runs %d bytes: lower its docInventory entry from %d to %d", pkg, got, allowed, got)
		}
	}
}

// packageDocs is the size in bytes of each package's doc, by package
// directory: the `//` run each non-test file's package clause follows (go doc
// reads no test file), outside testdata and generated files.
func packageDocs(root string) (map[string]int, error) {
	docs := map[string]int{}
	err := sourcetree.Walk(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if generatedRE.Match(raw) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if n := docBytes(raw); n > 0 {
			docs[filepath.ToSlash(filepath.Dir(rel))] += n
		}
		return nil
	})
	return docs, err
}

// docBytes is the length of the `//` run the package clause follows, each
// line counted with its newline; a directive ends a run, as in blocksOverBound.
func docBytes(raw []byte) int {
	run := 0
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		s := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(s, "//") && !strings.HasPrefix(s, "//go:") && !strings.HasPrefix(s, "//line "):
			run += len(line) + 1
		case strings.HasPrefix(s, "package "):
			return run
		default:
			run = 0
		}
	}
	return 0
}
