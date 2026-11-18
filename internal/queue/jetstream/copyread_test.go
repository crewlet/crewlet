package jetstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsapi/jsapitest"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream/linktest"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// copiedStream is the stream the copy-read cases read: COPYREAD, on a member's
// own client, holding what each case publishes to it.
const copiedStream = "COPYREAD"

// copyReadQueue is a member's own queue with [copiedStream] made on it, its
// first message on copyread.a.
func copyReadQueue(t *testing.T) *Queue {
	t.Helper()
	q := openForTest(t, Config{})
	if _, err := q.js.CreateStream(t.Context(), jetstream.StreamConfig{
		Name: copiedStream, Subjects: []string{"copyread.>"},
	}); err != nil {
		t.Fatalf("create the stream: %v", err)
	}
	copyPublish(t, q, "copyread.a", 1)
	return q
}

// copyPublish stores one message on subject and fails the case unless the
// stream gave it seq.
func copyPublish(t *testing.T, q *Queue, subject string, seq uint64) {
	t.Helper()
	ack, err := q.js.Publish(t.Context(), subject, []byte(subject))
	if err != nil {
		t.Fatalf("publish on %s: %v", subject, err)
	}
	if ack.Sequence != seq {
		t.Fatalf("the message on %s was stored at %d, want %d", subject, ack.Sequence, seq)
	}
}

// A READ KEEPS ONLY A CONSUMER THE STREAM'S LEADER RUNS.
//
// Only the leader's copy answers what the leader's read answers: a follower's
// may call an existing entry gone or a dropped one held. A consumer's create
// answer names its server, or none on an unclustered server (which holds the
// stream's one copy), so a read keeps it only where that is the leader's.
//
// Mutation: keep a consumer whatever server it names, and the follower's row
// is kept.
func TestAReadKeepsOnlyAConsumerTheStreamsLeaderRuns(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		info *jetstream.ConsumerInfo
		want bool
	}{
		{"on the leader", &jetstream.ConsumerInfo{Cluster: &jetstream.ClusterInfo{Leader: "member-b"}}, true},
		{"on a follower", &jetstream.ConsumerInfo{Cluster: &jetstream.ClusterInfo{Leader: "member-a"}}, false},
		{"on a server that is not clustered", &jetstream.ConsumerInfo{}, true},
		{"described by nothing", nil, false},
	} {
		if got := hosted(c.info, "member-b"); got != c.want {
			t.Errorf("a consumer %s, the stream led by member-b: kept %t, want %t", c.name, got, c.want)
		}
	}
}

// A READ MAKES CONSUMERS UNTIL ONE IS ON THE LEADER, FOR AS MANY AS LEAVE ONE
// MISS IN A MILLION IN A HEALTHY CLUSTER.
//
// A healthy cluster places each consumer uniformly among the stream's replicas
// (every live one counts as caught up: package doc, "A message too large for a
// leader's reply is read raw"): fewer placements miss the leader more than
// once in a million reads, and more spend consumers on a broker that has
// already failed. A one-replica stream needs one.
//
// Mutation: make any other number of consumers, and its row goes red.
func TestAReadMakesConsumersUntilOneIsOnTheLeaderForAsManyAsLeaveOneMissInAMillion(t *testing.T) {
	t.Parallel()
	for replicas, want := range map[int]int{0: 1, 1: 1, 3: 35, 5: 62} {
		got := placements(replicas)
		if got != want {
			t.Errorf("a stream keeping %d replicas is given %d placements, want %d", replicas, got, want)
		}
		if replicas > 1 {
			miss := float64(replicas-1) / float64(replicas)
			if odds := math.Pow(miss, float64(got)); odds > unplacedOdds {
				t.Errorf("%d placements among %d replicas all miss the leader %g of the time, "+
					"past one in a million", got, replicas, odds)
			}
			if odds := math.Pow(miss, float64(got-1)); odds <= unplacedOdds {
				t.Errorf("%d placements among %d replicas already miss the leader only %g of the "+
					"time: the last is spent for nothing", got-1, replicas, odds)
			}
		}
	}
}

