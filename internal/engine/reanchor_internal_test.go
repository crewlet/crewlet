package engine

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	natsjs "github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
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
// Mutations, each red here: the status read from the domain's identity names
// the deleted stream; the transition handed that identity refuses the live
// confirmation; the checkpoint committed under the old stream's instant stops
// the relaunched applier; every domain's checkpoint moved moves the pages
// log's; the follow skipped leaves the domain on the deleted stream's instant;
// the audit row written without the checkpoint's instant names the year one as
// the stream walked away from; and the audit row's record id spelled apart
// from the op id the record was published under names a record nothing
// published; a heartbeat that states no instant leaves every peer judging this
// node by its generation alone; and one that states the live stream's rather
// than the one its checkpoint counts on tells every peer this node holds the
// live stream's history before it has applied a record of it.
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
	// THE ROW STATES THE STREAM THE CHECKPOINT COUNTS ON — the deleted one,
	// until this node follows the live one — because that is what a peer's
	// reanchor judges this node's history by.
	stated := func() time.Time {
		t.Helper()
		rows, err := back.Fleet.Positions(t.Context())
		if err != nil {
			t.Fatalf("read the positions register: %v", err)
		}
		for _, row := range rows {
			if row.NodeID == s.nodeID {
				return row.Domains[tracker.Domain{}.Name()].StreamCreatedAt
			}
		}
		t.Fatalf("the register holds no row for %s", s.nodeID)
		return time.Time{}
	}
	if got := stated(); !got.Truncate(time.Microsecond).Equal(was) {
		t.Fatalf("after the rebuild this node's row states stream %s, want the "+
			"deleted %s its applier still runs on", got, was)
	}

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
	if got := stated(); !got.Equal(live) {
		t.Errorf("after following, this node's row states stream %s, want the "+
			"live %s", got, live)
	}
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
// Each refusal is a REFUSAL — the operator waits for the transition that is
// running — rather than a failure to run again at once.
//
// Mutation: drop the reanchor from the rejoin's single-flight check and the
// adoption starts mid-transition; drop either check from beginReanchor and the
// reanchor starts mid-adoption, or beside another; drop the refusal from
// either and it reads as a failure.
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
	if err := s.beginReanchor(); !errors.Is(err, errTransitionRunning) ||
		!errors.Is(err, statelog.ErrReanchorRefused) {
		t.Fatalf("a second reanchor beside the first = %v, want it refused", err)
	}
	s.endReanchor()

	s.requestRejoin(time.Now())
	if !adopting() {
		t.Fatal("no adoption started once the reanchor had finished")
	}
	if err := s.beginReanchor(); !errors.Is(err, errTransitionRunning) ||
		!errors.Is(err, statelog.ErrReanchorRefused) {
		t.Fatalf("a reanchor during an adoption = %v, want it refused", err)
	}
	close(release)
	s.done.Wait()
	if err := s.beginReanchor(); err != nil {
		t.Fatalf("a reanchor after the adoption finished: %v", err)
	}
	s.endReanchor()
}

// ONLY A PEER ON THE LIVE STREAM HOLDS HISTORY A REANCHOR WOULD DISCARD.
//
// ONE DEFINITION, because the permission check counts these and the refusal
// names them: if the two disagreed, a reanchor would refuse naming nobody, or
// permit while naming someone. What a row is judged on is the stream its
// numbers count on — the instant it states — at any generation, because the
// peer that holds the live stream's history is the one that re-anchored onto
// it, a generation above this node. A row from a build that states no instant
// is judged by this node's generation, the only test it allows.
//
// Mutation: judge a stated instant by the generation instead and the
// re-anchored peer goes uncounted while the rebuilt peer is counted.
func TestOnlyAPeerOnTheLiveStreamHoldsHistoryAReanchorWouldDiscard(t *testing.T) {
	t.Parallel()
	deleted := time.Date(2031, 4, 1, 3, 0, 0, 0, time.UTC)
	live := time.Date(2031, 4, 2, 3, 0, 0, 123_456_789, time.UTC)
	rows := []coord.NodePositions{
		{NodeID: "self", Domains: map[string]coord.DomainPosition{
			"tracker": {Generation: 3, AppliedThrough: 900, StreamCreatedAt: deleted},
		}},
		{NodeID: "re-anchored", Domains: map[string]coord.DomainPosition{
			"tracker": {Generation: 4, AppliedThrough: 5, StreamCreatedAt: live},
		}},
		{NodeID: "on-the-deleted-stream", Domains: map[string]coord.DomainPosition{
			"tracker": {Generation: 3, AppliedThrough: 900, StreamCreatedAt: deleted},
		}},
		{NodeID: "followed-applied-nothing", Domains: map[string]coord.DomainPosition{
			"tracker": {Generation: 4, AppliedThrough: 0, StreamCreatedAt: live},
		}},
		{NodeID: "older-build-this-generation", Domains: map[string]coord.DomainPosition{
			"tracker": {Generation: 3, AppliedThrough: 900},
		}},
		{NodeID: "older-build-another-generation", Domains: map[string]coord.DomainPosition{
			"tracker": {Generation: 2, AppliedThrough: 900},
		}},
		{NodeID: "runs-another-domain", Domains: map[string]coord.DomainPosition{
			"vectors": {Generation: 4, AppliedThrough: 900, StreamCreatedAt: live},
		}},
	}
	got := hydratedPeers(rows, "tracker", 3, live, "self")
	want := []string{"re-anchored", "older-build-this-generation"}
	if !slices.Equal(got, want) {
		t.Fatalf("hydrated peers = %v, want %v: this node is not its own peer, a "+
			"peer still counting on the deleted stream holds none of the live "+
			"one's history, one that has applied nothing holds no history, and "+
			"a row stating no instant is judged by this node's generation",
			got, want)
	}
}

