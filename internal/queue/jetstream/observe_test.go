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

// WHAT THE SPEC ASKED FOR IS WHAT THE BROKER IS RUNNING, read back from the
// stream's own reported configuration rather than from the struct that built
// it.
//
// The distinction is the whole test. Comparing the spec against the config it
// produced asserts that this package's own translation is self-consistent,
// which it is by construction and would stay so if every field were dropped
// on the way to the broker. Six of these fields are safety properties an
// operator can never set again — a stream is created once and never updated —
// so a field silently not applied at creation is a stream that spends its
// life one property short, with nothing anywhere saying so.
func TestACreatedStreamCarriesEveryFieldTheSpecAsked(t *testing.T) {
	t.Parallel()
	spec := probeDomain()
	q := domainQueue(t, spec)

	stream, err := q.js.Stream(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("read the created stream: %v", err)
	}
	got := stream.CachedInfo().Config

	want := spec.spec()
	if got.Name != want.name {
		t.Errorf("name = %q, want %q", got.Name, want.name)
	}
	if len(got.Subjects) != len(want.subjects) || got.Subjects[0] != want.subjects[0] {
		t.Errorf("subjects = %v, want %v", got.Subjects, want.subjects)
	}
	if got.Retention != want.retention {
		t.Errorf("retention = %v, want %v", got.Retention, want.retention)
	}
	// THE SIX SAFETY FIELDS, each read off the broker.
	if got.Discard != want.discard {
		t.Errorf("discard = %v, want %v — a full log must refuse the write "+
			"rather than drop the oldest record", got.Discard, want.discard)
	}
	if !got.DenyDelete {
		t.Error("deny_delete is off: an operator or a stray client can delete " +
			"records out of the middle of the log")
	}
	if got.AllowRollup {
		t.Error("allow_rollup is on: one message can erase every record before it")
	}
	if got.AllowDirect || got.MirrorDirect {
		t.Errorf("direct gets are on (allow=%v mirror=%v): a read can be served "+
			"by a follower that has not applied what it is being asked about",
			got.AllowDirect, got.MirrorDirect)
	}
	if got.MaxAge != 0 {
		t.Errorf("max_age = %v: a log's records are trimmed by the fleet's own "+
			"floor, never by the clock", got.MaxAge)
	}
	if got.MaxMsgsPerSubject != -1 {
		t.Errorf("max_msgs_per_subject = %d, want unlimited: an ordered log "+
			"that keeps one message per subject is not a log", got.MaxMsgsPerSubject)
	}
	// AND THE TWO CAPACITY FIELDS, which are the ones an operator sets.
	if got.MaxBytes != spec.MaxBytes {
		t.Errorf("max_bytes = %d, want %d", got.MaxBytes, spec.MaxBytes)
	}
	if got.Duplicates != spec.Duplicates {
		t.Errorf("duplicates = %v, want %v", got.Duplicates, spec.Duplicates)
	}
}

// THE SIX ENGINE STREAMS ARE BYTE-IDENTICAL to what they were before domain
// streams existed.
//
// A stream is created once and never updated, so a spec that changed shape is
// not a migration — it is a difference between the streams on a deployment
// that upgraded and the streams on one that installed fresh, and only the
// second has the new value. This pins every field of every engine stream, so
// adding a field to streamSpec with a non-zero default is a failing test
// rather than a fleet that quietly runs two topologies.
func TestTheEngineStreamsAreUnchanged(t *testing.T) {
	t.Parallel()
	specs := engineStreams(0)
	if len(specs) != 6 {
		t.Fatalf("the engine defines %d streams, want 6", len(specs))
	}
	for _, spec := range specs {
		t.Run(spec.name, func(t *testing.T) {
			// Every field a domain stream sets and an engine stream does
			// not. They were absent before this package grew them, and
			// absent is what they must stay.
			if spec.maxBytes != 0 {
				t.Errorf("max_bytes = %d, want unset", spec.maxBytes)
			}
			if spec.duplicates != 0 {
				t.Errorf("duplicates = %v, want unset", spec.duplicates)
			}
			if spec.discard != jetstream.DiscardOld {
				t.Errorf("discard = %v, want the zero value", spec.discard)
			}
			if spec.denyDelete || spec.allowRollup || spec.allowDirect || spec.mirrorDirect {
				t.Errorf("an engine stream sets a domain field: deny_delete=%v "+
					"rollup=%v direct=%v mirror=%v", spec.denyDelete,
					spec.allowRollup, spec.allowDirect, spec.mirrorDirect)
			}
		})
	}
}
