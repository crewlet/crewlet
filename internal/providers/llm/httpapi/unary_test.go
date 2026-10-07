package httpapi

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// respond is a middleware chain's next step answering with one response.
func respond(status int, contentType, body string) func(*http.Request) (*http.Response, error) {
	return func(*http.Request) (*http.Response, error) {
		h := http.Header{}
		if contentType != "" {
			h.Set("Content-Type", contentType)
		}
		return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}, nil
	}
}

// A STREAMING REQUEST ANSWERED UNARY IS KEPT, AND EVERYTHING ELSE PASSES
// THROUGH UNCHANGED.
//
// The SDKs read any 2xx body as an event stream, so a whole answer is lost
// unless it is taken before the decoder sees it — and a stream, or a body that
// is not the adapter's answer, must reach the decoder byte for byte, or a
// mislabelled stream stops decoding and an error body stops reading as one.
func TestAUnaryAnswerIsKeptAndEverythingElsePassesThrough(t *testing.T) {
	t.Parallel()
	isAnswer := func(body []byte) bool { return strings.HasPrefix(string(body), "{") }
	for _, tc := range []struct {
		name        string
		status      int
		contentType string
		body        string
		taken       bool
		consulted   bool
		decoderSees string
	}{
		{"a JSON answer is kept", 200, "application/json", `{"id":"1"}`, true, true, ""},
		{"an answer with no type is kept", 200, "", `{"id":"1"}`, true, true, ""},
		{"an event stream is never read", 200, "text/event-stream; charset=utf-8",
			"data: {}\n\n", false, false, "data: {}\n\n"},
		{"a body that is not the answer is handed on whole", 200, "text/plain",
			"event: x\ndata: {}\n\n", false, true, "event: x\ndata: {}\n\n"},
		{"an error status is the SDK's to read", 400, "application/json",
			`{"error":{}}`, false, false, `{"error":{}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			consulted := false
			u := NewUnaryAnswer(func(body []byte) bool {
				consulted = true
				return isAnswer(body)
			})
			req, err := http.NewRequest(http.MethodPost, "http://example.com", nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			res, err := u.Middleware(req, respond(tc.status, tc.contentType, tc.body))
			if err != nil {
				t.Fatalf("Middleware: %v", err)
			}
			got, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if u.Taken() != tc.taken || consulted != tc.consulted || string(got) != tc.decoderSees {
				t.Errorf("taken=%v consulted=%v decoder sees %q, want taken=%v consulted=%v and %q",
					u.Taken(), consulted, got, tc.taken, tc.consulted, tc.decoderSees)
			}
		})
	}
}
