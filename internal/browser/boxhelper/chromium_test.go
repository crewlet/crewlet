package boxhelper_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/browser/boxhelper/boxhelpertest"
	"github.com/crewlet/crewlet/internal/browser/chromiumtest"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
	"github.com/crewlet/crewlet/internal/shellword"
)

// sandboxRewrite is the pinned Playwright's words for a Chromium whose own
// sandbox would not start, which replace Chromium's (doRewriteStartupLog), as
// a launch's error carries them.
const sandboxRewrite = "browserType.launchPersistentContext: Target page, context or browser has been closed\n" +
	"Browser logs:\nChromium sandboxing failed!\n================================\n" +
	"To avoid the sandboxing issue, do either of the following:\n" +
	"  - (preferred): Configure your environment to support sandboxing\n" +
	"  - (alternative): Launch Chromium without sandbox using 'chromiumSandbox: false' option\n" +
	"================================\n"

// THE SANDBOX FAILURE NAMES THE CAUSE THE BOX SHOWS: what the box shows of
// why Chromium's own sandbox would not start is read in one order, the first
// that holds: run as root, Ubuntu's AppArmor restriction on user
// namespaces, user namespaces turned off either way, a seccomp filter on the
// process, else unknown. A launch failing in Playwright's words for a sandbox
// that would not start is chromium_sandbox_unavailable with that cause where
// the cell requires the sandbox, and an exit like any other where it waives
// it; Chromium's own words alone, which Playwright replaces, are no sandbox
// failure. No host shows more than one of these, so the probe reads each.
//
// Mutations, each red: read root last; match only the raw marker Playwright
// replaced.
func TestTheSandboxFailureNamesTheCauseTheBoxShows(t *testing.T) {
	t.Parallel()
	const (
		apparmor  = "/proc/sys/kernel/apparmor_restrict_unprivileged_userns"
		maxUserns = "/proc/sys/user/max_user_namespaces"
		clone     = "/proc/sys/kernel/unprivileged_userns_clone"
		status    = "/proc/self/status"
	)
	filtered := "Name:\tnode\nSeccomp:\t2\nSeccomp_filters:\t1\n"
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		t.Parallel()
		probe := boxhelpertest.Start(t, env, boxhelpertest.Options{}).Probe(t)
		for _, tc := range []struct {
			name  string
			uid   int
			files map[string]string
			want  string
		}{
			{"root, whatever else holds", 0, map[string]string{apparmor: "1", maxUserns: "0", status: filtered}, "root"},
			{"AppArmor's restriction", 1000, map[string]string{apparmor: "1\n", maxUserns: "0", status: filtered}, "apparmor"},
			{"user namespaces turned off", 1000, map[string]string{apparmor: "0", maxUserns: "0", status: filtered},
				"userns"},
			{"Debian's switch for them off", 1000, map[string]string{maxUserns: "63431", clone: "0"}, "userns"},
			{"a seccomp filter", 1000, map[string]string{apparmor: "0", maxUserns: "63431", clone: "1", status: filtered},
				"seccomp"},
			{"strict seccomp, no filter", 1000, map[string]string{status: "Seccomp:\t1\n"}, "unknown"},
			{"nothing it can tell", 1000, map[string]string{}, "unknown"},
		} {
			var got struct{ Cause string }
			probe.Do(map[string]any{"op": "sandbox_cause", "uid": tc.uid, "files": tc.files}, &got)
			if got.Cause != tc.want {
				t.Errorf("%s: the box shows %q, want %q", tc.name, got.Cause, tc.want)
			}
		}
		appArmored := map[string]string{apparmor: "1"}
		for _, tc := range []struct {
			name, message, sandbox, reason, cause string
		}{
			{"Playwright's words, the sandbox required", sandboxRewrite, "required", "chromium_sandbox_unavailable",
				"apparmor"},
			{"Playwright's words, the sandbox waived", sandboxRewrite, "waived", "chromium_start_failed", "exited"},
			{"Chromium's own words, which Playwright replaces", "browserType.launchPersistentContext: Target page, " +
				"context or browser has been closed\nBrowser logs:\n[1:1:FATAL:zygote_host_impl_linux.cc(132)] No usable " +
				"sandbox! See crbug.com/638180.", "required", "chromium_start_failed", "exited"},
		} {
			var got struct{ Reason, Cause string }
			probe.Do(map[string]any{"op": "launch_failure", "message": tc.message, "sandbox": tc.sandbox, "uid": 1000,
				"files": appArmored}, &got)
			if got.Reason != tc.reason || got.Cause != tc.cause {
				t.Errorf("%s: read as %s %s, want %s %s", tc.name, got.Reason, got.Cause, tc.reason, tc.cause)
			}
		}
	})
}

