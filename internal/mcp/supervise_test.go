package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
)

// The bridge's supervision, against real child processes that die.
//
// Every case here describes a failure that used to have no end: a child that
// crashed after a successful start stayed "running" in the bridge, its tools
// stayed in every catalogue, and each call failed on a broken pipe until
// something restarted it; a shared server whose start failed waited for the
// next config apply. A fake transport cannot reproduce either — the property
// is what the engine notices about a PROCESS — so the helper server dies for
// real, and counts its own starts in a file the test reads.

// supervisedBridge is a bridge with sup, its servers stopped at cleanup, and
// its log recorded.
func supervisedBridge(t *testing.T, sup Supervision) (*Bridge, *logRecorder) {
	t.Helper()
	log, rec := recorder()
	b := NewBridge(log, sup)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = b.StopAll(ctx)
	})
	return b, rec
}

// countedSpec is a helper server that records each start in a file of its
// own, returned beside it.
func countedSpec(t *testing.T, name, mode string, env map[string]string) (Spec, string) {
	t.Helper()
	starts := filepath.Join(t.TempDir(), "starts")
	full := map[string]string{helperStartsEnv: starts}
	for k, v := range env {
		full[k] = v
	}
	return helperSpec(t, name, mode, full), starts
}

// eventually polls cond until it holds or the deadline passes. The deadline
// is long because a restart is a real spawn and handshake under the race
// detector; a case that is fast when it works pays nothing for it.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func serving(b *Bridge, name string) bool {
	h, ok := b.Health(name)
	return ok && h.Serving
}

func down(b *Bridge, name string) bool {
	h, ok := b.Health(name)
	return ok && !h.Serving
}

// A CHILD THAT DIES IS RESTARTED, AND THE TOOL A TURN ALREADY HOLDS WORKS
// AGAIN. The bridge has to notice the death on its own — nothing calls the
// server between its death and the next turn — and the restarted child has to
// be reachable through the SAME Tool object, because that object is what every
// registry built before the death holds.
func TestAChildThatDiesIsRestartedAndTheToolItServedWorksAgain(t *testing.T) {
	t.Parallel()
	b, rec := supervisedBridge(t, Supervision{})
	spec, starts := countedSpec(t, "crashy", "serve", map[string]string{helperExitAfterEnv: "1"})
	change, err := b.Add(t.Context(), spec)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	tool := change.Added[0]

	// The one call it answers, before it exits on the spot. Its reply has to
	// arrive although the process is gone by the time it is read.
	res, err := tool.Call(t.Context(), map[string]any{"q": "first"})
	if err != nil || res.Failed || !strings.Contains(res.Output, "Result of search") {
		t.Fatalf("the call before the crash = %+v, %v", res, err)
	}

	// Restarted without anybody asking: a healthy child's first death
	// restarts at once.
	eventually(t, "the restarted child", func() bool {
		return startsIn(starts) == 2 && serving(b, "crashy")
	})
	if !rec.hasAttr("server_exited", "cause", "its process exited (exit status "+
		strconv.Itoa(helperCrashCode)+")") {
		t.Errorf("the death was not reported with the child's exit status: %+v",
			rec.find("server_exited"))
	}
	if !rec.has("server_restarted") {
		t.Error("the restart was not reported")
	}

	again, err := tool.Call(t.Context(), map[string]any{"q": "second"})
	if err != nil || again.Failed || !strings.Contains(again.Output, "Result of search") {
		t.Fatalf("the tool held from before the crash = %+v, %v: it still points at the dead child",
			again, err)
	}
	if live, _ := b.Tool("search"); live != tool {
		t.Error("the restart replaced a tool whose listing did not change, so every " +
			"registry holding it would have to be refiled for nothing")
	}
}

