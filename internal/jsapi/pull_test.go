package jsapi_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsapi/jsapitest"
	"github.com/crewlet/crewlet/internal/jsprovision"
)

// pulledStream is the stream the pull cases read, on [pullServer].
const pulledStream = "PULLS"

// pullServer is [testBroker]'s broker, a connection to it and a client of its
// account's API, with [pulledStream] made on it holding the bodies given, in
// order, on pulls.a.
func pullServer(t *testing.T, bodies ...[]byte) (*server.Server, *nats.Conn, jetstream.JetStream) {
	t.Helper()
	srv, nc := testBroker(t, "jsapi-pull-test")
	js, err := jsapi.Account().Client(nc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.CreateStream(t.Context(), jetstream.StreamConfig{
		Name: pulledStream, Subjects: []string{"pulls.>"},
	}); err != nil {
		t.Fatalf("create the stream: %v", err)
	}
	for _, body := range bodies {
		if _, err := js.Publish(t.Context(), "pulls.a", body); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	return srv, nc, js
}

// pullsOf makes a consumer named name of [pulledStream], filtered to filter,
// and opens a reader's pulls of it.
func pullsOf(t *testing.T, nc *nats.Conn, js jetstream.JetStream, name, filter string) *jsapi.Pulls {
	t.Helper()
	cons, err := js.CreateConsumer(t.Context(), pulledStream, jetstream.ConsumerConfig{
		Name: name, FilterSubject: filter, AckPolicy: jetstream.AckNonePolicy,
	})
	if err != nil {
		t.Fatalf("create the consumer %s: %v", name, err)
	}
	p, err := jsapi.NewPulls(nc, jsapi.Account(), pulledStream, cons, jsprovision.Clustered(false).Timing())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Stop)
	return p
}

// next is the next word on p's inbox, the case failing where none comes in
// half a minute: a liveness bound, never the claim.
func next(t *testing.T, p *jsapi.Pulls) jsapi.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	ev, err := p.Next(ctx)
	if err != nil {
		t.Fatalf("the next word on the pulls' inbox: %v", err)
	}
	return ev
}

// A PULL WHOSE BYTES RUN OUT EXACTLY ENDS WITH NO STATUS, AND IS REPORTED
// ENDED.
//
// The server counts a delivery against a pull's byte bound as the subject, the
// reply, the header block and the payload, and a delivery that leaves nothing
// of the bound is its last for the pull: it sends no status after it. A reader
// that waited for a status there would wait out the pull's whole expiry for
// nothing, so the delivery itself is reported as the end. A pull with no byte
// bound ends the same way at its count's last delivery; one whose count runs
// out with bytes to spare ends at the `409 Batch Completed` that follows its
// last delivery, never at the delivery. Here a pull bounded at exactly the
// first two messages' bytes, as a consumer of the same shape counted them, is
// ended at the second; the next pull's first word is the third message,
// so no status of the first came after its end; and that pull, its count of
// one filled with bytes to spare, ends at its Batch Completed.
//
// Mutation: never report a delivery as the end of a bounded pull, and the
// second message reads as one more to come.
func TestAPullWhoseBytesRunOutExactlyEndsWithNoStatusAndIsReportedEnded(t *testing.T) {
	t.Parallel()
	_, nc, js := pullServer(t, []byte("first"), bytes.Repeat([]byte("second"), 50), []byte("third"))

	// THE SIZES AS THE SERVER COUNTS THEM, read off a consumer whose name is
	// as long as the reader's, so each delivery's reply is the same length.
	measured := pullsOf(t, nc, js, "measure", "pulls.a")
	if err := measured.Ask(3, 0, 10*time.Second, 0); err != nil {
		t.Fatal(err)
	}
	var sizes []int
	for i := range 3 {
		ev := next(t, measured)
		if ev.Kind != jsapi.Delivered {
			t.Fatalf("delivery %d of an unbounded pull of three = %q", i+1, ev.Kind)
		}
		if last := i == 2; ev.Ended != last {
			t.Errorf("delivery %d of an unbounded pull of three ended it %t, want only the "+
				"third", i+1, ev.Ended)
		}
		sizes = append(sizes, ev.Msg.Size())
	}

	p := pullsOf(t, nc, js, "readers", "pulls.a")
	if err := p.Ask(10, sizes[0]+sizes[1], 10*time.Second, 0); err != nil {
		t.Fatal(err)
	}
	for i, want := range []bool{false, true} {
		ev := next(t, p)
		if ev.Kind != jsapi.Delivered || ev.Meta.Sequence.Stream != uint64(i+1) || ev.Ended != want {
			t.Fatalf("word %d of a pull bounded at two messages' bytes = %q at %v, ended %t; "+
				"want delivery %d, ended %t", i+1, ev.Kind, ev.Meta, ev.Ended, i+1, want)
		}
	}
	if err := p.Ask(1, 1<<20, 10*time.Second, 0); err != nil {
		t.Fatal(err)
	}
	if ev := next(t, p); ev.Kind != jsapi.Delivered || ev.Meta.Sequence.Stream != 3 || ev.Ended {
		t.Fatalf("the next pull's first word = %q (%s %s), ended %t; want the third message, "+
			"not yet the end: a status of the pull before it came after its end",
			ev.Kind, ev.Code, ev.Description, ev.Ended)
	}
	if ev := next(t, p); ev.Kind != jsapi.BatchCompleted || !ev.Ended {
		t.Errorf("a pull of one filled with bytes to spare then said %q, ended %t; want its "+
			"Batch Completed, ending it", ev.Kind, ev.Ended)
	}
}

