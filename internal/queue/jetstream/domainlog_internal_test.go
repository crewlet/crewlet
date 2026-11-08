package jetstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsprovision"
)

// notYetVisibleJS answers `Stream` with "not found" for the first n calls and
// then delegates, which is the clustered window this file's fix exists for: a
// create committed by the metadata leader that the member which made it cannot
// see yet.
//
// EMBEDS THE INTERFACE rather than implementing it, because the one method
// under test is one of dozens and a hand-written stub would be a list of
// panics that has to be maintained against the vendored client.
type notYetVisibleJS struct {
	jetstream.JetStream
	notFound atomic.Int32
	calls    atomic.Int32
}

func (f *notYetVisibleJS) Stream(context.Context, string) (jetstream.Stream, error) {
	f.calls.Add(1)
	if f.notFound.Add(-1) >= 0 {
		return nil, jetstream.ErrStreamNotFound
	}
	// A NIL STREAM WITH A NIL ERROR is enough: nothing in openProvisioned
	// touches the handle, and building a real one would need a broker to
	// test a decision that has none in it.
	return nil, nil
}

// shortReadBack is the production timing with a read-back window short enough
// for a case to wait out, and a placement cadence to match: what the cases
// below discriminate on is how many looks the window held and what the last
// one said, which is the same at a tenth of a second as at five.
func shortReadBack() jsprovision.Timing {
	t := jsprovision.Clustered(false).Timing()
	t.ReadBack, t.PlacementRetry = 100*time.Millisecond, 5*time.Millisecond
	return t
}

// A CREATE THAT HAS NOT PROPAGATED YET IS WAITED OUT, not reported.
//
// This is the failure it was found by: a clustered boot died with `open the
// log "CREWLET_TRACKER_VECTORS": stream not found` on the node whose own
// create of that stream had just returned success. The lookup was the one step
// on the provisioning path with no tolerance for a peer-visible-but-not-yet-
// local create, and a state log that cannot open the stream it just made takes
// the whole engine down with it.
//
// IN A SYNCTEST BUBBLE ([inBubble]): four looks at the placement cadence take
// fifteen milliseconds of the window's hundred, which is the claim; on the
// machine's clock a loaded race-detector run delayed the looks' timers past
// the window, and the case failed on the scheduler's latency.
func TestALookupWaitsOutACreateThatHasNotPropagated(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		js := &notYetVisibleJS{}
		js.notFound.Store(3)
		q := &Queue{js: js, timing: shortReadBack()}

		if _, err := q.openProvisioned(t.Context(), "CREWLET_TRACKER_VECTORS"); err != nil {
			t.Fatalf("a stream that appeared on the fourth look was reported as %v", err)
		}
		if got := js.calls.Load(); got != 4 {
			t.Errorf("the lookup was made %d times, want 4 — three not-founds "+
				"waited out and the one that answered", got)
		}
	})
}

// inBubble runs f in a synctest bubble, where a timer fires at exactly its
// time and time moves only once every goroutine of the case is blocked.
//
// FOR THE CASES THAT HOLD A TIMER-DRIVEN READ TO A WINDOW: what each claims is
// how many requests the read's own cadence fits inside the caller's budget —
// two lost requests, their re-asks and the answer inside four terms; a re-ask
// sent before a late answer arrives — and on the machine's clock that is also
// a claim about how late a loaded race-detector run wakes a timer's goroutine.
// Measured so, beside build-and-vet loops on four cores: the never-answered
// case failed 6 runs in 90 at a load average of 45 to 47, its 200 ms budget
// holding 110 ms of cadence, and the propagation case 1 in 300 at 25 to 30,
// its four looks inside a 100 ms window. In the bubble the budgets are
// unchanged and exact, so a case fails on the read's cadence and on nothing
// else.
func inBubble(t *testing.T, f func(*testing.T)) {
	t.Helper()
	synctest.Test(t, f)
}

// armedWaits records each wait a fake was handed: what it waited on, the
// instant it was armed and the deadline it was armed with. It reads no clock
// but the one the case runs on — a bubbled case's, where the deadline less the
// instant is exactly what the wait was handed.
type armedWaits struct {
	mu    sync.Mutex
	waits []armedWait
}

// armedWait is one wait an [armedWaits] recorded.
type armedWait struct {
	what         string
	at, deadline time.Time
}

func (w armedWait) String() string {
	const instant = "15:04:05.999999999"
	return fmt.Sprintf("%s armed at %s to end at %s", w.what, w.at.Format(instant),
		w.deadline.Format(instant))
}

// note records a wait on ctx for what, and reports whether ctx carried a
// deadline; one with none is not recorded.
func (w *armedWaits) note(ctx context.Context, what string) bool {
	deadline, ok := ctx.Deadline()
	if !ok {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.waits = append(w.waits, armedWait{what: what, at: time.Now(), deadline: deadline})
	return true
}

func (w *armedWaits) all() []armedWait {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.waits)
}

// AND A STREAM THAT REALLY IS GONE IS STILL REPORTED — with its own error,
// not the deadline's.
//
// The tolerance above is bounded for exactly this: if "not found" were waited
// out for ever, a genuinely missing stream would hang a boot instead of naming
// itself, which is the trade openBucket's placement retry documents in the
// other direction.
func TestAMissingStreamIsStillReportedAsMissing(t *testing.T) {
	t.Parallel()
	js := &notYetVisibleJS{}
	// More not-founds than the budget can hold at the retry cadence, so the
	// wait runs out rather than succeeding late — at [shortReadBack]'s
	// window, which is the half this case waits out.
	js.notFound.Store(1 << 30)
	q := &Queue{js: js, timing: shortReadBack()}

	_, err := q.openProvisioned(t.Context(), "CREWLET_NOTHING")
	if !errors.Is(err, jetstream.ErrStreamNotFound) {
		t.Fatalf("a missing stream reported %v, want the broker's own "+
			"not-found rather than a bare deadline", err)
	}
}

// A CANCELLED CALLER IS NOT HELD for the whole tolerance. The retry shortens
// against the parent like every other budget on this path, so a boot somebody
// interrupted gives the prompt back rather than finishing its wait.
func TestACancelledLookupReturnsAtOnce(t *testing.T) {
	t.Parallel()
	js := &notYetVisibleJS{}
	js.notFound.Store(1 << 30)
	q := &Queue{js: js}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := q.openProvisioned(ctx, "CREWLET_NOTHING"); err == nil {
		t.Fatal("a cancelled lookup reported success")
	}
	if got := js.calls.Load(); got > 1 {
		t.Errorf("a cancelled lookup made %d calls, want at most the first", got)
	}
}

// deadlineJS is a metadata group that HOLDS every create it is asked — no
// reply, so each request ends when its own term does, as on a cluster whose
// group has not settled — and records the wait each one was handed.
type deadlineJS struct {
	jetstream.JetStream
	creates armedWaits
	// asked counts every create, one with no deadline included, and holds
	// is how many the case's budget can hold: past it a create is ANSWERED,
	// so one asked for ever — a create no budget bounds — ends.
	asked, holds int
	undeadlined  bool
}