// A DEATH AFTER A HEALTHY RUN BEGINS A NEW SERIES; ONE SOON AFTER A RESTART
// CONTINUES THE OLD ONE. A child that stayed up for the schedule's ceiling
// was healthy, so its death is restarted at once, as its first was; one that
// dies again moments after being restarted is a server crashing on something,
// and its restarts back off. Read off the attempt number each restart logs,
// which is the schedule's own count, rather than off a wall clock.
func TestAHealthyRunStartsANewRestartSeriesAndAQuickDeathDoesNot(t *testing.T) {
	t.Parallel()
	const base = 50 * time.Millisecond
	stable := Supervision{RetryBase: base}.ceiling()
	b, rec := supervisedBridge(t, Supervision{RetryBase: base})
	spec, starts := countedSpec(t, "flaky", "serve", map[string]string{helperExitAfterEnv: "1"})
	change, err := b.Add(t.Context(), spec)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	tool := change.Added[0]
	restarts := func() []recorded { return rec.find("server_restarted") }
	crash := func(want int) {
		t.Helper()
		if res, err := tool.Call(t.Context(), nil); err != nil || res.Failed {
			t.Fatalf("the call before crash %d = %+v, %v", want, res, err)
		}
		eventually(t, "restart "+strconv.Itoa(want), func() bool {
			return len(restarts()) == want && serving(b, "flaky")
		})
	}

	crash(1) // the first death of a fresh server
	time.Sleep(stable + stable/2)
	crash(2) // after a healthy run
	crash(3) // at once, after a restart

	got := make([]any, 0, 3)
	for _, r := range restarts() {
		got = append(got, r.Attrs["attempt"])
	}
	want := []any{int64(1), int64(1), int64(2)}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("restart attempts = %v, want %v: a death after a healthy run "+
				"must begin a new series, and one straight after a restart continue it",
				got, want)
		}
	}
	if n := startsIn(starts); n != 4 {
		t.Errorf("started %d times, want the first and three restarts", n)
	}
}

// A SERVER THAT CAME UP ON A RETRY HAS ITS FIRST DEATH RESTARTED AT ONCE, as
// one that came up on its first start does. The retries that brought it up
// were a series of attempts to START it, and that series ended when it came
// up; carried into its first death, a shared server whose vendor was down at
// boot — the case its retries exist for — would wait the schedule's next step
// after a crash that any other server comes straight back from. Read off the
// attempt number each restart logs, as the case above is.
func TestAServerThatCameUpOnARetryHasItsFirstDeathRestartedAtOnce(t *testing.T) {
	t.Parallel()
	b, rec := supervisedBridge(t, Supervision{
		RetryFailedStarts: true,
		RetryBase:         50 * time.Millisecond,
	})
	spec, starts := countedSpec(t, "late-then-crashy", "serve", map[string]string{
		helperFailStartsEnv: "1",
		helperExitAfterEnv:  "1",
	})
	spec.StartupTimeout = 5 * time.Second
	if _, err := b.Add(t.Context(), spec); err == nil {
		t.Fatal("a start that exits at once was reported started")
	}
	eventually(t, "the retried start", func() bool { return serving(b, "late-then-crashy") })
	tool, ok := b.Tool("search")
	if !ok {
		t.Fatal("the retried start's tool is not in the catalogue")
	}
	if res, err := tool.Call(t.Context(), nil); err != nil || res.Failed {
		t.Fatalf("the call before the crash = %+v, %v", res, err)
	}
	eventually(t, "the restart after the crash", func() bool {
		return len(rec.find("server_restarted")) == 2 && serving(b, "late-then-crashy")
	})

	restarts := rec.find("server_restarted")
	if got := restarts[1].Attrs["attempt"]; got != int64(1) {
		t.Fatalf("the first death of a server that came up on a retry was restarted as "+
			"attempt %v of a series, want 1: it waited for the schedule (all restarts: %v)",
			got, restarts)
	}
	if n := startsIn(starts); n != 3 {
		t.Errorf("started %d times, want the failed start, its retry and one restart", n)
	}
}

// A HEALTHY CHILD IS LEFT ALONE: the control for the restart cases above. A supervisor
// that restarted on anything but a session that ended would pass it and tear
// down working servers.
func TestAHealthyChildIsNotRestarted(t *testing.T) {
	t.Parallel()
	b, rec := supervisedBridge(t, Supervision{RetryBase: 10 * time.Millisecond})
	spec, starts := countedSpec(t, "steady", "serve", nil)
	change, err := b.Add(t.Context(), spec)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	for i := range 5 {
		res, err := change.Added[0].Call(t.Context(), map[string]any{"i": i})
		if err != nil || res.Failed {
			t.Fatalf("call %d = %+v, %v", i, res, err)
		}
	}
	// Long enough for several attempts at the schedule's base.
	time.Sleep(200 * time.Millisecond)
	if n := startsIn(starts); n != 1 {
		t.Fatalf("a healthy child was started %d times", n)
	}
	if rec.has("server_exited") || rec.has("server_restarted") {
		t.Fatal("a healthy child was reported dead or restarted")
	}
}

