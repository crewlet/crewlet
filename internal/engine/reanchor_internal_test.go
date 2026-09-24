package engine

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	natsjs "github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A REANCHOR AFTER A REBUILD UNDER A RUNNING NODE FOLLOWS THE LIVE STREAM, AND
// MOVES ONE DOMAIN.
//
// The instant an applier booted against names the DELETED stream once a log is
// rebuilt under a running node. A status verb that prints it asks the operator
// to confirm the dead stream; a transition that commits the moved checkpoint
// under it leaves an applier that stops, naming the recreation the verb was
// run to clear. And a transition that moves every registered domain's
// checkpoint to the rebuilt stream's numbers and instant stops every OTHER
// domain at its next boot, over a recreation that never happened to them.
//
// Mutations, each red here: the status read from the applier's boot identity
// names the deleted stream; the transition handed that identity refuses the
// live confirmation; the checkpoint committed under the old stream's instant
// stops the relaunched applier; every domain's checkpoint moved moves the
// pages log's; the follow skipped leaves the domain on the deleted stream's
// instant; the audit row written without the checkpoint's instant names the
// year one as the stream walked away from; and the audit row's record id
// spelled apart from the op id the record was published under names a record
// nothing published.
func TestAReanchorFollowsTheLiveStreamAndMovesOneDomain(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
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
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg, Backends: back})
	if err != nil {
		back.Close(context.Background())
		t.Fatalf("New: %v", err)
	}
	// ONCE, because the restart below needs the first node down before it
	// opens the same files, and a failure before that point still has to
	// release them.
	stopFirst := sync.OnceFunc(func() {
		e.Stop(context.Background())
		back.Close(context.Background())
	})
	t.Cleanup(stopFirst)
	s := e.native.log
	waitUntil(t, 20*time.Second, "the node to admit seats", e.NativeHydrated)
	running := s.Domain(tracker.Domain{}.Name())
	spec := tracker.Domain{}.Stream()
	untouched := func() map[string]statelog.Position {
		out := map[string]statelog.Position{}
		for _, d := range []statelog.Domain{pages.Domain{}, search.Domain{}} {
			at, _, _, err := statelog.CursorFor(t.Context(), back.Store.Replicated(),
				d.Stream().Name)
			if err != nil {
				t.Fatalf("read %s's checkpoint: %v", d.Name(), err)
			}
			out[d.Name()] = at
		}
		return out
	}
	before := untouched()

	q, ok := back.Queue.(interface{ Conn() *nats.Conn })
	if !ok {
		t.Fatal("the backends carry no broker connection to rebuild a stream on")
	}
	js, err := natsjs.New(q.Conn())
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	old, err := js.Stream(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("read the running stream: %v", err)
	}
	was := old.CachedInfo().Created.UTC().Truncate(time.Microsecond)

	// A RECORD APPLIED OFF THE OLD STREAM, so the checkpoint names it. A node
	// that never applied anything holds a checkpoint with no instant, which
	// reads as a first sight against any stream — so a transition that
	// committed under the wrong instant would pass unnoticed here.
	if _, err := e.native.writer.WriteView(t.Context(), "op-view", tracker.View{
		ID: "v-before", Name: "Before the rebuild", Type: tracker.ViewList,
		Container: tracker.Container{Kind: tracker.ContainerWorkspace},
	}); err != nil {
		t.Fatalf("write a view: %v", err)
	}
	waitUntil(t, 20*time.Second, "the checkpoint to name the old stream", func() bool {
		_, created, found, err := statelog.CursorFor(t.Context(),
			back.Store.Replicated(), spec.Name)
		return err == nil && found && created.Equal(was)
	})

	// THE REBUILD, under a node that never stops, with the SAME
	// configuration: what a rebuild changes is the stream's identity and its
	// sequences, and one that also changed a safety field would be refused
	// at the next boot for that instead.
	if err := js.DeleteStream(t.Context(), spec.Name); err != nil {
		t.Fatalf("delete the stream: %v", err)
	}
	rebuilt, err := js.CreateStream(t.Context(), old.CachedInfo().Config)
	if err != nil {
		t.Fatalf("rebuild the stream: %v", err)
	}
	live := rebuilt.CachedInfo().Created.UTC()
	s.publishPositions(t.Context())

	// THE STATUS NAMES THE LIVE STREAM, not the one this node booted on.
	created, generation, err := e.ReanchorStatus(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("ReanchorStatus: %v", err)
	}
	if !created.Equal(live) {
		t.Fatalf("the status names %s and the live stream was created at %s — "+
			"an operator echoing it re-anchors onto a stream that no longer "+
			"exists", created, live)
	}

	gen, err := e.Reanchor(t.Context(), ReanchorRequest{
		Stream: spec.Name, Confirm: created.Format(time.RFC3339Nano), By: "ops",
	})
	if err != nil {
		t.Fatalf("Reanchor: %v", err)
	}
	if gen != generation+1 {
		t.Fatalf("re-anchored to generation %d, want %d", gen, generation+1)
	}
	// THE AUDIT ROW NAMES BOTH STREAMS — the one walked away from is gone,
	// and this row is the only place its identity survives — AND THE
	// RECORD, by the op id it was published under.
	var prev, next int64
	var record string
	if err := back.Store.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `
			SELECT prev_stream_created_at, new_stream_created_at, record_id
			FROM tracker_log_generations WHERE generation = ?`, gen).Scan(
			&prev, &next, &record)
	}); err != nil {
		t.Fatalf("read generation %d's audit row: %v", gen, err)
	}
	subject := tracker.GenerationSubject(gen)
	holder, held, err := running.publisher.Holder(t.Context(),
		statelog.Subject{Kind: string(subject.Kind), ID: subject.ID})
	switch {
	case err != nil:
		t.Fatalf("read generation %d's record: %v", gen, err)
	case !held:
		t.Fatalf("no generation record holds generation %d on the new stream", gen)
	case record != holder:
		t.Errorf("the audit row names record %q, and the generation record on "+
			"the stream was published as %q", record, holder)
	}
	if got := store.DecodeTime(prev); !got.Equal(was) {
		t.Errorf("the audit row names the old stream as created at %s, want %s",
			got, was)
	}
	if got := store.DecodeTime(next); !got.Equal(live.Truncate(time.Microsecond)) {
		t.Errorf("the audit row names the new stream as created at %s, want %s",
			got, live)
	}
	if got := untouched(); got[pages.Domain{}.Name()] != before[pages.Domain{}.Name()] ||
		got[search.Domain{}.Name()] != before[search.Domain{}.Name()] {
		t.Fatalf("the other domains' checkpoints moved from %v to %v — a "+
			"recreation of one stream is a fact about that stream", before, got)
	}

	// THE DOMAIN FOLLOWS THE LIVE STREAM without a restart: its reads stop
	// refusing `wrong_stream` and its applier runs.
	if !running.identity().Equal(live) {
		t.Fatalf("the domain runs against %s after the transition, want the "+
			"live %s", running.identity(), live)
	}
	s.publishPositions(t.Context())
	health, err := s.health(t.Context(), running)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if code := health.Refusal(time.Now()); code == statelog.RefuseWrongStream {
		t.Fatal("the re-anchored domain still refuses wrong_stream")
	}
	if stopped := running.runner.Stopped(); stopped != nil {
		t.Fatalf("the re-anchored domain's applier is stopped: %v", stopped)
	}
	stopFirst()

	// AND THE NEXT BOOT RESUMES rather than stopping on the checkpoint the
	// transition committed.
	back2, err := OpenBackends(t.Context(), &b, cfg)
	if err != nil {
		t.Fatalf("OpenBackends again: %v", err)
	}
	t.Cleanup(func() { back2.Close(context.Background()) })
	e2, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg, Backends: back2})
	if err != nil {
		t.Fatalf("New again: %v", err)
	}
	t.Cleanup(func() { e2.Stop(context.Background()) })
	restarted := e2.native.log.Domain(tracker.Domain{}.Name())
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if stopped := restarted.runner.Stopped(); stopped != nil {
			t.Fatalf("after a reanchor the restarted node's tracker applier "+
				"stopped: %v", stopped)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := restarted.runner.Committed().Generation; got != gen {
		t.Errorf("the restarted node resumed at generation %d, want %d", got, gen)
	}
}

