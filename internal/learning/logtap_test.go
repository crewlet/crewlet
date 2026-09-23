package learning_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/logging"
)

// logs is every record this package's tests wrote, at every level.
//
// Installed as the PROCESS's sink, from TestMain, because that is the one
// owner [logging.Configure] admits besides the CLI: the package logger
// resolves the process root per record, so a line the decider logs is
// reachable only there. A case finds its own records by content it alone
// wrote, which is what lets parallel cases share one sink.
//
// DEBUG, because some of what these tests hold is that a value with no other
// copy reaches the debug twin of a line — a level that dropped debug would
// make every such case vacuous.
var logs tap

// tap is a concurrency-safe sink the JSON handler writes one record per line
// into.
type tap struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (t *tap) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.Write(p)
}

// records is every record named event, decoded.
func (t *tap) records(tb testing.TB, event string) []map[string]any {
	tb.Helper()
	t.mu.Lock()
	raw := bytes.Clone(t.buf.Bytes())
	t.mu.Unlock()

	var out []map[string]any
	lines := bufio.NewScanner(bytes.NewReader(raw))
	lines.Buffer(nil, len(raw)+1)
	for lines.Scan() {
		var record map[string]any
		if err := json.Unmarshal(lines.Bytes(), &record); err != nil {
			tb.Fatalf("a log line is not a JSON record: %v\n%s", err, lines.Bytes())
		}
		if record["msg"] == event {
			out = append(out, record)
		}
	}
	if err := lines.Err(); err != nil {
		tb.Fatalf("reading the captured log: %v", err)
	}
	return out
}

func TestMain(m *testing.M) {
	logging.Configure(slog.LevelDebug, logging.FormatJSON, &logs)
	os.Exit(m.Run())
}
