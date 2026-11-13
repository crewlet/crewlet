package jetstream

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// sendsJS answers each send of a write from its script — an acknowledgement
// once the script has run out — and records, of every send it was handed, the
// instant, the deadline of the context it was handed and the bytes its message
// carried beside its subject, measured here rather than by the code under
// test.
//
// EMBEDS THE INTERFACE for the reason notYetVisibleJS gives: the two methods a
// write calls are two of dozens.
type sendsJS struct {
	jetstream.JetStream
	answers []error
	sends   []sentWrite
}

// sentWrite is one send as sendsJS saw it.
type sentWrite struct {
	at, deadline time.Time
	set          bool
	bytes        int
}

func (j *sendsJS) Options() jetstream.JetStreamOptions {
	return jetstream.JetStreamOptions{DefaultTimeout: jsapi.RequestTerm}
}

func (j *sendsJS) PublishMsg(ctx context.Context, msg *nats.Msg, _ ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	deadline, set := ctx.Deadline()
	j.sends = append(j.sends, sentWrite{at: time.Now(), deadline: deadline, set: set,
		bytes: (&nats.Msg{Header: msg.Header}).Size() + len(msg.Data)})
	if len(j.answers) > 0 {
		answer := j.answers[0]
		j.answers = j.answers[1:]
		if answer != nil {
			return nil, answer
		}
	}
	return &jetstream.PubAck{Stream: "PROBE", Sequence: uint64(len(j.sends))}, nil
}

// aWrite is one of the queue's acknowledged writes, made on q in ctx.
type aWrite struct {
	name  string
	write func(ctx context.Context, q *Queue) error
}

// The subjects the writes below resolve, each to a stream [budgetedQueue]
// knows already.
var (
	budgetEvents  = topics.Event("budget_probe")
	budgetLetters = topics.DeadLetter("budget.probe", "grp")
)

// everyWrite is every acknowledged write the queue makes, each at a size of
// its own and with the headers it sends: a publish, a keep, the delivery
// budget's dead letter, and a state log's append carrying its operation's id
// and its expectation.
func everyWrite() []aWrite {
	return []aWrite{
		{"a publish", func(ctx context.Context, q *Queue) error {
			return q.Publish(ctx, budgetEvents, ev(1))
		}},
		{"a dead letter's keep", func(ctx context.Context, q *Queue) error {
			_, err := q.KeepDeadLetter(ctx, budgetLetters, "a budget probe", []byte(`{"v":1}`),
				make([]byte, 3<<20))
			return err
		}},
		{"the delivery budget's dead letter", func(ctx context.Context, q *Queue) error {
			return q.deadLetter(ctx, budgetLetters, make([]byte, 5<<20))
		}},
		anAppend,
	}
}

// anAppend is a state log's conditional append, as the state log's publisher
// makes one: an operation's id and an expectation beside its record.
var anAppend = aWrite{"a state log's append", func(ctx context.Context, q *Queue) error {
	at := uint64(41)
	_, _, err := (&DomainLog{js: q.js, q: q}).Append(ctx, "crewlet.probe.log.budget", "op-budget",
		&at, make([]byte, 7<<20))
	return err
}}

// budgetedQueue is a queue over js whose connection is paced at pace, every
// stream its writes resolve already known to it, so a write goes straight to
// its sends.
func budgetedQueue(t *testing.T, js jetstream.JetStream, pace time.Duration) *Queue {
	t.Helper()
	streams := map[string]struct{}{}
	for _, subject := range []string{budgetEvents, budgetLetters} {
		spec, err := specForSubject(subject, 0)
		if err != nil {
			t.Fatalf("the stream %s resolves to: %v", subject, err)
		}
		streams[spec.name] = struct{}{}
	}
	return &Queue{js: js, link: link{pace: pace}, streams: streams}
}