// A PULL A WAITING CONSUMER HOLDS IS HEARTBEATED UNTIL IT EXPIRES.
//
// A pull asked with an idle heartbeat is sent one while the consumer holds it
// with nothing to deliver: the server's word that it is alive, which a reader
// tells from silence. A heartbeat ends nothing; the pull ends at its expiry.
// Here a consumer of a subject that holds nothing is pulled for two seconds
// with a heartbeat every quarter of one: the reader hears heartbeats, none of
// them an end, and then the expiry, which is.
//
// Mutation: drop the heartbeat from the request, and the expiry is the first
// word.
func TestAPullAWaitingConsumerHoldsIsHeartbeatedUntilItExpires(t *testing.T) {
	t.Parallel()
	_, nc, js := pullServer(t)
	p := pullsOf(t, nc, js, "waiting", "pulls.none")
	if err := p.Ask(1, 1<<20, 2*time.Second, 250*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	heartbeats := 0
	for {
		ev := next(t, p)
		if ev.Kind == jsapi.Heartbeat && !ev.Ended {
			heartbeats++
			continue
		}
		if ev.Kind != jsapi.Expired || !ev.Ended {
			t.Fatalf("after %d heartbeats the pull said %q (%s %s), ended %t; want its "+
				"expiry, ending it", heartbeats, ev.Kind, ev.Code, ev.Description, ev.Ended)
		}
		break
	}
	if heartbeats == 0 {
		t.Error("the pull expired with no heartbeat heard while the consumer held it")
	}
}

// A PULL IS ASKED ON ONE INBOX FOR ITS WHOLE READ.
//
// The client's fetch subscribes an inbox for one pull and drops it when the
// client decides the pull is over, so a delivery the server made to it later,
// still crossing a link, landed on an inbox nobody held. A reader's pulls are
// every one asked on the one inbox it holds until it stops: here three pulls
// reach the broker on one reply subject, and the first, answered only after
// the other two were asked, still delivers to the reader.
//
// Mutation: subscribe an inbox for each pull, and the three replies differ.
func TestAPullIsAskedOnOneInboxForItsWholeRead(t *testing.T) {
	t.Parallel()
	srv, nc, js := pullServer(t, []byte("held"))
	p := pullsOf(t, nc, js, "inbox", "pulls.a")
	held := jsapitest.Hold(t, srv, jsapitest.ConsumerNext(pulledStream))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var reads []*jsapitest.Read
	for range 3 {
		if err := p.Ask(1, 0, 10*time.Second, 0); err != nil {
			t.Fatal(err)
		}
		read, err := held.Next(ctx)
		if err != nil {
			t.Fatalf("a pull never reached the broker: %v", err)
		}
		reads = append(reads, read)
	}
	for _, read := range reads[1:] {
		if read.Reply() != reads[0].Reply() {
			t.Fatalf("pulls asked on %q and %q, want one inbox", reads[0].Reply(), read.Reply())
		}
	}
	held.Release()
	if err := reads[0].Answer(); err != nil {
		t.Fatalf("send the first pull on: %v", err)
	}
	if ev := next(t, p); ev.Kind != jsapi.Delivered || string(ev.Msg.Data) != "held" {
		t.Errorf("the first pull, answered late, delivered %q (%s %s), want the message", ev.Kind,
			ev.Code, ev.Description)
	}
}

// A DELIVERY IS A LEADER'S REPLY WITHOUT ITS BASE64.
//
// A consumer's delivery carries a message as it was stored, where a leader's
// reply carries it base64-encoded inside JSON, so what a pull of n bytes takes
// to arrive is the reply's arrival for a reply of n bytes: handled at a second
// a mebibyte and crossing at the link's pace, raw.
//
// Mutation: count the delivery's bytes as a reply's, and every row is a third
// long.
func TestDeliveringIsArrivalWithoutBase64(t *testing.T) {
	t.Parallel()
	for _, pace := range []time.Duration{0, time.Second, 30 * time.Second} {
		if got, want := jsapi.Delivering(1<<20, pace), jsapi.HandlingPace+pace; got != want {
			t.Errorf("a mebibyte delivered at %v a mebibyte arrives in %v, want %v", pace, got, want)
		}
		for _, n := range []int{0, 1, 4 << 10, 512 << 10, 8 << 20} {
			arrival, delivering := jsapi.Arrival(n, pace), jsapi.Delivering(jsapi.ReplyBytes(n), pace)
			// One rounding against two: a nanosecond apart at most.
			if diff := arrival - delivering; diff < -1 || diff > 1 {
				t.Errorf("a reply of a %d-byte message arrives in %v at %v a mebibyte, and a "+
					"delivery of its %d bytes in %v", n, arrival, pace, jsapi.ReplyBytes(n), delivering)
			}
		}
	}
}

// A PULL THE SERVER WOULD REFUSE IS REFUSED BY NAME, BEFORE IT IS SENT.
//
// A pull with no expiry stands on the server for good, and one whose heartbeat
// is more than half its expiry the server answers with `400 Bad Request -
// heartbeat value too large`; each is refused here, naming what is wrong, with
// NOTHING SENT — a refused pull that went out anyway would leave that `400` on
// the reader's one inbox, read as the end of whichever pull it asked next.
// Here five such pulls are refused, each naming its field, and then one with a
// heartbeat of exactly half its expiry, which the server takes, is asked: it
// is the ONLY pull to reach the broker before a marker published after it on
// the same connection, and answered, it is held and heartbeated until it
// expires, never refused.
//
// Mutation: send the pull before its checks, and five more reach the broker,
// the first of them a refusal on the inbox.
func TestAPullTheServerWouldRefuseIsRefusedByName(t *testing.T) {
	t.Parallel()
	srv, nc, js := pullServer(t)
	p := pullsOf(t, nc, js, "refused", "pulls.none")
	held := jsapitest.Hold(t, srv, jsapitest.ConsumerNext(pulledStream))
	for _, tc := range []struct {
		name               string
		n, maxBytes        int
		expires, heartbeat time.Duration
		names              string
	}{
		{"no delivery asked", 0, 0, time.Second, 0, "delivery"},
		{"a negative byte bound", 1, -1, time.Second, 0, "byte bound"},
		{"no expiry", 1, 0, 0, 0, "expiry"},
		{"a heartbeat past half the expiry", 1, 0, time.Second, 501 * time.Millisecond, "heartbeat"},
		{"a negative heartbeat", 1, 0, time.Second, -time.Millisecond, "heartbeat"},
	} {
		err := p.Ask(tc.n, tc.maxBytes, tc.expires, tc.heartbeat)
		if err == nil || !strings.Contains(err.Error(), tc.names) {
			t.Errorf("%s: asked with %v, want refused naming its %s", tc.name, err, tc.names)
		}
	}
	if err := p.Ask(1, 0, time.Second, 500*time.Millisecond); err != nil {
		t.Fatalf("a heartbeat of half the expiry, which the server takes: %v", err)
	}

	// THE MARKER, on the pulls' own subject and connection, reaches the
	// broker after every pull the reader sent before it.
	const marker = "jsapi.pull-test.marker"
	if err := nc.PublishRequest(fmt.Sprintf(server.JSApiRequestNextT, pulledStream, "refused"),
		marker, nil); err != nil {
		t.Fatalf("send the marker: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var sent []*jsapitest.Read
	for {
		read, err := held.Next(ctx)
		if err != nil {
			t.Fatalf("the marker never reached the broker, after %d pulls: %v", len(sent), err)
		}
		if read.Reply() == marker {
			break
		}
		sent = append(sent, read)
	}
	if len(sent) != 1 {
		t.Fatalf("%d pulls reached the broker, want only the one asked with a heartbeat of half "+
			"its expiry", len(sent))
	}
	held.Release()
	if err := sent[0].Answer(); err != nil {
		t.Fatalf("send the pull on: %v", err)
	}
	for {
		ev := next(t, p)
		if ev.Kind == jsapi.Heartbeat {
			continue
		}
		if ev.Kind != jsapi.Expired {
			t.Errorf("the pull asked with a heartbeat of half its expiry said %q (%s %s), want "+
				"its heartbeats and then its expiry", ev.Kind, ev.Code, ev.Description)
		}
		break
	}
}
