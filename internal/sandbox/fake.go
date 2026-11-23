package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/browser/hostreach"
	"github.com/crewlet/crewlet/internal/queue"
)

// The in-process test doubles live in the package, not a _test.go file: the
// coordinator, the waiter, the tool and the engine all drive a sandbox, and a
// double per consumer would model the state machine four ways, each agreeing
// with the code beside it. They model only the SHAPE the real backends share (a
// box with a filesystem, a background job that finishes when a test says so, a
// provider that can lose a box); where a real backend's behaviour is
// load-bearing (process groups, the pause reaper, path escapes) its test runs
// against the real thing (local_test.go).

// FakeSandbox is an in-memory box.
type FakeSandbox struct {
	mu    sync.Mutex
	id    string
	home  string
	files map[string][]byte
	// timeouts is every refresh of the box's timer, with when it came: see
	// [FakeSandbox.Timeouts].
	timeouts []FakeTimeout
	paused   bool
	closed   bool
	// reclaimed is a box its provider killed ([FakeProvider.Kill]): unreadable.
	reclaimed  bool
	commands   []string
	background []string
	// execs is every command the box was asked to run, foreground and
	// detached, with the environment it was given: see [FakeSandbox.Execs].
	execs []FakeExec
	// readCap is the whole-read cap a suite gave the box, zero for
	// [MaxFileBytes]: see [FakeSandbox.CapReads].
	readCap int
	// delivered is how many bytes its reads have handed out: see
	// [FakeSandbox.Delivered].
	delivered int64

	// ExecFunc, when set, answers Exec instead of the default success.
	ExecFunc func(ctx context.Context, cmd string, opts ExecOptions) (ExecResult, error)

	// EndJobFunc, when set, answers EndJob instead of the default success,
	// and KillJobFunc KillJob: a box whose job's script was not there to ask,
	// one that could not be reached, a kill that ended the job. Every call is
	// recorded either way ([FakeSandbox.JobCalls]).
	EndJobFunc  func(ctx context.Context, commandID string, script int) error
	KillJobFunc func(ctx context.Context, commandID string) error
	jobCalls    []string

	// PauseFunc, when set, is asked first by every Pause, and a non-nil
	// answer is that pause's error, leaving the box unpaused: a backend slow
	// to snapshot the box, or one that could not.
	PauseFunc func(ctx context.Context) error

	// ReadErr, when set, is consulted by every read with the file's path,
	// and a non-nil answer is that read's error — standing in for a box
	// whose file could not be read back (a transport failure, not a size).
	ReadErr func(path string) error

	// Streams, when set, wraps every stream the box opens (OpenFile,
	// OpenTail, OpenRange) with the file's path, and its reader is what the
	// caller reads: a stream that fails after k bytes, or one slowed inside a
	// synctest bubble. [FakeSandbox.Delivered] counts what it hands out.
	Streams func(path string, r io.Reader) io.Reader

	// Kernel is the kernel the double's runtime runs on, which
	// [FakeSandbox.EngineKernel] answers: empty, [FakeKernel].
	Kernel hostreach.Kernel

	// ReachFunc, when set, answers the launch's check of the box's network
	// ([hostreach.Command]) instead of a clean answer on the double's kernel:
	// a box that dials the engine's port, prints another kernel or nothing.
	ReachFunc func(ctx context.Context, opts ExecOptions) (ExecResult, error)
}

// FakeKernel is the kernel a [FakeSandbox]'s runtime runs on unless its
// Kernel names another: a boot id the double's box prints as its own.
const FakeKernel hostreach.Kernel = "7d4e1c2a-9b3f-4e6d-a5c8-1f2e3d4c5b6a"

var _ Sandbox = (*FakeSandbox)(nil)

// NewFakeSandbox mints a box with the given id. It reads files whole up to
// [MaxFileBytes], as every real box does.
func NewFakeSandbox(id string) *FakeSandbox {
	return &FakeSandbox{id: id, home: DefaultHome, files: map[string][]byte{}}
}

// CapReads holds the box's whole reads to n bytes rather than [MaxFileBytes],
// and returns the box.
//
// FOR A SUITE THAT HAS TO GO PAST THE CAP: at 32 MiB each over-cap stream,
// report or question takes seconds to build, copy and redact under -race, and
// every branch is the same at any cap, so the suite runs at a cap it can fill,
// sizes what it writes from [FakeSandbox.ReadCap], and the file contract holds
// real backends to the real figure. n must be positive: a zero cap refuses
// every file, and no case means that.
func (s *FakeSandbox) CapReads(n int) *FakeSandbox {
	if n <= 0 {
		panic(fmt.Sprintf("sandbox: FakeSandbox.CapReads(%d): a cap must be at least one byte", n))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readCap = n
	return s
}

// ReadCap is the most the box reads whole: [MaxFileBytes], or what
// [FakeSandbox.CapReads] set.
func (s *FakeSandbox) ReadCap() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return readLimit(s.readCap)
}

// ID is the box's identifier.
func (s *FakeSandbox) ID() string { return s.id }

// Home is the box's root directory: a fixed path nothing on disk backs.
func (s *FakeSandbox) Home() string { return s.home }

// EngineKernel answers as a container box does ([containerBox.EngineKernel]):
// its Kernel, else [FakeKernel].
func (s *FakeSandbox) EngineKernel() (hostreach.Kernel, error) {
	if s.Kernel != "" {
		return s.Kernel, nil
	}
	return FakeKernel, nil
}

// FakeExec is one command a [FakeSandbox] was asked to run, and the
// environment and directory it was given.
type FakeExec struct {
	Command    string
	Env        map[string]string
	Cwd        string
	Background bool
}

// Exec records the command and returns whatever the test queued for it,
// or an empty success. The launch's check of the box's network is answered
// first, by ReachFunc, else as a box off its host's loopback on the double's
// kernel answers it, so the double's boxes ready a browser as a bridged box
// on the engine's kernel does.
func (s *FakeSandbox) Exec(ctx context.Context, cmd string, opts ExecOptions) (ExecResult, error) {
	s.mu.Lock()
	s.execs = append(s.execs, FakeExec{Command: cmd, Env: maps.Clone(opts.Env), Cwd: opts.Cwd})
	s.mu.Unlock()
	if cmd == hostreach.Command() {
		if s.ReachFunc != nil {
			return s.ReachFunc(ctx, opts)
		}
		kernel, _ := s.EngineKernel()
		return ExecResult{Stdout: hostreach.Answer(kernel)}, nil
	}
	if s.ExecFunc != nil {
		return s.ExecFunc(ctx, cmd, opts)
	}
	s.mu.Lock()
	s.commands = append(s.commands, cmd)
	s.mu.Unlock()
	return ExecResult{}, nil
}

// StartBackground records the command and hands back a handle whose job
// finishes when a test calls [FakeRunner.Finish].
func (s *FakeSandbox) StartBackground(ctx context.Context, cmd string, opts ExecOptions) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.background = append(s.background, cmd)
	s.execs = append(s.execs, FakeExec{Command: cmd, Env: maps.Clone(opts.Env), Cwd: opts.Cwd, Background: true})
	return strconv.Itoa(len(s.background)), nil
}

// Execs is every command the box was asked to run — foreground, a scripted
// [FakeSandbox.ExecFunc]'s included, and detached — in order, each with the
// environment and directory it was given: the only record of a run's
// environment reaching a fake box.
func (s *FakeSandbox) Execs() []FakeExec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.execs)
}

// Job answers through [FakeSandbox.Exec], the way a remote box does, so a
// test scripts liveness with ExecFunc ([remoteJob]).
func (s *FakeSandbox) Job(ctx context.Context, commandID string) (JobState, error) {
	return remoteJob(ctx, s, commandID)
}

