package memsync

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsapi/jsapitest"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// broker starts an in-process JetStream holding the account a node's broker
// holds: every stream the queue provisions ([js.EngineStreams]). The memory
// stream among them retains one message per subject, which makes it a keyed
// table, and carries the creation instant its create stamped, as the queue's
// create stamps it.
func broker(t *testing.T) *nats.Conn {
	t.Helper()
	_, conn := brokerServer(t)
	return conn
}

// brokerServer is [broker] with the server beside the connection, for a case
// that stands between a read and the stream's leader ([jsapitest]).
func brokerServer(t *testing.T) (*server.Server, *nats.Conn) {
	t.Helper()
	return brokerServerCarrying(t, 0)
}

// brokerCarrying is [broker] whose server holds every connection to messages
// of maxPayload bytes, its max_payload, for a case about the largest message
// the transport carries.
func brokerCarrying(t *testing.T, maxPayload int32) *nats.Conn {
	t.Helper()
	_, conn := brokerServerCarrying(t, maxPayload)
	return conn
}

// brokerServerCarrying is [brokerServer] at a max_payload of maxPayload bytes,
// and at nats-server's own default, a mebibyte, where it is 0.
func brokerServerCarrying(t *testing.T, maxPayload int32) (*server.Server, *nats.Conn) {
	t.Helper()
	return brokerServerWith(t, func(opts *server.Options) { opts.MaxPayload = maxPayload })
}

// brokerServerWith is [brokerServer] with configure's changes to its options,
// provisioned through and answering on a connection made with connect's
// options too, for a case about what a server's settings refuse a connection.
func brokerServerWith(t *testing.T, configure func(*server.Options),
	connect ...nats.Option) (*server.Server, *nats.Conn) {

	t.Helper()
	dir := brokertest.TempDir(t)
	opts := &server.Options{
		ServerName: "memsync-test", JetStream: true, Port: -1,
		DontListen: true, StoreDir: dir,
		JetStreamMaxStore: brokertest.StoreMaxBytes,
	}
	configure(opts)
	ns, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("configure the broker: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(30 * time.Second) {
		ns.Shutdown()
		t.Fatal("the broker did not become ready")
	}
	t.Cleanup(func() { ns.Shutdown(); ns.WaitForShutdown() })

	conn, err := nats.Connect("", append([]nats.Option{nats.InProcessServer(ns)}, connect...)...)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)

	client, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	// PROVISIONED ONCE, AS THE QUEUE PROVISIONS IT, beside the engine's other
	// streams: a case about a remade stream only deletes and creates it. The
	// broker removes a deleted stream's files after the delete answers, and
	// the account's stream directory with them when nothing else is in it,
	// from under a create already begun ("error creating store for stream").
	for _, stream := range js.EngineStreams(js.Config{StoreDir: dir}, time.Now()) {
		if _, err := client.CreateStream(t.Context(), stream); err != nil {
			t.Fatalf("provision %s as the queue provisions it: %v", stream.Name, err)
		}
	}
	return ns, conn
}

// jsOn is a JetStream client over conn in the API this test's broker speaks:
// it is started raw, with no domain, so a client addresses the account's own
// JetStream.
func jsOn(t *testing.T, conn *nats.Conn) jetstream.JetStream {
	t.Helper()
	client, err := jsapi.Account().Client(conn)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	return client
}

// syncerOn is a syncer of the seat over db on the broker conn reaches. It
// HOLDS the seat as the node that has held it all along ([heldFromEmpty]), so
// a case publishes the seat with it as a node holding it does; a hydration it
// runs takes the seat anew, its holding with it.
func syncerOn(t *testing.T, db *store.DB, conn *nats.Conn) *Syncer {
	t.Helper()
	s := newSyncer(db, jsOn(t, conn), func(string) string { return seat.AgentID })
	if s == nil {
		t.Fatal("New returned nil with a store and a connection")
	}
	return heldFromEmpty(s, seat.Handle)
}

