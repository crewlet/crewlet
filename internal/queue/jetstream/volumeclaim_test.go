package jetstream

import (
	"testing"
)

// A BROKER'S CLAIM ON ITS VOLUME IS ITS LIMIT LESS WHAT IT STORES, AND ONLY A
// BROKER WITH A VOLUME HAS ONE.
//
// A neighbour on the broker's disk — the code cache, whose derived budget is
// what the volume has room for — has to leave the broker what it may still
// write. That is bytes, never ceilings: a stream reserving a gibibyte and
// holding nothing has written nothing, and a message the broker accepted has
// spent the disk whatever its stream reserved. A broker keeping its streams in
// memory, and one somebody else operates, holds no part of this node's disk.
//
// Mutation: report the reservations instead of the bytes stored, and the
// reservation moves the claim.
func TestAVolumeClaimIsWhatTheBrokerMayStillWrite(t *testing.T) {
	t.Parallel()
	const declared = int64(9) << 30
	dir := t.TempDir()
	q := newQueueWith(t, Config{StoreDir: dir, StoreMaxBytes: declared})

	before, found, err := q.VolumeClaim()
	if err != nil || !found {
		t.Fatalf("VolumeClaim = (%+v, %t, %v), want the file store's claim", before, found, err)
	}
	if before.Dir != dir || before.Limit != declared || before.Unclaimed() != declared-before.Stored {
		t.Fatalf("claim = %+v (unclaimed %d), want %s's %d-byte limit less what it stores",
			before, before.Unclaimed(), dir, declared)
	}

	const ceiling = int64(1) << 30
	if err := q.EnsureDomainStream(t.Context(), budgetLog("CLAIM", ceiling)); err != nil {
		t.Fatalf("EnsureDomainStream: %v", err)
	}
	reserved, _, err := q.VolumeClaim()
	if err != nil {
		t.Fatalf("VolumeClaim: %v", err)
	}
	if before.Unclaimed()-reserved.Unclaimed() >= ceiling {
		t.Fatalf("a %d-byte reservation took %d bytes from the claim: the volume is spent "+
			"by bytes written, never by a ceiling", ceiling, before.Unclaimed()-reserved.Unclaimed())
	}

	payload := make([]byte, 256<<10)
	for range 4 {
		if _, err := q.js.Publish(t.Context(), "crewlet.budget.CLAIM.x", payload); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}
	wrote, _, err := q.VolumeClaim()
	if err != nil {
		t.Fatalf("VolumeClaim: %v", err)
	}
	if spent := reserved.Unclaimed() - wrote.Unclaimed(); spent < 4*int64(len(payload)) {
		t.Fatalf("a mebibyte written took %d bytes from the claim", spent)
	}

	if claim, found, err := newQueue(t).VolumeClaim(); err != nil || found {
		t.Errorf("a broker holding its streams in memory: VolumeClaim = (%+v, %t, %v), "+
			"want none", claim, found, err)
	}
	// AN EXTERNAL QUEUE IS ONE WITH NO EMBEDDED SERVER, the field VolumeClaim
	// branches on; nothing is put back, the queue being this case's own.
	external := newQueueWith(t, Config{StoreDir: t.TempDir()})
	server := external.embedded
	external.embedded = nil
	claim, found, err := external.VolumeClaim()
	external.embedded = server
	if err != nil || found {
		t.Errorf("an external broker: VolumeClaim = (%+v, %t, %v), want none", claim, found, err)
	}
}

// A CLAIM'S ROOM NEVER GOES BELOW NOTHING, AND A SERVER HOLDING TO NO LIMIT
// MAY TAKE THE WHOLE VOLUME.
func TestAVolumeClaimsRoom(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		claim VolumeClaim
		want  int64
	}{
		{VolumeClaim{Limit: 10, Stored: 4}, 6},
		{VolumeClaim{Limit: 10, Stored: 12}, 0},
		{VolumeClaim{Limit: -1, Stored: 12}, -1},
	} {
		if got := tc.claim.Unclaimed(); got != tc.want {
			t.Errorf("%+v.Unclaimed() = %d, want %d", tc.claim, got, tc.want)
		}
	}
}
