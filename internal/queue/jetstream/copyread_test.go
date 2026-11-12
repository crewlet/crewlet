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
// A consumer reads the copy of the replica the broker placed it on, and only
// the leader's copy answers what the leader's read answers: one behind it
// calls an entry that exists gone, and one that has not applied a drop calls
// the dropped entry held. A consumer's create answer names the server it runs
// on — or no server, on one that is not clustered, which holds the stream's
// one copy — so a read keeps the consumer only where that is the leader's.
//
// Mutation: keep a consumer whatever server it names, and the row of a
// follower's consumer is kept.
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
// A healthy cluster's broker places each consumer uniformly among the
// stream's replicas — every live one counts as caught up there, which is the
// order it prefers (copyread.go's head) — so a read that made a consumer fewer
// times gave up on the leader's copy more often than once in a million reads,
// and one that made it more times spent a broker's consumers on a read that a
// broker which never places one there has already failed. A stream of one
// replica is led by it, and one consumer is enough.
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
// leader's copy states as it is made, with no pull at all: a consumer begun at
// a sequence counts what the copy holds there and past it, matching its
// filter, and one made for the newest on a subject counts it, or nothing. This
// holds the broker to it, as the pinned server answers it (nats-server
// v2.15.2, consumer.go's streamNumPending, read into the create answer's
// NumPending): a sequence past the stream's end counts nothing, one removed
// with messages past it counts them, one removed at the stream's end counts
// nothing, a subject holding nothing counts nothing, and a subject's newest
// counts one.
//
// Mutation: the broker's — a server that counted otherwise would fail a row
// here before any copy read could misread it.
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
// On a member's own connection the leader's read crosses no link — at most a
// cluster route, which carries any size. A client of a broker somebody else
// runs crosses that broker's routes and its delivery to its own client, and
// no server holds either to a max_payload: only a leaf's server checks what
// its link carries (nats-server v2.15.2, leafnode.go). So on both, a dead
// letter's frame and its drop's check, and a state log's record and a
// subject's newest, each at the transport's ceiling, are read whole from the
// leader in one request, with no consumer made for any of them — where a
// read through consumers is the stream's info and a consumer or more the
// metadata leader makes and deletes, on the write path of every create on an
// external fleet's large logs.
//
// Mutation: decide the copy reads by the connection's pace rather than by the
// leaf's link, and the external client's reads make consumers; or read from
// a copy on a member's own connection, and the member's do.
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
// The leader's read is one request, where a read through a consumer is the
// stream's info, a consumer or more the metadata leader makes and deletes and
// a pull — so a frame is read through a consumer only where the leader's
// reply could not cross the leaf's link: past the transport, or past the
// ceiling at the link's pace. Here a queue across a leaf's link at the floor
// reads a small entry, one of two mebibytes — whose reply fits the transport
// and takes over a minute to cross at the floor, past the thirty seconds a
// leader's read is asked for — and one at the keep's ceiling, the leader's
// message reads held by the case: the small one's read reaches the leader and
// is answered there, and neither of the others reaches a leader's read at
// all. The queue is [leafLinkedQueue]'s, so the case can hold the leader's
// reads.
//
// Mutation: read every frame across a leaf's link from a copy, and the small
// entry's read never reaches the leader; from the leader, and the ceiling's
// does; or route by the transport alone, and the two mebibytes' does.
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

	held := jsapitest.Hold(t, ns, jsapitest.MsgGet(stream), nats.Token(leaderReadsToken))
	type answer struct {
		frame []byte
		held  bool
		err   error
	}
	done := make(chan answer, 1)
	go func() {
		frame, ok, err := q.DeadLetterFrame(ctx, subject, kept[0].Seq)
		done <- answer{frame, ok, err}
	}()
	// LIVENESS: the small entry's read reaches the leader's read once its
	// copy has said what the sequence holds; this ends a wait for one that
	// never does.
	wait, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	read, err := held.Next(wait)
	if err != nil {
		t.Fatalf("the small entry's frame never reached the leader's read: %v", err)
	}
	held.Release()
	if err := read.Answer(); err != nil {
		t.Fatalf("answer the held read: %v", err)
	}
	if got := <-done; got.err != nil || !got.held || !bytes.Equal(got.frame, small) {
		t.Fatalf("the small entry's frame = %q, %t, %v, want %q", got.frame, got.held, got.err, small)
	}

	again := jsapitest.Hold(t, ns, jsapitest.MsgGet(stream), nats.Token(leaderReadsToken))
	for i, body := range [][]byte{middle, large} {
		entry := kept[i+1].Seq
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
		t.Errorf("reading and dropping the two large entries across a leaf's link asked the "+
			"leader for them %d times (%v), want none", n, err)
	}
}

