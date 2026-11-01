package toolsessiontest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The surface every case runs on.
const (
	// readTool answers at once.
	readTool   = "read"
	readAnswer = "read ok"

	// writeTool answers the call log's count for its call, and holds a call
	// whose value is holdValue until the case frees it.
	writeTool = "write"
	holdValue = "hold"

	// brokenTool panics with panicValue.
	brokenTool = "broken"
	panicValue = "a tool fell over"

	// activateTool offers postTool, which the surface reaches and does not
	// offer until then.
	activateTool = "activate"
	postTool     = "post"
	postAnswer   = "posted"

	// launchTool's work outlives its call: it asks the run to park.
	launchTool   = "launch"
	launchAnswer = "launched"
)

// answerWait bounds every wait for something a dispatcher owes the case — an
// answer, a record, a tool beginning. Far past anything a passing case takes,
// so it is what a failing one hits, never a slow one.
const answerWait = 10 * time.Second

// departureGrace is how long a case lets a session act on a client that walked
// away from a call before the call's tool returns. Not a bound on anything a
// passing dispatcher does — it does nothing with the departure — but time for
// one that let the departure reach the call to have done so: a cancelled
// request or a dropped connection is acted on within milliseconds.
const departureGrace = 100 * time.Millisecond

// runKey is the key of the value the run's own context carries.
type runKey struct{}

const runValue = "the run's"

// errSeatMoved is what a closed fence answers.
var errSeatMoved = errors.New("this node no longer holds the seat")

// fixture is one served run of the suite's surface, and what its tools saw.
type fixture struct {
	t      *testing.T
	served *Served

	fence atomic.Pointer[error]

	reads, posts, breaks, launches atomic.Int32

	// entered says a held write began; release lets every held write
	// return, once.
	entered chan struct{}
	release chan struct{}
	freed   sync.Once

	mu     sync.Mutex
	writes []write
}

// write is what one call of writeTool saw.
type write struct {
	value   string
	ordinal int
	// cancelled is whether its context had ended when it returned, and
	// carried the run's value off that context.
	cancelled bool
	carried   any
}

// newFixture serves the suite's surface through factory.
func newFixture(t *testing.T, factory Factory) *fixture {
	t.Helper()
	f := &fixture{t: t, entered: make(chan struct{}, 8), release: make(chan struct{})}
	spec := Spec{
		Context: context.WithValue(t.Context(), runKey{}, runValue),
		Tools: []Tool{
			{Name: readTool, Run: func(context.Context, ToolCall) string {
				f.reads.Add(1)
				return readAnswer
			}},
			{Name: writeTool, Run: f.write},
			{Name: brokenTool, Run: func(context.Context, ToolCall) string {
				f.breaks.Add(1)
				panic(panicValue)
			}},
			{Name: activateTool, Run: func(_ context.Context, call ToolCall) string {
				if !call.Activate(postTool) {
					return postTool + " is not on the surface"
				}
				return postTool + " is active"
			}},
			{Name: postTool, Run: func(context.Context, ToolCall) string {
				f.posts.Add(1)
				return postAnswer
			}},
			{Name: launchTool, Suspends: true, Run: func(context.Context, ToolCall) string {
				f.launches.Add(1)
				return launchAnswer
			}},
		},
		Offered: []string{readTool, writeTool, brokenTool, activateTool, launchTool},
		Fence: func() error {
			if err := f.fence.Load(); err != nil {
				return *err
			}
			return nil
		},
	}
	f.served = factory(t, spec)
	if f.served == nil {
		t.Fatal("toolsessiontest: the factory served no run")
	}
	// IN THIS ORDER, and after the factory's own teardown, so they run before
	// it: the held calls are let go, then the run ends — a run ends only once
	// the calls running on it have returned.
	t.Cleanup(func() { f.served.End(t) })
	t.Cleanup(f.free)
	return f
}

// write is writeTool.
func (f *fixture) write(ctx context.Context, call ToolCall) string {
	value, _ := call.Args["value"].(string)
	if value == holdValue {
		f.entered <- struct{}{}
		<-f.release
	}
	f.mu.Lock()
	f.writes = append(f.writes, write{value: value, ordinal: call.Ordinal,
		cancelled: ctx.Err() != nil, carried: ctx.Value(runKey{})})
	f.mu.Unlock()
	return strconv.Itoa(call.Ordinal)
}

// written is every write that returned.
func (f *fixture) written() []write {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.writes)
}

// held waits until a held write has begun.
func (f *fixture) held() {
	f.t.Helper()
	select {
	case <-f.entered:
	case <-time.After(answerWait):
		f.t.Fatalf("the held %s never began", writeTool)
	}
}

