package issuereport

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/panics"
)

// The child a crash case runs: this test binary, told which crash to have and
// where the runtime writes it.
const (
	crashChildEnv = "CREWLET_ISSUEREPORT_CRASH_CHILD"
	crashFileEnv  = "CREWLET_ISSUEREPORT_CRASH_FILE"
	crashCanary   = "Acme-Payroll-Corp jane.doe@acme.example"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(crashChildEnv); mode != "" {
		crashChild(mode, os.Getenv(crashFileEnv))
		return
	}
	os.Exit(m.Run())
}

// crashChild arms the crash output as a node does, and crashes as mode says.
func crashChild(mode, path string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		os.Exit(3)
	}
	if err := debug.SetCrashOutput(f, debug.CrashOptions{}); err != nil {
		os.Exit(3)
	}
	switch mode {
	case "panic":
		callsWritesNilMap()
	case "explicit":
		panic(crashCanary)
	case "concurrent":
		writesOneMapFromMany()
	case "overflow":
		debug.SetMaxStack(256 << 10)
		os.Exit(recursesForever(0))
	case "sync":
		unlocksAnUnlockedMutex()
	}
	os.Exit(4)
}

//go:noinline
func writesOneMapFromMany() {
	shared := map[int]int{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for i := 0; ; i++ {
				shared[i%64] = i
			}
		})
	}
	wg.Wait()
}

//go:noinline
func recursesForever(n int) int {
	if n < 0 {
		return 0
	}
	return recursesForever(n+1) + 1
}

//go:noinline
func unlocksAnUnlockedMutex() {
	var mu sync.Mutex
	mu.Unlock()
}

// crashed runs the child and reads what the runtime wrote.
func crashed(t *testing.T, mode string) (crash, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "crash")
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), crashChildEnv+"="+mode, crashFileEnv+"="+path)
	var exit *exec.ExitError
	if err := cmd.Run(); !errors.As(err, &exit) {
		t.Fatalf("the child did not crash: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	read, ok := parseCrash(string(raw))
	if !ok {
		t.Fatalf("the crash was not read:\n%s", raw)
	}
	return read, string(raw)
}

// THE CRASH THIS TOOLCHAIN WRITES IS READ, kind and place, from the runtime's
// own output rather than a sample of it: a Go release that changes the trace's
// shape fails here.
func TestTheCrashThisToolchainWritesIsRead(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		mode, site string
		kind       FatalKind
		panicKind  panics.Kind
	}{
		{"panic", "internal/issuereport.writesNilMap", FatalPanic, panics.KindNilMapWrite},
		{"explicit", "internal/issuereport.crashChild", FatalPanic, panics.KindExplicit},
		{"concurrent", "internal/issuereport.writesOneMapFromMany.func1", FatalConcurrentMap, ""},
		{"overflow", "internal/issuereport.recursesForever", FatalStackOverflow, ""},
		{"sync", "internal/issuereport.unlocksAnUnlockedMutex", FatalSyncMisuse, ""},
	} {
		t.Run(c.mode, func(t *testing.T) {
			t.Parallel()
			read, raw := crashed(t, c.mode)
			if read.kind != c.kind || read.panicKind != c.panicKind {
				t.Errorf("read %s/%s, want %s/%s:\n%s", read.kind, read.panicKind, c.kind, c.panicKind, raw)
			}
			signal := crashSignal(read)
			if signal.Location.Site != Site(c.site) || signal.Location.File == "" {
				t.Errorf("site = %+v, want %s and its file", signal.Location, c.site)
			}
			if len(signal.Frames) == 0 || signal.Frames[0].Function != signal.Location.Site || len(signal.Frames) > maxFrames {
				t.Errorf("frames = %+v", signal.Frames)
			}
			if signal.Held {
				t.Errorf("a crash in this package was held")
			}
			body, err := json.Marshal(signal.Facts)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), "Acme") || strings.Contains(read.frames[0].File, "Acme") {
				t.Errorf("a value reached the record: %s", body)
			}
		})
	}
}

// moduleTrace is a crash's output as the runtime writes it, the crashing
// goroutine's frames given.
func moduleTrace(head string, frames ...string) string {
	var b strings.Builder
	b.WriteString(head + "\n\ngoroutine 7 gp=0x1 m=0 mp=0x2 [running]:\n")
	for _, fn := range frames {
		b.WriteString(fn + "(0x140001, {0x2a, 0x3})\n\t/srv/acme/build/x.go:42 +0x1c fp=0x1 sp=0x2 pc=0x3\n")
	}
	b.WriteString("created by github.com/crewlet/crewlet/internal/x.start in goroutine 1\n\t/srv/acme/build/x.go:9 +0x9\n")
	return b.String()
}

