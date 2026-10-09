package e2e

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/solo"
)

// The process-wide log sink for this package's tests.
//
// TestMain owns it, which is the ONLY legitimate owner besides the CLI. What
// happens when `run` installs a writer it was handed instead: 29 parallel tests
// pointing the global at their own buffers, a data race, and one test's lines
// in another's output. The single-engine cases here run in parallel with each
// other (the fleet cases run alone, before them — see [noParallel]), so the
// sink is installed exactly once, here, and the recorder below is safe to
// write from many goroutines.
//
// It also runs through [solo.Run], which declares that this package needs the
// test runner to itself: cluster_test.go stands up a fleet of engines, each
// embedding its own NATS server, in this one process. See
// `go doc ./internal/solo`.
func TestMain(m *testing.M) {
	logging.Configure(slog.LevelDebug, logging.FormatJSON, logs)
	os.Exit(solo.Run(m))
}

// logs indexes what a test can assert on, and passes on what a PERSON needs.
//
// THREE JOBS, and conflating the first two cost a real CI investigation. This
// is the engine's ONLY log destination for the whole package, so it sees every
// line of every company boot at debug; keeping all of them would be a large
// buffer for no reason, and discarding all but the asserted ones discarded the
// engine's own account of every failure. A run of this suite failed in CI with
// nothing but "timed out waiting for the suspended turn to be resumed";
// `sandbox_resume_no_execute_state`, which named the cause on the line above
// it, went nowhere, and `-v` on the gates job bought nothing at all.
//
//  1. A case that asserts on log lines WATCHES its trace before it starts the
//     work, and only a watched trace's lines are kept ([traceRecorder.watch]).
//  2. Every line at WARN or above is written through to stderr, which is the
//     one record that survives a crash or a suite timeout — no cleanup runs
//     for either.
//  3. And it is KEPT, numbered, so a failing case prints the lines logged
//     while it ran under its own name ([traceRecorder.attribute]). Stderr
//     alone stopped doing that when the cases started running concurrently:
//     `go test` frames a test's own t.Log output, and credits a line an
//     engine goroutine writes to stderr to whichever test printed last.
var logs = &traceRecorder{through: os.Stderr}

// traceKept is how many lines one watched trace keeps, and loudKept how many
// WARN and ERROR lines the ring holds.
//
// traceKept was the bound the whole RUN shared — the first 4096 trace-carrying
// lines of the package, whichever cases logged them — so the one case that
// reads lines back passed or failed on how many lines every case before it had
// written, and "no log line carried the trace" blamed the engine for a harness
// limit. Each watched trace keeps this many now: a turn logs a few hundred
// lines at debug, so one that reached it has run away rather than run, and its
// first lines are the ones a correlation assertion reads.
//
// loudKept is about three and a half times the 2,345 WARN and ERROR lines one
// whole run of this package logged (measured on the commit before the cases
// ran in parallel), so a failing case's window — at most the whole parallel
// phase — is not cut short by the ring in practice; when it is, the dump says
// how many lines fell out rather than presenting what is left as the whole.
const (
	traceKept = 4096
	loudKept  = 8192
)

type traceRecorder struct {
	// through receives the lines a person reads. Never the buffers' job:
	// the buffers answer assertions and failures, this answers a crash.
	through io.Writer

	mu sync.Mutex
	// traces holds the lines of every watched trace, by trace id. A trace
	// nobody watches is never kept, which is what makes a case's assertion
	// independent of how much every other case logged before it.
	traces map[string][]string
	// loud is the ring of WARN and ERROR lines; loudSeen counts every one
	// ever written, so line n of the run sits at loud[n % loudKept] while it
	// is still held.
	loud     []string
	loudSeen int
	// attributed is every case already printing its window on failure, so a
	// case that boots several nodes prints it once.
	attributed map[*testing.T]bool
}

// loud reports whether a line is one a reader of a failing run needs.
//
// Matched on the rendered JSON rather than a slog level hook, because this is
// an io.Writer: by the time a line arrives its level is a field like any
// other. The two names are what logging.FormatJSON emits for slog's WARN and
// ERROR.
func loud(line string) bool {
	return strings.Contains(line, `"level":"WARN"`) ||
		strings.Contains(line, `"level":"ERROR"`)
}

// traceKey is how a rendered line carries a trace id: the attribute
// logging.FormatJSON writes for the trace a line's context is in.
const traceKey = `"trace_id":"`

func (r *traceRecorder) Write(p []byte) (int, error) {
	line := string(p)
	traced := strings.Contains(line, traceKey)
	isLoud := loud(line)
	if traced || isLoud {
		r.mu.Lock()
		if traced {
			r.keepTraced(line)
		}
		if isLoud {
			if len(r.loud) < loudKept {
				r.loud = append(r.loud, line)
			} else {
				r.loud[r.loudSeen%loudKept] = line
			}
			r.loudSeen++
		}
		r.mu.Unlock()
	}
	if r.through != nil && isLoud {
		// Best effort: a test's diagnostics must never fail the test.
		_, _ = r.through.Write(p)
	}
	return len(p), nil
}

