package jetstream

import (
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// domainQueue opens an embedded broker and returns it with a domain stream
// already provisioned from spec.
func domainQueue(t *testing.T, spec DomainStream) *Queue {
	t.Helper()
	q := newQueue(t)
	if err := q.EnsureDomainStream(t.Context(), spec); err != nil {
		t.Fatalf("EnsureDomainStream: %v", err)
	}
	return q
}

// probeDomain is the declaration the cases below vary from.
func probeDomain() DomainStream {
	return DomainStream{
		Name:       "CREWLET_PROBE_LOG",
		Subjects:   []string{"crewlet.probe.log.>"},
		MaxBytes:   16 << 20,
		Duplicates: 2 * time.Minute,
	}
}

// A SECOND BOOT WRITES NOTHING to a stream that already exists.
//
// The defect this closes has no crash in it: N nodes each applied their own
// Tier A to one shared stream on every boot, resolved by boot order, so a node
// coming up late with a smaller ceiling silently lowered one an operator had
// just raised — with Tier A UNCHANGED on every node, because max(M, M) is M.
//
// The assertion is on the stream's own reported configuration before and
// after, not on a call count: what matters is that the running stream is the
// same stream, whatever path got there.
func TestBootNeverUpdatesAnExistingStream(t *testing.T) {
	t.Parallel()
	q := domainQueue(t, probeDomain())

	before, err := q.js.Stream(t.Context(), probeDomain().Name)
	if err != nil {
		t.Fatalf("read the created stream: %v", err)
	}
	wantBytes := before.CachedInfo().Config.MaxBytes

	// A SECOND BOOT WITH A DIFFERENT CEILING. Capacity is reported and
	// never applied, so the running stream keeps the value it has.
	q.mu.Lock()
	delete(q.streams, probeDomain().Name)
	q.mu.Unlock()
	raised := probeDomain()
	raised.MaxBytes = 64 << 20
	if err := q.EnsureDomainStream(t.Context(), raised); err != nil {
		t.Fatalf("the second boot refused over a capacity difference: %v", err)
	}

	after, err := q.js.Stream(t.Context(), probeDomain().Name)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got := after.CachedInfo().Config.MaxBytes; got != wantBytes {
		t.Errorf("max_bytes = %d after a boot that wanted %d, want the running "+
			"stream's %d: a booting node is not the writer of a shared "+
			"stream's configuration", got, raised.MaxBytes, wantBytes)
	}
}

// A SAFETY MISMATCH REFUSES TO RUN, naming the field, what is running and what
// this node expected.
//
// Each of these changes what a message on the stream MEANS, so a node that
// carried on would be publishing durable records into a stream that drops
// them or lets any client delete them.
func TestASafetyFieldMismatchRefusesToRun(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		// mutate is applied to the config the SECOND boot expects, so the
		// running stream and this node's spec disagree on one field.
		mutate func(*jetstream.StreamConfig)
		field  string
	}{
		"subjects":             {func(c *jetstream.StreamConfig) { c.Subjects = []string{"crewlet.other.>"} }, "subjects"},
		"retention":            {func(c *jetstream.StreamConfig) { c.Retention = jetstream.InterestPolicy }, "retention"},
		"discard":              {func(c *jetstream.StreamConfig) { c.Discard = jetstream.DiscardOld }, "discard"},
		"deny_delete":          {func(c *jetstream.StreamConfig) { c.DenyDelete = false }, "deny_delete"},
		"allow_rollup":         {func(c *jetstream.StreamConfig) { c.AllowRollup = true }, "allow_rollup"},
		"allow_direct":         {func(c *jetstream.StreamConfig) { c.AllowDirect = true }, "allow_direct"},
		"max_age":              {func(c *jetstream.StreamConfig) { c.MaxAge = time.Hour }, "max_age"},
		"max_msgs_per_subject": {func(c *jetstream.StreamConfig) { c.MaxMsgsPerSubject = 1 }, "max_msgs_per_subject"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			q := domainQueue(t, probeDomain())
			live, err := q.js.Stream(t.Context(), probeDomain().Name)
			if err != nil {
				t.Fatalf("read the created stream: %v", err)
			}

			want := live.CachedInfo().Config
			tc.mutate(&want)
			err = q.observeStream(probeDomain().spec(), want, live)
			if err == nil {
				t.Fatalf("a stream differing on %s was accepted", tc.field)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("refusal = %q, want it to name the field %q: an "+
					"operator has to know which value to change", err, tc.field)
			}
		})
	}
}

