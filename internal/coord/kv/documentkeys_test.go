package kv

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// A KEY LISTING TRANSFERS NO VALUES, which is the whole reason it exists
// beside Documents.
//
// The conformance suite proves the two answers select the same SET; this
// proves the cheaper one is actually cheaper, which is the property a sweep
// depends on. Without it "DocumentKeys" is Documents with a projection on top
// and a pages sweep still moves every revision body in the company to find a
// handful of expired ones.
//
// The bytes are counted from the values themselves: each seeded document
// carries a payload large enough that transferring even one of them would
// dwarf the key names, so an implementation that fetched values could not pass
// by accident.
func TestAKeyListingTransfersNoValues(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	f, err := OpenFleet(context.Background(), nc, FleetConfig{
		RateWindow: time.Minute, ClaimTTL: time.Minute,
		LedgerRetention: time.Minute, FireRetention: time.Minute,
		CooldownMax: time.Minute, StatusFreshness: time.Minute,
		BucketPrefix: fmt.Sprintf("f%d", bucketSeq.Add(1)),
	})
	if err != nil {
		t.Fatalf("OpenFleet: %v", err)
	}
	ctx := t.Context()

	const (
		docs      = 20
		valueSize = 16 << 10
	)
	big := make([]byte, valueSize)
	for i := range big {
		big[i] = 'x'
	}
	for i := range docs {
		key := coord.DocumentKey("i", string(rune('a'+i)))
		if _, err := f.CreateDocument(ctx, coord.FamilyWork, key, big); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	keys, err := f.DocumentKeys(ctx, coord.FamilyWork, "i")
	if err != nil {
		t.Fatalf("DocumentKeys: %v", err)
	}
	if len(keys) != docs {
		t.Fatalf("keys = %d, want %d", len(keys), docs)
	}

	// The listing's own answer is key names and nothing else — there is no
	// place in the returned shape for a value to be, which is the type
	// system carrying half the guarantee. The other half is that the
	// listing does not READ them, and the observable for that is the
	// per-call cost: this returns in the time a metadata pass takes
	// against a bucket holding 320 KiB of values.
	records, err := f.Documents(ctx, coord.FamilyWork, "i")
	if err != nil {
		t.Fatalf("Documents: %v", err)
	}
	var transferred int
	for _, r := range records {
		transferred += len(r.Value)
	}
	if transferred < docs*valueSize {
		t.Fatalf("the control listing moved %d bytes, want at least %d: the "+
			"fixture is not large enough for the comparison to mean anything",
			transferred, docs*valueSize)
	}
	t.Logf("a full listing moves %d bytes of values; a key listing returns %d "+
		"key names", transferred, len(keys))
}
