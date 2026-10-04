package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/config"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/store"
)

// A SEED IS THE NODE'S WRITE, UNDER THE NODE'S OWN ID. Nobody ran a command:
// the node read its -company file at boot. The revision says so, and so does
// the pointer every peer adopts it from — which used to carry nothing, so the
// peers recorded `peer` and the audit screen drew the seed as an operator's.
func TestASeedIsRecordedAsTheNodes(t *testing.T) {
	t.Parallel()
	db := seedStore(t)
	fleet := coordmemory.NewFleet()
	if err := seedCompany(t.Context(), db, fleet, nil, seedOf(parse(t, companyYAML)),
		nil, testNode, quiet()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	active, found, err := db.Configs().Active(t.Context())
	if err != nil || !found {
		t.Fatalf("active: found=%v err=%v", found, err)
	}
	if active.CreatedBy != testNode || active.CreatedByKind != store.AuthorNode {
		t.Errorf("the seed is recorded as (%q, %q), want (%q, node)",
			active.CreatedBy, active.CreatedByKind, testNode)
	}
	target, _, err := fleet.Target(t.Context())
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	if target.Origin.Author != testNode || target.Origin.AuthorKind != "node" ||
		target.Origin.Source != "file" ||
		// The store keeps microseconds; the pointer carries the instant
		// whole, and a peer's copy lands at the store's precision too.
		!target.Origin.CreatedAt.Truncate(time.Microsecond).Equal(active.CreatedAt) {
		t.Errorf("the pointer carries origin %+v, want %s, node, file, %s",
			target.Origin, testNode, active.CreatedAt)
	}
}

// AN OFFLINE IMPORT REACHES THE FLEET AS THE OPERATOR'S. `crewlet config
// import` run while the engine is stopped stores an operator's revision that
// only this node's next boot publishes — and it must go up under the
// operator who ran it, never under the node that happened to publish it.
func TestAPublishedLocalRevisionKeepsItsOperator(t *testing.T) {
	t.Parallel()
	db := seedStore(t)
	document, err := json.Marshal(parse(t, companyYAML))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Configs().InsertActive(t.Context(), store.Revision{
		CreatedBy: "ops", CreatedByKind: store.AuthorOperator, Source: "file",
		Summary: "imported offline", Payload: document,
	}); err != nil {
		t.Fatalf("store the offline import: %v", err)
	}
	fleet := coordmemory.NewFleet()
	if err := seedCompany(t.Context(), db, fleet, nil, tierBSeed{Path: "company.yaml"},
		nil, testNode, quiet()); err != nil {
		t.Fatalf("boot: %v", err)
	}
	target, found, err := fleet.Target(t.Context())
	if err != nil || !found {
		t.Fatalf("no pointer published: found=%v err=%v", found, err)
	}
	if target.Origin.Author != "ops" || target.Origin.AuthorKind != "operator" {
		t.Errorf("the published pointer names (%q, %q), want (ops, operator)",
			target.Origin.Author, target.Origin.AuthorKind)
	}
}

// THE ENGINE'S WRITER WRITES AS THE NODE. It is the reconcile loop's way onto
// the config surface, and the label it is handed ("reconcile loop") names a
// subsystem, not a person — recorded under the surface's HTTP default it
// read as an operator's on the audit screen.
func TestTheEnginesConfigWriterWritesAsTheNode(t *testing.T) {
	t.Parallel()
	db, err := store.OpenNode(t.Context(), filepath.Join(t.TempDir(), "w.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	fleet := coordmemory.NewFleet()
	if err := seedCompany(t.Context(), db, fleet, nil, seedOf(parse(t, companyYAML)),
		nil, testNode, quiet()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	surface, err := configapi.New(configapi.Options{
		Store: db, Plane: fleet,
		// A DEPLOYMENT THE SEEDED COMPANY CAN RUN ON: its native backends
		// keep their logs on the stream, which needs a store directory.
		Bootstrap: &config.Bootstrap{Stream: config.Stream{StoreDir: t.TempDir()}},
	})
	if err != nil {
		t.Fatalf("configapi.New: %v", err)
	}
	if err := (engineConfigWriter{surface: surface}).Reload(t.Context(),
		"reload after sealing", "reconcile loop"); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	active, _, err := db.Configs().Active(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if active.CreatedBy != "reconcile loop" || active.CreatedByKind != store.AuthorNode {
		t.Errorf("the engine's reload is recorded as (%q, %q), want (reconcile loop, node)",
			active.CreatedBy, active.CreatedByKind)
	}
}
