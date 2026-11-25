package boxhelper

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/browser/boxhelper/live"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/words"
)

// Settings is what a launch decides of the box's browser beyond this build's
// constants: the cell's Chromium sandbox, resolved. [Write] refuses one whose
// Sandbox is not a valid setting, its zero value among them, rather than read
// it as either.
type Settings struct {
	Sandbox browser.ChromiumSandbox
}

// configFile is the one document the engine and the helper agree through,
// written beside the modules from this build's constants.
const configFile = "config.json"

// configVersion is config.json's shape. The helper refuses a later one whole
// (common.mjs, CONFIG_VERSION), and both move together.
const configVersion = 1

// protocol is the version of the hello on the box's sockets, which the shim
// and the supervisor agree on; it moves only for a change an older peer
// cannot read.
const protocol = 1

// The bounds of a box's install of the tree, config.json's install block. A
// cold install takes under a second (0.55-0.71 s, npm 10 and 11), so each
// bounds only a registry that fails, and a case whose subject is a bound
// shortens it in its own config.
const (
	// installTimeoutMS ends an npm that has not installed: a trickle, or an
	// address that drops packets, which npm's own timeouts never end. A cold
	// install takes under a second and npm's own bounds end a silent registry
	// in 42 s, so a minute ends only those, and leaves ensure room for a
	// Chromium and a supervisor inside its bound.
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
)

// The bounds of ensure and its supervisor, config.json's. A case whose
// subject is a bound shortens it in its own config.
const (
	// ensureDeadlineMS is five seconds under [EnsureTimeout], so ensure
	// prints its line first, since on the container cell the engine's bound
	// ends only the exec client: the five hold an install's KILL, its reap
	// (ensure.mjs's REAP_MS), the removal of what it staged, and the line.
	ensureDeadlineMS = int((EnsureTimeout - 5*time.Second) / time.Millisecond)
	// lockTakeoverMS is five seconds past an ensure's whole bound, so a lock
	// that old is a dead or hung ensure's.
	lockTakeoverMS = int((EnsureTimeout + 5*time.Second) / time.Millisecond)
	// supervisorWaitMS is how long ensure waits for a supervisor's first
	// word: launchTimeoutMS and the supervisor's own start (Node loading the
	// 7 MB core takes about a second in a loaded box), so a hung launch says
	// its own cause first; beside a whole install bound and its grace it still
	// fits under ensureDeadlineMS.
	supervisorWaitMS = 30_000
	// launchTimeoutMS is under supervisorWaitMS, so a launch that hangs says
	// its own cause, timeout. Playwright's default, 180 s, would hold ensure
	// past its wait, and a cold start in a loaded box takes seconds.
	launchTimeoutMS = 25_000
	// relaunchMax relaunches in any relaunchWindowMS, and the browser has
	// crashed out until the box's next ensure. A renderer killed for memory
	// comes back in one cold start; a fourth crash in ten minutes is one the
	// browser meets on every launch (a page, a box's limit), which more cold
	// starts only spend the box on, and a cause that cleared gets its browser
	// back in the next window.
	relaunchMax      = 3
	relaunchWindowMS = 600_000
	// stopGraceMS is how long a process the helper stops has to go before its
	// group is killed: Chromium closes a profile in well under a second, five
	// covers a loaded box, and npm, which defers its exit to an outstanding
	// fetch, is killed by the same grace, the suite's recipe's.
	stopGraceMS = 5_000
	// helloMS is how long a connection to the supervisor has to say its
	// hello, which a peer writes first (the shim at once): two seconds cover a
	// loaded box's scheduling, and a longer silence is ensure's look or a
	// stray.
	helloMS = 2_000
)

// The bounds Playwright MCP serves the model under. Each call's are the pin's
// own defaults, stated, since the arming bound a person's take waits on rests
// on them. The pin keeps every screenshot and download a model leaves
// unnamed and bounds none; at a few MB a full-page shot, 256 MiB keeps dozens
// to hundreds, a small share of a box's disk, the oldest going first.
const (
	mcpActionMS       = 5_000
	mcpNavigationMS   = 60_000
	mcpOutputMaxBytes = 256 << 20
)

// viewportWidth and viewportHeight size the page: 1280×800, the 16:10 laptop
// size sites lay their desktop view out for, 80 px taller than the 1280×720
// a persistent context emulates otherwise, so a screenshot shows more of a
// page; a window's size no longer sizes it.
const viewportWidth, viewportHeight = 1280, 800

// playwrightDisabled is the pinned Playwright's own --disable-features list,
// in its order, which the helper removes whole from the default switches
// (ignoreDefaultArgs matches exact strings) and gives again less
// [reenabled]. A pin whose list differs leaves its own switch in the command
// line, which TestChromiumRunsWithTheListedCommandLine refuses.
var playwrightDisabled = []string{
	"AvoidUnnecessaryBeforeUnloadCheckSync", "DestroyProfileOnBrowserClose", "HttpsUpgrades",
	"HttpsFirstBalancedModeAutoEnable", "LensOverlay", "MediaRouter", "PaintHolding",
	"ThirdPartyStoragePartitioning", "AvoidCorsURLLoaderRestartOnRedirect", "TcpPortRandomizationWin",
	"OptimizationHints", "NetworkTimeServiceQuerying", "AimEnabled", "PreconnectToSearch",
	"msForceBrowserSignIn", "msEdgeUpdateLaunchServicesPreferredVersion",
}

