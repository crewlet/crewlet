package stream_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/config"
)

// readUntil reads frames until one of kinds arrives, and reports what it
// skipped on the way.
func readUntil(t *testing.T, conn *websocket.Conn, kinds ...string) (map[string]any, []string) {
	t.Helper()
	var skipped []string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got := next(t, conn)
		kind, _ := got["kind"].(string)
		if slicesContains(kinds, kind) {
			return got, skipped
		}
		skipped = append(skipped, kind)
	}
	t.Fatalf("no %v frame arrived; saw %v", kinds, skipped)
	return nil, nil
}

// TestTheDegradedEnvelopeHoldsTheSocketOpen is the shape of a node that cannot
// serve, all three halves of it.
//
// A dashboard whose socket CLOSES reconnects on a backoff for as long as the
// node is degraded, learns nothing from any attempt, and shows "retrying" —
// which is strictly worse than a socket that is open and telling it what is
// wrong. So: the keepalives continue (the ping is answered and the health
// frame still arrives, and it is the health frame that says why), the pushes
// stop (they are derived from a copy the fleet has abandoned), and a query is
// REFUSED with `unavailable` rather than answered out of that copy. "There is
// no such work item" is an answer a person acts on: they file the duplicate.
func TestTheDegradedEnvelopeHoldsTheSocketOpen(t *testing.T) {
	t.Parallel()
	ran := make(chan struct{}, 1)
	f := newSocketWith(t, nil,
		func(context.Context, string, map[string]any) (any, error) {
			ran <- struct{}{}
			return map[string]any{"answered": true}, nil
		},
		stream.Options{
			// The node posture the health body carries (`shed`) is what
			// api.framePosture maps onto the frame posture; here the
			// mapping is stated directly, because the mapping's own
			// correctness is internal/api's test.
			Health: func() stream.Health { return stream.Health{Status: "shed"} },
			Posture: func(h stream.Health) stream.FramePosture {
				if h.Status == "shed" {
					return stream.FrameDegraded
				}
				return stream.FrameLive
			},
			HealthInterval: 10 * time.Millisecond,
		})
	f.svc.StartHealthTicks(t.Context())

	conn, _, err := f.dial(t, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// THE SNAPSHOT STILL ARRIVES. It is a direct frame — the answer to
	// this client's own connect — and it carries the node's health, so the
	// one frame a degraded client is handed is the one that says so.
	//
	// NOT NECESSARILY FIRST: the client is registered before its snapshot
	// is queued, deliberately (see Hub.Register), and this node ticks its
	// health every ten milliseconds, so on a loaded runner a keepalive can
	// land in between. That keepalive is the only thing that may.
	_, before := readUntil(t, conn, stream.KindSnapshot)
	for _, kind := range before {
		if kind != stream.KindHealth {
			t.Fatalf("a degraded socket received %q before its snapshot", kind)
		}
	}

	// THE PUSHES STOP. An ingested event would broadcast `event` and
	// `agents` on a serving node.
	f.svc.Ingest(livestate.Envelope{
		ID: "e1", Type: "agent_phase_started", Timestamp: "2026-06-14T12:00:00Z",
		Category: "system", Payload: map[string]any{"role": "Lead", "task_id": "t-1"},
	})

	// THE KEEPALIVES CONTINUE: the ping is answered, and nothing derived
	// from the projection arrived ahead of the pong.
	write(t, conn, map[string]any{"kind": "ping"})
	_, before = readUntil(t, conn, stream.KindPong)
	for _, kind := range before {
		if kind != stream.KindHealth {
			t.Errorf("a degraded socket received %q; only the health "+
				"keepalive survives (saw %v)", kind, before)
		}
	}

	// AND THE HEALTH FRAME ARRIVES, which is the whole reason the socket
	// is held open rather than closed.
	if _, _ = readUntil(t, conn, stream.KindHealth); t.Failed() {
		return
	}

	// THE QUERIES ARE REFUSED, and the query surface is never reached.
	write(t, conn, map[string]any{"kind": "query", "id": 1, "what": "agents"})
	answer, _ := readUntil(t, conn, stream.KindError, stream.KindResult)
	if answer["kind"] != stream.KindError || answer["error"] != stream.CodeUnavailable {
		t.Errorf("a degraded node answered %v, want an %q refusal",
			answer, stream.CodeUnavailable)
	}
	select {
	case <-ran:
		t.Error("the query ran on a degraded node: its copy of the company " +
			"is wrong rather than behind, so an answer out of it is one a " +
			"person acts on")
	default:
	}

	// AND THE SOCKET IS STILL OPEN.
	write(t, conn, map[string]any{"kind": "ping"})
	if got, _ := readUntil(t, conn, stream.KindPong); got["kind"] != stream.KindPong {
		t.Errorf("the socket did not survive the refusal: %v", got)
	}
}

// TestAServingNodeAnswersTheSameSocket is the control for the case above: with
// everything else identical and only the posture changed, the pushes arrive
// and the query runs. Without it, a socket that answered nothing at all for
// some unrelated reason would pass the degraded case.
func TestAServingNodeAnswersTheSameSocket(t *testing.T) {
	t.Parallel()
	ran := make(chan struct{}, 1)
	f := newSocketWith(t, func(a *config.APIAuth) {},
		func(context.Context, string, map[string]any) (any, error) {
			ran <- struct{}{}
			return map[string]any{"answered": true}, nil
		},
		stream.Options{
			Health:  func() stream.Health { return stream.Health{Status: "ok"} },
			Posture: func(stream.Health) stream.FramePosture { return stream.FrameLive },
		})

	conn, _, err := f.dial(t, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	next(t, conn)

	f.svc.Ingest(livestate.Envelope{
		ID: "e1", Type: "agent_phase_started", Timestamp: "2026-06-14T12:00:00Z",
		Category: "system", Payload: map[string]any{"role": "Lead", "task_id": "t-1"},
	})
	if got, _ := readUntil(t, conn, stream.KindEvent); got["kind"] != stream.KindEvent {
		t.Fatalf("a serving node withheld its pushes: %v", got)
	}

	write(t, conn, map[string]any{"kind": "query", "id": 1, "what": "agents"})
	answer, _ := readUntil(t, conn, stream.KindError, stream.KindResult)
	if answer["kind"] != stream.KindResult {
		t.Errorf("a serving node answered %v, want a result", answer)
	}
	select {
	case <-ran:
	default:
		t.Error("the query never reached the surface on a serving node")
	}
}

// TestAPostureChangeReachesAnAlreadyOpenSocket: the tab that is open when a
// node sheds is the one that most needs telling, and a posture read only at
// connect would leave it rendering the abandoned copy until somebody reloaded.
func TestAPostureChangeReachesAnAlreadyOpenSocket(t *testing.T) {
	t.Parallel()
	var status atomic.Pointer[string]
	serving := "ok"
	status.Store(&serving)

	f := newSocketWith(t, nil, nil, stream.Options{
		Health: func() stream.Health { return stream.Health{Status: *status.Load()} },
		Posture: func(h stream.Health) stream.FramePosture {
			if h.Status == "ok" {
				return stream.FrameLive
			}
			return stream.FrameDegraded
		},
		HealthInterval: 10 * time.Millisecond,
	})
	f.svc.StartHealthTicks(t.Context())

	conn, _, err := f.dial(t, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	next(t, conn)
	waitFor(t, func() bool { return f.svc.Hub().Posture() == stream.FrameLive },
		"the socket did not open live")

	shed := "shed"
	status.Store(&shed)
	waitFor(t, func() bool { return f.svc.Hub().Posture() == stream.FrameDegraded },
		"the tick never moved an open socket to the degraded posture")

	// And the refusal follows the change rather than the connect.
	write(t, conn, map[string]any{"kind": "query", "id": 1, "what": "agents"})
	answer, _ := readUntil(t, conn, stream.KindError, stream.KindResult)
	if answer["error"] != stream.CodeUnavailable {
		t.Errorf("answer = %v, want an %q refusal", answer, stream.CodeUnavailable)
	}
}
