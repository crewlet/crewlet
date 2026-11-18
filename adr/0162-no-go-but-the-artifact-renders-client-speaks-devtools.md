# ADR-0162: No Go but the artifact render's client speaks DevTools

- **Status:** accepted
- **Authority:** `internal/browser`
- **Enforced-by:** `internal/browser.TestNoGoFileButTheRendersClientNamesADevToolsMethod`, and Go's internal-package rule, which makes an import of `internal/artifactrender/internal/cdp` from outside `internal/artifactrender` a compile error
- **Measured:** on 2026-11-18 the walk read 7,885 Go files of the module, 4,512 of them tests, besides the client's 31, and the string literals of 484 packages of other modules that the module's builds and tests link, against the 909 commands and events the newest pin's protocol describes and the 897 the floor's does, and found none outside the client
- **Tag-status:** unreleased

## The decision

**The engine speaks the Chrome DevTools Protocol through one client, the
artifact render's, and no other Go names a DevTools method.** The client sits
at `internal/artifactrender/internal/cdp`, so Go's internal-package rule
refuses every importer outside `internal/artifactrender`. Every other Go file
of the module, tests included, and every string literal of a package of
another module that the module links, names no command or event either pinned
Chromium's own protocol describes.

## Why the obvious alternatives are wrong

**A shared DevTools client for any browser the engine meets.** DevTools is
full control of a browser, and a seat's executor holds a shell in its box
([ADR-0066](0066-every-executor-is-its-coding-clis-own-run.md)), so a browser
there is the model's to drive, and what it sends is the model's to shape. A
client any package can import is one import away from that path; one reachable
from the render alone keeps it from growing by accident.

**An import guard alone.** It holds the module's own client, and not a
third-party one (chromedp, cdproto, go-rod) imported anywhere, which needs
none of the module's paths. Such a client names its methods in its strings,
which the walk reads.

**A list of method names in the gate.** The list would itself name every
method the gate forbids, and drift from the browsers the suite drives. The
names come from each pin's own protocol instead, the floor's among them, which
still describes names the newest has dropped.

**Matching domain names alone.** The tree already holds a dotted word that is
no method (`"A Page.md#"` in `internal/docsgate`'s test), so the walk matches
whole names the pins describe.

## What this does not decide

What a browser runs that is not Go: the walk reads Go text alone, and a name
built at run time passes it, as `internal/browser`'s doc says. How the render
drives its browser, and what of a page reaches a model, are
`internal/artifactrender`'s.
