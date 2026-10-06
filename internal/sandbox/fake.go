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

	// ExecFunc, when set, answers Exec instead of the default success.
	ExecFunc func(ctx context.Context, cmd string, opts ExecOptions) (ExecResult, error)

	// ReadErr, when set, is consulted by every read with the file's path,
	// and a non-nil answer is that read's error — standing in for a box
	// whose file could not be read back (a transport failure, not a size).
	ReadErr func(path string) error
}

var _ Sandbox = (*FakeSandbox)(nil)

// NewFakeSandbox mints a box with the given id.
func NewFakeSandbox(id string) *FakeSandbox {
	return &FakeSandbox{id: id, home: DefaultHome, files: map[string][]byte{}}
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
// It REFUSES A FILE PAST [MaxFileBytes] exactly as every real backend does
// ([readCapped] is their rule too). It answered the whole file whatever its
// size, so every runner and coordinator test that ran through this twin
// certified a read no real box would make: a 33 MiB stdout read whole here was
// a run wedged or lost on every backend it could reach.
func (s *FakeSandbox) ReadFile(ctx context.Context, p string) ([]byte, error) {
	content, err := s.content(p)
	if err != nil || content == nil {
		return nil, err
	}
	return readCapped(bytes.NewReader(content), p)
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
	return box, nil
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

// FakeRunner is a coding agent whose job finishes when a test says so.
type FakeRunner struct {
	mu sync.Mutex

	name      string
	installed map[string]bool
	started   []RunRequest

	done   bool
	result Result

	// StartErr, when set, fails every Start.
	StartErr error
	// PollErr, when set, fails every Poll — the transient case the waiter
	// must retry rather than treat as completion.
	PollErr error
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
}

var _ Runner = (*FakeRunner)(nil)

// NewFakeRunner returns a runner under the given name.
func NewFakeRunner(name string) *FakeRunner {
	return &FakeRunner{name: name, installed: map[string]bool{}}
}

// Name is the coding agent's key in the runner registry.
func (r *FakeRunner) Name() string { return r.name }

// Install records that the agent was provisioned into the box.
func (r *FakeRunner) Install(ctx context.Context, box Sandbox) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.installed[box.ID()] = true
	return nil
}

// Start records the brief and hands back a handle that stays unfinished
// until a test calls [FakeRunner.Finish].
func (r *FakeRunner) Start(ctx context.Context, box Sandbox, req RunRequest) (RunHandle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.StartErr != nil {
		return RunHandle{}, r.StartErr
	}
	req.Env = maps.Clone(req.Env)
	r.started = append(r.started, req)
	return RunHandle{CommandID: fmt.Sprintf("cmd-%d", len(r.started)), PID: 4242}, nil
}

// Poll reports done only once [FakeRunner.Finish] has been called.
func (r *FakeRunner) Poll(ctx context.Context, box Sandbox, handle RunHandle) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.PollErr != nil {
		return false, r.PollErr
	}
	return r.done, nil
}

// Collect returns the result a test queued with [FakeRunner.Finish].
func (r *FakeRunner) Collect(ctx context.Context, box Sandbox, handle RunHandle) (Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.CollectErr != nil {
		return Result{}, r.CollectErr
	}
	if r.CollectFunc != nil {
		if err := r.CollectFunc(ctx); err != nil {
			return Result{}, err
		}
	}
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
func (r *FakeRunner) Follow(RunHandle) LiveReading { return &fakeReading{runner: r} }

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
	return LiveRead{Origin: origin, Source: source, Text: text, Finished: r.done}, nil
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

// Started is every run request handed to Start, in order.
func (r *FakeRunner) Started() []RunRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.started)
}
