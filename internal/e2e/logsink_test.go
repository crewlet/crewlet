package e2e

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/logging"
)

// The process-wide log sink for this package's tests.
//
// TestMain owns it, which is the ONLY legitimate owner besides the CLI. What
// happens when `run` installs a writer it was handed instead: 29 parallel tests pointing the global at their own buffers,
// a data race, and one test's lines in another's output. Tests here DO run in
// parallel, so the sink is installed exactly once, here, and the recorder below
// is safe to write from many goroutines.
func TestMain(m *testing.M) {
	logging.Configure(slog.LevelDebug, logging.FormatJSON, logs)
	os.Exit(m.Run())
}

// logs indexes what a test can assert on, and passes on what a PERSON needs.
//
// TWO JOBS, and conflating them cost a real CI investigation. Retaining every
// line of ~40 company boots would be a large buffer for no reason, so only the
// lines carrying a trace id are kept for [traceRecorder.linesFor] — but this
// is the engine's ONLY log destination for the whole package, so discarding
// the rest discarded the engine's own account of every failure. A run of this
// suite failed in CI with nothing but "timed out waiting for the suspended
// turn to be resumed"; `sandbox_resume_no_execute_state`, which named the
// cause on the line above it, went nowhere, and `-v` on the gates job bought
// nothing at all.
//
// So the buffer stays bounded and trace-scoped, and anything at WARN or above
// is written through to stderr, where `go test` attributes it to the failing
// test. A failure always logs at that level; the debug chatter of forty boots
// still does not.
var logs = &traceRecorder{through: os.Stderr}

type traceRecorder struct {
	// through receives the lines a person reads. Never the buffer's job:
	// the buffer answers assertions, this answers "why did it fail".
	through io.Writer

	mu    sync.Mutex
	lines []string
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

func (r *traceRecorder) Write(p []byte) (int, error) {
	line := string(p)
	if strings.Contains(line, `"trace_id":"`) {
		r.mu.Lock()
		// Bounded, so a pathological run cannot grow without limit.
		if len(r.lines) < 4096 {
			r.lines = append(r.lines, line)
		}
		r.mu.Unlock()
	}
	if r.through != nil && loud(line) {
		// Best effort: a test's diagnostics must never fail the test.
		_, _ = r.through.Write(p)
	}
	return len(p), nil
}

// linesFor returns the recorded lines carrying one trace id. Matching on the
// caller's own id is what makes this safe while other tests log in parallel.
func (r *traceRecorder) linesFor(traceID string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, line := range r.lines {
		if strings.Contains(line, `"trace_id":"`+traceID+`"`) {
			out = append(out, line)
		}
	}
	return out
}

var _ io.Writer = (*traceRecorder)(nil)
