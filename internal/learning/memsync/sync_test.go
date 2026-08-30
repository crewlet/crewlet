package memsync

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/store"
)

// broker starts an in-process JetStream with the memory stream provisioned
// the way the queue provisions it — one message retained per subject, which
// is what turns the stream from a log into a keyed table.
func broker(t *testing.T) *nats.Conn {
	t.Helper()
	ns, err := server.NewServer(&server.Options{
		ServerName: "memsync-test", JetStream: true, Port: -1,
		DontListen: true, StoreDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("configure the broker: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(30 * time.Second) {
		ns.Shutdown()
		t.Fatal("the broker did not become ready")
	}
	t.Cleanup(func() { ns.Shutdown(); ns.WaitForShutdown() })

	conn, err := nats.Connect("", nats.InProcessServer(ns))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)

	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if _, err := js.CreateOrUpdateStream(t.Context(), jetstream.StreamConfig{
		Name:              topics.MemoryStream,
		Subjects:          []string{topics.MemoryPrefix + ">"},
		Storage:           jetstream.FileStorage,
		Retention:         jetstream.LimitsPolicy,
		MaxMsgsPerSubject: 1,
	}); err != nil {
		t.Fatalf("provision the memory stream: %v", err)
	}
	return conn
}

func syncerOn(t *testing.T, db *store.DB, conn *nats.Conn) *Syncer {
	t.Helper()
	s, err := New(db, conn, func(string) string { return seat.AgentID })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s == nil {
		t.Fatal("New returned nil with a store and a connection")
	}
	return s
}

// THE FIX, end to end and through a real broker: a seat runs on one node,
// then moves to a node that has never seen it, and arrives remembering.
func TestASeatCarriesItsMemoryToANewNode(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	ctx := context.Background()

	oldOwner := openStore(t)
	seedMemory(t, oldOwner)
	if published, err := syncerOn(t, oldOwner, conn).Publish(ctx, seat.Handle); err != nil {
		t.Fatalf("publish: %v", err)
	} else if published != len(tables) {
		t.Fatalf("published %d rows, want one per table (%d)", published, len(tables))
	}

	// A different node: its own store, which has never run this seat.
	newOwner := openStore(t)
	for _, spec := range tables {
		if got := countRows(t, newOwner, spec.name); got != 0 {
			t.Fatalf("the new owner already holds %d %s rows", got, spec.name)
		}
	}

	carried, err := syncerOn(t, newOwner, conn).Hydrate(ctx, seat.Handle)
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	if carried != len(tables) {
		t.Fatalf("hydrated %d rows, want %d", carried, len(tables))
	}
	for _, spec := range tables {
		if got := countRows(t, newOwner, spec.name); got != 1 {
			t.Errorf("%s has %d rows after hydration, want 1", spec.name, got)
		}
	}
	var content string
	if err := newOwner.SQL().QueryRowContext(ctx,
		"SELECT content FROM agent_diary WHERE id = 'd1'").Scan(&content); err != nil {
		t.Fatalf("the seat did not arrive with its diary: %v", err)
	}
	if content != "the release train is thursdays" {
		t.Errorf("the carried diary entry says %q", content)
	}
}

// The changelog is a KEYED TABLE, not a log: publishing a seat repeatedly
// must leave the stream holding one message per row, or a long-lived seat's
// replay would grow without bound and hydration would slow down for ever.
func TestRepublishingCollapsesOntoOneMessagePerRow(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	ctx := context.Background()
	db := openStore(t)
	seedMemory(t, db)

	syncer := syncerOn(t, db, conn)
	for range 4 {
		// A fresh watermark each time is what a restarted node does, and
		// it is the case that would grow the stream if compaction were
		// not doing its job.
		syncer.Forget(seat.Handle)
		if _, err := syncer.Publish(ctx, seat.Handle); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	stream, err := js.Stream(ctx, topics.MemoryStream)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	if info.State.Msgs != uint64(len(tables)) {
		t.Fatalf("the stream holds %d messages after four publishes of %d rows — "+
			"it is accumulating a log rather than keeping a table",
			info.State.Msgs, len(tables))
	}
}

// The watermark is what keeps the steady-state cost proportional to what the
// seat just learned. A second publish with nothing new must carry nothing —
// except the small mutable tables, which are carried whole by design.
func TestASecondPublishCarriesOnlyWhatIsNew(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	ctx := context.Background()
	db := openStore(t)
	seedMemory(t, db)

	syncer := syncerOn(t, db, conn)
	if _, err := syncer.Publish(ctx, seat.Handle); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	again, err := syncer.Publish(ctx, seat.Handle)
	if err != nil {
		t.Fatalf("second publish: %v", err)
	}
	whole := 0
	for _, spec := range tables {
		if spec.wholeEachCycle {
			whole++
		}
	}
	if again != whole {
		t.Fatalf("a second publish carried %d rows, want only the %d "+
			"whole-each-cycle tables", again, whole)
	}
}

// One seat's replay must not carry another's memory into this node.
func TestHydrationTakesOnlyTheSeatItAsksFor(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	ctx := context.Background()

	db := openStore(t)
	seedMemory(t, db)
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO episodes (id, agent_handle, agent_role, turn_id, started_at,
		 ended_at, plan_summary, task_summary, tool_sequence, review_outcome,
		 duration_ms, kind)
		 VALUES ('theirs', 'ceo', 'CEO', 't9', 0, 0, 'p', 't', '[]', 'done', 1, 'raw')`,
	); err != nil {
		t.Fatalf("seed a peer's episode: %v", err)
	}
	// Publish both seats. The peer's syncer resolves its own agent id.
	syncerOn(t, db, conn).Publish(ctx, seat.Handle)
	peer, err := New(db, conn, func(string) string { return "other-agent-id" })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := peer.Publish(ctx, "ceo"); err != nil {
		t.Fatalf("publish the peer: %v", err)
	}

	newOwner := openStore(t)
	if _, err := syncerOn(t, newOwner, conn).Hydrate(ctx, seat.Handle); err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	var handle string
	if err := newOwner.SQL().QueryRowContext(ctx,
		"SELECT agent_handle FROM episodes").Scan(&handle); err != nil {
		t.Fatalf("read episodes: %v", err)
	}
	if handle != seat.Handle {
		t.Errorf("hydrating %q brought %q's episode", seat.Handle, handle)
	}
	if got := countRows(t, newOwner, "episodes"); got != 1 {
		t.Errorf("the new owner holds %d episodes, want only the seat's own", got)
	}
}

// A seat with nothing recorded hydrates to nothing rather than hanging or
// failing: a brand-new company's first acquisition is exactly this case.
func TestHydratingASeatWithNoMemoryIsAQuietNoOp(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	newOwner := openStore(t)

	carried, err := syncerOn(t, newOwner, conn).Hydrate(context.Background(), "nobody")
	if err != nil {
		t.Fatalf("hydrating an unknown seat: %v", err)
	}
	if carried != 0 {
		t.Errorf("hydrated %d rows for a seat that has never run", carried)
	}
}
