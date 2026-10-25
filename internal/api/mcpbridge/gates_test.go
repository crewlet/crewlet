package mcpbridge_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/toolsession"
)

// The gates a DETACHED run's calls pass, beyond the surface's own: the run
// outlives the turn that launched it, and its calls arrive on HTTP requests a
// box can drop, several at once, for as long as the run lasts.

// heldTool blocks a call until released, saying when it has begun, and
// reports what its call saw: the repeat count the run's call log gives it,
// whether its context was cancelled, and a value off that context.
type heldTool struct {
	name    string
	entered chan struct{}
	release chan struct{}
	hold    bool
	freed   sync.Once

	mu        sync.Mutex
	cancelled []bool
	values    []any
}

type ctxKey struct{}

func newHeldTool(name string, hold bool) *heldTool {
	return &heldTool{name: name, hold: hold,
		entered: make(chan struct{}, 8), release: make(chan struct{})}
}

// free lets every held call return, once. A case registers it AFTER the server
// and the client it dials, so a case that fails with a call still held does
// not leave the server's close waiting on it for ever.
func (h *heldTool) free() { h.freed.Do(func() { close(h.release) }) }

func (h *heldTool) Name() string        { return h.name }
func (h *heldTool) Description() string { return h.name + " does a thing" }
func (h *heldTool) Parameters() map[string]any {
	return map[string]any{"type": "object",
		"properties": map[string]any{"value": map[string]any{"type": "string"}}}
}

func (h *heldTool) Call(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{Output: "unreachable: a seat tool is called for its turn", Failed: true}, nil
}

func (h *heldTool) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {
	h.entered <- struct{}{}
	if hold, _ := args["value"].(string); hold == "hold" {
		<-h.release
	}
	h.mu.Lock()
	h.cancelled = append(h.cancelled, ctx.Err() != nil)
	h.values = append(h.values, ctx.Value(ctxKey{}))
	h.mu.Unlock()
	return tools.Result{Output: fmt.Sprint(turn.CallLog().Ordinal(h.name, args))}, nil
}

func (h *heldTool) seen() ([]bool, []any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]bool(nil), h.cancelled...), append([]any(nil), h.values...)
}

// panicky is a tool with a defect.
type panicky struct{ calls atomic.Int32 }

func (*panicky) Name() string               { return "broken" }
func (*panicky) Description() string        { return "falls over" }
func (*panicky) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (p *panicky) Call(context.Context, map[string]any) (tools.Result, error) {
	p.calls.Add(1)
	panic("a tool fell over")
}

// launcher starts work that outlives its call, and asks the loop to park with
// the call unanswered — run_sandbox's shape.
type launcher struct{ calls atomic.Int32 }

func (*launcher) Name() string               { return "launch" }
func (*launcher) Description() string        { return "starts work that outlives the call" }
func (*launcher) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (*launcher) Call(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{Output: "unreachable: a detached tool is called for its turn", Failed: true,
		Cause: tools.RefusalInternalError}, nil
}

func (l *launcher) CallDetached(context.Context, *turnctx.Turn, map[string]any) (tools.DetachedResult, error) {
	l.calls.Add(1)
	return tools.DetachedResult{Result: tools.Result{Output: "started"}, Suspend: true}, nil
}

// gated is a fixture over a surface of the given tools, on a turn with a call
// log, whose session's fence answers what fenceErr holds.
type gated struct {
	*fixture
	fenceErr atomic.Pointer[error]
	url      string
}

func newGated(t *testing.T, ctx context.Context, extra ...tools.Callable) *gated {
	t.Helper()
	f := newFixture(t)
	reg := tools.NewRegistry()
	names := []string{"read_page"}
	if err := reg.Register(f.byName["read_page"], tools.OriginBuiltin); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for _, tool := range extra {
		if err := reg.Register(tool, tools.OriginBuiltin); err != nil {
			t.Fatalf("Register(%s): %v", tool.Name(), err)
		}
		names = append(names, tool.Name())
	}
	turn := seatTurn()
	turn.Calls = turnctx.NewCallLog()
	f.surface = tools.NewSurface("execute", reg.Snapshot(), names).ForTurn(turn)
	g := &gated{fixture: f}
	f.session = &mcpbridge.Session{
		RunID: "run-1", Launch: "job-1", Handle: "dev", Role: "Engineer",
		Surface: f.surface, Ledger: f.ledger,
		Fence: func() error {
			if err := g.fenceErr.Load(); err != nil {
				return *err
			}
			return nil
		},
	}
	f.bridge = bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: f.server.URL,
	})
	mux := http.NewServeMux()
	mux.Handle(mcpbridge.PathPrefix+"{token}", f.bridge.Handler())
	f.server.Config.Handler = mux
	g.url = f.bridge.Open(ctx, f.session)
	if g.url == "" {
		t.Fatal("no endpoint was minted")
	}
	return g
}