// leafLinkedQueue is [accountQueue]'s — a queue dialled to a broker of its
// own, read through the account's API, so [jsapitest] can stand between its
// reads and the broker — whose connection is taken to cross a leaf's link:
// the fact the copy reads decide by ([link.leaf]). A leaf's own connection
// speaks the fleet's domain, whose API jsapitest cannot hold; a real leaf's
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
// A cluster drops a new consumer's first pull routinely, and a read whose
// consumer never got it waits for a delivery the broker was never asked for.
// One that waited out its pull's whole wait for it — a raw read across a link
// at the floor waits minutes — failed then as a heartbeat missed, and never
// asked again. So a pull that hears nothing for a quiet second asks its
// consumer, and one that holds no pull and has delivered nothing is made
// again at once. Here the first pull of a probe and of a raw read is each
// dropped, on a link paced so slow that the raw read waits hours for its
// message: each answers what the stream holds within a minute.
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
// Every request a copy read makes runs under its caller's context, so a
// caller that goes — a node's shutdown, which waits for the replays it
// cancelled — ends the read at once, where a pull bounded by its own wait held
// its caller for the rest of it: minutes, for a raw read at the floor. Here
// every pull is held by the case, on a link paced so slow that the raw read
// waits hours for its message, and the caller is cancelled once the read's
// first pull is held: the read ends with its caller's cancellation within a
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
// A probe's delivery is a few hundred bytes of headers, and on a slow link
// they wait behind whatever the broker sent the node first — a whole frame a
// replay is reading, an applier's batch. A pull whose inbox was dropped when
// the client stopped waiting for it, two seconds in, threw the delivery away
// as it arrived, and the read failed on the consumer's count of a delivery
// that never arrived. The read's inbox is held for the whole read, so a
// delivery is taken however late: here the broker's bytes are held back on
// the link for three seconds from the moment the probe's pull reaches it, and
// the probe answers what the sequence holds.
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

// A PAYLOAD READ OF A MESSAGE REMOVED SINCE ITS PROBE CARRIES NO LARGER ONE.
//
// A payload read's consumer is filtered to the probed message's subject, so
// where a peer removed the message after its probe, the next message on that
// subject is the consumer's first — and a raw read carried it across the link
// whole, up to eight mebibytes, only to discard it. Its pull is bounded by the
// probed message's size, so the broker answers with the bound instead of the
// larger message, and the read asks the leader's copy again what the sequence
// holds: nothing. Here a 64 KiB message is probed, removed, and followed on
// its subject by one of a mebibyte: the read answers it gone, and the
// mebibyte never reaches the reader.
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
// A payload's pull is bounded by the probed message's size, and the broker
// answers a pull whose next message is past the bound with the bound instead —
// which says the probed message is gone only where its size was counted right.
// So a read the bound held back asks the leader's copy again what the
// sequence holds, and answers gone only where that says so: here a message's
// size is counted far short, the bound holds it back, and the read refuses to
// answer rather than call a message that is there gone.
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
// The state log's twin of the frame's case above: on a log whose records may
// be as large as the transport, a record whose own reply crosses the leaf's
// link inside the ceiling is read with the leader's one request, at a term of
// its own reply's crossing (TestAConnectionsReadsAndWritesArePacedByOneDecision
// holds the term), and one whose reply does not is read raw. Here a small
// record's read reaches the leader's read the case holds, and is answered
// there; a record at the transport's ceiling reaches no leader's read at all.
//
// Mutation: read every record across a leaf's link raw, and the small
// record's read never reaches the leader; or every one from the leader, and
// the ceiling's does.
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
// Only the leader's copy answers what the leader's read answers, so a read
// deletes every consumer the broker placed elsewhere and keeps the one it
// placed on the server the stream's leader is named as — a follower of the
// broker's here, member-c, which the broker reaches on its fifth placement.
// The first three, a draw as large as the stream keeps replicas, are made back
// to back; the broker prefers replicas its metadata leader counts as caught
// up, so past them a read that keeps missing is made a metadata heartbeat
// apart, which is what it takes a leader passed over for that to be counted
// again. Exact, in a bubble.
//
// Mutation: keep any consumer, and the first is kept; or make every one back
// to back, or pause before the first draw's too, and the time it took is
// another.
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
// A broker that never places a consumer on the leader the read was told of —
// one whose leader has moved, or one that never counts it as caught up — is
// given [placements] of them, the first draw back to back and the rest a
// heartbeat apart, every one deleted; and then the read fails rather than
// read a copy that is not the leader's, as queue.ErrUnavailable: a broker the
// caller may ask again later, never the answer that the entry is gone. Exact,
// in a bubble.
//
// Mutation: keep any consumer, and the read answers from a copy that is not
// the leader's; or make a placement more or fewer, or none of them paced, and
// the count or the time is another.
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
