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
// LINK.
//
// Every read of a key is the stream leader's, whose reply carries the value
// base64-encoded, a third larger — and a leaf's server closes its whole link
// to the fleet on a reply past the transport, every lease and mailbox the node
// holds across it with it (crewlet/crewlet#1031). So the contract's bound on
// a value, coord.MaxValueBytes, is the largest whose message — with the most
// header a write of the client's carries — that reply carries back over a
// leaf's link, and a settled bucket refuses a value past it: one at the bound
// is written through a leaf and read back through it whole, from the leader,
// and one a byte past it is refused, by a put and by an update alike, before
// anything is sent, as coord.ErrValueTooLarge — never an outage.
//
// Mutation: drop the bound from Put or from Update, and its write a byte past
// it is stored; or state the contract's bound past what the reply carries,
// and the derivation is refused.
func TestACoordinationValueIsHeldToWhatItsLeadersReadCarriesBackAcrossALink(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	client := leafClient(t)
	// THROUGH THE ONE DOOR EVERY BUCKET IS OPENED BY, which waits out a leaf
	// whose link to its member is not up yet.
	raw, _, err := openBucket(ctx, client, false, jetstream.KeyValueConfig{
		Bucket: fmt.Sprintf("readback%d", bucketSeq.Add(1))})
	if err != nil {
		t.Fatalf("open the bucket through the leaf: %v", err)
	}
	b := settled(client, raw, AcrossLink)
	room := coord.MaxValueBytes
	if want := jsapi.MaxReadable(client.Conn().MaxPayload()) - kvHeaderBytes; room != want {
		t.Fatalf("a value is held to %d bytes, and a leaf's link carries back one of %d", room, want)
	}
	value := make([]byte, room)
	if _, err := rand.Read(value); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Put(ctx, "at-the-bound", value); err != nil {
		t.Fatalf("a value at the bound, %d bytes, was refused: %v", room, err)
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

// A CREATE REFUSED FOR ITS VALUE'S SIZE WAS NEVER SENT.
//
// A create writes through the bucket and, where the write's answer is
// anything but a refusal, marks it as one that may have landed — the mark a
// chat read's flush turns into "the write may have landed" for the person who
// made it. A value past what a key holds is refused before anything is sent,
// so its create says so as it is: too large, never unanswered, and nothing is
// written.
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
	b := settled(client, raw, AcrossLink)
	_, err = createKey(ctx, client, b, "too-large", make([]byte, coord.MaxValueBytes+1))
	if !errors.Is(err, coord.ErrValueTooLarge) || errors.Is(err, errCreateUnanswered) {
		t.Fatalf("a create a byte past the bound = %v, want coord.ErrValueTooLarge and not a "+
			"write that may have landed", err)
	}
	if _, err := b.Get(ctx, "too-large"); !errors.Is(err, jetstream.ErrKeyNotFound) {
		t.Errorf("the key after its refused create = %v, want nothing written", err)
	}
}

// A REFUSAL MADE BEFORE ANYTHING WAS SENT IS NEVER AN OUTAGE, NOR A WRITE THAT
// MAY HAVE LANDED.
//
// The store marks what it could not hear as unavailable, and a write whose
// answer it lost as one that may have landed; a value past what a key holds
// is refused before it is sent, a definite answer no retry changes, and
// marked either way its caller retried it for good or told a person a change
// that never went out may have landed.
//
// Mutation: mark a value's refusal as unavailable, or as unknown, and its row
// reads as an outage.
func TestARefusalMadeBeforeAnythingWasSentIsNeverMarked(t *testing.T) {
	t.Parallel()
	refused := coord.HoldValue("crewlet_test/key", coord.MaxValueBytes+1)
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