// EndJob records the end request — "end <commandID> <script>" — and answers
// EndJobFunc's verdict, or success. A pid below 2 is refused unsent, as every
// real box refuses one.
func (s *FakeSandbox) EndJob(ctx context.Context, commandID string, script int) error {
	if script < 2 {
		return fmt.Errorf("%w: pid %d is no process an end request is sent to", ErrNotJobProcess, script)
	}
	s.mu.Lock()
	s.jobCalls = append(s.jobCalls, fmt.Sprintf("end %s %d", commandID, script))
	end := s.EndJobFunc
	s.mu.Unlock()
	if end != nil {
		return end(ctx, commandID, script)
	}
	return nil
}

// KillJob records the kill — "kill <commandID>" — and answers KillJobFunc's
// verdict, or success.
func (s *FakeSandbox) KillJob(ctx context.Context, commandID string) error {
	s.mu.Lock()
	s.jobCalls = append(s.jobCalls, "kill "+commandID)
	kill := s.KillJobFunc
	s.mu.Unlock()
	if kill != nil {
		return kill(ctx, commandID)
	}
	return nil
}

// JobCalls is every end request and kill made of the box's job, in order.
func (s *FakeSandbox) JobCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.jobCalls)
}

// WriteFile stores the content in memory, where [FakeSandbox.Put] and
// ReadFile can see it — replaced whole under the box's lock, so a read never
// finds a write half done, as no real backend's does ([Sandbox.WriteFile]).
func (s *FakeSandbox) WriteFile(ctx context.Context, p string, content []byte) error {
	return s.WriteStream(ctx, p, bytes.NewReader(content))
}

// WriteStream stores what content yields, read whole BEFORE the box's lock is
// taken (a source is the caller's and may be slow), so a source that fails part
// way leaves the file as it was, as every real backend's staged write does
// ([Sandbox.WriteStream]). An empty source stores nil, read as an empty file.
func (s *FakeSandbox) WriteStream(ctx context.Context, p string, content io.Reader) error {
	got, err := io.ReadAll(readUnder(ctx, content))
	if err != nil {
		return fmt.Errorf("sandbox: write %s: %w", p, err)
	}
	if len(got) == 0 {
		got = nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("sandbox: box is closed")
	}
	s.files[path.Clean(p)] = got
	return nil
}

// ReadFile is empty-on-missing, matching every real backend: the runner polls
// for markers that do not exist until the job finishes.
//
// It REFUSES A FILE PAST ITS CAP — [MaxFileBytes] unless a suite set another
// ([FakeSandbox.CapReads]) — by the real backends' own rule ([readCapped]), so
// a test through this twin certifies no read a real box would refuse.
func (s *FakeSandbox) ReadFile(ctx context.Context, p string) ([]byte, error) {
	content, err := s.content(p)
	if err != nil || content == nil {
		return nil, err
	}
	return readCapped(s.delivering(bytes.NewReader(content)), p, s.ReadCap())
}

// OpenWhole opens the file as it stands now, refused past the box's whole-read
// cap as [FakeSandbox.ReadFile] refuses it.
func (s *FakeSandbox) OpenWhole(ctx context.Context, p string) (io.ReadCloser, int64, error) {
	content, err := s.content(p)
	if err != nil {
		return nil, 0, err
	}
	size := int64(len(content))
	if limit := s.ReadCap(); size > int64(limit) {
		return nil, 0, &FileTooLargeError{Path: p, Limit: limit, Size: size}
	}
	return io.NopCloser(s.streaming(p, content)), size, nil
}

// OpenFile streams the file whole, as every real backend does: a machine
// stream is never refused for its size.
func (s *FakeSandbox) OpenFile(ctx context.Context, p string) (io.ReadCloser, error) {
	content, err := s.content(p)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(s.streaming(p, content)), nil
}

// ReadTail answers the file's last n bytes and its whole size.
func (s *FakeSandbox) ReadTail(ctx context.Context, p string, n int) (FileTail, error) {
	content, err := s.content(p)
	if err != nil {
		return FileTail{}, err
	}
	tail := tailOf(content, n)
	s.mu.Lock()
	s.delivered += int64(len(tail.Data))
	s.mu.Unlock()
	return tail, nil
}

// OpenTail streams the file's last n bytes, with its whole size.
func (s *FakeSandbox) OpenTail(ctx context.Context, p string, n int64) (io.ReadCloser, int64, error) {
	content, err := s.content(p)
	if err != nil {
		return nil, 0, err
	}
	size := int64(len(content))
	start := max(size-max(n, 0), 0)
	return io.NopCloser(s.streaming(p, content[start:])), size, nil
}

// OpenRange streams at most n bytes of the file from off, with its whole size:
// nothing, with the size, for an offset at or past the end.
func (s *FakeSandbox) OpenRange(ctx context.Context, p string, off, n int64) (io.ReadCloser, int64, error) {
	if err := rangeRefused(p, off, n); err != nil {
		return nil, 0, err
	}
	content, err := s.content(p)
	if err != nil {
		return nil, 0, err
	}
	size := int64(len(content))
	from := min(off, size)
	return io.NopCloser(s.streaming(p, content[from:from+min(n, size-from)])), size, nil
}

// Delivered is how many bytes of its files the box's reads have handed out —
// each stream as far as its reader took it, a refused whole read as far as the
// refusal read: what a remote box would have sent over the wire, a reader's
// cost.
func (s *FakeSandbox) Delivered() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.delivered
}

// streaming is a stream of content, a file of the box at p: through
// [FakeSandbox.Streams] when a test set it, counted as it is read.
func (s *FakeSandbox) streaming(p string, content []byte) io.Reader {
	s.mu.Lock()
	wrap := s.Streams
	s.mu.Unlock()
	var r io.Reader = bytes.NewReader(content)
	if wrap != nil {
		r = wrap(path.Clean(p), r)
	}
	return s.delivering(r)
}

// delivering is r counting what it hands out into [FakeSandbox.Delivered].
func (s *FakeSandbox) delivering(r io.Reader) io.Reader { return deliveredReader{r: r, box: s} }

// deliveredReader is a read of the box's file, counted as it is read.
type deliveredReader struct {
	r   io.Reader
	box *FakeSandbox
}

func (d deliveredReader) Read(p []byte) (int, error) {
	n, err := d.r.Read(p)
	d.box.mu.Lock()
	d.box.delivered += int64(n)
	d.box.mu.Unlock()
	return n, err
}

// content is one file's bytes, or the error a test scripted for reading it.
func (s *FakeSandbox) content(p string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ReadErr != nil {
		if err := s.ReadErr(path.Clean(p)); err != nil {
			return nil, err
		}
	}
	return slices.Clone(s.files[path.Clean(p)]), nil
}

// SetTimeout records the refresh and when it came, and ends nothing: a case
// about a box its timer ends models the end from [FakeSandbox.Timeouts].
func (s *FakeSandbox) SetTimeout(ctx context.Context, seconds float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.timeouts = append(s.timeouts, FakeTimeout{At: time.Now(), Seconds: seconds})
	return nil
}

// FakeTimeout is one refresh of a [FakeSandbox]'s timer: when it was asked for,
// on the clock the case runs under, and the seconds it set the timer to.
type FakeTimeout struct {
	At      time.Time
	Seconds float64
}

// Timeouts is every refresh of the box's timer, in order — the waiter's
// keepalive and a launch's while it provisions the box alike.
func (s *FakeSandbox) Timeouts() []FakeTimeout {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.timeouts)
}