// A CONSUMER COUNTS WHAT IT HAS TO DELIVER AS IT IS MADE.
//
// A copy read answers "nothing there" from the count a consumer on the
// leader's copy states as it is made, with no pull (nats-server v2.15.2,
// consumer.go's streamNumPending, read into the create answer's NumPending).
// This holds the broker to it: past the stream's end, nothing; at a removed
// message, those past it; on a subject holding nothing, nothing; a subject's
// newest, one.
//
// Mutation: the broker's — a server that counted otherwise fails a row here
// before any copy read could misread it.
func TestAConsumerCountsWhatItHasToDeliverAsItIsMade(t *testing.T) {
	t.Parallel()
	q := copyReadQueue(t)
	copyPublish(t, q, "copyread.a", 2)
	copyPublish(t, q, "copyread.b", 3)
	copyPublish(t, q, "copyread.a", 4)
	stream, err := q.js.Stream(t.Context(), copiedStream)
	if err != nil {
		t.Fatal(err)
	}
	for _, seq := range []uint64{2, 4} {
		if err := stream.DeleteMsg(t.Context(), seq); err != nil {
			t.Fatalf("delete %d: %v", seq, err)
		}
	}
	counted := func(cfg jetstream.ConsumerConfig) uint64 {
		t.Helper()
		cons, drop, err := q.throwaway(t.Context(), copiedStream, cfg)
		if err != nil {
			t.Fatalf("make the consumer: %v", err)
		}
		defer drop()
		return cons.CachedInfo().NumPending
	}
	at := func(seq uint64) jetstream.ConsumerConfig {
		return jetstream.ConsumerConfig{DeliverPolicy: jetstream.DeliverByStartSequencePolicy,
			OptStartSeq: seq, HeadersOnly: true}
	}
	for _, c := range []struct {
		name string
		cfg  jetstream.ConsumerConfig
		want uint64
	}{
		{"begun past the stream's end", at(5), 0},
		{"begun at a removed message with one past it", at(2), 1},
		{"begun at a removed message at the stream's end", at(4), 0},
		{"filtered to a subject holding nothing", jetstream.ConsumerConfig{
			DeliverPolicy: jetstream.DeliverByStartSequencePolicy, OptStartSeq: 1,
			FilterSubject: "copyread.none"}, 0},
		{"made for the newest on a subject", jetstream.ConsumerConfig{
			DeliverPolicy: jetstream.DeliverLastPolicy, FilterSubject: "copyread.a",
			HeadersOnly: true}, 1},
		{"made for the newest on a subject holding nothing", jetstream.ConsumerConfig{
			DeliverPolicy: jetstream.DeliverLastPolicy, FilterSubject: "copyread.none",
			HeadersOnly: true}, 0},
	} {
		if got := counted(c.cfg); got != c.want {
			t.Errorf("a consumer %s counts %d to deliver, want %d", c.name, got, c.want)
		}
	}
}

// A COPY READ ANSWERS WHAT THE LEADER HOLDS, AND NOTHING ELSE.
//
// Read on a member, whose one server leads the stream: what a sequence holds,
// its subject and its size from the headers alone; the message itself, raw;
// and a subject's newest — and, where the leader holds nothing, nothing: a
// sequence never stored, one removed with messages past it, one removed at
// the stream's end, a subject never written.
//
// Mutation: take a delivery past the sequence for the message there, and a
// removed message reads as held; or answer a subject's newest as found
// whatever was delivered, and a subject never written reads as present.
func TestACopyReadAnswersWhatTheLeaderHoldsAndNothingElse(t *testing.T) {
	t.Parallel()
	q := copyReadQueue(t)
	copyPublish(t, q, "copyread.b", 2)
	copyPublish(t, q, "copyread.c", 3)
	copyPublish(t, q, "copyread.b", 4)
	leader, err := jsapi.NewLeader(q.js, copiedStream)
	if err != nil {
		t.Fatal(err)
	}
	c := q.copiesOf(copiedStream, leader)
	ctx := t.Context()

	at, err := c.at(ctx, 3)
	if err != nil || !at.held || at.subject != "copyread.c" || at.bytes < len("copyread.c") {
		t.Fatalf("what 3 holds = %+v, %v, want the message on copyread.c, its size counted", at, err)
	}
	msg, held, err := c.message(ctx, 3, at)
	if err != nil || !held || msg.subject != "copyread.c" || string(msg.data) != "copyread.c" ||
		msg.time.IsZero() {
		t.Fatalf("the message at 3 = %+v, %t, %v, want copyread.c's own bytes and its instant",
			msg, held, err)
	}
	if seq, found, err := c.last(ctx, "copyread.b"); err != nil || !found || seq != 4 {
		t.Fatalf("the newest on copyread.b = (%d, %t, %v), want 4", seq, found, err)
	}

	stream, err := q.js.Stream(ctx, copiedStream)
	if err != nil {
		t.Fatal(err)
	}
	for _, seq := range []uint64{2, 4} {
		if err := stream.DeleteMsg(ctx, seq); err != nil {
			t.Fatalf("delete %d: %v", seq, err)
		}
	}
	for name, seq := range map[string]uint64{"never stored": 9, "removed, with a message past it": 2,
		"removed at the stream's end": 4} {
		if at, err := c.at(ctx, seq); err != nil || at.held {
			t.Errorf("what %d holds, %s = %+v, %v, want nothing", seq, name, at, err)
		}
	}
	if seq, found, err := c.last(ctx, "copyread.none"); err != nil || found {
		t.Errorf("the newest on a subject never written = (%d, %t, %v), want none", seq, found, err)
	}
	if seq, found, err := c.last(ctx, "copyread.b"); err != nil || found {
		t.Errorf("the newest on copyread.b, its every message removed = (%d, %t, %v), want none",
			seq, found, err)
	}
	// A NEWER MESSAGE ON THE SUBJECT is the subject's newest, and never the
	// message a removed sequence held.
	copyPublish(t, q, "copyread.b", 5)
	if _, held, err := c.message(ctx, 2, copyAt{held: true, subject: "copyread.b", bytes: 64}); err != nil || held {
		t.Errorf("the message at 2, removed since its probe = held %t, %v, want nothing: the "+
			"next on its subject, at 5, is not it", held, err)
	}
	if seq, found, err := c.last(ctx, "copyread.b"); err != nil || !found || seq != 5 {
		t.Errorf("the newest on copyread.b = (%d, %t, %v), want 5", seq, found, err)
	}
}

