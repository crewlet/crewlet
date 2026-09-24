package kv

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/queue"
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

// A PART'S KEY IS NEVER DECODED AS A CALL, AND GOES WITH ITS LAUNCH.
//
// Every read of a launch's calls walks the launch's filter, which selects the
// parts too, and tells a call from a part by the depth of its key alone — so a
// part's key must fail the call decode and pass the part decode, and must sit
// under the filter the purge removes. Checked on the keys themselves, over ids
// with dots and spaces in them.
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

// A SUSPENSION PART IS NEVER DECODED AS A CALL OR AS A CALL'S PART, AND IT
// GOES WITH ITS LAUNCH.
//
// The parts sit under the launch's filter, which the purge removes, and at a
// call part's depth; what keeps them from being read as either is the word
// where a call part carries its call's number. And the suspension's own filter
// must select none of the launch's calls, or its read would move them.
func TestASuspensionPartsKeyIsNeverACallsAndGoesWithItsLaunch(t *testing.T) {
	t.Parallel()
	for _, ids := range [][2]string{{"turn-1", "launch-1"}, {"run.a", "launch.b"}, {"t:é", "l 1"}} {
		turnID, launchID := ids[0], ids[1]
		part := suspensionPartKey(turnID, launchID, 3)

		if seq, ok := bridgeCallSeq(part); ok {
			t.Errorf("%s: a suspension part's key decodes as call %d", turnID, seq)
		}
		if seq, n, ok := bridgePartAddress(part); ok {
			t.Errorf("%s: a suspension part's key decodes as part %d of call %d", turnID, n, seq)
		}
		if n, ok := suspensionPart(part); !ok || n != 3 {
			t.Errorf("%s: the suspension part's key decodes as part %d, %v", turnID, n, ok)
		}
		for _, other := range []string{bridgeCallKey(turnID, launchID, 3), bridgePartKey(turnID, launchID, 3, 1)} {
			if _, ok := suspensionPart(other); ok {
				t.Errorf("%s: the call key %q decodes as a suspension part", turnID, other)
			}
			if within := strings.TrimSuffix(suspensionPartFilter(turnID, launchID), ">"); strings.HasPrefix(other, within) {
				t.Errorf("%s: the suspension's filter selects the call key %q", turnID, other)
			}
		}
		if launch := strings.TrimSuffix(bridgeCallFilter(turnID, launchID), ">"); !strings.HasPrefix(part, launch) {
			t.Errorf("%s: the suspension part %q is outside the launch's filter, so a purge would leave it",
				turnID, part)
		}
	}
}

// THE CLIENT'S SIZE REFUSAL IS PERMANENT, NOT A BLIP — AND IT NAMES THE LIMIT
// THAT REFUSED IT.
//
// The open-time check reads what the server announced when this node
// connected, and a client re-reads that announcement on every reconnect — so a
// cluster member configured below the ceiling is met at write time. The same
// bytes are refused the same way every time, and an error that read as
// "unavailable" would be retried for ever. The limit the client enforced is
// that server's own, so the refusal names it, and the setting to change: one
// naming only the contract's ceiling would send a reader looking for an
// oversized value when a value within it was refused.
func TestTheClientsSizeRefusalIsPermanent(t *testing.T) {
	t.Parallel()
	refused := refusal(fmt.Errorf("publish: %w", nats.ErrMaxPayload), "record a part of a bridged call",
		"a part of a bridged call", 2<<20, 1<<20)
	if !errors.Is(refused, coord.ErrTooLarge) || errors.Is(refused, coord.ErrUnavailable) {
		t.Errorf("a payload refusal = %v, want coord.ErrTooLarge and not coord.ErrUnavailable", refused)
	}
	for _, want := range []string{
		"a part of a bridged call of 2097152 bytes",
		"announces 1048576 bytes, below the 8388608",
		"set max_payload to at least 8388608 on every server of the cluster",
	} {
		if !strings.Contains(refused.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, refused)
		}
	}

	// A LIMIT AT OR ABOVE THE CONTRACT'S IS NOT THE ONE THAT REFUSED: no value
	// within the ceiling reaches it, so the announcement was replaced after
	// the refusal, and the error must not present it as the cause.
	replaced := refusal(nats.ErrMaxPayload, "record a bridged call", "a bridged call", 2<<20, queue.MaxPayloadBytes)
	if !errors.Is(replaced, coord.ErrTooLarge) || strings.Contains(replaced.Error(), "below the") ||
		!strings.Contains(replaced.Error(), "since replaced") {
		t.Errorf("a refusal read against a limit the contract fits = %v, want it named as an "+
			"announcement replaced since", replaced)
	}

	down := refusal(nats.ErrConnectionClosed, "record a bridged call", "a bridged call", 10, 1<<20)
	if !errors.Is(down, coord.ErrUnavailable) || errors.Is(down, coord.ErrTooLarge) {
		t.Errorf("a closed connection = %v, want coord.ErrUnavailable", down)
	}
}

