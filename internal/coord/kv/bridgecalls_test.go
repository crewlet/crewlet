package kv

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/coord"
)

// A FLEET STORE REFUSES A SERVER THAT CANNOT CARRY ITS RECORDS.
//
// Its records are sized against the contract's payload ceiling, and an
// external NATS cluster left at nats-server's own 1 MiB default would refuse
// every record between the two at write time — a bridged call among them,
// lost from the only log a resume reads. Refused at open, the node does not
// boot, and the error names the server setting to change.
func TestAFleetStoreRefusesAServerBelowTheRecordCeiling(t *testing.T) {
	t.Parallel()
	nc := embeddedNATSAt(t, 1<<20)
	_, err := OpenFleet(context.Background(), nc, FleetConfig{
		RateWindow: time.Minute, ClaimTTL: time.Minute,
		LedgerRetention: time.Minute, FireRetention: time.Minute,
		FollowRetention: time.Minute,
		CooldownMax:     time.Minute, StatusFreshness: time.Minute,
		BucketPrefix: fmt.Sprintf("f%d", bucketSeq.Add(1)),
	})
	if err == nil {
		t.Fatal("a fleet store opened on a server that accepts 1 MiB messages")
	}
	if !strings.Contains(err.Error(), "max_payload") {
		t.Errorf("the refusal does not name the setting to change: %v", err)
	}
}

// THE CLIENT'S SIZE REFUSAL IS PERMANENT, NOT A BLIP.
//
// The open-time check reads what the server announced when this node
// connected, and a client re-reads that announcement on every reconnect — so a
// cluster member configured below the ceiling is met at write time. The same
// bytes are refused the same way every time, and an error that read as
// "unavailable" would be retried for ever.
func TestTheClientsSizeRefusalIsPermanent(t *testing.T) {
	t.Parallel()
	refused := createRefusal(fmt.Errorf("publish: %w", nats.ErrMaxPayload))
	if !errors.Is(refused, coord.ErrTooLarge) || errors.Is(refused, coord.ErrUnavailable) {
		t.Errorf("a payload refusal = %v, want coord.ErrTooLarge and not coord.ErrUnavailable", refused)
	}
	down := createRefusal(nats.ErrConnectionClosed)
	if !errors.Is(down, coord.ErrUnavailable) || errors.Is(down, coord.ErrTooLarge) {
		t.Errorf("a closed connection = %v, want coord.ErrUnavailable", down)
	}
}