func (g *gated) closeFence(err error) { g.fenceErr.Store(&err) }

// A SEAT THIS NODE NO LONGER SPEAKS FOR CALLS NOTHING. The run outlives the
// turn that launched it, and its seat can move to a peer, or a person can ask
// it to stop, while it runs: from the first call that finds the fence closed,
// that call and every later one are answered without running — whatever the
// fence says afterwards, because a seat that moved and came back is a new
// grant this run was never admitted under.
//
// Mutation: drop the fence before a call, and the closed fence runs the tool.
func TestABridgedCallIsFencedOnTheSeat(t *testing.T) {
	t.Parallel()
	g := newGated(t, t.Context())
	sess := dial(t, g.url)
	if res := callTool(t, sess, "read_page", map[string]any{}); res.IsError {
		t.Fatalf("a call on an open fence failed: %s", text(res))
	}

	g.closeFence(errors.New("this node no longer holds the seat"))
	res := callTool(t, sess, "read_page", map[string]any{})
	if !res.IsError {
		t.Fatalf("a call ran on a closed fence: %s", text(res))
	}
	if n := len(g.byName["read_page"].seen()); n != 1 {
		t.Fatalf("the tool ran %d times, want only the call before the fence closed", n)
	}
	if strings.Contains(text(res), "no longer holds") {
		t.Errorf("the answer names the engine's reason, which is the log's: %q", text(res))
	}

	g.fenceErr.Store(nil)
	if res := callTool(t, sess, "read_page", map[string]any{}); !res.IsError {
		t.Fatal("a call ran once the fence reopened, on a run admitted under the old grant")
	}
	// Every call is on the run's record, the two after the fence closed
	// refused there as not running.
	rows := g.ledger.rows()
	if len(rows) != 3 || rows[0].Failed {
		t.Fatalf("%d rows recorded, want the call that ran and the two refused: %+v", len(rows), rows)
	}
	for _, row := range rows[1:] {
		if !row.Failed || row.Refusal() != tools.RefusalNotRunning {
			t.Errorf("a refused call is recorded %+v, want it failed as not running", row)
		}
	}
}

// A FENCE THAT CLOSES WHILE A CALL RUNS lets that call's answer through — the
// tool ran, and what it did is done — and closes the run's tools: the next
// call is refused even though the fence it would ask has opened again.
//
// Mutation: drop the fence after a call, and the next call runs on the
// reopened fence.
func TestAFenceThatClosesMidCallStopsTheNextOne(t *testing.T) {
	t.Parallel()
	held := newHeldTool("write", false)
	g := newGated(t, t.Context(), held)
	sess := dial(t, g.url)
	t.Cleanup(held.free)

	done := make(chan *mcp.CallToolResult, 1)
	go func() {
		res, err := sess.CallTool(t.Context(), &mcp.CallToolParams{
			Name: "write", Arguments: map[string]any{"value": "hold"}})
		if err != nil {
			t.Errorf("the held call: %v", err)
		}
		done <- res
	}()
	<-held.entered
	g.closeFence(errors.New("a person asked the seat to stop"))
	held.free()
	if res := <-done; res == nil || res.IsError {
		t.Fatal("the call the fence closed under was not answered with what it did")
	}

	g.fenceErr.Store(nil)
	if res := callTool(t, sess, "write", map[string]any{"value": "next"}); !res.IsError {
		t.Fatal("a call ran after the fence closed under the run")
	}
	if cancelled, _ := held.seen(); len(cancelled) != 1 {
		t.Errorf("the tool ran %d times, want the one call before the fence closed", len(cancelled))
	}
}

// A CALL IS THE RUN'S, NOT THE REQUEST'S. A box that gives up on a call —
// its client's own timeout, a dropped connection — cancels nothing a tool is
// half-way through writing: the tool runs to its end under the run's launch
// context, reads that context's values, and its call is recorded.
//
// Mutation: run the call under the request's context, and the tool sees it
// cancelled and the launch's value missing.
func TestABoxThatHangsUpAbandonsTheAnswerNotTheCall(t *testing.T) {
	t.Parallel()
	held := newHeldTool("write", false)
	launch := context.WithValue(t.Context(), ctxKey{}, "the run's")
	g := newGated(t, launch, held)
	sess := dial(t, g.url)
	t.Cleanup(held.free)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := sess.CallTool(ctx, &mcp.CallToolParams{
			Name: "write", Arguments: map[string]any{"value": "hold"}})
		done <- err
	}()
	<-held.entered
	cancel()
	if err := <-done; err == nil {
		t.Fatal("the client's cancelled request was answered")
	}
	held.free()

	deadline := time.Now().Add(5 * time.Second)
	for len(g.ledger.rows()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancelled, values := held.seen()
	if len(cancelled) != 1 || cancelled[0] {
		t.Fatalf("the tool's context was cancelled with the request (%v)", cancelled)
	}
	if values[0] != "the run's" {
		t.Errorf("the tool ran under a context without the run's values: %v", values[0])
	}
	if rows := g.ledger.rows(); len(rows) != 1 || rows[0].Name != "write" {
		t.Errorf("the abandoned call was not recorded: %+v", rows)
	}
}

