package operator

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CONDITION'S ERROR GOES TO THE SURFACE'S OWN LOG, with the surface's own
// fields, at the one level rule — the only place it goes, since no answer
// carries it any more.
//
// LogCondition and AnswerCondition log through the logger they are handed, so
// a line carries the answering surface's component and what the request was
// doing (an artifact write's verb and operation, the read a configuration
// route made) beside the error: they logged the error alone under this
// package's component, so a line said something failed and nothing about
// what. The level is [statelog.ConditionLevel]'s, WARN exactly where no wait
// clears the condition and INFO for one that does, and this surface's own
// lines carry the operation its answer names.
//
// Not parallel: the last case installs the process's log sink.
//
// Mutation: log through this package's logger, drop the fields, or log every
// condition at one level, and a row goes red.
func TestAConditionIsLoggedWhereItsSurfaceSaysAndAtTheOneLevel(t *testing.T) {
	full := fmt.Errorf("artifactsdomain: publish: %w", &statelog.Unavailable{
		Reason: statelog.ReasonLogFull, Detail: "raise stream.artifacts_max_bytes"})
	blink := fmt.Errorf("dial nats://10.0.0.7:4222: %w", coord.ErrUnavailable)
	for name, c := range map[string]struct {
		err   error
		level string
	}{
		"a full log":                   {full, "level=WARN"},
		"a coordination store's blink": {blink, "level=INFO"},
	} {
		var out bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})).
			With("component", "surface")
		LogCondition(t.Context(), logger, "surface_unavailable", c.err, "verb", "publish")
		line := out.String()
		for _, want := range []string{c.level, "component=surface", "msg=surface_unavailable",
			"verb=publish", "error="} {
			if !strings.Contains(line, want) {
				t.Errorf("%s: LogCondition logged %q, want %s in it", name, line, want)
			}
		}

		out.Reset()
		rec := httptest.NewRecorder()
		if !AnswerCondition(t.Context(), rec, logger, "surface_unavailable", c.err,
			httpjson.Detail{"wrote_secrets": []string{"GITLAB_TOKEN"}},
			"what", "read the fleet's activation") {
			t.Fatalf("%s is not answered as a condition", name)
		}
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: AnswerCondition answered %d", name, rec.Code)
		}
		// THE SURFACE'S OWN DETAIL rides beside the sentence, which it does
		// not replace: what a /setup submission sealed before the condition.
		if body := rec.Body.String(); !strings.Contains(body, `"wrote_secrets":["GITLAB_TOKEN"]`) ||
			!strings.Contains(body, `"detail":"`) {
			t.Errorf("%s: AnswerCondition answered %s, want the surface's wrote_secrets "+
				"beside the condition's sentence", name, body)
		}
		if line := out.String(); !strings.Contains(line, c.level) ||
			!strings.Contains(line, "component=surface") ||
			!strings.Contains(line, `what="read the fleet's activation"`) {
			t.Errorf("%s: AnswerCondition logged %q", name, line)
		}
	}
	// NOTHING FOR A FAULT: it is the caller's to answer, and to log as one.
	var out bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&out, nil))
	if AnswerCondition(t.Context(), httptest.NewRecorder(), logger, "surface_unavailable",
		fmt.Errorf("disk I/O"), nil, "what", "x") || out.Len() != 0 {
		t.Errorf("a fault was answered or logged as a condition: %q", out.String())
	}

	// THIS SURFACE'S OWN, for the dispatch's refusals: its component and
	// the operation the answer names.
	var sink syncBuffer
	logging.Configure(slog.LevelInfo, logging.FormatText, &sink)
	t.Cleanup(func() { logging.Configure(slog.LevelError, logging.FormatText, io.Discard) })
	logCondition("operator_call_unavailable", full, "op_id", "op-1")
	if line := sink.String(); !strings.Contains(line, "level=WARN") ||
		!strings.Contains(line, "component=api.operator") ||
		!strings.Contains(line, "op_id=op-1") ||
		!strings.Contains(line, "artifacts_max_bytes") {
		t.Errorf("the dispatch's own condition logged %q", line)
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
