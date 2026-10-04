package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
)

// A SEED IS THE NODE'S WRITE, UNDER THE NODE'S OWN ID AND OF THE SYSTEM KIND.
// Nobody ran a command: the node read its -company file at boot, and which
// node did it is the fact somebody reading the history later needs. The
// revision says so, and so does the pointer every peer adopts it from — which
// used to carry nothing, so the peers recorded `peer` and the audit screen
// drew the seed as an operator's. Mutation: drop the origin from the seed's
// activation and the pointer check fails; record the seed under a constant
// name and the row check does.
func TestASeedIsRecordedAsTheSystems(t *testing.T) {
	t.Parallel()
	db := seedStore(t)
	fleet := coordmemory.NewFleet()
	if err := seedCompany(t.Context(), db, fleet, nil, seedOf(parse(t, companyYAML)),
		fixtureCipher, testNode, quiet()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	active, found, err := db.Configs().Active(t.Context())
	if err != nil || !found {
		t.Fatalf("active: found=%v err=%v", found, err)
	}
	if active.CreatedBy != testNode || active.CreatedByKind != iam.ActorSystem ||
		active.OperatorID != "" {
		t.Errorf("the seed is recorded as (%q, %q, %q), want (%q, system, no credential)",
			active.CreatedBy, active.CreatedByKind, active.OperatorID, testNode)
	}
	target, _, err := fleet.Target(t.Context())
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	want := coord.RevisionOrigin{Author: testNode, AuthorKind: iam.ActorSystem, Source: "file"}
	got := target.Origin
	// The store keeps microseconds; the pointer carries the instant whole,
	// and a peer's copy lands at the store's precision too.
	if !got.CreatedAt.Truncate(time.Microsecond).Equal(active.CreatedAt) {
		t.Errorf("the pointer's origin was made at %v, want the row's %v",
			got.CreatedAt, active.CreatedAt)
	}
	got.CreatedAt = time.Time{}
	if got != want {
		t.Errorf("the pointer carries origin %+v, want %+v", got, want)
	}
}

// AN OFFLINE IMPORT REACHES THE FLEET AS ITS AUTHOR'S. `crewlet config import`
// run while the engine is stopped stores a revision that only this node's next
// boot publishes — and it must go up under whoever ran it, with the credential
// beside them, never under the node that happened to publish it.
func TestAPublishedLocalRevisionKeepsItsAuthor(t *testing.T) {
	t.Parallel()
	db := seedStore(t)
	document, err := json.Marshal(parse(t, companyYAML))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := secrets.Seal(fixtureCipher, document)
	if err != nil {
		t.Fatal(err)
	}
	written := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	if _, err := db.Configs().InsertActive(t.Context(), store.Revision{
		CreatedBy: "ops", CreatedByKind: iam.ActorOperator, OperatorID: "token:ops",
		Source: "file", Summary: "imported offline", Payload: sealed, CreatedAt: written,
	}); err != nil {
		t.Fatalf("store the offline import: %v", err)
	}
	fleet := coordmemory.NewFleet()
	if err := seedCompany(t.Context(), db, fleet, nil, tierBSeed{Path: "company.yaml"},
		fixtureCipher, testNode, quiet()); err != nil {
		t.Fatalf("boot: %v", err)
	}
	target, found, err := fleet.Target(t.Context())
	if err != nil || !found {
		t.Fatalf("no pointer published: found=%v err=%v", found, err)
	}
	want := coord.RevisionOrigin{Author: "ops", AuthorKind: iam.ActorOperator,
		OperatorID: "token:ops", Source: "file", CreatedAt: written}
	if got := target.Origin; got.Author != want.Author || got.AuthorKind != want.AuthorKind ||
		got.OperatorID != want.OperatorID || got.Source != want.Source ||
		!got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("the published pointer carries %+v, want %+v", got, want)
	}
}

// THE ENGINE'S WRITER RECORDS THE ACTOR IT IS HANDED. It is the reconcile
// loop's way onto the config surface, and the engine names itself — of the
// system kind — rather than leaving the surface to fill in a default, which
// read as an operator's on the audit screen.
func TestTheEnginesConfigWriterRecordsTheActorItIsHanded(t *testing.T) {
	t.Parallel()
	db := seedStore(t)
	fleet := coordmemory.NewFleet()
	if err := seedCompany(t.Context(), db, fleet, nil, seedOf(parse(t, companyYAML)),
		fixtureCipher, testNode, quiet()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	surface, err := configapi.New(configapi.Options{
		Store: db, Plane: fleet, Cipher: fixtureCipher,
		Holders: func(context.Context, []string) (map[string][]configapi.SeatHolder, error) {
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("configapi.New: %v", err)
	}
	by := iam.Actor{Name: "reconcile loop", Kind: iam.ActorSystem}
	if err := (engineConfigWriter{surface: surface}).Reload(t.Context(),
		"reload after sealing", by); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	active, _, err := db.Configs().Active(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if active.CreatedBy != by.Name || active.CreatedByKind != by.Kind {
		t.Errorf("the engine's reload is recorded as (%q, %q), want (%q, %q)",
			active.CreatedBy, active.CreatedByKind, by.Name, by.Kind)
	}
}
