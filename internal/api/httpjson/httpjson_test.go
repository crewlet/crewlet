package httpjson_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
)

// A JSON SURFACE ANSWERS AS JSON, headers included.
//
// This is the trap that was live at five call sites: net/http.Error sets
// Content-Type text/plain AND X-Content-Type-Options nosniff, so a JSON
// literal handed to it produces the one combination guaranteed to stop a
// strict client parsing the body — the body says JSON, the headers swear
// otherwise, and the sniffing that would paper over it is switched off.
func TestEveryAnswerCarriesTheJSONContentType(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		write func(http.ResponseWriter)
		want  int
	}{
		{"a body", func(w http.ResponseWriter) {
			httpjson.Write(w, http.StatusOK, map[string]string{"status": "ok"})
		}, http.StatusOK},
		{"a failure", func(w http.ResponseWriter) {
			httpjson.Fail(w, http.StatusBadRequest, httpjson.CodeInvalidBody)
		}, http.StatusBadRequest},
		{"a body that will not marshal", func(w http.ResponseWriter) {
			httpjson.Write(w, http.StatusOK, make(chan int))
		}, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			tc.write(rec)

			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			if got := rec.Header().Get("X-Content-Type-Options"); got != "" {
				t.Errorf("X-Content-Type-Options = %q: nosniff on a JSON body is "+
					"what stops a strict client parsing it", got)
			}
			var into map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &into); err != nil {
				t.Errorf("body is not JSON: %v (%q)", err, rec.Body.String())
			}
		})
	}
}

// AN UNMARSHALABLE BODY STILL ANSWERS A CODE the caller can branch on, and
// does not answer 200 with nothing.
func TestAnEncodeFailureIsStillAJSONError(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	httpjson.Write(rec, http.StatusOK, make(chan int))

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body["error"] != string(httpjson.CodeEncodeFailed) {
		t.Errorf("error = %q, want %q", body["error"], httpjson.CodeEncodeFailed)
	}
}

// ONE SPELLING OF A 413, whichever surface refused. Three of them had drifted
// — body_too_large, value_too_large, and "payload too large" as plain text —
// and a client cannot branch on a vocabulary that depends on the route.
func TestABodyOverTheCapIsAlwaysTheSameCode(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("0123456789"))

	_, err := httpjson.ReadBody(rec, req, 4)
	if err == nil {
		t.Fatal("a body over the cap was accepted")
	}
	httpjson.Refuse(rec, err)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
	var body map[string]string
	if jsonErr := json.Unmarshal(rec.Body.Bytes(), &body); jsonErr != nil {
		t.Fatalf("body is not JSON: %v", jsonErr)
	}
	if body["error"] != string(httpjson.CodeBodyTooLarge) {
		t.Errorf("error = %q, want %q", body["error"], httpjson.CodeBodyTooLarge)
	}
}

// A BODY WITHIN THE CAP COMES BACK WHOLE, so the cap is a bound rather than a
// truncation nobody is told about.
func TestABodyWithinTheCapIsReadWhole(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("hello"))

	got, err := httpjson.ReadBody(rec, req, 1024)
	if err != nil {
		t.Fatalf("ReadBody: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("body = %q, want %q", got, "hello")
	}
}

// FailWith carries a route's extras, and `error` always survives them: a
// caller that branches on it must never find it missing because a route's
// extra map happened to use the same key.
func TestExtraFieldsNeverDisplaceTheErrorCode(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	httpjson.FailWith(rec, http.StatusConflict, httpjson.CodeInvalidBody,
		map[string]string{"field": "roles[0].llm", "error": "hijacked"})

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body["error"] != string(httpjson.CodeInvalidBody) {
		t.Errorf("error = %q, want the code rather than the extra", body["error"])
	}
	if body["field"] != "roles[0].llm" {
		t.Errorf("field = %q, want the route's extra to survive", body["field"])
	}
}

