package harness

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/crewlet/crewlet/internal/flock"
	"github.com/crewlet/crewlet/internal/procgroup"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// EVERY CHILD THIS PACKAGE STARTS FOR A HARNESS WRITES ITS STDOUT AND STDERR
// INTO FILES OF ITS OWN, NEVER A PIPE, but for the operator's terminal
// commands ([Provider.runInCredentialHome]), whose output is the CLI's own
// stdout and stderr, which the engine reads none of.
//
// A pipe is where a harness's answer was lost. Node documents that a write to a
// pipe is asynchronous on linux and darwin and may be "not written at all if
// process.exit() is called before an asynchronous write completes", while a
// write to a file is synchronous; OpenCode 2.0.32 and 2.1.4 (Bun) were
// measured cutting a run's answer at the pipe's 64 KiB, and whole into a file
// every time. No runtime a harness ships on decides its buffering by pipe
// against file (only by "is it a terminal"), so a file loses nothing a pipe
// carried, and measured on every shipped harness the probe could run, a line
// reaches a file no later than it reaches a pipe.
//
// So each stream is a [capture]: a regular file, made 0600 with O_EXCL in a
// directory only the engine writes, opened a second time read-write for the
// engine, given [maxOutput] of room on its volume, PINNED ([flock.Pin]) on the
// description the child is handed, and UNLINKED before the child starts — so
// nothing can open it by name, no later call can find it, and a crash leaves
// no file behind. A child handed or printing a credential captures into an
// anonymous file in memory instead on linux (memfd_create), so a minted token
// or a server's password never reaches a disk ([childSpec.credential]).
//
// Two goroutines per child: a READER that follows the files and hands each
// complete line to whoever reads it live, and a WATCHER that measures them on
// its own tick and enforces their bound, which never waits on a line's
// callback, so a slow callback cannot let a runaway writer past the bound. The
// watcher runs until the end has stopped the reader, a cut or not: past the
// first cut it goes on giving back and keeping room, since a writer the kill
// did not reach writes on until the end.
//
// The END is three conditions: the harness exited, its process group has no
// living member, and no process still holds either file's write description
// (the pin): exactly when a pipe would have reached its end, and its group
// besides. Within the grace, as before; then each file is read to the size it
// had at that instant and never again, and a capture something still holds
// goes to the provider's [janitor].

// endPoll is how often the end is asked once the harness has exited and
// something of its group, or a holder of its output, remains.
//
// The figure and the reason are the sandbox's teardown poll, which asks the
// same question (internal/sandbox's exitPollInterval): the answer is usually
// yes at the first ask, and the caller is holding a turn open. Measured on
// every harness the probe ran (twenty text functions and twenty runs with
// tools each, half of them ended in flight), nothing of a group was alive at
// the harness's reap but for Claude Code on Node, whose relay lingered 1 to
// 22 ms: one or two asks. Once the leader is reaped, an ask is a signal-0
// probe of the group and, while the group exists, a scan of the pid
// namespace's stat records, so asking faster would buy a few milliseconds
// with a scan of every process each time.
const endPoll = 20 * time.Millisecond

// readChunk is the most the reader reads of one capture in one call before
// it splits the lines.
//
// At a writer's 1 GB/s the reader makes about a thousand reads a second,
// each under a millisecond, so it keeps up long before the [maxOutput] a
// writer may run ahead. Memory per stream stays at one chunk plus the line
// being assembled, which the splitter bounds at maxOutput.
const readChunk = 1 << 20

// streamKind is how a capture is bounded, decided by what its reader does
// with it.
type streamKind int

const (
	// oneAnswer is a stream read as one answer at the end: a text function's
	// stdout, an auxiliary's. It is kept whole up to [maxOutput], and past
	// it the watcher kills the child and the run is refused, the verdict a
	// clipped answer always got.
	oneAnswer streamKind = iota
	// readToEnd is a stream whose every line is delivered: a run with tools'
	// JSONL record, every stderr, a server's stdout. Its first
	// [streamSpec.keep] bytes are kept for the end, and what the reader has
	// read past [maxOutput] is given back to the volume (a hole punched),
	// so it may run on past the bound as a pipe's stream did. It is ended
	// only when it gets maxOutput ahead of its reader, or when its volume
	// refuses the room ahead.
	readToEnd
)

// lastLine is what becomes of a stream's unterminated last line once the
// child is done.
type lastLine int

const (
	// lastLineDropped leaves it unread, as the line tee a stderr went
	// through always did: it is in what the stream keeps all the same.
	lastLineDropped lastLine = iota
	// lastLineHanded hands it on as a line: the device sign-in's, whose last
	// words a bufio.Scanner always delivered.
	lastLineHanded
	// lastLineEvent is a JSONL stream's rule (a line stream's): one JSON
	// object is a whole last event written without its line break and is
	// handed on; bytes beginning with '{' that do not decode are a stream
	// cut inside its last event ([collected.cutLine]); anything else is a
	// trailer, skipped as the extractor skips a line that does not decode.
	lastLineEvent
)

