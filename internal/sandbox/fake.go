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
)

// The in-process test doubles.
//
// They live in the package rather than a _test.go file because the coordinator,
// the waiter, the tool and the engine all drive a sandbox and all need one to
// drive. A double per consumer is how the state machine ends up modelled four
// different ways, with each copy agreeing with the code that grew beside it.
//
// What they model is the SHAPE the real backends share — a box with a
// filesystem, a background job that finishes when a test says so, a provider
// that can lose a box — and nothing else. Where a real backend's behaviour is
// load-bearing (process groups, the pause reaper, path escapes) the test for it
// runs against the real thing; see local_test.go.

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
	// reclaimed is a box its provider killed ([FakeProvider.Kill]), which
	// nothing reads any more.
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

	// ReadErr, when set, is consulted by every read with the file's path,
	// and a non-nil answer is that read's error — standing in for a box
	// whose file could not be read back (a transport failure, not a size).
	ReadErr func(path string) error
}

var _ Sandbox = (*FakeSandbox)(nil)

// NewFakeSandbox mints a box with the given id. It reads files whole up to
// [MaxFileBytes], as every real box does.
func NewFakeSandbox(id string) *FakeSandbox {
	return &FakeSandbox{id: id, home: DefaultHome, files: map[string][]byte{}}
}

// CapReads holds the box's whole reads to n bytes rather than [MaxFileBytes],
// and returns the box.
//
// FOR A SUITE THAT HAS TO GO PAST THE CAP: a stream, a report or a question
// one byte over it is the case such a suite is about, and at 32 MiB each one is
// built, copied and redacted under -race for seconds. Every branch it reaches
// is the same at any cap, so it runs at one it can fill, sizes what it writes
// from [FakeSandbox.ReadCap], and leaves the real figure to the file contract,
// which every real backend keeps at it. n must be positive: a cap of nothing
// refuses every file, and no case means that.
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

// Home is the box's root directory, which for a fake is a fixed path
// nothing on disk backs.
func (s *FakeSandbox) Home() string { return s.home }

// FakeExec is one command a [FakeSandbox] was asked to run, and the
// environment and directory it was given.
type FakeExec struct {
	Command    string
	Env        map[string]string
	Cwd        string
	Background bool
}

