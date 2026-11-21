package browser

// The Playwright release a seat's box installs, as internal/browser/boxhelper's
// tree/package-lock.json pins it, and the Chromium build its playwright-core
// expects (its browsers.json's chromium entry): literals naming the release
// somebody read, never read from the lock. boxhelper's package doc says why
// ("The lock is the release somebody read"), and its tests hold the releases to
// the lock and the build to the tree the suite installs from it.
const (
	// PlaywrightMCPRelease is the @playwright/mcp release the lock pins.
	PlaywrightMCPRelease = "0.0.86"
	// PlaywrightCoreRelease is the release of playwright and playwright-core
	// that @playwright/mcp pins exactly.
	PlaywrightCoreRelease = "1.66.0-alpha-2026-11-18"
	// ExpectedChromiumRevision is the Chromium build playwright-core expects,
	// by the revision its chromium-<revision> directory is named for.
	ExpectedChromiumRevision = "1255"
	// ExpectedChromiumVersion is that build's Chromium version.
	ExpectedChromiumVersion = "159.0.8154.6"
)
