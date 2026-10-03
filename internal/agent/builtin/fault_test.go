package builtin_test

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tools"
)

// A STORE FAULT IN A READ TOOL IS A FAULT: `internal_error`, a sentence that
// says the node broke without saying how, and the store's own error in the
// node's log — where whoever runs the node reads it — and nowhere else. It
// was `unavailable` carrying the error's text, so a model was told to try
// again for ever and handed a database path in its prompt, and a person's
// screen showed the same path under "try again in a moment".
//
// The control beside it is a read the STATE LOG refused: a condition waiting
// clears, which keeps `unavailable`, its own words and the hint it derived.
//
// NOT PARALLEL: it reads the process's log, which [logging.Configure] owns,
// and Go resumes every parallel test only after the sequential ones end.
//
// Mutation: let readFailure class an unmarked error `unavailable`, put the
// error back in its sentence, or drop the log line in faulted, and this goes
// red; class a state-log refusal as a fault and the control does.
func TestAStoreFaultInAReadToolIsAFault(t *testing.T) {
	logs := &lockedBuffer{}
	logging.Configure(slog.LevelInfo, logging.FormatText, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })

	disk := errors.New("tracker: read ENG-1: open /var/lib/crewlet/replicated.db: " +
		"disk I/O error")
	trk := newFakeTracker()
	trk.readErr = disk
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	got := callWork(t, reg, builtin.GetWorkItemTool, map[string]any{"item": "ENG-1"})
	if !got.Failed || tools.RefusalOf(got) != tools.RefusalInternalError {
		t.Fatalf("a store fault answered %q (%s), want internal_error", got.Output,
			tools.RefusalOf(got))
	}
	if strings.Contains(got.Output, "/var/lib") || strings.Contains(got.Output, "disk I/O") {
		t.Errorf("the store's own error reached the sentence: %q", got.Output)
	}
	if strings.Contains(got.Output, "Try again") || !strings.Contains(got.Output, "NOT an empty result") {
		t.Errorf("a fault's sentence %q invites a retry, or lets the model read "+
			"it as nothing found", got.Output)
	}
	if !errors.Is(got.Cause, disk) {
		t.Error("the fault's error is not beneath its class, so errors.Is lost it")
	}
	line := logs.lineWith("builtin_tool_fault")
	if !strings.Contains(line, builtin.GetWorkItemTool) || !strings.Contains(line, "/var/lib") {
		t.Errorf("the node's log has no line naming the tool and the error: %q\n%s",
			line, logs.String())
	}

	behind := &statelog.Refused{Code: statelog.RefuseBehind, Level: statelog.ReadSession,
		RetryAfter: 9 * time.Second, Detail: "this node is 40 records behind"}
	trk.readErr = behind
	got = callWork(t, reg, builtin.GetWorkItemTool, map[string]any{"item": "ENG-1"})
	if !got.Failed || tools.RefusalOf(got) != tools.RefusalUnavailable {
		t.Fatalf("a read the state log refused answered %q (%s), want unavailable",
			got.Output, tools.RefusalOf(got))
	}
	if hint := statelog.RetryAfter(got.Cause, 0); hint != 9*time.Second {
		t.Errorf("the refusal's hint reads %s through the result, want its own 9s", hint)
	}
	if !strings.Contains(got.Output, "40 records behind") || !strings.Contains(got.Output, "Try again") {
		t.Errorf("a condition's sentence %q does not say what it is and that a "+
			"retry clears it", got.Output)
	}
}

