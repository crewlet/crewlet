package cache_test

import (
	"bufio"
	"bytes"
	"cmp"
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

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE HARNESS: a prepared directory, a real pool over its HOME, and a cache
// opened over both with every log line kept for the case to read. Every open
// is a whole boot — Prepare, gitproc.New, Open — as a node's is: Prepare reads
// repos/ and hands the reading to Open, so what a case writes there before it
// opens is what the boot finds.

// suiteShare is whose every transfer a case runs through a world's pool is
// (gitproc.Cmd.Share, which a transfer requires): a machine token's, since no
// case here is about who waits for a transfer slot.
var suiteShare = gitproc.Share{Class: gitproc.ShareMachine, Principal: "suite"}

// pastEveryTimer is how far a case in a synctest bubble moves the clock before
// it reads, a second time, that something did NOT happen. The first read is
// once synctest.Wait returns: every other goroutine of the case is durably
// blocked by then, but one asleep on the bubble's clock is durably blocked
// too, so an event the code would make only after a wait on that clock of its
// own — a grace, a pace, a retry — has not been made yet, however wrong it
// is. The case then sleeps pastEveryTimer and waits again: the sleep ends
// only once the clock has reached its end, every timer due before then fired
// and what each woke run until it blocked, and the second Wait lets run what
// woke at that very instant. So the second read decides what the first
// cannot.
//
// Past the longest timer the cache arms, a day — the scrubber's wake at a
// copy's interval (ScrubInterval) and the backfill's at its next horizon
// (HorizonEvery) — so a bug that delays its event by any wait of the cache's
// own is past it too. It costs a bubble nothing, since the clock jumps from
// one timer to the next; and a case moves it only where nothing the span
// fires starts or ends any work on a healthy build, which each such case
// says.
const pastEveryTimer = max(cache.ScrubInterval, cache.HorizonEvery) + time.Second

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
	// transfers is every boot's pool's MaxTransfers, an operator's
	// gitlet.max_transfers; 0 is the pool's own default, sized to its
	// processes and its memory.
	transfers int
	// memory is every boot's pool's node memory; 0 is a tebibyte, so every
	// case's lanes and maintenance budget are its own, never whatever the
	// machine running the suite has left after its pool's lanes — which,
	// for a pool of eight cores on a sixteen-gigabyte runner, is little. A
	// case about the budget sets it, or sets machineMemory, which makes the
	// pool read the machine's own as a node's does.
	memory        int64
	machineMemory bool
}

// newWorld prepares a fresh cache directory and a real gitproc pool over the
// HOME Prepare names. Nothing is opened yet; the Layout holds the claim until
// an Open takes it or the case ends.
func newWorld(t testing.TB) *world {
	t.Helper()
	gittest.RequireNode(t)
	w := &world{logs: &logs{}}
	w.prepare(t, filepath.Join(t.TempDir(), "c"))
	return w
}

// prepare prepares dir — w's own directory again, after its first — and
// builds a real pool over the HOME it names. A Layout opens once, and
// Prepare moves the last pool's HOME aside, so every boot takes both anew;
// the Layout before it is released first (nothing, once an Open took it).
func (w *world) prepare(t testing.TB, dir string) {
	t.Helper()
	w.layout.Release()
	layout, err := cache.Prepare(t.Context(), dir)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	t.Cleanup(layout.Release)
	memory := cmp.Or(w.memory, 1<<40)
	if w.machineMemory {
		memory = 0
	}
	w.layout, w.pool = layout, newPoolOf(t, layout, w.gitPath, w.processes, w.transfers, memory)
}

// reboot prepares w's directory again: the boot's Prepare.
func (w *world) reboot(t testing.TB) { w.prepare(t, w.layout.Dir) }

// newPool is a real pool over layout's HOME.
func newPool(t testing.TB, layout cache.Layout) *gitproc.Pool {
	return newPoolOf(t, layout, "", 0, 0, 1<<40)
}

// newPoolOf is a pool over layout's HOME running the git at path ("" is the
// one on PATH, held to the node floor: gittest.RequireNode) with processes
// slots (0 is two) and transfers transfer slots (0 is the pool's default), on a
// node of memory bytes (0 is the machine's own, read as a node's is).
func newPoolOf(t testing.TB, layout cache.Layout, path string, processes, transfers int,
	memory int64) *gitproc.Pool {
	t.Helper()
	if path == "" {
		gittest.RequireNode(t)
	}
	if processes == 0 {
		processes = 2
	}
	pool, err := gitproc.New(t.Context(), gitproc.Options{Path: path, Home: layout.Home, MaxProcesses: processes,
		MaxTransfers: transfers, Memory: memory})
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
		Fleet:       soloFleet,
		Limits:      gitlet.DefaultLimits,
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
func (w *world) open(t testing.TB, mutate ...func(*cache.Options)) *cache.Cache {
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
// when the case moves it; and the timers a knob arms on it (At), each fired
// by the move that reaches its time.
type clock struct {
	mu     sync.Mutex
	at     time.Time
	timers []clockTimer
}

// clockTimer is a timer armed on a clock: the time it fires at, and the
// channel it fires on.
type clockTimer struct {
	at time.Time
	ch chan time.Time
}

func newClock() *clock { return &clock{at: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)} }

// Now is the clock's time; a cache's Options.Now.
func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

// Add moves the clock on by d, firing every timer it reaches, and answers the
// new time.
func (c *clock) Add(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
	c.fire()
	return c.at
}

// Set puts the clock at t, firing every timer it reaches.
func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = t
	c.fire()
}

