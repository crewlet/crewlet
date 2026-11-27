// Package chromiumtest hands a case the Chromiums the suite drives and
// launches them: the two Chrome for Testing builds scripts/chromium-pin.env
// pins, the newest and the floor ([browser.MinChromiumMajor]), and never
// whichever Chrome the machine has. A case that drives Chromium drives both
// ([Each]), so what it asserts holds at both ends of what the engine accepts —
// unless its doc says why the floor adds nothing to its claim, and it drives
// the newest alone ([Newest]).
//
// # Where the builds come from
//
// Every make target that runs the whole suite (`test`, `test-solo`,
// `test-norace`) fetches both (`make chromium`, the one recipe
// scripts/chromium.sh) and names them in CREWLET_TEST_CHROMIUM and
// CREWLET_TEST_CHROMIUM_FLOOR. A build that is not named is a missing
// prerequisite by internal/prereqtest's one rule: it fails the case under
// those targets, and skips it only in a bare `go test` outside make. A
// build that is named is held to its pin's major — the newest at or above
// MinChromiumMajor, the floor of MinChromiumMajor exactly, since a newer build
// in the floor's place is a floor nobody is testing — by what it answers to
// --version.
//
// A case whose browser something else launches — the box helper's Playwright,
// which finds a build in a browsers directory of its own layout — reads a
// pin's build through [Unpacked], the directory its zip unpacked it into, and
// [Version], the four numbers it answered to --version, and hands that browser
// [LoopbackOnly], the rules every launch here resolves hosts by.
//
// # How a launch runs
//
// [LaunchPort] opens a DevTools port on loopback, which the artifact render's
// client dials, as it dials the Chromium an operator runs, and through which
// [Protocol] reads a pin's own description of its protocol, the one reader of
// it, so no second decode drifts from the first: a case reads a projection of
// the answer ([Description.Methods], [Description.Parameters]) or decodes a
// view of its own from its bytes. Each launch runs in a process group of its
// own, with a profile in the case's temporary directory, and is killed with its
// whole group and waited for when the case ends. It ends with its test binary
// too, however the binary ends — interrupted, signalled, killed for memory, or
// at go test's -timeout, none of which runs a case's cleanup — rather than run
// on with a few hundred megabytes and a profile directory and nothing left to
// stop it: every launch holds Chromium's DevTools pipe, which nothing writes,
// and exits once its ends close, and on Linux the kernel kills it the moment
// its binary dies. Its flags, each for a reason:
//
//   - --headless=new: the only headless mode a Chrome for Testing `chrome`
//     has;
//   - --no-sandbox: whether Chromium's own sandbox starts is a property of the
//     host (Ubuntu from 23.10, GitHub's runners among them, restricts the
//     unprivileged user namespaces it needs), and no case here asserts it;
//   - --no-first-run and --no-default-browser-check: nothing waits on a
//     question nobody answers;
//   - --host-resolver-rules, mapping every host but localhost and
//     127.0.0.1 to nothing: the browser reaches no other host, by name or by
//     address, so nothing it does unasked leaves the machine — without it an
//     idle Chromium reached Google's servers on every run, the flags below
//     notwithstanding — and a case serves its pages on those two;
//   - --disable-background-networking, --disable-component-update and
//     --disable-sync: the background work a flag reaches is never started,
//     and what none reaches the resolver's rules refuse;
//   - --password-store=basic and --use-mock-keychain: macOS never prompts for
//     its keychain and hangs a case;
//   - about:blank, last, unless a case names its first tab's address among a
//     launch's args: the first tab opens on nothing, where it would open the
//     new tab page, and a headless Chromium opens one tab from its command
//     line alone.
//
// Real-browser cases run one Chromium at a time — no t.Parallel among them —
// since each takes a few hundred megabytes and a core beside the rest of a
// four-vCPU shard.
//
// Test support (the name ends in `test`), so internal/fixedwait reads it as
// test code: it holds no fixed wait, and every bound in it is a context's.
package chromiumtest

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/procgroup"
)