func (f *deadlineJS) CreateConsumer(ctx context.Context, _ string,
	_ jetstream.ConsumerConfig) (jetstream.Consumer, error) {

	f.asked++
	if !f.creates.note(ctx, "create") {
		f.undeadlined = true
		return nil, errors.New("a create with no deadline, refused rather than held for ever")
	}
	if f.asked > f.holds {
		return nil, errors.New("a create asked more often than its budget holds, refused")
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// Consumer is the read-back ensureDurableConsumer makes when a create fails,
// and it refuses too: this case is about the DEADLINES the creates carried,
// and a read-back that answered would only add a second path to reason about.
func (f *deadlineJS) Consumer(context.Context, string, string) (jetstream.Consumer, error) {
	return nil, errors.New("not there either")
}

// A DURABLE CONSUMER GETS THE PROVISIONING BUDGET, like every other replicated
// create on this path.
//
// It did not. Nothing here set a deadline, so the caller's context reached
// nats.go with none of its own — an engine boot's has none — and the client's
// FIVE-SECOND default applied instead. That is a twenty-fourth of the clustered
// budget, and shorter than [jsprovision.SlowAfter], so the slow-create
// breadcrumb beside this call could never fire and the retry window it
// describes did not exist.
//
// # Held, in a bubble, and read off the waits it was handed
//
// The create is HELD, as a group that has not settled holds it, so it is
// asked again until its budget is spent; and the case runs IN A SYNCTEST
// BUBBLE ([inBubble]), where the two minutes that takes are the bubble's and
// each request's deadline less the instant it was armed is exactly the term it
// was handed. So the budget is the instant the last request was armed to end
// at, held to the clustered budget from the call, and every request's term to
// the clustered ask term or what the budget had left; and the breadcrumb is
// seen to fire. It used to be one request, answered at once, whose time left
// was read off the machine's clock once the call had returned and held above
// the breadcrumb's threshold: a runner that lost five seconds after the create
// failed it, and a create on a budget of a single term, or on the solo budget,
// passed it.
//
// Mutation: create on the caller's context with no budget of its own, and the
// create is asked past the clustered budget; give the budget a single term, or
// read the topology as solo, and the asking ends early.
func TestADurableConsumerCreateCarriesTheProvisioningBudget(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		// THE CLUSTERED TIMING AS PRODUCTION HAS IT, which is what the
		// waits are held to — never the queue's own, which a queue that
		// read its topology wrongly would answer for the wrong topology.
		clustered := jsprovision.Clustered(true).Timing()
		// AT MOST ONE REQUEST A RE-ASK FITS IN THE BUDGET, whatever the
		// term: a create asked more often than that is one no budget bounds.
		js := &deadlineJS{holds: int(clustered.Budget/clustered.ReAsk) + 1}
		var logged bytes.Buffer
		// A NAMED CLUSTER is what selects the clustered budget — Replicas
		// does not, and a test that set only it would exercise the solo
		// path while claiming to cover this one. See [Queue.Clustered].
		q := &Queue{js: js, cfg: Config{ClusterName: "crewlet-test"},
			log: slog.New(slog.NewTextHandler(&logged, nil))}

		// A CALLER WITH NO DEADLINE OF ITS OWN, which is what a boot
		// passes.
		began := time.Now()
		_, _, _ = q.ensureDurableConsumer(t.Context(), "CREWLET_AGENT",
			jetstream.ConsumerConfig{Durable: "agent-ceo"})

		if js.undeadlined {
			t.Fatal("the create ran with no deadline, so nats.go's 5s default " +
				"applies and neither the budget nor the breadcrumb exists")
		}
		creates := js.creates.all()
		if len(creates) == 0 {
			t.Fatal("the create was never asked")
		}
		// THE BUDGET: the creates are asked until the clustered budget
		// from the call is spent, and no further.
		budgetEnds := began.Add(clustered.Budget)
		if last := creates[len(creates)-1]; !last.deadline.Equal(budgetEnds) {
			t.Errorf("the create was asked until %v after the call (%d requests), want "+
				"the %v clustered budget exactly", last.deadline.Sub(began), len(creates),
				clustered.Budget)
		}
		// AND EACH REQUEST ITS TERM: the clustered ask term, or what the
		// budget had left when it was asked.
		for i, c := range creates {
			want := min(clustered.AskTerm, budgetEnds.Sub(c.at))
			if got := c.deadline.Sub(c.at); got != want {
				t.Errorf("create %d, asked %v after the call, was handed %v, want %v — "+
					"the clustered %v ask term, or what the budget had left",
					i+1, c.at.Sub(began), got, want, clustered.AskTerm)
			}
		}
		// The breadcrumb the client's default never let fire.
		if line := logLine(t, &logged, "jetstream_consumer_slow"); !strings.Contains(line,
			"waited="+jsprovision.SlowAfter.String()) {
			t.Errorf("the slow-create breadcrumb fired at another threshold than %v: %s",
				jsprovision.SlowAfter, line)
		}
	})
}

// racedJS is a broker on which THIS node's create loses: the create reports
// the consumer already there, and the read-back finds it.
type racedJS struct {
	jetstream.JetStream
	created int
}

func (f *racedJS) CreateConsumer(context.Context, string,
	jetstream.ConsumerConfig) (jetstream.Consumer, error) {

	f.created++
	return nil, jetstream.ErrConsumerExists
}

// The read-back answers, which is the whole point: the consumer exists,
// somebody else made it, and the boot must carry on.
func (f *racedJS) Consumer(context.Context, string, string) (jetstream.Consumer, error) {
	return nil, nil
}

// A CONSUMER THE READ-BACK RECOVERED WAS NOT CREATED BY THIS CALL.
//
// # Why the bool is worth being exact about
//
// [queue.EventQueue] says EnsureSubscription "reports whether this call
// created it", and the read-back is the one path that can answer wrongly: it
// exists precisely because somebody ELSE's create is what put the consumer
// there. Derived from the preceding lookup alone, every member of a fleet
// booting together reported that it had made every mailbox — the lookup says
// not-found on all of them, and the recovery is invisible to it.
//
// Nothing branches on the bool today; it is a log line and a contract the
// conformance suite holds both backends to. That is the reason to keep it
// honest rather than to let it drift: a count that is wrong on every node is
// worse than no count, because it reads exactly like a correct one.
func TestAConsumerFoundByTheReadBackIsNotReportedAsCreated(t *testing.T) {
	t.Parallel()
	js := &racedJS{}
	q := &Queue{js: js, cfg: Config{ClusterName: "crewlet-test"}, log: slog.Default()}

	cons, won, err := q.ensureDurableConsumer(t.Context(), "CREWLET_AGENT",
		jetstream.ConsumerConfig{Durable: "agent-ceo"})
	if err != nil {
		t.Fatalf("a consumer a peer created failed the boot: %v", err)
	}
	if cons != nil {
		t.Errorf("the fake returns a nil consumer; got %v", cons)
	}
	if won {
		t.Error("a consumer recovered by the read-back was reported as created " +
			"by this call — on a fleet booting together every member claims to " +
			"have made every mailbox")
	}
	if js.created != 1 {
		t.Errorf("the create ran %d times, want exactly one attempt", js.created)
	}
}

// heldCreateJS is the shape a clustered boot actually produces: the create is
// HELD by the server while the metadata group settles, outlives its deadline,
// and comes back a timeout — with the consumer there all the same.
type heldCreateJS struct {
	jetstream.JetStream
	lookups atomic.Int32
	creates atomic.Int32
}

func (f *heldCreateJS) Consumer(context.Context, string, string) (jetstream.Consumer, error) {
	if f.lookups.Add(1) == 1 {
		// The propagation window: this node made it on an earlier boot
		// and has not been told about it yet.
		return nil, jetstream.ErrConsumerNotFound
	}
	return foundConsumer{}, nil
}