// A FLEET WHOSE STREAM WAS REBUILT UNDER IT CAN RE-ANCHOR.
//
// A rebuild leaves every node at the generation it had, having applied records
// off a stream that no longer exists. Counted by generation, every node would
// count every other as hydrated, each one's reanchor would be refused naming
// the others — and no flag overrides that refusal and nothing expires it, so
// the fleet could never follow its own log. Counted by the stream each row
// states, nobody is hydrated on the live stream until somebody re-anchors onto
// it, and then that node is exactly the one the others are told to adopt from.
//
// And the most-caught-up rule compares one stream's numbers: the node furthest
// along the deleted stream is the one permitted, whatever a node still on an
// older stream reports.
//
// Mutation: count by generation alone and the furthest node is refused naming
// its peers; take the highest position across every stream and it is refused
// over node-d's number on a stream the fleet left before this one.
func TestAFleetWhoseStreamWasRebuiltCanReanchor(t *testing.T) {
	t.Parallel()
	older := time.Date(2031, 3, 1, 3, 0, 0, 0, time.UTC)
	deleted := time.Date(2031, 4, 1, 3, 0, 0, 0, time.UTC)
	live := time.Date(2031, 4, 2, 3, 0, 0, 0, time.UTC)
	on := func(seq uint64, generation uint32, stream time.Time) coord.DomainPosition {
		return coord.DomainPosition{Seq: seq, AppliedThrough: seq,
			Generation: generation, StreamCreatedAt: stream}
	}
	// node-d NEVER FOLLOWED the reanchor before this one: it still reports
	// a position on the stream the fleet left then.
	fleet := func(b coord.DomainPosition) []coord.NodePositions {
		var rows []coord.NodePositions
		for node, at := range map[string]coord.DomainPosition{
			"node-a": on(900, 3, deleted), "node-b": b,
			"node-c": on(880, 3, deleted), "node-d": on(5000, 2, older),
		} {
			rows = append(rows, coord.NodePositions{NodeID: node,
				Domains: map[string]coord.DomainPosition{"tracker": at}})
		}
		return rows
	}
	inputs := func(rows []coord.NodePositions, self string, position uint64) statelog.ReanchorInputs {
		return statelog.ReanchorInputs{
			StreamCreatedAt: live, Generation: 3, Position: position,
			Highest:          highestOn(rows, "tracker", 3, deleted),
			PeersHydrated:    len(hydratedPeers(rows, "tracker", 3, live, self)),
			RegisterReadable: true,
		}
	}
	confirmed := statelog.ReanchorGuard{Confirm: live.Format(time.RFC3339)}

	rows := fleet(on(905, 3, deleted))
	for _, self := range []string{"node-a", "node-b", "node-c"} {
		if got := hydratedPeers(rows, "tracker", 3, live, self); len(got) != 0 {
			t.Errorf("%s counts %v as hydrated on the live stream, and every one "+
				"of them is counting on another", self, got)
		}
	}
	if _, err := statelog.PermitReanchor(inputs(rows, "node-b", 905), confirmed); err != nil {
		t.Fatalf("the rebuilt fleet's furthest node was refused: %v", err)
	}
	if _, err := statelog.PermitReanchor(inputs(rows, "node-a", 900), confirmed); err == nil {
		t.Fatal("a node behind its peers on the deleted stream was permitted " +
			"unforced, so the records only node-b applied are what it discards")
	}

	// ONCE node-b HAS RE-ANCHORED AND APPLIED ITS RECORD, it is the peer the
	// others adopt from — a generation above them.
	rows = fleet(on(1, 4, live))
	if got := hydratedPeers(rows, "tracker", 3, live, "node-a"); !slices.Equal(got, []string{"node-b"}) {
		t.Fatalf("after node-b re-anchored, node-a counts %v as hydrated, want "+
			"[node-b]", got)
	}
}