// streamSpec is how one stream of a child is captured.
type streamSpec struct {
	kind streamKind
	// keep is how many of a [readToEnd] stream's first bytes are kept for
	// the end: [maxOutput] for a record and a stderr, which is what today's
	// capped buffers kept, and nothing for a server's stdout and the device
	// sign-in, which keep nothing. A [oneAnswer] stream keeps all of itself.
	keep int64
	// onLine receives each complete line as the reader reads it, from the
	// reader's goroutine, in order. Nil reads the stream only for its end.
	onLine func(string)
	// mirror receives every byte as the reader reads it, before the lines
	// are split: the capture-token command's UI, whose prompt ends without
	// a line break and has to reach the operator's terminal as it is.
	mirror io.Writer
	// last is the rule for the unterminated last line.
	last lastLine
}

// cutWhy is why the engine ended a run over its output.
type cutWhy string

const (
	// cutBound: a one-answer stream passed [maxOutput].
	cutBound cutWhy = "bound"
	// cutBehind: a stream read to its end ran more than maxOutput ahead of
	// the engine's reading.
	cutBehind cutWhy = "behind"
	// cutRoom: the volume would not keep the room ahead of a stream, or
	// would not give back what was read of it.
	cutRoom cutWhy = "room"
)

// outputCut is a run the engine ended over its output: the cause its call is
// cancelled with, and what [rawResult.cut] carries.
type outputCut struct {
	stream string
	why    cutWhy
	// size is the stream's size when the watcher measured it.
	size int64
	// dir and err are a refused room's: where, and what the volume said.
	dir string
	err error
}

func (c *outputCut) Error() string {
	switch c.why {
	case cutBehind:
		return fmt.Sprintf("its %s ran more than %d bytes ahead of the engine's reading of it "+
			"(%d written), so the engine ended it", c.stream, maxOutput, c.size)
	case cutRoom:
		return fmt.Sprintf("the volume under %s would not keep %d bytes of room ahead of its %s "+
			"(%v), so the engine ended it; free room on that volume or move the entry's "+
			"state_dir", c.dir, maxOutput, c.stream, c.err)
	default:
		return fmt.Sprintf("its %s passed the %d-byte bound on one answer (%d written), so the "+
			"engine ended it", c.stream, maxOutput, c.size)
	}
}

// captureHooks are the seams the suite drives the capture through: what the
// volume and the kernel answer, and the ticks of the reader, the watcher,
// the end and the janitor. Every nil field is the platform's own.
type captureHooks struct {
	// reserve gives f room for n bytes from off, the size kept.
	reserve func(f *os.File, off, n int64) error
	// punch gives back n bytes of f from off, the size kept.
	punch func(f *os.File, off, n int64) error
	// pinned is [flock.Pinned].
	pinned func(probe *os.File) (bool, error)
	// betweenOpens runs between a file capture's two opens, with its path.
	betweenOpens func(path string)
	// ticker is a ticker of period d and its stop.
	ticker func(d time.Duration) (<-chan time.Time, func())
	// deadline fires at the instant given.
	deadline func(at time.Time) <-chan time.Time
	// watched is told, after each of the watcher's measures, a stream's
	// size and its allocated bytes.
	watched func(stream string, size, allocated int64)
	// current answers, for the group a child leads, whether it is still the
	// one recorded: [procgroup.Leader.Current].
	current func(leader procgroup.Leader) (bool, error)
	// promptCut is told the instant os/exec cut a prompt's copy ([promptCopy]).
	promptCut func(at time.Time)
}

// platformHooks is every seam answered by the platform.
var platformHooks = &captureHooks{}

func (h *captureHooks) doReserve(f *os.File, off, n int64) error {
	if h != nil && h.reserve != nil {
		return h.reserve(f, off, n)
	}
	return reserveRoom(f, off, n)
}

func (h *captureHooks) doPunch(f *os.File, off, n int64) error {
	if h != nil && h.punch != nil {
		return h.punch(f, off, n)
	}
	return punchHole(f, off, n)
}

func (h *captureHooks) doPinned(probe *os.File) (bool, error) {
	if h != nil && h.pinned != nil {
		return h.pinned(probe)
	}
	return flock.Pinned(probe)
}

func (h *captureHooks) newTicker(d time.Duration) (<-chan time.Time, func()) {
	if h != nil && h.ticker != nil {
		return h.ticker(d)
	}
	t := time.NewTicker(d)
	return t.C, t.Stop
}