// CreateConsumer is HELD: no reply comes, and the request ends when its own
// term does — the deadline the caller set, never one the server chose.
func (f *heldCreateJS) CreateConsumer(ctx context.Context, _ string,
	_ jetstream.ConsumerConfig) (jetstream.Consumer, error) {

	f.creates.Add(1)
	<-ctx.Done()
	return nil, ctx.Err()
}

// foundConsumer is the consumer the read-back finds: its state AGREES with a
// checkpoint of zero, so it is kept rather than rebuilt.
type foundConsumer struct {
	jetstream.Consumer
}

func (foundConsumer) CachedInfo() *jetstream.ConsumerInfo { return nil }

func (foundConsumer) Info(context.Context) (*jetstream.ConsumerInfo, error) {
	return &jetstream.ConsumerInfo{}, nil
}

// A HELD CREATE THAT TIMED OUT IS READ BACK, not just an explicit
// already-exists.
//
// # Why the tidy error is the one that matters least
//
// The state-log consumer's name carries the node id, so no peer races it —
// but this node's OWN create meets the consumer it made on an earlier boot,
// and the create announces that in two shapes. ErrConsumerExists is the tidy
// one and needs the configs to differ. The other is a TIMEOUT: the server
// holds the request while the metadata group settles and the deadline runs
// out underneath it, with the consumer perfectly well placed.
//
// That second shape is what a clustered boot produces, and gating the
// read-back on ErrConsumerExists alone left it reporting the timeout — a boot
// failing over a consumer that was there, which is the failure this whole
// change exists to remove and the one [Queue.ensureDurableConsumer] has always
// handled for mailbox consumers.
//
// # Why the budget is a third of a second
//
// The read-back runs only once the create's WHOLE budget is spent: a held
// create is an unanswered request, so [jsprovision.Timing.Place] asks it again
// until the budget ends, and only then is the question handed to the read-back.
// At the clustered budget that is two minutes of a case doing nothing, and
// handing the open a caller's deadline instead would not reach the branch
// honestly — the read-back runs on that same context and would find it spent.
// So the queue's own timing is shortened, keeping the one relation that makes
// the re-ask observable: the budget holds several terms and the pauses between
// them, so a create that is asked once and never again is told apart from one
// re-asked until the budget ran out.
func TestAHeldConsumerCreateIsReadBackRatherThanReported(t *testing.T) {
	t.Parallel()
	js := &heldCreateJS{}
	timing := jsprovision.Clustered(true).Timing()
	timing.Budget, timing.AskTerm, timing.ReAsk =
		300*time.Millisecond, 50*time.Millisecond, 20*time.Millisecond
	q := &Queue{js: js, cfg: Config{ClusterName: "crewlet-test"}, log: slog.Default(),
		timing: timing}

	cons, err := q.DomainConsumer(t.Context(), "CREWLET_TRACKER_LOG", "node-a", 0)
	if err != nil {
		t.Fatalf("a create that was held and timed out failed the boot, even "+
			"though the read-back found the consumer: %v", err)
	}
	if cons == nil {
		t.Fatal("no consumer came back")
	}
	// EXHAUSTED, NOT ANSWERED: a held request is one nobody replied to, so
	// it is asked again until the budget ends — one create would be a
	// branch reached by something other than the budget running out.
	if got := js.creates.Load(); got < 2 {
		t.Errorf("the held create was sent %d time(s); an unanswered create is "+
			"asked again until its budget is spent", got)
	}
	if got := js.lookups.Load(); got < 2 {
		t.Errorf("the consumer was looked up %d time(s); the read-back after "+
			"the timed-out create never ran", got)
	}
}

// unsettledLookupJS is a metadata group that answers no read of the consumer
// until the consumer's create reaches it: every lookup is DROPPED — no reply,
// so it ends when its own term does — and the create is what settles the
// question. It records the wait each create was handed.
type unsettledLookupJS struct {
	jetstream.JetStream

	// lookups is ATOMIC: a lookup nobody answers stays outstanding while
	// the next is asked, so two of them can count at once.
	lookups atomic.Int32
	creates int
	armed   armedWaits
	// undeadlined is a create handed no deadline at all.
	undeadlined bool
	made        *jetstream.ConsumerInfo
}

func (f *unsettledLookupJS) Consumer(ctx context.Context, _, _ string) (jetstream.Consumer, error) {
	f.lookups.Add(1)
	if f.made == nil {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return reportingConsumer{info: f.made}, nil
}

func (f *unsettledLookupJS) CreateConsumer(ctx context.Context, _ string,
	cfg jetstream.ConsumerConfig) (jetstream.Consumer, error) {

	f.creates++
	if !f.armed.note(ctx, "create") {
		f.undeadlined = true
	}
	if err := ctx.Err(); err != nil {
		// A REQUEST ON A SPENT CONTEXT NEVER LEAVES THE CLIENT: nats.go
		// answers it with the context's own error and sends nothing.
		return nil, err
	}
	f.made = &jetstream.ConsumerInfo{Config: cfg}
	return reportingConsumer{info: f.made}, nil
}

// AN EXHAUSTED LOOKUP LEAVES THE CREATE ITS WHOLE BUDGET.
//
// # What it was
//
// The create's deadline was derived BEFORE the lookup, so it ran while the
// lookup did, and a lookup nobody answered spent up to its whole ceiling of the
// create's budget. On a solo broker the two are equal — thirty seconds each —
// so a lookup asked until its ceiling ran out handed the create a context that
// had already expired: the create never reached the broker, the read-back found
// nothing, and the open failed reporting a consumer that "is not there" over a
// create that was never sent. On a fleet the create kept ninety seconds of the
// two minutes [jsprovision.Budget] promises it. Every other create site derives
// the create's deadline after its lookup returns, which is what the lookup's
// own comment already said this one did.
//
// # Staged where the failure is total, scaled down
//
// Budget equal to Lookup, as production's solo timing has them, so a create
// whose budget the lookup shared is handed nothing at all — and the time the
// create was handed is asserted too, which also catches the share a fleet's
// larger budget would lose. The read term and the pause are a fraction of the
// ceiling so the lookup is asked several times before it gives up: EXHAUSTED
// rather than answered, which is the only way the create reaches the budget
// question at all. The group answers nothing until the create arrives, as one
// that is still forming does.
//
// # The whole budget, exactly
//
// IN A SYNCTEST BUBBLE ([inBubble]), the create's deadline less the instant
// it was armed is exactly what the create was handed, and it must be the whole
// budget. On the machine's clock it was the time left once the create
// arrived, held to half the budget: a runner that lost a quarter of a second
// between deriving the create's deadline and sending it failed that, and a
// create a quarter of whose budget the lookup had spent passed it.
//
// Mutation: derive the create's budget before the lookup, and the create is
// handed nothing; hand it three quarters of the budget, what a lookup that
// spent a quarter leaves, and it is short by that quarter.
func TestAnExhaustedLookupLeavesTheCreateItsWholeBudget(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		timing := jsprovision.Clustered(false).Timing()
		timing.Budget, timing.Lookup = 500*time.Millisecond, 500*time.Millisecond
		timing.ReadTerm, timing.ReAsk = 50*time.Millisecond, 20*time.Millisecond
		timing.ReadBack = 200 * time.Millisecond
		js := &unsettledLookupJS{}
		q := &Queue{js: js, log: slog.Default(), timing: timing}

		if _, err := q.DomainConsumer(t.Context(), "CREWLET_TRACKER_LOG", "node-a", 0); err != nil {
			t.Fatalf("an open whose lookup went unanswered failed rather than "+
				"creating the consumer: %v", err)
		}
		if js.undeadlined {
			t.Error("the create was handed no deadline at all")
		}
		if armed := js.armed.all(); len(armed) != 1 ||
			armed[0].deadline.Sub(armed[0].at) != timing.Budget {
			t.Errorf("the create was armed as %v, want its whole %v budget once — "+
				"the lookup before it spent the rest", armed, timing.Budget)
		}
		if js.creates != 1 {
			t.Errorf("the consumer was created %d time(s), want once", js.creates)
		}
		if got := js.lookups.Load(); got < 2 {
			t.Errorf("the consumer was looked up %d time(s) before the create; the "+
				"lookup is asked again until its ceiling is spent, and once means "+
				"this case never reached the exhaustion it is about", got)
		}
	})
}

