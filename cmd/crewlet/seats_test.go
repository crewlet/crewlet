package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// fakeActNode answers the act transport, recording what it was sent.
type fakeActNode struct {
	server      *httptest.Server
	tool        string
	contentType string
	body        struct {
		RequestID string         `json:"request_id"`
		Args      map[string]any `json:"args"`
	}
	status  int
	answer  map[string]any
	refusal map[string]string

	// hangUp drops the connection without an answer, as a node that died
	// mid-request or a proxy that gave up does, handing the request id it
	// was sent to hungUp first: a dropped connection carries nothing a
	// reader of the fields above could synchronise on.
	hangUp bool
	hungUp chan string
}

func newFakeActNode(t *testing.T) *fakeActNode {
	t.Helper()
	n := &fakeActNode{status: http.StatusOK, hungUp: make(chan string, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /operator/act/{tool}", func(w http.ResponseWriter, r *http.Request) {
		n.tool, n.contentType = r.PathValue("tool"), r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &n.body)
		if n.hangUp {
			n.hungUp <- n.body.RequestID
			conn, _, err := http.NewResponseController(w).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(n.status)
		if n.refusal != nil {
			_ = json.NewEncoder(w).Encode(n.refusal)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tool": n.tool, "outcome": n.answer["outcome"], "position": nil, "receipt": n.answer,
		})
	})
	n.server = httptest.NewServer(mux)
	t.Cleanup(n.server.Close)
	return n
}

// A PAUSE FROM A SHELL IS THE DASHBOARD'S GESTURE, over its route.
//
// The same tool, the same act transport, a JSON body with a fresh request id
// and the arguments the tool reads — so a pause from a terminal and one from a
// profile screen are one gesture with one record, attributed to the person the
// token is bound to.
func TestSeatsPauseActsThroughTheActTransport(t *testing.T) {
	t.Parallel()
	node := newFakeActNode(t)
	node.answer = map[string]any{"handle": "swe", "outcome": "applied", "changed": true,
		"paused_by": "jane-token", "paused_by_seat": "jane", "paused_at": "2026-09-25T09:00:00Z",
		"stop_running": true}
	stdout, stderr, err := cli(t, "seats", "pause", "@swe", "-stop",
		"-reason", "looping on one ticket", bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatalf("seats pause: %v\n%s", err, stderr)
	}
	if node.tool != "pause_seat" || node.contentType != "application/json" {
		t.Errorf("posted to %q as %q, want pause_seat as application/json", node.tool, node.contentType)
	}
	if id, err := uuid.Parse(node.body.RequestID); err != nil || id.Version() != 7 {
		t.Errorf("request_id %q is not a UUIDv7: the act transport refuses the call", node.body.RequestID)
	}
	if node.body.Args["handle"] != "swe" || node.body.Args["stop_running"] != true ||
		node.body.Args["reason"] != "looping on one ticket" {
		t.Errorf("args = %v, want the handle, the stop and the reason", node.body.Args)
	}
	if !strings.Contains(stdout, "paused swe (by jane") || !strings.Contains(stdout, "next round") {
		t.Errorf("stdout does not say who paused it and that the turn stops:\n%s", stdout)
	}
}

// A RESUME CARRIES ONLY THE HANDLE, and a retry names the request it repeats.
func TestSeatsResumeRetriesUnderTheSameRequest(t *testing.T) {
	t.Parallel()
	node := newFakeActNode(t)
	node.answer = map[string]any{"handle": "swe", "outcome": "unknown"}
	stdout, _, err := cli(t, "seats", "resume", "swe", bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatalf("seats resume: %v", err)
	}
	if node.tool != "resume_seat" || len(node.body.Args) != 1 {
		t.Errorf("posted %v to %q, want only the handle to resume_seat", node.body.Args, node.tool)
	}
	first := node.body.RequestID
	if !strings.Contains(stdout, "-request-id "+first) {
		t.Fatalf("an unknown outcome did not print the id to retry with:\n%s", stdout)
	}
	if _, _, err := cli(t, "seats", "resume", "swe", "-request-id", first,
		bootstrapForURL(t, node.server.URL)); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if node.body.RequestID != first {
		t.Errorf("the retry sent %q, want the first attempt's %q", node.body.RequestID, first)
	}
}

