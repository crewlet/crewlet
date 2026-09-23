package engine

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A RECORD ON A STATE LOG SIGNED UNDER A KEY THIS NODE LACKS REACHES THE AUDIT
// FEED, NAMING THE KEY AND WHAT THE NODE HOLDS.
//
// The broker has no auth of its own, so a frame under an unknown key is either
// a keyring rotation half done or something that is not the fleet writing to
// it — and an administrator watching the dashboard is the reader who needs to
// know which. The applier logs and counts it either way; the witness the
// engine hands every runner is what puts it where that reader looks.
//
// Mutation: build the runners without `Witness:` and nothing arrives.
func TestARecordUnderAnUnknownKeyReachesTheAuditFeed(t *testing.T) {
	t.Parallel()
	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	cfg, err := config.ParseCompany([]byte(nativeCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	back, err := OpenBackends(t.Context(), &b, cfg)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg, Backends: back})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })

	var mu sync.Mutex
	var heard []*events.Event
	back.Queue.AddPublishListener(func(_ context.Context, _ string, ev *events.Event) {
		if ev.Type == (types.RecordUnverifiable{}).EventType() {
			mu.Lock()
			heard = append(heard, ev)
			mu.Unlock()
		}
	})

	// A FRAME UNDER A KEY NOBODY IN THIS FLEET HOLDS, appended the way
	// anything that can reach the broker could.
	domain := tracker.Domain{}
	stranger, err := statelog.NewSigner(domain.Name(),
		statelog.OneKey("stranger", "material-no-node-holds"))
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	q, ok := back.Queue.(*jetstream.Queue)
	if !ok {
		t.Fatalf("the stream is %T, not the JetStream backend", back.Queue)
	}
	log, err := q.DomainLog(t.Context(), domain.Stream().Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	seq, _, err := log.Append(t.Context(), topics.TrackerLogPrefix+".task.forged",
		"", nil, stranger.Seal([]byte(`{"v":1}`)))
	if err != nil {
		t.Fatalf("append: %v", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(heard)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(heard) != 1 {
		t.Fatalf("heard %d unverifiable-record events, want 1", len(heard))
	}
	row, ok := events.DataAs[*types.RecordUnverifiable](heard[0])
	if !ok {
		t.Fatalf("the event carries %T", heard[0].Data)
	}
	if row.Domain != domain.Name() || row.KeyID != "stranger" || len(row.Held) == 0 ||
		row.Position != (statelog.Position{Stream: domain.Stream().Name, Seq: seq}).String() {
		t.Errorf("heard %+v, want the tracker's record at %d under key %q",
			row, seq, "stranger")
	}
	if heard[0].Source != e.id {
		t.Errorf("the event names %q as its source, want this node %q",
			heard[0].Source, e.id)
	}
}