// Pause marks the box snapshotted, once [FakeSandbox.PauseFunc] answers no
// error where one is set; [FakeSandbox.Paused] reports it.
func (s *FakeSandbox) Pause(ctx context.Context) error {
	if s.PauseFunc != nil {
		if err := s.PauseFunc(ctx); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paused = true
	return nil
}

// Close marks the box torn down; [FakeSandbox.Closed] reports it.
func (s *FakeSandbox) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// Keepalives is how many times this box's TTL was refreshed: by the waiter's
// keepalive, and by a launch while it provisioned the box.
func (s *FakeSandbox) Keepalives() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.timeouts)
}

// unreadable reports whether the box can no longer be read: paused, or
// reclaimed by its provider.
func (s *FakeSandbox) unreadable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paused || s.reclaimed
}

// Paused reports whether the box is currently snapshotted.
func (s *FakeSandbox) Paused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paused
}

// Closed reports whether the box was torn down.
func (s *FakeSandbox) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Commands is every foreground command run in the box, in order.
func (s *FakeSandbox) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.commands)
}

// Background is every command started detached, in order.
func (s *FakeSandbox) Background() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.background)
}

// Files is every path the box holds a file at, sorted: what a case lists of
// a writer that names its files afresh each time.
func (s *FakeSandbox) Files() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Sorted(maps.Keys(s.files))
}

// Put seeds a file, standing in for something the job wrote.
func (s *FakeSandbox) Put(p, content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[path.Clean(p)] = []byte(content)
}

// FakeProvider mints [FakeSandbox]es and remembers them, so Connect returns
// the box a Create handed out and a reconnect across a simulated restart is
// testable.
type FakeProvider struct {
	mu    sync.Mutex
	boxes map[string]*FakeSandbox
	next  int

	// CreateErr, when set, fails every Create.
	CreateErr error
	// Vanished are box ids Connect refuses, standing in for a box the
	// provider reclaimed under the engine.
	Vanished map[string]bool
	// Killed records every Kill, in order — the pause reaper's assertion.
	Killed []string
	// KillErr, when set, fails every Kill after recording it, standing in
	// for a provider that could not be reached to reclaim a box.
	KillErr error
	// AttachFunc, when set, is called by every Attach before it answers,
	// WITHOUT the provider's lock, and a non-nil answer fails the Attach: a
	// control plane slow or silent for one box while it answers the rest.
	AttachFunc func(ctx context.Context, sandboxID string) error
	// KillFunc, when set, is called by every Kill on a live context before
	// it answers, WITHOUT the provider's lock, and a non-nil answer fails
	// the Kill unrecorded: a control plane slow or silent on a reclaim.
	KillFunc func(ctx context.Context, sandboxID string) error
	// Made, when set, is handed every box Create makes, before Create
	// answers: a case scripts a fresh box's commands there.
	Made func(box *FakeSandbox)

	// runs is the run each box was made for ([HostBox]), kept only for a box
	// made for a turn in a cell on the engine's host: the one kind a local
	// backend lists.
	runs map[string]HostBox
}

var _ Provider = (*FakeProvider)(nil)

// NewFakeProvider returns an empty provider.
func NewFakeProvider() *FakeProvider {
	return &FakeProvider{boxes: map[string]*FakeSandbox{}, Vanished: map[string]bool{}}
}

// Kind names the backend, for the messages a real provider's errors carry.
func (p *FakeProvider) Kind() string { return "fake" }

// Create mints a box and remembers it, so a later Connect hands back the
// same one.
func (p *FakeProvider) Create(ctx context.Context, spec Spec) (Sandbox, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.CreateErr != nil {
		return nil, p.CreateErr
	}
	p.next++
	box := NewFakeSandbox(fmt.Sprintf("box-%d", p.next))
	p.boxes[box.id] = box
	if p.Made != nil {
		p.Made(box)
	}
	if spec.TurnID != "" && spec.Placement.OnEngineHost() {
		if p.runs == nil {
			p.runs = map[string]HostBox{}
		}
		p.runs[box.id] = HostBox{SandboxID: box.id, TurnID: spec.TurnID, Placement: spec.Placement}
	}
	return box, nil
}

// RunBoxes lists the boxes made for a turn in a cell on the engine's host
// that have not been killed, as [Local.RunBoxes] does.
func (p *FakeProvider) RunBoxes() ([]HostBox, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []HostBox
	for _, id := range slices.Sorted(maps.Keys(p.runs)) {
		if _, live := p.boxes[id]; live {
			out = append(out, p.runs[id])
		}
	}
	return out, nil
}

// Connect returns a previously created box, resumed.
func (p *FakeProvider) Connect(ctx context.Context, sandboxID string) (Sandbox, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Vanished[sandboxID] {
		return nil, boxGone{fmt.Errorf("sandbox %q is gone", sandboxID)}
	}
	box, ok := p.boxes[sandboxID]
	if !ok {
		return nil, boxGone{fmt.Errorf("sandbox %q is gone", sandboxID)}
	}
	// Every real backend's Connect resumes the box.
	box.mu.Lock()
	box.paused = false
	box.mu.Unlock()
	return box, nil
}

