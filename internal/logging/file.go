package logging

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// DefaultMaxSizeMB is how large the live log file grows before it rotates.
//
// A line here is a structured record, not prose: ~200 bytes in `text`,
// ~300-600 in `json` once `component`, `node`, the event name and a handful
// of attributes are on it. 100 MB is therefore on the order of 200 000 lines
// — days of an `info`-level company, and hours of a `debug` one, which is the
// window an incident is actually read back over. It is also small enough that
// `grep` over one file is instant and that the file can be attached to a bug
// report, which a multi-gigabyte log cannot.
const DefaultMaxSizeMB = 100

// MaxSizeMBCeiling is the largest max_size_mb that still means what it says.
//
// The cap is held in BYTES — `int64(maxSizeMB) << 20` — so a value at or
// above 1<<43 MB wraps int64 to zero or negative. A non-positive cap makes
// the guard in [File.Write] true for every record after the first, so the
// sink rotates on EVERY LINE and the whole estate collapses to max_backups+1
// single-record files: the exact inverse of what the number asked for, and
// silent, because rotating is what success looks like.
//
// That is not an abstract worry. The documented way to run beside an
// external logrotate(8) is to give this field "a size this engine will never
// reach", so the one instruction in the guide is the one that walks toward
// the cliff. 1<<30 MB is a pebibyte — past any filesystem this ships to
// (ext4 caps a single file at 16 TiB) and 8192 times under the wrap.
const MaxSizeMBCeiling = 1 << 30

// MaxBackupsCeiling is the largest max_backups a rotation can honour.
//
// [File.rotate] renames max_backups-1 files each time it fires, so the value
// is a syscall count as well as a history depth. Unbounded, a config typo of
// a few billion does not wrap — it HANGS the first rotation for hours in
// ENOENT renames, with the process still logging and nothing saying why.
// A thousand rotated files is already far past what anybody reads back, and
// bounds a rotation at a millisecond of metadata work.
const MaxBackupsCeiling = 1000

// DefaultMaxBackups is how many rotated files are kept beside the live one.
//
// Five plus the live file bounds the estate at 600 MB with the default size —
// a log directory that fits beside the store on the smallest host anyone runs
// this on, and enough history that a restart loop overnight has not thrown
// away the first failure by morning.
const DefaultMaxBackups = 5

// DirMode is the mode of a log directory this package creates, and FileMode
// the mode of the log file itself.
//
// A log line is redacted (see internal/redact) but it is not a public
// document: it carries seat handles, channel names, repository and issue
// identifiers, and whatever a vendor put in an error string. The engine's own
// files are 0600 for the same reason — see internal/hostbox — and a deployment
// that wants its logs readable by a shipper running as another user says so
// with its own `chmod`, which is a decision an operator makes rather than one
// a default makes for them.
const (
	DirMode  os.FileMode = 0o700
	FileMode os.FileMode = 0o600
)

// FileOptions is what an operator asked for about the log file.
//
// The two caps are POINTERS so that "nothing was said" and "0" are different
// answers: `max_backups: 0` is the deployment that wants its disk capped at
// one file and no history, and reading it as "apply the default" would give
// that operator five files they asked not to have. See [File].
type FileOptions struct {
	// Path is the live log file. A relative path is relative to the
	// process's working directory, exactly as `store.path` is.
	Path string
	// MaxSizeMB is the size the live file reaches before it rotates. Nil
	// is [DefaultMaxSizeMB]; a value below 1 is refused by the config
	// validator rather than clamped here.
	MaxSizeMB *int
	// MaxBackups is how many rotated files are kept. Nil is
	// [DefaultMaxBackups]; 0 keeps none, so the estate is one file.
	MaxBackups *int
}