// A CONNECTION NO LEAF'S LINK STANDS ON READS FROM THE LEADER, AND MAKES NO
// CONSUMER.
//
// Only a leaf's server holds its link to a max_payload (nats-server v2.15.2,
// leafnode.go), so on a member's own connection and an external broker's
// client a dead letter's frame and drop check and a state log's record and
// newest, at the transport's ceiling, are each one leader request: never a
// consumer read's stream info and metadata-leader creates and deletes.
//
// Mutation: decide copy reads by the connection's pace, not the leaf's link,
// and the external client makes consumers; or read from a copy on a member's
// own connection, and the member does.
func TestAConnectionNoLeafsLinkStandsOnReadsFromTheLeaderAndMakesNoConsumer(t *testing.T) {
	t.Parallel()
	_, external := accountQueue(t, probeDomain())
	for _, c := range []struct {
		name string
		q    *Queue
	}{
		{"a member's own connection", openForTest(t, Config{})},
		{"a client of a broker somebody else runs", external},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			readWholeFromTheLeader(t, c.q)
		})
	}
}

// readWholeFromTheLeader keeps a dead letter and appends a record at the
// transport's ceiling on q, and holds q's reads of them — a frame, a drop, a
// subject's newest and a record by its position — to their exact answers with
// no consumer made.
func readWholeFromTheLeader(t *testing.T, q *Queue) {
	t.Helper()
	ctx := t.Context()
	subject := topics.DeadLetter("whole.letters", "grp")
	header := []byte(`{"v":1}`)
	body := incompressible(t, queue.MaxPayloadBytes-queue.LetterFraming-len(header))
	if _, err := q.KeepDeadLetter(ctx, subject, "at the ceiling", header, body); err != nil {
		t.Fatalf("KeepDeadLetter: %v", err)
	}
	kept, err := q.DeadLettersOn(ctx, subject)
	if err != nil || len(kept) != 1 {
		t.Fatalf("the subject lists %d entries (%v), want one", len(kept), err)
	}
	spec := DomainStream{Name: "CREWLET_WHOLE_LOG", Subjects: []string{"crewlet.whole.log.>"},
		MaxBytes: 4 * queue.MaxPayloadBytes, MaxMessageBytes: queue.MaxPayloadBytes,
		Duplicates: 2 * time.Minute}
	if err := q.EnsureDomainStream(ctx, spec); err != nil {
		t.Fatalf("EnsureDomainStream: %v", err)
	}
	log, err := q.DomainLog(ctx, spec.Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	const recordSubject = "crewlet.whole.log.object.big"
	record := incompressible(t, statelog.MaxTransportRecordBytes)
	none := uint64(0)
	seq, _, err := log.Append(ctx, recordSubject, "whole-create", &none, record)
	if err != nil {
		t.Fatalf("append: %v", err)
	}

	before := q.ConsumerProposals()
	if frame, held, err := q.DeadLetterFrame(ctx, subject, kept[0].Seq); err != nil || !held ||
		!bytes.Equal(frame, body) {
		t.Fatalf("DeadLetterFrame = %d bytes, %t, %v, want the body kept", len(frame), held, err)
	}
	if err := q.DropDeadLetter(ctx, subject, kept[0].Seq); err != nil {
		t.Fatalf("DropDeadLetter: %v", err)
	}
	if last, found, err := log.LastSeq(ctx, recordSubject); err != nil || !found || last != seq {
		t.Fatalf("LastSeq = (%d, %t, %v), want %d", last, found, err, seq)
	}
	if _, payload, _, ok, err := log.At(ctx, seq); err != nil || !ok || !bytes.Equal(payload, record) {
		t.Fatalf("At = %d bytes, %t, %v, want the record", len(payload), ok, err)
	}
	if made := q.ConsumerProposals() - before; made != 0 {
		t.Errorf("the reads made %d consumers, want none: the leader answers them whole", made)
	}
}

// ACROSS A LEAF'S LINK, A DEAD LETTER WHOSE LEADER'S REPLY CROSSES IT IS THE
// LEADER'S READ, AND ONE WHOSE REPLY DOES NOT CROSSES RAW.
//
// The leader's read is one request, a consumer read several, so a frame takes
// a consumer only where the leader's reply would pass the transport, on which
// a leaf's server closes its whole link; a reply that fits is given its
// arrival. At the floor's pace a small entry and a 2 MiB one (over a minute to
// cross) reach the leader's held read, and one at the keep's ceiling never does.
//
// Mutation: read every frame from a copy, or route by crossing time, and the
// small or the 2 MiB one (respectively) misses the leader; read all from it,
// and the ceiling's reaches it.
func TestAFrameAcrossALeafsLinkIsTheLeadersWhereItsReplyCrossesAndRawWhereItDoesNot(t *testing.T) {
	t.Parallel()
	ns, q := leafLinkedQueue(t, probeDomain())
	ctx := t.Context()
	subject := topics.DeadLetter("linked.letters", "grp")
	header := []byte(`{"v":1}`)
	small := []byte("a small frame")
	middle := incompressible(t, 2<<20)
	large := incompressible(t, queue.MaxPayloadBytes-queue.LetterFraming-len(header))
	for i, body := range [][]byte{small, middle, large} {
		if _, err := q.KeepDeadLetter(ctx, subject, string(rune('a'+i)), header, body); err != nil {
			t.Fatalf("KeepDeadLetter: %v", err)
		}
	}
	kept, err := q.DeadLettersOn(ctx, subject)
	if err != nil || len(kept) != 3 {
		t.Fatalf("the subject lists %d entries (%v), want three", len(kept), err)
	}
	stream, err := q.streamFor(ctx, subject)
	if err != nil {
		t.Fatal(err)
	}

	type answer struct {
		frame []byte
		held  bool
		err   error
	}
	for i, body := range [][]byte{small, middle} {
		entry := kept[i].Seq
		held := jsapitest.Hold(t, ns, jsapitest.MsgGet(stream), nats.Token(leaderReadsToken))
		done := make(chan answer, 1)
		go func() {
			frame, ok, err := q.DeadLetterFrame(ctx, subject, entry)
			done <- answer{frame, ok, err}
		}()
		// LIVENESS: the entry's read reaches the leader's read once its copy
		// has said what the sequence holds; this ends a wait for one that
		// never does.
		wait, cancel := context.WithTimeout(ctx, 30*time.Second)
		read, err := held.Next(wait)
		cancel()
		if err != nil {
			t.Fatalf("the frame of %d bytes never reached the leader's read: %v", len(body), err)
		}
		held.Release()
		if err := read.Answer(); err != nil {
			t.Fatalf("answer the held read: %v", err)
		}
		if got := <-done; got.err != nil || !got.held || !bytes.Equal(got.frame, body) {
			t.Fatalf("the frame of %d bytes = %d bytes, %t, %v, want its body", len(body), len(got.frame),
				got.held, got.err)
		}
	}

	again := jsapitest.Hold(t, ns, jsapitest.MsgGet(stream), nats.Token(leaderReadsToken))
	for i, body := range [][]byte{small, middle, large} {
		if i < 2 {
			// THE TWO THE LEADER ANSWERED, dropped: a drop asks the leader
			// only through the copies across a leaf's link.
			if err := q.DropDeadLetter(ctx, subject, kept[i].Seq); err != nil {
				t.Fatalf("DropDeadLetter: %v", err)
			}
			continue
		}
		entry := kept[i].Seq
		frame, ok, err := q.DeadLetterFrame(ctx, subject, entry)
		if err != nil || !ok || !bytes.Equal(frame, body) {
			t.Fatalf("the frame of %d bytes = %d bytes, %t, %v, want its body", len(body),
				len(frame), ok, err)
		}
		if err := q.DropDeadLetter(ctx, subject, entry); err != nil {
			t.Fatalf("DropDeadLetter: %v", err)
		}
	}
	if n, err := again.Fence(ctx, q.nc); err != nil || n != 0 {
		t.Errorf("reading the entry at the ceiling and dropping all three across a leaf's link "+
			"asked the leader %d times (%v), want none", n, err)
	}
}

// leafLinkedQueue is [accountQueue]'s queue, whose reads [jsapitest] can hold,
// taken to cross a leaf's link ([link.leaf], what copy reads decide by). A
// real leaf speaks the fleet's domain, whose API jsapitest cannot hold; its
// reads are held by largereads_test.go.
func leafLinkedQueue(t *testing.T, spec DomainStream) (*server.Server, *Queue) {
	t.Helper()
	ns, q := accountQueue(t, spec)
	q.link.leaf = true
	return ns, q
}

// relayedLeafLinkedQueue is [leafLinkedQueue] dialled through a link the case
// can hold the broker's bytes back on ([linktest.Link.Delay]).
func relayedLeafLinkedQueue(t *testing.T) (*server.Server, *linktest.Link, *Queue) {
	t.Helper()
	ns := accountBroker(t)
	// FAST TOWARDS THE BROKER: what is held here is what comes back.
	relay := linktest.Slow(t, ns.Addr().String(), 1<<30)
	q, err := Open(t.Context(), Config{URL: "nats://" + relay.Addr(), Token: leaderReadsToken})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	q.link.leaf = true
	return ns, relay, q
}

// copiedOn makes [copiedStream] on q, its first message body on copyread.a,
// and answers a copy read of it.
func copiedOn(t *testing.T, q *Queue, body []byte) *copyRead {
	t.Helper()
	if _, err := q.js.CreateStream(t.Context(), jetstream.StreamConfig{
		Name: copiedStream, Subjects: []string{"copyread.>"},
	}); err != nil {
		t.Fatalf("create the stream: %v", err)
	}
	if ack, err := q.js.Publish(t.Context(), "copyread.a", body); err != nil || ack.Sequence != 1 {
		t.Fatalf("publish the first message = %+v, %v, want it at 1", ack, err)
	}
	leader, err := jsapi.NewLeader(q.js, copiedStream)
	if err != nil {
		t.Fatal(err)
	}
	return q.copiesOf(copiedStream, leader)
}

// A COPY READ WHOSE FIRST PULL THE CLUSTER DROPS MAKES ITS CONSUMER AGAIN.
//
// A cluster routinely drops a new consumer's first pull, and a read that waits
// out its pull (minutes, raw at the floor) waits for a delivery never asked
// for. So a pull quiet for a second asks its consumer, and one holding no pull
// and having delivered nothing is made again at once. Here a probe's and a raw
// read's first pulls are dropped, on a link so slow the raw read's message
// takes hours: each answers within a minute.
//
// Mutation: wait out a quiet pull on the consumer it was asked of, rather
// than ask the consumer and make it again, and the raw read waits for hours.
func TestACopyReadWhoseFirstPullTheClusterDropsMakesItsConsumerAgain(t *testing.T) {
	t.Parallel()
	ns, q := leafLinkedQueue(t, probeDomain())
	q.link.pace = time.Hour
	body := incompressible(t, 64<<10)
	c := copiedOn(t, q, body)
	at, err := c.at(t.Context(), 1)
	if err != nil || !at.held || at.subject != "copyread.a" {
		t.Fatalf("what 1 holds = %+v, %v, want the message on copyread.a", at, err)
	}
	for _, read := range []struct {
		name string
		read func(context.Context) error
	}{
		{"a probe of what a sequence holds", func(ctx context.Context) error {
			found, err := c.at(ctx, 1)
			if err == nil && found != at {
				err = fmt.Errorf("it found %+v, want %+v", found, at)
			}
			return err
		}},
		{"a raw read of a message", func(ctx context.Context) error {
			msg, held, err := c.message(ctx, 1, at)
			if err == nil && (!held || !bytes.Equal(msg.data, body)) {
				err = fmt.Errorf("it read %d bytes, held %t, want the %d-byte body", len(msg.data),
					held, len(body))
			}
			return err
		}},
	} {
		dropped := jsapitest.DropFirst(t, ns, jsapitest.ConsumerNext(copiedStream), 1,
			nats.Token(leaderReadsToken))
		done := make(chan error, 1)
		go func() { done <- read.read(t.Context()) }()
		liveness := time.NewTimer(time.Minute) //fixedwait:liveness
		select {
		case err := <-done:
			liveness.Stop()
			if err != nil {
				t.Errorf("%s whose first pull was dropped: %v", read.name, err)
			}
		case <-liveness.C:
			t.Fatalf("%s whose first pull was dropped answered nothing in a minute: it waited "+
				"on a consumer that never got the pull", read.name)
		}
		if n := dropped.Count(); n != 1 {
			t.Errorf("%s: %d pulls were dropped, want the first", read.name, n)
		}
	}
}

// A COPY READ ENDS WITH ITS CALLER.
//
// Every request a copy read makes runs under its caller's context, so a caller
// that goes (a node's shutdown waits for the replays it cancelled) ends the
// read at once, not after its pull's own wait, minutes raw at the floor. Here
// every pull is held on a link so slow the message takes hours, and the
// caller is cancelled once the first pull is held: the read ends within a
// minute.
//
// Mutation: pull under a context of the read's own rather than its caller's,
// and the read outlives its caller by hours.
func TestACopyReadEndsWithItsCaller(t *testing.T) {
	t.Parallel()
	ns, q := leafLinkedQueue(t, probeDomain())
	q.link.pace = time.Hour
	c := copiedOn(t, q, incompressible(t, 64<<10))
	at, err := c.at(t.Context(), 1)
	if err != nil || !at.held {
		t.Fatalf("what 1 holds = %+v, %v, want the message", at, err)
	}
	held := jsapitest.Hold(t, ns, jsapitest.ConsumerNext(copiedStream), nats.Token(leaderReadsToken))
	caller, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, _, err := c.message(caller, 1, at)
		done <- err
	}()
	wait, stop := context.WithTimeout(t.Context(), 30*time.Second)
	defer stop()
	if _, err := held.Next(wait); err != nil {
		t.Fatalf("the read's pull never reached the broker: %v", err)
	}
	cancel()
	liveness := time.NewTimer(time.Minute) //fixedwait:liveness
	defer liveness.Stop()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the read whose caller went ended with %v, want its caller's cancellation", err)
		}
	case <-liveness.C:
		t.Fatal("the read outlived its caller by a minute")
	}
}