// existingOnCreateJS is a clustered boot whose lookup fell inside the
// propagation window, and whose create was answered with the consumer that
// was ALREADY THERE — which nats.go does when the configurations match, and
// they match for a node whose rows are gone, because it asks for DeliverAll
// from nothing exactly as it did on its first boot.
type existingOnCreateJS struct {
	jetstream.JetStream

	// streamErr is what reading the stream's first sequence answers; nil
	// answers a stream whose first survivor is 1.
	streamErr error

	creates, deletes int
}

func (f *existingOnCreateJS) Consumer(context.Context, string, string) (jetstream.Consumer, error) {
	return nil, jetstream.ErrConsumerNotFound
}

func (f *existingOnCreateJS) CreateConsumer(_ context.Context, _ string,
	cfg jetstream.ConsumerConfig) (jetstream.Consumer, error) {

	f.creates++
	if f.creates == 1 {
		// THE CONSUMER THE LAST RUN LEFT: it delivered and was told it
		// handled everything through 5.
		return reportingConsumer{info: &jetstream.ConsumerInfo{Config: cfg,
			Delivered: jetstream.SequenceInfo{Stream: 5},
			AckFloor:  jetstream.SequenceInfo{Stream: 5}}}, nil
	}
	return reportingConsumer{info: &jetstream.ConsumerInfo{Config: cfg}}, nil
}

func (f *existingOnCreateJS) DeleteConsumer(context.Context, string, string) error {
	f.deletes++
	return nil
}

func (f *existingOnCreateJS) Stream(context.Context, string) (jetstream.Stream, error) {
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	return firstSurvivorStream{first: 1}, nil
}

// reportingConsumer is a consumer whose state is the one the broker returned
// with it.
type reportingConsumer struct {
	jetstream.Consumer
	info *jetstream.ConsumerInfo
}

func (c reportingConsumer) CachedInfo() *jetstream.ConsumerInfo { return c.info }

// firstSurvivorStream reports the first sequence a stream still holds.
type firstSurvivorStream struct {
	jetstream.Stream
	first uint64
}

func (s firstSurvivorStream) CachedInfo() *jetstream.StreamInfo {
	return &jetstream.StreamInfo{State: jetstream.StreamState{FirstSeq: s.first}}
}

// A CREATE ANSWERED WITH AN EXISTING CONSUMER IS JUDGED LIKE A LOOKUP THAT
// FOUND ONE.
//
// A single real broker cannot stage this: its lookup is authoritative, so an
// existing consumer is always found rather than created. A clustered member
// can — its lookup is unanswered or falls inside the propagation window, the
// create goes ahead, and the broker hands back the consumer that was there all
// along. The node that loses its rows is precisely the one whose create
// matches the old configuration, so this is the path its consumer comes back
// by; taken as a fresh create, the rebuild never happens on the topology where
// losing a node's disk is most survivable.
//
// And reading the stream's first sequence is a refinement, not a gate: when it
// cannot be read the consumer is judged on its raw positions and still
// rebuilt, because a rebuild is always correct. What the read decides is
// whether a rebuild is needed and what it is reported as — and a raw
// acknowledged-past is also what a node below the trim floor looks like, so
// the warning stays a warning and says it may be either.
func TestACreateAnsweredWithAnExistingConsumerIsJudged(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		streamErr error
	}{
		{"the stream's first sequence read", nil},
		{"the stream's first sequence unreadable", errors.New("stream info refused")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			js := &existingOnCreateJS{streamErr: tc.streamErr}
			var logged bytes.Buffer
			q := &Queue{js: js, cfg: Config{ClusterName: "crewlet-test"},
				log: slog.New(slog.NewTextHandler(&logged, nil))}

			cons, err := q.DomainConsumer(t.Context(), "CREWLET_PAGES_LOG", "node-0", 0)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if js.deletes != 1 || js.creates != 2 {
				t.Fatalf("the create came back with a consumer acknowledged through 5 "+
					"for a node whose rows are at 0, and it was deleted %d time(s) "+
					"and created %d — want it rebuilt once, or the node waits for "+
					"records the broker will never hand over again",
					js.deletes, js.creates)
			}
			held, err := cons.consumerFor(t.Context())
			if err != nil {
				t.Fatalf("the handle holds no consumer: %v", err)
			}
			if info := held.CachedInfo(); info.Delivered.Stream != 0 || info.AckFloor.Stream != 0 {
				t.Fatalf("the handle holds a consumer delivered through %d and "+
					"acknowledged through %d, want the rebuilt one",
					held.CachedInfo().Delivered.Stream, held.CachedInfo().AckFloor.Stream)
			}
			line := logLine(t, &logged, "jetstream_domain_consumer_rebuilt")
			if !strings.Contains(line, "level=WARN") {
				t.Errorf("a consumer acknowledged past the checkpoint was rebuilt "+
					"below a warning — the one drift that says the rows went "+
					"backwards: %s", line)
			}
			unread := strings.Contains(line, "first sequence could not be read")
			if unread != (tc.streamErr != nil) {
				t.Errorf("the rebuild's detail says the first sequence could not "+
					"be read: %t, want %t — unread, an acknowledged-past may be a "+
					"node below the trim floor rather than a database that went "+
					"backwards, and the operator is told so: %s",
					unread, tc.streamErr != nil, line)
			}
		})
	}
}

// logLine is the one line logged under event, failing the case when there is
// not exactly one.
func logLine(t *testing.T, logged *bytes.Buffer, event string) string {
	t.Helper()
	var found []string
	for line := range strings.Lines(logged.String()) {
		if strings.Contains(line, "msg="+event+" ") {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d line(s) logged as %s, want one:\n%s", len(found), event,
			logged.String())
	}
	return found[0]
}

// failedRebuildJS is a broker holding the consumer an earlier run left, whose
// metadata group refuses the rebuild in one of the ways a group that is not
// settled does.
type failedRebuildJS struct {
	jetstream.JetStream

	// held is what the broker holds under this node's consumer name: nil
	// for nothing.
	held *jetstream.ConsumerInfo

	// deleteErr refuses the delete, so the consumer stays where it was.
	deleteErr error
	// createErr refuses the create; with createLands the consumer is
	// placed all the same, which is a create whose answer was not the
	// outcome.
	createErr   error
	createLands bool
	// readBackErr answers every lookup after the open's own one, which is
	// the broker being asked what it holds and not saying.
	readBackErr error

	lookups, deletes int
}

