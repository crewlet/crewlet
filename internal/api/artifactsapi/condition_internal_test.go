package artifactsapi

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
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/objstore"
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

// THE FILE STORE'S TWO CONDITIONS ARE NO FAULT OF THIS NODE'S, and are read as
// a project file's upload reads them: a full store is `503` with no
// Retry-After, in the words that say who makes room and a message that says
// waiting won't help, where it fell to the fault's `500`; and a store that did
// not take the bytes is `503` under an election's wait, which this route also
// answered as a fault. Mutation: drop the store's classification of either and
// its row fails; write the code's own message under no Retry-After and the
// first row does.
func TestAFullStoreSaysWhoMakesRoom(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		err          error
		says, retry  string
		messageWaits bool
	}{
		"a full store": {fmt.Errorf("artifactupload: put: objstore: store the object: %w",
			objstore.ErrFull), objstore.FullWords, "", false},
		"a store that did not answer": {fmt.Errorf("artifactupload: put: objstore: store "+
			"the object: %w: %w", objstore.ErrUnanswered, errors.New("s3: 503 SlowDown")),
			"The company's files couldn't be saved just now. Try again in a moment.", "4", true},
	} {
		rec := httptest.NewRecorder()
		writeFailed(rec, httptest.NewRequest(http.MethodPut, "/artifacts/uploads/u/files/a", nil),
			"op-1", "transfer the file", c.err)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusServiceUnavailable || body["detail"] != c.says {
			t.Errorf("%s answered %d %v, want 503 saying %q", name, rec.Code, body, c.says)
		}
		if got := rec.Header().Get("Retry-After"); got != c.retry {
			t.Errorf("%s: Retry-After %q, want %q", name, got, c.retry)
		}
		message, _ := body["message"].(string)
		if waits := strings.Contains(message, "in a moment"); waits != c.messageWaits {
			t.Errorf("%s: the message %q invites a wait: %v, want %v", name, message, waits,
				c.messageWaits)
		}
		if text := rec.Body.String(); strings.Contains(text, "objstore") ||
			strings.Contains(text, "SlowDown") {
			t.Errorf("%s carried its error's own text: %s", name, text)
		}
	}
}

// A FAILURE NO CONDITION CLAIMS IS THE NODE'S OWN FAULT: `500` with no
// Retry-After and none of its own text, on a read and a write alike — a read's
// was a 503 telling a person to come back for it. Mutation: answer a read's
// fault as a 503 again and its row fails.
func TestAFaultIsTheNodesOwn(t *testing.T) {
	t.Parallel()
	fault := fmt.Errorf("artifactsdomain: read /var/lib/crewlet/replicated.db: disk I/O")
	for name, answer := range map[string]func(http.ResponseWriter, *http.Request){
		"a read's fault": func(w http.ResponseWriter, r *http.Request) {
			readFailed(w, r, fault, "version 3")
		},
		"a write's fault": func(w http.ResponseWriter, r *http.Request) {
			writeFailed(w, r, "op-1", "publish", fault)
		},
	} {
		rec := httptest.NewRecorder()
		answer(rec, httptest.NewRequest(http.MethodGet, "/artifacts/a", nil))
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusInternalServerError || body["error"] != "internal_error" {
			t.Errorf("%s answered %d %v, want 500 internal_error", name, rec.Code, body)
		}
		if got := rec.Header().Get("Retry-After"); got != "" {
			t.Errorf("%s: Retry-After %q, and no wait clears it", name, got)
		}
		if text := rec.Body.String(); strings.Contains(text, "/var/lib") {
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
