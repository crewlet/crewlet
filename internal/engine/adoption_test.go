package engine_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
)

// A REVISION ANOTHER NODE WROTE KEEPS ITS AUTHOR ON THIS ONE.
//
// A revision is stored first by the node that served the write, and every
// other node meets it through the pointer. Those nodes recorded it as written
// by `peer`, from `fleet`, at the instant it was activated — so one revision
// had a different author on every node, and Settings › Audit, reading
// whichever node served it, could not say an operator had written it. The
// pointer carries the origin now, and the adoption stores what it carries.
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
}

// A POINTER WHOSE ORIGIN SAYS NOTHING NAMES NOBODY, AND THE ADOPTION SAYS SO.
//
// The row records the author as NOT RECORDED — never this node, and never the
// `peer` placeholder, which read on the audit screen as a writer that does not
// exist. What the pointer does say still holds of this node's copy: it came
// from the fleet, activated then.
func TestAPointerWithNoOriginIsAdoptedWithNoAuthor(t *testing.T) {
	t.Parallel()
	p := newPlane(t)
	if _, err := p.fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "with-no-origin", Summary: "s",
		Payload: p.seal(t, yamlToJSON(t, grownCompanyDoc)), At: pinnedNow,
	}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	got, found, err := p.store.Configs().Get(t.Context(), "with-no-origin")
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if got.CreatedBy != "" || got.CreatedByKind != "" {
		t.Errorf("adopted as written by (%q, %q), want no author: nothing said who",
			got.CreatedBy, got.CreatedByKind)
	}
	if got.Source != "fleet" || !got.CreatedAt.Equal(pinnedNow) {
		t.Errorf("source %q at %s, want fleet at the activation %s",
			got.Source, got.CreatedAt, pinnedNow)
	}
}