// A PROBE TAKES A DELIVERY THAT REACHES IT LATE.
//
// A probe's few hundred bytes of headers can wait on a slow link behind what
// the broker sent first (a frame a replay reads, an applier's batch); an inbox
// closed when the client stopped waiting would drop them and fail the read on
// the consumer's count. The read's inbox is held for the whole read, so a
// delivery is taken however late: here the broker's bytes are held back three
// seconds once the probe's pull reaches it, and the probe answers.
//
// Mutation: fail a quiet pull whose consumer delivered what has not arrived,
// rather than wait for it on the same inbox, and the probe fails.
func TestAProbeTakesADeliveryThatReachesItLate(t *testing.T) {
	t.Parallel()
	ns, relay, q := relayedLeafLinkedQueue(t)
	c := copiedOn(t, q, []byte("a probed message"))
	held := jsapitest.Hold(t, ns, jsapitest.ConsumerNext(copiedStream), nats.Token(leaderReadsToken))
	type answer struct {
		at  copyAt
		err error
	}
	done := make(chan answer, 1)
	go func() {
		at, err := c.at(t.Context(), 1)
		done <- answer{at, err}
	}()
	wait, stop := context.WithTimeout(t.Context(), 30*time.Second)
	defer stop()
	pull, err := held.Next(wait)
	if err != nil {
		t.Fatalf("the probe's pull never reached the broker: %v", err)
	}
	// PAST THE TWO SECONDS a pull of a quiet second was waited for.
	relay.Delay(3 * time.Second)
	held.Release()
	if err := pull.Answer(); err != nil {
		t.Fatalf("send the probe's pull on: %v", err)
	}
	liveness := time.NewTimer(time.Minute) //fixedwait:liveness
	defer liveness.Stop()
	select {
	case got := <-done:
		if got.err != nil || !got.at.held || got.at.subject != "copyread.a" {
			t.Errorf("the probe whose delivery reached it late = %+v, %v, want the message on "+
				"copyread.a", got.at, got.err)
		}
	case <-liveness.C:
		t.Fatal("the probe answered nothing in a minute")
	}
}

