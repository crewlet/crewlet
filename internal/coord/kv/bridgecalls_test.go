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

// A BUILD THAT PREDATES PARTS NEVER TAKES ONE FOR A CALL.
//
// A rolling upgrade puts such a build on this bucket, reading launches a newer
// build wrote. It walks a launch by the launch's filter, which selects the
// parts too, and decodes each key with bridgeCallSeq — the same decode this
// build reads calls with — so the part's key must fail that decode, and must
// sit under the filter its purge removes. Checked on the keys themselves,
// because that older build is not here to ask: what it runs is this decode.
func TestAPartsKeyIsNeverDecodedAsACallAndGoesWithItsLaunch(t *testing.T) {
	t.Parallel()
	for _, ids := range [][2]string{{"turn-1", "launch-1"}, {"run.a", "launch.b"}, {"t:é", "l 1"}} {
		turnID, launchID := ids[0], ids[1]
		call, part := bridgeCallKey(turnID, launchID, 7), bridgePartKey(turnID, launchID, 7, 1)

		if seq, ok := bridgeCallSeq(part); ok {
			t.Errorf("%s: a part's key decodes as call %d", turnID, seq)
		}
		if seq, ok := bridgeCallSeq(call); !ok || seq != 7 {
			t.Errorf("%s: the call's own key decodes as %d, %v", turnID, seq, ok)
		}
		if seq, n, ok := bridgePartAddress(part); !ok || seq != 7 || n != 1 {
			t.Errorf("%s: the part's key decodes as call %d part %d, %v", turnID, seq, n, ok)
		}
		if _, _, ok := bridgePartAddress(call); ok {
			t.Errorf("%s: a call's key decodes as a part", turnID)
		}
		if !strings.HasPrefix(part, call+coord.KeySeparator) {
			t.Errorf("%s: the part %q is not filed under its call %q", turnID, part, call)
		}
		if launch := strings.TrimSuffix(bridgeCallFilter(turnID, launchID), ">"); !strings.HasPrefix(part, launch) {
			t.Errorf("%s: the part %q is outside the launch's filter, so a purge would leave it", turnID, part)
		}
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