// Attach returns a previously created box WITHOUT resuming it, and refuses a
// paused one with [ErrBoxPaused], as the remote backend does: the stricter of
// the two real answers, so a reader certified here copes with a box it may not
// read.
func (p *FakeProvider) Attach(ctx context.Context, sandboxID string) (Sandbox, error) {
	p.mu.Lock()
	attach := p.AttachFunc
	p.mu.Unlock()
	if attach != nil {
		if err := attach(ctx, sandboxID); err != nil {
			return nil, err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	box, ok := p.boxes[sandboxID]
	if p.Vanished[sandboxID] || !ok {
		return nil, boxGone{fmt.Errorf("sandbox %q is gone", sandboxID)}
	}
	if box.Paused() {
		return nil, boxPaused{fmt.Errorf("sandbox %q is paused", sandboxID)}
	}
	return box, nil
}

// Kill records the id and forgets the box. It HONOURS ctx, as every real
// provider does (E2B's is an HTTP call, the local one waits on a process
// group): a fake that reaped on a dead context would leave every
// teardown-detach guard untested, the box reclaimed whether or not the caller
// detached.
func (p *FakeProvider) Kill(ctx context.Context, sandboxID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	kill := p.KillFunc
	p.mu.Unlock()
	if kill != nil {
		if err := kill(ctx, sandboxID); err != nil {
			return err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Killed = append(p.Killed, sandboxID)
	if p.KillErr != nil {
		return p.KillErr
	}
	if box := p.boxes[sandboxID]; box != nil {
		box.mu.Lock()
		box.reclaimed = true
		box.mu.Unlock()
	}
	delete(p.boxes, sandboxID)
	return nil
}

// Box returns a previously created box by id, or nil.
func (p *FakeProvider) Box(id string) *FakeSandbox {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.boxes[id]
}

// KilledIDs is every id passed to Kill, in order.
func (p *FakeProvider) KilledIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.Killed)
}

// FakeRunner is a harness whose job finishes when a test says so — or,
// with a [FakeRunner.Player], whose every job is played by that function.
type FakeRunner struct {
	mu sync.Mutex

	name      string
	installed map[string]bool
	started   []RunRequest

	done   bool
	result Result
	// lost is what [FakeRunner.Lose] took down with its engine, nil before
	// it is called.
	lost *fakeLoss

	// Player, when set, is what each job started on this runner DOES: called
	// once per job with its start request, its answer the job's result. Set
	// it before the first Start.
	//
	// PLAYED ON THE JOB'S FIRST POLL OR COLLECT, never at Start: a real job
	// works after its launch returned and its turn parked, so a play inside
	// Start would land its bridged calls mid-launch. It runs on the poller's
	// goroutine, so none here outlives the test. A job this runner did not
	// start (a previous process's) is reported done and failed, saying so:
	// its play lived in that process.
	Player func(ctx context.Context, req RunRequest) Result
	plays  map[string]*fakePlay

	// MainThread, when set, answers a collection's read of a job's main
	// thread ([FakeRunner.ReadMainThread]); unset, it answers no reading,
	// which a collection of a runner that reads its thread takes for a thread
	// not read. Set it before the first collection.
	MainThread func(ctx context.Context, box Sandbox, ask MainThreadAsk) MainThreadReading

	// Thread is what [FakeRunner.ThreadTraits] answers: what the harness
	// this fake stands in for settles by reading its main thread; zero reads
	// none. Set it before the first collection.
	Thread ThreadTraits

	// Pin is what [FakeRunner.PinnedWindow] answers of a model, 0 where it
	// is unset: the window the harness this fake stands in for pins a run to.
	Pin func(model string) int

	// Fresh is what [FakeRunner.StartsFresh] answers: the harness this fake
	// stands in for continues no earlier session. Set it before the first
	// launch.
	Fresh bool

	// TakesNotes is what [FakeRunner.ReadsNotes] answers: whether the
	// harness this fake stands in for takes a person's notes between its own
	// steps. Set it before the first collection.
	TakesNotes bool

	// NotesOff is what [FakeRunner.NotesOffWhy] answers: what in a box can
	// switch the reading of notes of the harness this fake stands in for off,
	// "" where it says nothing. Set it before the first collection.
	NotesOff string

	// DeferAt is what [FakeRunner.DefersMCPAt] answers: how many MCP tools
	// make the harness this fake stands in for hide them all, 0 for none. Set
	// it before the first launch.
	DeferAt int

	// Reports, where set, makes the fake a minting runner (minted.go): what
	// [FakeRunner.ReportedSession] answers of a job's box. Set it before the
	// launch whose job should mint.
	Reports func(ctx context.Context, box Sandbox, handle RunHandle) (string, error)

	// CallIDKey is what [FakeRunner.CallIDMeta] answers: the `_meta` key the
	// harness this fake stands in for sends each call's id under, "" for
	// none. Set it before the launch that should read it.
	CallIDKey string

	// StartErr, when set, fails every Start.
	StartErr error
	// PollErr, when set, fails every Poll — the transient case the waiter
	// must retry rather than treat as completion.
	PollErr error
	// PollFunc, when set, answers every Poll in place of the queued state,
	// called WITHOUT the runner's lock so one box's poll can be held while
	// another's runs, as a real runner's can.
	PollFunc func(ctx context.Context, box Sandbox) (JobState, error)
	// CollectErr, when set, fails every Collect.
	CollectErr error
	// CollectFunc, when set, is called by every Collect with its context,
	// and a non-nil answer fails it — for a collection whose failure
	// depends on what happened to the caller meanwhile.
	CollectFunc func(ctx context.Context) error
	// LiveErr, when set, fails every read of a live reading.
	LiveErr error
	// LiveGate, when set, is waited on by every read of a live reading
	// before it answers — a box that takes a while to read.
	LiveGate chan struct{}

	// EndFunc, when set, answers every request to end a job
	// ([FakeRunner.AskEnd]) in place of the default (a script that hears it
	// and ends its job), called without the runner's lock. Unheard makes
	// every request answer [EndUnheard] and ask nothing: a script that
	// records no end protocol. Set either before the first stop or loss.
	EndFunc func(ctx context.Context, box Sandbox, handle RunHandle) (EndAsk, error)
	Unheard bool
	// endsAs is what a job asked to end reports ([FakeRunner.EndsAs]), nil
	// for what it would have reported as it stood; ended is every box whose
	// job heard an end request and ended, by the box's id; asks counts the
	// requests made.
	endsAs *Result
	ended  map[string]bool
	asks   int

	// Session, when set, is the harness session every box this runner is
	// asked about holds, which [FakeRunner.TakeSession] hands a capture. Set
	// it before the first collection.
	Session *FakeSession
	// takes is every capture asked of this runner, in order.
	takes []SessionTake

	// Carries makes this fake restore a session carried from another box
	// ([SessionRestorer]): one its own harness wrote, in [FakeArchiveFormat],
	// which every box can take up unless CarryWhy says why its harness
	// cannot. Unset, it restores none. Set both before the first launch.
	Carries  bool
	CarryWhy string
	// CarryVersion is the release every box's harness answers when asked
	// whether it continues a session ([FakeRunner.CheckCarry],
	// [CarryCheck.Version]), "" for an answer this build cannot read. Set it
	// before the launch that asks.
	CarryVersion string
	// CarryResumeUnread makes every box's harness answer a release whose
	// resume this build did not read ([CarryCheck.ResumeRead]); unset, it
	// answers one it did. Set it before the launch that asks.
	CarryResumeUnread bool
	// InPlaceWhy, set, is why every box's harness continues no copy of a
	// session the box itself holds ([FakeRunner.CheckInPlace]); unset, each
	// continues its own. Set it before the launch that asks.
	InPlaceWhy string
	// EntryBound makes this fake's harness one not measured to send a resumed
	// session's calls on its launch's route, so a session continues only under
	// the entry it ran under ([EntryContinues]); unset, it continues under any.
	// Set it before the launch that asks.
	EntryBound bool
	// carried is every archive a job was started with, read whole as a
	// runner writes it into its box.
	carried []FakeCarried

	// Holdings, when set, answers a reading of what a kept session holds
	// ([FakeRunner.ReadHoldings]), once the archive's stream has been read to
	// its end; unset, every reading answers none. Set it before the relaunch
	// that asks.
	Holdings func(SessionArchive, HoldingsAsk) (SessionHoldings, error)
	// holdingsAsks is every ask a reading was handed, in order.
	holdingsAsks []HoldingsAsk

	// SavesSessions is what [FakeRunner.Saves] answers: whether the harness
	// this fake stands in for has its session saved while its run still
	// works, as both executor harnesses' runners do; false stands for one
	// that saves none. A save packs [FakeRunner.Session] as it stands then.
	// Set it before the first poll that may offer a save.
	SavesSessions bool
	// PackSaveErr, when set, fails every [FakeRunner.PackSave], as a pack
	// that a file rewritten under it refused fails. Packing, when set, runs
	// as each save is packed, before the pack is made, handed the pack's
	// context and the box, and is called without the runner's lock: so a
	// case can hold a pack, record a renewal while one is made, or spend its
	// time. A pack whose context is done once Packing returns fails with
	// the context's error, as a box's pack cut off by its caller does.
	PackSaveErr error
	Packing     func(ctx context.Context, box Sandbox)
	// StreamedErr, when set, fails every [FakeRunner.Streamed].
	StreamedErr error
	// streamed is how far the stream of each box's job has run, by the
	// box's id ([FakeRunner.Stream]), and streamedReads how many times it
	// was read.
	streamed      map[string]int64
	streamedReads int
	// saves are the saves packed and neither taken nor released yet, by
	// path; packedIn is the box every save was packed in, by path, taken or
	// not; released is every save released, in order; and packs numbers
	// the saves, so each has a path of its own.
	saves    map[string]fakeSave
	packedIn map[string]string
	released []string
	packs    int
	// Point, when set, is what a read of a box's OpenCode point answers
	// ([FakeRunner.ReadBoxPoint]); unset, every read is an error, a box
	// that could not be asked. Set it before the launch that reads.
	Point func(PointAsk) (BoxPoint, error)
	// pointAsks is every ask a read of a point was handed, in order.
	pointAsks []PointAsk

	// The job's live account: what it has said, under which origin and
	// from which source. See [FakeRunner.Say].
	account string
	origin  string
	source  OutputSource
	reads   int
	waiting int
	// spent is what the job reports having spent, read by every live
	// reading. See [FakeRunner.Spend].
	spent int
}

var _ Runner = (*FakeRunner)(nil)

// Spend is the job reporting it has spent n tokens in all, as a live reading
// of a real job's stream reports.
func (r *FakeRunner) Spend(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spent = n
}

// fakePlay is one job a [FakeRunner.Player] plays: the request it was started
// with, and the result once it has been played.
type fakePlay struct {
	req    RunRequest
	once   sync.Once
	result Result
	// finished is set once the play has answered.
	finished atomic.Bool
}

// played reports whether the job's play has answered.
func (p *fakePlay) played() bool { return p.finished.Load() }

// NewFakeRunner returns a runner under the given name.
func NewFakeRunner(name string) *FakeRunner {
	return &FakeRunner{name: name, installed: map[string]bool{}, plays: map[string]*fakePlay{},
		ended: map[string]bool{}, streamed: map[string]int64{}, saves: map[string]fakeSave{},
		packedIn: map[string]string{}}
}

// fakeScriptPID is the pid a [FakeRunner]'s job's script answers an end request
// on, standing in for the one a real script records in its box.
const fakeScriptPID = 4243

var _ JobEnder = (*FakeRunner)(nil)

// AskEnd asks the job in box to end, as a real runner does: through the box
// ([Sandbox.EndJob]), the job by default hearing it and ending, so its next
// poll reports it over and its collection reads what [FakeRunner.EndsAs] set,
// or what it would have reported as it stood. [FakeRunner.EndFunc] and
// [FakeRunner.Unheard] say otherwise; a box that answers the pid is no
// process of the job is [EndGone].
func (r *FakeRunner) AskEnd(ctx context.Context, box Sandbox, handle RunHandle) (EndAsk, error) {
	r.mu.Lock()
	end, unheard := r.EndFunc, r.Unheard
	r.asks++
	r.mu.Unlock()
	switch {
	case end != nil:
		return end(ctx, box, handle)
	case unheard:
		return EndUnheard, nil
	}
	switch err := box.EndJob(ctx, handle.CommandID, fakeScriptPID); {
	case errors.Is(err, ErrNotJobProcess):
		return EndGone, nil
	case err != nil:
		return "", err
	}
	r.EndJobIn(box)
	return EndAsked, nil
}

// EndsAs makes a job asked to end report result once it has.
func (r *FakeRunner) EndsAs(result Result) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.endsAs = &result
}

// EndJobIn ends the job in box as a request would: over at its next poll,
// reporting what [FakeRunner.EndsAs] set. A [FakeRunner.EndFunc] calls it to
// let a request end the job.
func (r *FakeRunner) EndJobIn(box Sandbox) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ended == nil {
		r.ended = map[string]bool{}
	}
	r.ended[box.ID()] = true
}

