package chromiumtest

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/procgroup"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
)

// stub is an executable that answers --version as a Chrome for Testing build
// of version does.
func stub(t *testing.T, version string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chrome")
	script := fmt.Sprintf("#!/bin/sh\necho 'Google Chrome for Testing %s '\n", version)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// executable is an executable shell script of body, in a directory of its
// own named chrome-linux64, as a Chrome for Testing build keeps its own.
func executable(t *testing.T, body string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "chrome-linux64")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "chrome")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// A PIN IS NAMED, REQUIRED OR SKIPPED — and held to its major. Unset, a pin's
// build is a missing prerequisite (internal/prereqtest): it fails under the
// requirement the make targets set, naming its variable and `make chromium`,
// and skips otherwise. Named, it is an absolute path to an executable whose
// --version decides: the newest at or above browser.MinChromiumMajor, the
// floor of it exactly. A build that cannot answer fails saying what it
// printed, what fetches it again, and on Linux what it links; a good build is
// handed back. Not parallel: it sets the variables the helpers read.
//
// Mutations: drop the floor's exact-major check (a build of the next major
// stands for it), the newest's (one of the major below stands for it), or what
// a build that cannot answer printed from its failure — each red.
func TestAPinIsNamedRequiredOrSkipped(t *testing.T) {
	floor := browser.MinChromiumMajor
	floorBuild := stub(t, fmt.Sprintf("%d.0.8010.52", floor))
	newestBuild := stub(t, fmt.Sprintf("%d.0.8103.38", floor+4))
	belowTheFloor := stub(t, fmt.Sprintf("%d.0.7977.82", floor-1))
	aMajorUp := stub(t, fmt.Sprintf("%d.0.8037.92", floor+1))
	notChromium := stub(t, "of no version")
	const loader = "chrome: error while loading shared libraries: libnss3.so: cannot open shared object file"
	cannotStart := executable(t, "echo '"+loader+"' >&2\nexit 127\n")
	// What a build that cannot start is failed with: what it printed, what
	// fetches it again, and on Linux the list beside it of what it links.
	cannotStartSays := []string{loader, "`make chromium` fetches it"}
	if runtime.GOOS == "linux" {
		cannotStartSays = append(cannotStartSays, filepath.Join(filepath.Dir(cannotStart), "deb.deps"))
	}
	for _, tc := range []struct {
		name, env, require, value string
		find                      func(testing.TB) string
		fatal, skip               string   // a fragment of each; neither: the build back
		says                      []string // more fragments of the failure
	}{
		{name: "the newest unset and required", env: newestEnv, require: "1", find: Newest,
			fatal: newestEnv + " is unset"},
		{name: "the newest unset", env: newestEnv, find: Newest, skip: "`make chromium` fetches it"},
		{name: "the floor unset and required", env: floorEnv, require: "1", find: Floor, fatal: floorEnv + " is unset"},
		{name: "the floor unset", env: floorEnv, find: Floor, skip: "`make chromium` fetches it"},
		{name: "relative", env: newestEnv, require: "1", value: "chrome-linux64/chrome", find: Newest,
			fatal: "not an absolute path"},
		{name: "missing", env: floorEnv, require: "1", value: filepath.Join(t.TempDir(), "chrome"), find: Floor,
			fatal: "names nothing this case can reach"},
		{name: "no Chrome for Testing version", env: newestEnv, require: "1", value: notChromium, find: Newest,
			fatal: "names no Chrome for Testing version"},
		{name: "a newest below the floor", env: newestEnv, require: "1", value: belowTheFloor, find: Newest,
			fatal: fmt.Sprintf("below browser.MinChromiumMajor %d", floor)},
		{name: "a floor of the next major", env: floorEnv, require: "1", value: aMajorUp, find: Floor,
			fatal: "a floor nobody is testing"},
		{name: "a build that cannot start", env: newestEnv, require: "1", value: cannotStart, find: Newest,
			fatal: "exit status 127", says: cannotStartSays},
		{name: "a newest build", env: newestEnv, require: "1", value: newestBuild, find: Newest},
		{name: "the floor's build", env: floorEnv, require: "1", value: floorBuild, find: Floor},
		{name: "the floor's build as the newest", env: newestEnv, require: "1", value: floorBuild, find: Newest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(prereqtest.RequireEnv, tc.require)
			t.Setenv(newestEnv, "")
			t.Setenv(floorEnv, "")
			t.Setenv(tc.env, tc.value)
			var got string
			ended := prereqtest.Run(t, func(tb testing.TB) { got = tc.find(tb) })
			switch {
			case tc.fatal != "":
				if !strings.Contains(ended.Fatal, tc.fatal) || !strings.Contains(ended.Fatal, tc.env) || ended.Skip != "" {
					t.Errorf("failed %q, skipped %q; want a failure naming %s and saying %q", ended.Fatal, ended.Skip, tc.env, tc.fatal)
				}
				if tc.require == "1" && tc.value == "" && !strings.Contains(ended.Fatal, "make chromium") {
					t.Errorf("the failure %q does not name `make chromium`", ended.Fatal)
				}
				for _, want := range tc.says {
					if !strings.Contains(ended.Fatal, want) {
						t.Errorf("the failure %q does not say %q", ended.Fatal, want)
					}
				}
			case tc.skip != "":
				if !strings.Contains(ended.Skip, tc.skip) || ended.Fatal != "" {
					t.Errorf("failed %q, skipped %q; want a skip saying %q", ended.Fatal, ended.Skip, tc.skip)
				}
			default:
				if ended != (prereqtest.Ended{}) || got != tc.value {
					t.Errorf("found %q (ended %+v), want %s", got, ended, tc.value)
				}
			}
		})
	}
}