// keepTraced files a line under EVERY watched trace it carries, with r.mu held.
//
// NOT ONLY THE FIRST IT NAMES. The logger appends the context's trace after
// the record's own attributes (internal/logging), so a line that also logs a
// trace id of its own — the OTLP receiver's, the scheduler's — carries two, and
// filing it under the first lost it to the trace whose context wrote it. The
// closing quote is part of the match, so a watched id is never matched inside a
// longer one.
func (r *traceRecorder) keepTraced(line string) {
	for id, kept := range r.traces {
		if len(kept) < traceKept && strings.Contains(line, traceKey+id+`"`) {
			r.traces[id] = append(kept, line)
		}
	}
}

// watch starts keeping the lines that carry traceID, for as long as t runs.
//
// CALLED BEFORE THE WORK THAT LOGS UNDER IT, since a line written before the
// trace was watched is not kept.
func (r *traceRecorder) watch(t *testing.T, traceID string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.traces == nil {
		r.traces = map[string][]string{}
	}
	if _, already := r.traces[traceID]; !already {
		r.traces[traceID] = nil
	}
	t.Cleanup(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.traces, traceID)
	})
}

// linesFor returns the recorded lines carrying one watched trace id. Matching
// on the caller's own id is what makes this safe while other tests log in
// parallel.
func (r *traceRecorder) linesFor(traceID string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.traces[traceID]...)
}

// attribute makes every WARN and ERROR line logged from now until t ends part
// of t's own output, if t fails. Every node constructor here calls it, so a
// case that stands a node up gets it without asking.
//
// Every line of the window, other cases' engines' included: a line carries no
// mark of which engine wrote it, and the window is the one thing certain to
// hold all of this case's own. CALLED BEFORE THE CASE'S ENGINES ARE BUILT, so
// its cleanup runs after their stops (cleanups run last-registered first) and
// the window covers what an engine says on its way down.
func (r *traceRecorder) attribute(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.attributed[t] {
		return
	}
	if r.attributed == nil {
		r.attributed = map[*testing.T]bool{}
	}
	r.attributed[t] = true
	from := r.loudSeen
	t.Cleanup(func() {
		r.mu.Lock()
		delete(r.attributed, t)
		r.mu.Unlock()
		if !t.Failed() {
			return
		}
		lines, lost := r.loudSince(from)
		if len(lines) == 0 && lost == 0 {
			return
		}
		t.Logf("%d WARN/ERROR line(s) were logged while this case ran, by every "+
			"engine in the process — the lines name none:", len(lines)+lost)
		if lost > 0 {
			t.Logf("(the first %d fell out of the %d-line ring before this case "+
				"ended)", lost, loudKept)
		}
		for _, line := range lines {
			t.Log(strings.TrimRight(line, "\n"))
		}
	})
}

// loudSince is every loud line from number from on that the ring still holds,
// and how many of them it no longer does.
func (r *traceRecorder) loudSince(from int) (lines []string, lost int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if oldest := r.loudSeen - len(r.loud); from < oldest {
		lost, from = oldest-from, oldest
	}
	for n := from; n < r.loudSeen; n++ {
		lines = append(lines, r.loud[n%loudKept])
	}
	return lines, lost
}

var _ io.Writer = (*traceRecorder)(nil)

// A LINE IS KEPT UNDER EVERY WATCHED TRACE IT CARRIES, and under no trace it
// does not.
//
// The one case reading lines back asserts that a turn's lines carry its
// trigger's trace, so a line the recorder files under the wrong trace, or under
// none, reads as an engine that ran without correlation. A line that logs a
// trace id of its own carries the context's after it, which is the shape filing
// by the first id lost; and a watched id inside a longer one is a different
// trace, which a match without its closing quote would take.
func TestALineIsKeptUnderEveryWatchedTraceItCarries(t *testing.T) {
	t.Parallel()
	const (
		ctxTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
		ownTrace = "0af7651916cd43dd8448eb211c80319c"
		stranger = "ffffffffffffffffffffffffffffffff"
	)
	r := &traceRecorder{}
	r.watch(t, ctxTrace)
	r.watch(t, ownTrace)

	both := `{"level":"INFO","msg":"spans_received","trace_id":"` + ownTrace +
		`","trace_id":"` + ctxTrace + `"}` + "\n"
	longer := `{"level":"INFO","msg":"elsewhere","trace_id":"` + ctxTrace + `ff"}` + "\n"
	unwatched := `{"level":"INFO","msg":"elsewhere","trace_id":"` + stranger + `"}` + "\n"
	for _, line := range []string{both, longer, unwatched} {
		if _, err := r.Write([]byte(line)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	for _, id := range []string{ctxTrace, ownTrace} {
		if got := r.linesFor(id); len(got) != 1 || got[0] != both {
			t.Errorf("trace %s kept %q, want only the line carrying it", id, got)
		}
	}
}
