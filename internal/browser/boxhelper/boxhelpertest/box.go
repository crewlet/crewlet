package boxhelpertest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/browser/boxhelper"
	"github.com/crewlet/crewlet/internal/browser/chromiumtest"
	"github.com/crewlet/crewlet/internal/procgroup"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
	"github.com/crewlet/crewlet/internal/shellword"
)

// Env is what a case of the helper runs under: a Node, the npm it brings, and
// the pinned Chromium the box's browser is.
type Env struct {
	// Node is the node the case runs the helper's modules under, the box's
	// bin/node.
	Node string
	// NPM is the npm the box brings, its bin/npm, run under the box's node as
	// a box's own npm is.
	NPM string
	// Chrome is the pinned Chromium the box's browser is, "" for a case that
	// drives none.
	Chrome string
}

// Each runs f once under each end of what a box may bring, as the subtests
// newest, the machine's node and npm with the newest pinned Chromium
// ([Newest]), and floor, the floor's node and npm ([Floor], [FloorNPM]) with
// the floor's Chromium: the oldest Node with the oldest Chromium, the worst
// pairing the floors admit. The names are chromiumtest.Each's.
func Each(t *testing.T, f func(t *testing.T, env Env)) {
	t.Helper()
	t.Run("newest", func(t *testing.T) { f(t, Newest(t)) })
	t.Run("floor", func(t *testing.T) {
		f(t, Env{Node: Floor(t), NPM: FloorNPM(t), Chrome: chromiumtest.Floor(t)})
	})
}

// EachNode runs f once under each Node a box may bring, each with its own
// npm and no Chromium, as the subtests newest, the machine's node and npm
// ([Node], [NPM]), and floor, the floor's pinned build and the npm it bundles
// ([Floor], [FloorNPM]): for a case of the helper that drives no browser.
func EachNode(t *testing.T, f func(t *testing.T, env Env)) {
	t.Helper()
	t.Run("newest", func(t *testing.T) { f(t, Env{Node: Node(t), NPM: NPM(t)}) })
	t.Run("floor", func(t *testing.T) { f(t, Env{Node: Floor(t), NPM: FloorNPM(t)}) })
}

// Newest is the newest pair alone, the machine's node and npm with the newest
// pinned Chromium, for a case whose doc says why the floor adds nothing to
// its claim.
func Newest(t testing.TB) Env {
	t.Helper()
	return Env{Node: Node(t), NPM: NPM(t), Chrome: chromiumtest.Newest(t)}
}

// Route is how a case lays its Chromium out in the box, for ensure to find.
type Route int

const (
	// Expected lays the pin's build out as Playwright's installer lays the
	// build it expects, in the box's own browsers directory, its version the
	// one the case's config expects.
	Expected Route = iota
	// OnPath puts the pin's build on the box's PATH as chromium.
	OnPath
	// ExpectedAndOnPath does both.
	ExpectedAndOnPath
	// NoChromium lays out none.
	NoChromium
)

// Cache is the npm cache a box starts with.
type Cache int

const (
	// CacheSuite is a copy of the npm cache the suite's tree filled
	// ([Tree]), at the box's ~/.npm, so the box installs the tree offline.
	CacheSuite Cache = iota
	// CacheNone is an empty cache, so npm fetches every package from the
	// registry the case names.
	CacheNone
)

// Options is how a case's box differs from what the engine writes.
type Options struct {
	// Sandbox is the cell's Chromium sandbox; unset, waived, so a case not
	// about the sandbox passes on a host whose own will not start.
	Sandbox browser.ChromiumSandbox
	// Route is how the box's Chromium is laid out.
	Route Route
	// Cache is the npm cache the box starts with.
	Cache Cache
	// Config holds config.json's keys the case sets over the engine's values,
	// merged into objects key by key: a bound shortened where the case's
	// subject is that bound.
	Config map[string]any
}