// A PIN'S VERSION IS THE ONE ITS BUILD ANSWERED: Version is the four numbers
// the build a pin's variable names gave --version, the newest's and the
// floor's alike, and a path neither names is refused, so no case names a
// version that is not a pin's own. Not parallel: it sets the variables.
//
// Mutations, each red: hand back the major alone; answer for any path, the
// variables unread.
func TestAPinsVersionIsTheOneItsBuildAnswered(t *testing.T) {
	floor := browser.MinChromiumMajor
	newestVersion, floorVersion := fmt.Sprintf("%d.0.8103.38", floor+4), fmt.Sprintf("%d.0.8010.52", floor)
	newestBuild, floorBuild := stub(t, newestVersion), stub(t, floorVersion)
	t.Setenv(prereqtest.RequireEnv, "1")
	t.Setenv(newestEnv, newestBuild)
	t.Setenv(floorEnv, floorBuild)
	for _, tc := range []struct{ name, chrome, want, fatal string }{
		{name: "the newest", chrome: newestBuild, want: newestVersion},
		{name: "the floor", chrome: floorBuild, want: floorVersion},
		{name: "a build no pin names", chrome: stub(t, newestVersion), fatal: "neither pin's build"},
		{name: "no path", fatal: "neither pin's build"},
	} {
		var got string
		ended := prereqtest.Run(t, func(tb testing.TB) { got = Version(tb, tc.chrome) })
		if !strings.Contains(ended.Fatal, tc.fatal) || (tc.fatal == "") != (ended.Fatal == "") || got != tc.want {
			t.Errorf("%s: Version is %q (failed %q), want %q (a failure saying %q)", tc.name, got, ended.Fatal,
				tc.want, tc.fatal)
		}
	}
}

