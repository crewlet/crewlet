package engine_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// EVERY NODE RECORDS ITS CAPTURE STAMP VERSION BEFORE IT CLAIMS PRESENCE, at
// Start, which every node runs whatever its roles, so the media gate reads the
// version once the node is offline; a node whose record cannot be written does
// not start, and holds no presence to settle a run under.
//
// Mutation: write no record at Start, or write it after the node claims
// presence, and it goes red.
func TestEveryNodeRecordsItsCaptureStampBeforeItClaimsPresence(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("start: %v", err)
	}
	got, err := e.Backends().Fleet.CaptureStamps(t.Context())
	if err != nil || len(got) != 1 || got[0].CaptureStampVersion != sandbox.CaptureStampVersion {
		t.Errorf("the started node's records read %+v, %v, want its own at this build's %d", got, err,
			sandbox.CaptureStampVersion)
	}

	b := bootstrap(t, func(b *config.Bootstrap) {
		brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	})
	back, err := openBackends(t, b)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	borrowed := *back
	borrowed.Fleet = unrecordedStamps{back.Fleet}
	refused := newEngine(t, engine.Options{Bootstrap: b, Backends: &borrowed})
	if err := refused.Start(t.Context()); err == nil {
		t.Fatal("a node whose capture stamp record could not be written started")
	}
	held, err := refused.Backends().Coord.ListLive(t.Context(), coord.ClassNode)
	if err != nil || len(held) != 0 {
		t.Errorf("a node whose record could not be written holds presence %+v (%v), want none", held, err)
	}
}

// unrecordedStamps is a fleet whose capture stamp records cannot be written.
type unrecordedStamps struct{ stampedFleet }

// stampedFleet names the embedded fleet so the field is not called Fleet,
// which is one of the interface's own methods.
type stampedFleet = coord.Fleet

func (unrecordedStamps) PutCaptureStamp(context.Context, coord.CaptureStampRecord) error {
	return errors.New("the store did not answer")
}