// The variables the make targets that run the whole suite name the two builds
// in (the Makefile's CHROMIUM_ENV, from scripts/chromium.sh); nothing the
// engine runs reads them.
const (
	newestEnv = "CREWLET_TEST_CHROMIUM"
	floorEnv  = "CREWLET_TEST_CHROMIUM_FLOOR"
)

// remedy is what makes a pin's build present, for the words of a case that
// lacks it: its loop runs the case's own package.
func remedy(pin string) string {
	return fmt.Sprintf("`make chromium` fetches it and `scripts/chromium.sh %s` prints its path, which the make "+
		"targets that run the whole suite name here; for this package alone, as three statements, each "+
		"substitution its own: newest=$(scripts/chromium.sh newest) && floor=$(scripts/chromium.sh floor) && "+
		"%s=\"$newest\" %s=\"$floor\" go test %s", pin, newestEnv, floorEnv, testedPackage())
}

// testedPackage is the package this test binary tests, as go test names it
// from the module's root: a test binary's build info gives its main package
// the tested package's path with .test after it.
var testedPackage = sync.OnceValue(func() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "<the case's package>"
	}
	tested := strings.TrimSuffix(bi.Path, ".test")
	if rel, ok := strings.CutPrefix(tested, bi.Main.Path+"/"); ok {
		return "./" + rel + "/"
	}
	return tested
})

// Newest is the newest pinned Chromium, Chrome for Testing's Stable: an
// executable at or above browser.MinChromiumMajor.
func Newest(t testing.TB) string {
	t.Helper()
	chrome := prereqtest.Named(t, newestEnv, "the newest pinned Chromium, which every case that drives Chromium "+
		"drives", remedy("newest"))
	if v := versionOf(t, newestEnv, "newest", chrome); v.major < browser.MinChromiumMajor {
		t.Fatalf("%s=%s is Chromium %s, below browser.MinChromiumMajor %d, the oldest the engine accepts: %s",
			newestEnv, chrome, v.version, browser.MinChromiumMajor, remedy("newest"))
	}
	return chrome
}

// Floor is the floor's pinned Chromium, the newest patch of
// browser.MinChromiumMajor: an executable of that major exactly, since a newer
// one in its place is a floor nobody is testing.
func Floor(t testing.TB) string {
	t.Helper()
	chrome := prereqtest.Named(t, floorEnv, "the floor's pinned Chromium, the oldest the engine accepts, which every "+
		"case that drives Chromium drives unless its doc says why the floor adds nothing to its claim", remedy("floor"))
	if v := versionOf(t, floorEnv, "floor", chrome); v.major != browser.MinChromiumMajor {
		t.Fatalf("%s=%s is Chromium %s, and the floor is major %d (browser.MinChromiumMajor): a floor nobody is "+
			"testing — %s", floorEnv, chrome, v.version, browser.MinChromiumMajor, remedy("floor"))
	}
	return chrome
}

// Each runs f once on each pinned Chromium, as the subtests newest and floor.
func Each(t *testing.T, f func(t *testing.T, chrome string)) {
	t.Helper()
	t.Run("newest", func(t *testing.T) { f(t, Newest(t)) })
	t.Run("floor", func(t *testing.T) { f(t, Floor(t)) })
}

// Version is the four numbers chrome, a pin's build as [Newest] or [Floor]
// handed it back, answered to --version: the answer they held it to, asked
// once a test binary, so no case reads a build's version a second way.
func Version(t testing.TB, chrome string) string {
	t.Helper()
	for _, pin := range []struct{ env, name string }{{newestEnv, "newest"}, {floorEnv, "floor"}} {
		if chrome != "" && os.Getenv(pin.env) == chrome {
			return versionOf(t, pin.env, pin.name, chrome).version
		}
	}
	t.Fatalf("%s is neither pin's build (%s, %s): Version reads the answer a pin's build gave", chrome, newestEnv,
		floorEnv)
	return ""
}