// A SERVER THAT IS DOWN IS REFUSED HONESTLY, and NOTHING STARTS IT WHILE ITS
// OWNER SAYS NO. The owner's gate is what keeps a per-role child — one seat's
// identity at the vendor — from being restarted by a node whose lease has
// lapsed; while it is shut, the server stays down and every call says so in
// the engine's words, classed unavailable, rather than as a broken pipe.
func TestADownServerIsRefusedHonestlyAndHeldBackByItsOwner(t *testing.T) {
	t.Parallel()
	var mayStart atomic.Bool
	b, rec := supervisedBridge(t, Supervision{
		MayStart:  mayStart.Load,
		RetryBase: 20 * time.Millisecond,
	})
	spec, starts := countedSpec(t, "seat-child", "serve", map[string]string{helperExitAfterEnv: "1"})
	change, err := b.Add(t.Context(), spec)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	tool := change.Added[0]
	if res, err := tool.Call(t.Context(), nil); err != nil || res.Failed {
		t.Fatalf("the call before the crash = %+v, %v", res, err)
	}
	eventually(t, "the start to be held back", func() bool {
		return rec.has("server_restart_held_back")
	})

	res, err := tool.Call(t.Context(), nil)
	if err != nil {
		t.Fatalf("a call to a down server must not be a Go error: %v", err)
	}
	if !res.Failed || res.Refusal != RefusalUnavailable || res.Unknown {
		t.Fatalf("refusal = %+v, want a failed call classed unavailable, sent nowhere", res)
	}
	for _, want := range []string{"not running", "exit status " + strconv.Itoa(helperCrashCode),
		"restarts it on its own"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("refusal %q does not say %q", res.Output, want)
		}
	}
	for _, leak := range []string{"broken pipe", "EOF", "closed"} {
		if strings.Contains(res.Output, leak) {
			t.Errorf("refusal %q reports the transport's %q instead of the server's state",
				res.Output, leak)
		}
	}
	if b.Has("seat-child") {
		t.Error("Has says yes for a server that is down")
	}
	if h, _ := b.Health("seat-child"); !strings.Contains(h.Down, "held back") {
		t.Errorf("health = %+v, want the hold named", h)
	}
	// The engine's own path to a server — no catalogue, no model — is told
	// the same thing as a sentinel it can branch on.
	if _, err := b.Call(t.Context(), "seat-child", "search", nil); !errors.Is(err, ErrNotRunning) {
		t.Errorf("Call on a down server = %v, want ErrNotRunning", err)
	}
	if n := startsIn(starts); n != 1 {
		t.Fatalf("the child was started %d times while its owner said no", n)
	}

	mayStart.Store(true)
	eventually(t, "the restart once its owner allows it", func() bool { return serving(b, "seat-child") })
	if res, err := tool.Call(t.Context(), nil); err != nil || res.Failed {
		t.Fatalf("the call after the restart = %+v, %v", res, err)
	}
}

