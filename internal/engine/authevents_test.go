package engine_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/authevents"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/statelog"
)

// feed is what a node published, as its own publish listener heard it — the
// listener that writes the node's store row.
type feed struct {
	mu   sync.Mutex
	seen []*events.Event
}

func (f *feed) listen(e *engine.Engine) {
	e.Backends().Queue.AddPublishListener(func(_ context.Context, _ string, ev *events.Event) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.seen = append(f.seen, ev)
	})
}

func (f *feed) of(eventType string) []*events.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*events.Event
	for _, ev := range f.seen {
		if ev.Type == eventType {
			out = append(out, ev)
		}
	}
	return out
}

// THE IDENTITY WRITER THE NODE BUILDS ANNOUNCES ON THE NODE'S OWN FEED.
//
// The writer's announcement is only as real as the seam the engine fills: a
// writer built without it compiles, passes every iamdomain test and says
// nothing on a deployment. What is asserted is the ordinary path — the event
// is published through the node's queue under the node's name, which is what
// puts it in the store and on every dashboard.
//
// Mutation: drop `Events:` from the engine's WriterDeps and nothing arrives.
func TestTheNodesIdentityWriterAnnouncesOnItsFeed(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	if e.AuthEvents() == nil {
		t.Fatal("an engine New returned has no authentication audit trail")
	}
	heard := &feed{}
	heard.listen(e)
	if _, err := e.IAMWriter().InvalidateAll(t.Context(),
		statelog.NewOpID(time.Now(), "restore"), "restored from a backup"); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	bumped := (types.IAMSessionGenerationBumped{}).EventType()
	waitFor(t, "the generation bump on the node's feed", func() bool {
		return len(heard.of(bumped)) == 1
	})
	ev := heard.of(bumped)[0]
	row, ok := events.DataAs[*types.IAMSessionGenerationBumped](ev)
	if !ok {
		t.Fatalf("the event carries %T", ev.Data)
	}
	if row.Generation != 1 || row.Reason != "restored from a backup" || ev.Source == "" {
		t.Errorf("heard %+v from %q", row, ev.Source)
	}
}

// WHAT THE NODE WRITES FOR ITSELF IS THE ENGINE'S IN THE IDENTITY TRAIL.
//
// The node's own writer authors the sweeps, a re-seal, a password re-hashed at
// this build's cost and a company-wide invalidation, and every one of them
// reaches `iam_history` under the kind the writer was built with. Built as a
// machine, each of those rows was drawn as the login of a service account
// named after the node id; the engine is its own principal kind so that its
// housekeeping is never read as somebody's decision. The row names the node,
// as the engine, through no credential.
//
// Mutation: build the engine's identity writer as [iam.KindMachine] again and
// the row's kind is a machine.
func TestTheNodesOwnIdentityWritesAreRecordedAsTheEngine(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	result, err := e.IAMWriter().InvalidateAll(t.Context(),
		statelog.NewOpID(time.Now(), "restore"), "restored from a backup")
	if err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	if err := e.IAM().AwaitApplied(t.Context(), uint64(result.Position.Packed())); err != nil {
		t.Fatalf("wait for the invalidation to apply here: %v", err)
	}
	page, err := e.IAM().History(t.Context(), iamdomain.HistoryQuery{
		Op: iamdomain.OpInvalidate, Limit: 1,
	})
	if err != nil {
		t.Fatalf("read the identity trail: %v", err)
	}
	if len(page.Entries) != 1 {
		t.Fatalf("the trail holds %d invalidations, want the one written", len(page.Entries))
	}
	row := page.Entries[0]
	if row.Actor != e.Node().ID() || row.ActorKind != iam.KindEngine || row.OperatorID != "" {
		t.Errorf("the invalidation is authored (%q, %s, %q), want the node %q as "+
			"the engine through no credential", row.Actor, row.ActorKind,
			row.OperatorID, e.Node().ID())
	}
}

// STOPPING A NODE PUBLISHES THE MINUTE OF FAILED ATTEMPTS IT STILL HOLDS.
//
// The trail's loop publishes a minute once it has closed, so at shutdown the
// open one exists only in memory. The engine's teardown order is what saves
// it: the loop is stopped — and flushes — before the broker it publishes onto
// is closed. Reversed, the last minute of a brute-force attempt that ended in
// a restart is the one minute nobody can read.
//
// Mutation: stop the trail after the backends close and the row never arrives.
func TestStoppingANodePublishesTheFailuresItStillHolds(t *testing.T) {
	t.Parallel()
	e, err := engine.New(t.Context(), engine.Options{
		Bootstrap: bootstrap(t, func(b *config.Bootstrap) {
			brokertest.StoreIn(&b.Stream, filepath.Join(t.TempDir(), "stream"))
		}),
		Company: parsedCompany(t, companyDoc),
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	heard := &feed{}
	heard.listen(e)
	e.AuthEvents().Failed(t.Context(), authevents.Failure{
		Source: "203.0.113.9", Method: types.FailPassword,
	})
	e.Stop(context.Background())
	rows := heard.of((types.IAMLoginFailures{}).EventType())
	if len(rows) != 1 {
		t.Fatalf("heard %d failure rows after the stop, want the 1 it held", len(rows))
	}
}
