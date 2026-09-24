package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/tracker"
)

// AN EVICTION IS WRITTEN AS WHOEVER PRESSED IT, through the node's own
// tracker writer.
//
// The route handed the gate nothing about its caller, so the record — and the
// row every node applies it into, the one `crewlet retention` prints as
// "evicted by" — named THIS NODE's writer: "who stopped node-9 writing" had
// one answer, the node that happened to serve the request. Mutation: hand the
// gate's writer through unchanged and the row names the node.
func TestAnEvictionIsWrittenAsWhoPressedIt(t *testing.T) {
	t.Parallel()
	boot := bootstrapFor(t, 0)
	company, err := config.ParseCompany([]byte(companyYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e, err := engine.New(t.Context(), engine.Options{Bootstrap: boot, Company: company})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	gate := nativeNodes(e)
	if gate == nil {
		t.Fatal("the node runs no tracker, so the case would certify nothing")
	}

	by := iam.Actor{Name: "jane.doe", Kind: iam.ActorOperator,
		OperatorID: "pat:0192f00d-0000-7000-8000-00000000000a"}
	if _, err := gate.EvictNode(t.Context(), uuid.NewString(), "node-9", by); err != nil {
		t.Fatalf("evict: %v", err)
	}
	stream := tracker.Domain{}.Stream().Name
	for deadline := time.Now().Add(10 * time.Second); ; {
		rows, err := tracker.Evictions(t.Context(), e.Backends().Store.Replicated(), stream)
		if err != nil {
			t.Fatalf("read the evictions: %v", err)
		}
		if len(rows) == 1 {
			if rows[0].NodeID != "node-9" || rows[0].By != by.Name {
				t.Errorf("the eviction of %s is recorded by %q, want %q — the "+
					"person who pressed it", rows[0].NodeID, rows[0].By, by.Name)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the eviction never applied: %+v", rows)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