// endedLocked reports whether the job in box heard a request to end it and
// ended. A NIL BOX is a case asking the runner alone about a job, as the
// engine's rigs poll it, and holds no job a request ended.
func (r *FakeRunner) endedLocked(box Sandbox) bool {
	return box != nil && r.ended[box.ID()]
}

// Asks is how many requests to end a job were made of this runner.
func (r *FakeRunner) Asks() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.asks
}

// play is a played job's result, playing it first if nothing has yet; ok is
// false where no Player is set and the job finishes when a test says so.
func (r *FakeRunner) play(ctx context.Context, handle RunHandle) (Result, bool) {
	r.mu.Lock()
	player := r.Player
	p := r.plays[handle.CommandID]
	r.mu.Unlock()
	if player == nil {
		return Result{}, false
	}
	if p == nil {
		return Result{Error: "the fake runner did not start this job: its play lived in " +
			"the process that started it"}, true
	}
	// ONCE, under the job's own guard rather than the runner's lock: a play
	// takes as long as its work, and no other job's poll or read of what was
	// started may queue behind it.
	p.once.Do(func() {
		p.result = player(ctx, p.req)
		p.finished.Store(true)
	})
	return p.result, true
}

// Name is the harness's key in the runner registry.
func (r *FakeRunner) Name() string { return r.name }

// ReadsNotes answers [FakeRunner.TakesNotes]: the optional answer a runner
// whose harness takes a person's notes between its steps gives the coordinator.
func (r *FakeRunner) ReadsNotes() bool { return r.TakesNotes }

// MintsSession answers whether [FakeRunner.Reports] is set: the fake stands in
// for a harness that mints its session's id as it runs.
func (r *FakeRunner) MintsSession() bool { return r.Reports != nil }

// ReportedSession answers [FakeRunner.Reports], "" where it is unset.
func (r *FakeRunner) ReportedSession(ctx context.Context, box Sandbox, handle RunHandle) (string, error) {
	if r.Reports == nil {
		return "", nil
	}
	return r.Reports(ctx, box, handle)
}

// NotesOffWhy answers [FakeRunner.NotesOff]: the optional answer a runner gives
// the warning a run that never asked for its notes gets ([notesOffExplainer]).
func (r *FakeRunner) NotesOffWhy() string { return r.NotesOff }

// DefersMCPAt answers [FakeRunner.DeferAt]: the optional answer a runner whose
// harness hides every MCP tool past a count gives the launch ([MCPDeferrer]).
func (r *FakeRunner) DefersMCPAt() int { return r.DeferAt }

// CallIDMeta answers [FakeRunner.CallIDKey]: the optional answer a runner whose
// harness names each of its calls gives the launch ([CallIdentifier]).
func (r *FakeRunner) CallIDMeta() string { return r.CallIDKey }

var (
	_ CallIdentifier = (*FakeRunner)(nil)
	_ MCPDeferrer    = (*FakeRunner)(nil)
)

// Install records that the agent was provisioned into the box.
func (r *FakeRunner) Install(ctx context.Context, box Sandbox) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.installed[box.ID()] = true
	return nil
}

// Start records the request and hands back a handle that stays unfinished
// until a test calls [FakeRunner.Finish] or the job is played.
func (r *FakeRunner) Start(ctx context.Context, box Sandbox, req RunRequest) (RunHandle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.StartErr != nil {
		return RunHandle{}, r.StartErr
	}
	req.Env = maps.Clone(req.Env)
	// THE CARRIED ARCHIVE IS READ AS THE RUN STARTS, as a runner writes it
	// into its box: the launch releases what it spooled once Start returns.
	if c := req.Carried; c != nil {
		body, err := io.ReadAll(c.Body)
		if err != nil {
			return RunHandle{}, fmt.Errorf("fake: reading the carried session: %w", err)
		}
		r.carried = append(r.carried, FakeCarried{Archive: c.Archive, Body: body, Replace: c.Replace})
	}
	r.started = append(r.started, req)
	handle := RunHandle{CommandID: fmt.Sprintf("cmd-%d", len(r.started)), PID: 4242}
	// THE SESSION THE RUN WORKS IN, NAMED AS IT STARTS, as a runner names it
	// before its harness starts: the one it continues or one minted for it,
	// plus, for a continuing run, the fresh one its script starts in that
	// one's place. What a box ran is read off these and its word
	// ([PendingRun.SessionRan]), never off its collection.
	handle.SessionID = "session-" + handle.CommandID
	if req.Resume != "" {
		handle.SessionID = req.Resume
		handle.FreshSessionID = "fresh-" + handle.CommandID
	}
	if r.Player != nil {
		r.plays[handle.CommandID] = &fakePlay{req: req}
	}
	return handle, nil
}

