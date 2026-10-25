package jetstream

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"

	"github.com/crewlet/crewlet/internal/jsapi/jsapitest"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/queue"
)

// accountQueue is a queue dialled to a broker of its own with JetStream and no
// domain — an external cluster's shape, read through the account's API — with
// one domain log provisioned, so [jsapitest] can stand between the queue's
// reads and the broker.
func accountQueue(t *testing.T, spec DomainStream) (*server.Server, *Queue) {
	t.Helper()
	ns, err := server.NewServer(&server.Options{
		Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true,
		JetStream: true, StoreDir: t.TempDir(),
		MaxPayload: int32(queue.MaxPayloadBytes),
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
	q, err := Open(t.Context(), Config{URL: ns.ClientURL()})
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
	held := jsapitest.Hold(t, ns, jsapitest.ConsumerInfo(probeDomain().Name, cons.Name()))
	const subject = "crewlet.probe.log.task.a"
	appendN(t, log, subject, 5)

	pending := func(want uint64) {
		t.Helper()
		start := time.Now()
		got, err := cons.Pending(context.Background())
		if took := time.Since(start); took > 100*time.Millisecond {
			t.Errorf("Pending took %v: a count the pull already holds was asked "+
				"of the broker", took)
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
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := cons.Pending(context.Background())
		if err != nil {
			t.Fatalf("Pending: %v", err)
		}
		if got == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Pending = %d with two records delivered into the pull and "+
				"not taken, want 2", got)
		}
		time.Sleep(10 * time.Millisecond)
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
	held := jsapitest.Hold(t, ns, jsapitest.ConsumerInfo(probeDomain().Name, cons.Name()))
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
func TestAGroupAckFloorAMemberDropsIsAskedAgain(t *testing.T) {
	t.Parallel()
	ns, q := accountQueue(t, probeDomain())
	log, err := q.DomainLog(t.Context(), probeDomain().Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	group, err := log.Group(t.Context(), "feed")
	if err != nil {
		t.Fatalf("open the group: %v", err)
	}
	if err := group.Stop(); err != nil {
		t.Fatalf("stop the group: %v", err)
	}
	held := jsapitest.DropFirst(t, ns,
		jsapitest.ConsumerInfo(probeDomain().Name, domainGroupName(probeDomain().Name, "feed")), 1)

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

	start := time.Now()
	if floor, exists, err := log.GroupAckFloor(t.Context(), "never"); err != nil || exists || floor != 0 {
		t.Errorf("a group never opened = (%d, %v, %v), want (0, false, nil)", floor, exists, err)
	}
	if took := time.Since(start); took > timing.ReadTerm {
		t.Errorf("a group never opened took %v to answer, past one read term: "+
			"an answer was asked again as if nobody had given it", took)
	}
}

// A SUBSCRIPTION LISTING WHOSE PAGES A MEMBER DROPS IS ASKED AGAIN.
//
// Both listings it reads — the stream names, and each stream's consumers —
// only the metadata leader answers, and a member that still believes in a
// leader the group has just lost drops a page without a word. The client's
// listers asked each page once and held it for the client's five seconds, and
// the mailbox sweep failed for the tick over a reply nobody would send. Each
// interception drops exactly its own first request, so neither listing's
// re-ask is the other's.
func TestASubscriptionListingAMemberDropsIsAskedAgain(t *testing.T) {
	t.Parallel()
	ns, q := accountQueue(t, probeDomain())
	if _, err := q.EnsureSubscription(t.Context(), "dropped.alpha", "grp"); err != nil {
		t.Fatalf("EnsureSubscription: %v", err)
	}
	stream, err := q.streamFor(t.Context(), "dropped.alpha")
	if err != nil {
		t.Fatal(err)
	}
	names := jsapitest.DropFirst(t, ns, jsapitest.StreamNames(), 1)
	consumers := jsapitest.DropFirst(t, ns, jsapitest.ConsumerList(stream), 1)

	// A dropped names page costs a term and a re-ask; a dropped consumers
	// page the four seconds the metadata leader waits for a consumer's own
	// leaders besides.
	timing := q.provisioning()
	ctx, cancel := context.WithTimeout(t.Context(), 3*(timing.ReadTerm+timing.ReAsk)+4*time.Second)
	defer cancel()
	got, err := q.ListSubscriptions(ctx, "dropped.>")
	if err != nil {
		t.Fatalf("a listing whose pages the broker dropped once failed rather than "+
			"asking again: %v", err)
	}
	if want := (queue.Subscription{Topic: "dropped.alpha", Group: "grp"}); len(got) != 1 || got[0] != want {
		t.Errorf("listed %v, want [%v]", got, want)
	}
	if n, m := names.Count(), consumers.Count(); n != 1 || m != 1 {
		t.Errorf("the broker held %d stream-name and %d consumer pages, want the 1 "+
			"of each it dropped", n, m)
	}
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
	held := jsapitest.DropFirst(t, ns, jsapitest.AccountInfo(), 1)

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
func TestADeadlineLessReadNobodyAnswersEndsAtTheClientsDefault(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		subject string
		read    func(context.Context, *Queue) error
	}{
		{"a group's acknowledgement floor",
			jsapitest.ConsumerInfo(probeDomain().Name, domainGroupName(probeDomain().Name, "feed")),
			func(ctx context.Context, q *Queue) error {
				log, err := q.DomainLog(t.Context(), probeDomain().Name)
				if err != nil {
					return err
				}
				_, _, err = log.GroupAckFloor(ctx, "feed")
				return err
			}},
		{"an account limit", jsapitest.AccountInfo(),
			func(ctx context.Context, q *Queue) error {
				_, err := q.budget(ctx, false)
				return err
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ns, q := accountQueue(t, probeDomain())
			bound := q.js.Options().DefaultTimeout
			held := jsapitest.Hold(t, ns, c.subject)
			done := make(chan error, 1)
			start := time.Now()
			go func() { done <- c.read(context.Background(), q) }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("a read nobody answered succeeded")
				}
				if took := time.Since(start); took < bound {
					t.Errorf("the read gave up after %v, inside its client's %v", took, bound)
				}
			case <-time.After(jsprovision.LookupBudget / 2):
				t.Fatalf("a read nobody answers was still being asked %v later, past its "+
					"client's %v default (%d requests)", jsprovision.LookupBudget/2, bound,
					held.Count())
			}
			// AT NONE, TWO AND FOUR SECONDS: the third a second inside the
			// bound, which a timer the load delays may miss, so it is the
			// two the bound must hold that are asked for.
			if n := held.Count(); n < 2 || n > 3 {
				t.Errorf("the read was asked %d times inside its client's %v, want it "+
					"asked again inside it — at none, two and four seconds", n, bound)
			}
		})
	}
}
