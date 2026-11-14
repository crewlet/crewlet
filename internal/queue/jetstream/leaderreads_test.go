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
// The applier asks it on every run it holds records for, so at least once a
// committed run, per domain, per data node — and it was a consumer info, which
// only the consumer's LEADER answers, the server serves only once its main API
// queue is empty, and a member that still believes in a leader the group has
// just lost drops without a word: a request on the commit path, its wait added
// to every linearizable read whose barrier is in the run. Every record the
// pull delivers already carries the count in its own metadata, so the answer
// is what the pull holds — the newest delivery's count of what follows it, and
// what it delivered that no fetch has taken — and a broker that never answers
// a consumer info changes nothing.
//
// NOTHING ASKED IS READ OFF THE BROKER, never the clock: every consumer info
// is held where it would be answered, and each count is followed by a look at
// how many were. It used to be each count held to a tenth of a second by the
// test's clock, which a runner that lost that much in a count failed.
//
// Mutation: answer the count with a consumer info, as it was, and the broker
// holds the request the count sent.
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
	// THE CONSUMER'S OWN COUNT OF THE FIVE, before the fetch whose second
	// record carries it. The server acknowledges an append before it tells
	// the stream's consumers of it, from a queue of its own, so a fetch made
	// as the appends return can be served from a count still short of them,
	// and a delivery's count is what it was when sent: Pending is a hint, and
	// the case holds it exact. So the case waits for the consumer to have
	// counted all five — asking its info itself, before the info is held —
	// and fetches then.
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
// The trim's feed term reads it on the trim tick, whose lifetime carries no
// deadline; only the consumer's leader answers it, so one request a member
// dropped waited out the client's five seconds and left the term unreadable —
// a trim blocked for the tick over a group that was there all along. And a
// group never opened is still answered at once as never opened: the hedge
// asks again only while nobody answers.
//
// # At once is asked once
//
// So the read of a group never opened is held to ONE request, counted where
// the broker receives them, with the read term an HOUR: the hedge's own re-ask
// is then out of reach whatever the load, and a second request is one asked
// after the answer came. It used to be held to one read term by the test's
// clock, which a runner that lost a second around the read failed.
//
// Mutation: settle a not-found as the provisioning read-backs do, and the read
// asks again after its answer; read a not-found as silence, and it asks until
// its bound and fails.
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
// far, in the order the broker read them — every request q sent before the
// call, and none after.
//
// ON THE SUBJECT THE BROKER'S JETSTREAM SERVES and ON THE QUEUE'S OWN
// CONNECTION, whose protocol the broker reads in order: the subscription is in
// place before any request after it arrives, and a flush of the connection is
// answered only once the broker has handed this connection a copy of every
// request sent before it — so what it returns is final for those, however
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
// AS ITS CALLER WAITS — and a caller that sets no deadline waits the client's
// five seconds on a consumers page, as it did when the page was one request.
//
// Both listings it reads — the stream names, and each stream's consumers —
// only the metadata leader answers, and a member that still believes in a
// leader the group has just lost drops a page without a word. A names page is
// presumed lost after the read term and asked again two seconds after it was
// sent, whoever the caller. A consumers page is given the four seconds the
// metadata leader waits for the consumers' own leaders beside the read term,
// five seconds, which is the client's whole default: a caller whose deadline
// outlasts that and a re-ask has the page asked again, and one with none — the
// mailbox sweep, on the maintenance worker's lifetime — asks it once and fails
// its tick when a member drops it. Each interception is of its own listing's
// subject, so neither listing's re-ask is the other's.
//
// # Two of the cases in a bubble
//
// The first case's claim and the third's are about the clock — when a dropped
// page is asked again, and what deadline each request carries — so both run
// IN A SYNCTEST BUBBLE ([inBubble]), with the case as the metadata leader on
// the client the listing is built over ([listerOn], through [jsapi.Meta]'s
// seam), and each request read off the instant it was asked and the deadline
// it carried ([armedWaits]), exactly:
//
//   - The first drops each page's first request, held until its context
//     ends, and answers the next with the page. The names page is asked at
//     none and two seconds and the consumers page at two and eight, every
//     request carries the caller's deadline, and the listing ends at the
//     answer.
//   - The third answers the names page at once and the consumers page never.
//     Each page is given exactly the client's default, the consumers page is
//     asked once, and the listing ends on its deadline.
//
// Each used to read the machine's clock. The first held the whole listing to
// a ten-second context against eight seconds of cadence, so a runner that
// lost two and a half seconds between the pages failed it, and a re-ask a
// second late passed it. The third timed the listing and held it between the
// default and a read term past it, so a runner that lost a second and a half
// between the pages failed it, and a page held half a second past the default
// passed it. Neither could move into a bubble before the seam: the broker
// answers the names page, its connection's reader hands each reply in from
// outside the bubble, and the consumers page went out on that raw connection,
// where no fake client saw it. The second case stays on the broker, which
// drops a page where every reader's request goes, whatever the reader was
// built with ([jsapitest]).
//
// Mutation: re-ask a page a second late, or give the consumers page the read
// term alone, and a page is asked again at the wrong instant; abandon each
// request at its term, or hold a caller's deadline to the client's default,
// and a request carries the wrong deadline; give the consumers page half a
// read term past the client's default, or every page the lookup ceiling, and
// a deadline-less page is given more than the default; let a request outlive
// its context, and the listing ends past the page's deadline; put the pages
// on the client's connection whatever the client, and neither bubbled case
// hears one; read a page nobody answered as anything but silence, and the
// listing is not the broker unavailable; never ask a page again, and the
// first two cases' listings fail.
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
		// context ends, in front of a leader that answers the next with the
		// page: the consumers page as the broker gave it.
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
			// Each request as the case received it: its page, and the
			// instant it was asked and the deadline it carried, from the
			// call. Exported fields, so a failure prints the durations as
			// durations.
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
// on it itself ([jsapi.Meta]'s seam), with whatever answer returns, and has no
// connection: a request put anywhere but on the client reaches nothing, and
// fails. So a case can be the metadata leader inside a synctest bubble, where
// no broker's reply can reach a request — the connection's reader would hand
// it in from outside the bubble.
//
// EMBEDS THE INTERFACE, as this package's other fakes do: a listing asks only
// Options and Conn of the client it is built over.
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
// CALLER WITH NO DEADLINE, END AT THE CLIENT'S OWN DEFAULT — asked again
// inside it, and never stretched to the lookup ceiling the reads asked again
// from the start are held to ([askRead]).
//
// Each was one request the client held for its five seconds where the caller
// set none: the trim tick reads the floor on its lifetime, and a boot sizes
// the state logs from the account's limit — a data pod's startup window
// counts that read at five seconds. Hedged, each asks again inside the bound,
// at none, two and four seconds, and a broker that never answers costs its
// caller no more than it did.
//
// # The read in a bubble, the broker outside it
//
// The read runs IN A SYNCTEST BUBBLE ([inBubble]) against the real broker:
// nobody answers it, so nothing the connection's own reader hands over ever
// reaches a request of the bubble, and the read's every timer is the bubble's.
// So it ends at the client's default exactly, and asks at none, two and four
// seconds exactly — three requests, counted where the broker holds them,
// behind a sentinel the queue sends after the read. On the machine's clock the
// end was held only from below and the asking to two or three, a timer the
// load delayed past the bound missing the third, and a read still asking at
// fifteen seconds was what told one held to the ceiling apart.
//
// Mutation: bound the read by the lookup ceiling, as the reads asked again
// from the start are, and it asks until the ceiling; bound it by a read term,
// and it gives up after one request.
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
// largest: across a link — here a queue dialled to a broker of its own, an
// external cluster's shape — a re-ask sent while the first reply is still
// crossing puts the whole record on the link again, ahead of the node's lease
// renewals, every two seconds until the first lands. So a read by position
// is presumed lost only once its log's largest reply could have arrived —
// crossed at the object store's floor, and been handled at the handling pace
// — for this log's mebibyte, longer than any caller's deadline here — and is
// asked once. The last-sequence read is the
// write path's: every create asks it, and a request a member dropped across
// an election has to be asked again in seconds, so it keeps the read term.
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
// The reading itself is held below (linkOf); this holds the one site that
// hands it to a queue, on real queues: a member's own client, a leaf's, and a
// client of an external cluster, each opened as a node opens one. A member
// paced as though it crossed a link would hold its reads and writes to
// crossings they never make; a leaf or an external client left unpaced would
// hold a large write to the request term alone; a leaf taken for no leaf's
// link would ask the leader for a reply its link closes on; and an external
// client taken for a leaf's would read through consumers what one request of
// the leader answers.
//
// Mutation: build a queue with any link but its connection's reading, and its
// row goes red.
func TestAQueueIsPacedByHowItsOwnConnectionWasMade(t *testing.T) {
	t.Parallel()
	srv := externaltest.Start(t, queue.MaxPayloadBytes)
	for _, c := range []struct {
		name string
		q    *Queue
		want link
	}{
		{"a member's own client", openForTest(t, Config{}), link{}},
		{"a leaf's client", openLeafForTest(t, Config{}), link{pace: objstore.MiBPace, leaf: true}},
		{"a client of an external cluster", newQueueWith(t, Config{URL: srv.URL(),
			Token: externaltest.Token}), link{pace: objstore.MiBPace}},
	} {
		if c.q.link != c.want {
			t.Errorf("%s crosses %+v, want %+v", c.name, c.q.link, c.want)
		}
	}
}