// A CRASH IS READ BY ITS SHAPE AND THE RUNTIME'S FRAME NAMES, a host that ran out
// of memory or threads is no report at all, and the arguments are never kept.
func TestACrashIsReadByItsShape(t *testing.T) {
	t.Parallel()
	const fault = "[signal SIGSEGV: segmentation violation code=0x2 addr=0xdeadbeef000 pc=0x1045a6f70]"
	cases := []struct {
		name  string
		text  string
		kind  FatalKind
		read  bool
		sites int
	}{
		{"a native fault under the engine", moduleTrace(fault, "runtime.throw", "runtime.sigpanic",
			modulePrefix+"internal/store.(*DB).query"), FatalNativeFault, true, 1},
		{"a native fault on a thread of its own", fault + "\n\ngoroutine 9 [running]:\n" +
			"github.com/tursodatabase/turso-go.call(0x1)\n\t/go/pkg/turso.go:7 +0x1\n", FatalNativeFault, true, 0},
		{"every goroutine asleep", "\ngoroutine 1 [chan receive]:\n" + modulePrefix +
			"internal/x.wait()\n\t/srv/x.go:3 +0x1\n", FatalDeadlock, true, 0},
		{"out of memory", "\nruntime stack:\nruntime.throw({0x1, 0x2})\n\t/go/src/runtime/panic.go:1 +0x1\n" +
			"runtime.(*mcache).refill(0x1)\n\t/go/src/runtime/mcache.go:2 +0x1\n" + moduleTrace("", modulePrefix+"internal/x.f"),
			"", false, 0},
		{"no thread to be had", "\nruntime stack:\nruntime.throw({0x1, 0x2})\n\t/go/src/runtime/panic.go:1 +0x1\n" +
			"runtime.newosproc(0x1)\n\t/go/src/runtime/os_linux.go:2 +0x1\n", "", false, 0},
		{"another fatal error", moduleTrace("", "runtime.throw", modulePrefix+"internal/x.f"), FatalOther, true, 1},
		{"nothing written", "\n\n", "", false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			read, ok := parseCrash(c.text)
			if ok != c.read || read.kind != c.kind {
				t.Fatalf("read %v %q, want %v %q", ok, read.kind, c.read, c.kind)
			}
			if got := len(read.frames); got != c.sites {
				t.Errorf("%d module frames, want %d: %+v", got, c.sites, read.frames)
			}
			for _, f := range read.frames {
				if strings.Contains(f.Function, "0x") || strings.Contains(f.Function, "(0") {
					t.Errorf("a frame kept its arguments: %q", f.Function)
				}
			}
			if !ok {
				return
			}
			signal := crashSignal(read)
			if c.sites == 0 && signal.Location.Site != "" {
				t.Errorf("a crash with no frame of the engine's named %s", signal.Location.Site)
			}
		})
	}
}

// A CRASH ANYBODY COULD STEER IS HELD: any module frame of the crashing goroutine,
// not only the few a report carries, in code that reads outside input or runs a
// tool call on a model's arguments.
func TestACrashOnAnExposedPathIsHeld(t *testing.T) {
	t.Parallel()
	deep := []string{modulePrefix + "internal/x.a", modulePrefix + "internal/x.b", modulePrefix + "internal/x.c",
		modulePrefix + "internal/x.d", modulePrefix + "internal/x.e", modulePrefix + "internal/x.f"}
	for name, c := range map[string]struct {
		frames []string
		held   bool
	}{
		"the engine's own":           {deep, false},
		"a webhook receiver on top":  {[]string{modulePrefix + "internal/api/webhooks.(*Receiver).github"}, true},
		"a tool call far down":       {slices.Concat(deep, []string{modulePrefix + "internal/agent/toolloop.(*dispatcher).execute"}), true},
		"a tool server's call below": {slices.Concat(deep[:1], []string{modulePrefix + "internal/mcp.(*Tool).Call"}), true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			read, ok := parseCrash(moduleTrace("panic: runtime error: index out of range [3] with length 1", c.frames...))
			if !ok {
				t.Fatal("not read")
			}
			if signal := crashSignal(read); signal.Held != c.held {
				t.Errorf("held = %v, want %v", signal.Held, c.held)
			}
		})
	}
}

