package tracker

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/statelog"
)

// claimWriter is a node's writer over an in-memory coordination backend: the
// whole of what a claim needs, and nothing a claim does not.
func claimWriter(t *testing.T) *Writer {
	t.Helper()
	w, err := NewWriter(WriterDeps{
		Publisher: &statelog.Publisher{}, Claims: memory.New(), NodeID: "node-a",
		Actor: "node-a", ActorKind: AuthorSystem,
	})
	if err != nil {
		t.Fatalf("build the writer: %v", err)
	}
	return w
}

// A WALK IS HELD ONCE ON ITS OWN NODE TOO.
//
// A lease names its holder by node, and a same-owner claim is a RENEW — so two
// walks of one duplicate on one node (two requests, or the duty finishing a
// merge this node is still walking) were both told yes, and the first to end
// released the lease the second was still walking under. Every copy of the
// node's writer shares the in-process half, which is what a surface serving
// many parties hands out.
func TestAWalkIsHeldOnceOnItsOwnNodeToo(t *testing.T) {
	t.Parallel()
	w := claimWriter(t)
	first, err := w.hold(t.Context(), mergeClaim("dup"))
	if err != nil {
		t.Fatalf("the first walk could not take its claim: %v", err)
	}
	second := w.As("ana", AuthorHuman, Provenance{})
	if _, err := second.hold(t.Context(), mergeClaim("dup")); !errors.Is(err, errWalkRunning) {
		t.Fatalf("a second walk of the same duplicate on the same node = %v, "+
			"want it refused as already running", err)
	}
	if _, err := second.hold(t.Context(), mergeClaim("other")); err != nil {
		t.Errorf("a walk of a DIFFERENT duplicate was refused: %v", err)
	}
	first.release(t.Context())
	again, err := second.hold(t.Context(), mergeClaim("dup"))
	if err != nil {
		t.Fatalf("the claim is still refused after its walk released it: %v", err)
	}
	again.release(t.Context())
}

// A BULK IS ADMITTED ONCE ON ITS OWN NODE TOO, for the same reason: a second
// bulk from this node renewed the fleet lease rather than being refused by it,
// and the first to finish released the lease the second was still applying
// under — admitting a third from anywhere.
func TestABulkIsAdmittedOnceOnItsOwnNodeToo(t *testing.T) {
	t.Parallel()
	w := claimWriter(t)
	release, err := w.admit(t.Context(), 10)
	if err != nil {
		t.Fatalf("the first bulk was refused: %v", err)
	}
	if _, err := w.As("ana", AuthorHuman, Provenance{}).admit(t.Context(), 10); !errors.Is(err, ErrBulkInFlight) {
		t.Fatalf("a second bulk on the same node = %v, want ErrBulkInFlight", err)
	}
	release()
	again, err := w.admit(t.Context(), 10)
	if err != nil {
		t.Fatalf("a bulk after the first finished was refused: %v", err)
	}
	again()
}