// Box is a seat's box on disk, as the engine leaves one before the helper
// first runs in it: the helper written, and the lock's two files beside it.
type Box struct {
	// Home is the box's home.
	Home string
	// Folder is the helper's folder in it, boxhelper.Folder(Home).
	Folder string
	// GlobalNPMRC is the box's global npmrc, empty, so the machine's own never
	// reaches the box: etc/npmrc under the global prefix its environment
	// names, where its npm finds it as an image's npm finds its own under the
	// prefix it was installed in, and where a case writes what an image's
	// global npmrc would say.
	GlobalNPMRC string
	// Workspace is the job's working directory in it, the root its shim names,
	// as a harness runs there.
	Workspace string

	env  Env
	opts Options
}

// homePrefix starts the name of every home Start makes: short, since a box's
// sockets lie under its home and macOS bounds a socket's path.
const homePrefix = "cb"

// ownerFile is a home's record of the test binary that made it, and
// supervisorFile of the supervisor last started in it.
const (
	ownerFile      = "owner"
	supervisorFile = "supervisor"
)

// npmPrefix is the name of the box's global npm prefix in its home, and
// npmCalls of where a stand-in npm records its command lines.
const (
	npmPrefix = ".npm-prefix"
	npmCalls  = "npm-calls"
)

// refusedRegistry is the registry a box's npm is pointed at unless a case
// names one: a port nothing listens on, so a box installs from its cache or
// fails in about 2.4 s, and never reads the network.
const refusedRegistry = "http://127.0.0.1:1/"

// self is this test binary, as every home it makes names it.
var self = sync.OnceValues(func() (procgroup.Leader, error) { return procgroup.Identify(os.Getpid()) })

// swept is the sweep of the temporary directory's homes, once a test binary.
var swept = sync.OnceValue(func() error { return sweep(os.TempDir()) })

