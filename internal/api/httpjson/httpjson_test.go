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

// FailWith carries a route's detail, and the two RESERVED KEYS always survive
// it: a caller that branches on `error` must never find it missing because a
// route's detail happened to use the same key, and a person must never be
// shown a sentence a route wrote over the vocabulary's own.
func TestExtraFieldsNeverDisplaceTheErrorCode(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	httpjson.FailWith(rec, http.StatusConflict, httpjson.CodeInvalidBody,
		map[string]string{
			"field":   "roles[0].llm",
			"error":   "hijacked",
			"message": "whatever this route felt like saying",
		})

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body["error"] != string(httpjson.CodeInvalidBody) {
		t.Errorf("error = %q, want the code rather than the extra", body["error"])
	}
	if body["message"] != httpjson.CodeInvalidBody.Message() {
		t.Errorf("message = %q, want the code's own sentence: the copy a person "+
			"reads belongs to the vocabulary, not to the call site", body["message"])
	}
	if body["field"] != "roles[0].llm" {
		t.Errorf("field = %q, want the route's detail to survive", body["field"])
	}
}

// THE DETAIL IS STRUCTURED JSON, not a sentence and not a bag of strings.
//
// It is the half a client BRANCHES on — `retry_after_ms` is waited on,
// `missing_grant` is rendered beside the permission it names, `problems` is
// put field by field beside the inputs — and every one of those needs the
// value's own type. The old shape was flat text: a number arrived as "4000"
// for the client to parse back, and anything with structure arrived as prose
// for it to read.
//
// THE CONTROL IS THE SECOND HALF: the same fact through the text form comes
// back as a string. Route [httpjson.FailWithFields] through a
// map[string]string — which is what "detail is a flat string" meant — and the
// first half of this test goes red.
func TestDetailSurvivesAsStructuredJson(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	httpjson.FailWithFields(rec, http.StatusServiceUnavailable, httpjson.CodeUnavailable,
		httpjson.Detail{
			"retry_after_ms": 4000,
			"missing_grant":  "secrets.reveal",
			"problems": []map[string]any{
				{"path": "roles[0].llm", "segments": []any{"roles", 0, "llm"}},
			},
		})

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, rec.Body)
	}
	if body["error"] != string(httpjson.CodeUnavailable) ||
		body["message"] != httpjson.CodeUnavailable.Message() {
		t.Errorf("the envelope lost its own two keys: %v", body)
	}
	if got, isNumber := body["retry_after_ms"].(float64); !isNumber || got != 4000 {
		t.Errorf("retry_after_ms = %#v, want the number 4000: a client that has "+
			"to parse it out of a string is reading prose", body["retry_after_ms"])
	}
	if body["missing_grant"] != "secrets.reveal" {
		t.Errorf("missing_grant = %#v", body["missing_grant"])
	}
	problems, ok := body["problems"].([]any)
	if !ok || len(problems) != 1 {
		t.Fatalf("problems = %#v, want the list as it was given", body["problems"])
	}
	first, _ := problems[0].(map[string]any)
	segments, _ := first["segments"].([]any)
	if first["path"] != "roles[0].llm" || len(segments) != 3 || segments[1] != float64(0) {
		t.Errorf("problems[0] = %#v, want its shape intact", first)
	}

	// The control. The text form is still there for the detail that
	// genuinely is text — a field path, a hint — and this is what it does
	// to a number, which is the shape this test exists to keep out of the
	// structured one.
	flat := httptest.NewRecorder()
	httpjson.FailWith(flat, http.StatusServiceUnavailable, httpjson.CodeUnavailable,
		map[string]string{"retry_after_ms": "4000"})
	var text map[string]any
	if err := json.Unmarshal(flat.Body.Bytes(), &text); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if _, isNumber := text["retry_after_ms"].(float64); isNumber {
		t.Error("the text form produced a number, so this control proves nothing " +
			"about the structured one")
	}
}