// File is the engine's log file: an io.Writer that rotates itself.
//
// # Why rotation is built in rather than left to logrotate(8)
//
// Because the alternative is a disk-fill incident with the engine's name on
// it. A node writes for months without being looked at, and `crewlet run`
// runs in containers as often as under systemd — where there is no
// logrotate, no cron to drive it, and no way to signal a reopen. A log sink
// that grows without bound is not a feature with a caveat; it is the one
// failure this whole file exists to avoid, so the cap is not optional and
// there is no "never rotate" spelling. An operator who already runs
// logrotate can point it at this file with `copytruncate` and set a size
// this engine will simply never reach.
//
// # A record is never split across two files
//
// Two things buy that, and both have to hold. Rotation happens only BETWEEN
// Write calls, and every handler this package installs — slog's own text and
// JSON handlers, and [consoleHandler] — renders a whole record into a buffer
// and issues exactly ONE Write for it. A handler that wrote a record in two
// calls would break this and nothing here could detect it. Half a JSON object
// at the end of one file and half at the start of the next is a parse error
// in whatever is shipping the log — the one corruption a rotator can cause on
// its own.
//
// The size check then happens BEFORE the write that would overflow rather
// than after it, which is what keeps every file at or under the cap rather
// than at the cap plus one record. That matters because the cap is a disk
// budget: `max_size_mb × (max_backups + 1)` has to be the truth, not an
// estimate. A single record larger than the whole cap is the one exception —
// it is written whole into an empty file rather than rotating forever around
// something that can never fit.
//
// # A write failure is reported, not swallowed, and never fails a turn
//
// slog discards whatever a handler's Handle returns, so an error reaching
// [File.Write] has nowhere to go by the ordinary route: a log disk that
// filled would simply stop recording, silently, which is the failure mode
// this project keeps finding in its own past. So the first failure of an
// episode is announced on the report writer — stderr — and the recovery is
// announced too. It is never reported through slog itself: a logger logging
// about its own sink is a loop.
//
// The notice says what becomes of the dropped records, which is not always
// the same thing — see [fateOfTheseLines]. Under `logging.stderr: false`
// this file is the only destination the node has, so a failure here loses
// the log rather than diverting it, and the notice says so in those words.
type File struct {
	path       string
	maxSize    int64
	maxBackups int
	// report is where a write failure is announced. Never a slog logger —
	// see the type doc.
	report io.Writer

	// mu serialises whole records onto the file, the same guarantee
	// syncWriter gives the console sink: every goroutine in the engine
	// logs, and a record split by another goroutine's write is a line
	// nothing can parse.
	mu     sync.Mutex
	f      *os.File
	size   int64
	failed bool
	closed bool
}

// errFileClosed is what a write after [File.Close] answers.
//
// Closing is the LAST thing a shutdown does and the sink is detached from
// the handler first (see the CLI's runEngine), so reaching this means the
// ordering was broken — which is worth an error AND a line on the report
// writer rather than a silent drop. slog discards a handler's error, so the
// error alone would be indistinguishable from working.
var errFileClosed = errors.New("log file is closed")

// OpenFile opens (or creates) the log file and returns the sink to install.
//
// It APPENDS to an existing file rather than rotating on start: a restart
// loop is exactly when the previous incarnation's last lines matter, and a
// rotate-on-open would push the interesting file one further down the stack
// on every boot — five restarts and the first failure is gone.
//
// It fails rather than falling back to stderr alone. Every other bad value
// in this package resolves to a default because a misspelled log level must
// never be why a company will not boot; a path is not that. An operator who
// configured a durable record and silently did not get one has no way to
// discover it, and the error names the path and the permission that has to
// change.
func OpenFile(opts FileOptions, report io.Writer) (*File, error) {
	if opts.Path == "" {
		return nil, errors.New("logging.file.path is empty; name the file to write or remove the block")
	}
	if report == nil {
		report = os.Stderr
	}
	maxSizeMB := DefaultMaxSizeMB
	if opts.MaxSizeMB != nil {
		maxSizeMB = *opts.MaxSizeMB
	}
	if maxSizeMB < 1 {
		return nil, fmt.Errorf(
			"logging.file.max_size_mb is %d; it must be at least 1, because a "+
				"log file with no ceiling fills the disk the engine runs on",
			maxSizeMB)
	}
	if maxSizeMB > MaxSizeMBCeiling {
		return nil, fmt.Errorf(
			"logging.file.max_size_mb is %d; it must be at most %d (a pebibyte). "+
				"The cap is held in bytes, so a larger value wraps to zero or "+
				"negative and the file would rotate on every single line — the "+
				"opposite of what it asks for. To run beside logrotate(8), any "+
				"size this host cannot fill will do",
			maxSizeMB, MaxSizeMBCeiling)
	}
	maxBackups := DefaultMaxBackups
	if opts.MaxBackups != nil {
		maxBackups = *opts.MaxBackups
	}
	if maxBackups < 0 || maxBackups > MaxBackupsCeiling {
		return nil, fmt.Errorf(
			"logging.file.max_backups is %d; it must be between 0 (keep no "+
				"rotated files) and %d — each rotation renames every kept file, "+
				"so a larger value stalls the rotation rather than keeping more "+
				"history", maxBackups, MaxBackupsCeiling)
	}

	f := &File{
		path:       opts.Path,
		maxSize:    int64(maxSizeMB) << 20,
		maxBackups: maxBackups,
		report:     report,
	}
	if err := f.open(); err != nil {
		return nil, err
	}
	return f, nil
}

