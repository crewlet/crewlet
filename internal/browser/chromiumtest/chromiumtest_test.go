package chromiumtest

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
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

	"github.com/crewlet/crewlet/internal/browser/cdp"
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
// build is a missing prerequisite by internal/prereqtest's rule: under the
// requirement the make targets that run the suite set it fails, naming its
// variable and `make chromium`; unset otherwise, it skips. Named, it must be an
// absolute path to an executable, and what it answers to --version decides: the
// newest must be at or above cdp.MinMajor, and the floor of cdp.MinMajor
// exactly, since a newer build in the floor's place is a floor nobody is
// testing. A build that cannot answer at all fails saying what it printed,
// what fetches it again, and on Linux the list of the packages it links. A
// good build is handed back.
//
// Not parallel: it sets the variables the helpers read.
//
// Mutations: drop the floor's exact-major check, and a build of MinMajor+1
// stands for the floor — red; drop the newest's check against MinMajor, and a
// build of MinMajor-1 stands for the newest — red; fail a build that cannot
// answer with its exit status alone, and the failure says neither what it
// printed nor what to do — red.
func TestAPinIsNamedRequiredOrSkipped(t *testing.T) {
	floorBuild := stub(t, fmt.Sprintf("%d.0.8010.52", cdp.MinMajor))
	newestBuild := stub(t, fmt.Sprintf("%d.0.8103.38", cdp.MinMajor+4))
	belowTheFloor := stub(t, fmt.Sprintf("%d.0.7977.82", cdp.MinMajor-1))
	aMajorUp := stub(t, fmt.Sprintf("%d.0.8037.92", cdp.MinMajor+1))
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
			fatal: "names nothing this case can run"},
		{name: "no Chrome for Testing version", env: newestEnv, require: "1", value: notChromium, find: Newest,
			fatal: "names no Chrome for Testing version"},
		{name: "a newest below the floor", env: newestEnv, require: "1", value: belowTheFloor, find: Newest,
			fatal: fmt.Sprintf("below cdp.MinMajor %d", cdp.MinMajor)},
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