// A COPY READ ACROSS A SLOW LINK WAITS OUT ITS MESSAGE BEFORE ITS PROBE GIVES
// UP.
//
// A raw read's quiet pull asks its consumer about itself, and the answer
// crosses the link behind the message being pulled; held to the read term and
// lookup ceiling alone it arrives too late, so the probe is also given the
// message's crossing, as a leader's read is given its reply's arrival. Here
// the ceiling is 2 s, the crossing minutes, and the broker's bytes are held
// back 5 s once the pull reaches it: the read answers the message.
//
// Mutation: ask the probe at the queue's timing alone, and the read fails at
// the ceiling.
func TestACopyReadAcrossASlowLinkWaitsOutItsMessageBeforeItsProbeGivesUp(t *testing.T) {
	t.Parallel()
	ns, relay, q := relayedLeafLinkedQueue(t)
	body := incompressible(t, 64<<10)
	c := copiedOn(t, q, body)
	at, err := c.at(t.Context(), 1)
	if err != nil || !at.held || at.subject != "copyread.a" {
		t.Fatalf("what 1 holds = %+v, %v, want the message on copyread.a", at, err)
	}
	// THE CEILING SHORTER THAN THE HOLD, and the message's crossing at the
	// link's pace longer than both, set while nothing else uses the queue.
	timing := q.provisioning()
	timing.Lookup = 2 * time.Second
	q.timing = timing
	q.link.pace = time.Hour
	if crossing := c.crossing(at.bytes); crossing <= 5*time.Second+timing.Lookup {
		t.Fatalf("the message crosses in %v at the case's pace, want longer than the hold "+
			"and the ceiling", crossing)
	}
	held := jsapitest.Hold(t, ns, jsapitest.ConsumerNext(copiedStream), nats.Token(leaderReadsToken))
	type answer struct {
		msg  stored
		held bool
		err  error
	}
	done := make(chan answer, 1)
	go func() {
		msg, held, err := c.message(t.Context(), 1, at)
		done <- answer{msg, held, err}
	}()
	wait, stop := context.WithTimeout(t.Context(), 30*time.Second)
	defer stop()
	pull, err := held.Next(wait)
	if err != nil {
		t.Fatalf("the read's pull never reached the broker: %v", err)
	}
	// PAST THE QUIET SECOND AND THE CEILING after it: every answer of the
	// probe arrives after the ceiling, behind the message.
	relay.Delay(5 * time.Second)
	held.Release()
	if err := pull.Answer(); err != nil {
		t.Fatalf("send the read's pull on: %v", err)
	}
	liveness := time.NewTimer(time.Minute) //fixedwait:liveness
	defer liveness.Stop()
	select {
	case got := <-done:
		if got.err != nil || !got.held || !bytes.Equal(got.msg.data, body) {
			t.Errorf("the read whose message was still crossing = %d bytes, held %t, %v: want "+
				"the %d-byte body", len(got.msg.data), got.held, got.err, len(body))
		}
	case <-liveness.C:
		t.Fatal("the read answered nothing in a minute")
	}
}

