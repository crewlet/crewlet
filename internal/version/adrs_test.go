package version

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The ADR tree is cited by number from the code it governs, and nothing about
// a citation is checked by the compiler: `adr-104` in a comment is prose, and
// so is `adrs/104-pulsar-redelivery-economics.md` in a markdown link. A record
// that is renamed, renumbered or removed therefore leaves every pointer at it
// dangling with no symptom at all — the build stays green, the tests stay
// green, and the next reader follows a reference to nothing.
//
// That is the failure mode this file exists for, and it is the same posture
// the rest of this package takes about the release surface: a rule with no
// other symptom is asserted rather than remembered.

// adrCitation matches the short form (adr-104) and the path form
// (adrs/104-pulsar-redelivery-economics.md), capturing the number in both.
var adrCitation = regexp.MustCompile(`\badrs?[-/](\d{3})\b`)

// adrFilename matches a record's own filename: three digits, a hyphen, a slug.
var adrFilename = regexp.MustCompile(`^(\d{3})-[a-z0-9-]+\.md$`)

func TestEveryADRCitationResolves(t *testing.T) {
	t.Parallel()
	root, _ := moduleRoot(t)
	known := adrNumbers(t, root)

	type site struct{ file, line string }
	dangling := map[string][]site{}

	walkRepo(t, root, func(path string, body []byte) {
		rel, _ := filepath.Rel(root, path)
		for i, line := range strings.Split(string(body), "\n") {
			for _, m := range adrCitation.FindAllStringSubmatch(line, -1) {
				num := m[1]
				if _, ok := known[num]; ok {
					continue
				}
				dangling[num] = append(dangling[num], site{
					file: rel + ":" + strconv.Itoa(i+1),
					line: elide(strings.TrimSpace(line)),
				})
			}
		}
	})

	if len(dangling) == 0 {
		return
	}
	nums := make([]string, 0, len(dangling))
	for n := range dangling {
		nums = append(nums, n)
	}
	sort.Strings(nums)
	var b strings.Builder
	b.WriteString("citations of ADRs that do not exist in adrs/:\n")
	for _, n := range nums {
		b.WriteString("\n  adr-" + n + " is cited by:\n")
		for _, s := range dangling[n] {
			b.WriteString("    " + s.file + "\n      " + s.line + "\n")
		}
	}
	b.WriteString("\nAn ADR is never renumbered and never reused, so a dangling citation means\n")
	b.WriteString("the record was renamed or removed. Repoint the citation, or restate the\n")
	b.WriteString("reasoning where it is cited — do not delete the pointer and leave the\n")
	b.WriteString("claim it was supporting unexplained.")
	t.Fatal(b.String())
}

func TestNoTwoADRsClaimOneNumber(t *testing.T) {
	t.Parallel()
	root, _ := moduleRoot(t)
	// adrNumbers fails on a collision; a number resolving to two files would
	// make every citation of it ambiguous, and nothing else would notice.
	adrNumbers(t, root)
}

// adrNumbers reads adrs/ and returns number -> filename, failing on a
// malformed name or a duplicated number.
func adrNumbers(t *testing.T, root string) map[string]string {
	t.Helper()
	dir := filepath.Join(root, "adrs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading adrs/: %v", err)
	}
	out := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name == "README.md" {
			continue
		}
		m := adrFilename.FindStringSubmatch(name)
		if m == nil {
			t.Errorf("adrs/%s does not match NNN-slug.md, so no citation can reach it", name)
			continue
		}
		if prev, dup := out[m[1]]; dup {
			t.Errorf("adr-%s is claimed by both %s and %s; every citation of it is ambiguous", m[1], prev, name)
			continue
		}
		out[m[1]] = name
	}
	if len(out) == 0 {
		t.Fatal("adrs/ holds no records; this assertion would pass vacuously")
	}
	return out
}

// walkRepo visits every text file git would track, skipping .git and the ADR
// tree's own cross-references (which adrNumbers already covers by filename).
func walkRepo(t *testing.T, root string, visit func(path string, body []byte)) {
	t.Helper()
	skipDir := map[string]bool{".git": true, "node_modules": true}
	textExt := map[string]bool{
		".go": true, ".md": true, ".yml": true, ".yaml": true, ".sql": true,
		".js": true, ".html": true, ".css": true, ".json": true, ".sh": true,
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !textExt[filepath.Ext(name)] && name != "Makefile" && name != "Dockerfile" {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		visit(path, body)
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %v", err)
	}
}

// elide keeps a citation site readable in a failure message. A prose line in
// docs/ can run to a full paragraph, and printing forty of them buries the
// list of numbers the reader actually needs.
func elide(line string) string {
	const max = 110
	if len(line) <= max {
		return line
	}
	return line[:max] + "…"
}

// moduleRoot walks up from this package until it finds the go.mod that owns
// it. Walking beats counting "../.." because these assertions are about a
// package that can move, and a relative hop that has to be edited alongside
// the move is one more way for them to go quiet.
func moduleRoot(t *testing.T) (dir, modulePath string) {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("locating this package: %v", err)
	}
	for {
		raw, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil {
			for _, line := range strings.Split(string(raw), "\n") {
				if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
					return dir, strings.TrimSpace(rest)
				}
			}
			t.Fatalf("%s declares no module path", filepath.Join(dir, "go.mod"))
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above this package")
		}
		dir = parent
	}
}
