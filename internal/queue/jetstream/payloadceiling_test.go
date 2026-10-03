package jetstream

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"

	"github.com/crewlet/crewlet/internal/queue"
)

// externalServer runs a plain nats-server at the given max_payload — zero
// leaves nats-server's own default — as somebody else's cluster would.
func externalServer(t *testing.T, maxPayload int32) *server.Server {
	t.Helper()
	ns, err := server.NewServer(&server.Options{
		Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true,
		MaxPayload: maxPayload,
	})
	if err != nil {
		t.Fatalf("configure an external server: %v", err)
	}
	go ns.Start()
	t.Cleanup(ns.Shutdown)
	if !ns.ReadyForConnections(30 * time.Second) {
		t.Fatal("the external server never became ready")
	}
	return ns
}

// AN EXTERNAL BROKER BELOW THE CONTRACT IS REFUSED AT THE DIAL, naming the one
// setting that fixes it. Accepted, a node on nats-server's 1 MiB default
// started cleanly and then had every file chunk — a mebibyte and its framing —
// and every event over a mebibyte refused at its publish, with nothing saying
// the broker was the reason.
func TestAnExternalBrokerBelowThePayloadContractIsRefused(t *testing.T) {
	t.Parallel()
	ns := externalServer(t, 0)
	_, err := Dial(Config{URL: ns.ClientURL()})
	if !errors.Is(err, ErrPayloadCeiling) {
		t.Fatalf("Dial against a default server = %v, want ErrPayloadCeiling", err)
	}
	for _, says := range []string{ns.ClientURL(), "max_payload: 8MB", "every server"} {
		if !strings.Contains(err.Error(), says) {
			t.Errorf("the refusal %q does not say %q", err, says)
		}
	}
}

// AND ONE AT THE CONTRACT IS DIALLED AS BEFORE.
func TestAnExternalBrokerAtThePayloadContractIsDialled(t *testing.T) {
	t.Parallel()
	ns := externalServer(t, int32(queue.MaxPayloadBytes))
	nc, err := Dial(Config{URL: ns.ClientURL()})
	if err != nil {
		t.Fatalf("Dial against a server at the contract: %v", err)
	}
	nc.Close()
}
