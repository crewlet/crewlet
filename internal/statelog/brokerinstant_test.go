package statelog_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/recordseal"
)

// A RECORD'S BROKER INSTANT REACHES THE APPLIER AS ONE VALUE, WHETHER IT WAS
// APPLIED AS IT ARRIVED OR HELD BACK AND APPLIED LATER, AND IN WHATEVER ZONE
// THE NODE RUNS: UTC at a microsecond on both paths (package doc, "A record's
// broker instant is one value, however the record arrived"). The expected
// value is spelled out, not computed by the framework's helper, so a helper
// drifting from the store's encoding is caught rather than agreed with.
//
// Mutations: hand the live record the broker's instant as it arrived, and both
// live records differ from what the held-back one was handed (the zone and the
// nanoseconds); keep only its zone or only its precision, and one half of each
// difference remains.
func TestARecordsBrokerInstantReachesTheApplierAsOneValue(t *testing.T) {
	t.Parallel()
	east := time.FixedZone("east", 5*60*60+30*60)
	west := time.FixedZone("west", -7*60*60)
	// THE BROKER'S OWN, as its client reports it: nanoseconds, and the zone
	// of the node that read it.
	broker := func(seq uint64, zone *time.Location) time.Time {
		return time.Unix(1_700_000_000, 123_456_789).Add(time.Duration(seq) * time.Second).In(zone)
	}

	h := newApplyHarness(t, probeDomain{})
	h.fetch.offerFramedStored(1, broker(1, east), sealedUnder(t, "k9", env(1, "edit", "a", "op-1", 1)))
	h.fetch.offerStored(2, broker(2, east), env(2, "edit", "b", "op-2", 1))
	h.fetch.offerStored(3, broker(3, west), env(3, "edit", "c", "op-3", 1))
	if err := h.run(3); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := h.retainedCount(); got != 1 {
		t.Fatalf("retained %d, want the one record signed under k9", got)
	}

	// THE KEY ARRIVES, and the restarted runner reprocesses the record it
	// held back — through the deferred table, which is the other path.
	h.keyedRunner(recordseal.Keyring{ActiveID: "k1", Keys: []recordseal.Key{
		{ID: "k1", Material: "test-material"},
		{ID: "k9", Material: "material-this-node-lacks-k9"},
	}})
	if err := h.boot(0); err != nil {
		t.Fatalf("the reprocess did not release the record: %v", err)
	}

	paths := map[uint64]string{1: "held back", 2: "live, east of UTC", 3: "live, west of UTC"}
	for seq := uint64(1); seq <= 3; seq++ {
		handed, applied := h.applier.instantAt(seq)
		if !applied {
			t.Fatalf("the record at %d (%s) was never applied", seq, paths[seq])
		}
		want := time.Unix(1_700_000_000+int64(seq), 123_456_000).UTC()
		for _, got := range []struct {
			name string
			at   time.Time
		}{{"the record's", handed.record}, {"the options'", handed.options}} {
			if got.at.Location() != time.UTC || !got.at.Equal(want) {
				t.Errorf("the record at %d (%s): %s broker instant is %s, want %s — "+
					"the store's encoding, UTC at a microsecond, on every path",
					seq, paths[seq], got.name, got.at.Format(time.RFC3339Nano),
					want.Format(time.RFC3339Nano))
			}
			if gotBytes, wantBytes := marshalInstant(t, got.at), marshalInstant(t, want); gotBytes != wantBytes {
				t.Errorf("the record at %d (%s): %s broker instant encodes as %s in a "+
					"document, want %s", seq, paths[seq], got.name, gotBytes, wantBytes)
			}
		}
	}
}

// marshalInstant is an instant as a JSON document carries it.
func marshalInstant(t *testing.T, at time.Time) string {
	t.Helper()
	encoded, err := json.Marshal(at)
	if err != nil {
		t.Fatalf("encode %v: %v", at, err)
	}
	return string(encoded)
}