// EACH WRITE IS BUDGETED FOR THE BYTES IT SENDS, through the one budget a
// write of the queue's is given.
//
// Where its caller set no deadline, a write on a member's own connection waits
// the request term alone, and one across a link the request term and the
// crossing of exactly the bytes it sends — its header block and its payload,
// as the client handed the send them, measured here. A caller's deadline,
// longer than any budget, is the send's as it is. Each write is driven whole,
// so what is held is the context its send was handed, never a helper's answer
// beside it. Exact, in a bubble.
//
// Mutation: budget an append for its record alone, pace a write by the object
// store's floor rather than by its connection, or hold a write to its budget
// with a timeout of its own rather than through the queue's one budget, and a
// row goes red: the append across a link a header block short, every write on
// a member a crossing over, and the caller's hour cut to the budget.
func TestEachWriteIsBudgetedForTheBytesItSends(t *testing.T) {
	t.Parallel()
	for _, link := range []struct {
		name string
		pace time.Duration
	}{
		{"on a member's own connection", linkOf(&embeddedServer{}).pace},
		{"across a leaf's link", linkOf(&embeddedServer{leaf: true}).pace},
	} {
		for _, w := range everyWrite() {
			t.Run(w.name+" "+link.name, func(t *testing.T) {
				t.Parallel()
				synctest.Test(t, func(t *testing.T) {
					js := &sendsJS{}
					if err := w.write(context.Background(), budgetedQueue(t, js, link.pace)); err != nil {
						t.Fatalf("the write: %v", err)
					}
					if len(js.sends) != 1 {
						t.Fatalf("%d sends, want the one", len(js.sends))
					}
					s := js.sends[0]
					want := jsapi.RequestTerm + jsapi.Sending(s.bytes, link.pace)
					if !s.set || s.deadline.Sub(s.at) != want {
						t.Errorf("with no deadline of its caller's it waits %v (set %t), want the "+
							"request term and the crossing of the %d bytes it sent, %v",
							s.deadline.Sub(s.at), s.set, s.bytes, want)
					}

					js = &sendsJS{}
					caller, cancel := context.WithTimeout(context.Background(), time.Hour)
					defer cancel()
					if err := w.write(caller, budgetedQueue(t, js, link.pace)); err != nil {
						t.Fatalf("the write with its caller's deadline: %v", err)
					}
					kept, _ := caller.Deadline()
					if len(js.sends) != 1 || !js.sends[0].deadline.Equal(kept) {
						t.Errorf("with its caller's hour it was sent %+v, want the one send held "+
							"to the caller's deadline, %v", js.sends, kept)
					}
				})
			})
		}
	}
}

// A WRITE SENT AGAIN IS GIVEN ITS OWN CROSSING.
//
// A write is sent again where its answer decided nothing: nobody heard it, or
// the leader refused an append without comparing anything because another
// write to its subject was in flight. Every send carries the whole message
// across the link again, so where the caller set no deadline each one after
// the first is given a crossing more than the one before; given the first
// send's budget, the second had what the first left of the request term, and
// a large record across a slow link read as no answer while it was crossing.
// Exact, in a bubble.
//
// Mutation: give every send the first send's budget, and each second send is
// a crossing short.
func TestAWriteSentAgainIsGivenItsOwnCrossing(t *testing.T) {
	t.Parallel()
	inFlight := &jetstream.APIError{Code: 400,
		ErrorCode:   jetstream.JSErrCodeStreamWrongLastSequenceConstant,
		Description: "wrong last sequence"}
	for _, c := range []struct {
		answer string
		err    error
		writes []aWrite
	}{
		{"nobody heard it", jetstream.ErrNoStreamResponse, everyWrite()},
		{"the leader had another write in flight", inFlight, []aWrite{anAppend}},
	} {
		for _, w := range c.writes {
			t.Run(w.name+" "+c.answer, func(t *testing.T) {
				t.Parallel()
				synctest.Test(t, func(t *testing.T) {
					js := &sendsJS{answers: []error{c.err}}
					q := budgetedQueue(t, js, linkOf(&embeddedServer{leaf: true}).pace)
					if err := w.write(context.Background(), q); err != nil {
						t.Fatalf("the write sent again: %v", err)
					}
					if len(js.sends) != 2 {
						t.Fatalf("%d sends, want the one answered %q and the one sent again",
							len(js.sends), c.answer)
					}
					first, again := js.sends[0], js.sends[1]
					crossing := jsapi.Sending(first.bytes, q.link.pace)
					if gained := again.deadline.Sub(first.deadline); !again.set || gained != crossing {
						t.Errorf("the send again was given %v more than the first (set %t), "+
							"want its own crossing, %v", gained, again.set, crossing)
					}
				})
			})
		}
	}
}

// countedJS counts the sends the queue hands its client.
type countedJS struct {
	jetstream.JetStream
	sends atomic.Int32
}

func (c *countedJS) PublishMsg(ctx context.Context, msg *nats.Msg, opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	c.sends.Add(1)
	return c.JetStream.PublishMsg(ctx, msg, opts...)
}