func (h *captureHooks) after(at time.Time) <-chan time.Time {
	if h != nil && h.deadline != nil {
		return h.deadline(at)
	}
	return time.After(time.Until(at))
}

// capture is one stream of one child: the engine's read-write description of
// the file the child writes, and where the reader and the watcher are in it.
type capture struct {
	stream string
	r      *os.File
	// memory is a capture in memory (a memfd), which has no room to keep.
	memory bool
	// dir is where a file capture was made, for an error that names it.
	dir   string
	spec  streamSpec
	hooks *captureHooks
	// block is the filesystem's block size, which a punch is aligned to.
	block int64

	// delivered is how far the reader has read and handed on: written by the
	// reader, read by the watcher.
	delivered atomic.Int64
	// split is the reader's.
	split lineSplitter

	// given and room are the watcher's: how far what was read has been given
	// back, and how far room is reserved.
	given, room int64
}

// newCapture makes one capture in dir for stream (2.2 of the design): a file
// made, opened twice, given its room, pinned and unlinked, or on linux, for a
// child handed or printing a credential, a file in memory. setting names
// what the operator sets to move dir, for an error. It returns the capture and
// the child's write description, which the caller hands to the child and then
// closes.
func newCapture(dir, setting, stream string, credential bool, spec streamSpec,
	hooks *captureHooks) (*capture, *os.File, error) {
	c := &capture{stream: stream, dir: dir, spec: spec, hooks: hooks, split: lineSplitter{on: spec.onLine}}
	var w *os.File
	if credential && memoryCaptures {
		r, mw, err := openMemoryCapture(stream)
		if err != nil {
			return nil, nil, fmt.Errorf("making its %s's capture in memory: %w", stream, err)
		}
		c.r, w, c.memory = r, mw, true
		c.block = blockSize(r)
	} else {
		r, fw, err := c.openFile(setting)
		if err != nil {
			return nil, nil, err
		}
		c.r, w = r, fw
	}
	if err := flock.Pin(w); err != nil {
		c.discard(w)
		return nil, nil, fmt.Errorf("pinning its %s's capture in %s: %w", stream, dir, err)
	}
	pinned, err := hooks.doPinned(c.r)
	switch {
	case err != nil:
		c.discard(w)
		return nil, nil, fmt.Errorf("asking whether its %s's capture in %s is pinned: %w", stream, dir, err)
	case !pinned:
		c.discard(w)
		return nil, nil, fmt.Errorf("the filesystem under %s keeps no lock per open file "+
			"description, so the engine could not see who still holds the %s it would hand "+
			"the harness; %s must be a local filesystem that does (ext4, xfs, btrfs, tmpfs, "+
			"APFS)", dir, stream, setting)
	}
	return c, w, nil
}

// openFile makes a file capture's file, its two descriptions and its room,
// and removes its name. The pin is the caller's, which comes before the name
// goes only in the sense that matters: nobody else can open the file by name
// in between, since its directory is the engine's alone and its name random.
func (c *capture) openFile(setting string) (r, w *os.File, err error) {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	path := filepath.Join(c.dir, ".crewlet-"+c.stream+"-"+hex.EncodeToString(raw[:]))
	w, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600) //nolint:gosec // a random name in the engine's own directory
	if err != nil {
		return nil, nil, fmt.Errorf("making its %s's capture in %s (under %s): %w", c.stream, c.dir, setting, err)
	}
	// THE NAME GOES ON EVERY WAY OUT: once both descriptions are open,
	// nothing needs it, and a capture refused below must leave nothing.
	defer func() { _ = os.Remove(path) }()
	if c.hooks != nil && c.hooks.betweenOpens != nil {
		c.hooks.betweenOpens(path)
	}
	r, err = reopenCapture(w, path)
	if err != nil {
		_ = w.Close()
		return nil, nil, fmt.Errorf("opening its %s's capture in %s for the engine: %w", c.stream, c.dir, err)
	}
	c.block = blockSize(r)
	if err := c.hooks.doReserve(r, 0, maxOutput); err != nil {
		_ = r.Close()
		_ = w.Close()
		return nil, nil, fmt.Errorf("the volume under %s would not reserve %d bytes of room for "+
			"its %s (%w); free room there, or set %s to a volume that has it", c.dir, maxOutput,
			c.stream, err, setting)
	}
	c.room = maxOutput
	return r, w, nil
}

// discard closes a capture refused before any child was given it.
func (c *capture) discard(w *os.File) {
	_ = w.Close()
	_ = c.r.Close()
}

