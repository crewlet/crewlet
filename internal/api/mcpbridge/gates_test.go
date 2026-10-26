package mcpbridge_test

import (
	"context"
	"errors"
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

// The gates a DETACHED run's calls pass, beyond the surface's own, as only the
// bridge keeps them. The gates every dispatcher keeps — the fence, a panic, the
// run's context, a call after the run ended, the order calls arrived in, an
// activation's answer and the class of every refusal — are toolsessiontest's,
// which this package runs (contract_test.go).

// heldTool holds a call until freed, saying when it has begun.
type heldTool struct {
	name    string
	entered chan struct{}
	release chan struct{}
	freed   sync.Once
}

func newHeldTool(name string) *heldTool {
	return &heldTool{name: name, entered: make(chan struct{}, 8), release: make(chan struct{})}
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

func (h *heldTool) Call(_ context.Context, args map[string]any) (tools.Result, error) {
	h.entered <- struct{}{}
	if hold, _ := args["value"].(string); hold == "hold" {
		<-h.release
	}
	return tools.Result{Output: h.name + " done"}, nil
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

// launcher starts work that outlives its call. Served on a [parking] surface
// that names it, its result asks the loop to park with the call unanswered.
type launcher struct{ calls atomic.Int32 }

func (*launcher) Name() string               { return "launch" }
func (*launcher) Description() string        { return "starts work that outlives the call" }
func (*launcher) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (l *launcher) Call(context.Context, map[string]any) (tools.Result, error) {
	l.calls.Add(1)
	return tools.Result{Output: "started"}, nil
}

// gated is a fixture over a surface of the given tools, on a turn with a call
// log, whose session's fence answers what fenceErr holds.
type gated struct {
	*fixture
	fenceErr atomic.Pointer[error]
	url      string
}

func newGated(t *testing.T, extra ...tools.Callable) *gated {
	t.Helper()
	return newGatedParking(t, nil, extra...)
}

// newGatedParking is [newGated] whose surface parks on the tools parks names
// ([parking]).
func newGatedParking(t *testing.T, parks map[string]bool, extra ...tools.Callable) *gated {
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
	if len(parks) > 0 {
		f.session.Surface = parking{Surface: f.surface, parks: parks}
		f.session.Caller = actingSeat{f.surface}
	}
	f.bridge = bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: f.server.URL,
	})
	mux := http.NewServeMux()
	mux.Handle(mcpbridge.PathPrefix+"{token}", f.bridge.Handler())
	f.server.Config.Handler = mux
	g.url = f.bridge.Open(t.Context(), f.session)
	if g.url == "" {
		t.Fatal("no endpoint was minted")
	}
	return g
}

func (g *gated) closeFence(err error) { g.fenceErr.Store(&err) }

// A BOX IS ANSWERED AT ONCE WHEN ITS RUN'S TOOLS CLOSE. The tool loop stops the
// CLI of a run it ends and withholds every answer until it is gone; nothing
// here can stop the CLI in a box. So the box is answered at once: the call its
// tools closed under with what it did, since it ran, and every call after it
// refused, told to end its turn — never told why, which is the engine's log's
// and names the seat's grant.
//
// Mutation: answer the call the fence closed under as refused, or name the
// fence's reason in a refusal, and a case goes red.
func TestABoxIsAnsweredAtOnceWhenItsRunsToolsClose(t *testing.T) {
	t.Parallel()
	const reason = "this node no longer holds the seat"
	for _, tc := range []struct {
		name string
		// close closes the run's tools, and says what the call that closed
		// them must be answered: its own output, or a failure.
		close func(t *testing.T, g *gated, held *heldTool, sess *mcp.ClientSession)
	}{
		{"the fence closed before a call", func(t *testing.T, g *gated, _ *heldTool, sess *mcp.ClientSession) {
			g.closeFence(errors.New(reason))
			res := callTool(t, sess, "read_page", map[string]any{})
			if !res.IsError || strings.Contains(text(res), reason) {
				t.Errorf("the call on a closed fence was answered %v %q, want a refusal "+
					"that names no reason", res.IsError, text(res))
			}
		}},
		{"the fence closed during a call", func(t *testing.T, g *gated, held *heldTool, sess *mcp.ClientSession) {
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
			g.closeFence(errors.New(reason))
			held.free()
			if res := <-done; res == nil || res.IsError || text(res) != "write done" {
				t.Errorf("the call the fence closed under was answered %+v, want what it did", res)
			}
		}},
		{"a tool panicked", func(t *testing.T, _ *gated, _ *heldTool, sess *mcp.ClientSession) {
			res := callTool(t, sess, "broken", map[string]any{})
			if !res.IsError || strings.Contains(text(res), "fell over") {
				t.Errorf("the panicking call was answered %v %q, want a failure that does "+
					"not carry the panic", res.IsError, text(res))
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			held := newHeldTool("write")
			g := newGated(t, held, &panicky{})
			sess := dial(t, g.url)
			t.Cleanup(held.free)

			tc.close(t, g, held, sess)
			g.fenceErr.Store(nil)
			res := callTool(t, sess, "read_page", map[string]any{})
			if !res.IsError || !strings.Contains(text(res), "End your turn") ||
				strings.Contains(text(res), reason) {
				t.Errorf("a call once the run's tools closed was answered %v %q, want the box "+
					"told to end its turn, and not why", res.IsError, text(res))
			}
		})
	}
}

// A SUSPEND IS REFUSED, AND THE CALL THAT RAN IS RECORDED. A detached run's
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
	g := newGatedParking(t, map[string]bool{"launch": true}, launch)
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
	g := newGated(t)
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