// AN R3-CONFIGURED NODE AGAINST AN R1 STREAM REFUSES, because an acknowledged
// publish there is proving fewer copies than stream.replicas promises.
//
// The other direction is FINE and asserted here too: an R1 development node
// against an R3 stream starts normally, since it is getting more durability
// than it asked for rather than less.
func TestR3ConfiguredAgainstAnR1StreamRefusesDurably(t *testing.T) {
	t.Parallel()
	q := domainQueue(t, probeDomain())
	live, err := q.js.Stream(t.Context(), probeDomain().Name)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	wantThree := live.CachedInfo().Config
	wantThree.Replicas = 3
	err = q.observeStream(probeDomain().spec(), wantThree, live)
	if err == nil {
		t.Fatal("a node configured for 3 replicas accepted a 1-replica stream")
	}
	if !strings.Contains(err.Error(), "replicated") {
		t.Errorf("refusal = %q, want it to say what is replicated how far", err)
	}

	wantOne := live.CachedInfo().Config
	wantOne.Replicas = 1
	if err := q.observeStream(probeDomain().spec(), wantOne, live); err != nil {
		t.Errorf("a node configured for 1 replica refused a 1-replica stream: %v", err)
	}
}

// A CAPACITY MISMATCH IS REPORTED, NOT APPLIED and not refused.
//
// The ceiling is an operator's question with an operator's verb behind it, and
// a boot that refused over it would take a fleet down for a difference nobody
// is losing data to.
func TestACapacityFieldMismatchIsReportedNotApplied(t *testing.T) {
	t.Parallel()
	q := domainQueue(t, probeDomain())
	live, err := q.js.Stream(t.Context(), probeDomain().Name)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	for name, mutate := range map[string]func(*jetstream.StreamConfig){
		"max_bytes":  func(c *jetstream.StreamConfig) { c.MaxBytes = 999 << 20 },
		"duplicates": func(c *jetstream.StreamConfig) { c.Duplicates = time.Hour },
	} {
		t.Run(name, func(t *testing.T) {
			want := live.CachedInfo().Config
			mutate(&want)
			if err := q.observeStream(probeDomain().spec(), want, live); err != nil {
				t.Errorf("a %s difference refused the boot: %v — capacity is "+
					"reported, because the remedy is an operator gesture and "+
					"nobody is losing data to the difference", name, err)
			}
			if len(capacityDifferences(want, live.CachedInfo().Config)) == 0 {
				t.Errorf("the %s difference was not detected at all, so the "+
					"report an operator reads is empty", name)
			}
		})
	}
}

// TWO CONCURRENT BOOTS CREATE ONE STREAM, and the loser holds the winner's
// stream to the same comparison rather than trusting it because it lost by
// milliseconds.
func TestTwoConcurrentBootsCreateOneStream(t *testing.T) {
	t.Parallel()
	q := domainQueue(t, probeDomain())

	// The second "boot" is this process forgetting it provisioned the
	// stream, which is exactly the state a peer's boot is in.
	q.mu.Lock()
	delete(q.streams, probeDomain().Name)
	q.mu.Unlock()
	if err := q.EnsureDomainStream(t.Context(), probeDomain()); err != nil {
		t.Fatalf("the second boot failed against an identical stream: %v", err)
	}

	seen := 0
	for name := range q.js.StreamNames(t.Context()).Name() {
		if name == probeDomain().Name {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("the domain stream exists %d times, want once", seen)
	}
}