// Exec records the command and returns whatever the test queued for it,
// or an empty success.
func (s *FakeSandbox) Exec(ctx context.Context, cmd string, opts ExecOptions) (ExecResult, error) {
	s.mu.Lock()
	s.execs = append(s.execs, FakeExec{Command: cmd, Env: maps.Clone(opts.Env), Cwd: opts.Cwd})
	s.mu.Unlock()
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
// environment and directory it was given: what a case about a run's
// environment reaching its box reads, since nothing else of a fake box keeps
// one.
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
// taken — a source is the caller's and may be slow — and only then put in the
// file's place, so a source that fails part way leaves the file as it was, as
// every real backend's staged write does ([Sandbox.WriteStream]). An empty
// source stores what an empty slice always did: nothing, read as an empty file.
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
// ([FakeSandbox.CapReads]) — exactly as every real backend does ([readCapped]
// is their rule too). It answered the whole file whatever its size, so every
// runner and coordinator test that ran through this twin certified a read no
// real box would make: a 33 MiB stdout read whole here was a run wedged or
// lost on every backend it could reach.
func (s *FakeSandbox) ReadFile(ctx context.Context, p string) ([]byte, error) {
	content, err := s.content(p)
	if err != nil || content == nil {
		return nil, err
	}
	return readCapped(s.delivering(bytes.NewReader(content)), p, s.ReadCap())
}

// OpenFile streams the file whole, as every real backend does: a machine
// stream is never refused for its size.
func (s *FakeSandbox) OpenFile(ctx context.Context, p string) (io.ReadCloser, error) {
	content, err := s.content(p)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(s.delivering(bytes.NewReader(content))), nil
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
	return io.NopCloser(s.delivering(bytes.NewReader(content[start:]))), size, nil
}

// Delivered is how many bytes of its files the box's reads have handed out —
// each stream counted as far as its reader took it, a refused whole read as
// far as the refusal read: what a remote box would have sent over the wire,
// which a suite holds a reader's cost to.
func (s *FakeSandbox) Delivered() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.delivered
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

// SetTimeout records the refresh, and when it came, and ends nothing: a case
// about a box its timer can end models the end itself, from
// [FakeSandbox.Timeouts].
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

// Pause marks the box snapshotted; [FakeSandbox.Paused] reports it.
func (s *FakeSandbox) Pause(ctx context.Context) error {
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

// Put seeds a file, standing in for something the job wrote.
func (s *FakeSandbox) Put(p, content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[path.Clean(p)] = []byte(content)
}

// FakeProvider mints [FakeSandbox]es and remembers them, so Connect returns
// the same box a Create handed out — which is what makes a reconnect across a
// simulated restart testable.
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
	// WITHOUT the provider's lock, and a non-nil answer fails the Attach —
	// a control plane that is slow to answer, or does not, for one box
	// while it answers for the rest.
	AttachFunc func(ctx context.Context, sandboxID string) error
	// KillFunc, when set, is called by every Kill on a live context before
	// it answers, WITHOUT the provider's lock, and a non-nil answer fails
	// the Kill unrecorded — a control plane that is slow to answer a
	// reclaim, or does not.
	KillFunc func(ctx context.Context, sandboxID string) error
	// Made, when set, is handed every box Create makes, before Create
	// answers: a case scripts a fresh box's commands there.
	Made func(box *FakeSandbox)

	// runs is the run each box was made for, as a local box records it
	// ([HostBox]): kept for a box made for a turn in a cell on the engine's
	// host, the one kind a local backend lists.
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

// Connect returns a previously created box, which is what makes a
// reconnect across a simulated restart testable.
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
	// Connect auto-resumes, matching every real backend.
	box.mu.Lock()
	box.paused = false
	box.mu.Unlock()
	return box, nil
}

// Attach returns a previously created box WITHOUT resuming it, and refuses a
// paused one with [ErrBoxPaused] — what the remote backend does, and the
// stricter of the two real answers, so a reader certified here is one that
// copes with a box it may not read.
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

// Kill records the id and forgets the box.
// Kill HONOURS ctx, like every real provider: E2B's is an HTTP call and the
// local one waits on a process group, so both fail on a dead context. A fake
// that reaped regardless would make every teardown-detach guard untestable —
// the box would come back reclaimed whether or not the caller detached.
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
	// once per job with the request it was started with, and its answer is
	// the job's result. Set it before the first Start.
	//
	// PLAYED ON THE JOB'S FIRST POLL OR COLLECT, never at Start: a detached
	// job does its work after its launch has returned and its turn has
	// parked, and a play that ran inside Start would make every bridged call
	// it makes land while the launch is still in flight — a shape no real
	// job has. And played on the poller's goroutine rather than one of the
	// runner's own, so there is no goroutine here to outlive the test or the
	// process: the job's work is bounded by the poll that asked for it.
	//
	// A job this runner did not start — one a previous process launched —
	// is reported done and failed, saying so: its play lived in the process
	// that started it, and a fake has nothing to resume it from.
	Player func(ctx context.Context, req RunRequest) Result
	plays  map[string]*fakePlay

	// MainThread, when set, is what a collection's read of a job's main
	// thread answers ([FakeRunner.ReadMainThread]); unset, it answers no
	// reading, which a Claude Code collection takes for a thread not read.
	// Set it before the first collection.
	MainThread func(ctx context.Context, box Sandbox, ask MainThreadAsk) MainThreadReading

	// TakesNotes is what [FakeRunner.ReadsNotes] answers: whether the harness
	// this fake stands in for takes a person's notes between its own steps,
	// as a real runner's harness taught to does. Set it before the first
	// collection.
	TakesNotes bool

	// CallIDKey is what [FakeRunner.CallIDMeta] answers: the `_meta` key the
	// harness this fake stands in for sends each call's own id under, as a
	// real runner of a harness that sends one declares it. "" declares none,
	// as a harness whose `_meta` carries no id does. Set it before the launch
	// that should read it.
	CallIDKey string

	// StartErr, when set, fails every Start.
	StartErr error
	// PollErr, when set, fails every Poll — the transient case the waiter
	// must retry rather than treat as completion.
	PollErr error
	// PollFunc, when set, answers every Poll in place of the queued state,
	// and is called WITHOUT the runner's lock — so one box's poll can be
	// held while another's runs, as a real runner's are, rather than every
	// poll queueing behind the fake's own mutex.
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
	// ([FakeRunner.AskEnd]) in place of the default — a script that hears it
	// and ends its job — and is called without the runner's lock. Unheard
	// makes every request answer [EndUnheard] and ask nothing: a script that
	// records no end protocol, as an older build's records none. Set either
	// before the first stop or loss.
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
	// asked about holds: what [FakeRunner.TakeSession] hands a capture,
	// and what makes this fake a [SessionTaker]. Set it before the first
	// collection.
	Session *FakeSession
	// takes is every capture asked of this runner, in order.
	takes []SessionTake

	// Carries makes this fake restore a session carried from another box
	// ([SessionRestorer]): one its own harness wrote, in [FakeArchiveFormat],
	// which every box it is asked about can take up unless CarryWhy says why
	// its harness cannot — what a runner of a harness that packs and restores
	// answers. Unset, it restores none. Set both before the first launch.
	Carries  bool
	CarryWhy string
	// carried is every archive a job was started with, read whole as a
	// runner writes it into its box.
	carried []FakeCarried

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
		ended: map[string]bool{}}
}