// standInChromium puts on the box's PATH, as chromium, a stand-in answering
// --version as the newest pin does and running body when launched.
func standInChromium(t *testing.T, box *boxhelpertest.Box, body string) {
	t.Helper()
	script := "#!/bin/sh\n[ \"$1\" = --version ] && { echo 'Chromium 157.0.8103.50'; exit 0; }\n" + body
	if err := os.WriteFile(filepath.Join(box.Home, "bin", "chromium"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// PLAYWRIGHT'S LAUNCH FAILURES STILL READ AS THEIR REASONS: a Chromium failing
// to start, its own words wrapped as the pinned Playwright wraps them, is the
// cause ensure names, each marker read alone: a library the loader cannot load,
// or one Chromium loads itself; a profile whose lock it cannot take, or another
// Chromium of this host serves; an exit 21 alone; an exit of its own; no answer
// on its pipe in a shortened bound; and no usable sandbox,
// chromium_sandbox_unavailable (the host's cause) where the cell requires the
// sandbox and an exit where it waives it. Stand-ins say each, so a pin that
// rewords what it carries fails here.
//
// Mutation: drop any one marker from the classifier (its row reads exited, or
// unknown where Playwright's rewrite carried it).
func TestPlaywrightsLaunchFailuresStillReadAsTheirReasons(t *testing.T) {
	t.Parallel()
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		t.Parallel()
		for _, tc := range []struct {
			name, body string
			sandbox    browser.ChromiumSandbox
			config     map[string]any
			reason     browser.EnsureReason
			cause      browser.EnsureCause // empty: any of reason's
		}{
			{name: "a library the loader cannot load", body: "echo 'chrome: error while loading shared libraries: " +
				"libnss3.so: wrong ELF class: ELFCLASS32' >&2\nexit 127\n",
				reason: browser.ReasonChromiumStartFailed, cause: browser.CauseLibraries},
			{name: "a library Chromium loads itself", body: "echo '[1:1:FATAL:egl_util.cc(44)] Failed to load GLES " +
				"library: libGLESv2.so: cannot open shared object file: No such file or directory' >&2\nexit 1\n",
				reason: browser.ReasonChromiumStartFailed, cause: browser.CauseLibraries},
			{name: "a profile whose lock it cannot take", body: "echo '[1:1:ERROR:chrome_browser_main.cc(1404)] Failed " +
				"to create a ProcessSingleton for your profile directory. This means that running multiple instances " +
				"would start multiple browser processes rather than opening a new window in the existing process. " +
				"Aborting now to avoid profile corruption.' >&2\nexit 1\n",
				reason: browser.ReasonChromiumStartFailed, cause: browser.CauseProfileHeld},
			{name: "a profile another Chromium of this host serves", body: "echo 'Opening in existing browser " +
				"session.'\nexit 0\n", reason: browser.ReasonChromiumStartFailed, cause: browser.CauseProfileHeld},
			{name: "an exit 21 with no words", body: "exit 21\n",
				reason: browser.ReasonChromiumStartFailed, cause: browser.CauseProfileHeld},
			{name: "an exit of its own", body: "echo 'a browser that ends in words nobody reads' >&2\nexit 1\n",
				reason: browser.ReasonChromiumStartFailed, cause: browser.CauseExited},
			{name: "no answer on its pipe", body: "exec /bin/cat <&3 >/dev/null\n",
				config: map[string]any{"launch_timeout_ms": 1000},
				reason: browser.ReasonChromiumStartFailed, cause: browser.CauseTimeout},
			{name: "no usable sandbox, required", body: "echo '[1:1:FATAL:zygote_host_impl_linux.cc(132)] No usable " +
				"sandbox! See https://crbug.com/638180.' >&2\nexit 1\n", sandbox: browser.SandboxRequired,
				reason: browser.ReasonChromiumSandboxUnavailable},
			{name: "no usable sandbox, waived", body: "echo '[1:1:FATAL:zygote_host_impl_linux.cc(132)] No usable " +
				"sandbox! See https://crbug.com/638180.' >&2\nexit 1\n", sandbox: browser.SandboxWaived,
				reason: browser.ReasonChromiumStartFailed, cause: browser.CauseExited},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				box := boxhelpertest.Start(t, env, boxhelpertest.Options{Route: boxhelpertest.NoChromium,
					Sandbox: tc.sandbox, Config: tc.config})
				standInChromium(t, box, tc.body)
				got := box.Ensure(t)
				if got.Verdict != browser.EnsureWithheld || got.Reason != tc.reason ||
					(tc.cause != "" && got.Cause != tc.cause) || !slices.Contains(tc.reason.Causes(), got.Cause) {
					t.Errorf("ensure answered %+v, want withheld %s %s", got, tc.reason, tc.cause)
				}
			})
		}
	})
}