// A PIN'S BUILD IS THE DIRECTORY ITS ZIP UNPACKED: Unpacked is the
// chrome-<platform> directory a build keeps its executable in on this
// platform, read from the executable's path, and a path laid out any other
// way is refused rather than read as a build.
//
// Mutations, each red: hand back the directory above it; take whatever
// directory the executable sits in.
func TestAPinsBuildIsTheDirectoryItsZipUnpacked(t *testing.T) {
	t.Parallel()
	b, ok := builds[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		t.Fatalf("no build layout for %s/%s, a platform the engine is released for", runtime.GOOS, runtime.GOARCH)
	}
	root, exe := t.TempDir(), filepath.FromSlash(b.exe)
	build := filepath.Join(root, "build", b.dir)
	for _, tc := range []struct{ name, chrome, want string }{
		{name: "a build's executable", chrome: filepath.Join(build, exe), want: build},
		{name: "an executable in another directory", chrome: filepath.Join(root, "elsewhere", exe)},
		{name: "a relative path", chrome: filepath.Join(b.dir, exe)},
		{name: "the build's directory", chrome: build},
	} {
		var got string
		ended := prereqtest.Run(t, func(tb testing.TB) { got = Unpacked(tb, tc.chrome) })
		refused := strings.Contains(ended.Fatal, "not where a Chrome for Testing build keeps its executable")
		if got != tc.want || refused != (tc.want == "") {
			t.Errorf("%s: Unpacked(%s) is %q (failed %q), want %q", tc.name, tc.chrome, got, ended.Fatal, tc.want)
		}
	}
}

// holdingEnv runs TestALaunchedBrowserEndsWithItsTestBinary as the binary it
// kills: one that launches the build the variable names and holds it.
const holdingEnv = "CHROMIUMTEST_HOLD_A_BROWSER"

// A LAUNCHED BROWSER ENDS WITH ITS TEST BINARY, however the binary ends: a
// binary killed outright runs no case's cleanup, as an interrupt, a signal, an
// out-of-memory kill or go test's -timeout runs none, and its browser — a few
// hundred megabytes and a profile directory — must not run on with nothing
// left to stop it. This binary runs itself again holding a browser LaunchPort
// launched, kills that run with SIGKILL, and the browser it held is gone. On
// both pins: one of the two ties is the browser's own — it exits once its
// DevTools pipe closes — and what a build does moves with its version.
//
// Mutation: drop both of what ties a browser to its binary — the DevTools
// pipe LaunchPort holds and, on Linux, the parent-death signal; either one
// alone holds it there — and the browser runs on — red.
func TestALaunchedBrowserEndsWithItsTestBinary(t *testing.T) {
	if chrome := os.Getenv(holdingEnv); chrome != "" {
		ep := LaunchPort(t, chrome)
		fmt.Printf("chromium %d\n", ep.Process.cmd.Process.Pid)
		procgrouptest.Hold()
		return
	}
	Each(t, endsWithItsBinary)
}

// endsWithItsBinary is TestALaunchedBrowserEndsWithItsTestBinary on one build.
func endsWithItsBinary(t *testing.T, chrome string) {
	ctx, cancel := context.WithTimeout(t.Context(), listenTimeout)
	defer cancel()
	//nolint:gosec // os.Args[0] is this test binary, run again on the one case.
	holder := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestALaunchedBrowserEndsWithItsTestBinary$",
		"-test.count=1")
	holder.Env = append(os.Environ(), holdingEnv+"="+chrome, procgrouptest.GORACE+"="+procgrouptest.StandInRaceOptions())
	out, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = holder.Process.Kill()
		_ = holder.Wait()
	})
	launched := make(chan int, 1)
	go func() {
		lines := bufio.NewScanner(out)
		for lines.Scan() {
			var pid int
			if _, err := fmt.Sscanf(lines.Text(), "chromium %d", &pid); err == nil {
				launched <- pid
			}
		}
	}()
	var browser int
	select {
	case browser = <-launched:
	case <-ctx.Done():
		t.Fatalf("the binary holding a browser never said it had launched one: %v", context.Cause(ctx))
	}
	if err := holder.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = holder.Wait()
	// A browser whose binary died is killed at once, or exits within a
	// second of its pipe's end; a minute is one that runs on.
	if !procgrouptest.AwaitGone(t, browser, time.Minute) {
		// Still its own, so its group is no stranger's to kill.
		_ = procgroup.Kill(browser)
		t.Fatalf("the browser %d its test binary launched ran on once the binary was killed", browser)
	}
}

