package secretsapi_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/secretsapi"
	"github.com/crewlet/crewlet/internal/secrets"
)

// revealBody spells a POST /secrets/reveal body naming these variables.
func revealBody(t *testing.T, names ...string) string {
	t.Helper()
	raw, err := json.Marshal(map[string][]string{"names": names})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// SEVERAL VALUES ARE REVEALED IN ONE ANSWER, and exactly the ones asked.
//
// It is how a CLI run resolving a company document through a running node is
// handed the variables its command reads that the node lists, a batch at a
// time and exactly those: `values` for the names held and `missing` for the
// names nothing holds — a name nothing holds is absent from `values`, never
// present and empty, because an empty value is a value and a reader that took
// it for one would stop resolving the name from anywhere else — and a name
// nobody asked for is not in the answer at all. And the answer is a
// credential, so it is never cacheable.
func TestSeveralValuesAreRevealedInOneAnswer(t *testing.T) {
	t.Parallel()
	h, _ := surface(t, cipherFor(t, "k1"), "k1")
	for name, value := range map[string]string{"A": "first", "B": "", "UNASKED": "third"} {
		if code, body := call(t, h, http.MethodPut, "/secrets/"+name, value); code != http.StatusOK {
			t.Fatalf("seed %s = %d %s", name, code, body)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/secrets/reveal",
		strings.NewReader(revealBody(t, "B", "A", "NOPE", "A"))))
	if rec.Code != http.StatusOK {
		t.Fatalf("reveal = %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Values  map[string]string `json:"values"`
		Missing []string          `json:"missing"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	if len(got.Values) != 2 || got.Values["A"] != "first" {
		t.Errorf("values = %v, want A and B and nothing else", got.Values)
	}
	if value, held := got.Values["B"]; !held || value != "" {
		t.Errorf("B, stored empty, came back (%q, held=%t): a stored empty value "+
			"is a value", value, held)
	}
	if len(got.Missing) != 1 || got.Missing[0] != "NOPE" {
		t.Errorf("missing = %v, want the one name nothing holds", got.Missing)
	}
	if strings.Contains(rec.Body.String(), "third") {
		t.Errorf("a value nobody asked for was revealed: %s", rec.Body.String())
	}
	if cache := rec.Header().Get("Cache-Control"); cache != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cache)
	}
}

// A REVEAL OF SEVERAL NAMES REFUSES WHAT A DOCUMENT COULD NOT HAVE ASKED, each
// in the engine's envelope and before anything is opened: no names, more than
// one request reads, a field it does not read, a query, a name no reference
// can hold, and the engine's own key.
func TestARevealOfSeveralNamesRefusesWhatNoDocumentAsks(t *testing.T) {
	t.Parallel()
	h, _ := surface(t, cipherFor(t, "k1"), "k1")
	if code, body := call(t, h, http.MethodPut, "/secrets/A", "first"); code != http.StatusOK {
		t.Fatalf("seed = %d %s", code, body)
	}
	tooMany := make([]string, secretsapi.MaxRevealNames+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("NAME_%d", i)
	}
	for _, tc := range []struct {
		name, path, body string
		status           int
		code             httpjson.Code
	}{
		{"no names", "/secrets/reveal", revealBody(t), http.StatusBadRequest,
			httpjson.CodeInvalidBody},
		{"more names than one request reads", "/secrets/reveal",
			revealBody(t, tooMany...), http.StatusBadRequest, httpjson.CodeInvalidBody},
		{"a field it does not read", "/secrets/reveal",
			`{"names":["A"],"all":true}`, http.StatusBadRequest, httpjson.CodeInvalidBody},
		{"a body that is not JSON", "/secrets/reveal", "A", http.StatusBadRequest,
			httpjson.CodeInvalidBody},
		{"a query", "/secrets/reveal?names=A", revealBody(t, "A"),
			http.StatusBadRequest, httpjson.CodeInvalidQuery},
		{"a name no reference can hold", "/secrets/reveal",
			revealBody(t, "A", "gitlab-token"), http.StatusBadRequest,
			httpjson.CodeInvalidName},
		{"the engine's own key", "/secrets/reveal",
			revealBody(t, "A", "iam/blind-index-key"), http.StatusForbidden,
			httpjson.CodeReservedName},
	} {
		code, raw := call(t, h, http.MethodPost, tc.path, tc.body)
		var body map[string]any
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Errorf("%s: the refusal is not JSON: %s", tc.name, raw)
			continue
		}
		if code != tc.status || body["error"] != string(tc.code) {
			t.Errorf("%s answered %d %v, want %d %s", tc.name, code,
				body["error"], tc.status, tc.code)
		}
		if body["message"] != tc.code.Message() {
			t.Errorf("%s carries message %q, want the vocabulary's sentence",
				tc.name, body["message"])
		}
		if strings.Contains(raw, "first") {
			t.Errorf("%s was refused carrying a value: %s", tc.name, raw)
		}
	}
}

// A BODY THAT DOES NOT DECODE IS TOLD IN WORDS ([plainjson.Unreadable]): which
// field held what where something else belongs, or a field the route does not
// read, never encoding/json's own sentence, which names Go's struct and the
// field's Go type ("json: cannot unmarshal string into Go struct field
// revealRequest.names of type []string"). Mutation: answer err.Error() again
// and every row goes red.
func TestARevealBodyThatDoesNotDecodeIsToldInWords(t *testing.T) {
	t.Parallel()
	h, _ := surface(t, cipherFor(t, "k1"), "k1")
	for _, tc := range []struct{ body, says string }{
		{`{"names":"A"}`, `In the body, "names" holds text where a list belongs.`},
		{`{"names":["A"],"all":true}`, `In the body, "all" isn't a field this takes.`},
		{`[1]`, "The body is a list where an object belongs."},
	} {
		code, raw := call(t, h, http.MethodPost, "/secrets/reveal", tc.body)
		var body map[string]any
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Errorf("%s: the refusal is not JSON: %s", tc.body, raw)
			continue
		}
		if code != http.StatusBadRequest || body["error"] != string(httpjson.CodeInvalidBody) ||
			body["detail"] != tc.says {
			t.Errorf("%s answered %d %v, want 400 invalid_body saying %q", tc.body,
				code, body, tc.says)
		}
	}
}

// THE LONGEST ANSWER FITS THE BOUND A READER IS GIVEN.
//
// [secretsapi.MaxRevealAnswerBytes] is what a client is told to be prepared to
// read, so it has to hold the worst answer the route can write: every name it
// takes, each as long as the body allows, each holding the largest value the
// write admits in the characters JSON spells longest. Mutation: count a value
// at its stored length rather than its escaped one, and this answer overruns
// the bound — a client holding to it would refuse a read the write allowed.
func TestTheLongestRevealFitsTheBoundAReaderIsGiven(t *testing.T) {
	t.Parallel()
	h, _ := surface(t, cipherFor(t, "k1"), "k1")
	value := strings.Repeat("<", secrets.MaxValueBytes)
	// AS LONG AS THE BODY LETS EVERY NAME BE: the envelope's own bytes
	// come off the cap before it is shared out.
	room := (secretsapi.MaxRevealBodyBytes-len(`{"names":[]}`))/secretsapi.MaxRevealNames - len(`"",`)
	names := make([]string, secretsapi.MaxRevealNames)
	for i := range names {
		prefix := fmt.Sprintf("N%d_", i)
		names[i] = prefix + strings.Repeat("X", room-len(prefix))
		if code, body := call(t, h, http.MethodPut, "/secrets/"+names[i], value); code != http.StatusOK {
			t.Fatalf("seed %d = %d %s", i, code, body)
		}
	}
	body := revealBody(t, names...)
	if len(body) > secretsapi.MaxRevealBodyBytes {
		t.Fatalf("the fixture's body is %d bytes, over the %d the route reads",
			len(body), secretsapi.MaxRevealBodyBytes)
	}
	code, raw := call(t, h, http.MethodPost, "/secrets/reveal", body)
	if code != http.StatusOK {
		t.Fatalf("reveal = %d %.300s", code, raw)
	}
	if len(raw) > secretsapi.MaxRevealAnswerBytes {
		t.Errorf("the answer is %d bytes, over MaxRevealAnswerBytes (%d)",
			len(raw), secretsapi.MaxRevealAnswerBytes)
	}
}

// AND THE LONGEST SINGLE REVEAL FITS THE BOUND ITS READER IS GIVEN.
//
// `GET /secrets/{name}?reveal=true` answers the value as a JSON string too, so
// the largest value the write admits, in the characters JSON spells longest,
// comes back six times its stored length. A reader held to the write's limit
// refused it: a value the store took and the break-glass read could not hand
// back. Mutation: count the value at its stored length in RevealAnswerBytes,
// and this answer overruns it.
func TestTheLongestSingleRevealFitsTheBoundAReaderIsGiven(t *testing.T) {
	t.Parallel()
	h, _ := surface(t, cipherFor(t, "k1"), "k1")
	const name = "PEM_KEY"
	value := strings.Repeat("<", secrets.MaxValueBytes)
	if code, body := call(t, h, http.MethodPut, "/secrets/"+name, value); code != http.StatusOK {
		t.Fatalf("seed = %d %s", code, body)
	}
	code, raw := call(t, h, http.MethodGet, "/secrets/"+name+"?reveal=true", "")
	if code != http.StatusOK {
		t.Fatalf("reveal = %d %.300s", code, raw)
	}
	if len(raw) > secretsapi.RevealAnswerBytes(name) {
		t.Errorf("the answer is %d bytes, over RevealAnswerBytes (%d)",
			len(raw), secretsapi.RevealAnswerBytes(name))
	}
}
