package engine

import (
	"errors"
	"testing"
	"time"

	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/queue/memory"
)

// A NODE THAT CANNOT HOLD WHAT IT WOULD RESERVE TAKES NO UPLOAD: with no object
// store, no coordination store or no estate it answers so, naming what is
// missing, rather than a pipeline that fails at its first byte.
func TestANodeWithoutItsStoresTakesNoUploads(t *testing.T) {
	t.Parallel()
	if _, err := (&Engine{}).ArtifactUploads(); !errors.Is(err, ErrNoUploads) {
		t.Fatalf("an engine with no object store answered %v", err)
	}
	store, err := objstore.NewStore(memobj.New())
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{objects: &objectStore{store: store}}
	if _, err := e.ArtifactUploads(); !errors.Is(err, ErrNoUploads) {
		t.Fatalf("an engine with no coordination store answered %v", err)
	}
}

// EVERY NODE WITH ITS STORES BUILDS ONE PIPELINE, once, over the estate router,
// so a node without `data` publishes as a data node does.
func TestANodeBuildsOnePipelineOverItsRouter(t *testing.T) {
	t.Parallel()
	store, err := objstore.NewStore(memobj.New())
	if err != nil {
		t.Fatal(err)
	}
	q := memory.NewBroker().Client()
	if err := q.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	router, err := estate.NewRouter(estate.RouterOptions{
		Self: "node-1", Queue: q, Placement: oneDataNode{}, Git: noGitNodes{},
		Session: estate.NewSession(), Envelopes: routerEnvelopes(t), Now: time.Now, Timing: estate.Production(),
	})
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{objects: &objectStore{store: store},
		backends: &Backends{Fleet: coordmemory.NewFleet()}, router: router}
	first, err := e.ArtifactUploads()
	if err != nil {
		t.Fatalf("the pipeline was not built: %v", err)
	}
	again, err := e.ArtifactUploads()
	if err != nil || again != first {
		t.Fatalf("a second call built another pipeline (%v)", err)
	}
	if got := e.artifactLimits(); got.PendingBytes == 0 || got.CompanyBytes == 0 {
		t.Fatalf("an engine with no Tier A stamps %+v, want the defaults", got)
	}
}