// A HELD REPORT IS KEPT AND NEVER SENT: the company's record holds it, frames and
// all, for a person to report privately, and the relay hears nothing.
func TestAHeldReportIsKeptAndNeverSent(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	read, _ := parseCrash(moduleTrace("panic: assignment to entry in nil map",
		modulePrefix+"internal/x.parse", modulePrefix+"internal/api/webhooks.(*Receiver).github"))
	signal := crashSignal(read)
	r.report.Observe(context.Background(), signal)
	entry, _ := entryOf(ClassProcessCrashed)
	component, at := entry.placeOf(signal.Location)
	fingerprint := entry.fingerprintOf(component, at.Site, nil, signal.Facts)
	var held coord.IssueReport
	waitFor(t, "the held record", func() bool {
		rec, found, err := r.fleet.IssueReport(context.Background(), string(fingerprint))
		held = rec
		return err == nil && found
	})
	r.idle()
	if held.Status != coord.IssueReportHeld || len(r.relay.received()) != 0 {
		t.Fatalf("record %+v, %d sent", held, len(r.relay.received()))
	}
	rows, err := r.report.List(context.Background())
	if err != nil || len(rows) != 1 || rows[0].Status != string(coord.IssueReportHeld) ||
		!strings.Contains(string(rows[0].Payload), `"frames"`) {
		t.Errorf("rows = %+v, %v", rows, err)
	}
}

// writeCopy writes a node's copy of the setting, as a process before this one did.
func writeCopy(t *testing.T, dir, word string) string {
	t.Helper()
	kept := filepath.Join(dir, NextStartDir)
	if err := os.MkdirAll(kept, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kept, settingFile), []byte(word), 0o600); err != nil {
		t.Fatal(err)
	}
	return kept
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// disarmed ends a case that armed the process's crash output.
func disarmed(t *testing.T) {
	t.Cleanup(func() { _ = debug.SetCrashOutput(nil, debug.CrashOptions{}) })
}

// OFF MEANS OFF ACROSS A RESTART: a node whose copy says off arms nothing, and
// what an earlier process left is deleted unread. Not parallel: crash output is
// the process's.
func TestOffMeansOffAcrossARestart(t *testing.T) {
	disarmed(t)
	dir := t.TempDir()
	kept := writeCopy(t, dir, settingOff)
	for _, name := range []string{crashFile, leftCrashFile} {
		if err := os.WriteFile(filepath.Join(kept, name), []byte(moduleTrace("panic: x", modulePrefix+"internal/x.f")), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	n, err := OpenNextStart(dir, releaseOrigin)
	if err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(kept, crashFile)) || exists(filepath.Join(kept, leftCrashFile)) {
		t.Error("an off copy kept the last crash")
	}
	if left, err := n.Apply(false); err != nil || left != nil {
		t.Errorf("an off revision handed over %v, %v", left, err)
	}
}

// AN ON COPY ARMS AT START, AND THE FIRST REVISION DECIDES: on reads the last
// process's crash once, off deletes it unread and disarms.
func TestAnOnCopyArmsAndTheFirstRevisionDecides(t *testing.T) {
	disarmed(t)
	last := moduleTrace("panic: assignment to entry in nil map", modulePrefix+"internal/x.f")
	for _, revision := range []bool{true, false} {
		dir := t.TempDir()
		kept := writeCopy(t, dir, settingOn)
		if err := os.WriteFile(filepath.Join(kept, crashFile), []byte(last), 0o600); err != nil {
			t.Fatal(err)
		}
		n, err := OpenNextStart(dir, releaseOrigin)
		if err != nil {
			t.Fatal(err)
		}
		armed, err := os.Stat(filepath.Join(kept, crashFile))
		if err != nil || armed.Size() != 0 || armed.Mode().Perm() != 0o600 {
			t.Fatalf("crash output not armed on a fresh 0600 file: %v %v", armed, err)
		}
		left, err := n.Apply(revision)
		if err != nil {
			t.Fatal(err)
		}
		if revision && (len(left) != 1 || left[0].Class != ClassProcessCrashed) {
			t.Errorf("an on revision handed over %+v", left)
		}
		if !revision && left != nil {
			t.Errorf("an off revision read the last crash: %+v", left)
		}
		if exists(filepath.Join(kept, leftCrashFile)) || exists(filepath.Join(kept, crashFile)) != revision {
			t.Errorf("revision %v left the crash files as they were", revision)
		}
		if again, _ := n.Apply(revision); again != nil {
			t.Errorf("the last crash was handed over twice")
		}
		if copied, _ := os.ReadFile(filepath.Join(kept, settingFile)); (string(copied) == settingOn) != revision {
			t.Errorf("the copy says %q after revision %v", copied, revision)
		}
	}
}

// AN INERT BUILD ARMS NOTHING, whatever its copy says.
func TestAnInertBuildKeepsNoCrash(t *testing.T) {
	disarmed(t)
	dir := t.TempDir()
	kept := writeCopy(t, dir, settingOn)
	n, err := OpenNextStart(dir, Origin{Build: Build{Version: "dev"}})
	if err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(kept, crashFile)) {
		t.Error("an inert build armed crash output")
	}
	if left, _ := n.Apply(true); left != nil || exists(filepath.Join(kept, crashFile)) {
		t.Error("an inert build armed on a revision")
	}
}
