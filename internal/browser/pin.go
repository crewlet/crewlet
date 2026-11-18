package browser

// The pinned Playwright tree for a seat's own browser
// (scripts/playwright-mcp-pin.env), which internal/browser/boxhelper embeds,
// and the Chromium build its playwright-core expects (its browsers.json).
// Here rather than in boxhelper, so a package that reads them links no
// tarball (the package doc); boxhelper's TestTheEmbeddedTreeIsThePinnedRelease
// holds all four to the pin file and the embedded tarballs.
const (
	PlaywrightMCPRelease     = "0.0.86"
	PlaywrightCoreRelease    = "1.66.0-alpha-2026-11-18"
	ExpectedChromiumRevision = "1255"
	ExpectedChromiumVersion  = "159.0.8154.6"
)