// builds is where a Chrome for Testing build keeps its executable inside the
// directory its zip unpacks into, by platform: chromium.sh's layout, which
// Playwright's browsers directory repeats.
var builds = map[string]struct{ dir, exe string }{
	"linux/amd64":  {"chrome-linux64", "chrome"},
	"linux/arm64":  {"chrome-linux-arm64", "chrome"},
	"darwin/amd64": {"chrome-mac-x64", "Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing"},
	"darwin/arm64": {"chrome-mac-arm64", "Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing"},
}

// Unpacked is the directory chrome's build was unpacked into,
// chrome-<platform> as its zip names it, read from the executable's path. A
// case laying a browsers directory out links it there: never a copy of a few
// hundred megabytes, and never a link to the whole build, beside which a
// launch may write (Playwright marks a directory it validated), while
// scripts/chromium.sh promises an intact build is never written into.
func Unpacked(t testing.TB, chrome string) string {
	t.Helper()
	b, ok := builds[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		t.Fatalf("Chrome for Testing publishes no build for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	dir := strings.TrimSuffix(chrome, string(filepath.Separator)+filepath.FromSlash(b.exe))
	if !filepath.IsAbs(chrome) || dir == chrome || filepath.Base(dir) != b.dir {
		t.Fatalf("%s is not where a Chrome for Testing build keeps its executable, /…/%s/%s", chrome, b.dir, b.exe)
	}
	return dir
}

// version is what a build said to --version.
type version struct {
	version string
	major   int
}

// versionAnswer is the last word of a build's --version line: Chrome for
// Testing's four numbers ("Google Chrome for Testing 157.0.8103.38").
var versionAnswer = regexp.MustCompile(`([0-9]+)\.[0-9]+\.[0-9]+\.[0-9]+$`)

// versionTimeout bounds one --version: a build answers in well under a
// second, so a minute is a hang, never a slow machine.
const versionTimeout = time.Minute

// versions is each build's --version, asked once per test binary.
var versions sync.Map // path → func() (version, error)

// versionStderr is how much of what a build that could not answer --version
// printed a failure carries: the end of it, where a loader or a build says
// why, in a line or two.
const versionStderr = 1 << 10

// versionOf is the version chrome, pin's build, says it is, or t fails naming
// env, what the build printed if it could not answer, and what fetches pin's
// build again.
func versionOf(t testing.TB, env, pin, chrome string) version {
	t.Helper()
	ask, _ := versions.LoadOrStore(chrome, sync.OnceValues(func() (version, error) {
		ctx, cancel := context.WithTimeout(context.Background(), versionTimeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, chrome, "--version").Output()
		if err != nil {
			return version{}, cannotStart(chrome, err)
		}
		said := strings.TrimSpace(string(out))
		m := versionAnswer.FindStringSubmatch(said)
		if m == nil {
			return version{}, fmt.Errorf("it answered --version with %q, which names no Chrome for Testing version", said)
		}
		major, err := strconv.Atoi(m[1])
		if err != nil {
			return version{}, err
		}
		return version{version: m[0], major: major}, nil
	}))
	v, err := ask.(func() (version, error))()
	if err != nil {
		t.Fatalf("%s=%s: %v; %s", env, chrome, err, remedy(pin))
	}
	return v
}

// cannotStart is a build's failure to answer --version: what it printed last,
// and on Linux where the list of what it links is, as scripts/chromium.sh
// says it when the same build fails its own check there.
func cannotStart(chrome string, err error) error {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return err
	}
	if said := strings.TrimSpace(string(exit.Stderr)); said != "" {
		if len(said) > versionStderr {
			said = "..." + said[len(said)-versionStderr:]
		}
		err = fmt.Errorf("%w, saying: %s", err, said)
	}
	if runtime.GOOS == "linux" {
		dir := filepath.Dir(chrome)
		err = fmt.Errorf("%w (on Linux a build that cannot start lacks a library: %s, for Debian and Ubuntu, "+
			"or %s, for Fedora and RHEL, names every package it needs)", err,
			filepath.Join(dir, "deb.deps"), filepath.Join(dir, "rpm.deps"))
	}
	return err
}

// LoopbackOnly is the host resolver's rules for every launch: every host — a
// name, and an address written as one, which the rules read too — resolves
// to nothing but localhost and 127.0.0.1, which resolve as they always do.
const LoopbackOnly = "MAP * ~NOTFOUND , EXCLUDE localhost , EXCLUDE 127.0.0.1"

// flags is what every launch runs Chromium with, each flag's reason in the
// package doc; profile is the case's own directory.
func flags(profile string) []string {
	return []string{
		"--headless=new",
		"--no-sandbox",
		"--no-first-run",
		"--no-default-browser-check",
		"--host-resolver-rules=" + LoopbackOnly,
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-sync",
		"--password-store=basic",
		"--use-mock-keychain",
		"--user-data-dir=" + profile,
	}
}

// waitDelay is how long a killed Chromium's output is waited for once its
// group is dead: every process of it is gone with the group, so its stderr
// closes at once, and the delay only turns a process that escaped the group
// into a failure rather than a hang.
const waitDelay = 10 * time.Second

// Process is a launched Chromium, its whole process group.
//
// Nothing waits for it before its case ends, and that is what makes every
// signal to its group safe: a group is signalled by its leader's pid, which is
// never recycled while the leader is unreaped, and only the case's own cleanup
// reaps it, after its last kill.
type Process struct {
	cmd *exec.Cmd
}

// Kill kills the Chromium and every process it started, at once, as an
// out-of-memory kill or a crash would end it. The case still reaps it when it
// ends.
func (p *Process) Kill() error { return procgroup.Kill(p.cmd.Process.Pid) }

// tail keeps the last bytes a launch wrote to its stderr, for a case that
// failed to say why.
type tail struct {
	mu  sync.Mutex
	buf []byte
}

// tailBytes is how much of a Chromium's stderr a failed case reports: the
// last screen or two, where its reason is.
const tailBytes = 8 << 10

func (w *tail) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if over := len(w.buf) - tailBytes; over > 0 {
		w.buf = append(w.buf[:0], w.buf[over:]...)
	}
	return len(p), nil
}

