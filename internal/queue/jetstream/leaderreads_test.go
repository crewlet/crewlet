package jetstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsapi/jsapitest"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/queue/jetstream/externaltest"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// leaderReadsToken is what accountQueue's broker requires: the dial refuses an
// external server that authenticates nobody, and jsapitest's interception
// signs in with it too.
const leaderReadsToken = "leader-reads"

// accountQueue is a queue dialled to a broker of its own with JetStream and no
// domain — an external cluster's shape, read through the account's API — with
// one domain log provisioned, so [jsapitest] can stand between the queue's
// reads and the broker.
func accountQueue(t *testing.T, spec DomainStream) (*server.Server, *Queue) {
	t.Helper()
	ns := accountBroker(t)
	q, err := Open(t.Context(), Config{URL: ns.ClientURL(), Token: leaderReadsToken})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	if err := q.EnsureDomainStream(t.Context(), spec); err != nil {
		t.Fatalf("EnsureDomainStream: %v", err)
	}
	return ns, q
}

// accountBroker is [accountQueue]'s broker: JetStream with no domain, holding
// a message to the transport, and requiring leaderReadsToken.
func accountBroker(t *testing.T) *server.Server {
	t.Helper()
	ns, err := server.NewServer(&server.Options{
		Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true,
		JetStream: true, StoreDir: brokertest.TempDir(t),
		JetStreamMaxStore: brokertest.StoreMaxBytes,
		MaxPayload:        int32(queue.MaxPayloadBytes),
		// An external server authenticates somebody, or the dial refuses it.
		Authorization: leaderReadsToken,
	})
	if err != nil {
		t.Fatalf("configure the broker: %v", err)
	}
	go ns.Start()
	t.Cleanup(func() {
		ns.Shutdown()
		ns.WaitForShutdown()
	})
	if !ns.ReadyForConnections(30 * time.Second) {
		t.Fatal("the broker never became ready")
	}
	return ns
}

