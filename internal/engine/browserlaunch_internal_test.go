package engine

import (
	"encoding/json"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/browser/boxhelper"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// seatLaunch is one executor launch of a seat on a claude-code entry.
type seatLaunch struct {
	// cell is the entry's run_in, and webDenied its web_tools: denied.
	cell      sandbox.Placement
	webDenied bool
	// settings is the seat's role.sandbox block.
	settings *org.RoleSandbox
	// pinned is the providers.sandbox of the revision the turn is pinned to,
	// and live, where set, that of a revision the node applied since.
	pinned, live *config.SandboxProvider
}

// run launches s and answers the job's row and the box the job names.
func (s seatLaunch) run(t *testing.T) (sandbox.PendingRun, *sandbox.FakeSandbox) {
	t.Helper()
	c, seat := chainCompany(t, chainMember{key: "api", agent: "claude-code", model: "claude-golden",
		runIn: config.Placement(s.cell), webDenied: s.webDenied})
	seat.Sandbox = s.settings
	c.Config.Providers.Sandbox = s.pinned
	e := launchReadyEngine(t, c)
	if s.live != nil {
		next, nextConfig := *c, *c.Config
		nextConfig.Providers.Sandbox = s.live
		next.Config = &nextConfig
		e.epoch.current.Store(&next)
	}
	l := launcherOn(e, c, seat, "t1")
	if _, err := l.LaunchExecutor(t.Context(), launchRequest(l, nil)); err != nil {
		t.Fatalf("LaunchExecutor: %v", err)
	}
	row, found, err := e.sandbox.Load().pending.Get(t.Context(), "t1")
	if err != nil || !found {
		t.Fatalf("the launch's row: found %v, err %v", found, err)
	}
	provider, err := c.catalogue.Provider(s.cell)
	if err != nil {
		t.Fatalf("the %s cell's provider: %v", s.cell, err)
	}
	box := provider.(*sandbox.FakeProvider).Box(row.SandboxID)
	if box == nil {
		t.Fatalf("the %s cell holds no box %q, the one the job names", s.cell, row.SandboxID)
	}
	return row, box
}

// localCatalogue is a providers.sandbox whose local backend's chromium_sandbox
// is written as s.
func localCatalogue(s browser.ChromiumSandbox) *config.SandboxProvider {
	return &config.SandboxProvider{Local: &config.LocalSandbox{ChromiumSandbox: s}}
}

// helperSandbox is the Chromium sandbox the browser helper's config.json in box
// names, the one document the engine and the helper agree through, and
// whether the launch wrote one there.
func helperSandbox(t *testing.T, box *sandbox.FakeSandbox) (browser.ChromiumSandbox, bool) {
	t.Helper()
	raw, err := box.ReadFile(t.Context(), path.Join(boxhelper.Folder(box.Home()), "config.json"))
	if err != nil {
		t.Fatalf("reading the helper's config.json: %v", err)
	}
	if raw == nil {
		return "", false
	}
	var helper struct {
		Sandbox browser.ChromiumSandbox `json:"sandbox"`
	}
	if err := json.Unmarshal(raw, &helper); err != nil {
		t.Fatalf("the helper's config.json: %v", err)
	}
	return helper.Sandbox, true
}

// helperFiles is every file box holds in the browser helper's folder.
func helperFiles(box *sandbox.FakeSandbox) []string {
	folder := boxhelper.Folder(box.Home()) + "/"
	return slices.DeleteFunc(box.Files(), func(p string) bool { return !strings.HasPrefix(p, folder) })
}

// A BROWSER SEAT'S LAUNCH CARRIES ITS CHROMIUM SANDBOX SETTING into the helper
// its box is handed: required unless local.chromium_sandbox is exactly waived,
// a value this build does not know reading as the stricter, and read off the
// turn's pinned revision, as the member and the cell are, never off a revision
// the node applied since.
//
// Mutations, each red: read an unset chromium_sandbox as waived; read the
// field as written, defaulting only the empty value; read the node's live
// revision; leave Browser off the launch request.
func TestABrowserSeatsLaunchCarriesItsChromiumSandboxSetting(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		pinned, live *config.SandboxProvider
		want         browser.ChromiumSandbox
	}{
		"unset":                            {pinned: localCatalogue(""), want: browser.SandboxRequired},
		"waived":                           {pinned: localCatalogue(browser.SandboxWaived), want: browser.SandboxWaived},
		"a value this build does not know": {pinned: localCatalogue("off"), want: browser.SandboxRequired},
		"the turn's revision, not the node's live one": {pinned: localCatalogue(browser.SandboxWaived),
			live: localCatalogue(""), want: browser.SandboxWaived},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			row, box := seatLaunch{cell: sandbox.Container, settings: &org.RoleSandbox{Browser: true},
				pinned: tc.pinned, live: tc.live}.run(t)
			if row.Launch.Browser == nil {
				t.Fatal("the job records no browser for a browser seat's launch")
			}
			if got, written := helperSandbox(t, box); !written || got != tc.want {
				t.Fatalf("the box's helper names Chromium's sandbox %q (written %v), want %q", got, written, tc.want)
			}
		})
	}
}

