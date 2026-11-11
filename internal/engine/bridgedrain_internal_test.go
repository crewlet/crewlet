package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/tools"
)

// recordedCalls is a bridge session's ledger that keeps every call it is
// handed, for a case that reads what the bridge recorded and when.
type recordedCalls struct {
	mu    sync.Mutex
	calls []mcpbridge.Call
}

func (l *recordedCalls) Append(_ context.Context, _ string, c mcpbridge.Call) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, c)
	return nil
}

func (l *recordedCalls) Settle(context.Context, string) bool { return false }

func (l *recordedCalls) End(context.Context, string) bool { return false }

func (l *recordedCalls) rows() []mcpbridge.Call {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]mcpbridge.Call(nil), l.calls...)
}

// closedNow reports whether ch is closed at this moment.
func closedNow(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// A DRAIN GIVES ITS SEATS BACK, THEN WAITS FOR THEIR BRIDGED CALLS. A stopping
// node releases every seat it holds, and the release ends the bridge sessions
// of the seat's executor runs, waiting for no call still running on them: the
// box is answered that its call is still running, and the seat's lease is
// given back while the call still runs. The call runs on here and writes its
// record through the store and the broker the stop's teardown closes, so the
// drain waits for it, once its seats are given back, and returns once it is
// recorded — where the release used to wait for it while holding the seat's
// lease. Read off the line the bridge's wait writes as it begins, so the order
// is the drain's own and no clock's.
//
// NOT IN PARALLEL: it reads the process's log ([captureEngineLogs]).
//
// Mutation: leave the drain's wait for the bridge's calls out, and the drain
// returns while a call of its seat's run still runs, unrecorded.
func TestADrainGivesItsSeatsBackAndThenWaitsForTheirBridgedCalls(t *testing.T) {
	logs := captureEngineLogs(t)
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	bridge := bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: server.URL,
	})
	mux := http.NewServeMux()
	mux.Handle(mcpbridge.PathPrefix+"{token}", bridge.Handler())
	server.Config.Handler = mux
	r := newDrainRig(t)
	r.bridge = bridge
	e := r.start(t)

	// A SESSION OF ONE OF THE SWE SEAT'S RUNS, as its launch opens one, with a
	// tool that holds its call until the case lets it go.
	held := newHeldTool("delegate")
	var freed sync.Once
	free := func() { freed.Do(func() { close(held.release) }) }
	// FREED FIRST of everything the case built, so no stop waits on it.
	t.Cleanup(free)
	company := e.Company()
	swe := company.Org.Role("swe")
	if swe == nil {
		t.Fatal("the rig's company has no swe seat")
	}
	reg := tools.NewRegistry()
	if err := reg.Register(held, tools.OriginBuiltin); err != nil {
		t.Fatalf("Register: %v", err)
	}
	turn := &turnctx.Turn{RunID: "t-drained", Seat: swe, Org: company.Org}
	ledger := &recordedCalls{}
	url := bridge.Open(t.Context(), &mcpbridge.Session{
		RunID: "t-drained", Launch: "job-drained", Handle: "swe", Role: swe.Name,
		Surface: tools.NewSurface("execute", reg.Snapshot(), []string{"delegate"}).ForTurn(turn),
		Ledger:  ledger,
	})
	if url == "" {
		t.Fatal("the bridge opened no session for the seat's run")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	box, err := mcp.NewClient(&mcp.Implementation{Name: "coding-agent", Version: "1"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: httpxtest.Pool(t)}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = box.Close() })

	answered := heldBoxCall(ctx, box, "delegate")
	select {
	case <-held.entered:
	case <-ctx.Done():
		t.Fatal("the call never reached its tool")
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		e.Drain(context.Background())
	}()

	// THE SEAT GOES BACK WHILE ITS RUN'S CALL IS HELD, and the box is told
	// its call is still running, the run going on.
	eventually(t, "the swe seat's lease to be given back while its run's call is held", func() bool {
		return !slices.Contains(e.node.Host().Held(), "swe")
	})
	select {
	case res := <-answered:
		if said := toolAnswerText(res); res.IsError || !strings.Contains(said, "still running") ||
			strings.Contains(said, "End your turn") {
			t.Errorf("the box's held call was answered %v %q, want it told the call is still running "+
				"and not to end its turn", res.IsError, said)
		}
	case <-ctx.Done():
		t.Fatal("the box was not answered as its seat was released")
	}

	// AND THE DRAIN WAITS FOR THE CALL, where it would have returned.
	eventually(t, "the drain to wait for the bridged call, or to return", func() bool {
		return len(logs.about("mcp_bridge_calls_draining", "component", "api.mcpbridge")) > 0 ||
			closedNow(drained)
	})
	if closedNow(drained) {
		t.Fatal("the drain returned while a call of its seat's run still ran, unrecorded")
	}
	free()
	select {
	case <-drained:
	case <-ctx.Done():
		t.Fatal("the drain did not return once the call was recorded")
	}
	if rows := ledger.rows(); len(rows) != 1 || rows[0].Name != "delegate" || !rows[0].Abandoned {
		t.Errorf("the call is recorded %+v as the drain returned, want it once, as one nobody waited for", rows)
	}
}