func (w *tail) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}

// command is chrome under every launch's flags and args, its first tab on
// about:blank or on the one address args name (a headless Chromium opens one
// tab from its command line), holding Chromium's DevTools pipe, which nothing
// writes, on the pipes p hands it, in a process group of its own that t's end
// kills, and tied to this test binary (tie).
//
// The launch's cleanup kills that group, and only then reaps the leader. The
// context's Cancel stays the standard library's, which kills the leader alone
// and refuses one already reaped: t's context ends just before the cleanup
// runs, and nothing orders os/exec's Cancel before the cleanup's Wait, so a
// Cancel that signalled the group by the leader's pid could reach a recycled
// one.
func command(t testing.TB, p *pipes, chrome string, args ...string) *exec.Cmd {
	argv := append(flags(t.TempDir()), "--remote-debugging-pipe")
	argv = append(argv, args...)
	if !slices.ContainsFunc(args, func(arg string) bool { return !strings.HasPrefix(arg, "-") }) {
		argv = append(argv, "about:blank")
	}
	cmd := exec.CommandContext(t.Context(), chrome, argv...)
	cmd.ExtraFiles = []*os.File{p.chromeReads, p.chromeWrites}
	procgroup.Set(cmd)
	tie(cmd)
	cmd.WaitDelay = waitDelay
	return cmd
}

