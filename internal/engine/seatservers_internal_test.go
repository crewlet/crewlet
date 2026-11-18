package engine

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/mcp"
)

// heldSeatChildren registers a seat's children the way a claim does, with held
// standing in for the seat host's answer, and stops them when the case ends.
func heldSeatChildren(t *testing.T, e *Engine, handle string, held func() bool) *seatChildren {
	t.Helper()
	s := e.newSeatChildren(t.Context(), handle)
	s.held = held
	e.mcpMu.Lock()
	if e.seatMCP == nil {
		e.seatMCP = map[string]*seatChildren{}
	}
	e.seatMCP[handle] = s
	e.mcpMu.Unlock()
	t.Cleanup(func() { e.stopSeatServers(t.Context(), handle) })
	return s
}

// seatOutcomes is what this node reports for one seat's children.
func seatOutcomes(e *Engine, handle string) []mcpOutcome {
	e.mcpMu.Lock()
	defer e.mcpMu.Unlock()
	return e.mcpSeats[handle]
}

// hangingCompany is a company whose one per-role server is a process that
// never answers the handshake, so every start of it lasts its whole startup
// timeout, given in seconds.
func hangingCompany(t *testing.T, startupTimeoutSeconds int) *Company {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(fmt.Sprintf(`
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
mcp_servers:
  - name: tracker
    shared: false
    command: "sleep"
    args: ["60"]
    startup_timeout_seconds: %d
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    mcp_env:
      tracker:
        SEAT_TOKEN: "ceo-token"
`, startupTimeoutSeconds)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	company, err := NewCompany(cfg)
	if err != nil {
		t.Fatalf("NewCompany: %v", err)
	}
	return company
}

// A PASS STARTS NO CHILD FOR A SEAT THIS NODE CANNOT PROVE IT HOLDS.
//
// A child is the seat's identity at the vendor, and an apply's or a retry's
// pass may land after the lease lapsed, before the heartbeat noticed. Such a
// pass starts nothing; it is retried, and starts the child once the lease is
// provable again.
func TestAPassStartsNoChildForASeatThisNodeCannotProve(t *testing.T) {
	t.Parallel()
	e := &Engine{mcp: mcp.NewBridge(nil, mcp.Supervision{})}
	e.epoch.current.Store(mcpStatusCompany(t, false))
	var provable atomic.Bool
	s := heldSeatChildren(t, e, "ceo", provable.Load)

	e.convergeSeat(t.Context(), s, true)
	if got := seatOutcomes(e, "ceo"); len(got) != 0 {
		t.Fatalf("a seat this node cannot prove it holds had a start attempted: %+v", got)
	}
	s.mu.Lock()
	armed := s.retry != nil
	s.mu.Unlock()
	if !armed {
		t.Fatal("a pass refused for want of the lease was not retried, so the seat " +
			"would keep no child even once its lease was provable again")
	}

	provable.Store(true)
	e.convergeSeat(t.Context(), s, true)
	got := seatOutcomes(e, "ceo")
	if len(got) != 1 || got[0].server != "tracker" || got[0].err == nil {
		t.Fatalf("outcomes once the lease is provable = %+v, want the tracker's "+
			"start attempted (and failed, its binary being absent)", got)
	}
}

// AN APPLY HANDS A SEAT'S PASS TO ITS RUNNER AND DOES NOT WAIT FOR IT, or
// every later apply stage would wait behind a vendor handshake. Here the pass
// blocks in its first step for ten seconds; the apply must return at once and
// leave a runner going, which then files what the pass concluded.
func TestAnApplyDoesNotWaitForASeatsPass(t *testing.T) {
	t.Parallel()
	e := &Engine{mcp: mcp.NewBridge(nil, mcp.Supervision{})}
	e.epoch.current.Store(mcpStatusCompany(t, false))
	unblock := make(chan struct{})
	heldSeatChildren(t, e, "ceo", func() bool {
		select {
		case <-unblock:
		case <-time.After(10 * time.Second):
		}
		return true
	})
	let := sync.OnceFunc(func() { close(unblock) })
	// Registered after the seat's own cleanup, so it runs first and the
	// release is not left waiting on a pass held here.
	t.Cleanup(let)

	begun := time.Now()
	e.reconcileSeatServers(t.Context())
	if took := time.Since(begun); took > 2*time.Second {
		t.Fatalf("the apply's seat stage took %v: it waited for the seat's pass, "+
			"and every stage after it with it", took)
	}
	if got := seatOutcomes(e, "ceo"); len(got) != 0 {
		t.Fatalf("precondition: the pass concluded while it was still blocked: %+v", got)
	}

	let()
	SettleSeatServers(e)
	got := seatOutcomes(e, "ceo")
	if len(got) != 1 || got[0].server != "tracker" {
		t.Errorf("outcomes once the runner finished = %+v, want the tracker's start "+
			"attempted: the apply asked for a pass and nothing ran it", got)
	}
}

// AN APPLY THAT FINDS A SEAT BEFORE ITS CLAIM'S FIRST PASS IS FOLDED INTO THE
// CLAIM, which returns only once that pass has run.
//
// The claim is the seat's runner from before it registers the seat, so an
// apply landing before its first pass can only ask it to go again, and the
// mailbox never opens on children still starting. The pass here takes a
// second, so a claim that returned while another goroutine ran it would find
// nothing concluded.
func TestAnApplyBeforeTheClaimsFirstPassIsFoldedIntoIt(t *testing.T) {
	t.Parallel()
	e := &Engine{mcp: mcp.NewBridge(nil, mcp.Supervision{})}
	e.epoch.current.Store(mcpStatusCompany(t, false))
	var passes atomic.Int32

	// The claim, split where an apply can land: registered first...
	s := e.claimSeatChildren(t.Context(), "ceo")
	t.Cleanup(func() { e.stopSeatServers(t.Context(), "ceo") })
	s.held = func() bool {
		if passes.Add(1) == 1 {
			time.Sleep(time.Second)
		}
		return true
	}
	// ...then the apply, which must leave the pass to the claim: a pass it
	// started would be under way well within the wait below...
	e.reconcileSeatServers(t.Context())
	time.Sleep(100 * time.Millisecond)
	if n := passes.Load(); n != 0 {
		t.Fatalf("the apply ran %d pass(es) over a seat its claim had registered and "+
			"not yet started: it made itself the seat's runner", n)
	}
	// ...then the rest of the claim.
	e.runSeatPasses(t.Context(), s)

	if got := seatOutcomes(e, "ceo"); len(got) != 1 || got[0].server != "tracker" {
		t.Fatalf("the claim returned with outcomes %+v: its seat's children had not "+
			"converged, and the mailbox would open on them", got)
	}
	SettleSeatServers(e)
	if n := passes.Load(); n != 1 {
		t.Errorf("%d passes ran for one claim and one apply before it: the apply's "+
			"request was not folded into the claim's pass", n)
	}
}

// AND THE SAME THROUGH THE WHOLE CLAIM, in its widest window: the claim has
// registered the seat and is retiring a PREDECESSOR (a failed acquire's state,
// its pass stuck) when the apply lands. The seat's own pass is a start lasting
// its two-second timeout, so a claim that let the apply run it would return
// seconds before anything was concluded.
func TestAnApplyWhileTheClaimRetiresAPredecessorIsFoldedIntoIt(t *testing.T) {
	t.Parallel()
	e := &Engine{mcp: mcp.NewBridge(nil, mcp.Supervision{})}
	e.epoch.current.Store(hangingCompany(t, 2))

	stuck, unstick := make(chan struct{}), make(chan struct{})
	predecessor := heldSeatChildren(t, e, "ceo", func() bool {
		close(stuck)
		<-unstick
		return false
	})
	go e.convergeSeat(context.WithoutCancel(t.Context()), predecessor, false)
	<-stuck

	claimed := make(chan struct{})
	go func() {
		defer close(claimed)
		e.startSeatServers(t.Context(), "ceo")
	}()
	for registered := false; !registered; time.Sleep(time.Millisecond) {
		e.mcpMu.Lock()
		registered = e.seatMCP["ceo"] != predecessor
		e.mcpMu.Unlock()
	}

	e.reconcileSeatServers(t.Context())
	close(unstick)
	select {
	case <-claimed:
	case <-time.After(30 * time.Second):
		t.Fatal("the claim has not returned in 30s")
	}
	got := seatOutcomes(e, "ceo")
	if len(got) != 1 || got[0].server != "tracker" || got[0].err == nil {
		t.Fatalf("the claim returned with outcomes %+v, want the tracker's start "+
			"concluded: the apply ran the seat's pass and the claim, finding it "+
			"running, returned with the mailbox about to open on a start in flight", got)
	}
}

// A RELEASED SEAT RUNS NO PASS AGAIN, armed retry or not.
//
// Here the retry is due every few milliseconds, so a timer that survived the
// release would run a pass, which starts children, within the wait below.
func TestAReleasedSeatRunsNoPassAgain(t *testing.T) {
	t.Parallel()
	e := &Engine{mcp: mcp.NewBridge(nil, mcp.Supervision{}), mcpRetryBase: time.Millisecond}
	e.epoch.current.Store(mcpStatusCompany(t, false))
	var passes atomic.Int32
	s := heldSeatChildren(t, e, "ceo", func() bool {
		passes.Add(1)
		return true
	})

	e.convergeSeat(t.Context(), s, false)
	for deadline := time.Now().Add(10 * time.Second); passes.Load() < 3; {
		if time.Now().After(deadline) {
			t.Fatalf("the failing child was retried %d times in 10s, want the "+
				"retry running so the release has something to stop", passes.Load()-1)
		}
		time.Sleep(time.Millisecond)
	}

	e.stopSeatServers(t.Context(), "ceo")
	after := passes.Load()
	time.Sleep(100 * time.Millisecond)
	e.convergeSeat(t.Context(), s, true)
	if got := passes.Load(); got != after {
		t.Errorf("%d passes ran after the release", got-after)
	}
	if got := seatOutcomes(e, "ceo"); got != nil {
		t.Errorf("a released seat still reports outcomes: %+v", got)
	}
}

// A RELEASE ENDS A START IN FLIGHT rather than waiting it out.
//
// A start that never hears back waits its whole startup timeout, so the
// release cancels the pass: handing the seat on costs a child's teardown, not
// its timeout. The child here never answers the handshake, given a minute to.
func TestAReleaseEndsAStartInFlight(t *testing.T) {
	t.Parallel()
	company := hangingCompany(t, 60)
	e := &Engine{mcp: mcp.NewBridge(nil, mcp.Supervision{})}
	e.epoch.current.Store(company)
	s := heldSeatChildren(t, e, "ceo", func() bool { return true })

	done := make(chan struct{})
	go func() {
		defer close(done)
		// An apply's pass: nothing but the release can end it early.
		e.convergeSeat(context.WithoutCancel(t.Context()), s, true)
	}()
	for {
		s.mu.Lock()
		running := s.running
		s.mu.Unlock()
		if running {
			break
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // the child spawned, its handshake unanswered

	begun := time.Now()
	e.stopSeatServers(t.Context(), "ceo")
	if took := time.Since(begun); took > 15*time.Second {
		t.Errorf("the release took %v: it waited out the start in flight instead "+
			"of ending it", took)
	}
	<-done
}

// A SPEC THAT CAN NEVER LAUNCH IS NOT RETRIED: only a revision changes it, and
// its apply runs a pass of its own. A start that fails for a reason that can
// pass (here an absent binary, which an image update can supply) is.
func TestOnlyAFailureThatCanPassIsRetried(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	s := heldSeatChildren(t, e, "ceo", func() bool { return true })

	invalid := mcp.Spec{Name: mcp.InstanceName("tracker", "CEO")}
	if outcomes, retry := bringUp(t.Context(), s, []mcp.Spec{invalid}, false); retry ||
		len(outcomes) != 1 || outcomes[0].err == nil {
		t.Errorf("a spec with no command: outcomes %+v, retry %v; want one failure "+
			"and no retry", outcomes, retry)
	}
	absent := mcp.Spec{
		Name:    mcp.InstanceName("tracker", "CEO"),
		Command: "/nonexistent/definitely-not-a-tracker-server",
	}
	if _, retry := bringUp(t.Context(), s, []mcp.Spec{absent}, false); !retry {
		t.Error("a start that failed for a reason that can pass was not retried")
	}
}

// A SEAT'S RETRY IS THE MCP RESTART SCHEDULE ([mcp.RestartWait], whose own
// suite holds it), moved by the engine's one knob: the base doubling up to
// twenty bases, each wait spread by a fifth either way. A second copy here
// would drift from it, and an unbounded doubling overflows into a negative wait
// that reads as "due now", every tick.
func TestASeatRetryWaitsOnTheMCPRestartSchedule(t *testing.T) {
	t.Parallel()
	within := func(e *Engine, failures int, want time.Duration) {
		t.Helper()
		low, high := want*4/5, want*6/5
		if got := e.seatRetryDelay(failures); got < low || got > high {
			t.Errorf("retry after %d failures waits %v, want %v ± a fifth", failures, got, want)
		}
	}
	defaults := &Engine{}
	within(defaults, 1, mcp.RestartBase)
	within(defaults, 2, 2*mcp.RestartBase)
	within(defaults, 3, 4*mcp.RestartBase)
	for _, failures := range []int{6, 20, 1 << 20} {
		within(defaults, failures, mcp.RestartCeiling)
	}
	scaled := &Engine{mcpRetryBase: time.Second}
	within(scaled, 1, time.Second)
	within(scaled, 1<<20, 20*time.Second)
}
