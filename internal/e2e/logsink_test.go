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
// happens when `run` installs a writer it was handed instead: 29 parallel tests pointing the global at their own buffers,
// a data race, and one test's lines in another's output. Tests here DO run in
// parallel, so the sink is installed exactly once, here, and the recorder below
// is safe to write from many goroutines.
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
// TWO JOBS, and conflating them cost a real CI investigation. Retaining every
// line of ~40 company boots would be a large buffer for no reason, so only the
// lines of a trace a case WATCHES are kept for [traceRecorder.linesFor] — but
// this is the engine's ONLY log destination for the whole package, so
// discarding the rest discarded the engine's own account of every failure. A
// run of this suite failed in CI with nothing but "timed out waiting for the
// suspended turn to be resumed"; `sandbox_resume_no_execute_state`, which
// named the cause on the line above it, went nowhere, and `-v` on the gates job
// bought nothing at all.
//
// So the buffer stays bounded and trace-scoped, and anything at WARN or above
// is written through to stderr, where `go test` attributes it to the failing
// test. A failure always logs at that level; the debug chatter of forty boots
// still does not.
var logs = &traceRecorder{through: os.Stderr}

// traceKept is how many lines one watched trace keeps.
//
// It was the bound the whole RUN shared — the first 4096 trace-carrying lines
// of the package, whichever cases logged them — so the one case that reads
// lines back passed or failed on how many lines every case before it had
// written, and "no log line carried the trace" blamed the engine for a
// harness limit. Each watched trace keeps this many now: a turn logs a few
// hundred lines at debug, so one that reached it has run away rather than
// run, and its first lines are the ones a correlation assertion reads.
const traceKept = 4096

type traceRecorder struct {
	// through receives the lines a person reads. Never the buffer's job:
	// the buffer answers assertions, this answers "why did it fail".
	through io.Writer

	mu sync.Mutex
	// traces holds the lines of every watched trace, by trace id. A trace
	// nobody watches is never kept, which is what makes a case's assertion
	// independent of how much every other case logged before it.
	traces map[string][]string
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

// traceOf is the trace id a rendered line carries, or "".
func traceOf(line string) string {
	const key = `"trace_id":"`
	at := strings.Index(line, key)
	if at < 0 {
		return ""
	}
	rest := line[at+len(key):]
	if end := strings.IndexByte(rest, '"'); end >= 0 {
		return rest[:end]
	}
	return ""
}

func (r *traceRecorder) Write(p []byte) (int, error) {
	line := string(p)
	if trace := traceOf(line); trace != "" {
		r.mu.Lock()
		if kept, watched := r.traces[trace]; watched && len(kept) < traceKept {
			r.traces[trace] = append(kept, line)
		}
		r.mu.Unlock()
	}
	if r.through != nil && loud(line) {
		// Best effort: a test's diagnostics must never fail the test.
		_, _ = r.through.Write(p)
	}
	return len(p), nil
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

var _ io.Writer = (*traceRecorder)(nil)