// CALLS MADE AT ONCE COUNT IN THE ORDER THEY ARRIVED. A CLI makes a
// response's calls together and they finish in whatever order their tools
// take; the repeat count an operation id carries is read against the calls
// ADMITTED before, so the second call counts the first although the first has
// not returned — and a re-run, making the same calls, derives the same ids.
//
// Mutation: record a call in the run's log when it returns, and the second
// call counts nothing before it.
func TestCallsMadeAtOnceCountInTheOrderTheyArrived(t *testing.T) {
	t.Parallel()
	held := newHeldTool("write", false)
	g := newGated(t, t.Context(), held)
	sess := dial(t, g.url)
	t.Cleanup(held.free)

	first := make(chan *mcp.CallToolResult, 1)
	go func() {
		res, err := sess.CallTool(t.Context(), &mcp.CallToolParams{
			Name: "write", Arguments: map[string]any{"value": "hold"}})
		if err != nil {
			t.Errorf("first call: %v", err)
		}
		first <- res
	}()
	<-held.entered
	second := callTool(t, sess, "write", map[string]any{"value": "other"})
	if got := text(second); got != "1" {
		t.Fatalf("the second call counts %s different calls before it, want the "+
			"first, admitted before it and still running", got)
	}
	held.free()
	if got := text(<-first); got != "0" {
		t.Errorf("the first call counts %s, want 0: it saw a call admitted after it", got)
	}
	rows := g.ledger.rows()
	if len(rows) != 2 {
		t.Fatalf("%d rows, want 2", len(rows))
	}
	// The rows land in the order the calls finished; each carries the
	// order they were made in.
	seqs := map[string]int{}
	for _, row := range rows {
		seqs[row.Args["value"].(string)] = row.Seq
	}
	if seqs["hold"] != 1 || seqs["other"] != 2 {
		t.Errorf("the rows' admission order is %v, want hold 1 and other 2", seqs)
	}
}

// A PANIC IS CONTAINED: the call is answered failed, the process stays up,
// the stack goes to the log, the call is recorded, and the run's tools close —
// a defect does not run again on the run's next call.
//
// Mutation: drop the recover, and the test binary dies.
func TestAToolThatPanicsClosesTheRunsToolsAndNotTheProcess(t *testing.T) {
	t.Parallel()
	broken := &panicky{}
	g := newGated(t, t.Context(), broken)
	sess := dial(t, g.url)

	res := callTool(t, sess, "broken", map[string]any{})
	if !res.IsError || strings.Contains(text(res), "fell over") {
		t.Fatalf("a panicking call answered %v %q, want a failure that does not "+
			"carry the panic", res.IsError, text(res))
	}
	if again := callTool(t, sess, "read_page", map[string]any{}); !again.IsError {
		t.Fatal("the run's tools stayed open after a tool panicked")
	}
	if n := broken.calls.Load(); n != 1 {
		t.Errorf("the broken tool ran %d times", n)
	}
	rows := g.ledger.rows()
	if len(rows) != 2 || !rows[0].Failed || rows[0].Name != "broken" {
		t.Fatalf("the broken call is not recorded as failed: %+v", rows)
	}
	if rows[1].Name != "read_page" || rows[1].Refusal() != tools.RefusalNotRunning {
		t.Errorf("the call after the panic is recorded %+v, want it refused as not running", rows[1])
	}
}

// A CALL THAT REACHES A RUN AFTER IT ENDED IS NOT RUN. The route's gate stops
// a new request once the run is closed; one already past it when the run
// ended reaches the dispatcher, which refuses it.
func TestACallThatArrivesAfterItsRunEndedIsNotRun(t *testing.T) {
	t.Parallel()
	g := newGated(t, t.Context())
	opened := mcpbridge.Dispatcher(g.session)
	g.bridge.Close("run-1")
	res := opened.Call(toolsession.Call{Name: "read_page", Arguments: map[string]any{}})
	if !res.Failed {
		t.Fatalf("a call on an ended run ran: %q", res.Output)
	}
	if n := len(g.byName["read_page"].seen()); n != 0 {
		t.Errorf("the tool ran %d times on an ended run", n)
	}
}