// Path is the live file this sink writes, for the line that says where the
// logs went.
func (f *File) Path() string { return f.path }

// open creates the directory and opens the live file for appending.
//
// The size is read from the file rather than counted from zero, or an
// appended-to file would be allowed to grow by a whole cap beyond it on
// every restart.
func (f *File) open() error {
	dir := filepath.Dir(f.path)
	if err := os.MkdirAll(dir, DirMode); err != nil {
		return fmt.Errorf("logging.file.path %q: create %s: %w", f.path, dir, err)
	}
	file, err := os.OpenFile(f.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, FileMode)
	if err != nil {
		return fmt.Errorf("logging.file.path %q: %w", f.path, err)
	}
	size := int64(0)
	if info, statErr := file.Stat(); statErr == nil {
		size = info.Size()
	}
	f.f, f.size = file, size
	return nil
}

// Write puts one whole record in the file, rotating first if it would not fit.
func (f *File) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		// REPORTED, not dropped in silence. Closing is the last thing a
		// shutdown does and the sink is detached from the handler first
		// (see the CLI's attachLogFile), so a line arriving here means
		// that ordering was broken and every line after it is being
		// thrown away. A logger that quietly stopped recording is the
		// failure this whole file is written against.
		return 0, f.note(errFileClosed)
	}
	// A REOPEN RATHER THAN A DEAD SINK. The file handle is nil only after
	// an open or a rotate failed, and the cause is usually transient and
	// outside this process — a directory replaced under a bind mount, a
	// full disk that a sweep then cleared. Retrying costs one syscall on
	// the lines after a failure and nothing at all on a healthy node.
	if f.f == nil {
		if err := f.open(); err != nil {
			return 0, f.note(err)
		}
	}
	// THE CHECK IS BEFORE THE WRITE — see the type doc. `f.size > 0` is
	// what stops a record larger than the whole cap rotating on every
	// line: an empty file takes it whole.
	if f.size > 0 && f.size+int64(len(p)) > f.maxSize {
		if err := f.rotate(); err != nil {
			return 0, f.note(err)
		}
	}
	n, err := f.f.Write(p)
	f.size += int64(n)
	if err != nil {
		// THE DESCRIPTOR IS GIVEN UP ON, not just this write. The two
		// failures that recover on their own recover only through a
		// fresh open — a file replaced underneath a bind mount, a
		// descriptor closed by something outside this package — and the
		// one that does not (a full disk) pays one extra openat per line
		// on a node that is already recording nothing. Holding a
		// descriptor that has failed once buys the opposite: a sink that
		// stays dead after the cause has gone away.
		_ = f.f.Close()
		f.f = nil
		return n, f.note(err)
	}
	f.recovered()
	return n, nil
}