// fakeScriptPID is the pid a [FakeRunner]'s job's script answers an end request
// on, as a real script records its own in its box.
const fakeScriptPID = 4243

var _ JobEnder = (*FakeRunner)(nil)

// AskEnd asks the job in box to end, as a real runner does: the request sent
// through the box ([Sandbox.EndJob], which a [FakeSandbox] records), and the
// job — by default — hearing it and ending, so the next poll of its box
// reports it over and its collection reads what [FakeRunner.EndsAs] set, or
// what it would have reported as it stood. [FakeRunner.EndFunc] and
// [FakeRunner.Unheard] say otherwise; a box that answers the pid is no process
// of the job is [EndGone].
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

// EndJobIn is the job in box ending as a request to end it would leave it: over
// at its next poll, reporting what [FakeRunner.EndsAs] set — what an
// [FakeRunner.EndFunc] calls where it lets a request end the job.
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
	// ONCE, under the job's own guard rather than the runner's lock: the
	// play can take as long as its work does, and every other job's poll —
	// and every read of what was started — must not queue behind it.
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

// CallIDMeta answers [FakeRunner.CallIDKey]: the optional answer a runner whose
// harness names each of its calls gives the launch ([CallIdentifier]).
func (r *FakeRunner) CallIDMeta() string { return r.CallIDKey }

var _ CallIdentifier = (*FakeRunner)(nil)

// Install records that the agent was provisioned into the box.
func (r *FakeRunner) Install(ctx context.Context, box Sandbox) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.installed[box.ID()] = true
	return nil
}