// A BROKER BLIP AND A BUSY STORE ARE CONDITIONS, NOT FAULTS: each clears by
// waiting — a reconnect completes, the writers ahead of this one commit — so
// the tool answers `unavailable` and a cause a surface derives a Retry-After
// from, in words of its own rather than the client's or the driver's. They
// were faults the moment the fault line was drawn, because neither backend
// marked them: a2a_ask and steer_turn answered a broker that was reconnecting
// as a broken node, and a write that waited out a busy store as one too. The
// marks are the backends' — queue.ErrUnavailable, store.ErrBusy — and the
// control is the same words with no mark, which stays a fault: a classifier
// that read text would call any error naming a lock a wait.
//
// Mutation: drop either case from [builtin.Condition], and its half goes red;
// match on the error's words instead of the mark, and the controls do.
func TestABrokerBlipAndABusyStoreAreConditions(t *testing.T) {
	t.Parallel()
	locked := errors.New("turso: error: database is locked")
	reconnecting := errors.New("nats: timeout")

	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
	update := func() tools.Result {
		return callWork(t, reg, builtin.UpdateWorkItemTool,
			map[string]any{"item": "ENG-1", "priority": "urgent"})
	}

	// THE STORE'S GIVE-UP, wrapped the way the store wraps it.
	trk.writeErr = fmt.Errorf("tracker: update ENG-1: %w",
		fmt.Errorf("%w: %w", store.ErrBusy, locked))
	got := update()
	assertCondition(t, "a write the store was too busy to take", got, store.ErrBusy, "turso")

	trk.writeErr = fmt.Errorf("tracker: update ENG-1: %w", locked)
	if got := update(); tools.RefusalOf(got) != tools.RefusalInternalError {
		t.Errorf("a lock error the store did not mark busy answered %s (%q); the "+
			"store marks exactly the give-up a wait clears, and words are not a mark",
			tools.RefusalOf(got), got.Output)
	}

	// THE QUEUE'S MARK, on the ask steer_turn scatters its note through.
	blip := fmt.Errorf("ask crewlet.seat.steer: %w", fmt.Errorf(
		"%w: this node's connection to it is reconnecting: %w",
		queue.ErrUnavailable, reconnecting))
	got, _ = steerCall(t, steerTool(t, &fleetAsker{err: blip}, steerFleet{}, "r-1"),
		"run-1", "use staging")
	assertCondition(t, "a note the broker did not take", got, queue.ErrUnavailable, "nats:")
	if !strings.Contains(got.Output, "Try again") || !strings.Contains(got.Output, "Nothing was sent") {
		t.Errorf("a note the broker did not take answered %q; it says neither that "+
			"trying again clears it nor that no note went out", got.Output)
	}

	unmarked := fmt.Errorf("ask crewlet.seat.steer: %w", reconnecting)
	got, _ = steerCall(t, steerTool(t, &fleetAsker{err: unmarked}, steerFleet{}, "r-1"),
		"run-1", "use staging")
	if tools.RefusalOf(got) != tools.RefusalInternalError {
		t.Errorf("a broker error the queue did not mark answered %s (%q); an "+
			"unmarked failure is this node's, whatever its words", tools.RefusalOf(got),
			got.Output)
	}
}

// assertCondition holds a failed result to what a condition answers: the
// unavailable class — which is what tells a surface to answer 503 and a model
// that the call may be made again — a sentence that keeps the backend's own
// words out, the mark beneath the class, and a cause from which a surface
// derives its own Retry-After rather than the none a refusal waiting cannot
// clear carries. How a tool words the retry is its own; steer_turn's is
// asserted where it is called.
func assertCondition(t *testing.T, what string, got tools.Result, mark error, words string) {
	t.Helper()
	if !got.Failed || tools.RefusalOf(got) != tools.RefusalUnavailable {
		t.Fatalf("%s answered %q (%s), want unavailable", what, got.Output,
			tools.RefusalOf(got))
	}
	if strings.Contains(got.Output, words) {
		t.Errorf("%s: the backend's own words reached the sentence: %q", what, got.Output)
	}
	if !errors.Is(got.Cause, mark) {
		t.Errorf("%s: the cause %v does not carry %v beneath its class", what,
			got.Cause, mark)
	}
	if hint := statelog.RetryAfter(got.Cause, time.Second); hint != time.Second {
		t.Errorf("%s: a surface would answer it with a Retry-After of %s, want its "+
			"own hint — the cause reads as a refusal no wait clears", what, hint)
	}
}

// lockedBuffer is a log destination a test can read while a stray goroutine
// left by an earlier case may still be writing to it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// lineWith is the first logged line carrying event, or "".
func (b *lockedBuffer) lineWith(event string) string {
	for _, line := range strings.Split(b.String(), "\n") {
		if strings.Contains(line, event) {
			return line
		}
	}
	return ""
}
