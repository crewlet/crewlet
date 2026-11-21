package main

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/kv/kvtest"
	"github.com/crewlet/crewlet/internal/iam"
)

// bootLinkRate is the pace the case holds a leaf's link to, each way: 160 KiB
// a second, about five times the slowest a link is allowed, so a body at its
// bound takes about fifty seconds to cross back, past the flat five seconds a
// read without a deadline of its own is given.
const bootLinkRate = 160 << 10

// A BOOT ACROSS A SLOW LINK READS THE ACTIVATION POINTER. A node without
// `data` joins a fleet running a company at its bound: its boot reads the
// pointer, with no deadline of its own, then compares the body to its file.
// With the sealed document in the pointer, the boot's first read carried eight
// mebibytes under the client's flat five seconds and the boot died; the
// pointer alone crosses at once, and the body is given its size's arrival.
//
// Mutation: carry the payload in the pointer again, and the boot fails.
func TestABootAcrossASlowLinkReadsTheActivationPointer(t *testing.T) {
	t.Parallel()
	across := kvtest.SlowLeaf(t, bootLinkRate)
	peer, err := across.Member.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "peer-revision", Summary: "from a peer", At: time.Now(),
		Payload: kvtest.SealedAtTheBound(t, fixtureCipher, []byte(`{"name":"Peer"}`), 1<<10),
		Origin: coord.RevisionOrigin{Author: "maya", AuthorKind: iam.ActorOperator,
			Source: "api", CreatedAt: time.Now()},
	})
	if err != nil {
		t.Fatalf("a peer's activation at its bound: %v", err)
	}
	db := seedStore(t)
	if err := bootSeed(t, db, across.Leaf, seedOf(parse(t, companyYAML)), fixtureCipher); err != nil {
		t.Fatalf("a boot across the leaf's link: %v", err)
	}
	assertFleetOn(t, across.Leaf, peer)
}
