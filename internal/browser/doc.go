// Package browser holds what the engine decides about Chromium that more than
// one package reads: the oldest major the engine accepts and the suite
// certifies ([MinChromiumMajor]), the Playwright release a seat's box installs
// and the Chromium build it expects ([PlaywrightMCPRelease],
// [ExpectedChromiumVersion]), the closed values of a box's ensure answer
// ([EnsureReason] and its fellows), and that no Go but the artifact render's
// client speaks DevTools (ADR-0165).
//
// # The floor
//
// [MinChromiumMajor] is the oldest Chromium major the engine accepts — the
// artifact render's DevTools client refuses an older browser, naming both
// versions — and the oldest the suite certifies: scripts/chromium-pin.env pins
// its newest patch beside Chrome for Testing's Stable, and every case that
// drives Chromium runs on both (chromiumtest.Each), unless its doc says why
// the floor adds nothing to its claim. The box helper's cases pair each pin
// with a Node instead (boxhelpertest.Each): this floor's with the floor Node.
//
// It is Chrome for Testing's first major published for all four platforms the
// engine is released for: 153, its first build for linux-arm64 (Chrome for
// Testing's per-milestone listing, read 2026-11-08 and again 2026-11-10). A
// floor a supported development platform cannot fetch is a gate that platform
// skips, so no older major can be the floor, however little a consumer needs.
// The render's client needs no newer protocol: every command, event and field
// it declares was described by major 142 (its package doc says how that was
// read), so the platform bound sets the floor.
//
// Raising it is argued here with what was measured, and moves the floor pin
// to the newest patch of the new major in the same change:
// TestThePinsMeetTheFloor fails while the two disagree.
//
// # The Playwright release
//
// A seat's box installs Playwright MCP with npm, from the lock
// internal/browser/boxhelper keeps in tree/. Its two releases
// ([PlaywrightMCPRelease], [PlaywrightCoreRelease]) and the Chromium build that
// playwright-core expects ([ExpectedChromiumRevision],
// [ExpectedChromiumVersion]) are decisions about the seat's browser, so they sit
// here beside the floor, each the release somebody read: boxhelper's package
// doc says how its tests hold them to the lock, and keep a bump of the lock
// alone red until a person has read it and moved these.
//
// # A box's ensure
//
// The box helper's ensure (internal/browser/boxhelper) answers in closed
// values, each a named type with Valid(), so a value off the wire is a value
// rather than a panic: its verdict ([EnsureVerdict]); why a browser is
// withheld and the cause ensure could tell ([EnsureReason], [EnsureCause]),
// every reason with the causes it takes, the box's in the order ensure checks
// them and then the engine's own ([EnsureReason.Printed]); the build it chose
// ([ChromiumBuild]); the cell's sandbox ([ChromiumSandbox], whose zero value
// is neither setting); and how its supervisor was come by
// ([SupervisorStart]). They live here, never in the helper that prints them,
// so a package that reads them, a configuration or an event's record, links
// none of the helper's embedded modules and depends on no helper.
//
// # DevTools only through the artifact render's client
//
// DevTools is full control of a browser — its pages' script, storage and
// network — and a seat's executor holds a shell in its box (ADR-0066), so a
// browser there is the model's to drive. The engine therefore speaks DevTools
// through one client, the artifact render's
// (internal/artifactrender/internal/cdp), which drives a Chromium the operator
// runs, and Go's internal-package rule refuses every importer of it outside
// internal/artifactrender at compile time. So that no second client grows
// beside it, no Go file of the module names a DevTools method but those in
// that client's own directory, none beneath it (ADR-0165).
//
// TestNoGoFileButTheRendersClientNamesADevToolsMethod holds that. It reads
// every comment and every string and rune literal of the module's Go files,
// tests and generated files included and testdata aside; each run of string
// literals one expression joins by + read together, from the syntax tree, so
// that a parenthesised operand continues a run and a call's arguments never
// join the one around the call; and every value again with JSON's \u escapes
// decoded, since a name reaches a browser as JSON; and the string literals,
// not the comments, of every package of another module the module's builds
// and tests link (go list -deps -test), since a third-party DevTools client
// names its methods in strings. It fails on any command or event either
// pinned Chromium's own protocol describes (chromiumtest.Methods): the names
// come from each pin, so the test spells none, and the floor's arm finds the
// names the newest has dropped. It is a tripwire on Go text, not a proof: a
// name built at run time — by Sprintf, Join, a constant joined to a literal,
// or a byte or rune slice — passes it, and so does every file that is not Go.
package browser