// A STRUCTURED REFUSAL KEEPS ITS STRUCTURE, and `error` still wins over it.
//
// A validation refusal carries a list of problems and a derived hierarchy,
// which a map of strings cannot hold; flattening them into text would put the
// dashboard back to parsing a message to find a field.
func TestStructuredFieldsKeepTheirShapeAndNeverDisplaceTheErrorCode(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	fields := map[string]any{
		"problems": []map[string]any{{"path": "roles[0].llm", "segments": []any{"roles", 0, "llm"}}},
		"error":    "hijacked",
	}
	httpjson.FailWithFields(rec, http.StatusBadRequest, httpjson.CodeInvalidQuery, fields)

	var body struct {
		Error    string `json:"error"`
		Problems []struct {
			Path     string `json:"path"`
			Segments []any  `json:"segments"`
		} `json:"problems"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, rec.Body)
	}
	if body.Error != string(httpjson.CodeInvalidQuery) {
		t.Errorf("error = %q, want the code rather than the extra", body.Error)
	}
	if len(body.Problems) != 1 || body.Problems[0].Path != "roles[0].llm" ||
		len(body.Problems[0].Segments) != 3 || body.Problems[0].Segments[1] != float64(0) {
		t.Errorf("problems = %+v, want the list as it was given", body.Problems)
	}
	if fields["error"] != "hijacked" {
		t.Errorf("the caller's map was written to: error = %v", fields["error"])
	}
}

// declared is every code this package names, the three groups together: the
// writer's own, the query-answer set the socket shares, and the setup pass's.
//
// Hand-listed so that adding a constant without adding it here is caught by
// the count below rather than passing as "nothing to check".
var declared = []httpjson.Code{
	httpjson.CodeEncodeFailed, httpjson.CodeBodyTooLarge,
	httpjson.CodeUnreadableBody, httpjson.CodeInvalidBody,
	httpjson.CodeInvalidQuery, httpjson.CodeInternalError,
	httpjson.CodeDraining, httpjson.CodeInvalidToken,

	httpjson.CodeUnknownQuery, httpjson.CodeUnauthorized,
	httpjson.CodeQueryFailed, httpjson.CodeNotFound,
	httpjson.CodeBadParams, httpjson.CodeUnavailable,

	httpjson.CodePassInFlight, httpjson.CodeNotProvisionable,
	httpjson.CodeNoPublicBaseURL, httpjson.CodeRequirementsOutstanding,
	httpjson.CodeRunNotFound, httpjson.CodeVendorRefused,
}

// Every declared code is Valid, and an invented one is not — the guard that
// keeps a fifth spelling of "too large" from appearing.
func TestOnlyTheDeclaredCodesAreValid(t *testing.T) {
	t.Parallel()
	for _, code := range declared {
		if !code.Valid() {
			t.Errorf("%q is declared but not Valid", code)
		}
	}
	if httpjson.Code("value_too_large").Valid() {
		t.Error("a spelling this package does not define reported Valid")
	}
	if httpjson.Code("").Valid() {
		t.Error("the zero Code reported Valid, so a refusal that named no code " +
			"at all would read as a member of the vocabulary")
	}
}

// EVERY CODE CARRIES A SENTENCE A PERSON READS, because the dashboard renders
// it verbatim: whatever is written here is product copy, shown to an operator
// in a toast or beside a form.
//
// So this is shaped like copy review rather than like a type check. A message
// that reads as a log line — a bare code, a snake_case token, a fragment with
// no full stop — fails, because the alternative is that one of them reaches a
// screen and nobody notices until a person is confused by it.
func TestEveryCodeCarriesASentenceAPersonReads(t *testing.T) {
	t.Parallel()
	for _, code := range declared {
		message := code.Message()
		switch {
		case message == "":
			t.Errorf("%q has no message: a refusal a person is shown nothing for",
				code)
			continue
		case !strings.Contains(message, " "):
			t.Errorf("%q says %q, which is a token rather than a sentence",
				code, message)
		case !strings.HasSuffix(message, "."):
			t.Errorf("%q says %q, which is a fragment: it ends up mid-sentence "+
				"on a screen", code, message)
		case strings.Contains(message, "_"):
			t.Errorf("%q says %q — a snake_case token in the copy means a code "+
				"or a field name leaked into the half a person reads", code, message)
		case message[0] < 'A' || message[0] > 'Z':
			t.Errorf("%q says %q, which does not begin as a sentence", code, message)
		}
	}
	if httpjson.Code("not_on_the_table").Message() != "" {
		t.Error("a code outside the vocabulary answered a message, so Valid and " +
			"Message are reading two different tables")
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
