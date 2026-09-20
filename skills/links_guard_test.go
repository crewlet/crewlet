// Package skills_test guards the documentation links carried by the prose
// this repository publishes.
//
// It has no non-test source because it certifies content rather than code:
// the skill files beside it and the docs tree they point into are shipped
// artefacts, and this is the check that says so.
package skills_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// docsLink matches an absolute link into the documentation site and captures
// the path, which is what decides the verdict below.
//
// The character class stops at '#', ')' and whitespace, so a fragment or a
// closing markdown paren never lands inside the capture. That is what makes
// the check uniform: /concepts/turn-engine and /concepts/turn-engine#workers
// carry the same path, so they get the same answer, and the fix for the
// second one is a slash before the '#' rather than at the end.
var docsLink = regexp.MustCompile(`https://docs\.crewlet\.ai(/[A-Za-z0-9/._-]*)`)

// publishedTrees are the directories whose markdown is served at
// docs.crewlet.ai. The docs site syncs docs/ into its pages and copies
// skills/ through verbatim beside them, so a link written in either one is
// handed to a reader, to an assistant, and to a crawler exactly as typed.
var publishedTrees = []string{"docs", "skills"}

// TestDocumentationLinksCarryTheTrailingSlash fails the build when published
// prose links a documentation PAGE without its trailing slash.
//
// docs.crewlet.ai is built with Astro's trailingSlash: 'always' behind
// Cloudflare Pages, so every page there is a directory:
// /concepts/tool-skills is a 308 to /concepts/tool-skills/. The slashless
// spelling resolves, which is exactly why eight of them sat in
// company-architect/SKILL.md unnoticed, and it is still the wrong URL to
// hand out.
//
// It costs more than a redirect. This prose is read by an assistant that
// fetches precisely what it is given, so each link is a wasted round trip on
// every fetch. And it is published: the docs site serves skills/ alongside
// the pages themselves, as text/markdown with no noindex, so a crawler
// reading it harvests these URLs and files each one under "page with
// redirect" instead of indexing the page it reaches. Two of them — the
// quickstart and the turn engine — were reported that way in Google Search
// Console before this check existed.
//
// # What is exempt, and why
//
// A final path segment containing a dot names a FILE, not a page:
// /schema/company.schema.json is served as itself and a trailing slash there
// would be wrong — it is also the URL tools resolve $ref against and the one
// crewlet schema stamps as $id, so appending a slash would break editor
// autocomplete rather than tidy it. A path already ending in '/' is the
// correct spelling and passes. The bare origin has no path to judge.
func TestDocumentationLinksCarryTheTrailingSlash(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	var offences []string

	for _, tree := range publishedTrees {
		dir := filepath.Join(root, tree)
		if _, err := os.Stat(dir); err != nil {
			// Every tree here is a shipped artefact. One that is missing is a
			// broken checkout or a rename nobody swept, and both are findings
			// — skipping would hide the only event this check can respond to.
			t.Fatalf("expected the published tree %s at %s: %v", tree, dir, err)
		}

		err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
				return nil
			}

			source, err := os.ReadFile(path) //nolint:gosec // a path this walk produced
			if err != nil {
				return err
			}

			relative, err := filepath.Rel(root, path)
			if err != nil {
				relative = path
			}

			for number, line := range strings.Split(string(source), "\n") {
				for _, match := range docsLink.FindAllStringSubmatch(line, -1) {
					if pageNeedsSlash(match[1]) {
						offences = append(offences, fmt.Sprintf(
							"%s:%d: %s", relative, number+1, match[0]))
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", dir, err)
		}
	}

	if len(offences) > 0 {
		t.Errorf("published prose links documentation pages without the "+
			"trailing slash, so each one is served as a redirect rather than "+
			"as the page it reaches:\n\t%s",
			strings.Join(offences, "\n\t"))
	}
}

// pageNeedsSlash reports whether a docs.crewlet.ai path names a page that is
// spelled without its trailing slash. See the exemptions on the test above.
func pageNeedsSlash(path string) bool {
	last := path[strings.LastIndex(path, "/")+1:]
	if last == "" {
		return false // already correct, or the bare origin
	}
	return !strings.Contains(last, ".") // a dot means a file, which is served as itself
}

// repoRoot is the module root, found from this file rather than from the
// working directory so the walk above covers the same trees however the
// suite is invoked.
func repoRoot(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's own source file")
	}
	root := filepath.Dir(filepath.Dir(file))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("expected the module root at %s: %v", root, err)
	}
	return root
}
