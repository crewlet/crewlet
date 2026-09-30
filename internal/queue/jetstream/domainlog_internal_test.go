package jetstream

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
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

// A CREATE THAT HAS NOT PROPAGATED YET IS WAITED OUT, not reported.
//
// This is the failure it was found by: a clustered boot died with `open the
// log "CREWLET_TRACKER_VECTORS": stream not found` on the node whose own
// create of that stream had just returned success. The lookup was the one step
// on the provisioning path with no tolerance for a peer-visible-but-not-yet-
// local create, and a state log that cannot open the stream it just made takes
// the whole engine down with it.
func TestALookupWaitsOutACreateThatHasNotPropagated(t *testing.T) {
	t.Parallel()
	js := &notYetVisibleJS{}
	js.notFound.Store(3)
	q := &Queue{js: js}

	if _, err := q.openProvisioned(t.Context(), "CREWLET_TRACKER_VECTORS"); err != nil {
		t.Fatalf("a stream that appeared on the fourth look was reported as %v", err)
	}
	if got := js.calls.Load(); got != 4 {
		t.Errorf("the lookup was made %d times, want 4 — three not-founds "+
			"waited out and the one that answered", got)
	}
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
	// wait runs out rather than succeeding late.
	js.notFound.Store(1 << 30)
	q := &Queue{js: js}

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

// deadlineJS records the deadline the call it was handed actually carried.
type deadlineJS struct {
	jetstream.JetStream
	had   bool
	until time.Time
}

func (f *deadlineJS) CreateConsumer(ctx context.Context, _ string,
	_ jetstream.ConsumerConfig) (jetstream.Consumer, error) {

	f.until, f.had = ctx.Deadline()
	return nil, errors.New("refused, so the read-back below runs")
}

// Consumer is the read-back ensureDurableConsumer makes when a create fails,
// and it refuses too: this case is about the DEADLINE the create carried, and
// a read-back that answered would only add a second path to reason about.
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
func TestADurableConsumerCreateCarriesTheProvisioningBudget(t *testing.T) {
	t.Parallel()
	js := &deadlineJS{}
	// A NAMED CLUSTER is what selects the clustered budget — Replicas does
	// not, and a test that set only it would exercise the solo path while
	// claiming to cover this one. See [Queue.Clustered].
	q := &Queue{js: js, cfg: Config{ClusterName: "crewlet-test"}, log: slog.Default()}

	// A CALLER WITH NO DEADLINE OF ITS OWN, which is what a boot passes.
	_, _, _ = q.ensureDurableConsumer(t.Context(), "CREWLET_AGENT",
		jetstream.ConsumerConfig{Durable: "agent-ceo"})

	if !js.had {
		t.Fatal("the create ran with no deadline, so nats.go's 5s default " +
			"applies and neither the budget nor the breadcrumb exists")
	}
	// It must be the CLUSTERED budget rather than the client's default.
	if left := time.Until(js.until); left <= jsprovision.SlowAfter {
		t.Errorf("the create got %v, which is inside the %v slow threshold — "+
			"the breadcrumb could never fire", left, jsprovision.SlowAfter)
	}
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

// alignJS answers the alignment's Info with a consumer that needs updating, and
// records the deadline UpdateConsumer was handed.
type alignJS struct {
	jetstream.JetStream
	had   bool
	until time.Time
}

func (f *alignJS) UpdateConsumer(ctx context.Context, _ string,
	_ jetstream.ConsumerConfig) (jetstream.Consumer, error) {

	f.until, f.had = ctx.Deadline()
	return nil, ctx.Err()
}

// staleConsumer reports a configuration the alignment has to change, so the
// update below it actually runs — at a position that AGREES with a checkpoint
// of zero, so the consumer is kept and aligned rather than rebuilt.
type staleConsumer struct {
	jetstream.Consumer
}

func (staleConsumer) CachedInfo() *jetstream.ConsumerInfo { return nil }

func (staleConsumer) Info(context.Context) (*jetstream.ConsumerInfo, error) {
	return &jetstream.ConsumerInfo{Config: jetstream.ConsumerConfig{
		// Neither matches this build's, so the alignment updates.
		MaxAckPending: 1,
		AckWait:       time.Second,
	}}, nil
}

// THE ALIGNMENT GETS THE PROVISIONING BUDGET, AND ONLY A LIVE CALLER CAN GIVE
// IT ONE.
//
// # The two ways to get this wrong, which are each other's repair
//
// Handed the PER-CREATE context, the alignment inherits a deadline that may
// have just expired — that is what puts the recovery path here at all — and
// [context.WithTimeout] cannot revive it, because it only ever shortens. Info
// and UpdateConsumer then fail instantly and the boot dies one line below the
// read-back that saved it. So the call sites pass the context that bounds the
// BOOT.
//
// Handed that context and nothing else, the alignment reaches nats.go with no
// deadline at all and the client's five-second default decides it — and
// UpdateConsumer is a write against the same metadata group as the create,
// which is allowed twenty-four times longer. So the budget is derived here.
//
// Both halves are asserted below: the budget is applied, and a caller's own
// shorter deadline still wins.
func TestTheAlignmentGetsTheProvisioningBudget(t *testing.T) {
	t.Parallel()
	js := &alignJS{}
	q := &Queue{js: js, cfg: Config{ClusterName: "crewlet-test"}, log: slog.Default()}

	c := &DomainConsumer{q: q, stream: "CREWLET_AGENT", name: "statelog__align"}

	// A CALLER WITH NO DEADLINE OF ITS OWN, which is what an engine boot
	// passes and what the call sites hand this.
	if err := c.resumeAt(t.Context(), staleConsumer{}, 0); err != nil {
		t.Fatalf("the alignment failed: %v", err)
	}
	if !js.had {
		t.Fatal("the update ran with no deadline of its own, so nats.go's 5s " +
			"default decides a replicated write on a clustered boot")
	}
	if left := time.Until(js.until); left <= jsprovision.SlowAfter {
		t.Errorf("the update got %v, which is shorter than the slow-create "+
			"threshold and so cannot be the provisioning budget", left)
	}

	// AND A TIGHTER CALLER STILL WINS, which is what keeps the aggregate
	// ceilings above this meaningful.
	short, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	js.had = false
	if err := c.resumeAt(short, staleConsumer{}, 0); err != nil {
		t.Fatalf("the alignment failed under a short caller: %v", err)
	}
	if left := time.Until(js.until); left > time.Second {
		t.Errorf("a caller with a 50ms deadline saw the update given %v — "+
			"WithTimeout is supposed to only ever shorten", left)
	}
}

// heldCreateJS is the shape a clustered boot actually produces: the create is
// HELD by the server while the metadata group settles, outlives its deadline,
// and comes back a timeout — with the consumer there all the same.
type heldCreateJS struct {
	jetstream.JetStream
	lookups int
}

func (f *heldCreateJS) Consumer(context.Context, string, string) (jetstream.Consumer, error) {
	f.lookups++
	if f.lookups == 1 {
		// The propagation window: this node made it on an earlier boot
		// and has not been told about it yet.
		return nil, jetstream.ErrConsumerNotFound
	}
	return staleConsumer{}, nil
}

func (f *heldCreateJS) CreateConsumer(context.Context, string,
	jetstream.ConsumerConfig) (jetstream.Consumer, error) {

	return nil, context.DeadlineExceeded
}

func (f *heldCreateJS) UpdateConsumer(context.Context, string,
	jetstream.ConsumerConfig) (jetstream.Consumer, error) {

	return staleConsumer{}, nil
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
func TestAHeldConsumerCreateIsReadBackRatherThanReported(t *testing.T) {
	t.Parallel()
	js := &heldCreateJS{}
	q := &Queue{js: js, cfg: Config{ClusterName: "crewlet-test"}, log: slog.Default()}

	cons, err := q.DomainConsumer(t.Context(), "CREWLET_TRACKER_LOG", "node-a", 0)
	if err != nil {
		t.Fatalf("a create that was held and timed out failed the boot, even "+
			"though the read-back found the consumer: %v", err)
	}
	if cons == nil {
		t.Fatal("no consumer came back")
	}
	if js.lookups < 2 {
		t.Errorf("the consumer was looked up %d time(s); the read-back after "+
			"the timed-out create never ran", js.lookups)
	}
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
// the second. It records the deadline every delete carried.
type droppedDeleteJS struct {
	jetstream.JetStream
	deletes int
	had     []bool
	until   []time.Time
}

func (f *droppedDeleteJS) DeleteConsumer(ctx context.Context, _, _ string) error {
	f.deletes++
	until, had := ctx.Deadline()
	f.until, f.had = append(f.until, until), append(f.had, had)
	if f.deletes == 1 {
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
// What bounds the re-asking as a whole is the provisioning budget, and that is
// not asserted here: a group that never answers is only distinguishable from
// one that answers late by waiting the budget out.
func TestAResetsDeleteIsReAskedOnADeadlineOfItsOwn(t *testing.T) {
	t.Parallel()
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
	if js.deletes != 2 {
		t.Fatalf("the delete was asked %d time(s), want 2 — once dropped, once "+
			"answered", js.deletes)
	}
	for i, had := range js.had {
		if !had {
			t.Fatalf("delete %d carried no deadline, so nats.go's 5s default "+
				"decides a replicated write", i+1)
		}
		if left := time.Until(js.until[i]); left <= jsprovision.SlowAfter {
			t.Errorf("delete %d got %v, inside the %v slow threshold — no "+
				"clustered term or budget is that short, so this is the "+
				"client's own default deciding", i+1, left, jsprovision.SlowAfter)
		}
	}
	if got := c.want.OptStartSeq; got != 4 {
		t.Fatalf("the reset landed at %d, want 4", got)
	}
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
	deletes int
}

func (f *firstReadJS) Consumer(context.Context, string, string) (jetstream.Consumer, error) {
	return reportingConsumer{info: f.held}, nil
}

func (f *firstReadJS) Stream(ctx context.Context, _ string) (jetstream.Stream, error) {
	f.reads++
	until, had := ctx.Deadline()
	f.until, f.had = append(f.until, until), append(f.had, had)
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
// log, three domains' worth spent more than the ceiling their consumers share,
// and the commonest disagreement a busy restart leaves — deliveries in flight
// for the process that stopped — is one no first sequence can change.
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
	// two bounded the read is visible in the deadline it carried.
	const ceiling = 3 * time.Second
	cfg := Config{ClusterName: "crewlet-test", LookupBudget: ceiling}
	name := domainConsumerName("CREWLET_PAGES_LOG", "node-0")

	t.Run("placed at the first survivor", func(t *testing.T) {
		t.Parallel()
		js := &firstReadJS{held: &jetstream.ConsumerInfo{
			Config:    domainConsumerConfig(name, 2),
			Delivered: jetstream.SequenceInfo{Stream: 4},
			AckFloor:  jetstream.SequenceInfo{Stream: 4}}}
		q := &Queue{js: js, cfg: cfg, log: slog.Default()}
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
		if js.deletes != 0 {
			t.Fatal("a consumer the broker placed at the first survivor was " +
				"rebuilt once its first sequence had been read")
		}
	})

	t.Run("in flight at the checkpoint", func(t *testing.T) {
		t.Parallel()
		js := &firstReadJS{held: &jetstream.ConsumerInfo{
			Config:        domainConsumerConfig(name, 2),
			Delivered:     jetstream.SequenceInfo{Stream: 4},
			AckFloor:      jetstream.SequenceInfo{Stream: 2},
			NumAckPending: 2}}
		q := &Queue{js: js, cfg: cfg, log: slog.Default()}
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
