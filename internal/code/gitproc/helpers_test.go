package gitproc_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// hang is the hang detector every blocking case waits under: thirty seconds,
// never a performance claim. These cases run in make test's shared half under
// -race beside every other package, so a duration that IS a subject is
// asserted by comparing two events of one case, or allowed ten times its
// configured value — never as a tight wall-clock bound.
const hang = 30 * time.Second

// suiteShare is whose every transfer a case runs is: a machine's — a
// credential acting as nobody, as a CI runner's is (gitproc.Cmd.Share is
// required of a transfer). A case whose subject is the share names its own.
var suiteShare = gitproc.Share{Class: gitproc.ShareMachine, Principal: "suite"}

// fakeVersion is the version every stand-in git answers with: the node floor
// exactly, the oldest git a pool is built over.
var fakeVersion = "git version " + gitproc.NodeMinVersion.String()

// newPool is a pool over o with a HOME of its own.
func newPool(t testing.TB, o gitproc.Options) *gitproc.Pool {
	t.Helper()
	if o.Home == "" {
		o.Home = filepath.Join(t.TempDir(), "home")
	}
	p, err := gitproc.New(t.Context(), o)
	if err != nil {
		t.Fatalf("gitproc.New: %v", err)
	}
	return p
}

// fakePool is a pool over a stand-in git running body.
func fakePool(t testing.TB, body string, o gitproc.Options) *gitproc.Pool {
	t.Helper()
	o.Path = gittest.Fake(t, fakeVersion, body)
	return newPool(t, o)
}

// realPool is a pool over the real git, held to the node floor before the
// pool refuses it ([gittest.RequireNode]): the node pin under every make
// target. A test whose SUBJECT is what git does in a pool runs on both of the
// node's gits instead ([onNodeGits]).
func realPool(t testing.TB, o gitproc.Options) *gitproc.Pool {
	t.Helper()
	return poolOn(t, gittest.RequireNode(t), o)
}

// poolOn is a pool over the real git at git.
func poolOn(t testing.TB, git string, o gitproc.Options) *gitproc.Pool {
	t.Helper()
	o.Path = git
	return newPool(t, o)
}

// onNodeGits runs body once on each of the two gits that bound what a pool
// may run, each a parallel subtest of its own: the NODE PIN — the PATH's git,
// which every make target makes the newest release (gittest.RequireNode),
// what the image builds — and the NODE FLOOR's own build, the newest patch of
// gitproc.NodeMinVersion's series (gittest.NodeFloor), the oldest git
// gitproc.New admits. Every claim about what a real git says or does in a
// pool — the format table, what a request can reach, what the pool's
// environment keeps from git — is a claim about every git a git node may
// run, and measured on the pin alone it was a claim about one of them: the
// floor's own expectations were wrong in three arms of the format table, and
// nothing ran them (ADR-0080, "each floor is a measured claim"). Under every
// make target the floor's build is required (CREWLET_REQUIRE_GIT_FLOOR=1);
// a bare `go test` without it skips the floor's arm alone.
func onNodeGits(t *testing.T, body func(t *testing.T, git string)) {
	t.Helper()
	for _, g := range []struct {
		name string
		git  func(testing.TB) string
	}{
		{"node pin", gittest.RequireNode},
		{"node floor", gittest.NodeFloor},
	} {
		t.Run(g.name, func(t *testing.T) {
			t.Parallel()
			body(t, g.git(t))
		})
	}
}

// quote is s as one POSIX shell word.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// countingWriter counts what it is given and keeps none of it.
type countingWriter struct{ n int64 }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

// outcome is one Run's answer.
type outcome struct {
	res gitproc.Result
	err error
}

// start runs c in a goroutine.
func start(ctx context.Context, p *gitproc.Pool, c gitproc.Cmd) <-chan outcome {
	done := make(chan outcome, 1)
	go func() {
		res, err := p.Run(ctx, c)
		done <- outcome{res, err}
	}()
	return done
}

// await is a run's outcome, failing the case if it has not returned within
// the hang detector.
func await(t testing.TB, done <-chan outcome, what string) outcome {
	t.Helper()
	select {
	case o := <-done:
		return o
	case <-time.After(hang):
		t.Fatalf("%s had not returned after %v", what, hang)
		return outcome{}
	}
}

// pidFile is where a stand-in body writes a pid ($$ or $!), and readPID
// waits for it.
type pidFile struct{ path string }

func newPIDFile(t testing.TB) pidFile {
	return pidFile{path: filepath.Join(t.TempDir(), "pid")}
}

func (f pidFile) shell() string { return quote(f.path) }

