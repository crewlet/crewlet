package kv

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"testing"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/jsapi"
)

// A COORDINATION VALUE IS HELD TO WHAT ITS LEADER'S READ CARRIES BACK ACROSS A
// LINK. The leader's reply carries the value base64-encoded, a third larger,
// and a leaf closes its whole link on a reply past the transport. So
// coord.MaxValueBytes, with the largest write header (a run's widest standing
// in it), is the most that reply carries over a leaf: a value at it written
// with that standing round-trips whole from the leader, and one a byte past is
// refused by Put and Update before sending, as coord.ErrValueTooLarge.
//
// Mutation: drop the bound from Put or from Update, and its write a byte past
// it is stored; count kvHeaderBytes without the standing, or state the
// contract's bound past what the reply carries, and the derivation is refused.
func TestACoordinationValueIsHeldToWhatItsLeadersReadCarriesBackAcrossALink(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	client := leafClient(t)
	// openBucket waits out a leaf whose link is not up yet.
	raw, _, err := openBucket(ctx, client, false, jetstream.KeyValueConfig{
		Bucket: fmt.Sprintf("readback%d", bucketSeq.Add(1))})
	if err != nil {
		t.Fatalf("open the bucket through the leaf: %v", err)
	}
	b := settled(client, raw, AcrossLink, runsHolding)
	room := coord.MaxValueBytes
	if want := jsapi.MaxReadable(client.Conn().MaxPayload()) - kvHeaderBytes; room != want {
		t.Fatalf("a value is held to %d bytes, and a leaf's link carries back one of %d", room, want)
	}
	value := make([]byte, room)
	if _, err := rand.Read(value); err != nil {
		t.Fatal(err)
	}
	if _, err := b.(settledBucket).update(ctx, "at-the-bound", value, 0, &widestStanding); err != nil {
		t.Fatalf("a value at the bound, %d bytes, with the widest standing was refused: %v", room, err)
	}
	reader, err := newLeaderReader(client, raw)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := reader.last(ctx, "at-the-bound")
	if err != nil || !bytes.Equal(entry.Value(), value) {
		t.Fatalf("the value at the bound read back through the leaf = %v: want its %d bytes, "+
			"from the leader, the link up", err, room)
	}

	if _, err := b.Put(ctx, "past-the-bound", make([]byte, room+1)); !errors.Is(err, coord.ErrValueTooLarge) ||
		errors.Is(err, coord.ErrUnavailable) {
		t.Errorf("a put a byte past the bound = %v, want coord.ErrValueTooLarge and no outage: no "+
			"leaf could read it back", err)
	}
	rev, err := b.Put(ctx, "updated", []byte("small"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := b.Update(ctx, "updated", make([]byte, room+1), rev); !errors.Is(err, coord.ErrValueTooLarge) ||
		errors.Is(err, coord.ErrUnavailable) {
		t.Errorf("an update a byte past the bound = %v, want coord.ErrValueTooLarge and no outage: "+
			"no leaf could read it back", err)
	}
}

// A CREATE REFUSED FOR ITS VALUE'S SIZE WAS NEVER SENT. A create whose answer
// is not a refusal is marked as one that may have landed, which a person is
// told; a value past what a key holds is refused before sending, so its create
// reports too large, never unanswered, and nothing is written.
//
// Mutation: let the create mark a value's refusal as unanswered, and it reads
// as a write that may have landed.
func TestACreateRefusedForItsValuesSizeWasNeverSent(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	client := leafClient(t)
	raw, _, err := openBucket(ctx, client, false, jetstream.KeyValueConfig{
		Bucket: fmt.Sprintf("readback%d", bucketSeq.Add(1))})
	if err != nil {
		t.Fatalf("open the bucket through the leaf: %v", err)
	}
	b := settled(client, raw, AcrossLink, runsHolding)
	_, err = createKey(ctx, client, b, "too-large", make([]byte, coord.MaxValueBytes+1), nil)
	if !errors.Is(err, coord.ErrValueTooLarge) || errors.Is(err, errCreateUnanswered) {
		t.Fatalf("a create a byte past the bound = %v, want coord.ErrValueTooLarge and not a "+
			"write that may have landed", err)
	}
	if _, err := b.Get(ctx, "too-large"); !errors.Is(err, jetstream.ErrKeyNotFound) {
		t.Errorf("the key after its refused create = %v, want nothing written", err)
	}
}

// A REFUSAL MADE BEFORE ANYTHING WAS SENT IS NEVER AN OUTAGE, NOR A WRITE THAT
// MAY HAVE LANDED. A value too large is a definite answer no retry changes;
// marked unavailable or unknown, its caller would retry it forever or tell a
// person a change that never went out may have landed.
//
// Mutation: mark a value's refusal as unavailable, or as unknown, and its row
// reads as an outage.
func TestARefusalMadeBeforeAnythingWasSentIsNeverMarked(t *testing.T) {
	t.Parallel()
	refused := coord.HoldValue("crewlet_test/key", coord.MaxValueBytes+1, coord.MaxValueBytes)
	for name, err := range map[string]error{
		"unavailable":  unavailable("write the key", refused),
		"writeUnknown": writeUnknown("write the key", refused),
	} {
		if !errors.Is(err, coord.ErrValueTooLarge) || errors.Is(err, coord.ErrUnavailable) ||
			errors.Is(err, coord.ErrWriteUnknown) {
			t.Errorf("%s of a value past what a key holds = %v, want coord.ErrValueTooLarge "+
				"alone", name, err)
		}
	}
}
