package jetstream

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/queue"
)

// A MESSAGE TOO LARGE FOR THE SERVER NAMES THE SERVER'S OWN LIMIT, on every
// verb that sends one.
//
// The embedded broker is configured at exactly [queue.MaxPayloadBytes], so on
// the default topology the contract's number and the connection's are one
// number and naming either is right. An EXTERNAL server states its own
// max_payload — nats-server's default is a mebibyte — and the client refuses
// against what the server said. So on the topology where an operator has a
// knob to turn, the refusal named a limit eight times the one it came from,
// and a scatter request between the two failed with the client's bare error
// rather than the contract's [queue.ErrTooLarge], which is the one failure a
// producer must be able to tell from a broker that is merely down.
//
// The server here is external and allows a quarter of the contract, and each
// message is larger than that and smaller than the contract.
func TestAMessageTooLargeNamesTheServersOwnLimit(t *testing.T) {
	t.Parallel()
	const limit = queue.MaxPayloadBytes / 4
	ns, err := server.NewServer(&server.Options{
		Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true,
		JetStream: true, StoreDir: t.TempDir(), MaxPayload: limit,
	})
	if err != nil {
		t.Fatalf("configure an external server: %v", err)
	}
	go ns.Start()
	t.Cleanup(func() {
		ns.Shutdown()
		ns.WaitForShutdown()
	})
	if !ns.ReadyForConnections(30 * time.Second) {
		t.Fatal("the external server never became ready")
	}
	q := newQueueWith(t, Config{URL: ns.ClientURL()})
	if got := q.Conn().MaxPayload(); got != limit {
		t.Fatalf("the client reads a max_payload of %d off the server, want %d — "+
			"the case below would measure a different limit", got, limit)
	}
	size := limit + 1024
	names := strconv.Itoa(limit) + "-byte limit"

	t.Run("publish", func(t *testing.T) {
		t.Parallel()
		e := ev(1)
		e.Extra = map[string]json.RawMessage{
			"blob": json.RawMessage(`"` + strings.Repeat("x", size) + `"`),
		}
		err := q.Publish(t.Context(), "crewlet.agent.big.inbox", e)
		if !errors.Is(err, queue.ErrTooLarge) {
			t.Fatalf("an event over the server's limit was refused with %v, "+
				"want %v", err, queue.ErrTooLarge)
		}
		if !strings.Contains(err.Error(), names) {
			t.Errorf("the refusal %q does not name the server's %d-byte "+
				"max_payload, the limit it came from", err, limit)
		}
	})

	t.Run("ask", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		_, err := q.Ask(ctx, "crewlet.test.big.ask", make([]byte, size), 1)
		if !errors.Is(err, queue.ErrTooLarge) {
			t.Fatalf("a request over the server's limit and inside the "+
				"contract's was refused with %v, want %v", err, queue.ErrTooLarge)
		}
		if !strings.Contains(err.Error(), names) {
			t.Errorf("the refusal %q does not name the server's %d-byte "+
				"max_payload", err, limit)
		}
	})

	t.Run("append", func(t *testing.T) {
		t.Parallel()
		if err := q.EnsureDomainStream(t.Context(), probeDomain()); err != nil {
			t.Fatalf("EnsureDomainStream: %v", err)
		}
		log, err := q.DomainLog(t.Context(), probeDomain().Name)
		if err != nil {
			t.Fatalf("DomainLog: %v", err)
		}
		_, _, err = log.Append(t.Context(), "crewlet.probe.log.object.big", "",
			nil, make([]byte, size))
		// THE CLIENT'S OWN SENTINEL, still: the state log classifies an
		// append by it, and a wrap that dropped it would turn a permanent
		// refusal into the unknown answer every round retries.
		if !errors.Is(err, nats.ErrMaxPayload) {
			t.Fatalf("a record over the server's limit was refused with %v, "+
				"want one wrapping %v", err, nats.ErrMaxPayload)
		}
		for _, want := range []string{strconv.Itoa(size) + " bytes", names} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal %q does not say %q — the record's size and "+
					"the server's limit are the whole of what its remedy turns on",
					err, want)
			}
		}
	})
}