// reenabled are the features of Playwright's list a browser of untrusted
// pages keeps on: a navigation upgraded to HTTPS, and a third party's storage
// partitioned by the site that embeds it.
var reenabled = []string{"HttpsUpgrades", "HttpsFirstBalancedModeAutoEnable", "ThirdPartyStoragePartitioning"}

// ignoredDefaults are Playwright's default switches the helper removes, each
// a test-oriented default that weakens browsing untrusted pages: popups
// unblocked, WebGL on a software rasteriser, and the features above off.
func ignoredDefaults() []string {
	return []string{
		"--disable-popup-blocking",
		"--enable-unsafe-swiftshader",
		"--disable-features=" + strings.Join(playwrightDisabled, ","),
	}
}

// args are the switches the helper gives Chromium after Playwright's own:
// Playwright's disabled features less [reenabled].
func args() []string {
	kept := slices.DeleteFunc(slices.Clone(playwrightDisabled), func(f string) bool { return slices.Contains(reenabled, f) })
	return []string{"--disable-features=" + strings.Join(kept, ",")}
}

// The places ensure looks for a Chromium, which chromium.mjs walks in the
// order a build is trusted: Playwright's browsers directories, as its own
// installer and an image's recipe lay them out; the branded Chrome
// Playwright's chrome channel finds on Linux, where every box runs; and the
// names a Chromium goes by on PATH, never chromium-browser, Ubuntu's snap
// shim.
var (
	searchPlaywright = []string{"$PLAYWRIGHT_BROWSERS_PATH", "/ms-playwright", "~/.cache/ms-playwright"}
	searchChrome     = []string{"/opt/google/chrome/chrome"}
	searchPath       = []string{"chromium", "google-chrome", "google-chrome-stable"}
)