// A LAUNCHED BROWSER REACHES NO HOST BUT LOOPBACK, so nothing it does unasked
// leaves the machine. A page served on 127.0.0.1 fetches localhost, a name
// under .localhost (which Chromium answers itself, with no network), ::1 and an
// address of RFC 5737's range, the last two written as addresses, and reports
// each outcome: only localhost is reached, and no server is asked for a host
// but 127.0.0.1 and localhost. A network may refuse the address off the
// machine too, so ::1, which only the rules refuse, is the address the
// mutation turns. On both pins, since what a build does moves with its version.
//
// Mutation: drop the resolver's rules from every launch's flags, and the name
// under .localhost and ::1 reach their servers — red.
func TestALaunchedBrowserReachesNoHostButLoopback(t *testing.T) {
	Each(t, func(t *testing.T, chrome string) {
		v4, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		v6, err := net.Listen("tcp6", "[::1]:0")
		if err != nil {
			t.Fatalf("listening on ::1, the loopback address the case fetches for the rules to refuse: %v", err)
		}
		port, port6 := v4.Addr().(*net.TCPAddr).Port, v6.Addr().(*net.TCPAddr).Port
		arms := map[string]string{
			"localhost":                  fmt.Sprintf("http://localhost:%d/fetched", port),
			"a name under .localhost":    fmt.Sprintf("http://reached.localhost:%d/fetched", port),
			"::1":                        fmt.Sprintf("http://[::1]:%d/fetched", port6),
			"an address off the machine": fmt.Sprintf("http://192.0.2.1:%d/fetched", port),
		}
		page := loopbackPage(t, arms)
		type ask struct{ server, host string }
		var mu sync.Mutex
		var asked []ask // every Host either server was asked for
		reports := make(chan [2]string, len(arms))
		serve(t, v4, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			asked = append(asked, ask{"127.0.0.1", r.Host})
			mu.Unlock()
			switch r.URL.Path {
			case "/":
				_, _ = w.Write([]byte(page))
			case "/report":
				select { // never held: the server's Close waits for its handlers
				case reports <- [2]string{r.URL.Query().Get("arm"), r.URL.Query().Get("outcome")}:
				default:
				}
			}
		})
		serve(t, v6, func(_ http.ResponseWriter, r *http.Request) {
			mu.Lock()
			asked = append(asked, ask{"::1", r.Host})
			mu.Unlock()
		})
		hosts := func() []ask {
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(asked)
		}

		LaunchPort(t, chrome, fmt.Sprintf("http://127.0.0.1:%d/", port))
		ctx, cancel := context.WithTimeout(t.Context(), listenTimeout)
		defer cancel()
		outcomes := map[string]string{}
		for len(outcomes) < len(arms) {
			select {
			case r := <-reports:
				want := map[bool]string{true: "reached", false: "refused"}[r[0] == "localhost"]
				if outcomes[r[0]] = r[1]; r[1] != want {
					t.Fatalf("the fetch of %s %s, want %s; the servers were asked for %+v", r[0], r[1], want, hosts())
				}
			case <-ctx.Done():
				t.Fatalf("the page reported %v of its %d fetches within %s; the servers were asked for %+v",
					outcomes, len(arms), listenTimeout, hosts())
			}
		}
		own, local := fmt.Sprintf("127.0.0.1:%d", port), fmt.Sprintf("localhost:%d", port)
		if !slices.Contains(hosts(), ask{"127.0.0.1", own}) {
			t.Errorf("the page was never served by 127.0.0.1: the servers were asked for %+v", hosts())
		}
		// Not which server was asked: the two listeners' ports may be one
		// number, and localhost then reaches the ::1 server as its own.
		for _, a := range hosts() {
			if a.host != own && a.host != local {
				t.Errorf("the %s server was asked for %s, which the browser must never reach: the servers were "+
					"asked for %+v", a.server, a.host, hosts())
			}
		}
	})
}

// loopbackPage is a document that fetches each arm's address, without CORS
// and past any cache, and reports to its own origin whether each was reached.
func loopbackPage(t *testing.T, arms map[string]string) string {
	t.Helper()
	encoded, err := json.Marshal(arms)
	if err != nil {
		t.Fatal(err)
	}
	return `<!doctype html><script>
for (const [arm, url] of Object.entries(` + string(encoded) + `)) {
  fetch(url, {mode: "no-cors", cache: "no-store"}).then(() => "reached", () => "refused").then((outcome) =>
    fetch("/report?arm=" + encodeURIComponent(arm) + "&outcome=" + outcome, {cache: "no-store"}));
}
</script>`
}

