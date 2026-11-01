// Package crewlet embeds what the binary carries from the repository's own
// documentation: the API reference, which the Dashboard Assistant reads a
// route's documentation from (internal/assistant/catalog's describe_api).
//
// AT THE MODULE ROOT, because an embed directive reaches only files beneath the
// package that states it, and docs/ is published to the documentation site
// whole: a Go file inside it would be served there as a page asset. Embedded
// rather than copied, so the reference the model reads is the page a person
// reads, byte for byte, and a walk in the catalogue holds it to the route
// table this build mounts.
package crewlet

import _ "embed"

// APIReference is docs/reference/api-endpoints.md as this build ships it.
//
//go:embed docs/reference/api-endpoints.md
var APIReference string
