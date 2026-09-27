package webhooks_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/crewlet/crewlet/internal/api/httpjson"
)

// EVERY REFUSAL ON THE WEBHOOK EDGE IS THE ENGINE'S ONE ENVELOPE.
//
// The edge answered `{"error": "invalid signature"}` — a code with a space in
// it that no client could find in any vocabulary, and no sentence — and
// `{"status": "unavailable", "reason": …}`, an envelope of its own. A vendor
// reads only the status and the Retry-After, and both are pinned here
// unchanged; what the envelope buys is the operator reading the body in the
// vendor's delivery log, a proxy's access log or the engine's own. So each
// refusal is asserted by its status, its Retry-After, a code ON THE TABLE and
// the sentence that code carries.
func TestEveryWebhookRefusalIsTheEnginesEnvelope(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		path       string
		headers    map[string]string
		arrange    func(*edge)
		status     int
		code       httpjson.Code
		retryAfter string
	}{
		{name: "a forged signature", path: "/webhooks/github",
			headers: githubDelivery(issueBody, "wrong"),
			status:  http.StatusUnauthorized, code: httpjson.CodeInvalidSignature},
		{name: "a Forge delivery with no invocation token", path: "/webhooks/forge",
			status: http.StatusUnauthorized, code: httpjson.CodeInvalidSignature},
		{name: "a per-seat delivery to a seat with no app", path: "/webhooks/slack/nobody",
			status: http.StatusUnauthorized, code: httpjson.CodeInvalidSignature},
		{name: "a node with no active revision", path: "/webhooks/github",
			headers: githubDelivery(issueBody, "gh-secret"),
			arrange: func(e *edge) { *e.configured = false },
			status:  http.StatusServiceUnavailable, code: httpjson.CodeNoActiveRevision,
			retryAfter: "15"},
		{name: "a route with no secret", path: "/webhooks/github",
			headers: githubDelivery(issueBody, "gh-secret"),
			arrange: func(e *edge) { e.secrets.GitHub = "" },
			status:  http.StatusServiceUnavailable, code: httpjson.CodeNoWebhookSecret,
			retryAfter: "300"},
		{name: "a GitLab signing key in a form GitLab never signs with", path: "/webhooks/gitlab",
			arrange: func(e *edge) { e.secrets.GitLab = "a-plain-string" },
			status:  http.StatusServiceUnavailable, code: httpjson.CodeUnusableWebhookSecret,
			retryAfter: "300"},
		{name: "a shared token too short to be the check", path: "/webhooks/datadog",
			headers: datadogDelivery("letmein"),
			arrange: func(e *edge) { e.secrets.Datadog = "letmein" },
			status:  http.StatusServiceUnavailable, code: httpjson.CodeUnusableWebhookSecret,
			retryAfter: "300"},
		{name: "a verified delivery the broker would not take", path: "/webhooks/github",
			headers: githubDelivery(issueBody, "gh-secret"),
			arrange: func(e *edge) { e.published.fail(errors.New("nats: connection closed")) },
			status:  http.StatusServiceUnavailable, code: httpjson.CodeUnavailable,
			retryAfter: "15"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEdge(t)
			if tc.arrange != nil {
				tc.arrange(e)
			}
			body := issueBody
			if tc.path == "/webhooks/datadog" {
				body = []byte(`{"id":"n-1","title":"CPU high","alert_transition":"Triggered"}`)
			}
			res := e.post(t, tc.path, body, tc.headers)
			if res.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", res.Code, tc.status, res.Body)
			}
			if got := res.Header().Get("Retry-After"); got != tc.retryAfter {
				t.Errorf("Retry-After = %q, want %q", got, tc.retryAfter)
			}
			if got := res.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want the envelope's", got)
			}
			var answer map[string]any
			if err := json.Unmarshal(res.Body.Bytes(), &answer); err != nil {
				t.Fatalf("the refusal is not JSON: %v: %s", err, res.Body)
			}
			if answer["error"] != string(tc.code) || !tc.code.Valid() {
				t.Errorf("error = %v, want %q from the engine's table", answer["error"], tc.code)
			}
			if answer["message"] != tc.code.Message() {
				t.Errorf("message = %v, want the sentence the table carries for %s",
					answer["message"], tc.code)
			}
			if _, private := answer["status"]; private {
				t.Errorf("the refusal still carries the edge's own `status` key: %v", answer)
			}
		})
	}
}