// Start lays a box out on disk for a case run under env, as o says: a home of
// its own holding the box's bin directory (env's node and npm, and the pin's
// build where o puts it on PATH), its Chromium laid out by o's route, its npm
// cache and an empty global npmrc, and the helper written into it
// ([Box.Write]). The case's end stops any supervisor the box runs, kills what
// a failed case left running on it, and removes it.
func Start(t testing.TB, env Env, o Options) *Box {
	t.Helper()
	if err := swept(); err != nil {
		t.Logf("sweeping the homes of test binaries that are gone: %v", err)
	}
	owner, err := self()
	if err != nil {
		t.Fatalf("identifying this test binary, which its homes name: %v", err)
	}
	home, err := os.MkdirTemp("", homePrefix) //nolint:usetesting // a test's name is too long for the box's sockets
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// After its supervisor's stop: what still runs on the box would write
		// its home back, with no owner file for a later sweep to judge.
		killed, err := stopWhatRuns(home)
		if err != nil {
			t.Errorf("stopping what the box still runs: %v", err)
		}
		for _, pid := range killed {
			procgrouptest.AwaitGone(t, pid, goneWithin)
		}
		if err := os.RemoveAll(home); err != nil {
			t.Errorf("removing the box's home: %v", err)
		}
	})
	if err := os.WriteFile(filepath.Join(home, ownerFile), []byte(owner.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if o.Sandbox == "" {
		o.Sandbox = browser.SandboxWaived
	}
	b := &Box{Home: home, Folder: boxhelper.Folder(home), GlobalNPMRC: filepath.Join(home, npmPrefix, "etc", "npmrc"),
		Workspace: filepath.Join(home, "workspace"), env: env, opts: o}
	t.Cleanup(func() { b.Stop(t) })
	b.layOut(t)
	b.Write(t)
	return b
}

// layOut makes the box's bin directory, linking its node and npm, its
// workspace, its npm cache, and its empty global npmrc, and lays its Chromium
// out.
func (b *Box) layOut(t testing.TB) {
	t.Helper()
	bin := filepath.Join(b.Home, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(b.Workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	link(t, b.env.Node, filepath.Join(bin, "node"))
	link(t, b.env.NPM, filepath.Join(bin, "npm"))
	if b.opts.Cache == CacheSuite {
		if err := os.CopyFS(filepath.Join(b.Home, ".npm"), os.DirFS(filepath.Join(Tree(t), "npm-cache"))); err != nil {
			t.Fatalf("copying the suite tree's npm cache into the box: %v", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(b.GlobalNPMRC), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.GlobalNPMRC, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if b.env.Chrome == "" {
		return
	}
	if r := b.opts.Route; r == Expected || r == ExpectedAndOnPath {
		build := filepath.Join(b.Home, "ms-playwright", "chromium-"+browser.ExpectedChromiumRevision)
		if err := os.MkdirAll(build, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(build, "INSTALLATION_COMPLETE"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		unpacked := chromiumtest.Unpacked(t, b.env.Chrome)
		link(t, unpacked, filepath.Join(build, filepath.Base(unpacked)))
	}
	if r := b.opts.Route; r == OnPath || r == ExpectedAndOnPath {
		link(t, b.env.Chrome, filepath.Join(bin, "chromium"))
	}
}

// link links name to target, failing t.
func link(t testing.TB, target, name string) {
	t.Helper()
	if err := os.Symlink(target, name); err != nil {
		t.Fatal(err)
	}
}

// Write writes the helper into the box through [disk], as the engine does on
// every launch, for the case's sandbox, then sets over the engine's config
// the case's own browsers directory alone to search, the version of the pin's
// build as the one expected, the switch that holds the browser to loopback,
// and the case's own keys.
func (b *Box) Write(t testing.TB) {
	t.Helper()
	if err := boxhelper.Write(t.Context(), disk{}, b.Home, boxhelper.Settings{Sandbox: b.opts.Sandbox}); err != nil {
		t.Fatal(err)
	}
	config := b.Config(t)
	chromium := config["chromium"].(map[string]any)
	search := chromium["search"].(map[string]any)
	search["playwright"] = []string{filepath.Join(b.Home, "ms-playwright")}
	search["chrome"] = []string{}
	if b.env.Chrome != "" {
		chromium["expected_version"] = chromiumtest.Version(t, b.env.Chrome)
	}
	switches := config["switches"].(map[string]any)
	switches["args"] = append(switches["args"].([]any), "--host-resolver-rules="+chromiumtest.LoopbackOnly)
	merge(config, b.opts.Config)
	b.SetConfig(t, config)
}

// merge sets over's keys in into, an object of one merged into an object of
// the other key by key, and anything else in its place.
func merge(into, over map[string]any) {
	for key, v := range over {
		sub, ok := v.(map[string]any)
		if have, isObject := into[key].(map[string]any); ok && isObject {
			merge(have, sub)
			continue
		}
		into[key] = v
	}
}

// Config is the box's config.json, decoded.
func (b *Box) Config(t testing.TB) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(b.Folder, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	return config
}

// SetConfig writes config as the box's config.json, as the engine's writes
// leave it.
func (b *Box) SetConfig(t testing.TB, config map[string]any) {
	t.Helper()
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := (disk{}).WriteFile(t.Context(), filepath.Join(b.Folder, "config.json"), append(raw, '\n')); err != nil {
		t.Fatal(err)
	}
}

// Environ is the environment the box runs the helper in: its own bin
// directory alone on PATH, so its npm runs under its node and no case finds
// the machine's own programs; its home; the refused registry; and its global
// npm prefix, under which its npm finds its empty global npmrc. A case's own
// variables go after it, where the last of a name wins.
func (b *Box) Environ() []string {
	return []string{"PATH=" + filepath.Join(b.Home, "bin"), "HOME=" + b.Home, "NPM_CONFIG_REGISTRY=" + refusedRegistry,
		"NPM_CONFIG_PREFIX=" + filepath.Join(b.Home, npmPrefix)}
}

// StandInNPM puts a stand-in for npm at the box's bin/npm: an sh script that
// records each command line it is run with ([Box.NPMCalls]), answers one that
// carries --version with version, and otherwise runs body, where $REAL_NPM
// is the case's own npm and $DIR the directory it runs in, which npm ci
// installs into. The box's PATH is its bin alone, so the script finds the
// system's tools after it, and the real npm's `env node` still finds the
// box's node first.
func (b *Box) StandInNPM(t testing.TB, version, body string) {
	t.Helper()
	script := "#!/bin/sh\nPATH=\"$PATH:/usr/bin:/bin\"\nREAL_NPM=" + shellword.Quote(b.env.NPM) + "\n" +
		"printf '%s\\000' \"$@\" >>" + shellword.Quote(filepath.Join(b.Home, npmCalls)) + "\n" +
		"printf '\\n' >>" + shellword.Quote(filepath.Join(b.Home, npmCalls)) + "\n" +
		"for a; do [ \"$a\" = --version ] && { echo " + shellword.Quote(version) + "; exit 0; }; done\n" +
		"DIR=$PWD\n" + body + "\n"
	npm := filepath.Join(b.Home, "bin", "npm")
	if err := os.Remove(npm); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.WriteFile(npm, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// ensureWait bounds one ensure as the engine does, with a margin for the
// shell it runs under: past it, ensure has hung, never been slow.
const ensureWait = boxhelper.EnsureTimeout + 10*time.Second

// Ensure runs the helper's ensure in the box under the box's environment and
// extra, as `sh -c` and boxhelper.EnsureScript (never a login shell, whose
// profile may rebuild PATH), and reads its line with the engine's own
// parser. A supervisor it leaves running is recorded for the sweep.
func (b *Box) Ensure(t testing.TB, extra ...string) boxhelper.Ensure {
	t.Helper()
	stdout, stderr := b.EnsureOutput(t, extra...)
	e, err := boxhelper.ParseEnsure(stdout)
	if err != nil {
		t.Fatalf("ensure answered %q: %v; its stderr:\n%s", stdout, err, stderr)
	}
	if e.Verdict == browser.EnsureReady {
		b.recordSupervisor(t)
	}
	return e
}

// EnsureOutput runs ensure as [Box.Ensure] does and answers what it printed.
func (b *Box) EnsureOutput(t testing.TB, extra ...string) (stdout, stderr string) {
	t.Helper()
	stdout, stderr, err := b.RunEnsure(t.Context(), b.Home, extra...)
	if err != nil {
		t.Fatalf("ensure: %v; its stdout %q; its stderr:\n%s", err, stdout, stderr)
	}
	return stdout, stderr
}

// waitDelay bounds the wait for ensure's pipes to close once its shell has
// exited: at once, unless a child it left holds them, past which its output
// is cut rather than the case hung.
const waitDelay = time.Second

// RunEnsure runs ensure for the box whose home is home under the box's
// environment and extra, failing nothing: for a case that runs several at
// once, or one in a home it moved the box's folder to.
func (b *Box) RunEnsure(ctx context.Context, home string, extra ...string) (stdout, stderr string, err error) {
	ctx, cancel := context.WithTimeout(ctx, ensureWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", boxhelper.EnsureScript(home))
	cmd.Env = append(b.Environ(), extra...)
	cmd.Dir = b.Home
	cmd.WaitDelay = waitDelay
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	return out.String(), errOut.String(), err
}

// Supervisor is the box's supervisor as its supervisor.json records it.
type Supervisor struct {
	PID    int    `json:"pid"`
	State  string `json:"state"`
	Helper string `json:"helper"`
	Digest string `json:"launch_digest"`
	// Boot is drawn once a supervisor and kept across its relaunches.
	Boot     string `json:"boot"`
	Chromium *struct {
		PID     int    `json:"pid"`
		Version string `json:"version"`
		Build   string `json:"build"`
		Sandbox string `json:"sandbox"`
	} `json:"chromium"`
	Relaunches []int64 `json:"relaunches_ms"`
	Failure    *struct {
		Reason string `json:"reason"`
		Cause  string `json:"cause"`
	} `json:"failure"`
}

// Supervisor reads the box's supervisor.json, failing t if there is none.
func (b *Box) Supervisor(t testing.TB) Supervisor {
	t.Helper()
	s, ok := b.supervisor(t)
	if !ok {
		t.Fatalf("%s holds no supervisor.json", b.Folder)
	}
	return s
}

func (b *Box) supervisor(t testing.TB) (Supervisor, bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(b.Folder, "supervisor.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return Supervisor{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	var s Supervisor
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("%s's supervisor.json: %v", b.Folder, err)
	}
	return s, true
}

// recordSupervisor records the supervisor's identity in the home, for the
// sweep of a test binary that dies before its cleanup: ensure detaches it, so
// nothing else ties it to this binary.
func (b *Box) recordSupervisor(t testing.TB) {
	t.Helper()
	leader, err := procgroup.Identify(b.Supervisor(t).PID)
	if err != nil {
		t.Fatalf("identifying the box's supervisor: %v", err)
	}
	if err := os.WriteFile(filepath.Join(b.Home, supervisorFile), []byte(leader.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// goneWithin bounds a wait for a stopped process to go: past it, something
// holds it, never a slow machine.
const goneWithin = time.Minute

// Stop stops the box's supervisor, if one runs, as a TERM does, and waits for
// it and its Chromium to go. One that outlives its TERM fails t and is
// killed with its group, so no case leaves a supervisor running.
func (b *Box) Stop(t testing.TB) {
	t.Helper()
	s, ok := b.supervisor(t)
	if !ok || !b.supervising(s.PID) {
		return
	}
	if err := procgroup.Terminate(s.PID); err != nil && b.supervising(s.PID) {
		t.Errorf("stopping the box's supervisor: %v", err)
	}
	if !procgrouptest.AwaitGone(t, s.PID, goneWithin) {
		t.Errorf("the box's supervisor, pid %d, did not stop within %s of its TERM: killed", s.PID, goneWithin)
		_ = procgroup.Kill(s.PID)
		procgrouptest.AwaitGone(t, s.PID, goneWithin)
	}
	if s.Chromium != nil && s.Chromium.PID > 0 && !procgrouptest.AwaitGone(t, s.Chromium.PID, goneWithin) {
		t.Errorf("the box's Chromium, pid %d, outlived its supervisor by %s", s.Chromium.PID, goneWithin)
	}
}

// pollEvery paces a wait on what a box's supervisor records, which nothing
// announces to a reader that is not its parent: it writes supervisor.json at
// every change, and a case waits on the change, never on the time.
const pollEvery = 20 * time.Millisecond

// AwaitSupervisor waits for the box's supervisor.json to record what want
// says, and answers the record, failing the case on none within goneWithin.
func (b *Box) AwaitSupervisor(t testing.TB, what string, want func(Supervisor) bool) Supervisor {
	t.Helper()
	deadline := time.Now().Add(goneWithin)
	for {
		s, ok := b.supervisor(t)
		if ok && want(s) {
			return s
		}
		if time.Now().After(deadline) { //fixedwait:measure a poll's own deadline: the case fails on it
			t.Fatalf("the box's supervisor.json records %+v, and not %s, %s on", s, what, goneWithin)
		}
		time.Sleep(pollEvery) //fixedwait:liveness
	}
}

// Browsers is the pid of every process running on the box's profile, as its
// command line names it: none, once nothing holds the profile.
func (b *Box) Browsers(t testing.TB) []int {
	t.Helper()
	pids, err := processesCarrying("--user-data-dir=" + filepath.Join(b.Folder, "profile"))
	if err != nil {
		t.Fatalf("listing the processes on the box's profile: %v", err)
	}
	return pids
}

// supervising is whether pid runs the helper's supervise on this box's launch.
func (b *Box) supervising(pid int) bool {
	if pid <= 0 {
		return false
	}
	args, err := commandLine(pid)
	if err != nil {
		return false
	}
	return strings.HasSuffix(strings.Join(args, " "),
		" supervise --launch "+filepath.Join(b.Folder, "launch.json"))
}

// NPMCalls is each command line a stand-in npm was run with, in order, its
// arguments after the executable.
func (b *Box) NPMCalls(t testing.TB) [][]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(b.Home, npmCalls))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	for line := range bytes.Lines(raw) {
		args := strings.Split(strings.TrimSuffix(string(line), "\n"), "\x00")
		calls = append(calls, args[:len(args)-1])
	}
	return calls
}

// disk is a box's own upload service, on this machine: each file staged
// beside its path and renamed over it, at the loose modes such a service may
// leave, 0644 files in 0755 directories, which the helper holds to its own.
type disk struct{}

// WriteFile implements boxhelper.Writer.
func (disk) WriteFile(ctx context.Context, path string, content []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	staged, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(staged.Name()) }()
	_, err = staged.Write(content)
	if err == nil {
		err = staged.Chmod(0o644)
	}
	if closeErr := staged.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(staged.Name(), path)
}

// sweep removes every home in dir whose test binary is gone, stopping what
// still runs for it first, and reports the homes it could not judge or
// remove.
func sweep(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), homePrefix) {
			continue
		}
		home := filepath.Join(dir, entry.Name())
		owner, ok := leaderIn(home, ownerFile)
		if !ok {
			continue
		}
		running, err := alive(owner)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", home, err))
		case !running:
			if _, err := stopWhatRuns(home); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", home, err))
				continue
			}
			if err := os.RemoveAll(home); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// stopWhatRuns kills what home's box still runs, and answers each pid it
// killed: the process group of the supervisor the home records, while its
// leader is still the one recorded, so a recycled pid costs no other group
// its life; any other supervisor on the box's launch.json, which two ensures
// that both started one leave unrecorded, with the group it leads; and every
// process on its profile, a browser, which no other process names. A
// supervisor killed outright leaves its browser in a group of its own until
// its pipe closes, writing the profile as it goes.
func stopWhatRuns(home string) ([]int, error) {
	var killed []int
	var errs []error
	if s, ok := leaderIn(home, supervisorFile); ok {
		current, err := s.Current()
		switch {
		case err != nil:
			errs = append(errs, err)
		case current:
			errs = append(errs, procgroup.Kill(s.PID))
			killed = append(killed, s.PID)
		}
	}
	folder := boxhelper.Folder(home)
	supervisors, err := processesCarrying(filepath.Join(folder, "launch.json"))
	errs = append(errs, err)
	for _, pid := range supervisors {
		if proc, found, inspectErr := procgroup.Inspect(pid); inspectErr == nil && found && proc.Group == pid {
			_ = procgroup.Kill(pid)
		} else if p, findErr := os.FindProcess(pid); findErr == nil {
			_ = p.Kill()
		}
		killed = append(killed, pid)
	}
	browsers, err := processesCarrying("--user-data-dir=" + filepath.Join(folder, "profile"))
	errs = append(errs, err)
	for _, pid := range browsers {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
		killed = append(killed, pid)
	}
	return killed, errors.Join(errs...)
}

// leaderBound is the longest owner or supervisor file a home holds: a pid
// and a start time take under 64 bytes, so a longer file is none Start wrote.
const leaderBound = 256

// leaderIn is the process home's file name names, where it names one.
func leaderIn(home, name string) (procgroup.Leader, bool) {
	file := filepath.Join(home, name)
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || info.Size() > leaderBound {
		return procgroup.Leader{}, false
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return procgroup.Leader{}, false
	}
	leader, err := procgroup.ParseLeader(strings.TrimSpace(string(raw)))
	return leader, err == nil
}

// alive is whether the test binary owner names still runs: the kernel reports
// its pid with the start time recorded, and not exited.
func alive(owner procgroup.Leader) (bool, error) {
	proc, found, err := procgroup.Inspect(owner.PID)
	if err != nil {
		return false, err
	}
	return found && proc.Start == owner.Start && !proc.Zombie, nil
}
