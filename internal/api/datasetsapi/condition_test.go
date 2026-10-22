package datasetsapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A DATASET THE NODE COULD NOT READ FOR A CONDITION SAYS WHICH, AND WHEN TO
// COME BACK, in a person's words.
//
// Every one was "this node could not read <dataset> right now" under an
// election's four seconds: a source's question refused because this node's log
// is at its ceiling was polled every four seconds for ever, and a store
// dataset whose read the state log refused — an error no question wrapped —
// was logged as a failure. Now each is told in [builtin.ConditionSentence]
// under its own hint, none where no wait clears it, and a failure that is no
// condition is the node's own fault: `500`, with no Retry-After, where it was
// a 503 under the same four seconds and a board polled a fault for ever.
//
// AND A CONDITION'S ERROR GOES TO THIS SURFACE'S LOG with the board and the
// dataset it was for, which the fields the caller hands settle carry: a line
// with the error alone, under another package's component, named neither.
//
// Not parallel: it installs the process's log sink.
//
// Mutation: answer the question's arm under the election hint again, or leave
// a bare state-log refusal to the arm below it, and their rows go red; answer
// the fault as a 503 and its row does; log the condition without the caller's
// fields and the log rows do.
func TestADatasetsConditionSaysWhichAndWhenToComeBack(t *testing.T) {
	var out syncBuffer
	logging.Configure(slog.LevelInfo, logging.FormatText, &out)
	t.Cleanup(func() { logging.Configure(slog.LevelError, logging.FormatText, io.Discard) })
	const detail = "raise stream.tracker_max_bytes on 10.0.0.7"
	full := &statelog.Refused{Code: statelog.RefuseLogFull, Level: statelog.ReadSession,
		Detail: detail}
	fullSaid, _ := builtin.ConditionSentence(full)
	for name, c := range map[string]struct {
		err    error
		status int
		says   any
		retry  string
	}{
		"a source's question on a full log": {fmt.Errorf("%w: %w", queries.ErrUnavailable,
			full), http.StatusServiceUnavailable, fullSaid, ""},
		"a store dataset on a full log": {fmt.Errorf("artifactdata: docs: %w", full),
			http.StatusServiceUnavailable, fullSaid, ""},
		"a failure that is no condition": {fmt.Errorf("read /var/lib/crewlet/replicated.db: disk I/O"),
			http.StatusInternalServerError, nil, ""},
	} {
		out.Reset()
		rec := httptest.NewRecorder()
		ok := settle(rec, httptest.NewRequest(http.MethodGet, "/x", nil), result{err: c.err},
			&Reading{}, "open_items", "spent", "artifact", "art-zz", "dataset", "ds-zz")
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if ok || rec.Code != c.status || body["detail"] != c.says {
			t.Errorf("%s answered %v %d %v, want %d with %v", name, ok, rec.Code, body,
				c.status, c.says)
		}
		if after := rec.Header().Get("Retry-After"); after != c.retry {
			t.Errorf("%s: Retry-After %q, want %q", name, after, c.retry)
		}
		if line := out.String(); !strings.Contains(line, "component=datasetsapi") ||
			!strings.Contains(line, "artifact=art-zz") ||
			!strings.Contains(line, "dataset=ds-zz") {
			t.Errorf("%s logged %q, want the board and the dataset under "+
				"datasetsapi", name, line)
		}
		if text := rec.Body.String(); strings.Contains(text, "10.0.0.7") ||
			strings.Contains(text, "/var/lib") {
			t.Errorf("%s carried its error's own text: %s", name, text)
		}
	}
}

// syncBuffer is a log sink the logger may write from any goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}