// rebuiltAt is the creation instant of a consumer the fake's create makes,
// which is what tells it from the one the earlier run left.
var rebuiltAt = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

func (f *failedRebuildJS) Consumer(context.Context, string, string) (jetstream.Consumer, error) {
	f.lookups++
	if f.lookups > 1 && f.readBackErr != nil {
		return nil, f.readBackErr
	}
	if f.held == nil {
		return nil, jetstream.ErrConsumerNotFound
	}
	return reportingConsumer{info: f.held}, nil
}

func (f *failedRebuildJS) DeleteConsumer(context.Context, string, string) error {
	f.deletes++
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.held = nil
	return nil
}

func (f *failedRebuildJS) CreateConsumer(_ context.Context, _ string,
	cfg jetstream.ConsumerConfig) (jetstream.Consumer, error) {

	fresh := &jetstream.ConsumerInfo{Config: cfg, Created: rebuiltAt,
		Delivered: jetstream.SequenceInfo{Stream: cfg.OptStartSeq - 1},
		AckFloor:  jetstream.SequenceInfo{Stream: cfg.OptStartSeq - 1}}
	if f.createErr != nil {
		if f.createLands {
			f.held = fresh
		}
		return nil, f.createErr
	}
	f.held = fresh
	return reportingConsumer{info: fresh}, nil
}

func (f *failedRebuildJS) Stream(context.Context, string) (jetstream.Stream, error) {
	return firstSurvivorStream{first: 1}, nil
}

// A FAILED REBUILD LEAVES A NODE ON ITS OLD CONSUMER ONLY WHEN THAT COSTS TIME
// RATHER THAN RECORDS.
//
// # The boot this is
//
// A clustered restart whose metadata group is not answering — a leader lost
// mid-boot answers JSClusterNotAvail within about ten seconds, and that is an
// answer, so it is not re-asked. Before the rebuild existed that boot kept its
// consumer and came up; with it, every drift failed the boot, including the
// two that cost only time. What each drift does now:
//
//   - BEHIND and IN FLIGHT keep whichever consumer the broker holds, which a
//     lookup establishes rather than the error, and the node comes up. The
//     rebuilt one counts: a create can place the consumer and still report
//     failure.
//   - AHEAD, acknowledged or delivered, fails the open without asking: a node
//     left reading either can wait for ever.
//   - A broker that holds none after the failure — the delete landed, the
//     create did not — fails the open, as the absent path's failed create
//     does; so does one that cannot say what it holds.
//
// The handle a kept open returns is checked for WHICH consumer it names,
// because naming the wrong one fails every fetch for the life of the process
// while the boot reports success.
func TestAFailedRebuildKeepsOnlyAConsumerThatCostsTime(t *testing.T) {
	t.Parallel()
	// The answer a leaderless metadata group sends, and an answer is not
	// re-asked; the create's is an ordinary refusal of the same kind.
	notAvailable := &jetstream.APIError{Code: 503, ErrorCode: 10008,
		Description: "JetStream system temporarily unavailable"}
	createRefused := &jetstream.APIError{Code: 500,
		ErrorCode: jetstream.JSErrCodeConsumerCreate, Description: "consumer create failed"}
	lastRun := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	// THE LAST RUN'S CONSUMERS, against a checkpoint of 4.
	left := func(delivered, ackFloor uint64, inFlight int) *jetstream.ConsumerInfo {
		return &jetstream.ConsumerInfo{Created: lastRun,
			Config:        domainConsumerConfig("statelog__failed", 0),
			Delivered:     jetstream.SequenceInfo{Stream: delivered},
			AckFloor:      jetstream.SequenceInfo{Stream: ackFloor},
			NumAckPending: inFlight}
	}
	behind, inFlight := left(2, 2, 0), left(4, 2, 2)
	acknowledged, delivered := left(6, 6, 0), left(6, 4, 2)

	for _, tc := range []struct {
		name string
		js   *failedRebuildJS
		// cause is the refusal the failed open must carry; nil for an
		// open that comes up.
		cause error
		// lookups is how many times the open asks for the consumer: its
		// own lookup, and the read-back only where one can change the
		// answer.
		lookups int
		// holds is the creation instant of the consumer a kept handle
		// names, and logged the line it is kept under.
		holds  time.Time
		logged string
	}{
		{name: "behind, delete refused",
			js:      &failedRebuildJS{held: behind, deleteErr: notAvailable},
			lookups: 2, holds: lastRun,
			logged: "jetstream_domain_consumer_rebuild_failed"},
		{name: "in flight, delete refused",
			js:      &failedRebuildJS{held: inFlight, deleteErr: notAvailable},
			lookups: 2, holds: lastRun,
			logged: "jetstream_domain_consumer_rebuild_failed"},
		{name: "in flight, create refused after placing it",
			js: &failedRebuildJS{held: inFlight, createErr: createRefused,
				createLands: true},
			lookups: 2, holds: rebuiltAt,
			logged: "jetstream_domain_consumer_rebuilt"},
		{name: "acknowledged past, delete refused",
			js:    &failedRebuildJS{held: acknowledged, deleteErr: notAvailable},
			cause: notAvailable, lookups: 1},
		{name: "delivered past, delete refused",
			js:    &failedRebuildJS{held: delivered, deleteErr: notAvailable},
			cause: notAvailable, lookups: 1},
		{name: "acknowledged past, create refused after placing it",
			js: &failedRebuildJS{held: acknowledged, createErr: createRefused,
				createLands: true},
			cause: createRefused, lookups: 1},
		{name: "behind, deleted and not created again",
			js:    &failedRebuildJS{held: behind, createErr: createRefused},
			cause: createRefused, lookups: 2},
		{name: "in flight, delete refused and the broker cannot say what it holds",
			js: &failedRebuildJS{held: inFlight, deleteErr: notAvailable,
				readBackErr: notAvailable},
			cause: notAvailable, lookups: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var logged bytes.Buffer
			q := &Queue{js: tc.js, cfg: Config{ClusterName: "crewlet-test"},
				log: slog.New(slog.NewTextHandler(&logged, nil))}

			cons, err := q.DomainConsumer(t.Context(), "CREWLET_PAGES_LOG", "node-0", 4)
			if tc.js.deletes != 1 {
				t.Fatalf("the rebuild's delete was asked %d time(s), want once — "+
					"this case stages a consumer that disagrees with the "+
					"checkpoint and nothing else", tc.js.deletes)
			}
			if tc.js.lookups != tc.lookups {
				t.Errorf("the open looked the consumer up %d time(s), want %d — "+
					"what the broker holds after a failed rebuild is asked where "+
					"it can keep the node up, and only there", tc.js.lookups,
					tc.lookups)
			}
			if tc.cause != nil {
				if err == nil {
					t.Fatal("a consumer whose rebuild failed was kept where a node " +
						"left reading it can wait for ever, or where the broker " +
						"holds none or cannot say, and the boot reported success")
				}
				if !errors.Is(err, tc.cause) {
					t.Errorf("the failed open does not carry the refusal it "+
						"failed on (%v): %v", tc.cause, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("a consumer whose drift costs only time failed the boot "+
					"when its rebuild failed: %v", err)
			}
			held, err := cons.consumerFor(t.Context())
			if err != nil {
				t.Fatalf("the kept handle names no consumer: %v", err)
			}
			if got := held.CachedInfo().Created; !got.Equal(tc.holds) {
				t.Fatalf("the kept handle names the consumer created %s, want the "+
					"one the broker holds, created %s — a handle naming the "+
					"wrong one fails every fetch for the life of the process",
					got, tc.holds)
			}
			if line := logLine(t, &logged, tc.logged); !strings.Contains(line, "level=WARN") &&
				tc.logged == "jetstream_domain_consumer_rebuild_failed" {
				t.Errorf("a consumer kept after its rebuild failed was not "+
					"reported as a warning: %s", line)
			}
		})
	}
}

// droppedDeleteJS is a metadata group that drops the first delete it is asked
// for — a leader change, a routed queue discarded at its limit — and answers
// the second. It records the wait every delete was handed.
type droppedDeleteJS struct {
	jetstream.JetStream
	asked       int
	deletes     armedWaits
	undeadlined bool
}

func (f *droppedDeleteJS) DeleteConsumer(ctx context.Context, _, _ string) error {
	f.asked++
	if !f.deletes.note(ctx, "delete") {
		f.undeadlined = true
	}
	if f.asked == 1 {
		return nats.ErrTimeout
	}
	return nil
}

func (f *droppedDeleteJS) CreateConsumer(_ context.Context, _ string,
	cfg jetstream.ConsumerConfig) (jetstream.Consumer, error) {

	return reportingConsumer{info: &jetstream.ConsumerInfo{Config: cfg}}, nil
}

// A RESET'S DELETE IS RE-ASKED LIKE ITS CREATE, on a deadline of its own.
//
// The delete is a request to the same metadata group as the create after it,
// and it went out on the caller's context with neither a term nor a re-ask. An
// adoption's context has no deadline, so nats.go's five-second default decided
// a replicated write on a clustered fleet; and a request the group dropped
// failed the reset on the spot rather than being asked again — which, on the
// open that now reaches this for a consumer AHEAD of its rows, is a boot that
// fails over a question nobody heard.
//
// A DEADLINE OF ITS OWN is each request's own clustered ask term, from the
// instant it was asked. IN A SYNCTEST BUBBLE ([inBubble]) that is read off the
// request's deadline less the instant it was armed, exactly. It used to be
// each deadline's time left, read off the machine's clock once the reset had
// returned and held above [jsprovision.SlowAfter]: a runner that lost five
// seconds in the reset failed it, and a re-ask handed what the first request
// had left, or the solo term, passed it.
//
// What bounds the re-asking as a whole is the provisioning budget, and that is
// not asserted here: a group that never answers is only distinguishable from
// one that answers late by waiting the budget out.
//
// Mutation: ask the delete once, on the reset's budget, and the dropped one
// fails the reset; arm one request's deadline for every re-ask, or read the
// topology as solo, and a delete is handed another term than its own.
func TestAResetsDeleteIsReAskedOnADeadlineOfItsOwn(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		js := &droppedDeleteJS{}
		q := &Queue{js: js, cfg: Config{ClusterName: "crewlet-test"}, log: slog.Default()}
		c := &DomainConsumer{q: q, stream: "CREWLET_PAGES_LOG", name: "statelog__reset",
			want: domainConsumerConfig("statelog__reset", 0)}

		// A CALLER WITH NO DEADLINE OF ITS OWN, which is what an adoption
		// passes.
		if err := c.Reset(context.Background(), 3); err != nil {
			t.Fatalf("a reset whose first delete the group dropped failed instead of "+
				"asking again: %v", err)
		}
		if js.asked != 2 {
			t.Fatalf("the delete was asked %d time(s), want 2 — once dropped, once "+
				"answered", js.asked)
		}
		if js.undeadlined {
			t.Fatal("a delete carried no deadline, so nats.go's 5s default decides " +
				"a replicated write")
		}
		// THE CLUSTERED TERM AS PRODUCTION HAS IT, never the queue's own,
		// which a queue that read its topology wrongly would answer for the
		// wrong topology.
		term := jsprovision.Clustered(true).AskTerm()
		for i, d := range js.deletes.all() {
			if got := d.deadline.Sub(d.at); got != term {
				t.Errorf("delete %d was handed %v from the instant it was asked, want "+
					"the clustered %v ask term of its own", i+1, got, term)
			}
		}
		if got := c.want.OptStartSeq; got != 4 {
			t.Fatalf("the reset landed at %d, want 4", got)
		}
	})
}

