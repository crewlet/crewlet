package sandbox

import (
	"context"
	"errors"
	"fmt"
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
}

var _ Sandbox = (*FakeSandbox)(nil)

// NewFakeSandbox mints a box with the given id.
func NewFakeSandbox(id string) *FakeSandbox {
	return &FakeSandbox{id: id, home: DefaultHome, files: map[string][]byte{}}
}

func (s *FakeSandbox) ID() string   { return s.id }
func (s *FakeSandbox) Home() string { return s.home }

func (s *FakeSandbox) Exec(ctx context.Context, cmd string, opts ExecOptions) (ExecResult, error) {
	if s.ExecFunc != nil {
		return s.ExecFunc(ctx, cmd, opts)
	}
	s.mu.Lock()
	s.commands = append(s.commands, cmd)
	s.mu.Unlock()
	return ExecResult{}, nil
}

func (s *FakeSandbox) StartBackground(ctx context.Context, cmd string, opts ExecOptions) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.background = append(s.background, cmd)
	return strconv.Itoa(len(s.background)), nil
}

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
func (s *FakeSandbox) ReadFile(ctx context.Context, p string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.files[path.Clean(p)]), nil
}

func (s *FakeSandbox) SetTimeout(ctx context.Context, seconds float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.timeoutCalls++
	return nil
}

func (s *FakeSandbox) Pause(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paused = true
	return nil
}

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
}

var _ Provider = (*FakeProvider)(nil)

// NewFakeProvider returns an empty provider.
func NewFakeProvider() *FakeProvider {
	return &FakeProvider{boxes: map[string]*FakeSandbox{}, Vanished: map[string]bool{}}
}

func (p *FakeProvider) Kind() string { return "fake" }

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

func (p *FakeProvider) Connect(ctx context.Context, sandboxID string) (Sandbox, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Vanished[sandboxID] {
		return nil, fmt.Errorf("sandbox %q is gone", sandboxID)
	}
	box, ok := p.boxes[sandboxID]
	if !ok {
		return nil, fmt.Errorf("sandbox %q is gone", sandboxID)
	}
	// Connect auto-resumes, matching every real backend.
	box.mu.Lock()
	box.paused = false
	box.mu.Unlock()
	return box, nil
}

func (p *FakeProvider) Kill(ctx context.Context, sandboxID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Killed = append(p.Killed, sandboxID)
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
}

var _ Runner = (*FakeRunner)(nil)

// NewFakeRunner returns a runner under the given name.
func NewFakeRunner(name string) *FakeRunner {
	return &FakeRunner{name: name, installed: map[string]bool{}}
}

func (r *FakeRunner) Name() string { return r.name }

func (r *FakeRunner) Install(ctx context.Context, box Sandbox) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.installed[box.ID()] = true
	return nil
}

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

func (r *FakeRunner) Poll(ctx context.Context, box Sandbox, handle RunHandle) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.PollErr != nil {
		return false, r.PollErr
	}
	return r.done, nil
}

func (r *FakeRunner) Collect(ctx context.Context, box Sandbox, handle RunHandle) (Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.CollectErr != nil {
		return Result{}, r.CollectErr
	}
	return r.result, nil
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