// A PAYLOAD READ OF A MESSAGE REMOVED SINCE ITS PROBE CARRIES NO LARGER ONE.
//
// A payload read's consumer is filtered to the probed subject, so once the
// probed message is removed the next on that subject comes first, up to 8 MiB
// carried across the link to be discarded. The pull is bounded by the probed
// size, so the broker answers with the bound and the read asks the leader's
// copy again: nothing. Here a 64 KiB message is probed, removed and followed by
// a 1 MiB one: the read answers it gone, and the mebibyte never crosses.
//
// Mutation: pull the payload with no byte bound, and the mebibyte crosses.
func TestAPayloadReadOfAMessageRemovedSinceItsProbeCarriesNoLargerOne(t *testing.T) {
	t.Parallel()
	q := copyReadQueue(t)
	ctx := t.Context()
	small := incompressible(t, 64<<10)
	if ack, err := q.js.Publish(ctx, "copyread.x", small); err != nil || ack.Sequence != 2 {
		t.Fatalf("publish the probed message = %+v, %v, want it at 2", ack, err)
	}
	leader, err := jsapi.NewLeader(q.js, copiedStream)
	if err != nil {
		t.Fatal(err)
	}
	c := q.copiesOf(copiedStream, leader)
	at, err := c.at(ctx, 2)
	if err != nil || !at.held || at.subject != "copyread.x" {
		t.Fatalf("what 2 holds = %+v, %v, want the message on copyread.x", at, err)
	}
	large := incompressible(t, 1<<20)
	if ack, err := q.js.Publish(ctx, "copyread.x", large); err != nil || ack.Sequence != 3 {
		t.Fatalf("publish the larger message = %+v, %v, want it at 3", ack, err)
	}
	stream, err := q.js.Stream(ctx, copiedStream)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.DeleteMsg(ctx, 2); err != nil {
		t.Fatalf("remove the probed message: %v", err)
	}
	before := q.nc.Stats().InBytes
	if msg, held, err := c.message(ctx, 2, at); err != nil || held {
		t.Fatalf("the read of the removed message = %d bytes, held %t, %v: want it gone",
			len(msg.data), held, err)
	}
	if crossed := q.nc.Stats().InBytes - before; crossed >= uint64(len(large)) {
		t.Errorf("reading the removed message brought in %d bytes: the %d-byte message after it "+
			"crossed", crossed, len(large))
	}
}