// A MEMBER'S DEAD LETTERS ARE READ GIVEN THE HANDLING OF THE LARGEST REPLY.
//
// A member's own connection crosses no link, but a dead letter may be as large
// as the transport, and the leader's reply to a read of one — eleven
// megabytes, the entry base64-encoded in JSON — is built by the leader and
// decoded by this process. Read at the bare read term, such a read was asked
// again at two seconds and at four while the leader was still building the
// first reply under load, every re-ask another whole reply, and ran out its
// five seconds with none of them answered (crewlet/crewlet#1153). So a
// member's dead letters are read through a leader carrying the transport's
// ceiling at no pace: its term is the read term and that reply's handling.
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
// LEAF'S CROSSES A LEAF'S — the facts the engine hands the object store as
// its AcrossLink, and the copy reads decide by, read here off how the queue's
// connection was made. A member's own connection alone is given no pace: a
// member's record read is given its reply's handling and no crossing, its
// reply crossing at most a cluster route. A leaf's and an external cluster's
// client are paced at the
// object store's floor, the slowest a link is allowed to be; and only the
// leaf's has a server on its side of the link that holds what crosses it to
// its max_payload, which no server holds a delivery to its own client to.
//
// Mutation: pace a member's own connection, leave a leaf's or an external
// cluster's unpaced, take an external dial for a leaf's link or a leaf's for
// none, and its row goes red.
func TestOnlyAMembersOwnConnectionCrossesNoLink(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		embedded *embeddedServer
		want     link
	}{
		{"a member's own client", &embeddedServer{}, link{}},
		{"a leaf's client", &embeddedServer{leaf: true}, link{pace: objstore.MiBPace, leaf: true}},
		{"a client of an external cluster", nil, link{pace: objstore.MiBPace}},
	} {
		if got := linkOf(c.embedded); got != c.want {
			t.Errorf("%s: its connection crosses %+v, want %+v", c.name, got, c.want)
		}
	}
}
