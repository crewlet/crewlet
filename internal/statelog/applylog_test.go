package statelog_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
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
		Domain: gatingDomain{}, Spec: specOf(gatingDomain{}), Layout: layoutOf(gatingDomain{}), LogID: logOf(gatingDomain{}),
		Applier:    h.applier,
		Fetch:      h.fetch,
		Log:        h.fetch,
		Node:       h.db,
		DB:         h.estate,
		Checkpoint: statelog.Position{Generation: 1},
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

// gatedLine runs a REAL runner over domain on the records offer puts on its
// log — the first of which a gate drops — and returns the one
// `statelog_record_gated` line it wrote for that drop.
func gatedLine(t *testing.T, domain statelog.Domain, offer func(h *applyHarness)) map[string]any {
	t.Helper()
	h := newApplyHarness(t, domain)
	logs := &lockedBuffer{}
	runner, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain: domain, Spec: specOf(domain), Layout: layoutOf(domain),
		LogID:      logOf(domain),
		Applier:    h.applier,
		Fetch:      h.fetch,
		Log:        h.fetch,
		Node:       h.db,
		DB:         h.estate,
		Checkpoint: statelog.Position{Generation: 1},
		Metrics:    h.metrics,
		Logger:     slog.New(slog.NewJSONHandler(logs, nil)),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	h.runner = runner
	offer(h)
	if err := h.run(1); err != nil {
		t.Fatalf("run: %v", err)
	}
	lines := logRecords(t, logs.Bytes(), "statelog_record_gated")
	if len(lines) != 1 {
		t.Fatalf("%d statelog_record_gated lines for one dropped record, want one",
			len(lines))
	}
	return lines[0]
}

// THE REPLICATION GUIDE SAYS WHAT THE RECORD-GATED LINE CARRIES, UNDER EVERY
// GATE.
//
// The line is a dropped record's only witness, and the guide's row is what an
// operator reads it by — the row the `statelog_write_gated` row and the
// records-gated alarm both send them to. It named three gates of seven, every
// one the framework's, so an operator following a `released` refusal to the
// drop behind it read that the line never carries that gate; and it named no
// key but `log` and `belongs_to`, so nothing said the line names the node that
// wrote the record, the one fact the alarm's remedy says to read it for.
//
// So the row is held to the line itself — every key a domain's gate puts on it
// and every key the partition's adds — and to every reason that is a gate's
// ([reasonDecisions]), since the line is logged under whichever one dropped the
// record.
func TestTheReplicationGuideSaysWhatTheRecordGatedLineCarries(t *testing.T) {
	t.Parallel()
	lines := []map[string]any{
		// A DOMAIN'S GATE, which every node asks of its own rows.
		gatedLine(t, probeDomain{}, func(h *applyHarness) {
			h.applier.gate, h.applier.gated[1] = statelog.ReasonReleased, true
			h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
		}),
		// THE PARTITION'S, which also names the log and where the record
		// belongs.
		gatedLine(t, placingDomain{}, func(h *applyHarness) {
			h.fetch.offer(1, env(1, strayKind, "b", "op-2", 1))
		}),
	}
	row := guideRow(t, "statelog_record_gated")
	for _, line := range lines {
		for key := range line {
			switch key {
			case slog.TimeKey, slog.LevelKey, slog.MessageKey:
				continue
			}
			if !strings.Contains(row, "`"+key+"`") {
				t.Errorf("the guide's statelog_record_gated row never names `%s`, a "+
					"key the line carries under the gate %v: %s", key, line["gate"], row)
			}
		}
	}
	for _, reason := range statelog.Reasons() {
		if reasonDecisions[reason].gate && !strings.Contains(row, "`"+string(reason)+"`") {
			t.Errorf("the guide's statelog_record_gated row never names `%s`, a gate "+
				"the line is logged under: %s", reason, row)
		}
	}
	if !strings.Contains(row, "`"+metrics.StatelogRecordsGated+"`") {
		t.Errorf("the guide's statelog_record_gated row never names `%s`, which "+
			"counts the lines: %s", metrics.StatelogRecordsGated, row)
	}
}

// guideRow is the replication guide's table row for one log line.
func guideRow(t *testing.T, line string) string {
	t.Helper()
	guide, err := os.ReadFile(filepath.Join(sourcetree.Root(t), "docs", "guides", "replication.md"))
	if err != nil {
		t.Fatalf("read the replication guide: %v", err)
	}
	for row := range strings.SplitSeq(string(guide), "\n") {
		if strings.HasPrefix(row, "| `"+line+"` |") {
			return row
		}
	}
	t.Fatalf("the replication guide's table of state-log lines has no %s row", line)
	return ""
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

// THE RECORDS-GATED ALARM POINTS AT FIELDS ITS LOG LINE CARRIES.
//
// A dropped record is recoverable by nothing, and the alarm's remedy is what
// sends an operator to the one witness there is: the applier's
// `statelog_record_gated` line. It told them the line named "the operator",
// which no line carries — the node that wrote the record is its `writer` —
// so the fact the remedy exists to hand over was the one it misnamed. So every
// name the remedy puts in backticks is held to the line itself: the first is
// the line's own message, and every other is a key on it.
func TestTheRecordsGatedRemedyNamesWhatItsLineCarries(t *testing.T) {
	t.Parallel()
	line := gatedLine(t, probeDomain{}, func(h *applyHarness) {
		h.applier.gate, h.applier.gated[1] = statelog.ReasonEvicted, true
		h.fetch.offer(1, env(1, "edit", "a", "op-1", 1))
	})

	alarm, found := find(statelog.Evaluate(statelog.Reading{RecordsGated: 1}),
		statelog.KindRecordsGated)
	if !found {
		t.Fatalf("%s did not fire on one dropped record", statelog.KindRecordsGated)
	}
	// EVERY OTHER SPAN between two backticks is a name.
	parts := strings.Split(alarm.Remedy, "`")
	var named []string
	for i := 1; i < len(parts); i += 2 {
		named = append(named, parts[i])
	}
	if len(named) < 2 || named[0] != "statelog_record_gated" {
		t.Fatalf("the %s remedy %q does not name the statelog_record_gated line "+
			"and a field on it — the line is the only witness a dropped record has",
			statelog.KindRecordsGated, alarm.Remedy)
	}
	for _, key := range named[1:] {
		if _, carried := line[key]; !carried {
			t.Errorf("the %s remedy sends an operator to the line's %q, which the "+
				"line does not carry: %v", statelog.KindRecordsGated, key, line)
		}
	}
	if !strings.Contains(alarm.Remedy, "`writer`") {
		t.Errorf("the %s remedy %q does not name the `writer` — which node wrote "+
			"the record is what an operator reads the line for",
			statelog.KindRecordsGated, alarm.Remedy)
	}
}