// firstReadJS holds one consumer the last run left, and records how the
// stream's first sequence was asked for: its first request is DROPPED, which
// is the reply a metadata group that is not settled does not send.
type firstReadJS struct {
	jetstream.JetStream
	held *jetstream.ConsumerInfo

	reads   int
	had     []bool
	until   []time.Time
	at      []time.Time
	deletes int
}

func (f *firstReadJS) Consumer(context.Context, string, string) (jetstream.Consumer, error) {
	return reportingConsumer{info: f.held}, nil
}

func (f *firstReadJS) Stream(ctx context.Context, _ string) (jetstream.Stream, error) {
	f.reads++
	until, had := ctx.Deadline()
	f.until, f.had, f.at = append(f.until, until), append(f.had, had), append(f.at, time.Now())
	if f.reads == 1 {
		return nil, nats.ErrTimeout
	}
	// Everything below 5 was trimmed.
	return firstSurvivorStream{first: 5}, nil
}

func (f *firstReadJS) DeleteConsumer(context.Context, string, string) error {
	f.deletes++
	return nil
}

func (f *firstReadJS) CreateConsumer(_ context.Context, _ string,
	cfg jetstream.ConsumerConfig) (jetstream.Consumer, error) {

	return reportingConsumer{info: &jetstream.ConsumerInfo{Config: cfg}}, nil
}

