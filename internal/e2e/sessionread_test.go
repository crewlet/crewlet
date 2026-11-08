package e2e

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// activateTheRunningCompany stores the company the node runs as a revision and
// activates it, through `PUT /config` as `crewlet config import` does, then
// applies the activation the way a node's reconcile loop does ([applyByTick]).
//
// A CAPTURE READS WHETHER TO KEEP A SESSION OFF THE REVISION THE FLEET'S
// ACTIVATION POINTER NAMES, never off the company a node booted from: the
// budget it carries decides which ended sessions every node releases. This
// harness boots its node from the company file and stores no revision, so
// until something activates one, every capture is refused for want of the
// fleet's word and no session is kept.
func activateTheRunningCompany(t *testing.T, n *node) {
	t.Helper()
	company := n.engine.Company()
	if company == nil || company.Config == nil {
		t.Fatal("the node runs no company to activate")
	}
	status, body := bearer(t, n, http.MethodPut, "/config", *company.Config,
		map[string]string{"X-Summary": "activate the company the node runs"})
	if status != http.StatusCreated {
		t.Fatalf("PUT /config with the running company answered %d: %s", status, body)
	}
	applyByTick(t, n, "the activated company", func(c *engine.Company) bool {
		return !c.ActivatedAt.Equal(harnessActivation)
	})
}

// keepAnotherSeatsSession keeps a session of a turn of handle's on n — its
// object sealed in the fleet's store, captured and ended on the executor log
// through the engine's own writer, its ask ask — and answers the turn. The log
// keeps rows by seat, so to every reader it is that seat's kept session as any
// run's would be, without a second seat's run to keep one.
func keepAnotherSeatsSession(t *testing.T, n *node, handle, ask string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	store, err := n.engine.ExecutorSessions()
	writer := n.engine.ExecutorWriter()
	if err != nil || store == nil || writer == nil {
		t.Fatalf("the node keeps no session to write another seat's beside the run's: (%v, %v, %v)",
			store, writer, err)
	}
	turn := statelog.NewOpID(time.Now(), "")
	put, err := store.Put(ctx, handle, turn, io.NopCloser(bytes.NewReader([]byte(handle+"'s own session"))))
	if err != nil {
		t.Fatalf("seal %s's session: %v", handle, err)
	}
	call := func() executordomain.Call {
		return executordomain.Call{By: iam.Actor{Name: "executor", Kind: iam.ActorSystem},
			OpID: statelog.NewOpID(time.Now(), "executor")}
	}
	conversation := "chat:" + handle
	captured, err := writer.Capture(ctx, call(), executordomain.NewCapture{Handle: handle, Turn: turn,
		ConversationKey: conversation, Harness: "claude-code", HarnessVersion: "2.1.0",
		SessionID: "session-" + turn, Object: put.Object, ContentKey: put.ContentKey, ArchiveFormat: 1,
		LaunchSeq: 1, CaptureSeq: 1, Home: "/home/agent", Workspace: "/work", SeatBudget: 64 << 20})
	if err != nil || !captured.Recorded {
		t.Fatalf("capture %s's session: (%+v, %v)", handle, captured, err)
	}
	if _, err := writer.End(ctx, call(), executordomain.NewEnd{Handle: handle, Turn: turn,
		ConversationKey: conversation, Ask: ask,
		Ending: executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}}); err != nil {
		t.Fatalf("end %s's turn: %v", handle, err)
	}
	return turn
}

// resultHolding is the first of results holding every one of words, or "".
func resultHolding(results []string, words ...string) string {
	for _, r := range results {
		holds := true
		for _, w := range words {
			holds = holds && strings.Contains(r, w)
		}
		if holds {
			return r
		}
	}
	return ""
}