// free lets every held write return.
func (f *fixture) free() { f.freed.Do(func() { close(f.release) }) }

func (f *fixture) closeFence() { err := errSeatMoved; f.fence.Store(&err) }
func (f *fixture) openFence()  { f.fence.Store(nil) }

// dial connects a client the way a harness's own does: it lists the tools
// before it calls one, and re-lists when told the list changed — relistDelay
// after the notice, as a harness that re-reads it a moment later.
func (f *fixture) dial(relistDelay time.Duration) *mcp.ClientSession {
	f.t.Helper()
	cs, _ := f.connect(relistDelay)
	return cs
}

// connect is [fixture.dial], and the way to hang up under it — see
// [Served.Dial].
func (f *fixture) connect(relistDelay time.Duration) (*mcp.ClientSession, func()) {
	t := f.t
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "toolsessiontest", Version: "1"},
		&mcp.ClientOptions{
			ToolListChangedHandler: func(ctx context.Context, req *mcp.ToolListChangedRequest) {
				time.Sleep(relistDelay)
				_, _ = req.Session.ListTools(ctx, nil)
			},
		})
	transport, hangUp := f.served.Dial(t)
	cs, err := client.Connect(t.Context(), transport, nil)
	if err != nil {
		t.Fatalf("connect to the run's session: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	if _, err := cs.ListTools(t.Context(), nil); err != nil {
		t.Fatalf("list the run's tools: %v", err)
	}
	return cs, hangUp
}

// call makes one call and waits for its answer, as a harness does on a run that
// goes on.
func (f *fixture) call(cs *mcp.ClientSession, name string, args any) *mcp.CallToolResult {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(f.t.Context(), answerWait)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		f.t.Fatalf("call %s: %v", name, err)
	}
	return res
}

// pending is a call whose answer the case does not wait for — see the package
// doc, "what a case asserts once a run is ending".
type pending struct {
	done chan struct{}
	res  *mcp.CallToolResult
	err  error
}

// send makes one call without waiting for its answer.
func (f *fixture) send(cs *mcp.ClientSession, name string, args any) *pending {
	p := &pending{done: make(chan struct{})}
	go func() {
		defer close(p.done)
		p.res, p.err = cs.CallTool(f.t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	}()
	return p
}

// answer is what reached the client for p: an answer, or the error its call
// failed with. Collected once the run has ended, by when every answer a
// dispatcher held has gone.
func (p *pending) answer(t *testing.T) (*mcp.CallToolResult, error) {
	t.Helper()
	p.over(t)
	return p.res, p.err
}

// over waits until p's call has come back, whatever reached the client: once
// its run has ended, no dispatcher still holds its answer.
func (p *pending) over(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(answerWait):
		t.Fatal("a call was never answered, though its run has ended")
	}
}

// recorded waits until the dispatcher's record holds n calls, and returns them
// in the order the calls reached it.
func (f *fixture) recorded(n int) []Recorded {
	f.t.Helper()
	deadline := time.Now().Add(answerWait)
	for {
		got := f.served.Recorded(f.t)
		if len(got) >= n || time.Now().After(deadline) {
			slices.SortFunc(got, func(a, b Recorded) int { return a.Seq - b.Seq })
			if len(got) != n {
				f.t.Fatalf("the dispatcher's record holds %d calls, want %d: %s",
					len(got), n, describe(got))
			}
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// describe renders a record for a failure message.
func describe(calls []Recorded) string {
	if len(calls) == 0 {
		return "none"
	}
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, fmt.Sprintf("#%d %s failed=%v class=%q %q",
			c.Seq, c.Name, c.Failed, string(c.Class), c.Output))
	}
	return strings.Join(out, "; ")
}

// text is a result's text.
func text(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// listed is every tool the session lists now.
func (f *fixture) listed(cs *mcp.ClientSession) []string {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(f.t.Context(), answerWait)
	defer cancel()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		f.t.Fatalf("list the run's tools: %v", err)
	}
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// shaped reports whether got is the shape's answer for name, for some reason
// given: what it says before the reason and after it, around a reason that is
// not empty. The reason is the session's own words for what it could not
// read, which the suite does not restate.
func shaped(got string, shape func(name, why string) string, name string) bool {
	const marker = "\x00why\x00"
	before, after, ok := strings.Cut(shape(name, marker), marker)
	if !ok {
		return got == shape(name, "")
	}
	return len(got) > len(before)+len(after) &&
		strings.HasPrefix(got, before) && strings.HasSuffix(got, after)
}