// pipes are a launch's two DevTools pipes: fd 3, which Chromium reads, and
// fd 4, which it writes. Every launch holds them, and that is what ends a
// Chromium with the test binary that started it whatever ends the binary — an
// interrupt, a signal, an out-of-memory kill, go test's -timeout, none of
// which runs a case's cleanup — on every platform: the binary holds its ends
// until the case's cleanup, the kernel closes them when the binary goes, and a
// Chromium whose pipe is closed exits, rather than run on with a few hundred
// megabytes and a profile directory and nothing left to stop it. On Linux the
// kernel's parent-death signal ends it sooner (tie).
type pipes struct {
	chromeReads, chromeWrites *os.File // Chromium's ends, its own once it starts
	weRead, weWrite           *os.File // the test binary's
}

// newPipes makes a launch's pipes, every end closed when t ends unless
// something closed it first: the test binary's ends, which ends a Chromium
// still reading them, and Chromium's, which a launch that failed to start
// left open.
func newPipes(t testing.TB) *pipes {
	t.Helper()
	var p pipes
	var err error
	if p.chromeReads, p.weWrite, err = os.Pipe(); err != nil {
		t.Fatal(err)
	}
	if p.weRead, p.chromeWrites, err = os.Pipe(); err != nil {
		_ = p.chromeReads.Close()
		_ = p.weWrite.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p.started()
		_ = p.weWrite.Close()
		_ = p.weRead.Close()
	})
	return &p
}

// started closes Chromium's ends once it holds its own copies: then its exit
// is the pipes' end. A second close of an end does nothing.
func (p *pipes) started() {
	_ = p.chromeReads.Close()
	_ = p.chromeWrites.Close()
}

// Endpoint is a Chromium launched with a DevTools port on loopback.
type Endpoint struct {
	// Browser is its browser websocket, as it printed it:
	// ws://127.0.0.1:<port>/devtools/browser/<id>.
	Browser string
	// HTTP is its DevTools HTTP base, http://127.0.0.1:<port>, which
	// answers /json/version and /json/protocol.
	HTTP string
	// Process is the Chromium itself.
	Process *Process
}

// listening is the line Chromium prints to stderr once its port is open.
var listening = regexp.MustCompile(`DevTools listening on (ws://([^/\s]+)/devtools/browser/\S+)`)

// stderrLine is the longest stderr line a launch reads: far past any line
// Chromium prints, so the reader drains its stderr to the end.
const stderrLine = 1 << 20

// listenTimeout bounds a launch's wait for its port: a Chromium opens it in
// well under ten seconds, even on a loaded four-vCPU runner, so a minute is a
// launch that will never print it.
const listenTimeout = time.Minute

// LaunchPort launches chrome with a DevTools port on loopback, chosen by
// Chromium (--remote-debugging-port=0), and returns where it listens once it
// has printed so; it is ended with t. It holds the DevTools pipe too, which
// nothing writes, to end the browser with the test binary (pipes). args come
// before the first tab's about:blank, which an address among them replaces.
func LaunchPort(t testing.TB, chrome string, args ...string) Endpoint {
	t.Helper()
	p := newPipes(t)
	cmd := command(t, p, chrome, append([]string{"--remote-debugging-port=0"}, args...)...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", chrome, err)
	}
	p.started()
	kept := &tail{}
	found := make(chan [2]string, 1)
	read := make(chan struct{}) // closed once stderr has ended: Chromium exited
	go func() {
		defer close(read)
		lines := bufio.NewScanner(stderr)
		lines.Buffer(make([]byte, 0, 64<<10), stderrLine)
		for lines.Scan() {
			_, _ = kept.Write(append(lines.Bytes(), '\n'))
			if m := listening.FindStringSubmatch(lines.Text()); m != nil {
				select {
				case found <- [2]string{m[1], m[2]}:
				default:
				}
			}
		}
	}()
	t.Cleanup(func() {
		_ = procgroup.Kill(cmd.Process.Pid)
		// Wait reads nothing of stderr, so its reader finishes first;
		// with the group dead that is at once, and the bound is for a
		// process that escaped it.
		ctx, cancel := context.WithTimeout(context.Background(), waitDelay)
		defer cancel()
		select {
		case <-read:
		case <-ctx.Done():
		}
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("%s's stderr ended with:\n%s", chrome, kept)
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), listenTimeout)
	defer cancel()
	select {
	case m := <-found:
		return Endpoint{Browser: m[0], HTTP: "http://" + m[1], Process: &Process{cmd: cmd}}
	case <-read:
		t.Fatalf("%s exited before it opened its DevTools port; its stderr:\n%s", chrome, kept)
	case <-ctx.Done():
		t.Fatalf("%s printed no DevTools port within %s; its stderr:\n%s", chrome, listenTimeout, kept)
	}
	return Endpoint{}
}