// size is the capture's size now.
func (c *capture) size() (int64, error) {
	info, err := c.r.Stat()
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// pinned reports whether anything still holds the child's description.
func (c *capture) pinned() (bool, error) { return c.hooks.doPinned(c.r) }

// contents reads the capture from its start up to n bytes.
func (c *capture) contents(n int64) string {
	if n <= 0 {
		return ""
	}
	buf := make([]byte, n)
	got, _ := c.r.ReadAt(buf, 0)
	return string(buf[:got])
}

// hand delivers what the reader read: every byte to the mirror, and the
// complete lines to the line callback.
func (c *capture) hand(b []byte) {
	if c.spec.mirror != nil {
		_, _ = c.spec.mirror.Write(b)
	}
	if c.spec.onLine != nil || c.spec.last == lastLineEvent {
		c.split.write(b)
	}
}

// limit is how far the reader may read of a capture whose size is size: all of
// it, but no more than [maxOutput] of one answer.
func (c *capture) limit(size int64) int64 {
	if c.spec.kind == oneAnswer && size > maxOutput {
		return maxOutput
	}
	return size
}

// kept is how much of a capture of size bytes the end keeps.
func (c *capture) kept(size int64) int64 {
	keep := int64(maxOutput)
	if c.spec.kind == readToEnd {
		keep = c.spec.keep
	}
	return min(size, keep)
}

// check is the watcher's measure of one capture: the bound, how far the
// writer is ahead, what to give back and the room ahead. A run to end is the
// returned cut.
func (c *capture) check() *outputCut {
	size, err := c.size()
	if err != nil {
		// A measure the kernel would not give decides nothing: the next
		// tick measures again, and the end reads what is there.
		return nil //nolint:nilerr // see above
	}
	if c.hooks != nil && c.hooks.watched != nil {
		c.hooks.watched(c.stream, size, allocated(c.r))
	}
	if c.spec.kind == oneAnswer {
		if size <= maxOutput {
			return nil
		}
		// PAST THE BOUND NOTHING IS READ ([capture.limit]) and the run is
		// over, so what lands there is given back as it lands: a writer the
		// kill did not reach (one that escaped the group) holds no more of
		// the volume than a tick of its writes until the end. A punch the
		// volume refuses changes nothing: the bound has already ended the run.
		_ = c.giveBack(maxOutput, size)
		return &outputCut{stream: c.stream, why: cutBound, size: size}
	}
	delivered := c.delivered.Load()
	if size-delivered > maxOutput {
		return &outputCut{stream: c.stream, why: cutBehind, size: size}
	}
	// GIVEN BACK ONLY PAST maxOutput, so a filesystem that cannot punch
	// matters only to a stream that long; and only what was read, past what
	// is kept, so nothing the reader has not handed on and nothing the end
	// reads is lost.
	if size > maxOutput {
		if err := c.giveBack(c.spec.keep, delivered); err != nil {
			return &outputCut{stream: c.stream, why: cutRoom, size: size, dir: c.where(), err: err}
		}
	}
	if !c.memory {
		want := delivered + maxOutput
		if from := max(c.room, size); want > from {
			if err := c.hooks.doReserve(c.r, from, want-from); err != nil {
				return &outputCut{stream: c.stream, why: cutRoom, size: size, dir: c.where(), err: err}
			}
			c.room = want
		}
	}
	return nil
}

// giveBack punches out of the capture what lies from `from` to `to`, aligned
// inward to whole blocks, past what it gave back before.
func (c *capture) giveBack(from, to int64) error {
	start := roundUp(max(from, c.given), c.block)
	end := to / c.block * c.block
	if end <= start {
		return nil
	}
	if err := c.hooks.doPunch(c.r, start, end-start); err != nil {
		return err
	}
	c.given = end
	return nil
}

// where is the place a refused room names: the capture's directory, or memory.
func (c *capture) where() string {
	if c.memory {
		return "memory"
	}
	return c.dir
}

// empty gives back everything the capture holds: its blocks, its room and its
// size, through the engine's read-write description, while a holder may still
// append to it.
func (c *capture) empty() error { return c.r.Truncate(0) }

// sameFile refuses a capture whose two descriptions are not of one file: its
// path was replaced between the opens, which only a reopen by path (darwin's)
// can meet.
func sameFile(w, r *os.File) error {
	wi, err := w.Stat()
	if err != nil {
		return err
	}
	ri, err := r.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(wi, ri) {
		return errors.New("its path was replaced between its two opens, so the file the engine " +
			"would read is not the one the harness would write")
	}
	return nil
}

// roundUp is n rounded up to a multiple of block.
func roundUp(n, block int64) int64 { return (n + block - 1) / block * block }

// childSpec is one child the engine host starts for a harness: what
// [startChild] needs to build it, capture its output and end it.
type childSpec struct {
	binary harnessprofile.Binary
	args   []string
	dir    string
	env    []string
	// stdin is the child's stdin: a reader os/exec copies (through a
	// [promptCopy]), the operator's terminal, or nil for none.
	stdin io.Reader
	// outDir is where its file captures are made: the call's directory, or
	// the workspace's captures directory for a child no call owns.
	outDir string
	// setting names what an operator sets to move outDir, for an error.
	setting string
	// credential is a child handed or printing a credential: captured in
	// memory on linux, so none of it reaches a disk.
	credential bool
	// merged is one capture for both streams, in the order they were
	// written: the device sign-in.
	merged bool
	// foreground is a child with no group of its own, attached to the
	// operator's terminal: the capture-token command. Nothing but its own
	// process is ever signalled.
	foreground bool
	// out is stdout's capture (both streams' when merged), err stderr's.
	out, err streamSpec
	// grace is how long a signalled child has, and how long the end waits.
	grace time.Duration
	// cancel ends the call the child serves, with the cut that ended it; nil
	// for a child no call owns.
	cancel func(error)
	hooks  *captureHooks
	// janitor takes a capture still held at the end; nil is the package's
	// own.
	janitor *janitor
}

// orphanJanitor is the janitor of a child started with none of its own.
var orphanJanitor janitor

// childRun is one started child and its captures.
type childRun struct {
	// logCtx is what its log lines are told: the caller's, never its
	// cancellation, which often is what ended the child.
	logCtx     context.Context
	cmd        *exec.Cmd
	binary     harnessprofile.Binary
	group      harnessGroup
	foreground bool
	caps       []*capture
	hooks      *captureHooks
	janitor    *janitor
	cancel     func(error)
	// send signals the group at the end: procgroup.Kill.
	send func(pid int) error

	cut    atomic.Pointer[outputCut]
	shrunk atomic.Pointer[string]
	// prompt is its stdin's copy, where os/exec copies one.
	prompt *promptCopy

	// stop stops the reader, and watchStop the watcher, which the end stops
	// once the reader is done.
	stop      chan struct{}
	watchStop chan struct{}
	readDone  chan struct{}
	watchDone chan struct{}
	// exited carries what cmd.Wait reported, once.
	exited chan error
}

// startChild is THE ONE PLACE a harness child is built and started on the
// engine host, the operator's terminal commands aside
// ([Provider.runInCredentialHome]): its captures made, the command built with
// them as its stdout and stderr, its group set and recorded, and the reader
// and the watcher started. An error means no child ran.
func startChild(ctx context.Context, spec childSpec) (*childRun, error) {
	if spec.outDir == "" {
		return nil, errors.New("no directory to capture its output in was given")
	}
	hooks := spec.hooks
	if hooks == nil {
		hooks = platformHooks
	}
	var caps []*capture
	var ws []*os.File
	closeAll := func() {
		for _, w := range ws {
			_ = w.Close()
		}
		for _, c := range caps {
			_ = c.r.Close()
		}
	}
	streams := []struct {
		name string
		spec streamSpec
	}{{"stdout", spec.out}, {"stderr", spec.err}}
	if spec.merged {
		streams = streams[:1]
		streams[0].name = "output"
	}
	for _, s := range streams {
		c, w, err := newCapture(spec.outDir, spec.setting, s.name, spec.credential, s.spec, hooks)
		if err != nil {
			closeAll()
			return nil, err
		}
		caps, ws = append(caps, c), append(ws, w)
	}

	logCtx := context.WithoutCancel(ctx)
	cmd := exec.CommandContext(ctx, string(spec.binary), spec.args...) //nolint:gosec // binary and args come from a validated profile
	cmd.Dir = spec.dir
	cmd.Env = spec.env
	var prompt *promptCopy
	switch in := spec.stdin.(type) {
	case nil, *os.File:
		// None, or the operator's terminal, which os/exec hands on as it is.
		cmd.Stdin = spec.stdin
	default:
		prompt = &promptCopy{r: in, hooks: hooks}
		cmd.Stdin = prompt
	}
	// *os.File, so os/exec hands them to the child as they are and starts no
	// copier: there is no pipe anywhere between the child and its capture.
	cmd.Stdout, cmd.Stderr = ws[0], ws[len(ws)-1]
	// recorded is the group as recordGroup read it, once it has.
	var recorded atomic.Pointer[harnessGroup]
	if !spec.foreground {
		procgroup.Set(cmd)
		// Cancel signals the GROUP rather than the one process os/exec knows
		// about, CHECKED like every other signal to it ([harnessGroup.signal]):
		// os/exec runs it on its own goroutine when the call's context ends,
		// which can fall after Process.Wait has reaped the harness and before
		// Wait has returned. A signal the group did not get goes to the
		// leader through os/exec's handle, which never reaches another
		// process; so does one that comes before the group is recorded, a
		// window of one system call in which nothing has waited on the
		// harness.
		cmd.Cancel = func() error {
			if g := recorded.Load(); g != nil && g.signal(logCtx, "its call ended", "SIGTERM", procgroup.Terminate) {
				return nil
			}
			return cmd.Process.Signal(syscall.SIGTERM)
		}
	}
	// WaitDelay bounds what is left of os/exec's own: a leader that ignores
	// SIGTERM once the call ended, and its stdin copier, held by a helper
	// with the prompt unread, whose cut the end counts its grace from
	// ([childRun.letGoBy]). It bounds no output: there is no copier for it.
	cmd.WaitDelay = spec.grace
	startErr := cmd.Start()
	// THE ENGINE'S COPIES GO NOW: the child has its own, and the pin counts
	// the description's holders, of which these are no longer any.
	for _, w := range ws {
		_ = w.Close()
	}
	ws = nil
	if startErr != nil {
		closeAll()
		return nil, startErr
	}
	c := &childRun{
		logCtx: logCtx, cmd: cmd, binary: spec.binary, foreground: spec.foreground,
		caps: caps, hooks: hooks, janitor: spec.janitor, cancel: spec.cancel, send: procgroup.Kill,
		prompt: prompt, stop: make(chan struct{}), watchStop: make(chan struct{}),
		readDone: make(chan struct{}), watchDone: make(chan struct{}), exited: make(chan error, 1),
	}
	if c.janitor == nil {
		c.janitor = &orphanJanitor
	}
	if !spec.foreground {
		group, err := recordGroup(cmd)
		if err != nil {
			// Killed and reaped already: no harness ran that anything
			// could later signal safely.
			closeAll()
			return nil, fmt.Errorf("it started, but its process identity could not be read, so "+
				"it was ended at once (no later signal to its group could be checked): %w", err)
		}
		if hooks.current != nil {
			leader := group.leader
			group.current = func() (bool, error) { return hooks.current(leader) }
		}
		c.group = group
		recorded.Store(&group)
	}
	go c.read()
	go c.watch()
	go func() { c.exited <- cmd.Wait() }()
	return c, nil
}

// read is the reader: it follows every capture from where it is, in
// [readChunk] pieces, handing on what it reads, until it has caught up, and
// then waits a [tailPoll]. A one-answer capture is read no further than
// [maxOutput]: past it the run is over.
func (c *childRun) read() {
	defer close(c.readDone)
	tick, stopTick := c.hooks.newTicker(tailPoll)
	defer stopTick()
	buf := make([]byte, readChunk)
	for {
		progressed := false
		for _, cp := range c.caps {
			if c.readSome(cp, buf, -1) {
				progressed = true
			}
		}
		if progressed {
			select {
			case <-c.stop:
				return
			default:
				continue
			}
		}
		select {
		case <-c.stop:
			return
		case <-tick:
		}
	}
}

// readSome reads one chunk of cp past what it delivered, up to upTo where
// that is not negative and to its size otherwise, and reports whether it read
// anything.
func (c *childRun) readSome(cp *capture, buf []byte, upTo int64) bool {
	size, err := cp.size()
	if err != nil {
		return false
	}
	off := cp.delivered.Load()
	if size < off {
		// A FILE THAT SHRANK UNDER THE READER: something truncated what the
		// harness wrote, and nothing read of it can be shown to be whole.
		stream := cp.stream
		c.shrunk.CompareAndSwap(nil, &stream)
		return false
	}
	end := cp.limit(size)
	if upTo >= 0 {
		end = min(end, cp.limit(upTo))
	}
	if end <= off {
		return false
	}
	n := min(end-off, int64(len(buf)))
	got, _ := cp.r.ReadAt(buf[:n], off)
	if got <= 0 {
		return false
	}
	cp.hand(buf[:got])
	cp.delivered.Store(off + int64(got))
	return true
}

// watch is the watcher: every [tailPoll] it measures each capture and
// enforces its bound ([capture.check]), and nothing it does waits on a line's
// callback or a lock one holds.
//
// IT RUNS UNTIL THE END STOPS IT, after the reader, and not only until a cut:
// the first cut ends the run ([childRun.endRun] acts once), but a writer the
// kill does not reach, one that escaped the group, writes on until the end,
// and every tick still gives back what was read, or what lies past a one
// answer's bound, and keeps the room ahead. So such a writer holds no more of
// the volume than a tick of its writes past what the bound allows, before the
// end as after it, where the janitor takes over.
func (c *childRun) watch() {
	defer close(c.watchDone)
	tick, stopTick := c.hooks.newTicker(tailPoll)
	defer stopTick()
	for {
		select {
		case <-c.watchStop:
			return
		case <-tick:
		}
		for _, cp := range c.caps {
			if cut := cp.check(); cut != nil {
				c.endRun(cut)
			}
		}
	}
}

// endRun ends the run over its output: the child killed FIRST, its group or
// (a foreground child) its process alone — a grace would let a runaway writer
// write the grace times its rate more — and then the call cancelled with the
// cut.
func (c *childRun) endRun(cut *outputCut) {
	if !c.cut.CompareAndSwap(nil, cut) {
		return
	}
	reason := "its " + cut.stream + " passed its bound"
	if c.foreground {
		_ = c.cmd.Process.Kill()
	} else {
		c.group.signal(c.logCtx, reason, "SIGKILL", c.send)
	}
	switch cut.why {
	case cutBehind:
		log.WarnContext(c.logCtx, "harness_output_behind", "binary", c.binary, "stream", cut.stream,
			"bytes_ahead", cut.size-c.capOf(cut.stream).delivered.Load())
	case cutRoom:
		log.WarnContext(c.logCtx, "harness_output_room", "binary", c.binary, "stream", cut.stream,
			"path", cut.dir, "error", cut.err)
	default:
		log.WarnContext(c.logCtx, "harness_output_bounded", "binary", c.binary, "stream", cut.stream,
			"limit_bytes", maxOutput, "size_bytes", cut.size)
	}
	if c.cancel != nil {
		c.cancel(cut)
	}
}

// letGoBy is when the end lets go of a child whose grace was counted to
// counted: never later than the instant os/exec cut its prompt's copy, held
// unread. os/exec cuts it exactly a grace after the harness exited or its
// call ended, and only then does Wait return, so a grace counted from Wait's
// return would be a second one.
func (c *childRun) letGoBy(counted time.Time) time.Time {
	if c.prompt == nil {
		return counted
	}
	if at := c.prompt.cut.Load(); at != nil && at.Before(counted) {
		return *at
	}
	return counted
}

// promptCopy is a child's stdin as os/exec copies it, which shows the engine
// how the copy ended.
//
// os/exec copies a stdin that is not a file into a pipe with io.Copy, which
// hands the copy to a source's WriteTo: so this copies the prompt itself,
// into the pipe's write end. When a helper holds the pipe with the prompt
// unread, the copy blocks until os/exec, its WaitDelay out, closes that end
// under it, and the write fails with os.ErrClosed: the instant it does is
// [promptCopy.cut]. A harness that exited without reading the rest, with
// nothing else holding the pipe, fails the write with EPIPE instead, which
// os/exec ignores, as before.
type promptCopy struct {
	r     io.Reader
	hooks *captureHooks
	// cut is the instant os/exec closed the pipe under the copy.
	cut atomic.Pointer[time.Time]
}

func (p *promptCopy) Read(b []byte) (int, error) { return p.r.Read(b) }

// WriteTo copies the prompt into w, the pipe os/exec made.
func (p *promptCopy) WriteTo(w io.Writer) (int64, error) {
	n, err := io.Copy(w, p.r)
	if errors.Is(err, os.ErrClosed) {
		at := time.Now()
		p.cut.Store(&at)
		if p.hooks != nil && p.hooks.promptCut != nil {
			p.hooks.promptCut(at)
		}
	}
	return n, err
}

// capOf is the capture of stream.
func (c *childRun) capOf(stream string) *capture {
	for _, cp := range c.caps {
		if cp.stream == stream {
			return cp
		}
	}
	return c.caps[0]
}

// signal sends a signal to the child's group, checked against the group
// recorded at its start ([harnessGroup.signal]), and reports whether it was
// sent; a foreground child has no group, and nothing is sent.
func (c *childRun) signal(reason, signal string, send func(int) error) bool {
	if c.foreground {
		return false
	}
	return c.group.signal(c.logCtx, reason, signal, send)
}

// groupLives answers the end's first question: does the child's group have a
// living member? An error answers yes: the process table could not be read,
// and a group nobody can see is not a group seen to be gone. A foreground
// child has no group to ask.
func (c *childRun) groupLives() bool {
	if c.foreground {
		return false
	}
	live, err := c.group.current()
	return err != nil || live
}

// holders answers the end's second: does each capture's write description
// still have a holder? An error answers yes, for the same reason.
func (c *childRun) holders() []bool {
	held := make([]bool, len(c.caps))
	for i, cp := range c.caps {
		pinned, err := cp.pinned()
		held[i] = err != nil || pinned
	}
	return held
}

// ending is the end's verdict: each capture's size at the instant of the end,
// whether each was still held then, and whether a holder sat outside the
// group.
type ending struct {
	sizes   []int64
	held    []bool
	escaped bool
}

// end waits for the end once the harness has exited (3.1 of the design): no
// living member of its group and no holder of either capture's description,
// asked at once and then every [endPoll] until letGoBy. At letGoBy a capture
// still held is held, and what lives of the group is killed — AFTER the held
// streams are decided and the sizes measured, so a stream that reached its
// end only because of the kill counts as cut. The reader and the watcher are
// stopped before the measure, the watcher last, so it bounds the captures for
// as long as the reader may still be in a line's callback: nothing is read
// past the measure.
func (c *childRun) end(letGoBy time.Time) ending {
	deadline := c.hooks.after(letGoBy)
	tick, stopTick := c.hooks.newTicker(endPoll)
	defer stopTick()
	live, held := c.groupLives(), c.holders()
wait:
	for live || slices.Contains(held, true) {
		select {
		case <-deadline:
			live, held = c.groupLives(), c.holders()
			break wait
		case <-tick:
			live, held = c.groupLives(), c.holders()
		}
	}
	close(c.stop)
	<-c.readDone
	close(c.watchStop)
	<-c.watchDone
	e := ending{held: held, sizes: make([]int64, len(c.caps))}
	for i, cp := range c.caps {
		e.sizes[i], _ = cp.size()
	}
	if live {
		c.signal("it outlived the harness past the grace", "SIGKILL", c.send)
	}
	// A HOLDER OUTSIDE THE GROUP: the group is gone, or never had a member
	// left, and the description is still held. procgroup addresses groups,
	// and this process left the one recorded, so it is not signalled.
	e.escaped = !live && slices.Contains(held, true)
	return e
}

// collected is what the end read of a child's captures.
type collected struct {
	// out and errOut are what each stream keeps: stdout and stderr, or the
	// merged capture's in out.
	out, errOut string
	// cutLine is a line stream whose stdout ended inside an event.
	cutLine bool
}

// collect reads each capture up to the size the end measured, and never past
// it: the reader's last read, the rule for the last line, and what each
// stream keeps, from its start.
func (c *childRun) collect(e ending) collected {
	var got collected
	buf := make([]byte, readChunk)
	for i, cp := range c.caps {
		for c.readSome(cp, buf, e.sizes[i]) {
		}
		if cp.spec.kind == oneAnswer && e.sizes[i] > maxOutput {
			// Past the bound within the watcher's last tick: the verdict is
			// the bound's all the same.
			c.cut.CompareAndSwap(nil, &outputCut{stream: cp.stream, why: cutBound, size: e.sizes[i]})
		}
		if c.lastLine(cp) {
			got.cutLine = true
		}
		kept := cp.contents(cp.kept(e.sizes[i]))
		if i == 0 {
			got.out = kept
		} else {
			got.errOut = kept
		}
	}
	return got
}

// lastLine applies a capture's rule to its unterminated last line, and
// reports a line stream cut inside its last event.
func (c *childRun) lastLine(cp *capture) bool {
	rest := cp.split.rest()
	if len(rest) == 0 || cp.spec.onLine == nil && cp.spec.last != lastLineEvent {
		return false
	}
	switch cp.spec.last {
	case lastLineDropped:
		return false
	case lastLineHanded:
		cp.spec.onLine(string(rest))
	case lastLineEvent:
		line := bytes.TrimSpace(rest)
		if _, ok := decodeObject(string(line)); ok {
			if cp.spec.onLine != nil {
				cp.spec.onLine(string(rest))
			}
			return false
		}
		return len(line) > 0 && line[0] == '{'
	}
	return false
}

// close lets go of the captures: one nothing holds is closed, and one still
// held goes to the janitor, which empties it until its holder lets go.
func (c *childRun) close() {
	for _, cp := range c.caps {
		pinned, err := cp.pinned()
		if err == nil && !pinned {
			_ = cp.r.Close()
			continue
		}
		c.janitor.hold(c.logCtx, cp)
	}
}

// lineSplitter hands each complete line written to it to on.
//
// ONE LINE IS BOUNDED at [maxOutput]: a line longer than that is skipped to its
// end rather than held, and the lines after it still arrive. The capture is
// the buffer; this holds only the line being assembled.
type lineSplitter struct {
	on  func(string)
	buf []byte
	// skipping is a line past the bound, dropped up to its newline.
	skipping bool
}

func (s *lineSplitter) write(b []byte) {
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			if !s.skipping {
				s.buf = append(s.buf, b...)
			}
			if len(s.buf) > maxOutput {
				s.buf, s.skipping = nil, true
			}
			return
		}
		if !s.skipping && s.on != nil {
			s.on(string(append(s.buf, b[:i]...)))
		}
		s.buf, s.skipping = s.buf[:0], false
		b = b[i+1:]
	}
}

// rest is the unterminated line written last, or nothing.
func (s *lineSplitter) rest() []byte {
	if s.skipping {
		return nil
	}
	return s.buf
}