// A REANCHOR AND A RUNTIME ADOPTION NEVER RUN TOGETHER.
//
// Both rewrite this node's replicated estate with appliers halted. An adoption
// that starts during a reanchor replaces the file the reanchor is writing and
// relaunches every applier, the one the reanchor is holding down among them; a
// reanchor that starts during an adoption writes a file about to be replaced;
// and a second reanchor halts and relaunches the loop the first is holding
// down.
//
// Mutation: drop the reanchor from the rejoin's single-flight check and the
// adoption starts mid-transition; drop either check from beginReanchor and the
// reanchor starts mid-adoption, or beside another.
func TestAReanchorAndARuntimeAdoptionExcludeEachOther(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	s := &stateLog{run: t.Context()}
	s.rejoin = func(context.Context) error {
		<-release
		return nil
	}
	adopting := func() bool {
		s.rejoinMu.Lock()
		defer s.rejoinMu.Unlock()
		return s.rejoining
	}

	if err := s.beginReanchor(); err != nil {
		t.Fatalf("the first reanchor: %v", err)
	}
	s.requestRejoin(time.Now())
	if adopting() {
		t.Fatal("an adoption started while a reanchor was running")
	}
	if err := s.beginReanchor(); !errors.Is(err, errTransitionRunning) {
		t.Fatalf("a second reanchor beside the first = %v, want it refused", err)
	}
	s.endReanchor()

	s.requestRejoin(time.Now())
	if !adopting() {
		t.Fatal("no adoption started once the reanchor had finished")
	}
	if err := s.beginReanchor(); !errors.Is(err, errTransitionRunning) {
		t.Fatalf("a reanchor during an adoption = %v, want it refused", err)
	}
	close(release)
	s.done.Wait()
	if err := s.beginReanchor(); err != nil {
		t.Fatalf("a reanchor after the adoption finished: %v", err)
	}
	s.endReanchor()
}