// Protocol is chrome's own description of its protocol, its /json/protocol
// read from a launch under t: per call, since a launch runs under its caller's
// t, whose end ends it. An answer that describes fewer than minDomains
// domains fails t.
func Protocol(t testing.TB, chrome string) Description {
	t.Helper()
	ep := LaunchPort(t, chrome)
	ctx, cancel := context.WithTimeout(t.Context(), listenTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.HTTP+"/json/protocol", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := httpx.Client(0).Do(req)
	if err != nil {
		t.Fatalf("%s/json/protocol: %v", ep.HTTP, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s/json/protocol answered %s", ep.HTTP, resp.Status)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s/json/protocol: %v", ep.HTTP, err)
	}
	d, err := describe(raw)
	if err != nil {
		t.Fatalf("%s/json/protocol: %v", ep.HTTP, err)
	}
	return d
}

// Description is a pin's own description of its protocol, as [Protocol] read
// it: the answer whole, for a case that decodes a view of its own, and the
// projections the module's cases read.
type Description struct {
	// Raw is the /json/protocol answer as the pin sent it.
	Raw     []byte
	domains []describedDomain
}

// describedDomain is what the projections read of a domain: its name, and
// each command's and event's name with its parameters' names.
type describedDomain struct {
	Domain   string             `json:"domain"`
	Commands []describedMessage `json:"commands"`
	Events   []describedMessage `json:"events"`
}

type describedMessage struct {
	Name       string `json:"name"`
	Parameters []struct {
		Name string `json:"name"`
	} `json:"parameters"`
}

// describe decodes a /json/protocol answer, refusing one that describes
// fewer than minDomains domains.
func describe(raw []byte) (Description, error) {
	var desc struct {
		Domains []describedDomain `json:"domains"`
	}
	if err := json.Unmarshal(raw, &desc); err != nil {
		return Description{}, err
	}
	if len(desc.Domains) < minDomains {
		return Description{}, fmt.Errorf("it described %d domains, fewer than %d: it is describing nothing",
			len(desc.Domains), minDomains)
	}
	return Description{Raw: raw, domains: desc.Domains}, nil
}

// minDomains is the fewest domains a pin's protocol may describe: both pins
// describe nearly sixty, so an answer of fewer than twenty is one that
// describes nothing the walk could hold Go to.
const minDomains = 20

// Methods is every command and event d describes, each spelled as its domain
// and its name joined by a dot.
func (d Description) Methods() map[string]bool {
	methods := map[string]bool{}
	for _, domain := range d.domains {
		for _, m := range slices.Concat(domain.Commands, domain.Events) {
			methods[domain.Domain+"."+m.Name] = true
		}
	}
	return methods
}

// Parameters is every parameter of every command d describes, spelled as the
// command and the parameter joined by a dot: what a caller may put in a
// command's params. An event's parameters are what the browser sends, so
// none is here.
func (d Description) Parameters() map[string]bool {
	params := map[string]bool{}
	for _, domain := range d.domains {
		for _, c := range domain.Commands {
			for _, p := range c.Parameters {
				params[domain.Domain+"."+c.Name+"."+p.Name] = true
			}
		}
	}
	return params
}
