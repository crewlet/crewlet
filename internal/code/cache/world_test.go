package cache_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE HARNESS: a prepared directory, a real pool over its HOME, and a cache
// opened over both with every log line kept for the case to read. Every open
// is a whole boot — Prepare, gitproc.New, Open — as a node's is: Prepare reads
// repos/ and hands the reading to Open, so what a case writes there before it
// opens is what the boot finds.

// world is one cache directory and the pool that serves it.
type world struct {
	layout cache.Layout
	pool   *gitproc.Pool
	logs   *logs
	// gitPath is the git every boot's pool runs; "" is the one on PATH.
	gitPath string
	// processes is every boot's pool's MaxProcesses; 0 is two, whose
	// maintenance lane is ONE slot — the lane a case holds whole by holding
	// one process.
	processes int
}

// newWorld prepares a fresh cache directory and a real gitproc pool over the
// HOME Prepare names. Nothing is opened yet; the Layout holds the claim until
// an Open takes it or the case ends.
func newWorld(t *testing.T) *world {
	t.Helper()
	gittest.Require(t)
	w := &world{logs: &logs{}}
	w.prepare(t, filepath.Join(t.TempDir(), "c"))
	return w
}

// prepare prepares dir — w's own directory again, after its first — and
// builds a real pool over the HOME it names. A Layout opens once, and
// Prepare moves the last pool's HOME aside, so every boot takes both anew;
// the Layout before it is released first (nothing, once an Open took it).
func (w *world) prepare(t *testing.T, dir string) {
	t.Helper()
	w.layout.Release()
	layout, err := cache.Prepare(t.Context(), dir)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	t.Cleanup(layout.Release)
	w.layout, w.pool = layout, newPoolOf(t, layout, w.gitPath, w.processes)
}

// reboot prepares w's directory again: the boot's Prepare.
func (w *world) reboot(t *testing.T) { w.prepare(t, w.layout.Dir) }

// newPool is a real pool over layout's HOME.
func newPool(t *testing.T, layout cache.Layout) *gitproc.Pool { return newPoolOf(t, layout, "", 0) }

// newPoolOf is a pool over layout's HOME running the git at path ("" is the
// one on PATH) with processes slots (0 is two).
func newPoolOf(t *testing.T, layout cache.Layout, path string, processes int) *gitproc.Pool {
	t.Helper()
	if processes == 0 {
		processes = 2
	}
	pool, err := gitproc.New(t.Context(), gitproc.Options{Path: path, Home: layout.Home, MaxProcesses: processes})
	if err != nil {
		t.Fatalf("gitproc.New over %s: %v", layout.Home, err)
	}
	return pool
}

// options is a valid Options value over w, with each mutate applied.
func (w *world) options(t testing.TB, mutate ...func(*cache.Options)) cache.Options {
	rec, err := metrics.New()
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	o := cache.Options{
		Layout:      w.layout,
		MaxBytes:    1 << 30,
		Git:         w.pool,
		Objects:     noObjects{t: t},
		Fetch:       noRows{t: t},
		Limits:      code.DefaultLimits,
		HookCommand: []string{"/bin/true"},
		Logger:      w.logs.logger(),
		Metrics:     rec,
		Now:         time.Now,
	}
	for _, m := range mutate {
		m(&o)
	}
	return o
}

// open boots a cache over w — Prepare, a pool, Open — and stops it when the
// case ends.
func (w *world) open(t *testing.T, mutate ...func(*cache.Options)) *cache.Cache {
	t.Helper()
	w.reboot(t)
	c, err := cache.Open(t.Context(), w.options(t, mutate...))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { c.Stop(context.Background()) })
	return c
}

// clock is a case's own time: what the cache reads as now — a last use, a
// rebuild's cooldown, the maintainer's debounce and interval — moved only
// when the case moves it.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func newClock() *clock { return &clock{at: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)} }

// Now is the clock's time; a cache's Options.Now.
func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

// Add moves the clock on by d and answers the new time.
func (c *clock) Add(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
	return c.at
}

// Set puts the clock at t.
func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = t
}

// on is an Options mutation that runs the cache on the clock.
func (c *clock) on(o *cache.Options) { o.Now = c.Now }

// path is a path under the cache's root.
func (w *world) path(parts ...string) string {
	return filepath.Join(append([]string{w.layout.Dir}, parts...)...)
}

// noRows is a Fetcher a case fails on: opening a cache reads no rows.
type noRows struct{ t testing.TB }

func (n noRows) RepoState(context.Context, code.StateQuery) (code.RepoSnapshot, error) {
	n.t.Errorf("a cache read a repository's rows where nothing may")
	return code.RepoSnapshot{}, io.ErrUnexpectedEOF
}

// noObjects is an Objects a case fails on: opening a cache reads no pack.
type noObjects struct{ t testing.TB }

func (n noObjects) Open(context.Context, objstore.Object) (io.ReadCloser, error) {
	n.t.Errorf("a cache read the object store where nothing may")
	return nil, io.ErrUnexpectedEOF
}

// logs keeps every line a cache logs, as JSON.
type logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logs) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// lines is every logged line whose message is msg, decoded.
func (l *logs) lines(t testing.TB, msg string) []map[string]any {
	t.Helper()
	l.mu.Lock()
	raw := append([]byte(nil), l.buf.Bytes()...)
	l.mu.Unlock()
	var out []map[string]any
	scan := bufio.NewScanner(bytes.NewReader(raw))
	scan.Buffer(nil, 1<<20)
	for scan.Scan() {
		var line map[string]any
		if err := json.Unmarshal(scan.Bytes(), &line); err != nil {
			t.Fatalf("a log line is not JSON: %v: %s", err, scan.Bytes())
		}
		if line["msg"] == msg {
			out = append(out, line)
		}
	}
	return out
}

// names is the sorted names in dir.
func names(t testing.TB, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("listing %s: %v", dir, err)
	}
	out := make([]string, 0, len(list))
	for _, d := range list {
		out = append(out, d.Name())
	}
	sort.Strings(out)
	return out
}

// write writes content at path, making its directory.
func write(t testing.TB, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// read is the content at path.
func read(t testing.TB, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// exists reports whether path is there, as itself.
func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
