package secretsapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CONDITION OF THE NODE IS NOT A FAULT HERE EITHER.
//
// Every failure this surface did not name was `500 internal_error`, a
// coordination store that blinked and a node behind its log among them, which
// told an administrator the engine had broken and offered no retry. A
// condition is `503` in a person's words for it ([builtin.ConditionSentence]),
// under its own hint, none where no wait clears it, and its error goes to the
// log; a fault keeps its `500` (the control).
//
// Not parallel: it installs the process's log sink.
//
// Mutation: drop the condition's arm from fail and the first two rows answer
// 500; answer the fault as a condition and the control does; log through
// another package's logger, or without what the request was doing, and the
// log assertion goes red.
func TestAConditionIsNotAFault(t *testing.T) {
	var out syncBuffer
	logging.Configure(slog.LevelInfo, logging.FormatText, &out)
	t.Cleanup(func() { logging.Configure(slog.LevelError, logging.FormatText, io.Discard) })
	blink := fmt.Errorf("dial nats://10.0.0.7:4222: %w", coord.ErrUnavailable)
	full := fmt.Errorf("iamdomain: read: %w", &statelog.Refused{Code: statelog.RefuseLogFull,
		Level: statelog.ReadSession, Detail: "raise stream.identity_max_bytes"})
	blinkSaid, _ := builtin.ConditionSentence(blink)
	fullSaid, _ := builtin.ConditionSentence(full)
	for name, c := range map[string]struct {
		err    error
		status int
		says   string
		retry  bool
	}{
		"a coordination store that blinked": {blink, http.StatusServiceUnavailable, blinkSaid, true},
		"a log at its ceiling":              {full, http.StatusServiceUnavailable, fullSaid, false},
		"a fault":                           {errors.New("open /var/lib/crewlet/node.db: disk I/O"), http.StatusInternalServerError, "", false},
	} {
		out.Reset()
		rec := httptest.NewRecorder()
		(&Service{}).fail(rec, httptest.NewRequest(http.MethodGet, "/", nil), "count the identity estate's sealed values", c.err)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != c.status || (c.says != "" && body["detail"] != c.says) {
			t.Errorf("%s answered %d %v, want %d with %q", name, rec.Code, body, c.status, c.says)
		}
		if retry := rec.Header().Get("Retry-After") != ""; retry != c.retry {
			t.Errorf("%s: Retry-After %q, want one: %v", name, rec.Header().Get("Retry-After"), c.retry)
		}
		if text := rec.Body.String(); strings.Contains(text, "10.0.0.7") ||
			strings.Contains(text, "/var/lib") || strings.Contains(text, "identity_max") {
			t.Errorf("%s carried its error's own text: %s", name, text)
		}
		// THE ERROR GOES TO THIS SURFACE'S LOG, under its own component and
		// with what the request was doing, at WARN exactly where no wait
		// clears it: the answer no longer carries it, so this line is the
		// only place it goes.
		if c.says != "" {
			line := out.String()
			level := "level=INFO"
			if !c.retry {
				level = "level=WARN"
			}
			if !strings.Contains(line, "msg=secret_request_unavailable") || !strings.Contains(line, "component=api.secrets") ||
				!strings.Contains(line, level) || !strings.Contains(line, "what=") ||
				!strings.Contains(line, c.err.Error()) {
				t.Errorf("%s logged %q, want secret_request_unavailable under api.secrets at %s with what "+
					"the request did and the error", name, line, level)
			}
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