// rotate renames the live file down the stack and opens a fresh one.
//
// # Why a numbered shift rather than a timestamp in the name
//
// A timestamped name (`crewlet-2026-09-13T10-00-00.log`) needs a clock, a
// scan and a sort to work out which files to prune, and the sort has to keep
// agreeing with the format string forever. The shift needs neither: `.1` is
// always the newest rotated file and `.N` always the oldest, pruning is
// removing one name, and the whole thing is deterministic in a test. The
// cost is N renames per rotation with N bounded by max_backups — metadata
// operations, on a path that runs once per 100 MB.
//
// Everything past the cap is pruned FIRST, so nothing is renamed over a file
// that is still wanted, and a missing file at any step is not an error: an
// operator clearing out `.3` by hand must not break the next rotation.
func (f *File) rotate() error {
	if f.f != nil {
		// A close error is reported but not fatal: the bytes are the
		// kernel's now, and refusing to rotate would leave the sink
		// pinned to a file it has already given up on.
		_ = f.f.Close()
		f.f = nil
	}
	// Every backup the shift below would push past the cap, gone before
	// the shift rather than after it.
	if err := f.pruneFrom(f.maxBackups); err != nil {
		return err
	}
	if f.maxBackups == 0 {
		// KEEP NONE means the estate is one file: the live one is
		// dropped rather than renamed. This is the explicit `0`, never
		// an unset value — see [FileOptions].
		if err := os.Remove(f.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return f.open()
	}
	for i := f.maxBackups - 1; i >= 1; i-- {
		if err := os.Rename(f.backup(i), f.backup(i+1)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := os.Rename(f.path, f.backup(1)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return f.open()
}

// pruneFrom removes every rotated file numbered n or higher.
//
// # Why the directory is read rather than counted down from max_backups
//
// Because the cap is a config value, and config values change. Lowering
// `max_backups` from 5 to 2 with a rotation that only ever removed `.2`
// would leave `.3`, `.4` and `.5` on the disk for the life of the
// deployment: the estate would stay at its old size while the document said
// otherwise, which is the one thing this cap exists to make true. Reading the
// directory is one readdir per rotation — once per `max_size_mb` of log —
// and it converges on the configured number from either direction.
//
// # It deletes only what it wrote
//
// A sibling that is not `<path>.<number>` is somebody else's: an operator's
// own `crewlet.log.old`, an editor's swap file, a shipper's cursor. Those are
// left alone, whatever they are named.
func (f *File) pruneFrom(n int) error {
	dir := filepath.Dir(f.path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	prefix := filepath.Base(f.path) + "."
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, prefix) {
			continue
		}
		i, convErr := strconv.Atoi(name[len(prefix):])
		if convErr != nil || i < n {
			continue
		}
		if err = os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// backup names the i-th rotated file: `crewlet.log.1` is the newest.
func (f *File) backup(i int) string { return fmt.Sprintf("%s.%d", f.path, i) }

// note announces the first failure of an episode and returns the error it
// was given, so the caller stays honest about what happened.
//
// ONCE PER EPISODE, not once per line: a full disk fails every write, and a
// process that printed a line about it for each one would bury the engine's
// own output under its complaint about not being able to write it.
func (f *File) note(err error) error {
	if !f.failed {
		f.failed = true
		fmt.Fprintf(f.report, "crewlet: log file %s: %v — %s\n",
			f.path, err, fateOfTheseLines())
	}
	return err
}

// fateOfTheseLines says what actually happens to the records this sink is
// dropping, which depends on whether anything else is installed.
//
// It used to say "logging continues on stderr" unconditionally, and under
// `logging.stderr: false` that was exactly backwards: the file is then the
// ONLY destination, so a failure loses the log entirely — and the single
// line reaching the operator told them it did not. A notice that is wrong in
// the worst case is worse than no notice, because it is the case somebody
// acts on.
//
// Reading [current] takes [mu] while [File.mu] is already held. That
// ordering is safe and must stay one-directional: nothing under mu writes to
// a log file — [install] builds handlers and prints its own backstop notice
// to the CONSOLE writer — so mu is never taken before File.mu.
func fateOfTheseLines() string {
	mu.Lock()
	defer mu.Unlock()
	if current.consoleOff {
		return "AND THESE LOG LINES ARE LOST: logging.stderr is false, so this " +
			"file is the only destination this node has"
	}
	return "logging continues on stderr"
}

// recovered closes an episode, so the next failure is announced again and an
// operator watching stderr is told the record is whole once more.
func (f *File) recovered() {
	if f.failed {
		f.failed = false
		fmt.Fprintf(f.report, "crewlet: log file %s: writing again\n", f.path)
	}
}

// Close closes the live file. Detach the sink from the logger first — see
// [errFileClosed].
func (f *File) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	if f.f == nil {
		return nil
	}
	err := f.f.Close()
	f.f = nil
	return err
}
