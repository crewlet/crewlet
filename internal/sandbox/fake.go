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
	"sync"
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
	// TimeoutCalls counts SetTimeout calls — the waiter's keepalive.
	// Exported so a test can assert the heartbeat actually happened.
	mu           sync.Mutex
	id           string
	home         string
	files        map[string][]byte
	timeoutCalls int
	paused       bool
	closed       bool
	commands     []string
	background   []string
	// readCap is the whole-read cap a suite gave the box, zero for
	// [MaxFileBytes]: see [FakeSandbox.CapReads].
	readCap int

	// ExecFunc, when set, answers Exec instead of the default success.
	ExecFunc func(ctx context.Context, cmd string, opts ExecOptions) (ExecResult, error)

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

// Exec records the command and returns whatever the test queued for it,
// or an empty success.
func (s *FakeSandbox) Exec(ctx context.Context, cmd string, opts ExecOptions) (ExecResult, error) {
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
	return strconv.Itoa(len(s.background)), nil
}

// JobRunning answers through [FakeSandbox.Exec], the way a remote box does,
// so a test scripts liveness with ExecFunc.
func (s *FakeSandbox) JobRunning(ctx context.Context, commandID string) (bool, error) {
	return probeByKill(ctx, s, commandID)
}

// WriteFile stores the content in memory, where [FakeSandbox.Put] and
// ReadFile can see it.
func (s *FakeSandbox) WriteFile(ctx context.Context, p string, content []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("sandbox: box is closed")
	}
	s.files[path.Clean(p)] = slices.Clone(content)
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
	return readCapped(bytes.NewReader(content), p, s.ReadCap())
}

// OpenFile streams the file whole, as every real backend does: a machine
// stream is never refused for its size.
func (s *FakeSandbox) OpenFile(ctx context.Context, p string) (io.ReadCloser, error) {
	content, err := s.content(p)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(content)), nil
}

// ReadTail answers the file's last n bytes and its whole size.
func (s *FakeSandbox) ReadTail(ctx context.Context, p string, n int) (FileTail, error) {
	content, err := s.content(p)
	if err != nil {
		return FileTail{}, err
	}
	return tailOf(content, n), nil
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

// SetTimeout is accepted and ignored: nothing in a fake box can outrun a
// deadline.
func (s *FakeSandbox) SetTimeout(ctx context.Context, seconds float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.timeoutCalls++
	return nil
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

// Keepalives is how many times the waiter refreshed this box's TTL.
func (s *FakeSandbox) Keepalives() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.timeoutCalls
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
	defer p.mu.Unlock()
	p.Killed = append(p.Killed, sandboxID)
	if p.KillErr != nil {
		return p.KillErr
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

// FakeRunner is a coding agent whose job finishes when a test says so — or,
// with a [FakeRunner.Player], whose every job is played by that function.
type FakeRunner struct {
	mu sync.Mutex

	name      string
	installed map[string]bool
	started   []RunRequest

	done   bool
	result Result

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

	// TakesNotes is what [FakeRunner.ReadsNotes] answers: whether the CLI
	// this fake stands in for takes a person's notes between its own steps,
	// as a real runner's CLI taught to does. Set it before the first
	// collection.
	TakesNotes bool

	// StartErr, when set, fails every Start.
	StartErr error
	// PollErr, when set, fails every Poll — the transient case the waiter
	// must retry rather than treat as completion.
	PollErr error
	// PollFunc, when set, answers every Poll in place of the queued state,
	// and is called WITHOUT the runner's lock — so one box's poll can be
	// held while another's runs, as a real runner's are, rather than every
	// poll queueing behind the fake's own mutex.
	PollFunc func(ctx context.Context, box Sandbox) (bool, error)
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
}

// NewFakeRunner returns a runner under the given name.
func NewFakeRunner(name string) *FakeRunner {
	return &FakeRunner{name: name, installed: map[string]bool{}, plays: map[string]*fakePlay{}}
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
	p.once.Do(func() { p.result = player(ctx, p.req) })
	return p.result, true
}

// Name is the coding agent's key in the runner registry.
func (r *FakeRunner) Name() string { return r.name }

// ReadsNotes answers [FakeRunner.TakesNotes]: the optional answer a runner
// whose CLI takes a person's notes between its steps gives the coordinator.
func (r *FakeRunner) ReadsNotes() bool { return r.TakesNotes }

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
	r.started = append(r.started, req)
	handle := RunHandle{CommandID: fmt.Sprintf("cmd-%d", len(r.started)), PID: 4242}
	if r.Player != nil {
		r.plays[handle.CommandID] = &fakePlay{req: req}
	}
	return handle, nil
}

// Poll reports done once [FakeRunner.Finish] has been called — or, under a
// [FakeRunner.Player], once the job has been played, which this poll does.
func (r *FakeRunner) Poll(ctx context.Context, box Sandbox, handle RunHandle) (bool, error) {
	r.mu.Lock()
	poll := r.PollFunc
	r.mu.Unlock()
	if poll != nil {
		return poll(ctx, box)
	}
	if _, played := r.play(ctx, handle); played {
		return true, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.PollErr != nil {
		return false, r.PollErr
	}
	return r.done, nil
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

// Installed reports whether the agent was installed in this box.
func (r *FakeRunner) Installed(sandboxID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.installed[sandboxID]
}

// Started is every run request handed to Start, in order — whole, the steer
// wiring a job was handed among it ([RunRequest.Steer]), so a case can play the
// CLI's own side of the run against what the launch gave it.
func (r *FakeRunner) Started() []RunRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.started)
}