// THE APPLIER'S PENDING COUNT ASKS THE BROKER NOTHING.
//
// The applier asks it at least once per committed run, per domain, per data
// node. A consumer info is answered only by the consumer's leader, once its
// main API queue is empty, and a member still believing in a lost leader drops
// it silently: a wait on the commit path. Every delivered record carries the
// count in its metadata, so the answer is what the pull holds: the newest
// delivery's count of what follows it, plus what it delivered that no fetch
// has taken. Each count is checked against the consumer infos the broker
// held, never the clock.
//
// Mutation: answer the count with a consumer info, and the broker holds one.
func TestTheApplierPendingCountAsksTheBrokerNothing(t *testing.T) {
	t.Parallel()
	ns, q := accountQueue(t, probeDomain())
	log, err := q.DomainLog(t.Context(), probeDomain().Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	cons, err := q.DomainConsumer(t.Context(), probeDomain().Name, "node-a", 0)
	if err != nil {
		t.Fatalf("open the consumer: %v", err)
	}
	t.Cleanup(cons.Close)
	const subject = "crewlet.probe.log.task.a"
	appendN(t, log, subject, 5)
	// THE CASE WAITS FOR THE CONSUMER TO COUNT ALL FIVE BEFORE THE FETCH: the
	// server acknowledges an append before it tells the stream's consumers, so
	// a fetch made as the appends return can carry a count short of them, and
	// the case holds Pending exact. Its info is asked here, before it is held.
	deadline := time.Now().Add(10 * time.Second)
	for {
		info, err := consumerInfo(t, cons)
		if err != nil {
			t.Fatalf("consumer info: %v", err)
		}
		if info.NumPending == 5 {
			break
		}
		if time.Until(deadline) <= 0 { //fixedwait:measure a poll's own deadline
			t.Fatalf("the consumer counts %d of the five appended records pending", info.NumPending)
		}
		// LIVENESS: paces a poll of the broker's own count, which decides;
		// a broker that never counts the five fails at the deadline above.
		time.Sleep(5 * time.Millisecond) //fixedwait:liveness
	}
	held := jsapitest.Hold(t, ns, jsapitest.ConsumerInfo(probeDomain().Name, cons.Name()), nats.Token(leaderReadsToken))

	pending := func(want uint64) {
		t.Helper()
		got, err := cons.Pending(context.Background())
		if n := held.Count(); n != 0 {
			t.Errorf("Pending sent the broker %d consumer info(s): a count the pull "+
				"already holds was asked of the broker", n)
		}
		if err != nil || got != want {
			t.Fatalf("Pending = (%d, %v), want (%d, nil)", got, err, want)
		}
	}
	// A RUN PART-FETCHED: two of the five delivered, and the second's own
	// metadata counts the three behind it.
	if got, err := cons.Fetch(t.Context(), 2, 0, 30*time.Second); err != nil || len(got) != 2 {
		t.Fatalf("Fetch(2) = (%d records, %v), want 2", len(got), err)
	}
	pending(3)
	// The rest, under a request that goes on standing for its wait.
	if got, err := cons.Fetch(t.Context(), 16, 0, 30*time.Second); err != nil || len(got) != 3 {
		t.Fatalf("Fetch(16) = (%d records, %v), want the 3 behind the first", len(got), err)
	}
	pending(0)
	// WHAT THE STANDING REQUEST DELIVERS AND NO FETCH HAS TAKEN is still to
	// come for the applier: held in the pull, it is counted.
	appendN(t, log, subject, 2)
	deadline = time.Now().Add(10 * time.Second)
	for {
		got, err := cons.Pending(context.Background())
		if err != nil {
			t.Fatalf("Pending: %v", err)
		}
		if got == 2 {
			break
		}
		if time.Until(deadline) <= 0 { //fixedwait:measure a poll's own deadline
			t.Fatalf("Pending = %d with two records delivered into the pull and "+
				"not taken, want 2", got)
		}
		// LIVENESS: paces a poll of the count, which decides; a pull that
		// never counts what it holds fails at the deadline above.
		time.Sleep(10 * time.Millisecond) //fixedwait:liveness
	}

	// AN APPLIER'S RUNS: a fetch and a count each, as nextRun asks them.
	for run := range 32 {
		appendN(t, log, subject, 3)
		got, err := cons.Fetch(t.Context(), 64, 0, 30*time.Second)
		if err != nil || len(got) == 0 {
			t.Fatalf("run %d: Fetch = (%d records, %v)", run, len(got), err)
		}
		for _, msg := range got {
			if err := msg.Ack(); err != nil {
				t.Fatalf("run %d: ack: %v", run, err)
			}
		}
		if _, err := cons.Pending(context.Background()); err != nil {
			t.Fatalf("run %d: Pending: %v", run, err)
		}
	}
	if n := held.Count(); n != 0 {
		t.Errorf("the applier's counts sent %d consumer infos to the broker, want none", n)
	}
}

// A PULL THAT HAS DELIVERED NOTHING COUNTS NOTHING, and says so rather than
// answering zero: zero is the answer that closes the applier's run as LEVEL
// with the log, which only a delivery's own count can say.
func TestAPendingCountBeforeAnyDeliveryIsUnknown(t *testing.T) {
	t.Parallel()
	ns, q := accountQueue(t, probeDomain())
	cons, err := q.DomainConsumer(t.Context(), probeDomain().Name, "node-a", 0)
	if err != nil {
		t.Fatalf("open the consumer: %v", err)
	}
	t.Cleanup(cons.Close)
	held := jsapitest.Hold(t, ns, jsapitest.ConsumerInfo(probeDomain().Name, cons.Name()), nats.Token(leaderReadsToken))
	if got, err := cons.Pending(context.Background()); err == nil {
		t.Errorf("Pending before any fetch = (%d, nil), want an error: nothing "+
			"has counted what follows", got)
	}
	// A PULL THAT STANDS AND HAS DELIVERED NOTHING counts nothing either.
	if got, err := cons.Fetch(t.Context(), 4, 0, 50*time.Millisecond); err != nil || len(got) != 0 {
		t.Fatalf("Fetch on an empty log = (%d records, %v), want none", len(got), err)
	}
	if got, err := cons.Pending(context.Background()); err == nil {
		t.Errorf("Pending after a fetch that delivered nothing = (%d, nil), want "+
			"an error: nothing has counted what follows", got)
	}
	if n := held.Count(); n != 0 {
		t.Errorf("an unknown count sent %d consumer infos to the broker, want none", n)
	}
}

// A GROUP'S ACKNOWLEDGEMENT FLOOR A MEMBER DROPS IS ASKED AGAIN.
//
// The trim's feed term reads it on the trim tick, which carries no deadline,
// and only the consumer's leader answers: one dropped request would wait out
// the client's five seconds and block the trim for the tick. A group never
// opened is still answered at once, and asked ONCE: with the read term an
// hour the hedge's own re-ask is out of reach, so a second request, counted
// where the broker receives them, is one asked after the answer came.
//
// Mutation: settle a not-found as the provisioning read-backs do, and it is
// asked after its answer; read it as silence, and it is asked to its bound.
func TestAGroupAckFloorAMemberDropsIsAskedAgain(t *testing.T) {
	t.Parallel()
	ns, q := accountQueue(t, probeDomain())
	log, err := q.DomainLog(t.Context(), probeDomain().Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	group, _, err := log.Group(t.Context(), "feed", 0)
	if err != nil {
		t.Fatalf("open the group: %v", err)
	}
	if err := group.Stop(); err != nil {
		t.Fatalf("stop the group: %v", err)
	}
	held := jsapitest.DropFirst(t, ns,
		jsapitest.ConsumerInfo(probeDomain().Name, domainGroupName(probeDomain().Name, "feed")), 1,
		nats.Token(leaderReadsToken))

	timing := q.provisioning()
	ctx, cancel := context.WithTimeout(t.Context(), 3*(timing.ReadTerm+timing.ReAsk))
	defer cancel()
	if _, exists, err := log.GroupAckFloor(ctx, "feed"); err != nil || !exists {
		t.Fatalf("GroupAckFloor behind one dropped read = (exists %v, %v): a read "+
			"the broker dropped once was not asked again", exists, err)
	}
	if got := held.Count(); got != 1 {
		t.Errorf("the broker dropped %d reads, want the 1 it was told to", got)
	}

	// THE READ TERM AN HOUR, set while nothing else uses the queue: the
	// first half is done, and its reads have returned.
	timing.ReadTerm = time.Hour
	q.timing = timing
	asked := apiRequests(t, q, jsapitest.ConsumerInfo(probeDomain().Name,
		domainGroupName(probeDomain().Name, "never")))
	if floor, exists, err := log.GroupAckFloor(t.Context(), "never"); err != nil || exists || floor != 0 {
		t.Errorf("a group never opened = (%d, %v, %v), want (0, false, nil)", floor, exists, err)
	}
	if n := len(asked()); n != 1 {
		t.Errorf("a group never opened was asked about %d times, want once: an answer "+
			"was asked again as if nobody had given it", n)
	}
}

// apiRequests listens for the requests q sends on the broker's API subjects
// matching subject from here on, and returns the subjects of those sent so
// far, in the order the broker read them.
//
// It listens on the queue's own connection, which the broker reads in order:
// a flush is answered only once the broker has handed this connection a copy
// of every request sent before it, so the result is final for those however
// late the runner delivers them.
func apiRequests(t *testing.T, q *Queue, subject string) func() []string {
	t.Helper()
	copies := make(chan *nats.Msg, 64)
	sub, err := q.nc.ChanSubscribe(subject, copies)
	if err != nil {
		t.Fatalf("subscribe to %s: %v", subject, err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := q.nc.Flush(); err != nil {
		t.Fatalf("flush the subscription to %s: %v", subject, err)
	}
	var seen []string
	return func() []string {
		t.Helper()
		if err := q.nc.Flush(); err != nil {
			t.Fatalf("flush the requests on %s: %v", subject, err)
		}
		for {
			select {
			case msg := <-copies:
				seen = append(seen, msg.Subject)
			default:
				if dropped, err := sub.Dropped(); err != nil || dropped > 0 {
					t.Fatalf("the requests on %s overflowed their listener (%d dropped, %v)",
						subject, dropped, err)
				}
				return slices.Clone(seen)
			}
		}
	}
}

// A SUBSCRIPTION LISTING'S PAGES A MEMBER DROPS ARE ASKED AGAIN FOR AS LONG
// AS ITS CALLER WAITS.
//
// Only the metadata leader answers either listing, and a member believing in
// a lost leader drops a page silently. A names page is asked again a read term
// and a re-ask after it was sent; a consumers page is given the metadata
// leader's four-second wait beside the read term, the client's whole default:
// a caller whose deadline outlasts that and a re-ask has it asked again, and a
// deadline-less one (the mailbox sweep) asks it once. The first and third
// cases, about the clock, run in a synctest bubble ([inBubble]) as the metadata
// leader ([listerOn]), reading each request's instant and deadline
// ([armedWaits]); the second stays on the broker ([jsapitest]).
func TestASubscriptionListingAMemberDropsIsAskedAgain(t *testing.T) {
	t.Parallel()
	// listing is a queue holding one subscription, and the stream it is on.
	listing := func(t *testing.T) (*server.Server, *Queue, string) {
		t.Helper()
		ns, q := accountQueue(t, probeDomain())
		if _, err := q.EnsureSubscription(t.Context(), "dropped.alpha", "grp"); err != nil {
			t.Fatalf("EnsureSubscription: %v", err)
		}
		stream, err := q.streamFor(t.Context(), "dropped.alpha")
		if err != nil {
			t.Fatal(err)
		}
		return ns, q, stream
	}
	want := queue.Subscription{Topic: "dropped.alpha", Group: "grp"}

	t.Run("a caller with a deadline past a consumers page and a re-ask", func(t *testing.T) {
		t.Parallel()
		_, q, stream := listing(t)
		names, consumers := jsapitest.StreamNames(), jsapitest.ConsumerList(stream)
		// A MEMBER THAT DROPS EACH PAGE'S FIRST REQUEST, held until its
		// context ends, in front of a leader that answers the next.
		// Mutation: re-ask a page a second late, give the consumers page the
		// read term alone, abandon a request at its term or hold the caller's
		// deadline to the client's default, and a request is asked at the
		// wrong instant or carries the wrong deadline.
		pages := map[string][]byte{names: streamNamesPage(t, stream), consumers: consumersPage(t, q, stream)}
		sent := map[string]*atomic.Int32{names: new(atomic.Int32), consumers: new(atomic.Int32)}
		var asked armedWaits
		lister := listerOn(q, func(ctx context.Context, subject string) (*nats.Msg, error) {
			page, ok := pages[subject]
			if !ok {
				return nil, fmt.Errorf("the listing asked %s, which is none of its pages", subject)
			}
			if !asked.note(ctx, subject) {
				return nil, fmt.Errorf("the page on %s was asked with no deadline", subject)
			}
			if sent[subject].Add(1) == 1 {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return &nats.Msg{Subject: subject, Data: page}, nil
		})
		// A DEADLINE PAST BOTH RE-ASKS: a names page is asked again a read
		// term and a re-ask after it was sent, and a consumers page the
		// metadata leader's four-second wait later besides.
		namesAgain := jsprovision.ReadTerm + jsprovision.ReAsk
		consumersAgain := jsprovision.ReadTerm + 4*time.Second + jsprovision.ReAsk
		budget := 3*(jsprovision.ReadTerm+jsprovision.ReAsk) + 4*time.Second
		inBubble(t, func(t *testing.T) {
			start := time.Now()
			ctx, cancel := context.WithTimeout(t.Context(), budget)
			defer cancel()
			got, err := lister.ListSubscriptions(ctx, "dropped.>")
			ended := time.Now()
			if err != nil {
				t.Fatalf("a listing whose pages a member dropped once failed rather than "+
					"asking again: %v", err)
			}
			if len(got) != 1 || got[0] != want {
				t.Errorf("listed %v, want [%v]", got, want)
			}
			// Exported fields, so a failure prints the durations as durations.
			type request struct {
				Page         string
				At, Deadline time.Duration
			}
			var requests []request
			for _, w := range asked.all() {
				requests = append(requests, request{w.what, w.at.Sub(start), w.deadline.Sub(start)})
			}
			wantRequests := []request{
				{names, 0, budget},
				{names, namesAgain, budget},
				{consumers, namesAgain, budget},
				{consumers, namesAgain + consumersAgain, budget},
			}
			if !slices.Equal(requests, wantRequests) {
				t.Errorf("the listing asked %+v, want %+v: each page asked again once its "+
					"term and a re-ask had passed, every request carrying the caller's "+
					"deadline", requests, wantRequests)
			}
			if took := ended.Sub(start); took != namesAgain+consumersAgain {
				t.Errorf("the listing ended %v in, want %v, when its consumers page was "+
					"answered", took, namesAgain+consumersAgain)
			}
		})
	})

	t.Run("no deadline: a names page dropped is asked again", func(t *testing.T) {
		t.Parallel()
		ns, q, _ := listing(t)
		names := jsapitest.DropFirst(t, ns, jsapitest.StreamNames(), 1, nats.Token(leaderReadsToken))
		got, err := q.ListSubscriptions(context.Background(), "dropped.>")
		if err != nil {
			t.Fatalf("a deadline-less listing whose names page the broker dropped once "+
				"failed rather than asking again: %v", err)
		}
		if len(got) != 1 || got[0] != want {
			t.Errorf("listed %v, want [%v]", got, want)
		}
		if n := names.Count(); n != 1 {
			t.Errorf("the broker held %d stream-name pages, want the 1 it dropped", n)
		}
	})

	t.Run("no deadline: a consumers page is asked once, inside the client's default", func(t *testing.T) {
		t.Parallel()
		_, q, stream := listing(t)
		names, consumers := jsapitest.StreamNames(), jsapitest.ConsumerList(stream)
		namesPage := streamNamesPage(t, stream)
		// THE CASE IS THE METADATA LEADER: the names page answered at once,
		// naming the subscription's stream, and its consumers page never.
		// Mutation: give a page more than the client's default (half a read
		// term past it, or the lookup ceiling), let a request outlive its
		// context, or read silence as anything but the broker unavailable.
		var asked armedWaits
		lister := listerOn(q, func(ctx context.Context, subject string) (*nats.Msg, error) {
			if !asked.note(ctx, subject) {
				return nil, fmt.Errorf("the page on %s was asked with no deadline", subject)
			}
			switch subject {
			case names:
				return &nats.Msg{Subject: subject, Data: namesPage}, nil
			case consumers:
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("the listing asked %s, which is none of its pages", subject)
		})
		bound := q.js.Options().DefaultTimeout
		inBubble(t, func(t *testing.T) {
			got, err := lister.ListSubscriptions(context.Background(), "dropped.>")
			ended := time.Now()
			if !errors.Is(err, queue.ErrUnavailable) {
				t.Fatalf("a deadline-less listing whose consumers page nobody answered = (%v, %v), "+
					"want the broker unavailable", got, err)
			}
			pages := asked.all()
			if len(pages) != 2 || pages[0].what != names || pages[1].what != consumers {
				t.Fatalf("the listing asked %v, want its names page and then its consumers page, "+
					"once each inside the client's %v: a consumers page's term is the metadata "+
					"leader's four-second wait beside the read term, which is the whole of that "+
					"default", pages, bound)
			}
			for _, p := range pages {
				if armed := p.deadline.Sub(p.at); armed != bound {
					t.Errorf("%v: given %v, want the client's %v default", p, armed, bound)
				}
			}
			if !ended.Equal(pages[1].deadline) {
				t.Errorf("the listing ended %v after its consumers page was asked, want at that "+
					"page's deadline, %v after", ended.Sub(pages[1].at), bound)
			}
		})
	})
}

// answeringJS is a JetStream client that ANSWERS the requests a listing puts
// on it ([jsapi.Meta]'s seam) with answer, and has no connection, so a request
// put anywhere else fails. A case can then be the metadata leader inside a
// synctest bubble, which no broker's reply can enter: the connection's reader
// hands it in from outside. It embeds the interface because a listing asks
// only Options and Conn of the client it is built over.
type answeringJS struct {
	jetstream.JetStream
	opts   jetstream.JetStreamOptions
	answer func(ctx context.Context, subject string) (*nats.Msg, error)
}

func (f *answeringJS) Options() jetstream.JetStreamOptions { return f.opts }

// Conn is no connection.
func (f *answeringJS) Conn() *nats.Conn { return nil }

// RequestWithContext is a request put on the client, which answer answers.
func (f *answeringJS) RequestWithContext(ctx context.Context, subject string, _ []byte) (*nats.Msg, error) {
	return f.answer(ctx, subject)
}

// listerOn is a queue over q's own connection — up, so a page nobody answers
// is the broker unavailable — whose listings put their pages on a client that
// answer answers ([answeringJS]).
func listerOn(q *Queue, answer func(ctx context.Context, subject string) (*nats.Msg, error)) *Queue {
	return &Queue{log: q.log, nc: q.nc, js: &answeringJS{opts: q.js.Options(), answer: answer}}
}

// streamNamesPage is the metadata leader's answer to a stream names listing
// that holds one stream, name.
func streamNamesPage(t *testing.T, name string) []byte {
	t.Helper()
	page, err := json.Marshal(server.JSApiStreamNamesResponse{
		ApiResponse: server.ApiResponse{Type: server.JSApiStreamNamesResponseType},
		ApiPaged:    server.ApiPaged{Total: 1, Limit: 1024},
		Streams:     []string{name},
	})
	if err != nil {
		t.Fatal(err)
	}
	return page
}

// consumersPage is the broker's own first page of stream's consumers, asked
// on q's connection before a bubble, so that a case inside one answers with
// what the metadata leader answered.
func consumersPage(t *testing.T, q *Queue, stream string) []byte {
	t.Helper()
	req, err := json.Marshal(server.JSApiConsumersRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// LIVENESS: the case's own broker answers a page at once; this only ends
	// a wait for one that never does.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	reply, err := q.nc.RequestWithContext(ctx, jsapitest.ConsumerList(stream), req)
	if err != nil {
		t.Fatalf("ask the broker for %s's consumers: %v", stream, err)
	}
	return reply.Data
}

// AN ACCOUNT LIMIT A MEMBER DROPS IS ASKED AGAIN.
//
// Only the metadata leader answers an account's JetStream info, and the state
// logs are sized from it at boot: one request a member dropped was held for
// the client's five seconds, and on a broker whose limit is the only one
// readable the sizing then had no ceiling to hold a log to.
func TestAnAccountLimitAMemberDropsIsAskedAgain(t *testing.T) {
	t.Parallel()
	ns, q := accountQueue(t, probeDomain())
	held := jsapitest.DropFirst(t, ns, jsapitest.AccountInfo(), 1, nats.Token(leaderReadsToken))

	timing := q.provisioning()
	ctx, cancel := context.WithTimeout(t.Context(), 3*(timing.ReadTerm+timing.ReAsk))
	defer cancel()
	if _, err := q.StreamBudget(ctx); err != nil {
		t.Fatalf("an account limit the broker dropped once was not asked again: %v", err)
	}
	if n := held.Count(); n != 1 {
		t.Errorf("the broker held %d account infos, want the 1 it dropped", n)
	}
}

// A GROUP'S ACKNOWLEDGEMENT FLOOR AND AN ACCOUNT LIMIT NOBODY ANSWERS, FOR A
// CALLER WITH NO DEADLINE (the trim tick; a boot sizing the state logs, which
// a data pod's startup window counts at five seconds), END AT THE CLIENT'S
// OWN DEFAULT: asked at none, two and four seconds, never stretched to the
// lookup ceiling of the reads asked again from the start ([askRead]).
//
// The read runs in a synctest bubble ([inBubble]) against the real broker:
// nobody answers, so every timer is the bubble's, and the three requests are
// counted where the broker holds them, behind a sentinel ([heldBefore]).
//
// Mutation: bound the read by the lookup ceiling, and it asks until the
// ceiling; bound it by a read term, and it gives up after one request.
func TestADeadlineLessReadNobodyAnswersEndsAtTheClientsDefault(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		subject string
		read    func(context.Context, *Queue, *DomainLog) error
	}{
		{"a group's acknowledgement floor",
			jsapitest.ConsumerInfo(probeDomain().Name, domainGroupName(probeDomain().Name, "feed")),
			func(ctx context.Context, _ *Queue, log *DomainLog) error {
				_, _, err := log.GroupAckFloor(ctx, "feed")
				return err
			}},
		{"an account limit", jsapitest.AccountInfo(),
			func(ctx context.Context, q *Queue, _ *DomainLog) error {
				_, err := q.budget(ctx, false)
				return err
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ns, q := accountQueue(t, probeDomain())
			// OPENED BEFORE THE BUBBLE: the broker answers the open, and
			// an answer the connection's reader hands to a request made in
			// a bubble is a use of the bubble from outside it.
			log, err := q.DomainLog(t.Context(), probeDomain().Name)
			if err != nil {
				t.Fatalf("open the log: %v", err)
			}
			bound := q.js.Options().DefaultTimeout
			held := jsapitest.Hold(t, ns, c.subject, nats.Token(leaderReadsToken))
			inBubble(t, func(t *testing.T) {
				start := time.Now()
				if err := c.read(context.Background(), q, log); err == nil {
					t.Fatal("a read nobody answered succeeded")
				}
				if took := time.Since(start); took != bound {
					t.Errorf("the read gave up after %v, want its client's %v default",
						took, bound)
				}
			})
			if n := heldBefore(t, q, held, c.subject); n != 3 {
				t.Errorf("the read was asked %d times inside its client's %v, want it "+
					"asked again inside it — at none, two and four seconds", n, bound)
			}
		})
	}
}

// heldBefore is how many reads held was handed before a sentinel q sends on
// subject now: every request q sent on it before, since the broker reads a
// connection in order and hands what it maps to the interception in that
// order, however late the runner delivers them.
func heldBefore(t *testing.T, q *Queue, held *jsapitest.Held, subject string) int {
	t.Helper()
	sentinel := q.nc.NewInbox()
	if err := q.nc.PublishRequest(subject, sentinel, nil); err != nil {
		t.Fatalf("send the sentinel on %s: %v", subject, err)
	}
	// LIVENESS: the sentinel reaches the interception once the reads before
	// it have; this only ends a wait for one that never does.
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	for {
		r, err := held.Next(ctx)
		if err != nil {
			t.Fatalf("the sentinel sent on %s never reached the interception: %v", subject, err)
		}
		if r.Reply() == sentinel {
			return r.N - 1
		}
	}
}

// A RECORD READ BY ITS POSITION ACROSS A LINK IS GIVEN ITS RECORD'S ARRIVAL,
// and a subject's newest, which a writer waits on, is not.
//
// Both replies carry a whole record, base64-encoded, as large as the log's
// largest, and across a link a re-ask sent while the first reply crosses puts
// the record on the link again, ahead of the node's lease renewals. So a read
// by position is presumed lost only once its largest reply could have arrived
// (crossed at the object store's floor and handled at the handling pace; for
// this log's mebibyte, past any deadline here) and is asked once. The
// last-sequence read is on every create's path and must be asked again in
// seconds after an election, so it keeps the read term.
func TestARecordReadAcrossALinkIsGivenItsRecordsArrival(t *testing.T) {
	t.Parallel()
	spec := probeDomain()
	spec.MaxMessageBytes = 1 << 20
	ns, q := accountQueue(t, spec)
	log, err := q.DomainLog(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	const subject = "crewlet.probe.log.task.a"
	appendN(t, log, subject, 1)
	if want := jsapi.Arrival(1<<20, objstore.MiBPace); log.arrival != want {
		t.Fatalf("the log across a link gives a record read %v beyond the read term, want "+
			"its largest message's arrival at the object store's floor, %v", log.arrival, want)
	}

	// Past two of the read term's re-asks, and well inside the crossing.
	timing := q.provisioning()
	wait := 2*(timing.ReadTerm+timing.ReAsk) + timing.ReadTerm/2
	for _, c := range []struct {
		name     string
		read     func(context.Context) error
		min, max int
	}{
		{"a record by its position", func(ctx context.Context) error {
			_, _, _, _, err := log.At(ctx, 1)
			return err
		}, 1, 1},
		// AT NONE, TWO AND FOUR SECONDS: the third half a second inside the
		// wait, which a timer the load delays may miss.
		{"a subject's newest record", func(ctx context.Context) error {
			_, _, err := log.LastSeq(ctx, subject)
			return err
		}, 2, 3},
	} {
		held := jsapitest.Hold(t, ns, jsapitest.MsgGet(spec.Name), nats.Token(leaderReadsToken))
		ctx, cancel := context.WithTimeout(t.Context(), wait)
		err := c.read(ctx)
		cancel()
		held.Release()
		if !jsprovision.Unanswered(context.Background(), err) {
			t.Errorf("%s nobody answered ended with %v, not as a read nobody answered", c.name, err)
		}
		if n := held.Count(); n < c.min || n > c.max {
			t.Errorf("%s across a link was asked %d times in %v, want %d to %d", c.name, n, wait,
				c.min, c.max)
		}
	}
}

// A QUEUE'S LINK IS READ OFF HOW ITS OWN CONNECTION WAS MADE, where the queue
// is built.
//
// TestOnlyAMembersOwnConnectionCrossesNoLink holds the reading (linkOf); this
// holds the one site that hands it to a queue, on real queues opened as a
// node opens them. A member paced as if across a link holds its reads and
// writes to crossings they never make; an unpaced leaf or external client
// holds a large write to the request term alone; a leaf taken for no leaf asks
// the leader for a reply its link closes on; an external client taken for a
// leaf reads through consumers what one request of the leader answers.
//
// Mutation: build a queue with any link but its connection's reading.
func TestAQueueIsPacedByHowItsOwnConnectionWasMade(t *testing.T) {
	t.Parallel()
	srv := externaltest.Start(t, queue.MaxPayloadBytes)
	for _, c := range []struct {
		name    string
		q       *Queue
		want    link
		metered bool // the connection's link carries its meter (a member's crosses none)
	}{
		{"a member's own client", openForTest(t, Config{}), link{}, false},
		{"a leaf's client", openLeafForTest(t, Config{}), link{pace: objstore.MiBPace, leaf: true}, true},
		{"a client of an external cluster", newQueueWith(t, Config{URL: srv.URL(),
			Token: externaltest.Token}), link{pace: objstore.MiBPace}, true},
	} {
		got := c.q.link
		if metered := got.meter != nil; metered != c.metered {
			t.Errorf("%s's link is metered %v, want %v", c.name, metered, c.metered)
		}
		// The link's window is its own too: one of the link's size wherever
		// it is metered, and compared apart, as the meter is.
		if windowed := got.window != nil; windowed != c.metered ||
			(windowed && got.window.Size() != jsapi.LinkWindow) {
			t.Errorf("%s's link window is %v, want one of %d bytes only across a link", c.name,
				got.window, jsapi.LinkWindow)
		}
		got.meter, got.window = nil, nil
		if got != c.want {
			t.Errorf("%s crosses %+v, want %+v", c.name, got, c.want)
		}
	}
}

// A MEMBER'S DEAD LETTERS ARE READ GIVEN THE HANDLING OF THE LARGEST REPLY.
//
// A member's own connection crosses no link, but a dead letter may be as large
// as the transport, and the leader's reply to a read of one (eleven megabytes,
// base64 in JSON) is built by the leader and decoded here. At the bare read
// term it is asked again at two and four seconds while the leader still builds
// the first reply under load, each re-ask another whole reply, and none is
// answered inside five seconds. So its term is the read term plus the
// handling of a reply at the transport's ceiling, at no pace.
//
// Mutation: read a member's dead letters through the plain leader, and its
// term is the bare read term.
func TestAMembersDeadLettersAreReadGivenTheHandlingOfTheLargestReply(t *testing.T) {
	t.Parallel()
	q := openForTest(t, Config{})
	letters, err := q.deadLetters(t.Context(), topics.DeadLetter("member.letters", "grp"))
	if err != nil {
		t.Fatalf("open the dead letters: %v", err)
	}
	if letters.copies != nil {
		t.Fatal("a member's own connection reads its dead letters through copies, where no leaf's " +
			"link stands between it and the leader")
	}
	want := jsprovision.ReadTerm + jsapi.Handling(int(q.nc.MaxPayload()))
	if got := letters.leader.Term(); got != want {
		t.Errorf("a member's dead letters are read at a term of %v, want the read term and the "+
			"handling of a reply as large as the transport carries, %v", got, want)
	}
}

// ONLY A CONNECTION TO THE NODE'S OWN MEMBER CROSSES NO LINK, AND ONLY A
// LEAF'S CROSSES A LEAF'S: the facts the engine hands the object store as its
// AcrossLink and the copy reads decide by, read off how the connection was
// made. A member's own connection alone is given no pace, and its record read
// the reply's handling and no crossing (its reply crosses at most a cluster
// route). A leaf's and an external cluster's client are paced at the object
// store's floor, the slowest a link may be, and carry their link's meter; only
// the leaf's has a server on its side holding what crosses to its max_payload,
// which no server holds a delivery to its own client to.
//
// Mutation: change any row's pace, leaf flag, meter or window, or give a
// member's record read no handling or a crossing, and that row goes red.
func TestOnlyAMembersOwnConnectionCrossesNoLink(t *testing.T) {
	t.Parallel()
	// THE METER IS THE LINK'S: a leaf's tunnel's, the external dial's own,
	// and none for a member, whatever a dial would have handed it.
	tunnelled, dialled := &LinkMeter{}, &LinkMeter{}
	for _, c := range []struct {
		name     string
		embedded *embeddedServer
		want     link
	}{
		{"a member's own client", &embeddedServer{}, link{}},
		{"a leaf's client", &embeddedServer{leaf: true, tunnel: &tunnel{meter: tunnelled}},
			link{pace: objstore.MiBPace, leaf: true, meter: tunnelled}},
		{"a client of an external cluster", nil, link{pace: objstore.MiBPace, meter: dialled}},
	} {
		got := linkOf(c.embedded, dialled)
		// ITS LINK'S ONE WINDOW, of the link's size, wherever it crosses
		// one: compared apart, since each link makes its own.
		if across := c.want.pace > 0; (got.window != nil) != across ||
			(across && got.window.Size() != jsapi.LinkWindow) {
			t.Errorf("%s: its link's window is %+v, want one of %d bytes only across a link",
				c.name, got.window, jsapi.LinkWindow)
		}
		got.window = nil
		if got != c.want {
			t.Errorf("%s: its connection crosses %+v, want %+v", c.name, got, c.want)
		}
	}

	// A MEMBER'S OWN LOG: its record read is given the handling of a reply of
	// its largest record, and no crossing.
	q := openForTest(t, Config{})
	spec := probeDomain()
	spec.MaxMessageBytes = 1 << 20
	if err := q.EnsureDomainStream(t.Context(), spec); err != nil {
		t.Fatalf("EnsureDomainStream: %v", err)
	}
	log, err := q.DomainLog(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	if want := jsapi.Handling(int(spec.MaxMessageBytes)); log.arrival != want {
		t.Errorf("a member's record read is given %v beyond the read term, want the handling of a reply "+
			"of its largest record and no crossing, %v", log.arrival, want)
	}
}