// Poll reports the job ended once [FakeRunner.Finish] has been called — or,
// under a [FakeRunner.Player], once the job has been played, which this poll
// does — and lost once [FakeRunner.Lose] took it down with its engine.
func (r *FakeRunner) Poll(ctx context.Context, box Sandbox, handle RunHandle) (JobState, error) {
	r.mu.Lock()
	poll := r.PollFunc
	_, lost := r.lostJob(handle)
	ended := r.endedLocked(box)
	r.mu.Unlock()
	if poll != nil {
		return poll(ctx, box)
	}
	if lost {
		return JobLost, nil
	}
	// AN ENDED JOB IS OVER, whatever else is staged: its script wrote its
	// done marker, which a real runner reads before it asks anything else.
	if ended {
		return JobEnded, nil
	}
	if _, played := r.play(ctx, handle); played {
		return JobEnded, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.PollErr != nil {
		return "", r.PollErr
	}
	if r.done {
		return JobEnded, nil
	}
	return JobRunning, nil
}

// Collect returns the result a test queued with [FakeRunner.Finish], or the
// one the job's play answered.
func (r *FakeRunner) Collect(ctx context.Context, box Sandbox, handle RunHandle, _ *queue.AnswerBudget) (Result, error) {
	r.mu.Lock()
	if r.CollectErr != nil {
		r.mu.Unlock()
		return Result{}, r.CollectErr
	}
	collect := r.CollectFunc
	r.mu.Unlock()
	if collect != nil {
		if err := collect(ctx); err != nil {
			return Result{}, err
		}
	}
	r.mu.Lock()
	partial, lost := r.lostJob(handle)
	endsAs := r.endsAs
	ended := r.endedLocked(box)
	r.mu.Unlock()
	if lost {
		return partial, nil
	}
	if ended && endsAs != nil {
		return *endsAs, nil
	}
	if result, played := r.play(ctx, handle); played {
		return result, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.result, nil
}

// Follow begins a live reading of the account a test writes with
// [FakeRunner.Say].
//
// THE ACCOUNT IS DETERMINISTIC, as a real runner's derivation is: the same
// account under the same origin reads the same text, so a reading begun afresh
// (another owner on the same build) continues where an earlier one stopped.
// [FakeRunner.Rewrite] is the other case: a build whose parser or redaction
// derives different text under the same origin.
func (r *FakeRunner) Follow(handle RunHandle) LiveReading {
	r.mu.Lock()
	defer r.mu.Unlock()
	return &fakeReading{runner: r}
}

// Say is the job writing more of its account, from source, under the current
// origin ("fake@0" until [FakeRunner.Restart] names another).
func (r *FakeRunner) Say(source OutputSource, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.source = source
	r.account += text
}

// Restart begins the account anew under another origin — a stream read again
// from another place, or a switch from one account to the other.
func (r *FakeRunner) Restart(origin string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.origin, r.account = origin, ""
}

// Rewrite replaces the account wholesale under the SAME origin: what a node
// whose build derives the stream differently would read.
func (r *FakeRunner) Rewrite(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.account = text
}

// Reads is how many times a live reading read the box.
func (r *FakeRunner) Reads() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reads
}

// Waiting is how many reads have begun and not answered yet.
func (r *FakeRunner) Waiting() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.waiting
}

// fakeReading is one live reading of a [FakeRunner]'s account.
type fakeReading struct {
	runner *FakeRunner
	origin string
	at     int
}

func (f *fakeReading) Read(ctx context.Context, _ Sandbox) (LiveRead, error) {
	r := f.runner
	r.mu.Lock()
	gate := r.LiveGate
	r.waiting++
	r.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			r.mu.Lock()
			r.waiting--
			r.mu.Unlock()
			return LiveRead{}, ctx.Err()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.waiting--
	r.reads++
	if r.LiveErr != nil {
		return LiveRead{}, r.LiveErr
	}
	origin := r.origin
	if origin == "" {
		origin = "fake@0"
	}
	if origin != f.origin || f.at > len(r.account) {
		f.origin, f.at = origin, 0
	}
	source := r.source
	if source == "" {
		source = SourceNone
	}
	text := r.account[f.at:]
	f.at = len(r.account)
	return LiveRead{Origin: origin, Source: source, Text: text, Finished: r.done, Spent: r.spent}, nil
}

// Finish makes the next Poll report done and the next Collect return result.
func (r *FakeRunner) Finish(result Result) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.done = true
	r.result = result
}

// Lose takes every job running now down with its engine, as a host's restart
// does mid-step: each this runner started and has not played, and each it is
// asked about that it never started (a previous process's). A poll of one
// reports [JobLost], and a collection reads partial, what it had said before
// it went. A job started afterwards is not lost.
func (r *FakeRunner) Lose(partial Result) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lost = &fakeLoss{before: len(r.started), partial: partial}
}

// fakeLoss is what [FakeRunner.Lose] took down: the jobs started before it,
// and what each had said.
type fakeLoss struct {
	before  int
	partial Result
}

// lostJob is what a lost job had said, and whether the job handle names was
// lost. Called under the runner's lock.
func (r *FakeRunner) lostJob(handle RunHandle) (Result, bool) {
	if r.lost == nil {
		return Result{}, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(handle.CommandID, "cmd-"))
	switch {
	case err != nil || !strings.HasPrefix(handle.CommandID, "cmd-") || n < 1 || n > len(r.started):
		// NOT THIS RUNNER'S: a previous process started it, and it went
		// with that process.
		return r.lost.partial, true
	case n > r.lost.before:
		return Result{}, false
	}
	if p := r.plays[handle.CommandID]; p != nil && p.played() {
		return Result{}, false
	}
	return r.lost.partial, true
}

// FakeSession is a session a [FakeRunner]'s boxes hold, as a capture takes it.
type FakeSession struct {
	// Archive describes it, and Body is its archive's bytes — what the
	// capture is handed as they are, the fake scrubbing nothing.
	Archive SessionArchive
	Body    []byte

	// TakeErr fails the take, as a box whose session cannot be read does,
	// and KeepErr the record of its digest in the box.
	TakeErr, KeepErr error

	// ContextTokens is the context its transcript records for the session's
	// last request ([TakenSession.Context]), zero for none.
	ContextTokens int64

	// Count is what the take counts the session at ([TakenSession.Count]),
	// nil for a take that counts nothing.
	Count *ResumedCount

	// NextRequest is what the take estimates the session's next request at
	// ([TakenSession.Next]), zero for none.
	NextRequest NextRequest

	// CountProvider is the provider the take names beside its count
	// ([TakenSession.CountProvider]), "" for none.
	CountProvider string

	// RunLines is the run start's line count the take found whole
	// ([TakenSession.RunLines]), 0 for none, answered once the take was
	// read to its end.
	RunLines int64

	// ResumeRead is what the take says of the archive's release
	// ([TakenSession.ResumeRead]).
	ResumeRead bool

	// Taking, where set, runs with the take's context as a take reads the
	// box, after the capture placed the session from the run's log and before
	// the archive is handed over: a case lands a call between the two there,
	// or spends the take's time as a pack and scrub at their bound do.
	Taking func(ctx context.Context)

	// Streaming, where set, runs with the take's context as the archive is
	// first read (its place reserved, spooling), so a case can spend the
	// take's time where a stream at its bound does. Held until that context
	// ends, the read fails, as a stalled stream's does.
	Streaming func(ctx context.Context)

	// Keeping, where set, runs with the digest's context just before the
	// take's digest is recorded in the box ([TakenSession.Keep]): a box slow
	// to answer the write.
	Keeping func(ctx context.Context)

	// kept counts the digests recorded.
	kept atomic.Int32
}

// Kept is how many captures of the session recorded their digest in the box.
func (s *FakeSession) Kept() int { return int(s.kept.Load()) }

