// Package browser holds what the engine decides about Chromium that more than
// one package reads: the oldest major the engine accepts and the suite
// certifies ([MinChromiumMajor]), the Playwright release a seat's box installs
// and the Chromium build it expects ([PlaywrightMCPRelease],
// [ExpectedChromiumVersion]) and whether a box's is older ([Outdated]), the
// closed values of a box's ensure answer ([EnsureReason] and its fellows), the
// name of a seat's browser server ([ServerName]) and where a seat's browser
// may run, and that no Go but the artifact render's client speaks DevTools
// (ADR-0165).
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
// them and then the engine's own ([EnsureReason.Printed]), each pair held to
// one rule ([EnsureReason.Takes]); the build it chose
// ([ChromiumBuild]); the cell's sandbox ([ChromiumSandbox], whose zero value
// is neither setting); and how its supervisor was come by
// ([SupervisorStart]). They live here, never in the helper that prints them,
// so a package that reads them, a configuration or an event's record, links
// none of the helper's embedded modules and depends on no helper.
//
// # A seat's browser
//
// A seat whose role.sandbox sets browser gets a headless Chromium in its
// executor's box, driven by its harness through the box's own MCP server,
// [ServerName], a name config reserves so that no company server takes it.
// Where it may run is recorded as ADR-0181.
//
// The rule names the one cell it admits: every run of the seat lands in a
// container box, off its host's network. A direct box is the engine host
// itself, where a page would reach an agent running as the engine's user
// beside the fleet's keys, and a container on the host's network reaches
// every service listening on the host's loopback, where a page could lead
// the agent. A cell the rule does not name is refused by the same comparison,
// with no branch of its own, so a cell added later is refused until a
// decision admits it. A seat whose entry denies its harness the web
// (web_tools: denied) is refused too: a browser would open the web that
// denial closes.
//
// Config refuses a seat any of whose runs can land elsewhere, by its boxes
// placed as the engine places them. It is an admission rule, since nothing
// that runs depends on it, so a revision another build admitted still
// applies, with a warning.
//
// So the launch holds the rule again: a revision another build admitted, or a
// default a reload moved, can still land a run outside a container
// (cell_not_container) or on an entry that denies the web (web_denied), each
// withheld before anything runs in the box. Whether the box is off its host's
// network the launch asks the box itself, before it writes the helper
// (host_network, internal/browser/hostreach): a kept box keeps the network it
// was made on whatever a later revision says, and a runtime's own
// configuration or a virtual machine's design leads a box to the host's
// loopback by no setting config reads. A browser the box already runs, a kept
// box's from a launch whose check passed, is stopped there, so its pages run
// on in no box that may reach the host's loopback.
//
// Nor does a browser go where it would hide the run's own tools. A harness
// that hides every MCP server's tools behind its own search once a run's
// servers list some number of them (Muse Code, whose runner states the number
// it was measured to hide them at) would hide submit_work and ask_and_wait
// with the browser's, which a run needs on every request and a browser cannot
// make up for. So the launch withholds the browser where the run's servers,
// the bridge's and the box's own, list fewer tools than that number and with
// the browser's would reach it (bridge_deferred), counting the browser's from
// the list a box serves (boxhelper.Tools), never a figure of its own, and the
// box's own servers' from the seat's tools, each whole, as a box reaches a
// server. Servers at the number on their own are hidden either way, so their
// run keeps its browser; servers the harness loads of its own, its box home's
// and a repository's, are not counted, since the launch does not know them.
// It reads only the launch's own counts, so it is decided after web_denied
// and before host_network, whose check reads the engine's host and runs in
// the box: no verdict of that check could hand such a run its browser, so it
// pays for none. And, as for the cell and the web, it stops no browser a kept
// box already runs: the run is handed no server, and only host_network finds
// that the box may lead a page to its host's loopback.
//
// Chromium keeps its own sandbox in the box unless the catalogue waives it
// (local.chromium_sandbox). A value this build does not know reads as
// required, the stricter, so this build runs a later build's revision rather
// than refusing it and splitting the fleet's epoch.
//
// Every launch writes the helper and runs ensure, a kept box's too, since the
// engine's build decides the helper and a kept box's supervisor may be another
// build's. Ensure runs in the box's home with the run's environment, the one
// its setup ran with, so the box's npm reaches the mirror, token, proxy and CA
// the run's own npm would; a kept container box holds no environment of its
// own, so the launch hands it. The supervisor keeps its allowlist.
//
// The run is handed the box's browser server only where ensure answered
// ready: a box that cannot run the browser would hand the model a server that
// fails its first call. The server is the shim's plain command line for the
// job, rooted at the box's workspace, with no environment and no secret, the
// same in a fresh session's wiring, and it replaces a server of its name.
// What the box prints is read into this package's closed values and never
// reaches a prompt: the run is told its browser, or why it has none, in the
// engine's words. An engine reason takes a cause only where one can be told.
//
// [Outdated] warns and never withholds: a browser older than the build the
// pin expects is recorded so, since a floor at the patch level would withhold
// every image's browser each time the pin moves.
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