// serve answers l with h until t ends.
func serve(t *testing.T, l net.Listener, h http.HandlerFunc) {
	srv := httptest.NewUnstartedServer(h)
	_ = srv.Listener.Close()
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
}

// EACH PIN'S PROTOCOL NAMES ITS COMMANDS' PARAMETERS, from the one answer
// Protocol read: on both pins Methods is every command and event the answer
// describes, Parameters every command's parameter and no event's, each name's
// method among Methods, and each pin names hundreds of both; and an answer
// describing fewer than minDomains domains is refused, since it describes
// nothing a case could hold its names to.
//
// Mutations, each red: project parameters from events too; drop the
// minDomains check.
func TestEachPinsProtocolNamesItsCommandsParameters(t *testing.T) {
	t.Run("an answer describing too few domains", func(t *testing.T) {
		few := `{"domains":[{"domain":"Probe","commands":[{"name":"touch","parameters":[{"name":"x"}]}]}]}`
		if _, err := describe([]byte(few)); err == nil {
			t.Error("an answer describing one domain was taken, and it describes nothing")
		}
	})
	Each(t, func(t *testing.T, chrome string) {
		d := Protocol(t, chrome)
		type entry struct {
			Name       string `json:"name"`
			Parameters []struct {
				Name string `json:"name"`
			} `json:"parameters"`
		}
		var answer struct {
			Domains []struct {
				Domain   string  `json:"domain"`
				Commands []entry `json:"commands"`
				Events   []entry `json:"events"`
			} `json:"domains"`
		}
		if err := json.Unmarshal(d.Raw, &answer); err != nil {
			t.Fatalf("the answer Protocol kept is no description: %v", err)
		}
		methods, params, eventParams := map[string]bool{}, map[string]bool{}, map[string]bool{}
		for _, domain := range answer.Domains {
			for _, c := range domain.Commands {
				methods[domain.Domain+"."+c.Name] = true
				for _, p := range c.Parameters {
					params[domain.Domain+"."+c.Name+"."+p.Name] = true
				}
			}
			for _, e := range domain.Events {
				methods[domain.Domain+"."+e.Name] = true
				for _, p := range e.Parameters {
					eventParams[domain.Domain+"."+e.Name+"."+p.Name] = true
				}
			}
		}
		gotMethods, gotParams := d.Methods(), d.Parameters()
		if !maps.Equal(gotMethods, methods) {
			t.Errorf("Methods names %d, and the answer describes %d commands and events", len(gotMethods),
				len(methods))
		}
		if !maps.Equal(gotParams, params) {
			t.Errorf("Parameters names %d, and the answer describes %d parameters of commands", len(gotParams),
				len(params))
		}
		var ofEvents, unnamed []string
		for p := range gotParams {
			if eventParams[p] && !params[p] {
				ofEvents = append(ofEvents, p)
			}
			if method := p[:strings.LastIndex(p, ".")]; !gotMethods[method] {
				unnamed = append(unnamed, p)
			}
		}
		if len(ofEvents) > 0 {
			slices.Sort(ofEvents)
			t.Errorf("Parameters names %d parameters of events, which no caller sends, %s first", len(ofEvents),
				ofEvents[0])
		}
		if len(unnamed) > 0 {
			slices.Sort(unnamed)
			t.Errorf("Parameters names %d parameters whose method Methods does not name, %s first", len(unnamed),
				unnamed[0])
		}
		if len(gotMethods) < minNamed || len(gotParams) < minNamed {
			t.Errorf("the pin names %d methods and %d parameters: fewer than %d of either is a projection that "+
				"read nothing", len(gotMethods), len(gotParams), minNamed)
		}
	})
}

// minNamed is the fewest methods, and the fewest parameters, a pin's
// projections may name: both pins name about 900 methods and 1,190
// parameters of commands (2026-11-25), so fewer than 200 is a projection that
// read nothing.
const minNamed = 200
