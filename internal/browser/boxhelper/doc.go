// Package boxhelper holds the Playwright tree for a seat's own browser in its
// box: @playwright/mcp and the playwright-core release it names as an exact
// dependency, each the npm tarball the registry serves, committed under pw/
// and embedded ([Tarballs]).
//
// # Why the tarballs, committed
//
// Embedded, so that what a box receives is the bytes the registry signed,
// from the engine itself: no npm runs in a box, no box reaches the registry,
// and a bump reaches every box with the engine that carries it, never waiting
// on an image rebuilt for it. Committed, so `go build` needs no network and no
// Node, and no build step stands between the registry's bytes and the
// binary's. The `playwright` package @playwright/mcp also names is its test
// runner: the server's entry loads playwright-core's bundle alone, so it is
// not shipped.
//
// # Licences
//
// Both packages are Apache-2.0, and each tarball keeps the licence files
// upstream ships in it, unmodified: package/LICENSE, and in playwright-core's
// also NOTICE, ThirdPartyNotices.txt and lib/*.LICENSE, for the code its
// bundles carry from others. No package the release binary links embeds them
// (go list -deps ./cmd/crewlet), so the notices a release ships, which
// scripts/notices writes for what the binary links, name none of them; a
// change that links this package into the binary makes them part of what a
// release must carry.
//
// # The pin
//
// scripts/playwright-mcp-pin.env pins both tarballs by version and SHA-256,
// and internal/browser holds their releases and the Chromium build
// playwright-core expects, for the packages that read them without linking
// these tarballs. TestTheEmbeddedTreeIsThePinnedRelease holds the embedded
// tarballs, the pin file and those constants to each other. Nothing fetches
// the tree at build or test time: `make playwright-mcp`
// (scripts/playwright-mcp.sh) writes it into pw/ for a bump, refusing a
// tarball that is not the pin's, and .github/workflows/pins.yml's weekly leg
// (scripts/playwright-mcp-pin-check.sh) holds the pin to @playwright/mcp's
// latest release, proposing a bump only over tarballs whose integrity npm's
// registry key signed.
package boxhelper