// A PAYLOAD READ NEVER ANSWERS A MESSAGE GONE ON ITS BYTE BOUND'S WORD ALONE.
//
// The broker answers a pull whose next message is past the probed size's
// bound with the bound, which means gone only if the size was counted right.
// So the read asks the leader's copy again and answers gone only where it says
// so: here the size is counted far short, and the read refuses rather than
// call a present message gone.
//
// Mutation: take the bound's word for the message's going, and the message
// still there reads as gone.
func TestAPayloadReadNeverAnswersAMessageGoneOnItsByteBoundsWordAlone(t *testing.T) {
	t.Parallel()
	q := copyReadQueue(t)
	ctx := t.Context()
	body := incompressible(t, 64<<10)
	if ack, err := q.js.Publish(ctx, "copyread.x", body); err != nil || ack.Sequence != 2 {
		t.Fatalf("publish the message = %+v, %v, want it at 2", ack, err)
	}
	leader, err := jsapi.NewLeader(q.js, copiedStream)
	if err != nil {
		t.Fatal(err)
	}
	c := q.copiesOf(copiedStream, leader)
	miscounted := copyAt{held: true, subject: "copyread.x", bytes: 1}
	if msg, held, err := c.message(ctx, 2, miscounted); err == nil || held {
		t.Errorf("a read whose message the bound held back = %d bytes, held %t, %v: want a "+
			"refusal, the message being there", len(msg.data), held, err)
	}
}

