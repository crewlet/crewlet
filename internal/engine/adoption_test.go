package engine_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
)

// A REVISION ANOTHER NODE WROTE KEEPS ITS AUTHOR ON THIS ONE: every node but
// the writer meets it through the pointer, and adopts the origin the pointer
// carries rather than `peer`, `fleet` and the activation instant, so Settings ›
// Audit names one author whichever node serves it.
func TestAnAdoptedRevisionKeepsItsAuthor(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	written := pinnedNow.Add(-3 * time.Hour)
	if _, err := p.fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "written-on-node-b", Summary: "grow the company",
		Payload: p.seal(t, yamlToJSON(t, grownCompanyDoc)), At: pinnedNow,
		Origin: coord.RevisionOrigin{
			Author: "maya", AuthorKind: iam.ActorOperator, OperatorID: "token:deploy",
			Source: "api", CreatedAt: written,
		},
	}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	got, found, err := p.store.Configs().Get(t.Context(), "written-on-node-b")
	if err != nil || !found {
		t.Fatalf("the adopted revision is not in this node's history: found=%v err=%v", found, err)
	}
	if got.CreatedBy != "maya" || got.CreatedByKind != iam.ActorOperator ||
		got.OperatorID != "token:deploy" {
		t.Errorf("adopted as written by (%q, %q, %q), want (maya, operator, "+
			"token:deploy) — the author the origin recorded",
			got.CreatedBy, got.CreatedByKind, got.OperatorID)
	}
	if got.Source != "api" || !got.CreatedAt.Equal(written) {
		t.Errorf("adopted with source %q at %s, want api at %s — how and when it was "+
			"written, not when this node met it", got.Source, got.CreatedAt, written)
	}
	// ACTIVATED when the pointer says, not when written: applied here, it is
	// the boot row, at the instant this node's config history shows.
	if !got.Boots || !got.ActivatedAt.Equal(pinnedNow) {
		t.Errorf("adopted boots=%v at %s, want the boot row at the pointer's %s",
			got.Boots, got.ActivatedAt, pinnedNow)
	}
}
