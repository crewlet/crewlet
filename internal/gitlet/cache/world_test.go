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
// opened over both, every log line kept. Every open is a whole boot (Prepare,
// gitproc.New, Open) as a node's is, so what a case writes under repos/ before
// it opens is what the boot finds.

// suiteShare is the Share every transfer through a world's pool runs as
// (gitproc.Cmd.Share): a machine token's, since no case here is about who
// waits for a transfer slot.
var suiteShare = gitproc.Share{Class: gitproc.ShareMachine, Principal: "suite"}

// pastEveryTimer is how far a synctest case moves the clock before reading, a
// second time, that something did NOT happen. synctest.Wait counts a
// goroutine asleep on the bubble's clock as durably blocked, so after the
// first Wait an event the code makes only after a wait of its own (a grace, a
// pace, a retry) has not happened yet, however wrong. Sleeping pastEveryTimer
// fires every timer due by then and runs what each woke; a second Wait runs
// what woke at that instant, so the second read decides what the first cannot.
//
// It is past the cache's longest timer, a day (ScrubInterval, HorizonEvery),
// so past any delay a wait of the cache's own adds, and costs a bubble nothing.
// A case moves it only where nothing in the span starts or ends work on a
// healthy build, which each such case says.
const pastEveryTimer = max(cache.ScrubInterval, cache.HorizonEvery) + time.Second

// world is one cache directory and the pool that serves it.
type world struct {
	layout cache.Layout
	pool   *gitproc.Pool
	logs   *logs
	// gitPath is the git every boot's pool runs; "" is the one on PATH.
	gitPath string
	// processes is every boot's pool's MaxProcesses; 0 is two, whose
	// maintenance lane is ONE slot, which a case holds by holding one process.
	processes int
	// transfers is every boot's pool's MaxTransfers (gitlet.max_transfers);
	// 0 is the pool's own default.
	transfers int
	// memory is every boot's pool's node memory; 0 is a tebibyte, so a case's
	// lanes and maintenance budget never depend on the runner's memory. A case
	// about the budget sets it, or sets machineMemory to make the pool read
	// the machine's own as a node's does.
	memory        int64
	machineMemory bool
}

// newWorld prepares a fresh cache directory and a real pool over its HOME.
// Nothing is opened; the Layout holds the claim until an Open takes it or the
// case ends.
func newWorld(t testing.TB) *world {
	t.Helper()
	gittest.RequireNode(t)
	w := &world{logs: &logs{}}
	w.prepare(t, filepath.Join(t.TempDir(), "c"))
	return w
}

// prepare prepares dir and builds a real pool over its HOME. A Layout opens
// once and Prepare moves the last pool's HOME aside, so every boot takes both
// anew, releasing the previous Layout first (a no-op once an Open took it).
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

// open boots a cache over w (Prepare, a pool, Open), stopped when the case
// ends.
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

// clock is a case's own time: the cache's now (a last use, a rebuild's
// cooldown, the maintainer's debounce and interval), moved only by the case,
// and the timers a knob arms on it (At), each fired by the move reaching it.
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

// At is a timer on the clock for t: a channel the clock's time is sent on once
// it reads t or later, AT ONCE if it already does, as a timer reset to a
// non-positive wait fires. A knob standing in for a loop's timer answers one
// of these, never a channel the case sends on by hand: a loop may re-arm from
// a pass that read the clock before the case moved it, so a hand send can
// land on a channel nobody waits on any more while the loop waits on its new
// wake, which nothing sends on.
func (c *clock) At(t time.Time) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.timers = append(c.timers, clockTimer{at: t, ch: ch})
	c.fire()
	return ch
}

// fire sends on every timer the clock has reached and drops it: each fires
// once, into its channel's buffer of one, so moving the clock never waits on
// whoever armed it. c.mu is held.
func (c *clock) fire() {
	kept := c.timers[:0]
	for _, tm := range c.timers {
		//fixedwait:stated a timer's instant and the fake clock's own time, both on the case's clock
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
	// logging, when set, is told each line's message on the logging goroutine
	// before the line is written, so a case can hold a line to tell what was
	// logged before it from after. Set it before the cache is opened.
	//
	// NO LOGGER LOCK IS HELD WHILE IT RUNS: heard calls it ahead of the JSON
	// handler, never from Write, which runs under a lock every derived logger
	// shares. A goroutine queued on a sync.Mutex is not durably blocked, so a
	// hold there would hang the hold's synctest.Wait to the binary's timeout.
	//
	// A hook holding a line waits with synctest.Wait, never a timer; holds
	// only a line logged with no lock held that another goroutine of the case
	// may take; and holds on one goroutine at a time (two Waits at once panic).
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

// heard is the handler of logs' loggers: the JSON handler over logs, which a
// line reaches only after logs.logging is told of it, With and WithGroup
// included.
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

// exists reports whether path is there, a symlink not followed.
func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