// chromiumSwitches is the command line TestChromiumRunsWithTheListedCommandLine
// holds the browser to after its executable, as the pinned Playwright builds it from its defaults less the
// helper's ignored ones, the helper's own, and what Playwright adds last.
var chromiumSwitches = []string{
	"--disable-field-trial-config",
	"--disable-background-networking",
	"--disable-background-timer-throttling",
	"--disable-backgrounding-occluded-windows",
	"--disable-back-forward-cache",
	"--disable-breakpad",
	"--disable-component-extensions-with-background-pages",
	"--disable-component-update",
	"--no-default-browser-check",
	"--disable-default-apps",
	"--disable-dev-shm-usage",
	"--disable-edgeupdater",
	"--disable-extensions",
	"--enable-features=CDPScreenshotNewSurface,SkipListAccountsWithoutGaiaCookies",
	"--allow-pre-commit-input",
	"--disable-hang-monitor",
	"--disable-ipc-flooding-protection",
	"--disable-prompt-on-repost",
	"--disable-renderer-backgrounding",
	"--disable-updater-scheduler",
	"--force-color-profile=srgb",
	"--metrics-recording-only",
	"--optimization-guide-performance-class=8",
	"--no-first-run",
	"--password-store=basic",
	"--use-mock-keychain",
	"--no-service-autorun",
	"--disable-search-engine-choice-screen",
	"--unsafely-disable-devtools-self-xss-warnings",
	"--edge-skip-compat-layer-relaunch",
	"--disable-stack-profiler",
	"--disable-infobars",
	"--disable-sync",
	"--do-not-de-elevate",
	"--headless",
	"--hide-scrollbars",
	"--mute-audio",
	"--blink-settings=primaryHoverType=2,availableHoverTypes=2,primaryPointerType=4,availablePointerTypes=4",
}

// helperFeatures is the helper's own --disable-features: Playwright's list less
// the three a browser of untrusted pages keeps on.
const helperFeatures = "--disable-features=AvoidUnnecessaryBeforeUnloadCheckSync,DestroyProfileOnBrowserClose," +
	"LensOverlay,MediaRouter,PaintHolding,AvoidCorsURLLoaderRestartOnRedirect,TcpPortRandomizationWin," +
	"OptimizationHints,NetworkTimeServiceQuerying,AimEnabled,PreconnectToSearch,msForceBrowserSignIn," +
	"msEdgeUpdateLaunchServicesPreferredVersion"

// commandLineFor is the command line TestChromiumRunsWithTheListedCommandLine
// expects of box's browser under sandbox: the listed switches, --no-sandbox only where the cell waives the
// sandbox, the helper's features, the suite's one switch, then what
// Playwright adds for a persistent context on a pipe.
func commandLineFor(box *boxhelpertest.Box, sandbox browser.ChromiumSandbox) []string {
	want := slices.Clone(chromiumSwitches)
	if sandbox == browser.SandboxWaived {
		want = append(want, "--no-sandbox")
	}
	return append(want, helperFeatures, "--host-resolver-rules="+chromiumtest.LoopbackOnly,
		"--user-data-dir="+filepath.Join(box.Folder, "profile"), "--remote-debugging-pipe", "about:blank")
}

// browserArgv is the arguments the kernel holds for box's browser, read from
// the pid its supervisor.json records, after its executable.
func browserArgv(t *testing.T, box *boxhelpertest.Box) []string {
	t.Helper()
	s := box.Supervisor(t)
	if s.Chromium == nil || s.Chromium.PID <= 0 {
		t.Fatalf("the supervisor records no browser: %+v", s)
	}
	return boxhelpertest.Argv(t, s.Chromium.PID, launchOf(t, box).Executable)
}

// launchChromium is the Chromium a box's launch.json chose.
type launchChromium struct {
	Route        string `json:"route"`
	Channel      string `json:"channel"`
	Executable   string `json:"executable"`
	BrowsersPath string `json:"browsers_path"`
	Version      string `json:"version"`
	Build        string `json:"build"`
}

func launchOf(t *testing.T, box *boxhelpertest.Box) launchChromium {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(box.Folder, "launch.json"))
	if err != nil {
		t.Fatal(err)
	}
	var launch struct{ Chromium launchChromium }
	if err := json.Unmarshal(raw, &launch); err != nil {
		t.Fatal(err)
	}
	return launch.Chromium
}

