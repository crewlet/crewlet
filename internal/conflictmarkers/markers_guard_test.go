// Package conflictmarkers_test fails the build when a file in this tree still
// carries the markers of a merge conflict.
//
// It has no non-test source for the reason internal/docslinks gives: it
// certifies content rather than code, and that content is every text file in
// the repository.
//
// # Why a gate
//
// git writes a conflict into the file as ordinary lines and nothing refuses a
// commit that still carries them. A Go file fails to build, which is the one
// place a marker is loud. Everywhere else it is silent: a markdown page renders
// the markers as prose and the docs site publishes it, a YAML or SQL comment
// swallows them, and the committed dashboard bundle ships them in the binary.
// Rebasing a long branch resolves the same page several times, one commit at a
// time, and each of those commits is permanent history in a public repository
// — so a marker has to be refused at the commit that carries it, not found in
// review of the tip.
//
// # What a marker is
//
// git's own spelling at its default size: a line that STARTS with seven `<`,
// seven `|` (the merge base, in the diff3 style) or seven `>`, followed by a
// space and a label or by nothing — never one of those characters six or eight
// times, and never indented, because git writes neither. Those three are never
// anything else, so they are refused in every text file.
//
// The fourth, the line of exactly seven `=` that divides the two sides, is the
// hard one: it is ALSO a Markdown setext heading underline, the line under a
// title that makes it a first-level heading. So it is refused wherever it
// cannot be one — inside a conflict, between a start marker and its end, where
// it is always the divider, and everywhere outside a Markdown file — and
// allowed in Markdown directly under a line of text, where it is a heading.
// The cost, stated rather than hidden: a conflict whose start and end markers
// were deleted and whose divider was not, in Markdown, under a line of text,
// reads exactly like a heading and is not caught. Nothing can tell those two
// apart from the file alone, and refusing every seven-`=` underline in a
// docs tree would forbid valid Markdown to catch a botched resolution that
// also leaves the page visibly broken.
//
// A page that needs to SHOW a marker — documentation about resolving one —
// writes it so that no line starts with it: indented, inside the code block
// that shows it.
package conflictmarkers_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// buildOutput are the top-level directories .gitignore names as build output.
// They are on disk in a checkout that has run a release build and are nobody's
// text: goreleaser's archives and the notices file its hook generates.
var buildOutput = map[string]bool{"dist": true, "build": true}

// binaryProbe is how much of a file is read to decide it is not text — git's
// own figure for the same decision, and for the same reason: a NUL byte in
// the first eight thousand says binary, and reading the rest of a sixty-
// megabyte binary to find no marker in it is the whole cost of this gate.
const binaryProbe = 8000

// A FILE IN THIS TREE CARRYING A CONFLICT MARKER FAILS THE BUILD.
//
// The disk rather than the index, as every gate over this tree reads it
// (see [sourcetree]): a file nobody has added yet is one `git add -A` from a
// commit — and the `.orig` copy a merge tool leaves beside the file it
// resolved is exactly such a file, carrying the conflict whole.
func TestNoFileCarriesAConflictMarker(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	var offences []string
	files := 0
	err := sourcetree.Walk(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			relative = path
		}
		if entry.IsDir() {
			if buildOutput[relative] {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		body, text, err := readText(path)
		if err != nil || !text {
			return err
		}
		files++
		for _, found := range markers(path, body) {
			offences = append(offences, fmt.Sprintf("%s:%d: %s",
				relative, found.line, found.text))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the tree: %v", err)
	}
	// A WALK THAT READ ALMOST NOTHING certified almost nothing — a root
	// resolved to the wrong directory passes this with no offence found.
	// The tree holds thousands of text files; a few hundred is a floor no
	// real checkout comes near.
	if files < 500 {
		t.Fatalf("read %d text files under %s — this gate was certifying almost "+
			"nothing", files, root)
	}
	if len(offences) > 0 {
		t.Errorf("these files still carry the markers of a merge conflict — "+
			"resolve each and delete its markers:\n\t%s",
			strings.Join(offences, "\n\t"))
	}
}

// readText reads a file, and reports false for one that is binary by git's own
// test: a NUL byte in its first [binaryProbe] bytes. A binary file is not read
// past that.
func readText(path string) ([]byte, bool, error) {
	file, err := os.Open(path) //nolint:gosec // a path this walk produced
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = file.Close() }()
	head := make([]byte, binaryProbe)
	n, err := io.ReadFull(file, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, false, err
	}
	head = head[:n]
	if bytes.IndexByte(head, 0) >= 0 {
		return nil, false, nil
	}
	rest, err := io.ReadAll(file)
	if err != nil {
		return nil, false, err
	}
	return append(head, rest...), true, nil
}

// marker is one line a conflict left behind.
type marker struct {
	line int
	text string
}

// markers finds every conflict marker in one file's text. See the package doc
// for what is and is not one, and why the divider depends on where it stands.
func markers(name string, body []byte) []marker {
	markdown := isMarkdown(name)
	var (
		found    []marker
		inside   bool
		previous string
	)
	for number, raw := range strings.Split(string(body), "\n") {
		// A CHECKOUT WITH CRLF LINE ENDINGS writes the markers with them
		// too, and a comparison that kept the carriage return would match
		// none of them.
		line := strings.TrimSuffix(raw, "\r")
		switch {
		case isMarker(line, '<'):
			inside = true
			found = append(found, marker{number + 1, line})
		case isMarker(line, '|'):
			found = append(found, marker{number + 1, line})
		case isMarker(line, '>'):
			inside = false
			found = append(found, marker{number + 1, line})
		case line == divider:
			heading := markdown && strings.TrimSpace(previous) != ""
			if inside || !heading {
				found = append(found, marker{number + 1, line})
			}
		}
		previous = line
	}
	return found
}

// divider is the line between a conflict's two sides.
var divider = strings.Repeat("=", 7)

// isMarker reports whether a line is one of git's three labelled markers made
// of c: exactly seven of it at the start, then a space and a label, or nothing.
func isMarker(line string, c byte) bool {
	fence := strings.Repeat(string(c), 7)
	return line == fence || strings.HasPrefix(line, fence+" ")
}

// isMarkdown reports a file whose seven-`=` line can be a heading underline.
func isMarkdown(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md", ".markdown", ".mdx":
		return true
	}
	return false
}

