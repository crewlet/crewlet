// Package crewlet embeds what the binary carries from the repository's own
// documentation: the API reference, which the Dashboard Assistant reads a
// route's documentation from (internal/assistant/catalog's describe_api), and
// the whole documentation with the decision records, which it searches and
// reads to explain the engine's own behaviour (internal/enginedocs).
//
// AT THE MODULE ROOT, because an embed directive reaches only files beneath the
// package that states it, and docs/ is published to the documentation site
// whole: a Go file inside it would be served there as a page asset. Embedded
// rather than copied, so the reference the model reads is the page a person
// reads, byte for byte, and a walk in the catalogue holds it to the route
// table this build mounts.
package crewlet

import (
	"embed"
	"sync"
)

// APIReference is docs/reference/api-endpoints.md as this build ships it, read
// from [Docs] once: the page is part of the tree, so it is embedded once.
var APIReference = sync.OnceValue(func() string {
	page, err := Docs.ReadFile("docs/reference/api-endpoints.md")
	if err != nil {
		// THE DIRECTIVE BELOW EMBEDS docs/reference WHOLE, so the page is in
		// every build that compiled; a build without it is a broken tree.
		panic("crewlet: the embedded documentation holds no API reference: " + err.Error())
	}
	return string(page)
})

// Docs is the documentation this build ships, docs/ and the decision records
// under adr/, by their paths in the tree: every page, and none of the images
// beside them, which no reader of the text needs.
//
//go:embed docs/*.md docs/concepts docs/getting-started docs/guides docs/integrations
//go:embed docs/reference adr/*.md
var Docs embed.FS