// CHROMIUM RUNS WITH THE LISTED COMMAND LINE: the browser the supervisor
// records runs, by what the kernel holds for it, exactly the listed switches
// in their order, the suite's one switch to loopback with them: Playwright's
// defaults less popups unblocked, the software rasteriser and the features a
// browser of untrusted pages keeps on; --no-sandbox only where the cell waives
// the sandbox; no DevTools port. Where the cell requires the sandbox and this
// host cannot start it (an oracle beside the helper says which), the browser
// is withheld for the host's cause, never launched without it.
//
// Mutations, each red: drop ignoreDefaultArgs; drop one of the three features
// from the reenabled list; omit chromiumSandbox (--no-sandbox under
// required).
func TestChromiumRunsWithTheListedCommandLine(t *testing.T) {
	boxhelpertest.Each(t, func(t *testing.T, env boxhelpertest.Env) {
		for _, sandbox := range []browser.ChromiumSandbox{browser.SandboxWaived, browser.SandboxRequired} {
			t.Run(string(sandbox), func(t *testing.T) {
				box := boxhelpertest.Start(t, env, boxhelpertest.Options{Sandbox: sandbox})
				got := box.Ensure(t)
				if sandbox == browser.SandboxRequired && !boxhelpertest.SandboxRuns(t) {
					if got.Verdict != browser.EnsureWithheld || got.Reason != browser.ReasonChromiumSandboxUnavailable ||
						got.Cause == browser.CauseLauncher {
						t.Errorf("this host cannot start Chromium's sandbox, and ensure answered %+v, want it withheld "+
							"as chromium_sandbox_unavailable for the host's cause, the browser launched with it", got)
					}
					return
				}
				if got.Verdict != browser.EnsureReady || got.Sandbox != sandbox {
					t.Fatalf("ensure answered %+v, want ready under %s", got, sandbox)
				}
				if argv, want := browserArgv(t, box), commandLineFor(box, sandbox); !slices.Equal(argv, want) {
					t.Errorf("the browser runs\n%q\nwant\n%q", argv, want)
				}
			})
		}
	})
}

