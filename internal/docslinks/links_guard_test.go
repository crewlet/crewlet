// Package docslinks_test guards the documentation links carried by the prose
// this repository publishes.
//
// It has no non-test source because it certifies content rather than code:
// the docs tree and the skill files are shipped artefacts, and this is the
// check that says so.
//
// WHY IT DOES NOT LIVE IN skills/
//
// It did, next to the file that prompted it, until that turned out to be the
// one place it could not go. The docs site copies schema/ and skills/ into its
// static output verbatim and serves every file in them over HTTP — so this
// test was published at /next/skills/links_guard_test.go, 200, as
// text/markdown, with the slashless URL in its own documentation. A guard
// against publishing redirecting links was publishing one. internal/ is not
// copied, so nothing here is served.
package docslinks_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// docsOrigin is where the documentation is published, and therefore both what
// makes a link this check's business and what it prints when reporting one.
const docsOrigin = "https://docs.crewlet.ai"

// docsLink matches an absolute link into the documentation site and captures
// the path, which is what decides the verdict below.
//
// The character class stops at '#', ')' and whitespace, so a fragment or a
// closing markdown paren never lands inside the capture. That is what makes
// the check uniform: /concepts/turn-engine and /concepts/turn-engine#workers
// carry the same path, so they get the same answer, and the fix for the
// second one is a slash before the '#' rather than at the end.
//
// A '.' IS in the class, because a file has an extension — which is why the
// verdict below has to tell an extension from a full stop.
var docsLink = regexp.MustCompile(regexp.QuoteMeta(docsOrigin) + `(/[A-Za-z0-9/._-]*)`)

// publishedTrees are the directories served at docs.crewlet.ai. The docs site
// syncs docs/ into its pages and copies skills/ through verbatim beside them,
// so a link written in either one is handed to a reader, to an assistant, and
// to a crawler exactly as typed.
//
// EVERY file in them is walked, not just the markdown. The site copies these
// trees wholesale — it does not filter by extension — so whatever is in them
// is served, and a check that only read .md would be blind to anything else
// somebody parks here. It was: this very file sat in skills/ and shipped.
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
			if entry.IsDir() {
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
					if page, missing := pageMissingSlash(match[1]); missing {
						offences = append(offences, fmt.Sprintf(
							"%s:%d: %s%s", relative, number+1, docsOrigin, page))
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

// pageMissingSlash reports whether a docs.crewlet.ai path names a page spelled
// without its trailing slash, and returns the path with any sentence
// punctuation removed so the message names the URL rather than the prose
// around it. See the exemptions on the test above.
func pageMissingSlash(path string) (string, bool) {
	// A bare URL at the end of a sentence carries the full stop into the
	// match, and a dot is exactly how a file is recognised below — so left
	// alone, "See https://docs.crewlet.ai/concepts/tool-skills." would read as
	// a file and be exempted, which is the one spelling this check most needs
	// to catch. Only a TRAILING run is punctuation; a dot inside the segment
	// is an extension, so company.schema.json is still a file either way.
	path = strings.TrimRight(path, ".")

	last := path[strings.LastIndex(path, "/")+1:]
	if last == "" {
		return path, false // already correct, or the bare origin
	}
	// A dot means a file, which is served as itself.
	return path, !strings.Contains(last, ".")
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
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("expected the module root at %s: %v", root, err)
	}
	return root
}

// TestPageMissingSlashReadsPunctuationAsProse pins the cases that decide the
// walk above, and in particular the one a review caught: a bare URL ending a
// sentence used to be read as a file and exempted, so the guard stayed green
// on exactly the spelling it exists to find.
func TestPageMissingSlashReadsPunctuationAsProse(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name    string
		path    string
		want    string
		missing bool
	}{
		{"a page", "/concepts/tool-skills", "/concepts/tool-skills", true},
		{"a page ending a sentence", "/concepts/tool-skills.", "/concepts/tool-skills", true},
		{"a page ending a thought", "/concepts/tool-skills...", "/concepts/tool-skills", true},
		{"a page already correct", "/concepts/tool-skills/", "/concepts/tool-skills/", false},
		{"a file", "/schema/company.schema.json", "/schema/company.schema.json", false},
		{
			"a file ending a sentence",
			"/schema/company.schema.json.",
			"/schema/company.schema.json",
			false,
		},
		{"the bare origin", "/", "/", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			path, missing := pageMissingSlash(testCase.path)
			if path != testCase.want || missing != testCase.missing {
				t.Errorf("pageMissingSlash(%q) = (%q, %v), want (%q, %v)",
					testCase.path, path, missing, testCase.want, testCase.missing)
			}
		})
	}
}
