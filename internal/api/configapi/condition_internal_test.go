package configapi

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
	"github.com/crewlet/crewlet/internal/store"
)

// A CONDITION OF THE NODE IS NOT A FAULT HERE EITHER.
//
// Every failure this surface did not name was `500 internal_error`, beside
// the coordination store's own arm, which answers the fleet's pointer it could
// not read with its reason and the Retry-After every undecided request here
// takes: a store busy with other writes, one closed while the node adopts a
// snapshot, a log at its ceiling, a request out of time, each told an
// administrator the engine had broken and offered no retry. A condition is
// `503` in a person's words for it ([builtin.ConditionSentence]), under its
// own hint, none where no wait clears it, and its error goes to the log; a
// fault keeps its `500` (the control).
//
// Not parallel: it installs the process's log sink.
//
// Mutation: drop the condition's arm from fail and the busy store and the
// full log answer 500; put it before the coordination store's arm and that
// row loses its reason; answer the fault as a condition and the control
// does; log through another package's logger, or without what the request was
// doing, and the log assertion goes red.
func TestAConditionIsNotAFault(t *testing.T) {
	var out syncBuffer
	logging.Configure(slog.LevelInfo, logging.FormatText, &out)
	t.Cleanup(func() { logging.Configure(slog.LevelError, logging.FormatText, io.Discard) })
	blink := fmt.Errorf("dial nats://10.0.0.7:4222: %w", coord.ErrUnavailable)
	busy := fmt.Errorf("configapi: store the config: %w", store.ErrBusy)
	full := fmt.Errorf("iamdomain: read: %w", &statelog.Refused{Code: statelog.RefuseLogFull,
		Level: statelog.ReadSession, Detail: "raise stream.identity_max_bytes"})
	busySaid, _ := builtin.ConditionSentence(busy)
	fullSaid, _ := builtin.ConditionSentence(full)
	for name, c := range map[string]struct {
		err    error
		status int
		// field and says are the body's sentence: the coordination store's
		// own arm states its reason, every other condition its detail.
		field, says string
		retry       bool
		level       string
	}{
		"a coordination store that blinked": {blink, http.StatusServiceUnavailable, "reason",
			"the fleet's activation could not be read from the coordination store", true, "level=WARN"},
		"a store busy with other writes": {busy, http.StatusServiceUnavailable, "detail", busySaid, true,
			"level=INFO"},
		"a log at its ceiling": {full, http.StatusServiceUnavailable, "detail", fullSaid, false, "level=WARN"},
		"a fault": {errors.New("open /var/lib/crewlet/node.db: disk I/O"),
			http.StatusInternalServerError, "", "", false, "level=ERROR"},
	} {
		out.Reset()
		rec := httptest.NewRecorder()
		(&Service{}).fail(rec, httptest.NewRequest(http.MethodGet, "/", nil), "read the fleet's activation", c.err)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != c.status || (c.says != "" && body[c.field] != c.says) {
			t.Errorf("%s answered %d %v, want %d with %s %q", name, rec.Code, body, c.status, c.field, c.says)
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
		line := out.String()
		if !strings.Contains(line, "component=api.config") || !strings.Contains(line, c.level) ||
			!strings.Contains(line, "what=") || !strings.Contains(line, c.err.Error()) {
			t.Errorf("%s logged %q, want a line under api.config at %s with what the request "+
				"did and the error", name, line, c.level)
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