// EVERY OUTCOME NOBODY CAN VOUCH FOR NAMES THE REQUEST THAT FINISHES IT.
//
// There are two besides a 200 that says `unknown` itself, and neither looks
// like one. The act transport answers a write nobody can vouch for — a tool's
// unknown write, or a call cut short — with 503 `unavailable` and `outcome:
// unknown`, whose class alone is a refusal's; read as one, it told a person
// "this node cannot serve that" and never the id their retry needs. And a
// node that never answered may have paused the seat anyway. The request id is
// minted inside this command, so if it does not print it, nothing can.
func TestSeatsTellTheRequestToRetryWheneverTheOutcomeIsUnknown(t *testing.T) {
	t.Run("answered unknown", func(t *testing.T) {
		node := newFakeActNode(t)
		node.status = http.StatusServiceUnavailable
		node.refusal = map[string]string{"error": "unavailable", "tool": "pause_seat",
			"outcome": "unknown",
			"detail":  "the call was interrupted before it answered"}
		stdout, _, err := cli(t, "seats", "pause", "swe", bootstrapForURL(t, node.server.URL))
		if err != nil {
			t.Fatalf("an outcome the node answered unknown was reported as a failure: %v", err)
		}
		if !strings.Contains(stdout, "-request-id "+node.body.RequestID) ||
			!strings.Contains(stdout, "interrupted before it answered") {
			t.Errorf("the answer does not name the request to retry and the node's "+
				"reason:\n%s", stdout)
		}
	})
	t.Run("not answered", func(t *testing.T) {
		node := newFakeActNode(t)
		node.hangUp = true
		_, stderr, err := cli(t, "seats", "pause", "swe", bootstrapForURL(t, node.server.URL))
		if err == nil {
			t.Fatal("a request nobody answered was reported as done")
		}
		sent := <-node.hungUp
		if sent == "" || !strings.Contains(stderr, "-request-id "+sent) {
			t.Errorf("a pause nobody answered does not name the request to retry:\n%s", stderr)
		}
	})
}

// AN UNBOUND TOKEN IS TOLD WHICH SEAT TO BIND IT ON, not that it is wrong:
// it is a valid credential that names nobody, and the node's hint is the fix.
func TestSeatsUnderAnUnboundTokenCarryTheRemedy(t *testing.T) {
	t.Parallel()
	node := newFakeActNode(t)
	node.status = http.StatusForbidden
	node.refusal = map[string]string{"error": "unbound", "tool": "pause_seat",
		"detail": `the token "ci" is bound to no seat, so there is no person to act as`,
		"hint":   "give a human seat contact.crewlet_operator_id: ci"}
	_, _, err := cli(t, "seats", "pause", "swe", bootstrapForURL(t, node.server.URL))
	if err == nil || !strings.Contains(err.Error(), "contact.crewlet_operator_id: ci") ||
		strings.Contains(err.Error(), "check it against the") {
		t.Errorf("err = %v, want the node's remedy rather than a bad-token guess", err)
	}
}

// A GESTURE A SEAT CANNOT BE NAMED FOR is refused before anything is sent.
func TestSeatsNeedsAHandle(t *testing.T) {
	t.Parallel()
	node := newFakeActNode(t)
	_, _, err := cli(t, "seats", "pause", "-url", node.server.URL, "-token", "t")
	if err == nil || !strings.Contains(err.Error(), "name the seat") {
		t.Fatalf("a pause naming no seat = %v, want it refused for want of a handle", err)
	}
	if node.tool != "" {
		t.Errorf("a pause naming no seat reached the node as %q", node.tool)
	}
}