// Start records the request and hands back a handle that stays unfinished
// until a test calls [FakeRunner.Finish].
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
	// before its harness starts: the one it continues, or one minted for it —
	// and, for a run that continues one, the session its script starts of its
	// own in that one's place. What a job's box ran is read off these and its
	// word ([PendingRun.SessionRan]), never off what its collection reports.
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
func (r *FakeRunner) Collect(ctx context.Context, box Sandbox, handle RunHandle) (Result, error) {
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
// THE ACCOUNT IS DETERMINISTIC, as a real runner's derivation is: every
// reading of the same account under the same origin reads the same text, so a
// reading begun afresh — another owner on the same build — continues where an
// earlier one stopped. [FakeRunner.Rewrite] is the other case: the same origin
// deriving different text, a build whose parser or redaction changed.
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

// Lose takes every job running now down with its engine — the jobs a host's
// restart killed mid-step: each this runner has started so far and not played,
// and each it is asked about that it never started, which a previous process
// of the engine did. A poll of one reports it [JobLost], and a collection of
// one reads partial, what it had said before it went. A job started after it
// is not lost.
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

	// Taking, where set, runs as a take reads the box — after the capture
	// read the run's log to place the session, and before the archive is
	// handed over — so a case can land a call between the two, as a call
	// answered while its box is read does.
	Taking func()

	// kept counts the digests recorded.
	kept atomic.Int32
}

// Kept is how many captures of the session recorded their digest in the box.
func (s *FakeSession) Kept() int { return int(s.kept.Load()) }

// TakeSession hands a capture the runner's [FakeRunner.Session], recording what
// it was asked ([FakeRunner.Takes]); a runner with none holds no session.
//
// A BOX THAT IS PAUSED OR RECLAIMED IS NOT READ, as a remote box paused for its
// snapshot is not, and a reclaimed one of any cell: a capture made after the
// pause or the reclaim it must come before is refused here as it would be
// there.
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
	if session == nil {
		return TakenSession{}, nil
	}
	if session.TakeErr != nil {
		return TakenSession{}, session.TakeErr
	}
	if session.Taking != nil {
		session.Taking()
	}
	var count func() *ResumedCount
	if session.Count != nil {
		count = func() *ResumedCount { counted := *session.Count; return &counted }
	}
	return TakenSession{
		Archive: session.Archive,
		Body:    io.NopCloser(bytes.NewReader(session.Body)),
		Context: func() int64 { return session.ContextTokens },
		Count:   count,
		Keep: func(context.Context) error {
			if session.KeepErr != nil {
				return session.KeepErr
			}
			session.kept.Add(1)
			return nil
		},
	}, nil
}

// Takes is every capture asked of this runner, in order.
func (r *FakeRunner) Takes() []SessionTake {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.takes)
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

// Restores answers as a runner of a harness that packs and restores does where
// [FakeRunner.Carries] is set — a session its own harness wrote, in
// [FakeArchiveFormat] — and restores nothing otherwise.
func (r *FakeRunner) Restores(archive SessionArchive) (bool, string) {
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

// CheckCarry answers [FakeRunner.Restores], and then [FakeRunner.CarryWhy] for
// the box's own harness.
func (r *FakeRunner) CheckCarry(_ context.Context, _ Sandbox, archive SessionArchive) (CarryCheck, error) {
	if ok, why := r.Restores(archive); !ok {
		return CarryCheck{Why: why}, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.CarryWhy != "" {
		return CarryCheck{Why: r.CarryWhy}, nil
	}
	return CarryCheck{Continues: true}, nil
}

// Installed reports whether the agent was installed in this box.
func (r *FakeRunner) Installed(sandboxID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.installed[sandboxID]
}

// Started is every run request handed to Start, in order — whole, the steer
// wiring a job was handed among it ([RunRequest.Steer]), so a case can play the
// harness's own side of the run against what the launch gave it.
func (r *FakeRunner) Started() []RunRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.started)
}

var _ mainThreadReader = (*FakeRunner)(nil)

// ReadMainThread answers a collection's read of a job's main thread with what
// [FakeRunner.MainThread] says, and no reading where it is unset.
func (f *FakeRunner) ReadMainThread(ctx context.Context, box Sandbox, ask MainThreadAsk) MainThreadReading {
	f.mu.Lock()
	read := f.MainThread
	f.mu.Unlock()
	if read == nil {
		return MainThreadReading{}
	}
	return read(ctx, box, ask)
}