// ACROSS A LEAF'S LINK, A RECORD WHOSE LEADER'S REPLY CROSSES IT IS THE
// LEADER'S READ, AND ONE WHOSE REPLY DOES NOT CROSSES RAW.
//
// The state log's twin of the frame's case above, on a log whose records may
// be as large as the transport: a record whose reply the leaf's server carries
// is one leader request at its reply's arrival
// (TestAConnectionsReadsAndWritesArePacedByOneDecision holds the term), and
// one whose reply does not is read raw. A small record reaches the held
// leader's read; one at the ceiling never does.
//
// Mutation: read every record raw, and the small one misses the leader; or
// every one from it, and the ceiling's reaches it.
func TestARecordAcrossALeafsLinkIsTheLeadersWhereItsReplyCrossesAndRawWhereItDoesNot(t *testing.T) {
	t.Parallel()
	ns, q := leafLinkedQueue(t, probeDomain())
	ctx := t.Context()
	spec := DomainStream{Name: "CREWLET_LINKED_LOG", Subjects: []string{"crewlet.linked.log.>"},
		MaxBytes: 4 * queue.MaxPayloadBytes, MaxMessageBytes: queue.MaxPayloadBytes,
		Duplicates: 2 * time.Minute}
	if err := q.EnsureDomainStream(ctx, spec); err != nil {
		t.Fatalf("EnsureDomainStream: %v", err)
	}
	log, err := q.DomainLog(ctx, spec.Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	if log.copies == nil {
		t.Fatal("a log of records as large as the transport is read from the leader alone " +
			"across a leaf's link")
	}
	small := []byte(`{"a":"small record"}`)
	large := incompressible(t, statelog.MaxTransportRecordBytes)
	var seqs []uint64
	for i, record := range [][]byte{small, large} {
		none := uint64(0)
		seq, _, err := log.Append(ctx, fmt.Sprintf("crewlet.linked.log.object.%d", i),
			fmt.Sprintf("linked-%d", i), &none, record)
		if err != nil {
			t.Fatalf("append record %d: %v", i, err)
		}
		seqs = append(seqs, seq)
	}

	held := jsapitest.Hold(t, ns, jsapitest.MsgGet(spec.Name), nats.Token(leaderReadsToken))
	type answer struct {
		payload []byte
		ok      bool
		err     error
	}
	done := make(chan answer, 1)
	go func() {
		_, payload, _, ok, err := log.At(ctx, seqs[0])
		done <- answer{payload, ok, err}
	}()
	// LIVENESS: the small record's read reaches the leader's read once its
	// copy has said what the position holds; this ends a wait for one that
	// never does.
	wait, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	read, err := held.Next(wait)
	if err != nil {
		t.Fatalf("the small record's read never reached the leader's read: %v", err)
	}
	held.Release()
	if err := read.Answer(); err != nil {
		t.Fatalf("answer the held read: %v", err)
	}
	if got := <-done; got.err != nil || !got.ok || !bytes.Equal(got.payload, small) {
		t.Fatalf("the small record = %q, %t, %v, want %q", got.payload, got.ok, got.err, small)
	}

	again := jsapitest.Hold(t, ns, jsapitest.MsgGet(spec.Name), nats.Token(leaderReadsToken))
	if _, payload, _, ok, err := log.At(ctx, seqs[1]); err != nil || !ok || !bytes.Equal(payload, large) {
		t.Fatalf("the record at the ceiling = %d bytes, %t, %v, want its %d bytes", len(payload),
			ok, err, len(large))
	}
	if n, err := again.Fence(ctx, q.nc); err != nil || n != 0 {
		t.Errorf("reading the record at the ceiling across a leaf's link asked the leader for it "+
			"%d times (%v), want none", n, err)
	}
}

// placingJS is a broker that places each consumer a read makes on the next of
// its servers in turn, as the consumer's create answer names it, and records
// the consumers it is asked to delete.
type placingJS struct {
	jetstream.JetStream
	servers []string

	mu      sync.Mutex
	made    int
	deleted []string
}

func (j *placingJS) CreateConsumer(_ context.Context, _ string,
	cfg jetstream.ConsumerConfig) (jetstream.Consumer, error) {

	j.mu.Lock()
	defer j.mu.Unlock()
	info := &jetstream.ConsumerInfo{Name: fmt.Sprintf("placed-%d", j.made), Config: cfg,
		Cluster: &jetstream.ClusterInfo{Leader: j.servers[j.made%len(j.servers)]}}
	j.made++
	return placedConsumer{info: info}, nil
}

func (j *placingJS) DeleteConsumer(_ context.Context, _ string, name string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.deleted = append(j.deleted, name)
	return nil
}

// placedConsumer is a consumer as [placingJS] placed it.
type placedConsumer struct {
	jetstream.Consumer
	info *jetstream.ConsumerInfo
}

func (c placedConsumer) CachedInfo() *jetstream.ConsumerInfo { return c.info }

// A READ KEEPS THE CONSUMER ITS BROKER PLACED ON THE LEADER, MAKING THE FIRST
// DRAW BACK TO BACK AND EVERY ONE AFTER A HEARTBEAT APART.
//
// A read deletes every consumer placed off the stream's leader and keeps the
// one on it: here member-c, reached on the fifth placement. The first draw, as
// many as the stream's replicas, is made back to back; the broker prefers
// replicas its metadata leader counts as caught up, and a passed-over leader
// takes a metadata heartbeat to count again, so later ones are a heartbeat
// apart. Exact, in a bubble.
//
// Mutation: keep any consumer, and the first is kept; or make every one back
// to back, or pause before the first draw's too, and the time differs.
func TestAReadKeepsTheConsumerItsBrokerPlacedOnTheLeader(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		js := &placingJS{servers: []string{"member-a", "member-b", "member-a", "member-b", "member-c"}}
		c := &copyRead{q: &Queue{js: js}, stream: "PLACED", pause: placementPause,
			leading: func(context.Context) (string, int, error) { return "member-c", 3, nil }}
		began := time.Now()
		cons, drop, err := c.onLeader(context.Background(), jetstream.ConsumerConfig{})
		took := time.Since(began)
		if err != nil {
			t.Fatalf("a read the broker placed on its leader the fifth time: %v", err)
		}
		if got := cons.CachedInfo().Cluster.Leader; got != "member-c" {
			t.Errorf("the read kept a consumer on %s, want the one on member-c, its leader", got)
		}
		if want := 2 * placementPause; took != want {
			t.Errorf("the five placements took %v, want the first three back to back and two "+
				"a heartbeat apart, %v", took, want)
		}
		drop()
		js.mu.Lock()
		defer js.mu.Unlock()
		if js.made != 5 || len(js.deleted) != 5 {
			t.Errorf("the read made %d consumers and deleted %d, want five of each: four "+
				"misplaced and the one it read", js.made, len(js.deleted))
		}
	})
}

// A READ NO CONSUMER OF WHICH IS PLACED ON THE LEADER FAILS AFTER ITS
// PLACEMENTS, AS AN OUTAGE.
//
// A broker that never places a consumer on the named leader (it moved, or is
// never counted caught up) is given [placements] of them, paced as above and
// every one deleted; then the read fails as queue.ErrUnavailable, to be asked
// again later, never the answer that the entry is gone. Exact, in a bubble.
//
// Mutation: keep any consumer, and the read answers from a non-leader copy; or
// make a placement more or fewer, or none paced, and the count or time differs.
func TestAReadNoConsumerOfWhichIsPlacedOnTheLeaderFailsAfterItsPlacements(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		js := &placingJS{servers: []string{"member-a", "member-b", "member-c"}}
		q := &Queue{js: js}
		c := &copyRead{q: q, stream: "UNPLACED", pause: placementPause,
			leading: func(context.Context) (string, int, error) { return "member-x", 3, nil }}
		began := time.Now()
		_, _, err := c.onLeader(context.Background(), jetstream.ConsumerConfig{})
		took := time.Since(began)
		if !errors.Is(err, queue.ErrUnavailable) {
			t.Fatalf("a read the broker never placed on its leader = %v, want queue.ErrUnavailable", err)
		}
		tries := placements(3)
		if made := q.ConsumerProposals(); made != tries {
			t.Errorf("the read made %d consumers, want its %d placements", made, tries)
		}
		if want := time.Duration(tries-3) * placementPause; took != want {
			t.Errorf("the placements took %v, want the first three back to back and the rest "+
				"a heartbeat apart, %v", took, want)
		}
		js.mu.Lock()
		defer js.mu.Unlock()
		if len(js.deleted) != tries {
			t.Errorf("the read deleted %d of its %d consumers, want every one", len(js.deleted), tries)
		}
	})
}
