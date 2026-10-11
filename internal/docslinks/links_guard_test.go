// Package docslinks_test guards every link into the documentation site that
// this repository carries: in the docs and the skill it publishes, in the
// dashboard a person clicks through, in the schemas an editor resolves, and
// in the code and the process files beside them.
//
// It has no non-test source because it certifies content rather than code:
// the links are shipped artefacts, and this is the check that says so.
//
// # Why it does not live in skills/
//
// It did, next to the file that prompted it, until that turned out to be the
// one place it could not go. The docs site copies schema/ and skills/ into its
// static output verbatim and serves every file in them over HTTP — so this
// test was published under the site's skills path for main, 200, as
// text/markdown, with the slashless URL in its own documentation. A guard
// against publishing redirecting links was publishing one. internal/ is not
// copied, so nothing here is served.
//
// # What the site builds
//
// Each product lives under its own path and a version segment:
//
//	/engine/latest/   the engine's newest release
//	/engine/next/     main, unreleased
//	/engine/vX.Y/     an older release line
//
// and the same three under /operator/. docs/<page>.md is published at
// /engine/<version>/<page>/, docs/index.md at /engine/<version>/, and the raw
// schema/ and skills/ trees beside the pages, file for file. Nothing is built
// anywhere else, and nothing forwards a path the site used to serve: a link
// outside that shape is a 404 however it once resolved.
package docslinks_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// docsOrigin is where the documentation is published, and therefore both what
// makes a link this check's business and what it prints when reporting one.
const docsOrigin = "https://docs.crewlet.ai"

// docsLink matches an absolute link into the documentation site and captures
// the path, which is what decides the verdicts below.
//
// The character class stops at '#', ')' and whitespace, so a fragment or a
// closing markdown paren never lands inside the capture. That is what makes
// the check uniform: a page with and without a #fragment carries the same
// path, so they get the same answer, and the fix for the second one is a
// slash before the '#' rather than at the end.
//
// A '.' IS in the class, because a file has an extension — which is why the
// verdict below has to tell an extension from a full stop.
var docsLink = regexp.MustCompile(regexp.QuoteMeta(docsOrigin) + `(/[A-Za-z0-9/._-]*)`)

// buildOutput are the top-level directories .gitignore names as build output:
// goreleaser's archives and the notices file its hook generates, on disk in a
// checkout that has run a release build and nobody's prose.
var buildOutput = map[string]bool{"dist": true, "build": true}

// binaryProbe is how much of a file is read to decide it is not text — git's
// own figure for the same decision: a NUL byte in the first eight thousand
// says binary.
const binaryProbe = 8000

// link is one docs.crewlet.ai URL found in the tree.
type link struct {
	file string // relative to the module root
	line int
	path string // what docsLink captured
}

// treeLinks is every link into the site this tree carries, read once for both
// tests: the walk is the whole cost of either.
var treeLinks = sync.OnceValues(func() ([]link, error) {
	root, err := sourcetree.ModuleRoot()
	if err != nil {
		return nil, err
	}
	return walkLinks(root)
})

