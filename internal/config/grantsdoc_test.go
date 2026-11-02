package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// EVERY SHIPPED `grants:` THAT LISTS THE PERMISSIONS LISTS EVERY ONE, IN THE
// ORDER iam DECLARES THEM.
//
// A Tier A block somebody copies says beside its `grants:` line what may be
// written there, and the names are the identity estate's ([iam.AllGrants]):
// the API's refusal, the CLI's flag and the schema's description all read
// them off that list. The comments beside the shipped blocks were typed by
// hand, and the quickstart's went on offering `tracker` and `knowledge`, which
// no build accepts, after the configuration reference and the example had
// moved on. So every annotation of a `grants:` line under docs/ or examples/
// (the comment on its line and the comment lines continuing it, and the
// comment block directly above it) that names two permissions or more names
// all of them, joined as strings.Join joins them, in declaration order.
//
// Mutation: put `tracker` back into the quickstart's list and this names the
// file and the line.
func TestEveryShippedGrantsAnnotationNamesEveryPermission(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	names := make([]string, len(iam.AllGrants))
	for i, g := range iam.AllGrants {
		names[i] = string(g)
	}
	want := strings.Join(names, ", ")
	listed := map[string]bool{}
	for _, dir := range []string{"docs", "examples"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			if ext := filepath.Ext(path); ext != ".md" && ext != ".yaml" && ext != ".yml" {
				return nil
			}
			data, err := os.ReadFile(path) //nolint:gosec // a path this repository ships
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			lines := strings.Split(string(data), "\n")
			for i, line := range lines {
				if !grantsLine.MatchString(line) {
					continue
				}
				note := grantsAnnotation(lines, i)
				if namedPermissions(note, names) < 2 {
					continue
				}
				listed[filepath.ToSlash(rel)] = true
				if !strings.Contains(note, want) {
					t.Errorf("%s:%d annotates its grants with %q, want every permission "+
						"in iam.AllGrants' order: %s", rel, i+1, note, want)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// THE BLOCKS THIS IS FOR, so a layout the walk stops recognising fails
	// here rather than certifying nothing.
	for _, file := range []string{"docs/getting-started/quickstart.md",
		"docs/getting-started/configuration.md", "examples/nimbus.config.yaml"} {
		if !listed[file] {
			t.Errorf("%s lists the permissions beside a grants: line, and this walk "+
				"found no such list there", file)
		}
	}
}

// grantsLine is a YAML `grants:` key holding a flow list.
var grantsLine = regexp.MustCompile(`^\s*grants:\s*\[`)

// commentLine is a line holding nothing but a YAML comment.
var commentLine = regexp.MustCompile(`^\s*#`)

// grantsAnnotation is the comment annotating the `grants:` line at i: the
// comment on that line with the comment lines continuing it below, and the
// comment lines directly above it, without the comment marks and backticks
// and with the spaces collapsed.
func grantsAnnotation(lines []string, i int) string {
	var parts []string
	if _, after, ok := strings.Cut(lines[i], "#"); ok {
		parts = append(parts, after)
		for j := i + 1; j < len(lines) && commentLine.MatchString(lines[j]); j++ {
			parts = append(parts, lines[j])
		}
	}
	var above []string
	for j := i - 1; j >= 0 && commentLine.MatchString(lines[j]); j-- {
		above = append([]string{lines[j]}, above...)
	}
	parts = append(parts, above...)
	text := strings.NewReplacer("#", " ", "`", "").Replace(strings.Join(parts, " "))
	return strings.Join(strings.Fields(text), " ")
}

// namedPermissions is how many of names note mentions as a word.
func namedPermissions(note string, names []string) int {
	n := 0
	for _, name := range names {
		if regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`).MatchString(note) {
			n++
		}
	}
	return n
}
