package boxhelper

import (
	"encoding/json"
	"strings"

	"github.com/crewlet/crewlet/internal/browser"
)

// configFile is the one document the engine and the helper agree through,
// written beside the modules from this build's constants.
const configFile = "config.json"

// configVersion is config.json's shape. The helper refuses a later one whole
// (common.mjs, CONFIG_VERSION), and both move together.
const configVersion = 1

// The bounds of a box's install of the tree, config.json's install block. A
// cold install takes under a second (0.55-0.71 s, npm 10 and 11), so each
// bounds only a registry that fails, and a case whose subject is a bound
// shortens it in its own config.
const (
	// installTimeoutMS ends an npm that has not installed: a trickle, or an
	// address that drops packets, which npm's own timeouts never end. A cold
	// install takes under a second and npm's own bounds end a silent registry
	// in 42 s, so a minute ends only those.
	installTimeoutMS = 60_000
	// installFetchTimeoutMS is npm's idle bound on a connection gone silent,
	// never on a connect or a trickle: two tries and the wait between them end
	// inside installTimeoutMS (2 × 20 + 2 = 42 s), so a silent registry reads
	// as npm's own registry_unreachable, not timeout.
	installFetchTimeoutMS = 20_000
	// installFetchRetries is one retry: it rides out a reset, a 5xx, a 429 or
	// a cold mirror, and a second would need a third silence inside the bound.
	installFetchRetries = 1
	// installFetchRetryWaitMS is the wait before the retry, npm's least and
	// most wait alike, so it is exactly this whatever factor a box sets: a
	// refused registry fails in about 2.4 s.
	installFetchRetryWaitMS = 2_000
	// stopGraceMS is how long npm has to go after a TERM before its group is
	// killed, the suite's recipe's grace: npm goes at once unless a fetch is
	// outstanding, which it defers its exit to, so the KILL follows that one.
	stopGraceMS = 5_000
)

// config is config.json.
type config struct {
	V int `json:"v"`
	// Helper is the modules' [Version].
	Helper string `json:"helper"`
	// Pin is the @playwright/mcp release the lock installs.
	Pin string `json:"pin"`
	// PinDigest names the lock's two files in the 12 hex digits the tree's
	// name carries ([pinDigest]).
	PinDigest string `json:"pin_digest"`
	// InstalledDigest is the digest of the files a whole install of the lock
	// holds (tree/installed.sha256), which an install must match before it is
	// published.
	InstalledDigest string `json:"installed_digest"`
	// NPMFloor is [MinNPMMajor], the oldest npm a box installs with.
	NPMFloor int `json:"npm_floor"`
	// Install is the install's bounds.
	Install struct {
		TimeoutMS        int `json:"timeout_ms"`
		FetchTimeoutMS   int `json:"fetch_timeout_ms"`
		FetchRetries     int `json:"fetch_retries"`
		FetchRetryWaitMS int `json:"fetch_retry_wait_ms"`
	} `json:"install"`
	StopGraceMS int `json:"stop_grace_ms"`
}

// configJSON is config.json as [Write] puts it into a box, every value a
// constant of this build.
func configJSON() ([]byte, error) {
	c := config{V: configVersion, Helper: Version(), Pin: browser.PlaywrightMCPRelease,
		PinDigest: pinDigest(manifest, lock), InstalledDigest: strings.TrimSuffix(installed, "\n"),
		NPMFloor: MinNPMMajor, StopGraceMS: stopGraceMS}
	c.Install.TimeoutMS, c.Install.FetchTimeoutMS = installTimeoutMS, installFetchTimeoutMS
	c.Install.FetchRetries, c.Install.FetchRetryWaitMS = installFetchRetries, installFetchRetryWaitMS
	body, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}