// A SERVER BELOW THE CEILING, MET AT WRITE TIME, IS NAMED BY ITS OWN LIMIT.
//
// What a refusal names is read off the connection the store writes through,
// not assumed: a cluster member left at nats-server's own 1 MiB default refuses
// a value the contract's ceiling admits, and every write of a detached run's
// records — its own record included — has to say which limit refused it, as a
// refusal rather than a blip: read as unavailable, the caller would retry a
// write that can never land, and a suspension it could have kept in parts would
// be lost as a store outage. The store is built on such a server by hand,
// because OpenFleet refuses to open on one — which is why production meets it
// only after a reconnect.
func TestAValueARealServerRefusesNamesThatServersLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	nc := embeddedNATSAt(t, 1<<20)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	bucket := func(name string) jetstream.KeyValue {
		t.Helper()
		kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
			Bucket: fmt.Sprintf("c%d_%s", bucketSeq.Add(1), name),
		})
		if err != nil {
			t.Fatalf("create the %s bucket: %v", name, err)
		}
		return kv
	}
	f := &FleetStore{js: js, calls: bucket("calls"), runs: bucket("runs")}
	if created, err := f.CreateSandboxRun(ctx, "turn-2", []byte(`{"status":"launching"}`)); err != nil || !created {
		t.Fatalf("create the run an update refuses over: %v, %v", created, err)
	}
	record, _, err := f.SandboxRun(ctx, "turn-2")
	if err != nil {
		t.Fatalf("read the run back: %v", err)
	}
	value := bytes.Repeat([]byte("p"), 2<<20)
	for name, write := range map[string]func() error{
		"a run created": func() error {
			_, err := f.CreateSandboxRun(ctx, "turn-1", value)
			return err
		},
		"a run updated": func() error {
			_, err := f.UpdateSandboxRun(ctx, "turn-2", value, record.Version)
			return err
		},
		"a suspension part": func() error {
			_, err := f.CreateSuspensionPart(ctx, "turn-1", "launch-1", 1, value)
			return err
		},
		"an append": func() error {
			_, err := f.AppendBridgeCall(ctx, "turn-1", "launch-1", value)
			return err
		},
		"a record at a reserved number": func() error {
			_, err := f.CreateBridgeCall(ctx, "turn-1", "launch-1", 7, value)
			return err
		},
		"a part": func() error {
			_, err := f.CreateBridgeCallPart(ctx, "turn-1", "launch-1", 7, 1, value)
			return err
		},
	} {
		err := write()
		if !errors.Is(err, coord.ErrTooLarge) || errors.Is(err, coord.ErrUnavailable) {
			t.Errorf("%s the server refused = %v, want coord.ErrTooLarge", name, err)
			continue
		}
		if !strings.Contains(err.Error(), "announces 1048576 bytes") {
			t.Errorf("%s: the refusal does not name the server's own limit: %v", name, err)
		}
	}
}