// distributionChromium puts on box's PATH, as chromium, what a distribution's
// build may be: one that answers --version with words after its version, as
// Debian's does, and runs the case's pinned build when launched.
func distributionChromium(t *testing.T, box *boxhelpertest.Box, env boxhelpertest.Env) {
	t.Helper()
	said := "Chromium " + chromiumtest.Version(t, env.Chrome) + " built on Debian 12.2, running on Debian 12.2"
	script := "#!/bin/sh\n[ \"$1\" = --version ] && { echo " + shellword.Quote(said) + "; exit 0; }\nexec " +
		shellword.Quote(env.Chrome) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(box.Home, "bin", "chromium"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// THE CHROMIUM IS CHOSEN IN THE ORDER ITS BUILD IS TRUSTED: the build the
// pinned Playwright expects, laid out as its installer lays it and answering
// the version expected, launches through Playwright's chromium channel as
// expected, whatever else the box carries; the same directory answering
// another version is an executable of no trust, untested; a Chromium on PATH
// is one too, and comes before such a directory; and a distribution's, which
// says more after its version, is read by that version.
//
// Mutations, each red: trust the directory's name alone (another version
// reads as expected); read a version only where it ends the answer (the
// distribution's build is no Chromium).
func TestTheChromiumIsChosenInTheOrderItsBuildIsTrusted(t *testing.T) {
	boxhelpertest.Each(t, func(t *testing.T, env boxhelpertest.Env) {
		version := chromiumtest.Version(t, env.Chrome)
		for _, tc := range []struct {
			name         string
			route        boxhelpertest.Route
			another      bool // the config expects another version than the build's
			distribution bool // the one on PATH says more after its version
			want         launchChromium
			onPath       bool // the executable chosen is the one on PATH
		}{
			{name: "the expected build", route: boxhelpertest.Expected,
				want: launchChromium{Route: "channel", Channel: "chromium", Build: "expected"}},
			{name: "its directory, another version", route: boxhelpertest.Expected, another: true,
				want: launchChromium{Route: "executable", Build: "untested"}},
			{name: "a chromium on PATH", route: boxhelpertest.OnPath,
				want: launchChromium{Route: "executable", Build: "untested"}, onPath: true},
			{name: "both", route: boxhelpertest.ExpectedAndOnPath,
				want: launchChromium{Route: "channel", Channel: "chromium", Build: "expected"}},
			{name: "PATH before another version's directory", route: boxhelpertest.ExpectedAndOnPath, another: true,
				want: launchChromium{Route: "executable", Build: "untested"}, onPath: true},
			{name: "a distribution's on PATH", route: boxhelpertest.NoChromium, distribution: true,
				want: launchChromium{Route: "executable", Build: "untested"}, onPath: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				box := boxhelpertest.Start(t, env, boxhelpertest.Options{Route: tc.route})
				if tc.distribution {
					distributionChromium(t, box, env)
				}
				if tc.another {
					config := box.Config(t)
					config["chromium"].(map[string]any)["expected_version"] = "1.2.3.4"
					box.SetConfig(t, config)
				}
				got := box.Ensure(t)
				chosen := launchOf(t, box)
				if got.Verdict != browser.EnsureReady || string(got.Build) != tc.want.Build || got.Chromium != version ||
					chosen.Route != tc.want.Route || chosen.Channel != tc.want.Channel || chosen.Build != tc.want.Build {
					t.Errorf("ensure answered %+v and chose %+v, want %+v", got, chosen, tc.want)
				}
				if onPath := chosen.Executable == filepath.Join(box.Home, "bin", "chromium"); onPath != tc.onPath {
					t.Errorf("ensure chose %s, the one on PATH %t, want %t", chosen.Executable, onPath, tc.onPath)
				}
			})
		}
	})
}

// launcher puts on box's PATH, as chromium, what a distribution's launcher
// may be: a script that runs the case's pinned build with --no-sandbox.
func launcher(t *testing.T, box *boxhelpertest.Box, env boxhelpertest.Env) {
	t.Helper()
	script := "#!/bin/sh\nexec " + shellword.Quote(env.Chrome) + " --no-sandbox \"$@\"\n"
	if err := os.WriteFile(filepath.Join(box.Home, "bin", "chromium"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// nothingOnProfile fails t unless every process on box's profile goes: a
// browser's processes go together, so one past the bound ends the wait.
func nothingOnProfile(t *testing.T, box *boxhelpertest.Box) {
	t.Helper()
	for _, pid := range box.Browsers(t) {
		if !procgrouptest.AwaitGone(t, pid, goneWithin) {
			t.Errorf("pid %d still runs on the profile of a browser withheld %s on", pid, goneWithin)
			return
		}
	}
}

// A LAUNCHER THAT DROPS THE SANDBOX IS WITHHELD UNDER REQUIRED: where the
// cell requires Chromium's own sandbox, a chromium on PATH that runs the pin
// with --no-sandbox, as a distribution's launcher may, starts on any host,
// and is withheld as chromium_sandbox_unavailable launcher, read off the
// command line the kernel holds for it, nothing left on its profile; one whose
// command line cannot be read (its profile's lock unreadable, or none from the
// kernel, as once the browser's main thread exits: the probe's faults) is
// withheld as unknown, since nothing then holds it to the sandbox.
//
// Mutations, each red: trust the switch (the launcher's browser is ready as
// required); take no command line for one without the switch (its row is
// ready).
func TestALauncherThatDropsTheSandboxIsWithheldUnderRequired(t *testing.T) {
	boxhelpertest.Each(t, func(t *testing.T, env boxhelpertest.Env) {
		box := boxhelpertest.Start(t, env, boxhelpertest.Options{Route: boxhelpertest.NoChromium,
			Sandbox: browser.SandboxRequired})
		launcher(t, box, env)
		got := box.Ensure(t)
		if got.Verdict != browser.EnsureWithheld || got.Reason != browser.ReasonChromiumSandboxUnavailable ||
			got.Cause != browser.CauseLauncher {
			t.Errorf("ensure answered %+v, want withheld chromium_sandbox_unavailable launcher", got)
		}
		nothingOnProfile(t, box)
		// One ensure left running, as a failed row may, goes before the probe's
		// supervisor writes over its record.
		box.Stop(t)
		for _, unread := range []struct {
			name string
			fail *fault
		}{
			{"its profile's lock unreadable", &fault{Method: "readlink", Errno: "ENOENT", After: 1}},
			{"no command line from the kernel", &fault{Method: "readFile", BlankReading: true}},
		} {
			var answered supervisedAnswer
			box.Probe(t).Do(map[string]any{"op": "supervise", "launch": filepath.Join(box.Folder, "launch.json"),
				"fail": unread.fail}, &answered)
			if answered.State != "failed" ||
				!slices.Equal(answered.Reports, []string{"failed chromium_sandbox_unavailable unknown"}) {
				t.Errorf("a browser with %s was answered %+v, want it withheld as unknown", unread.name, answered)
			}
			nothingOnProfile(t, box)
		}
	})
}