// A SUSPEND IS REFUSED, AND THE CALL THAT RAN IS RECORDED. A box-mode run's
// loop is its CLI's own, which would wait for ever on a call parked unanswered,
// so the box is told at once the tool cannot be used from a coding run — and
// the run goes on. But the tool ran, and started what it would have parked on,
// so the run's record holds the call as it holds any call that ran: when, for
// how long, who answered, and refused as forbidden.
//
// Mutation: answer the suspend without recording it, and the ledger is empty.
func TestASuspendIsRefusedAndRecordedAsACallThatRan(t *testing.T) {
	t.Parallel()
	launch := &launcher{}
	g := newGated(t, t.Context(), launch)
	sess := dial(t, g.url)
	res := callTool(t, sess, "launch", map[string]any{})
	if !res.IsError || !strings.Contains(text(res), "cannot be used from a coding run") {
		t.Fatalf("a suspend was answered %v %q, want the refusal", res.IsError, text(res))
	}
	rows := g.ledger.rows()
	if len(rows) != 1 {
		t.Fatalf("%d rows, want the call that ran", len(rows))
	}
	if r := rows[0]; r.Name != "launch" || !r.Failed || r.Refusal() != tools.RefusalForbidden ||
		r.StartedAt.IsZero() || r.Origin != string(tools.OriginBuiltin) {
		t.Errorf("the refused suspend is recorded %+v, want a timed call that ran, refused "+
			"as forbidden", r)
	}
	if again := callTool(t, sess, "read_page", map[string]any{}); again.IsError {
		t.Errorf("the run's next call failed after a refused suspend: %q", text(again))
	}
	if n := launch.calls.Load(); n != 1 {
		t.Errorf("the tool ran %d times", n)
	}
}

// EVERY RECORDED CALL SAYS WHEN IT RAN, FOR HOW LONG AND WHO ANSWERED, and a
// call whose arguments did not parse is recorded unrun, untimed and failed.
func TestARecordedCallCarriesItsTimingAndWhoAnswered(t *testing.T) {
	t.Parallel()
	g := newGated(t, t.Context())
	before := time.Now()
	callTool(t, dial(t, g.url), "read_page", map[string]any{"text": "one"})
	opened := mcpbridge.Dispatcher(g.session)
	res := opened.Call(toolsession.Call{Name: "read_page",
		ArgumentsError: "unexpected end of JSON input"})
	if !res.Failed || !strings.Contains(res.Output, "NOT run") {
		t.Fatalf("a call whose arguments did not parse was answered %q", res.Output)
	}

	rows := g.ledger.rows()
	if len(rows) != 2 {
		t.Fatalf("%d rows, want 2", len(rows))
	}
	ran := rows[0]
	if ran.Seq != 1 || ran.StartedAt.Before(before.Truncate(time.Second)) ||
		ran.StartedAt.Location() != time.UTC || ran.Duration <= 0 ||
		ran.Origin != string(tools.OriginBuiltin) {
		t.Errorf("the call that ran is recorded %+v, want its order, start, "+
			"duration and origin", ran)
	}
	unrun := rows[1]
	if unrun.Seq != 2 || !unrun.Failed || !unrun.StartedAt.IsZero() || unrun.Duration != 0 {
		t.Errorf("the call that did not run is recorded %+v, want it failed and untimed", unrun)
	}
	if n := len(g.byName["read_page"].seen()); n != 1 {
		t.Errorf("the tool ran %d times, want only the call whose arguments parsed", n)
	}
}

// TWO RUNS ARE TWO SESSIONS, whatever they share: closing one leaves the
// other's box connected and its token working.
func TestClosingOneRunLeavesAnotherServing(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	first := f.open(t)
	other := &mcpbridge.Session{RunID: "run-2", Launch: "job-2", Handle: "dev", Role: "Engineer",
		Surface: f.surface, Ledger: f.ledger}
	second := f.bridge.Open(t.Context(), other)
	keep := dial(t, second)
	dial(t, first)

	f.bridge.Close("run-1")
	if f.bridge.Live() != 1 {
		t.Fatalf("%d sessions live after closing one of two", f.bridge.Live())
	}
	if res := callTool(t, keep, "read_page", map[string]any{}); res.IsError {
		t.Errorf("closing one run ended another's: %s", text(res))
	}
	if other.Connections() == 0 {
		t.Error("the other run's box lost its connection")
	}
}
