package operator_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/coord"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// diskFault is a store's error as a driver words one: a database path, which
// is exactly what must reach the node's log and never a caller.
const diskFault = "open /var/lib/crewlet/replicated.db: disk I/O error"

// faultyReader is the stub tracker reader with its item read broken by err.
type faultyReader struct {
	stubWorkReader
	err error
}

func (f faultyReader) Task(context.Context, string, tracker.DetailWants,
	statelog.Freshness) (tracker.TaskDetail, error) {

	return tracker.TaskDetail{}, f.err
}

// faultySurface is the act route over a tracker whose item reads fail with err.
func faultySurface(t *testing.T, err error) http.Handler {
	t.Helper()
	return guarded(newSurface(t, operator.Options{
		Halves: fixed(operator.Halves{
			Work: builtin.WorkDeps{Reader: faultyReader{err: err},
				Writer: (&recordingWork{}).writer},
		}),
		Org: company,
	}))
}

// A FAULT OF THE NODE IS `500 internal_error`, NOT A 503. A store this node
// cannot read is not cleared by waiting, so the `503` with a two-second
// Retry-After it used to answer had a client polling a broken node for ever —
// and the sentence it carried was the store's own error, a database path, in
// the client's `detail`. The control beside it is the state log's refusal,
// which keeps its `503` and the hint the refusal derived from this node's
// own backlog.
//
// Mutation: map the fault class to 503 in classAnswers, or let readFailure
// class an unmarked error `unavailable`, and the first half goes red; class a
// state-log refusal as a fault and the second does.
func TestAStoreFaultIsAFiveHundredWithNoRetryAfter(t *testing.T) {
	t.Parallel()
	body := `{"args":{"item":"ENG-1","status":"done"}}`

	status, answer, header := actAnswer(t, faultySurface(t, errors.New(diskFault)),
		tracker.UpdateWorkItemTool, body)
	if status != http.StatusInternalServerError ||
		answer["error"] != string(crewletmcp.RefusalInternalError) {
		t.Fatalf("a store fault answered %d %v, want 500 internal_error", status, answer)
	}
	if after := header.Get("Retry-After"); after != "" {
		t.Errorf("a fault told the client to come back in %s seconds", after)
	}
	detail, _ := answer["detail"].(string)
	if strings.Contains(detail, "/var/lib") || strings.Contains(fmt.Sprint(answer), "/var/lib") {
		t.Errorf("the store's own error reached the client: %v", answer)
	}
	if !strings.Contains(detail, "log") || answer["tool"] != tracker.UpdateWorkItemTool {
		t.Errorf("the fault's answer %v does not say where its reason is, or for "+
			"which tool", answer)
	}

	behind := &statelog.Refused{Code: statelog.RefuseBehind, Level: statelog.ReadSession,
		RetryAfter: 9 * time.Second, Detail: "this node is 40 records behind"}
	status, answer, header = actAnswer(t, faultySurface(t, behind),
		tracker.UpdateWorkItemTool, body)
	if status != http.StatusServiceUnavailable ||
		answer["error"] != string(crewletmcp.RefusalUnavailable) {
		t.Fatalf("a read the state log refused answered %d %v, want 503 unavailable",
			status, answer)
	}
	if after := header.Get("Retry-After"); after != "9" {
		t.Errorf("Retry-After = %q, want the refusal's own 9 seconds", after)
	}
}

// A WRITER'S ERROR IS WORDED FOR WHOEVER READS IT, on the surfaces that call a
// writer without a tool. The human write surface handed [operator.Fail] the
// error's own text whatever it was, so a disk error's path was the `detail`;
// [operator.FailWrite] tells a fault in fixed words and leaves its error to
// the log, a condition in its own words with its hint, and the domain's
// refusal as the domain composed it.
func TestAWritersErrorIsWordedForWhoeverReadsIt(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		err    error
		status int
		code   httpjson.Code
		says   string
		retry  bool
	}{
		{"a fault of the node", fmt.Errorf("tracker: read the ledger: %s", diskFault),
			http.StatusInternalServerError, httpjson.CodeInternalError, "node's log", false},
		{"a coordination store that did not answer",
			fmt.Errorf("dial nats://10.0.0.7:4222: %w", coord.ErrUnavailable),
			http.StatusServiceUnavailable, httpjson.CodeUnavailable,
			"the coordination store did not answer", true},
		{"a refusal the domain composed",
			fmt.Errorf("%w: the comment is longer than a comment may be", tracker.ErrInvalid),
			http.StatusUnprocessableEntity, httpjson.CodeInvalid, "longer than", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			operator.FailWrite(t.Context(), rec, tc.err, newKey(), nil)
			body := decode(t, rec)
			if rec.Code != tc.status || body["error"] != string(tc.code) {
				t.Fatalf("answered %d %v, want %d %s", rec.Code, body, tc.status, tc.code)
			}
			detail, _ := body["detail"].(string)
			if !strings.Contains(detail, tc.says) {
				t.Errorf("detail = %q, want it to say %q", detail, tc.says)
			}
			for _, never := range []string{"/var/lib", "10.0.0.7"} {
				if strings.Contains(detail, never) {
					t.Errorf("detail = %q carries the error's own %q", detail, never)
				}
			}
			after := rec.Header().Get("Retry-After")
			if seconds, err := strconv.Atoi(after); tc.retry != (err == nil && seconds > 0) {
				t.Errorf("Retry-After = %q, want one only for a condition", after)
			}
		})
	}
}

// THE CLASS OF A WRITER'S ERROR IS THE TOOLS' OWN RULE: a content refusal the
// domain marked keeps its class, a condition waiting clears is `unavailable`,
// a walk that stopped at a step nobody can confirm is `unavailable` — its
// tracker sentence says the same operation finishes it — and anything
// unmarked is a fault.
func TestAWritersErrorIsClassedByTheToolsRule(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err  error
		want crewletmcp.Refusal
	}{
		{tracker.ErrNoTask, crewletmcp.RefusalNotFound},
		{&statelog.Unavailable{Reason: statelog.ReasonLogFull}, crewletmcp.RefusalUnavailable},
		{fmt.Errorf("x: %w", coord.ErrUnavailable), crewletmcp.RefusalUnavailable},
		{fmt.Errorf("stopped: %w", tracker.ErrStepUnresolved), crewletmcp.RefusalUnavailable},
		{errors.New(diskFault), crewletmcp.RefusalInternalError},
	} {
		got := crewletmcp.RefusalOf(crewletmcp.Result{Failed: true,
			Cause: operator.ClassifyWrite(tc.err)})
		if got != tc.want {
			t.Errorf("ClassifyWrite(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// decode is a recorded answer's JSON body.
func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("answered %d with a body that is not JSON: %q", rec.Code, rec.Body)
	}
	return body
}

// actAnswer is [act] as the founder, under a fresh key, with the headers.
func actAnswer(t *testing.T, h http.Handler, tool, body string) (int, map[string]any,
	http.Header) {

	t.Helper()
	r := httptest.NewRequest(http.MethodPost, operator.ActPathPrefix+tool, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer founder")
	r.Header.Set(opkey.Header, newKey())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code, decode(t, rec), rec.Header()
}
