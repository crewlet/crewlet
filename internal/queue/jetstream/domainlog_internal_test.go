package jetstream

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

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
	// Replicas > 1 is the clustered case, whose budget is the long one.
	q := &Queue{js: js, cfg: Config{Replicas: 3}, log: slog.Default()}

	// A CALLER WITH NO DEADLINE OF ITS OWN, which is what a boot passes.
	_, _ = q.ensureDurableConsumer(t.Context(), "CREWLET_AGENT",
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