// THE STREAM'S FIRST SEQUENCE IS A READ: asked only where it can change the
// answer, re-asked when nobody replies, and bounded as a read.
//
// # What it was
//
// One request on the provisioning budget — two minutes on a fleet — made for
// every consumer that disagreed with its checkpoint in any way. A request the
// metadata group dropped stalled its domain for all of it with nothing in the
// log, one per domain spent more than the ceiling every domain's consumer
// shares, and the commonest disagreement a busy restart leaves — deliveries
// in flight for the process that stopped — is one no first sequence can
// change.
//
// # The two stagings
//
// A consumer the broker placed at a trimmed stream's first survivor, which
// only the first sequence keeps: its dropped first request must be asked
// again, each ask must be bounded by the queue's lookup ceiling rather than
// the provisioning budget, and the answer must keep it. And one holding
// deliveries in flight at its checkpoint, which is rebuilt without the read.
func TestTheFirstSequenceIsAReadAskedOnlyWhereItCounts(t *testing.T) {
	t.Parallel()
	// A LOOKUP CEILING FAR BELOW THE PROVISIONING BUDGET, so which of the
	// two bounded the read is visible in the deadline it carried. Every
	// other duration is production's, which is what the deadlines are read
	// against.
	const ceiling = 3 * time.Second
	cfg := Config{ClusterName: "crewlet-test"}
	timing := jsprovision.Clustered(true).Timing()
	timing.Lookup = ceiling
	name := domainConsumerName("CREWLET_PAGES_LOG", "node-0")

	t.Run("placed at the first survivor", func(t *testing.T) {
		t.Parallel()
		// IN A SYNCTEST BUBBLE ([inBubble]), so the re-ask is held to its
		// cadence exactly: on the machine's clock this was a second of
		// slack on how late a loaded race-detector run wakes the timer's
		// goroutine, which the read never promised.
		inBubble(t, func(t *testing.T) {
			js := &firstReadJS{held: &jetstream.ConsumerInfo{
				Config:    domainConsumerConfig(name, 2),
				Delivered: jetstream.SequenceInfo{Stream: 4},
				AckFloor:  jetstream.SequenceInfo{Stream: 4}}}
			q := &Queue{js: js, cfg: cfg, log: slog.Default(), timing: timing}
			if _, err := q.DomainConsumer(t.Context(), "CREWLET_PAGES_LOG", "node-0", 2); err != nil {
				t.Fatalf("open: %v", err)
			}
			if js.reads != 2 {
				t.Fatalf("the first sequence was asked for %d time(s), want 2 — a "+
					"dropped request asked again rather than being the whole answer",
					js.reads)
			}
			for i, had := range js.had {
				if !had {
					t.Fatalf("ask %d carried no deadline of its own", i+1)
				}
				if left := time.Until(js.until[i]); left > ceiling {
					t.Errorf("ask %d was given %v, past the %v lookup ceiling — it "+
						"is bounded as a write", i+1, left, ceiling)
				}
			}
			// ASKED AGAIN AT A READ'S CADENCE: the dropped request is told
			// nothing at once, so it is presumed lost at once and the next
			// follows a re-ask later — never a write's term, which a read
			// the server dropped would wait out for a reply nobody will
			// send.
			if gap := js.at[1].Sub(js.at[0]); gap != timing.ReAsk {
				t.Errorf("the dropped ask was asked again %v later, want the %v "+
					"re-ask after a request told nothing at once — it is held for "+
					"the %v read term or a write's %v term", gap, timing.ReAsk,
					timing.ReadTerm, timing.AskTerm)
			}
			if js.deletes != 0 {
				t.Fatal("a consumer the broker placed at the first survivor was " +
					"rebuilt once its first sequence had been read")
			}
		})
	})

	t.Run("in flight at the checkpoint", func(t *testing.T) {
		t.Parallel()
		js := &firstReadJS{held: &jetstream.ConsumerInfo{
			Config:        domainConsumerConfig(name, 2),
			Delivered:     jetstream.SequenceInfo{Stream: 4},
			AckFloor:      jetstream.SequenceInfo{Stream: 2},
			NumAckPending: 2}}
		q := &Queue{js: js, cfg: cfg, log: slog.Default(), timing: timing}
		if _, err := q.DomainConsumer(t.Context(), "CREWLET_PAGES_LOG", "node-0", 4); err != nil {
			t.Fatalf("open: %v", err)
		}
		if js.reads != 0 {
			t.Errorf("the first sequence was asked for %d time(s) for a consumer "+
				"holding deliveries at its checkpoint, which no first sequence "+
				"changes", js.reads)
		}
		if js.deletes != 1 {
			t.Errorf("a consumer holding a gone reader's deliveries was deleted "+
				"%d time(s), want it rebuilt once", js.deletes)
		}
	})
}

// silentLeaderStream is a stream handle whose LEADER never answers the first
// lost requests made of it: each of them waits out its own context, as a
// request does that reached a member still believing in a leader the cluster
// has just lost, and every request after them is answered at once — by the
// leader the cluster elected meanwhile.
//
// EMBEDS THE INTERFACE, for [notYetVisibleJS]'s reason.
type silentLeaderStream struct {
	jetstream.Stream
	lost  atomic.Int32
	calls atomic.Int32
}

// unanswered counts a request, and makes it one of the lost ones while any
// are left: it waits out ctx, as a request nobody answers does.
func (s *silentLeaderStream) unanswered(ctx context.Context) error {
	s.calls.Add(1)
	if s.lost.Add(-1) < 0 {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

func (s *silentLeaderStream) Info(ctx context.Context, _ ...jetstream.StreamInfoOpt) (*jetstream.StreamInfo, error) {
	if err := s.unanswered(ctx); err != nil {
		return nil, err
	}
	return leadersInfo(), nil
}

func (s *silentLeaderStream) GetLastMsgForSubject(ctx context.Context, subject string) (*jetstream.RawStreamMsg, error) {
	if err := s.unanswered(ctx); err != nil {
		return nil, err
	}
	return leadersLast(subject), nil
}

func (s *silentLeaderStream) GetMsg(ctx context.Context, seq uint64, _ ...jetstream.GetMsgOpt) (*jetstream.RawStreamMsg, error) {
	if err := s.unanswered(ctx); err != nil {
		return nil, err
	}
	return leadersRecord(seq), nil
}

// leadersInfo, leadersLast and leadersRecord are what the leader answers each
// read with once it answers at all — answers only, so a stream that counts
// its requests its own way counts each of them once.
func leadersInfo() *jetstream.StreamInfo {
	return &jetstream.StreamInfo{State: jetstream.StreamState{FirstSeq: 1, LastSeq: 7, Bytes: 640},
		Config: jetstream.StreamConfig{Name: "CREWLET_CODE_LOG", MaxBytes: 1 << 20}}
}

func leadersLast(subject string) *jetstream.RawStreamMsg {
	return &jetstream.RawStreamMsg{Subject: subject, Sequence: 7}
}

func leadersRecord(seq uint64) *jetstream.RawStreamMsg {
	return &jetstream.RawStreamMsg{Subject: "crewlet.code.repo.r", Sequence: seq}
}

// shortReads is the production timing with a read's term and its re-ask short
// enough for a case to wait out two lost requests in a fraction of a second.
func shortReads() jsprovision.Timing {
	t := jsprovision.Clustered(true).Timing()
	t.ReadTerm, t.ReAsk = 50*time.Millisecond, 5*time.Millisecond
	return t
}

// A READ OF A DOMAIN LOG THAT ITS LEADER NEVER ANSWERED IS ASKED AGAIN, within
// the caller's budget, rather than waited on for the whole of it.
//
// A domain log allows no direct get, so its info, its last message on a
// subject and its record at a sequence are answered by the stream's leader or
// by nobody — and a member that still believes in a leader the cluster has
// just lost stays silent. Held for its caller's whole context, one such
// request cost the committer's thirty-second budget: the gate reserve's usage
// reading before a push's record, `context deadline exceeded`, a push nothing
// decided reported `unknown`, while a request sent a second later was answered
// by the new leader at once. Each read here is lost twice and then answered,
// inside a budget a whole-context wait would have spent on the first.
func TestADomainLogReadItsLeaderNeverAnsweredIsAskedAgain(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		read func(ctx context.Context, l *DomainLog) error
	}{
		{"stats", func(ctx context.Context, l *DomainLog) error {
			stats, err := l.Stats(ctx)
			if err == nil && (stats.LastSeq != 7 || stats.MaxBytes != 1<<20) {
				err = errors.New("the stats are not the answer the leader gave")
			}
			return err
		}},
		{"last_on_a_subject", func(ctx context.Context, l *DomainLog) error {
			seq, ok, err := l.LastSeq(ctx, "crewlet.code.repo.r")
			if err == nil && (!ok || seq != 7) {
				err = errors.New("the last sequence is not the answer the leader gave")
			}
			return err
		}},
		{"record_at_a_sequence", func(ctx context.Context, l *DomainLog) error {
			_, _, _, ok, err := l.At(ctx, 3)
			if err == nil && !ok {
				err = errors.New("the record the leader answered with was reported gone")
			}
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				s := &silentLeaderStream{}
				s.lost.Store(2)
				l := logOver(s)
				// A BUDGET FOUR TERMS LONG: room for two lost requests,
				// their re-asks and the answer, and none for a wait on
				// the first.
				ctx, cancel := context.WithTimeout(t.Context(), 4*shortReads().ReadTerm)
				defer cancel()
				if err := c.read(ctx, l); err != nil {
					t.Fatalf("a read its leader lost twice: %v", err)
				}
				if got := s.calls.Load(); got != 3 {
					t.Fatalf("the read was asked %d times, want 3 — two lost, one answered", got)
				}
			})
		})
	}
}