// THE LAUNCH, NOT THE ENGINE, WITHHOLDS A BROWSER SEAT'S BROWSER. The engine
// hands a browser seat's launch its browser whatever the run is, since only
// the launch's step knows the box the run got: in every cell but container the
// job records it withheld as cell_not_container, on an entry that denies the
// web as web_denied, and none of those boxes is handed the helper.
//
// Mutations, each red: hand no browser outside a container, which moves the
// cell rule out of the launch; hand none to a run denied the web.
func TestTheLaunchNotTheEngineWithholdsABrowserSeatsBrowser(t *testing.T) {
	t.Parallel()
	browserSeat := func(cell sandbox.Placement, webDenied bool) seatLaunch {
		return seatLaunch{cell: cell, webDenied: webDenied, settings: &org.RoleSandbox{Browser: true},
			pinned: localCatalogue("")}
	}
	type withheld struct {
		launch seatLaunch
		want   browser.EnsureReason
	}
	cases := map[string]withheld{
		"an entry that denies the web": {browserSeat(sandbox.Container, true), browser.ReasonWebDenied},
	}
	for _, cell := range sandbox.Placements {
		if cell != sandbox.Container {
			cases["in "+string(cell)] = withheld{browserSeat(cell, false), browser.ReasonCellNotContainer}
		}
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			row, box := tc.launch.run(t)
			want := types.ExecutorBrowser{State: browser.EnsureWithheld, Reason: tc.want}
			if got := row.Launch.Browser; got == nil || *got != want {
				t.Fatalf("the job records the browser as %+v, want %+v", got, want)
			}
			if files := helperFiles(box); len(files) != 0 {
				t.Errorf("the box was handed the browser's helper: %q", files)
			}
		})
	}
}

// A SEAT WITH NO BROWSER IS HANDED NONE, whether it writes no role.sandbox
// block or one that leaves browser unset: its container box, where a browser
// seat's would be readied, gets no helper, and its job records no browser.
//
// Mutation, red: hand one to a seat whose block leaves browser unset.
func TestASeatWithNoBrowserIsHandedNone(t *testing.T) {
	t.Parallel()
	for name, settings := range map[string]*org.RoleSandbox{
		"no role.sandbox block":         nil,
		"a block that names no browser": {Env: map[string]string{"CI": "1"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			row, box := seatLaunch{cell: sandbox.Container, settings: settings, pinned: localCatalogue("")}.run(t)
			if got := row.Launch.Browser; got != nil {
				t.Errorf("the job of a seat with no browser records one: %+v", got)
			}
			if files := helperFiles(box); len(files) != 0 {
				t.Errorf("the box of a seat with no browser was handed its helper: %q", files)
			}
		})
	}
}