// A REFUSED REANCHOR LEAVES THE DOMAIN'S APPLIER RUNNING, AND ITS ROWS AS THEY
// WERE.
//
// A refusal is the ordinary answer on a healthy fleet — a confirmation that
// names another instant, a generation another reanchor's record holds, a peer
// hydrated on the live stream — and a transition that stopped the loop before
// deciding would interrupt the domain it refused: every caller waiting on the
// loop told it stopped, the caught-up latch cleared when it started again. So
// the permission and the generation's holder are read while the loop runs, and
// a refusal touches nothing: not the loop, and not one row's version.
//
// Mutation: halt the applier before the permission is decided and each
// refusal below ends the loop it found and starts another; decide the holder
// only by the claim after the halt and the held generation does the same.
func TestARefusedReanchorLeavesTheApplierRunning(t *testing.T) {
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
	t.Cleanup(func() { back.Close(context.Background()) })
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg, Backends: back})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	s := e.native.log
	waitUntil(t, 20*time.Second, "the node to admit seats", e.NativeHydrated)
	running := s.Domain(tracker.Domain{}.Name())
	spec := tracker.Domain{}.Stream()

	// THE LOOP'S OWN DONE CHANNEL, which a halt closes and a relaunch
	// replaces: the same open channel after a call is the same loop,
	// never interrupted.
	loop := func() chan struct{} {
		s.applyMu.Lock()
		defer s.applyMu.Unlock()
		return running.applyDone
	}
	stillRunning := func(refusal string, before chan struct{}) {
		t.Helper()
		if after := loop(); after != before {
			t.Fatalf("%s: the refused reanchor ended the tracker's apply loop "+
				"and started another", refusal)
		}
		select {
		case <-before:
			t.Fatalf("%s: the refused reanchor ended the tracker's apply loop", refusal)
		default:
		}
	}

	created, generation, err := e.ReanchorStatus(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("ReanchorStatus: %v", err)
	}
	// A ROW WITH A VERSION, which a transition's reset would raise into the
	// next generation's number space.
	if _, err := e.native.writer.WriteView(t.Context(), "op-view", tracker.View{
		ID: "v-kept", Name: "Kept as it is", Type: tracker.ViewList,
		Container: tracker.Container{Kind: tracker.ContainerWorkspace},
	}); err != nil {
		t.Fatalf("write a view: %v", err)
	}
	version := func() int64 {
		t.Helper()
		var v int64
		if err := back.Store.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRowContext(t.Context(),
				`SELECT version FROM tracker_views WHERE id = 'v-kept'`).Scan(&v)
		}); err != nil {
			t.Fatalf("read the view's version: %v", err)
		}
		return v
	}
	waitUntil(t, 20*time.Second, "the view to apply", func() bool {
		var n int
		err := back.Store.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRowContext(t.Context(),
				`SELECT COUNT(*) FROM tracker_views WHERE id = 'v-kept'`).Scan(&n)
		})
		return err == nil && n == 1
	})
	kept := version()

	before := loop()
	if before == nil {
		t.Fatal("the tracker's applier is not running")
	}
	if _, err := e.Reanchor(t.Context(), ReanchorRequest{
		Stream: spec.Name, Confirm: created.Add(time.Hour).Format(time.RFC3339),
		By: "ops",
	}); !errors.Is(err, statelog.ErrReanchorRefused) {
		t.Fatalf("a confirmation naming another instant = %v, want a refusal", err)
	}
	stillRunning("a confirmation naming another instant", before)

	// ANOTHER REANCHOR'S RECORD HOLDS THE GENERATION this one would take:
	// the claim would meet it, and the read before the halt does.
	rival, err := tracker.NewWriter(tracker.WriterDeps{
		Publisher: running.publisher, NodeID: "node-b",
		Actor: "ops-b", ActorKind: tracker.AuthorOperator,
	})
	if err != nil {
		t.Fatalf("build node-b's writer: %v", err)
	}
	if err := rival.PublishGeneration(t.Context(), generation+1,
		statelog.ReanchorInputs{StreamCreatedAt: created}); err != nil {
		t.Fatalf("publish node-b's generation record: %v", err)
	}
	_, err = e.Reanchor(t.Context(), ReanchorRequest{
		Stream: spec.Name, Confirm: created.Format(time.RFC3339Nano), By: "ops",
	})
	var elsewhere *statelog.ClaimedElsewhere
	if !errors.Is(err, statelog.ErrReanchorRefused) || !errors.As(err, &elsewhere) {
		t.Fatalf("a reanchor onto a generation another record holds = %v, want "+
			"a refusal naming the holder", err)
	}
	stillRunning("a generation another record holds", before)
	if got := version(); got != kept {
		t.Errorf("the refused reanchor moved a row's version from %d to %d", kept, got)
	}

	// A PEER HYDRATED ON THE LIVE STREAM, which is what a healthy fleet's
	// every other node is.
	if err := back.Fleet.PutPositions(t.Context(), coord.NodePositions{
		NodeID: "node-peer", At: time.Now().UTC(),
		Domains: map[string]coord.DomainPosition{tracker.Domain{}.Name(): {
			Seq: 7, AppliedThrough: 7,
			Generation:      running.runner.Committed().Generation,
			StreamCreatedAt: created,
		}},
	}); err != nil {
		t.Fatalf("publish the peer's row: %v", err)
	}
	_, err = e.Reanchor(t.Context(), ReanchorRequest{
		Stream: spec.Name, Confirm: created.Format(time.RFC3339Nano), By: "ops",
	})
	if !errors.Is(err, statelog.ErrReanchorRefused) {
		t.Fatalf("a reanchor beside a hydrated peer = %v, want a refusal", err)
	}
	if !strings.Contains(err.Error(), "node-peer") {
		t.Errorf("the refusal does not name the hydrated peer: %v", err)
	}
	stillRunning("a hydrated peer", before)
}

