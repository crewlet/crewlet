package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// backupNode is a node that answers POST /backup, recording what it was
// asked for.
type backupNode struct {
	server *httptest.Server
	dir    string
	token  string
	answer map[string]any
	status int
}

func newBackupNode(t *testing.T) *backupNode {
	t.Helper()
	n := &backupNode{status: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /backup", func(w http.ResponseWriter, r *http.Request) {
		n.dir = r.URL.Query().Get("dir")
		n.token = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(n.status)
		body := n.answer
		if body == nil {
			body = map[string]any{
				"taken_at":    "2026-08-30T12:00:00Z",
				"finished_at": "2026-08-30T12:00:02Z",
				"node_id":     "node-0",
				"store": map[string]any{
					"file": "store.db", "source": "/data/company.db",
					"bytes": 262144, "migrations": []string{"0001_events.sql"},
				},
				"streams": []map[string]any{
					{"name": "CREWLET_AGENT", "file": "streams/CREWLET_AGENT.snapshot",
						"bytes": 4096, "messages": 12},
					{"name": "KV_crewlet_budgets", "file": "streams/KV_crewlet_budgets.snapshot",
						"bytes": 512, "messages": 3},
				},
			}
		}
		raw, err := json.Marshal(body)
		if err != nil {
			t.Errorf("marshal: %v", err)
		}
		_, _ = w.Write(raw)
	})
	n.server = httptest.NewServer(mux)
	t.Cleanup(n.server.Close)
	return n
}

// The destination has to reach the node, and it has to reach it as the
// operator typed it: this is a path on the ENGINE'S host, so a command that
// quietly rewrote it would write the company's state somewhere nobody named.
func TestBackupSendsTheDestinationAndReportsWhatWasCaptured(t *testing.T) {
	node := newBackupNode(t)
	stdout, _, err := cli(t, "backup", bootstrapForURL(t, node.server.URL), "-dir", "/var/backups/tonight")
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if node.dir != "/var/backups/tonight" {
		t.Errorf("the node was asked for %q", node.dir)
	}
	if node.token != "Bearer t0ken" {
		t.Errorf("the configured token did not reach the node: %q", node.token)
	}
	// Both estates named, so an operator can see what they actually got
	// rather than inferring it from an exit code.
	for _, want := range []string{"/var/backups/tonight", "store", "CREWLET_AGENT", "12 messages"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the report never mentions %q:\n%s", want, stdout)
		}
	}
	// A KV bucket is reported as a bucket, not as the KV_ stream it
	// happens to be stored as — the operator configured buckets.
	if !strings.Contains(stdout, "bucket crewlet_budgets") {
		t.Errorf("the coordination bucket is not named as one:\n%s", stdout)
	}
}

// A missing -dir is refused before anything is sent: there is no sensible
// default for "where should this company's entire durable state be written".
func TestBackupWithoutADestinationIsRefused(t *testing.T) {
	node := newBackupNode(t)
	_, _, err := cli(t, "backup", bootstrapForURL(t, node.server.URL))
	if err == nil {
		t.Fatal("a backup with no destination was accepted")
	}
	if node.dir != "" {
		t.Errorf("the node was contacted anyway, with dir=%q", node.dir)
	}
}

// A relative path is refused HERE, with the reason, rather than being sent
// for the node to reject: the operator needs to know it would have been
// resolved on another host, which an HTTP error cannot convey as well.
func TestBackupRefusesARelativeDestinationLocally(t *testing.T) {
	node := newBackupNode(t)
	_, _, err := cli(t, "backup", bootstrapForURL(t, node.server.URL), "-dir", "backups/tonight")
	if err == nil {
		t.Fatal("a relative destination was accepted")
	}
	if !strings.Contains(err.Error(), "engine's host") {
		t.Errorf("the refusal does not explain why: %v", err)
	}
	if node.dir != "" {
		t.Error("the node was contacted with a path it would have resolved itself")
	}
}

// A node holding only one estate produces something that is NOT a restorable
// backup, and the command has to say so — the manifest alone shows a dash
// that reads as "nothing to back up" rather than "this is incomplete".
func TestBackupSaysWhenANodeHoldsOnlyPartOfTheState(t *testing.T) {
	node := newBackupNode(t)
	node.answer = map[string]any{
		"taken_at": "2026-08-30T12:00:00Z", "finished_at": "2026-08-30T12:00:01Z",
		"node_id": "ingress-1",
		"streams": []map[string]any{
			{"name": "CREWLET_AGENT", "file": "streams/a.snapshot", "bytes": 10, "messages": 1},
		},
	}
	stdout, _, err := cli(t, "backup", bootstrapForURL(t, node.server.URL), "-dir", "/tmp/partial")
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if !strings.Contains(stdout, "only part of the deployment's state") {
		t.Errorf("a partial backup was reported as a whole one:\n%s", stdout)
	}
}

// The node refusing is reported as the node's answer, not swallowed.
func TestBackupReportsANodeThatCannotTakeOne(t *testing.T) {
	node := newBackupNode(t)
	node.status = http.StatusServiceUnavailable
	node.answer = map[string]any{"error": "nothing_to_back_up"}
	_, _, err := cli(t, "backup", bootstrapForURL(t, node.server.URL), "-dir", "/tmp/x")
	if err == nil {
		t.Fatal("a 503 was reported as a successful backup")
	}
	if !strings.Contains(err.Error(), "nothing_to_back_up") {
		t.Errorf("the node's reason was lost: %v", err)
	}
}
