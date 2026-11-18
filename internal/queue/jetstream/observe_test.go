package jetstream

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/queue"
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
		Name:            "CREWLET_PROBE_LOG",
		Subjects:        []string{"crewlet.probe.log.>"},
		MaxBytes:        16 << 20,
		MaxMessageBytes: 64 << 10,
		Duplicates:      2 * time.Minute,
	}
}

// A DOMAIN'S LARGEST MESSAGE IS THE STREAM'S, AND THE BROKER REFUSES PAST IT.
//
// The publisher refuses an oversized record itself; this holds a peer on a
// build that does not. A byte past the declaration is refused with the
// broker's per-message code (10054), which the state log's classifier reads as
// a record too large rather than a full log. A declaration the broker's 32-bit
// field cannot carry is refused at the door: a negative there means unlimited.
func TestADomainsLargestMessageIsEnforcedByTheBroker(t *testing.T) {
	t.Parallel()
	spec := probeDomain()
	q := domainQueue(t, spec)
	live, err := q.js.Stream(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("read the created stream: %v", err)
	}
	if got := int64(live.CachedInfo().Config.MaxMsgSize); got != spec.MaxMessageBytes {
		t.Fatalf("the stream was created with max_msg_size %d, want the declared %d",
			got, spec.MaxMessageBytes)
	}
	log, err := q.DomainLog(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	fits := make([]byte, spec.MaxMessageBytes-1<<10)
	if _, _, err := log.Append(t.Context(), "crewlet.probe.log.a", "op-fits", nil, fits); err != nil {
		t.Fatalf("a message inside the declaration was refused: %v", err)
	}
	over := make([]byte, spec.MaxMessageBytes+1)
	_, _, err = log.Append(t.Context(), "crewlet.probe.log.b", "op-over", nil, over)
	var apiErr *jetstream.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode != 10054 {
		t.Fatalf("a message a byte past the declaration answered %v, want the "+
			"broker's per-message refusal (10054)", err)
	}

	for _, bad := range []int64{-1, math.MaxInt32 + 1} {
		wrong := probeDomain()
		wrong.Name = "CREWLET_PROBE_WRONG"
		wrong.Subjects = []string{"crewlet.probewrong.log.>"}
		wrong.MaxMessageBytes = bad
		if err := q.EnsureDomainStream(t.Context(), wrong); err == nil ||
			!strings.Contains(err.Error(), "max_msg_size") {
			t.Errorf("a largest message of %d was provisioned (%v), and the "+
				"broker's 32-bit field would carry it as something else", bad, err)
		}
	}
}

// A SECOND BOOT WRITES NOTHING to a stream that already exists.
//
// Otherwise each node applies its own Tier A to the shared stream, and a late
// boot with a smaller ceiling silently lowers one an operator raised. Asserted
// on the stream's reported configuration, not a call count: the running stream
// must be unchanged whatever path got there.
func TestBootNeverUpdatesAnExistingStream(t *testing.T) {
	t.Parallel()
	q := domainQueue(t, probeDomain())

	before, err := q.js.Stream(t.Context(), probeDomain().Name)
	if err != nil {
		t.Fatalf("read the created stream: %v", err)
	}
	wantBytes := before.CachedInfo().Config.MaxBytes

	// A second boot with a different ceiling: capacity is reported, never
	// applied.
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
// Each changes what a message on the stream MEANS: a node that carried on
// could publish durable records into a stream that drops or deletes them.
func TestASafetyFieldMismatchRefusesToRun(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		// mutate makes the SECOND boot's expectation differ on one field.
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

// AN R3-CONFIGURED NODE AGAINST AN R1 STREAM REFUSES: an acknowledged publish
// there proves fewer copies than stream.replicas promises. The other direction
// (more durability than asked) starts normally.
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
// The ceiling has an operator's verb behind it; refusing would take a fleet
// down over a difference nobody loses data to.
func TestACapacityFieldMismatchIsReportedNotApplied(t *testing.T) {
	t.Parallel()
	q := domainQueue(t, probeDomain())
	live, err := q.js.Stream(t.Context(), probeDomain().Name)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	for name, mutate := range map[string]func(*jetstream.StreamConfig){
		"max_bytes":    func(c *jetstream.StreamConfig) { c.MaxBytes = 999 << 20 },
		"max_msg_size": func(c *jetstream.StreamConfig) { c.MaxMsgSize = 1 << 20 },
		"duplicates":   func(c *jetstream.StreamConfig) { c.Duplicates = time.Hour },
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

	// The second "boot": forgetting the stream was provisioned, as a peer
	// booting has.
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
// broker, not from the struct that built it (which would pass even if every
// field were dropped on the way). A stream is created once and never updated,
// so a safety field not applied at creation is missing for the stream's life.
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

// THE ENGINE STREAMS SET NO DOMAIN FIELD but the dead letters' duplicate
// window.
//
// A stream is created once and never updated, so a changed spec reaches only
// fresh installs: a new streamSpec field with a non-zero default must fail
// here rather than leave a fleet quietly running two topologies.
//
// The window is pinned too: a keep's identity is its message id, and
// CREWLET_DLQ's window makes a second keep of one identity nothing
// (queue.DeadLetterWindow).
func TestTheEngineStreamsAreUnchanged(t *testing.T) {
	t.Parallel()
	specs := engineStreams(0)
	if len(specs) != 8 {
		t.Fatalf("the engine defines %d streams, want 8", len(specs))
	}
	for _, spec := range specs {
		t.Run(spec.name, func(t *testing.T) {
			// Every field a domain stream sets and an engine stream must not.
			if spec.maxBytes != 0 {
				t.Errorf("max_bytes = %d, want unset", spec.maxBytes)
			}
			window := time.Duration(0)
			if spec.name == streamDeadLetter {
				window = queue.DeadLetterWindow
			}
			if spec.duplicates != window {
				t.Errorf("duplicates = %v, want %v", spec.duplicates, window)
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

// A LOST CREATION RACE WRITES NOTHING EITHER: the rule of
// [TestBootNeverUpdatesAnExistingStream] on the path that test cannot reach.
//
// [Queue.createOrObserveStream] reads first, but N nodes booting together all
// read "absent" and all call create. The loser's create must fail with
// [jetstream.ErrStreamNameAlreadyInUse] so the caller reads back what the
// winner made; `CreateOrUpdateStream` never returns it, it UPDATES, so every
// loser would rewrite the shared stream with its own configuration.
func TestALostCreationRaceNeverRewritesTheStream(t *testing.T) {
	t.Parallel()
	q := domainQueue(t, probeDomain())
	name := probeDomain().Name

	before, err := q.js.Stream(t.Context(), name)
	if err != nil {
		t.Fatalf("read the created stream: %v", err)
	}
	want := before.CachedInfo().Config.MaxBytes

	// The loser's own call: after a peer's create committed, before this
	// node looks again.
	lost := before.CachedInfo().Config
	lost.MaxBytes = want / 2
	err = q.createStream(t.Context(), lost)
	if err == nil {
		t.Fatal("creating a stream that already exists reported success, so " +
			"the caller's read-back branch never runs and a losing node's " +
			"configuration is what the fleet ends up with")
	}

	after, err := q.js.Stream(t.Context(), name)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got := after.CachedInfo().Config.MaxBytes; got != want {
		t.Errorf("max_bytes = %d after a lost creation race that wanted %d, "+
			"want the running stream's %d — the node that lost the race "+
			"rewrote the stream it lost to", got, lost.MaxBytes, want)
	}
}
