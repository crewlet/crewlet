package docslinks_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// EVERY PUBLISHED PAGE IS REACHABLE FROM THE INDEX.
//
// `docs/index.md` is the documentation site's NAVIGATION SOURCE, so a page
// nothing links to is a page the site does not carry: the file is in the
// repository, the prose is written, and no reader has a path to it. That
// failure has no symptom here — the tree builds, the links that do exist all
// resolve, and the page simply is not there — which is why it needs a check
// rather than a convention.
//
// REACHABILITY, not "linked from somewhere". Two orphan pages that link to
// each other satisfy the weaker rule and are just as unreachable, and a
// section moved out of the index takes its whole subtree with it in exactly
// that shape.
func TestEveryDocsPageIsReachableFromTheIndex(t *testing.T) {
	t.Parallel()
	root := filepath.Join(repoRoot(t), "docs")

	reached := map[string]bool{"index.md": true}
	queue := []string{"index.md"}
	for len(queue) > 0 {
		page := queue[0]
		queue = queue[1:]
		for _, link := range pageLinks(t, root, page) {
			if reached[link] {
				continue
			}
			reached[link] = true
			queue = append(queue, link)
		}
	}

	var orphans []string
	unusedExemptions := map[string]bool{}
	for page := range notNavigation {
		unusedExemptions[page] = true
	}
	for _, page := range docsPages(t, root) {
		if reached[page] {
			delete(unusedExemptions, page)
			continue
		}
		if _, exempt := notNavigation[page]; exempt {
			delete(unusedExemptions, page)
			continue
		}
		orphans = append(orphans, page)
	}
	if len(orphans) > 0 {
		sort.Strings(orphans)
		t.Errorf("no path from docs/index.md reaches these pages, so the site "+
			"does not carry them: %v\nLink each one from index.md, or from a page "+
			"the index already reaches. If a page is genuinely not navigation, "+
			"add it to notNavigation with the reason.", orphans)
	}

	// TWO-SIDED, so an exemption that stopped being needed fails rather
	// than sitting there. A stale entry is how the next orphan under the
	// same path slips through unnoticed.
	for page := range unusedExemptions {
		t.Errorf("%s is exempted from the navigation walk and is not in the docs "+
			"tree — drop the entry", page)
	}
}

// notNavigation is every markdown file under docs/ that is deliberately NOT a
// page of the site, with the reason. Each entry is a decision, and the walk
// fails on one that is no longer there.
var notNavigation = map[string]string{
	"assets/README.md": "a manifest of the brand files for whoever edits them, " +
		"addressed to this repository rather than to a reader of the site",
}

// TestAnUnreachablePageIsDetected is the control. Without it the walk above
// passes on a tree where nothing is reachable at all — a broken reader would
// report every page as fine by finding none to check.
func TestAnUnreachablePageIsDetected(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("index.md", "# Index\n\n- [Linked](guides/linked.md)\n")
	write("guides/linked.md", "# Linked\n\n- [Deeper](../concepts/deeper.md)\n")
	write("concepts/deeper.md", "# Deeper\n")
	write("concepts/orphan.md", "# Orphan\n")
	// TWO ORPHANS THAT LINK TO EACH OTHER, which is the pair a
	// "linked from somewhere" rule would pass and a reachability walk
	// must not.
	write("concepts/pair-a.md", "# A\n\n- [B](pair-b.md)\n")
	write("concepts/pair-b.md", "# B\n\n- [A](pair-a.md)\n")

	reached := map[string]bool{"index.md": true}
	queue := []string{"index.md"}
	for len(queue) > 0 {
		page := queue[0]
		queue = queue[1:]
		for _, link := range pageLinks(t, root, page) {
			if !reached[link] {
				reached[link] = true
				queue = append(queue, link)
			}
		}
	}
	var orphans []string
	for _, page := range docsPages(t, root) {
		if !reached[page] {
			orphans = append(orphans, page)
		}
	}
	sort.Strings(orphans)
	want := []string{"concepts/orphan.md", "concepts/pair-a.md", "concepts/pair-b.md"}
	if strings.Join(orphans, ",") != strings.Join(want, ",") {
		t.Errorf("the walk found %v, want %v — it must reach a page through "+
			"another page and must not count a cycle among orphans as reached",
			orphans, want)
	}
}

// docsPages is every markdown page under the tree, slash-separated and
// relative to it.
func docsPages(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := sourcetree.Walk(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk the docs tree: %v", err)
	}
	return out
}

// relativeMarkdownLink captures the target of a markdown link that points at
// another page in this tree.
//
// RELATIVE ONLY. An absolute link to the published site is the other guard's
// business, and one to any other host reaches nothing here.
var relativeMarkdownLink = regexp.MustCompile(`\]\(([^)#\s]+\.md)(?:#[^)\s]*)?\)`)

// pageLinks is every page this one links to, as a path relative to the tree's
// root.
//
// THREE KINDS ARE DROPPED RATHER THAN REPORTED, because this walk answers
// reachability and the other guard is what judges a link's shape: one that
// carries a scheme (a `.md` on GitHub is not a page of this site), one that
// climbs out of the tree, and one whose file is not there. A missing target
// is a broken link rather than an orphan, and reporting it here would put two
// different faults under one message.
func pageLinks(t *testing.T, root, page string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(page)))
	if err != nil {
		t.Fatalf("read %s: %v", page, err)
	}
	dir := filepath.Dir(page)
	var out []string
	for _, match := range relativeMarkdownLink.FindAllStringSubmatch(string(raw), -1) {
		if strings.Contains(match[1], "://") {
			continue
		}
		target := filepath.ToSlash(filepath.Clean(filepath.Join(dir, match[1])))
		if strings.HasPrefix(target, "..") {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(target))); err != nil {
			continue
		}
		out = append(out, target)
	}
	return out
}