// A NODE THAT BOOTS OVER A REBUILT LOG STATES THE STREAM ITS CHECKPOINT COUNTS
// ON, AND IS JUDGED — AND JUDGES ITS PEERS — BY IT.
//
// A node restarted after its log was rebuilt hands its applier the live
// stream, and the applier stops on a checkpoint recorded under the deleted
// one: it commits nothing, so every number the node states for the domain is
// still a position on the deleted stream. Stated against the live stream
// instead, each restarted node told every peer it had applied records off the
// live stream — every reanchor was refused naming a peer, the register has no
// age to clear it, and the most-caught-up comparison saw only other restarted
// nodes. The fleet could never follow its own log.
//
// So the row states the checkpoint's stream, the permission's two readings are
// taken against the two streams they are about — the highest position on the
// stream this node's checkpoint counts on, and the peers hydrated on the live
// one — and a reanchor moves the domain onto the live stream.
//
// Mutations, each red here: take the identity from the live stream at boot and
// the row states it with a position counted on the deleted one; swap the two
// instants in reanchorInputs and the peer that re-anchored sets the highest
// position while the peer furthest along the deleted stream is named
// hydrated.
func TestANodeBootedOverARebuiltLogStatesTheStreamItsCheckpointCountsOn(t *testing.T) {
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
	waitUntil(t, 20*time.Second, "the node to admit seats", e.NativeHydrated)
	spec := tracker.Domain{}.Stream()
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

	// A RECORD APPLIED OFF THE OLD STREAM, so the checkpoint names it and
	// the node has a position to state on it.
	if _, err := e.native.writer.WriteView(t.Context(), "op-view", tracker.View{
		ID: "v-before", Name: "Before the rebuild", Type: tracker.ViewList,
		Container: tracker.Container{Kind: tracker.ContainerWorkspace},
	}); err != nil {
		t.Fatalf("write a view: %v", err)
	}
	var applied uint64
	var generation uint32
	waitUntil(t, 20*time.Second, "the checkpoint to name the old stream", func() bool {
		at, created, found, err := statelog.CursorFor(t.Context(),
			back.Store.Replicated(), spec.Name)
		applied, generation = at.Seq, at.Generation
		return err == nil && found && created.Equal(was) && at.Seq > 0
	})

	// THE REBUILD, and then the restart: the same configuration, a new
	// creation instant, sequences counting from 1 again.
	if err := js.DeleteStream(t.Context(), spec.Name); err != nil {
		t.Fatalf("delete the stream: %v", err)
	}
	rebuilt, err := js.CreateStream(t.Context(), old.CachedInfo().Config)
	if err != nil {
		t.Fatalf("rebuild the stream: %v", err)
	}
	live := rebuilt.CachedInfo().Created.UTC()
	stopFirst()

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
	s := e2.native.log
	running := s.Domain(tracker.Domain{}.Name())
	waitUntil(t, 10*time.Second, "the applier to stop on the old checkpoint", func() bool {
		return errors.Is(running.runner.Stopped(), statelog.ErrStreamRecreated)
	})

	// THE ROW STATES THE DELETED STREAM, with the position counted on it.
	if got := running.identity(); !got.Equal(was) {
		t.Fatalf("the domain states stream %s, want the deleted %s its "+
			"checkpoint counts on — the live %s is a stream it has applied "+
			"nothing of", got, was, live)
	}
	s.publishPositions(t.Context())
	rows, err := back2.Fleet.Positions(t.Context())
	if err != nil {
		t.Fatalf("read the positions register: %v", err)
	}
	var own coord.DomainPosition
	for _, row := range rows {
		if row.NodeID == s.nodeID {
			own = row.Domains[tracker.Domain{}.Name()]
		}
	}
	if !own.StreamCreatedAt.Equal(was) || own.AppliedThrough != applied {
		t.Fatalf("the restarted node's row states %d applied on stream %s, want "+
			"%d on the deleted %s — a peer reads the live stream's instant with "+
			"a nonzero position as history a reanchor would discard",
			own.AppliedThrough, own.StreamCreatedAt, applied, was)
	}

	// A PEER ON EACH STREAM: one further along the deleted stream, and one
	// that re-anchored onto the live one — at a higher sequence, so a
	// comparison against the wrong stream picks the wrong peer.
	for _, peer := range []coord.NodePositions{
		{NodeID: "node-deleted", At: time.Now().UTC(),
			Domains: map[string]coord.DomainPosition{tracker.Domain{}.Name(): {
				Seq: applied + 10, AppliedThrough: applied + 10,
				Generation: generation, StreamCreatedAt: was,
			}}},
		{NodeID: "node-live", At: time.Now().UTC(),
			Domains: map[string]coord.DomainPosition{tracker.Domain{}.Name(): {
				Seq: applied + 5000, AppliedThrough: applied + 5000,
				Generation: generation + 1, StreamCreatedAt: live,
			}}},
	} {
		if err := back2.Fleet.PutPositions(t.Context(), peer); err != nil {
			t.Fatalf("publish %s's row: %v", peer.NodeID, err)
		}
	}
	in, hydrated := e2.reanchorInputs(t.Context(), running)
	if in.Highest != applied+10 {
		t.Errorf("the highest position on this node's stream is %d, want node-"+
			"deleted's %d — the node that re-anchored counts on another log",
			in.Highest, applied+10)
	}
	if !slices.Equal(hydrated, []string{"node-live"}) || in.PeersHydrated != 1 {
		t.Errorf("the peers hydrated on the live stream are %v (%d), want "+
			"[node-live]", hydrated, in.PeersHydrated)
	}
	if !in.StreamCreatedAt.Equal(live) || !in.PrevStreamCreatedAt.Equal(was) {
		t.Errorf("the inputs name the live stream %s and the one walked away "+
			"from %s, want %s and %s", in.StreamCreatedAt, in.PrevStreamCreatedAt,
			live, was)
	}

	// AND ONCE NEITHER PEER STANDS IN ITS WAY, THIS NODE RE-ANCHORS and
	// states the live stream from then on.
	for _, peer := range []string{"node-deleted", "node-live"} {
		if err := back2.Fleet.ForgetPositions(t.Context(), peer); err != nil {
			t.Fatalf("forget %s: %v", peer, err)
		}
	}
	if _, err := e2.Reanchor(t.Context(), ReanchorRequest{
		Stream: spec.Name, Confirm: live.Format(time.RFC3339Nano), By: "ops",
	}); err != nil {
		t.Fatalf("Reanchor: %v", err)
	}
	if got := running.identity(); !got.Equal(live) {
		t.Fatalf("after the reanchor the domain states %s, want the live %s", got, live)
	}
	waitUntil(t, 10*time.Second, "the re-anchored applier to run", func() bool {
		return running.runner.Stopped() == nil &&
			running.runner.Committed().Generation == generation+1
	})
}
