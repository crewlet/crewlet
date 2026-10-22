package artifactsapi

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
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CONDITION OF THE NODE IS TOLD IN A PERSON'S WORDS FOR IT, on a write and on
// a read of an artifact alike, under its own hint.
//
// A write the state log refused was answered "this node could not publish
// right now: " and the refusal's own message, the framework's name and the
// detail written for whoever runs the node; a read was "could not read version
// 3 right now", which asked a person to come back to a log at its ceiling.
// Both are told in [builtin.ConditionSentence], whose advice agrees with the
// Retry-After, and the refusal's own text reaches neither.
//
// AND ITS ERROR GOES TO THIS SURFACE'S LOG, under its own component and with
// what the gesture was — the verb and the operation of a write, what a read
// was of — at WARN exactly where no wait clears it, since the answer no longer
// carries it: a line with the error alone, under another package's component,
// said something failed and nothing about what.
//
// Not parallel: it installs the process's log sink.
//
// Mutation: answer a write in [builtin.Condition]'s words again, or a read in
// its fixed sentence, and its rows go red; log through another logger or
// without the gesture's fields, and the log rows do.
func TestAConditionIsToldInAPersonsWords(t *testing.T) {
	var out syncBuffer
	logging.Configure(slog.LevelInfo, logging.FormatText, &out)
	t.Cleanup(func() { logging.Configure(slog.LevelError, logging.FormatText, io.Discard) })
	const detail = "raise stream.artifacts_max_bytes on 10.0.0.7"
	full := fmt.Errorf("artifactsdomain: publish: %w", &statelog.Unavailable{
		Reason: statelog.ReasonLogFull, Detail: detail})
	behind := fmt.Errorf("artifactsdomain: read: %w", &statelog.Refused{
		Code: statelog.RefuseBehind, Level: statelog.ReadSession, Detail: detail,
		RetryAfter: 1})
	write := []string{"msg=artifacts_write_unavailable", "verb=publish", "op_id=op-1"}
	read := []string{"msg=artifacts_read_unavailable", `what="version 3"`}
	for name, c := range map[string]struct {
		answer func(http.ResponseWriter, *http.Request)
		err    error
		retry  bool
		logged []string
	}{
		"a write on a full log": {func(w http.ResponseWriter, r *http.Request) {
			writeFailed(w, r, "op-1", "publish", full)
		}, full, false, write},
		"a write on a node behind": {func(w http.ResponseWriter, r *http.Request) {
			writeFailed(w, r, "op-1", "publish", behind)
		}, behind, true, write},
		"a read on a full log": {func(w http.ResponseWriter, r *http.Request) {
			readFailed(w, r, full, "version 3")
		}, full, false, read},
		"a read on a node behind": {func(w http.ResponseWriter, r *http.Request) {
			readFailed(w, r, behind, "version 3")
		}, behind, true, read},
	} {
		out.Reset()
		rec := httptest.NewRecorder()
		c.answer(rec, httptest.NewRequest(http.MethodGet, "/artifacts/a", nil))
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		want, _ := builtin.ConditionSentence(c.err)
		if rec.Code != http.StatusServiceUnavailable || body["detail"] != want {
			t.Errorf("%s answered %d %v, want 503 with %q", name, rec.Code, body, want)
		}
		if text := rec.Body.String(); strings.Contains(text, "10.0.0.7") ||
			strings.Contains(text, "statelog") {
			t.Errorf("%s carried the refusal's own text: %s", name, text)
		}
		if retry := rec.Header().Get("Retry-After") != ""; retry != c.retry {
			t.Errorf("%s: Retry-After %q, want one: %v", name,
				rec.Header().Get("Retry-After"), c.retry)
		}
		level := "level=WARN"
		if c.retry {
			level = "level=INFO"
		}
		line := out.String()
		for _, want := range append([]string{level, "component=artifactsapi",
			"10.0.0.7"}, c.logged...) {
			if !strings.Contains(line, want) {
				t.Errorf("%s logged %q, want %s in it", name, line, want)
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
