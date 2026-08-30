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

// logs keeps only what a test could assert on.
//
// Retaining every line of ~40 company boots would be a large buffer for no
// reason, so this keeps the lines that carry a trace id and discards the rest.
// That is also the only thing any test here looks for.
var logs = &traceRecorder{}

type traceRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *traceRecorder) Write(p []byte) (int, error) {
	if idx := strings.Index(string(p), `"trace_id":"`); idx >= 0 {
		r.mu.Lock()
		// Bounded, so a pathological run cannot grow without limit.
		if len(r.lines) < 4096 {
			r.lines = append(r.lines, string(p))
		}
		r.mu.Unlock()
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
