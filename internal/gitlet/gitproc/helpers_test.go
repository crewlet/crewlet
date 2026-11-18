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

	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// hang is the hang detector every blocking case waits under, never a
// performance claim: under -race beside every other package, a duration that
// is a case's subject is asserted by comparing two events of the case, or
// allowed ten times its configured value.
const hang = 30 * time.Second

// suiteShare is the share of every transfer a case runs (gitproc.Cmd.Share is
// required of one): a machine's, as a CI runner's is. A case whose subject is
// the share names its own.
var suiteShare = gitproc.Share{Class: gitproc.ShareMachine, Principal: "suite"}

// fakeVersion is the version every stand-in git answers with: the node floor,
// the oldest git a pool is built over.
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

// realPool is a pool over the PATH's git, the node pin under the make targets,
// held to the node floor ([gittest.RequireNode]). A test whose SUBJECT is what
// git does in a pool runs on both of the node's gits instead ([onNodeGits]).
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

// onNodeGits runs body once on each git that bounds what a pool may run, each
// a parallel subtest: the NODE PIN, the PATH's git the image builds
// (gittest.RequireNode), and the NODE FLOOR's build, the newest patch of
// gitproc.NodeMinVersion's series (gittest.NodeFloor). A claim about what a
// real git does in a pool is a claim about every git a Gitlet node may run,
// and the pin alone does not prove it for the floor (ADR-0087, "each floor is
// a measured claim"). The make targets require the floor's build
// (internal/prereqtest); a bare `go test` skips its arm alone.
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
	case <-time.After(hang): //fixedwait:liveness
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
		if time.Now().After(deadline) { //fixedwait:measure a poll's own deadline
			t.Fatalf("no pid in %s after %v", f.path, hang)
		}
		time.Sleep(10 * time.Millisecond) //fixedwait:liveness
	}
}

// written is the pid in the file when a whole one is there, without waiting:
// for a run that has RETURNED, whose group the sweep ended, a file absent or
// cut short never fills.
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
// a run's tree was up (its pid written) before the deadline fired, and returns
// that run's outcome and the pid. A deadline that beats a loaded runner's shell
// start-up ends a run that never did what the case is about, so it is retried
// rather than failed; the tries sum within the hang detector. It times
// nothing: a case holds the wait the pool armed (gitproc.SeeGrace), not how
// long Run took.
func pastItsDeadline(t *testing.T, p *gitproc.Pool, pids pidFile, first time.Duration) (o outcome, pid int) {
	t.Helper()
	deadline := first
	for ; deadline <= hang/3; deadline *= 2 {
		if err := os.Remove(pids.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		o = await(t, start(t.Context(), p, gitproc.Cmd{Args: []string{"x"}, Class: gitproc.ClassRead, Deadline: deadline}), "a run past its deadline")
		if pid, ok := pids.written(t); ok {
			return o, pid
		}
	}
	t.Fatalf("no run wrote its pid before a deadline of up to %v fired", deadline/2)
	return outcome{}, 0
}

// awaitGauge waits, up to the hang detector, until name's series for class
// ("" for an instrument recorded without one) reads want. Polled, because a
// gauge is Set AFTER the change it reports (a run once its process is
// identified, a loan once the lane has its cores back), so a case that saw the
// change another way may otherwise read it before the Set.
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
		if time.Now().After(deadline) { //fixedwait:measure a poll's own deadline
			if s.Name != name {
				t.Fatalf("%s is not recorded after %v, want %v", series, hang, want)
			}
			t.Fatalf("%s reads %v after %v, want %v", series, s.Value, hang, want)
		}
		time.Sleep(10 * time.Millisecond) //fixedwait:liveness
	}
}

// awaitWaiting waits, up to the hang detector, until want runs wait in p's
// lane of class c (gitproc.Waiting). A case that lets a slot go to a queued
// run, or ends a queued run's caller, waits here first: a run still on its way
// to the lane would take a free slot or meet its end unqueued, and the case
// would pass without the path it names, or queue out of order and fail.
func awaitWaiting(t *testing.T, p *gitproc.Pool, c gitproc.Class, want int64) {
	t.Helper()
	deadline := time.Now().Add(hang)
	for {
		got := gitproc.Waiting(p, c)
		if got == want {
			return
		}
		if time.Now().After(deadline) { //fixedwait:measure a poll's own deadline
			t.Fatalf("%d runs wait in the %s lane after %v, want %d", got, c, hang, want)
		}
		time.Sleep(10 * time.Millisecond) //fixedwait:liveness
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