// AN EXECUTOR READS ITS SEAT'S SESSION OVER THE BRIDGE (#519).
//
// With the company the node runs activated as the fleet's revision, which is
// what a capture asks whether to keep a session, a seat's first turn runs its
// executor in a real box, whose harness — the stand-in of codingcli_test.go —
// files its transcript where Claude Code does; the run's collection keeps that
// session in the object store, and the turn's end names it on the executor
// log. Another seat keeps a session of its own, its ask holding the same word.
// The seat's next turn, on another conversation, has its executor call
// `list_sessions` with a word of the first turn's ask, `read_session` with that
// word as `match`, and `read_session` of the other seat's turn and of a turn
// nobody has — each over the run's own bridge, under the run's token: the
// listing finds the first turn and not the other seat's, the read hands back
// the first run's brief fenced as a kept session's words, and the other seat's
// turn is not found, in the very words a turn nobody has gets, sent to the
// listing. That the node holding the seat answers a bridged call as the run's
// seat whatever the request carried is the bridge's own case
// (mcpbridge.TestABridgedCallActsAsTheRunsSeatBehindTheGuard); this is the
// seat's reads end to end.
//
// Mutation: leave the session tools off the seat's registry, and the run's
// calls are refused: no listing, read or not-found answer ever comes back; read
// a turn's row past its own seat (`agent_handle >= ?`), and the other seat's
// session is found.
func TestAnExecutorReadsItsSeatsSessionOverTheBridge(t *testing.T) {
	t.Parallel()
	n := startCoding(t, codingModeWork)
	activateTheRunningCompany(t, n.node)
	waitFor(t, "the seat to be claimed", func() bool {
		return slices.Contains(n.engine.Node().Host().Held(), "swe")
	})
	n.wakeInDirectThread(t, "swe", "the zebrafish deploy is flaking, please fix it",
		codingConversation, codingThread)
	var first executordomain.Session
	waitFor(t, "the first turn's session to be kept", func() bool {
		for _, row := range n.executorRows(t, "swe") {
			if row.Ended() && row.HasObject() {
				first = row
				return true
			}
		}
		return false
	}, n.diagnose)

	// ANOTHER SEAT'S, sorting after the run's own: a read that went past its
	// seat would find it.
	theirs := keepAnotherSeatsSession(t, n.node, "triage", "the zebrafish deploy, triaged")

	// THE NEXT TURN'S EXECUTOR reads it, on a conversation of its own, so its
	// launch starts a session of its own rather than continuing the first.
	stranger := statelog.NewOpID(time.Now(), "")
	n.model.callOnExecute(builtin.ListSessionsTool, map[string]any{"query": "zebrafish"})
	n.model.callOnExecute(builtin.ReadSessionTool, map[string]any{"turn_id": first.Turn, "match": "zebrafish"})
	n.model.callOnExecute(builtin.ReadSessionTool, map[string]any{"turn_id": theirs})
	n.model.callOnExecute(builtin.ReadSessionTool, map[string]any{"turn_id": stranger})
	n.wakeInDirectThread(t, "swe", "write up what the last fix did", "slack:D-later", "root-2")
	waitFor(t, "the second turn's executor to read the first turn's session", func() bool {
		return resultHolding(n.model.toolResults(), "keeps no executor session", stranger) != ""
	}, n.diagnose)

	results := n.model.toolResults()
	listing := resultHolding(results, first.Turn, `"searched"`, "zebrafish")
	if listing == "" || strings.Contains(listing, theirs) {
		t.Errorf("no listing found the first turn by its ask alone, without the other seat's; the run's "+
			"tool results are %q", results)
	}
	if other := resultHolding(results, theirs); other == "" || strings.ReplaceAll(other, theirs, "T") !=
		strings.ReplaceAll(resultHolding(results, stranger), stranger, "T") {
		t.Errorf("the other seat's turn answered %q, want what a turn nobody has gets, word for word", other)
	}
	if resultHolding(results, "source=executor_session trust=external authors=agent:swe",
		"zebrafish", `"text_from"`) == "" {
		t.Errorf("no read handed the first run's brief back fenced as the seat's kept session; the "+
			"run's tool results are %q", results)
	}
	if notFound := resultHolding(results, stranger); !strings.Contains(notFound, builtin.ListSessionsTool) {
		t.Errorf("a turn the seat keeps no session of answered %q, want not found and the listing named",
			notFound)
	}
}
