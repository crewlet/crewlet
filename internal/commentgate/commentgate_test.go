package commentgate

import (
	"bufio"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// blockBound is how many lines a comment block outside a package doc may run.
// Twelve hold a decision and why the obvious alternative is wrong; in the tree
// of 2026-11-17 nine blocks in ten ran shorter, while blocks over ten lines
// were 42% of all comment bytes. The argument behind a longer one belongs in
// the package doc, written once, where a reader goes for it on purpose.
const blockBound = 12

// rowBound is how long one internal/README.md row may run, in characters: the
// package's name and one line saying what it is for.
const rowBound = 300

// TestNoPackageGrowsItsCommentsPastTheBound holds every package to its
// inventory of comment lines past blockBound: zero for a package the inventory
// does not name, and exactly the recorded count for one it does — more is
// refused, and fewer is a number to lower in the same change.
func TestNoPackageGrowsItsCommentsPastTheBound(t *testing.T) {
	t.Parallel()
	over, err := commentsOverBound(sourcetree.Root(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(over) == 0 {
		t.Fatal("found no comment block past the bound anywhere, so the walk read nothing")
	}
	for _, pkg := range keysOf(over, excessInventory) {
		got, allowed := excessOf(over[pkg]), excessInventory[pkg]
		switch {
		case got > allowed:
			t.Errorf("%s runs %d comment lines past the %d-line bound and may run %d: say it in fewer lines, or write the "+
				"argument once in the package doc and point there. Its blocks past the bound:\n%s",
				pkg, got, blockBound, allowed, listBlocks(over[pkg]))
		case got < allowed:
			t.Errorf("%s now runs %d comment lines past the bound: lower its excessInventory entry from %d to %d", pkg, got, allowed, got)
		}
	}
}

// TestNoReadmeRowGrowsPastOneLine holds every internal/README.md row to
// rowBound, or to the length rowInventory records for a row that predates it.
func TestNoReadmeRowGrowsPastOneLine(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(sourcetree.Root(t), "internal", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]int{}
	for _, m := range rowRE.FindAllStringSubmatch(string(raw), -1) {
		rows[strings.ReplaceAll(m[2], "`", "")] = utf8.RuneCountInString(m[1])
	}
	if len(rows) == 0 {
		t.Fatal("found no row in internal/README.md, so this gate would pass on nothing")
	}
	for _, key := range keysOf(rows, rowInventory) {
		got, ok := rows[key]
		allowed, inventoried := rowInventory[key]
		switch {
		case !ok:
			t.Errorf("rowInventory names the row %q, which internal/README.md no longer has: drop its entry", key)
		case !inventoried && got > rowBound:
			t.Errorf("the %q row runs %d characters, past the %d of one line: say what the package is for, and put "+
				"its decisions in its package doc", key, got, rowBound)
		case inventoried && got > allowed:
			t.Errorf("the %q row grew to %d characters from %d: put what was added in the package doc instead", key, got, allowed)
		case inventoried && got <= rowBound:
			t.Errorf("the %q row is one line now: drop its rowInventory entry", key)
		case inventoried && got < allowed:
			t.Errorf("the %q row now runs %d characters: lower its rowInventory entry from %d to %d", key, got, allowed, got)
		}
	}
}

// rowRE is a table row whose first cell names packages: the whole line, and
// that cell.
var rowRE = regexp.MustCompile("(?m)^(\\| (`[^|]*?) \\| .*)$")

// block is one comment block past the bound.
type block struct {
	file  string
	line  int
	lines int
}

// commentsOverBound is every comment block past blockBound, by package
// directory, over the tree's Go files, tests included (they are read as much
// as the code), outside testdata and generated files.
func commentsOverBound(root string) (map[string][]block, error) {
	over := map[string][]block{}
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
		if !strings.HasSuffix(path, ".go") {
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
		rel = filepath.ToSlash(rel)
		pkg := filepath.ToSlash(filepath.Dir(rel))
		over[pkg] = append(over[pkg], blocksOverBound(rel, raw)...)
		if len(over[pkg]) == 0 {
			delete(over, pkg)
		}
		return nil
	})
	return over, err
}

var generatedRE = regexp.MustCompile(`(?m)^// Code generated .* DO NOT EDIT\.$`)

// blocksOverBound is one file's comment blocks past blockBound. A block is a
// run of `//` lines; a directive (`//go:`, `//line `) ends a run rather than
// joining it, and the run the package clause follows is the package doc.
func blocksOverBound(file string, raw []byte) []block {
	var (
		out         []block
		start, n, i int
	)
	flush := func(next string) {
		if n > blockBound && !strings.HasPrefix(next, "package ") {
			out = append(out, block{file: file, line: start, lines: n})
		}
		n = 0
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		i++
		s := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(s, "//") && !strings.HasPrefix(s, "//go:") && !strings.HasPrefix(s, "//line ") {
			if n == 0 {
				start = i
			}
			n++
			continue
		}
		flush(s)
	}
	flush("")
	return out
}

func excessOf(blocks []block) int {
	sum := 0
	for _, b := range blocks {
		sum += b.lines - blockBound
	}
	return sum
}

func listBlocks(blocks []block) string {
	var b strings.Builder
	for _, bl := range blocks {
		fmt.Fprintf(&b, "\t%s:%d: %d lines\n", bl.file, bl.line, bl.lines)
	}
	return b.String()
}

func keysOf[V, W any](a map[string]V, b map[string]W) []string {
	seen := map[string]bool{}
	for k := range a {
		seen[k] = true
	}
	for k := range b {
		seen[k] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