// Every declared code is Valid, and an invented one is not — the guard that
// keeps a fifth spelling of "too large" from appearing.
func TestOnlyTheDeclaredCodesAreValid(t *testing.T) {
	t.Parallel()
	for _, code := range []httpjson.Code{
		httpjson.CodeEncodeFailed, httpjson.CodeBodyTooLarge,
		httpjson.CodeUnreadableBody, httpjson.CodeInvalidBody,
		httpjson.CodeInternalError,
	} {
		if !code.Valid() {
			t.Errorf("%q is declared but not Valid", code)
		}
	}
	if httpjson.Code("value_too_large").Valid() {
		t.Error("a spelling this package does not define reported Valid")
	}
}

// A DRIBBLING CLIENT IS CUT OFF, which a size cap cannot do.
//
// The cap stops a client sending too much; nothing stopped one sending very
// little, very slowly. A request that stays under every byte limit and simply
// never finishes held a handler goroutine and a connection slot for as long as
// the client cared to keep it — on the one surface an unauthenticated caller
// can reach.
//
// Asserted by CAPTURING the deadline rather than by waiting for it to fire:
// waiting would put [httpjson.BodyReadTimeout] of wall clock into every run of
// the suite to re-prove something net/http already guarantees. What is this
// package's own is that the deadline is set at all, and when.
func TestTheBodyReadCarriesADeadline(t *testing.T) {
	t.Parallel()
	w := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"a":1}`))

	before := time.Now()
	if _, err := httpjson.ReadBody(w, r, 1<<20); err != nil {
		t.Fatalf("ReadBody: %v", err)
	}
	if w.deadline.IsZero() {
		t.Fatal("the body was read with no read deadline: a client that " +
			"dribbles holds the handler and its connection slot indefinitely")
	}
	// Bounded on both sides, so a deadline of "now" or of an hour would
	// both fail: the first cuts off every real client, the second is not a
	// bound on the trickle this exists to stop.
	if got := w.deadline.Sub(before); got < httpjson.BodyReadTimeout ||
		got > httpjson.BodyReadTimeout+time.Second {
		t.Errorf("read deadline set %v out, want %v", got, httpjson.BodyReadTimeout)
	}
}

// A READ DEADLINE THAT CANNOT BE SET IS REPORTED, not swallowed.
//
// Only http.ErrNotSupported is ignorable — it means the writer has no
// connection. Any other failure is a connection in a state the read should not
// be attempted on.
func TestADeadlineFailureRefusesTheRead(t *testing.T) {
	t.Parallel()
	boom := errors.New("connection is gone")
	w := &deadlineWriter{ResponseRecorder: httptest.NewRecorder(), err: boom}
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"a":1}`))

	if _, err := httpjson.ReadBody(w, r, 1<<20); !errors.Is(err, boom) {
		t.Errorf("ReadBody = %v, want the deadline failure", err)
	}
}

// deadlineWriter is a recorder that can carry a read deadline, which is what
// http.NewResponseController looks for.
type deadlineWriter struct {
	*httptest.ResponseRecorder
	deadline time.Time
	err      error
}

func (w *deadlineWriter) SetReadDeadline(t time.Time) error {
	if w.err != nil {
		return w.err
	}
	w.deadline = t
	return nil
}

// A RECORDER HAS NO CONNECTION, and that must not fail the read.
//
// http.NewResponseController reports http.ErrNotSupported for a
// ResponseWriter that cannot carry a deadline — every httptest.ResponseRecorder
// and every wrapping writer in the tree. Treating that as a read failure would
// break every handler under test for a property a recorder cannot violate.
func TestAWriterWithNoDeadlineStillReads(t *testing.T) {
	t.Parallel()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"a":1}`))

	got, err := httpjson.ReadBody(w, r, 1<<20)
	if err != nil {
		t.Fatalf("ReadBody against a recorder: %v", err)
	}
	if string(got) != `{"a":1}` {
		t.Errorf("read %q, want the whole body", got)
	}
}