// A CALL THE SERVER DIES UNDER WAS SENT AND NOBODY KNOWS WHAT IT DID. It is
// refused as unavailable like any call to a down server, and — unless the tool
// is known to be a read — marked as possibly landed, so a caller looks before
// it makes a write twice.
func TestACallTheServerDiesUnderIsRefusedAsPossiblyLanded(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		annotations string
		unknown     bool
	}{
		{"a write", "", true},
		{"a read", `{"readOnlyHint":true}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, _ := supervisedBridge(t, Supervision{MayStart: func() bool { return false }})
			spec, _ := countedSpec(t, "dies", "serve", map[string]string{
				helperCallEnv:  "die",
				helperToolsEnv: toolsJSON([3]string{"search", "Search", tc.annotations}),
			})
			change, err := b.Add(t.Context(), spec)
			if err != nil {
				t.Fatalf("Add: %v", err)
			}
			res, err := change.Added[0].Call(t.Context(), nil)
			if err != nil {
				t.Fatalf("a call the server died under must not be a Go error: %v", err)
			}
			if !res.Failed || res.Refusal != RefusalUnavailable {
				t.Fatalf("result = %+v, want a refusal classed unavailable", res)
			}
			if res.Unknown != tc.unknown {
				t.Errorf("unknown = %v, want %v: %q", res.Unknown, tc.unknown, res.Output)
			}
			if !strings.Contains(res.Output, "went down during this call") {
				t.Errorf("refusal %q does not say the server died under the call", res.Output)
			}
			if tc.unknown && !strings.Contains(res.Output, "check before making it again") {
				t.Errorf("refusal %q does not tell the caller to look before repeating it",
					res.Output)
			}
		})
	}
}

// A CALL IN FLIGHT WHEN ITS OWNER STOPS THE SERVER WAS SENT TOO. A revision
// that removed the server, or a seat let go under a stop budget, ends the
// session under the call exactly as a death does, and the answer must say what
// a death's says: no answer came, and unless the tool is a read, whether it
// took effect is unknown. Told the tool "cannot be called" — the answer to a
// call that never left the engine — a model reads the write as never made and
// makes it again.
func TestACallInFlightWhenItsServerIsStoppedIsRefusedAsPossiblyLanded(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		annotations string
		unknown     bool
	}{
		{"a write", "", true},
		{"a read", `{"readOnlyHint":true}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, _ := supervisedBridge(t, Supervision{})
			seen := filepath.Join(t.TempDir(), "call-seen")
			spec, _ := countedSpec(t, "stopping", "serve", map[string]string{
				helperCallEnv:     "hang",
				helperCallSeenEnv: seen,
				helperToolsEnv:    toolsJSON([3]string{"search", "Search", tc.annotations}),
			})
			change, err := b.Add(t.Context(), spec)
			if err != nil {
				t.Fatalf("Add: %v", err)
			}
			type answer struct {
				res Result
				err error
			}
			answered := make(chan answer, 1)
			go func() {
				res, err := change.Added[0].Call(t.Context(), nil)
				answered <- answer{res, err}
			}()
			eventually(t, "the call to reach the server", func() bool {
				_, err := os.Stat(seen)
				return err == nil
			})

			// BOUNDED, as a release's stop is: the session's own close waits
			// for the call in flight, so it is the budget running out — and
			// the tree killed under it — that ends the session under the call.
			stopCtx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
			defer cancel()
			_, _ = b.Stop(stopCtx, "stopping")

			var got answer
			select {
			case got = <-answered:
			case <-time.After(30 * time.Second):
				t.Fatal("the call in flight never returned after its server was stopped")
			}
			if got.err != nil {
				t.Fatalf("a call its server was stopped under must not be a Go error: %v", got.err)
			}
			res := got.res
			if !res.Failed || res.Refusal != RefusalUnavailable {
				t.Fatalf("result = %+v, want a refusal classed unavailable", res)
			}
			if res.Unknown != tc.unknown {
				t.Errorf("unknown = %v, want %v: %q", res.Unknown, tc.unknown, res.Output)
			}
			if strings.Contains(res.Output, "cannot be called") || !strings.Contains(res.Output, "got no answer") {
				t.Errorf("refusal %q tells a call that was sent as one that never was", res.Output)
			}
			if !strings.Contains(res.Output, "stopped during this call") {
				t.Errorf("refusal %q does not say the server was stopped, not crashed", res.Output)
			}
			if tc.unknown && !strings.Contains(res.Output, "check before making it again") {
				t.Errorf("refusal %q does not tell the caller to look before repeating it",
					res.Output)
			}
		})
	}
}

// A FAILED START IS RETRIED BY A BRIDGE THAT KEEPS IT — the shared servers',
// whose only other retry is the next config apply. The start still fails to
// its caller, and the bridge does not claim to serve it; then it comes up on
// the schedule, and the owner is told its catalogue grew.
func TestAFailedStartIsRetriedByABridgeThatKeepsIt(t *testing.T) {
	t.Parallel()
	changed := make(chan struct{}, 4)
	b, rec := supervisedBridge(t, Supervision{
		RetryFailedStarts: true,
		RetryBase:         20 * time.Millisecond,
		CatalogueChanged:  func(context.Context) { changed <- struct{}{} },
	})
	spec, starts := countedSpec(t, "late-vendor", "serve", map[string]string{helperFailStartsEnv: "2"})
	spec.StartupTimeout = 5 * time.Second
	if _, err := b.Add(t.Context(), spec); err == nil {
		t.Fatal("a start that exits at once was reported started")
	}
	if b.Has("late-vendor") {
		t.Fatal("Has says yes for a server whose start failed")
	}
	if _, ok := b.Tool("search"); ok {
		t.Fatal("a failed start contributed a tool")
	}
	if h, ok := b.Health("late-vendor"); !ok || h.Serving || !strings.Contains(h.Down, "would not start") {
		t.Fatalf("health = %+v, %v: the bridge must keep the server, down, with why", h, ok)
	}
	if !rec.has("server_start_retry_scheduled") {
		t.Error("the retry was not announced")
	}

	select {
	case <-changed:
	case <-time.After(30 * time.Second):
		t.Fatal("the retried start never came up")
	}
	if !b.Has("late-vendor") {
		t.Fatal("the owner was told the catalogue changed before the server served it")
	}
	if _, ok := b.Tool("search"); !ok {
		t.Fatal("the retried start's tools are not in the catalogue")
	}
	// Two refused starts, the second of them a retry that failed, then one
	// that came up: retried until it worked, not once.
	if n := startsIn(starts); n != 3 {
		t.Errorf("the server was started %d times, want 3", n)
	}
	if !rec.has("server_restart_failed") {
		t.Error("the retry that failed was not reported")
	}
}