// A seat moved to a node that has never seen it arrives remembering, end to
// end through a real broker.
func TestASeatCarriesItsMemoryToANewNode(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	ctx := context.Background()

	oldOwner := openStore(t)
	seedMemory(t, oldOwner)
	if published, err := syncerOn(t, oldOwner, conn).PublishOwn(ctx, seat.Handle); err != nil {
		t.Fatalf("publish: %v", err)
	} else if published.Rows != len(tables) {
		t.Fatalf("published %d rows, want one per table (%d)", published.Rows, len(tables))
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

// A HYDRATION WHOSE IDENTITY READ A MEMBER DROPS ASKS AGAIN RATHER THAN
// REFUSING THE SEAT.
//
// The changelog's identity comes from its stream's info, which only the
// leader answers; a member still believing in a lost leader drops the
// request silently, so asked once it waited out the hydration's whole bound
// and refused the seat over a second-long election.
func TestAHydrationWhoseIdentityReadAMemberDropsAsksAgain(t *testing.T) {
	t.Parallel()
	ns, conn := brokerServer(t)
	ctx := context.Background()
	oldOwner := openStore(t)
	seedMemory(t, oldOwner)
	if _, err := syncerOn(t, oldOwner, conn).PublishOwn(ctx, seat.Handle); err != nil {
		t.Fatalf("publish: %v", err)
	}
	held := jsapitest.DropFirst(t, ns, jsapitest.StreamInfo(topics.MemoryStream), 1)

	carried, err := syncerOn(t, openStore(t), conn).Hydrate(ctx, seat.Handle)
	if err != nil {
		t.Fatalf("a hydration whose identity read the broker dropped once "+
			"refused the seat rather than asking again: %v", err)
	}
	if carried != len(tables) {
		t.Errorf("hydrated %d rows, want %d", carried, len(tables))
	}
	if n := held.Count(); n != 1 {
		t.Errorf("the broker held %d stream infos, want the 1 it dropped", n)
	}
}

// A HYDRATION WHOSE BARRIER A MEMBER DROPS ASKS AGAIN RATHER THAN REFUSING THE
// SEAT.
//
// The barrier every table's replay reads to is published before the first
// table, and only the leader takes it; a member that still believes it leads
// drops it with no reply, which the client's own retry (on `no responders`
// alone) never re-asks, so sent once it waited out the fifteen-second bound.
// It is asked again at the read term, as a metadata read is. The broker here
// silently drops the first publish to the seat's barrier subject.
//
// Mutation: publish the barrier once, with no re-ask, and this goes red.
func TestAHydrationWhoseBarrierAMemberDropsAsksAgain(t *testing.T) {
	t.Parallel()
	ns, conn := brokerServer(t)
	ctx := context.Background()
	oldOwner := openStore(t)
	seedMemory(t, oldOwner)
	if _, err := syncerOn(t, oldOwner, conn).PublishOwn(ctx, seat.Handle); err != nil {
		t.Fatalf("publish: %v", err)
	}
	held := jsapitest.DropFirst(t, ns, barrierSubject(seat.AgentID), 1)

	carried, err := syncerOn(t, openStore(t), conn).Hydrate(ctx, seat.Handle)
	if err != nil {
		t.Fatalf("a hydration whose barrier the broker dropped once refused the seat "+
			"rather than asking again: %v", err)
	}
	if carried != len(tables) {
		t.Errorf("hydrated %d rows, want %d", carried, len(tables))
	}
	if n := held.Count(); n != 1 {
		t.Errorf("the broker held %d publishes of the barrier, want the 1 it dropped", n)
	}
}

// noLeaderOnceJS is a JetStream whose first publish to the seat's barrier
// subject found no stream leader: the client's own retries, on `no
// responders`, all met none, and it answered [jetstream.ErrNoStreamResponse].
type noLeaderOnceJS struct {
	jetstream.JetStream
	once    sync.Once
	refused atomic.Int32
}

func (j *noLeaderOnceJS) Publish(ctx context.Context, subject string, data []byte,
	opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {

	first := false
	if subject == barrierSubject(seat.AgentID) {
		j.once.Do(func() { first = true })
	}
	if first {
		j.refused.Add(1)
		return nil, jetstream.ErrNoStreamResponse
	}
	return j.JetStream.Publish(ctx, subject, data, opts...)
}

// A BARRIER NO STREAM LEADER TOOK IS PUBLISHED AGAIN.
//
// With no leader nothing subscribes to the changelog's subjects: the client
// retries `no responders` twice, a quarter of a second apart, then answers
// jetstream.ErrNoStreamResponse, which an election past half a second
// outlasts. That is silence, not an answer, so the barrier is asked again
// after the read's pause. Here the first publish finds no leader.
//
// Mutation: take the client's no-stream-response for an answer, and this goes
// red.
func TestABarrierNoStreamLeaderTookIsPublishedAgain(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	oldOwner := openStore(t)
	seedMemory(t, oldOwner)
	if _, err := syncerOn(t, oldOwner, conn).PublishOwn(t.Context(), seat.Handle); err != nil {
		t.Fatalf("publish: %v", err)
	}

	syncer := syncerOn(t, openStore(t), conn)
	missed := &noLeaderOnceJS{JetStream: syncer.js}
	syncer.js = missed
	carried, err := syncer.Hydrate(t.Context(), seat.Handle)
	if err != nil {
		t.Fatalf("a hydration whose barrier found no stream leader once refused the seat "+
			"rather than publishing it again: %v", err)
	}
	if carried != len(tables) {
		t.Errorf("hydrated %d rows, want %d", carried, len(tables))
	}
	if n := missed.refused.Load(); n != 1 {
		t.Errorf("%d publishes of the barrier found no leader, want the 1 this case refused", n)
	}
}

// refusingJS is a JetStream whose broker answers every publish of the seat's
// barrier with refusal, as a broker answers a publish it will not store.
type refusingJS struct {
	jetstream.JetStream
	refusal error
}

func (j refusingJS) Publish(ctx context.Context, subject string, data []byte,
	opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {

	if subject == barrierSubject(seat.AgentID) {
		return nil, j.refusal
	}
	return j.JetStream.Publish(ctx, subject, data, opts...)
}

// A BROKER AT ITS STORAGE LIMIT REFUSES THE SEAT WITH ITS OWN ANSWER.
//
// A hydration's first write is the seat's barrier; an account at its storage
// limit refuses it, and the seat is refused with the broker's answer whole,
// which says what to raise. Here the broker answers as nats-server's
// JSAccountResourcesExceededErr does.
//
// Mutation: word the refusal without wrapping the broker's answer, and this
// goes red.
func TestABrokerAtItsStorageLimitRefusesTheSeatWithItsOwnAnswer(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	holder := openStore(t)
	seedMemory(t, holder)
	if _, err := syncerOn(t, holder, conn).PublishOwn(t.Context(), seat.Handle); err != nil {
		t.Fatalf("publish: %v", err)
	}

	fresh := openStore(t)
	syncer := syncerOn(t, fresh, conn)
	full := &jetstream.APIError{Code: 400, ErrorCode: 10002, Description: "resource limits exceeded for account"}
	syncer.js = refusingJS{JetStream: syncer.js, refusal: full}
	carried, err := syncer.Hydrate(t.Context(), seat.Handle)
	var answer *jetstream.APIError
	if !errors.As(err, &answer) || answer.ErrorCode != full.ErrorCode {
		t.Fatalf("a hydration whose barrier the broker refused answered (%d, %v), want the seat "+
			"refused with the broker's own refusal", carried, err)
	}
	if held := storeHolds(t, fresh); carried != 0 || held != 0 {
		t.Errorf("the refused hydration reported %d rows and left %d in the store, want none: it "+
			"reads no row before its barrier", carried, held)
	}
}

// A BARRIER THE BROKER WILL NOT LET THIS NODE PUBLISH IS REFUSED BY NAME.
//
// A broker denying publish permission on the memory subjects never answers
// the barrier: it sends a permissions error the client keeps as the
// connection's last error, and the hydration names that refusal rather than
// timing out on nothing an operator can act on. Here a user may publish
// anywhere but crewlet.memory.>, another published the seat's rows, and the
// hydration is cut at three seconds, which its first two requests fit in,
// rather than the barrier's thirty.
//
// Mutation: refuse an unanswered barrier without reading the connection's last
// error, and this goes red.
func TestABarrierTheBrokerWillNotLetThisNodePublishIsRefusedByName(t *testing.T) {
	t.Parallel()
	users := func(opts *server.Options) {
		opts.Users = []*server.User{
			{Username: "fleet", Password: "fleet"},
			{Username: "satellite", Password: "satellite", Permissions: &server.Permissions{
				Publish: &server.SubjectPermission{Deny: []string{topics.MemoryPrefix + ">"}},
			}},
		}
	}
	ns, conn := brokerServerWith(t, users, nats.UserInfo("fleet", "fleet"))
	holder := openStore(t)
	seedMemory(t, holder)
	if _, err := syncerOn(t, holder, conn).PublishOwn(t.Context(), seat.Handle); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// AN ERROR HANDLER OF ITS OWN, as every engine connection has one: left to
	// the client's default, the permissions error is written to stderr.
	satellite, err := nats.Connect("", nats.InProcessServer(ns), nats.UserInfo("satellite", "satellite"),
		nats.ErrorHandler(func(*nats.Conn, *nats.Subscription, error) {}))
	if err != nil {
		t.Fatalf("connect as the satellite: %v", err)
	}
	t.Cleanup(satellite.Close)
	fresh := openStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	carried, err := syncerOn(t, fresh, satellite).Hydrate(ctx, seat.Handle)
	if !errors.Is(err, nats.ErrPermissionViolation) {
		t.Fatalf("a hydration whose barrier the broker would not let it publish answered (%d, %v), "+
			"want the seat refused naming the broker's permissions violation", carried, err)
	}
	if !strings.Contains(err.Error(), barrierSubject(seat.AgentID)) {
		t.Errorf("the refusal does not name the barrier's subject: %v", err)
	}
	if held := storeHolds(t, fresh); carried != 0 || held != 0 {
		t.Errorf("the refused hydration reported %d rows and left %d in the store, want none: it "+
			"reads no row before its barrier", carried, held)
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

	for range 4 {
		// A fresh syncer, with no watermark, is a restarted node: the case that
		// would grow the stream if compaction were not doing its job.
		if _, err := syncerOn(t, db, conn).PublishOwn(ctx, seat.Handle); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	client, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	stream, err := client.Stream(ctx, topics.MemoryStream)
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
	if _, err := syncer.PublishOwn(ctx, seat.Handle); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	again, err := syncer.PublishOwn(ctx, seat.Handle)
	if err != nil {
		t.Fatalf("second publish: %v", err)
	}
	whole := 0
	for _, spec := range tables {
		if spec.wholeEachCycle {
			whole++
		}
	}
	if again.Rows != whole {
		t.Fatalf("a second publish carried %d rows, want only the %d "+
			"whole-each-cycle tables", again.Rows, whole)
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
	// The peer's syncer resolves its own agent id.
	syncerOn(t, db, conn).PublishOwn(ctx, seat.Handle)
	peer := heldFromEmpty(newSyncer(db, jsOn(t, conn), func(string) string { return "other-agent-id" }),
		"ceo")
	if _, err := peer.PublishOwn(ctx, "ceo"); err != nil {
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

// A broker that cannot answer must FAIL the hydration, not report an empty
// memory.
//
// "Nothing recorded" and "could not ask" are different facts: hydration runs
// inside seat acquisition, which gates on its error alone, so a swallowed
// error admits the seat with an empty memory and reports success.
//
// This covers a broker gone before the hydration asks anything; a replay
// that cannot open, which a closed connection never reaches, has its own
// case below.
func TestAnUnreachableBrokerFailsTheHydrationRatherThanReportingNoMemory(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	db := openStore(t)
	syncer := syncerOn(t, db, conn)

	// Seed real memory, so a wrong answer here cannot be excused as the
	// seat genuinely having none.
	seedMemory(t, db)
	if _, err := syncer.PublishOwn(context.Background(), seat.Handle); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Now take the broker away.
	conn.Close()

	carried, err := syncer.Hydrate(context.Background(), seat.Handle)
	if err == nil {
		t.Fatalf("hydrating through a closed connection reported success with "+
			"%d rows — the seat would take work having silently forgotten "+
			"everything it knows", carried)
	}
	if carried != 0 {
		t.Errorf("a failed hydration reports %d rows carried", carried)
	}
}

// openFailsJS is a JetStream whose consumers the broker will not make. It
// embeds the real one, so it overrides only the call under test and stays
// correct as the interface grows.
type openFailsJS struct {
	jetstream.JetStream
}

func (openFailsJS) CreateConsumer(context.Context, string, jetstream.ConsumerConfig) (jetstream.Consumer, error) {
	return nil, errors.New("the broker did not answer")
}

// The broker stops answering after the hydration has read the changelog's
// identity and published its barrier, before a table's replay opens (the
// request whose reply carries how much there is to carry).
//
// The window the case above cannot reach. Read as an empty table, it returns
// SUCCESS with an empty memory, admitting a seat that forgot everything: the
// seat host gates on this error alone.
//
// Mutation: read a replay that could not open as an empty table, and this
// goes red.
func TestABrokerThatCannotOpenAReplayFailsTheHydration(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	ctx := context.Background()

	db := openStore(t)
	seedMemory(t, db)
	if _, err := syncerOn(t, db, conn).PublishOwn(ctx, seat.Handle); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// A new node, with a JetStream that makes no consumer.
	newOwner := openStore(t)
	syncer := syncerOn(t, newOwner, conn)
	syncer.js = openFailsJS{syncer.js}

	carried, err := syncer.Hydrate(ctx, seat.Handle)
	if err == nil {
		t.Fatalf("a broker that could not open a replay reported success with %d rows", carried)
	}
	if carried != 0 {
		t.Errorf("a failed hydration reports %d rows carried", carried)
	}
	if got := countRows(t, newOwner, "agent_diary"); got != 0 {
		t.Errorf("the failed hydration left %d diary rows behind", got)
	}
}

// A RECREATED CHANGELOG IS DETECTED, and the node REFUSES the seat rather
// than hydrating it empty.
//
// A stream deleted and remade restarts at sequence 1 holding nothing, which
// every check in Hydrate reads as a seat with nothing to carry: the seat is
// admitted having silently lost its diary and episodes. Only the stream's
// creation instant tells the two apart: a recreated stream's sequences are
// plausible, and an empty stream and an emptied one count the same.
func TestMemoryChangelogRecreationIsDetected(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	ctx := context.Background()

	// A node that has seen the stream once and hydrated from it.
	owner := openStore(t)
	seedMemory(t, owner)
	if _, err := syncerOn(t, owner, conn).PublishOwn(ctx, seat.Handle); err != nil {
		t.Fatalf("publish: %v", err)
	}
	node := openStore(t)
	if _, err := syncerOn(t, node, conn).Hydrate(ctx, seat.Handle); err != nil {
		t.Fatalf("the first hydration failed: %v", err)
	}

	// THE STREAM IS LOST AND THE QUEUE PROVISIONS IT AGAIN: same name,
	// subjects and settings, empty, stamped anew by its create
	// ([statelog.CreatedAtKey]). Stamps compare at a microsecond
	// ([statelog.IdentityOf]), and this one is minted after a publish and a
	// hydration the first create preceded, so the two differ with no clock wait.
	client := jsOn(t, conn)
	lost, err := client.Stream(ctx, topics.MemoryStream)
	if err != nil {
		t.Fatalf("read the changelog: %v", err)
	}
	again := lost.CachedInfo().Config
	again.Metadata = statelog.StampCreatedAt(again.Metadata, time.Now())
	if err := client.DeleteStream(ctx, topics.MemoryStream); err != nil {
		t.Fatalf("delete the changelog: %v", err)
	}
	if _, err := client.CreateStream(ctx, again); err != nil {
		t.Fatalf("provision the changelog again: %v", err)
	}

	carried, err := syncerOn(t, node, conn).Hydrate(ctx, seat.Handle)
	if !errors.Is(err, statelog.ErrStreamRecreated) {
		t.Fatalf("hydrating from a recreated changelog carried %d rows and "+
			"answered %v — a seat admitted from an empty stream reports "+
			"success and has forgotten everything", carried, err)
	}
	if carried != 0 {
		t.Errorf("%d rows were carried from a stream that holds none", carried)
	}
}

// A RESTORED CHANGELOG IS THE ONE THIS NODE SAW.
//
// A snapshot restore remakes the changelog from its snapshot's configuration,
// every row back, and the broker dates the stream to the restore: read off
// the broker, a restored node would refuse every seat as recreated. The
// changelog carries its create's stamp in its configuration, which the
// restore hands back.
func TestARestoredChangelogIsTheOneThisNodeSaw(t *testing.T) {
	t.Parallel()
	conn := broker(t) // the changelog stamped by its create, as the queue makes it
	ctx := context.Background()
	client := jsOn(t, conn)

	owner := openStore(t)
	seedMemory(t, owner)
	if _, err := syncerOn(t, owner, conn).PublishOwn(ctx, seat.Handle); err != nil {
		t.Fatalf("publish: %v", err)
	}
	node := openStore(t)
	if _, err := syncerOn(t, node, conn).Hydrate(ctx, seat.Handle); err != nil {
		t.Fatalf("the first hydration failed: %v", err)
	}
	original, err := client.Stream(ctx, topics.MemoryStream)
	if err != nil {
		t.Fatalf("read the changelog: %v", err)
	}

	// RESTORED: the delete and the create are the restore itself, from the
	// stream's own configuration, every row back. The account holds the
	// engine's other streams ([broker]), so the delete leaves no empty stream
	// directory for the broker's cleanup to take from under the create.
	if err := client.DeleteStream(ctx, topics.MemoryStream); err != nil {
		t.Fatalf("delete the changelog: %v", err)
	}
	if _, err := client.CreateStream(ctx, original.CachedInfo().Config); err != nil {
		t.Fatalf("make the changelog again from its configuration: %v", err)
	}
	if _, err := syncerOn(t, owner, conn).PublishOwn(ctx, seat.Handle); err != nil {
		t.Fatalf("put the rows back: %v", err)
	}
	restored, err := client.Stream(ctx, topics.MemoryStream)
	if err != nil {
		t.Fatalf("read the restored changelog: %v", err)
	}
	if statelog.IdentityOf(original.CachedInfo().Created, restored.CachedInfo().Created, true) !=
		statelog.StreamRecreated {
		t.Fatalf("the broker dated the restored changelog %v and the original %v — this "+
			"case proves nothing unless the broker's own instant moved",
			restored.CachedInfo().Created, original.CachedInfo().Created)
	}

	carried, err := syncerOn(t, node, conn).Hydrate(ctx, seat.Handle)
	if err != nil {
		t.Fatalf("hydrating from the restored changelog was refused: %v — every seat "+
			"of a node restored from a backup would be", err)
	}
	if carried != len(tables) {
		t.Fatalf("hydrated %d rows from the restored changelog, want %d", carried, len(tables))
	}
}

// A FIRST BOOT IS NOT A RECREATION. A fresh node, a fresh company and the
// first boot after this guard shipped all reach a stream they have no record
// of, and refusing there would make every first boot an outage.
func TestAStreamThisNodeHasNeverSeenIsNotARecreation(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	ctx := context.Background()

	owner := openStore(t)
	seedMemory(t, owner)
	if _, err := syncerOn(t, owner, conn).PublishOwn(ctx, seat.Handle); err != nil {
		t.Fatalf("publish: %v", err)
	}
	fresh := openStore(t)
	carried, err := syncerOn(t, fresh, conn).Hydrate(ctx, seat.Handle)
	if err != nil {
		t.Fatalf("a node seeing the changelog for the first time was refused: %v", err)
	}
	if carried != len(tables) {
		t.Fatalf("carried %d rows, want %d", carried, len(tables))
	}

	// AND THE SECOND HYDRATION IS THE SAME STREAM: a node that never recorded
	// what it saw would treat every boot as a first sight, and the guard would
	// never fire.
	if _, err := syncerOn(t, fresh, conn).Hydrate(ctx, seat.Handle); err != nil {
		t.Fatalf("the second hydration was refused: %v", err)
	}
}

// THE COMPARISON ITSELF, over the four answers it can give. A pure function
// over two instants, so every consumer reaches the same verdict and the one
// answer that is neither "same" nor "recreated" is reachable in a test.
func TestIdentityTellsTheFourCasesApart(t *testing.T) {
	t.Parallel()
	early := time.Date(2031, 4, 2, 3, 0, 0, 0, time.UTC)
	later := early.Add(time.Hour)
	for name, tc := range map[string]struct {
		recorded, current time.Time
		known             bool
		want              statelog.StreamState
	}{
		"the stream this node last saw":     {early, early, true, statelog.StreamSame},
		"a stream it has no record of":      {time.Time{}, early, false, statelog.StreamFirstSight},
		"a record with a zero instant":      {time.Time{}, early, true, statelog.StreamFirstSight},
		"a different stream, same name":     {early, later, true, statelog.StreamRecreated},
		"an instant nobody could establish": {early, time.Time{}, true, statelog.StreamUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := statelog.IdentityOf(tc.recorded, tc.current, tc.known); got != tc.want {
				t.Fatalf("IdentityOf = %s, want %s", got, tc.want)
			}
		})
	}
}

// A SEAT WHOSE ID CHANGED HYDRATES NOTHING: the changelog's subjects are keyed
// on the agent id, so a seat under another id — a changed handle, which is a
// new seat (ADR-0013) — finds none of the old one's memory.
func TestASeatWhoseIDMovedFindsNoneOfItsMemory(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	ctx := context.Background()

	oldOwner := openStore(t)
	seedMemory(t, oldOwner)
	if _, err := syncerOn(t, oldOwner, conn).PublishOwn(ctx, seat.Handle); err != nil {
		t.Fatalf("publish: %v", err)
	}

	newOwner := openStore(t)
	moved := newSyncer(newOwner, jsOn(t, conn), func(string) string {
		return "11111111-2222-4333-8444-555555555555"
	})
	carried, err := moved.Hydrate(ctx, seat.Handle)
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	if carried != 0 {
		t.Fatalf("a seat under a different id hydrated %d rows; the subjects "+
			"are not keyed on the id at all", carried)
	}
}

// A SEAT WITH NO ID HAS NO ADDRESS, so neither half touches the changelog for
// it: published, its rows would go to `crewlet.memory..<table>.<hash>`, an
// empty seat token the broker refuses, failing every flush with a warning
// that reads as a broker fault.
//
// The control is the same seat with its id: its rows publish, so the empty
// answer is what kept them off the stream.
func TestASeatWithNoIDCarriesNothing(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	ctx := context.Background()
	db := openStore(t)
	seedMemory(t, db)

	nobody := newSyncer(db, jsOn(t, conn), func(string) string { return "" })
	if published, err := nobody.PublishOwn(ctx, seat.Handle); err != nil || published.Rows != 0 {
		t.Fatalf("a seat with no id published %d rows (%v), want none and no error",
			published.Rows, err)
	}
	if carried, err := nobody.Hydrate(ctx, seat.Handle); err != nil || carried != 0 {
		t.Fatalf("a seat with no id hydrated %d rows (%v), want none and no error",
			carried, err)
	}

	if published, err := syncerOn(t, db, conn).PublishOwn(ctx, seat.Handle); err != nil ||
		published.Rows != len(tables) {
		t.Fatalf("the control published %d rows (%v), want one per table (%d)",
			published.Rows, err, len(tables))
	}
}
