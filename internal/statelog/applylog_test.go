package statelog_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A STOP IS WRITTEN ONCE, AND THE LINE SAYS WHAT RESUMES IT.
//
// The runner's line went to a discarding logger, so the engine wrote its own
// `statelog_applier_stopped` when the loop returned, carrying the one sentence
// saying what the stop costs and what ends it. With the runner's line reaching
// the log, one stop read as two — so the runner's is the only one, and it
// carries that sentence beside the stream and the position it froze at. The
// engine's half of the same invariant is its own test, since only a booted
// engine can show which lines it adds.
func TestAStopIsWrittenOnceSayingWhatResumesIt(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t, gatingDomain{})
	logs := &lockedBuffer{}
	runner, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain:     gatingDomain{},
		Applier:    h.applier,
		Fetch:      h.fetch,
		Log:        h.fetch,
		DB:         h.db.Replicated(),
		Generation: 1,
		Metrics:    h.metrics,
		Logger:     slog.New(slog.NewJSONHandler(logs, nil)),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
	h.fetch.offer(2, env(2, "eviction", "node-b", "op-2", 9)) // a gate, unreadable

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if err := runner.Run(ctx); !errors.Is(err, statelog.ErrStopped) {
		t.Fatalf("Run over a gate this build cannot read returned %v, want a stop", err)
	}

	stops := logRecords(t, logs.Bytes(), "statelog_applier_stopped")
	if len(stops) != 1 {
		t.Fatalf("%d statelog_applier_stopped lines for one stop, want one — "+
			"anything counting the event counts two stops", len(stops))
	}
	line := stops[0]
	for key, want := range map[string]any{
		"level":  "ERROR",
		"domain": gatingDomain{}.Name(),
		"stream": probeStream,
	} {
		if line[key] != want {
			t.Errorf("statelog_applier_stopped %s = %v, want %v", key, line[key], want)
		}
	}
	if position, _ := line["position"].(string); position == "" {
		t.Error("statelog_applier_stopped does not say where the rows froze")
	}
	if cause, _ := line["error"].(string); !strings.Contains(cause, "eviction") {
		t.Errorf("statelog_applier_stopped does not name the record it stopped on: %q", cause)
	}
	detail, _ := line["detail"].(string)
	for _, says := range []string{"refuses", "seats", "reanchor"} {
		if !strings.Contains(detail, says) {
			t.Errorf("statelog_applier_stopped's detail does not say %q — what "+
				"the stop costs and what resumes it was the engine's line's to "+
				"say, and that line is gone: %q", says, detail)
		}
	}
}

// lockedBuffer is a log destination safe to write from the loop's goroutine
// while a test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// Bytes is a copy of everything written so far.
func (b *lockedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}

// logRecords decodes every JSON line with this message.
func logRecords(t *testing.T, written []byte, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(string(written)), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("a log line is not JSON: %q: %v", line, err)
		}
		if record["msg"] == msg {
			out = append(out, record)
		}
	}
	return out
}
