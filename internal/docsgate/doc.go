// Package docsgate holds the repository's prose to the files it points at: every
// relative link in a markdown page names a file that exists, every `#anchor`
// names a heading the target page actually carries, and every page under
// docs/ is reachable from docs/index.md.
//
// # Why prose needs a gate
//
// docs/ is published at docs.crewlet.ai and its index is the site's
// navigation, so a page the index does not list is a page the site cannot
// build, and a link whose target was renamed is a dead link a stranger follows.
// Nothing compiles markdown. Before this package existed, links and anchors in
// this tree were checked by nobody: a heading renamed in one commit broke every
// `page.md#old-heading` elsewhere with no symptom until somebody clicked it, and
// a rename sweep that ended "grep the old name and finish at zero hits" could
// not grep an anchor, because the anchor is a SLUG of the heading rather than
// the heading's own text.
//
// # The slug is GitHub's
//
// An anchor is resolved the way GitHub renders it — the heading's rendered
// text, lower-cased, with every character that is not a letter, a mark, a
// digit, a connector (`_`), a space or a hyphen removed, and each space turned
// into a hyphen; a repeated heading takes `-1`, `-2`, … in document order. That
// rule is what a reader on github.com gets, which is where CONTRIBUTING.md,
// every pull request and every package doc send people. A private
// approximation (collapsing runs of hyphens, say) would pass anchors that are
// dead on the renderer everybody reads this tree on, so the rule is written
// out once and pinned by a table of headings with their known slugs.
//
// # What it reads, and what it deliberately does not
//
// Every markdown page on disk in THIS tree, as [internal/sourcetree] defines
// it — so never VCS metadata, a `node_modules` or a nested checkout under
// `.claude/worktrees/`, whose copies of these pages are at another commit and
// would report its dead links as this tree's — outside build output (`dist/`),
// the committed dashboard bundle and the dashboard's own markdown RENDERING
// FIXTURES (dashboard/src/test/markdown), which are hostile input on purpose.
// On disk rather than tracked, for sourcetree's reason: a page nobody has
// added yet is still the page the commit that adds it publishes. A link
// inside a fenced block or an inline code span is an example rather than a
// link and is not followed. An absolute URL is not fetched: whether a third
// party's page still exists is not a property of this tree, and a gate that
// fails on somebody else's outage is one that gets skipped.
//
// The dashboard's own addresses (`#/work/ENG-42`) are held by the dashboard's
// suite, dashboard/src/app/links.test.ts, against the route resolver itself —
// this package cannot evaluate a route, and a second copy of the route table
// here would be the drift the table's own gate exists to prevent.
//
// The package is test-only: this file is its documentation and the gates are
// its tests.
package docsgate
