package configapi_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
)

// THE ENGINE'S OWN WRITE READS AS THE ENGINE'S — on the row and on the
// pointer every peer adopts the revision from.
//
// The reconcile loop writes through this same service rather than through the
// HTTP door, and under the HTTP defaults its reload after sealing a credential
// read as an operator's on the audit screen. The writer states its kind, and
// the engine's is [iam.ActorSystem]: the vocabulary every durable author column
// writes, so the audit screen reads one set of kinds whichever trail it is on.
// Mutation: drop Origin from the activation request and the pointer check
// fails; write the kind as anything but the actor's and the row check does.
func TestTheEnginesWriteIsRecordedAsTheSystems(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, companyDoc)

	by := iam.Actor{Name: "node-a", Kind: iam.ActorSystem}
	applied, err := s.service().Reload(t.Context(), "reload after sealing", by)
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	got, found, err := s.configs.Get(t.Context(), applied.RevisionID)
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if got.CreatedBy != by.Name || got.CreatedByKind != iam.ActorSystem || got.OperatorID != "" {
		t.Errorf("stored as (%q, %q, %q), want (node-a, system, no credential)",
			got.CreatedBy, got.CreatedByKind, got.OperatorID)
	}
	target, _, err := s.plane.Target(t.Context())
	if err != nil {
		t.Fatalf("Target: %v", err)
	}
	want := coord.RevisionOrigin{
		Author: by.Name, AuthorKind: iam.ActorSystem, Source: "api", CreatedAt: pinned,
	}
	if !target.Origin.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("the pointer's origin was made at %v, want the write's own %v",
			target.Origin.CreatedAt, want.CreatedAt)
	}
	target.Origin.CreatedAt = want.CreatedAt
	if target.Origin != want {
		t.Errorf("the pointer carries origin %+v, want %+v — every peer adopts "+
			"the revision from it", target.Origin, want)
	}
}

// A WRITE THAT DOES NOT SAY WHAT WROTE IT IS NOT STORED. Every caller of the
// service states an author; one that forgot would otherwise put a row in the
// history that a reader has to guess about.
func TestAWriteWithNoAuthorKindIsRefused(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, companyDoc)
	before := s.activeDocument(t)
	if _, err := s.service().Apply(t.Context(), configapi.ApplyRequest{
		Patch: []byte(`{"mission":"x"}`), Summary: "s", By: iam.Actor{Name: "who"},
	}); err == nil {
		t.Fatal("a write with no author kind was stored and activated")
	}
	if after := s.activeDocument(t); after != before {
		t.Error("a refused write changed the active document")
	}
}