// walkLinks reads every text file under root and returns the links in it.
//
// THE WHOLE TREE rather than the directories the site publishes. It read only
// docs/ and skills/ once, and the dashboard's Learn more and retention links
// sat outside that, slashless, for as long as the guard was green — a link a
// person clicks in the product is handed out exactly as surely as one in a
// page. The schemas' $id, the Go that stamps it and the workflow comments are
// all read the same way, so there is no list of places to forget to extend.
func walkLinks(root string) ([]link, error) {
	var found []link
	files := 0
	err := sourcetree.Walk(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
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
		if !bytes.Contains(body, []byte(docsOrigin)) {
			return nil
		}
		for number, line := range strings.Split(string(body), "\n") {
			for _, match := range docsLink.FindAllStringSubmatchIndex(line, -1) {
				// A PREFIX, not a link: a template literal goes on to
				// interpolate the rest, so what was captured is only the
				// part of an address the code composes, and whoever
				// composes it holds the whole (the dashboard's Learn more
				// is held to docs/integrations/ by its own suite).
				if strings.HasPrefix(line[match[1]:], "${") {
					continue
				}
				found = append(found, link{
					file: relative,
					line: number + 1,
					path: line[match[2]:match[3]],
				})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// A WALK THAT READ ALMOST NOTHING certified almost nothing — a root
	// resolved to the wrong directory finds no link and passes. The tree
	// holds thousands of text files; a few hundred is a floor no real
	// checkout comes near.
	if files < 500 {
		return nil, fmt.Errorf("read %d text files under %s — this guard was "+
			"certifying almost nothing", files, root)
	}
	return found, nil
}

// readText reads a file, and reports false for one that is binary by git's own
// test: a NUL byte in its first [binaryProbe] bytes.
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

// loadLinks is [treeLinks] for a test, which also fails when the tree
// carries too few links for the verdicts to have been exercised: the schemas,
// the skill and the dashboard hold more than this between them.
func loadLinks(t *testing.T) []link {
	t.Helper()
	found, err := treeLinks()
	if err != nil {
		t.Fatal(err)
	}
	if len(found) < 15 {
		t.Fatalf("found %d links to %s in the tree — this guard was certifying "+
			"almost nothing", len(found), docsOrigin)
	}
	return found
}

// TestDocumentationLinksCarryTheTrailingSlash fails the build when the tree
// links a documentation PAGE without its trailing slash.
//
// docs.crewlet.ai is built with Astro's trailingSlash: 'always' behind
// Cloudflare Pages, so every page there is a directory and the slashless
// spelling is a 308 to the slashed one. It resolves, which is exactly why
// eight of them sat in company-architect/SKILL.md unnoticed, and it is still
// the wrong URL to hand out.
//
// It costs more than a redirect. The skill is read by an assistant that
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
// A final path segment containing a dot names a FILE, not a page: a schema is
// served as itself and a trailing slash there would be wrong — it is also the
// URL tools resolve $ref against and the one crewlet schema stamps as $id, so
// appending a slash would break editor autocomplete rather than tidy it. A
// path already ending in '/' is the correct spelling and passes. The bare
// origin has no path to judge.
func TestDocumentationLinksCarryTheTrailingSlash(t *testing.T) {
	t.Parallel()

	var offences []string
	for _, found := range loadLinks(t) {
		if page, missing := pageMissingSlash(found.path); missing {
			offences = append(offences, fmt.Sprintf(
				"%s:%d: %s%s", found.file, found.line, docsOrigin, page))
		}
	}
	if len(offences) > 0 {
		t.Errorf("these link documentation pages without the trailing slash, "+
			"so each one is served as a redirect rather than as the page it "+
			"reaches:\n\t%s",
			strings.Join(offences, "\n\t"))
	}
}

// pageMissingSlash reports whether a docs.crewlet.ai path names a page spelled
// without its trailing slash, and returns the path with any sentence
// punctuation removed so the message names the URL rather than the prose
// around it. See the exemptions on the test above.
func pageMissingSlash(path string) (string, bool) {
	path = withoutFullStop(path)

	last := path[strings.LastIndex(path, "/")+1:]
	if last == "" {
		return path, false // already correct, or the bare origin
	}
	// A dot means a file, which is served as itself.
	return path, !strings.Contains(last, ".")
}

// withoutFullStop removes the sentence punctuation a bare URL ending a
// sentence carries into the match.
//
// A dot is exactly how a file is recognised, so left alone a page ending a
// sentence would read as a file and be exempted, which is the one spelling
// the slash check most needs to catch. Only a TRAILING run is punctuation; a
// dot inside the segment is an extension, so company.schema.json is still a
// file either way.
func withoutFullStop(path string) string {
	return strings.TrimRight(path, ".")
}

// TestPageMissingSlashReadsPunctuationAsProse pins the cases that decide the
// slash check, and in particular the one a review caught: a bare URL ending a
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
		{"a page", "/engine/latest/concepts/tool-skills", "/engine/latest/concepts/tool-skills", true},
		{"a page ending a sentence", "/engine/latest/concepts/tool-skills.", "/engine/latest/concepts/tool-skills", true},
		{"a page ending a thought", "/engine/latest/concepts/tool-skills...", "/engine/latest/concepts/tool-skills", true},
		{"a page already correct", "/engine/latest/concepts/tool-skills/", "/engine/latest/concepts/tool-skills/", false},
		{"a file", "/engine/latest/schema/company.schema.json", "/engine/latest/schema/company.schema.json", false},
		{
			"a file ending a sentence",
			"/engine/latest/schema/company.schema.json.",
			"/engine/latest/schema/company.schema.json",
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

// TestDocumentationLinksNameAPublishedPath fails the build when the tree links
// a path the documentation site does not build.
//
// The site used to serve the engine at its root, and when it moved under
// /engine/ nothing forwarded the old paths: a root-shaped link is a 404, and
// so is a page or a schema somebody renamed under a link that still names
// it. Both look perfectly plausible in review, and neither has any symptom
// short of a reader following it.
//
// Under /engine/latest/ and /engine/next/ the page must exist in this tree's
// docs/ (or the file in schema/ or skills/). For next/ that is exactly what
// is built. For latest/ the check rests on two rules the site keeps, and is
// only as good as they are:
//
//   - latest/ is main or a release of this tree, never another program.
//     Until this engine's first release it is a redirect to next/; from then
//     it is the newest release, cut from this tree. The one tag older than
//     that, v0.1.0, predates this engine, and the site publishes it at
//     /engine/v0.1/ and never at latest/.
//   - what the release lacks, latest/ redirects to next/: a page or a
//     schema/ or skills/ file only main has answers at its latest/ path with
//     a redirect to its next/ copy.
//
// So a link to a page or file this tree has works from the day it merges,
// at main's copy or a release's. What this cannot see is a release's copy of
// a page that main has since changed — a heading a #fragment names, say — so
// a fragment is not checked here. A link to an older line (/engine/vX.Y/) or
// into the operator's tree cannot be checked from here and is held to its
// shape alone.
func TestDocumentationLinksNameAPublishedPath(t *testing.T) {
	t.Parallel()

	root := sourcetree.Root(t)
	var offences []string
	for _, found := range loadLinks(t) {
		if problem := unpublished(root, found.path); problem != "" {
			offences = append(offences, fmt.Sprintf("%s:%d: %s%s — %s",
				found.file, found.line, docsOrigin, withoutFullStop(found.path), problem))
		}
	}
	if len(offences) > 0 {
		t.Errorf("these link paths the documentation site does not build, so "+
			"each one is a 404 (the engine's pages are /engine/latest/<page>/ "+
			"for docs/<page>.md, its schemas and skills "+
			"/engine/latest/schema/<file> and /engine/latest/skills/<file>):\n\t%s",
			strings.Join(offences, "\n\t"))
	}
}

// versioned is a path under a product and a version segment: the only paths
// the site builds besides its own home.
var versioned = regexp.MustCompile(`^/(engine|operator)/(latest|next|v[0-9]+\.[0-9]+)/(.*)$`)

// rawTrees are the engine's directories the site publishes file for file
// beside its pages.
var rawTrees = []string{"schema/", "skills/"}

// unpublished returns why a docs.crewlet.ai path names nothing the site
// builds, or "" when it names something. root is the module root, whose
// docs/, schema/ and skills/ are what the engine's versions are built from.
func unpublished(root, path string) string {
	path = withoutFullStop(path)
	if path == "" || path == "/" {
		return "" // the site's home
	}
	parts := versioned.FindStringSubmatch(path)
	if parts == nil {
		return "not under /engine/ or /operator/ and a version (latest, next or " +
			"vX.Y), where the site builds nothing"
	}
	product, version, rest := parts[1], parts[2], parts[3]
	if product != "engine" || strings.HasPrefix(version, "v") {
		return "" // a tree that is not this one
	}

	for _, tree := range rawTrees {
		if strings.HasPrefix(rest, tree) {
			if !isFile(filepath.Join(root, filepath.FromSlash(rest))) {
				return "names no file in " + strings.TrimSuffix(tree, "/") + "/"
			}
			return ""
		}
	}

	page := strings.TrimSuffix(rest, "/")
	if page == "" {
		page = "index"
	}
	last := page[strings.LastIndex(page, "/")+1:]
	if strings.Contains(last, ".") {
		// A file the pages carry beside them, such as a figure.
		if !isFile(filepath.Join(root, "docs", filepath.FromSlash(page))) {
			return "names no file under docs/"
		}
		return ""
	}
	if !isFile(filepath.Join(root, "docs", filepath.FromSlash(page)+".md")) {
		return "names no page: docs/" + page + ".md does not exist"
	}
	return ""
}

// isFile reports whether path is a regular file.
func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// TestUnpublishedKnowsWhatTheSiteBuilds pins the verdicts against this tree's
// own files, the old root-mounted shapes among them: each one answered
// before the move and is a 404 now.
func TestUnpublishedKnowsWhatTheSiteBuilds(t *testing.T) {
	t.Parallel()

	root := sourcetree.Root(t)
	for _, testCase := range []struct {
		path      string
		published bool
	}{
		{"", true},
		{"/", true},
		{"/engine/latest/", true},
		{"/engine/next/", true},
		{"/engine/latest/concepts/overview/", true},
		{"/engine/next/concepts/overview/", true},
		{"/engine/latest/concepts/overview/.", true},
		{"/engine/latest/schema/company.schema.json", true},
		{"/engine/next/schema/bootstrap.schema.json", true},
		{"/engine/latest/skills/company-architect/SKILL.md", true},
		{"/engine/v0.1/concepts/anything/", true},
		{"/operator/latest/anything/", true},
		{"/operator/next/", true},

		{"/concepts/overview/", false},
		{"/schema/company.schema.json", false},
		{"/next/concepts/overview/", false},
		{"/v0.1/concepts/overview/", false},
		{"/engine/", false},
		{"/engine/concepts/overview/", false},
		{"/operator/", false},
		{"/operator/concepts/", false},
		{"/engine/latest/concepts/no-such-page/", false},
		{"/engine/latest/schema/no-such.schema.json", false},
		{"/engine/latest/schema/", false},
		{"/engine/latest/skills/no-such/SKILL.md", false},
		{"/engine/latest/assets/no-such.png", false},
	} {
		t.Run(testCase.path, func(t *testing.T) {
			t.Parallel()
			problem := unpublished(root, testCase.path)
			if (problem == "") != testCase.published {
				t.Errorf("unpublished(%q) = %q, want published=%v",
					testCase.path, problem, testCase.published)
			}
		})
	}
}
