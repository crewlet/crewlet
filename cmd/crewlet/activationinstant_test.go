package main

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/store"
)

// A NODE'S HISTORY KEEPS THE INSTANT THE POINTER PUBLISHED IT AT.
//
// The pointer publishes an instant later than the activation it replaces
// (coord.ActivationAt), so a node whose clock runs behind the one that
// activated last is published later than it asked. Its history row's
// `activated_at` is the instant its config history shows for the activation
// and the one the retention sweep dates the row by — so the row has to carry
// the pointer's, or the history dates an activation the fleet never made.
//
// Asserted before any tick, because the tick records the same instant when it
// meets the pointer: what is held here is the import's own record of its write.
//
// Mutation: drop the activation note after an import lands and the row carries
// no instant at all.
func TestAnImportKeepsThePointersInstantInTheHistory(t *testing.T) {
	t.Parallel()
	db := seedStore(t)
	fleet := coordmemory.NewFleet()
	if err := bootSeed(t, db, fleet, seedOf(parse(t, companyYAML)), fixtureCipher); err != nil {
		t.Fatalf("seed: %v", err)
	}
	first := bootRow(t, db)
	// A PEER WITH A CLOCK TWO HOURS AHEAD re-activates the same revision —
	// the credential-rotation gesture.
	if _, err := fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: first.ID, Payload: first.Payload, At: time.Now().Add(2 * time.Hour),
		Origin: originOf(first), Expect: first.ID,
	}); err != nil {
		t.Fatalf("peer activate: %v", err)
	}
	changed := strings.Replace(companyYAML, "name: Acme", "name: Acme Imported", 1)
	plan, err := decideSeed(t.Context(), &config.Bootstrap{}, overrideOf(parse(t, changed)),
		storeOf(db, fixtureCipher), quiet())
	if err != nil {
		t.Fatal(err)
	}
	if err := seedCompany(t.Context(), db, fleet, nil, plan, fixtureCipher, testNode,
		builds, quiet()); err != nil {
		t.Fatalf("import: %v", err)
	}
	target, _, err := fleet.Target(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	imported, held, err := db.Configs().Get(t.Context(), target.RevisionID)
	if err != nil || !held {
		t.Fatalf("the import %s is not in this node's history (held=%v err=%v)",
			target.RevisionID, held, err)
	}
	if store.EncodeTime(imported.ActivatedAt) != store.EncodeTime(target.At) {
		t.Fatalf("this node's history has %s activated at %s, and the pointer at %s — "+
			"it dates an activation the fleet never made",
			imported.ID, imported.ActivatedAt, target.At)
	}
}

// AN OFFER THE BOOT PUBLISHED IS SERVED AT THE POINTER'S INSTANT, by the apply.
//
// The boot row's served instant ([store.Revision.ServedAt]) is the activation
// this node APPLIED: what the engine stamps the chart and the knowledge spaces
// with at its next boot. A publish is not an apply — the revision may still be
// refused by every node — so the boot confirms the offer and leaves its served
// instant to the reconciler's first apply of the epoch it published.
//
// Mutation: write the published instant as the served one when the offer is
// confirmed, and the boot row carries it before anything has applied it.
func TestAPublishedOfferIsServedAtThePointersInstant(t *testing.T) {
	t.Parallel()
	db := seedStore(t)
	fleet := coordmemory.NewFleet()
	if err := bootSeed(t, db, fleet, seedOf(parse(t, companyYAML)), fixtureCipher); err != nil {
		t.Fatalf("seed: %v", err)
	}
	offered := offlineImport(t, db, strings.Replace(companyYAML, "name: Acme",
		"name: Acme Offline", 1))

	plan, err := decideSeed(t.Context(), &config.Bootstrap{}, tierBSeed{Path: "company.yaml"},
		storeOf(db, fixtureCipher), quiet())
	if err != nil {
		t.Fatal(err)
	}
	if !plan.activatedAt.IsZero() {
		t.Errorf("the engine boots an offer at %s, want no instant: nothing has "+
			"activated it", plan.activatedAt)
	}
	if err := seedCompany(t.Context(), db, fleet, nil, plan, fixtureCipher, testNode,
		builds, quiet()); err != nil {
		t.Fatalf("boot: %v", err)
	}
	target, _, err := fleet.Target(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if target.RevisionID != offered {
		t.Fatalf("the pointer names %s, want the offer %s published over its base",
			target.RevisionID, offered)
	}
	booted := bootRow(t, db)
	if booted.ID != offered || booted.Offer != nil {
		t.Fatalf("this node boots on %s (offer %+v), want the published offer %s, "+
			"confirmed", booted.ID, booted.Offer, offered)
	}
	if !booted.ServedAt.IsZero() {
		t.Errorf("the published offer boots at %s before any apply, want no instant "+
			"until the reconciler serves it", booted.ServedAt)
	}
	applies(t, db, fleet, fixtureCipher)
	if booted := bootRow(t, db); store.EncodeTime(booted.ServedAt) != store.EncodeTime(target.At) {
		t.Errorf("once applied the offer boots at %s, want the pointer's %s",
			booted.ServedAt, target.At)
	}
}