// config is config.json.
type config struct {
	V int `json:"v"`
	// Helper is the modules' [Version].
	Helper   string `json:"helper"`
	Protocol int    `json:"protocol"`
	// Pin is the @playwright/mcp release the lock installs.
	Pin string `json:"pin"`
	// PinDigest names the lock's two files in the 12 hex digits the tree's
	// name carries ([pinDigest]).
	PinDigest string `json:"pin_digest"`
	// InstalledDigest is the digest of the files a whole install of the lock
	// holds (tree/installed.sha256), which an install must match before it is
	// published.
	InstalledDigest string `json:"installed_digest"`
	// NodeFloor and NPMFloor are [MinNodeMajor] and [MinNPMMajor].
	NodeFloor int `json:"node_floor"`
	NPMFloor  int `json:"npm_floor"`
	// Install is the install's bounds.
	Install struct {
		TimeoutMS        int `json:"timeout_ms"`
		FetchTimeoutMS   int `json:"fetch_timeout_ms"`
		FetchRetries     int `json:"fetch_retries"`
		FetchRetryWaitMS int `json:"fetch_retry_wait_ms"`
	} `json:"install"`
	Chromium configChromium          `json:"chromium"`
	Sandbox  browser.ChromiumSandbox `json:"sandbox"`
	Viewport struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"viewport"`
	Switches struct {
		IgnoreDefaultArgs []string `json:"ignore_default_args"`
		Args              []string `json:"args"`
	} `json:"switches"`
	// SupervisorEnv is every variable ensure hands the supervisor, where its
	// own environment sets it ([supervisorEnv]).
	SupervisorEnv    []string `json:"supervisor_env"`
	EnsureDeadlineMS int      `json:"ensure_deadline_ms"`
	LockTakeoverMS   int      `json:"lock_takeover_ms"`
	SupervisorWaitMS int      `json:"supervisor_wait_ms"`
	LaunchTimeoutMS  int      `json:"launch_timeout_ms"`
	Relaunch         struct {
		Max      int `json:"max"`
		WindowMS int `json:"window_ms"`
	} `json:"relaunch"`
	StopGraceMS int `json:"stop_grace_ms"`
	HelloMS     int `json:"hello_ms"`
	MCP         struct {
		OutputMaxBytes int `json:"output_max_bytes"`
		ActionMS       int `json:"action_ms"`
		NavigationMS   int `json:"navigation_ms"`
	} `json:"mcp"`
	// Frames, State and Relay are the live view's bounds: internal/browser's
	// wire and live's own knobs, so the box keeps what the hub refuses.
	Frames configFrames `json:"frames"`
	State  configState  `json:"state"`
	Relay  configRelay  `json:"relay"`
}

// configFrames is how the box captures its screen, and the bounds a frame
// keeps.
type configFrames struct {
	Quality         int     `json:"quality"`
	MaxWidth        int     `json:"max_width"`
	MaxHeight       int     `json:"max_height"`
	MaxBytes        int     `json:"max_bytes"`
	MetaBytes       int     `json:"meta_bytes"`
	JPEGHeaderBytes int     `json:"jpeg_header_bytes"`
	DeviceMax       int     `json:"device_max"`
	ScaleMin        float64 `json:"scale_min"`
	ScaleMax        float64 `json:"scale_max"`
	WatchFPS        int     `json:"watch_fps"`
	FlowMajor       int     `json:"flow_major"`
	QuietMS         int     `json:"quiet_ms"`
	OversizeQuality int     `json:"oversize_quality"`
	OversizeScale   float64 `json:"oversize_scale"`
}

// configState is the bounds of the box's state of its tabs, and how often
// their titles are read.
type configState struct {
	MaxBytes   int `json:"max_bytes"`
	MaxTabs    int `json:"max_tabs"`
	TitleChars int `json:"title_chars"`
	URLBytes   int `json:"url_bytes"`
	TitleMS    int `json:"title_ms"`
}

// configRelay is relay.sock's bound on one record's header.
type configRelay struct {
	HeaderBytes int `json:"header_bytes"`
}

// configChromium is the Chromium a box looks for: where, and the build the
// pinned Playwright expects.
type configChromium struct {
	ExpectedRevision string `json:"expected_revision"`
	ExpectedVersion  string `json:"expected_version"`
	Search           struct {
		Playwright []string `json:"playwright"`
		Chrome     []string `json:"chrome"`
		Path       []string `json:"path"`
	} `json:"search"`
}

// configJSON is config.json as [Write] puts it into a box for s, every other
// value a constant of this build.
func configJSON(s Settings) ([]byte, error) {
	if !s.Sandbox.Valid() {
		return nil, fmt.Errorf("boxhelper: Settings.Sandbox is %q, and a box's Chromium sandbox is %s: resolve the "+
			"cell's chromium_sandbox before writing the helper", s.Sandbox, words.Or(browser.ChromiumSandboxes()))
	}
	c := config{V: configVersion, Helper: Version(), Protocol: protocol, Pin: browser.PlaywrightMCPRelease,
		PinDigest: pinDigest(manifest, lock), InstalledDigest: strings.TrimSuffix(installed, "\n"),
		NodeFloor: MinNodeMajor, NPMFloor: MinNPMMajor, Sandbox: s.Sandbox}
	c.Install.TimeoutMS, c.Install.FetchTimeoutMS = installTimeoutMS, installFetchTimeoutMS
	c.Install.FetchRetries, c.Install.FetchRetryWaitMS = installFetchRetries, installFetchRetryWaitMS
	c.Chromium.ExpectedRevision = browser.ExpectedChromiumRevision
	c.Chromium.ExpectedVersion = browser.ExpectedChromiumVersion
	c.Chromium.Search.Playwright = searchPlaywright
	c.Chromium.Search.Chrome = searchChrome
	c.Chromium.Search.Path = searchPath
	c.Viewport.Width, c.Viewport.Height = viewportWidth, viewportHeight
	c.Switches.IgnoreDefaultArgs = ignoredDefaults()
	c.Switches.Args = args()
	c.SupervisorEnv = supervisorEnv()
	c.EnsureDeadlineMS, c.LockTakeoverMS, c.SupervisorWaitMS = ensureDeadlineMS, lockTakeoverMS, supervisorWaitMS
	c.LaunchTimeoutMS, c.StopGraceMS, c.HelloMS = launchTimeoutMS, stopGraceMS, helloMS
	c.Relaunch.Max, c.Relaunch.WindowMS = relaunchMax, relaunchWindowMS
	c.MCP.OutputMaxBytes, c.MCP.ActionMS, c.MCP.NavigationMS = mcpOutputMaxBytes, mcpActionMS, mcpNavigationMS
	c.Frames = configFrames{Quality: browser.FrameQuality, MaxWidth: browser.MaxFrameWidth,
		MaxHeight: browser.MaxFrameHeight, MaxBytes: browser.MaxFrameBytes, MetaBytes: browser.MaxFrameMetaBytes,
		JPEGHeaderBytes: browser.JPEGHeaderBytes, DeviceMax: browser.MaxDeviceSize, ScaleMin: browser.MinPageScale,
		ScaleMax: browser.MaxPageScale, WatchFPS: browser.WatchFPS, FlowMajor: live.ScreencastFlowMajor,
		QuietMS: live.QuietMS, OversizeQuality: live.OversizeQuality, OversizeScale: live.OversizeScale}
	c.State = configState{MaxBytes: browser.MaxStateBytes, MaxTabs: browser.MaxTabs, TitleChars: browser.MaxTitleChars,
		URLBytes: browser.MaxURLBytes, TitleMS: live.TitleMS}
	c.Relay = configRelay{HeaderBytes: live.MaxHeaderBytes}
	body, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

// supervisorEnv is the environment the supervisor may have, by name: the
// engine's one list for every child, hostbox.PassthroughEnv, and the names of
// hostbox.HostUserEnv, which in a box are the box's own. A variable a
// Chromium needs and this lacks belongs on that one list, for every child.
func supervisorEnv() []string {
	return slices.Concat(hostbox.PassthroughEnv, hostbox.HostUserEnv)
}