func (f pidFile) read(t testing.TB) int {
	t.Helper()
	deadline := time.Now().Add(hang)
	for {
		body, err := os.ReadFile(f.path)
		if err == nil && bytes.HasSuffix(body, []byte("\n")) {
			pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
			if err != nil {
				t.Fatalf("pid file %s holds %q", f.path, body)
			}
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pid in %s after %v", f.path, hang)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// written is the pid in the file when a whole one is there, without waiting:
// for a run that has RETURNED, whose group the sweep has ended, a file absent
// or cut short now never fills.
func (f pidFile) written(t testing.TB) (int, bool) {
	t.Helper()
	body, err := os.ReadFile(f.path)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && !bytes.HasSuffix(body, []byte("\n"))) {
		return 0, false
	}
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil {
		t.Fatalf("pid file %s holds %q", f.path, body)
	}
	return pid, true
}

// pastItsDeadline runs the stand-in under deadlines doubling from first until
// a run's tree was up — its pid written — before the deadline fired, and
// returns that run's outcome, its deadline, how long Run took and the pid.
//
// A deadline that fires before the shell has started its tree ends a run
// that never did what the case is about, and on a loaded -race runner a
// shell's start-up is a matter of scheduling: so a run whose pid never
// appeared is run again with more room rather than failed, and the case
// asserts only about a run whose tree existed when the deadline ended it.
// Each try is bounded; the sum stays inside the hang detector.
func pastItsDeadline(t *testing.T, p *gitproc.Pool, pids pidFile, first time.Duration) (o outcome, deadline, took time.Duration, pid int) {
	t.Helper()
	for deadline = first; deadline <= hang/3; deadline *= 2 {
		if err := os.Remove(pids.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		began := time.Now()
		o = await(t, start(t.Context(), p, gitproc.Cmd{Args: []string{"x"}, Class: gitproc.ClassRead, Deadline: deadline}), "a run past its deadline")
		took = time.Since(began)
		if pid, ok := pids.written(t); ok {
			return o, deadline, took, pid
		}
	}
	t.Fatalf("no run wrote its pid before a deadline of up to %v fired", deadline/2)
	return outcome{}, 0, 0, 0
}

// awaitGauge waits, up to the hang detector, until name's series for class
// ("" for an instrument recorded without one) reads want.
//
// A gauge is Set by the goroutine that made the change it reports, AFTER the
// change, so a case that saw the change some other way may read the gauge a
// moment before its Set: a run is counted only once its process has started
// and been identified, which may land after the process has begun to do what
// a case waits for, and a loan repaid when Cmd.Indexed closes is Set by the
// run's watcher after the lane has the cores back. Polled, the reading a case
// asserts is one the pool has published rather than one in flight.
func awaitGauge(t *testing.T, rec *metrics.Recorder, name, class string, want float64) {
	t.Helper()
	series := name
	if class != "" {
		series += "{class=" + class + "}"
	}
	deadline := time.Now().Add(hang)
	for {
		s := reading(rec, name, class)
		if s.Name == name && s.Value == want {
			return
		}
		if time.Now().After(deadline) {
			if s.Name != name {
				t.Fatalf("%s is not recorded after %v, want %v", series, hang, want)
			}
			t.Fatalf("%s reads %v after %v, want %v", series, s.Value, hang, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// logBuffer is a logger a case reads back.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *logBuffer) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// lines counts the log lines naming msg and holding every one of with.
func (b *logBuffer) lines(msg string, with ...string) int {
	n := 0
	for _, line := range strings.Split(b.String(), "\n") {
		if !strings.Contains(line, "msg="+msg) {
			continue
		}
		all := true
		for _, w := range with {
			all = all && strings.Contains(line, w)
		}
		if all {
			n++
		}
	}
	return n
}

// envOf parses `env` output into a map, minus the variables a shell adds on
// its way to exec.
func envOf(out []byte) map[string]string {
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch name {
		case "PWD", "OLDPWD", "SHLVL", "_":
			continue
		}
		got[name] = value
	}
	return got
}

// configOf is the GIT_CONFIG_COUNT pairs an environment carries, in order.
func configOf(t testing.TB, env map[string]string) []gitproc.KV {
	t.Helper()
	n, err := strconv.Atoi(env["GIT_CONFIG_COUNT"])
	if err != nil {
		t.Fatalf("GIT_CONFIG_COUNT %q: %v", env["GIT_CONFIG_COUNT"], err)
	}
	out := make([]gitproc.KV, n)
	for i := range n {
		out[i] = gitproc.KV{Key: env[fmt.Sprintf("GIT_CONFIG_KEY_%d", i)], Value: env[fmt.Sprintf("GIT_CONFIG_VALUE_%d", i)]}
	}
	return out
}