// TakeSession hands a capture the runner's [FakeRunner.Session], recording what
// it was asked ([FakeRunner.Takes]); a runner with none holds no session. A
// take that names a save ([SessionTake.Save]) is handed that save as
// [FakeRunner.PackSave] packed it, once, and the save is gone after: nothing
// where it was already taken or released, and an error where the path names
// no save packed in the box.
//
// A BOX THAT IS PAUSED OR RECLAIMED IS NOT READ, as no real one is: a capture
// made after the pause or reclaim it must precede is refused here as there.
func (r *FakeRunner) TakeSession(ctx context.Context, box Sandbox, take SessionTake) (TakenSession, error) {
	r.mu.Lock()
	recorded := take
	recorded.Scrub = slices.Clone(take.Scrub)
	r.takes = append(r.takes, recorded)
	session := r.Session
	r.mu.Unlock()
	if fake, ok := box.(*FakeSandbox); ok && fake.unreadable() {
		return TakenSession{}, fmt.Errorf("fake: box %s is paused or reclaimed, and its session cannot be "+
			"read", fake.ID())
	}
	var (
		archive SessionArchive
		raw     []byte
	)
	switch {
	case take.Save != "" && take.Pack:
		return TakenSession{}, errors.New("fake: a take of a save packs nothing")
	case take.Save != "":
		save, there, err := r.takeSave(box, take.Save)
		if err != nil || !there {
			return TakenSession{}, err
		}
		archive, raw = save.archive, save.body
		if session == nil {
			session = &FakeSession{}
		}
	case session == nil:
		return TakenSession{}, nil
	default:
		archive, raw = session.Archive, session.Body
	}
	if session.TakeErr != nil {
		return TakenSession{}, session.TakeErr
	}
	// ITS SCRUB SPOOLS NOTHING, so its spool holds nothing.
	if take.Spool.Held != nil {
		take.Spool.Held(0)
	}
	// A MINTING FAKE'S SESSION IS THE ONE BOUND TO ITS JOB, as a real runner
	// names it ([SessionTake.Session]); one with none bound takes nothing.
	if r.MintsSession() {
		if take.Session == "" {
			return TakenSession{}, fmt.Errorf("fake: %w: no session is bound to the job", ErrSessionBindRefused)
		}
		archive.SessionID = take.Session
	}
	if session.Taking != nil {
		session.Taking(ctx)
	}
	var count func() *ResumedCount
	if session.Count != nil {
		count = func() *ResumedCount { counted := *session.Count; return &counted }
	}
	body := &takenFake{Reader: bytes.NewReader(raw), take: ctx}
	if session.Streaming != nil {
		body.stall = func() { session.Streaming(ctx) }
	}
	return TakenSession{
		Archive:    archive,
		Body:       body,
		ResumeRead: session.ResumeRead,
		Context:    func() int64 { return session.ContextTokens },
		Next:       func() NextRequest { return session.NextRequest },
		Count:      count,
		CountProvider: func() string {
			return session.CountProvider
		},
		RunLines: func() int64 {
			if !body.ended.Load() {
				return 0
			}
			return session.RunLines
		},
		Keep: func(ctx context.Context) error {
			if session.Keeping != nil {
				session.Keeping(ctx)
			}
			if session.KeepErr != nil {
				return session.KeepErr
			}
			session.kept.Add(1)
			return nil
		},
	}, nil
}

// takenFake is a fake take's body. Its reads fail once it is closed, as the
// executor's do, so a spool its context cut keeps nothing; and a first read
// stall (Streaming) held until the take's context ended fails, as the box's
// stream the spool closes at that same end does ([Coordinator.spool]).
type takenFake struct {
	io.Reader
	take   context.Context
	stall  func()
	once   sync.Once
	closed atomic.Bool
	// ended says the take was read to its end, which a real take's RunLines
	// waits for ([TakenSession.RunLines]).
	ended atomic.Bool
}

func (b *takenFake) Read(p []byte) (int, error) {
	if b.stall != nil {
		b.once.Do(b.stall)
		if err := b.take.Err(); err != nil {
			return 0, fmt.Errorf("fake: the take's stream stalled past its context: %w", err)
		}
	}
	if b.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	n, err := b.Reader.Read(p)
	if errors.Is(err, io.EOF) {
		b.ended.Store(true)
	}
	return n, err
}

func (b *takenFake) Close() error {
	b.closed.Store(true)
	return nil
}

// Takes is every capture asked of this runner, in order.
func (r *FakeRunner) Takes() []SessionTake {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.takes)
}

// fakeSave is a save a [FakeRunner] packed: the box it was packed in, by its
// id, and the session as it stood then.
type fakeSave struct {
	box     string
	archive SessionArchive
	body    []byte
}

// Saves is [FakeRunner.SavesSessions].
func (r *FakeRunner) Saves() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.SavesSessions
}

// Streamed is how far the stream of the job in box has run, as
// [FakeRunner.Stream] moved it: zero for a box whose stream never moved.
func (r *FakeRunner) Streamed(_ context.Context, box Sandbox) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.streamedReads++
	if r.StreamedErr != nil {
		return 0, r.StreamedErr
	}
	return r.streamed[box.ID()], nil
}

// Stream moves the stream of the job in box on by n bytes, as its harness
// printing a step of its work does.
func (r *FakeRunner) Stream(box Sandbox, n int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.streamed[box.ID()] += n
}

// StreamedReads is how many times [FakeRunner.Streamed] was asked.
func (r *FakeRunner) StreamedReads() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.streamedReads
}

// PackSave packs a save of [FakeRunner.Session] as it stands, for taker, at a
// path of that save's own in box, which a take naming it streams once
// ([SessionTake.Save]) and a release removes ([FakeRunner.ReleaseSave]). As a
// real runner's: a zero description where nothing is saved (a fake that saves
// no sessions, or holds none), and an error for a taker that names nobody or
// a box that is paused or reclaimed. [FakeRunner.Packing] runs first, and
// [FakeRunner.PackSaveErr] fails it.
func (r *FakeRunner) PackSave(ctx context.Context, box Sandbox, taker, session string) (SessionArchive, error) {
	r.mu.Lock()
	saves, packing, failed := r.SavesSessions, r.Packing, r.PackSaveErr
	r.mu.Unlock()
	switch {
	case !saves:
		return SessionArchive{}, nil
	case taker == "":
		return SessionArchive{}, errors.New("fake: a save names no taker")
	case r.MintsSession() && session == "":
		return SessionArchive{}, fmt.Errorf("fake: %w: no session is bound to the job", ErrSessionBindRefused)
	}
	if fake, ok := box.(*FakeSandbox); ok && fake.unreadable() {
		return SessionArchive{}, fmt.Errorf("fake: box %s is paused or reclaimed, and its session cannot be "+
			"saved", fake.ID())
	}
	if packing != nil {
		packing(ctx, box)
	}
	if err := ctx.Err(); err != nil {
		return SessionArchive{}, fmt.Errorf("fake: the save was cut off as it was packed: %w", err)
	}
	if failed != nil {
		return SessionArchive{}, failed
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Session == nil {
		return SessionArchive{}, nil
	}
	r.packs++
	archive := r.Session.Archive
	archive.Path = path.Join(box.Home(), "session-save."+taker+"."+strconv.Itoa(r.packs))
	if r.MintsSession() {
		archive.SessionID = session
	}
	r.saves[archive.Path] = fakeSave{box: box.ID(), archive: archive, body: bytes.Clone(r.Session.Body)}
	r.packedIn[archive.Path] = box.ID()
	return archive, nil
}

// takeSave hands over, and removes, the save at p in box: not there, with no
// error, where it was already taken or released, and an error where p names
// no save this fake packed in box, as a real take refuses one.
func (r *FakeRunner) takeSave(box Sandbox, p string) (fakeSave, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if in, packed := r.packedIn[p]; !packed || in != box.ID() {
		return fakeSave{}, false, fmt.Errorf("fake: %q is no save packed in box %s", p, box.ID())
	}
	save, there := r.saves[p]
	delete(r.saves, p)
	return save, there, nil
}

// ReleaseSave removes the save at p from box, recording that it was asked
// ([FakeRunner.Released]); a path that is no save this fake packed in box is
// refused, as a real runner refuses one.
func (r *FakeRunner) ReleaseSave(_ context.Context, box Sandbox, p string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if in, packed := r.packedIn[p]; !packed || in != box.ID() {
		return fmt.Errorf("fake: %q is no save packed in box %s, and a release removes a save and nothing else",
			p, box.ID())
	}
	r.released = append(r.released, p)
	delete(r.saves, p)
	return nil
}

// Released is every save released, by path, in order.
func (r *FakeRunner) Released() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.released)
}

