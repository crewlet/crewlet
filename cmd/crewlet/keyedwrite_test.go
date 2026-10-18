package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/statelog"
)

// fakeKeyedNode answers every write it is sent with one prepared answer,
// recording the operation key the write carried.
//
// ITS FIELDS ARE READ BEHIND A LOCK, for [fakePurgeNode]'s reason: a request it
// hangs up on gives the command nothing to synchronise with.
type fakeKeyedNode struct {
	server *httptest.Server

	mu   sync.Mutex
	key  string
	path string

	status int
	body   map[string]any
	hangUp bool
}

func newFakeKeyedNode(t *testing.T) *fakeKeyedNode {
	t.Helper()
	n := &fakeKeyedNode{}
	n.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		n.key, n.path = r.Header.Get(opkey.Header), r.URL.Path
		status, body, hangUp := n.status, n.body, n.hangUp
		n.mu.Unlock()
		if hangUp {
			if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
				_ = conn.Close()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(n.server.Close)
	return n
}

// sent is the operation key and the path the node last received.
func (n *fakeKeyedNode) sent() (key, path string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.key, n.path
}

// keyedCommands are the commands whose write is sent under an operation this
// command names, each as an operator types it against base — every one that
// reads its open outcome through [keyedWrite.settle].
func keyedCommands(base string) map[string][]string {
	return map[string][]string{
		"artifacts share": {"artifacts", "share", cliArtifact, "seat:bob", "viewer",
			"-api", base},
		"artifacts label": {"artifacts", "label", cliArtifact, "2", "launch", "-api", base},
		"artifacts trash": {"artifacts", "trash", cliArtifact, "-api", base},
		"artifacts purge": {"artifacts", "purge", cliArtifact, "-confirm", cliArtifact,
			"-api", base},
		"worklet purge": {"worklet", "purge", "t-1", "-reason", "why", "-confirm", "ENG-42",
			"-api", base},
		"seats pause":  {"seats", "pause", "swe", "-api", base},
		"seats resume": {"seats", "resume", "swe", "-api", base},
		"chatlet post": {"chatlet", "post", "c-eng", "-body", "hello", "-api", base},
		"chatlet erase": {"chatlet", "erase", "c-eng", "-message", "m-1", "-reason", "a pasted key",
			"-confirm", "1", "-api", base},
		"chatlet prune": {"chatlet", "prune", "c-eng", "-cutoff", "2031-03-24T04:35:00Z",
			"-confirm", "2031-03-24T04:35:00Z", "-api", base},
		"chatlet retention": {"chatlet", "retention", "c-eng", "-days", "30", "-confirm", "30",
			"-api", base},
	}
}

// EVERY KEYED WRITE FINISHES AN OPEN OUTCOME BY ONE RULE, whichever command
// made it.
//
// Five answers leave a write's outcome open — none at all, a 503 that is an
// unknown this node can settle, one it cannot, or a refusal that wrote
// nothing, and a 2xx whose outcome is none this build can read — and each
// FAILS, sending the operator one way, always under the operation THIS
// COMMAND SENT: the node scopes the key it is sent by the caller's own
// identity, so the scoped id an answer echoes is the same operation, and a
// fresh one would be a second write. And a reused key's 409 is offered no
// retry under it at all, since the node's own sentence sends that one to a new
// key. Mutation: drop the unvouched arm, the refusal arm, or the 503 bound,
// or read a 2xx's outcome as landed, and a row fails for every command.
func TestEveryKeyedWriteFinishesAnOpenOutcomeUnderItsOwnOperation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		body   map[string]any
		hangUp bool
		// says is what the answer must carry, beside the retry under the
		// sent operation; offers is false where no retry under it is right.
		says   string
		offers bool
	}{
		{name: "no answer", hangUp: true, says: "landed is unknown", offers: true},
		{name: "an unknown", status: http.StatusServiceUnavailable,
			body: map[string]any{"error": "unavailable", "outcome": "unknown",
				"op_id": "the-scoped-key.k0123456789abcdef"},
			says: "No acknowledgement", offers: true},
		{name: "an unvouched unknown", status: http.StatusServiceUnavailable,
			body: map[string]any{"error": "unavailable", "outcome": "unknown",
				"unvouched": true, "op_id": "the-scoped-key.k0123456789abcdef"},
			says: "another node with -api", offers: true},
		{name: "a refusal naming the operation", status: http.StatusServiceUnavailable,
			body: map[string]any{"error": "unavailable", "detail": "the log is full",
				"op_id": "the-scoped-key.k0123456789abcdef"},
			says: "This attempt wrote nothing", offers: true},
		{name: "a 2xx naming no outcome this build can read", status: http.StatusOK,
			body: map[string]any{"outcome": "unknown"},
			says: "could not confirm the write", offers: true},
		{name: "a 2xx naming an outcome a newer build wrote", status: http.StatusOK,
			body: map[string]any{"outcome": "deferred"},
			says: `the node answered "deferred"`, offers: true},
		{name: "a reused key", status: http.StatusConflict,
			body: map[string]any{"error": "invalid_input", "field": opkey.Header,
				"op_id":  "the-scoped-key.k0123456789abcdef",
				"detail": "send this one under a new key"},
			says: "new key", offers: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			node := newFakeKeyedNode(t)
			node.status, node.body, node.hangUp = tc.status, tc.body, tc.hangUp
			for command, args := range keyedCommands(node.server.URL) {
				_, stderr, err := cli(t, args...)
				if err == nil {
					t.Errorf("%s: an open outcome exited zero", command)
					continue
				}
				said := err.Error() + "\n" + stderr
				key, _ := node.sent()
				if key == "" {
					t.Fatalf("%s sent no operation key", command)
				}
				if !strings.Contains(said, tc.says) {
					t.Errorf("%s answered %q, want it to say %q", command, said, tc.says)
				}
				if offered := strings.Contains(said, "-op-id "+key); offered != tc.offers {
					t.Errorf("%s offered a retry under %s: %v, want %v:\n%s", command,
						key, offered, tc.offers, said)
				}
				if strings.Contains(said, "-op-id the-scoped-key") {
					t.Errorf("%s named the echoed id rather than the one it sent:\n%s",
						command, said)
				}
			}
		})
	}
}

// AN OPERATION ID PASTED FROM A TERMINAL IS THE SAME ID, its newline and all:
// trimmed, held to the node's rule and sent as the key, whichever command
// carries it — and one minted here carries the instant a node vouches for a
// retry by.
func TestEveryKeyedWriteSendsThePastedIDAndMintsOneThatCarriesItsInstant(t *testing.T) {
	t.Parallel()
	pasted := statelog.NewOpID(time.Now(), "earlier")
	node := newFakeKeyedNode(t)
	node.status = http.StatusOK
	node.body = map[string]any{"outcome": "applied"}
	for command, args := range keyedCommands(node.server.URL) {
		if _, _, err := cli(t, append(args, "-op-id", pasted+"\n")...); err != nil {
			t.Errorf("%s refused the pasted id: %v", command, err)
		}
		if key, _ := node.sent(); key != pasted {
			t.Errorf("%s sent %q for the pasted %q", command, key, pasted)
		}
		if _, _, err := cli(t, args...); err != nil {
			t.Errorf("%s: %v", command, err)
		}
		if key, _ := node.sent(); key == pasted {
			t.Errorf("%s sent the earlier id again with no -op-id", command)
		} else if _, minted := statelog.OpMintedAt(key); !minted {
			t.Errorf("%s minted %q, which carries no instant: no ledger could vouch "+
				"for its retry", command, key)
		}
	}
}
