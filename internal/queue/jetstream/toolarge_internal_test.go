package jetstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats-server/v2/server"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// A TOO-LARGE REFUSAL NAMES THE CEILING THAT REFUSED IT.
//
// The client refuses a message against the max_payload the server its
// connection is on announced, and that is the contract's number only on the
// embedded broker. A connection that failed over to an external server set
// lower refuses messages the contract says fit; naming the contract's number
// there sends the reader looking for an oversized producer when the fault is a
// server's setting. So the connection's ceiling is named, the contract's
// beside it where they differ, and the setting to change where the server's is
// the lower — and every one of them is still queue.ErrTooLarge, because the
// refusal is as permanent as any other and a producer must not retry it.
func TestATooLargeRefusalNamesTheCeilingThatRefusedIt(t *testing.T) {
	t.Parallel()
	contract := strconv.Itoa(queue.MaxPayloadBytes)
	for _, tc := range []struct {
		name    string
		ceiling int32
		// over is how far past the server's ceiling the event is.
		over int
		// contract is whether the contract's own number must be named.
		contract bool
		// remedy is whether max_payload must be named as the thing to
		// raise.
		remedy bool
	}{
		{name: "a server at the contract's ceiling",
			ceiling: queue.MaxPayloadBytes, over: 1},
		{name: "a server set below it refuses what the contract carries",
			ceiling: 64 << 10, over: 1, contract: true, remedy: true},
		{name: "a server set above it names both, and no setting to raise",
			ceiling: 2 * queue.MaxPayloadBytes, over: 1, contract: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := queueAtCeiling(t, tc.ceiling)
			ev := eventOfWireSize(t, int(tc.ceiling)+tc.over)
			wire := strconv.Itoa(int(tc.ceiling) + tc.over)

			err := q.Publish(t.Context(), "crewlet.events.oversized", ev)
			if !errors.Is(err, queue.ErrTooLarge) {
				t.Fatalf("Publish refused a message over the connection's ceiling "+
					"with %v, want queue.ErrTooLarge", err)
			}
			msg := err.Error()
			ceiling := strconv.Itoa(int(tc.ceiling)) + "-byte max_payload"
			if !strings.Contains(msg, ceiling) {
				t.Errorf("the refusal does not name the ceiling that refused it "+
					"(%q): %v", ceiling, msg)
			}
			if !strings.Contains(msg, wire+" bytes") {
				t.Errorf("the refusal does not name the %s bytes it measured: %v", wire, msg)
			}
			if got := strings.Contains(msg, contract+" bytes the queue contract"); got != tc.contract {
				t.Errorf("naming the contract's %s bytes = %v, want %v: %v",
					contract, got, tc.contract, msg)
			}
			if got := strings.Contains(msg, "set max_payload"); got != tc.remedy {
				t.Errorf("naming max_payload as the setting to raise = %v, want "+
					"%v: %v", got, tc.remedy, msg)
			}
		})
	}
}

// AN ASK THE SERVER REFUSES IS TOO LARGE TOO, and says against what.
//
// The request is under the contract's ceiling, so the check Ask makes first
// lets it through, and the server this connection is on is what refuses it.
// Untranslated, that refusal reaches a caller as the client's own error, which
// the contract forbids anything above this package to branch on.
func TestAnAskAServerSetLowerRefusesIsTooLarge(t *testing.T) {
	t.Parallel()
	const ceiling = 64 << 10
	q := queueAtCeiling(t, ceiling)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err := q.Ask(ctx, "crewlet.probe.ask", make([]byte, ceiling+1), 1)
	if !errors.Is(err, queue.ErrTooLarge) {
		t.Fatalf("an ask the server refused for its size returned %v, want "+
			"queue.ErrTooLarge", err)
	}
	if want := strconv.Itoa(ceiling) + "-byte max_payload"; !strings.Contains(err.Error(), want) {
		t.Errorf("the refusal does not name the ceiling that refused it (%q): %v", want, err)
	}
}