// SavesLeft is every save of box's packed and neither taken nor released, by
// path, sorted.
func (r *FakeRunner) SavesLeft(box Sandbox) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var left []string
	for p, save := range r.saves {
		if save.box == box.ID() {
			left = append(left, p)
		}
	}
	slices.Sort(left)
	return left
}

// FakeArchiveFormat is the one archive format a [FakeRunner] that carries
// sessions restores.
const FakeArchiveFormat = 1

// FakeCarried is an archive a job of a [FakeRunner] was started with
// ([RunRequest.Carried]), as its box was handed it.
type FakeCarried struct {
	Archive SessionArchive
	Body    []byte
	Replace bool
}

// Carried is every archive a job was started with, in order.
func (r *FakeRunner) Carried() []FakeCarried {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.carried)
}

var _ SessionRestorer = (*FakeRunner)(nil)

var _ SessionSaver = (*FakeRunner)(nil)

// RestoresUnder answers as a runner of a harness that packs and restores does
// where [FakeRunner.Carries] is set — a session its own harness wrote, in
// [FakeArchiveFormat], under an entry [EntryContinues] lets it continue under
// ([FakeRunner.EntryBound]) — and restores nothing otherwise.
func (r *FakeRunner) RestoresUnder(archive SessionArchive, entry string) (bool, string) {
	if ok, why := r.restores(archive); !ok {
		return false, why
	}
	return EntryContinues(archive.Entry, entry, r.name, r.ResumesOnLaunchRoute())
}

// ResumesOnLaunchRoute answers [launchRouter]: unset [FakeRunner.EntryBound].
func (r *FakeRunner) ResumesOnLaunchRoute() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.EntryBound
}

// restores is [FakeRunner.RestoresUnder] less the entry, which a box's own
// check ([FakeRunner.CheckCarry]) follows.
func (r *FakeRunner) restores(archive SessionArchive) (bool, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case !r.Carries:
		return false, "fake: this runner carries no session between boxes"
	case archive.Harness != r.name:
		return false, fmt.Sprintf("fake: the session was written by %q, and this runner drives %q",
			archive.Harness, r.name)
	case archive.Format != FakeArchiveFormat:
		return false, fmt.Sprintf("fake: the session's archive is format %d", archive.Format)
	}
	return true, ""
}

// CheckCarry answers [FakeRunner.restores], and then [FakeRunner.CarryWhy] for
// the box's own harness, with the release that harness answered
// ([FakeRunner.CarryVersion]) either way, as a real runner reads it before it
// decides.
func (r *FakeRunner) CheckCarry(_ context.Context, _ Sandbox, archive SessionArchive) (CarryCheck, error) {
	ok, why := r.restores(archive)
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case !ok:
		return CarryCheck{Version: r.CarryVersion, ResumeRead: !r.CarryResumeUnread, Why: why}, nil
	case r.CarryWhy != "":
		return CarryCheck{Version: r.CarryVersion, ResumeRead: !r.CarryResumeUnread, Why: r.CarryWhy}, nil
	}
	return CarryCheck{Version: r.CarryVersion, ResumeRead: !r.CarryResumeUnread, Continues: true}, nil
}

// CheckInPlace answers [FakeRunner.InPlaceWhy], with the release that harness
// answered ([FakeRunner.CarryVersion]), as a real runner reads it before it
// decides.
func (r *FakeRunner) CheckInPlace(context.Context, Sandbox) (CarryCheck, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return CarryCheck{Version: r.CarryVersion, ResumeRead: !r.CarryResumeUnread, Continues: r.InPlaceWhy == "",
		Why: r.InPlaceWhy}, nil
}

var _ HoldingsReader = (*FakeRunner)(nil)

// ReadHoldings records ask, reads what open streams to its end and closes it,
// and answers [FakeRunner.Holdings] of archive and ask, read through whatever
// calls it ([Coordinator.HoldingsOf]). Unset, it answers no reading and opens
// nothing, as a reader that refuses the ask before opening the archive does.
func (r *FakeRunner) ReadHoldings(ctx context.Context, archive SessionArchive,
	open func(context.Context) (io.ReadCloser, error), ask HoldingsAsk) (SessionHoldings, error) {

	r.mu.Lock()
	r.holdingsAsks = append(r.holdingsAsks, ask)
	holdings := r.Holdings
	r.mu.Unlock()
	if holdings == nil {
		return SessionHoldings{}, errors.New("fake: this runner reads no session's holdings")
	}
	body, err := open(ctx)
	if err != nil {
		return SessionHoldings{}, err
	}
	_, err = io.Copy(io.Discard, body)
	if closeErr := body.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return SessionHoldings{}, err
	}
	return holdings(archive, ask)
}

// HoldingsAsks is every ask a reading of this runner was handed, in order.
func (r *FakeRunner) HoldingsAsks() []HoldingsAsk {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.holdingsAsks)
}

// Installed reports whether the agent was installed in this box.
func (r *FakeRunner) Installed(sandboxID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.installed[sandboxID]
}

// Started is every run request handed to Start, in order and whole, steer
// wiring included ([RunRequest.Steer]), so a case can play the harness's side
// of the run against what the launch gave it.
func (r *FakeRunner) Started() []RunRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.started)
}

var _ PointReader = (*FakeRunner)(nil)

// ReadBoxPoint records ask and answers [FakeRunner.Point] of it, an error
// where it is unset.
func (r *FakeRunner) ReadBoxPoint(_ context.Context, _ Sandbox, ask PointAsk) (BoxPoint, error) {
	r.mu.Lock()
	r.pointAsks = append(r.pointAsks, ask)
	point := r.Point
	r.mu.Unlock()
	if point == nil {
		return BoxPoint{}, errors.New("fake: this runner reads no box point")
	}
	return point(ask)
}

// PointAsks is every ask a read of a box's OpenCode point was handed, in
// order.
func (r *FakeRunner) PointAsks() []PointAsk {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.pointAsks)
}

var _ mainThreadReader = (*FakeRunner)(nil)

// ThreadTraits answers [FakeRunner.Thread].
func (r *FakeRunner) ThreadTraits() ThreadTraits {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Thread
}

var _ windowPinner = (*FakeRunner)(nil)

// StartsFresh is [FakeRunner.Fresh] ([freshStarter]).
func (r *FakeRunner) StartsFresh() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Fresh
}

// PinnedWindow answers [FakeRunner.Pin] of model, 0 where it is unset.
func (r *FakeRunner) PinnedWindow(model string) int {
	r.mu.Lock()
	pin := r.Pin
	r.mu.Unlock()
	if pin == nil {
		return 0
	}
	return pin(model)
}

// ReadMainThread answers a collection's read of a job's main thread with what
// [FakeRunner.MainThread] says, and no reading where it is unset.
func (r *FakeRunner) ReadMainThread(ctx context.Context, box Sandbox, ask MainThreadAsk) MainThreadReading {
	r.mu.Lock()
	read := r.MainThread
	r.mu.Unlock()
	if read == nil {
		return MainThreadReading{}
	}
	return read(ctx, box, ask)
}
