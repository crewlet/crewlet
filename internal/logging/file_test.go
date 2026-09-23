package logging

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// sink opens a file sink under t.TempDir with the caps a case cares about,
// and returns it beside the buffer its failures are reported on.
func sink(t *testing.T, name string, maxSizeMB, maxBackups int) (*File, *bytes.Buffer, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	var report bytes.Buffer
	f, err := OpenFile(FileOptions{
		Path:       path,
		MaxSizeMB:  &maxSizeMB,
		MaxBackups: &maxBackups,
	}, &report)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f, &report, path
}

// record is one log line of a known size, so a case can say how many fit in
// a megabyte rather than guessing.
func record(i, size int) []byte {
	line := fmt.Sprintf("line=%06d ", i)
	return append([]byte(line+strings.Repeat("x", size-len(line)-1)), '\n')
}

// A RECORD IS NEVER SPLIT ACROSS TWO FILES.
//
// The whole reason the size check happens before the write rather than after
// it. A `json` log is read a line at a time, and half an object at the end of
// one file and the other half at the start of the next is a parse error in
// whatever is shipping it — the one corruption a rotator can cause by itself.
func TestRotationNeverSplitsARecord(t *testing.T) {
	f, _, path := sink(t, "crewlet.log", 1, 3)

	const size = 1024
	for i := range 1200*2 + 10 { // comfortably past two rotations
		if _, err := f.Write(record(i, size)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	for _, name := range []string{path, path + ".1", path + ".2"} {
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if len(body) == 0 {
			t.Fatalf("%s is empty, so this case is asserting nothing", name)
		}
		if body[len(body)-1] != '\n' {
			t.Errorf("%s does not end on a record boundary: %q", name, tail(body))
		}
		for n, line := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
			if len(line) != size-1 {
				t.Fatalf("%s line %d is %d bytes, not a whole %d-byte record: %q",
					name, n, len(line), size, line)
			}
		}
	}
}

func tail(b []byte) string {
	if len(b) > 64 {
		b = b[len(b)-64:]
	}
	return string(b)
}

// THE CAP IS A CEILING, not a target it may overshoot — on the live file AND
// on every file it rotates into.
//
// This is what the size check being BEFORE the write rather than after it
// buys, and the rotated half is the half that can fail on its own: a sink
// that wrote first and rotated afterwards keeps every live file under the cap
// at the moment anyone looks and still lands each rotated file at the cap
// plus one record. `max_size_mb × (max_backups + 1)` is a disk budget an
// operator sizes a volume from, so it has to be the truth rather than an
// estimate.
func TestNoFileEverExceedsTheCap(t *testing.T) {
	const cap1MB = 1 << 20
	f, _, path := sink(t, "crewlet.log", 1, 2)

	check := func(name string) {
		t.Helper()
		info, err := os.Stat(name)
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if info.Size() > cap1MB {
			t.Fatalf("%s reached %d bytes, past its %d cap",
				name, info.Size(), cap1MB)
		}
	}
	rotated := false
	for i := range 4000 {
		if _, err := f.Write(record(i, 512)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		check(path)
		check(path + ".1")
		check(path + ".2")
		if _, err := os.Stat(path + ".1"); err == nil {
			rotated = true
		}
	}
	if !rotated {
		t.Fatal("nothing rotated, so this case is asserting nothing")
	}
}

// LOWERING max_backups PRUNES WHAT THE HIGHER SETTING LEFT.
//
// The cap is a config value and config values change. A rotation that only
// ever removed the one file at the cap would leave `.3`, `.4` and `.5` on the
// disk for the life of the deployment after a change from 5 to 2 — the estate
// staying at its old size while the document said otherwise, which is the one
// thing the cap exists to make true.
func TestLoweringTheBackupCapPrunesTheOrphans(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "crewlet.log")
	// The estate a `max_backups: 5` deployment left behind.
	for i := 1; i <= 5; i++ {
		if err := os.WriteFile(fmt.Sprintf("%s.%d", path, i), []byte("old\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A sibling this sink did not write, which it must not delete.
	keep := path + ".old"
	if err := os.WriteFile(keep, []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	size, backups := 1, 2
	f, err := OpenFile(FileOptions{Path: path, MaxSizeMB: &size, MaxBackups: &backups}, nil)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	for i := range 1100 {
		if _, err = f.Write(record(i, 1024)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	for i := 3; i <= 5; i++ {
		name := fmt.Sprintf("%s.%d", path, i)
		if _, err = os.Stat(name); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s survived a max_backups of 2: %v", name, err)
		}
	}
	if _, err = os.Stat(path + ".2"); err != nil {
		t.Errorf("the estate was pruned past its own cap: %v", err)
	}
	if _, err = os.Stat(keep); err != nil {
		t.Errorf("the sink deleted a file it did not write: %v", err)
	}
}

// `.1` IS THE NEWEST. An operator reading back an incident opens `.1` first,
// and a stack that shifted the other way would hand them the oldest file
// they have.
func TestTheNewestBackupIsDotOne(t *testing.T) {
	f, _, path := sink(t, "crewlet.log", 1, 3)

	// Two full rotations, each marked so the order is readable.
	for round := range 3 {
		for i := range 1100 {
			if _, err := f.Write(record(round*1_000_000+i, 1024)); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
	}

	first := func(name string) string {
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		return strings.SplitN(string(body), "\n", 2)[0]
	}
	newest, older := first(path+".1"), first(path+".2")
	if newest <= older {
		t.Errorf(".1 (%q) is not newer than .2 (%q); the shift runs the wrong way",
			newest, older)
	}
}

// AND NOTHING IS KEPT PAST max_backups. The cap on the number of files is
// the only thing bounding the disk: a rotation that stopped pruning would
// grow forever while looking exactly like one that worked.
func TestBackupsBeyondTheCapArePruned(t *testing.T) {
	f, _, path := sink(t, "crewlet.log", 1, 2)

	for i := range 1100 * 5 {
		if _, err := f.Write(record(i, 1024)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	if _, err := os.Stat(path + ".2"); err != nil {
		t.Fatalf("the second backup is missing, so the cap was never reached: %v", err)
	}
	if _, err := os.Stat(path + ".3"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a third backup survived a max_backups of 2: %v", err)
	}
}

// ZERO BACKUPS IS A SETTING, and it is the zero value of its own type —
// which is why the config field is a pointer. A deployment that caps its
// disk at one file must get one file, not the default five.
func TestZeroBackupsKeepsNone(t *testing.T) {
	f, _, path := sink(t, "crewlet.log", 1, 0)

	// A sibling this sink did not write. `0` is the setting under which
	// "everything from index 0 upward" is pruned, so it is the one where a
	// prune that did not check the suffix is a NUMBER would take an
	// operator's own file with it.
	keep := path + ".old"
	if err := os.WriteFile(keep, []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for i := range 1100 * 2 {
		if _, err := f.Write(record(i, 1024)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the live file is gone: %v", err)
	}
	if _, err := os.Stat(path + ".1"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a backup was kept although max_backups is 0: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("the sink deleted a file it did not write: %v", err)
	}
}

// A RECORD LARGER THAN THE WHOLE CAP IS WRITTEN WHOLE. The alternative is a
// sink that rotates on every line and never records the one thing it was
// asked to — a stack trace, a vendor error body, an MCP server's stderr.
func TestARecordLargerThanTheCapIsWrittenWhole(t *testing.T) {
	f, _, path := sink(t, "crewlet.log", 1, 2)

	huge := record(1, 3<<20)
	if _, err := f.Write(huge); err != nil {
		t.Fatalf("write: %v", err)
	}
	// ASSERTED BEFORE THE SECOND WRITE, which would mask it: nothing has
	// rotated, because an EMPTY file takes the oversized record whole.
	// That is the `f.size > 0` half of the guard, and without this the
	// case could not fail on it — a sink that rotated the empty file first
	// would still land the record whole in `.1` and spend one extra
	// rotation per oversized line, shifting the backup stack out from
	// under the history somebody is reading.
	if _, err := os.Stat(path + ".1"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the empty live file was rotated before the oversized record: %v", err)
	}
	if body, err := os.ReadFile(path); err != nil {
		t.Fatalf("reading the live file: %v", err)
	} else if len(body) != len(huge) {
		t.Fatalf("the live file holds %d bytes, not the %d written whole",
			len(body), len(huge))
	}
	// And the next ordinary record rotates, because the file is over.
	if _, err := f.Write(record(2, 512)); err != nil {
		t.Fatalf("write: %v", err)
	}

	rotated, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("the oversized record did not end up in a rotated file: %v", err)
	}
	if len(rotated) != len(huge) {
		t.Errorf("the oversized record is %d bytes on disk, not the %d written",
			len(rotated), len(huge))
	}
}

// REOPENING APPENDS, and counts what is already there.
//
// Rotating on start would be worse than useless in the case that matters: a
// restart loop is exactly when the previous incarnation's last lines are the
// evidence, and five restarts would push them off the end of the stack.
func TestReopeningAppendsAndCountsWhatIsAlreadyThere(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "crewlet.log")
	size, backups := 1, 2

	first, err := OpenFile(FileOptions{Path: path, MaxSizeMB: &size, MaxBackups: &backups}, nil)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	// Exactly one megabyte: the cap is reached but not passed, so nothing
	// has rotated yet and the next record is the one that must.
	for i := range 1024 {
		if _, err = first.Write(record(i, 1024)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err = first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := OpenFile(FileOptions{Path: path, MaxSizeMB: &size, MaxBackups: &backups}, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if _, err = second.Write(record(1024, 1024)); err != nil {
		t.Fatalf("write after reopen: %v", err)
	}

	// The first run left the file exactly at its cap, so the one record
	// this run wrote is what crosses it — which the reopened sink can only
	// know by having stat'd the file it opened rather than starting its
	// count at zero. The previous run's lines are therefore in `.1`, whole:
	// rotated, never truncated.
	rotated, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("the reopened sink started counting from zero, so it never "+
			"rotated: %v", err)
	}
	if !strings.Contains(string(rotated), "line=000000 ") {
		t.Error("the reopen truncated the file the previous run wrote")
	}
	if n := strings.Count(string(rotated), "\n"); n != 1024 {
		t.Errorf("the rotated file holds %d records, want the 1024 the first "+
			"run wrote", n)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if !strings.Contains(string(body), "line=001024 ") {
		t.Errorf("the record written after the reopen is missing: %q", body)
	}
}

// THE DIRECTORY IS CREATED, and both it and the file are private.
//
// A log line is not a public document, and a path under a directory the
// operator has not made yet is the ordinary case (`/var/log/crewlet/crewlet.log`)
// rather than a mistake.
func TestTheDirectoryIsCreatedAndBothArePrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "crewlet.log")
	f, err := OpenFile(FileOptions{Path: path}, nil)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("the directory was not created: %v", err)
	}
	if got := dir.Mode().Perm(); got != DirMode {
		t.Errorf("log directory mode = %o, want %o", got, DirMode)
	}
	file, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := file.Mode().Perm(); got != FileMode {
		t.Errorf("log file mode = %o, want %o", got, FileMode)
	}
}

// A FAILING SINK SAYS SO ONCE, AND SAYS SO AGAIN WHEN IT RECOVERS.
//
// slog throws away whatever a handler's Handle returns, so a write error has
// nowhere to go by the ordinary route: a log disk that filled would simply
// stop recording, silently, which is the failure this project keeps finding
// in its own past. Once per episode rather than once per line, or the
// complaint buries the output it is complaining about.
func TestAFailureIsReportedOnceAndSoIsTheRecovery(t *testing.T) {
	f, report, _ := sink(t, "crewlet.log", 1, 1)

	// Close the descriptor behind the sink's back — the shape of a file
	// handle lost to something outside this package.
	f.mu.Lock()
	_ = f.f.Close()
	f.mu.Unlock()

	if _, err := f.Write([]byte("first\n")); err == nil {
		t.Fatal("a write to a closed descriptor reported success")
	}
	afterFirst := report.String()
	if !strings.Contains(afterFirst, "log file") {
		t.Fatalf("the failure was not announced: %q", afterFirst)
	}

	// The sink gave the descriptor up, so this one reopens and succeeds —
	// and says so, because an operator watching stderr needs to know the
	// record is whole again.
	if _, err := f.Write([]byte("second\n")); err != nil {
		t.Fatalf("the sink did not recover: %v", err)
	}
	got := report.String()
	if !strings.Contains(got, "writing again") {
		t.Errorf("the recovery was not announced: %q", got)
	}
	if n := strings.Count(got, "\n"); n != 2 {
		t.Errorf("the episode produced %d report lines, want one failure and "+
			"one recovery: %q", n, got)
	}
}

// AND IT DOES NOT REPEAT ITSELF while the failure lasts.
func TestAPersistentFailureIsAnnouncedOnce(t *testing.T) {
	f, report, path := sink(t, "crewlet.log", 1, 1)

	// Replace the directory with something no file can be created in, so
	// every reopen fails too.
	f.mu.Lock()
	_ = f.f.Close()
	f.f = nil
	f.path = filepath.Join(path, "impossible", "crewlet.log")
	f.mu.Unlock()

	for range 5 {
		if _, err := f.Write([]byte("line\n")); err == nil {
			t.Fatal("a write into an impossible path reported success")
		}
	}
	if n := strings.Count(report.String(), "\n"); n != 1 {
		t.Errorf("a persistent failure produced %d lines, want 1: %q",
			n, report.String())
	}
}

// WHOLE RECORDS, FROM EVERY GOROUTINE. Every subsystem in the engine logs,
// and a record interleaved with another's is a line nothing can parse — the
// same guarantee syncWriter gives the console sink.
func TestConcurrentWritesDoNotInterleave(t *testing.T) {
	f, _, path := sink(t, "crewlet.log", 8, 1)

	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 200 {
				if _, err := f.Write(record(g*1000+i, 256)); err != nil {
					t.Errorf("write: %v", err)
					return
				}
			}
		})
	}
	wg.Wait()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(lines) != 8*200 {
		t.Fatalf("got %d lines, want %d", len(lines), 8*200)
	}
	for n, line := range lines {
		if len(line) != 255 {
			t.Fatalf("line %d is %d bytes, so two writes interleaved: %q",
				n, len(line), line)
		}
	}
}

// A WRITE AFTER Close IS AN ERROR, not a silent drop. Closing is the last
// thing a shutdown does and the sink is detached from the handler first, so
// reaching this means the ordering was broken and that is worth knowing.
func TestWritingAfterCloseIsRefused(t *testing.T) {
	f, report, _ := sink(t, "crewlet.log", 1, 1)
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := f.Write([]byte("late\n")); !errors.Is(err, errFileClosed) {
		t.Errorf("write after close = %v, want errFileClosed", err)
	}
	// AND IT SAYS SO OUT LOUD. slog throws a handler's error away, so an
	// error nobody prints is indistinguishable from a sink that is
	// working — which is what makes the CLI's detach-then-close ordering
	// checkable at all rather than a comment nothing enforces.
	if !strings.Contains(report.String(), "closed") {
		t.Errorf("lines were dropped in silence after the close: %q", report.String())
	}
	// AND CLOSING TWICE IS FINE: a deferred teardown may run beside an
	// explicit one, and a second close must not fail a clean shutdown.
	if err := f.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// THE CAPS THAT ARE NOT SETTINGS ARE REFUSED BY NAME rather than resolved to
// a default. A file that rotates every zero bytes is not something an
// operator meant, and substituting 100 MB for it silently is how they end up
// unable to tell what their own document says.
func TestImpossibleOptionsAreRefused(t *testing.T) {
	zero, negative := 0, -1
	// 1<<43 MB is where int64(mb)<<20 wraps; the ceiling sits far below it.
	wraps, overSize, overBackups := 1<<43, MaxSizeMBCeiling+1, MaxBackupsCeiling+1
	for _, tc := range []struct {
		name string
		opts FileOptions
		want string
	}{
		{"no path", FileOptions{}, "path"},
		{"a zero size", FileOptions{Path: "x.log", MaxSizeMB: &zero}, "max_size_mb"},
		{"a negative size", FileOptions{Path: "x.log", MaxSizeMB: &negative}, "max_size_mb"},
		{"a negative backup count", FileOptions{Path: "x.log", MaxBackups: &negative}, "max_backups"},
		// THE CEILINGS. A size at or above 1<<43 MB wraps the byte
		// arithmetic to zero or negative, which makes the guard in Write
		// true for every record and rotates on EVERY LINE — the exact
		// inverse of the enormous number an operator wrote, and silent,
		// because rotating is what success looks like. A backup count is
		// a rename count per rotation, so a huge one stalls instead.
		{"a size that wraps the byte arithmetic",
			FileOptions{Path: "x.log", MaxSizeMB: &wraps}, "max_size_mb"},
		{"a size one past the ceiling",
			FileOptions{Path: "x.log", MaxSizeMB: &overSize}, "max_size_mb"},
		{"a backup count that would stall the rotation",
			FileOptions{Path: "x.log", MaxBackups: &overBackups}, "max_backups"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			if opts.Path != "" {
				opts.Path = filepath.Join(t.TempDir(), opts.Path)
			}
			f, err := OpenFile(opts, nil)
			if err == nil {
				_ = f.Close()
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error does not name %s: %v", tc.want, err)
			}
		})
	}
}

// AND THE DEFAULTS ARE THE DOCUMENTED ONES. They are named in the config
// field's own description and in the deployment guide, so a change here is a
// change to what two documents promise.
func TestUnsetCapsTakeTheDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crewlet.log")
	f, err := OpenFile(FileOptions{Path: path}, nil)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if want := int64(DefaultMaxSizeMB) << 20; f.maxSize != want {
		t.Errorf("max size = %d, want %d", f.maxSize, want)
	}
	if f.maxBackups != DefaultMaxBackups {
		t.Errorf("max backups = %d, want %d", f.maxBackups, DefaultMaxBackups)
	}
}

// THE SINK IS A LOG DESTINATION, end to end: a logger configured with it
// writes whole records into the file in the format that sink was given.
func TestAFileSinkCarriesTheEngineLog(t *testing.T) {
	f, _, path := sink(t, "crewlet.log", 1, 1)
	Configure(slog.LevelInfo, FormatConsole, io.Discard)
	t.Cleanup(func() { Configure(slog.LevelInfo, FormatConsole, io.Discard) })
	SetFile(FileSink{Writer: f, Format: FormatJSON})

	Get("seat.host").Info("seat_claimed", "seat", "eng.alice")
	SetFile(FileSink{})

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(body)), "{") {
		t.Errorf("the file did not get the format it was installed with: %q", body)
	}
	if !strings.Contains(string(body), "seat_claimed") {
		t.Errorf("the line did not reach the file: %q", body)
	}
}

// THE CEILING IS BELOW THE WRAP, with room to spare.
//
// [MaxSizeMBCeiling] exists only to keep `int64(maxSizeMB) << 20` positive,
// so the one thing that must stay true of it is the shift — and it is
// arithmetic nobody should have to redo by hand when the constant moves.
func TestTheSizeCeilingCannotWrapTheByteArithmetic(t *testing.T) {
	bytes := int64(MaxSizeMBCeiling) << 20
	if bytes <= 0 {
		t.Fatalf("MaxSizeMBCeiling (%d) already wraps: %d bytes",
			MaxSizeMBCeiling, bytes)
	}
	// And the value just past it is the one the refusal is protecting
	// against, so a ceiling raised to a safe-looking number that is not
	// safe fails here rather than in production.
	if int64(MaxSizeMBCeiling)<<20 < int64(DefaultMaxSizeMB)<<20 {
		t.Fatal("the ceiling is below the default, so no valid value exists")
	}
}

// THE FAILURE NOTICE SAYS WHAT ACTUALLY BECOMES OF THE DROPPED LINES.
//
// It used to say "logging continues on stderr" unconditionally. Under
// `logging.stderr: false` that is exactly backwards: the file is then the
// only destination this node has, so a failure loses the log rather than
// diverting it — and the single line reaching the operator told them it did
// not. A notice that is wrong in the worst case is worse than none, because
// the worst case is the one somebody acts on.
func TestTheFailureNoticeKnowsWhetherAnythingElseIsInstalled(t *testing.T) {
	for _, tc := range []struct {
		name       string
		consoleOff bool
		want, not  string
	}{
		{
			name: "with the console still installed",
			want: "logging continues on stderr", not: "LOST",
		},
		{
			name: "with the file as the only destination", consoleOff: true,
			want: "LOST", not: "logging continues on stderr",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "crewlet.log")
			var report bytes.Buffer
			size, backups := 1, 1
			f, err := OpenFile(FileOptions{
				Path: path, MaxSizeMB: &size, MaxBackups: &backups,
			}, &report)
			if err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			t.Cleanup(func() { _ = f.Close() })

			Configure(slog.LevelInfo, FormatText, io.Discard)
			SetFile(FileSink{Writer: f, Format: FormatText})
			t.Cleanup(func() {
				SetConsole(true)
				Configure(slog.LevelInfo, FormatConsole, io.Discard)
			})
			if tc.consoleOff {
				SetConsole(false)
			}

			// Close the descriptor behind the sink's back, the shape of a
			// file handle lost to something outside this package.
			f.mu.Lock()
			_ = f.f.Close()
			f.mu.Unlock()
			if _, err = f.Write([]byte("a_line\n")); err == nil {
				t.Fatal("a write to a closed descriptor reported success")
			}

			got := report.String()
			if !strings.Contains(got, tc.want) {
				t.Errorf("the notice does not say %q: %q", tc.want, got)
			}
			if strings.Contains(got, tc.not) {
				t.Errorf("the notice claims %q, which is false here: %q", tc.not, got)
			}
		})
	}
}

// A PARTIAL WRITE LEAVES NO FRAGMENT BEHIND.
//
// os.File returns a short count with its error — a disk that fills mid-write
// reports the bytes it took alongside ENOSPC — and a fragment left in the
// live file is the exact corruption the size check is arranged to prevent:
// the next write rotates half a record into a backup, and whatever ships the
// log hits half a JSON object at a file boundary.
//
// Driven through [File.rollBackPartial] rather than through Write, because
// os.File short-writes only on a full disk or a hit RLIMIT_FSIZE and neither
// is arrangeable on every platform this ships to.
func TestAPartialWriteIsRolledBack(t *testing.T) {
	f, _, path := sink(t, "crewlet.log", 1, 2)

	whole := []byte("a_whole_record\n")
	if _, err := f.Write(whole); err != nil {
		t.Fatalf("write: %v", err)
	}
	before := f.size

	// The fragment a failed write would have left, and the rollback.
	fragment := []byte("half_a_rec")
	f.mu.Lock()
	if _, err := f.f.Write(fragment); err != nil {
		t.Fatalf("planting the fragment: %v", err)
	}
	f.size += int64(len(fragment))
	n, err := f.rollBackPartial(before, len(fragment), errors.New("no space left on device"))
	f.mu.Unlock()

	if err == nil {
		t.Fatal("the rollback swallowed the write error")
	}
	if n != 0 {
		t.Errorf("a rolled-back write reported %d bytes written, want 0", n)
	}
	if f.size != before {
		t.Errorf("the size counter is %d, want %d — the next rotation would "+
			"fire early and rotate the fragment", f.size, before)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if string(body) != string(whole) {
		t.Errorf("the fragment survived in the file: %q", body)
	}
}

// AND A ROLLBACK THAT CANNOT HAPPEN SAYS SO, rather than leaving half a
// record in the file with nothing pointing at it.
func TestAFailedRollbackIsNamedInTheError(t *testing.T) {
	f, _, _ := sink(t, "crewlet.log", 1, 2)

	// A closed descriptor cannot be truncated.
	f.mu.Lock()
	_ = f.f.Close()
	n, err := f.rollBackPartial(0, 10, errors.New("no space left on device"))
	f.mu.Unlock()

	if n != 10 {
		t.Errorf("a failed rollback reported %d bytes written, want the 10 the "+
			"file still holds", n)
	}
	if !strings.Contains(err.Error(), "could not be rolled back") {
		t.Errorf("the error does not mention the fragment left behind: %v", err)
	}
	if !strings.Contains(err.Error(), "no space left on device") {
		t.Errorf("the original write error was lost: %v", err)
	}
}

// `<path>.0` IS NOT OURS. This rotator numbers backups from 1 and has never
// created a `.0`, so a prune that started at 0 — which `max_backups: 0` asks
// for in so many words — would delete an operator's own file on the first
// rotation.
func TestAZeroSuffixSiblingIsNeverPruned(t *testing.T) {
	f, _, path := sink(t, "crewlet.log", 1, 0)

	zero := path + ".0"
	if err := os.WriteFile(zero, []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := range 1100 * 2 {
		if _, err := f.Write(record(i, 1024)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if _, err := os.Stat(zero); err != nil {
		t.Errorf("the sink deleted `<path>.0`, which it never wrote: %v", err)
	}
}

// A LOWERED CAP CONVERGES AT OPEN, not only at the next rotation.
//
// The rotation that would prune the old setting's files may be days away on
// a quiet node, or never — so a deployment that lowered max_backups would
// keep paying for the higher one's disk with nothing saying why.
func TestOpeningPrunesBackupsAboveTheCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "crewlet.log")
	for i := 1; i <= 6; i++ {
		if err := os.WriteFile(fmt.Sprintf("%s.%d", path, i), []byte("old\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	keep := path + ".old"
	if err := os.WriteFile(keep, []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	size, backups := 1, 2
	f, err := OpenFile(FileOptions{Path: path, MaxSizeMB: &size, MaxBackups: &backups}, io.Discard)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	// `.1` and `.2` are the cap, and nothing rotated, so both survive.
	for i := 1; i <= 2; i++ {
		if _, err = os.Stat(fmt.Sprintf("%s.%d", path, i)); err != nil {
			t.Errorf("a backup inside the cap was pruned at open: %v", err)
		}
	}
	for i := 3; i <= 6; i++ {
		name := fmt.Sprintf("%s.%d", path, i)
		if _, err = os.Stat(name); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s survived an open at max_backups 2: %v", name, err)
		}
	}
	if _, err = os.Stat(keep); err != nil {
		t.Errorf("the sink deleted a file it did not write: %v", err)
	}
}