// A REPLY TOO LARGE TO CARRY IS SAID BY THE NODE THAT HAD IT.
//
// Its asker counts this node as one that did not answer, which is all it can
// see, so the answering side is the only place the reason exists. A departed
// asker is not this case: a publish to a mailbox nobody reads succeeds.
func TestAReplyTooLargeToCarryIsLoggedWhereItWasRefused(t *testing.T) {
	t.Parallel()
	const ceiling = 64 << 10
	q := queueAtCeiling(t, ceiling)
	lines := &syncBuffer{}
	q.log = slog.New(slog.NewTextHandler(lines, &slog.HandlerOptions{Level: slog.LevelDebug}))

	const subject = "crewlet.probe.reply"
	stop, err := q.Serve(t.Context(), subject, func(context.Context, []byte) ([]byte, error) {
		return make([]byte, ceiling+1), nil
	})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	replies, err := q.Ask(ctx, subject, []byte("q"), 1)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(replies) != 0 {
		t.Fatalf("the asker got %d replies to a reply the server refuses", len(replies))
	}
	want := strconv.Itoa(ceiling) + "-byte max_payload"
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := lines.String()
		if strings.Contains(got, "scatter_reply_too_large") && strings.Contains(got, want) &&
			strings.Contains(got, "level=WARN") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no WARN scatter_reply_too_large naming %q was logged; "+
				"the log reads:\n%s", want, got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A DEAD LETTER THE CONNECTION REFUSES FOR ITS SIZE SAYS WHY.
//
// It was carried once already, so the refusal is a server this connection is
// on accepting less than the one it was published through, and the line is
// the only record of the letter.
func TestADeadLetterRefusedForItsSizeNamesTheCeiling(t *testing.T) {
	t.Parallel()
	const ceiling = 64 << 10
	q := queueAtCeiling(t, ceiling)
	lines := &syncBuffer{}
	q.log = slog.New(slog.NewTextHandler(lines, &slog.HandlerOptions{Level: slog.LevelDebug}))

	q.deadLetter(t.Context(), "crewlet.agent.big.inbox", "agent-big", make([]byte, ceiling+1))

	got := lines.String()
	if !strings.Contains(got, "dead_letter_failed") {
		t.Fatalf("a refused dead letter logged nothing; the log reads:\n%s", got)
	}
	for _, want := range []string{
		strconv.Itoa(ceiling) + "-byte max_payload", "set max_payload",
		queue.ErrTooLarge.Error(),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the dead_letter_failed line does not say %q:\n%s", want, got)
		}
	}
}

// queueAtCeiling is a queue on a broker whose max_payload is ceiling — the
// server a connection is on after failing over to one set differently from the
// embedded broker's own.
func queueAtCeiling(t *testing.T, ceiling int32) *Queue {
	t.Helper()
	ns, err := server.NewServer(&server.Options{
		ServerName: "ceiling-probe",
		JetStream:  true,
		Port:       -1,
		DontListen: true,
		StoreDir:   t.TempDir(),
		MaxPayload: ceiling,
		NoSigs:     true,
	})
	if err != nil {
		t.Fatalf("configure a broker at a %d-byte ceiling: %v", ceiling, err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(30 * time.Second) {
		ns.Shutdown()
		t.Fatal("the broker did not accept connections")
	}
	t.Cleanup(func() {
		ns.Shutdown()
		ns.WaitForShutdown()
	})
	q, err := newQueueOn(t.Context(), Config{}, &embeddedServer{ns: ns, inProcess: true}, false)
	if err != nil {
		t.Fatalf("connect a queue: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	if got := q.nc.MaxPayload(); got != int64(ceiling) {
		t.Fatalf("the connection announces a %d-byte ceiling, want %d", got, ceiling)
	}
	return q
}

// eventOfWireSize is an event whose encoding is exactly size bytes, which is
// what the client measures a publish of it by.
func eventOfWireSize(t *testing.T, size int) *events.Event {
	t.Helper()
	ev := &events.Event{ID: uuid.New(), Type: "probe.oversized",
		Timestamp: time.Now().UTC(), Source: "toolarge"}
	// Extra rather than Data, and under a key that is no envelope field:
	// those are reserved and dropped on encode.
	ev.Extra = map[string]json.RawMessage{"blob": json.RawMessage(`""`)}
	base, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	pad := size - len(base)
	if pad < 0 {
		t.Fatalf("an event cannot be as small as %d bytes", size)
	}
	ev.Extra["blob"] = json.RawMessage(`"` + strings.Repeat("x", pad) + `"`)
	wire, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(wire) != size {
		t.Fatalf("the event encodes to %d bytes, want %d", len(wire), size)
	}
	return ev
}

// syncBuffer is a log sink a NATS callback goroutine writes and a test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