// THE MARKERS ARE GIT'S OWN SPELLING, AND THE DIVIDER IS JUDGED BY WHERE IT
// STANDS.
//
// Every input here is built from [fence] rather than typed, because this file
// is in the tree the gate above walks: a marker at the start of one of its own
// lines would be a finding against itself.
func TestAMarkerIsGitsOwnSpelling(t *testing.T) {
	t.Parallel()
	start, base, end := fence('<')+" HEAD", fence('|')+" base", fence('>')+" topic"
	for _, tc := range []struct {
		name  string
		file  string
		lines []string
		want  []int
	}{
		{"a whole conflict in code", "x.go",
			[]string{"a", start, "ours", divider, "theirs", end, "b"}, []int{2, 4, 6}},
		{"a conflict in the diff3 style", "x.go",
			[]string{start, "ours", base, "was", divider, "theirs", end},
			[]int{1, 3, 5, 7}},
		{"markers with no label", "x.yaml",
			[]string{fence('<'), divider, fence('>')}, []int{1, 2, 3}},
		{"a checkout with CRLF endings", "x.go",
			[]string{start + "\r", divider + "\r", end + "\r"}, []int{1, 2, 3}},
		// THE SETEXT UNDERLINE, which is the reason the divider is not
		// judged alone.
		{"a markdown heading underline", "page.md",
			[]string{"A title", divider, "", "prose"}, nil},
		{"a markdown heading underline in another spelling", "page.markdown",
			[]string{"A title", divider}, nil},
		{"a divider under text inside a markdown conflict", "page.md",
			[]string{start, "ours", divider, "theirs", end}, []int{1, 3, 5}},
		{"a divider under nothing in markdown", "page.md",
			[]string{"prose", "", divider, "more"}, []int{3}},
		{"a divider at the top of a markdown file", "page.md",
			[]string{divider, "prose"}, []int{1}},
		{"a divider under text outside markdown", "x.yaml",
			[]string{"key: value", divider}, []int{2}},
		// AND WHAT GIT NEVER WRITES.
		{"six and eight of a kind", "x.go",
			[]string{strings.Repeat("<", 6) + " HEAD", strings.Repeat("<", 8) + " HEAD",
				strings.Repeat("=", 6), strings.Repeat("=", 8),
				strings.Repeat(">", 8) + " topic"}, nil},
		{"an indented marker", "page.md",
			[]string{" " + start, "    " + divider, "\t" + end}, nil},
		{"a marker glued to its label", "x.go",
			[]string{fence('<') + "HEAD", fence('>') + "topic"}, nil},
		{"a marker mid-line", "x.go",
			[]string{"x := \"" + start + "\""}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got []int
			for _, found := range markers(tc.file, []byte(strings.Join(tc.lines, "\n"))) {
				got = append(got, found.line)
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("markers on lines %v, want %v", got, tc.want)
			}
		})
	}
}

// fence is seven of c: a marker without its label.
func fence(c byte) string { return strings.Repeat(string(c), 7) }

// A BINARY FILE IS NOT READ PAST ITS PROBE, and a text file is read whole —
// however long, since a marker can sit anywhere in it.
func TestOnlyTextIsRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name string, body []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}
	long := append(bytes.Repeat([]byte("prose\n"), 2*binaryProbe),
		[]byte(fence('<')+" HEAD\n")...)
	if body, text, err := readText(write("long.md", long)); err != nil || !text ||
		len(markers("long.md", body)) != 1 {
		t.Errorf("a long text file = (%d bytes, text %v, %v), want it whole with "+
			"its one marker", len(body), text, err)
	}
	binary := append([]byte{0x7f, 'E', 'L', 'F', 0}, long...)
	if _, text, err := readText(write("crewlet", binary)); err != nil || text {
		t.Errorf("a binary file read as text %v (%v)", text, err)
	}
	if _, text, err := readText(write("empty.txt", nil)); err != nil || !text {
		t.Errorf("an empty file = (text %v, %v), want text", text, err)
	}
}