// A WRITE NOBODY HEARD IS SENT AGAIN BY THE QUEUE, NEVER BY ITS CLIENT.
//
// The client sends a message nobody heard again inside the ONE context it was
// handed, and across a link to an external cluster that answer comes after the
// message has crossed whole — so a re-send the client made crossed again
// inside a budget sized for one crossing. The queue turns the client's
// re-sends off and makes them itself, each in a context of its own. Here a
// responder of the case's own answers the first send as a server answers one
// nobody heard, and acknowledges the second: the client was handed two sends,
// and the responder heard two.
//
// Mutation: leave the client's own re-sends on, and the responder hears two
// sends inside the one the queue handed its client.
func TestAWriteNobodyHeardIsSentAgainByTheQueueNeverItsClient(t *testing.T) {
	t.Parallel()
	q := newQueue(t)
	counted := &countedJS{JetStream: q.js}
	const subject = "probe.nobody.heard"
	var heard atomic.Int32
	sub, err := q.nc.Subscribe(subject, func(m *nats.Msg) {
		if heard.Add(1) == 1 {
			_ = m.RespondMsg(&nats.Msg{Header: nats.Header{"Status": []string{"503"}}})
			return
		}
		_ = m.Respond([]byte(`{"stream":"PROBE","seq":7}`))
	})
	if err != nil {
		t.Fatalf("subscribe the responder: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := q.nc.Flush(); err != nil {
		t.Fatalf("flush the responder: %v", err)
	}
	// THE ONE LOOP EVERY WRITE OF THE QUEUE'S IS SENT BY, over a client
	// that counts what it is handed.
	msg := &nats.Msg{Subject: subject, Data: []byte("probe")}
	ack, err := sendWithin(t.Context(), q.writing(msg), counted, msg)
	if err != nil || ack == nil || ack.Sequence != 7 {
		t.Fatalf("a write nobody heard the first time = (%+v, %v), want it acknowledged the second",
			ack, err)
	}
	if sends, heard := counted.sends.Load(), heard.Load(); sends != 2 || heard != 2 {
		t.Errorf("the queue handed its client %d sends and the responder heard %d, want two "+
			"each: every send the queue's own", sends, heard)
	}
}

// A WRITE COUNTS WHAT IT SENDS.
//
// The budget a write is given counts its message — its header block and its
// payload — read off the message as it is built ([carried]), so the count
// holds only while the message built is the whole of what crosses: a header
// the client wrote from an option, after the count was taken, would cross
// uncounted. So each shape a write builds — a publish, a keep with a header
// and without, an append with each of its headers, both or neither — is sent
// to a responder of the case's own, and the count held to what the responder
// was handed as it crossed.
//
// Mutation: count a message's payload alone, or have a write's send add a
// header the message did not carry, and a row with headers goes red.
func TestAWriteCountsWhatItSends(t *testing.T) {
	t.Parallel()
	q := newQueue(t)
	const subjects = "probe.counted"
	crossed := make(chan int, 1)
	sub, err := q.nc.Subscribe(subjects+".>", func(m *nats.Msg) {
		crossed <- m.Size() - len(m.Subject) - len(m.Reply)
		_ = m.Respond([]byte(`{"stream":"PROBE","seq":1}`))
	})
	if err != nil {
		t.Fatalf("subscribe the responder: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := q.nc.Flush(); err != nil {
		t.Fatalf("flush the responder: %v", err)
	}
	zero := uint64(0)
	for i, c := range []struct {
		name string
		msg  func(subject string) *nats.Msg
	}{
		{"a publish", func(s string) *nats.Msg { return &nats.Msg{Subject: s, Data: []byte(`{"type":"probe"}`)} }},
		{"a keep with its keeper's header", func(s string) *nats.Msg {
			return letterMsg(s, "CREWLET_DLQ", "counted", []byte(`{"v":1}`), []byte("body"))
		}},
		{"a keep with none", func(s string) *nats.Msg {
			return letterMsg(s, "CREWLET_DLQ", "counted", nil, []byte("body"))
		}},
		{"an append with neither header", func(s string) *nats.Msg { return appendMsg(s, "", nil, []byte("{}")) }},
		{"an append with its operation's id", func(s string) *nats.Msg { return appendMsg(s, "op-counted", nil, []byte("{}")) }},
		{"an append with its expectation", func(s string) *nats.Msg { return appendMsg(s, "", &zero, []byte("{}")) }},
		{"an append with both", func(s string) *nats.Msg {
			return appendMsg(s, "op-counted-too", &zero, []byte("{}"))
		}},
	} {
		msg := c.msg(fmt.Sprintf("%s.%d", subjects, i))
		counted := carried(msg)
		if _, err := q.write(t.Context(), msg); err != nil {
			t.Fatalf("%s: the write: %v", c.name, err)
		}
		if got := <-crossed; got != counted {
			t.Errorf("%s: its budget counts %d bytes, and %d crossed", c.name, counted, got)
		}
	}
}

// A CONNECTION'S READS AND WRITES ARE PACED BY ONE DECISION.
//
// How slow a connection may be is decided once, where it is made ([linkOf]),
// and every crossing on it reads that one decision: a state log record's read
// by its position from the leader — given its log's largest record's
// crossing, or on a log read through its copies the record's own — and read
// raw; a dead letter's frame from the leader and raw; and a write's request.
// Decided apart, a pace changed for one of them — made configurable, or per
// link — would leave the others at the old one. Held here at a pace no link
// is given today, so a crossing that decided its own pace is told from one
// that read the connection's, across a leaf's link, where every one of them
// is made.
//
// Mutation: pace a record's read — from the leader or raw — a frame's read or
// a write by the object store's floor itself rather than by the connection's
// pace, and its row goes red.
func TestAConnectionsReadsAndWritesArePacedByOneDecision(t *testing.T) {
	t.Parallel()
	const pace = 7 * time.Second
	spec := probeDomain()
	q := domainQueue(t, spec)
	q.link = link{pace: pace, leaf: true}
	log, err := q.DomainLog(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	largest := min(q.nc.MaxPayload(), spec.MaxMessageBytes)
	if want := jsapi.Crossing(int(largest), pace); log.crossing != want {
		t.Errorf("a record's read is given %v to cross, want its largest record's crossing at "+
			"the connection's pace, %v", log.crossing, want)
	}
	// A LOG READ THROUGH ITS COPIES: its records as large as the transport,
	// each read from the leader at its own reply's crossing where that
	// reply crosses, and otherwise raw, waited for its own crossing.
	copied := DomainStream{Name: "CREWLET_PACED_LOG", Subjects: []string{"crewlet.paced.log.>"},
		MaxBytes: 4 * queue.MaxPayloadBytes, MaxMessageBytes: queue.MaxPayloadBytes,
		Duplicates: 2 * time.Minute}
	if err := q.EnsureDomainStream(t.Context(), copied); err != nil {
		t.Fatalf("EnsureDomainStream: %v", err)
	}
	copiedLog, err := q.DomainLog(t.Context(), copied.Name)
	if err != nil {
		t.Fatalf("open the log of large records: %v", err)
	}
	if copiedLog.copies == nil {
		t.Fatal("a log of records as large as the transport is read from the leader alone " +
			"across a leaf's link, whose reply of its largest record cannot cross it")
	}
	const record = 64 << 10
	if want := jsapi.Crossing(record, pace); copiedLog.replyCrossing(record) != want {
		t.Errorf("a record read from the leader is given %v to cross, want its own reply's "+
			"crossing at the connection's pace, %v", copiedLog.replyCrossing(record), want)
	}
	if want := jsapi.Sending(record, pace); copiedLog.copies.crossing(record) != want {
		t.Errorf("a record read raw waits %v for its crossing, want its own at the "+
			"connection's pace, %v", copiedLog.copies.crossing(record), want)
	}
	letters, err := q.deadLetters(t.Context(), topics.DeadLetter("pace.probe", "grp"))
	if err != nil {
		t.Fatalf("open the dead letters: %v", err)
	}
	// A DEAD LETTER'S FRAME across the leaf's link: read from the leader at a
	// term of its reply's crossing where that reply crosses, and otherwise
	// raw, waited for the entry's own crossing — each at the connection's
	// pace.
	if letters.copies == nil {
		t.Fatal("a connection across a leaf's link reads its dead letters from the leader " +
			"alone, whose reply of an entry as large as the transport cannot cross it")
	}
	const entry = 64 << 10
	if want := jsprovision.ReadTerm + jsapi.Crossing(entry, pace); letters.carrying(q, entry).Term() != want {
		t.Errorf("a dead letter's frame is read from the leader at a term of %v, want the read "+
			"term and its reply's crossing at the connection's pace, %v",
			letters.carrying(q, entry).Term(), want)
	}
	if want := jsapi.Sending(entry, pace); letters.copies.crossing(entry) != want {
		t.Errorf("a dead letter's frame read raw waits %v for its crossing, want its own at "+
			"the connection's pace, %v", letters.copies.crossing(entry), want)
	}
	synctest.Test(t, func(t *testing.T) {
		js := &sendsJS{}
		if err := budgetedQueue(t, js, pace).Publish(context.Background(), budgetEvents, ev(1)); err != nil {
			t.Fatalf("publish: %v", err)
		}
		s := js.sends[0]
		if want := jsapi.RequestTerm + jsapi.Sending(s.bytes, pace); s.deadline.Sub(s.at) != want {
			t.Errorf("a write waits %v, want the request term and its crossing at the "+
				"connection's pace, %v", s.deadline.Sub(s.at), want)
		}
	})
}
