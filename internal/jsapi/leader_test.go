package jsapi_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsapi/jsapitest"
	"github.com/crewlet/crewlet/internal/jsprovision"
)

// leaderServer is a broker with no JetStream domain ([jsapitest]'s
// requirement) and one stream, LOG, holding one message on log.a, and a
// leader reader of it through the account's API.
func leaderServer(t *testing.T) (*server.Server, *jsapi.Leader) {
	t.Helper()
	srv, js := leaderServerFor(t)
	leader, err := jsapi.NewLeader(js, "LOG")
	if err != nil {
		t.Fatal(err)
	}
	return srv, leader
}

// leaderServerFor is [leaderServer]'s broker and a client of its account's
// API built with opts.
func leaderServerFor(t *testing.T, opts ...jetstream.JetStreamOpt) (*server.Server, jetstream.JetStream) {
	t.Helper()
	srv, err := server.NewServer(&server.Options{
		ServerName: "jsapi-leader-test",
		JetStream:  true,
		DontListen: true,
		StoreDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("configure the broker: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(30 * time.Second) {
		srv.Shutdown()
		t.Fatal("the broker did not become ready")
	}
	t.Cleanup(func() {
		srv.Shutdown()
		srv.WaitForShutdown()
	})
	nc, err := nats.Connect("", nats.InProcessServer(srv))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	// The account's own API, as [jsapi.Account] builds it — through
	// jetstream.New, which is that API with the client's options.
	js, err := jetstream.New(nc, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if api, err := jsapi.Of(js); err != nil || api != jsapi.Account() {
		t.Fatalf("the case's client speaks %v (%v), want the account's own API", api, err)
	}
	if _, err := js.CreateStream(t.Context(), jetstream.StreamConfig{
		Name: "LOG", Subjects: []string{"log.>"},
	}); err != nil {
		t.Fatalf("create the stream: %v", err)
	}
	if _, err := js.Publish(t.Context(), "log.a", []byte("held")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	return srv, js
}

// A LEADER READ A MEMBER DROPS IS ASKED AGAIN.
//
// The broker decides a read when a worker takes it from its API queue, and a
// member that still believes in a leader the group has just lost returns
// without a word: nothing keeps the request to answer later. One request
// waited out its caller's deadline for a reply nobody was going to send — the
// object collector's boot pass lost its read of the files bucket that way, and
// the write fence's floor read is the same request. Asked again after
// the read's term, the second request reaches the leader the first never did.
//
// The context is a few terms long, so a read that never asks again fails it.
func TestALeaderReadAMemberDropsIsAskedAgain(t *testing.T) {
	t.Parallel()
	srv, leader := leaderServer(t)
	held := jsapitest.DropFirst(t, srv, jsapitest.MsgGet("LOG"), 1)

	ctx, cancel := context.WithTimeout(t.Context(), 3*(jsprovision.ReadTerm+jsprovision.ReAsk))
	defer cancel()
	msg, err := leader.Last(ctx, "log.a")
	if err != nil {
		t.Fatalf("a read the broker dropped once was not asked again: %v", err)
	}
	if string(msg.Data) != "held" || msg.Subject != "log.a" {
		t.Errorf("read %q on %s, want \"held\" on log.a", msg.Data, msg.Subject)
	}
	if got := held.Count(); got != 1 {
		t.Errorf("the broker held %d reads, want the 1 it dropped", got)
	}
}

// A LATE ANSWER TO A READ ALREADY ASKED AGAIN IS TAKEN.
//
// A request behind a backed-up API queue is answered late rather than never.
// Thrown away at its term and replaced, a late reply is never heard — and the
// replacement joins the same queue a term behind it, so once the leader's
// queue answers slower than the term no read is taken at all. So every request
// stays outstanding while the next is sent: here the first is dropped, the
// second answered only after the third has been asked, and the third dropped
// as well — the second's answer is the only one there is.
//
// THE TAP IS THE PROOF that it was the second's: released, the broker hands
// every request on the stream's subject to the tap as well as to the leader —
// the second's, sent on, and any fourth the read sent before it was answered
// — and the read must have returned on the second's alone.
func TestALateAnswerToALeaderReadAlreadyAskedAgainIsTaken(t *testing.T) {
	t.Parallel()
	srv, leader := leaderServer(t)
	held := jsapitest.Hold(t, srv, jsapitest.MsgGet("LOG"))
	tap, err := nats.Connect("", nats.InProcessServer(srv))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tap.Close)
	after, err := tap.SubscribeSync(jsapitest.MsgGet("LOG"))
	if err != nil {
		t.Fatal(err)
	}
	if err := tap.Flush(); err != nil {
		t.Fatal(err)
	}

	type sent struct {
		reply string
		err   error
	}
	answered := make(chan sent, 1)
	go func() {
		ctx := t.Context()
		if _, err := held.Next(ctx); err != nil { // the first, dropped
			answered <- sent{err: err}
			return
		}
		second, err := held.Next(ctx)
		if err != nil {
			answered <- sent{err: err}
			return
		}
		if _, err := held.Next(ctx); err != nil { // the third, dropped
			answered <- sent{err: err}
			return
		}
		held.Release()
		answered <- sent{reply: second.Reply(), err: second.Answer()}
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 5*(jsprovision.ReadTerm+jsprovision.ReAsk))
	defer cancel()
	msg, err := leader.Last(ctx, "log.a")
	if err != nil {
		t.Fatalf("a read whose only answer came late, to a request already "+
			"asked again, failed: %v", err)
	}
	if string(msg.Data) != "held" {
		t.Errorf("read %q, want \"held\"", msg.Data)
	}
	second := <-answered
	if second.err != nil {
		t.Fatalf("the case could not answer the second read: %v", second.err)
	}
	if err := tap.Flush(); err != nil {
		t.Fatal(err)
	}
	var reached []string
	for {
		m, err := after.NextMsg(50 * time.Millisecond)
		if errors.Is(err, nats.ErrTimeout) {
			break
		}
		if err != nil {
			t.Fatalf("read the tap: %v", err)
		}
		reached = append(reached, m.Reply)
	}
	if len(reached) != 1 || reached[0] != second.reply {
		t.Errorf("after the release the leader was asked by %q, want only the "+
			"second request's %q: the answer taken was not its late one",
			reached, second.reply)
	}
}

// A READ NOBODY ANSWERS ENDS WITH ITS CALLER, NAMING THE STREAM — every
// request it sent outstanding to the end, and none outliving it.
//
// The caller's deadline falls just short of a fourth request, so the third —
// two terms and two re-asks in — has nearly two seconds to spare, and a timer
// never fires early, so no fourth can be counted.
func TestALeaderReadNobodyAnswersEndsWithItsCaller(t *testing.T) {
	t.Parallel()
	srv, leader := leaderServer(t)
	held := jsapitest.Hold(t, srv, jsapitest.MsgGet("LOG"))

	ctx, cancel := context.WithTimeout(t.Context(),
		3*(jsprovision.ReadTerm+jsprovision.ReAsk)-jsprovision.ReadTerm/10)
	defer cancel()
	_, err := leader.Last(ctx, "log.a")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a read nobody answered returned %v, want its caller's deadline", err)
	}
	if got := held.Count(); got != 3 {
		t.Errorf("the read asked %d times in two terms and two re-asks, want 3", got)
	}
}

// A LEADER READ WHOSE CALLER SET NO DEADLINE ENDS AT ITS CLIENT'S OWN DEFAULT —
// a message read and every page of a listing alike — asked again inside it,
// and never stretched to the lookup ceiling.
//
// Each was ONE request the client held for its DefaultTimeout where its
// caller set none, and every such caller — a boot's steps, a duty's tick, a
// heartbeat — was sized against that bound: a pod's startup window counts
// each read at it. Hedged, a read asks again inside the bound, so a request a
// member dropped is asked again where the new leader answers it, and a broker
// that never answers costs no longer than it did. The client here is given a
// default of 3.9 seconds — just short of a third request, at four — so each
// read is asked at none and two, twice, the second with nearly two seconds to
// spare, and a read held to the thirty-second ceiling instead is told apart
// by far more than a late timer.
func TestALeaderReadWithNoDeadlineEndsAtItsClientsDefault(t *testing.T) {
	t.Parallel()
	const bound = 3900 * time.Millisecond
	for _, c := range []struct {
		name    string
		subject string
		read    func(context.Context, jetstream.JetStream) error
	}{
		{"a message", jsapitest.MsgGet("LOG"), func(ctx context.Context, js jetstream.JetStream) error {
			l, err := jsapi.NewLeader(js, "LOG")
			if err != nil {
				return err
			}
			_, err = l.Last(ctx, "log.a")
			return err
		}},
		{"a subjects page", jsapitest.StreamInfo("LOG"), func(ctx context.Context, js jetstream.JetStream) error {
			l, err := jsapi.NewLeader(js, "LOG")
			if err != nil {
				return err
			}
			return l.Subjects(ctx, "log.>", func(map[string]uint64) error { return nil })
		}},
		{"a stream names page", jsapitest.StreamNames(), func(ctx context.Context, js jetstream.JetStream) error {
			m, err := jsapi.NewMeta(js)
			if err != nil {
				return err
			}
			return m.StreamNames(ctx, func(string) error { return nil })
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv, js := leaderServerFor(t, jetstream.WithDefaultTimeout(bound))
			held := jsapitest.Hold(t, srv, c.subject)
			done := make(chan error, 1)
			start := time.Now()
			go func() { done <- c.read(context.Background(), js) }()
			select {
			case err := <-done:
				if !jsprovision.Unanswered(context.Background(), err) {
					t.Errorf("a read nobody answered ended with %v, which its caller "+
						"cannot read as silence", err)
				}
				if took := time.Since(start); took < bound {
					t.Errorf("the read gave up after %v, inside its client's %v", took, bound)
				}
			case <-time.After(jsprovision.LookupBudget / 2):
				t.Fatalf("a read nobody answers was still being asked %v later, past its "+
					"client's %v default (%d requests)", jsprovision.LookupBudget/2, bound,
					held.Count())
			}
			if n := held.Count(); n != 2 {
				t.Errorf("the read was asked %d times inside its client's %v, want 2: at "+
					"none and two seconds", n, bound)
			}
		})
	}
}