// AND A BRIDGE THAT DOES NOT KEEP A FAILED START DOES NOT RETRY IT: its owner
// does, and two retry loops over one child would race each other to start it.
func TestABridgeThatLeavesFailedStartsToItsOwnerRetriesNothing(t *testing.T) {
	t.Parallel()
	b, _ := supervisedBridge(t, Supervision{RetryBase: 10 * time.Millisecond})
	spec, starts := countedSpec(t, "owner-retries", "serve", map[string]string{helperFailStartsEnv: "1"})
	spec.StartupTimeout = 5 * time.Second
	if _, err := b.Add(t.Context(), spec); err == nil {
		t.Fatal("a start that exits at once was reported started")
	}
	time.Sleep(200 * time.Millisecond)
	if n := startsIn(starts); n != 1 {
		t.Fatalf("the bridge started the server %d times, want only its owner's one", n)
	}
	if servers := b.Servers(); len(servers) != 0 {
		t.Fatalf("the bridge kept %v of a start its owner retries", servers)
	}
}

// AN OWNER'S RECONCILE OF A DOWN SERVER ASKS FOR AN ATTEMPT NOW, AND STILL
// NAMES ITS TOOLS. Whoever reconciles — a config apply — has a reason to
// believe now is different, and a retry minutes off is the wait the operator
// re-applied to skip. The reconcile does not wait for the attempt and says the
// server is down; and it hands back the tools the server still lists, the same
// objects, because a fresh registry that left them out would list a down
// server's tools in another order from the same revision applied while it was
// up, or not at all.
func TestAReconcileOfADownServerAsksForAnAttemptNowAndNamesItsTools(t *testing.T) {
	t.Parallel()
	// The DEFAULT schedule, its restart held back until the case allows it:
	// the next attempt on its own is fifteen seconds away, so a server that
	// comes up well inside the deadline below came up because it was asked.
	var mayStart atomic.Bool
	b, rec := supervisedBridge(t, Supervision{MayStart: mayStart.Load})
	spec, starts := countedSpec(t, "nudged", "serve", map[string]string{helperExitAfterEnv: "1"})
	added, err := b.Add(t.Context(), spec)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	tool := added.Added[0]
	if res, err := tool.Call(t.Context(), nil); err != nil || res.Failed {
		t.Fatalf("the call before the crash = %+v, %v", res, err)
	}
	eventually(t, "the restart to be held back", func() bool {
		return rec.has("server_restart_held_back")
	})
	mayStart.Store(true)

	change, err := b.Reconcile(t.Context(), spec)
	if !errors.Is(err, ErrNotRunning) {
		t.Fatalf("reconciling a down server returned %v, want ErrNotRunning", err)
	}
	if len(change.Added) != 1 || change.Added[0] != tool {
		t.Fatalf("reconciling a down server named %v, want the tool it still lists", addedNames(change))
	}
	deadline := time.Now().Add(RestartBase / 2)
	for !serving(b, "nudged") {
		if time.Now().After(deadline) {
			t.Fatalf("not up %s after the reconcile asked (%d starts): it waited for its "+
				"schedule instead", RestartBase/2, startsIn(starts))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A START THAT FAILED IS ASKED AGAIN BY A RECONCILE TOO, on a bridge that keeps
// it — and names no tools, since it never listed any.
func TestAReconcileOfAFailedStartAsksForAnAttemptNow(t *testing.T) {
	t.Parallel()
	b, _ := supervisedBridge(t, Supervision{RetryFailedStarts: true})
	spec, starts := countedSpec(t, "late", "serve", map[string]string{helperFailStartsEnv: "1"})
	spec.StartupTimeout = 5 * time.Second
	if _, err := b.Add(t.Context(), spec); err == nil {
		t.Fatal("a start that exits at once was reported started")
	}

	change, err := b.Reconcile(t.Context(), spec)
	if !errors.Is(err, ErrNotRunning) {
		t.Fatalf("reconciling a failed start returned %v, want ErrNotRunning", err)
	}
	if len(change.Added) != 0 {
		t.Fatalf("a server that never started reported tools: %v", addedNames(change))
	}
	deadline := time.Now().Add(RestartBase / 2)
	for !serving(b, "late") {
		if time.Now().After(deadline) {
			t.Fatalf("not up %s after the reconcile asked (%d starts): it waited for its "+
				"schedule instead", RestartBase/2, startsIn(starts))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// RETRYNOW IS THE SAME REQUEST WITHOUT A RECONCILE, for an owner that leaves a
// down server to its supervisor rather than reconcile it — a seat's runner,
// whose pass must not start a second copy of a child its bridge is already
// bringing back. It asks a down server for an attempt now, leaves a serving
// one alone, and lifts no hold its owner has put on starting it.
func TestRetryNowAsksADownServerForAnAttemptAndNothingElse(t *testing.T) {
	t.Parallel()
	var mayStart atomic.Bool
	b, rec := supervisedBridge(t, Supervision{MayStart: mayStart.Load})
	spec, starts := countedSpec(t, "seat-child", "serve", map[string]string{helperExitAfterEnv: "1"})
	added, err := b.Add(t.Context(), spec)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if b.RetryNow("seat-child") {
		t.Fatal("RetryNow reported a serving server as down")
	}
	if b.RetryNow("nobody") {
		t.Fatal("RetryNow reported a server the bridge does not run as down")
	}
	if res, err := added.Added[0].Call(t.Context(), nil); err != nil || res.Failed {
		t.Fatalf("the call before the crash = %+v, %v", res, err)
	}
	eventually(t, "the restart to be held back", func() bool {
		return rec.has("server_restart_held_back")
	})
	if !b.Runs(spec) {
		t.Fatal("a down server reads as one its bridge does not run, so its owner would " +
			"start a second copy beside the supervisor bringing it back")
	}

	if !b.RetryNow("seat-child") {
		t.Fatal("RetryNow did not report a down server as down")
	}
	// Held back still: asked again at once, and refused again.
	eventually(t, "the asked-for attempt to be held back too", func() bool {
		return len(rec.find("server_restart_held_back")) >= 2
	})
	if n := startsIn(starts); n != 1 {
		t.Fatalf("RetryNow started the server %d times past its owner's hold", n-1)
	}

	mayStart.Store(true)
	b.RetryNow("seat-child")
	deadline := time.Now().Add(RestartBase / 2)
	for !serving(b, "seat-child") {
		if time.Now().After(deadline) {
			t.Fatalf("not up %s after RetryNow: it waited for its schedule instead", RestartBase/2)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// THE RESTART SCHEDULE: base, doubling, the ceiling twenty bases, each wait
// spread by a fifth either way, and the whole of it scaled by a base other
// than the default. Every retry of a server that will not come back waits
// this, whoever makes it, so a schedule that drifted would retry a vendor in
// lockstep or hammer it.
func TestRestartWaitDoublesFromItsBaseToTwentyBasesSpreadByAFifth(t *testing.T) {
	t.Parallel()
	within := func(n int, base, want time.Duration) {
		t.Helper()
		low := time.Duration(float64(want) * (1 - restartJitter))
		high := time.Duration(float64(want) * (1 + restartJitter))
		for range 200 {
			if got := RestartWait(n, base); got < low || got > high {
				t.Fatalf("RestartWait(%d, %s) = %s, want within %s..%s", n, base, got, low, high)
			}
		}
	}
	within(1, 0, RestartBase)
	within(2, 0, 2*RestartBase)
	within(3, 0, 4*RestartBase)
	for _, n := range []int{6, 7, 40} {
		within(n, 0, RestartCeiling)
	}
	within(1, time.Second, time.Second)
	within(40, time.Second, 20*time.Second)

	spread := map[time.Duration]struct{}{}
	for range 50 {
		spread[RestartWait(1, 0)] = struct{}{}
	}
	if len(spread) < 2 {
		t.Fatal("every wait is the same: a fleet's servers would retry one vendor in lockstep")
	}
}

// STOP ENDS THE SUPERVISION, and nothing is started behind it. A retry that
// was due when its server was stopped would otherwise bring back a server a
// revision removed — or a seat's child after its release.
func TestStopEndsSupervisionAndNothingStartsAfterIt(t *testing.T) {
	t.Parallel()
	b, _ := supervisedBridge(t, Supervision{
		RetryFailedStarts: true,
		RetryBase:         20 * time.Millisecond,
	})
	spec, starts := countedSpec(t, "never", "serve", map[string]string{helperFailStartsEnv: "1000"})
	spec.StartupTimeout = 5 * time.Second
	if _, err := b.Add(t.Context(), spec); err == nil {
		t.Fatal("a start that exits at once was reported started")
	}
	eventually(t, "a retry", func() bool { return startsIn(starts) >= 2 })

	if _, err := b.Stop(t.Context(), "never"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	stoppedAt := startsIn(starts)
	time.Sleep(300 * time.Millisecond)
	if n := startsIn(starts); n != stoppedAt {
		t.Fatalf("%d starts after Stop returned", n-stoppedAt)
	}
	if servers := b.Servers(); len(servers) != 0 {
		t.Fatalf("Stop left %v supervised", servers)
	}
}

// A RESTART THAT LISTS DIFFERENT TOOLS IS A CHANGE, and only that kind is:
// the owner is told so it can refile the registries built from the old
// listing, and the catalogue holds the new tools rather than the old ones.
func TestARestartThatListsDifferentToolsChangesTheCatalogue(t *testing.T) {
	t.Parallel()
	changed := make(chan struct{}, 4)
	b, _ := supervisedBridge(t, Supervision{CatalogueChanged: func(context.Context) { changed <- struct{}{} }})
	spec, _ := countedSpec(t, "upgraded", "serve", map[string]string{
		helperExitAfterEnv: "1",
		helperToolsEnv:     toolsJSON([3]string{"search_" + startToken, "Search", ""}),
	})
	change, err := b.Add(t.Context(), spec)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := addedNames(change); len(got) != 1 || got[0] != "search_1" {
		t.Fatalf("first listing = %v", got)
	}
	if res, err := change.Added[0].Call(t.Context(), nil); err != nil || res.Failed {
		t.Fatalf("the call before the crash = %+v, %v", res, err)
	}
	select {
	case <-changed:
	case <-time.After(30 * time.Second):
		t.Fatal("a restart that listed different tools never reported a change")
	}
	if _, ok := b.Tool("search_2"); !ok {
		t.Error("the restarted server's new tool is not in the catalogue")
	}
	if _, ok := b.Tool("search_1"); ok {
		t.Error("the tool the restarted server no longer lists is still in the catalogue")
	}
}

// THE CHILD'S OWN EXIT IS WHAT IS WATCHED, not only the pipe. A package runner
// whose server process inherits its stdout leaves that pipe open when the
// runner exits, so a supervisor that waited for the session to end would wait
// for ever while every call hung; the engine waits for the process it
// started, and reaps the tree it leaves behind before starting another.
func TestALauncherThatExitsIsNoticedThoughItsChildHoldsTheOutput(t *testing.T) {
	t.Parallel()
	b, rec := supervisedBridge(t, Supervision{MayStart: func() bool { return false }})
	spec, _ := countedSpec(t, "runner", "launcher", map[string]string{helperExitAfterEnv: "1"})
	change, err := b.Add(t.Context(), spec)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if res, err := change.Added[0].Call(t.Context(), nil); err != nil || res.Failed {
		t.Fatalf("the call before the runner exits = %+v, %v", res, err)
	}
	eventually(t, "the runner's exit to be noticed", func() bool { return down(b, "runner") })
	eventually(t, "the death to be reported", func() bool { return rec.has("server_exited") })
	if !rec.hasAttr("server_exited", "cause", "its process exited (exit status "+
		strconv.Itoa(helperCrashCode)+")") {
		t.Errorf("the runner's exit was not what was reported: %+v", rec.find("server_exited"))
	}
	// The grandchild held the descriptors, so clearing the dead server away
	// had to signal its group — and it must be gone before a replacement
	// could start beside it.
	assertProcessGone(t, waitForGrandchildPIDIn(t, rec))
}

// A SESSION THAT ENDS WHILE ITS PROCESS LIVES ON IS NOTICED TOO: the server
// closed its output, and no answer can come back however long the process
// runs. That is the only end an HTTP server's session has — there is no
// process to wait for — so the transport's own read is watched as well as
// the child, and the server is cleared away and restarted.
func TestASessionThatEndsWhileItsProcessLivesIsNoticed(t *testing.T) {
	t.Parallel()
	b, rec := supervisedBridge(t, Supervision{})
	spec, starts := countedSpec(t, "hangs-up", "serve", map[string]string{helperHangupAfterEnv: "1"})
	change, err := b.Add(t.Context(), spec)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	tool := change.Added[0]
	if res, err := tool.Call(t.Context(), nil); err != nil || res.Failed {
		t.Fatalf("the call before the hangup = %+v, %v", res, err)
	}
	eventually(t, "the restarted server", func() bool {
		return startsIn(starts) == 2 && serving(b, "hangs-up")
	})
	exited := rec.find("server_exited")
	if len(exited) == 0 || !strings.Contains(fmt.Sprint(exited[0].Attrs["cause"]), "the session ended") {
		t.Errorf("the hangup was not reported as the session ending: %+v", exited)
	}
	if res, err := tool.Call(t.Context(), nil); err != nil || res.Failed {
		t.Fatalf("the tool held from before the hangup = %+v, %v", res, err)
	}
}

// A CALL TO A SESSION ALREADY KNOWN TO BE OVER IS NOT SENT. Between a child's
// death and its supervisor clearing it away, a tool still reaches the dead
// session; sent there, the call would fail on the pipe and be told as one the
// server died under — possibly landed, look before repeating it — when it
// never left the engine. Here there is no supervisor at all, so the dead
// session is what the tool reaches for as long as the case runs.
func TestACallToASessionAlreadyOverIsRefusedUnsent(t *testing.T) {
	t.Parallel()
	c := mustConnect(t, helperSpec(t, "over", "serve", map[string]string{helperExitAfterEnv: "1"}))
	defs, err := c.listTools(t.Context())
	if err != nil {
		t.Fatalf("listTools: %v", err)
	}
	tool := toolOn(c, defs[0], Spec{Name: "over"})
	if res, err := tool.Call(t.Context(), nil); err != nil || res.Failed {
		t.Fatalf("the call before the crash = %+v, %v", res, err)
	}
	eventually(t, "the session to end", c.gone)

	res, err := tool.Call(t.Context(), nil)
	if err != nil {
		t.Fatalf("a call to an ended session must not be a Go error: %v", err)
	}
	if !res.Failed || res.Refusal != RefusalUnavailable {
		t.Fatalf("result = %+v, want a refusal classed unavailable", res)
	}
	if res.Unknown || !strings.Contains(res.Output, "cannot be called right now") {
		t.Errorf("result = %+v: a call the engine never sent was told as one that may "+
			"have landed", res)
	}
}

// STOP WAITS FOR A RESTART IN FLIGHT. Once Stop returns nothing of the server
// may be running — not the child that died, and not the replacement its
// supervisor was in the middle of starting, which would otherwise outlive the
// stop that was meant to end it: a server a revision removed, or a seat's
// identity on a node that has just let the seat go. The replacement here
// hangs its handshake, so it is still being started when Stop is asked.
func TestStopWaitsForARestartInFlight(t *testing.T) {
	t.Parallel()
	b, _ := supervisedBridge(t, Supervision{})
	spec, starts := countedSpec(t, "hangs", "serve", map[string]string{
		helperExitAfterEnv: "1",
		helperMuteFromEnv:  "2",
	})
	spec.StartupTimeout = time.Minute
	change, err := b.Add(t.Context(), spec)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if res, err := change.Added[0].Call(t.Context(), nil); err != nil || res.Failed {
		t.Fatalf("the call before the crash = %+v, %v", res, err)
	}
	eventually(t, "the restart to be in flight", func() bool { return startsIn(starts) == 2 })
	data, err := os.ReadFile(starts)
	if err != nil {
		t.Fatalf("read starts: %v", err)
	}
	pids := strings.Fields(string(data))
	replacement, err := strconv.Atoi(pids[1])
	if err != nil {
		t.Fatalf("the replacement's pid %q: %v", pids[1], err)
	}

	if _, err := b.Stop(t.Context(), "hangs"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	running, err := procgrouptest.Running(replacement)
	if err != nil {
		t.Fatalf("reading the kernel's record of pid %d: %v", replacement, err)
	}
	if running {
		t.Fatalf("the replacement the supervisor was starting (pid %d) is still running "+
			"after Stop returned", replacement)
	}
}