// AND ONE NOBODY EVER ANSWERS ENDS AT THE LOOKUP CEILING, for a caller with no
// deadline of its own: the trim tick reads every log's state on its node's
// lifetime, in the loop that also evaluates the node's alarms, and asked until
// somebody answered, a read of a broker the node could not reach held both for
// good.
func TestADomainLogReadNobodyAnswersEndsAtTheLookupCeiling(t *testing.T) {
	t.Parallel()
	s := &silentLeaderStream{}
	s.lost.Store(1 << 30)
	l := logOver(s)
	// A CEILING OF TWENTY REQUESTS rather than production's thirty seconds,
	// so the case waits it out in about a second.
	ceiling := 20 * (l.q.timing.ReadTerm + l.q.timing.ReAsk)
	l.q.timing.Lookup = ceiling
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		_, err := l.Stats(ctx)
		done <- err
	}()
	select {
	case err := <-done:
		if !jsprovision.Unanswered(ctx, err) {
			t.Errorf("a read nobody answered ended with %v, not as silence", err)
		}
	case <-time.After(10 * ceiling):
		cancel()
		<-done
		t.Fatalf("a read nobody answers was still being asked %v later, ten times "+
			"its %v ceiling (%d requests)", 10*ceiling, ceiling, s.calls.Load())
	}
}

// logOver is a domain log over s, read at [shortReads].
func logOver(s jetstream.Stream) *DomainLog {
	return &DomainLog{stream: s, name: "CREWLET_CODE_LOG", q: &Queue{timing: shortReads()},
		info: func(ctx context.Context) (*jetstream.StreamInfo, error) { return s.Info(ctx) }}
}

// slowLeaderStream is a stream handle whose LEADER answers every request late,
// after late — the leader alive and only slow, its API queue backed up — and
// counts the requests made of it, each ONCE: it answered through the silent
// stream's own reads, which count a request again, so the late-answer case's
// premise — a second request asked beside the first — held for a read that
// asked only once.
type slowLeaderStream struct {
	silentLeaderStream
	late time.Duration
}

func (s *slowLeaderStream) slow(ctx context.Context) error {
	s.calls.Add(1)
	select {
	case <-time.After(s.late):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *slowLeaderStream) Info(ctx context.Context, _ ...jetstream.StreamInfoOpt) (*jetstream.StreamInfo, error) {
	if err := s.slow(ctx); err != nil {
		return nil, err
	}
	return leadersInfo(), nil
}

func (s *slowLeaderStream) GetLastMsgForSubject(ctx context.Context, subject string) (*jetstream.RawStreamMsg, error) {
	if err := s.slow(ctx); err != nil {
		return nil, err
	}
	return leadersLast(subject), nil
}

func (s *slowLeaderStream) GetMsg(ctx context.Context, seq uint64, _ ...jetstream.GetMsgOpt) (*jetstream.RawStreamMsg, error) {
	if err := s.slow(ctx); err != nil {
		return nil, err
	}
	return leadersRecord(seq), nil
}

// A READ OF A DOMAIN LOG THAT ITS LEADER ANSWERS LATE IS STILL ANSWERED: every
// request it is asked is answered half a read term after the term ends, as a
// leader whose API queue is backed up answers — slowly, never not at all.
// Each request is presumed lost at its term and another is asked, and the
// first one's late answer is taken. Abandoned at its term instead, every
// request was thrown away before its answer came, so no reading of a leader
// slower than the term was ever taken — and the gate reserve that admits every
// append and every barrier through the usage reading failed each of them at
// its own budget, every write and every linearizable read on the node at once.
func TestADomainLogReadItsLeaderAnswersLateIsAnswered(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		read func(ctx context.Context, l *DomainLog) error
	}{
		{"stats", func(ctx context.Context, l *DomainLog) error {
			_, err := l.Stats(ctx)
			return err
		}},
		{"last_on_a_subject", func(ctx context.Context, l *DomainLog) error {
			_, _, err := l.LastSeq(ctx, "crewlet.code.repo.r")
			return err
		}},
		{"record_at_a_sequence", func(ctx context.Context, l *DomainLog) error {
			_, _, _, _, err := l.At(ctx, 3)
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			// IN A BUBBLE, so the premise below is the cadence's alone: the
			// second request goes out a re-ask after the first's term, two
			// fifths of a term before the first's answer, a margin a loaded
			// run's late timer could cross on the machine's clock.
			inBubble(t, func(t *testing.T) {
				term := shortReads().ReadTerm
				s := &slowLeaderStream{late: term + term/2}
				// A BUDGET TEN TERMS LONG, which an abandoned request
				// never reaches the end of with an answer: each is thrown
				// away half a term before it would have been answered.
				ctx, cancel := context.WithTimeout(t.Context(), 10*term)
				defer cancel()
				if err := c.read(ctx, logOver(s)); err != nil {
					t.Fatalf("a read its leader answers %v late: %v", s.late, err)
				}
				// THE PREMISE: the answer taken came after a request's
				// term, so a second request was asked beside the first.
				if got := s.calls.Load(); got < 2 {
					t.Errorf("the read was asked %d time(s): its answer came inside the term, "+
						"and the case proves nothing about a late one", got)
				}
			})
		})
	}
}

// A LOG'S MEMBER COUNT IS THE STREAM'S OWN, AND EVERY STATE READ KEEPS IT.
//
// What a barrier that did not commit means turns on it (statelog's read
// index): a majority that did not agree where several members store the log,
// the member's own stalled write where one does. So it is the replication
// factor the broker reports for the stream — never Tier A's, which the running
// stream may exceed — and a stream an operator re-replicates under a running
// node is seen at the next read of its state rather than at the next restart.
// An answer stating no factor leaves the last one standing, and a log whose
// state nobody has read is one member, the fewest a stream has.
func TestALogsMemberCountIsTheStreamsOwnAndEveryStateReadKeepsIt(t *testing.T) {
	t.Parallel()
	var replicas atomic.Int32
	l := &DomainLog{name: "CREWLET_CODE_LOG", q: &Queue{timing: shortReads()},
		info: func(context.Context) (*jetstream.StreamInfo, error) {
			return &jetstream.StreamInfo{Config: jetstream.StreamConfig{
				Replicas: int(replicas.Load())}}, nil
		}}
	if got := l.Members(); got != 1 {
		t.Fatalf("a log whose state nobody read is stored by %d members, want 1", got)
	}
	for _, step := range []struct {
		name     string
		reported int32
		want     int
	}{
		{"re-replicated to three", 3, 3},
		{"an answer stating no factor", 0, 3},
		{"back to one", 1, 1},
	} {
		replicas.Store(step.reported)
		if _, err := l.stateInfo(t.Context()); err != nil {
			t.Fatalf("%s: read the log's state: %v", step.name, err)
		}
		if got := l.Members(); got != step.want {
			t.Errorf("%s: the log is stored by %d members, want %d", step.name, got, step.want)
		}
	}
}