// At is a timer on the clock for t: a channel the clock's time is sent on
// once the clock reads t or later — AT ONCE where it does already, as a timer
// reset to a wait that is not positive fires at once. A knob standing in for
// a loop's timer answers one of these, never a channel the case sends on by
// hand: a loop re-arms its wait from a pass that read the clock before the
// case moved it (a kick it took while it waited), so the channel the case saw
// armed may be one nobody waits on any more, and a send on it is lost while
// the loop waits on the wake it armed after, which nothing sends on.
func (c *clock) At(t time.Time) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.timers = append(c.timers, clockTimer{at: t, ch: ch})
	c.fire()
	return ch
}

// fire sends on every timer whose time the clock has reached and forgets it:
// each fires once, into the one place its channel keeps, so moving the clock
// never waits for whoever armed one. c.mu is held.
func (c *clock) fire() {
	kept := c.timers[:0]
	for _, tm := range c.timers {
		if tm.at.After(c.at) {
			kept = append(kept, tm)
			continue
		}
		tm.ch <- c.at
	}
	clear(c.timers[len(kept):])
	c.timers = kept
}

// on is an Options mutation that runs the cache on the clock.
func (c *clock) on(o *cache.Options) { o.Now = c.Now }

// path is a path under the cache's root.
func (w *world) path(parts ...string) string {
	return filepath.Join(append([]string{w.layout.Dir}, parts...)...)
}

// noRows is a Fetcher a case fails on: opening a cache reads no rows.
type noRows struct{ t testing.TB }

func (n noRows) RepoState(context.Context, gitlet.StateQuery) (gitlet.RepoSnapshot, error) {
	n.t.Errorf("a cache read a repository's rows where nothing may")
	return gitlet.RepoSnapshot{}, io.ErrUnexpectedEOF
}

func (n noRows) Read(context.Context, gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
	n.t.Errorf("a cache read a repository's pushes where nothing may")
	return gitlet.ReadAnswer{}, io.ErrUnexpectedEOF
}

// soloFleet is a node with no fleet: one Gitlet node and one data node, itself,
// which a backfill paces its pages by.
func soloFleet() (int, int, error) { return 1, 1, nil }

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
	// logging, when set, is told each line's message as the line is logged,
	// on the goroutine logging it and before the line is written: a case
	// holds a line there to tell one logged before what it watches from one
	// logged after. Set before the cache that logs here is opened.
	//
	// NO LOCK OF THE LOGGER'S IS HELD WHILE IT RUNS. It is told by the
	// logger's own handler (heard), ahead of the JSON handler, and never from
	// Write: the JSON handler calls Write holding a lock that every logger
	// made from it shares, so a line held there would hold every other line
	// of the cache behind it, and a goroutine queued on a sync.Mutex is not
	// durably blocked — the hold's synctest.Wait would never return, and the
	// case would hang to the binary's timeout rather than fail.
	//
	// A hook that holds a line waits with synctest.Wait, never on a timer,
	// which nothing fires once the case has returned. It holds only a line
	// its code logs with no lock held that another goroutine of the case may
	// take — one queued on that lock would keep the Wait from returning just
	// the same — and holds it on one goroutine at a time: two Waits at once
	// panic.
	logging func(msg string)
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// logger logs every level into l, telling l.logging of each line first.
func (l *logs) logger() *slog.Logger {
	return slog.New(heard{slog.NewJSONHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug}), l})
}

// heard is the handler of a logger logs made: the JSON handler over logs,
// which each line reaches only once logs.logging has been told of it. A
// logger made from it with With or WithGroup tells logging too.
type heard struct {
	slog.Handler
	l *logs
}

func (h heard) Handle(ctx context.Context, r slog.Record) error {
	if h.l.logging != nil {
		h.l.logging(r.Message)
	}
	return h.Handler.Handle(ctx, r)
}

func (h heard) WithAttrs(attrs []slog.Attr) slog.Handler {
	return heard{h.Handler.WithAttrs(attrs), h.l}
}

func (h heard) WithGroup(name string) slog.Handler {
	return heard{h.Handler.WithGroup(name), h.l}
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
