package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/statelog"
)

// fakeActNode answers the act transport, recording what it was sent.
type fakeActNode struct {
	server        *httptest.Server
	tool          string
	contentType   string
	authorization string
	key           string
	body          struct {
		Args map[string]any `json:"args"`
	}
	raw     map[string]json.RawMessage
	status  int
	answer  map[string]any
	refusal map[string]any
}

func newFakeActNode(t *testing.T) *fakeActNode {
	t.Helper()
	n := &fakeActNode{status: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /operator/act/{tool}", func(w http.ResponseWriter, r *http.Request) {
		n.tool, n.contentType = r.PathValue("tool"), r.Header.Get("Content-Type")
		n.authorization, n.key = r.Header.Get("Authorization"), r.Header.Get(opkey.Header)
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &n.body)
		n.raw = nil
		_ = json.Unmarshal(raw, &n.raw)
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
// The same tool, the same act transport: the arguments the tool reads as
// `{"args": …}` and nothing else beside them (the transport decodes with
// unknown fields refused), the operation key in the header it requires, and
// the credential from the environment — so a pause from a terminal and one
// from a profile screen are one gesture with one record.
func TestSeatsPauseActsThroughTheActTransport(t *testing.T) {
	node := newFakeActNode(t)
	node.answer = map[string]any{"handle": "swe", "outcome": "applied", "changed": true,
		"paused_by": "jane.doe", "paused_by_kind": "human", "operator_id": "pat:0192",
		"paused_at": "2026-09-25T09:00:00Z", "stop_running": true}
	stdout, stderr, err := cli(t, "seats", "pause", "@swe", "-stop",
		"-reason", "looping on one ticket", bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatalf("seats pause: %v\n%s", err, stderr)
	}
	if node.tool != "pause_seat" || node.contentType != "application/json" {
		t.Errorf("posted to %q as %q, want pause_seat as application/json", node.tool, node.contentType)
	}
	if node.authorization != "Bearer "+cliFixtureToken {
		t.Errorf("Authorization = %q, want the token from %s", node.authorization, apiTokenEnv)
	}
	if err := statelog.CheckCallerOpID(node.key); err != nil || node.key == "" {
		t.Errorf("%s = %q, which the act transport refuses: %v", opkey.Header, node.key, err)
	}
	if len(node.raw) != 1 || node.raw["args"] == nil {
		t.Errorf("the body carries %v, want the arguments alone", node.raw)
	}
	if node.body.Args["handle"] != "swe" || node.body.Args["stop_running"] != true ||
		node.body.Args["reason"] != "looping on one ticket" {
		t.Errorf("args = %v, want the handle, the stop and the reason", node.body.Args)
	}
	if !strings.Contains(stdout, "paused swe (by jane.doe (through pat:0192)") ||
		!strings.Contains(stdout, "next round") {
		t.Errorf("stdout does not say who paused it, through what, and that the turn stops:\n%s", stdout)
	}
}

// A RESUME CARRIES ONLY THE HANDLE, and a retry names the operation it
// repeats — under the same key, so the node's ledger answers it as the first
// attempt rather than a second gesture.
func TestSeatsResumeRetriesUnderTheSameOperation(t *testing.T) {
	node := newFakeActNode(t)
	node.answer = map[string]any{"handle": "swe", "outcome": "unknown"}
	stdout, _, err := cli(t, "seats", "resume", "swe", bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatalf("seats resume: %v", err)
	}
	if node.tool != "resume_seat" || len(node.body.Args) != 1 {
		t.Errorf("posted %v to %q, want only the handle to resume_seat", node.body.Args, node.tool)
	}
	first := node.key
	if !strings.Contains(stdout, "-op-id "+first) {
		t.Fatalf("an unknown outcome did not print the id to retry with:\n%s", stdout)
	}
	if _, _, err := cli(t, "seats", "resume", "swe", "-op-id", first,
		bootstrapForURL(t, node.server.URL)); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if node.key != first {
		t.Errorf("the retry sent %q, want the first attempt's %q", node.key, first)
	}
}

// AN UNKNOWN THE NODE ANSWERS 503 IS STILL A RETRY UNDER THE SAME KEY, never a
// refusal to start over from: the write may have landed.
func TestSeatsAnUnsettledRefusalNamesTheOperation(t *testing.T) {
	node := newFakeActNode(t)
	node.status = http.StatusServiceUnavailable
	node.refusal = map[string]any{"error": "unavailable", "outcome": "unknown",
		"op_id": "whatever-the-node-echoes"}
	_, _, err := cli(t, "seats", "pause", "swe", bootstrapForURL(t, node.server.URL))
	if err == nil || !strings.Contains(err.Error(), "-op-id "+node.key) {
		t.Errorf("err = %v, want the retry named under the key this command sent (%s)", err, node.key)
	}
}

// A GESTURE THE CALLER MAY NOT MAKE names the grant, through the shared
// refusal: the token is fine and the authority is what is missing.
func TestSeatsARefusalOnAuthorityNamesTheGrant(t *testing.T) {
	node := newFakeActNode(t)
	node.status = http.StatusForbidden
	node.refusal = map[string]any{"error": "unauthorized", "reason": "not_lead",
		"grants": []string{"fleet:operate"}}
	_, _, err := cli(t, "seats", "pause", "swe", bootstrapForURL(t, node.server.URL))
	if err == nil || !strings.Contains(err.Error(), "fleet:operate") ||
		strings.Contains(err.Error(), "did not accept") {
		t.Errorf("err = %v, want the grant that would admit the caller", err)
	}
}

// A GESTURE A SEAT CANNOT BE NAMED FOR is refused before anything is sent —
// and so is an operation id the node would refuse.
func TestSeatsNeedsAHandleAndAWellFormedOperation(t *testing.T) {
	node := newFakeActNode(t)
	_, _, err := cli(t, "seats", "pause", "-url", node.server.URL)
	if err == nil || !strings.Contains(err.Error(), "name the seat") {
		t.Fatalf("a pause naming no seat = %v, want it refused for want of a handle", err)
	}
	_, _, err = cli(t, "seats", "pause", "swe", "-url", node.server.URL, "-op-id", "has space")
	if err == nil || !strings.Contains(err.Error(), "-op-id") {
		t.Fatalf("a malformed -op-id = %v, want it refused naming the flag", err)
	}
	if node.tool != "" {
		t.Errorf("a refused gesture reached the node as %q", node.tool)
	}
}

// THE CREDENTIAL IS NEVER A FLAG, here as on every command that talks to a
// node: a token typed as an argument is in shell history and `ps`.
func TestSeatsTakesNoTokenFlag(t *testing.T) {
	node := newFakeActNode(t)
	if _, _, err := cli(t, "seats", "pause", "swe", "-url", node.server.URL,
		"-token", "t"); err == nil {
		t.Fatal("-token was accepted")
	}
	if node.tool != "" {
		t.Errorf("a refused flag reached the node as %q", node.tool)
	}
}
